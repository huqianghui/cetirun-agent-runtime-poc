// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/auth"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/config"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/kube"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func adminManagementRequest(method, body string) *http.Request {
	r := httptest.NewRequest(method, "/api/v1/admin/instances/alice", strings.NewReader(body))
	r.SetPathValue("name", "alice")
	return r.WithContext(auth.NewContext(r.Context(), auth.Claims{Alias: "administrator", Subject: "admin-subject", IsAdmin: true}))
}

func adminManagementHandler(c client.Client) *AdminInstanceHandler {
	return &AdminInstanceHandler{Kube: kube.NewClient(c), Config: config.Config{AllowedAgentTypes: []string{"hermes", "openclaw", "custom"}}}
}

func adminManagedInstance() *api.Agent {
	instance := sampleInstance()
	instance.UID = types.UID("foreign-agent-uid")
	instance.ResourceVersion = "7"
	instance.Spec.Agent = api.HarnessSpec{
		Type:              api.AgentTypeOpenClaw,
		Image:             ptr.To("registry.example.com/agents/openclaw@sha256:" + strings.Repeat("a", 64)),
		Username:          ptr.To("vibe"),
		Port:              ptr.To(int32(8080)),
		RegistrySecretRef: &api.RegistrySecretRef{Name: "registry-source-foreign"},
	}
	instance.Spec.User.Alias = "foreign-owner"
	instance.Spec.User.DisplayName = "Foreign Owner"
	instance.Spec.User.Email = "foreign-owner@example.com"
	instance.Spec.Model = "shared/original"
	instance.Spec.Helm.Version = "1.2.3"
	instance.Spec.GatewayTokenSecretRef = "gateway-token"
	instance.Annotations = map[string]string{api.AnnotationOwnerTenantID: "tenant-foreign", api.AnnotationOwnerObjectID: "object-foreign"}
	return instance
}

func TestAdminAgentDetailAndUpdateCrossOwnerDoNotReadSecrets(t *testing.T) {
	instance := adminManagedInstance()
	secretReads := 0
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithGlobalResourceVersionCounter().WithObjects(
		instance,
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "gateway-token", Namespace: "cetirun-alice"}},
	).WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, storage client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
		if _, ok := object.(*corev1.Secret); ok {
			secretReads++
		}
		return storage.Get(ctx, key, object, opts...)
	}}).Build()
	h := adminManagementHandler(fc)

	get := httptest.NewRecorder()
	h.GetInstance(get, adminManagementRequest(http.MethodGet, ""))
	if get.Code != http.StatusOK {
		t.Fatalf("admin detail: status=%d body=%s", get.Code, get.Body.String())
	}
	if strings.Contains(get.Body.String(), "gateway-token") || strings.Contains(get.Body.String(), "registry-source-foreign") {
		t.Fatalf("admin detail disclosed a secret reference: %s", get.Body.String())
	}
	var detail AdminInstanceDetail
	if err := json.Unmarshal(get.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Alias != "foreign-owner" || !detail.RegistryAuthConfigured || detail.UID != string(instance.UID) || detail.ResourceVersion == "" {
		t.Fatalf("unexpected cross-owner detail: %+v", detail)
	}

	current, err := h.Kube.GetInstanceByName(context.Background(), instance.Name)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"uid":"` + string(current.UID) + `","resourceVersion":"` + current.ResourceVersion + `","displayName":" Updated foreign agent ","model":"shared/updated","agent":{"image":"registry.example.com/agent:2"}}`
	update := httptest.NewRecorder()
	h.UpdateInstance(update, adminManagementRequest(http.MethodPatch, body))
	if update.Code != http.StatusOK {
		t.Fatalf("admin update: status=%d body=%s", update.Code, update.Body.String())
	}
	updated, err := h.Kube.GetInstanceByName(context.Background(), instance.Name)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Spec.User.DisplayName != "Updated foreign agent" || updated.Spec.Model != "shared/updated" || updated.Spec.Agent.Image == nil || *updated.Spec.Agent.Image != "registry.example.com/agent:2" {
		t.Fatalf("requested update not persisted: %+v", updated.Spec)
	}
	if updated.Spec.Team != instance.Spec.Team || updated.Spec.SandboxRef != instance.Spec.SandboxRef || !reflect.DeepEqual(updated.Spec.Agent.RegistrySecretRef, instance.Spec.Agent.RegistrySecretRef) || !reflect.DeepEqual(updated.Spec.Agent.Port, instance.Spec.Agent.Port) {
		t.Fatalf("admin edit changed protected or unrelated state: %+v", updated.Spec)
	}
	if secretReads != 0 {
		t.Fatalf("admin read/update read %d Secret objects", secretReads)
	}
}

func TestAdminAgentAuditRecordsFieldsWithoutValues(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	instance := adminManagedInstance()
	instance.Spec.Agent.Type = api.AgentTypeCustom
	h := adminManagementHandler(newFakeClient(instance))
	w := httptest.NewRecorder()
	h.UpdateInstance(w, adminManagementRequest(http.MethodPatch,
		`{"uid":"foreign-agent-uid","resourceVersion":"7","displayName":"Synthetic audit display","agent":{"image":"registry.example.com/agent:audit-test","port":43210}}`))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var record struct {
		Actor  string   `json:"actor"`
		Action string   `json:"action"`
		UID    string   `json:"uid"`
		Fields []string `json:"fields"`
		Status int      `json:"status"`
	}
	if err := json.Unmarshal(logs.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record.Actor != "administrator" || record.Action != "update" || record.UID != "foreign-agent-uid" || record.Status != http.StatusOK ||
		!reflect.DeepEqual(record.Fields, []string{"agent.image", "agent.port", "displayName"}) {
		t.Fatalf("unexpected audit record: %+v", record)
	}
	for _, value := range []string{"Synthetic audit display", "registry.example.com/agent:audit-test", "43210"} {
		if strings.Contains(logs.String(), value) {
			t.Fatalf("audit log included a configuration value: %s", value)
		}
	}
}

func TestAdminAgentUpdateRequiresVersionAndStrictAllowlist(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		want int
	}{
		{"missing version", `{"displayName":"new name"}`, http.StatusPreconditionRequired},
		{"unknown top-level field", `{"uid":"foreign-agent-uid","resourceVersion":"7","team":"other"}`, http.StatusBadRequest},
		{"immutable nested field", `{"uid":"foreign-agent-uid","resourceVersion":"7","agent":{"type":"custom"}}`, http.StatusBadRequest},
		{"credential field", `{"uid":"foreign-agent-uid","resourceVersion":"7","agent":{"registrySecretRef":{"name":"foreign"}}}`, http.StatusBadRequest},
		{"duplicate field", `{"uid":"foreign-agent-uid","uid":"foreign-agent-uid","resourceVersion":"7","displayName":"new"}`, http.StatusBadRequest},
		{"stale replacement uid", `{"uid":"old-uid","resourceVersion":"7","displayName":"new"}`, http.StatusConflict},
		{"stale resource version", `{"uid":"foreign-agent-uid","resourceVersion":"old","displayName":"new"}`, http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			instance := adminManagedInstance()
			h := adminManagementHandler(newFakeClient(instance))
			w := httptest.NewRecorder()
			h.UpdateInstance(w, adminManagementRequest(http.MethodPatch, test.body))
			if w.Code != test.want {
				t.Fatalf("status=%d want=%d body=%s", w.Code, test.want, w.Body.String())
			}
			current, err := h.Kube.GetInstanceByName(context.Background(), instance.Name)
			if err != nil || current.Spec.User.DisplayName != instance.Spec.User.DisplayName || !reflect.DeepEqual(current.Spec.Agent.RegistrySecretRef, instance.Spec.Agent.RegistrySecretRef) {
				t.Fatalf("rejected update persisted changes: instance=%+v err=%v", current, err)
			}
		})
	}
}

func TestAdminAgentUpdateRejectsRetiredTeamsEnabledWithoutMutation(t *testing.T) {
	for _, value := range []string{"true", "false", "null"} {
		t.Run(value, func(t *testing.T) {
			instance := adminManagedInstance()
			h := adminManagementHandler(newFakeClient(instance))
			before, err := h.Kube.GetInstanceByName(context.Background(), instance.Name)
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			h.UpdateInstance(w, adminManagementRequest(http.MethodPatch,
				`{"uid":"foreign-agent-uid","resourceVersion":"7","teamsEnabled":`+value+`}`))
			if w.Code != http.StatusBadRequest || errorCode(t, w) != "invalid_body" {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			after, err := h.Kube.GetInstanceByName(context.Background(), instance.Name)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatal("rejected retired field mutated the Agent")
			}
		})
	}
}

func TestAdminAgentUpdateValidatesRuntimeSpecificFieldsAndRequestShape(t *testing.T) {
	for _, test := range []struct {
		name      string
		configure func(*api.Agent)
		body      string
		want      int
	}{
		{"valid Custom web UI edit", func(instance *api.Agent) {
			instance.Spec.Agent.Type = api.AgentTypeCustom
			instance.Spec.Agent.Image = ptr.To(managementImage)
			instance.Spec.Agent.Username = ptr.To("vibe")
		}, `{"agent":{"webUI":true}}`, http.StatusOK},
		{"builtin web UI true rejected", func(*api.Agent) {}, `{"agent":{"webUI":true}}`, http.StatusBadRequest},
		{"builtin web UI false rejected", func(*api.Agent) {}, `{"agent":{"webUI":false}}`, http.StatusBadRequest},
		{"invalid Custom port", func(instance *api.Agent) { instance.Spec.Agent.Type = api.AgentTypeCustom }, `{"agent":{"port":0}}`, http.StatusBadRequest},
		{"pinned image cannot clear", func(*api.Agent) {}, `{"agent":{"image":null}}`, http.StatusBadRequest},
		{"null top level field", func(*api.Agent) {}, `{"displayName":null}`, http.StatusBadRequest},
		{"null agent", func(*api.Agent) {}, `{"agent":null}`, http.StatusBadRequest},
		{"non-object body", func(*api.Agent) {}, `[]`, http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			instance := adminManagedInstance()
			test.configure(instance)
			h := adminManagementHandler(newFakeClient(instance))
			current, err := h.Kube.GetInstanceByName(context.Background(), instance.Name)
			if err != nil {
				t.Fatal(err)
			}
			body := test.body
			if strings.HasPrefix(body, "{") {
				body = `{"uid":` + fmt.Sprintf("%q", current.UID) + `,"resourceVersion":` + fmt.Sprintf("%q", current.ResourceVersion) + `,` + body[1:]
			}
			w := httptest.NewRecorder()
			h.UpdateInstance(w, adminManagementRequest(http.MethodPatch, body))
			if w.Code != test.want {
				t.Fatalf("status=%d want=%d body=%s", w.Code, test.want, w.Body.String())
			}
		})
	}
}

func TestAdminAgentPortUpdatesAreCustomOnly(t *testing.T) {
	for _, agentType := range []api.AgentType{api.AgentTypeOpenClaw, api.AgentTypeHermes, "", api.AgentTypeCustom} {
		for _, port := range []string{"9090", "null", "8080"} {
			t.Run(string(agentType)+"/"+port, func(t *testing.T) {
				instance := adminManagedInstance()
				instance.Spec.Agent.Type = agentType
				h := adminManagementHandler(newFakeClient(instance))
				before, err := h.Kube.GetInstanceByName(context.Background(), instance.Name)
				if err != nil {
					t.Fatal(err)
				}
				w := httptest.NewRecorder()
				h.UpdateInstance(w, adminManagementRequest(http.MethodPatch,
					fmt.Sprintf(`{"uid":%q,"resourceVersion":%q,"displayName":"Updated name","agent":{"port":%s}}`, before.UID, before.ResourceVersion, port)))
				after, err := h.Kube.GetInstanceByName(context.Background(), instance.Name)
				if err != nil {
					t.Fatal(err)
				}
				if agentType != api.AgentTypeCustom {
					if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "agent.port is configurable only for custom agents") {
						t.Fatalf("built-in port update: status=%d body=%s", w.Code, w.Body.String())
					}
					if !reflect.DeepEqual(before, after) {
						t.Fatal("rejected port update changed the Agent")
					}
					return
				}
				if w.Code != http.StatusOK || after.Spec.User.DisplayName != "Updated name" {
					t.Fatalf("Custom port update: status=%d body=%s", w.Code, w.Body.String())
				}
				var expected *int32
				if err := json.Unmarshal([]byte(port), &expected); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(after.Spec.Agent.Port, expected) {
					t.Fatal("Custom port change was not persisted")
				}
			})
		}
	}
}

func TestAdminAgentUpdateRejectsCrossSiteAndAllowsMetadataWhenRuntimeDisabled(t *testing.T) {
	instance := adminManagedInstance()
	h := adminManagementHandler(newFakeClient(instance))
	request := adminManagementRequest(http.MethodPatch, `{"uid":"foreign-agent-uid","resourceVersion":"7","displayName":"new"}`)
	request.Header.Set("Sec-Fetch-Site", "cross-site")
	response := httptest.NewRecorder()
	h.UpdateInstance(response, request)
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "invalid_origin") {
		t.Fatalf("cross-site update: status=%d body=%s", response.Code, response.Body.String())
	}

	disabled := adminManagedInstance()
	metadataOnly := adminManagementHandler(newFakeClient(disabled))
	metadataOnly.Config.AllowedAgentTypes = []string{"custom"}
	response = httptest.NewRecorder()
	metadataOnly.UpdateInstance(response, adminManagementRequest(http.MethodPatch, `{"uid":"foreign-agent-uid","resourceVersion":"7","displayName":"metadata only"}`))
	if response.Code != http.StatusOK {
		t.Fatalf("metadata update was blocked by disabled runtime type: %d %s", response.Code, response.Body.String())
	}
	current, err := metadataOnly.Kube.GetInstanceByName(context.Background(), disabled.Name)
	if err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	metadataOnly.UpdateInstance(response, adminManagementRequest(http.MethodPatch,
		fmt.Sprintf(`{"uid":%q,"resourceVersion":%q,"agent":{"image":"registry.example.com/agent:2"}}`, current.UID, current.ResourceVersion)))
	if response.Code != http.StatusForbidden {
		t.Fatalf("admin bypassed the runtime type restriction: %d %s", response.Code, response.Body.String())
	}
}

func TestAdminAgentUpdateRejectsDeletingInstanceAndAdminDoesNotBypassOwnerRoute(t *testing.T) {
	instance := adminManagedInstance()
	now := metav1.Now()
	instance.Finalizers = []string{api.Finalizer}
	instance.DeletionTimestamp = &now
	h := adminManagementHandler(newFakeClient(instance))
	w := httptest.NewRecorder()
	h.UpdateInstance(w, adminManagementRequest(http.MethodPatch, `{"uid":"foreign-agent-uid","resourceVersion":"7","displayName":"new"}`))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "instance_deleting") {
		t.Fatalf("deleting update: status=%d body=%s", w.Code, w.Body.String())
	}

	active := adminManagedInstance()
	ownerHandler := &InstanceHandler{Kube: kube.NewClient(newFakeClient(active)), Config: config.Config{AllowedAgentTypes: []string{"openclaw"}}}
	ownerRequest := adminManagementRequest(http.MethodPatch, `{"uid":"foreign-agent-uid","resourceVersion":"7","displayName":"new"}`)
	ownerRequest.SetPathValue("name", active.Name)
	ownerResponse := httptest.NewRecorder()
	ownerHandler.UpdateInstanceByName(ownerResponse, ownerRequest)
	if ownerResponse.Code != http.StatusForbidden {
		t.Fatalf("ordinary owner route accepted a cross-owner admin request: %d %s", ownerResponse.Code, ownerResponse.Body.String())
	}
}

func TestAdminAgentDeleteAcceptsVersionedRequestAndRetainsFinalizer(t *testing.T) {
	instance := adminManagedInstance()
	instance.Finalizers = []string{api.Finalizer}
	h := adminManagementHandler(newFakeClient(instance))
	w := httptest.NewRecorder()
	h.DeleteInstance(w, adminManagementRequest(http.MethodDelete, `{"uid":"foreign-agent-uid","resourceVersion":"7"}`))
	if w.Code != http.StatusAccepted {
		t.Fatalf("delete: status=%d body=%s", w.Code, w.Body.String())
	}
	current, err := h.Kube.GetInstanceByName(context.Background(), instance.Name)
	if err != nil || current.DeletionTimestamp.IsZero() || !reflect.DeepEqual(current.Finalizers, []string{api.Finalizer}) {
		t.Fatalf("delete did not retain operator cleanup state: instance=%+v err=%v", current, err)
	}
}

func TestAdminAgentDeleteRejectsStaleVersionWithoutDeleting(t *testing.T) {
	instance := adminManagedInstance()
	h := adminManagementHandler(newFakeClient(instance))
	w := httptest.NewRecorder()
	h.DeleteInstance(w, adminManagementRequest(http.MethodDelete, `{"uid":"previous-agent","resourceVersion":"7"}`))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "instance_changed") {
		t.Fatalf("stale delete: status=%d body=%s", w.Code, w.Body.String())
	}
	current, err := h.Kube.GetInstanceByName(context.Background(), instance.Name)
	if err != nil || !current.DeletionTimestamp.IsZero() {
		t.Fatalf("stale delete changed Agent: instance=%+v err=%v", current, err)
	}
}

func TestAdminAgentStatusReportsDiagnosticPartialFailures(t *testing.T) {
	instance := adminManagedInstance()
	instance.Status.Namespace = "cetirun-alice"
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: instance.Status.Namespace, UID: "namespace-uid", OwnerReferences: []metav1.OwnerReference{{
		APIVersion: api.GroupVersion.String(), Kind: "Agent", Name: instance.Name, UID: instance.UID,
	}}}}
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(instance, namespace).
		WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, storage client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			switch list.(type) {
			case *corev1.EventList, *corev1.PodList:
				return errors.New("synthetic diagnostic list failure")
			default:
				return storage.List(ctx, list, opts...)
			}
		}}).Build()
	h := adminManagementHandler(fc)
	w := httptest.NewRecorder()
	h.GetInstanceStatus(w, adminManagementRequest(http.MethodGet, ""))
	if w.Code != http.StatusOK {
		t.Fatalf("diagnostic partial failure should retain status response: %d %s", w.Code, w.Body.String())
	}
	var status AdminInstanceStatus
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.UID != string(instance.UID) || len(status.Errors) != 3 || status.Errors[0].Section != "events" || status.Errors[1].Section != "nsEvents" || status.Errors[2].Section != "pods" {
		t.Fatalf("partial diagnostics were not reported per section: %+v", status)
	}
}

func TestAdminAgentStatusDoesNotQueryAnUnassignedNamespace(t *testing.T) {
	instance := adminManagedInstance()
	instance.Status.Namespace = ""
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(instance).
		WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, storage client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			options := (&client.ListOptions{}).ApplyOptions(opts)
			if _, ok := list.(*corev1.EventList); !ok || options.FieldSelector == nil ||
				!strings.Contains(options.FieldSelector.String(), "involvedObject.uid="+string(instance.UID)) {
				t.Fatalf("unassigned Agent queried namespace diagnostics: %T %+v", list, options)
			}
			return nil
		}}).Build()
	w := httptest.NewRecorder()
	adminManagementHandler(fc).GetInstanceStatus(w, adminManagementRequest(http.MethodGet, ""))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var status AdminInstanceStatus
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if len(status.Errors) != 2 || len(status.Pods) != 0 || len(status.NsEvents) != 0 {
		t.Fatalf("unassigned namespace did not retain diagnostic errors: %+v", status)
	}
}

func TestAdminAgentStatusRejectsSameNameReplacement(t *testing.T) {
	instance := adminManagedInstance()
	instance.Status.Namespace = "cetirun-alice"
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: instance.Status.Namespace, UID: "namespace-uid", OwnerReferences: []metav1.OwnerReference{{
		APIVersion: api.GroupVersion.String(), Kind: "Agent", Name: instance.Name, UID: instance.UID,
	}}}}
	agentGets := 0
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(instance, namespace).
		WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, storage client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
			if err := storage.Get(ctx, key, object, opts...); err != nil {
				return err
			}
			agent, ok := object.(*api.Agent)
			if !ok {
				return nil
			}
			agentGets++
			if agentGets != 1 {
				return nil
			}
			replacement := agent.DeepCopy()
			replacement.UID = "replacement-agent-uid"
			replacement.ResourceVersion = ""
			if err := storage.Delete(ctx, agent); err != nil {
				return err
			}
			return storage.Create(ctx, replacement)
		}}).Build()
	h := adminManagementHandler(fc)
	w := httptest.NewRecorder()
	h.GetInstanceStatus(w, adminManagementRequest(http.MethodGet, ""))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "instance_changed") {
		t.Fatalf("same-name replacement status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestAdminAgentStatusDiscardsDiagnosticsFromReplacedNamespace(t *testing.T) {
	instance := adminManagedInstance()
	instance.Status.Namespace = "cetirun-alice"
	owner := metav1.OwnerReference{APIVersion: api.GroupVersion.String(), Kind: "Agent", Name: instance.Name, UID: instance.UID}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: instance.Status.Namespace, UID: "namespace-before", OwnerReferences: []metav1.OwnerReference{owner}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "agent-pod", Namespace: instance.Status.Namespace}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	fc := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(instance, namespace, pod).
		WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, storage client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*corev1.EventList); ok {
				return nil
			}
			if err := storage.List(ctx, list, opts...); err != nil {
				return err
			}
			if _, ok := list.(*corev1.PodList); !ok {
				return nil
			}
			current := &corev1.Namespace{}
			if err := storage.Get(ctx, client.ObjectKey{Name: instance.Status.Namespace}, current); err != nil {
				return err
			}
			if err := storage.Delete(ctx, current); err != nil {
				return err
			}
			replacement := current.DeepCopy()
			replacement.UID = "namespace-after"
			replacement.ResourceVersion = ""
			return storage.Create(ctx, replacement)
		}}).Build()
	w := httptest.NewRecorder()
	adminManagementHandler(fc).GetInstanceStatus(w, adminManagementRequest(http.MethodGet, ""))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var status AdminInstanceStatus
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if len(status.Pods) != 0 || len(status.NsEvents) != 0 || len(status.Errors) != 2 ||
		status.Errors[0].Section != "pods" || status.Errors[1].Section != "nsEvents" {
		t.Fatalf("replacement namespace diagnostics were retained: %+v", status)
	}
}

// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/auth"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/config"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/kube"
	"github.com/Azure/CetiRun/app/internal/registryauth"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const registryCreateBody = `{"agent":{"type":"hermes","image":"registry.example.com/agent:1"},"team":"aks","registryAuth":{"username":"registry-user","password":"synthetic-write-only-token"}}`

func registryHandler(t *testing.T, failSave *bool) (*InstanceHandler, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithStatusSubresource(&api.Agent{}).
		WithObjects(gatewayTeam("aks"), catalogConfigMap("gpt-6", "gpt-6-astra")).
		WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if review, ok := obj.(*authorizationv1.SubjectAccessReview); ok {
				review.Status.Denied = true
				return nil
			}
			if secret, ok := obj.(*corev1.Secret); ok && secret.Type == corev1.SecretTypeDockerConfigJson && *failSave {
				return errors.New("synthetic-write-only-token raw upstream response")
			}
			if obj.GetUID() == "" {
				obj.SetUID(types.UID("uid-" + obj.GetName()))
			}
			return c.Create(ctx, obj, opts...)
		}}).Build()
	return &InstanceHandler{Kube: kube.NewClient(c), Config: config.Config{MaxInstancesPerUser: 1, PortalOrigin: "https://portal.example.com", AIGatewayEnabled: true, AIGatewayCatalogNamespace: "cetirun-system"}}, c
}

func registryRequest(method, body string, instance *api.Agent) *http.Request {
	r := httptest.NewRequest(method, "https://portal.example.com/api/v1/instances", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "https://portal.example.com")
	r.Header.Set("Idempotency-Key", "synthetic-create-request-0001")
	if instance != nil {
		r.SetPathValue("name", instance.Name)
		r.Header.Set("If-Match", kube.RegistryETag(instance))
	}
	return withClaims(r, "alice", "Alice Example", "alice@example.com")
}

func createdRegistryInstance(t *testing.T, h *InstanceHandler) *api.Agent {
	t.Helper()
	w := httptest.NewRecorder()
	h.CreateInstance(w, registryRequest(http.MethodPost, registryCreateBody, nil))
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	a, err := h.Kube.GetInstanceByName(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestRegistryPartialCreateRetryBeforeQuota(t *testing.T) {
	failing := true
	h, c := registryHandler(t, &failing)
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	w := httptest.NewRecorder()
	h.CreateInstance(w, registryRequest(http.MethodPost, registryCreateBody, nil))
	if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String()+logs.String(), "synthetic-write-only-token") {
		t.Fatalf("unsafe partial failure: %d", w.Code)
	}
	first, err := h.Kube.GetInstanceByName(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if first.Spec.Agent.RegistrySecretRef == nil || first.UID == "" {
		t.Fatal("partial create lost recovery identity")
	}
	namespaces := &corev1.NamespaceList{}
	_ = c.List(context.Background(), namespaces)
	if len(namespaces.Items) != 0 {
		t.Fatal("Registry Portal path created a runtime namespace")
	}
	failing = false
	// A new handler and kube wrapper models Portal restart.
	h.Kube = kube.NewClient(c)
	w = httptest.NewRecorder()
	h.CreateInstance(w, registryRequest(http.MethodPost, registryCreateBody, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("resume failed behind quota: %d %s", w.Code, w.Body.String())
	}
	current, _ := h.Kube.GetInstanceByName(context.Background(), "alice")
	if current.UID != first.UID || !h.Kube.RegistryAuthConfigured(context.Background(), current) {
		t.Fatal("retry did not finish the same instance")
	}
	source := &corev1.Secret{}
	key := client.ObjectKey{Namespace: registryauth.DefaultNamespace, Name: current.Spec.Agent.RegistrySecretRef.Name}
	_ = c.Get(context.Background(), key, source)
	before := source.DeepCopy()
	w = httptest.NewRecorder()
	h.CreateInstance(w, registryRequest(http.MethodPost, strings.ReplaceAll(registryCreateBody, "synthetic-write-only-token", "different-token"), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("completed retry: %d", w.Code)
	}
	_ = c.Get(context.Background(), key, source)
	if !reflect.DeepEqual(before, source) {
		t.Fatal("create retry rotated saved credentials")
	}
	var response InstanceResponse
	if json.Unmarshal(w.Body.Bytes(), &response) != nil || !response.RegistryAuthConfigured || response.Agent.RegistrySecretRef == nil {
		t.Fatal("missing configured state")
	}
	if strings.Contains(w.Body.String(), "registry-user") || strings.Contains(w.Body.String(), "synthetic-write-only-token") || strings.Contains(w.Body.String(), ".dockerconfigjson") {
		t.Fatal("credentials echoed")
	}
	for _, mutate := range []func(*http.Request){
		func(r *http.Request) { r.Header.Set("Idempotency-Key", "different-create-request-0001") },
		func(r *http.Request) { r.Body = ioBody(strings.ReplaceAll(registryCreateBody, "agent:1", "agent:2")) },
	} {
		r := registryRequest(http.MethodPost, registryCreateBody, nil)
		mutate(r)
		w = httptest.NewRecorder()
		h.CreateInstance(w, r)
		if w.Code != http.StatusConflict {
			t.Fatalf("mismatched retry: %d", w.Code)
		}
	}
}

type registryAuditCounter struct {
	client.Client
	reviews atomic.Int64
	allow   atomic.Bool
}

func (c *registryAuditCounter) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	if review, ok := object.(*authorizationv1.SubjectAccessReview); ok {
		c.reviews.Add(1)
		review.Status.Allowed = c.allow.Load()
		review.Status.Denied = !review.Status.Allowed
		return nil
	}
	return c.Client.Create(ctx, object, options...)
}

func TestRegistryCompletedCreateRetrySkipsAuditButPUTRechecks(t *testing.T) {
	failing := false
	h, original := registryHandler(t, &failing)
	c := &registryAuditCounter{Client: original}
	h.Kube = kube.NewClient(c)
	agent := createdRegistryInstance(t, h)
	if c.reviews.Load() == 0 {
		t.Fatal("initial credential save skipped authorization")
	}
	c.reviews.Store(0)
	w := httptest.NewRecorder()
	h.CreateInstance(w, registryRequest(http.MethodPost, registryCreateBody, nil))
	if w.Code != http.StatusOK || c.reviews.Load() != 0 {
		t.Fatalf("completed creation retried a write audit: status=%d reviews=%d", w.Code, c.reviews.Load())
	}
	c.allow.Store(true)
	w = httptest.NewRecorder()
	h.PutRegistryAuth(w, registryRequest(http.MethodPut, `{"username":"example","password":"replacement-token"}`, agent))
	if w.Code != http.StatusServiceUnavailable || c.reviews.Load() == 0 {
		t.Fatalf("credential replacement did not recheck changed permissions: status=%d reviews=%d", w.Code, c.reviews.Load())
	}
}

type stringBody struct{ *strings.Reader }

func (stringBody) Close() error  { return nil }
func ioBody(s string) stringBody { return stringBody{strings.NewReader(s)} }

func TestRegistryReplaceRemoveAndConcurrency(t *testing.T) {
	failing := false
	h, c := registryHandler(t, &failing)
	agent := createdRegistryInstance(t, h)
	key := client.ObjectKey{Namespace: registryauth.DefaultNamespace, Name: agent.Spec.Agent.RegistrySecretRef.Name}
	source := &corev1.Secret{}
	_ = c.Get(context.Background(), key, source)
	originalUID := source.UID
	body := `{"username":"next-user","password":"next-synthetic-token"}`
	w := httptest.NewRecorder()
	h.PutRegistryAuth(w, registryRequest(http.MethodPut, body, agent))
	if w.Code != http.StatusOK {
		t.Fatalf("rotation failed: %d %s", w.Code, w.Body.String())
	}
	_ = c.Get(context.Background(), key, source)
	if source.UID != originalUID || !strings.Contains(string(source.Data[corev1.DockerConfigJsonKey]), "next-user") {
		t.Fatal("rotation did not update same source")
	}
	// An old If-Match cannot rotate or detach a newer operation.
	w = httptest.NewRecorder()
	h.DeleteRegistryAuth(w, registryRequest(http.MethodDelete, "", agent))
	if w.Code != http.StatusConflict {
		t.Fatal("stale write was accepted")
	}
	agent, _ = h.Kube.GetInstanceByName(context.Background(), "alice")
	w = httptest.NewRecorder()
	h.DeleteRegistryAuth(w, registryRequest(http.MethodDelete, "", agent))
	if w.Code != http.StatusOK {
		t.Fatalf("remove failed: %d", w.Code)
	}
	agent, _ = h.Kube.GetInstanceByName(context.Background(), "alice")
	if agent.Spec.Agent.RegistrySecretRef != nil || h.Kube.RegistryAuthConfigured(context.Background(), agent) {
		t.Fatal("reference was not detached")
	}
	if c.Get(context.Background(), key, source) != nil {
		t.Fatal("Portal deleted a source before Operator removed its binding")
	}
	w = httptest.NewRecorder()
	h.PutRegistryAuth(w, registryRequest(http.MethodPut, body, agent))
	if w.Code != http.StatusOK {
		t.Fatalf("reattach failed: %d", w.Code)
	}
	agent, _ = h.Kube.GetInstanceByName(context.Background(), "alice")
	if agent.Spec.Agent.RegistrySecretRef.Name == key.Name {
		t.Fatal("detached source reused")
	}
}

func TestRegistryCreateRetryCannotBecomeAnotherInstance(t *testing.T) {
	failing := false
	h, _ := registryHandler(t, &failing)
	h.Config.MaxInstancesPerUser = 0
	createdRegistryInstance(t, h)
	w := httptest.NewRecorder()
	body := strings.Replace(registryCreateBody, `{"agent":`, `{"instanceName":"different","agent":`, 1)
	h.CreateInstance(w, registryRequest(http.MethodPost, body, nil))
	if w.Code != http.StatusConflict {
		t.Fatalf("request ID was reused for another instance: %d", w.Code)
	}
	instances, err := h.Kube.ListAllInstances(context.Background())
	if err != nil || len(instances) != 1 {
		t.Fatal("creation retry created a second Agent")
	}
}

func TestRegistryAuthorizationStrictInputsAndNoLeak(t *testing.T) {
	for _, test := range []struct {
		name, body string
		mutate     func(*http.Request)
		status     int
	}{
		{"empty", `{"username":"example","password":""}`, nil, 400},
		{"mask", `{"username":"example","password":"********"}`, nil, 400},
		{"host", `{"username":"example","password":"synthetic","host":"other.example.com"}`, nil, 400},
		{"duplicate", `{"username":"example","password":"synthetic","password":"other"}`, nil, 400},
		{"oversized", `{"username":"example","password":"` + strings.Repeat("x", registryauth.MaxBodyBytes) + `"}`, nil, 400},
		{"origin", `{"username":"example","password":"synthetic"}`, func(r *http.Request) { r.Header.Set("Origin", "https://other.example.com") }, 403},
		{"owner", `{"username":"example","password":"synthetic"}`, func(r *http.Request) { *r = *withClaims(r, "alice", "Other Alice", "alice@other.example.com") }, 403},
		{"version", `{"username":"example","password":"synthetic"}`, func(r *http.Request) { r.Header.Set("If-Match", `"old-uid/1"`) }, 409},
	} {
		t.Run(test.name, func(t *testing.T) {
			failing := false
			h, c := registryHandler(t, &failing)
			agent := createdRegistryInstance(t, h)
			source := &corev1.Secret{}
			key := client.ObjectKey{Namespace: registryauth.DefaultNamespace, Name: agent.Spec.Agent.RegistrySecretRef.Name}
			_ = c.Get(context.Background(), key, source)
			before := source.DeepCopy()
			r := registryRequest(http.MethodPut, test.body, agent)
			if test.mutate != nil {
				test.mutate(r)
			}
			w := httptest.NewRecorder()
			h.PutRegistryAuth(w, r)
			if w.Code != test.status || strings.Contains(w.Body.String(), "synthetic") {
				t.Fatalf("unsafe rejection: %d", w.Code)
			}
			_ = c.Get(context.Background(), key, source)
			if !reflect.DeepEqual(before, source) {
				t.Fatal("invalid update touched source")
			}
		})
	}
}

func TestOrdinaryAgentPatchIsStrictAndPreservesRegistry(t *testing.T) {
	failing := false
	h, _ := registryHandler(t, &failing)
	current := createdRegistryInstance(t, h)
	version := `"uid":"` + string(current.UID) + `","resourceVersion":"` + current.ResourceVersion + `"`
	for _, invalid := range []string{`null`, `{"registrySecretRef":null}`, `{"registryAuth":{}}`, `{"image":null}`, `{"image":""}`, `{"type":"custom"}`, `{"username":null}`, `{"username":"root"}`, `{"port":65536}`, `{"port":1,"port":2}`} {
		w := httptest.NewRecorder()
		h.UpdateInstanceByName(w, registryRequest(http.MethodPatch, `{`+version+`,"agent":`+invalid+`}`, current))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("accepted invalid patch: %d", w.Code)
		}
	}
	w := httptest.NewRecorder()
	h.UpdateInstanceByName(w, registryRequest(http.MethodPatch, `{"agent":{"port":9090}}`, current))
	if w.Code != http.StatusPreconditionRequired {
		t.Fatal("Registry If-Match replaced ordinary management body preconditions")
	}
	w = httptest.NewRecorder()
	h.UpdateInstanceByName(w, registryRequest(http.MethodPatch, `{`+version+`,"agent":{"image":"other.example.com/agent:2","port":null}}`, current))
	if w.Code != http.StatusOK {
		t.Fatalf("versioned patch: %d %s", w.Code, w.Body.String())
	}
	next, err := h.Kube.GetInstanceByName(context.Background(), current.Name)
	if err != nil || next.Spec.Agent.Port != nil || !reflect.DeepEqual(current.Spec.Agent.RegistrySecretRef, next.Spec.Agent.RegistrySecretRef) {
		t.Fatal("patch lost Registry reference or field presence")
	}
	for _, body := range []string{`{"agentType":"hermes"}`, `{"agent":{"registrySecretRef":{"name":"foreign"}}}`, `{"registryAuth":{"username":"u","password":"p"}}`} {
		w := httptest.NewRecorder()
		h.CreateInstance(w, registryRequest(http.MethodPost, body, nil))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("invalid creation accepted: %d", w.Code)
		}
	}
}

func TestCustomRegistryCreateRecoveryAndVersionedManagement(t *testing.T) {
	for _, port := range []string{"", `,"port":8080`} {
		t.Run("port"+port, func(t *testing.T) {
			failing := true
			h, c := registryHandler(t, &failing)
			h.Config.AllowedAgentTypes = []string{"hermes", "custom"}
			body := `{"agent":{"type":"custom","image":"` + managementImage + `","username":"assistant"` + port + `},"team":"aks","registryAuth":{"username":"synthetic-user","password":"synthetic-token"}}`
			request := func(method, body string, instance *api.Agent) *http.Request {
				return withAgentIdentityClaims(registryRequest(method, body, instance), "alice", managementTenant, managementOwner)
			}
			w := httptest.NewRecorder()
			h.CreateInstance(w, request(http.MethodPost, body, nil))
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("partial Custom create: %d %s", w.Code, w.Body.String())
			}
			first, err := h.Kube.GetInstanceByName(context.Background(), "alice")
			if err != nil {
				t.Fatal(err)
			}
			if first.Spec.GatewayTokenSecretRef != "" || first.Spec.AgentIdentity != nil || first.Spec.Tailscale != nil ||
				first.Annotations[api.AnnotationOwnerObjectID] != managementOwner || first.Annotations[kube.AnnotationCreateRequest] == "" {
				t.Fatal("Custom gained native settings or lost ownership/retry annotations")
			}
			failing = false
			h.Kube = kube.NewClient(c)
			w = httptest.NewRecorder()
			h.CreateInstance(w, request(http.MethodPost, body, nil))
			if w.Code != http.StatusOK {
				t.Fatalf("Custom retry failed behind quota: %d %s", w.Code, w.Body.String())
			}
			current, _ := h.Kube.GetInstanceByName(context.Background(), "alice")
			if current.UID != first.UID || !h.Kube.RegistryAuthConfigured(context.Background(), current) || (port == "") != (current.Spec.Agent.Port == nil) {
				t.Fatal("Custom retry changed identity/port or failed to save credentials")
			}
			secrets, namespaces := &corev1.SecretList{}, &corev1.NamespaceList{}
			if c.List(context.Background(), secrets) != nil || c.List(context.Background(), namespaces) != nil || len(secrets.Items) != 1 || len(namespaces.Items) != 0 || secrets.Items[0].Namespace != registryauth.DefaultNamespace {
				t.Fatal("Custom Registry creation created native or runtime resources")
			}
			version := `"uid":"` + string(current.UID) + `","resourceVersion":"` + current.ResourceVersion + `"`
			w = httptest.NewRecorder()
			h.UpdateInstanceByName(w, request(http.MethodPatch, `{`+version+`,"agent":{"image":"`+replacementImage+`"}}`, current))
			if w.Code != http.StatusOK {
				t.Fatalf("Custom Registry image update: %d %s", w.Code, w.Body.String())
			}
			imageUpdated, err := h.Kube.GetInstanceByName(context.Background(), "alice")
			if err != nil || imageUpdated.UID != current.UID || *imageUpdated.Spec.Agent.Image != replacementImage ||
				!reflect.DeepEqual(imageUpdated.Spec.Agent.RegistrySecretRef, current.Spec.Agent.RegistrySecretRef) {
				t.Fatalf("image update changed the Registry binding or instance identity: %v", err)
			}
			source := &corev1.Secret{}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(&secrets.Items[0]), source); err != nil ||
				!reflect.DeepEqual(source, &secrets.Items[0]) {
				t.Fatalf("image update changed Registry credentials: %v", err)
			}
			version = `"uid":"` + string(imageUpdated.UID) + `","resourceVersion":"` + imageUpdated.ResourceVersion + `"`
			w = httptest.NewRecorder()
			h.UpdateInstanceByName(w, request(http.MethodPatch, `{`+version+`,"agent":{"port":null},"displayName":"Renamed"}`, imageUpdated))
			if w.Code != http.StatusOK {
				t.Fatalf("Custom Registry edit: %d %s", w.Code, w.Body.String())
			}
			updated, _ := h.Kube.GetInstanceByName(context.Background(), "alice")
			if updated.Spec.Agent.Port != nil || updated.Spec.User.DisplayName != "Renamed" || !reflect.DeepEqual(updated.Spec.Agent.RegistrySecretRef, current.Spec.Agent.RegistrySecretRef) {
				t.Fatal("Custom edit changed Registry binding or lost the requested fields")
			}
			w = httptest.NewRecorder()
			h.DeleteRegistryAuth(w, request(http.MethodDelete, "", current))
			if w.Code != http.StatusConflict {
				t.Fatal("stale Registry write survived a management edit")
			}
		})
	}
}

func TestRegistryOwnerUsesAuthoritativeEntraIdentity(t *testing.T) {
	instance := customManagementInstance()
	instance.Annotations[kube.AnnotationCreateCaller] = "alice@example.com"
	for _, test := range []struct {
		claims  auth.Claims
		allowed bool
	}{
		{auth.Claims{Alias: "alice", Email: "alice@example.com"}, false},
		{auth.Claims{Alias: "alice", Email: "alice@example.com", TenantID: managementTenant, ObjectID: "another-owner"}, false},
		{auth.Claims{Alias: "renamed-alias", TenantID: managementTenant, ObjectID: managementOwner}, true},
	} {
		if registryOwner(instance, test.claims) != test.allowed {
			t.Fatal("Registry ownership disagrees with the verified Entra owner")
		}
	}
}

func TestRegistryForeignSourceAndDeletingAgent(t *testing.T) {
	failing := false
	h, c := registryHandler(t, &failing)
	agent := createdRegistryInstance(t, h)
	source := &corev1.Secret{}
	key := client.ObjectKey{Namespace: registryauth.DefaultNamespace, Name: agent.Spec.Agent.RegistrySecretRef.Name}
	_ = c.Get(context.Background(), key, source)
	source.OwnerReferences[0].UID = "old-instance-uid"
	_ = c.Update(context.Background(), source)
	if h.Kube.RegistryAuthConfigured(context.Background(), agent) {
		t.Fatal("old instance source reported as configured")
	}
	w := httptest.NewRecorder()
	h.PutRegistryAuth(w, registryRequest(http.MethodPut, `{"username":"example","password":"synthetic"}`, agent))
	if w.Code != http.StatusConflict {
		t.Fatalf("foreign source adopted: %d", w.Code)
	}
	agent, _ = h.Kube.GetInstanceByName(context.Background(), "alice")
	_ = c.Delete(context.Background(), agent)
	agent, _ = h.Kube.GetInstanceByName(context.Background(), "alice")
	if agent.DeletionTimestamp == nil {
		t.Fatal("test did not enter deletion")
	}
	w = httptest.NewRecorder()
	h.DeleteRegistryAuth(w, registryRequest(http.MethodDelete, "", agent))
	if w.Code != http.StatusConflict {
		t.Fatal("deleting instance accepted mutation")
	}
}

func TestRegistryRetryKeepsRetainedChartWhenDefaultRoutingChanges(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		types  []api.AgentType
		status int
		code   string
	}{
		{"unknown", nil, http.StatusServiceUnavailable, "agent_types_unknown"},
		{"unsupported", []api.AgentType{api.AgentTypeOpenClaw}, http.StatusConflict, "agent_chart_unsupported"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx := context.Background()
			failing := true
			h, storage := registryHandler(t, &failing)
			h.Config.AIGatewayEnabled = true
			h.Config.AIGatewayCatalogNamespace = "cetirun-system"
			catalog := catalogConfigMap("gpt-6-mini")
			replaceTestCatalog(t, storage, catalog)
			original := createdPartialRegistryForPolicy(t, h)
			// The Operator pins the chart accepted at creation, independently of
			// a later default-chart change or transient catalog-routing read.
			original.Spec.Helm = api.HelmSpec{Version: "1.0.0"}
			if err := storage.Update(ctx, original); err != nil {
				t.Fatal(err)
			}
			changed := routingCatalog(&api.AIGatewayRouting{Chart: "oci://registry.example.com/helm/cetirun:2.0.0", AgentTypes: scenario.types}, true, "gpt-6-mini")
			catalog.Data = changed.Data
			if err := storage.Update(ctx, catalog); err != nil {
				t.Fatal(err)
			}
			failing = false
			h.Kube = kube.NewClient(storage) // No process-local recovery state.
			for range 2 {                    // Recover credentials, then recover the completed response.
				w := httptest.NewRecorder()
				h.CreateInstance(w, registryRequest(http.MethodPost, registryCreateBody, nil))
				if w.Code != http.StatusOK {
					t.Fatalf("retained retry: %d %s", w.Code, w.Body.String())
				}
			}
			current, err := h.Kube.GetInstanceByName(ctx, original.Name)
			if err != nil {
				t.Fatal(err)
			}
			if current.UID != original.UID || current.Spec.Helm.Version != "1.0.0" || !h.Kube.RegistryAuthConfigured(ctx, current) {
				t.Fatal("retry did not preserve and complete the retained Agent")
			}
			// A different creation still requires the current default chart.
			body := strings.Replace(registryCreateBody, `"team":"aks"`, `"team":"aks","instanceName":"second"`, 1)
			r := registryRequest(http.MethodPost, body, nil)
			r.Header.Set("Idempotency-Key", "synthetic-create-request-0002")
			w := httptest.NewRecorder()
			h.CreateInstance(w, r)
			if w.Code != scenario.status || errorCode(t, w) != scenario.code {
				t.Fatalf("new create: %d %s", w.Code, w.Body.String())
			}
			if _, err := h.Kube.GetInstanceByName(ctx, "alice-second"); !kube.IsNotFound(err) {
				t.Fatal("new Agent bypassed the default-chart check")
			}
		})
	}
}

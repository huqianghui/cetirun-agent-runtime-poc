// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/config"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/kube"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func TestCustomNativeUpdatesHaveNoSideEffects(t *testing.T) {
	for _, byName := range []bool{false, true} {
		for _, body := range []string{`{"gatewayToken":"sk-dummy"}`, `{"tailscaleAuthKey":"sk-dummy"}`} {
			body = strings.Replace(body, "{", `{"uid":"agent-alice","resourceVersion":"1",`, 1)
			instance := sampleInstance()
			instance.Spec.Agent.Type = api.AgentTypeCustom
			fc := newFakeClient(instance)
			h := &InstanceHandler{Kube: kube.NewClient(fc), Config: config.Config{TailscaleEnabled: true}}
			request := withClaims(httptest.NewRequest(http.MethodPatch, "/api/v1/instance", bytes.NewBufferString(body)), "alice", "Example", "alice@example.com")
			request.SetPathValue("name", instance.Name)
			response := httptest.NewRecorder()
			if byName {
				h.UpdateInstanceByName(response, request)
			} else {
				h.UpdateInstance(response, request)
			}
			var payload ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if response.Code != http.StatusBadRequest || payload.Error.Code != "unsupported_custom_feature" ||
				strings.Contains(payload.Error.Message, "Teams") {
				t.Fatalf("byName=%v body=%s status=%d", byName, body, response.Code)
			}
			namespaces, secrets := &corev1.NamespaceList{}, &corev1.SecretList{}
			if err := fc.List(context.Background(), namespaces); err != nil {
				t.Fatal(err)
			}
			if err := fc.List(context.Background(), secrets); err != nil {
				t.Fatal(err)
			}
			if len(namespaces.Items) != 0 || len(secrets.Items) != 0 {
				t.Fatal("rejected Custom update created native resources")
			}
		}
	}
}

func TestRetiredTeamsEnabledOwnerPatchesDoNotMutate(t *testing.T) {
	for _, byName := range []bool{false, true} {
		for _, value := range []string{"true", "false", "null"} {
			t.Run(fmt.Sprintf("byName=%t/%s", byName, value), func(t *testing.T) {
				instance := sampleInstance()
				fc := newFakeClient(instance)
				h := &InstanceHandler{Kube: kube.NewClient(fc)}
				before, err := h.Kube.GetInstance(context.Background(), instance.Name)
				if err != nil {
					t.Fatal(err)
				}
				request := withClaims(httptest.NewRequest(http.MethodPatch, "/api/v1/instance",
					bytes.NewBufferString(`{"uid":"agent-alice","resourceVersion":"1","teamsEnabled":`+value+`}`)), "alice", "Example", "alice@example.com")
				request.SetPathValue("name", instance.Name)
				response := httptest.NewRecorder()
				if byName {
					h.UpdateInstanceByName(response, request)
				} else {
					h.UpdateInstance(response, request)
				}
				if response.Code != http.StatusBadRequest || errorCode(t, response) != "invalid_body" {
					t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
				}
				after, err := h.Kube.GetInstance(context.Background(), instance.Name)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(before, after) {
					t.Fatal("rejected retired field mutated the Agent")
				}
			})
		}
	}
}

func TestCustomWebUIResponseDoesNotAdvertiseDesktopOrCredentials(t *testing.T) {
	instance := sampleInstance()
	instance.Spec.Agent = api.HarnessSpec{Type: api.AgentTypeCustom, Port: ptr.To[int32](8080), WebUI: true}
	instance.Status.AccessURL = "https://platform.example.com/ui/alice/"
	instance.Status.Conditions = []metav1.Condition{{Type: api.ConditionIngressReady, Status: metav1.ConditionTrue, Reason: "PlatformWebUIConfigured"}}
	operator := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "cetirun-system", Name: "cetirun-operator"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "manager", Args: []string{"--chart-version=1.0.0", "--domain-suffix=platform.example.com"}},
		}}}}}
	h := &InstanceHandler{Kube: kube.NewClient(newFakeClient(instance, operator))}
	response, err := h.toInstanceResponse(context.Background(), instance)
	if err != nil {
		t.Fatal(err)
	}
	if response.WebUIURL != instance.Status.AccessURL || response.AccessURL != "" || response.GatewayToken != "" {
		t.Fatalf("wrong Custom Web UI response: %#v", response)
	}
	instance.Status.Phase = "Suspended"
	response, err = h.toInstanceResponse(context.Background(), instance)
	if err != nil || response.WebUIURL != "" {
		t.Fatal("suspended instance exposes a Web UI link")
	}
}

func TestCustomOrdinaryUpdateRemainsAvailable(t *testing.T) {
	instance := sampleInstance()
	instance.Spec.Agent.Type = api.AgentTypeCustom
	fc := newFakeClient(instance)
	h := &InstanceHandler{Kube: kube.NewClient(fc), Config: config.Config{}}
	request := withClaims(httptest.NewRequest(http.MethodPatch, "/api/v1/instance", bytes.NewBufferString(`{"uid":"agent-alice","resourceVersion":"1","displayName":"Updated"}`)), "alice", "Example", "alice@example.com")
	response := httptest.NewRecorder()
	h.UpdateInstance(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("ordinary Custom update failed: %d %s", response.Code, response.Body.String())
	}
}

func TestCustomWebUICreateThroughNestedAgent(t *testing.T) {
	h := customManagementHandler()
	w := httptest.NewRecorder()
	h.CreateInstance(w, managementRequest(http.MethodPost,
		`{"agent":{"type":"custom","image":"`+managementImage+`","port":8080,"webUI":true},"team":"aks"}`))
	if w.Code != http.StatusCreated {
		t.Fatalf("create Web UI: %d %s", w.Code, w.Body.String())
	}
	var response InstanceResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Agent.WebUI || !ptr.Equal(response.Agent.Port, ptr.To[int32](8080)) || response.WebUIURL != "" || response.AccessURL != "" {
		t.Fatalf("wrong Web UI creation response: %s", w.Body.String())
	}
	stored, err := h.Kube.GetInstance(context.Background(), "alice")
	if err != nil || !stored.Spec.Agent.WebUI || !ptr.Equal(stored.Spec.Agent.Port, response.Agent.Port) {
		t.Fatalf("Web UI was not persisted: %v", err)
	}
}

func TestCustomWebUIManagementPatch(t *testing.T) {
	for _, tc := range []struct {
		fields string
		ui     bool
		port   *int32
		code   int
	}{
		{`"displayName":"Updated"`, true, ptr.To[int32](8080), http.StatusOK},
		{`"agent":{"image":"` + managementImage + `"}`, true, ptr.To[int32](8080), http.StatusOK},
		{`"agent":{"port":9090}`, true, ptr.To[int32](9090), http.StatusOK},
		{`"agent":{"webUI":false}`, false, ptr.To[int32](8080), http.StatusOK},
		{`"agent":{"webUI":null}`, false, ptr.To[int32](8080), http.StatusOK},
		{`"agent":{"webUI":false,"port":null}`, false, nil, http.StatusOK},
		{`"agent":{"port":null}`, true, ptr.To[int32](8080), http.StatusBadRequest},
		{`"agent":{"webUI":false,"webUI":true}`, true, ptr.To[int32](8080), http.StatusBadRequest},
		{`"agent":{"webUI":{"port":9090}}`, true, ptr.To[int32](8080), http.StatusBadRequest},
	} {
		t.Run(tc.fields, func(t *testing.T) {
			instance := customManagementInstance()
			instance.Spec.Agent.WebUI = true
			h := customManagementHandler(instance)
			w := httptest.NewRecorder()
			body := fmt.Sprintf(`{"uid":%q,"resourceVersion":%q,%s}`, instance.UID, instance.ResourceVersion, tc.fields)
			h.UpdateInstanceByName(w, managementRequest(http.MethodPatch, body))
			if w.Code != tc.code {
				t.Fatalf("update Web UI: %d %s", w.Code, w.Body.String())
			}
			stored, err := h.Kube.GetInstance(context.Background(), "alice")
			if err != nil || stored.Spec.Agent.WebUI != tc.ui || !ptr.Equal(stored.Spec.Agent.Port, tc.port) {
				t.Fatalf("wrong Web UI after patch: %v", err)
			}
		})
	}
}

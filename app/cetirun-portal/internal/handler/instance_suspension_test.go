// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/internal/chataccess"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func managementSuspensionWorkload(agent *api.Agent) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: chataccess.ServiceName(agent.Name), Namespace: "cetirun-" + agent.Name,
			UID: "workload-uid", OwnerReferences: []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(),
				Kind: "Agent", Name: agent.Name, UID: agent.UID, Controller: ptr.To(true)}}},
		Spec: appsv1.StatefulSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "cetirun", VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/home/vibe"}}}},
			Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "home"},
			}}},
		}}},
	}
}

func TestOwnerSuspensionDoesNotOfferEphemeralHome(t *testing.T) {
	agent := customManagementInstance()
	agent.UID, agent.Spec.Agent.Type = "agent-uid", api.AgentTypeOpenClaw
	workload := managementSuspensionWorkload(agent)
	workload.Spec.Template.Spec.Volumes[0].VolumeSource = corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}
	h := customManagementHandler(agent, workload)
	response, err := h.toInstanceResponse(context.Background(), agent)
	if err != nil {
		t.Fatal(err)
	}
	if response.SuspendAvailable || response.SuspensionUnavailableReason == "" {
		t.Fatal("the owner was offered retained suspension for ephemeral HOME")
	}
	current, err := h.Kube.GetInstanceByName(context.Background(), agent.Name)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.SuspendInstanceByName(w, managementRequest(http.MethodPost,
		fmt.Sprintf(`{"uid":%q,"resourceVersion":%q}`, current.UID, current.ResourceVersion)))
	if w.Code != http.StatusConflict {
		t.Fatalf("ephemeral HOME suspension status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestOwnerSuspendResumeUsesExistingIntent(t *testing.T) {
	for _, kind := range []api.AgentType{api.AgentTypeOpenClaw, api.AgentTypeHermes, api.AgentTypeCustom} {
		t.Run(string(kind), func(t *testing.T) {
			agent := customManagementInstance()
			agent.UID, agent.Generation, agent.Spec.Agent.Type = "agent-uid", 2, kind
			agent.Status = api.AgentStatus{Phase: api.PhaseReady, ObservedGeneration: 2,
				Conditions: []metav1.Condition{{Type: api.ConditionReady, Status: metav1.ConditionTrue}}}
			h := customManagementHandler(agent, managementSuspensionWorkload(agent))
			for _, suspended := range []bool{true, false} {
				current, err := h.Kube.GetInstanceByName(context.Background(), agent.Name)
				if err != nil {
					t.Fatal(err)
				}
				body := fmt.Sprintf(`{"uid":%q,"resourceVersion":%q}`, current.UID, current.ResourceVersion)
				for attempt := 0; attempt < 2; attempt++ {
					w := httptest.NewRecorder()
					r := managementRequest(http.MethodPost, body)
					if suspended {
						h.SuspendInstanceByName(w, r)
					} else {
						h.ResumeInstanceByName(w, r)
					}
					if w.Code != http.StatusAccepted {
						t.Fatalf("suspended=%t attempt=%d: %d %s", suspended, attempt, w.Code, w.Body.String())
					}
					var response InstanceResponse
					if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
						t.Fatal(err)
					}
					if response.Suspended != suspended || response.SuspendAvailable == suspended ||
						response.ResumeAvailable != suspended || response.UID != "agent-uid" ||
						response.Phase != api.PhaseReady || suspended && response.RestartAvailable {
						t.Fatalf("intent and observed status were conflated: %+v", response)
					}
					h = &InstanceHandler{Kube: h.Kube, Config: h.Config}
				}
			}
		})
	}
}

func TestOwnerSuspendResumeAuthorizationAndGuards(t *testing.T) {
	for _, suspended := range []bool{true, false} {
		for _, tc := range []struct {
			name   string
			change func(*http.Request, *api.Agent)
			status int
			code   string
		}{
			{"foreign owner", func(_ *http.Request, a *api.Agent) { a.Annotations[api.AnnotationOwnerObjectID] = managementTenant }, 403, "forbidden"},
			{"foreign tenant", func(_ *http.Request, a *api.Agent) { a.Annotations[api.AnnotationOwnerTenantID] = managementOwner }, 403, "forbidden"},
			{"cross-site", func(r *http.Request, _ *api.Agent) { r.Header.Set("Sec-Fetch-Site", "cross-site") }, 403, "invalid_origin"},
			{"wrong origin", func(r *http.Request, _ *api.Agent) { r.Header.Set("Origin", "https://other.example.com") }, 403, "invalid_origin"},
			{"stale UID", func(_ *http.Request, a *api.Agent) { a.UID = "replacement-uid" }, 409, "instance_changed"},
			{"stale version", func(_ *http.Request, a *api.Agent) { a.ResourceVersion = "8" }, 409, "instance_changed"},
			{"single-start", func(_ *http.Request, a *api.Agent) { a.Finalizers = []string{api.SandboxPreparationFinalizer} }, 409, "suspension_unavailable"},
			{"deleting", func(_ *http.Request, a *api.Agent) {
				now := metav1.Now()
				a.Finalizers, a.DeletionTimestamp = []string{api.Finalizer}, &now
			}, 409, "instance_deleting"},
		} {
			t.Run(fmt.Sprintf("%s/suspended=%t", tc.name, suspended), func(t *testing.T) {
				agent := customManagementInstance()
				agent.UID, agent.ResourceVersion, agent.Spec.Suspended = "agent-uid", "7", !suspended
				r := managementRequest(http.MethodPost, `{"uid":"agent-uid","resourceVersion":"7"}`)
				tc.change(r, agent)
				h := customManagementHandler(agent)
				h.Config.PortalOrigin = "https://portal.example.com"
				w := httptest.NewRecorder()
				h.setInstanceSuspended(w, r, agent.Name, suspended)
				var response struct {
					Error struct{ Code string }
				}
				if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				if w.Code != tc.status || response.Error.Code != tc.code {
					t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
				}
				current, err := h.Kube.GetInstanceByName(context.Background(), agent.Name)
				if err != nil || current.Spec.Suspended != agent.Spec.Suspended {
					t.Fatalf("rejected request changed intent: %v", err)
				}
			})
		}
	}
}

func TestOwnerSuspensionRequiresAuthenticationAndExactVersionBody(t *testing.T) {
	h := customManagementHandler()
	for _, call := range []func(http.ResponseWriter, *http.Request){
		h.SuspendInstance, h.ResumeInstance, h.SuspendInstanceByName, h.ResumeInstanceByName,
	} {
		w := httptest.NewRecorder()
		call(w, httptest.NewRequest(http.MethodPost, "/api/v1/instance/suspend", nil))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("missing authentication: %d", w.Code)
		}
		for _, body := range []string{"", `{}`, `{"uid":null,"resourceVersion":"7"}`,
			`{"uid":"agent-uid","resourceVersion":"7","suspended":true}`,
			`{"uid":"agent-uid","resourceVersion":"7","generation":2}`,
			`{"uid":"agent-uid","uid":"other","resourceVersion":"7"}`} {
			w := httptest.NewRecorder()
			call(w, managementRequest(http.MethodPost, body))
			if w.Code != http.StatusBadRequest && w.Code != http.StatusPreconditionRequired {
				t.Fatalf("invalid version accepted: %d body=%s", w.Code, body)
			}
		}
	}
}

func TestOwnerSuspensionGenerationHeader(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values []string
		status int
		code   string
	}{
		{"missing", nil, http.StatusNotFound, "instance_not_found"},
		{"valid", []string{"2"}, http.StatusNotFound, "instance_not_found"},
		{"zero", []string{"0"}, http.StatusBadRequest, "invalid_generation"},
		{"negative", []string{"-1"}, http.StatusBadRequest, "invalid_generation"},
		{"not a number", []string{"two"}, http.StatusBadRequest, "invalid_generation"},
		{"multiple", []string{"2", "3"}, http.StatusBadRequest, "invalid_generation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := customManagementHandler()
			r := managementRequest(http.MethodPost, `{"uid":"agent-uid","resourceVersion":"7"}`)
			for _, value := range tc.values {
				r.Header.Add(agentGenerationHeader, value)
			}
			w := httptest.NewRecorder()
			h.SuspendInstanceByName(w, r)
			var response struct {
				Error struct{ Code string }
			}
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if w.Code != tc.status || response.Error.Code != tc.code {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

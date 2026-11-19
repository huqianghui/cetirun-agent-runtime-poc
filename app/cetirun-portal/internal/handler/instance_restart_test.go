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
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/kube"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestOwnerRestartUsesVersionedIdempotentManagementAPI(t *testing.T) {
	for _, kind := range []api.AgentType{api.AgentTypeOpenClaw, api.AgentTypeHermes, api.AgentTypeCustom} {
		t.Run(string(kind), func(t *testing.T) {
			agent := customManagementInstance()
			agent.UID, agent.Generation, agent.Spec.Agent.Type = "agent-uid", 2, kind
			agent.Status = api.AgentStatus{Phase: api.PhaseReady, ObservedGeneration: 2,
				Conditions: []metav1.Condition{{Type: api.ConditionReady, Status: metav1.ConditionTrue}}}
			h := customManagementHandler(agent)
			current, err := h.Kube.GetInstanceByName(context.Background(), agent.Name)
			if err != nil {
				t.Fatal(err)
			}
			body := fmt.Sprintf(`{"uid":%q,"resourceVersion":%q}`, current.UID, current.ResourceVersion)
			for attempt := 0; attempt < 2; attempt++ {
				r := managementRequest(http.MethodPost, body)
				r.Header.Set("Idempotency-Key", "request-00000001")
				w := httptest.NewRecorder()
				h.RestartInstanceByName(w, r)
				if w.Code != http.StatusAccepted {
					t.Fatalf("attempt %d: %d %s", attempt, w.Code, w.Body.String())
				}
				var response InstanceResponse
				if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				if response.RestartRequestID != "request-00000001" || response.ObservedRestartRequestID != "" ||
					response.RestartAvailable || response.UID != "agent-uid" {
					t.Fatalf("accepted request falsely claimed completion: %+v", response)
				}
				// A new handler models recovery on another Portal replica.
				h = &InstanceHandler{Kube: h.Kube, Config: h.Config}
			}
		})
	}
}

func TestOwnerRestartRejectsForeignOrInvalidManagementRequests(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*http.Request, *api.Agent)
		status int
	}{
		{"foreign owner", func(_ *http.Request, a *api.Agent) { a.Annotations[api.AnnotationOwnerObjectID] = managementTenant }, http.StatusForbidden},
		{"cross-site", func(r *http.Request, _ *api.Agent) { r.Header.Set("Sec-Fetch-Site", "cross-site") }, http.StatusForbidden},
		{"missing idempotency key", func(r *http.Request, _ *api.Agent) { r.Header.Del("Idempotency-Key") }, http.StatusBadRequest},
		{"suspended", func(_ *http.Request, a *api.Agent) { a.Spec.Suspended = true }, http.StatusConflict},
		{"migration pending", func(_ *http.Request, a *api.Agent) {
			a.Spec.PlannedMigration = &api.PlannedMigrationRequest{RequestID: "migration-00000001",
				Target: api.MigrationObject{Name: "node-b", UID: "node-b-uid"}}
		}, http.StatusConflict},
		{"single-start", func(_ *http.Request, a *api.Agent) { a.Finalizers = []string{api.SandboxPreparationFinalizer} }, http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			agent := customManagementInstance()
			agent.UID, agent.ResourceVersion, agent.Generation = "agent-uid", "7", 2
			agent.Status = api.AgentStatus{Phase: api.PhaseReady, ObservedGeneration: 2,
				Conditions: []metav1.Condition{{Type: api.ConditionReady, Status: metav1.ConditionTrue}}}
			r := managementRequest(http.MethodPost, `{"uid":"agent-uid","resourceVersion":"7"}`)
			r.Header.Set("Idempotency-Key", "request-00000001")
			test.change(r, agent)
			h := customManagementHandler(agent)
			w := httptest.NewRecorder()
			h.RestartInstanceByName(w, r)
			if w.Code != test.status {
				t.Fatalf("status=%d want=%d: %s", w.Code, test.status, w.Body.String())
			}
			current, err := h.Kube.GetInstanceByName(context.Background(), agent.Name)
			if err != nil || current.Spec.RestartRequestID != "" {
				t.Fatalf("rejected request persisted: %v", err)
			}
		})
	}
}

func TestRestartRequiresAnExactVersionBody(t *testing.T) {
	for _, body := range []string{"", `{}`, `{"uid":"agent-uid"}`, `{"uid":"agent-uid","resourceVersion":"7","suspended":false}`} {
		h := &InstanceHandler{Kube: kube.NewClient(newFakeClient())}
		r := managementRequest(http.MethodPost, body)
		r.Header.Set("Idempotency-Key", "request-00000001")
		w := httptest.NewRecorder()
		h.RestartInstanceByName(w, r)
		if w.Code != http.StatusBadRequest && w.Code != http.StatusPreconditionRequired {
			t.Fatalf("invalid version body accepted: status=%d body=%s", w.Code, body)
		}
	}
}

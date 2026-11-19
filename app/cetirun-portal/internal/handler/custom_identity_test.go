// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
)

func TestCreateGovernedCustomAgentIdentity(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "unqualified", true: "explicitly-enabled"}[enabled], func(t *testing.T) {
			team, policy := creationPolicy()
			policy.Spec.Harnesses.Allowed = []api.AgentType{api.AgentTypeCustom}
			policy.Spec.AgentIdentity = api.AgentPolicyAgentIdentity{Allowed: true, CredentialServices: []string{"azure-resource-manager"}}
			h, kube := creationPolicyHandler(team, policy)
			h.Config.AgentToolsEnabled = true
			if enabled {
				h.Config.AgentToolsRuntimeTypes = []string{"custom"}
			}
			if options := readCreateOptions(t, h, team.Name); options.AgentIdentity.CustomAllowed != enabled {
				t.Fatal("Custom identity options did not reflect the deployment opt-in")
			}
			response := httptest.NewRecorder()
			h.CreateInstance(response, managementRequest(http.MethodPost,
				`{"team":"policy-team","agent":{"type":"custom","image":"registry.example.com/custom:1"},"agentIdentityEnabled":true}`))
			if !enabled {
				if response.Code != http.StatusBadRequest {
					t.Fatalf("unqualified Custom identity was not rejected: %d %s", response.Code, response.Body)
				}
				assertNoCreationWrites(t, kube)
				return
			}
			if response.Code != http.StatusCreated {
				t.Fatalf("qualified Custom creation failed: %d %s", response.Code, response.Body)
			}
			var agents api.AgentList
			if err := kube.List(context.Background(), &agents); err != nil || len(agents.Items) != 1 {
				t.Fatal("Custom Agent was not persisted:", err)
			}
			agent := agents.Items[0]
			if agent.Spec.AgentIdentity == nil || !agent.Spec.AgentIdentity.Enabled || agent.Spec.SandboxRef == "" ||
				agent.Annotations[api.AnnotationOwnerObjectID] == "" || agent.Spec.GatewayTokenSecretRef != "" {
				t.Fatal("Custom identity lost its governed owner or Sandbox binding")
			}
			handler := ToolConnectionsHandler{RuntimeTypes: []string{"custom"}}
			if !handler.runtimeSupported(&agent) {
				t.Fatal("qualified Custom cannot manage its Connections")
			}
			agent.Spec.SandboxRef = ""
			if handler.runtimeSupported(&agent) {
				t.Fatal("legacy Custom inherited governed Connections support")
			}
		})
	}
}

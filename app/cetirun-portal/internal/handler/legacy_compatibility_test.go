// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package handler

import (
	"context"
	"net/http"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/config"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestLegacyCreationDoesNotRequireSandboxConfiguration(t *testing.T) {
	team := &api.Team{ObjectMeta: metav1.ObjectMeta{Name: "example-team"}}
	for _, body := range []string{
		`{"agent":{"type":"openclaw"}}`,
		`{"agent":{"type":"openclaw"},"team":"example-team"}`,
	} {
		response, _ := createAgent(t, config.Config{}, body, team)
		if response.Code != http.StatusConflict || errorCode(t, response) != "gateway_required" {
			t.Fatalf("direct creation was accepted: status=%d body=%s", response.Code, response.Body.String())
		}
	}
}

func TestLegacyGatewayTeamDoesNotRequireSandbox(t *testing.T) {
	team := &api.Team{ObjectMeta: metav1.ObjectMeta{Name: "example-team"}}
	response, kube := createAgent(t, gatewayConfig(), `{"agent":{"type":"openclaw"},"team":"example-team"}`,
		team, catalogConfigMap("example-model"))
	if response.Code != http.StatusCreated {
		t.Fatalf("legacy Gateway Team was blocked: status=%d body=%s", response.Code, response.Body.String())
	}
	agent := &api.Agent{}
	if err := kube.Get(context.Background(), client.ObjectKey{Name: "alice"}, agent); err != nil {
		t.Fatal(err)
	}
	if agent.Spec.SandboxRef != "" || !agent.Spec.SharedModelsEnabled() || !agent.Spec.GitHubCopilotEnabled() {
		t.Fatal("legacy default model sources or Sandbox opt-out were lost")
	}
	team.Spec.AgentPolicyRef = "example-policy"
	response, _ = createAgent(t, gatewayConfig(), `{"agent":{"type":"openclaw"},"team":"example-team"}`,
		team, catalogConfigMap("example-model"))
	if response.Code != http.StatusConflict || errorCode(t, response) != "team_not_configured" {
		t.Fatal("partial Team policy was silently treated as legacy")
	}
}

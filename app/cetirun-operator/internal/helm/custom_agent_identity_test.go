// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"strings"
	"testing"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
)

func customIdentityInput() map[string]any {
	input := customInput(8080)
	b := input["customBindings"].(runtimebindings.Custom)
	b.AgentIdentity = &runtimebindings.AgentIdentity{BrokerURL: "http://cetirun-agent-broker.credentials.svc.cluster.local:8080"}
	input["customBindings"] = b
	return input
}

func TestCustomAgentIdentityIsRejected(t *testing.T) {
	_, err := PrepareChart(testChart(t), customIdentityInput())
	if err == nil || !strings.Contains(err.Error(), "Custom does not support Agent Identity") {
		t.Fatalf("Custom Agent Identity was accepted: %v", err)
	}
}

func TestCustomRejectsNativeIdentityAndToolOverrides(t *testing.T) {
	for name, value := range map[string]any{
		"agent identity":  map[string]any{"enabled": true},
		"connections MCP": map[string]any{"enabled": true},
	} {
		t.Run(name, func(t *testing.T) {
			input := customInput(8080)
			if name == "agent identity" {
				input["agentIdentity"] = value
			} else {
				input["connectionsMCP"] = value
			}
			if _, err := PrepareChart(testChart(t), input); err == nil {
				t.Fatalf("Custom accepted %s override", name)
			}
		})
	}
}

func TestCustomPlainWorkloadHasNoIdentityTools(t *testing.T) {
	plain, err := PrepareChart(testChart(t), customInput(8080))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain.manifest), "CETIRUN_AGENT_BROKER_URL") || strings.Contains(string(plain.manifest), "agent-identity-tools") {
		t.Fatal("identity tools appeared without an approved binding")
	}
	for _, value := range []string{"cetirun-agent", "azure-skill", "CETIRUN_AGENT_IDENTITY_ENABLED"} {
		if strings.Contains(string(plain.manifest), value) {
			t.Fatalf("plain Custom workload contains obsolete identity asset %q", value)
		}
	}
}

// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
)

func TestBuiltinValueBuilderRuntimeGate(t *testing.T) {
	tests := []struct {
		name  string
		agent api.HarnessSpec
		valid bool
	}{
		{"openclaw", api.HarnessSpec{Type: api.AgentTypeOpenClaw}, true},
		{"hermes", api.HarnessSpec{Type: api.AgentTypeHermes, Username: ptr.To("vibe")}, true},
		{"missing type", api.HarnessSpec{}, false},
		{"unknown type", api.HarnessSpec{Type: "unknown"}, false},
		{"custom", api.HarnessSpec{Type: api.AgentTypeCustom, Image: ptr.To("registry.example.com/agent:1")}, false},
		// Qualification is performed against the selected chart during Prepare.
		{"image override", api.HarnessSpec{Type: api.AgentTypeHermes, Image: ptr.To("registry.example.com/agent:1")}, true},
		{"port override", api.HarnessSpec{Type: api.AgentTypeOpenClaw, Port: ptr.To[int32](8080)}, true},
		{"registry reference", api.HarnessSpec{Type: api.AgentTypeOpenClaw, RegistrySecretRef: &api.RegistrySecretRef{Name: "source"}}, true},
		{"username override", api.HarnessSpec{Type: api.AgentTypeOpenClaw, Username: ptr.To("alice")}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instance := newTestInstance("alice", "Example User", "", "gateway-token")
			instance.Spec.Agent = tt.agent
			values, err := buildHelmValues(instance, nil, "example.com", "", nil, "")
			if (err == nil) != tt.valid {
				t.Fatalf("valid=%v, error=%v", tt.valid, err)
			}
			if !tt.valid && values != nil {
				t.Fatal("unsupported agent must not render built-in Helm values")
			}
		})
	}
}

func TestCustomRuntimeGateDefersRegistryToSynchronizer(t *testing.T) {
	agent := api.HarnessSpec{Type: api.AgentTypeCustom, Image: ptr.To("registry.example.com/agent:1"),
		Username: ptr.To("worker")}
	if err := validateAgentRuntime(agent); err != nil {
		t.Fatal(err)
	}
	agent.RegistrySecretRef = &api.RegistrySecretRef{Name: "source"}
	if err := validateAgentRuntime(agent); err != nil {
		t.Fatal(err)
	}
}

func TestAgentTypeCannotBeOverriddenByHelm(t *testing.T) {
	instance := newTestInstance("alice", "Example User", "", "gateway-token")
	instance.Spec.Agent.Type = api.AgentTypeHermes
	values, err := buildHelmValues(instance, nil, "example.com", "", map[string]interface{}{"agentType": "openclaw"}, "")
	if err != nil || values["agentType"] != "hermes" {
		t.Fatalf("deployment defaults must not override instance type: %v, %v", values["agentType"], err)
	}
	for _, raw := range []string{`"openclaw"`, `"custom"`, `null`, `{}`} {
		instance.Spec.Helm.Values["agentType"] = runtime.RawExtension{Raw: []byte(raw)}
		if _, err := buildHelmValues(instance, nil, "example.com", "", nil, ""); err == nil {
			t.Errorf("conflicting helm.values.agentType accepted: %s", raw)
		}
	}
}

func TestRuntimePlanCannotBeOverriddenByHelm(t *testing.T) {
	for key, raw := range map[string]string{
		"runtime":      `{"agent":{"image":"injected"}}`,
		"agentRequest": `{"type":"custom"}`,
		"image":        `{"repository":"other"}`,
		"hermes":       `{"image":{"tag":"other"}}`,
		"service":      `{"targetPort":8080}`,
	} {
		instance := newTestInstance("example", "Example", "", "gateway-token")
		instance.Spec.Agent.Type = api.AgentTypeHermes
		instance.Spec.Helm.Values[key] = runtime.RawExtension{Raw: []byte(raw)}
		if _, err := buildHelmValues(instance, nil, "example.com", "", nil, ""); err == nil {
			t.Errorf("protected %s override accepted", key)
		}
	}
}

func TestNativeRoutesUsePinnedPort(t *testing.T) {
	for _, typ := range []api.AgentType{api.AgentTypeHermes, api.AgentTypeOpenClaw} {
		instance := newTestInstance("example", "Example", "", "gateway-token")
		instance.Spec.Agent = api.HarnessSpec{Type: typ, Port: ptr.To[int32](8080)}
		vs := buildVirtualService(instance, "example.com", "")
		found := false
		for _, route := range vs.Spec.Http {
			for _, destination := range route.Route {
				if destination.Destination.Port.Number == 8080 {
					found = true
				}
			}
		}
		if !found {
			t.Errorf("%s route ignored spec.agent.port", typ)
		}
	}
	instance := newTestInstance("example", "Example", "", "gateway-token")
	instance.Spec.Agent.Type = "unknown"
	if buildVirtualService(instance, "example.com", "") != nil {
		t.Fatal("unknown type inherited a native route")
	}
	instance.Spec.Agent.Type = api.AgentTypeCustom
	route := buildVirtualService(instance, "example.com", "")
	if route == nil {
		t.Fatal("Custom lost platform desktop access")
	}
	for _, rule := range route.Spec.Http {
		for _, destination := range rule.Route {
			if destination.Destination.Port.Number != novncPort {
				t.Fatal("Custom inherited a native Agent or terminal route")
			}
		}
	}
}

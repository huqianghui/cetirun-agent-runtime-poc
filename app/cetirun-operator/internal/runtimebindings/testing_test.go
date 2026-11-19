// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimebindings

import (
	"testing"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/agents"
)

func TestRejectCustomOverrideInjection(t *testing.T) {
	for _, key := range []string{"runtime", "customBindings", "agentRequest", "command", "initContainers", "sidecars",
		"extraEnv", "extraEnvFrom", "podSecurityContext", "securityContext", "extraVolumes", "extraVolumeMounts",
		"secrets", "imagePullSecrets", "logger", "service", "ingress", "persistence"} {
		if err := ValidateOverrides(map[string]any{key: nil}); err == nil {
			t.Errorf("accepted %s", key)
		}
	}
	if err := ValidateOverrides(map[string]any{"skills": map[string]any{"command": "injected"}}); err == nil {
		t.Fatal("accepted an unsupported skills field")
	}
}

func TestBindingsRejectUnapprovedInputs(t *testing.T) {
	agent, _ := agents.ResolveCustom(agents.Request{Type: "custom", Image: "example.com/agent:1"})
	b := testBindings()
	if err := b.Validate(agent); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*Custom){
		"missing model":   func(b *Custom) { b.Model.UID = "" },
		"missing home":    func(b *Custom) { b.Home.Name = "" },
		"missing claim":   func(b *Custom) { b.Home.Claim = nil },
		"root group":      func(b *Custom) { b.Home.FSGroup = 0 },
		"wrong image":     func(b *Custom) { b.ApprovedImage = "example.com/other:1" },
		"wrong type":      func(b *Custom) { b.ApprovedType = "hermes" },
		"no policy":       func(b *Custom) { b.PolicyRevision = "" },
		"no canonical":    func(b *Custom) { delete(b.Context.Files, "canonical") },
		"no memory":       func(b *Custom) { delete(b.Context.Files, "memory") },
		"traversal":       func(b *Custom) { b.Context.Files["extra"] = "skills/../../token" },
		"shadow":          func(b *Custom) { b.Context.Files["extra"] = "skills/cetirun-memory" },
		"duplicate":       func(b *Custom) { b.Context.Files["extra"] = "AGENTS.md" },
		"wildcard caller": func(b *Custom) { b.Network.Callers = []Peer{{Namespace: "example"}} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			b := testBindings()
			mutate(&b)
			if err := b.Validate(agent); err == nil {
				t.Fatal("invalid receipt accepted")
			}
		})
	}
}

func TestHomePlanDoesNotRequirePVCIdentity(t *testing.T) {
	agent, _ := agents.ResolveCustom(agents.Request{Type: "custom", Image: "example.com/agent:1"})
	b := testBindings()
	if err := b.Validate(agent); err != nil {
		t.Fatalf("Chart-owned HOME required a pre-existing PVC: %v", err)
	}
	for _, claim := range []HomeClaim{
		{StorageClass: "", StorageSize: "100Gi", AccessMode: "ReadWriteOnce"},
		{StorageClass: "default", StorageSize: "0", AccessMode: "ReadWriteOnce"},
		{StorageClass: "default", StorageSize: "100Gi", AccessMode: "ReadOnlyMany"},
	} {
		b.Home.Claim = &claim
		if err := b.Validate(agent); err == nil {
			t.Fatal("invalid HOME claim template was accepted")
		}
	}
}

func testBindings() Custom {
	ref := func(name string) ObjectReference {
		return ObjectReference{Name: name, UID: name + "-uid", Revision: "1"}
	}
	return Custom{
		AgentName: "example", AgentUID: "agent-uid", Generation: 1, Namespace: ref("cetirun-example"), ApprovedType: "custom",
		ApprovedImage: "example.com/agent:1", PolicyRevision: "policy-1",
		Model: ModelAccess{ObjectReference: ref("model"), BaseURL: "http://gateway.example.com/v1", Key: "key"},
		Home: Home{Name: "home", FSGroup: 2000,
			Claim: &HomeClaim{StorageClass: "default", StorageSize: "100Gi", AccessMode: "ReadWriteOnce"}},
		Context: Context{ObjectReference: ref("context"), Files: map[string]string{
			"canonical": "AGENTS.md", "memory": "skills/cetirun-memory/SKILL.md"}},
		Network: Network{Revision: "network-1", Gateway: Peer{Namespace: "models", PodLabels: map[string]string{"app": "gateway"}}, GatewayPort: 8080,
			DNS: Peer{Namespace: "kube-system", PodLabels: map[string]string{"k8s-app": "kube-dns"}}},
	}
}

func TestSharedValuesDropNativeSettings(t *testing.T) {
	values := map[string]any{"persistence": map[string]any{"size": "8Gi"}, "nodeSelector": map[string]any{"pool": "agents"}}
	shared := SharedValues(values)
	if _, ok := shared["persistence"]; ok || shared["nodeSelector"] == nil || len(shared) != 1 {
		t.Fatalf("shared values = %v", shared)
	}
	if _, ok := values["persistence"]; !ok {
		t.Fatal("SharedValues mutated its input")
	}
	if err := ValidateOverrides(shared); err != nil {
		t.Fatal(err)
	}
}

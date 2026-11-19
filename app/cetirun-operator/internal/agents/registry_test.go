// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package agents_test

import (
	"testing"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/agents"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/agents/builtin"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/agents/hermes"
	"k8s.io/utils/ptr"
)

func TestRegistry(t *testing.T) {
	if _, err := agents.NewRegistry(hermes.Harness{}, hermes.Harness{}); err == nil {
		t.Fatal("duplicate registration accepted")
	}
	if _, err := agents.NewRegistry(nil); err == nil {
		t.Fatal("nil harness accepted")
	}
	r := builtin.NewRegistry()
	for _, name := range []string{"hermes", "openclaw", "custom"} {
		h, err := r.Lookup(name)
		if err != nil || h.Describe().Type != name {
			t.Fatalf("lookup %s: %v", name, err)
		}
	}
	for _, name := range []string{"", "unknown"} {
		if _, err := r.Lookup(name); err == nil {
			t.Fatalf("%s fell through to a built-in", name)
		}
	}
}

func TestResolve(t *testing.T) {
	profile := agents.Profile{ID: "release/hermes", Image: "registry.example.com/hermes:1", Port: 8642,
		QualifiedImages: []string{"registry.example.com/hermes@sha256:abcdef"}}
	resolved, err := agents.Resolve(agents.Request{Type: "hermes"}, profile)
	if err != nil || resolved.Image != profile.Image || resolved.Port == nil || *resolved.Port != 8642 || resolved.HomeDir != "/home/vibe" {
		t.Fatalf("defaults: %+v %v", resolved, err)
	}
	if _, err := agents.Resolve(agents.Request{Type: "hermes", Image: profile.QualifiedImages[0]}, profile); err != nil {
		t.Fatal(err)
	}
	for _, req := range []agents.Request{
		{Type: "hermes", Image: "registry.example.com/other:1"},
		{Type: "hermes", Username: "root"},
		{Type: "hermes", Port: ptr.To[int32](-1)},
		{Type: "hermes", Port: ptr.To[int32](0)},
		{Type: "hermes", Port: ptr.To[int32](65536)},
	} {
		if _, err := agents.Resolve(req, profile); err == nil {
			t.Errorf("invalid request accepted: %+v", req)
		}
	}
}

func TestRendererCannotAccessEnvironmentOrExecuteTemplates(t *testing.T) {
	for _, source := range []string{`{{ env "HOME" }}`, `{{ lookup "v1" "Secret" "" "" }}`, `{{ tpl "code" . }}`, `{{ randAlphaNum 10 }}`, `{{ getHostByName "example.com" }}`} {
		if _, err := agents.RenderConfig(agents.ResolvedAgent{Type: "hermes"}, agents.Bindings{ConfigTemplate: source}); err == nil {
			t.Errorf("unsupported function allowed: %s", source)
		}
	}
}

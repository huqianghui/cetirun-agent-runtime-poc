// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package agents

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"text/template"
)

func TestEmptySkillsBindingRequiresNativeConsistentInputs(t *testing.T) {
	bindings := Bindings{
		NativeSandbox: true, Model: ModelBinding{Gateway: true},
		Skills:         SkillsBinding{Empty: true, MountPath: "/home/vibe/.aks/skills"},
		ConfigTemplate: `{{ dig "skillsMode" "" .Values.sandboxWorkload }}`,
	}
	agent := ResolvedAgent{Type: "openclaw"}
	config, err := RenderConfig(agent, bindings)
	if err != nil || strings.TrimSpace(config) != "empty" {
		t.Fatal("typed empty skills were not projected into the native template:", err)
	}
	for name, mutate := range map[string]func(*Bindings){
		"non-native":     func(b *Bindings) { b.NativeSandbox = false },
		"no Gateway":     func(b *Bindings) { b.Model.Gateway = false },
		"shared storage": func(b *Bindings) { b.Skills.Shared = true },
		"packages":       func(b *Bindings) { b.Skills.NPM = []string{"example-package"} },
		"other path":     func(b *Bindings) { b.Skills.MountPath = "/other-skills" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := bindings
			mutate(&changed)
			if _, err := RenderConfig(agent, changed); err == nil {
				t.Fatal("inconsistent native empty-skills binding was accepted")
			}
		})
	}
	agent.Type = "hermes"
	if _, err := RenderConfig(agent, bindings); err == nil {
		t.Fatal("empty-skills binding was accepted outside native OpenClaw")
	}
	raw, err := json.Marshal(SkillsBinding{})
	if err != nil || strings.Contains(string(raw), `"Empty"`) {
		t.Fatal("legacy runtime binding serialized shape changed:", err)
	}
}

func TestNativeProxyBindingReachesTemplateWithoutChangingLegacyInputs(t *testing.T) {
	bindings := Bindings{
		NativeSandbox: true, Model: ModelBinding{Gateway: true},
		TrustedProxyIPs: []string{"192.0.2.10", "2001:db8::10"},
		ConfigTemplate:  `{{ dig "trustedProxyIPs" (list) (.Values.sandboxWorkload | default dict) | toPrettyJson }}`,
	}
	agent := ResolvedAgent{Type: "openclaw"}
	rendered, err := RenderConfig(agent, bindings)
	if err != nil {
		t.Fatal(err)
	}
	var proxies []string
	if err := json.Unmarshal([]byte(rendered), &proxies); err != nil || !slices.Equal(proxies, bindings.TrustedProxyIPs) {
		t.Fatal("typed native proxy binding did not reach the shared native template:", err)
	}
	for name, mutate := range map[string]func(*Bindings){
		"non-native": func(b *Bindings) { b.NativeSandbox = false },
		"no Gateway": func(b *Bindings) { b.Model.Gateway = false },
		"Tailscale":  func(b *Bindings) { b.Integrations.Tailscale = true },
	} {
		t.Run(name, func(t *testing.T) {
			changed := bindings
			mutate(&changed)
			if _, err := RenderConfig(agent, changed); err == nil {
				t.Fatal("native proxy trust reached a different execution mode")
			}
		})
	}
	if _, err := RenderConfig(ResolvedAgent{Type: "hermes"}, bindings); err == nil {
		t.Fatal("native proxy binding reached Hermes")
	}
	bindings.TrustedProxyIPs = nil
	rendered, err = RenderConfig(agent, bindings)
	if err != nil || strings.TrimSpace(rendered) != "[]" {
		t.Fatal("omitted proxy binding invented a fallback:", err)
	}
	raw, err := json.Marshal(Bindings{})
	if err != nil || strings.Contains(string(raw), `"TrustedProxyIPs"`) {
		t.Fatal("default binding serialized shape changed:", err)
	}
}

func TestIngressMTLSBindingRequiresCommonLoopbackProxy(t *testing.T) {
	bindings := Bindings{
		NativeSandbox: true, ReconciledSandbox: true, IngressMTLS: true,
		Model:           ModelBinding{Gateway: true},
		TrustedProxyIPs: []string{"127.0.0.1"},
		ConfigTemplate:  `{{ dig "ingressMTLS" false (.Values.sandboxWorkload | default dict) }}`,
	}
	for _, harness := range []string{"openclaw", "hermes"} {
		t.Run(harness, func(t *testing.T) {
			agent := ResolvedAgent{Type: harness}
			rendered, err := RenderConfig(agent, bindings)
			if err != nil || strings.TrimSpace(rendered) != "true" {
				t.Fatal("managed mTLS binding did not reach the native template:", err)
			}
			for name, mutate := range map[string]func(*Bindings){
				"legacy":       func(b *Bindings) { b.NativeSandbox = false },
				"single-start": func(b *Bindings) { b.ReconciledSandbox = false },
				"no Gateway":   func(b *Bindings) { b.Model.Gateway = false },
				"Tailscale":    func(b *Bindings) { b.Integrations.Tailscale = true },
				"missing":      func(b *Bindings) { b.TrustedProxyIPs = nil },
				"sidecar":      func(b *Bindings) { b.TrustedProxyIPs = []string{"127.0.0.6"} },
				"CIDR":         func(b *Bindings) { b.TrustedProxyIPs = []string{"127.0.0.0/8"} },
				"extra proxy":  func(b *Bindings) { b.TrustedProxyIPs = []string{"127.0.0.1", "192.0.2.10"} },
			} {
				t.Run(name, func(t *testing.T) {
					changed := bindings
					mutate(&changed)
					if _, err := RenderConfig(agent, changed); err == nil {
						t.Fatal("inconsistent managed mTLS binding was accepted")
					}
				})
			}
		})
	}
	raw, err := json.Marshal(Bindings{})
	if err != nil || strings.Contains(string(raw), `"IngressMTLS"`) {
		t.Fatal("default binding serialized shape changed:", err)
	}
}

func TestNativeConfigAssetsUseRegisteredFunctions(t *testing.T) {
	for _, harness := range []string{"openclaw", "hermes"} {
		t.Run(harness, func(t *testing.T) {
			path := filepath.Join("..", "..", "..", "cetirun-chart", "agents", harness, "config", "native.tpl")
			asset, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := template.New(harness).Funcs(nativeTemplateFunctions()).Parse(string(asset)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package config

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
)

func TestLoadSandboxRuntimeProfiles(t *testing.T) {
	profile := SandboxRuntime{ChartVersion: "0.0.0-dev-test", Image: "registry.example.com/openclaw@sha256:" + strings.Repeat("a", 64), Port: 18789}
	raw, _ := json.Marshal(map[string]SandboxRuntime{"openclaw": profile})
	t.Setenv("SANDBOX_RUNTIME_PROFILES", string(raw))
	t.Setenv("SANDBOX_RUNTIME_DEFAULTS_FROM_CONTROLLER", "true")
	got, ok := Load().SandboxRuntimeFor(api.AgentTypeOpenClaw)
	if !ok || got != profile {
		t.Fatalf("approved profile missing: %+v", got)
	}
	for _, invalid := range []string{string(raw) + " trailing", `{"openclaw":{"unknown":true}}`, `{"openclaw":null}`, `{"custom":{}}`} {
		t.Setenv("SANDBOX_RUNTIME_PROFILES", invalid)
		cfg := Load()
		if _, ok := cfg.SandboxRuntimeFor(api.AgentTypeOpenClaw); ok || cfg.SandboxControllerDefaults {
			t.Fatal("invalid runtime configuration authorized defaults")
		}
	}
}

func TestSandboxRuntimeChartWiring(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("Helm is required for the Portal chart rendering check")
	}
	profiles := `{"openclaw":{"chartVersion":"1.2.3","image":"registry.example.com/openclaw@sha256:` + strings.Repeat("a", 64) + `","port":18789}}`
	out, err := exec.Command(helm, "template", "portal", "../../chart", "--set", "istio.host=portal.example.com", "--set-json", "sandboxRuntimeProfiles="+profiles).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "name: SANDBOX_RUNTIME_PROFILES") || !strings.Contains(string(out), `\"chartVersion\":\"1.2.3\"`) {
		t.Fatalf("runtime chart wiring failed: %v\n%s", err, out)
	}
}

func TestSandboxRuntimeControllerDefaultsRequireOptIn(t *testing.T) {
	t.Setenv("SANDBOX_RUNTIME_PROFILES", "")
	for _, value := range []string{"", "false", "true", "TRUE", "1"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("SANDBOX_RUNTIME_DEFAULTS_FROM_CONTROLLER", value)
			if got := Load().SandboxControllerDefaults; got != (value == "true") {
				t.Fatalf("controller defaults opt-in = %t for %q", got, value)
			}
		})
	}
}

func TestSandboxRuntimeControllerDefaultsChartWiring(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("Helm is required for the Portal chart rendering check")
	}
	for _, value := range []string{"", "false", "true", "1", "invalid"} {
		t.Run(value, func(t *testing.T) {
			args := []string{"template", "portal", "../../chart", "--set", "istio.host=portal.example.com"}
			if value != "" {
				args = append(args, "--set", "sandboxRuntimeDefaultsFromController="+value)
			}
			out, err := exec.Command(helm, args...).CombinedOutput()
			if value == "1" || value == "invalid" {
				if err == nil || !strings.Contains(string(out), "sandboxRuntimeDefaultsFromController must be a boolean") {
					t.Fatalf("invalid opt-in was not rejected: %v\n%s", err, out)
				}
				return
			}
			if err != nil {
				t.Fatalf("Portal chart render: %v\n%s", err, out)
			}
			if present := strings.Contains(string(out), "name: SANDBOX_RUNTIME_DEFAULTS_FROM_CONTROLLER"); present != (value == "true") {
				t.Fatalf("controller-defaults environment presence = %t for %q", present, value)
			}
		})
	}
}

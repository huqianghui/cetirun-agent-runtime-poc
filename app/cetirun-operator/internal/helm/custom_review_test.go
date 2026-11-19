// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	"helm.sh/helm/v3/pkg/chartutil"
	"sigs.k8s.io/yaml"
)

func TestCustomScalarLikeBindingNames(t *testing.T) {
	for _, name := range []string{"yes", "no", "on", "off", "true", "false", "null", "123", "1e3"} {
		for _, port := range []any{nil, 8080} {
			input := customInput(port)
			bindings := input["customBindings"].(runtimebindings.Custom)
			bindings.AgentName = name
			bindings.Model.Name, bindings.Model.Key = name, name
			bindings.Home.Name = "data-openclaw-" + name + "-cetirun-0"
			bindings.Home.Claim.StorageClass = name
			bindings.Context.Name = name
			bindings.Namespace.Name = "cetirun-" + name
			input["customBindings"] = bindings
			input["fullnameOverride"] = "openclaw-" + name + "-cetirun"
			input["agentRequest"].(map[string]any)["releaseName"] = "openclaw-" + name
			p, err := prepareChart(testChart(t), input, false)
			if err != nil {
				t.Fatal(err)
			}
			if err := p.prepareCustomAction(customActionConfig(), bindings.Namespace.Name, p.releaseName); err != nil {
				t.Fatalf("scalar-like name %q rejected: %v", name, err)
			}
		}
	}
}
func TestCustomPlatformValuesAreCheckedBeforeAction(t *testing.T) {
	for _, values := range []map[string]any{
		{"podDisruptionBudget": map[string]any{"enabled": true, "maxUnavailable": 1}},
		{"podDisruptionBudget": map[string]any{"enabled": true, "minAvailable": "abc"}},
		{"networkPolicy": map[string]any{"blockedCIDRs": []any{"10.0.0.0/33"}}},
		{"podAnnotations": map[string]any{"bad key": "value"}},
		{"podAnnotations": map[string]any{"example.com/value": true}},
		// The platform's log and metrics collection reads adx-mon/ annotations.
		{"podAnnotations": map[string]any{"adx-mon/scrape": "true"}},
		{"podAnnotations": map[string]any{"adx-mon/log-destination": "CetiRunLogs:Platform"}},
	} {
		input := customInput(nil)
		for key, value := range values {
			input[key] = value
		}
		if _, err := PrepareChart(testChart(t), input); err == nil {
			t.Fatalf("API-invalid configuration accepted: %v", values)
		}
	}
	for _, value := range []any{1, "1", "010", "1e2", "50%"} {
		input := customInput(nil)
		input["podDisruptionBudget"] = map[string]any{"enabled": true, "minAvailable": value}
		if _, err := PrepareChart(testChart(t), input); err != nil {
			t.Fatalf("valid PDB value %v rejected: %v", value, err)
		}
	}
}

func TestCustomNestedNullUsesEffectiveHelmDefaults(t *testing.T) {
	for _, values := range []map[string]any{
		{"desktop": map[string]any{"enabled": nil}},
		{"desktop": map[string]any{"logging": nil}},
		{"desktop": map[string]any{"resources": nil}},
		{"desktop": map[string]any{"screenResolution": nil}},
		{"resources": map[string]any{"requests": nil}},
		{"execRestriction": map[string]any{"enabled": nil}},
		{"networkPolicy": map[string]any{"enabled": nil}},
	} {
		input := customInput(8080)
		for key, value := range values {
			input[key] = value
		}
		p, err := prepareChart(testChart(t), input, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := p.prepareCustomAction(customActionConfig(), "cetirun-example", p.releaseName); err != nil {
			t.Fatalf("nested null disagreed with Helm defaults: %v: %v", values, err)
		}
	}
}

func TestCustomWorkloadHashClassifiesInputs(t *testing.T) {
	first, err := PrepareChart(testChart(t), customInput(8080))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		key      string
		value    any
		workload bool
	}{
		{"ttyd", map[string]any{"enabled": false, "port": 9000, "shell": "/bin/sh"}, false},
		{"networkPolicy", map[string]any{"blockedCIDRs": []any{"10.0.0.0/8"}}, false},
		{"podDisruptionBudget", map[string]any{"enabled": true}, false},
		{"podAnnotations", map[string]any{"example.com/rollout": "new"}, true},
		{"desktop", map[string]any{"logging": true}, true},
		{"resources", map[string]any{"requests": map[string]any{"cpu": "300m"}}, true},
	} {
		input := customInput(8080)
		input[test.key] = test.value
		next, err := PrepareChart(testChart(t), input)
		if err != nil {
			t.Fatal(err)
		}
		if (next.WorkloadHash != first.WorkloadHash) != test.workload {
			t.Fatalf("%s workload classification incorrect", test.key)
		}
		if test.key == "ttyd" && next.PlanHash != first.PlanHash {
			t.Fatal("disabled ttyd settings changed the full plan")
		}
	}
}

func TestCustomRejectedObjectMatrix(t *testing.T) {
	for _, change := range []string{"hpa", "sidecar", "binding", "service-port", "service-type", "ingress", "duplicate", "namespace", "finalizer", "keep-annotation"} {
		t.Run(change, func(t *testing.T) {
			input := customInput(8080)
			input["autoscaling"] = map[string]any{"enabled": true, "minReplicas": 1, "maxReplicas": 1}
			p, err := prepareChart(testChart(t), input, false)
			if err != nil {
				t.Fatal(err)
			}
			cfg := customActionConfig()
			cfg.Capabilities.APIVersions = append(cfg.Capabilities.APIVersions, "networking.istio.io/v1/Sidecar")
			if err := p.prepareCustomAction(cfg, "cetirun-example", p.releaseName); err != nil {
				t.Fatal(err)
			}
			objects := platformRender(t, p, cfg.Capabilities)
			switch change {
			case "hpa":
				object(objects["HorizontalPodAutoscaler"]["spec"])["minReplicas"] = 2
			case "sidecar":
				array(object(objects["Sidecar"]["spec"])["egress"])[0].(map[string]any)["hosts"] = []any{"*/*"}
			case "binding":
				object(objects["ValidatingAdmissionPolicyBinding"]["spec"])["validationActions"] = []any{"Warn"}
			case "service-port":
				array(object(objects["Service"]["spec"])["ports"])[0].(map[string]any)["port"] = 9000
			case "service-type":
				object(objects["Service"]["spec"])["type"] = "NodePort"
			case "ingress":
				object(objects["NetworkPolicy"]["spec"])["ingress"] = []any{map[string]any{}}
			case "namespace":
				object(objects["NetworkPolicy"]["metadata"])["namespace"] = "other"
			case "finalizer":
				object(objects["NetworkPolicy"]["metadata"])["finalizers"] = []any{"example.com/unowned"}
			case "keep-annotation":
				object(objects["NetworkPolicy"]["metadata"])["annotations"] = map[string]any{"helm.sh/resource-policy": "keep"}
			}
			var payload bytes.Buffer
			for _, value := range objects {
				data, err := yaml.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				payload.WriteString("---\n")
				payload.Write(data)
			}
			if change == "duplicate" {
				data, _ := yaml.Marshal(objects["Service"])
				payload.WriteString("---\n")
				payload.Write(data)
			}
			// Exercise the boundary itself, not the already-sealed digest.
			p.manifestHash = ""
			p.sealedInput = ""
			if err := p.validateCustomManifest("cetirun-example", p.releaseName, payload.Bytes()); err == nil {
				t.Fatal("unapproved object mutation accepted")
			}
		})
	}
}

func TestCustomClientRequiresAPIPreflight(t *testing.T) {
	p, err := PrepareChart(testChart(t), customInput(nil))
	if err != nil {
		t.Fatal(err)
	}
	err = NewClient(ChartReference{}).InstallOrUpgrade("cetirun-example", p.releaseName, p)
	if err == nil || !strings.Contains(err.Error(), "API preflight") {
		t.Fatalf("missing API preflight did not fail before in-cluster setup: %v", err)
	}
}

func TestCustomCapabilitySelectionIsAnInput(t *testing.T) {
	p, err := prepareChart(testChart(t), customInput(nil), false)
	if err != nil {
		t.Fatal(err)
	}
	p.setCustomCapabilities(chartutil.DefaultCapabilities)
	first, workload := p.PlanHash, p.WorkloadHash
	caps := chartutil.DefaultCapabilities.Copy()
	caps.APIVersions = append(caps.APIVersions, "networking.istio.io/v1/Sidecar")
	p.setCustomCapabilities(caps)
	if p.PlanHash == first || p.WorkloadHash != workload {
		t.Fatal("mesh capability must change full inputs, not the writer hash")
	}
}

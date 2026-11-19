// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
)

func customCopilotInput() map[string]any {
	input := customInput(8080)
	b := input["customBindings"].(runtimebindings.Custom)
	b.Copilot = &runtimebindings.CopilotProxy{Image: "registry.example.com/cetirun-ai-gateway:0.1.1", KeyID: "012345abcdef", ResponsesModels: []string{"example-responses"}, SignInGeneration: "abcdef"}
	input["customBindings"] = b
	return input
}

func TestCustomCopilotIsSeparateAndGatewayOnly(t *testing.T) {
	p, err := PrepareChart(testChart(t), customCopilotInput())
	if err != nil {
		t.Fatal(err)
	}
	objects := object(p.Values["runtime"])["copilotResources"].([]map[string]any)
	if len(objects) != 5 {
		t.Fatalf("expected five proxy resources, got %d", len(objects))
	}
	for _, item := range objects {
		o := object(item)
		if o["kind"] == "Deployment" {
			spec := object(object(object(o["spec"])["template"])["spec"])
			if spec["runtimeClassName"] != nil || spec["automountServiceAccountToken"] != false {
				t.Fatal("proxy inherited Agent execution or Kubernetes credentials")
			}
			raw, _ := json.Marshal(spec)
			if strings.Contains(string(raw), "persistentVolumeClaim") || strings.Contains(string(raw), "/home/") || strings.Contains(string(raw), "ai-gateway-virtual-key") {
				t.Fatal("proxy shares Agent credentials or HOME")
			}
		}
		if o["kind"] == "NetworkPolicy" {
			ingress := array(object(o["spec"])["ingress"])
			if len(ingress) != 1 || len(array(object(ingress[0])["from"])) != 1 {
				t.Fatal("proxy admits extra callers")
			}
			caller := object(array(object(ingress[0])["from"])[0])
			if object(object(caller["namespaceSelector"])["matchLabels"])["kubernetes.io/metadata.name"] != "models" || object(object(caller["podSelector"])["matchLabels"])["app"] != "gateway" {
				t.Fatal("proxy is not restricted to the Gateway")
			}
		}
	}
	contribution, err := p.customContribution()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(contribution)
	for _, secret := range []string{"ai-gateway-copilot-proxy", "copilot-token", "LITELLM_MASTER_KEY"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("proxy credential entered the Agent contribution")
		}
	}
}

func TestCustomCopilotRequiresCompatibleChartAndExactManifest(t *testing.T) {
	old := testChart(t)
	delete(old.Metadata.Annotations, "cetirun.microsoft.com/custom-copilot")
	if _, err := PrepareChart(old, customCopilotInput()); err == nil {
		t.Fatal("legacy Chart silently dropped the proxy")
	}
	p, err := PrepareChart(testChart(t), customCopilotInput())
	if err != nil {
		t.Fatal(err)
	}
	resources := object(p.Values["runtime"])["copilotResources"].([]map[string]any)
	object(resources[0])["automountServiceAccountToken"] = true
	if err := p.ValidateCustom("cetirun-example", "openclaw-example"); err == nil {
		t.Fatal("modified proxy permission was accepted")
	}
}

func TestCustomProxyOnlyUpdatesDoNotRestartAgent(t *testing.T) {
	input := customCopilotInput()
	first, err := PrepareChart(testChart(t), input)
	if err != nil {
		t.Fatal(err)
	}
	b := input["customBindings"].(runtimebindings.Custom)
	b.Copilot.KeyID = "fedcba987654"
	b.Copilot.SignInGeneration = "bbbbbb"
	input["customBindings"] = b
	second, err := PrepareChart(testChart(t), input)
	if err != nil {
		t.Fatal(err)
	}
	if first.WorkloadHash != second.WorkloadHash || first.PlanHash == second.PlanHash {
		t.Fatal("proxy update was not separated from Agent HOME handoff")
	}
	b.Model.Revision = "2"
	input["customBindings"] = b
	third, err := PrepareChart(testChart(t), input)
	if err != nil {
		t.Fatal(err)
	}
	if second.WorkloadHash == third.WorkloadHash {
		t.Fatal("changed Virtual Key did not restart the Agent")
	}
}

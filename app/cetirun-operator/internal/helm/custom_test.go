// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	"helm.sh/helm/v3/pkg/chart"
)

func customInput(port any) map[string]any {
	ref := func(name string) runtimebindings.ObjectReference {
		return runtimebindings.ObjectReference{Name: name, UID: name + "-uid", Revision: "1"}
	}
	image := "registry.example.com/agent@sha256:" + strings.Repeat("a", 64)
	b := runtimebindings.Custom{
		AgentName: "example", AgentUID: "example-uid", Generation: 1, Namespace: ref("cetirun-example"),
		ApprovedType: "custom", ApprovedImage: image, PolicyRevision: "policy-1",
		Model: runtimebindings.ModelAccess{ObjectReference: ref("model"), BaseURL: "http://gateway.models.svc/v1", Key: "api-key"},
		Home: runtimebindings.Home{Name: "data-openclaw-example-cetirun-0", FSGroup: 2000,
			Claim: &runtimebindings.HomeClaim{StorageClass: "default", StorageSize: "100Gi", AccessMode: "ReadWriteOnce"}},
		Context: runtimebindings.Context{ObjectReference: ref("context"), Files: map[string]string{
			"canonical": "AGENTS.md", "memory": "skills/cetirun-memory/SKILL.md", "example": "skills/example/SKILL.md",
		}},
		Network: runtimebindings.Network{Revision: "network-1", GatewayPort: 8080,
			Gateway: runtimebindings.Peer{Namespace: "models", PodLabels: map[string]string{"app": "gateway"}},
			DNS:     runtimebindings.Peer{Namespace: "kube-system", PodLabels: map[string]string{"k8s-app": "kube-dns"}},
			Callers: []runtimebindings.Peer{{Namespace: "callers", PodLabels: map[string]string{"app": "approved-client"}}},
		},
	}
	return map[string]any{
		"ttyd":      map[string]any{"enabled": false},
		"agentType": "custom", "customBindings": b, "fullnameOverride": "openclaw-example-cetirun",
		"agentRequest": map[string]any{"type": "custom", "image": image, "username": "worker", "port": port, "releaseName": "openclaw-example"},
	}
}

func TestCustomChartBothModes(t *testing.T) {
	for _, port := range []any{nil, 8080} {
		chrt := testChart(t)
		// Custom does not need native profiles, templates or a preferred model.
		files := chrt.Files[:0]
		for _, file := range chrt.Files {
			if !strings.HasPrefix(file.Name, "agents/hermes/") && !strings.HasPrefix(file.Name, "agents/openclaw/") {
				files = append(files, file)
			}
		}
		chrt.Files = files
		prepared, err := PrepareChart(chrt, customInput(port))
		if err != nil {
			t.Fatal(err)
		}
		if (prepared.Agent.Port == nil) != (port == nil) || prepared.Agent.HomeDir != "/home/worker" {
			t.Fatal("Custom resolution changed optional port or HOME")
		}
		if err := prepared.ValidateCustom("cetirun-example", "openclaw-example"); err != nil {
			t.Fatal(err)
		}
		runtime := object(prepared.Values["runtime"])
		raw, _ := json.Marshal(runtime)
		for _, value := range []string{"sk-dummy", "gateway-token", "openclaw.json", "hermes-config.yaml"} {
			if strings.Contains(string(raw), value) {
				t.Fatalf("unexpected native or secret data in runtime: %s", value)
			}
		}
		if prepared.Values["customBindings"] != nil || prepared.Values["agentRequest"] != nil {
			t.Fatal("preparation-only input leaked into release values")
		}
	}
}

func TestCustomChartRejectsMissingBindingsAndOverrides(t *testing.T) {
	for _, key := range []string{"customBindings", "runtime", "sidecars", "extraEnvFrom", "podSecurityContext", "persistence",
		"extraVolumes", "extraVolumeMounts", "imagePullSecrets", "initContainers", "desktop", "logger", "secrets"} {
		input := customInput(nil)
		if key == "customBindings" {
			input[key] = map[string]any{}
		} else {
			input[key] = map[string]any{"injected": true}
		}
		if _, err := PrepareChart(testChart(t), input); err == nil {
			t.Fatalf("%s override accepted", key)
		}
	}
}

func TestCustomBindingRevisionChangesPlan(t *testing.T) {
	input := customInput(nil)
	first, err := PrepareChart(testChart(t), input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := PrepareChart(testChart(t), input)
	if err != nil || first.PlanHash != second.PlanHash {
		t.Fatal("plan is not deterministic")
	}
	b := input["customBindings"].(runtimebindings.Custom)
	b.Model.Revision = "2"
	input["customBindings"] = b
	second, err = PrepareChart(testChart(t), input)
	if err != nil || first.PlanHash == second.PlanHash {
		t.Fatal("model revision did not invalidate the plan")
	}
	b.Context.Revision = "2"
	input["customBindings"] = b
	third, err := PrepareChart(testChart(t), input)
	if err != nil || second.PlanHash == third.PlanHash {
		t.Fatal("context revision did not invalidate the plan")
	}
}

func TestCustomFinalManifestRejectsExtraResourcesAndMutation(t *testing.T) {
	for name, extra := range map[string]string{
		"hook":    "apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: injected\n  annotations:\n    helm.sh/hook: pre-upgrade\n",
		"secret":  "apiVersion: v1\nkind: Secret\nmetadata:\n  name: injected\n",
		"ingress": "apiVersion: networking.k8s.io/v1\nkind: Ingress\nmetadata:\n  name: injected\n",
	} {
		t.Run(name, func(t *testing.T) {
			chrt := testChart(t)
			chrt.Templates = append(chrt.Templates, &chart.File{Name: "templates/extra.yaml", Data: []byte(extra)})
			if _, err := PrepareChart(chrt, customInput(8080)); err == nil {
				t.Fatal("unexpected rendered resource accepted")
			}
		})
	}
	for _, change := range [][2]string{
		{"automountServiceAccountToken: false", "automountServiceAccountToken: true"},
		{"runAsNonRoot: true", "runAsUser: 1024\nrunAsNonRoot: true"},
		{"readOnly: true", "readOnly: false"},
		{"type: OnDelete", "type: RollingUpdate"},
		{`runtimeClassName: {{ . | quote }}`, `runtimeClassName: unapproved-runtime`},
	} {
		chrt := testChart(t)
		target := "templates/agents/custom/_runtime.tpl"
		if strings.HasPrefix(change[0], "runtimeClassName") {
			target = "templates/_platform.tpl"
		}
		for _, file := range chrt.Templates {
			if file.Name == target {
				if !strings.Contains(string(file.Data), change[0]) {
					t.Fatal("rendered mutation did not match")
				}
				file.Data = []byte(strings.ReplaceAll(string(file.Data), change[0], change[1]))
			}
		}
		if _, err := PrepareChart(chrt, customInput(8080)); err == nil {
			t.Fatalf("rendered mutation accepted: %s", change[1])
		}
	}
}

func TestCustomResourceErrorsFailBeforeActionPreparation(t *testing.T) {
	for _, resources := range []map[string]any{
		{"requests": map[string]any{"cpu": "8"}, "limits": map[string]any{"cpu": "1"}},
		{"requests": map[string]any{"memory": "-1Mi"}},
	} {
		input := customInput(nil)
		input["resources"] = resources
		if _, err := PrepareChart(testChart(t), input); err == nil {
			t.Fatal("invalid resources accepted")
		}
	}
}

func TestCustomPreparedManifestCannotChangeBeforeApply(t *testing.T) {
	prepared, err := PrepareChart(testChart(t), customInput(nil))
	if err != nil {
		t.Fatal(err)
	}
	prepared.Values["resources"] = map[string]any{"requests": map[string]any{"cpu": "2"}}
	if err := prepared.ValidateCustom("cetirun-example", "openclaw-example"); err == nil {
		t.Fatal("post-validation plan mutation was accepted")
	}
}

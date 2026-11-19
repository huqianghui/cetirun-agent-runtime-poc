// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"encoding/json"
	"strings"
	"testing"

	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload/testfixture"
	"helm.sh/helm/v3/pkg/release"
)

func TestSandboxSDKStagesEmptySkillsWithoutClaimIdentity(t *testing.T) {
	input := testfixture.New("openclaw").Input
	input.Skills, input.SkillsMode = nil, w.SkillsModeEmpty
	input.Helpers.Desktop = false
	input.GatewayKey = &w.ObjectIdentity{Name: "gateway-key", UID: "gateway-key-uid"}
	input.AuthorizedSecrets = append(input.AuthorizedSecrets, input.GatewayKey.Name)
	bundle, err := w.Build(input)
	if err != nil {
		t.Fatal(err)
	}
	values := sampleInput("openclaw", "openai", false)
	secrets := values["secrets"].(map[string]any)
	secrets["existingSecret"], secrets["model"], secrets["apiBase"] = "runtime-secret", "example-model", "http://gateway.example.com/v1"
	values["aiGateway"] = map[string]any{
		"enabled": true, "keySecretName": input.GatewayKey.Name, "keySecretKey": "api-key",
		"models": []any{map[string]any{"id": "example-model", "name": "Example"}},
	}
	prepared, err := PrepareSandboxChart(testChart(t), values, nil, bundle)
	if err != nil {
		t.Fatal(err)
	}
	cfg, api, _ := sandboxSDKConfig()
	for revision := 1; revision <= 2; revision++ {
		if revision == 2 {
			driftSandboxTestConfig(&sandboxActionKube{objects: api.objects})
		}
		if err := stageSandboxTest(prepared, cfg, sandboxStageCheck); err != nil {
			t.Fatal("real Helm staging rejected empty skills:", err)
		}
		last, err := cfg.Releases.Last(input.ReleaseName)
		if err != nil || last.Info.Status != release.StatusDeployed || last.Version != revision {
			t.Fatal("empty-skills stage was not durably recorded:", err)
		}
		seal, err := sandboxReleaseSeal(last)
		if err != nil || seal.Skills != (w.ObjectIdentity{}) ||
			object(last.Config[sandboxStageConfigKey])["skillsMode"] != string(w.SkillsModeEmpty) {
			t.Fatal("stored provenance lost the explicit empty skills source:", err)
		}
		original := last.Config[sandboxStageConfigKey]
		invalids := []sandboxStageSeal{seal, seal, seal}
		invalids[0].SkillsMode = ""
		invalids[1].SkillsMode = "unsupported"
		invalids[2].Skills = w.ObjectIdentity{Name: "skills", UID: "other-skills"}
		for _, invalid := range invalids {
			if invalid.sameOwner(seal) {
				t.Fatal("different skills source was accepted as the same sealed owner")
			}
			invalid.Digest = invalid.digest()
			encoded, err := json.Marshal(invalid)
			if err != nil {
				t.Fatal(err)
			}
			var value map[string]any
			if err := json.Unmarshal(encoded, &value); err != nil {
				t.Fatal(err)
			}
			last.Config[sandboxStageConfigKey] = value
			if _, err := sandboxReleaseSeal(last); err == nil {
				t.Fatal("inconsistent stored skills provenance was accepted")
			}
		}
		last.Config[sandboxStageConfigKey] = original
	}
	for _, resource := range api.objects {
		if resource.GetKind() == "PersistentVolumeClaim" || resource.GetKind() == "PersistentVolume" {
			t.Fatal("empty-skills Helm staging created a storage resource")
		}
	}
}

func TestSandboxSealPreservesDefaultSkillsWireShape(t *testing.T) {
	seal := sandboxSealIdentity(testfixture.New("openclaw").Input)
	raw, err := json.Marshal(seal)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"skillsMode"`) {
		t.Fatal("the existing PVC-backed seal gained a serialized mode")
	}
}

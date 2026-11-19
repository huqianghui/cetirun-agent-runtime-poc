// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxworkload_test

import (
	"encoding/json"
	"strings"
	"testing"

	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload/testfixture"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func emptySkillsInput() w.Input {
	input := testfixture.New("openclaw").Input
	input.Skills, input.SkillsMode = nil, w.SkillsModeEmpty
	input.Helpers.Desktop = false
	input.GatewayKey = &w.ObjectIdentity{Name: "gateway-key", UID: "gateway-key-uid"}
	input.AuthorizedSecrets = append(input.AuthorizedSecrets, input.GatewayKey.Name)
	return input
}

func TestEmptySkillsBundleRejectsInconsistentInputs(t *testing.T) {
	if _, err := w.Build(emptySkillsInput()); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*w.Input){
		"unknown mode":      func(i *w.Input) { i.SkillsMode = "unsupported" },
		"implicit empty":    func(i *w.Input) { i.SkillsMode = "" },
		"non-native":        func(i *w.Input) { i.GatewayKey = nil },
		"Hermes":            func(i *w.Input) { i.AgentType = "hermes"; i.Helpers.Doctor = false },
		"empty PVC pointer": func(i *w.Input) { i.Skills = &corev1.PersistentVolumeClaim{} },
		"PVC and empty": func(i *w.Input) {
			i.Skills = &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "skills", UID: "skills-uid"}}
		},
		"identity volume collision": func(i *w.Input) {
			i.Controls.Volumes = []corev1.Volume{{Name: "empty-skills", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			input := emptySkillsInput()
			mutate(&input)
			if _, err := w.Build(input); err == nil {
				t.Fatal("inconsistent empty-skills bundle was accepted")
			}
		})
	}
}

func TestEmptySkillsBundleNeverDiscardsRequestedPackages(t *testing.T) {
	bundle, err := w.Build(emptySkillsInput())
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]any{
		"secrets":   map[string]any{"existingSecret": "runtime-secret"},
		"aiGateway": map[string]any{"enabled": true, "keySecretName": "gateway-key", "keySecretKey": "api-key"},
	}
	for _, skills := range []any{
		"invalid", map[string]any{"npm": []string{"example-package"}},
		map[string]any{"clawHub": []string{"example-skill"}},
		map[string]any{"npm": "invalid"}, map[string]any{"existingClaim": "skills"},
	} {
		values["skills"] = skills
		if _, err := bundle.Apply(values); err == nil {
			t.Fatalf("empty skills discarded requested source: %#v", skills)
		}
	}
	values["skills"] = map[string]any{"npm": []string{}, "clawHub": []string{}}
	out, err := bundle.Apply(values)
	if err != nil {
		t.Fatal(err)
	}
	skills := out["skills"].(map[string]any)
	if skills["shared"] != false || skills["existingClaim"] != "" || skills["mountPath"] != w.SkillsPath ||
		out[w.ValuesKey].(map[string]any)["skillsMode"] != string(w.SkillsModeEmpty) {
		t.Fatal("explicit empty source did not reach the protected chart contract")
	}
}

func TestPublishedSkillsBundleNeverDiscardsRequestedPackages(t *testing.T) {
	for _, harness := range []string{"openclaw", "hermes"} {
		t.Run(harness, func(t *testing.T) {
			bundle, err := w.Build(testfixture.New(harness).Input)
			if err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"npm", "clawHub"} {
				values := map[string]any{
					"secrets": map[string]any{"existingSecret": "runtime-secret"},
					"skills":  map[string]any{field: []string{"synthetic-selected-skill"}},
				}
				if _, err := bundle.Apply(values); err == nil || !strings.Contains(err.Error(), "SkillsMaterializationPending") {
					t.Fatalf("published PVC silently discarded %s selection: %v", field, err)
				}
			}
			values, err := bundle.Apply(map[string]any{
				"secrets": map[string]any{"existingSecret": "runtime-secret"},
				"skills":  map[string]any{"npm": []string{}, "clawHub": []string{}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if values["skills"].(map[string]any)["existingClaim"] != "skills-example" {
				t.Fatal("valid published skills source was not retained")
			}
		})
	}
}

func TestSkillsModePreservesDefaultSerializedShapes(t *testing.T) {
	input := testfixture.New("openclaw").Input
	for _, value := range []any{input, w.Expectation{Skills: w.ObjectIdentity{Name: input.Skills.Name, UID: input.Skills.UID}}} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.ToLower(string(raw)), `"skillsmode"`) {
			t.Fatal("default PVC representation gained a serialized mode")
		}
	}
	bundle, err := w.Build(input)
	if err != nil {
		t.Fatal(err)
	}
	values, err := bundle.Apply(map[string]any{"secrets": map[string]any{"existingSecret": "runtime-secret"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, present := values[w.ValuesKey].(map[string]any)["skillsMode"]; present {
		t.Fatal("default PVC chart values gained an empty-source field")
	}
}

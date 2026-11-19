// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxworkload_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload/testfixture"
)

// skillMarketplaceInput is a common-executor built-in Agent whose skills claim
// the Skill Marketplace provides, for the Team of testfixture.New.
func skillMarketplaceInput(kind string) w.Input {
	input := testfixture.New(kind).Input
	input.Reconciled = true
	input.Helpers.Doctor, input.Helpers.Desktop = false, false
	input.GatewayKey = &w.ObjectIdentity{Name: "gateway-key", UID: "gateway-key-uid"}
	input.AuthorizedSecrets = append(input.AuthorizedSecrets, input.GatewayKey.Name)
	input.SkillMarketplace = &w.SkillMarketplaceInput{
		TeamPath:        api.SkillMarketplaceTeamPath(string(input.Resolution.Binding.TeamUID)),
		SelectionDigest: api.SkillMarketplaceSelectionDigest([]string{"example-skill"}),
	}
	return input
}

func skillMarketplaceValues() map[string]any {
	return map[string]any{
		"secrets":   map[string]any{"existingSecret": "runtime-secret"},
		"aiGateway": map[string]any{"enabled": true, "keySecretName": "gateway-key", "keySecretKey": "api-key"},
		// Platform values cannot select a subtree or selection themselves.
		"skills": map[string]any{"subPath": "teams/another-team", "marketplace": map[string]any{"selectionDigest": "sha256:forged"}},
	}
}

func TestSkillMarketplaceSelectsTheTeamSubtree(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		t.Run(kind, func(t *testing.T) {
			input := skillMarketplaceInput(kind)
			bundle, err := w.Build(input)
			if err != nil {
				t.Fatal(err)
			}
			values, err := bundle.Apply(skillMarketplaceValues())
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]any{
				"shared": true, "existingClaim": input.Skills.Name, "mountPath": w.SkillsPath,
				"npm": []any{}, "clawHub": []any{},
				"subPath":     "teams/team-uid",
				"marketplace": map[string]any{"selectionDigest": input.SkillMarketplace.SelectionDigest},
			}
			if !reflect.DeepEqual(values["skills"], want) {
				t.Fatalf("protected skills = %#v, want %#v", values["skills"], want)
			}
			// Without the marketplace the protected skills stay as before.
			input.SkillMarketplace = nil
			bundle, err = w.Build(input)
			if err != nil {
				t.Fatal(err)
			}
			values, err = bundle.Apply(skillMarketplaceValues())
			if err != nil {
				t.Fatal(err)
			}
			delete(want, "subPath")
			delete(want, "marketplace")
			if !reflect.DeepEqual(values["skills"], want) {
				t.Fatalf("protected skills without the marketplace = %#v, want %#v", values["skills"], want)
			}
			raw, err := json.Marshal(bundle.Identity())
			if err != nil || strings.Contains(string(raw), "SkillMarketplace") {
				t.Fatal("an input without the marketplace gained a serialized field:", err)
			}
		})
	}
}

func TestSkillMarketplaceInputRequiresTheBoundTeamAndAClaim(t *testing.T) {
	for name, mutate := range map[string]func(*w.Input){
		"held lifetime": func(i *w.Input) { i.Reconciled = false },
		"empty skills":  func(i *w.Input) { i.Skills, i.SkillsMode = nil, w.SkillsModeEmpty },
		"another Team":  func(i *w.Input) { i.SkillMarketplace.TeamPath = "teams/another-team" },
		"claim root":    func(i *w.Input) { i.SkillMarketplace.TeamPath = "" },
		"parent path":   func(i *w.Input) { i.SkillMarketplace.TeamPath = "teams/../team-uid" },
		"nested path":   func(i *w.Input) { i.SkillMarketplace.TeamPath = "teams/team-uid/releases" },
		"Team UID path": func(i *w.Input) {
			i.Resolution.Binding.TeamUID, i.Controls.Binding.TeamUID = "team/uid", "team/uid"
			i.SkillMarketplace.TeamPath = "teams/team/uid"
		},
		"missing selection": func(i *w.Input) { i.SkillMarketplace.SelectionDigest = "" },
		"bare digest": func(i *w.Input) {
			i.SkillMarketplace.SelectionDigest = strings.TrimPrefix(i.SkillMarketplace.SelectionDigest, "sha256:")
		},
		"uppercase digest": func(i *w.Input) {
			i.SkillMarketplace.SelectionDigest = strings.ToUpper(i.SkillMarketplace.SelectionDigest)
		},
		"short digest": func(i *w.Input) { i.SkillMarketplace.SelectionDigest = i.SkillMarketplace.SelectionDigest[:20] },
	} {
		t.Run(name, func(t *testing.T) {
			input := skillMarketplaceInput("openclaw")
			mutate(&input)
			if _, err := w.Build(input); err == nil || !strings.Contains(err.Error(), "Skill Marketplace") {
				t.Fatal("an invalid Skill Marketplace selection was accepted or misclassified:", err)
			}
		})
	}
}

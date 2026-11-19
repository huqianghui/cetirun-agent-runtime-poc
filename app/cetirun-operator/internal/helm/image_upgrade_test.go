// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"errors"
	"strings"
	"testing"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/release"
)

func TestResolveImageUpgrade(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, scenario := range []string{"default", "same chart", "owner image", "target default", "custom", "no release", "different deployed image", "different deployed type", "download failure", "invalid target profile", "qualified old default", "explicit tag and digest"} {
			t.Run(kind+"/"+scenario, func(t *testing.T) {
				old, target := testChart(t), testChart(t)
				if kind == "hermes" {
					useHistoricalHermesProfile(old)
				}
				old.Metadata.Version, old.Metadata.AppVersion = "1.0.0", "1.0.0"
				target.Metadata.Version, target.Metadata.AppVersion = "2.0.0", "2.0.0"
				registry := "registry.example.com"
				image := registry + "/" + kind + "-gateway:1.0.0"
				want := registry + "/" + kind + "-gateway:2.0.0"
				previous := &release.Release{Chart: old, Config: map[string]any{"runtime": map[string]any{
					"agent": map[string]any{"type": kind, "image": image},
				}}}
				requestKind, version, wantLoad, wantError := kind, "2.0.0", true, false
				switch scenario {
				case "same chart":
					version, want, wantLoad = "1.0.0", image, false
				case "owner image":
					image = registry + "/owner-image:1"
					want, wantLoad = image, false
				case "target default":
					image, wantLoad = want, false
				case "custom":
					requestKind, want, wantLoad = "custom", image, false
				case "no release":
					previous, want, wantLoad = nil, image, false
				case "different deployed image":
					object(object(previous.Config["runtime"])["agent"])["image"] = "registry.example.com/owner-image:1"
					want, wantLoad = image, false
				case "different deployed type":
					object(object(previous.Config["runtime"])["agent"])["type"] = "custom"
					want, wantLoad = image, false
				case "download failure":
					wantError = true
				case "invalid target profile":
					target.Files = nil
					wantError = true
				case "qualified old default":
					for _, file := range target.Files {
						if file.Name == "agents/"+kind+"/profile.yaml" {
							file.Data = []byte(strings.Replace(string(file.Data), "qualifiedImages: []", "qualifiedImages: ["+image+"]", 1))
						}
					}
				case "explicit tag and digest":
					registry = ""
					image, want = "registry.example.com/old:old-tag@sha256:"+strings.Repeat("a", 64), "registry.example.com/new:new-tag@sha256:"+strings.Repeat("b", 64)
					object(old.Values[kind])["image"] = map[string]any{"repository": "registry.example.com/old", "tag": "old-tag", "digest": "sha256:" + strings.Repeat("a", 64)}
					object(target.Values[kind])["image"] = map[string]any{"repository": "registry.example.com/new", "tag": "new-tag", "digest": "sha256:" + strings.Repeat("b", 64)}
					object(object(previous.Config["runtime"])["agent"])["image"] = image
				}
				loaded := false
				got, err := resolveImageUpgrade(previous, version, requestKind, image, registry, func() (*chart.Chart, error) {
					loaded = true
					if scenario == "download failure" {
						return nil, errors.New("synthetic registry unavailable")
					}
					return target, nil
				})
				if loaded != wantLoad {
					t.Fatalf("target chart loaded=%t, want %t", loaded, wantLoad)
				}
				if (err != nil) != wantError {
					t.Fatalf("error=%v, want error=%t", err, wantError)
				}
				if wantError {
					return
				}
				if got != want {
					t.Fatalf("image=%q, want %q", got, want)
				}
				if scenario == "default" || scenario == "qualified old default" || scenario == "explicit tag and digest" {
					input := sampleInput(kind, "openai", false)
					input["agentRequest"] = map[string]any{"type": kind, "registry": registry, "image": got}
					prepared, err := PrepareChart(target, input)
					if err != nil || prepared.Agent.Image != want {
						t.Fatalf("migrated default cannot prepare the target chart: %v", err)
					}
				}
			})
		}
	}
}

func useHistoricalHermesProfile(chrt *chart.Chart) {
	for _, file := range chrt.Files {
		if file.Name == "agents/hermes/profile.yaml" {
			file.Data = []byte(`contractVersion: v1
nativeVersion: "0.21.5"
port: 8642
# Additional exact image references validated with this profile.
qualifiedImages: []
`)
		}
	}
}

func TestImageUpgradeRequiresExplicitRegistryMigration(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, version := range []string{"1.0.0", "2.0.0"} {
			t.Run(kind+"/"+version, func(t *testing.T) {
				old, target := testChart(t), testChart(t)
				old.Metadata.Version, old.Metadata.AppVersion = "1.0.0", "1.0.0"
				target.Metadata.Version, target.Metadata.AppVersion = version, version
				image := "cetirun.azurecr.io/" + kind + "-gateway:1.0.0"
				registry := "mcr.microsoft.com/aks/ai-runtime/cetirun"
				previous := &release.Release{Chart: old, Config: map[string]any{"runtime": map[string]any{
					"agent": map[string]any{"type": kind, "image": image},
				}}}
				got, err := resolveImageUpgrade(previous, version, kind, image, registry, func() (*chart.Chart, error) {
					t.Fatal("registry migration must not infer that the old pin is a target default")
					return nil, nil
				})
				if err != nil || got != image {
					t.Fatalf("old pin was changed: %q, %v", got, err)
				}
				input := sampleInput(kind, "openai", false)
				input["agentRequest"] = map[string]any{"type": kind, "registry": registry, "image": got}
				if _, err := PrepareChart(target, input); err == nil || !strings.Contains(err.Error(), "not qualified") {
					t.Fatalf("previous registry pin must require migration: %v", err)
				}
				want := registry + "/" + kind + "-gateway:" + version
				object(input["agentRequest"])["image"] = want
				prepared, err := PrepareChart(target, input)
				if err != nil || prepared.Agent.Image != want {
					t.Fatalf("explicit target pin did not recover: %v", err)
				}
			})
		}
	}
}

func TestImageUpgradePreservesOwnerImageQualification(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, test := range []struct {
			name      string
			qualified bool
		}{
			{name: "unqualified", qualified: false},
			{name: "qualified", qualified: true},
		} {
			t.Run(kind+"/"+test.name, func(t *testing.T) {
				chrt := testChart(t)
				image := "registry.example.com/owner-image:1"
				if test.qualified {
					for _, file := range chrt.Files {
						if file.Name == "agents/"+kind+"/profile.yaml" {
							file.Data = []byte(strings.Replace(string(file.Data), "qualifiedImages: []", "qualifiedImages: ["+image+"]", 1))
						}
					}
				}
				input := sampleInput(kind, "openai", false)
				input["agentRequest"] = map[string]any{"type": kind, "image": image}
				prepared, err := PrepareChart(chrt, input)
				if test.qualified {
					if err != nil || prepared.Agent.Image != image {
						t.Fatalf("qualified owner image was not retained: %v", err)
					}
				} else if err == nil || !strings.Contains(err.Error(), "not qualified") {
					t.Fatalf("unqualified owner image was not rejected: %v", err)
				}
			})
		}
	}
}

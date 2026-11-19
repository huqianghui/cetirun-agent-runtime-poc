// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const skillMarketplaceOverrideMessage = "skillOverrides cannot add or remove Skill Marketplace skills; select them on the Team"

// skillMarketplaceStorage is a synthetic storage block of an enabled marketplace.
const skillMarketplaceStorage = `"storage":{"resourceGroup":"example-rg","storageAccount":"exampleskills",` +
	`"containerName":"marketplace","storageClassName":"azureblob-nfs-premium"}`

func TestParseSkillMarketplaceConfig(t *testing.T) {
	for _, test := range []struct {
		name, raw string
		enabled   bool
	}{
		{"empty", "", false},
		{"enabled", `{"enabled":true,` + skillMarketplaceStorage + `}`, true},
		{"disabled", `{"enabled": false}`, false},
		{"disabled with storage", `{"enabled": false,` + skillMarketplaceStorage + `}`, false},
		{"field omitted", `{}`, false},
		{"surrounding whitespace", "\n {\"enabled\": true, " + skillMarketplaceStorage + "} \n", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			config, err := ParseSkillMarketplaceConfig(test.raw)
			if err != nil || config.Enabled != test.enabled {
				t.Fatalf("ParseSkillMarketplaceConfig(%q) = %+v, %v; want enabled=%t", test.raw, config, err, test.enabled)
			}
			if test.enabled && (config.Storage == nil || config.Storage.ContainerName != "marketplace") {
				t.Fatalf("storage %+v", config.Storage)
			}
		})
	}
	storage := func(field, value string) string {
		values := map[string]string{"resourceGroup": "example-rg", "storageAccount": "exampleskills",
			"containerName": "marketplace", "storageClassName": "azureblob-nfs-premium"}
		values[field] = value
		return fmt.Sprintf(`{"enabled":true,"storage":{"resourceGroup":%q,"storageAccount":%q,"containerName":%q,"storageClassName":%q}}`,
			values["resourceGroup"], values["storageAccount"], values["containerName"], values["storageClassName"])
	}
	for _, test := range []struct{ name, raw, problem string }{
		{"unknown field", `{"enabled":true,"catalog":"other"}`, `unknown field "catalog"`},
		{"misspelled field", `{"enable":true}`, `unknown field "enable"`},
		{"field case", `{"Enabled":true}`, `unknown field "Enabled"`},
		{"duplicate field", `{"enabled":false,"enabled":true}`, `duplicate field "enabled"`},
		{"string value", `{"enabled":"true"}`, "cannot unmarshal string"},
		{"not an object", `true`, "cannot unmarshal bool"},
		{"null", `null`, "null is not a JSON object"},
		{"malformed", `{"enabled":tru}`, "invalid character"},
		{"truncated", `{"enabled":true`, "unexpected end of JSON input"},
		{"trailing data", `{"enabled":true}{}`, "invalid character"},
		{"blank", " ", "unexpected end of JSON input"},
		{"oversized", `{"enabled":true}` + strings.Repeat(" ", maxSkillMarketplaceConfigBytes), "longer than 1024 bytes"},
		{"no storage", `{"enabled":true}`, "storage is required"},
		{"unknown storage field", `{"enabled":true,"storage":{"path":"x"}}`, `unknown field "storage.path"`},
		{"resource group", storage("resourceGroup", "rg."), "storage.resourceGroup"},
		{"storage account", storage("storageAccount", "Example"), "storage.storageAccount"},
		{"container", storage("containerName", "a--b"), "storage.containerName"},
		{"short container", storage("containerName", "ab"), "storage.containerName"},
		{"storage class", storage("storageClassName", "Not_A_Class"), "storage.storageClassName"},
	} {
		t.Run(test.name, func(t *testing.T) {
			// A refused value never enables the marketplace, even when part of
			// it decoded.
			config, err := ParseSkillMarketplaceConfig(test.raw)
			if err == nil || config.Enabled ||
				!strings.Contains(err.Error(), "invalid CETIRUN_SKILL_MARKETPLACE configuration") ||
				!strings.Contains(err.Error(), test.problem) {
				t.Fatalf("ParseSkillMarketplaceConfig(%q) = %+v, %v; want an error naming %q", test.raw, config, err, test.problem)
			}
		})
	}
}

// With the marketplace, Team entries without the npm: prefix are the
// marketplace's to deliver, so they stay out of the reserved clawHub list.
// npm packages are unchanged, and without the marketplace nothing changes.
func TestSkillMarketplaceKeepsTeamSkillsOutOfClawHub(t *testing.T) {
	team := &api.Team{Spec: api.TeamSpec{Skills: []string{"npm:@openclaw/matrix", "pdf-tools", "@example/fetch"}}}
	for _, test := range []struct {
		marketplace bool
		clawHub     any
	}{
		{false, []any{"@example/fetch", "pdf-tools"}},
		{true, nil},
	} {
		values := map[string]any{}
		if err := applySkillValues(values, team, api.SkillOverrideSpec{}, "", test.marketplace); err != nil {
			t.Fatal(err)
		}
		skills, _ := values["skills"].(map[string]any)
		if !reflect.DeepEqual(skills["npm"], []any{"@openclaw/matrix"}) || !reflect.DeepEqual(skills["clawHub"], test.clawHub) {
			t.Fatalf("marketplace=%t: skills = %#v", test.marketplace, skills)
		}
	}
	// A Team that selects only marketplace skills renders no skill values.
	values := map[string]any{}
	team = &api.Team{Spec: api.TeamSpec{Skills: []string{"pdf-tools"}}}
	if err := applySkillValues(values, team, api.SkillOverrideSpec{}, "", true); err != nil || values["skills"] != nil {
		t.Fatalf("a marketplace-only selection rendered skill values: %#v, %v", values["skills"], err)
	}
}

// A Sandbox Agent cannot install skills at its start. Without the
// marketplace, the selection of a policy-bound Team names skills that nothing
// delivers: the error says which, and what to do. It keeps the reason of
// skills that are not materialized. Agents of a Team without a policy keep
// their ClawHub installs, and npm entries are left to the existing checks.
func TestSkillsOfAPolicyBoundTeamNeedTheMarketplace(t *testing.T) {
	team := &api.Team{ObjectMeta: metav1.ObjectMeta{Name: "research"},
		Spec: api.TeamSpec{AgentPolicyRef: "policy", Skills: []string{"npm:@openclaw/matrix", "pdf-tools", "zh-notes"}}}
	err := skillsWithoutMarketplace(team, api.SkillOverrideSpec{}, false)
	if !errors.Is(err, errSkillMarketplaceOff) || !errors.Is(err, runtimebindings.ErrSkillsMaterializationPending) ||
		err.Error() != "SkillsMaterializationPending: the Team research selects the skills pdf-tools, zh-notes, which Sandbox Agents "+
			"receive only from the Skill Marketplace, and the Skill Marketplace is off: turn it on, or remove the skills from the Team" {
		t.Fatalf("error %v", err)
	}
	if err := skillsWithoutMarketplace(team, api.SkillOverrideSpec{}, true); err != nil {
		t.Fatalf("with the marketplace: %v", err)
	}
	if err := skillsWithoutMarketplace(team, api.SkillOverrideSpec{Remove: []string{"pdf-tools", "zh-notes"}}, false); err != nil {
		t.Fatalf("npm entries: %v", err)
	}
	if err := skillsWithoutMarketplace(&api.Team{Spec: api.TeamSpec{Skills: []string{"pdf-tools"}}}, api.SkillOverrideSpec{}, false); err != nil {
		t.Fatalf("a Team without a policy keeps ClawHub installs: %v", err)
	}
	// Both values paths refuse the selection before anything else.
	agent := newTestInstance("alice", "Alice", "research", "gateway-token")
	if _, err := (&AgentReconciler{DomainSuffix: "example.com"}).builtinHelmValues(agent, team, AgentIdentityHelmConfig{}); !errors.Is(err, errSkillMarketplaceOff) {
		t.Fatalf("built-in values: %v", err)
	}
	agent.Spec.Agent.Type, agent.Spec.GatewayTokenSecretRef = api.AgentTypeCustom, ""
	if _, err := (&AgentReconciler{}).customRuntimeValues(agent, team); !errors.Is(err, errSkillMarketplaceOff) {
		t.Fatalf("Custom values: %v", err)
	}
}

// The reconciler's switch reaches every built-in values path: the release
// values and the Sandbox platform values.
func TestSkillMarketplaceSwitchReachesBuiltinValues(t *testing.T) {
	agent := newTestInstance("alice", "Alice", "platform", "gateway-token")
	team := &api.Team{Spec: api.TeamSpec{Skills: []string{"npm:@openclaw/matrix", "pdf-tools"}}}
	for _, marketplace := range []bool{false, true} {
		r := &AgentReconciler{DomainSuffix: "example.com", SkillMarketplace: marketplace}
		release, err := r.builtinHelmValues(agent, team, AgentIdentityHelmConfig{})
		if err != nil {
			t.Fatal(err)
		}
		platform, _, err := r.sandboxHelmValues(agent, team)
		if err != nil {
			t.Fatal(err)
		}
		for name, values := range map[string]map[string]any{"release": release, "Sandbox platform": platform} {
			skills := values["skills"].(map[string]any)
			if _, clawHub := skills["clawHub"]; clawHub == marketplace || !reflect.DeepEqual(skills["npm"], []any{"@openclaw/matrix"}) {
				t.Fatalf("marketplace=%t: %s skills = %#v", marketplace, name, skills)
			}
		}
	}
}

// With the marketplace, an Agent cannot add or remove a marketplace skill
// itself, whether or not its Team selects it. npm overrides are unchanged,
// and without the marketplace nothing is refused.
func TestSkillMarketplaceRefusesAgentSkillOverrides(t *testing.T) {
	team := &api.Team{Spec: api.TeamSpec{Skills: []string{"npm:@openclaw/matrix", "pdf-tools"}}}
	for _, test := range []struct {
		name      string
		overrides api.SkillOverrideSpec
		refused   bool
	}{
		{"none", api.SkillOverrideSpec{}, false},
		{"npm packages", api.SkillOverrideSpec{Add: []string{"npm:extra"}, Remove: []string{"npm:@openclaw/matrix"}}, false},
		{"add", api.SkillOverrideSpec{Add: []string{"npm:extra", "diagram-tools"}}, true},
		{"remove a Team skill", api.SkillOverrideSpec{Remove: []string{"pdf-tools"}}, true},
		{"remove an unselected skill", api.SkillOverrideSpec{Remove: []string{"diagram-tools"}}, true},
	} {
		for _, marketplace := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/marketplace=%t", test.name, marketplace), func(t *testing.T) {
				err := applySkillValues(map[string]any{}, team, test.overrides, "", marketplace)
				if !marketplace || !test.refused {
					if err != nil {
						t.Fatalf("the override was refused: %v", err)
					}
					return
				}
				if !errors.Is(err, errSkillMarketplaceOverride) || err.Error() != skillMarketplaceOverrideMessage {
					t.Fatalf("err = %v, want %q", err, skillMarketplaceOverrideMessage)
				}
			})
		}
	}
}

// The refusal reaches the Agent like its other configuration errors: Failed,
// with AgentConfigInvalid and the reason, before the chart is prepared.
// Without the marketplace, the same override does not stop the pass there.
func TestSkillMarketplaceOverrideFailsAgentConfiguration(t *testing.T) {
	for _, marketplace := range []bool{false, true} {
		t.Run(fmt.Sprintf("marketplace=%t", marketplace), func(t *testing.T) {
			r, agent, _ := legacyGatewayFixture(t)
			r.SkillMarketplace = marketplace
			// End the pass at chart preparation; the chart is not under test.
			stop := errors.New("synthetic stop at chart preparation")
			chart := &failingPrepareHelm{err: stop}
			r.HelmClient = chart
			ctx := context.Background()
			key := client.ObjectKeyFromObject(agent)
			current := &api.Agent{}
			if err := r.Get(ctx, key, current); err != nil {
				t.Fatal(err)
			}
			current.Spec.SkillOverrides.Add = []string{"pdf-tools"}
			if err := r.Update(ctx, current); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err == nil {
				t.Fatal("the configuration error was not reported")
			}
			if err := r.Get(ctx, key, current); err != nil {
				t.Fatal(err)
			}
			want, preparations := stop.Error(), 1
			if marketplace {
				want, preparations = skillMarketplaceOverrideMessage, 0
			}
			condition := meta.FindStatusCondition(current.Status.Conditions, api.ConditionAgentConfigReady)
			if current.Status.Phase != api.PhaseFailed || condition == nil || condition.Reason != "AgentConfigInvalid" ||
				condition.Message != want || chart.prepares != preparations {
				t.Fatalf("phase = %q, AgentConfigReady = %+v, chart preparations = %d; want Failed with %q after %d",
					current.Status.Phase, condition, chart.prepares, want, preparations)
			}
		})
	}
}

// The switch also reaches the values a built-in Agent's release is installed
// with, rendered by the real agent chart.
func TestSkillMarketplaceInstalledReleaseValues(t *testing.T) {
	for _, marketplace := range []bool{false, true} {
		t.Run(fmt.Sprintf("marketplace=%t", marketplace), func(t *testing.T) {
			r, agent, _ := legacyGatewayFixture(t)
			r.SkillMarketplace = marketplace
			chart := &chartHelm{}
			r.HelmClient = chart
			ctx := context.Background()
			team := &api.Team{}
			if err := r.Get(ctx, client.ObjectKey{Name: agent.Spec.Team}, team); err != nil {
				t.Fatal(err)
			}
			team.Spec.Skills = []string{"npm:@openclaw/matrix", "pdf-tools"}
			if err := r.Update(ctx, team); err != nil {
				t.Fatal(err)
			}
			for pass := 0; pass < 4 && chart.installs == 0; pass++ {
				if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(agent)}); err != nil {
					t.Fatalf("reconcile %d: %v", pass, err)
				}
			}
			if chart.installs != 1 {
				t.Fatalf("installs = %d, want 1", chart.installs)
			}
			// Installed values include the chart's defaults, an empty clawHub list.
			want := []any{"pdf-tools"}
			if marketplace {
				want = []any{}
			}
			skills := chart.lastValues["skills"].(map[string]any)
			if !reflect.DeepEqual(skills["clawHub"], want) || !reflect.DeepEqual(skills["npm"], []any{"@openclaw/matrix"}) {
				t.Fatalf("installed skills = %#v", skills)
			}
		})
	}
}

// A policy-bound native Agent, the marketplace's scope, is refused the same
// way. The add is one the AgentPolicy allows, so only the override is wrong;
// the running workload and the Agent's key are kept.
func TestSkillMarketplaceOverrideFailsNativeAgentConfiguration(t *testing.T) {
	for _, test := range []struct {
		name      string
		overrides api.SkillOverrideSpec
	}{
		{"add", api.SkillOverrideSpec{Add: []string{"example-skill"}}},
		{"remove", api.SkillOverrideSpec{Remove: []string{"example-skill"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			f := newSandboxRuntimeFixtureForAgent(t, true, nil, true, "openclaw", true, false)
			f.r.SkillMarketplace = true
			team := &api.Team{}
			if err := f.kube.Get(ctx, client.ObjectKey{Name: f.agent.Spec.Team}, team); err != nil {
				t.Fatal(err)
			}
			policy := &api.AgentPolicy{}
			if err := f.kube.Get(ctx, client.ObjectKey{Name: team.Spec.AgentPolicyRef}, policy); err != nil {
				t.Fatal(err)
			}
			policy.Spec.Skills.Allowed = []string{"example-skill"}
			policy.Generation++
			if err := f.kube.Update(ctx, policy); err != nil {
				t.Fatal(err)
			}
			policy.Status.ObservedGeneration = policy.Generation
			for i := range policy.Status.Conditions {
				policy.Status.Conditions[i].ObservedGeneration = policy.Generation
			}
			if err := f.kube.Status().Update(ctx, policy); err != nil {
				t.Fatal(err)
			}
			f.reconcileOK(t)
			before := f.current(t)
			keyID, replicas := before.Status.AIGateway.KeyID, f.workload(t).Spec.Replicas
			if keyID == "" || replicas == nil || *replicas != 1 {
				t.Fatalf("fixture Agent is not running with a key: key %q, replicas %v", keyID, replicas)
			}
			before.Spec.SkillOverrides = test.overrides
			before.Generation++
			if err := f.kube.Update(ctx, before); err != nil {
				t.Fatal(err)
			}
			if _, err := f.reconcile(); err == nil {
				t.Fatal("the configuration error was not reported")
			}
			current := f.current(t)
			condition := meta.FindStatusCondition(current.Status.Conditions, api.ConditionAgentConfigReady)
			if current.Status.Phase != api.PhaseFailed || condition == nil || condition.Reason != "AgentConfigInvalid" ||
				condition.Message != skillMarketplaceOverrideMessage {
				t.Fatalf("phase = %q, AgentConfigReady = %+v; want Failed with AgentConfigInvalid and %q",
					current.Status.Phase, condition, skillMarketplaceOverrideMessage)
			}
			if current.Status.AIGateway == nil || current.Status.AIGateway.KeyID != keyID || *f.workload(t).Spec.Replicas != 1 {
				t.Fatal("the configuration error revoked the key or stopped the workload")
			}
		})
	}
}

// Custom values never request skill installs. With the marketplace, Team
// marketplace skills are dropped instead of becoming ClawHub installs (the
// HOME initializer copies them from the Team's release), npm packages still
// wait for materialization, and overrides are refused.
func TestSkillMarketplaceCustomSkills(t *testing.T) {
	instance := &api.Agent{Spec: api.AgentSpec{Agent: api.HarnessSpec{Type: api.AgentTypeCustom}}}
	team := &api.Team{Spec: api.TeamSpec{Skills: []string{"pdf-tools"}}}
	if _, err := (&AgentReconciler{}).customOverrides(instance, team); !errors.Is(err, runtimebindings.ErrSkillsMaterializationPending) {
		t.Fatalf("without the marketplace a Team skill must wait for materialization: %v", err)
	}
	r := &AgentReconciler{SkillMarketplace: true}
	values, err := r.customOverrides(instance, team)
	if err != nil || values["skills"] != nil {
		t.Fatalf("a marketplace skill reached Custom values: %#v, %v", values["skills"], err)
	}
	team.Spec.Skills = append(team.Spec.Skills, "npm:example-skill")
	if _, err := r.customOverrides(instance, team); !errors.Is(err, runtimebindings.ErrSkillsMaterializationPending) {
		t.Fatalf("an npm Team skill no longer waits for materialization: %v", err)
	}
	instance.Spec.SkillOverrides.Remove = []string{"pdf-tools"}
	if _, err := r.customOverrides(instance, team); !errors.Is(err, errSkillMarketplaceOverride) {
		t.Fatalf("a Custom override of a marketplace skill was not refused: %v", err)
	}
}

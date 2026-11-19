// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
)

// errSkillMarketplaceOverride is the configuration error of an Agent whose
// skillOverrides add or remove a Skill Marketplace skill. The marketplace
// publishes each Team's selection, so the selection belongs on the Team.
var errSkillMarketplaceOverride = errors.New("skillOverrides cannot add or remove Skill Marketplace skills; select them on the Team")

// errSkillMarketplaceOff is the configuration error of an Agent of a
// policy-bound Team that selects skills while the Skill Marketplace is off.
var errSkillMarketplaceOff = errors.New("the Skill Marketplace is off")

// skillsWithoutMarketplace refuses the selection of a policy-bound Team that
// names skills while the Skill Marketplace is off. A Sandbox Agent cannot
// install skills at its start: it receives them only from the Skill
// Marketplace, so the Agent would fail later with a less helpful message. The
// reason keeps the handling of skills that are not materialized.
func skillsWithoutMarketplace(team *api.Team, overrides api.SkillOverrideSpec, marketplace bool) error {
	if marketplace || team == nil || team.Spec.AgentPolicyRef == "" {
		return nil
	}
	_, named, err := classifySkills(computeEffectiveSkills(team, overrides.Add, overrides.Remove))
	if err != nil || len(named) == 0 {
		return nil
	}
	return fmt.Errorf("%w: the Team %s selects the skills %s, which Sandbox Agents receive only from the Skill Marketplace, and %w: turn it on, or remove the skills from the Team",
		runtimebindings.ErrSkillsMaterializationPending, team.Name, strings.Join(named, ", "), errSkillMarketplaceOff)
}

// sandboxSkillsMode selects the empty skills source for a native Agent without
// a shared skills storage selection whose effective selection is empty, and the
// skills claim otherwise. A global catalog does not require every Team to publish. With the Skill Marketplace, the claim holds the subtree of every
// Team, and the Agent's HOME initializer copies its Team's release from it.
func sandboxSkillsMode(native bool, agent *api.Agent, team *api.Team) sandboxworkload.SkillsMode {
	selection := computeEffectiveSkills(team, agent.Spec.SkillOverrides.Add, agent.Spec.SkillOverrides.Remove)
	if native && len(selection) == 0 && (team == nil || team.Spec.SkillStorage == nil) {
		return sandboxworkload.SkillsModeEmpty
	}
	return ""
}

// applySkillValues keeps global, Team and per-Agent skill selection independent
// of the Agent runtime. Package materialization remains runtime-specific.
//
// With the Skill Marketplace (marketplace true), every entry without the npm:
// prefix names a marketplace skill, which the marketplace delivers for the
// Team: such entries never reach the reserved clawHub list, and an Agent's
// skillOverrides may not add or remove them. npm entries are unchanged.
func applySkillValues(values map[string]any, team *api.Team, overrides api.SkillOverrideSpec, storageAccount string, marketplace bool) error {
	if marketplace && (slices.ContainsFunc(overrides.Add, api.IsSkillMarketplaceReference) ||
		slices.ContainsFunc(overrides.Remove, api.IsSkillMarketplaceReference)) {
		return errSkillMarketplaceOverride
	}
	if team != nil && team.Spec.SkillStorage != nil {
		s := team.Spec.SkillStorage
		storage := map[string]any{
			"shared": true, "resourceGroup": s.ResourceGroup,
			"storageAccount": s.StorageAccount, "containerName": s.ContainerName,
		}
		for key, value := range map[string]string{
			"storageClassName": s.StorageClassName, "storageSize": s.StorageSize, "mountPath": s.MountPath,
		} {
			if value != "" {
				storage[key] = value
			}
		}
		values["skills"] = deepMerge(values["skills"], storage)
	}
	effective := computeEffectiveSkills(team, overrides.Add, overrides.Remove)
	if len(effective) > 0 {
		npm, clawHub, err := classifySkills(effective)
		if err != nil {
			return fmt.Errorf("classify skills: %w", err)
		}
		if marketplace {
			clawHub = nil
		}
		skills := map[string]any{}
		if len(npm) > 0 {
			skills["npm"] = stringsToInterfaces(npm)
		}
		if len(clawHub) > 0 {
			skills["clawHub"] = stringsToInterfaces(clawHub)
		}
		if len(skills) > 0 {
			values["skills"] = deepMerge(values["skills"], skills)
		}
	}
	if storageAccount != "" {
		values["skills"] = deepMerge(values["skills"], map[string]any{"storageAccount": storageAccount})
	}
	return nil
}

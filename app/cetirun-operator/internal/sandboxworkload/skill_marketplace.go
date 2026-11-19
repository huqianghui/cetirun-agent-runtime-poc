// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxworkload

import (
	"fmt"
	"regexp"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
)

const (
	// SkillMarketplaceMountPath is where the HOME initializer reads the Team's
	// subtree of a Skill Marketplace skills claim.
	SkillMarketplaceMountPath = "/etc/cetirun-shared-skills"
	// SkillMarketplaceSelectionEnv gives the HOME initializer the Team
	// selection digest that the current release must carry.
	SkillMarketplaceSelectionEnv = "CETIRUN_SKILLS_RELEASE_SELECTION"
)

var (
	// The Agent chart accepts the same subtree form: one path segment below
	// the teams directory.
	skillMarketplaceTeamUID   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	skillMarketplaceSelection = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

// SkillMarketplaceInput selects the Skill Marketplace release of the Agent's
// Team on its skills claim. The claim holds every Team's subtree: TeamPath is
// the subtree of the bound Team (api.SkillMarketplaceTeamPath), and
// SelectionDigest is the Team selection digest that the release must carry
// (api.SkillMarketplaceSelectionDigest). Only a common-executor Agent with a
// skills claim uses it.
type SkillMarketplaceInput struct {
	TeamPath        string
	SelectionDigest string
}

func validateSkillMarketplace(input Input) error {
	marketplace := input.SkillMarketplace
	if marketplace == nil {
		return nil
	}
	if !input.Reconciled || input.SkillsMode == SkillsModeEmpty {
		return fmt.Errorf("Skill Marketplace skills require a common-executor Agent with a skills claim")
	}
	team := string(input.Resolution.Binding.TeamUID)
	if !skillMarketplaceTeamUID.MatchString(team) || marketplace.TeamPath != api.SkillMarketplaceTeamPath(team) {
		return fmt.Errorf("Skill Marketplace skills require the bound Team's subtree of the skills claim")
	}
	if !skillMarketplaceSelection.MatchString(marketplace.SelectionDigest) {
		return fmt.Errorf("Skill Marketplace skills require a sha256 Team selection digest")
	}
	return nil
}

// validateSkillMarketplaceTemplate requires the rendered Pod to read only the
// Team's subtree of the skills claim, in the HOME initializer, and to give it
// the Team's selection. An Agent chart that ignored these values would mount
// the whole claim, with the subtree of every Team.
func (b *Bundle) validateSkillMarketplaceTemplate(spec corev1.PodSpec) error {
	marketplace := b.input.SkillMarketplace
	if marketplace == nil {
		return nil
	}
	volume := b.input.SkillsMode.volume(skillsIdentity(b.input.Skills))
	volumes := 0
	for _, actual := range spec.Volumes {
		if actual.Name == volume.Name {
			if !equality.Semantic.DeepEqual(actual, volume) {
				return fmt.Errorf("Skill Marketplace skills require the read-only skills claim")
			}
			volumes++
		}
	}
	mount := corev1.VolumeMount{Name: volume.Name, MountPath: SkillMarketplaceMountPath, SubPath: marketplace.TeamPath, ReadOnly: true}
	mounts, selections := 0, 0
	for group, containers := range [][]corev1.Container{spec.Containers, spec.InitContainers} {
		for _, container := range containers {
			initializer := group == 1 && container.Name == "init-home-data"
			for _, actual := range container.VolumeMounts {
				if actual.Name != mount.Name {
					continue
				}
				if !initializer || !equality.Semantic.DeepEqual(actual, mount) {
					return fmt.Errorf("Skill Marketplace skills may only be mounted by the HOME initializer, read-only and limited to the Team's subtree")
				}
				mounts++
			}
			for _, env := range container.Env {
				if env.Name != SkillMarketplaceSelectionEnv {
					continue
				}
				if !initializer || env.Value != marketplace.SelectionDigest || env.ValueFrom != nil {
					return fmt.Errorf("only the HOME initializer receives the Team's Skill Marketplace selection")
				}
				selections++
			}
		}
	}
	if volumes != 1 || mounts != 1 || selections != 1 {
		return fmt.Errorf("the Agent chart does not render the Team's Skill Marketplace subtree and selection")
	}
	return nil
}

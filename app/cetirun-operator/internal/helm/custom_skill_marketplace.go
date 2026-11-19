// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"errors"
	"fmt"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"helm.sh/helm/v3/pkg/chart"
)

// customSkillMarketplaceAnnotation marks an Agent chart whose Custom runtime
// copies the release of the Team's Skill Marketplace selection.
const customSkillMarketplaceAnnotation = "cetirun.microsoft.com/custom-skill-marketplace"

// ErrCustomSkillMarketplaceUnsupported reports an Agent chart that cannot give
// a Custom Agent its Team's Skill Marketplace skills. An earlier chart refuses
// the Skill Marketplace values of a Custom Agent.
var ErrCustomSkillMarketplaceUnsupported = errors.New("does not deliver Skill Marketplace skills to Custom Agents")

// checkCustomSkillMarketplace refuses an Agent chart without the annotation
// for a Custom Agent that receives Skill Marketplace skills, before rendering.
func checkCustomSkillMarketplace(chrt *chart.Chart, input sandboxworkload.Input) error {
	if input.AgentType != "custom" || input.SkillMarketplace == nil ||
		chrt.Metadata.Annotations[customSkillMarketplaceAnnotation] == "v1" {
		return nil
	}
	return fmt.Errorf("Agent chart %s %w; upgrade the Agent to receive the skills of its Team",
		chrt.Metadata.Version, ErrCustomSkillMarketplaceUnsupported)
}

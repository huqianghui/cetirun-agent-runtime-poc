// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package handler

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	cetirunv1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
)

// TeamModelRouterDTO is one smart-routing model of a Team, as administrators
// edit it: the name Agents request, the classifier and the model of each tier.
// A tier names a shared model, or copilot/<model> for a GitHub Copilot model.
// ClassifierModel is the shared model of the llm classifier, or the shared
// decision model of the jev classifier.
type TeamModelRouterDTO struct {
	Name            string `json:"name"`
	Classifier      string `json:"classifier,omitempty"`
	ClassifierModel string `json:"classifierModel,omitempty"`
	Simple          string `json:"simple"`
	Medium          string `json:"medium"`
	Complex         string `json:"complex"`
	Reasoning       string `json:"reasoning,omitempty"`
}

// TeamModelRouterStatusDTO is the operator's report on one router.
type TeamModelRouterStatusDTO struct {
	Name    string `json:"name"`
	Ready   bool   `json:"ready"`
	Message string `json:"message,omitempty"`
}

const maxTeamModelRouters = 10

var (
	modelRouterNamePattern  = regexp.MustCompile(`^[^\s/]+$`)
	modelRouterModelPattern = regexp.MustCompile(`^[^\s]+$`)
)

func toTeamModelRouterDTOs(team *cetirunv1alpha1.Team) ([]TeamModelRouterDTO, []TeamModelRouterStatusDTO) {
	var routers []TeamModelRouterDTO
	for _, router := range team.Spec.ModelRouters {
		routers = append(routers, TeamModelRouterDTO{
			Name: router.Name, Classifier: router.Classifier.Type, ClassifierModel: router.Classifier.Model,
			Simple: router.Tiers.Simple, Medium: router.Tiers.Medium, Complex: router.Tiers.Complex, Reasoning: router.Tiers.Reasoning,
		})
	}
	var statuses []TeamModelRouterStatusDTO
	for _, status := range team.Status.ModelRouters {
		statuses = append(statuses, TeamModelRouterStatusDTO{Name: status.Name, Ready: status.Ready, Message: status.Message})
	}
	return routers, statuses
}

func toTeamModelRouters(routers []TeamModelRouterDTO) []cetirunv1alpha1.TeamModelRouter {
	var out []cetirunv1alpha1.TeamModelRouter
	for _, router := range routers {
		classifier := strings.TrimSpace(router.Classifier)
		if classifier == "" {
			classifier = cetirunv1alpha1.ModelRouterClassifierHeuristic
		}
		model := ""
		if classifier == cetirunv1alpha1.ModelRouterClassifierLLM || classifier == cetirunv1alpha1.ModelRouterClassifierJev {
			model = strings.TrimSpace(router.ClassifierModel)
		}
		out = append(out, cetirunv1alpha1.TeamModelRouter{
			Name:       strings.TrimSpace(router.Name),
			Classifier: cetirunv1alpha1.TeamModelRouterClassifier{Type: classifier, Model: model},
			Tiers: cetirunv1alpha1.TeamModelRouterTiers{
				Simple: strings.TrimSpace(router.Simple), Medium: strings.TrimSpace(router.Medium),
				Complex: strings.TrimSpace(router.Complex), Reasoning: strings.TrimSpace(router.Reasoning),
			},
		})
	}
	return out
}

// validateTeamModelRouters mirrors the Team CRD's checks with messages that
// name the router. Whether the models exist and an Agent may use them is the
// operator's to check; it reports that on the Team and on each Agent.
func validateTeamModelRouters(routers []TeamModelRouterDTO) string {
	if len(routers) > maxTeamModelRouters {
		return fmt.Sprintf("A Team can define at most %d smart-routing models", maxTeamModelRouters)
	}
	seen := map[string]bool{}
	validModel := func(model string, required bool) bool {
		model = strings.TrimSpace(model)
		if model == "" {
			return !required
		}
		return utf8.RuneCountInString(model) <= 256 && modelRouterModelPattern.MatchString(model)
	}
	for _, router := range routers {
		name := strings.TrimSpace(router.Name)
		switch {
		case name == "" || utf8.RuneCountInString(name) > 64 || !modelRouterNamePattern.MatchString(name):
			return "Smart-routing model names need 1 to 64 characters without spaces or slashes"
		case strings.HasPrefix(strings.ToLower(name), "cetirun-"):
			return fmt.Sprintf("Smart-routing model %s: names starting with cetirun- are reserved", name)
		case seen[name]:
			return fmt.Sprintf("Smart-routing model %s is defined twice", name)
		}
		seen[name] = true
		classifier := strings.TrimSpace(router.Classifier)
		classifierModel := strings.TrimSpace(router.ClassifierModel)
		usableClassifierModel := validModel(classifierModel, true) && !strings.HasPrefix(classifierModel, cetirunv1alpha1.AIGatewayCopilotModelPrefix)
		switch classifier {
		case "", cetirunv1alpha1.ModelRouterClassifierHeuristic:
		case cetirunv1alpha1.ModelRouterClassifierLLM:
			if !usableClassifierModel {
				return fmt.Sprintf("Smart-routing model %s: the model classifier needs a shared model", name)
			}
		case cetirunv1alpha1.ModelRouterClassifierJev:
			if !usableClassifierModel {
				return fmt.Sprintf("Smart-routing model %s: the decision classifier needs a shared decision model", name)
			}
		default:
			return fmt.Sprintf("Smart-routing model %s: the classifier must be heuristic, llm or jev", name)
		}
		for _, tier := range []struct {
			name, model string
			required    bool
		}{{"simple", router.Simple, true}, {"medium", router.Medium, true}, {"complex", router.Complex, true}, {"reasoning", router.Reasoning, false}} {
			if !validModel(tier.model, tier.required) {
				return fmt.Sprintf("Smart-routing model %s: the %s tier needs a model name without spaces", name, tier.name)
			}
		}
	}
	return ""
}

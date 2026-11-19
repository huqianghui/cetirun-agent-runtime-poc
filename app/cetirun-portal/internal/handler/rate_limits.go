// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package handler

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	cetirunv1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
)

// Minimum AI Gateway rate limits, as in the AgentPolicy CRD. The Gateway
// rejects every request of a key with a zero limit, so an empty field means
// no limit and is never sent as zero.
const (
	minRateLimitRequests int64 = 1
	minRateLimitTokens   int64 = 4096
	maxRateLimitModels         = 64
	maxRateLimitModelLen       = 256
)

// AgentPolicyRateLimitsDTO are the per-Agent AI Gateway limits of a policy.
// Every Agent of a Team using the policy gets the same limits on its own key.
type AgentPolicyRateLimitsDTO struct {
	MaxParallelRequests *int64                                  `json:"maxParallelRequests,omitempty"`
	RPM                 *int64                                  `json:"rpm,omitempty"`
	TPM                 *int64                                  `json:"tpm,omitempty"`
	PerModel            map[string]AgentPolicyModelRateLimitDTO `json:"perModel,omitempty"`
}

// AgentPolicyModelRateLimitDTO limits one model, by its Gateway model name.
type AgentPolicyModelRateLimitDTO struct {
	RPM *int64 `json:"rpm,omitempty"`
	TPM *int64 `json:"tpm,omitempty"`
}

func copyLimit(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func toAgentPolicyRateLimitsDTO(limits *cetirunv1alpha1.AgentPolicyRateLimits) *AgentPolicyRateLimitsDTO {
	if limits == nil {
		return nil
	}
	dto := &AgentPolicyRateLimitsDTO{
		MaxParallelRequests: copyLimit(limits.MaxParallelRequests), RPM: copyLimit(limits.RPM), TPM: copyLimit(limits.TPM),
	}
	for model, value := range limits.PerModel {
		if dto.PerModel == nil {
			dto.PerModel = map[string]AgentPolicyModelRateLimitDTO{}
		}
		dto.PerModel[model] = AgentPolicyModelRateLimitDTO{RPM: copyLimit(value.RPM), TPM: copyLimit(value.TPM)}
	}
	return dto
}

// toAgentPolicyRateLimits returns nil when no limit is set, so a policy saved
// without limits carries no empty rateLimits object.
func toAgentPolicyRateLimits(dto *AgentPolicyRateLimitsDTO) *cetirunv1alpha1.AgentPolicyRateLimits {
	if dto == nil || (dto.MaxParallelRequests == nil && dto.RPM == nil && dto.TPM == nil && len(dto.PerModel) == 0) {
		return nil
	}
	limits := &cetirunv1alpha1.AgentPolicyRateLimits{
		MaxParallelRequests: copyLimit(dto.MaxParallelRequests), RPM: copyLimit(dto.RPM), TPM: copyLimit(dto.TPM),
	}
	for model, value := range dto.PerModel {
		if limits.PerModel == nil {
			limits.PerModel = map[string]cetirunv1alpha1.AgentPolicyModelRateLimit{}
		}
		limits.PerModel[model] = cetirunv1alpha1.AgentPolicyModelRateLimit{RPM: copyLimit(value.RPM), TPM: copyLimit(value.TPM)}
	}
	return limits
}

// maxRateLimitParallel is the CRD's maximum for maxParallelRequests, the
// largest value the Gateway stores (a 32-bit integer).
const maxRateLimitParallel = 2147483647

// validateAgentPolicyRateLimits applies the CRD's rules, so the administrator
// gets a message before the API server rejects the write.
func validateAgentPolicyRateLimits(limits *AgentPolicyRateLimitsDTO) string {
	if limits == nil {
		return ""
	}
	check := func(name string, value *int64, minimum int64) string {
		if value != nil && *value < minimum {
			return fmt.Sprintf("rateLimits.%s must be at least %d; leave it empty for no limit", name, minimum)
		}
		return ""
	}
	if value := limits.MaxParallelRequests; value != nil && *value > maxRateLimitParallel {
		return fmt.Sprintf("rateLimits.maxParallelRequests must be at most %d", maxRateLimitParallel)
	}
	for _, message := range []string{
		check("maxParallelRequests", limits.MaxParallelRequests, minRateLimitRequests),
		check("rpm", limits.RPM, minRateLimitRequests),
		check("tpm", limits.TPM, minRateLimitTokens),
	} {
		if message != "" {
			return message
		}
	}
	if len(limits.PerModel) > maxRateLimitModels {
		return fmt.Sprintf("rateLimits.perModel may limit at most %d models", maxRateLimitModels)
	}
	for _, model := range slices.Sorted(maps.Keys(limits.PerModel)) {
		value := limits.PerModel[model]
		if model == "" || utf8.RuneCountInString(model) > maxRateLimitModelLen || strings.ContainsAny(model, " \t\r\n\v\f") {
			return "rateLimits.perModel names must be Gateway model names of 1 to 256 characters without whitespace"
		}
		if value.RPM == nil && value.TPM == nil {
			return "rateLimits.perModel." + model + " must set rpm, tpm, or both"
		}
		for _, message := range []string{
			check("perModel."+model+".rpm", value.RPM, minRateLimitRequests),
			check("perModel."+model+".tpm", value.TPM, minRateLimitTokens),
		} {
			if message != "" {
				return message
			}
		}
	}
	return ""
}

// policyRateLimits returns the AI Gateway limits of the policy that governs
// the Agent, or nil when its Team has no policy or none can be read. It is
// shown to the owner; the operator applies the same values to the Agent's key.
func (h *InstanceHandler) policyRateLimits(ctx context.Context, instance *cetirunv1alpha1.Agent) *AgentPolicyRateLimitsDTO {
	if instance.Spec.Team == "" {
		return nil
	}
	team, err := h.Kube.GetTeam(ctx, instance.Spec.Team)
	if err != nil || !team.Spec.PolicyConfigured() || team.Spec.AgentPolicyRef == "" {
		if err != nil {
			slog.Debug("Agent rate limits unavailable: Team not readable", "team", instance.Spec.Team, "error", err)
		}
		return nil
	}
	policy, err := h.Kube.GetAgentPolicy(ctx, team.Spec.AgentPolicyRef)
	if err != nil {
		slog.Debug("Agent rate limits unavailable: AgentPolicy not readable", "policy", team.Spec.AgentPolicyRef, "error", err)
		return nil
	}
	return toAgentPolicyRateLimitsDTO(policy.Spec.ModelAccess.RateLimits)
}

// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"
	"maps"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"

	cetirunv1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/aigateway"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
)

// policyAIGatewayKeyLimits returns the request limits the policy sets on the
// virtual key of every Agent that uses it. The limits are deliberately not
// part of the Gateway policy revision: changing them updates each key in
// place and does not start a policy reprojection.
func policyAIGatewayKeyLimits(policy *cetirunv1alpha1.AgentPolicy) (aigateway.KeyLimits, error) {
	if policy == nil || policy.Spec.ModelAccess.RateLimits == nil {
		return aigateway.KeyLimits{}, nil
	}
	source := policy.Spec.ModelAccess.RateLimits
	if errs := sandbox.ValidatePolicyRateLimits(source, field.NewPath("spec", "modelAccess", "rateLimits")); len(errs) != 0 {
		return aigateway.KeyLimits{}, fmt.Errorf("policy rate limits are invalid: %w", errs.ToAggregate())
	}
	limits := aigateway.KeyLimits{
		MaxParallelRequests: cloneLimit(source.MaxParallelRequests), RPM: cloneLimit(source.RPM), TPM: cloneLimit(source.TPM),
	}
	for model, value := range source.PerModel {
		if value.RPM != nil {
			if limits.ModelRPM == nil {
				limits.ModelRPM = map[string]int64{}
			}
			limits.ModelRPM[model] = *value.RPM
		}
		if value.TPM != nil {
			if limits.ModelTPM == nil {
				limits.ModelTPM = map[string]int64{}
			}
			limits.ModelTPM[model] = *value.TPM
		}
	}
	return limits, nil
}

func cloneLimit(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

// withTeamRouterModelLimits copies a per-model limit the policy sets for a
// router of the Agent's Team, by the router's name, to the router's internal
// name. The Gateway counts a router's requests under its internal name, also
// those the Agent sends by the router's alias, since LiteLLM counts a request
// after it resolves the alias. The limit also stays under the router's name:
// a key without that alias can reach another model by the name, such as one
// of the Agent's GitHub Copilot models, which the limit must still restrict.
// The alias is decided later in the same pass, after the Copilot routes are
// registered, so neither copy depends on it. Where the policy also limits the
// internal name, the smaller value is kept. A name that is also a shared
// model stays the shared model's only: such a router is misconfigured and
// never granted.
func (r *AgentReconciler) withTeamRouterModelLimits(limits aigateway.KeyLimits, team *cetirunv1alpha1.Team) aigateway.KeyLimits {
	if team == nil || len(team.Spec.ModelRouters) == 0 || (len(limits.ModelRPM) == 0 && len(limits.ModelTPM) == 0) {
		return limits
	}
	shared := r.aiGatewayCatalog.sharedRouters().shared
	internal := map[string]string{}
	for _, router := range team.Spec.ModelRouters {
		if !shared[router.Name] {
			internal[router.Name] = modelRouterGatewayName(team.Name, router.Name)
		}
	}
	limits.ModelRPM = withCopiedModelLimits(limits.ModelRPM, internal)
	limits.ModelTPM = withCopiedModelLimits(limits.ModelTPM, internal)
	return limits
}

// withCopiedModelLimits returns values with the limit of each model that
// names maps also set under the mapped name. Where the mapped name has a limit
// already, the smaller value is kept. values is not changed.
func withCopiedModelLimits(values map[string]int64, names map[string]string) map[string]int64 {
	if len(values) == 0 {
		return values
	}
	copied := maps.Clone(values)
	for model, value := range values {
		name, ok := names[model]
		if !ok {
			continue
		}
		if current, ok := copied[name]; ok && current <= value {
			continue
		}
		copied[name] = value
	}
	return copied
}

// reconcileAIGatewayKeyLimits brings an existing policy-bound key's request
// limits to the policy's values without replacing or blocking the key, so its
// spend and other settings are kept. Legacy keys are never changed: an
// administrator may manage their limits in the Gateway.
func (r *AgentReconciler) reconcileAIGatewayKeyLimits(ctx context.Context, client aiGatewayKeyClient, instance *cetirunv1alpha1.Agent,
	token string, key *aigateway.Key, grant aiGatewayGrant,
) error {
	if aiGatewayKeyLimitsMatch(key, grant) {
		return nil
	}
	if err := client.UpdateKeyLimits(ctx, token, grant.limits); err != nil {
		return err
	}
	r.EventRecorder.Eventf(instance, corev1.EventTypeNormal, "AIGatewayKeyLimitsUpdated",
		"Updated AI Gateway virtual key %s request limits from its AgentPolicy", aiGatewayKeyAlias(instance))
	return nil
}

// aiGatewayKeyLimitsMatch is the read-back check for a key's request limits.
// Only policy-bound keys carry policy limits.
func aiGatewayKeyLimitsMatch(key *aigateway.Key, grant aiGatewayGrant) bool {
	return !grant.policyBound || key.Limits.Equal(grant.limits)
}

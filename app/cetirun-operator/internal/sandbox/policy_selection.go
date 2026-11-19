// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandbox

import (
	"slices"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
)

const (
	HoldFinalizer        = api.SandboxHoldFinalizer
	PreparationFinalizer = api.SandboxPreparationFinalizer
	BootstrapFinalizer   = api.SandboxBootstrapFinalizer
)

// Requested detects policy assignment intent, not a valid or enforced binding.
func Requested(agent *api.Agent, team *api.Team) bool {
	if agent != nil && (slices.Contains(agent.Finalizers, HoldFinalizer) ||
		slices.Contains(agent.Finalizers, PreparationFinalizer) || slices.Contains(agent.Finalizers, BootstrapFinalizer) ||
		agent.Spec.SandboxRef != "" ||
		(agent.Spec.ModelAccess != nil && agent.Spec.ModelAccess.MonthlyBudgetUSD != "") ||
		agent.Status.Policy != nil ||
		(agent.Status.AIGateway != nil && agent.Status.AIGateway.PolicyRevision != "") ||
		agent.Status.Sandbox != nil || meta.FindStatusCondition(agent.Status.Conditions, api.ConditionSandboxReady) != nil) {
		return true
	}
	return TeamPolicyConfigured(team)
}

func TeamPolicyConfigured(team *api.Team) bool {
	return team != nil && team.Spec.PolicyConfigured()
}

// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package handler

import (
	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GovernanceStatus is the current, user-visible reason an Agent is held by
// policy, identity, platform, or dependency governance. It deliberately
// contains only the condition fields already exposed by the Agent responses.
type GovernanceStatus struct {
	Category           string `json:"category"`
	Reason             string `json:"reason"`
	Message            string `json:"message"`
	Condition          string `json:"condition"`
	ObservedGeneration int64  `json:"observedGeneration"`
}

// governanceStatus returns only an observation for the current spec. A prior
// denial must not be presented as a current block after an Agent update.
func governanceStatus(instance *api.Agent) *GovernanceStatus {
	if instance == nil || instance.Generation <= 0 || !instance.DeletionTimestamp.IsZero() || instance.Spec.Suspended {
		return nil
	}

	var selected *metav1.Condition
	selectedPriority := 0
	selectedGeneration := int64(0)
	for i := range instance.Status.Conditions {
		condition := &instance.Status.Conditions[i]
		observedGeneration, current := currentGovernanceObservation(instance, condition)
		if !current {
			continue
		}
		priority := governancePriority(condition)
		if priority == 0 || priority >= selectedPriority && selected != nil {
			continue
		}
		selected, selectedPriority, selectedGeneration = condition, priority, observedGeneration
	}
	if selected == nil {
		return nil
	}
	return &GovernanceStatus{
		Category:           governanceCategory(selected),
		Reason:             selected.Reason,
		Message:            governanceMessage(selected),
		Condition:          selected.Type,
		ObservedGeneration: selectedGeneration,
	}
}

// governanceMessage uses fixed operator-facing text for the list summary. A
// PolicyResolved denial is the exception: it is the controller's validated,
// user-actionable policy explanation and is more useful than the generic hold.
func governanceMessage(condition *metav1.Condition) string {
	if condition.Type == api.ConditionPolicyResolved && condition.Reason == "PolicyDenied" && condition.Message != "" {
		return condition.Message
	}
	if condition.Type == api.ConditionAgentIdentityReady {
		switch condition.Reason {
		case "OwnerBindingMissing", "OwnerTenantMismatch", "DeploymentDisabled":
			// Use the more specific, stable messages below.
		default:
			if condition.Reason == "Provisioning" {
				return "Agent Identity provisioning is in progress."
			}
			return "Agent Identity is not ready."
		}
	}
	switch condition.Reason {
	case "PolicyDenied":
		return "The Agent configuration is not allowed by the selected policy."
	case "OwnerBindingMissing":
		return "The trusted Agent Identity owner binding is missing or invalid."
	case "OwnerTenantMismatch":
		return "The Agent Identity owner tenant does not match the deployment tenant."
	case "DeploymentDisabled":
		return "Agent Identity is disabled for this deployment."
	case "UnsupportedControl":
		return "A requested Sandbox control is not qualified."
	case "RuntimeUnavailable":
		return "The selected Sandbox runtime is unavailable."
	case "DependencyUnavailable", "PrerequisitesUnavailable":
		return "A required policy, Sandbox, or platform dependency is unavailable."
	case "ObservationUnavailable":
		return "Fresh evidence for the current governance binding is unavailable."
	case "UnauthorizedBinding":
		return "The current binding or identity grant is not authorized."
	case "UnapprovedGrant":
		return "A requested identity or role grant is not approved."
	case "PolicyConflict":
		return "Policy rules conflict with required platform controls."
	case "PolicyApplyFailed":
		return "Sandbox control processing failed; execution remains held."
	default:
		return "The Operator is waiting for governance prerequisites."
	}
}

func currentGovernanceObservation(instance *api.Agent, condition *metav1.Condition) (int64, bool) {
	if condition.Status != metav1.ConditionFalse && condition.Status != metav1.ConditionUnknown {
		return 0, false
	}
	if governancePriority(condition) == 0 {
		return 0, false
	}
	if condition.ObservedGeneration != 0 {
		return condition.ObservedGeneration, condition.ObservedGeneration == instance.Generation
	}

	// The operator's setCondition does not populate ObservedGeneration. The
	// policy and identity controllers write their typed status and conditions in
	// the same status transaction, so their current substatus is sufficient
	// evidence for only these condition records with no observed generation.
	switch condition.Type {
	case api.ConditionPolicyResolved, api.ConditionPolicyApplied:
		if currentPolicyCondition(instance, condition) {
			return instance.Generation, true
		}
	case api.ConditionAgentIdentityReady:
		if currentAgentIdentityCondition(instance, condition) {
			return instance.Generation, true
		}
	}
	return 0, false
}

func currentPolicyCondition(instance *api.Agent, condition *metav1.Condition) bool {
	policy := instance.Status.Policy
	if policy == nil || policy.ObservedGeneration != instance.Generation {
		return false
	}
	switch condition.Type {
	case api.ConditionPolicyResolved:
		return (condition.Reason == "PolicyDenied" && policy.Phase == "Denied") ||
			(condition.Reason == "DependencyUnavailable" && policy.Phase == "Pending")
	case api.ConditionPolicyApplied:
		return (condition.Reason == "PolicyDenied" && policy.Phase == "Denied") ||
			((condition.Reason == "DependencyUnavailable" || condition.Reason == "EnforcementPending") && policy.Phase == "Pending")
	default:
		return false
	}
}

func currentAgentIdentityCondition(instance *api.Agent, condition *metav1.Condition) bool {
	identity := instance.Status.AgentIdentity
	if identity == nil || identity.ObservedGeneration != instance.Generation || identity.Phase == api.AgentIdentityPhaseReady {
		return false
	}
	if condition.Reason == "Provisioning" {
		return identity.Phase == api.AgentIdentityPhaseProvisioning && identity.Reason == ""
	}
	return condition.Reason != "" && condition.Reason == identity.Reason
}

func governancePriority(condition *metav1.Condition) int {
	if condition.Type == api.ConditionAgentIdentityReady {
		// The identity controller records its own current lifecycle reasons here.
		// They must not be hidden behind the later policy enforcement hold.
		if condition.Reason == "Provisioning" {
			return 70
		}
		return 20
	}
	switch condition.Reason {
	case "PolicyDenied":
		switch condition.Type {
		case api.ConditionPolicyResolved:
			return 10
		case api.ConditionPolicyApplied:
			return 11
		case api.ConditionSandboxReady:
			return 12
		case api.ConditionReady:
			return 13
		}
	case "UnsupportedControl", "RuntimeUnavailable":
		if condition.Type == api.ConditionSandboxReady {
			return 30
		}
		if condition.Type == api.ConditionReady {
			return 31
		}
	case "DependencyUnavailable", "ObservationUnavailable", "PrerequisitesUnavailable":
		switch condition.Type {
		case api.ConditionPolicyResolved:
			return 25
		case api.ConditionPolicyApplied:
			return 26
		case api.ConditionSandboxReady:
			return 27
		}
	case "UnauthorizedBinding", "UnapprovedGrant", "PolicyConflict", "PolicyApplyFailed":
		if condition.Type == api.ConditionSandboxReady {
			return 40
		}
		if condition.Type == api.ConditionReady {
			return 41
		}
	case "EnforcementPending":
		if condition.Type == api.ConditionPolicyApplied {
			return 60
		}
		if condition.Type == api.ConditionSandboxReady {
			return 61
		}
		if condition.Type == api.ConditionReady {
			return 62
		}
	}
	return 0
}

func governanceCategory(condition *metav1.Condition) string {
	if condition.Type == api.ConditionAgentIdentityReady {
		if condition.Reason == "Provisioning" {
			return "pending"
		}
		return "identity"
	}
	switch condition.Reason {
	case "PolicyDenied", "UnauthorizedBinding", "UnapprovedGrant", "PolicyConflict", "PolicyApplyFailed":
		return "policy"
	case "OwnerBindingMissing", "OwnerTenantMismatch", "DeploymentDisabled":
		return "identity"
	case "UnsupportedControl", "RuntimeUnavailable":
		return "platform"
	case "DependencyUnavailable", "ObservationUnavailable", "PrerequisitesUnavailable":
		return "dependency"
	default:
		return "pending"
	}
}

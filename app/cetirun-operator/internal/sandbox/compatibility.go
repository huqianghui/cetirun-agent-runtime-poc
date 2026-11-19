// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandbox

import (
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation/field"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
)

// TeamCompatibilityReport evaluates only persisted Team, Policy and Sandbox
// configuration. Callers remain responsible for current platform observations
// and for runtime authorization and enforcement.
func TeamCompatibilityReport(team *api.Team, resources Resources, registrations map[api.SandboxType]Registration, nativeExecution bool) api.TeamCompatibilityStatus {
	report := api.TeamCompatibilityStatus{
		State: api.CompatibilityCompatible,
		Team:  compatibilityReference(team.Name, string(team.UID), team.Generation),
	}
	if team.Spec.AgentPolicyRef != "" {
		if policy := resources.AgentPolicies[team.Spec.AgentPolicyRef]; policy != nil {
			report.Policy = compatibilityReference(policy.Name, string(policy.UID), policy.Generation)
		} else {
			report.Policy = compatibilityReference(team.Spec.AgentPolicyRef, "", 0)
		}
	}
	policy := resources.AgentPolicies[team.Spec.AgentPolicyRef]
	if !team.Spec.PolicyConfigured() {
		return report
	}
	if team.Spec.AgentPolicyRef == "" {
		report.Issues = append(report.Issues, compatibilityIssue("policy_missing", "spec.agentPolicyRef", "Choose an Agent Policy.", &report.Team, nil, nil))
	}
	if len(team.Spec.SandboxRefs) == 0 {
		report.Issues = append(report.Issues, compatibilityIssue("sandbox_missing", "spec.sandboxRefs", "Choose at least one Sandbox.", &report.Team, referencePointer(report.Policy), nil))
	}
	if team.Spec.DefaultSandboxRef == "" || !slices.Contains(team.Spec.SandboxRefs, team.Spec.DefaultSandboxRef) {
		report.Issues = append(report.Issues, compatibilityIssue("default_sandbox_invalid", "spec.defaultSandboxRef", "Choose a default Sandbox from the selected Sandboxes.", &report.Team, referencePointer(report.Policy), nil))
	}
	if policy == nil && team.Spec.AgentPolicyRef != "" {
		report.Issues = append(report.Issues, compatibilityIssue("policy_not_found", "spec.agentPolicyRef", "The selected Agent Policy was not found.", &report.Team, referencePointer(report.Policy), nil))
	}
	var policySpec api.AgentPolicySpec
	policyValid := false
	if policy != nil {
		var errs field.ErrorList
		policySpec, errs = NormalizeAgentPolicy(policy.Spec)
		if len(errs) != 0 {
			report.Issues = append(report.Issues, compatibilityIssue("policy_invalid", "spec.agentPolicyRef", "The selected Agent Policy is invalid.", &report.Team, referencePointer(report.Policy), nil))
		} else {
			policyValid = true
		}
	}
	if policyValid && len(team.Spec.SandboxRefs) > 1 && !policySpec.SandboxSelection.AllowMultiple {
		report.Issues = append(report.Issues, compatibilityIssue("multiple_sandboxes_not_allowed", "spec.sandboxRefs", "The selected Agent Policy allows one Sandbox per Team.", &report.Team, referencePointer(report.Policy), nil))
	}
	for _, name := range team.Spec.SandboxRefs {
		box := resources.Sandboxes[name]
		status := api.SandboxCompatibilityStatus{State: api.CompatibilityCompatible, Sandbox: compatibilityReference(name, "", 0)}
		if box != nil {
			status.Sandbox = compatibilityReference(box.Name, string(box.UID), box.Generation)
		}
		if box == nil {
			status.State, status.Reason = api.CompatibilityIncompatible, "SandboxNotFound"
			status.Issues = append(status.Issues, compatibilityIssue("sandbox_not_found", "spec.sandboxRefs", "The selected Sandbox was not found.", &report.Team, referencePointer(report.Policy), referencePointer(status.Sandbox)))
		} else if !policyValid {
			status.State, status.Reason = api.CompatibilityIncompatible, "PolicyInvalid"
		} else if normalized, errs := NormalizeSandbox(box.Spec); len(errs) != 0 {
			status.State, status.Reason = api.CompatibilityIncompatible, "SandboxInvalid"
			status.Issues = append(status.Issues, compatibilityIssue("sandbox_invalid", "spec.sandboxRefs", "The selected Sandbox is invalid.", &report.Team, referencePointer(report.Policy), referencePointer(status.Sandbox)))
		} else if registration, found := registrations[normalized.Type]; !found {
			status.State, status.Reason = api.CompatibilityIncompatible, "RuntimeNotRegistered"
			status.Issues = append(status.Issues, compatibilityIssue("runtime_not_registered", "spec.sandboxRefs", "The Sandbox runtime is not registered by the platform.", &report.Team, referencePointer(report.Policy), referencePointer(status.Sandbox)))
		} else {
			if err := CheckExecutionProfile(registration.Capabilities, nativeExecution); err != nil {
				status.State, status.Reason = api.CompatibilityIncompatible, "ExecutionProfileUnsupported"
				status.Issues = append(status.Issues, compatibilityIssue("execution_profile_unsupported", "spec.sandboxRefs", "The Sandbox execution profile is not enabled by this platform.", &report.Team, referencePointer(report.Policy), referencePointer(status.Sandbox)))
			}
			if errs := CheckCompatibility(normalized, policySpec, registration.Capabilities); len(errs) != 0 {
				status.State, status.Reason = api.CompatibilityIncompatible, "SandboxIncompatible"
				status.Issues = append(status.Issues, compatibilityFieldIssues(errs, &report.Team, referencePointer(report.Policy), referencePointer(status.Sandbox))...)
			}
		}
		report.Sandboxes = append(report.Sandboxes, status)
		report.Issues = append(report.Issues, status.Issues...)
	}
	if len(report.Issues) != 0 {
		report.State, report.Reason, report.Message = api.CompatibilityIncompatible, "ConfigurationIncompatible", "The Team Policy and selected Sandboxes are incompatible."
	}
	return report
}

// CompatibilityIssues returns the safe issue representation shared by status
// reporting and Kubernetes admission responses.
func CompatibilityIssues(report api.TeamCompatibilityStatus) []api.CompatibilityIssue {
	return slices.Clone(report.Issues)
}

func compatibilityReference(name, uid string, generation int64) api.CompatibilityReference {
	return api.CompatibilityReference{Name: name, UID: uid, Generation: generation}
}

func referencePointer(value api.CompatibilityReference) *api.CompatibilityReference { return &value }

func compatibilityIssue(code, field, message string, team, policy, box *api.CompatibilityReference) api.CompatibilityIssue {
	return api.CompatibilityIssue{Code: code, Field: field, Message: message, Team: team, Policy: policy, Sandbox: box}
}

func compatibilityFieldIssues(errs field.ErrorList, team, policy, box *api.CompatibilityReference) []api.CompatibilityIssue {
	issues := make([]api.CompatibilityIssue, 0, len(errs))
	seen := map[string]bool{}
	for _, err := range errs {
		code, path := compatibilityCodeForField(err.Field)
		if seen[code] {
			continue
		}
		seen[code] = true
		message, ok := api.CompatibilityIssueMessage(code)
		if !ok {
			code, path, message = "sandbox_incompatible", "spec.agentPolicyRef", "The selected Sandbox does not support one or more requested Policy controls."
		}
		issues = append(issues, compatibilityIssue(code, path, message, team, policy, box))
	}
	return issues
}

func compatibilityCodeForField(value string) (string, string) {
	path := safeCompatibilityField(value)
	switch {
	case strings.Contains(value, "blockedPorts") || strings.Contains(value, ".ports") || strings.Contains(value, "protocols"):
		return "network_protocol_unsupported", path
	case strings.Contains(value, "storage.home.accessMode"):
		return "home_access_mode_unsupported", path
	case strings.Contains(value, "allowedInternalCIDRs") || strings.Contains(value, "blockedCIDRs") || strings.Contains(value, "ipFamilies"):
		return "network_address_family_unsupported", path
	case strings.Contains(value, "readOnlyRootFilesystem"):
		return "read_only_root_filesystem_unsupported", path
	case strings.Contains(value, "authenticatedIngress") || strings.Contains(value, "disabledIngress") || strings.Contains(value, "mutualTLS"):
		return "ingress_unsupported", path
	case strings.Contains(value, "blockedFQDNs") || strings.Contains(value, "guestExternalNetwork") || strings.Contains(value, "denyPrecedence") || strings.Contains(value, "nodeTraffic") || strings.Contains(value, "dnsIPRestrictions"):
		return "network_control_unsupported", path
	case strings.Contains(value, "kubernetesAccess"):
		return "kubernetes_access_unsupported", path
	case strings.Contains(value, "entraID"):
		return "entra_id_unsupported", path
	default:
		return "sandbox_incompatible", "spec.agentPolicyRef"
	}
}

func safeCompatibilityField(value string) string {
	if strings.HasPrefix(value, "spec.") || strings.HasPrefix(value, "registration.") {
		return value
	}
	return "spec.agentPolicyRef"
}

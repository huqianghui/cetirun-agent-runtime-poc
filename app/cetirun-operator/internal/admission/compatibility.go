// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxadmission

import (
	"context"
	"encoding/json"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// executionCompatibilityReport adds safe diagnostics for the native topology
// and grant checks that are intentionally outside sandbox.CheckCompatibility.
// It mirrors admission's read-only checks and has no reservation or write side
// effects, so it can report every selected Sandbox before admission rejects.
func (h *Handler) executionCompatibilityReport(ctx context.Context, team *api.Team, resources sandbox.Resources, registrations map[api.SandboxType]sandbox.Registration) api.TeamCompatibilityStatus {
	report := sandbox.TeamCompatibilityReport(team, resources, registrations, h.config.NativeExecution)
	policy := resources.AgentPolicies[team.Spec.AgentPolicyRef]
	if policy == nil || h.platformSnapshot == nil {
		return report
	}
	for index := range report.Sandboxes {
		status := &report.Sandboxes[index]
		box := resources.Sandboxes[status.Sandbox.Name]
		if box == nil || status.State == api.CompatibilityIncompatible {
			continue
		}
		normalized, errs := sandbox.NormalizeSandbox(box.Spec)
		if len(errs) != 0 {
			continue
		}
		registration, registered := registrations[normalized.Type]
		platform, supported := h.platformSnapshot.Platforms[normalized.Type]
		if !registered || !supported {
			continue
		}
		if err := sandboxpolicy.ValidateNativeTeamPolicyBinding(ctx, policy, registration.Capabilities, platform, h.reader, h.config.NativeExecution); err != nil {
			code := "sandbox_incompatible"
			if policy.Spec.KubernetesAccess != nil {
				code = "kubernetes_access_unsupported"
			} else if len(policy.Spec.Network.BlockedCIDRs) != 0 || len(policy.Spec.Network.BlockedPorts) != 0 || len(policy.Spec.Network.AllowedInternalCIDRs) != 0 {
				code = "network_control_unsupported"
			}
			message, _ := api.CompatibilityIssueMessage(code)
			issue := api.CompatibilityIssue{Code: code, Field: "spec.agentPolicyRef", Message: message,
				Team: &report.Team, Policy: &report.Policy, Sandbox: &status.Sandbox}
			status.State, status.Reason = api.CompatibilityIncompatible, "PlatformControlUnsupported"
			status.Issues = append(status.Issues, issue)
			report.Issues = append(report.Issues, issue)
			report.State, report.Reason, report.Message = api.CompatibilityIncompatible, "ConfigurationIncompatible", "The Team Policy and selected Sandboxes are incompatible."
		}
	}
	return report
}

// compatibilityDenied keeps an admission rejection useful to kubectl while
// exposing only the fixed compatibility contract in StatusDetails for Portal.
func compatibilityDenied(message string, report api.TeamCompatibilityStatus) admission.Response {
	response := admission.Denied(message)
	issues := sandbox.CompatibilityIssues(report)
	if len(issues) == 0 {
		return response
	}
	causes := make([]metav1.StatusCause, 0, len(issues))
	for _, issue := range issues {
		if !issue.Valid() {
			continue
		}
		encoded, err := json.Marshal(issue)
		if err != nil {
			continue
		}
		causes = append(causes, metav1.StatusCause{Type: metav1.CauseType("CetiRunCompatibility"), Field: issue.Field, Message: string(encoded)})
	}
	if len(causes) != 0 {
		response.Result.Details = &metav1.StatusDetails{Causes: causes}
	}
	return response
}

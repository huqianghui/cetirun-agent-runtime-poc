// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	"slices"
	"testing"

	"k8s.io/utils/ptr"
)

func TestLegacyResourcesDoNotRequirePolicyBindings(t *testing.T) {
	agent := apiContractCRD(t, "agents").Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	for _, field := range []string{"team", "sandboxRef"} {
		if slices.Contains(agent.Required, field) {
			t.Errorf("legacy Agent creation requires new binding %s", field)
		}
	}
	team := apiContractCRD(t, "teams").Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	for _, field := range []string{"agentPolicyRef", "sandboxRefs", "defaultSandboxRef"} {
		if slices.Contains(team.Required, field) {
			t.Errorf("legacy Team creation requires new binding %s", field)
		}
	}
}

func TestLegacyAndPolicyModelSourceDefaultsRemainDistinct(t *testing.T) {
	for _, sandboxRef := range []string{"", "standard"} {
		spec := AgentSpec{SandboxRef: sandboxRef}
		wantDefault := sandboxRef == ""
		if spec.SharedModelsEnabled() != wantDefault || spec.GitHubCopilotEnabled() != wantDefault {
			t.Fatalf("unexpected omitted model sources for Sandbox %q", sandboxRef)
		}
		spec.ModelAccess = &ModelAccessSpec{SharedModels: ptr.To(false), GitHubCopilot: ptr.To(true)}
		if spec.SharedModelsEnabled() || !spec.GitHubCopilotEnabled() {
			t.Fatalf("explicit model requests were not preserved for Sandbox %q", sandboxRef)
		}
	}
}

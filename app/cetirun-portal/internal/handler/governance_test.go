// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package handler

import (
	"context"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/kube"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func governanceCondition(conditionType, reason string, status metav1.ConditionStatus, generation int64) metav1.Condition {
	return metav1.Condition{
		Type: conditionType, Status: status, Reason: reason, Message: reason + " message", ObservedGeneration: generation,
	}
}

func TestGovernanceStatusCurrentConditions(t *testing.T) {
	for _, tc := range []struct {
		name       string
		conditions []metav1.Condition
		category   string
		reason     string
		wantNil    bool
	}{
		{
			name: "policy denial outranks generic hold",
			conditions: []metav1.Condition{
				governanceCondition(api.ConditionSandboxReady, "EnforcementPending", metav1.ConditionFalse, 7),
				governanceCondition(api.ConditionPolicyResolved, "PolicyDenied", metav1.ConditionFalse, 7),
			},
			category: "policy", reason: "PolicyDenied",
		},
		{
			name:       "identity owner binding",
			conditions: []metav1.Condition{governanceCondition(api.ConditionAgentIdentityReady, "OwnerBindingMissing", metav1.ConditionFalse, 7)},
			category:   "identity", reason: "OwnerBindingMissing",
		},
		{
			name: "identity failure outranks generic enforcement hold",
			conditions: []metav1.Condition{
				governanceCondition(api.ConditionPolicyApplied, "EnforcementPending", metav1.ConditionFalse, 7),
				governanceCondition(api.ConditionAgentIdentityReady, "CredentialUnavailable", metav1.ConditionFalse, 7),
			},
			category: "identity", reason: "CredentialUnavailable",
		},
		{
			name:       "unsupported platform control",
			conditions: []metav1.Condition{governanceCondition(api.ConditionSandboxReady, "UnsupportedControl", metav1.ConditionFalse, 7)},
			category:   "platform", reason: "UnsupportedControl",
		},
		{
			name:       "dependency unavailable is current unknown",
			conditions: []metav1.Condition{governanceCondition(api.ConditionPolicyResolved, "DependencyUnavailable", metav1.ConditionUnknown, 7)},
			category:   "dependency", reason: "DependencyUnavailable",
		},
		{
			name:       "approval is policy because grant type is ambiguous",
			conditions: []metav1.Condition{governanceCondition(api.ConditionSandboxReady, "UnauthorizedBinding", metav1.ConditionFalse, 7)},
			category:   "policy", reason: "UnauthorizedBinding",
		},
		{
			name: "current policy dependency outranks approval hold",
			conditions: []metav1.Condition{
				governanceCondition(api.ConditionPolicyResolved, "DependencyUnavailable", metav1.ConditionUnknown, 7),
				governanceCondition(api.ConditionSandboxReady, "UnauthorizedBinding", metav1.ConditionFalse, 7),
			},
			category: "dependency", reason: "DependencyUnavailable",
		},
		{
			name:       "ordinary pending enforcement",
			conditions: []metav1.Condition{governanceCondition(api.ConditionPolicyApplied, "EnforcementPending", metav1.ConditionFalse, 7)},
			category:   "pending", reason: "EnforcementPending",
		},
		{
			name:       "stale denial is ignored after update",
			conditions: []metav1.Condition{governanceCondition(api.ConditionPolicyResolved, "PolicyDenied", metav1.ConditionFalse, 6)},
			wantNil:    true,
		},
		{
			name:       "unrelated condition is ignored",
			conditions: []metav1.Condition{governanceCondition(api.ConditionReady, "Pending", metav1.ConditionFalse, 7)},
			wantNil:    true,
		},
		{
			name:       "unrelated policy-like reason is ignored",
			conditions: []metav1.Condition{governanceCondition(api.ConditionPodReady, "PolicyDenied", metav1.ConditionFalse, 7)},
			wantNil:    true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			instance := &api.Agent{ObjectMeta: metav1.ObjectMeta{Generation: 7}, Status: api.AgentStatus{Conditions: tc.conditions}}
			got := governanceStatus(instance)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("governanceStatus() = %+v, want nil", got)
				}
				return
			}
			if got == nil || got.Category != tc.category || got.Reason != tc.reason || got.ObservedGeneration != 7 {
				t.Fatalf("governanceStatus() = %+v, want category=%q reason=%q", got, tc.category, tc.reason)
			}
		})
	}
}

func TestGovernanceStatusSuppressesInactiveLifecycle(t *testing.T) {
	base := &api.Agent{ObjectMeta: metav1.ObjectMeta{Generation: 7}, Status: api.AgentStatus{Conditions: []metav1.Condition{
		governanceCondition(api.ConditionPolicyResolved, "PolicyDenied", metav1.ConditionFalse, 7),
	}}}
	base.Spec.Suspended = true
	if got := governanceStatus(base); got != nil {
		t.Fatalf("suspended governanceStatus() = %+v, want nil", got)
	}
	base.Spec.Suspended = false
	now := metav1.Now()
	base.DeletionTimestamp = &now
	if got := governanceStatus(base); got != nil {
		t.Fatalf("deleting governanceStatus() = %+v, want nil", got)
	}
}

func TestGovernanceStatusCurrentZeroGenerationEvidence(t *testing.T) {
	policyCondition := governanceCondition(api.ConditionPolicyResolved, "PolicyDenied", metav1.ConditionFalse, 0)
	policyCondition.Message = "policy requires Agent Identity"
	platformCondition := governanceCondition(api.ConditionSandboxReady, "UnsupportedControl", metav1.ConditionFalse, 7)
	policy := &api.PolicyStatus{Phase: "Denied", ObservedGeneration: 7}
	instance := &api.Agent{ObjectMeta: metav1.ObjectMeta{Generation: 7}, Status: api.AgentStatus{
		Policy: policy, Conditions: []metav1.Condition{platformCondition, policyCondition},
	}}
	got := governanceStatus(instance)
	if got == nil || got.Reason != "PolicyDenied" || got.Condition != api.ConditionPolicyResolved || got.Message != policyCondition.Message || got.ObservedGeneration != 7 {
		t.Fatalf("policy governanceStatus() = %+v", got)
	}

	identityCondition := governanceCondition(api.ConditionAgentIdentityReady, "OwnerBindingMissing", metav1.ConditionFalse, 0)
	instance.Status = api.AgentStatus{AgentIdentity: &api.AgentIdentityStatus{
		Phase: "Failed", Reason: "OwnerBindingMissing", ObservedGeneration: 7,
	}, Conditions: []metav1.Condition{identityCondition}}
	got = governanceStatus(instance)
	if got == nil || got.Category != "identity" || got.Reason != "OwnerBindingMissing" || got.ObservedGeneration != 7 {
		t.Fatalf("identity governanceStatus() = %+v", got)
	}
}

func TestGovernanceStatusRejectsStaleAndMissingEvidence(t *testing.T) {
	for _, tc := range []struct {
		name      string
		condition metav1.Condition
		status    api.AgentStatus
	}{
		{
			name:      "nonzero stale condition cannot fall back to current policy",
			condition: governanceCondition(api.ConditionPolicyResolved, "PolicyDenied", metav1.ConditionFalse, 6),
			status:    api.AgentStatus{Policy: &api.PolicyStatus{Phase: "Denied", ObservedGeneration: 7}},
		},
		{
			name:      "zero condition requires current policy status",
			condition: governanceCondition(api.ConditionPolicyResolved, "PolicyDenied", metav1.ConditionFalse, 0),
			status:    api.AgentStatus{Policy: &api.PolicyStatus{Phase: "Denied", ObservedGeneration: 6}},
		},
		{
			name:      "future condition is not current",
			condition: governanceCondition(api.ConditionPolicyResolved, "PolicyDenied", metav1.ConditionFalse, 8),
			status:    api.AgentStatus{Policy: &api.PolicyStatus{Phase: "Denied", ObservedGeneration: 7}},
		},
		{
			name:      "zero identity condition requires matching reason",
			condition: governanceCondition(api.ConditionAgentIdentityReady, "OwnerBindingMissing", metav1.ConditionFalse, 0),
			status:    api.AgentStatus{AgentIdentity: &api.AgentIdentityStatus{Phase: "Failed", Reason: "CredentialUnavailable", ObservedGeneration: 7}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			instance := &api.Agent{ObjectMeta: metav1.ObjectMeta{Generation: 7}, Status: tc.status}
			instance.Status.Conditions = []metav1.Condition{tc.condition}
			if got := governanceStatus(instance); got != nil {
				t.Fatalf("governanceStatus() = %+v, want nil", got)
			}
		})
	}
}

func TestGovernanceStatusPrioritizesSpecificPolicyConditionRegardlessOfOrder(t *testing.T) {
	resolved := governanceCondition(api.ConditionPolicyResolved, "PolicyDenied", metav1.ConditionFalse, 7)
	resolved.Message = "Agent Identity is not permitted by policy"
	applied := governanceCondition(api.ConditionPolicyApplied, "PolicyDenied", metav1.ConditionFalse, 7)
	applied.Message = "Policy enforcement is withheld until the assignment and requests are valid"
	held := governanceCondition(api.ConditionReady, "PolicyDenied", metav1.ConditionFalse, 7)
	held.Message = "Sandbox execution remains held"
	for _, conditions := range [][]metav1.Condition{{held, applied, resolved}, {resolved, held, applied}} {
		got := governanceStatus(&api.Agent{ObjectMeta: metav1.ObjectMeta{Generation: 7}, Status: api.AgentStatus{Conditions: conditions}})
		if got == nil || got.Condition != api.ConditionPolicyResolved || got.Message != resolved.Message {
			t.Fatalf("governanceStatus() = %+v, want detailed PolicyResolved denial", got)
		}
	}
}

func TestGovernanceStatusProvisioningDoesNotHidePlatformBlock(t *testing.T) {
	identityCondition := governanceCondition(api.ConditionAgentIdentityReady, "Provisioning", metav1.ConditionFalse, 0)
	platformCondition := governanceCondition(api.ConditionSandboxReady, "UnsupportedControl", metav1.ConditionFalse, 7)
	instance := &api.Agent{ObjectMeta: metav1.ObjectMeta{Generation: 7}, Status: api.AgentStatus{
		AgentIdentity: &api.AgentIdentityStatus{Phase: "Provisioning", ObservedGeneration: 7},
		Conditions:    []metav1.Condition{identityCondition, platformCondition},
	}}
	got := governanceStatus(instance)
	if got == nil || got.Category != "platform" || got.Reason != "UnsupportedControl" {
		t.Fatalf("governanceStatus() = %+v, want platform block", got)
	}
}

func TestGovernanceStatusNormalizesIdentityFailureMessage(t *testing.T) {
	condition := governanceCondition(api.ConditionAgentIdentityReady, "CredentialUnavailable", metav1.ConditionFalse, 7)
	condition.Message = "directory client returned a diagnostic that is not for a list summary"
	got := governanceStatus(&api.Agent{ObjectMeta: metav1.ObjectMeta{Generation: 7}, Status: api.AgentStatus{Conditions: []metav1.Condition{condition}}})
	if got == nil || got.Category != "identity" || got.Message != "Agent Identity is not ready." {
		t.Fatalf("governanceStatus() = %+v", got)
	}
}

func TestGovernanceStatusCurrentPolicyDependencyOutranksHoldFallback(t *testing.T) {
	dependency := governanceCondition(api.ConditionPolicyResolved, "DependencyUnavailable", metav1.ConditionFalse, 0)
	held := governanceCondition(api.ConditionSandboxReady, "UnsupportedControl", metav1.ConditionFalse, 7)
	instance := &api.Agent{ObjectMeta: metav1.ObjectMeta{Generation: 7}, Status: api.AgentStatus{
		Policy:     &api.PolicyStatus{Phase: "Pending", ObservedGeneration: 7},
		Conditions: []metav1.Condition{held, dependency},
	}}
	got := governanceStatus(instance)
	if got == nil || got.Category != "dependency" || got.Reason != "DependencyUnavailable" || got.ObservedGeneration != 7 {
		t.Fatalf("governanceStatus() = %+v, want current dependency", got)
	}
}

func TestGovernanceStatusClearsAfterRecovery(t *testing.T) {
	instance := &api.Agent{ObjectMeta: metav1.ObjectMeta{Generation: 7}, Status: api.AgentStatus{
		Policy: &api.PolicyStatus{Phase: "Applied", ObservedGeneration: 7},
		Conditions: []metav1.Condition{
			governanceCondition(api.ConditionPolicyResolved, "EffectivePolicyResolved", metav1.ConditionTrue, 0),
			governanceCondition(api.ConditionPolicyApplied, "RequiredEnforcementVerified", metav1.ConditionTrue, 0),
		},
	}}
	if got := governanceStatus(instance); got != nil {
		t.Fatalf("governanceStatus() = %+v after recovery, want nil", got)
	}
}

func TestGovernanceStatusUsesSafeGenericSummaryMessage(t *testing.T) {
	condition := governanceCondition(api.ConditionPolicyApplied, "PolicyDenied", metav1.ConditionFalse, 7)
	condition.Message = "Policy evaluation failed for token sk-dummy-sensitive"
	instance := &api.Agent{ObjectMeta: metav1.ObjectMeta{Generation: 7}, Status: api.AgentStatus{Conditions: []metav1.Condition{condition}}}
	got := governanceStatus(instance)
	if got == nil || got.Message != "The Agent configuration is not allowed by the selected policy." {
		t.Fatalf("governanceStatus() = %+v", got)
	}
	if strings.Contains(got.Message, "sk-dummy-sensitive") {
		t.Fatalf("summary exposed a condition diagnostic: %q", got.Message)
	}
	instance.Status.Conditions[0].Type = api.ConditionAgentIdentityReady
	instance.Status.Conditions[0].Reason = "CredentialUnavailable"
	got = governanceStatus(instance)
	if got == nil || got.Message != "Agent Identity is not ready." {
		t.Fatalf("identity summary should explain the failure without raw diagnostics: %+v", got)
	}
}

func TestGovernanceResponseWiring(t *testing.T) {
	instance := sampleInstance()
	instance.Generation = 7
	instance.Status.Phase = "Pending"
	instance.Status.Conditions = []metav1.Condition{
		governanceCondition(api.ConditionPolicyResolved, "PolicyDenied", metav1.ConditionFalse, 7),
	}

	h := &InstanceHandler{Kube: kube.NewClient(newFakeClient())}
	owner, err := h.toInstanceResponse(context.Background(), instance)
	if err != nil {
		t.Fatal(err)
	}
	if owner.Governance == nil || owner.Governance.Reason != "PolicyDenied" {
		t.Fatalf("owner governance = %+v", owner.Governance)
	}
	if len(owner.Conditions) != 1 || owner.Conditions[0].ObservedGeneration != 7 {
		t.Fatalf("owner conditions = %+v", owner.Conditions)
	}

	admin := adminInstanceSummary(instance)
	if admin.Governance == nil || admin.Governance.Reason != "PolicyDenied" {
		t.Fatalf("admin governance = %+v", admin.Governance)
	}
	detail := adminInstanceDetail(instance)
	if detail.Governance == nil || len(detail.Conditions) != 1 || detail.Conditions[0].ObservedGeneration != 7 {
		t.Fatalf("admin detail = %+v", detail)
	}
}

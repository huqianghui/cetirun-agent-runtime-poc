// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"maps"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/aigateway"
)

func (f *fakeAIGateway) UpdateKeyLimits(_ context.Context, token string, limits aigateway.KeyLimits) error {
	key, ok := f.keys[token]
	if !ok {
		return &aigateway.APIError{StatusCode: 404, Message: "not found"}
	}
	f.limitUpdates = append(f.limitUpdates, cloneKeyLimits(limits))
	if !f.ignoreLimitUpdate {
		key.Limits = cloneKeyLimits(limits)
	}
	return nil
}

func cloneKeyLimits(limits aigateway.KeyLimits) aigateway.KeyLimits {
	limits.MaxParallelRequests = cloneLimit(limits.MaxParallelRequests)
	limits.RPM = cloneLimit(limits.RPM)
	limits.TPM = cloneLimit(limits.TPM)
	limits.ModelRPM = maps.Clone(limits.ModelRPM)
	limits.ModelTPM = maps.Clone(limits.ModelTPM)
	return limits
}

// setTestPolicyRateLimits changes the test policy's rate limits as an
// accepted new generation, the way the AgentPolicy controller would.
func setTestPolicyRateLimits(t *testing.T, kube client.Client, limits *api.AgentPolicyRateLimits) {
	t.Helper()
	ctx := context.Background()
	policy := &api.AgentPolicy{}
	if err := kube.Get(ctx, types.NamespacedName{Name: testPolicyName}, policy); err != nil {
		t.Fatal(err)
	}
	policy.Spec.ModelAccess.RateLimits = limits
	policy.Generation++
	if err := kube.Update(ctx, policy); err != nil {
		t.Fatal(err)
	}
	policy.Status.ObservedGeneration = policy.Generation
	policy.Status.Conditions[0].ObservedGeneration = policy.Generation
	if err := kube.Status().Update(ctx, policy); err != nil {
		t.Fatal(err)
	}
}

func exampleRateLimits() *api.AgentPolicyRateLimits {
	return &api.AgentPolicyRateLimits{
		MaxParallelRequests: ptr.To[int64](4), RPM: ptr.To[int64](60), TPM: ptr.To[int64](200000),
		PerModel: map[string]api.AgentPolicyModelRateLimit{
			testSharedModelOne: {RPM: ptr.To[int64](10), TPM: ptr.To[int64](50000)},
			"auto":             {RPM: ptr.To[int64](20)},
		},
	}
}

func exampleKeyLimits() aigateway.KeyLimits {
	return aigateway.KeyLimits{
		MaxParallelRequests: ptr.To[int64](4), RPM: ptr.To[int64](60), TPM: ptr.To[int64](200000),
		ModelRPM: map[string]int64{testSharedModelOne: 10, "auto": 20}, ModelTPM: map[string]int64{testSharedModelOne: 50000},
	}
}

func TestPolicyRateLimitsAreSetOnANewKey(t *testing.T) {
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	r, kube, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace())
	setTestPolicyRateLimits(t, kube, exampleRateLimits())
	if err := r.reconcileAIGateway(context.Background(), agent, true); err != nil {
		t.Fatal(err)
	}
	if len(gateway.generated) != 1 || !gateway.generated[0].Limits.Equal(exampleKeyLimits()) {
		t.Fatalf("create request limits = %+v, want %+v", gateway.generated, exampleKeyLimits())
	}
	if len(gateway.limitUpdates) != 0 || agent.Status.AIGateway.Phase != api.AIGatewayPhaseReady {
		t.Fatalf("a new key must carry its limits without a follow-up update: updates=%v status=%+v", gateway.limitUpdates, agent.Status.AIGateway)
	}
}

func TestPolicyRateLimitChangesUpdateTheSameKeyInPlace(t *testing.T) {
	ctx := context.Background()
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	r, kube, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace())
	if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
		t.Fatal(err)
	}
	if len(gateway.generated) != 1 || !gateway.generated[0].Limits.IsZero() {
		t.Fatalf("a policy without rate limits must create an unlimited key: %+v", gateway.generated)
	}
	token := aigateway.Token(gateway.generated[0].Key)
	gateway.keys[token].Spend = ptr.To(2.5)

	steps := []struct {
		name   string
		limits *api.AgentPolicyRateLimits
		want   aigateway.KeyLimits
	}{
		{"added", exampleRateLimits(), exampleKeyLimits()},
		{"changed and partly removed", &api.AgentPolicyRateLimits{RPM: ptr.To[int64](30), PerModel: map[string]api.AgentPolicyModelRateLimit{
			"auto": {TPM: ptr.To[int64](8192)},
		}}, aigateway.KeyLimits{RPM: ptr.To[int64](30), ModelTPM: map[string]int64{"auto": 8192}}},
		{"removed", nil, aigateway.KeyLimits{}},
	}
	for i, step := range steps {
		setTestPolicyRateLimits(t, kube, step.limits)
		if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		key := gateway.keys[token]
		if len(gateway.limitUpdates) != i+1 || !gateway.limitUpdates[i].Equal(step.want) || !key.Limits.Equal(step.want) {
			t.Fatalf("%s: updates=%+v key=%+v, want %+v", step.name, gateway.limitUpdates, key.Limits, step.want)
		}
		if len(gateway.generated) != 1 || len(gateway.keys) != 1 || key.Blocked || *key.Spend != 2.5 ||
			agent.Status.AIGateway.Phase != api.AIGatewayPhaseReady || agent.Status.AIGateway.Blocked {
			t.Fatalf("%s: a limit change must keep the same unblocked key and its spend: %+v %+v", step.name, key, agent.Status.AIGateway)
		}
		// A steady reconcile compares the read-back limits and sends nothing.
		if err := r.reconcileAIGateway(ctx, agent, true); err != nil || len(gateway.limitUpdates) != i+1 {
			t.Fatalf("%s: steady reconcile err=%v updates=%d", step.name, err, len(gateway.limitUpdates))
		}
	}
}

func TestPolicyRateLimitsRepairAKeyChangedInTheGateway(t *testing.T) {
	ctx := context.Background()
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	r, kube, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace())
	setTestPolicyRateLimits(t, kube, exampleRateLimits())
	if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
		t.Fatal(err)
	}
	token := aigateway.Token(gateway.generated[0].Key)
	gateway.keys[token].Limits = aigateway.KeyLimits{RPM: ptr.To[int64](1000)}
	if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
		t.Fatal(err)
	}
	if !gateway.keys[token].Limits.Equal(exampleKeyLimits()) {
		t.Fatalf("policy-bound key limits drifted: %+v", gateway.keys[token].Limits)
	}
}

func TestPolicyRateLimitsMustBeReadBack(t *testing.T) {
	ctx := context.Background()
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	r, kube, gateway := aiGatewayTestReconciler(t, agent, aiGatewayNamespace())
	if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
		t.Fatal(err)
	}
	setTestPolicyRateLimits(t, kube, exampleRateLimits())
	gateway.ignoreLimitUpdate = true
	err := r.reconcileAIGateway(ctx, agent, true)
	if err == nil || agent.Status.AIGateway.Phase != api.AIGatewayPhaseDegraded || agent.Status.AIGateway.Reason != "KeyReadbackMismatch" {
		t.Fatalf("an unapplied limit update was reported as ready: err=%v status=%+v", err, agent.Status.AIGateway)
	}
	if token := aigateway.Token(gateway.generated[0].Key); gateway.keys[token].Blocked {
		t.Fatal("a limit read-back mismatch must not block the key")
	}
}

func TestPolicyRateLimitsStayOutOfThePolicyRevisions(t *testing.T) {
	ctx := context.Background()
	agent := aiGatewayTestAgent(testAIGatewayTeam)
	r, kube, _ := aiGatewayTestReconciler(t, agent, aiGatewayNamespace())
	before, err := r.resolvePolicy(ctx, agent)
	if err != nil {
		t.Fatal(err)
	}
	// This reconciler has no Sandbox compiler, so its desired Sandbox
	// revision is empty; give both resolutions the same applied revision.
	withSandbox := func(resolution *policyResolution) *policyResolution {
		copied := *resolution
		copied.sandboxRevision = "sandbox-revision"
		return &copied
	}
	current := withSandbox(before)
	// A running Agent whose every projection matches the current revisions.
	agent.Status.Phase = api.PhaseReady
	agent.Status.Policy = &api.PolicyStatus{Enforcement: api.PolicyEnforcementStatus{
		Sandbox:    api.PolicyRevisionExpectation{DesiredRevision: current.sandboxRevision},
		Gateway:    api.PolicyRevisionExpectation{DesiredRevision: current.gatewayRevision},
		Credential: api.PolicyRevisionExpectation{DesiredRevision: current.credentialRevision},
	}}
	agent.Status.Sandbox = &api.SandboxStatus{AppliedRevision: current.sandboxRevision, TeamUID: current.team.UID, AgentPolicyUID: current.policy.UID}
	agent.Status.AIGateway = &api.AIGatewayStatus{Phase: api.AIGatewayPhaseReady, Routed: true, PolicyRevision: current.gatewayRevision}
	agent.Status.Conditions = []metav1.Condition{{Type: api.ConditionSandboxReady, Status: metav1.ConditionTrue, ObservedGeneration: agent.Generation}}
	if policyReprojectionMustHold(agent, current) {
		t.Fatal("test setup: a verified running Agent must not be held")
	}
	for _, limits := range []*api.AgentPolicyRateLimits{exampleRateLimits(), {TPM: ptr.To[int64](4096)}, nil} {
		setTestPolicyRateLimits(t, kube, limits)
		after, err := r.resolvePolicy(ctx, agent)
		if err != nil {
			t.Fatal(err)
		}
		if after.gatewayRevision != before.gatewayRevision || after.sandboxRevision != before.sandboxRevision ||
			after.credentialRevision != before.credentialRevision {
			t.Fatalf("rate limits %+v changed a policy revision", limits)
		}
		if policyReprojectionMustHold(agent, withSandbox(after)) {
			t.Fatalf("rate limits %+v would fence and restart a running Agent", limits)
		}
	}
}

func TestLegacyKeyLimitsAreLeftToTheGatewayAdministrator(t *testing.T) {
	ctx := context.Background()
	r, agent, gateway := legacyGatewayFixture(t)
	if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
		t.Fatal(err)
	}
	if len(gateway.generated) != 1 || !gateway.generated[0].Limits.IsZero() {
		t.Fatalf("a legacy key must be created without policy limits: %+v", gateway.generated)
	}
	token := aigateway.Token(gateway.generated[0].Key)
	administrator := aigateway.KeyLimits{MaxParallelRequests: ptr.To[int64](3), ModelRPM: map[string]int64{"example-model": 5}}
	gateway.keys[token].Limits = cloneKeyLimits(administrator)
	for range 2 {
		if err := r.reconcileAIGateway(ctx, agent, true); err != nil {
			t.Fatal(err)
		}
	}
	if len(gateway.limitUpdates) != 0 || !gateway.keys[token].Limits.Equal(administrator) {
		t.Fatalf("legacy reconcile changed administrator limits: updates=%+v key=%+v", gateway.limitUpdates, gateway.keys[token].Limits)
	}
}

func TestPolicyAIGatewayKeyLimitsRejectsValuesTheCRDRejects(t *testing.T) {
	for name, limits := range map[string]*api.AgentPolicyRateLimits{
		"zero parallel":      {MaxParallelRequests: ptr.To[int64](0)},
		"zero rpm":           {RPM: ptr.To[int64](0)},
		"small tpm":          {TPM: ptr.To[int64](4095)},
		"model small tpm":    {PerModel: map[string]api.AgentPolicyModelRateLimit{"m": {TPM: ptr.To[int64](100)}}},
		"model zero rpm":     {PerModel: map[string]api.AgentPolicyModelRateLimit{"m": {RPM: ptr.To[int64](0)}}},
		"model without both": {PerModel: map[string]api.AgentPolicyModelRateLimit{"m": {}}},
		"model with space":   {PerModel: map[string]api.AgentPolicyModelRateLimit{"a m": {RPM: ptr.To[int64](1)}}},
		"empty model":        {PerModel: map[string]api.AgentPolicyModelRateLimit{"": {RPM: ptr.To[int64](1)}}},
		"long model":         {PerModel: map[string]api.AgentPolicyModelRateLimit{strings.Repeat("m", 257): {RPM: ptr.To[int64](1)}}},
	} {
		policy := aiGatewayTestPolicy()
		policy.Spec.ModelAccess.RateLimits = limits
		if _, err := policyAIGatewayKeyLimits(policy); err == nil {
			t.Errorf("%s: accepted %+v", name, limits)
		}
	}
	policy := aiGatewayTestPolicy()
	policy.Spec.ModelAccess.RateLimits = exampleRateLimits()
	limits, err := policyAIGatewayKeyLimits(policy)
	if err != nil || !limits.Equal(exampleKeyLimits()) {
		t.Fatalf("limits = %+v, %v; want %+v", limits, err, exampleKeyLimits())
	}
	*policy.Spec.ModelAccess.RateLimits.RPM = 1
	if *limits.RPM != 60 {
		t.Fatal("key limits must not alias the policy object")
	}
}

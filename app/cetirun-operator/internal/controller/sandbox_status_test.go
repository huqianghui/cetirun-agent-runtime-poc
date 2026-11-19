// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller_test

import (
	"errors"
	"reflect"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/controller"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
)

func sandboxStatusFixture() (*api.Agent, controller.SandboxStatusInput) {
	agent := &api.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "example-agent", UID: "example-agent-uid", Generation: 5},
		Spec:       api.AgentSpec{Team: "example-team", SandboxRef: "example-sandbox"},
		Status: api.AgentStatus{
			ObservedGeneration: 3, AccessURL: "https://portal.example.com/ui/example-agent/",
			Conditions: []metav1.Condition{{
				Type: api.ConditionHelmDeployed, Status: metav1.ConditionTrue, Reason: "PreviousRender",
				LastTransitionTime: metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
			}},
		},
	}
	binding := sandbox.Binding{
		AgentUID: agent.UID, TeamUID: "example-team-uid", SandboxUID: "example-sandbox-uid",
		AgentPolicyUID: "example-policy-uid",
	}
	expected := sandbox.ExecutionIdentity{
		Binding: binding, Revision: "synthetic-compiled-revision", PodUID: "example-pod-uid",
		NodeUID: "example-node-uid", EnforcementEpoch: "synthetic-enforcement-epoch",
	}
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	input := controller.SandboxStatusInput{
		AgentGeneration: agent.Generation,
		Resolution:      &sandbox.Resolution{Binding: binding, Revision: "synthetic-foundation"},
		Plan: &sandboxpolicy.Plan{
			AgentName: agent.Name, Namespace: "cetirun-example-agent", Binding: binding,
			ResolutionRevision: "synthetic-foundation", Revision: expected.Revision,
			Network: sandboxpolicy.NetworkPlan{IngressMode: api.AgentPolicyIngressAuthenticated},
		},
		Expected: expected,
		Observation: &sandbox.Observation{
			ExecutionIdentity: expected, ObservedAt: now,
			RuntimeVerified: true, PodVerified: true, ControlsVerified: true,
		},
		Now: now, MaxAge: 15 * time.Second,
	}
	return agent, input
}

func TestSandboxAgentStatusRequiresCurrentCompleteEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*api.Agent, *controller.SandboxStatusInput)
		status metav1.ConditionStatus
		reason string
	}{
		{"no observation", func(_ *api.Agent, i *controller.SandboxStatusInput) { i.Observation = nil }, metav1.ConditionUnknown, "ObservationUnavailable"},
		{"no resolution", func(_ *api.Agent, i *controller.SandboxStatusInput) { i.Resolution = nil }, metav1.ConditionUnknown, "ObservationUnavailable"},
		{"no compiled plan", func(_ *api.Agent, i *controller.SandboxStatusInput) { i.Plan = nil }, metav1.ConditionUnknown, "ObservationUnavailable"},
		{"incomplete foundation", func(_ *api.Agent, i *controller.SandboxStatusInput) {
			i.Resolution.Revision = ""
			i.Plan.ResolutionRevision = ""
		}, metav1.ConditionUnknown, "ObservationUnavailable"},
		{"cleared binding", func(a *api.Agent, _ *controller.SandboxStatusInput) { a.Spec.SandboxRef = "" }, metav1.ConditionUnknown, "ObservationUnavailable"},
		{"no clock", func(_ *api.Agent, i *controller.SandboxStatusInput) { i.Now = time.Time{} }, metav1.ConditionUnknown, "ObservationUnavailable"},
		{"no freshness bound", func(_ *api.Agent, i *controller.SandboxStatusInput) { i.MaxAge = 0 }, metav1.ConditionUnknown, "ObservationUnavailable"},
		{"stale evidence", func(_ *api.Agent, i *controller.SandboxStatusInput) {
			i.Observation.ObservedAt = i.Now.Add(-16 * time.Second)
		}, metav1.ConditionUnknown, "ObservationUnavailable"},
		{"future evidence", func(_ *api.Agent, i *controller.SandboxStatusInput) {
			i.Observation.ObservedAt = i.Now.Add(time.Second)
		}, metav1.ConditionUnknown, "ObservationUnavailable"},
		{"Pod replaced", func(_ *api.Agent, i *controller.SandboxStatusInput) { i.Observation.PodUID = "previous-pod" }, metav1.ConditionUnknown, "ObservationUnavailable"},
		{"node replaced", func(_ *api.Agent, i *controller.SandboxStatusInput) { i.Observation.NodeUID = "previous-node" }, metav1.ConditionUnknown, "ObservationUnavailable"},
		{"epoch changed", func(_ *api.Agent, i *controller.SandboxStatusInput) {
			i.Observation.EnforcementEpoch = "previous-epoch"
		}, metav1.ConditionUnknown, "ObservationUnavailable"},
		{"revision changed", func(_ *api.Agent, i *controller.SandboxStatusInput) { i.Observation.Revision = "previous-revision" }, metav1.ConditionUnknown, "ObservationUnavailable"},
		{"Team replaced", func(_ *api.Agent, i *controller.SandboxStatusInput) { i.Observation.Binding.TeamUID = "previous-team" }, metav1.ConditionUnknown, "ObservationUnavailable"},
		{"policy replaced", func(_ *api.Agent, i *controller.SandboxStatusInput) {
			i.Resolution.Binding.AgentPolicyUID = "replacement-policy"
		}, metav1.ConditionUnknown, "ObservationUnavailable"},
		{"Sandbox replaced", func(_ *api.Agent, i *controller.SandboxStatusInput) {
			i.Resolution.Binding.SandboxUID = "replacement-sandbox"
		}, metav1.ConditionUnknown, "ObservationUnavailable"},
		{"foundation changed", func(_ *api.Agent, i *controller.SandboxStatusInput) { i.Resolution.Revision = "new-foundation" }, metav1.ConditionUnknown, "ObservationUnavailable"},
		{"generation changed", func(a *api.Agent, _ *controller.SandboxStatusInput) { a.Generation++ }, metav1.ConditionUnknown, "ObservationUnavailable"},
		{"Agent recreated", func(a *api.Agent, _ *controller.SandboxStatusInput) { a.UID = "replacement-agent" }, metav1.ConditionUnknown, "ObservationUnavailable"},
		{"controls pending", func(_ *api.Agent, i *controller.SandboxStatusInput) { i.Observation.ControlsVerified = false }, metav1.ConditionFalse, "EnforcementPending"},
		{"Pod pending", func(_ *api.Agent, i *controller.SandboxStatusInput) { i.Observation.PodVerified = false }, metav1.ConditionFalse, "EnforcementPending"},
		{"runtime pending", func(_ *api.Agent, i *controller.SandboxStatusInput) { i.Observation.RuntimeVerified = false }, metav1.ConditionFalse, "EnforcementPending"},
		{"unqualified control", func(_ *api.Agent, i *controller.SandboxStatusInput) { i.Failure = sandboxpolicy.ErrUnsupportedControl }, metav1.ConditionFalse, "UnsupportedControl"},
		{"unqualified control with operational failure", func(_ *api.Agent, i *controller.SandboxStatusInput) {
			i.Failure = errors.Join(sandboxpolicy.ErrUnsupportedControl, errors.New("synthetic source failure"))
		}, metav1.ConditionFalse, "PolicyApplyFailed"},
		{"unavailable dependency", func(_ *api.Agent, i *controller.SandboxStatusInput) { i.Failure = sandboxpolicy.ErrUnavailable }, metav1.ConditionUnknown, "ObservationUnavailable"},
		{"runtime unavailable", func(_ *api.Agent, i *controller.SandboxStatusInput) { i.Failure = sandbox.ErrRuntimeUnavailable }, metav1.ConditionFalse, "RuntimeUnavailable"},
		{"reference missing", func(_ *api.Agent, i *controller.SandboxStatusInput) {
			i.Failure = apierrors.NewNotFound(schema.GroupResource{Group: api.GroupVersion.Group, Resource: "sandboxes"}, "example-sandbox")
		}, metav1.ConditionFalse, "ReferenceMissing"},
		{"invalid binding", func(_ *api.Agent, i *controller.SandboxStatusInput) { i.Failure = sandbox.ErrUnauthorizedBinding }, metav1.ConditionFalse, "UnauthorizedBinding"},
		{"apply failed", func(_ *api.Agent, i *controller.SandboxStatusInput) {
			i.Failure = errors.New("synthetic apply failure")
		}, metav1.ConditionFalse, "PolicyApplyFailed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent, input := sandboxStatusFixture()
			tc.mutate(agent, &input)
			result, err := controller.UpdateSandboxAgentStatus(agent, input)
			if err == nil || !result.Hold || result.Verified || !result.Changed {
				t.Fatalf("unverified execution advertised success: %+v, %v", result, err)
			}
			condition := apimeta.FindStatusCondition(agent.Status.Conditions, api.ConditionSandboxReady)
			if condition == nil || condition.Status != tc.status || condition.Reason != tc.reason ||
				condition.ObservedGeneration != agent.Generation {
				t.Fatalf("unexpected combined Sandbox/policy enforcement condition: %+v", condition)
			}
			if len(agent.Status.Conditions) != 3 {
				t.Fatalf("failure should update only SandboxReady and Ready while preserving HelmDeployed: %+v", agent.Status.Conditions)
			}
			if agent.Status.Sandbox != nil || agent.Status.ObservedGeneration != 3 || agent.Status.AccessURL != "" ||
				agent.Status.Phase != api.PhasePending || apimeta.IsStatusConditionTrue(agent.Status.Conditions, api.ConditionReady) {
				t.Fatal("pending status advanced applied fields, exposed a URL or advertised Ready")
			}
			if !apimeta.IsStatusConditionTrue(agent.Status.Conditions, api.ConditionHelmDeployed) {
				t.Fatal("an unrelated condition was discarded")
			}
		})
	}
}

func TestSandboxAgentStatusCommitsAtomicallyAndRetainsAppliedHistory(t *testing.T) {
	agent, input := sandboxStatusFixture()
	result, err := controller.UpdateSandboxAgentStatus(agent, input)
	if err != nil || result.Hold || !result.Verified || !result.Changed {
		t.Fatalf("complete synthetic observation did not commit: %+v, %v", result, err)
	}
	expected := &api.SandboxStatus{
		TeamUID: input.Expected.Binding.TeamUID, SandboxUID: input.Expected.Binding.SandboxUID,
		AgentPolicyUID: input.Expected.Binding.AgentPolicyUID,
		AppliedRevision: input.Plan.Revision,
	}
	if !reflect.DeepEqual(agent.Status.Sandbox, expected) || agent.Status.ObservedGeneration != agent.Generation {
		t.Fatalf("applied binding was not committed as one unit: %+v", agent.Status.Sandbox)
	}
	if len(agent.Status.Conditions) != 2 || !apimeta.IsStatusConditionTrue(agent.Status.Conditions, api.ConditionSandboxReady) {
		t.Fatalf("verified combined enforcement should add only SandboxReady, preserving HelmDeployed: %+v", agent.Status.Conditions)
	}
	firstTransition := apimeta.FindStatusCondition(agent.Status.Conditions, api.ConditionSandboxReady).LastTransitionTime
	input.Now = input.Now.Add(time.Second)
	input.Observation.ObservedAt = input.Now
	result, err = controller.UpdateSandboxAgentStatus(agent, input)
	if err != nil || result.Changed ||
		!firstTransition.Equal(&apimeta.FindStatusCondition(agent.Status.Conditions, api.ConditionSandboxReady).LastTransitionTime) {
		t.Fatal("repeated successful observation fabricated a status transition")
	}
	input.Observation.ControlsVerified = false
	result, err = controller.UpdateSandboxAgentStatus(agent, input)
	condition := apimeta.FindStatusCondition(agent.Status.Conditions, api.ConditionSandboxReady)
	if err == nil || !result.Hold || condition.Reason != "EnforcementLost" ||
		!reflect.DeepEqual(agent.Status.Sandbox, expected) {
		t.Fatal("lost enforcement failed to hold execution or corrupted applied history")
	}
	input.Failure = sandboxpolicy.ErrUnsupportedControl
	input.Now = input.Now.Add(time.Second)
	transition := condition.LastTransitionTime
	if _, err := controller.UpdateSandboxAgentStatus(agent, input); err == nil {
		t.Fatal("unsupported new policy must fail")
	}
	if !transition.Equal(&apimeta.FindStatusCondition(agent.Status.Conditions, api.ConditionSandboxReady).LastTransitionTime) {
		t.Fatal("reason-only condition changes must preserve LastTransitionTime")
	}
	if !reflect.DeepEqual(agent.Status.Sandbox, expected) {
		t.Fatal("failed policy changed last-applied binding")
	}
}

func TestSandboxAgentDisabledIngressRequiresObservedAbsence(t *testing.T) {
	agent, input := sandboxStatusFixture()
	input.Plan.Network.IngressMode = api.AgentPolicyIngressDisabled
	input.Observation = nil
	if result, _ := controller.UpdateSandboxAgentStatus(agent, input); !result.Hold ||
		apimeta.IsStatusConditionTrue(agent.Status.Conditions, api.ConditionIngressReady) {
		t.Fatal("empty route rendering is not evidence of disabled ingress")
	}
	input.Observation = &sandbox.Observation{
		ExecutionIdentity: input.Expected, ObservedAt: input.Now,
		RuntimeVerified: true, PodVerified: true, ControlsVerified: true,
	}
	if result, err := controller.UpdateSandboxAgentStatus(agent, input); err != nil || !result.Verified || result.Hold {
		t.Fatalf("verified absence did not satisfy disabled ingress: %+v, %v", result, err)
	}
	if agent.Status.AccessURL != "" || !apimeta.IsStatusConditionTrue(agent.Status.Conditions, api.ConditionIngressReady) {
		t.Fatal("verified disabled ingress must clear access URL and be compliant")
	}
}

func TestSandboxAgentStatusRechecksEffectiveRevisionWithoutGenerationChange(t *testing.T) {
	agent, input := sandboxStatusFixture()
	if _, err := controller.UpdateSandboxAgentStatus(agent, input); err != nil {
		t.Fatal(err)
	}
	previous := *agent.Status.Sandbox
	generation := agent.Generation
	input.Plan.Revision = "synthetic-topology-or-grant-change"
	input.Expected.Revision = input.Plan.Revision
	result, err := controller.UpdateSandboxAgentStatus(agent, input)
	if !errors.Is(err, sandbox.ErrObservationUnavailable) || !result.Hold ||
		agent.Generation != generation || !reflect.DeepEqual(*agent.Status.Sandbox, previous) {
		t.Fatal("unchanged Agent generation skipped effective revision verification or advanced applied history")
	}
}

func TestSandboxAgentSuspensionAndDeletionDoNotAdvanceAppliedState(t *testing.T) {
	for _, deleting := range []bool{false, true} {
		agent, input := sandboxStatusFixture()
		if deleting {
			now := metav1.NewTime(input.Now)
			agent.DeletionTimestamp = &now
		} else {
			agent.Spec.Suspended = true
		}
		result, err := controller.UpdateSandboxAgentStatus(agent, input)
		if err != nil || !result.Hold || result.Verified || agent.Status.Sandbox != nil || agent.Status.ObservedGeneration != 3 {
			t.Fatalf("suspended or deleting Agent advanced applied state: %+v, %v", result, err)
		}
	}
}

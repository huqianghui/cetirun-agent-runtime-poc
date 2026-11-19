// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"errors"
	"fmt"
	"reflect"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
)

// SandboxStatusInput must be freshly resolved and compiled on every reconcile,
// even when Agent generation is unchanged. Expected is trusted Pod/node/epoch
// identity, never an identity copied from the observation being checked.
type SandboxStatusInput struct {
	AgentGeneration int64
	Resolution      *sandbox.Resolution
	Plan            *sandboxpolicy.Plan
	Expected        sandbox.ExecutionIdentity
	Observation     *sandbox.Observation
	Failure         error
	Now             time.Time
	MaxAge          time.Duration
}

type SandboxStatusResult struct {
	Changed  bool
	Hold     bool
	Verified bool
}

// UpdateSandboxAgentStatus mutates status only. The caller must persist it with
// a generation/resourceVersion check and retain the existing execution hold or
// qualified safety barrier on Hold. This helper is not a stop/fencing action.
// SandboxReady covers combined Sandbox and policy enforcement; configuration
// acceptance is reported separately on AgentPolicy, not as an Agent condition.
func UpdateSandboxAgentStatus(agent *api.Agent, input SandboxStatusInput) (SandboxStatusResult, error) {
	result := SandboxStatusResult{Hold: true}
	if agent == nil {
		return result, fmt.Errorf("%w: Agent is required", sandbox.ErrObservationUnavailable)
	}
	before := agent.DeepCopy().Status
	err := input.Failure
	if err == nil {
		if input.Resolution == nil || input.Plan == nil ||
			agent.Spec.Team == "" || agent.Spec.SandboxRef == "" ||
			input.AgentGeneration != agent.Generation || input.Resolution.Binding.AgentUID != agent.UID ||
			input.Plan.Binding != input.Resolution.Binding || input.Plan.AgentName != agent.Name ||
			input.Plan.Namespace == "" || input.Resolution.Revision == "" || input.Plan.Revision == "" ||
			input.Plan.ResolutionRevision != input.Resolution.Revision ||
			input.Expected.Binding != input.Plan.Binding || input.Expected.Revision != input.Plan.Revision {
			err = fmt.Errorf("%w: current Agent, binding and compiled revision must agree", sandbox.ErrObservationUnavailable)
		} else {
			err = sandbox.VerifyObservation(input.Expected, input.Observation, input.Now, input.MaxAge)
		}
	}
	status, reason, message := metav1.ConditionTrue, "PolicyApplied", "Current guest-external observation verifies the selected Sandbox and policy controls."
	switch {
	case !agent.DeletionTimestamp.IsZero():
		status, reason, message = metav1.ConditionFalse, "Deleting", "Agent deletion is in progress; execution remains held."
	case agent.Spec.Suspended:
		status, reason, message = metav1.ConditionFalse, "Suspended", "Agent suspension is requested; execution remains held."
	case err != nil:
		status, reason, message = sandboxStatusFailure(err)
		if errors.Is(err, sandbox.ErrEnforcementPending) && appliedMatches(agent, input.Expected) {
			reason, message = "EnforcementLost", "Previously applied controls are no longer verified; execution must remain held."
		}
	default:
		result.Hold, result.Verified = false, true
		binding := input.Plan.Binding
		agent.Status.Sandbox = &api.SandboxStatus{
			TeamUID: binding.TeamUID, SandboxUID: binding.SandboxUID,
			AgentPolicyUID: binding.AgentPolicyUID, AppliedRevision: input.Plan.Revision,
		}
		agent.Status.ObservedGeneration = agent.Generation
	}
	now := input.Now
	if now.IsZero() {
		now = time.Now()
	}
	apimeta.SetStatusCondition(&agent.Status.Conditions, metav1.Condition{
		Type: api.ConditionSandboxReady, Status: status, Reason: reason, Message: message,
		ObservedGeneration: agent.Generation, LastTransitionTime: metav1.NewTime(now),
	})
	if result.Hold {
		agent.Status.AccessURL = ""
		apimeta.SetStatusCondition(&agent.Status.Conditions, metav1.Condition{
			Type: api.ConditionReady, Status: metav1.ConditionFalse, Reason: reason, Message: message,
			ObservedGeneration: agent.Generation, LastTransitionTime: metav1.NewTime(now),
		})
		if agent.Spec.Suspended {
			agent.Status.Phase = api.PhaseSuspended
		} else if !agent.DeletionTimestamp.IsZero() {
			agent.Status.Phase = api.PhaseDeleting
		} else {
			agent.Status.Phase = api.PhasePending
		}
	}
	if input.Plan != nil && input.Plan.Network.IngressMode == api.AgentPolicyIngressDisabled {
		agent.Status.AccessURL = ""
		if result.Verified {
			apimeta.SetStatusCondition(&agent.Status.Conditions, metav1.Condition{
				Type: api.ConditionIngressReady, Status: metav1.ConditionTrue, Reason: "IngressDisabled",
				Message:            "Current control observation verifies that routes and inbound permissions are absent.",
				ObservedGeneration: agent.Generation, LastTransitionTime: metav1.NewTime(now),
			})
		}
	}
	result.Changed = !reflect.DeepEqual(before, agent.Status)
	return result, err
}

func appliedMatches(agent *api.Agent, expected sandbox.ExecutionIdentity) bool {
	applied := agent.Status.Sandbox
	return applied != nil && applied.TeamUID == expected.Binding.TeamUID && applied.SandboxUID == expected.Binding.SandboxUID &&
		applied.AgentPolicyUID == expected.Binding.AgentPolicyUID &&
		applied.AppliedRevision == expected.Revision
}

func sandboxStatusFailure(err error) (metav1.ConditionStatus, string, string) {
	switch {
	case errors.Is(err, sandbox.ErrObservationUnavailable):
		return metav1.ConditionUnknown, "ObservationUnavailable", "Fresh evidence for the current binding and execution identity is unavailable."
	case errors.Is(err, sandboxpolicy.ErrUnavailable):
		return metav1.ConditionUnknown, "ObservationUnavailable", "Current trusted platform dependencies are unavailable."
	case errors.Is(err, sandbox.ErrEnforcementPending):
		return metav1.ConditionFalse, "EnforcementPending", "Current observation does not yet verify every required control."
	case errors.Is(err, sandbox.ErrRuntimeUnavailable):
		return metav1.ConditionFalse, "RuntimeUnavailable", "The selected Sandbox runtime is unavailable."
	case sandboxPreparationUnsupportedError(err):
		return metav1.ConditionFalse, "UnsupportedControl", "A requested Sandbox control is not qualified."
	case apierrors.IsNotFound(err):
		return metav1.ConditionFalse, "ReferenceMissing", "A required Sandbox configuration reference is missing."
	case errors.Is(err, sandbox.ErrUnauthorizedBinding), errors.Is(err, sandboxpolicy.ErrUnapprovedGrant):
		return metav1.ConditionFalse, "UnauthorizedBinding", "The current binding or identity grant is not authorized."
	case errors.Is(err, sandboxpolicy.ErrConflict):
		return metav1.ConditionFalse, "PolicyConflict", "Policy rules conflict with required platform controls."
	default:
		return metav1.ConditionFalse, "PolicyApplyFailed", "Sandbox control processing failed; execution must remain held."
	}
}

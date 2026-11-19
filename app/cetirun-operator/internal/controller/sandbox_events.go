// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxbinding"
)

const sandboxEventUnresolved = "unresolved"

type sandboxEventBinding struct {
	AgentUID       string `json:"agentUID"`
	TeamUID        string `json:"teamUID"`
	SandboxUID     string `json:"sandboxUID"`
	AgentPolicyUID string `json:"agentPolicyUID"`
}

type sandboxTransitionEvent struct {
	event                  string
	resourceKind           string
	resourceUID            string
	generation             int64
	condition              string
	conditionStatus        metav1.ConditionStatus
	reason                 string
	attemptedBinding       sandboxEventBinding
	attemptedBindingSource string
	attemptedRevision      string
	appliedBinding         sandboxEventBinding
	appliedRevision        string
}

// Call only after an authoritative, optimistic status write succeeds. Comparing
// committed states avoids retry spam without a second write or in-memory ledger.
func emitSandboxAgentTransition(ctx context.Context, before, after *api.Agent) {
	next := sandboxAgentTransition(after, before.Status.Sandbox)
	if next.event == "" || next == sandboxAgentTransition(before, before.Status.Sandbox) {
		return
	}
	next.emit(ctx)
}

func sandboxAgentTransition(agent *api.Agent, history *api.SandboxStatus) sandboxTransitionEvent {
	condition := apimeta.FindStatusCondition(agent.Status.Conditions, api.ConditionSandboxReady)
	if condition == nil || condition.ObservedGeneration != agent.Generation ||
		agent.Spec.Suspended || !agent.DeletionTimestamp.IsZero() {
		return sandboxTransitionEvent{}
	}
	event := sandboxTransitionEvent{
		resourceKind: "Agent", resourceUID: sandboxEventUID(agent.UID), generation: agent.Generation,
		condition: condition.Type, conditionStatus: condition.Status, reason: condition.Reason,
		attemptedBinding: unresolvedSandboxEventBinding(), attemptedBindingSource: sandboxEventUnresolved,
		attemptedRevision: sandboxEventUnresolved, appliedBinding: unresolvedSandboxEventBinding(),
		appliedRevision: sandboxEventUnresolved,
	}
	event.attemptedBinding.AgentUID = event.resourceUID
	authorized, authorizationErr := sandboxbinding.AuthorizedBinding(agent)
	if authorizationErr == nil {
		event.attemptedBinding = sandboxEventBinding{
			AgentUID: sandboxEventUID(authorized.AgentUID), TeamUID: sandboxEventUID(authorized.TeamUID),
			SandboxUID: sandboxEventUID(authorized.SandboxUID), AgentPolicyUID: sandboxEventUID(authorized.AgentPolicyUID),
		}
		event.attemptedBindingSource = "authorization"
	}
	switch condition.Status {
	case metav1.ConditionTrue:
		applied := agent.Status.Sandbox
		if condition.Reason != "PolicyApplied" || agent.Status.ObservedGeneration != agent.Generation ||
			authorizationErr != nil || applied == nil ||
			applied.AppliedRevision == "" || applied.TeamUID != authorized.TeamUID || applied.SandboxUID != authorized.SandboxUID ||
			applied.AgentPolicyUID != authorized.AgentPolicyUID {
			return sandboxTransitionEvent{}
		}
		event.event = "sandbox.policy.applied"
		history = applied
		event.attemptedRevision = sandboxEventRevision(applied.AppliedRevision)
	case metav1.ConditionFalse:
		switch condition.Reason {
		case "EnforcementLost":
			event.event = "sandbox.enforcement.lost"
		case "EnforcementPending", "RuntimeUnavailable", "UnsupportedControl", "ReferenceMissing",
			"UnauthorizedBinding", "PolicyConflict", "PolicyApplyFailed":
			event.event = "sandbox.enforcement.failed"
		default:
			// Preparation and intentional lifecycle holds are not enforcement failures.
			return sandboxTransitionEvent{}
		}
	case metav1.ConditionUnknown:
		if condition.Reason != "ObservationUnavailable" {
			return sandboxTransitionEvent{}
		}
		event.event = "sandbox.enforcement.failed"
	default:
		return sandboxTransitionEvent{}
	}
	if history != nil {
		event.appliedBinding = sandboxEventBinding{
			AgentUID: event.resourceUID, TeamUID: sandboxEventUID(history.TeamUID),
			SandboxUID: sandboxEventUID(history.SandboxUID), AgentPolicyUID: sandboxEventUID(history.AgentPolicyUID),
		}
		event.appliedRevision = sandboxEventRevision(history.AppliedRevision)
	}
	return event
}

func emitSandboxPolicyTransition(ctx context.Context, before, after *api.AgentPolicy) {
	next := sandboxPolicyTransition(after)
	if next.event == "" || next == sandboxPolicyTransition(before) {
		return
	}
	next.emit(ctx)
}

func sandboxPolicyTransition(policy *api.AgentPolicy) sandboxTransitionEvent {
	condition := apimeta.FindStatusCondition(policy.Status.Conditions, api.ConditionAgentPolicyAccepted)
	if condition == nil || condition.ObservedGeneration != policy.Generation ||
		policy.Status.ObservedGeneration != policy.Generation {
		return sandboxTransitionEvent{}
	}
	event := sandboxTransitionEvent{
		resourceKind: "AgentPolicy", resourceUID: sandboxEventUID(policy.UID), generation: policy.Generation,
		condition: condition.Type, conditionStatus: condition.Status, reason: condition.Reason,
		attemptedBinding: unresolvedSandboxEventBinding(), attemptedBindingSource: "configuration",
		attemptedRevision: sandboxEventUnresolved, appliedBinding: unresolvedSandboxEventBinding(),
		appliedRevision: sandboxEventUnresolved,
	}
	event.attemptedBinding.AgentPolicyUID = event.resourceUID
	switch condition.Status {
	case metav1.ConditionTrue:
		if condition.Reason != "PolicyAccepted" {
			return sandboxTransitionEvent{}
		}
		event.event = "sandbox.policy.accepted"
	case metav1.ConditionFalse:
		switch condition.Reason {
		case "InvalidConfiguration", "UnsupportedControl", "UnapprovedGrant", "PolicyConflict":
			event.event = "sandbox.policy.rejected"
		default:
			return sandboxTransitionEvent{}
		}
	case metav1.ConditionUnknown:
		if condition.Reason != "PrerequisitesUnavailable" {
			return sandboxTransitionEvent{}
		}
		event.event = "sandbox.policy.rejected"
	default:
		return sandboxTransitionEvent{}
	}
	return event
}

func (event sandboxTransitionEvent) emit(ctx context.Context) {
	log.FromContext(ctx).WithName("sandbox-policy").Info(event.event,
		"event", event.event, "timestamp", time.Now().UTC().Format(time.RFC3339Nano),
		"transitionID", event.correlation(), "resourceKind", event.resourceKind, "resourceUID", event.resourceUID,
		"generation", event.generation, "condition", event.condition, "conditionStatus", event.conditionStatus,
		"reason", event.reason, "attemptedBinding", event.attemptedBinding,
		"attemptedBindingSource", event.attemptedBindingSource, "attemptedRevision", event.attemptedRevision,
		"appliedBinding", event.appliedBinding, "appliedRevision", event.appliedRevision)
}

func (event sandboxTransitionEvent) correlation() string {
	// Only sanitized semantic state participates, never clocks, resourceVersions,
	// free-form messages or provider errors. This is correlation, not exactly-once delivery.
	fields := []string{
		"sandbox-transition-v1", event.event, event.resourceKind, event.resourceUID,
		strconv.FormatInt(event.generation, 10), event.condition, string(event.conditionStatus), event.reason,
		event.attemptedBindingSource, event.attemptedRevision, event.appliedRevision,
	}
	for _, binding := range []sandboxEventBinding{event.attemptedBinding, event.appliedBinding} {
		fields = append(fields, binding.AgentUID, binding.TeamUID, binding.SandboxUID, binding.AgentPolicyUID)
	}
	return sandboxEventDigest(strings.Join(fields, "\x00"))
}

func unresolvedSandboxEventBinding() sandboxEventBinding {
	return sandboxEventBinding{
		AgentUID: sandboxEventUnresolved, TeamUID: sandboxEventUnresolved, SandboxUID: sandboxEventUnresolved,
		AgentPolicyUID: sandboxEventUnresolved,
	}
}

func sandboxEventUID(uid types.UID) string {
	value := string(uid)
	if value == "" {
		return sandboxEventUnresolved
	}
	if len(value) == 36 && value[8] == '-' && value[13] == '-' && value[18] == '-' && value[23] == '-' &&
		sandboxEventHex(value[:8]+value[9:13]+value[14:18]+value[19:23]+value[24:]) {
		return value
	}
	return "sha256:" + sandboxEventDigest(value)
}

func sandboxEventRevision(revision string) string {
	if revision == "" {
		return sandboxEventUnresolved
	}
	if len(revision) == sha256.Size*2 && sandboxEventHex(revision) {
		return revision
	}
	return "sha256:" + sandboxEventDigest(revision)
}

func sandboxEventHex(value string) bool {
	for _, char := range value {
		if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}

func sandboxEventDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

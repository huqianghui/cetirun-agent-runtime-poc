// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxplacement

import (
	"context"
	"encoding/json"
	"slices"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxbinding"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	strictjson "sigs.k8s.io/json"
)

func (r *registry) agentAssignment(pod *corev1.Pod) (*nodeReservation, *failure) {
	var assigned *nodeReservation
	for _, node := range r.Nodes {
		reservation := node.Reservation
		if reservation == nil {
			continue
		}
		// Namespace membership survives clearing owner metadata. Owner UIDs
		// only impose restrictions; they never establish placement authority.
		claimed := pod.Namespace == reservation.Namespace.Name
		for _, owner := range pod.OwnerReferences {
			if owner.UID == reservation.Agent.UID || owner.UID == reservation.StatefulSet.UID {
				claimed = true
			}
		}
		if !claimed {
			continue
		}
		if assigned != nil && *assigned != *reservation {
			return nil, denied("AgentAssignmentAmbiguous", "Pod namespace or ownership claims conflicting registered Agent assignments")
		}
		assigned = reservation
	}
	return assigned, nil
}

func (s *requestState) validateAgentPod(ctx context.Context, stateNamespace string, registry *registry, reservation *nodeReservation, pod *corev1.Pod, node *corev1.Node, binding bool) *failure {
	if pod.Namespace != reservation.Namespace.Name {
		return denied("ReservedNode", "destination Node is reserved for another workload identity")
	}
	namespace := &corev1.Namespace{}
	if failure := s.read(ctx, client.ObjectKey{Name: reservation.Namespace.Name}, namespace, "Namespace"); failure != nil {
		return failure
	}
	expected, failure := s.readAgentExpectation(ctx, stateNamespace, namespace, false)
	if failure != nil {
		return failure
	}
	if expected.Agent != reservation.Agent || expected.Namespace != reservation.Namespace || expected.Workload != reservation.StatefulSet {
		return denied("ExpectationMismatch", "controller expectation does not match the exact registered workload identities")
	}
	if expected.Placement != nil {
		if _, failure := s.readAssignment(ctx, registry, *expected); failure != nil {
			return failure
		}
	}
	owner := sandboxworkload.AgentOwner(reservation.Agent.Name, reservation.Agent.UID)
	agent := &api.Agent{}
	if failure := s.read(ctx, client.ObjectKey{Name: reservation.Agent.Name}, agent, "Agent"); failure != nil {
		return failure
	}
	authorized, err := sandboxbinding.AuthorizedBinding(agent)
	if err != nil || agent.UID != reservation.Agent.UID || agent.Generation != expected.AgentGeneration || authorized != expected.Binding {
		return denied("AgentMismatch", "current Agent identity or authorization differs from the controller expectation")
	}
	// Binding does not pass through the ordinary Pod CREATE validator. It must
	// not schedule an earlier pending Pod after its execution hold was restored.
	if binding && (expected.ExecutionHeld || agent.Spec.Suspended || slices.Contains(agent.Finalizers, sandbox.HoldFinalizer)) {
		return denied("ExecutionHeld", "an execution-held Agent Pod cannot be bound")
	}
	workload := &appsv1.StatefulSet{}
	if failure := s.read(ctx, client.ObjectKey{Namespace: namespace.Name, Name: reservation.StatefulSet.Name}, workload, "StatefulSet"); failure != nil {
		return failure
	}
	selector, err := metav1.LabelSelectorAsSelector(workload.Spec.Selector)
	if workload.UID != reservation.StatefulSet.UID || !equality.Semantic.DeepEqual(workload.OwnerReferences, []metav1.OwnerReference{owner}) ||
		workload.Annotations[sandboxworkload.AgentUIDAnnotation] != string(agent.UID) ||
		workload.Annotations[sandboxworkload.RevisionAnnotation] != expected.Revision ||
		ptr.Deref(workload.Spec.Replicas, 0) != 1 || workload.Spec.ServiceName != workload.Name ||
		len(workload.Spec.VolumeClaimTemplates) != 0 || workload.Status.UpdateRevision != expected.ControllerRevision ||
		err != nil || selector.Empty() || !selector.Matches(labels.Set(expected.Template.Labels)) ||
		!sandboxworkload.SamePodTemplate(workload.Spec.Template, expected.Template) {
		return denied("StatefulSetMismatch", "current StatefulSet ownership, selector, lifecycle intent or template differs from the expectation")
	}
	if !NodeMatchesSpec(node, &expected.Template.Spec) ||
		(expected.Template.Spec.NodeName != "" && expected.Template.Spec.NodeName != node.Name) {
		return denied("AgentSchedulingMismatch", "registered Node does not satisfy the Agent template's scheduling intent")
	}
	// Only the destination already checked against the registry is normalized.
	// Every other final Pod field is checked by the existing workload contract.
	if pod.Spec.NodeName != "" {
		expected.Template.Spec.NodeName = node.Name
	}
	if failure := validateAgentTemplate(pod, node, *expected); failure != nil {
		return failure
	}
	if expected.Template.Spec.ServiceAccountName == "" {
		return denied("ServiceAccountInvalid", "Agent template must identify its ServiceAccount")
	}
	account := &corev1.ServiceAccount{}
	return s.read(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: expected.Template.Spec.ServiceAccountName}, account, "ServiceAccount")
}

func (s *requestState) validateOptionalAgentPlacement(ctx context.Context, stateNamespace string, registry *registry, pod *corev1.Pod) *failure {
	namespace := &corev1.Namespace{}
	if failure := s.read(ctx, client.ObjectKey{Name: pod.Namespace}, namespace, "Namespace"); failure != nil {
		return failure
	}
	// Look up the protected state independently of both registry reservations
	// and Pod/namespace markers, which must not hide a recorded binding.
	expected, failure := s.readAgentExpectation(ctx, stateNamespace, namespace, true)
	if failure != nil || expected == nil {
		return failure
	}
	if _, failure := s.readAssignment(ctx, registry, *expected); failure != nil {
		return failure
	}
	if err := sandboxworkload.ValidatePod(pod, *expected); err != nil {
		return denied("AgentTemplateMismatch", "Pod differs from the controller-owned Agent template")
	}
	return nil
}

func (s *requestState) readAgentExpectation(ctx context.Context, stateNamespace string, namespace *corev1.Namespace, optionalPlacement bool) (*sandboxworkload.Expectation, *failure) {
	state := &corev1.ConfigMap{}
	if failure := s.read(ctx, client.ObjectKey{Namespace: stateNamespace,
		Name: sandboxworkload.PodExpectationName(namespace.Name, namespace.UID)}, state, "Expectation"); failure != nil {
		if optionalPlacement && failure.reason == "ExpectationMissing" {
			return nil, nil
		}
		return nil, failure
	}
	raw := state.Data[sandboxworkload.PodExpectationKey]
	var expected sandboxworkload.Expectation
	if len(raw) == 0 || len(raw) > maxExpectationBytes || len(state.Data) != 1 || len(state.BinaryData) != 0 {
		return nil, denied("ExpectationInvalid", "reserved placement requires a bounded controller-owned Pod expectation")
	}
	problems, err := strictjson.UnmarshalStrict([]byte(raw), &expected)
	if err != nil || len(problems) != 0 {
		return nil, denied("ExpectationInvalid", "controller expectation contains malformed, unknown or duplicate fields")
	}
	var lifecycle struct {
		ExecutionHeld *bool           `json:"executionHeld"`
		Placement     json.RawMessage `json:"placement"`
	}
	if json.Unmarshal([]byte(raw), &lifecycle) != nil || (len(lifecycle.Placement) != 0 && expected.Placement == nil) {
		return nil, denied("ExpectationInvalid", "controller expectation must record a complete placement binding when the field is present")
	}
	if optionalPlacement && expected.Placement == nil {
		return nil, nil
	}
	owner := sandboxworkload.AgentOwner(expected.Agent.Name, expected.Agent.UID)
	if namespace.Name == stateNamespace || namespace.Labels[sandboxworkload.ProtectedNamespaceLabel] != "true" ||
		namespace.Annotations[sandboxworkload.AgentNameAnnotation] != expected.Agent.Name ||
		namespace.Annotations[sandboxworkload.AgentUIDAnnotation] != string(expected.Agent.UID) ||
		!equality.Semantic.DeepEqual(namespace.OwnerReferences, []metav1.OwnerReference{owner}) {
		return nil, denied("NamespaceMismatch", "reserved workload namespace differs from its registered Agent ownership")
	}
	if expected.Version != sandboxworkload.ExpectationVersion || !validIdentity(expected.Agent) || !validIdentity(expected.Workload) ||
		expected.Namespace != (sandboxworkload.ObjectIdentity{Name: namespace.Name, UID: namespace.UID}) ||
		expected.WorkloadKind != sandboxworkload.AgentWorkload || expected.AgentGeneration <= 0 ||
		expected.Revision == "" || expected.ControllerRevision == "" || expected.Binding.AgentUID != expected.Agent.UID ||
		state.Annotations[sandboxworkload.RevisionAnnotation] != expected.Revision ||
		state.Annotations[sandboxworkload.AgentUIDAnnotation] != string(expected.Agent.UID) ||
		!equality.Semantic.DeepEqual(state.OwnerReferences, []metav1.OwnerReference{owner}) {
		return nil, denied("ExpectationMismatch", "controller expectation does not match the exact registered workload identities")
	}
	if lifecycle.ExecutionHeld == nil {
		return nil, denied("ExpectationInvalid", "controller expectation must explicitly record its execution hold")
	}
	return &expected, nil
}

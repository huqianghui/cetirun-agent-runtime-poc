// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxplacement

import (
	"context"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// ValidateScheduledPod compares a protected Pod with its caller-verified
// expectation, allowing topology additions only from its registered Node.
// The caller must still authorize the lifecycle, sources and previous Pod.
func (h *Handler) ValidateScheduledPod(ctx context.Context, request admission.Request, pod *corev1.Pod, expected sandboxworkload.Expectation) admission.Response {
	if failure := h.checkScheduledPod(ctx, pod, expected); failure != nil {
		return reject(ctx, request, failure)
	}
	return admission.Allowed("protected Pod template matches its current registered Node")
}

// CheckRunningPod reuses placement and topology validation after scheduling.
// The caller must independently pin the persisted Pod UID and execution epoch;
// this verifies current API inputs, not runtime or network enforcement.
func (h *Handler) CheckRunningPod(ctx context.Context, pod *corev1.Pod, expected sandboxworkload.Expectation) error {
	if pod == nil || pod.UID == "" || pod.ResourceVersion == "" || !pod.DeletionTimestamp.IsZero() {
		return invalid("InvalidRunningPod", "running Pod must have an active persisted identity")
	}
	expected.Template = *expected.Template.DeepCopy()
	if expected.Template.Spec.NodeName == "" {
		expected.Template.Spec.NodeName = pod.Spec.NodeName
	}
	if failure := h.checkScheduledPod(ctx, pod, expected); failure != nil {
		return failure
	}
	return nil
}

func (h *Handler) checkScheduledPod(ctx context.Context, pod *corev1.Pod, expected sandboxworkload.Expectation) *failure {
	ctx, cancel := context.WithTimeout(ctx, h.config.RequestTimeout)
	defer cancel()
	if pod == nil || pod.Spec.NodeName == "" {
		return invalid("InvalidPod", "scheduled Pod validation requires an explicit destination")
	}
	state := &requestState{reader: h.reader}
	registry, failure := state.readRegistry(ctx, h.config)
	if failure != nil {
		return failure
	}
	if expected.Placement != nil {
		if _, failure := state.readAssignment(ctx, registry, expected); failure != nil {
			return failure
		}
	}
	node, registered, failure := state.readNode(ctx, registry, pod.Spec.NodeName)
	if failure != nil {
		return failure
	}
	if registered.Reservation == nil || *registered.Reservation != (nodeReservation{
		Agent: expected.Agent, Namespace: expected.Namespace, StatefulSet: expected.Workload,
	}) {
		return denied("AgentPlacementMismatch", "protected Pod requires its exact registered reservation")
	}
	if failure := validateAgentTemplate(pod, node, expected); failure != nil {
		return failure
	}
	if failure := state.recheck(ctx); failure != nil {
		return failure
	}
	return nil
}

func validateAgentTemplate(pod *corev1.Pod, node *corev1.Node, expected sandboxworkload.Expectation) *failure {
	expected.Template = *expected.Template.DeepCopy()
	if !applyNodeTopology(pod, node, &expected.Template) {
		return denied("AgentTopologyMismatch", "Agent Pod topology labels differ from the current pinned Node")
	}
	if err := sandboxworkload.ValidatePod(pod, expected); err != nil {
		return denied("AgentTemplateMismatch", "Pod differs from the controller-owned Agent template")
	}
	return nil
}

func applyNodeTopology(pod *corev1.Pod, node *corev1.Node, template *corev1.PodTemplateSpec) bool {
	if pod.Spec.NodeName == "" {
		return true
	}
	if pod.Spec.NodeName != node.Name {
		return false
	}
	for _, key := range []string{corev1.LabelTopologyRegion, corev1.LabelTopologyZone} {
		value, present := pod.Labels[key]
		if !present {
			continue
		}
		current, found := node.Labels[key]
		if !found || value != current {
			return false
		}
		// Never override an explicit template value or discard incoming labels.
		if _, pinned := template.Labels[key]; !pinned {
			if template.Labels == nil {
				template.Labels = map[string]string{}
			}
			template.Labels[key] = current
		}
	}
	return true
}

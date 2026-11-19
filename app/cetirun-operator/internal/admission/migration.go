// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxadmission

import (
	"context"
	"slices"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	strictjson "sigs.k8s.io/json"
)

func (h *PodHandler) migrationAgent(ctx context.Context, namespace string) (*api.Agent, *corev1.Namespace, *podFailure) {
	ns := &corev1.Namespace{}
	if err := h.reader.Get(ctx, client.ObjectKey{Name: namespace}, ns); err != nil {
		return nil, nil, podReadFailure("Namespace", err)
	}
	agent := &api.Agent{}
	if err := h.reader.Get(ctx, client.ObjectKey{Name: ns.Annotations[w.AgentNameAnnotation]}, agent); err != nil {
		return nil, nil, podReadFailure("Agent", err)
	}
	if namespace == h.config.StateNamespace || ns.Labels[w.ProtectedNamespaceLabel] != "true" ||
		ns.Annotations[w.AgentUIDAnnotation] != string(agent.UID) ||
		!equality.Semantic.DeepEqual(ns.OwnerReferences, []metav1.OwnerReference{w.AgentOwner(agent.Name, agent.UID)}) {
		return nil, nil, podDenied("MigrationOwnershipChanged", "migration namespace ownership is not current")
	}
	return agent, ns, nil
}

func migrationFinalizerChange(request admission.Request, pod *corev1.Pod) (bool, bool) {
	if request.Operation != admissionv1.Update || request.SubResource != "" ||
		request.RequestSubResource != "" || pod.UID == "" || pod.ResourceVersion == "" {
		return false, false
	}
	old := &corev1.Pod{}
	if decodePod(request.OldObject.Raw, old) != nil {
		return false, false
	}
	add := slices.Contains(pod.Finalizers, api.MigrationPodFinalizer)
	if add == slices.Contains(old.Finalizers, api.MigrationPodFinalizer) {
		return false, false
	}
	current := pod.DeepCopy()
	old.Finalizers = slices.DeleteFunc(old.Finalizers, func(s string) bool { return s == api.MigrationPodFinalizer })
	current.Finalizers = slices.DeleteFunc(current.Finalizers, func(s string) bool { return s == api.MigrationPodFinalizer })
	if len(old.Finalizers) == 0 {
		old.Finalizers = nil
	}
	if len(current.Finalizers) == 0 {
		current.Finalizers = nil
	}
	old.ManagedFields, current.ManagedFields = nil, nil
	return equality.Semantic.DeepEqual(old, current), add
}

func (h *PodHandler) validateMigrationFinalizer(ctx context.Context, request admission.Request, pod *corev1.Pod, add bool) admission.Response {
	agent, namespace, failure := h.migrationAgent(ctx, pod.Namespace)
	if failure != nil {
		return rejectPod(ctx, request, failure)
	}
	state := agent.Status.PlannedMigration
	owner := metav1.GetControllerOf(pod)
	if state == nil || agent.Spec.PlannedMigration == nil || state.PlannedMigrationRequest != *agent.Spec.PlannedMigration ||
		state.SourcePod != (api.MigrationObject{Name: pod.Name, UID: pod.UID}) ||
		state.Namespace != (api.MigrationObject{Name: namespace.Name, UID: namespace.UID}) ||
		owner == nil || owner.Kind != "StatefulSet" || owner.Name != state.Workload.Name || owner.UID != state.Workload.UID ||
		(add && (state.Phase != api.MigrationProtecting || !pod.DeletionTimestamp.IsZero())) ||
		(!add && (!state.SourceTerminated() || pod.DeletionTimestamp.IsZero())) {
		return rejectPod(ctx, request, podDenied("MigrationUnacknowledged", "migration finalization requires its exact persisted checkpoint"))
	}
	current := &corev1.Pod{}
	if err := h.reader.Get(ctx, client.ObjectKeyFromObject(pod), current); err != nil {
		return rejectPod(ctx, request, podReadFailure("MigrationPod", err))
	}
	if current.UID != pod.UID || current.ResourceVersion != pod.ResourceVersion {
		return rejectPod(ctx, request, podDenied("MigrationPodChanged", "migration requires the current source Pod"))
	}
	return admission.Allowed("only the migration source finalizer changes under its durable checkpoint")
}

func (h *PodHandler) validateMigrationBinding(ctx context.Context, request admission.Request) admission.Response {
	ctx, cancel := context.WithTimeout(ctx, h.config.RequestTimeout)
	defer cancel()
	binding := &corev1.Binding{}
	problems, err := strictjson.UnmarshalStrict(request.Object.Raw, binding)
	if err != nil || len(problems) != 0 || binding.Name == "" || request.Namespace == "" ||
		(request.Name != "" && binding.Name != request.Name) || binding.Namespace != request.Namespace ||
		binding.Target.Name == "" || binding.Target.Namespace != "" ||
		(binding.Target.Kind != "" && binding.Target.Kind != "Node") {
		return rejectPod(ctx, request, podDenied("InvalidMigrationBinding", "invalid Pod binding request"))
	}
	agent, namespace, failure := h.migrationAgent(ctx, request.Namespace)
	if failure != nil {
		return rejectPod(ctx, request, failure)
	}
	if agent.Spec.PlannedMigration == nil && agent.Status.PlannedMigration == nil {
		return admission.Allowed("ordinary scheduling has no planned migration target")
	}
	pod := &corev1.Pod{}
	if err := h.reader.Get(ctx, client.ObjectKey{Namespace: request.Namespace, Name: binding.Name}, pod); err != nil {
		return rejectPod(ctx, request, podReadFailure("MigrationPod", err))
	}
	owner := metav1.GetControllerOf(pod)
	if owner == nil || owner.Kind != "StatefulSet" {
		return admission.Allowed("planned HOME migration does not pin auxiliary workloads")
	}
	state := agent.Status.PlannedMigration
	if state == nil && agent.MigrationNeedsCapture() && pod.Annotations[api.AnnotationMigrationRequest] == "" &&
		!agent.Spec.Suspended && !slices.Contains(agent.Finalizers, sandbox.HoldFinalizer) &&
		agent.DeletionTimestamp.IsZero() && namespace.DeletionTimestamp.IsZero() {
		return admission.Allowed("an uncaptured migration retains ordinary source scheduling")
	}
	if state == nil || agent.Spec.PlannedMigration == nil ||
		(state.PlannedMigrationRequest != *agent.Spec.PlannedMigration && !agent.MigrationNeedsCapture()) ||
		!state.CanStart() || agent.Spec.Suspended || slices.Contains(agent.Finalizers, sandbox.HoldFinalizer) ||
		!agent.DeletionTimestamp.IsZero() || !namespace.DeletionTimestamp.IsZero() ||
		state.Namespace != (api.MigrationObject{Name: namespace.Name, UID: namespace.UID}) ||
		state.Workload != (api.MigrationObject{Name: owner.Name, UID: owner.UID}) ||
		pod.UID == state.SourcePod.UID || pod.Annotations[api.AnnotationMigrationRequest] != state.RequestID ||
		binding.Target.Name != state.Target.Name || (binding.Target.UID != "" && binding.Target.UID != state.Target.UID) {
		return rejectPod(ctx, request, podDenied("MigrationBindingHeld", "migration binding requires an acknowledged move to the exact destination"))
	}
	node := &corev1.Node{}
	if err := h.reader.Get(ctx, client.ObjectKey{Name: state.Target.Name}, node); err != nil {
		return rejectPod(ctx, request, podReadFailure("MigrationNode", err))
	}
	if node.UID != state.Target.UID || node.Spec.ProviderID != state.Destination.ProviderID ||
		!node.DeletionTimestamp.IsZero() || node.Spec.Unschedulable ||
		!slices.ContainsFunc(node.Status.Conditions, func(c corev1.NodeCondition) bool {
			return c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue
		}) {
		return rejectPod(ctx, request, podDenied("MigrationNodeChanged", "migration destination Node is unavailable or replaced"))
	}
	return admission.Allowed("migration binds only to its current UID-pinned destination")
}

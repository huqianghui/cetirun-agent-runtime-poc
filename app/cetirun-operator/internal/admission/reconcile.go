// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxadmission

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/cetiadmin"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxbinding"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	admissionv1 "k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func (h *PodHandler) reconciledExpectation(ctx context.Context, namespace string) (w.ReconcileExpectation, *corev1.ConfigMap, bool, *podFailure) {
	var expected w.ReconcileExpectation
	ns := &corev1.Namespace{}
	if err := h.reader.Get(ctx, client.ObjectKey{Name: namespace}, ns); err != nil {
		return expected, nil, true, podReadFailure("Namespace", err)
	}
	state := &corev1.ConfigMap{}
	if err := h.reader.Get(ctx, client.ObjectKey{
		Namespace: h.config.StateNamespace, Name: w.PodExpectationName(namespace, ns.UID),
	}, state); err != nil {
		return expected, nil, true, podReadFailure("Expectation", err)
	}
	var header struct {
		Version string `json:"version"`
	}
	if json.Unmarshal([]byte(state.Data[w.PodExpectationKey]), &header) != nil || header.Version != w.ReconcileExpectationVersion {
		return expected, state, false, nil
	}
	expected, err := w.ReadReconcileExpectation(state)
	if err != nil {
		return expected, state, true, podDenied("ExpectationInvalid", "invalid current Sandbox expectation")
	}
	if ns.Name == h.config.StateNamespace || !ns.DeletionTimestamp.IsZero() ||
		ns.Labels[w.ProtectedNamespaceLabel] != "true" ||
		expected.Namespace != (w.ObjectIdentity{Name: ns.Name, UID: ns.UID}) ||
		ns.Annotations[w.AgentNameAnnotation] != expected.Agent.Name ||
		ns.Annotations[w.AgentUIDAnnotation] != string(expected.Agent.UID) ||
		!equality.Semantic.DeepEqual(ns.OwnerReferences, []metav1.OwnerReference{w.AgentOwner(expected.Agent.Name, expected.Agent.UID)}) {
		return expected, state, true, podDenied("NamespaceOwnershipChanged", "Sandbox namespace ownership changed")
	}
	return expected, state, true, nil
}

func (h *PodHandler) validateReconciledSources(ctx context.Context, expected w.ReconcileExpectation) *podFailure {
	if len(expected.Sources) < 6 || len(expected.SourceDigests) == 0 {
		return podDenied("SourceInventoryInvalid", "current Sandbox source inventory is incomplete")
	}
	legacy := w.Expectation{
		Agent: expected.Agent, Namespace: expected.Namespace, Binding: expected.Binding,
		Home: expected.Home, SinglePod: true, Sources: expected.Sources, SourceDigests: expected.SourceDigests,
	}
	for _, workload := range expected.Workloads {
		if workload.Kind == "StatefulSet" {
			legacy.Template, legacy.Overhead = workload.Template, workload.Overhead
		}
	}
	for _, source := range expected.Sources {
		if failure := h.readPodSource(ctx, legacy, source); failure != nil {
			return failure
		}
	}
	return nil
}

func (h *PodHandler) validateReconciledKubernetesGrants(ctx context.Context, expected w.ReconcileExpectation, confirmed bool) *podFailure {
	bindings, err := expected.KubernetesBindings()
	if err != nil {
		return podDenied("GrantInventoryInvalid", "current Kubernetes grant inventory is invalid")
	}
	journal := sandboxpolicy.GrantJournalStore{APIReader: h.reader, Namespace: h.config.StateNamespace,
		PlatformAccessVerifier: cetiadmin.RuntimeAccessBindingVerifier(h.config.CetiAdminSource)}
	verify := journal.VerifyKubernetesBindings
	if !confirmed {
		verify = journal.VerifyKubernetesBindingIntents
	}
	if err := verify(ctx, expected.Agent.Name, expected.Agent.UID, bindings); err != nil {
		return podDenied("KubernetesGrantsUnconfirmed", "current Kubernetes grants do not match confirmed journal authority")
	}
	return nil
}

func (h *PodHandler) validateReconciledPod(ctx context.Context, request admission.Request, pod *corev1.Pod) (admission.Response, bool) {
	expected, state, handled, failure := h.reconciledExpectation(ctx, request.Namespace)
	if !handled {
		return admission.Response{}, false
	}
	reject := func(failure *podFailure) (admission.Response, bool) {
		return rejectPod(ctx, request, failure), true
	}
	if failure != nil {
		return reject(failure)
	}
	agent, binding, err := sandboxbinding.Read(ctx, h.reader, client.ObjectKey{Name: expected.Agent.Name})
	if err != nil || agent.UID != expected.Agent.UID || agent.Generation != expected.AgentGeneration || binding != expected.Binding {
		return reject(podDenied("BindingUnauthorized", "Sandbox binding or desired generation is not current"))
	}
	if agent.Spec.Suspended || !agent.DeletionTimestamp.IsZero() || slices.Contains(agent.Finalizers, sandbox.HoldFinalizer) {
		return reject(podDenied("ExecutionHeld", "Sandbox execution is suspended, held or deleting"))
	}
	if agent.Spec.PlannedMigration != nil && request.Operation == admissionv1.Create && pod.Spec.NodeName != "" {
		return reject(podDenied("MigrationSchedulerRequired", "migrated workloads must use UID-checked scheduler binding"))
	}
	checkReservation := func() *podFailure {
		if h.coordination == nil {
			return nil
		}
		intent, err := sandboxcoordination.ReconciledIntentFor(expected)
		if err == nil {
			err = h.coordination.AuthorizeNativeStart(ctx, intent)
		}
		if err != nil {
			if errors.Is(err, sandboxcoordination.ErrConflict) {
				return podDenied("NativeReservationRequired", "current Sandbox source reservation is unavailable or stopping")
			}
			return podReadFailure("NativeReservation", err)
		}
		return nil
	}
	if failure := checkReservation(); failure != nil {
		return reject(failure)
	}
	if failure := h.validateReconciledSources(ctx, expected); failure != nil {
		return reject(failure)
	}
	if failure := h.validateReconciledKubernetesGrants(ctx, expected, true); failure != nil {
		return reject(failure)
	}
	if desired := expected.IdentityServiceAccount; desired != nil {
		namespace, account := &corev1.Namespace{}, &corev1.ServiceAccount{}
		if err := h.reader.Get(ctx, client.ObjectKey{Name: expected.Namespace.Name}, namespace); err != nil {
			return reject(podReadFailure("IdentityNamespace", err))
		}
		if err := h.reader.Get(ctx, client.ObjectKeyFromObject(desired), account); err != nil {
			return reject(podReadFailure("IdentityServiceAccount", err))
		}
		if err := expected.ValidateIdentityBinding(namespace, account, w.ObjectIdentity{Name: desired.Name, UID: desired.UID}); err != nil {
			return reject(podDenied("IdentityBindingMismatch", "current ServiceAccount differs from the recorded identity binding"))
		}
	}
	for _, desired := range expected.Controls {
		actual := &unstructured.Unstructured{}
		actual.SetGroupVersionKind(desired.GroupVersionKind())
		if err := h.reader.Get(ctx, client.ObjectKeyFromObject(&desired), actual); err != nil {
			return reject(podReadFailure("Control", err))
		}
		if !w.ReconciledObjectMatches(actual, &desired) {
			return reject(podDenied("ControlMismatch", "current Sandbox controls differ from the authorized plan"))
		}
	}
	if len(pod.OwnerReferences) != 1 || pod.OwnerReferences[0].APIVersion != "apps/v1" ||
		pod.OwnerReferences[0].UID == "" || !ptr.Deref(pod.OwnerReferences[0].Controller, false) ||
		!ptr.Deref(pod.OwnerReferences[0].BlockOwnerDeletion, false) {
		return reject(podDenied("WorkloadOwnership", "Pod requires one current workload controller"))
	}
	owner := pod.OwnerReferences[0]
	workloadName, kind, revision := owner.Name, owner.Kind, ""
	var replicaSet *appsv1.ReplicaSet
	if kind == "ReplicaSet" {
		replicaSet = &appsv1.ReplicaSet{}
		if err := h.reader.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: owner.Name}, replicaSet); err != nil {
			return reject(podReadFailure("ReplicaSet", err))
		}
		if replicaSet.UID != owner.UID || !replicaSet.DeletionTimestamp.IsZero() || len(replicaSet.OwnerReferences) != 1 ||
			replicaSet.OwnerReferences[0].APIVersion != "apps/v1" || replicaSet.OwnerReferences[0].Kind != "Deployment" ||
			!ptr.Deref(replicaSet.OwnerReferences[0].Controller, false) || !ptr.Deref(replicaSet.OwnerReferences[0].BlockOwnerDeletion, false) {
			return reject(podDenied("WorkloadOwnership", "ReplicaSet ownership is not current"))
		}
		kind, workloadName = "Deployment", replicaSet.OwnerReferences[0].Name
		revision = replicaSet.Labels["pod-template-hash"]
		if replicaSet.Spec.Template.Labels["pod-template-hash"] != revision || replicaSet.Name != workloadName+"-"+revision {
			return reject(podDenied("WorkloadOwnership", "ReplicaSet revision does not match its template"))
		}
	}
	index := slices.IndexFunc(expected.Workloads, func(workload w.RenderedWorkload) bool {
		return workload.Kind == kind && workload.Identity.Name == workloadName
	})
	if index < 0 {
		return reject(podDenied("WorkloadUnapproved", "Pod controller is not in the authorized chart"))
	}
	wanted := expected.Workloads[index]
	if wanted.Replicas != 1 {
		return reject(podDenied("ExecutionHeld", "this component is not eligible to start"))
	}
	var current client.Object
	var template corev1.PodTemplateSpec
	if kind == "StatefulSet" {
		workload := &appsv1.StatefulSet{}
		if err := h.reader.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: workloadName}, workload); err != nil {
			return reject(podReadFailure("Workload", err))
		}
		if workload.UID != owner.UID || workload.Spec.ServiceName != workloadName ||
			len(workload.Spec.VolumeClaimTemplates) != 0 || ptr.Deref(workload.Spec.Replicas, 0) != 1 {
			return reject(podDenied("WorkloadMismatch", "StatefulSet execution differs from desired state"))
		}
		current, template, revision = workload, workload.Spec.Template, workload.Status.UpdateRevision
	} else {
		workload := &appsv1.Deployment{}
		if err := h.reader.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: workloadName}, workload); err != nil {
			return reject(podReadFailure("Workload", err))
		}
		if replicaSet == nil || workload.UID != replicaSet.OwnerReferences[0].UID || ptr.Deref(workload.Spec.Replicas, 0) != 1 {
			return reject(podDenied("WorkloadMismatch", "Deployment execution differs from desired state"))
		}
		rsTemplate := replicaSet.Spec.Template.DeepCopy()
		rsTemplate.Labels = maps.Clone(rsTemplate.Labels)
		delete(rsTemplate.Labels, "pod-template-hash")
		if !w.SamePodTemplate(*rsTemplate, workload.Spec.Template) {
			return reject(podDenied("WorkloadMismatch", "ReplicaSet is not the current desired Deployment"))
		}
		current, template = workload, workload.Spec.Template
	}
	if !current.GetDeletionTimestamp().IsZero() ||
		(wanted.Identity.UID != "" && current.GetUID() != wanted.Identity.UID) ||
		!equality.Semantic.DeepEqual(current.GetOwnerReferences(), []metav1.OwnerReference{w.AgentOwner(agent.Name, agent.UID)}) ||
		!w.SamePodTemplate(template, wanted.Template) {
		return reject(podDenied("WorkloadMismatch", "workload identity or template differs from current controller state"))
	}
	if request.Operation == admissionv1.Update {
		old := &corev1.Pod{}
		persisted := &corev1.Pod{}
		if decodePod(request.OldObject.Raw, old) != nil {
			return reject(podDenied("PreviousPodMismatch", "invalid previous Pod"))
		}
		if err := h.reader.Get(ctx, client.ObjectKeyFromObject(pod), persisted); err != nil {
			return reject(podReadFailure("PreviousPod", err))
		}
		if old.UID != pod.UID || old.UID != persisted.UID || pod.ResourceVersion != persisted.ResourceVersion ||
			old.ResourceVersion != persisted.ResourceVersion || old.Spec.NodeName != persisted.Spec.NodeName ||
			pod.Spec.NodeName != persisted.Spec.NodeName {
			return reject(podDenied("PreviousPodMismatch", "Pod update does not match its persisted identity"))
		}
	}
	var validationErr error
	if request.Operation == admissionv1.Update && pod.Spec.NodeName != "" {
		node, failure := h.readAssignedNode(ctx, pod)
		if failure != nil {
			return reject(failure)
		}
		validationErr = w.ValidateRenderedPodOnNode(pod, node, wanted, owner, revision)
	} else {
		validationErr = w.ValidateRenderedPod(pod, wanted, owner, revision)
	}
	if validationErr != nil {
		message := "Pod differs from the current authorized chart template"
		var mismatch *w.PodMismatchError
		if errors.As(validationErr, &mismatch) {
			message = mismatch.Error()
		}

		return reject(podDenied("TemplateMismatch", message))
	}
	confirmed := &corev1.ConfigMap{}
	if err := h.reader.Get(ctx, client.ObjectKeyFromObject(state), confirmed); err != nil {
		return reject(podReadFailure("Expectation", err))
	}
	if confirmed.UID != state.UID || confirmed.ResourceVersion != state.ResourceVersion {
		return reject(podDenied("ExpectationChanged", "desired state changed during Pod admission"))
	}
	if failure := h.validateReconciledSources(ctx, expected); failure != nil {
		return reject(failure)
	}
	if failure := h.validateReconciledKubernetesGrants(ctx, expected, true); failure != nil {
		return reject(failure)
	}
	if failure := checkReservation(); failure != nil {
		return reject(failure)
	}
	return admission.Allowed("Pod matches current authorized common chart output"), true
}

func (h *PodHandler) readAssignedNode(ctx context.Context, pod *corev1.Pod) (*corev1.Node, *podFailure) {
	if pod.Spec.NodeName == "" {
		return nil, podDenied("NodeUnavailable", "the current Pod has no assigned Node")
	}
	node := &corev1.Node{}
	if err := h.reader.Get(ctx, client.ObjectKey{Name: pod.Spec.NodeName}, node); err != nil {
		return nil, podReadFailure("Node", err)
	}
	if node.UID == "" || !node.DeletionTimestamp.IsZero() {
		return nil, podDenied("NodeUnavailable", "the assigned Node is unavailable")
	}
	return node, nil
}

func controllerRuntimeDefaults(agent, old *api.Agent) bool {
	if old == nil || agent.UID != old.UID {
		return false
	}
	a, b := agent.DeepCopy(), old.DeepCopy()
	changed := false
	if b.Spec.Helm.Version == "" && a.Spec.Helm.Version != "" {
		b.Spec.Helm.Version, changed = a.Spec.Helm.Version, true
	}
	if b.Spec.Agent.Image == nil && a.Spec.Agent.Image != nil && *a.Spec.Agent.Image != "" {
		b.Spec.Agent.Image, changed = a.Spec.Agent.Image, true
	}
	if b.Spec.Agent.Port == nil && a.Spec.Agent.Port != nil && *a.Spec.Agent.Port > 0 && *a.Spec.Agent.Port <= 65535 {
		b.Spec.Agent.Port, changed = a.Spec.Agent.Port, true
	}
	b.Generation = a.Generation
	a.ManagedFields, b.ManagedFields = nil, nil
	return changed && equality.Semantic.DeepEqual(a, b)
}

func (h *Handler) reconciledHoldRelease(ctx context.Context, request admission.Request, agent, old *api.Agent) (admission.Response, bool) {
	if old == nil || !h.config.NativeExecution || request.UserInfo.Username != h.config.ControllerUsername ||
		!authenticated(request.UserInfo) || !rawMetadataOnly(request) ||
		slices.Contains(old.Finalizers, sandbox.PreparationFinalizer) ||
		!slices.Contains(old.Finalizers, sandbox.BootstrapFinalizer) || !slices.Contains(old.Finalizers, sandbox.HoldFinalizer) {
		return admission.Response{}, false
	}
	expected := old.DeepCopy()
	expected.Finalizers = slices.DeleteFunc(expected.Finalizers, func(value string) bool { return value == sandbox.HoldFinalizer })
	a := agent.DeepCopy()
	expected.ManagedFields, a.ManagedFields = nil, nil
	if !equality.Semantic.DeepEqual(expected, a) {
		return admission.Response{}, false
	}
	handler, err := NewPodHandler(h.reader, PodConfig{StateNamespace: h.config.PlatformNamespace, CetiAdminSource: h.config.cetiAdminSource()})
	if err != nil {
		return dependencyError(ctx, err), true
	}
	record, _, handled, failure := handler.reconciledExpectation(ctx, "cetirun-"+old.Name)
	if failure != nil {
		return rejectPod(ctx, request, failure), true
	}
	if !handled || record.Agent != (w.ObjectIdentity{Name: old.Name, UID: old.UID}) ||
		record.AgentGeneration != old.Generation || old.Spec.Suspended || !old.DeletionTimestamp.IsZero() {
		return admission.Denied("hold release requires the current reconciled Sandbox intent"), true
	}
	if failure := handler.validateReconciledSources(ctx, record); failure != nil {
		return rejectPod(ctx, request, failure), true
	}
	if failure := handler.validateReconciledKubernetesGrants(ctx, record, false); failure != nil {
		return rejectPod(ctx, request, failure), true
	}
	if _, err := h.platformRegistrations(ctx); err != nil {
		return dependencyError(ctx, err), true
	}
	return admission.Allowed("current common reconciliation can release its metadata-only hold"), true
}

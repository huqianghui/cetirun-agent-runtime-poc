// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxadmission

import (
	"context"
	"fmt"
	"slices"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxbinding"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	admissionv1 "k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func nativeHoldReleaseOnly(request admission.Request, agent, old *api.Agent) bool {
	if request.Operation != admissionv1.Update || old == nil || old.UID == "" || old.ResourceVersion == "" ||
		old.Generation < 1 || !old.DeletionTimestamp.IsZero() || old.Spec.Suspended ||
		old.Spec.Agent.Type != api.AgentTypeOpenClaw ||
		!rawMetadataOnly(request) {
		return false
	}
	finalizers := slices.DeleteFunc(slices.Clone(old.Finalizers), func(value string) bool {
		return value == sandbox.HoldFinalizer
	})
	if len(finalizers)+1 != len(old.Finalizers) || !slices.Equal(agent.Finalizers, finalizers) ||
		!slices.Contains(finalizers, sandbox.PreparationFinalizer) || !slices.Contains(finalizers, sandbox.BootstrapFinalizer) {
		return false
	}
	a, b := agent.DeepCopy(), old.DeepCopy()
	a.Finalizers, b.Finalizers, a.ManagedFields, b.ManagedFields = nil, nil, nil, nil
	return equality.Semantic.DeepEqual(a, b)
}

func (h *Handler) validateNativeHoldRelease(ctx context.Context, request admission.Request, old *api.Agent) admission.Response {
	if _, _, err := decodeRequest(request, ValidationStrict); err != nil {
		return admission.Denied("native hold release requires matching Agent admission fields")
	}
	current, binding, err := sandboxbinding.Read(ctx, h.reader, client.ObjectKey{Name: old.Name})
	if err != nil {
		return dependencyError(ctx, err)
	}
	if current.UID != old.UID || current.Generation != old.Generation || current.ResourceVersion != old.ResourceVersion {
		return admission.Denied("native Agent intent changed before hold release")
	}
	if _, err := h.platformRegistrations(ctx); err != nil {
		return dependencyError(ctx, err)
	}
	pods, err := NewPodHandler(h.reader, PodConfig{
		StateNamespace:            h.config.PlatformNamespace,
		CoordinationConfigMapName: h.config.CoordinationConfigMapName,
		CoordinationConfigMapUID:  h.config.CoordinationConfigMapUID,
		RequestTimeout:            h.requestTimeout,
		CetiAdminSource:           h.config.cetiAdminSource(),
	})
	if err != nil {
		return dependencyError(ctx, err)
	}
	namespace := "cetirun-" + old.Name
	expected, state, failure := pods.readExpectation(ctx, namespace)
	if failure != nil {
		return rejectPod(ctx, request, failure)
	}
	if !expected.ExecutionHeld || expected.SinglePod || expected.PodUID != "" ||
		expected.Agent != (w.ObjectIdentity{Name: old.Name, UID: old.UID}) ||
		expected.AgentGeneration != old.Generation || expected.Binding != binding ||
		!slices.Equal(expected.Template.Finalizers, []string{w.NativePodFinalizer}) ||
		!w.ValidInputDigest(expected.Revision) || expected.ValidateSkills() != nil {
		return admission.Denied("native hold release requires the exact held preparation")
	}
	receiptObject := &corev1.ConfigMap{}
	key := client.ObjectKey{Namespace: h.config.PlatformNamespace, Name: w.PreparationReceiptName(old.UID)}
	if err := h.reader.Get(ctx, key, receiptObject); err != nil {
		return dependencyError(ctx, err)
	}
	receipt, err := w.DecodePreparationReceipt(receiptObject, expected.Agent)
	if err != nil {
		return dependencyError(ctx, err)
	}
	inputs := receipt.Prerequisites
	if !receipt.NativeExecution || !receiptObject.DeletionTimestamp.IsZero() ||
		!slices.Contains(receiptObject.Finalizers, sandbox.PreparationFinalizer) ||
		receiptObject.Annotations[w.NativeStartAnnotation] != expected.Revision ||
		inputs.AgentGeneration != old.Generation || inputs.NamespaceUID != expected.Namespace.UID ||
		inputs.Home != expected.Home || inputs.Skills != expected.Skills || inputs.SkillsMode != expected.SkillsMode ||
		inputs.Placement != nil || inputs.RuntimeSecret.Name != old.Spec.GatewayTokenSecretRef ||
		inputs.RuntimeSecret.UID == "" || inputs.ChartVersion == "" ||
		(old.Spec.Helm.Version != "" && inputs.ChartVersion != old.Spec.Helm.Version) {
		return admission.Denied("native hold release requires the current recorded start and lifetime")
	}
	if !slices.ContainsFunc(expected.Sources, func(source w.SourceReference) bool {
		return source.APIVersion == "v1" && source.Kind == "Secret" && source.Namespace == namespace &&
			source.Name == inputs.RuntimeSecret.Name && source.UID == inputs.RuntimeSecret.UID
	}) {
		return admission.Denied("native preparation and admission disagree on the runtime Secret")
	}
	// Use the native effective-input checks without lifting the persisted Pod
	// hold. The controller captured these digests before requesting release.
	snapshot := expected
	snapshot.SinglePod = true
	if failure := pods.validatePodSources(ctx, snapshot, current); failure != nil {
		return rejectPod(ctx, request, failure)
	}
	workload := &appsv1.StatefulSet{}
	if err := h.reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: expected.Workload.Name}, workload); err != nil {
		return dependencyError(ctx, err)
	}
	if workload.UID != expected.Workload.UID || !workload.DeletionTimestamp.IsZero() ||
		!equality.Semantic.DeepEqual(workload.OwnerReferences, []metav1.OwnerReference{w.AgentOwner(old.Name, old.UID)}) ||
		workload.Annotations[w.RevisionAnnotation] != expected.Revision ||
		workload.Annotations[w.AgentUIDAnnotation] != string(old.UID) ||
		ptr.Deref(workload.Spec.Replicas, -1) != 0 ||
		workload.Status.UpdateRevision != expected.ControllerRevision || expected.ControllerRevision == "" ||
		workload.Spec.ServiceName != workload.Name || len(workload.Spec.VolumeClaimTemplates) != 0 ||
		!w.SamePodTemplate(workload.Spec.Template, expected.Template) {
		return admission.Denied("native hold release requires the current stopped workload")
	}
	if h.coordination != nil {
		intent, err := sandboxcoordination.NativeIntentFor(expected)
		if err != nil {
			return dependencyError(ctx, err)
		}
		if err := h.coordination.AuthorizeNativeStart(ctx, intent); err != nil {
			return dependencyError(ctx, err)
		}
	}
	for _, object := range []client.Object{current, state, receiptObject} {
		confirmed := object.DeepCopyObject().(client.Object)
		if err := h.reader.Get(ctx, client.ObjectKeyFromObject(object), confirmed); err != nil {
			return dependencyError(ctx, err)
		}
		if confirmed.GetUID() != object.GetUID() || confirmed.GetResourceVersion() != object.GetResourceVersion() ||
			!confirmed.GetDeletionTimestamp().IsZero() {
			return admission.Denied("native hold-release authority changed during validation")
		}
	}
	if err := ctx.Err(); err != nil {
		return dependencyError(ctx, fmt.Errorf("native hold release validation: %w", err))
	}
	return admission.Allowed("current native preparation permits the controller Agent hold release")
}

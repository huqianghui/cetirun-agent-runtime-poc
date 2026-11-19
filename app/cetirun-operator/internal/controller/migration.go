// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

var errMigrationIntentChanged = errors.New("migration intent changed")
var errMigrationSourceNotReady = errors.New("migration requires a stable single-Pod owned workload")

func (r *AgentReconciler) migrationCheckpoint(ctx context.Context, agent *api.Agent, phase string) error {
	agent.Status.PlannedMigration.Phase = phase
	return r.migrationWaiting(ctx, agent, nil)
}

func (r *AgentReconciler) migrationWaiting(ctx context.Context, agent *api.Agent, cause error) error {
	reason, message := "MigrationPending", "Waiting for acknowledged planned migration progress"
	if cause != nil {
		reason, message = "MigrationBlocked", cause.Error()
	}
	setCondition(&agent.Status.Conditions, api.ConditionPlannedMigrationReady, metav1.ConditionFalse, reason, message)
	if state := agent.Status.PlannedMigration; agent.Spec.Suspended || (state != nil && state.Phase != api.MigrationCompleted) {
		agent.Status.Phase, agent.Status.AccessURL = api.PhaseProvisioning, ""
		if agent.Spec.Suspended {
			agent.Status.Phase = api.PhasePending
		}
		setCondition(&agent.Status.Conditions, api.ConditionReady, metav1.ConditionFalse, reason, message)
	}
	return errors.Join(cause, r.updateStatus(ctx, agent))
}

func (r *AgentReconciler) checkMigrationIntent(ctx context.Context, agent *api.Agent) error {
	current := &api.Agent{}
	if err := r.customReader().Get(ctx, client.ObjectKeyFromObject(agent), current); err != nil {
		return err
	}
	if current.UID != agent.UID || current.Generation != agent.Generation ||
		current.Spec.PlannedMigration == nil || agent.Spec.PlannedMigration == nil ||
		*current.Spec.PlannedMigration != *agent.Spec.PlannedMigration {
		return fmt.Errorf("%w before its side effect", errMigrationIntentChanged)
	}
	if agent.MigrationNeedsCapture() && !equality.Semantic.DeepEqual(current.Status.PlannedMigration, agent.Status.PlannedMigration) {
		return fmt.Errorf("%w: checkpoint changed before source recovery", errMigrationIntentChanged)
	}
	return nil
}

func (r *AgentReconciler) authorizeMigrationSource(ctx context.Context, agent *api.Agent) error {
	if err := r.reconcileMigrationSourceAccess(ctx, agent); err != nil {
		if errors.Is(err, errMigrationIntentChanged) {
			return err
		}
		_, holdErr := r.holdSandbox(ctx, agent)
		return errors.Join(err, holdErr)
	}
	return nil
}

func (r *AgentReconciler) reconcileMigrationSourceAccess(ctx context.Context, agent *api.Agent) error {
	resolution, err := r.resolvePolicy(ctx, agent)
	if err == nil {
		// The new lifecycle request has not been applied yet. Check fresh
		// authorization against the still-running generation's projection,
		// without publishing an acknowledgement for the requested generation.
		observed := agent.DeepCopy()
		observed.Generation = agent.Status.ObservedGeneration
		if observed.Generation == 0 || !policyEnforcementVerified(observed, resolution) {
			err = fmt.Errorf("migration source authorization no longer matches its running projection")
		}
	}
	if err == nil {
		_, err = r.resolveSandboxInputs(ctx, agent, false, true)
	}
	if err != nil {
		err = errors.Join(err, r.isolateMigrationSourceAccess(ctx, agent))
	}
	return err
}

func (r *AgentReconciler) isolateMigrationSourceAccess(ctx context.Context, agent *api.Agent) error {
	if err := r.checkMigrationIntent(ctx, agent); err != nil {
		return err
	}
	var keyErr error
	if r.AIGateway.configured() {
		keyErr = r.beginAIGatewayPolicyReprojection(ctx, agent)
	} else if aiGatewayKeyWasIssued(agent) {
		keyErr = fmt.Errorf("migration source isolation requires the recorded Gateway")
	}
	routeErr := r.withdrawKubernetesSandboxRoute(ctx, agent)
	var networkErr error
	if state := agent.Status.PlannedMigration; state != nil && state.Phase == api.MigrationProtecting {
		networkErr = r.isolateProtectingMigrationNetwork(ctx, agent)
	}
	agent.Status.AccessURL = ""
	return errors.Join(keyErr, routeErr, networkErr)
}

// Deny traffic without deleting an incompletely protected source. The ordinary
// reconciler restores the owned policy only after migration can start again.
func (r *AgentReconciler) isolateProtectingMigrationNetwork(ctx context.Context, agent *api.Agent) error {
	state := agent.Status.PlannedMigration
	namespace := &corev1.Namespace{}
	if err := r.migrationGet(ctx, "", state.Namespace, namespace); err != nil {
		return err
	}
	if namespace.Name != instanceNamespace(agent.Name) || !sandboxBuiltinOwned(namespace, agent) {
		return fmt.Errorf("migration network namespace ownership changed")
	}
	record := &corev1.ConfigMap{}
	if err := r.customReader().Get(ctx, client.ObjectKey{
		Namespace: r.OperatorNamespace, Name: w.PodExpectationName(namespace.Name, namespace.UID),
	}, record); err != nil {
		return err
	}
	expected, err := w.ReadReconcileExpectation(record)
	if err != nil {
		return err
	}
	if expected.Agent.Name != agent.Name || expected.Agent.UID != agent.UID ||
		expected.Namespace.Name != state.Namespace.Name || expected.Namespace.UID != state.Namespace.UID {
		return fmt.Errorf("migration network expectation belongs to another lifetime")
	}
	for _, control := range expected.Controls {
		if control.GetAPIVersion() != "cilium.io/v2" || control.GetKind() != "CiliumNetworkPolicy" {
			continue
		}
		current := &unstructured.Unstructured{}
		current.SetGroupVersionKind(control.GroupVersionKind())
		if err := r.customReader().Get(ctx, client.ObjectKeyFromObject(&control), current); err != nil {
			return err
		}
		selector, found, err := unstructured.NestedMap(current.Object, "spec", "endpointSelector")
		recorded, recordedFound, recordedErr := unstructured.NestedMap(control.Object, "spec", "endpointSelector")
		if err != nil || recordedErr != nil || !found || !recordedFound ||
			!equality.Semantic.DeepEqual(selector, recorded) || current.GetNamespace() != namespace.Name ||
			current.GetUID() == "" || current.GetResourceVersion() == "" ||
			!sandboxBuiltinOwned(current, agent) || !current.GetDeletionTimestamp().IsZero() {
			return fmt.Errorf("migration network policy ownership or selector changed")
		}
		before := current.DeepCopy()
		for field, direction := range map[string]string{"ingressDeny": "fromEntities", "egressDeny": "toEntities"} {
			if err := unstructured.SetNestedSlice(current.Object, []any{map[string]any{direction: []any{"all"}}}, "spec", field); err != nil {
				return err
			}
		}
		if !equality.Semantic.DeepEqual(before.Object["spec"], current.Object["spec"]) {
			if err := r.checkMigrationIntent(ctx, agent); err != nil {
				return err
			}
			if err := r.Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
				return fmt.Errorf("isolate migration source network: %w", err)
			}
		}
		observed := &unstructured.Unstructured{}
		observed.SetGroupVersionKind(current.GroupVersionKind())
		if err := r.customReader().Get(ctx, client.ObjectKeyFromObject(current), observed); err != nil {
			return err
		}
		if observed.GetUID() != current.GetUID() || !observed.GetDeletionTimestamp().IsZero() || !sandboxBuiltinOwned(observed, agent) ||
			!equality.Semantic.DeepEqual(observed.Object["spec"], current.Object["spec"]) {
			return fmt.Errorf("migration source network isolation was not retained")
		}
		return nil
	}
	return fmt.Errorf("migration source has no recorded Cilium policy")
}

func (r *AgentReconciler) migrationFinalizer(ctx context.Context, agent *api.Agent, object client.Object, name string, add bool) error {
	if err := r.checkMigrationIntent(ctx, agent); err != nil {
		return err
	}
	if slices.Contains(object.GetFinalizers(), name) == add {
		return nil
	}
	if add && !object.GetDeletionTimestamp().IsZero() {
		return fmt.Errorf("migration cannot protect an already terminating object")
	}
	before := object.DeepCopyObject().(client.Object)
	finalizers := slices.Clone(object.GetFinalizers())
	if add {
		finalizers = append(finalizers, name)
	} else {
		finalizers = slices.DeleteFunc(finalizers, func(value string) bool { return value == name })
	}
	object.SetFinalizers(finalizers)
	return r.Patch(ctx, object, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

// reconcileMigration returns false while the common executor must stay stopped.
// A checkpoint is persisted before any finalizer, shutdown or replacement effect.
func (r *AgentReconciler) reconcileMigration(ctx context.Context, agent *api.Agent, reconciled bool) (bool, ctrl.Result, error) {
	request, state := agent.Spec.PlannedMigration, agent.Status.PlannedMigration
	if request == nil && state == nil {
		return true, ctrl.Result{}, nil
	}
	wait := ctrl.Result{RequeueAfter: 5 * time.Second}
	if !agent.DeletionTimestamp.IsZero() {
		done, err := r.cleanupMigration(ctx, agent)
		return done, wait, err
	}
	block := func(err error) (bool, ctrl.Result, error) {
		if errors.Is(err, errMigrationIntentChanged) {
			return false, ctrl.Result{}, err
		}
		if state != nil && state.Phase == api.MigrationProtecting {
			// Preserve source observations without retaining an obsolete grant.
			if agent.Spec.Suspended {
				err = errors.Join(err, r.isolateMigrationSourceAccess(ctx, agent))
			} else {
				err = errors.Join(err, r.reconcileMigrationSourceAccess(ctx, agent))
			}
		} else if agent.Spec.Suspended || (state != nil && state.Phase != api.MigrationCompleted) {
			err = errors.Join(err, r.stopReconciledSandboxForMigration(ctx, agent))
		}
		return false, wait, r.migrationWaiting(ctx, agent, err)
	}
	if request == nil || !api.ValidRestartRequestID(request.RequestID) || request.Target.Name == "" || request.Target.UID == "" {
		return block(fmt.Errorf("migration intent is missing or invalid"))
	}
	if agent.MigrationNeedsCapture() {
		if !reconciled || r.SandboxPreparation == nil || !r.SandboxPreparation.PlannedMigration {
			return block(errors.Join(fmt.Errorf("new migrations require explicitly enabled common Sandbox execution"),
				r.authorizeMigrationSource(ctx, agent)))
		}
		// Before capture, ordinary authorization and reconciliation must be
		// able to restore a source held by an earlier dependency outage.
		if !agent.Spec.Suspended && (agent.Status.Phase != api.PhaseReady ||
			!isConditionTrue(agent.Status.Conditions, api.ConditionReady) ||
			!isConditionTrue(agent.Status.Conditions, api.ConditionSandboxReady)) {
			return true, ctrl.Result{}, nil
		}
		if err := r.authorizeMigrationSource(ctx, agent); err != nil {
			return block(err)
		}
		captured, err := r.captureMigration(ctx, agent)
		if err != nil {
			// A stale Ready status must not prevent ordinary, authorized source
			// recovery before this request has captured any migration evidence.
			if errors.Is(err, errMigrationSourceNotReady) && !agent.Spec.Suspended {
				return true, ctrl.Result{}, nil
			}
			return block(err)
		}
		agent.Status.PlannedMigration = captured
		return false, wait, r.migrationCheckpoint(ctx, agent, api.MigrationProtecting)
	}
	if state.PlannedMigrationRequest != *request || state.Target != state.Destination.MigrationObject {
		return block(fmt.Errorf("an accepted migration cannot be replaced by another request"))
	}
	if state.CanStart() {
		if !reconciled {
			return block(fmt.Errorf("recorded migration requires its common-executor reader"))
		}
		if agent.Spec.Suspended && state.Phase != api.MigrationCompleted {
			return block(nil)
		}
		return true, ctrl.Result{}, nil
	}
	if state.Phase != api.MigrationProtecting {
		if err := r.stopReconciledSandboxForMigration(ctx, agent); err != nil {
			return false, wait, r.migrationWaiting(ctx, agent, err)
		}
	}
	volume, err := r.migrationHome(ctx, agent, state)
	if err != nil {
		return block(err)
	}
	workload := &appsv1.StatefulSet{}
	if err := r.migrationGet(ctx, state.Namespace.Name, state.Workload, workload); err != nil {
		return block(err)
	}
	if !sandboxBuiltinOwned(workload, agent) || !workload.DeletionTimestamp.IsZero() {
		return block(fmt.Errorf("migration workload ownership changed"))
	}
	switch state.Phase {
	case api.MigrationProtecting:
		digest, err := sandboxPreparationDigest(workload.Spec.Template)
		if err != nil || digest != state.WorkloadTemplateHash || workload.Spec.Replicas == nil ||
			*workload.Spec.Replicas < 0 || *workload.Spec.Replicas > 1 {
			return block(fmt.Errorf("migration source workload changed before protection"))
		}
		if _, err := r.checkMigrationNode(ctx, state.Source, state.Driver, true); err != nil {
			return block(err)
		}
		if !controllerutil.ContainsFinalizer(agent, sandbox.HoldFinalizer) {
			before := agent.DeepCopy()
			controllerutil.AddFinalizer(agent, sandbox.HoldFinalizer)
			if err := r.Patch(ctx, agent, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
				return block(err)
			}
		}
		pod, err := r.migrationSourcePod(ctx, state)
		if err != nil {
			return block(err)
		}
		if err := r.migrationFinalizer(ctx, agent, pod, api.MigrationPodFinalizer, true); err != nil {
			return block(err)
		}
		attachment := &storagev1.VolumeAttachment{}
		if err := r.migrationGet(ctx, "", state.Attachment, attachment); err != nil {
			return block(err)
		}
		if err := r.migrationFinalizer(ctx, agent, attachment, api.MigrationAttachmentFinalizer, true); err != nil {
			return block(err)
		}
		// Authorization failure can stop the source and clear its readiness.
		// Persist protection and the stop transition before that failure path.
		checkpointErr := r.migrationCheckpoint(ctx, agent, api.MigrationStopping)
		if agent.Spec.Suspended {
			return false, wait, errors.Join(checkpointErr, r.stopReconciledSandboxForMigration(ctx, agent))
		}
		authorizationErr := r.authorizeMigrationSource(ctx, agent)
		if checkpointErr != nil || authorizationErr != nil {
			return false, wait, errors.Join(checkpointErr, authorizationErr)
		}
		return false, wait, nil
	case api.MigrationStopping:
		if _, err := r.checkMigrationNode(ctx, state.Source, state.Driver, true); err != nil {
			return block(err)
		}
		pod, err := r.migrationSourcePod(ctx, state)
		if err != nil {
			return block(fmt.Errorf("source termination is unacknowledged: %w", err))
		}
		if !slices.Contains(pod.Finalizers, api.MigrationPodFinalizer) {
			return block(fmt.Errorf("source termination protection is missing"))
		}
		if !migrationPodTerminated(pod) {
			return false, wait, r.migrationWaiting(ctx, agent, nil)
		}
		return false, wait, r.migrationCheckpoint(ctx, agent, api.MigrationSourceTerminated)
	case api.MigrationSourceTerminated:
		if _, err := r.checkMigrationNode(ctx, state.Source, state.Driver, true); err != nil {
			return block(err)
		}
		if pod, err := r.migrationSourcePod(ctx, state); err == nil {
			if err := r.migrationFinalizer(ctx, agent, pod, api.MigrationPodFinalizer, false); err != nil {
				return block(err)
			}
			return false, wait, r.migrationWaiting(ctx, agent, nil)
		} else if !apierrors.IsNotFound(err) {
			return block(err)
		}
		pods := &corev1.PodList{}
		if err := r.customReader().List(ctx, pods, client.InNamespace(state.Namespace.Name)); err != nil {
			return block(err)
		}
		if len(pods.Items) != 0 {
			return false, wait, r.migrationWaiting(ctx, agent, nil)
		}
		attachment := &storagev1.VolumeAttachment{}
		if err := r.migrationGet(ctx, "", state.Attachment, attachment); err != nil {
			return block(fmt.Errorf("CSI detach is unacknowledged: %w", err))
		}
		if attachment.Spec.Attacher != state.Driver || attachment.Spec.NodeName != state.Source.Name ||
			ptr.Deref(attachment.Spec.Source.PersistentVolumeName, "") != state.Volume.Name ||
			!slices.Contains(attachment.Finalizers, api.MigrationAttachmentFinalizer) {
			return block(fmt.Errorf("source attachment identity or protection changed"))
		}
		if !migrationDetachAcknowledged(attachment, state.Driver) {
			if attachment.Status.DetachError != nil {
				return block(fmt.Errorf("CSI detach has not succeeded"))
			}
			return false, wait, r.migrationWaiting(ctx, agent, nil)
		}
		return false, wait, r.migrationCheckpoint(ctx, agent, api.MigrationDetached)
	case api.MigrationDetached:
		attachment := &storagev1.VolumeAttachment{}
		if err := r.migrationGet(ctx, "", state.Attachment, attachment); err == nil {
			if err := r.migrationFinalizer(ctx, agent, attachment, api.MigrationAttachmentFinalizer, false); err != nil {
				return block(err)
			}
			return false, wait, r.migrationWaiting(ctx, agent, nil)
		} else if !apierrors.IsNotFound(err) {
			return block(err)
		}
		attachments, err := r.migrationAttachments(ctx, state.Volume.Name)
		if err != nil {
			return block(err)
		}
		if len(attachments) != 0 {
			return block(fmt.Errorf("HOME still has an attachment before replacement"))
		}
		if err := r.stopNativeSandboxReservation(ctx, agent); err != nil {
			return block(err)
		}
		node, err := r.checkMigrationNode(ctx, state.Destination, state.Driver, false)
		if err != nil {
			return block(err)
		}
		spec := workload.Spec.Template.Spec.DeepCopy()
		if state.PreviousRequestID != "" {
			if workload.Spec.Template.Annotations[api.AnnotationMigrationRequest] != state.PreviousRequestID {
				return block(fmt.Errorf("previous migration template changed before replacement"))
			}
			if err := runtimebindings.UnpinMigrationNode(spec, state.Source.Name); err != nil {
				return block(err)
			}
		}
		if err := r.migrationScheduling(ctx, node, spec, volume); err != nil {
			return block(err)
		}
		return false, wait, r.migrationCheckpoint(ctx, agent, api.MigrationStarting)
	default:
		return block(fmt.Errorf("unknown migration checkpoint"))
	}
}

func (r *AgentReconciler) checkMigrationApply(ctx context.Context, agent *api.Agent, spec *corev1.PodSpec) error {
	if agent.Spec.PlannedMigration == nil {
		return nil
	}
	state := agent.Status.PlannedMigration
	if state == nil && agent.MigrationNeedsCapture() {
		return r.checkMigrationIntent(ctx, agent)
	}
	if !state.CanStart() || (state.PlannedMigrationRequest != *agent.Spec.PlannedMigration && !agent.MigrationNeedsCapture()) {
		return fmt.Errorf("migration has no acknowledged source shutdown and storage release")
	}
	if agent.Spec.Suspended {
		return nil
	}
	if err := r.checkMigrationIntent(ctx, agent); err != nil {
		return err
	}
	volume, err := r.migrationHome(ctx, agent, state)
	if err != nil {
		return err
	}
	node, err := r.checkMigrationNode(ctx, state.Destination, state.Driver, false)
	if err != nil {
		return err
	}
	if spec == nil {
		return fmt.Errorf("migration requires the final desired workload template")
	}
	if err := r.migrationScheduling(ctx, node, spec, volume); err != nil {
		return err
	}
	attachments, err := r.migrationAttachments(ctx, state.Volume.Name)
	if err != nil {
		return err
	}
	for _, attachment := range attachments {
		if attachment.Spec.NodeName != state.Target.Name || attachment.Spec.Attacher != state.Driver ||
			!attachment.DeletionTimestamp.IsZero() || attachment.Status.AttachError != nil || attachment.Status.DetachError != nil {
			return fmt.Errorf("migration HOME attachment is not limited to its current destination")
		}
	}
	return nil
}

// Destruction never starts a replacement. It releases only our captured
// finalizers, independently of foreground namespace/workload collection.
func (r *AgentReconciler) cleanupMigration(ctx context.Context, agent *api.Agent) (bool, error) {
	state := agent.Status.PlannedMigration
	if state == nil || state.CanStart() {
		return true, nil
	}
	if err := r.stopReconciledSandboxForMigration(ctx, agent); err != nil {
		return false, err
	}
	pod, err := r.migrationSourcePod(ctx, state)
	if err == nil {
		if slices.Contains(pod.Finalizers, api.MigrationPodFinalizer) {
			if !state.SourceTerminated() {
				if _, err := r.checkMigrationNode(ctx, state.Source, state.Driver, true); err != nil {
					return false, err
				}
				if !migrationPodTerminated(pod) {
					return false, nil
				}
				return false, r.migrationCheckpoint(ctx, agent, api.MigrationSourceTerminated)
			}
			if err := r.migrationFinalizer(ctx, agent, pod, api.MigrationPodFinalizer, false); err != nil {
				return false, err
			}
		}
		return false, nil
	}
	if !apierrors.IsNotFound(err) {
		return false, err
	}
	attachment := &storagev1.VolumeAttachment{}
	err = r.migrationGet(ctx, "", state.Attachment, attachment)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if !slices.Contains(attachment.Finalizers, api.MigrationAttachmentFinalizer) {
		return true, nil
	}
	if attachment.Spec.NodeName != state.Source.Name || attachment.Spec.Attacher != state.Driver ||
		ptr.Deref(attachment.Spec.Source.PersistentVolumeName, "") != state.Volume.Name {
		return false, fmt.Errorf("migration cleanup attachment identity changed")
	}
	if !migrationDetachAcknowledged(attachment, state.Driver) {
		if attachment.Status.DetachError != nil {
			return false, fmt.Errorf("migration cleanup waits for CSI detach acknowledgement")
		}
		return false, nil
	}
	return false, r.migrationFinalizer(ctx, agent, attachment, api.MigrationAttachmentFinalizer, false)
}

func (r *AgentReconciler) migrationPodReady(ctx context.Context, agent *api.Agent, workload *appsv1.StatefulSet) (bool, error) {
	if agent.Spec.PlannedMigration == nil {
		return true, nil
	}
	state := agent.Status.PlannedMigration
	if state == nil && agent.MigrationNeedsCapture() {
		return workload.Spec.Template.Annotations[api.AnnotationMigrationRequest] == "", nil
	}
	if state == nil || workload.Spec.Template.Annotations[api.AnnotationMigrationRequest] != state.RequestID {
		return false, nil
	}
	if err := r.checkMigrationApply(ctx, agent, &workload.Spec.Template.Spec); err != nil {
		return false, err
	}
	pods := &corev1.PodList{}
	if err := r.customReader().List(ctx, pods, client.InNamespace(state.Namespace.Name)); err != nil {
		return false, err
	}
	var replacement *corev1.Pod
	for i := range pods.Items {
		pod := &pods.Items[i]
		owner := metav1.GetControllerOf(pod)
		if owner == nil || owner.UID != workload.UID || owner.Name != workload.Name || owner.Kind != "StatefulSet" {
			continue
		}
		if replacement != nil || pod.UID == state.SourcePod.UID || pod.Spec.NodeName != state.Target.Name ||
			!pod.DeletionTimestamp.IsZero() || pod.Annotations[api.AnnotationMigrationRequest] != state.RequestID ||
			!slices.ContainsFunc(pod.Status.Conditions, func(c corev1.PodCondition) bool {
				return c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue
			}) {
			return false, nil
		}
		replacement = pod
	}
	attachments, err := r.migrationAttachments(ctx, state.Volume.Name)
	if err != nil {
		return false, err
	}
	if replacement == nil || len(attachments) != 1 || !attachments[0].Status.Attached || attachments[0].UID == "" {
		return false, nil
	}
	if state.Phase != api.MigrationCompleted {
		pod, attachment := migrationIdentity(replacement), migrationIdentity(&attachments[0])
		state.ReplacementPod, state.ReplacementAttachment = &pod, &attachment
	}
	return true, nil
}

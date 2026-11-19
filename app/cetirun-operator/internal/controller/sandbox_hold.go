// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	istiov1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"

	cetirunv1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxbinding"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
)

const sandboxHoldFinalizer = sandbox.HoldFinalizer

func sandboxRequested(agent *cetirunv1alpha1.Agent, team *cetirunv1alpha1.Team) bool {
	return sandbox.Requested(agent, team) || sandboxbinding.Present(agent)
}

// holdSandbox prevents runtime renderers from ignoring new policy fields.
// It requests an existing workload to stop but is not a qualified node fence.
// Once held, clearing spec fields cannot silently return an Agent to legacy mode.
func (r *AgentReconciler) holdSandbox(ctx context.Context, agent *cetirunv1alpha1.Agent) (ctrl.Result, error) {
	reconciled, err := r.usesReconciledSandbox(ctx, agent)
	if reconciled || err != nil {
		result, holdErr := r.holdReconciledSandbox(ctx, agent)
		return result, errors.Join(err, holdErr)
	}
	return r.holdSandboxWithPolicyFence(ctx, agent, nil)
}

type sandboxPolicyFenceStatus struct {
	phase   string
	reason  string
	message string
}

// fenceSandboxForPolicyReprojection performs only the fail-closed half of the
// transition. It must stop compute before the Gateway replacement grant is
// read back, but preparing a replacement workload at this point would depend
// on that not-yet-verified model projection and deadlock the transition.
func (r *AgentReconciler) fenceSandboxForPolicyReprojection(ctx context.Context, agent *cetirunv1alpha1.Agent) (ctrl.Result, error) {
	return r.holdSandboxWithPolicyFence(ctx, agent, &sandboxPolicyFenceStatus{
		phase: cetirunv1alpha1.PhasePending, reason: aiGatewayReasonPolicyReprojection,
		message: "Current policy projections are changing; Sandbox execution remains held until replacement enforcement is verified.",
	})
}

func (r *AgentReconciler) holdSandboxWithPolicyFence(ctx context.Context, agent *cetirunv1alpha1.Agent, policyFence *sandboxPolicyFenceStatus) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	var failures []error
	// Try to persist intent independently of status before workload side
	// effects. The finalizer also keeps newly held Agents available for safe
	// cleanup, but a failed metadata write cannot be allowed to skip shutdown.
	if !controllerutil.ContainsFinalizer(agent, sandboxHoldFinalizer) {
		base := agent.DeepCopy()
		controllerutil.AddFinalizer(agent, sandboxHoldFinalizer)
		if err := r.Patch(ctx, agent, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			// The workload fence is more important than the lifecycle marker. Do
			// not let an API write failure skip independent shutdown and route
			// removal. Restore the local view so status logic does not mistake an
			// unpersisted finalizer for durable intent.
			agent.Finalizers = base.Finalizers
			failures = append(failures, fmt.Errorf("persist Sandbox hold: %w", err))
		}
	}
	before := agent.DeepCopy().Status
	prepare := policyFence == nil && r.SandboxPreparation != nil && !agent.Spec.Suspended && sandboxPreparationSupported(agent)
	if !prepare {
		if err := r.revokeSandboxPreparationGrants(ctx, agent); err != nil {
			failures = append(failures, err)
		}
	}

	if agent.Spec.Agent.Type == cetirunv1alpha1.AgentTypeCustom {
		r.customPlans.Delete(agent.Name)
		if err := r.stopCustomWorkload(ctx, agent); err != nil {
			failures = append(failures, fmt.Errorf("stop sandbox-held Custom workload: %w", err))
		}
		if err := r.stopCustomTerminal(ctx, agent); err != nil {
			failures = append(failures, fmt.Errorf("stop sandbox-held Custom terminal: %w", err))
		}
		if err := r.removeCustomPlatformRoute(ctx, agent); err != nil {
			failures = append(failures, fmt.Errorf("remove sandbox-held Custom route: %w", err))
		}
		if err := r.retryCustomCompletion(ctx, agent); err != nil {
			failures = append(failures, fmt.Errorf("complete sandbox-held Custom apply: %w", err))
		}
	} else if err := r.stopSandboxBuiltin(ctx, agent); err != nil {
		failures = append(failures, err)
	}
	if recorded, err := r.recordedReconciledSandbox(ctx, agent); err != nil {
		failures = append(failures, err)
	} else if recorded {
		failures = append(failures, r.stopReconciledAuxiliaries(ctx, agent))
	}
	if prepare {
		if len(failures) == 0 {
			if agent.Status.AccessURL != "" || agent.Status.Phase == cetirunv1alpha1.PhaseReady ||
				apimeta.IsStatusConditionTrue(agent.Status.Conditions, cetirunv1alpha1.ConditionReady) ||
				apimeta.IsStatusConditionTrue(agent.Status.Conditions, cetirunv1alpha1.ConditionSandboxReady) {
				if err := r.sandboxPreparationStatus(ctx, agent, false, nil); err != nil {
					return ctrl.Result{}, err
				}
			}
			return r.reconcileSandboxPreparation(ctx, agent)
		}
		if err := r.revokeSandboxPreparationGrants(ctx, agent); err != nil {
			failures = append(failures, err)
		}
	}

	phase := cetirunv1alpha1.PhaseFailed
	reason := "UnsupportedControl"
	message := "Sandbox execution is not enabled: a qualified enforcement adapter and admission are required; workload execution is held."
	if policyFence != nil {
		phase, reason, message = policyFence.phase, policyFence.reason, policyFence.message
	} else if r.SandboxPreparation != nil && !sandboxPreparationSupported(agent) {
		message = "This Agent type or cloud/sidecar integration is not supported by native Sandbox preparation; execution remains held."
	}
	agent.Status.Phase = phase
	if agent.Spec.Suspended {
		agent.Status.Phase = cetirunv1alpha1.PhaseSuspended
		reason = "Suspended"
		message = "Suspension requested; Sandbox execution remains held and shutdown requires separate observation."
	}
	if len(failures) != 0 {
		agent.Status.Phase = cetirunv1alpha1.PhaseFailed
		reason = "PolicyApplyFailed"
		message = "Sandbox execution remains disabled, but workload shutdown, route removal or prior apply completion failed; retrying."
	}
	agent.Status.AccessURL = ""
	conditions := []string{cetirunv1alpha1.ConditionSandboxReady, cetirunv1alpha1.ConditionReady}
	if controllerutil.ContainsFinalizer(agent, sandbox.PreparationFinalizer) {
		conditions = append(conditions, cetirunv1alpha1.ConditionAgentConfigReady)
	}
	if agent.Spec.Agent.Type == cetirunv1alpha1.AgentTypeCustom {
		conditions = append(conditions, cetirunv1alpha1.ConditionIngressReady,
			cetirunv1alpha1.ConditionDesktopConfigured, cetirunv1alpha1.ConditionTerminalConfigured)
	}
	for _, conditionType := range conditions {
		apimeta.SetStatusCondition(&agent.Status.Conditions, metav1.Condition{
			Type: conditionType, Status: metav1.ConditionFalse,
			ObservedGeneration: agent.Generation, Reason: reason, Message: message,
		})
	}
	if !reflect.DeepEqual(before, agent.Status) {
		if err := r.updateStatus(ctx, agent); err != nil {
			failures = append(failures, err)
		}
		logger.Info("Holding sandbox-bound Agent", "name", agent.Name, "reason", reason)
		if r.EventRecorder != nil {
			r.EventRecorder.Event(agent, corev1.EventTypeWarning, reason, message)
		}
	}
	if err := errors.Join(failures...); err != nil {
		logger.Error(err, "Sandbox hold could not complete", "name", agent.Name)
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: time.Minute}, nil
}

// Built-in resources predating UID ownership cannot be automatically adopted.
func sandboxBuiltinOwned(object client.Object, agent *cetirunv1alpha1.Agent) bool {
	if agent.UID == "" || object.GetUID() == "" || !hasAgentOwner(object, agent) {
		return false
	}
	for _, owner := range object.GetOwnerReferences() {
		if owner.APIVersion != cetirunv1alpha1.GroupVersion.String() || owner.Kind != "Agent" ||
			owner.Name != agent.Name || owner.UID != agent.UID {
			return false
		}
	}
	for key, expected := range map[string]string{
		customAgentUID:                   string(agent.UID),
		"meta.helm.sh/release-name":      helmReleaseName(agent.Name),
		"meta.helm.sh/release-namespace": instanceNamespace(agent.Name),
	} {
		if value := object.GetAnnotations()[key]; value != "" && value != expected {
			return false
		}
	}
	return true
}

func (r *AgentReconciler) stopSandboxBuiltin(ctx context.Context, agent *cetirunv1alpha1.Agent) error {
	if err := r.stopSandboxBuiltinWorkload(ctx, agent); err != nil {
		return err
	}
	return r.stopNativeSandboxReservation(ctx, agent)
}

func (r *AgentReconciler) stopSandboxBuiltinWorkload(ctx context.Context, agent *cetirunv1alpha1.Agent) error {
	reader := r.customReader()
	namespace := instanceNamespace(agent.Name)
	if err := reader.Get(ctx, types.NamespacedName{Name: namespace}, &corev1.Namespace{}); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	var failures []error
	sts := &appsv1.StatefulSet{}
	key := types.NamespacedName{Namespace: namespace, Name: instanceServiceName(agent.Name)}
	if err := reader.Get(ctx, key, sts); err != nil {
		if !apierrors.IsNotFound(err) {
			failures = append(failures, fmt.Errorf("inspect sandbox-held workload: %w", err))
		}
	} else if !sandboxBuiltinOwned(sts, agent) {
		failures = append(failures, fmt.Errorf("sandbox-held workload ownership is unconfirmed; recovery is required"))
	} else if sts.Spec.Replicas == nil || *sts.Spec.Replicas != 0 ||
		(sts.Spec.PersistentVolumeClaimRetentionPolicy != nil &&
			sts.Spec.PersistentVolumeClaimRetentionPolicy.WhenScaled == appsv1.DeletePersistentVolumeClaimRetentionPolicyType) {
		base := sts.DeepCopy()
		zero := int32(0)
		sts.Spec.Replicas = &zero
		if sts.Spec.PersistentVolumeClaimRetentionPolicy == nil {
			sts.Spec.PersistentVolumeClaimRetentionPolicy = &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
				WhenDeleted: appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
			}
		}
		sts.Spec.PersistentVolumeClaimRetentionPolicy.WhenScaled = appsv1.RetainPersistentVolumeClaimRetentionPolicyType
		if err := r.Patch(ctx, sts, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			failures = append(failures, fmt.Errorf("stop sandbox-held workload: %w", err))
		}
	}
	route := &istiov1beta1.VirtualService{}
	key.Name = virtualServiceName(agent.Name)
	if err := reader.Get(ctx, key, route); err != nil {
		if !apierrors.IsNotFound(err) {
			failures = append(failures, fmt.Errorf("inspect sandbox-held route: %w", err))
		}
	} else if !sandboxBuiltinOwned(route, agent) {
		failures = append(failures, fmt.Errorf("sandbox-held route ownership is unconfirmed; recovery is required"))
	} else if err := r.Delete(ctx, route, client.Preconditions{UID: &route.UID, ResourceVersion: &route.ResourceVersion}); err != nil && !apierrors.IsNotFound(err) {
		failures = append(failures, fmt.Errorf("remove sandbox-held route: %w", err))
	}
	if (r.SandboxPreparation != nil && r.SandboxPreparation.NativeExecution) ||
		slices.Contains(sts.Spec.Template.Finalizers, sandboxworkload.NativePodFinalizer) ||
		!agent.DeletionTimestamp.IsZero() {
		if err := r.finishNativePodLifetime(ctx, agent); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (r *AgentReconciler) deleteSandboxBuiltin(ctx context.Context, agent *cetirunv1alpha1.Agent) (ctrl.Result, error) {
	if err := r.refreshSandboxCleanupAgent(ctx, agent); err != nil {
		return ctrl.Result{}, err
	}
	before := agent.DeepCopy().Status
	agent.Status.Phase = cetirunv1alpha1.PhaseDeleting
	agent.Status.AccessURL = ""
	conditions := []string{cetirunv1alpha1.ConditionReady, cetirunv1alpha1.ConditionSandboxReady}
	if controllerutil.ContainsFinalizer(agent, sandbox.PreparationFinalizer) {
		conditions = append(conditions, cetirunv1alpha1.ConditionAgentConfigReady)
	}
	for _, kind := range conditions {
		apimeta.SetStatusCondition(&agent.Status.Conditions, metav1.Condition{
			Type: kind, Status: metav1.ConditionFalse, ObservedGeneration: agent.Generation,
			Reason: "InstanceDeleting", Message: "Sandbox-held Agent cleanup requested",
		})
	}
	var statusErr error
	if !reflect.DeepEqual(before, agent.Status) {
		statusErr = r.updateStatus(ctx, agent)
	}
	// Failed revocation or status writes must not skip independent shutdown.
	revokeErr := r.revokeSandboxPreparationGrants(ctx, agent)
	stopErr := r.stopSandboxBuiltin(ctx, agent)
	gatewayErr := r.deleteAIGatewayKey(ctx, agent)
	if err := errors.Join(statusErr, revokeErr, stopErr, gatewayErr); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.cleanupRegistryCredentials(ctx, agent, true); err != nil {
		return r.registryWaiting(ctx, agent, "RegistryCleanupPending")
	}
	if err := r.checkSandboxPreparationCleanup(ctx, agent); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.checkSandboxBootstrapCleanup(ctx, agent); err != nil {
		return ctrl.Result{}, err
	}
	if agentIdentityRequested(agent) || agent.Status.AgentIdentity != nil {
		if err := r.deleteAgentIdentity(ctx, agent); err != nil {
			return ctrl.Result{}, fmt.Errorf("clean up sandbox-held Agent Identity: %w", err)
		}
	}
	ns := &corev1.Namespace{}
	err := r.customReader().Get(ctx, types.NamespacedName{Name: instanceNamespace(agent.Name)}, ns)
	if apierrors.IsNotFound(err) {
		if err := r.finishSandboxPreparation(ctx, agent); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.finishSandboxBootstrap(ctx, agent); err != nil {
			return ctrl.Result{}, err
		}
		base := agent.DeepCopy()
		controllerutil.RemoveFinalizer(agent, sandboxHoldFinalizer)
		controllerutil.RemoveFinalizer(agent, cetirunv1alpha1.Finalizer)
		return ctrl.Result{}, client.IgnoreNotFound(r.Patch(ctx, agent, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})))
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	operatorNamespace := r.OperatorNamespace
	if operatorNamespace == "" {
		operatorNamespace = "cetirun-system"
	}
	if !sandboxBuiltinOwned(ns, agent) || ns.Name == operatorNamespace {
		return ctrl.Result{}, fmt.Errorf("sandbox-held namespace ownership is unconfirmed; cleanup requires recovery")
	}
	if err := r.releaseSandboxBootstrapResources(ctx, agent, ns); err != nil {
		return ctrl.Result{}, err
	}
	if ns.DeletionTimestamp.IsZero() {
		if err := r.Delete(ctx, ns, client.Preconditions{UID: &ns.UID, ResourceVersion: &ns.ResourceVersion}); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

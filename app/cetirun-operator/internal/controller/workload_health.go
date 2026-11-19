// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"
	"time"

	cetirunv1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Kept separate from Ready/PodReady so a later prerequisite failure cannot be
// mistaken for a workload-only wait during an outage.
const builtInPrerequisitesReady = "BuiltInPrerequisitesReady"

// observeWorkload updates only PodReady. The caller combines that observation
// with prerequisite readiness and persists the status. In particular, observing
// a healthy Pod does not establish that the Agent's route or identity is ready.
func (r *AgentReconciler) observeWorkload(ctx context.Context, instance *cetirunv1alpha1.Agent) (bool, ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("name", instance.Name)
	ns := instanceNamespace(instance.Name)
	stsName := instanceServiceName(instance.Name)
	sts := &appsv1.StatefulSet{}
	stsKey := types.NamespacedName{Namespace: ns, Name: stsName}
	// Helm writes directly to the API; a cached pre-apply rollout can still be Ready.
	if err := r.customReader().Get(ctx, stsKey, sts); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("StatefulSet not found yet, requeueing", "name", stsName, "namespace", ns)
			setCondition(&instance.Status.Conditions, cetirunv1alpha1.ConditionPodReady,
				metav1.ConditionFalse, "StatefulSetNotFound", fmt.Sprintf("StatefulSet %s not found", stsName))
			return false, ctrl.Result{RequeueAfter: 15 * time.Second}, nil
		}
		logger.Error(err, "Failed to get StatefulSet", "name", stsName, "namespace", ns)
		setCondition(&instance.Status.Conditions, cetirunv1alpha1.ConditionPodReady,
			metav1.ConditionFalse, "StatefulSetGetFailed", fmt.Sprintf("Failed to get StatefulSet %s: %v", stsName, err))
		return false, ctrl.Result{}, err
	}

	desiredReplicas := int32(1)
	if sts.Spec.Replicas != nil {
		desiredReplicas = *sts.Spec.Replicas
	}
	rolloutComplete := (sts.Status.UpdatedReplicas >= desiredReplicas || registryOnlyRollout(sts)) &&
		sts.Status.ReadyReplicas >= desiredReplicas &&
		(sts.Status.ObservedGeneration >= sts.Generation || registryOnlyRollout(sts))
	restartReady, err := r.restartPodReady(ctx, instance, sts)
	if err != nil {
		setCondition(&instance.Status.Conditions, cetirunv1alpha1.ConditionPodReady,
			metav1.ConditionFalse, "RestartObservationFailed", err.Error())
		return false, ctrl.Result{}, err
	}
	rolloutComplete = rolloutComplete && restartReady
	migrationReady, err := r.migrationPodReady(ctx, instance, sts)
	if err != nil {
		setCondition(&instance.Status.Conditions, cetirunv1alpha1.ConditionPodReady,
			metav1.ConditionFalse, "MigrationObservationFailed", err.Error())
		return false, ctrl.Result{}, err
	}
	rolloutComplete = rolloutComplete && migrationReady
	if !rolloutComplete {
		logger.Info("StatefulSet rollout not complete, requeueing",
			"statefulset", stsName,
			"readyReplicas", sts.Status.ReadyReplicas,
			"updatedReplicas", sts.Status.UpdatedReplicas,
			"desiredReplicas", desiredReplicas,
			"stsGeneration", sts.Generation,
			"stsObservedGeneration", sts.Status.ObservedGeneration)
		setCondition(&instance.Status.Conditions, cetirunv1alpha1.ConditionPodReady,
			metav1.ConditionFalse, "PodNotReady",
			fmt.Sprintf("Waiting for rollout (ready: %d, updated: %d, desired: %d)",
				sts.Status.ReadyReplicas, sts.Status.UpdatedReplicas, desiredReplicas))
		return false, ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	setCondition(&instance.Status.Conditions, cetirunv1alpha1.ConditionPodReady,
		metav1.ConditionTrue, "PodReady", "StatefulSet pod is running and ready")
	return true, ctrl.Result{}, nil
}

// setWorkloadReadiness is used only when prerequisites have completed and the
// remaining readiness decision belongs to the workload. It never changes URLs.
func setWorkloadReadiness(instance *cetirunv1alpha1.Agent, ready bool) {
	if !ready {
		instance.Status.Phase = cetirunv1alpha1.PhaseProvisioning
		setCondition(&instance.Status.Conditions, cetirunv1alpha1.ConditionReady,
			metav1.ConditionFalse, "PodNotReady", "StatefulSet is not ready")
		return
	}
	instance.Status.Phase = cetirunv1alpha1.PhaseReady
	instance.Status.ObservedRestartRequestID = instance.Spec.RestartRequestID
	if state := instance.Status.PlannedMigration; state != nil && state.CanStart() &&
		instance.Spec.PlannedMigration != nil && state.PlannedMigrationRequest == *instance.Spec.PlannedMigration &&
		state.ReplacementPod != nil && state.ReplacementAttachment != nil {
		state.Phase = cetirunv1alpha1.MigrationCompleted
		setCondition(&instance.Status.Conditions, cetirunv1alpha1.ConditionPlannedMigrationReady, metav1.ConditionTrue,
			"MigrationCompleted", "Current destination Pod and CSI attachment are ready with the original HOME")
	}
	setCondition(&instance.Status.Conditions, cetirunv1alpha1.ConditionReady,
		metav1.ConditionTrue, "InstanceReady", "Instance is fully operational")
}

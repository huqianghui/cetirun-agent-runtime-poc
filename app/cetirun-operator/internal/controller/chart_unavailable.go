// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// chartUnavailableReason is the AgentConfigReady reason for a chart that
	// could not be downloaded again for an Agent that is already deployed.
	chartUnavailableReason = "AgentChartUnavailable"
	// chartUnavailableRetry is when the operator retries the full reconcile
	// of such an Agent.
	chartUnavailableRetry = 30 * time.Second
)

// preparationFailed handles an error from preparing a built-in Agent's chart.
//
// Every full reconcile downloads the Agent's chart again, including the
// periodic passes of an Agent at steady state. A download failure while the
// Agent runs the chart its current generation selects is a registry problem,
// not a configuration error: the release stays as it is, AgentConfigReady
// reports the failure as temporary, the workload's health is still observed,
// and the reconcile is retried. Any other error, and a download failure for an
// Agent that is new or whose spec changed since its chart was installed,
// rejects the configuration.
func (r *AgentReconciler) preparationFailed(ctx context.Context, instance *api.Agent, err error) (ctrl.Result, error) {
	var download *helm.ChartDownloadError
	if !errors.As(err, &download) || !chartInstalledForGeneration(instance) {
		return r.rejectAgentConfig(ctx, instance, err)
	}
	log.FromContext(ctx).Error(err, "Agent chart download failed; the deployed release is unchanged",
		"retryAfter", chartUnavailableRetry)
	previous := instance.Status.DeepCopy()
	message := fmt.Sprintf("Agent chart %s could not be downloaded from the registry; "+
		"the deployed release is unchanged and the operator will retry", download.Version)
	condition := meta.FindStatusCondition(instance.Status.Conditions, api.ConditionAgentConfigReady)
	reported := condition != nil && condition.Status == metav1.ConditionFalse &&
		condition.Reason == chartUnavailableReason && condition.Message == message
	setCondition(&instance.Status.Conditions, api.ConditionAgentConfigReady,
		metav1.ConditionFalse, chartUnavailableReason, message)

	// The release stays as installed, but the workload's health does not wait
	// for the registry: an Agent that loses its Pod stops being Ready, and is
	// Ready again when its Pod recovers. Do not override a suspension, failure
	// or provisioning wait for a different prerequisite.
	var observeErr error
	ready := meta.FindStatusCondition(instance.Status.Conditions, api.ConditionReady)
	workloadOnly := ready != nil && (ready.Status == metav1.ConditionTrue || ready.Reason == "PodNotReady")
	if !instance.Spec.Suspended &&
		(instance.Status.Phase == api.PhaseReady || instance.Status.Phase == api.PhaseProvisioning) {
		var workloadReady bool
		workloadReady, _, observeErr = r.observeWorkload(ctx, instance)
		if workloadOnly && isConditionTrue(instance.Status.Conditions, builtInPrerequisitesReady) {
			setWorkloadReadiness(instance, workloadReady)
		} else if workloadOnly && (!workloadReady || instance.Status.Phase == api.PhaseProvisioning) {
			instance.Status.Phase = api.PhaseProvisioning
			setCondition(&instance.Status.Conditions, api.ConditionReady,
				metav1.ConditionFalse, "PrerequisitesPending", "Agent prerequisites must be reconciled before readiness can recover")
		}
	}
	// Write only a change: each status write starts another reconcile.
	if !equality.Semantic.DeepEqual(*previous, instance.Status) {
		if err := r.updateStatus(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
	}
	if !reported {
		r.EventRecorder.Event(instance, corev1.EventTypeWarning, chartUnavailableReason, message)
	}
	if previous.Phase != api.PhaseReady && instance.Status.Phase == api.PhaseReady {
		r.EventRecorder.Event(instance, corev1.EventTypeNormal, "InstanceReady", "Agent fully operational")
	}
	if observeErr != nil {
		return ctrl.Result{}, observeErr
	}
	return ctrl.Result{RequeueAfter: chartUnavailableRetry}, nil
}

// chartInstalledForGeneration reports whether the Agent's chart was installed
// for its current generation, that is, for the chart version and settings
// its spec selects now.
func chartInstalledForGeneration(instance *api.Agent) bool {
	return isConditionTrue(instance.Status.Conditions, api.ConditionHelmDeployed) &&
		instance.Status.ObservedGeneration == instance.Generation
}

// chartUnavailable reports whether the Agent's last attempt to download its
// chart failed while the Agent was deployed.
func chartUnavailable(instance *api.Agent) bool {
	condition := meta.FindStatusCondition(instance.Status.Conditions, api.ConditionAgentConfigReady)
	return condition != nil && condition.Status == metav1.ConditionFalse && condition.Reason == chartUnavailableReason
}

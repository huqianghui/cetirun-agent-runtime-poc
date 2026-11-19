// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const customProviderTimeout = 30 * time.Second

type cachedCustomPlan struct {
	signature string
	plan      *helm.PreparedRelease
	expires   time.Time
}

func customInputSignature(version string, values map[string]any) (string, error) {
	data, err := json.Marshal(struct {
		Version string
		Values  map[string]any
	}{version, values})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func (r *AgentReconciler) cachedCustomPlan(instance *api.Agent, signature string, current *appsv1.StatefulSet) (*helm.PreparedRelease, bool) {
	entry, ok := r.customPlans.Load(instance.Name)
	if !ok || current == nil || !hasAgentOwner(current, instance) ||
		current.Spec.Replicas == nil || *current.Spec.Replicas != 1 {
		return nil, false
	}
	cached := entry.(cachedCustomPlan)
	if cached.signature != signature || !time.Now().Before(cached.expires) ||
		cached.plan.CacheKey() != instance.Status.ObservedOperatorHash ||
		current.Annotations[customPlanHash] != cached.plan.WorkloadHash {
		return nil, false
	}
	return cached.plan, true
}

func customPodReady(current *appsv1.StatefulSet) bool {
	return current != nil && current.Spec.Replicas != nil && *current.Spec.Replicas == 1 &&
		current.Status.ObservedGeneration >= current.Generation &&
		current.Status.ReadyReplicas == 1 && current.Status.UpdatedReplicas == 1
}

func (r *AgentReconciler) customPodReady(ctx context.Context, current *appsv1.StatefulSet) bool {
	if customPodReady(current) {
		return true
	}
	if current == nil || current.UID == "" || current.Spec.Replicas == nil || *current.Spec.Replicas != 1 ||
		current.Status.ReadyReplicas != 1 ||
		current.Annotations[customPlanHash] == "" {
		return false
	}
	pods := &corev1.PodList{}
	if r.customReader().List(ctx, pods, client.InNamespace(current.Namespace), client.MatchingLabels(current.Spec.Template.Labels)) != nil {
		return false
	}
	for _, pod := range pods.Items {
		owner := metav1.GetControllerOf(&pod)
		if owner == nil || owner.UID != current.UID || !pod.DeletionTimestamp.IsZero() ||
			pod.Annotations[customPlanHash] != current.Annotations[customPlanHash] {
			continue
		}
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				return true
			}
		}
	}
	return false
}

// Waiting describes the desired configuration, not the destruction of an
// already applied workload. Preserve independently observed process state.
func (r *AgentReconciler) keepCustomObservation(ctx context.Context, instance *api.Agent) bool {
	if instance.Status.ObservedOperatorHash == "" {
		return false
	}
	current := &appsv1.StatefulSet{}
	err := r.customReader().Get(ctx, types.NamespacedName{
		Namespace: instanceNamespace(instance.Name), Name: instanceServiceName(instance.Name),
	}, current)
	if err != nil {
		return !apierrors.IsNotFound(err)
	}
	if !hasAgentOwner(current, instance) || current.Annotations[customAgentUID] != string(instance.UID) {
		return false
	}
	instance.Status.Phase = api.PhaseProvisioning
	status := metav1.ConditionFalse
	if r.customPodReady(ctx, current) {
		instance.Status.Phase = api.PhaseReady
		status = metav1.ConditionTrue
	}
	setCondition(&instance.Status.Conditions, api.ConditionPodReady, status,
		"CustomAppliedPodObserved", "Observed the previously applied Custom workload")
	setCondition(&instance.Status.Conditions, api.ConditionReady, status,
		"CustomAppliedPodObserved", "Desired configuration is waiting; process readiness is observed independently")
	return true
}

func (r *AgentReconciler) rejectCustomConfig(ctx context.Context, instance *api.Agent, err error) (ctrl.Result, error) {
	if instance.Status.ObservedOperatorHash != "" {
		return r.customWaiting(ctx, instance, "AgentConfigInvalid", err.Error())
	}
	return r.rejectAgentConfig(ctx, instance, err)
}

func (r *AgentReconciler) customPreparationFailed(ctx context.Context, instance *api.Agent, err error) (ctrl.Result, error) {
	if errors.Is(err, helm.ErrTerminalAPIBindingPending) {
		return r.customWaiting(ctx, instance, "TerminalAPIBindingPending", "Waiting for API server network endpoints")
	}
	if helm.IsConfigurationError(err) {
		return r.rejectCustomConfig(ctx, instance, err)
	}
	if errors.Is(err, helm.ErrReleaseOperationPending) {
		return r.customWaiting(ctx, instance, "ReleaseOperationPending", err.Error())
	}
	if apierrors.IsForbidden(err) {
		return r.customWaiting(ctx, instance, "CustomPermissionDenied", "Operator permissions do not cover Custom preparation")
	}
	return r.customWaiting(ctx, instance, "CustomPreparationUnavailable", "Chart or Kubernetes API preparation is temporarily unavailable")
}

func hasCustomRecordOwner(record client.Object, instance *api.Agent) bool {
	return hasAgentOwner(record, instance) ||
		(!instance.DeletionTimestamp.IsZero() && len(record.GetOwnerReferences()) == 0)
}

func hasCustomWorkloadOwner(obj client.Object, instance *api.Agent) bool {
	return hasAgentOwner(obj, instance) ||
		(!instance.DeletionTimestamp.IsZero() && len(obj.GetOwnerReferences()) == 0 &&
			obj.GetAnnotations()[customAgentUID] == string(instance.UID))
}

func (r *AgentReconciler) stopCustomWorkload(ctx context.Context, instance *api.Agent) error {
	current := &appsv1.StatefulSet{}
	err := r.customReader().Get(ctx, types.NamespacedName{
		Namespace: instanceNamespace(instance.Name), Name: instanceServiceName(instance.Name),
	}, current)
	if err != nil {
		return client.IgnoreNotFound(err)
	}
	if !hasCustomWorkloadOwner(current, instance) || current.Annotations[customAgentUID] != string(instance.UID) {
		return errors.New("Custom workload owner is not confirmed")
	}
	if current.Spec.Replicas != nil && *current.Spec.Replicas == 0 {
		return nil
	}
	before := current.DeepCopy()
	zero := int32(0)
	current.Spec.Replicas = &zero
	return r.Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

func (r *AgentReconciler) customEvent(instance *api.Agent, eventType, reason, message string) {
	if r.EventRecorder != nil {
		r.EventRecorder.Event(instance, eventType, reason, message)
	}
}

func (r *AgentReconciler) customPodReadAccess(ctx context.Context, instance *api.Agent) error {
	return r.customReader().List(ctx, &corev1.PodList{}, client.InNamespace(instanceNamespace(instance.Name)))
}

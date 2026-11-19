// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *AgentReconciler) checkAgentGeneration(ctx context.Context, expected *api.Agent) error {
	current := &api.Agent{}
	if err := r.customReader().Get(ctx, client.ObjectKeyFromObject(expected), current); err != nil {
		return err
	}
	if current.UID != expected.UID || current.Generation != expected.Generation ||
		current.Spec.RestartRequestID != expected.Spec.RestartRequestID ||
		current.Spec.Suspended || !current.DeletionTimestamp.IsZero() {
		return fmt.Errorf("Agent changed before application")
	}
	return nil
}

func (r *AgentReconciler) restartPodReady(ctx context.Context, instance *api.Agent, workload *appsv1.StatefulSet) (bool, error) {
	requestID := instance.Spec.RestartRequestID
	if requestID == "" {
		return true, nil
	}
	if !api.ValidRestartRequestID(requestID) {
		return false, fmt.Errorf("invalid Agent restart request ID")
	}
	if instance.Spec.Suspended || !instance.DeletionTimestamp.IsZero() || workload == nil ||
		workload.UID == "" || !workload.DeletionTimestamp.IsZero() ||
		workload.Spec.Replicas == nil || *workload.Spec.Replicas != 1 ||
		workload.Spec.Template.Annotations[api.AnnotationRestartRequestID] != requestID {
		return false, nil
	}
	selector, err := metav1.LabelSelectorAsSelector(workload.Spec.Selector)
	if err != nil || workload.Spec.Selector == nil {
		return false, fmt.Errorf("restart workload has an invalid Pod selector")
	}
	pods := &corev1.PodList{}
	if err := r.customReader().List(ctx, pods, client.InNamespace(workload.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return false, fmt.Errorf("observe restart Pods: %w", err)
	}
	ready := 0
	for _, pod := range pods.Items {
		owner := metav1.GetControllerOf(&pod)
		if owner == nil || owner.Kind != "StatefulSet" || owner.Name != workload.Name || owner.UID != workload.UID {
			continue
		}
		if !pod.DeletionTimestamp.IsZero() || pod.Annotations[api.AnnotationRestartRequestID] != requestID {
			return false, nil
		}
		podReady := false
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				podReady = true
			}
		}
		if !podReady {
			return false, nil
		}
		ready++
	}
	return ready == 1, nil
}

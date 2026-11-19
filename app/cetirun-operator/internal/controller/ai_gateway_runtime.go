// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *AgentReconciler) stopGatewayNativeWorkload(ctx context.Context, agent *api.Agent) error {
	sts := &appsv1.StatefulSet{}
	key := client.ObjectKey{Namespace: instanceNamespace(agent.Name), Name: instanceServiceName(agent.Name)}
	if err := r.customReader().Get(ctx, key, sts); err != nil {
		return client.IgnoreNotFound(err)
	}
	ns := &corev1.Namespace{}
	if err := r.customReader().Get(ctx, client.ObjectKey{Name: key.Namespace}, ns); err != nil {
		return err
	}
	if !hasAgentOwner(ns, agent) ||
		(len(sts.OwnerReferences) != 0 && !hasAgentOwner(sts, agent)) ||
		(!hasAgentOwner(sts, agent) && (sts.Annotations["meta.helm.sh/release-name"] != helmReleaseName(agent.Name) ||
			sts.Annotations["meta.helm.sh/release-namespace"] != key.Namespace || sts.Labels["app.kubernetes.io/managed-by"] != "Helm")) {
		return fmt.Errorf("native workload ownership must be confirmed before stopping execution")
	}
	if sts.Spec.Replicas != nil && *sts.Spec.Replicas == 0 &&
		(sts.Spec.PersistentVolumeClaimRetentionPolicy == nil || sts.Spec.PersistentVolumeClaimRetentionPolicy.WhenScaled != appsv1.DeletePersistentVolumeClaimRetentionPolicyType) {
		return nil
	}
	before := sts.DeepCopy()
	zero := int32(0)
	sts.Spec.Replicas = &zero
	if sts.Spec.PersistentVolumeClaimRetentionPolicy == nil {
		sts.Spec.PersistentVolumeClaimRetentionPolicy = &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{WhenDeleted: appsv1.RetainPersistentVolumeClaimRetentionPolicyType}
	}
	sts.Spec.PersistentVolumeClaimRetentionPolicy.WhenScaled = appsv1.RetainPersistentVolumeClaimRetentionPolicyType
	return r.Patch(ctx, sts, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

func (r *AgentReconciler) gatewayRuntimeWaiting(ctx context.Context, agent *api.Agent, reason string) (ctrl.Result, error) {
	stopErr := errors.Join(r.stopGatewayNativeWorkload(ctx, agent), r.suspendCopilotProxy(ctx, agent), r.removeRegistryBuiltinRoute(ctx, agent))
	agent.Status.AccessURL = ""
	agent.Status.Phase = api.PhasePending
	agent.Status.ObservedGeneration = 0
	agent.Status.ObservedOperatorHash = ""
	for _, condition := range []string{api.ConditionReady, api.ConditionPodReady, api.ConditionIngressReady, api.ConditionAgentConfigReady} {
		setCondition(&agent.Status.Conditions, condition, metav1.ConditionFalse, reason,
			"Agent execution requires an AI Gateway binding and a supported Chart")
	}
	return ctrl.Result{RequeueAfter: 30 * time.Second}, errors.Join(stopErr, r.updateStatus(ctx, agent))
}

// Suspension blocks an existing key without provisioning a new one or consulting
// the model catalog. An outage must not prevent the independent compute stop.
func (r *AgentReconciler) suspendGatewayKey(ctx context.Context, agent *api.Agent) error {
	secret, err := r.aiGatewayKeySecret(ctx, agent)
	if err != nil {
		return err
	}
	if secret == nil && !aiGatewayKeyWasIssued(agent) {
		return nil
	}
	gateway, err := r.aiGatewayClient(ctx)
	if err != nil {
		return err
	}
	blocked, err := r.blockAIGatewayKeyIfPresent(ctx, gateway, agent, aiGatewayReasonSuspended)
	if err == nil && blocked {
		status := ensureAIGatewayStatus(agent)
		status.Blocked, status.Routed = true, false
	}
	return err
}

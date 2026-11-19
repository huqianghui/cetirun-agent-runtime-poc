// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"fmt"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func (r *AgentReconciler) sandboxBootstrapForCleanup(ctx context.Context, agent *api.Agent) (*corev1.ConfigMap, *sandboxBootstrapReceipt, error) {
	if r.OperatorNamespace == "" {
		if controllerutil.ContainsFinalizer(agent, sandbox.BootstrapFinalizer) {
			return nil, nil, fmt.Errorf("Sandbox bootstrap cleanup requires the operator state namespace")
		}
		return nil, nil, nil
	}
	object, receipt, err := r.readSandboxBootstrap(ctx, agent)
	if apierrors.IsNotFound(err) {
		if controllerutil.ContainsFinalizer(agent, sandbox.BootstrapFinalizer) {
			return nil, nil, fmt.Errorf("Sandbox bootstrap receipt is missing; cleanup requires recovery")
		}
		return nil, nil, nil
	}
	return object, receipt, err
}

// This validates ownership, not writer fencing. The preparation cleanup gate
// remains mandatory for any Agent that has progressed beyond bootstrap.
func (r *AgentReconciler) checkSandboxBootstrapCleanup(ctx context.Context, agent *api.Agent) error {
	object, receipt, err := r.sandboxBootstrapForCleanup(ctx, agent)
	if err != nil || receipt == nil {
		return err
	}
	for i, desired := range r.sandboxBootstrapObjects(agent, object, receipt) {
		current := desired.DeepCopyObject().(client.Object)
		if err := r.customReader().Get(ctx, client.ObjectKeyFromObject(desired), current); apierrors.IsNotFound(err) {
			if i == 0 && receipt.Skills == nil {
				return nil
			}
			continue
		} else if err != nil {
			return err
		}
		if err := validateSandboxBootstrapObject(current, desired, receipt.inventory()[i].UID, true); err != nil {
			return err
		}
	}
	var pods corev1.PodList
	if err := r.customReader().List(ctx, &pods, client.InNamespace(receipt.Namespace), client.Limit(1)); err != nil {
		return err
	}
	if len(pods.Items) != 0 || pods.Continue != "" {
		return fmt.Errorf("Sandbox bootstrap cleanup waits for Pods to terminate")
	}
	return ctx.Err()
}

func (r *AgentReconciler) releaseSandboxBootstrapResources(ctx context.Context, agent *api.Agent, namespace *corev1.Namespace) error {
	object, receipt, err := r.sandboxBootstrapForCleanup(ctx, agent)
	if err != nil {
		return err
	}
	if receipt == nil {
		if controllerutil.ContainsFinalizer(namespace, sandbox.BootstrapFinalizer) {
			return fmt.Errorf("Sandbox namespace bootstrap finalizer requires its deleting Agent and receipt")
		}
		return nil
	}
	if agent.DeletionTimestamp.IsZero() {
		return fmt.Errorf("Sandbox bootstrap finalizers require their deleting Agent")
	}
	desired := r.sandboxBootstrapObjects(agent, object, receipt)
	if err := validateSandboxBootstrapObject(namespace, desired[0], receipt.Objects[0].UID, true); err != nil {
		return err
	}
	// Release HOME and credentials before RBAC, after all cleanup gates pass.
	// Keep the namespace protected until every child release has succeeded.
	order := []int{3, 4, 1, 2}
	if receipt.Skills != nil {
		order = append([]int{5}, order...)
	}
	if receipt.ServiceAccount != nil {
		order = append([]int{len(receipt.Objects)}, order...)
	}
	for _, i := range order {
		current := desired[i].DeepCopyObject().(client.Object)
		if err := r.customReader().Get(ctx, client.ObjectKeyFromObject(current), current); apierrors.IsNotFound(err) {
			continue
		} else if err != nil {
			return err
		}
		if err := validateSandboxBootstrapObject(current, desired[i], receipt.inventory()[i].UID, true); err != nil {
			return err
		}
		if err := r.removeSandboxBootstrapFinalizer(ctx, current); err != nil {
			return err
		}
	}
	return r.removeSandboxBootstrapFinalizer(ctx, namespace)
}

func (r *AgentReconciler) removeSandboxBootstrapFinalizer(ctx context.Context, object client.Object) error {
	if !controllerutil.ContainsFinalizer(object, sandbox.BootstrapFinalizer) {
		return nil
	}
	if object.GetUID() == "" || object.GetResourceVersion() == "" {
		return fmt.Errorf("Sandbox bootstrap finalizer release requires a persisted identity")
	}
	controllerutil.RemoveFinalizer(object, sandbox.BootstrapFinalizer)
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{
		"uid": object.GetUID(), "resourceVersion": object.GetResourceVersion(), "finalizers": object.GetFinalizers(),
	}})
	if err != nil {
		return err
	}
	return r.Patch(ctx, object, client.RawPatch(types.MergePatchType, patch))
}

func (r *AgentReconciler) finishSandboxBootstrap(ctx context.Context, agent *api.Agent) error {
	if err := r.refreshSandboxCleanupAgent(ctx, agent); err != nil {
		return err
	}
	object, receipt, err := r.sandboxBootstrapForCleanup(ctx, agent)
	if err != nil || receipt == nil {
		return err
	}
	if err := r.customReader().Get(ctx, client.ObjectKey{Name: receipt.Namespace}, &corev1.Namespace{}); err == nil {
		return fmt.Errorf("Sandbox namespace must be absent before releasing bootstrap state")
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	if receipt.Skills != nil {
		if err := r.finishSandboxSkillsVolume(ctx, agent, object, receipt); err != nil {
			return err
		}
	}
	if controllerutil.ContainsFinalizer(agent, sandbox.BootstrapFinalizer) {
		base := agent.DeepCopy()
		controllerutil.RemoveFinalizer(agent, sandbox.BootstrapFinalizer)
		if err := r.Patch(ctx, agent, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
	}
	if controllerutil.ContainsFinalizer(object, sandbox.BootstrapFinalizer) {
		base := object.DeepCopy()
		controllerutil.RemoveFinalizer(object, sandbox.BootstrapFinalizer)
		if err := r.Patch(ctx, object, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return client.IgnoreNotFound(err)
		}
	}
	return client.IgnoreNotFound(r.Delete(ctx, object, client.Preconditions{UID: &object.UID, ResourceVersion: &object.ResourceVersion}))
}

func (r *AgentReconciler) finishSandboxSkillsVolume(ctx context.Context, agent *api.Agent, object *corev1.ConfigMap, receipt *sandboxBootstrapReceipt) error {
	claim := &corev1.PersistentVolumeClaim{}
	if err := r.customReader().Get(ctx, client.ObjectKey{Namespace: receipt.Namespace, Name: receipt.Objects[5].Name}, claim); err == nil {
		return fmt.Errorf("Sandbox skills claim must be absent before releasing its retained PV")
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	desired := r.sandboxBootstrapObjects(agent, object, receipt)[6]
	current := &corev1.PersistentVolume{}
	key := client.ObjectKeyFromObject(desired)
	if err := r.customReader().Get(ctx, key, current); err != nil {
		return client.IgnoreNotFound(err)
	}
	if err := validateSandboxBootstrapObject(current, desired, receipt.Objects[6].UID, true); err != nil {
		return err
	}
	if receipt.Objects[6].UID == "" {
		receipt.Objects[6].UID = current.UID
		if err := r.saveSandboxBootstrap(ctx, object, receipt); err != nil {
			return err
		}
	}
	if current.DeletionTimestamp.IsZero() {
		if err := r.Delete(ctx, current, client.Preconditions{UID: &current.UID, ResourceVersion: &current.ResourceVersion}); err != nil {
			return err
		}
		if err := r.customReader().Get(ctx, key, current); err != nil {
			return client.IgnoreNotFound(err)
		}
		if err := validateSandboxBootstrapObject(current, desired, receipt.Objects[6].UID, true); err != nil {
			return err
		}
	}
	if err := r.removeSandboxBootstrapFinalizer(ctx, current); err != nil {
		return err
	}
	if err := r.customReader().Get(ctx, key, &corev1.PersistentVolume{}); !apierrors.IsNotFound(err) {
		if err != nil {
			return err
		}
		return fmt.Errorf("Sandbox retained skills PV deletion is incomplete; bootstrap ownership remains protected")
	}
	return nil
}

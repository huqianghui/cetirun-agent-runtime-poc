// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const customCompletionFinalizer = "cetirun.microsoft.com/custom-apply-completion"

// ID identifies one attempt, not just its desired configuration. Providers must
// persist this identity before granting a lease, and resume it without acquiring
// a new lease or repeating the handoff.
type CustomApplyOperation struct {
	ID         string `json:"id"`
	PlanKey    string `json:"planKey"`
	Generation int64  `json:"generation"`
}

type customCompletion struct {
	Version   string               `json:"version"`
	AgentName string               `json:"agentName"`
	AgentUID  types.UID            `json:"agentUID"`
	Operation CustomApplyOperation `json:"operation"`
	Outcome   CustomApplyOutcome   `json:"outcome"`
}

func (r *AgentReconciler) completionKey(instance *api.Agent) types.NamespacedName {
	return r.customRecordKey(instance, "completion")
}

func validCompletion(completion customCompletion, instance *api.Agent) bool {
	if completion.Version != "v1" || completion.AgentName != instance.Name || completion.AgentUID != instance.UID {
		return false
	}
	operation := completion.Operation
	if operation.ID == "" || len(operation.ID) > 128 || operation.PlanKey == "" || len(operation.PlanKey) > 512 ||
		operation.Generation <= 0 || strings.ContainsAny(operation.ID+operation.PlanKey, "\r\n\t\x00") {
		return false
	}
	switch completion.Outcome {
	case CustomApplyNotStarted, CustomApplyUnknown, CustomApplySucceeded:
		return true
	default:
		return false
	}
}

func (r *AgentReconciler) readCustomCompletion(ctx context.Context, instance *api.Agent) (*corev1.ConfigMap, *customCompletion, error) {
	record := &corev1.ConfigMap{}
	if err := r.customReader().Get(ctx, r.completionKey(instance), record); apierrors.IsNotFound(err) {
		return nil, nil, nil
	} else if err != nil {
		return nil, nil, err
	}
	var completion customCompletion
	if !hasCustomRecordOwner(record, instance) || len(record.Data) != 1 || len(record.BinaryData) != 0 ||
		json.Unmarshal([]byte(record.Data["completion"]), &completion) != nil ||
		!validCompletion(completion, instance) {
		return nil, nil, fmt.Errorf("Custom completion record is invalid")
	}
	return record, &completion, nil
}

func (r *AgentReconciler) readCustomCompletionForOperation(ctx context.Context, instance *api.Agent, operation CustomApplyOperation) (*corev1.ConfigMap, *customCompletion, error) {
	record, completion, err := r.readCustomCompletion(ctx, instance)
	if err != nil {
		return nil, nil, err
	}
	if completion == nil || completion.Operation != operation {
		return nil, nil, fmt.Errorf("Custom completion identity changed")
	}
	return record, completion, nil
}

// The outbox is persisted before ApproveApply. It remains outside the instance
// namespace and survives foreground Agent GC until completion is acknowledged.
func (r *AgentReconciler) beginCustomCompletion(ctx context.Context, instance *api.Agent, planKey string) (CustomApplyOperation, error) {
	operation := CustomApplyOperation{ID: string(uuid.NewUUID()), PlanKey: planKey, Generation: instance.Generation}
	completion := customCompletion{
		Version: "v1", AgentName: instance.Name, AgentUID: instance.UID,
		Operation: operation, Outcome: CustomApplyNotStarted,
	}
	if !validCompletion(completion, instance) {
		return CustomApplyOperation{}, fmt.Errorf("invalid Custom operation identity")
	}
	key := r.completionKey(instance)
	data, err := json.Marshal(completion)
	if err != nil {
		return CustomApplyOperation{}, err
	}
	record := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:       key.Name,
			Namespace:  key.Namespace,
			Finalizers: []string{customCompletionFinalizer},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         api.GroupVersion.String(),
				Kind:               "Agent",
				Name:               instance.Name,
				UID:                instance.UID,
				Controller:         ptr.To(true),
				BlockOwnerDeletion: ptr.To(true),
			}},
		},
		Data: map[string]string{"completion": string(data)},
	}
	if err := r.Create(ctx, record); err != nil {
		return CustomApplyOperation{}, fmt.Errorf("cannot persist Custom completion outbox")
	}
	return operation, nil
}

func (r *AgentReconciler) recordCustomOutcome(ctx context.Context, instance *api.Agent, operation CustomApplyOperation, outcome CustomApplyOutcome) error {
	record, completion, err := r.readCustomCompletionForOperation(ctx, instance, operation)
	if err != nil {
		return err
	}
	if completion.Outcome == outcome {
		return nil
	}
	completion.Outcome = outcome
	if !validCompletion(*completion, instance) {
		return fmt.Errorf("invalid Custom completion outcome")
	}
	data, err := json.Marshal(completion)
	if err != nil {
		return err
	}
	record.Data["completion"] = string(data)
	return r.Update(ctx, record)
}

func (r *AgentReconciler) deliverCustomCompletion(ctx context.Context, instance *api.Agent, operation CustomApplyOperation, lease CustomApplyLease) error {
	record, completion, err := r.readCustomCompletionForOperation(ctx, instance, operation)
	if err != nil {
		return err
	}
	if lease == nil {
		if r.CustomBindings == nil {
			return fmt.Errorf("Custom completion provider is unavailable")
		}
		lease, err = r.CustomBindings.ResumeApply(ctx, instance.DeepCopy(), operation)
		if err != nil || lease == nil {
			return fmt.Errorf("Custom apply completion cannot be resumed")
		}
	}
	if err := lease.Complete(ctx, completion.Outcome); err != nil {
		return fmt.Errorf("Custom apply completion is still pending")
	}
	// Complete is idempotent for this operation ID. If removing the outbox
	// fails or the process exits here, recovery repeats Complete, never Helm.
	if controllerutil.ContainsFinalizer(record, customCompletionFinalizer) {
		controllerutil.RemoveFinalizer(record, customCompletionFinalizer)
		if err := r.Update(ctx, record); err != nil {
			return client.IgnoreNotFound(err)
		}
	}
	return client.IgnoreNotFound(r.Delete(ctx, record, client.Preconditions{UID: &record.UID, ResourceVersion: &record.ResourceVersion}))
}

func (r *AgentReconciler) finishCustomCompletion(ctx context.Context, instance *api.Agent, operation CustomApplyOperation, outcome CustomApplyOutcome, lease CustomApplyLease) error {
	if err := r.recordCustomOutcome(ctx, instance, operation, outcome); err != nil {
		return err
	}
	return r.deliverCustomCompletion(ctx, instance, operation, lease)
}

// Recovery precedes new configuration validation and even the "already applied"
// fast path. It also handles an older generation's completion after a restart.
func (r *AgentReconciler) retryCustomCompletion(ctx context.Context, instance *api.Agent) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, completion, err := r.readCustomCompletion(ctx, instance)
	if err != nil || completion == nil {
		return err
	}
	return r.deliverCustomCompletion(ctx, instance, completion.Operation, nil)
}

// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kube

import (
	"context"
	"errors"
	"fmt"
	"slices"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	ErrInvalidRestartRequest = errors.New("a valid Idempotency-Key, uid and resourceVersion are required")
	ErrInstanceSuspended     = errors.New("a suspended Agent cannot be restarted")
	ErrRestartUnsupported    = errors.New("retained single-start Agents do not support user-requested restart")
	ErrRestartNotReady       = errors.New("the Agent must be ready at its current generation with no pending restart")
	ErrMigrationPending      = errors.New("the planned migration must finish before requesting a restart")
)

func restartStateError(instance *api.Agent) error {
	if !instance.DeletionTimestamp.IsZero() {
		return ErrInstanceDeleting
	}
	if instance.Spec.Suspended {
		return ErrInstanceSuspended
	}
	if instance.MigrationPending() {
		return ErrMigrationPending
	}
	if slices.Contains(instance.Finalizers, api.SandboxPreparationFinalizer) {
		return ErrRestartUnsupported
	}
	return nil
}

func CanRestartInstance(instance *api.Agent) bool {
	return restartStateError(instance) == nil && instance.UID != "" && instance.ResourceVersion != "" &&
		instance.Status.Phase == api.PhaseReady && instance.Status.ObservedGeneration == instance.Generation &&
		meta.IsStatusConditionTrue(instance.Status.Conditions, api.ConditionReady) &&
		instance.Spec.RestartRequestID == instance.Status.ObservedRestartRequestID
}

func (c *Client) RestartInstanceByName(ctx context.Context, name string, version InstanceVersion,
	requestID string, authorize func(*api.Agent) error,
) (*api.Agent, error) {
	if version.UID == "" || version.ResourceVersion == "" || !api.ValidRestartRequestID(requestID) {
		return nil, ErrInvalidRestartRequest
	}
	instance, err := c.GetInstanceByName(ctx, name)
	if err != nil {
		return nil, err
	}
	if err := validateInstanceUpdate(instance, UpdateInstanceOpts{ExpectedUID: version.UID, Authorize: authorize}); err != nil {
		return nil, err
	}
	if err := restartStateError(instance); err != nil {
		return nil, err
	}
	// An exact retry can observe an accepted request after a lost response or
	// Portal restart, even if subsequent status writes changed resourceVersion.
	if instance.Spec.RestartRequestID == requestID {
		return instance, nil
	}
	if instance.ResourceVersion != version.ResourceVersion {
		return nil, ErrInstanceChanged
	}
	if !CanRestartInstance(instance) {
		return nil, ErrRestartNotReady
	}
	before := instance.DeepCopy()
	instance.Spec.RestartRequestID = requestID
	if err := c.client.Patch(ctx, instance, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}),
		client.FieldValidation(metav1.FieldValidationStrict)); err != nil {
		return nil, fmt.Errorf("request Agent restart: %w", err)
	}
	return instance, nil
}

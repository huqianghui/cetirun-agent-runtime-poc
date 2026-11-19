// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"fmt"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (r *AgentReconciler) bindCustomAgentIdentity(ctx context.Context, instance *api.Agent, bindings *runtimebindings.Custom) (bool, error) {
	if bindings.AgentIdentity != nil {
		return false, fmt.Errorf("UnsupportedFeature: Custom bindings cannot supply Agent Identity")
	}
	if agentIdentityRequested(instance) {
		return false, fmt.Errorf("UnsupportedFeature: Custom does not support Agent Identity")
	}
	status := instance.Status.AgentIdentity
	if status != nil && (status.Phase != api.AgentIdentityPhaseDisabled || status.ObservedGeneration != instance.Generation) {
		previous := instance.DeepCopy().Status
		err := r.reconcileAgentIdentity(ctx, instance)
		if err != nil {
			setCondition(&instance.Status.Conditions, api.ConditionAgentIdentityReady, metav1.ConditionFalse,
				"CleanupFailed", "Agent Identity cleanup is not ready")
		}
		if err := errors.Join(err, r.updateCustomStatus(ctx, instance, previous)); err != nil {
			return false, err
		}
	}
	return true, nil
}

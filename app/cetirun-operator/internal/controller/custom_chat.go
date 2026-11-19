// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/internal/chataccess"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *AgentReconciler) reconcileChatRoute(ctx context.Context, agent *api.Agent, enabled bool) error {
	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(chataccess.RouteGVK)
	key := client.ObjectKey{Namespace: instanceNamespace(agent.Name), Name: chataccess.RouteName(agent.Name)}
	err := r.customReader().Get(ctx, key, current)
	if apierrors.IsNotFound(err) {
		if !enabled {
			return nil
		}
		return r.Create(ctx, chataccess.PrivateRoute(agent.Name, string(agent.UID), key.Namespace, *agent.Spec.Agent.Port, r.ChatAccess.Callers))
	}
	if err != nil {
		return err
	}
	owner := metav1.GetControllerOf(current)
	if owner == nil || owner.UID != agent.UID || owner.Kind != "Agent" || current.GetAnnotations()[chataccess.AgentUIDAnnotation] != string(agent.UID) || !current.GetDeletionTimestamp().IsZero() {
		return fmt.Errorf("Chat route has foreign ownership or is deleting")
	}
	if !enabled {
		uid := current.GetUID()
		return r.Delete(ctx, current, client.Preconditions{UID: &uid})
	}
	desired := chataccess.PrivateRoute(agent.Name, string(agent.UID), key.Namespace, *agent.Spec.Agent.Port, r.ChatAccess.Callers)
	if chataccess.RouteSpecsEqual(current.Object["spec"], desired.Object["spec"]) {
		return nil
	}
	current.Object["spec"] = desired.Object["spec"]
	return r.Update(ctx, current)
}

// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kube

import (
	"context"
	"fmt"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const diagnosticAgentUIDAnnotation = "cetirun.microsoft.com/agent-uid"

// DiagnosticNamespace pins namespace observations to the namespace incarnation
// that was verified for an Agent.
type DiagnosticNamespace struct {
	Name                      string
	UID                       types.UID
	OwnerReferencePresent     bool
	AgentUIDAnnotationPresent bool
}

// GetInstanceEvents pins observations to an Agent incarnation, including when
// an older Agent with the same name still has events in the cluster.
func (c *Client) GetInstanceEvents(ctx context.Context, instance *api.Agent) ([]corev1.Event, error) {
	if instance.UID == "" {
		return nil, fmt.Errorf("Agent UID is required for diagnostics")
	}
	list := &corev1.EventList{}
	if err := c.client.List(ctx, list, client.MatchingFields{
		"involvedObject.uid":  string(instance.UID),
		"involvedObject.kind": "Agent",
	}); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// GetInstanceDiagnosticNamespace never falls back to a cluster-wide query.
// Legacy namespaces without ownership metadata retain the established naming
// convention; namespaces with ownership metadata must identify this Agent.
func (c *Client) GetInstanceDiagnosticNamespace(ctx context.Context, instance *api.Agent) (*DiagnosticNamespace, error) {
	ns := instance.Status.Namespace
	if ns == "" {
		return nil, nil
	}
	if ns != "cetirun-"+instance.Name {
		return nil, fmt.Errorf("Agent namespace does not match its resource name")
	}
	object := &corev1.Namespace{}
	if err := c.client.Get(ctx, client.ObjectKey{Name: ns}, object); err != nil {
		return nil, err
	}
	if err := verifyDiagnosticNamespace(object, instance); err != nil {
		return nil, err
	}
	if object.UID == "" {
		return nil, fmt.Errorf("namespace UID is required for diagnostics")
	}
	_, annotated := object.Annotations[diagnosticAgentUIDAnnotation]
	return &DiagnosticNamespace{
		Name: object.Name, UID: object.UID,
		OwnerReferencePresent: len(object.OwnerReferences) != 0, AgentUIDAnnotationPresent: annotated,
	}, nil
}

// VerifyInstanceDiagnosticNamespace confirms that a namespace remained the
// verified incarnation while its diagnostics were listed.
func (c *Client) VerifyInstanceDiagnosticNamespace(ctx context.Context, instance *api.Agent, namespace *DiagnosticNamespace) error {
	if namespace == nil || namespace.Name == "" || namespace.UID == "" {
		return fmt.Errorf("namespace diagnostic identity is required")
	}
	object := &corev1.Namespace{}
	if err := c.client.Get(ctx, client.ObjectKey{Name: namespace.Name}, object); err != nil {
		return err
	}
	if object.UID != namespace.UID {
		return fmt.Errorf("namespace was replaced while loading diagnostics")
	}
	if err := verifyDiagnosticNamespace(object, instance); err != nil {
		return err
	}
	if namespace.OwnerReferencePresent && len(object.OwnerReferences) == 0 {
		return fmt.Errorf("namespace ownership was removed while loading diagnostics")
	}
	if namespace.AgentUIDAnnotationPresent {
		if _, annotated := object.Annotations[diagnosticAgentUIDAnnotation]; !annotated {
			return fmt.Errorf("namespace Agent identity was removed while loading diagnostics")
		}
	}
	return nil
}

func verifyDiagnosticNamespace(object *corev1.Namespace, instance *api.Agent) error {
	if instance.UID == "" {
		return fmt.Errorf("Agent UID is required for namespace diagnostics")
	}
	if uid, annotated := object.Annotations[diagnosticAgentUIDAnnotation]; annotated && uid != string(instance.UID) {
		return fmt.Errorf("namespace identifies another Agent")
	}
	if len(object.OwnerReferences) == 0 {
		return nil
	}
	if len(object.OwnerReferences) != 1 {
		return fmt.Errorf("namespace has inconsistent ownership")
	}
	owner := object.OwnerReferences[0]
	if owner.APIVersion != api.GroupVersion.String() || owner.Kind != "Agent" || owner.Name != instance.Name || owner.UID != instance.UID {
		return fmt.Errorf("namespace belongs to another resource")
	}
	return nil
}

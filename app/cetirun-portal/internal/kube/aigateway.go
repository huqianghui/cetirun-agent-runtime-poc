// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kube

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cetirunv1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
)

// AgentUIDLabel marks instance-namespace Secrets with the UID of the Agent
// they belong to, so a Secret left from an earlier Agent of the same name is
// never used for a new one. The operator uses the same label.
const AgentUIDLabel = "cetirun.microsoft.com/agent-uid"

// ErrAgentReplaced reports that the Agent was deleted and created again
// between reading it and writing to it.
var ErrAgentReplaced = stderrors.New("the agent was replaced")

// InstanceNamespace is the namespace of an Agent's workload.
func InstanceNamespace(instanceName string) string {
	return "cetirun-" + instanceName
}

// GetSecret returns a Secret.
func (c *Client) GetSecret(ctx context.Context, namespace, name string) (*corev1.Secret, error) {
	secret := &corev1.Secret{}
	if err := c.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, secret); err != nil {
		return nil, err
	}
	return secret, nil
}

// CreateSecret creates a Secret.
func (c *Client) CreateSecret(ctx context.Context, secret *corev1.Secret) error {
	return c.client.Create(ctx, secret)
}

// UpdateSecret writes a Secret read earlier; a concurrent change fails with
// a conflict.
func (c *Client) UpdateSecret(ctx context.Context, secret *corev1.Secret) error {
	return c.client.Update(ctx, secret)
}

// IsConflict reports an optimistic-concurrency conflict.
func IsConflict(err error) bool {
	return errors.IsConflict(err)
}

// SetCopilotModels records the owner's GitHub Copilot models, the sign-in
// generation they belong to, and the time of the sign-in on the Agent for
// the operator. It writes only when the Agent still has the given UID, with a
// merge patch of just these annotations under the Agent's resource version,
// so fields this Portal's types do not know survive, as with the other
// instance writers. current, when given, runs after each read of the Agent
// and stops the write with its error; a write that another one overtakes
// conflicts and runs it again.
func (c *Client) SetCopilotModels(ctx context.Context, instanceName string, uid types.UID, models []cetirunv1alpha1.CopilotModel, generation string, current func(context.Context) error) error {
	raw, err := json.Marshal(models)
	if err != nil {
		return err
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		instance := &cetirunv1alpha1.Agent{}
		if err := c.client.Get(ctx, client.ObjectKey{Name: instanceName}, instance); err != nil {
			return err
		}
		if instance.UID != uid || instance.DeletionTimestamp != nil {
			return ErrAgentReplaced
		}
		if current != nil {
			if err := current(ctx); err != nil {
				return err
			}
		}
		before := instance.DeepCopy()
		if instance.Annotations == nil {
			instance.Annotations = map[string]string{}
		}
		instance.Annotations[cetirunv1alpha1.AnnotationCopilotModels] = string(raw)
		if generation != "" {
			instance.Annotations[cetirunv1alpha1.AnnotationCopilotSignInGeneration] = generation
		}
		// A new value on every sign-in wakes the operator even when the
		// account's models did not change.
		instance.Annotations[cetirunv1alpha1.AnnotationCopilotSignedInAt] = time.Now().UTC().Format(time.RFC3339Nano)
		return c.client.Patch(ctx, instance, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	})
}

// GetAIGatewayCatalog reads the shared models the operator publishes. It
// reports false when the catalog has not been published yet.
func (c *Client) GetAIGatewayCatalog(ctx context.Context, namespace string) (*cetirunv1alpha1.AIGatewayCatalog, bool, error) {
	configMap := &corev1.ConfigMap{}
	err := c.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: cetirunv1alpha1.AIGatewayCatalogConfigMap}, configMap)
	if errors.IsNotFound(err) {
		return &cetirunv1alpha1.AIGatewayCatalog{SharedModels: []cetirunv1alpha1.AIGatewayCatalogModel{}}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	raw, ok := configMap.Data[cetirunv1alpha1.AIGatewayCatalogKey]
	if !ok {
		return &cetirunv1alpha1.AIGatewayCatalog{SharedModels: []cetirunv1alpha1.AIGatewayCatalogModel{}}, false, nil
	}
	catalog := &cetirunv1alpha1.AIGatewayCatalog{}
	if err := json.Unmarshal([]byte(raw), catalog); err != nil {
		return nil, false, fmt.Errorf("decode the AI Gateway catalog: %w", err)
	}
	if catalog.SharedModels == nil {
		catalog.SharedModels = []cetirunv1alpha1.AIGatewayCatalogModel{}
	}
	return catalog, true, nil
}

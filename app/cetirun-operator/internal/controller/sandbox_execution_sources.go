// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *AgentReconciler) readSandboxExecutionPolicy(ctx context.Context, name string) (*api.AgentPolicy, string, error) {
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(api.GroupVersion.WithKind("AgentPolicy"))
	key := client.ObjectKey{Name: name}
	if err := r.APIReader.Get(ctx, key, object); err != nil {
		return nil, "", err
	}
	if client.ObjectKeyFromObject(object) != key {
		return nil, "", fmt.Errorf("%w: AgentPolicy source key changed", errSandboxSourceChanged)
	}
	// Capture the wire image before typed resolution so unknown controls cannot
	// disappear from the comparison. Only Gateway rate limits and the skill
	// allowlist are excluded.
	digest, err := w.ExecutionObjectDigest(object)
	if err != nil {
		return nil, "", err
	}
	policy := &api.AgentPolicy{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, policy); err != nil {
		return nil, "", err
	}
	return policy, digest, nil
}

func selectedSandboxExecutionSources(sources []w.SourceReference, binding sandbox.Binding) []w.SourceReference {
	selected := make([]w.SourceReference, 0, len(sources))
	for _, source := range sources {
		if source.APIVersion == api.GroupVersion.String() &&
			((source.Kind == "Sandbox" && source.UID != binding.SandboxUID) ||
				(source.Kind == "AgentPolicy" && source.UID != binding.AgentPolicyUID)) {
			continue
		}
		selected = append(selected, source)
	}
	return selected
}

func (r *AgentReconciler) executionSourceDigest(ctx context.Context, source w.SourceReference,
	box string, capture bool,
) (string, error) {
	var object client.Object
	if source.APIVersion == api.GroupVersion.String() && source.Kind == "Team" {
		object = &api.Team{}
	} else {
		version, err := schema.ParseGroupVersion(source.APIVersion)
		if err != nil {
			return "", err
		}
		resource := &unstructured.Unstructured{}
		resource.SetGroupVersionKind(version.WithKind(source.Kind))
		object = resource
	}
	key := client.ObjectKey{Namespace: source.Namespace, Name: source.Name}
	if err := r.APIReader.Get(ctx, key, object); err != nil {
		return "", err
	}
	if client.ObjectKeyFromObject(object) != key || object.GetUID() != source.UID ||
		object.GetResourceVersion() == "" || !object.GetDeletionTimestamp().IsZero() ||
		((capture || source.IsClusterRole()) && object.GetResourceVersion() != source.ResourceVersion) {
		return "", fmt.Errorf("%w: effective source identity changed: %s", errSandboxSourceChanged, w.SourceKey(source))
	}
	return w.ExecutionSourceDigest(object, box)
}

func (r *AgentReconciler) captureExecutionSourceDigests(ctx context.Context, intent sandboxcoordination.Intent) (map[string]string, error) {
	digests := map[string]string{}
	box := intent.SelectedSandbox()
	for _, source := range intent.Sources {
		if !w.EffectiveSource(source) {
			continue
		}
		digest, err := r.executionSourceDigest(ctx, source, box, true)
		if err != nil {
			return nil, err
		}
		digests[w.SourceKey(source)] = digest
	}
	return digests, ctx.Err()
}

func (r *AgentReconciler) verifyEffectiveExecutionSources(ctx context.Context, intent sandboxcoordination.Intent, running bool) error {
	if len(intent.Sources) == 0 || len(intent.Sources) > sandboxcoordination.MaxSources {
		return fmt.Errorf("execution source inventory exceeds its bound")
	}
	box := intent.SelectedSandbox()
	for _, source := range intent.Sources {
		if running && source.IsAgent() {
			if err := r.verifyRunningAgentSource(ctx, source); err != nil {
				return err
			}
			continue
		}
		if running && source == intent.Preparation {
			if _, err := r.readSandboxRunningExpectation(ctx, intent); err != nil {
				return err
			}
			continue
		}
		if source.APIVersion == "apps/v1" && source.Kind == "StatefulSet" &&
			source.Namespace == intent.Namespace.Name && source.Name == intent.Workload.Name {
			continue
		}
		if w.EffectiveSource(source) {
			current, err := r.executionSourceDigest(ctx, source, box, false)
			if err != nil {
				return err
			}
			if current != intent.SourceDigests[w.SourceKey(source)] {
				return fmt.Errorf("%w: effective source inputs changed: %s", errSandboxSourceChanged, w.SourceKey(source))
			}
		} else if _, err := r.readSandboxSource(ctx, source); err != nil {
			return err
		}
	}
	return ctx.Err()
}

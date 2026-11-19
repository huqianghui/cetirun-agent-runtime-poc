// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"fmt"
	"strings"

	"helm.sh/helm/v3/pkg/kube"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/cli-runtime/pkg/resource"
)

// Helm Update can POST after a NotFound without calling kube.Interface.Create.
// Inspect the unchanged release inventory before recording a pending revision.
// The caller's guard/admission still owns current UID and missing-object grants.
func (m *maintenanceMode) inspectRestoration(client kube.Interface, manifest string) error {
	resources, err := client.Build(strings.NewReader(manifest), false)
	if err != nil {
		return fmt.Errorf("build maintenance inspection inventory: %w", err)
	}
	if len(resources) != len(m.objects) {
		return configurationError(fmt.Errorf("maintenance inspection inventory changed"))
	}
	seen := map[string]bool{}
	for _, info := range resources {
		if err := m.ctx.Err(); err != nil {
			return err
		}
		obj, ok := info.Object.(*unstructured.Unstructured)
		if !ok || info.Client == nil || info.Mapping == nil || info.Mapping.Scope == nil {
			return configurationError(fmt.Errorf("maintenance requires exact inspectable manifest objects"))
		}
		key := maintenanceObjectKey(obj, m.namespace)
		expected := m.objects[key]
		if expected == nil || seen[key] || info.Name != expected.GetName() || info.Mapping.GroupVersionKind != expected.GroupVersionKind() {
			return configurationError(fmt.Errorf("maintenance inspection identity changed"))
		}
		seen[key] = true
		expected = expected.DeepCopy()
		if info.Mapping.Scope.Name() == meta.RESTScopeNameNamespace {
			if info.Namespace != m.namespace {
				return configurationError(fmt.Errorf("maintenance inspection crosses the release namespace"))
			}
			if expected.GetNamespace() == "" {
				expected.SetNamespace(m.namespace)
			}
		} else if info.Namespace != "" {
			return configurationError(fmt.Errorf("cluster-scoped maintenance object has a namespace"))
		}
		if !sameJSON(obj, expected) {
			return configurationError(fmt.Errorf("maintenance inspection differs from its validated manifest"))
		}
		current, err := resource.NewHelper(info.Client, info.Mapping).Get(info.Namespace, info.Name)
		if apierrors.IsNotFound(err) {
			if !maintenanceRestorable(info) {
				return configurationError(fmt.Errorf("maintenance cannot restore missing %s", obj.GetKind()))
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect maintenance %s: %w", obj.GetKind(), err)
		}
		metadata, err := meta.Accessor(current)
		if err != nil || metadata.GetUID() == "" || metadata.GetName() != info.Name || metadata.GetNamespace() != info.Namespace || metadata.GetDeletionTimestamp() != nil {
			return configurationError(fmt.Errorf("maintenance object identity is missing or terminating"))
		}
	}
	return nil
}

func maintenanceRestorable(info *resource.Info) bool {
	obj := info.Object.(*unstructured.Unstructured)
	if info.Mapping.Scope.Name() != meta.RESTScopeNameNamespace || obj.GetUID() != "" || obj.GetResourceVersion() != "" || obj.GetAnnotations()["helm.sh/hook"] != "" {
		return false
	}
	switch obj.GetAPIVersion() + "/" + obj.GetKind() {
	case "v1/ConfigMap":
		return obj.Object["immutable"] == nil || obj.Object["immutable"] == false
	case "apps/v1/StatefulSet", "apps/v1/Deployment":
		replicas, found, err := unstructured.NestedInt64(obj.Object, "spec", "replicas")
		return err == nil && found && replicas == 0
	default:
		return false
	}
}

// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/cetiadmin"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage/driver"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type cetiAdminReleaseReader interface {
	CurrentRelease(context.Context, string, string) (*release.Release, error)
}

// CetiAdminRuntimeResources resolves stable manifest names to actual UIDs.
// Transient hooks are diagnosed through namespace read access and checked by
// the maintenance handoff; their changing job UIDs do not rewrite a live grant.
func (r *AgentReconciler) CetiAdminRuntimeResources(ctx context.Context, agent *api.Agent, target cetiadmin.TargetIdentity) ([]cetiadmin.ManagedResource, error) {
	if agent == nil || agent.UID == "" || target.Agent != identityOf(agent) || target.Namespace.UID == "" ||
		target.Namespace.Name != instanceNamespace(agent.Name) || target.Release != helmReleaseName(agent.Name) {
		return nil, cetiadmin.ErrConflict
	}
	namespace := &corev1.Namespace{}
	if err := r.customReader().Get(ctx, client.ObjectKey{Name: target.Namespace.Name}, namespace); err != nil {
		return nil, err
	}
	if !identityMatches(target.Namespace, namespace) {
		return nil, cetiadmin.ErrConflict
	}
	reader, ok := r.HelmClient.(cetiAdminReleaseReader)
	if !ok {
		return nil, fmt.Errorf("CetiAdmin requires the current release manifest reader")
	}
	current, err := reader.CurrentRelease(ctx, target.Namespace.Name, target.Release)
	if errors.Is(err, driver.ErrReleaseNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if current == nil || current.Name != target.Release || current.Namespace != target.Namespace.Name || current.Version < 1 || current.Info == nil || len(current.Manifest) > 2<<20 {
		return nil, fmt.Errorf("CetiAdmin release identity or manifest is invalid")
	}
	decoder := yaml.NewYAMLOrJSONDecoder(strings.NewReader(current.Manifest), 4096)
	seen := map[string]bool{}
	recorded := map[string]bool{}
	var result []cetiadmin.ManagedResource
	count := 0
	for {
		object := &unstructured.Unstructured{}
		if err := decoder.Decode(object); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("CetiAdmin release manifest cannot be decoded")
		}
		if len(object.Object) == 0 {
			continue
		}
		count++
		if count > 256 {
			return nil, fmt.Errorf("CetiAdmin release contains too many manifest objects")
		}
		if object.GetAnnotations()["helm.sh/hook"] != "" {
			continue
		}
		if !cetiadmin.SupportedManifestKind(object.GroupVersionKind().Group, object.GetKind()) || object.GetName() == "" {
			return nil, fmt.Errorf("CetiAdmin release %q uses an unsupported manifest kind %s", target.Release, object.GroupVersionKind())
		}
		mapping, err := r.RESTMapper().RESTMapping(object.GroupVersionKind().GroupKind(), object.GroupVersionKind().Version)
		if err != nil {
			return nil, err
		}
		if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
			if object.GetNamespace() == "" {
				object.SetNamespace(target.Namespace.Name)
			}
			if object.GetNamespace() != target.Namespace.Name {
				return nil, fmt.Errorf("CetiAdmin release crosses the target namespace")
			}
		} else if object.GetNamespace() != "" {
			return nil, fmt.Errorf("CetiAdmin cluster object has a namespace")
		}
		key := object.GetAPIVersion() + "/" + object.GetKind() + "/" + object.GetNamespace() + "/" + object.GetName()
		if seen[key] {
			return nil, fmt.Errorf("CetiAdmin release repeats a manifest identity")
		}
		seen[key] = true
		actual := &unstructured.Unstructured{}
		actual.SetGroupVersionKind(object.GroupVersionKind())
		if err := r.customReader().Get(ctx, client.ObjectKeyFromObject(object), actual); apierrors.IsNotFound(err) {
			// Failed/first installations can have a partial manifest. Missing
			// objects grant no UID-based mutation permission.
			continue
		} else if err != nil {
			return nil, err
		}
		if !cetiAdminManifestOwned(actual, agent, target) {
			return nil, fmt.Errorf("CetiAdmin runtime object ownership conflicts")
		}
		if !recorded[key] {
			result = append(result, cetiadmin.ManagedResource{APIVersion: actual.GetAPIVersion(), Kind: actual.GetKind(), ObjectIdentity: identityOf(actual)})
			recorded[key] = true
		}
		if actual.GroupVersionKind() == (schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "StatefulSet"}) {
			claims, err := r.cetiAdminRuntimeClaims(ctx, actual, target)
			if err != nil {
				return nil, err
			}
			for _, claim := range claims {
				key := claim.APIVersion + "/" + claim.Kind + "/" + claim.Namespace + "/" + claim.Name
				if !recorded[key] {
					result = append(result, claim)
					recorded[key] = true
				}
			}
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].APIVersion+"/"+result[i].Kind+"/"+result[i].Name < result[j].APIVersion+"/"+result[j].Kind+"/"+result[j].Name
	})
	return result, nil
}

func cetiAdminManifestOwned(object client.Object, agent *api.Agent, target cetiadmin.TargetIdentity) bool {
	if object.GetUID() == "" || object.GetDeletionTimestamp() != nil {
		return false
	}
	if hasAgentOwner(object, agent) {
		return true
	}
	return len(object.GetOwnerReferences()) == 0 && object.GetAnnotations()["meta.helm.sh/release-name"] == target.Release &&
		object.GetAnnotations()["meta.helm.sh/release-namespace"] == target.Namespace.Name && object.GetLabels()["app.kubernetes.io/managed-by"] == "Helm"
}

func (r *AgentReconciler) cetiAdminRuntimeClaims(ctx context.Context, object *unstructured.Unstructured, target cetiadmin.TargetIdentity) ([]cetiadmin.ManagedResource, error) {
	var workload appsv1.StatefulSet
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &workload); err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, claim := range workload.Spec.VolumeClaimTemplates {
		names[claim.Name+"-"+workload.Name+"-0"] = true
	}
	for _, volume := range workload.Spec.Template.Spec.Volumes {
		if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName != "" {
			names[volume.PersistentVolumeClaim.ClaimName] = true
		}
	}
	var result []cetiadmin.ManagedResource
	for name := range names {
		claim := &corev1.PersistentVolumeClaim{}
		if err := r.customReader().Get(ctx, client.ObjectKey{Name: name, Namespace: target.Namespace.Name}, claim); apierrors.IsNotFound(err) {
			continue
		} else if err != nil {
			return nil, err
		}
		if claim.UID == "" || claim.DeletionTimestamp != nil {
			return nil, fmt.Errorf("CetiAdmin runtime claim is not current")
		}
		result = append(result, cetiadmin.ManagedResource{APIVersion: "v1", Kind: "PersistentVolumeClaim", ObjectIdentity: identityOf(claim)})
	}
	return result, nil
}

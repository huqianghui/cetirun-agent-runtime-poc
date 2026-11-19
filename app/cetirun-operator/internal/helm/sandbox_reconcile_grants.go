// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"context"
	"errors"
	"fmt"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"helm.sh/helm/v3/pkg/kube"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/rest"
)

type reconciledGrantKube struct {
	kube.Interface
	prepared *PreparedRelease
}

func (k *reconciledGrantKube) Create(resources kube.ResourceList) (*kube.Result, error) {
	result := &kube.Result{}
	for _, info := range resources {
		if info == nil || info.Object == nil {
			return result, fmt.Errorf("Sandbox grant creation requires a valid resource")
		}
		object, err := sandboxUnstructured(info.Object)
		if err != nil {
			return result, err
		}
		var created *kube.Result
		if w.KubernetesBindingResource(*object) || reconciledWorkloadResource(*object) {
			created, err = k.createGuarded(info, *object)
		} else {
			created, err = k.Interface.Create(kube.ResourceList{info})
		}
		if created != nil {
			result.Created = append(result.Created, created.Created...)
		}
		if err != nil {
			return result, err
		}
	}
	return result, nil
}

func reconciledWorkloadResource(object unstructured.Unstructured) bool {
	return object.GetAPIVersion() == "apps/v1" &&
		(object.GetKind() == "StatefulSet" || object.GetKind() == "Deployment")
}

type reconciledGrantRESTClient struct {
	resource.RESTClient
}

func (c reconciledGrantRESTClient) Post() *rest.Request {
	// A retry must not hide an uncertain write behind a later rejection.
	return c.RESTClient.Post().MaxRetries(0)
}

func (k *reconciledGrantKube) createGuarded(info *resource.Info, object unstructured.Unstructured) (*kube.Result, error) {
	if info.Client == nil || info.Mapping == nil {
		return nil, fmt.Errorf("guarded Sandbox creation requires its API resource mapping")
	}
	var complete func(error) error
	var err error
	if w.KubernetesBindingResource(object) {
		complete, err = k.prepared.AuthorizeSandboxBindingCreate(context.Background(), object)
	} else if object.GetKind() == "Deployment" {
		err = k.prepared.AuthorizeSandboxAuxiliaryApply(context.Background(), object)
		complete = func(err error) error { return err }
	} else {
		complete, err = k.prepared.AuthorizeSandboxWorkloadCreate(context.Background())
	}
	if err != nil {
		return nil, err
	}
	original := info.Client
	info.Client = reconciledGrantRESTClient{original}
	defer func() { info.Client = original }()
	result, err := k.Interface.Create(kube.ResourceList{info})
	return result, complete(err)
}

func sandboxBindingCreateRejected(err error) bool {
	var status apierrors.APIStatus
	if !errors.As(err, &status) || status.Status().Code < 400 || status.Status().Code >= 500 {
		return false
	}
	switch apierrors.ReasonForError(err) {
	case metav1.StatusReasonBadRequest, metav1.StatusReasonUnauthorized, metav1.StatusReasonForbidden,
		metav1.StatusReasonNotFound, metav1.StatusReasonAlreadyExists, metav1.StatusReasonConflict,
		metav1.StatusReasonInvalid, metav1.StatusReasonMethodNotAllowed, metav1.StatusReasonRequestEntityTooLarge,
		metav1.StatusReasonUnsupportedMediaType, metav1.StatusReasonTooManyRequests:
		return true
	default:
		return false
	}
}

func (k *reconciledGrantKube) Update(original, target kube.ResourceList, force bool) (*kube.Result, error) {
	if force {
		return &kube.Result{}, fmt.Errorf("Sandbox grant reconciliation cannot force replacement")
	}
	withoutGrants := func(resources kube.ResourceList) (kube.ResourceList, error) {
		var result kube.ResourceList
		for _, info := range resources {
			if info == nil || info.Object == nil {
				return nil, fmt.Errorf("Sandbox grant reconciliation requires a valid resource")
			}
			object, err := sandboxUnstructured(info.Object)
			if err != nil {
				return nil, err
			}
			if !w.KubernetesBindingResource(*object) {
				result = append(result, info)
			}
		}
		return result, nil
	}
	previous, err := withoutGrants(original)
	if err != nil {
		return &kube.Result{}, err
	}
	next, err := withoutGrants(target)
	if err != nil {
		return &kube.Result{}, err
	}
	result := &kube.Result{}
	for _, info := range next {
		object, err := sandboxUnstructured(info.Object)
		if err != nil {
			return result, err
		}
		var updated *kube.Result
		old := previous.Get(info)
		workload := reconciledWorkloadResource(*object)
		missing := false
		if workload {
			if info.Client == nil || info.Mapping == nil {
				return result, fmt.Errorf("workload reconciliation requires its API resource mapping")
			}
			if err := k.prepared.CheckSandboxApply(context.Background()); err != nil {
				return result, err
			}
			_, err = resource.NewHelper(info.Client, info.Mapping).Get(info.Namespace, info.Name)
			missing = apierrors.IsNotFound(err)
			if err != nil && !missing {
				return result, err
			}
			if !missing && old == nil {
				return result, fmt.Errorf("an unrecorded workload appeared before initial creation")
			}
		}
		if workload && missing {
			// Failed Helm history can contain a workload that was never created.
			// Only the durable controller checkpoint may prove non-submission.
			updated, err = k.createGuarded(info, *object)
		} else {
			if workload {
				if object.GetKind() == "Deployment" {
					err = k.prepared.AuthorizeSandboxAuxiliaryApply(context.Background(), *object)
				} else {
					err = k.prepared.AuthorizeSandboxApply(context.Background())
				}
				if err != nil {
					return result, err
				}
			}
			var before kube.ResourceList
			if old != nil {
				before = append(before, old)
			}
			updated, err = k.Interface.Update(before, kube.ResourceList{info}, false)
		}
		if updated != nil {
			result.Created = append(result.Created, updated.Created...)
			result.Updated = append(result.Updated, updated.Updated...)
			result.Deleted = append(result.Deleted, updated.Deleted...)
		}
		if err != nil {
			return result, err
		}
	}
	// Grant retirement belongs to the UID-bound journal, not Helm's
	// name-only pruning. Retire other obsolete objects only after all writes.
	var obsolete kube.ResourceList
	for _, info := range previous {
		if next.Get(info) == nil {
			obsolete = append(obsolete, info)
		}
	}
	retired, err := k.Interface.Update(obsolete, nil, false)
	if retired != nil {
		result.Deleted = append(result.Deleted, retired.Deleted...)
	}
	if err != nil {
		return result, err
	}
	for _, info := range target {
		desired, err := sandboxUnstructured(info.Object)
		if err != nil {
			return result, err
		}
		if !w.KubernetesBindingResource(*desired) {
			continue
		}
		if info.Client == nil || info.Mapping == nil {
			return result, fmt.Errorf("Kubernetes grant update requires its API resource mapping")
		}
		if err := k.prepared.CheckSandboxApply(context.Background()); err != nil {
			return result, err
		}
		currentObject, err := resource.NewHelper(info.Client, info.Mapping).Get(info.Namespace, info.Name)
		if apierrors.IsNotFound(err) {
			created, err := k.Create(kube.ResourceList{info})
			result.Created = append(result.Created, created.Created...)
			if err != nil {
				return result, err
			}
			continue
		}
		if err != nil {
			return result, err
		}
		current, err := sandboxUnstructured(currentObject)
		identity := desired.DeepCopy()
		// Release metadata may advance without changing the grant's identity.
		identity.SetLabels(map[string]string{sandboxpolicy.AgentUIDLabel: desired.GetLabels()[sandboxpolicy.AgentUIDLabel]})
		identity.SetAnnotations(map[string]string{sandboxpolicy.GrantRevisionAnnotation: desired.GetAnnotations()[sandboxpolicy.GrantRevisionAnnotation]})
		if err != nil || current.GetResourceVersion() == "" || !w.ReconciledObjectMatches(current, identity) {
			return result, fmt.Errorf("Kubernetes grant update requires its current owned binding")
		}
		patch, patchType, err := sandboxStagePatch(original.Get(info), info, current)
		if err != nil {
			return result, err
		}
		updated, err := info.Client.Patch(patchType).
			NamespaceIfScoped(info.Namespace, info.Mapping.Scope.Name() == meta.RESTScopeNameNamespace).
			Resource(info.Mapping.Resource.Resource).Name(info.Name).Body(patch).Do(context.Background()).Get()
		if err != nil {
			return result, fmt.Errorf("update current Kubernetes grant: %w", err)
		}
		if err := info.Refresh(updated, true); err != nil {
			return result, err
		}
		result.Updated = append(result.Updated, info)
	}
	return result, nil
}

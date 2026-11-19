// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	kubeyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
)

var ErrReleaseOperationPending = errors.New("Custom release operation is pending; coordinated recovery is required")

type ConfigurationError struct{ Err error }

func (e *ConfigurationError) Error() string { return e.Err.Error() }
func (e *ConfigurationError) Unwrap() error { return e.Err }

func IsConfigurationError(err error) bool {
	var configuration *ConfigurationError
	return errors.As(err, &configuration)
}

func configurationError(err error) error {
	if err == nil {
		return nil
	}
	return &ConfigurationError{Err: err}
}

// validateCustomAPI checks exactly the validated bytes, never source templates
// or lookup. It runs before lifecycle approval and performs no persistent writes.
func (p *PreparedRelease) validateCustomAPI(ctx context.Context, config *rest.Config) error {
	p.apiValidated = false
	config = rest.CopyConfig(config)
	config.Timeout = 30 * time.Second
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return err
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return err
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(discoveryClient))
	namespace := text(object(object(object(p.Values["runtime"])["bindings"])["namespace"])["name"])
	if err := p.ValidateCustom(namespace, p.releaseName); err != nil {
		return configurationError(err)
	}
	decoder := kubeyaml.NewYAMLOrJSONDecoder(bytes.NewReader(p.manifest), 4096)
	for {
		var obj unstructured.Unstructured
		if err := decoder.Decode(&obj); err == io.EOF {
			break
		} else if err != nil {
			return configurationError(err)
		}
		if len(obj.Object) == 0 {
			continue
		}
		gvk := obj.GroupVersionKind()
		mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			return err
		}
		var resource dynamic.ResourceInterface = dynamicClient.Resource(mapping.Resource)
		if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
			resource = dynamicClient.Resource(mapping.Resource).Namespace(namespace)
			obj.SetNamespace(namespace)
		}
		current, err := resource.Get(ctx, obj.GetName(), metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			_, err = resource.Create(ctx, &obj, metav1.CreateOptions{
				DryRun: []string{metav1.DryRunAll}, FieldValidation: metav1.FieldValidationStrict,
			})
		} else if err == nil {
			obj.SetResourceVersion(current.GetResourceVersion())
			raw, marshalErr := json.Marshal(obj.Object)
			if marshalErr != nil {
				return marshalErr
			}
			_, err = resource.Patch(ctx, obj.GetName(), types.MergePatchType, raw, metav1.PatchOptions{
				DryRun: []string{metav1.DryRunAll}, FieldValidation: metav1.FieldValidationStrict,
			})
		}
		if err != nil {
			if apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) {
				return configurationError(fmt.Errorf("Custom API preflight rejected %s: %w", obj.GetKind(), err))
			}
			return fmt.Errorf("Custom API preflight unavailable for %s: %w", obj.GetKind(), err)
		}
	}
	p.apiValidated = true
	return nil
}

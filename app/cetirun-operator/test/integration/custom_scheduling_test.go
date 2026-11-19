// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package integration_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestCustomSchedulingPreflightMatchesAPI(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	namespace := "custom-scheduling-validation"
	namespaces := coreClient.Resource(schema.GroupVersionResource{Version: "v1", Resource: "namespaces"})
	if _, err := namespaces.Create(ctx, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": namespace},
	}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	defer namespaces.Delete(ctx, namespace, metav1.DeleteOptions{})
	classes := coreClient.Resource(schema.GroupVersionResource{Group: "node.k8s.io", Version: "v1", Resource: "runtimeclasses"})
	className := "custom-test.example.com"
	if _, err := classes.Create(ctx, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "node.k8s.io/v1", "kind": "RuntimeClass",
		"metadata": map[string]any{"name": className}, "handler": "test",
	}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	defer classes.Delete(ctx, className, metav1.DeleteOptions{})
	pods := coreClient.Resource(schema.GroupVersionResource{Version: "v1", Resource: "pods"}).Namespace(namespace)
	for i, test := range []struct {
		name   string
		values map[string]any
		valid  bool
	}{
		{"none", map[string]any{}, true},
		{"valid", map[string]any{"runtimeClassName": className, "nodeSelector": map[string]any{"example.com/node": ""}}, true},
		{"invalid-class", map[string]any{"runtimeClassName": "INVALID_CLASS"}, false},
		{"invalid-value", map[string]any{"nodeSelector": map[string]any{"example.com/node": "value/with/slash"}}, false},
		{"invalid-key", map[string]any{"nodeSelector": map[string]any{"bad key": "value"}}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := map[string]any{"containers": []any{map[string]any{"name": "example", "image": "registry.example.com/agent:1"}}}
			for key, value := range test.values {
				spec[key] = value
			}
			_, apiErr := pods.Create(ctx, &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{
					"name": fmt.Sprintf("scheduling-%d", i), "namespace": namespace},
				"spec": spec,
			}}, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
			localErr := runtimebindings.ValidateScheduling(test.values)
			if (apiErr == nil) != test.valid || (localErr == nil) != test.valid {
				t.Fatalf("valid=%v API=%v preflight=%v", test.valid, apiErr, localErr)
			}
		})
	}
}

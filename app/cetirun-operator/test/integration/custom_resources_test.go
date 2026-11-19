// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestCustomResourcePreflightMatchesAPI(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ns := coreClient.Resource(schema.GroupVersionResource{Version: "v1", Resource: "namespaces"})
	namespace := "custom-resource-validation"
	if _, err := ns.Create(ctx, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": namespace},
	}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	defer ns.Delete(ctx, namespace, metav1.DeleteOptions{})
	pods := coreClient.Resource(schema.GroupVersionResource{Version: "v1", Resource: "pods"}).Namespace(namespace)
	for _, test := range []struct {
		name, resource, request, limit string
		valid                          bool
	}{
		{"cpu-request-only", "cpu", "100m", "", true},
		{"extended-request-only", "example.com/device", "1", "", false},
		{"extended-limit-only", "example.com/device", "", "1", true},
		{"extended-equal", "example.com/device", "1", "1", true},
		{"hugepage-request-only", "hugepages-2Mi", "4Mi", "", false},
		{"hugepage-nonmultiple", "hugepages-2Mi", "3Mi", "3Mi", false},
		{"hugepage-whole-pages", "hugepages-2Mi", "4Mi", "4Mi", true},
		{"hugepages-only", "hugepages-2Mi", "4Mi", "4Mi", false},
		{"native-qualified-resource", "kubernetes.io/device", "1", "1", true},
		{"native-qualified-request-only", "kubernetes.io/device", "0.5", "", true},
		{"reserved-request-prefix", "requests.example.com/device", "1", "1", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := map[string]any{"cpu": "100m"}
			limits := map[string]any{"cpu": "100m"}
			if test.request != "" {
				requests[test.resource] = test.request
			} else {
				delete(requests, test.resource)
			}
			if test.limit != "" {
				limits[test.resource] = test.limit
			} else {
				delete(limits, test.resource)
			}
			resources := map[string]any{"requests": requests, "limits": limits}
			if test.name == "hugepages-only" {
				delete(requests, "cpu")
				delete(limits, "cpu")
			}
			pod := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": test.name, "namespace": namespace},
				"spec": map[string]any{"containers": []any{map[string]any{
					"name": "example", "image": "registry.example.com/agent:1", "resources": resources,
				}}},
			}}
			_, apiErr := pods.Create(ctx, pod, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
			if (apiErr == nil) != test.valid {
				t.Fatalf("API valid=%v error=%v", test.valid, apiErr)
			}
			_, localErr := runtimebindings.ValidateResources(resources)
			if (localErr == nil) != test.valid {
				t.Fatalf("preflight/API disagree: preflight=%v API=%v", localErr, apiErr)
			}
		})
	}
}

// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"helm.sh/helm/v3/pkg/action"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestReconciledSandboxFirstWorkloadRejectionAfterControlAdoption(t *testing.T) {
	source, _, _ := sandboxSDKKubernetesAccessPlans(t)
	prepared := reconciledGrantSDKPlan(t, source, func(context.Context, unstructured.Unstructured) (func(context.Context) error, error) {
		return func(context.Context) error { return nil }, nil
	})
	cfg, server, _ := sandboxSDKConfig()
	for _, object := range prepared.sandboxReconcile.Controls {
		if object.GetKind() != "ServiceAccount" {
			continue
		}
		existing := object.DeepCopy()
		existing.SetUID("existing-control")
		existing.SetResourceVersion("existing-version")
		annotations := existing.GetAnnotations()
		annotations["meta.helm.sh/release-name"] = prepared.releaseName
		annotations["meta.helm.sh/release-namespace"] = prepared.sandboxReconcile.Namespace.Name
		existing.SetAnnotations(annotations)
		labels := existing.GetLabels()
		labels["app.kubernetes.io/managed-by"] = "Helm"
		existing.SetLabels(labels)
		server.objects[sandboxTestPath(existing)] = existing
	}
	if len(server.objects) == 0 {
		t.Fatal("fixture did not stage a control for Helm's initial update path")
	}
	cancelled, posts := false, 0
	prepared.sandboxWorkloadCreate = func(context.Context) (func(context.Context) error, error) {
		return func(context.Context) error { cancelled = true; return nil }, nil
	}
	server.respond = func(call sandboxSDKRequest) (*http.Response, error) {
		if call.method == http.MethodPost && strings.HasSuffix(call.path, "/statefulsets") {
			for _, control := range prepared.sandboxReconcile.Controls {
				if control.GetKind() == "ConfigMap" && server.objects[sandboxTestPath(&control)] == nil {
					t.Fatal("first workload was submitted before its earlier Helm controls")
				}
			}
			posts++
			return sandboxSDKStatus(http.StatusForbidden, metav1.StatusReasonForbidden)
		}
		return nil, nil
	}
	err := (&Client{}).installOrUpgrade(prepared.sandboxReconcile.Namespace.Name, prepared.releaseName, prepared,
		func(string) (*action.Configuration, error) { return cfg, nil })
	if err == nil || posts != 1 || !cancelled {
		t.Fatalf("first CREATE inside control adoption lost non-creation: posts=%d cancelled=%t err=%v", posts, cancelled, err)
	}
}

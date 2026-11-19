// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"helm.sh/helm/v3/pkg/action"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestReconciledSandboxSDKWorkloadSubmissionBoundary(t *testing.T) {
	for _, operation := range []string{"install", "adopt", "upgrade"} {
		for _, failure := range []string{"none", "grant-rejected", "grant-timeout", "workload-guard", "workload-timeout", "workload-lost-response"} {
			t.Run(operation+"/"+failure, func(t *testing.T) {
				source, _, _ := sandboxSDKKubernetesAccessPlans(t)
				bindingGuard := func(context.Context, unstructured.Unstructured) (func(context.Context) error, error) {
					return func(context.Context) error { return nil }, nil
				}
				prepared := reconciledGrantSDKPlan(t, source, bindingGuard)
				cfg, api, _ := sandboxSDKConfig()
				configure := func(string) (*action.Configuration, error) { return cfg, nil }
				if operation == "upgrade" {
					initial := reconciledGrantSDKPlan(t, source, bindingGuard)
					if err := (&Client{}).installOrUpgrade(initial.sandboxReconcile.Namespace.Name, initial.releaseName, initial, configure); err != nil {
						t.Fatal(err)
					}
					for path, object := range api.objects {
						if w.KubernetesBindingResource(*object) {
							delete(api.objects, path)
						} else if reconciledWorkloadResource(*object) {
							if err := unstructured.SetNestedField(object.Object, int64(0), "spec", "replicas"); err != nil {
								t.Fatal(err)
							}
						}
					}
				}
				if operation == "adopt" {
					for _, object := range prepared.sandboxReconcile.Controls {
						if object.GetKind() != "ServiceAccount" {
							continue
						}
						existing := object.DeepCopy()
						existing.SetUID("existing-service-account")
						existing.SetResourceVersion("existing-version")
						annotations := existing.GetAnnotations()
						annotations["meta.helm.sh/release-name"] = prepared.releaseName
						annotations["meta.helm.sh/release-namespace"] = prepared.sandboxReconcile.Namespace.Name
						existing.SetAnnotations(annotations)
						labels := existing.GetLabels()
						labels["app.kubernetes.io/managed-by"] = "Helm"
						existing.SetLabels(labels)
						api.objects[sandboxTestPath(existing)] = existing
					}
					if len(api.objects) == 0 {
						t.Fatal("fixture did not stage a resource for the install update path")
					}
				}
				rendered, submitted, workloadWrites := false, false, 0
				prepared.sandboxRenderGuard = func(context.Context) error {
					rendered = true
					return nil
				}
				prepared.sandboxGuard = func(context.Context) error {
					if !rendered {
						t.Fatal("workload submission preceded manifest authorization")
					}
					if failure == "workload-guard" {
						return errors.New("synthetic workload authority failure")
					}
					submitted = true
					return nil
				}
				api.before = func(call sandboxSDKRequest) {
					if call.method != http.MethodPost && call.method != http.MethodPatch && call.method != http.MethodPut {
						return
					}
					if !rendered {
						t.Fatal("Helm wrote a resource before manifest authorization")
					}
					if strings.Contains(call.path, "/statefulsets") || strings.Contains(call.path, "/deployments") {
						workloadWrites++
						if !submitted {
							t.Fatal("Helm submitted a workload without its durable checkpoint")
						}
					}
					if operation == "install" && strings.HasSuffix(call.path, "/clusterrolebindings") && submitted {
						t.Fatal("grant creation prematurely marked the first workload submitted")
					}
				}
				api.respond = func(call sandboxSDKRequest) (*http.Response, error) {
					if call.method != http.MethodPost && call.method != http.MethodPatch {
						return nil, nil
					}
					if strings.HasSuffix(call.path, "/clusterrolebindings") {
						switch failure {
						case "grant-rejected":
							return sandboxSDKStatus(http.StatusForbidden, metav1.StatusReasonForbidden)
						case "grant-timeout":
							return sandboxSDKStatus(http.StatusGatewayTimeout, metav1.StatusReasonTimeout)
						}
					}
					if strings.Contains(call.path, "/statefulsets") || strings.Contains(call.path, "/deployments") {
						switch failure {
						case "workload-timeout":
							return sandboxSDKStatus(http.StatusGatewayTimeout, metav1.StatusReasonTimeout)
						case "workload-lost-response":
							return nil, io.ErrUnexpectedEOF
						}
					}
					return nil, nil
				}
				err := (&Client{}).installOrUpgrade(prepared.sandboxReconcile.Namespace.Name, prepared.releaseName, prepared, configure)
				wantSubmission := failure != "workload-guard" &&
					(operation != "install" || (failure != "grant-rejected" && failure != "grant-timeout"))
				if (err != nil) != (failure != "none") || !rendered || submitted != wantSubmission ||
					(workloadWrites != 0) != wantSubmission {
					t.Fatalf("incorrect workload boundary: rendered=%t submitted=%t writes=%d wantSubmission=%t error=%v",
						rendered, submitted, workloadWrites, wantSubmission, err)
				}
			})
		}
	}
}

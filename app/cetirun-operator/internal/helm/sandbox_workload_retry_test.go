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

func TestReconciledSandboxRejectedFirstWorkloadRetriesKeepOutcome(t *testing.T) {
	source, _, _ := sandboxSDKKubernetesAccessPlans(t)
	cfg, server, _ := sandboxSDKConfig()
	reject := true
	server.respond = func(call sandboxSDKRequest) (*http.Response, error) {
		if reject && call.method == http.MethodPost && strings.HasSuffix(call.path, "/statefulsets") {
			return sandboxSDKStatus(http.StatusForbidden, metav1.StatusReasonForbidden)
		}
		return nil, nil
	}
	creates, cancellations := 0, 0
	for attempt := range 3 {
		prepared := reconciledGrantSDKPlan(t, source, func(context.Context, unstructured.Unstructured) (func(context.Context) error, error) {
			return func(context.Context) error { return nil }, nil
		})
		prepared.sandboxWorkloadCreate = func(context.Context) (func(context.Context) error, error) {
			creates++
			return func(context.Context) error { cancellations++; return nil }, nil
		}
		reject = attempt < 2
		err := (&Client{}).installOrUpgrade(prepared.sandboxReconcile.Namespace.Name, prepared.releaseName, prepared,
			func(string) (*action.Configuration, error) { return cfg, nil })
		if (err != nil) != reject || creates != attempt+1 || cancellations != min(attempt+1, 2) {
			t.Fatalf("failed release history lost first-CREATE proof: attempt=%d creates=%d cancellations=%d err=%v",
				attempt, creates, cancellations, err)
		}
	}
	workloads := 0
	for _, value := range server.objects {
		if value.GetKind() == "StatefulSet" {
			workloads++
		}
	}
	if workloads != 1 {
		t.Fatal("restored permission did not create exactly one workload")
	}
}

func TestReconciledSandboxMissingEarlierWorkloadKeepsOpaqueOutcome(t *testing.T) {
	for _, earlier := range []string{"created", "uncertain"} {
		t.Run(earlier, func(t *testing.T) {
			source, _, _ := sandboxSDKKubernetesAccessPlans(t)
			cfg, server, _ := sandboxSDKConfig()
			guard := func(context.Context, unstructured.Unstructured) (func(context.Context) error, error) {
				return func(context.Context) error { return nil }, nil
			}
			initial := reconciledGrantSDKPlan(t, source, guard)
			if earlier == "uncertain" {
				server.respond = func(call sandboxSDKRequest) (*http.Response, error) {
					if call.method == http.MethodPost && strings.HasSuffix(call.path, "/statefulsets") {
						return sandboxSDKStatus(http.StatusGatewayTimeout, metav1.StatusReasonTimeout)
					}
					return nil, nil
				}
			}
			err := (&Client{}).installOrUpgrade(initial.sandboxReconcile.Namespace.Name, initial.releaseName, initial,
				func(string) (*action.Configuration, error) { return cfg, nil })
			if (err != nil) != (earlier == "uncertain") {
				t.Fatal("unexpected initial submission result:", err)
			}
			for path, object := range server.objects {
				if object.GetKind() == "StatefulSet" {
					delete(server.objects, path)
				}
			}
			attempted := false
			next := reconciledGrantSDKPlan(t, source, guard)
			next.sandboxWorkloadCreate = func(context.Context) (func(context.Context) error, error) {
				attempted = true
				return nil, nil
			}
			server.respond = func(call sandboxSDKRequest) (*http.Response, error) {
				if call.method == http.MethodPost && strings.HasSuffix(call.path, "/statefulsets") {
					return sandboxSDKStatus(http.StatusForbidden, metav1.StatusReasonForbidden)
				}
				return nil, nil
			}
			err = (&Client{}).installOrUpgrade(next.sandboxReconcile.Namespace.Name, next.releaseName, next,
				func(string) (*action.Configuration, error) { return cfg, nil })
			if err == nil || !attempted || strings.Contains(err.Error(), "record rejected Sandbox workload creation") {
				t.Fatal("a later rejection invented non-creation authority for earlier work:", err)
			}
		})
	}
}

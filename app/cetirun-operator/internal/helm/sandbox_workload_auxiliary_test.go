// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"helm.sh/helm/v3/pkg/action"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestReconciledSandboxAuxiliaryKeepsPrimaryCreateOutcome(t *testing.T) {
	bundle, values := reconciledSandboxInputs(t)
	base, err := PrepareSandboxChart(testChart(t), values, nil, bundle)
	if err != nil {
		t.Fatal(err)
	}
	cfg, server, _ := sandboxSDKConfig()
	primarySubmitted, reject := false, true
	auxiliaryWrites, primaryWrites, cancellations := 0, 0, 0
	server.respond = func(call sandboxSDKRequest) (*http.Response, error) {
		if strings.Contains(call.path, "/deployments") && call.method != http.MethodGet {
			if primarySubmitted {
				t.Fatal("auxiliary write consumed the primary submission proof")
			}
			auxiliaryWrites++
		}
		if call.method == http.MethodPost && strings.HasSuffix(call.path, "/statefulsets") {
			if !primarySubmitted || auxiliaryWrites == 0 {
				t.Fatal("primary CREATE did not follow its own checkpoint and auxiliary apply")
			}
			primaryWrites++
			if reject {
				return sandboxSDKStatus(http.StatusForbidden, metav1.StatusReasonForbidden)
			}
		}
		return nil, nil
	}
	for attempt := range 3 {
		primarySubmitted = false
		values["aiGateway"].(map[string]any)["copilotProxy"].(map[string]any)["masterKeyId"] = fmt.Sprintf("%012x", attempt+1)
		guard := func(context.Context) error { return nil }
		primary := func(context.Context) error { primarySubmitted = true; return nil }
		create := func(context.Context) (func(context.Context) error, error) {
			if primarySubmitted {
				return nil, nil
			}
			primarySubmitted = true
			return func(context.Context) error { cancellations++; return nil }, nil
		}
		prepared, err := base.ReconcileSandboxWithPreflight(values, bundle, guard, guard, primary, nil, create)
		if err != nil {
			t.Fatal(err)
		}
		reject = attempt < 2
		err = (&Client{}).installOrUpgrade(prepared.sandboxReconcile.Namespace.Name, prepared.releaseName, prepared,
			func(string) (*action.Configuration, error) { return cfg, nil })
		if (err != nil) != reject || primaryWrites != attempt+1 || cancellations != min(attempt+1, 2) {
			t.Fatalf("auxiliary or failed history consumed primary proof: attempt=%d writes=%d cancellations=%d err=%v",
				attempt, primaryWrites, cancellations, err)
		}
	}
	if auxiliaryWrites < 3 {
		t.Fatal("repeated failed-history attempts did not update the existing auxiliary")
	}
}

func TestReconciledSandboxAuxiliaryAuthorityIsScoped(t *testing.T) {
	for _, scenario := range []string{"current", "primary", "foreign-name", "foreign-namespace", "changed-input", "stale-claim"} {
		t.Run(scenario, func(t *testing.T) {
			bundle, values := reconciledSandboxInputs(t)
			base, err := PrepareSandboxChart(testChart(t), values, nil, bundle)
			if err != nil {
				t.Fatal(err)
			}
			valid := true
			guard := func(context.Context) error {
				if !valid {
					return context.Canceled
				}
				return nil
			}
			prepared, err := base.ReconcileSandboxWithPreflight(values, bundle, guard, guard, guard, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			target := unstructured.Unstructured{}
			target.SetAPIVersion("apps/v1")
			target.SetNamespace(prepared.sandboxReconcile.Namespace.Name)
			for _, workload := range prepared.sandboxReconcile.Workloads {
				if workload.Kind == "Deployment" {
					target.SetKind(workload.Kind)
					target.SetName(workload.Identity.Name)
				}
			}
			if target.GetName() == "" {
				t.Fatal("fixture has no auxiliary")
			}
			switch scenario {
			case "primary":
				target.SetKind("StatefulSet")
			case "foreign-name":
				target.SetName("not-approved")
			case "foreign-namespace":
				target.SetNamespace("not-approved")
			case "changed-input":
				prepared.Values["replicaCount"] = 2
			case "stale-claim":
				valid = false
			}
			if err := prepared.AuthorizeSandboxAuxiliaryApply(context.Background(), target); (err == nil) != (scenario == "current") {
				t.Fatalf("auxiliary authority escaped its exact render/current claim: %v", err)
			}
		})
	}
}

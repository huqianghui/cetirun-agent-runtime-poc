// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxworkload_test

import (
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload/testfixture"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
)

func TestContainerWorkloadUsesOrdinaryResourcesAndCommonExecutor(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		t.Run(kind, func(t *testing.T) {
			input := testfixture.New(kind).Input
			input.Resolution.Sandbox.Type = api.SandboxContainer
			input.Resolution.Capabilities.Profile = sandbox.ProfileNativeSingleStart
			input.Resolution.Capabilities.PerContainerResourceLimits = true
			input.Resolution.Capabilities.EnforcementObservation = false
			input.Resolution.Capabilities.RestrictiveFailure = false
			input.Resolution.Capabilities.RuntimeClassName = "example-container"
			input.RuntimeClass.Name, input.RuntimeClass.Handler, input.RuntimeClass.Overhead = "example-container", "runc", nil
			if _, err := w.Build(input); err == nil || !strings.Contains(err.Error(), "common executor") {
				t.Fatal("container rendering accepted a held or single-start contract:", err)
			}
			input.Reconciled = true
			bundle, err := w.Build(input)
			if err != nil {
				t.Fatal(err)
			}
			if len(bundle.Overhead()) != 0 {
				t.Fatal("ordinary containers inherited VM overhead")
			}
			accounting := bundle.Accounting()
			for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
				request, limit := accounting.Pod.Requests[name], accounting.Pod.Limits[name]
				effectiveRequest, effectiveLimit := accounting.Effective.Requests[name], accounting.Effective.Limits[name]
				if request.Cmp(effectiveRequest) != 0 || limit.Cmp(effectiveLimit) != 0 {
					t.Fatal("ordinary effective resources include invented VM overhead")
				}
			}
			if !equality.Semantic.DeepEqual(bundle.Identity().Resolution.Sandbox.Resources, input.Resolution.Sandbox.Resources) {
				t.Fatal("ordinary runtime rewrote Agent/helper resource requests or limits")
			}
			for _, handler := range []string{"", "invalid/handler"} {
				input.RuntimeClass.Handler = handler
				if _, err := w.Build(input); err == nil {
					t.Fatal("container rendering accepted a missing or malformed runtime handler")
				}
			}
		})
	}
}

// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/controller"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestCustomInvalidPlatformSchedulingCannotReachHandoff(t *testing.T) {
	requireLegacyCustomRuntime(t)
	for _, defaults := range []map[string]any{
		{"runtimeClassName": "INVALID_CLASS"},
		{"nodeSelector": map[string]any{"example.com/node": "value/with/slash"}},
		{"nodeSelector": map[string]any{"bad key": ""}},
	} {
		cr, b, objects := customFixture("example", nil)
		scheme := testScheme()
		c := provisioningClient{Client: fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.Agent{}).WithObjects(objects...).Build()}
		p, h := &customProvider{bindings: b}, &fakeHelmClient{useHarness: true}
		r := &controller.AgentReconciler{Client: c, Scheme: scheme, HelmClient: h, CustomBindings: p,
			CetiRunHelmValues: defaults, EventRecorder: record.NewFakeRecorder(100)}
		controller.ConfigureTestGateway(t, r)
		_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: cr.Name}})
		if err == nil || p.applied || h.installCalled {
			t.Fatalf("invalid scheduling reached handoff/apply: err=%v approve=%v apply=%v", err, p.applied, h.installCalled)
		}
	}
}

func TestCustomIgnoresNativePlatformPersistence(t *testing.T) {
	requireLegacyCustomRuntime(t)
	defaults := map[string]any{"persistence": map[string]any{"storageClass": "native-home", "size": "8Gi"}}
	for _, instancePersistence := range []bool{false, true} {
		cr, b, objects := customFixture("example", nil)
		if instancePersistence {
			cr.Spec.Helm.Values["persistence"] = runtime.RawExtension{Raw: []byte(`{"size":"1Gi"}`)}
		}
		scheme := testScheme()
		c := provisioningClient{Client: fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.Agent{}).WithObjects(objects...).Build()}
		p, h := &customProvider{bindings: b}, &fakeHelmClient{useHarness: true}
		r := &controller.AgentReconciler{Client: c, Scheme: scheme, HelmClient: h, CustomBindings: p,
			CetiRunHelmValues: defaults, EventRecorder: record.NewFakeRecorder(100)}
		controller.ConfigureTestGateway(t, r)
		_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: cr.Name}})
		if instancePersistence {
			if err == nil || p.applied || h.installCalled {
				t.Fatalf("instance persistence reached handoff/apply: err=%v approve=%v apply=%v", err, p.applied, h.installCalled)
			}
			continue
		}
		if err != nil || !p.applied || !h.installCalled {
			t.Fatalf("native platform persistence blocked Custom: err=%v approve=%v apply=%v", err, p.applied, h.installCalled)
		}
		if strings.Contains(fmt.Sprint(h.lastValues), "native-home") {
			t.Fatal("native platform persistence reached the Custom release")
		}
	}
	if defaults["persistence"].(map[string]any)["storageClass"] != "native-home" {
		t.Fatal("Custom mutated the operator-wide values")
	}
}

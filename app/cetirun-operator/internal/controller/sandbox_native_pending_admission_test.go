// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	sandboxadmission "github.com/Azure/CetiRun/app/cetirun-operator/internal/admission"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload/testfixture"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func TestNativeCoordinationRetainsReservationForAdmittedUnstoredPod(t *testing.T) {
	ctx := context.Background()
	f, initial := nativeCoordinationFixture(t)
	before, after := nativePolicyChange(t, f, initial)
	if _, err := f.r.startKubernetesSandbox(ctx, f.current(t)); err != nil {
		t.Fatal(err)
	}
	store := f.r.SandboxNativeCoordination
	_, expected := f.expectation(t)
	pod := testfixture.Pod(*expected)
	handler, err := sandboxadmission.NewPodHandler(f.kube, sandboxadmission.PodConfig{
		StateNamespace: f.r.OperatorNamespace, CoordinationConfigMapName: store.Identity.Name,
		CoordinationConfigMapUID: string(store.Identity.UID),
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(pod)
	if err != nil {
		t.Fatal(err)
	}
	response := handler.Validate(ctx, admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UID: "delayed-first-create", Operation: admissionv1.Create, Namespace: pod.Namespace, Name: pod.Name,
		Resource: metav1.GroupVersionResource{Version: "v1", Resource: "pods"},
		Kind:     metav1.GroupVersionKind{Version: "v1", Kind: "Pod"}, Object: runtime.RawExtension{Raw: raw},
	}})
	if !response.Allowed {
		t.Fatalf("test did not reach successful admission: %+v", response.Result)
	}
	current := f.current(t)
	current.Spec.Suspended = true
	current.Generation++
	if err := f.kube.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	if err := f.r.stopSandboxBuiltin(ctx, f.current(t)); !errors.Is(err, sandboxcoordination.ErrUnavailable) {
		t.Fatal("zero replicas and an empty API list released an outstanding admitted CREATE:", err)
	}
	if _, err := store.AcquireConfigurationChange(ctx, before, after); !errors.Is(err, sandboxcoordination.ErrConflict) {
		t.Fatal("selected configuration changed before the admitted Pod outcome was observed:", err)
	}
	pod.UID = "delayed-initial-pod"
	if err := f.kube.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if err := f.r.stopSandboxBuiltin(ctx, f.current(t)); err == nil {
		t.Fatal("persisted but unterminated Pod released its reservation")
	}
	if err := f.kube.Delete(ctx, f.pod(t)); err != nil {
		t.Fatal(err)
	}
	if err := f.r.stopSandboxBuiltin(ctx, f.current(t)); err != nil {
		t.Fatal("ordinary termination did not release the observed Pod lifetime:", err)
	}
	if _, err := store.AcquireConfigurationChange(ctx, before, after); err != nil {
		t.Fatal("observed termination did not unblock configuration:", err)
	}
}

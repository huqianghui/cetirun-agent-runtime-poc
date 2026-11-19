// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	sandboxadmission "github.com/Azure/CetiRun/app/cetirun-operator/internal/admission"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload/testfixture"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func TestNativeCoordinationWiredStartupBlocksPendingMutation(t *testing.T) {
	ctx := context.Background()
	f, expected := nativeCoordinationFixture(t)
	before, after := nativePolicyChange(t, f, expected)
	change, err := f.r.SandboxNativeCoordination.AcquireConfigurationChange(ctx, before, after)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.SandboxNativeCoordination.BeginChange(ctx, change); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.startKubernetesSandbox(ctx, f.current(t)); !errors.Is(err, sandboxcoordination.ErrConflict) {
		t.Fatalf("actual native startup overlapped a pending selected mutation: %v", err)
	}
	receipt, _, err := f.r.readSandboxPreparationReceipt(ctx, f.current(t))
	if err != nil || receipt.Annotations[nativeStartAnnotation] != "" || ptr.Deref(f.workload(t).Spec.Replicas, -1) != 0 {
		t.Fatal("blocked startup consumed its one-shot marker or submitted a Pod:", err)
	}
}

func TestNativeCoordinationWiredStartAdmissionAndOrdinaryStop(t *testing.T) {
	ctx := context.Background()
	f, initial := nativeCoordinationFixture(t)
	before, after := nativePolicyChange(t, f, initial)
	if _, err := f.r.startKubernetesSandbox(ctx, f.current(t)); err != nil {
		t.Fatal(err)
	}
	store := f.r.SandboxNativeCoordination
	state, expected := f.expectation(t)
	if _, found, err := store.ReadNative(ctx, expected.Agent.UID); err != nil || !found {
		t.Fatal("actual native startup did not durably reserve sources:", err)
	}
	if _, err := store.AcquireConfigurationChange(ctx, before, after); !errors.Is(err, sandboxcoordination.ErrConflict) {
		t.Fatal("native startup did not block its configuration writer:", err)
	}
	handler, err := sandboxadmission.NewPodHandler(f.kube, sandboxadmission.PodConfig{
		StateNamespace: f.r.OperatorNamespace, CoordinationConfigMapName: store.Identity.Name,
		CoordinationConfigMapUID: string(store.Identity.UID),
	})
	if err != nil {
		t.Fatal(err)
	}
	pod := testfixture.Pod(*expected)
	raw, err := json.Marshal(pod)
	if err != nil {
		t.Fatal(err)
	}
	response := handler.Validate(ctx, admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UID: "synthetic-request", Operation: admissionv1.Create, Namespace: pod.Namespace, Name: pod.Name,
		Resource: metav1.GroupVersionResource{Version: "v1", Resource: "pods"},
		Kind:     metav1.GroupVersionKind{Version: "v1", Kind: "Pod"}, Object: runtime.RawExtension{Raw: raw},
	}})
	if !response.Allowed {
		t.Fatalf("reserved first native Pod was rejected: %+v", response.Result)
	}
	if !slices.Contains(expected.Template.Finalizers, w.NativePodFinalizer) || !slices.Contains(state.Finalizers, w.NativePodFinalizer) {
		t.Fatal("coordination removed initial Pod or retained expectation protection")
	}
	f.createPod(t, false)
	current := f.current(t)
	current.Spec.Suspended = true
	current.Generation++
	if err := f.kube.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	if err := f.r.stopSandboxBuiltin(ctx, f.current(t)); err == nil {
		t.Fatal("ordinary stop released before observing Pod termination")
	}
	if _, err := store.AcquireConfigurationChange(ctx, before, after); !errors.Is(err, sandboxcoordination.ErrConflict) {
		t.Fatal("terminating native work lost its source reservation:", err)
	}
	if err := f.kube.Delete(ctx, f.pod(t)); err != nil {
		t.Fatal(err)
	}
	if err := f.r.stopSandboxBuiltin(ctx, f.current(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcquireConfigurationChange(ctx, before, after); err != nil {
		t.Fatal("observed ordinary shutdown did not release selected sources:", err)
	}
}

func TestNativeCoordinationWiredObservationFailsClosed(t *testing.T) {
	ctx := context.Background()
	f, _ := nativeCoordinationFixture(t)
	if _, err := f.r.startKubernetesSandbox(ctx, f.current(t)); err != nil {
		t.Fatal(err)
	}
	state, expected := f.expectation(t)
	f.r.SandboxNativeCoordination.Identity.UID = "replaced-authority"
	if _, err := f.r.observeKubernetesSandbox(ctx, f.current(t), state, expected); !errors.Is(err, sandboxcoordination.ErrUnavailable) {
		t.Fatal("running native observation ignored a lost reservation:", err)
	}
}

// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxadmission

import (
	"context"
	"testing"

	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestNativeFirstPodCannotDisappearBeforeLifetimeIsRecorded(t *testing.T) {
	f := nativePodFixture(t)
	ctx := context.Background()
	pod := f.pod.DeepCopy()
	if response := f.handler.Validate(ctx, podRequest(t, pod, admissionv1.Create, nil)); !response.Allowed {
		t.Fatalf("first Pod rejected: %+v", response.Result)
	}
	pod.UID = "first-pod-uid"
	if err := f.client.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Delete(ctx, pod); err != nil {
		t.Fatal(err)
	}
	current := &corev1.Pod{}
	if err := f.client.Get(ctx, client.ObjectKeyFromObject(pod), current); apierrors.IsNotFound(err) {
		t.Fatal("first Pod disappeared before its creation allowance was consumed")
	} else if err != nil {
		t.Fatal(err)
	}
	if current.DeletionTimestamp.IsZero() {
		t.Fatal("test did not request early Pod deletion")
	}
	current.APIVersion, current.Kind = "v1", "Pod"
	withoutFinalizers := current.DeepCopy()
	withoutFinalizers.Finalizers = nil
	response := f.handler.Validate(ctx, podRequest(t, withoutFinalizers, admissionv1.Update, current))
	if response.Allowed {
		t.Fatal("unrecorded Pod lifetime protection could be removed")
	}
}

func TestNativePodRecordedFinalizationIsNarrowAndReadOnly(t *testing.T) {
	f := nativePodFixture(t)
	ctx := context.Background()
	pod := f.pod.DeepCopy()
	pod.UID = "first-pod-uid"
	if err := f.client.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	f.expected.PodUID, f.expected.ExecutionHeld = pod.UID, true
	saveNativePodExpectation(t, &f)
	pod.APIVersion, pod.Kind = "v1", "Pod"
	for _, change := range []bool{false, true} {
		next := pod.DeepCopy()
		next.Finalizers = nil
		next.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "controller", Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: "v1"}}
		if change {
			next.Spec.Containers[0].Image = "example.com/changed:1"
		}
		response := f.handler.Validate(ctx, podRequest(t, next, admissionv1.Update, pod))
		if response.Allowed == change {
			t.Fatalf("held finalizer removal allowed=%t with workload change=%t: %+v", response.Allowed, change, response.Result)
		}
	}
	current := &corev1.Pod{}
	if err := f.client.Get(ctx, client.ObjectKeyFromObject(pod), current); err != nil {
		t.Fatal(err)
	}
	if len(current.Finalizers) != 1 || current.Finalizers[0] != w.NativePodFinalizer {
		t.Fatal("validation mutated the Pod")
	}
}

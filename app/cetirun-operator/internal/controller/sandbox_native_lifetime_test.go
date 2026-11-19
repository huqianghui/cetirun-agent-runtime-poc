// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"slices"
	"testing"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestNativeSandboxForegroundDeletionRetainsLifetimeState(t *testing.T) {
	f := newNativeSandboxFixture(t, true)
	f.prepare(t)
	f.reconcileOK(t)
	pod := f.createPod(t, false)
	ctx := context.Background()
	state, _ := f.expectation(t)
	if err := f.kube.Delete(ctx, f.current(t)); err != nil {
		t.Fatal(err)
	}
	if err := f.kube.Delete(ctx, state); err != nil {
		t.Fatal(err)
	}
	if err := f.kube.Get(ctx, client.ObjectKeyFromObject(state), state); err != nil {
		t.Fatal("foreground GC removed the initial Pod record before finalization:", err)
	}
	if err := f.kube.Delete(ctx, pod); err != nil {
		t.Fatal(err)
	}
	f.finishNamespaceGC(t)
	f.reconcileOK(t)
	if err := f.kube.Get(ctx, client.ObjectKeyFromObject(pod), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatal("foreground deletion left the initial Pod protected:", err)
	}
	if err := f.kube.Get(ctx, client.ObjectKeyFromObject(state), &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Fatal("completed deletion retained native lifetime state:", err)
	}
}

func TestNativeSandboxDeletionBeforeFirstObservationConsumesLifetime(t *testing.T) {
	f := newNativeSandboxFixture(t, true)
	f.prepare(t)
	f.reconcileOK(t)
	pod := f.createPod(t, false)
	if !slices.Contains(pod.Finalizers, w.NativePodFinalizer) {
		t.Fatal("native chart omitted initial Pod lifetime protection")
	}
	ctx := context.Background()
	if err := f.kube.Delete(ctx, pod); err != nil {
		t.Fatal(err)
	}
	retained := &corev1.Pod{}
	if err := f.kube.Get(ctx, client.ObjectKeyFromObject(pod), retained); err != nil || retained.DeletionTimestamp.IsZero() {
		t.Fatal("early deletion did not retain the initial Pod:", err)
	}
	if _, err := f.reconcile(); err == nil {
		t.Fatal("early Pod deletion was treated as a successful start")
	}
	_, expected := f.expectation(t)
	if expected.PodUID != pod.UID || !slices.Contains(f.current(t).Finalizers, sandbox.HoldFinalizer) {
		t.Fatal("initial lifetime was not durably consumed before hold")
	}
	if err := f.kube.Get(ctx, client.ObjectKeyFromObject(pod), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatal("recorded initial Pod was not permitted to finish ordinary deletion:", err)
	}
	f.assertNotReady(t)
}

func TestNativeSandboxDeletingAgentConsumesUnobservedPod(t *testing.T) {
	f := newNativeSandboxFixture(t, true)
	f.prepare(t)
	f.reconcileOK(t)
	pod := f.createPod(t, false)
	ctx := context.Background()
	if err := f.kube.Delete(ctx, f.current(t)); err != nil {
		t.Fatal(err)
	}
	if err := f.kube.Delete(ctx, pod); err != nil {
		t.Fatal(err)
	}
	f.reconcileOK(t)
	if err := f.kube.Get(ctx, client.ObjectKeyFromObject(pod), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatal("deleting Agent could not finish initial Pod finalization:", err)
	}
}

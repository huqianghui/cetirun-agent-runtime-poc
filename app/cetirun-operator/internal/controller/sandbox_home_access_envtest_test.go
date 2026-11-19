// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package controller

import (
	"context"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func newReconciledStartAPIHarness(t *testing.T, homeMode corev1.PersistentVolumeAccessMode, network ...api.AgentPolicyNetwork) *nativeStartAPIHarness {
	t.Helper()
	return newReconciledStartAPIHarnessWithRegistration(t, homeMode, nil, network...)
}

func newReconciledStartAPIHarnessWithRegistration(t *testing.T, homeMode corev1.PersistentVolumeAccessMode,
	configure func(*sandbox.Capabilities), network ...api.AgentPolicyNetwork,
) *nativeStartAPIHarness {
	t.Helper()
	h := newNativeStartAPIHarnessWithRegistration(t, true, true, homeMode, configure, network...)
	return startReconciledAPIHarness(t, h)
}

func startReconciledAPIHarness(t *testing.T, h *nativeStartAPIHarness) *nativeStartAPIHarness {
	t.Helper()
	// Envtest does not run the ServiceAccount controller.
	h.kube = interceptor.NewClient(h.kube, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
			if err := c.Create(ctx, object, opts...); err != nil {
				return err
			}
			if namespace, ok := object.(*corev1.Namespace); ok {
				if err := h.admin.Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
					Namespace: namespace.Name, Name: "default",
				}}); err != nil {
					return err
				}
				if h.agent.Spec.Agent.Type == api.AgentTypeCustom {
					return h.admin.Create(ctx, &corev1.ConfigMap{
						ObjectMeta: metav1.ObjectMeta{Namespace: namespace.Name, Name: "kube-root-ca.crt"},
						Data:       map[string]string{"ca.crt": "synthetic-ca"},
					})
				}
			}
			return nil
		},
	})
	h.r.Client = h.kube
	installer := &nativeSandboxInstaller{kube: h.kube, chart: h.localHelm.chart}
	h.installer = installer
	h.r.HelmClient = &sharedSandboxInstaller{nativeSandboxInstaller: installer}
	for attempt := range 4 {
		h.reconcileOK(t)
		namespace := &corev1.Namespace{}
		err := h.admin.Get(h.ctx, client.ObjectKey{Name: instanceNamespace(h.agent.Name)}, namespace)
		if err == nil {
			break
		}
		if !apierrors.IsNotFound(err) || attempt == 3 {
			t.Fatal("common reconciliation did not create its owned namespace:", err, h.current(t).Status)
		}
	}
	for range 3 {
		h.reconcileOK(t)
	}
	return h
}

func TestReconciledSandboxRWOHomeAPIServer(t *testing.T) {
	h := newReconciledStartAPIHarness(t, corev1.ReadWriteOnce)
	assertNativeHomeAccessMode(t, h.nativeSandboxFixture, corev1.ReadWriteOnce)
	expected := reconciledExpectationForTest(t, h.nativeSandboxFixture)
	observed := *h.nativeSandboxFixture
	observed.kube = h.admin
	pod := reconciledPodForTest(t, &observed, expected.Workloads[0])
	pod.Spec.Overhead = nil
	changed := pod.DeepCopy()
	changed.Spec.Containers[0].Image = "registry.example.com/unapproved:example"
	if err := h.admin.Create(h.ctx, changed, client.DryRunAll); !apierrors.IsForbidden(err) {
		t.Fatal("served RWO Pod admission did not reject a changed image:", err)
	}
	h.create(t, pod)
	if pod.UID == "" {
		t.Fatal("served RWO Pod admission did not persist the current owned Pod")
	}
	home := &corev1.PersistentVolumeClaim{}
	if err := h.admin.Get(h.ctx, client.ObjectKey{Namespace: expected.Namespace.Name, Name: expected.Home.Name}, home); err != nil {
		t.Fatal(err)
	}
	changedHome := home.DeepCopy()
	changedHome.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod}
	if err := h.admin.Update(h.ctx, changedHome); !apierrors.IsInvalid(err) {
		t.Fatal("the served API allowed the retained HOME mode to change in place:", err)
	}
	assertNativeHomeAccessMode(t, h.nativeSandboxFixture, corev1.ReadWriteOnce)
}

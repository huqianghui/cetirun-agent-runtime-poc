// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package controller

import (
	"slices"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/ingressmtls"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestReconciledSandboxIngressMTLSAPIServer(t *testing.T) {
	testReconciledSandboxIngressMTLSAPIServer(t, api.SandboxKata)
}

func testReconciledSandboxIngressMTLSAPIServer(t *testing.T, runtimeType api.SandboxType) {
	ctrl.SetLogger(logr.Discard())
	for _, kind := range []string{"openclaw", "hermes", "custom"} {
		t.Run(kind, func(t *testing.T) {
			h := newNativeStartAPIHarnessWithRuntime(t, true, true, corev1.ReadWriteOnce, kind, runtimeType, nil)
			installer := &nativeSandboxInstaller{kube: h.kube, chart: h.localHelm.chart}
			h.installer = installer
			h.r.HelmClient = &sharedSandboxInstaller{nativeSandboxInstaller: installer}
			if _, err := h.r.reconcilePolicy(h.ctx, h.current(t)); err != nil {
				t.Fatal(err)
			}
			if err := h.r.bootstrapReconciledSandbox(h.ctx, h.current(t)); err != nil {
				t.Fatal(err)
			}
			h.create(t, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: instanceNamespace(h.agent.Name)}})
			if err := h.admin.Create(h.ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "platform"}}); client.IgnoreAlreadyExists(err) != nil {
				t.Fatal(err)
			}
			if kind == "custom" {
				h.create(t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "portal-system"}})
				h.create(t, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "kube-root-ca.crt", Namespace: instanceNamespace(h.agent.Name)},
					Data: map[string]string{"ca.crt": "synthetic-ca"}})
				withCustomMTLS(t, true)(h.r, h.admin)
			} else {
				withIngressMTLS(t, kind, true)(h.r, h.admin)
			}
			policy := &api.AgentPolicy{}
			team := &api.Team{}
			if err := h.admin.Get(h.ctx, client.ObjectKey{Name: h.agent.Spec.Team}, team); err != nil {
				t.Fatal(err)
			}
			if err := h.admin.Get(h.ctx, client.ObjectKey{Name: team.Spec.AgentPolicyRef}, policy); err != nil {
				t.Fatal(err)
			}
			reconciler := &AgentPolicyReconciler{Client: h.kube, APIReader: h.kube, Source: h.r.SandboxPreparation.Configuration, NativeExecution: true}
			if _, err := reconciler.Reconcile(h.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)}); err != nil {
				t.Fatal(err)
			}
			for range 4 {
				h.reconcileOK(t)
			}
			if runtimeType == api.SandboxContainer {
				assertContainerWorkload(t, h.nativeSandboxFixture, corev1.ReadWriteOnce)
			}
			expected := reconciledExpectationForTest(t, h.nativeSandboxFixture)
			observed := *h.nativeSandboxFixture
			observed.kube = h.admin
			pod := reconciledPodForTest(t, &observed, expected.Workloads[0])
			pod.Spec.Overhead = nil
			if err := h.admin.Create(h.ctx, pod, client.DryRunAll); err != nil {
				t.Fatal("served Pod admission rejected approved mTLS sources and helper:", err)
			}
			changed := pod.DeepCopy()
			changed.Spec.Containers = slices.DeleteFunc(changed.Spec.Containers, func(c corev1.Container) bool { return c.Name == ingressmtls.ContainerName })
			if err := h.admin.Create(h.ctx, changed, client.DryRunAll); !apierrors.IsForbidden(err) {
				t.Fatal("served Pod admission accepted a missing TLS helper:", err)
			}
			secret := &corev1.Secret{}
			if err := h.admin.Get(h.ctx, client.ObjectKey{Namespace: "platform", Name: "gateway-agent-example"}, secret); err != nil {
				t.Fatal(err)
			}
			secret.Data = map[string][]byte{"synthetic": []byte("changed")}
			if err := h.admin.Update(h.ctx, secret); err != nil {
				t.Fatal(err)
			}
			if err := h.admin.Create(h.ctx, pod, client.DryRunAll); !apierrors.IsForbidden(err) {
				t.Fatal("served Pod admission accepted a stale Gateway certificate approval:", err)
			}
			if _, err := h.reconcile(); err == nil {
				t.Fatal("certificate drift did not block common execution")
			}
			h.assertNotReady(t)
			h.assertNoRoute(t)
			if ptr.Deref(h.workload(t).Spec.Replicas, 1) != 0 {
				t.Fatal("certificate drift did not request shutdown")
			}
			if err := h.r.revokeSandboxPreparationGrants(h.ctx, h.current(t)); err != nil {
				t.Fatal("TLS source read grant cleanup failed:", err)
			}
			home := &corev1.PersistentVolumeClaim{}
			if err := h.admin.Get(h.ctx, client.ObjectKey{Namespace: expected.Namespace.Name, Name: expected.Home.Name}, home); err != nil || home.UID != expected.Home.UID {
				t.Fatal("TLS cleanup replaced or removed HOME:", err)
			}
		})
	}
}

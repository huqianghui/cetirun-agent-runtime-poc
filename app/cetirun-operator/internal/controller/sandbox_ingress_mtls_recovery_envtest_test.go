// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package controller

import (
	"context"
	"errors"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestReconciledSandboxIngressMTLSPreflightRecoveryAPIServer(t *testing.T) {
	testMTLSApplyRecoveryAPIServer(t, false)
}

func TestReconciledSandboxIngressMTLSFinalGuardRecoveryAPIServer(t *testing.T) {
	testMTLSApplyRecoveryAPIServer(t, true)
}

func testMTLSApplyRecoveryAPIServer(t *testing.T, finalGuard bool) {
	ctrl.SetLogger(logr.Discard())
	h := newNativeStartAPIHarnessConfigured(t, true, true, corev1.ReadWriteOnce, "openclaw", nil)
	installer := &nativeSandboxInstaller{kube: h.kube, chart: h.localHelm.chart}
	h.installer = installer
	shared := &sharedSandboxInstaller{nativeSandboxInstaller: installer}
	h.r.HelmClient = shared
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
	withIngressMTLS(t, "openclaw")(h.r, h.admin)
	team := &api.Team{}
	if err := h.admin.Get(h.ctx, client.ObjectKey{Name: h.agent.Spec.Team}, team); err != nil {
		t.Fatal(err)
	}
	policies := &AgentPolicyReconciler{Client: h.kube, APIReader: h.kube, Source: h.r.SandboxPreparation.Configuration, NativeExecution: true}
	if _, err := policies.Reconcile(h.ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: team.Spec.AgentPolicyRef}}); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("synthetic initial Helm history outage")
	var initial *helm.PreparedRelease
	originalClient, originalReader := h.r.Client, h.r.APIReader
	if finalGuard {
		injected = errors.New("synthetic final mTLS authority outage")
		outage := &mtlsPrerequisiteReadOutage{Reader: originalReader,
			key: client.ObjectKey{Namespace: instanceNamespace(h.agent.Name), Name: "agent-ingress-tls"}, failure: injected}
		h.r.APIReader = outage
		h.r.Client = interceptor.NewClient(h.kube, interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
				if err := c.Patch(ctx, object, patch, options...); err != nil {
					return err
				}
				if state, ok := object.(*corev1.ConfigMap); ok && state.Data[w.PodExpectationKey] != "" &&
					state.Annotations[sandboxApplyAttemptAnnotation] != "" {
					outage.active = true
				}
				return nil
			},
		})
	}
	shared.beforeApply = func(prepared *helm.PreparedRelease) error {
		initial = prepared
		if finalGuard {
			return nil
		}
		if err := prepared.CheckSandboxApply(h.ctx); err != nil {
			return err
		}
		return injected
	}
	var failed error
	for range 5 {
		_, failed = h.reconcile()
		if failed != nil {
			break
		}
	}
	if !errors.Is(failed, injected) || initial == nil || installer.stages != 0 {
		t.Fatal("API fixture did not stop at read-only history preflight:", failed)
	}
	h.r.Client, h.r.APIReader = originalClient, originalReader
	expected := reconciledExpectationForTest(t, h.nativeSandboxFixture)
	if err := h.admin.Get(h.ctx, client.ObjectKey{Namespace: expected.Namespace.Name, Name: instanceServiceName(h.agent.Name)}, &appsv1.StatefulSet{}); !apierrors.IsNotFound(err) {
		t.Fatal("read-only history failure created a StatefulSet:", err)
	}
	secretKey := client.ObjectKey{Namespace: expected.Namespace.Name, Name: "agent-ingress-tls"}
	before := &corev1.Secret{}
	if err := h.admin.Get(h.ctx, secretKey, before); err != nil {
		t.Fatal(err)
	}
	reader := &mtlsPrerequisiteReadOutage{Reader: h.r.APIReader, key: secretKey,
		failure: errors.New("synthetic TLS prerequisite outage"), active: true}
	h.r.APIReader = reader
	shared.beforeApply = nil
	if _, err := h.reconcile(); err == nil {
		t.Fatal("TLS prerequisite outage was ignored")
	}
	state := &corev1.ConfigMap{}
	if err := h.admin.Get(h.ctx, client.ObjectKey{Namespace: h.r.OperatorNamespace,
		Name: w.PodExpectationName(expected.Namespace.Name, expected.Namespace.UID)}, state); err != nil {
		t.Fatal(err)
	}
	if state.Annotations[sandboxApplyStateAnnotation] != sandboxApplyCancelled {
		t.Fatal("served pre-submit checkpoint was not safely cancelled")
	}
	if _, found, err := h.r.SandboxNativeCoordination.ReadNative(h.ctx, h.agent.UID); err != nil || found {
		t.Fatal("read-only failure stranded a served source reservation:", err)
	}
	reader.active = false
	after := &corev1.Secret{}
	if err := h.admin.Get(h.ctx, secretKey, after); err != nil || before.UID != after.UID || before.ResourceVersion != after.ResourceVersion {
		t.Fatal("transient fixture changed the live TLS Secret identity:", err)
	}
	for range 4 {
		h.reconcileOK(t)
	}
	current := reconciledExpectationForTest(t, h.nativeSandboxFixture)
	if current.Home != expected.Home || current.Namespace != expected.Namespace || current.Agent != expected.Agent || installer.stages != 1 {
		t.Fatal("served recovery lost the retained lifetime or replayed an apply")
	}
	if err := initial.AuthorizeSandboxApply(context.Background()); err == nil {
		t.Fatal("cancelled prepared release reused recovered API authority")
	}
	observed := *h.nativeSandboxFixture
	observed.kube = h.admin
	pod := reconciledPodForTest(t, &observed, current.Workloads[0])
	pod.Spec.Overhead = nil
	if err := h.admin.Create(h.ctx, pod, client.DryRunAll); err != nil {
		t.Fatal("served Pod admission rejected the recovered mTLS workload:", err)
	}
}

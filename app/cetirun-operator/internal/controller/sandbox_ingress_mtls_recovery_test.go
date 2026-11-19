// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type mtlsPrerequisiteReadOutage struct {
	client.Reader
	key     client.ObjectKey
	failure error
	active  bool
}

func (r *mtlsPrerequisiteReadOutage) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if r.active && key == r.key {
		return r.failure
	}
	return r.Reader.Get(ctx, key, object, options...)
}

func TestReconciledSandboxIngressMTLSInitialApplyRecovery(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, scenario := range []struct {
			name, failure, source, finish string
		}{
			{"history-outage-retry", "history", "outage", "retry"},
			{"configuration-outage-retry", "configuration", "outage", "retry"},
			{"history-recreated-retry", "history", "deleted", "retry"},
			{"configuration-outage-delete", "configuration", "outage", "delete"},
			{"history-deleted-delete", "history", "deleted", "delete"},
			{"configuration-outage-suspend-delete", "configuration", "outage", "suspend-delete"},
			{"uncertain-submitted", "submitted", "outage", "uncertain"},
		} {
			t.Run(kind+"/"+scenario.name, func(t *testing.T) {
				ctx := context.Background()
				f, _ := newUnreconciledSandboxRuntimeFixture(t, true, nil, true, kind, true, false,
					withNativeHomeAccessMode(t, corev1.ReadWriteOnce),
					withReconciledCoordination(t), withIngressMTLS(t, kind))
				installer := f.r.HelmClient.(*sharedSandboxInstaller)
				injected := errors.New("synthetic Helm " + scenario.failure + " failure")
				var failedRelease *helm.PreparedRelease
				installer.beforeApply = func(prepared *helm.PreparedRelease) error {
					failedRelease = prepared
					if scenario.failure == "submitted" {
						if err := prepared.AuthorizeSandboxApply(ctx); err != nil {
							return err
						}
					} else if err := prepared.CheckSandboxApply(ctx); err != nil {
						return err
					}
					return injected
				}
				var failed error
				for range 5 {
					_, failed = f.reconcile()
					if failed != nil {
						break
					}
				}
				if !errors.Is(failed, injected) || failedRelease == nil || f.installer.stages != 0 {
					t.Fatal("fixture did not fail before first workload application:", failed)
				}
				expected := reconciledExpectationForTest(t, f)
				workloadKey := client.ObjectKey{Namespace: expected.Namespace.Name, Name: instanceServiceName(f.agent.Name)}
				if err := f.kube.Get(ctx, workloadKey, &appsv1.StatefulSet{}); !apierrors.IsNotFound(err) {
					t.Fatal("failed first apply created a StatefulSet:", err)
				}
				stateKey := client.ObjectKey{Namespace: f.r.OperatorNamespace,
					Name: w.PodExpectationName(expected.Namespace.Name, expected.Namespace.UID)}
				state := &corev1.ConfigMap{}
				if err := f.kube.Get(ctx, stateKey, state); err != nil {
					t.Fatal(err)
				}
				wantState := sandboxApplyPending
				if scenario.failure == "submitted" {
					wantState = sandboxApplySubmitted
				}
				if state.Annotations[sandboxApplyStateAnnotation] != wantState {
					t.Fatalf("initial failure checkpoint = %q, want %q", state.Annotations[sandboxApplyStateAnnotation], wantState)
				}
				secretKey := client.ObjectKey{Namespace: expected.Namespace.Name, Name: "agent-ingress-tls"}
				original := &corev1.Secret{}
				if err := f.kube.Get(ctx, secretKey, original); err != nil {
					t.Fatal(err)
				}
				reader := &mtlsPrerequisiteReadOutage{
					Reader: f.r.APIReader, key: secretKey, failure: errors.New("synthetic TLS Secret read outage"),
				}
				f.r.APIReader = reader
				if scenario.source == "deleted" {
					if err := f.kube.Delete(ctx, original); err != nil {
						t.Fatal(err)
					}
				} else {
					reader.active = true
				}
				installer.beforeApply = nil
				if _, err := f.reconcile(); err == nil {
					t.Fatal("lost TLS prerequisite was ignored")
				}
				f.assertNotReady(t)
				ticket, found, err := f.r.SandboxNativeCoordination.ReadNative(ctx, f.agent.UID)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.kube.Get(ctx, stateKey, state); err != nil {
					t.Fatal(err)
				}
				if scenario.finish == "uncertain" {
					if !found || ticket.Reservation.Phase != sandboxcoordination.Stopping ||
						state.Annotations[sandboxApplyStateAnnotation] != sandboxApplySubmitted {
						t.Fatal("unknown submission was cancelled using workload absence")
					}
					reader.active = false
					for range 2 {
						if _, err := f.reconcile(); err == nil {
							t.Fatal("restored TLS source resumed an uncertain submission")
						}
					}
					agent := f.current(t)
					agent.Spec.Suspended = true
					agent.Generation++
					if err := f.kube.Update(ctx, agent); err != nil {
						t.Fatal(err)
					}
					if _, err := f.reconcile(); err == nil {
						t.Fatal("suspension ignored uncertain workload submission")
					}
					if err := f.kube.Delete(ctx, f.current(t)); err != nil {
						t.Fatal(err)
					}
					if _, err := f.reconcile(); err == nil {
						t.Fatal("normal deletion discarded uncertain submission")
					}
					if err := f.kube.Get(ctx, stateKey, state); err != nil ||
						state.Annotations[sandboxApplyStateAnnotation] != sandboxApplySubmitted || f.installer.stages != 0 {
						t.Fatal("uncertain submission checkpoint or no-replay boundary changed:", err)
					}
					return
				}
				if found || state.Annotations[sandboxApplyStateAnnotation] != sandboxApplyCancelled {
					t.Fatal("read-only failure and TLS loss stranded a stopping reservation")
				}
				if err := failedRelease.AuthorizeSandboxApply(ctx); err == nil {
					t.Fatal("cancelled prepared release remained authorized")
				}
				if scenario.finish != "retry" {
					if scenario.finish == "suspend-delete" {
						agent := f.current(t)
						agent.Spec.Suspended = true
						agent.Generation++
						if err := f.kube.Update(ctx, agent); err != nil {
							t.Fatal(err)
						}
						_, _ = f.reconcile()
						if _, found, err := f.r.SandboxNativeCoordination.ReadNative(ctx, f.agent.UID); err != nil || found {
							t.Fatal("suspension recreated a reservation while TLS was unavailable:", err)
						}
					}
					if err := f.kube.Delete(ctx, f.current(t)); err != nil {
						t.Fatal(err)
					}
					for range 3 {
						f.reconcileOK(t)
					}
					if err := f.kube.Get(ctx, client.ObjectKey{Name: f.agent.Name}, &api.Agent{}); !apierrors.IsNotFound(err) {
						t.Fatal("read-only failure blocked normal deletion:", err)
					}
					if err := f.kube.Get(ctx, client.ObjectKey{Namespace: f.r.OperatorNamespace,
						Name: sandboxpolicy.GrantJournalName(f.agent.UID)}, &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
						t.Fatal("normal deletion retained source-read grants:", err)
					}
					return
				}
				reader.active = false
				if scenario.source == "deleted" {
					replacement := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: secretKey.Namespace, Name: secretKey.Name}}
					if err := f.kube.Create(ctx, replacement); err != nil {
						t.Fatal(err)
					}
					if replacement.UID == original.UID {
						t.Fatal("replacement did not change Secret identity")
					}
					if _, err := f.reconcile(); err == nil || f.installer.stages != 0 {
						t.Fatal("recreated TLS Secret was adopted without new UID/version approval")
					}
					approveMTLSReplacementForRecovery(t, f, replacement)
				} else {
					current := &corev1.Secret{}
					if err := f.kube.Get(ctx, secretKey, current); err != nil ||
						current.UID != original.UID || current.ResourceVersion != original.ResourceVersion {
						t.Fatal("transient outage changed the approved Secret identity:", err)
					}
				}
				for range 4 {
					f.reconcileOK(t)
				}
				current := reconciledExpectationForTest(t, f)
				if current.Agent != expected.Agent || current.Home != expected.Home ||
					current.Namespace != expected.Namespace || f.installer.stages != 1 {
					t.Fatal("recovery replaced the lifetime/HOME or repeated application")
				}
				if _, found, err := f.r.SandboxNativeCoordination.ReadNative(ctx, f.agent.UID); err != nil || !found {
					t.Fatal("recovery did not reacquire current source authority:", err)
				}
				if err := failedRelease.AuthorizeSandboxApply(ctx); err == nil {
					t.Fatal("old prepared release reused recovered authority")
				}
			})
		}
	}
}

func approveMTLSReplacementForRecovery(t *testing.T, f *nativeSandboxFixture, replacement *corev1.Secret) {
	t.Helper()
	ctx := context.Background()
	configuration := &corev1.ConfigMap{}
	if err := f.kube.Get(ctx, client.ObjectKey{Namespace: f.r.OperatorNamespace, Name: "sandbox-platform"}, configuration); err != nil {
		t.Fatal(err)
	}
	var document SandboxPlatformConfiguration
	if err := json.Unmarshal([]byte(configuration.Data[SandboxPlatformConfigurationKey]), &document); err != nil {
		t.Fatal(err)
	}
	ref := &document.Platforms[api.SandboxKata].IngressMTLS.Bindings[0].ServerSecret
	ref.UID, ref.ResourceVersion = replacement.UID, replacement.ResourceVersion
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	configuration.Data[SandboxPlatformConfigurationKey] = string(raw)
	if err := f.kube.Update(ctx, configuration); err != nil {
		t.Fatal(err)
	}
}

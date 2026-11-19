// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload/testfixture"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestReconciledSandboxKubernetesInitialApplyRecovery(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, scenario := range []string{"history-retry", "history-delete", "submitted"} {
			t.Run(kind+"/"+scenario, func(t *testing.T) {
				ctx := context.Background()
				fixture := testfixture.New(kind)
				f, _ := newUnreconciledSandboxRuntimeFixture(t, true, nil, true, kind, true, false,
					withReconciledCoordination(t),
					func(r *AgentReconciler, author client.Client) {
						configureReconciledKubernetesAccess(t, r, author, fixture.Agent.Name, fixture.Policy.Name)
						installKubernetesFixtureCA(t, r, author, fixture.Agent.Name)
					})
				installer := f.r.HelmClient.(*sharedSandboxInstaller)
				injected := errors.New("synthetic Helm history read failure")
				installer.beforeApply = func(prepared *helm.PreparedRelease) error {
					if scenario == "submitted" {
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
				if !errors.Is(failed, injected) || f.installer.stages != 0 {
					t.Fatal("fixture did not fail before the first workload:", failed)
				}
				expected := reconciledExpectationForTest(t, f)
				workloadKey := client.ObjectKey{Namespace: expected.Namespace.Name, Name: instanceServiceName(f.agent.Name)}
				if err := f.kube.Get(ctx, workloadKey, &appsv1.StatefulSet{}); !apierrors.IsNotFound(err) {
					t.Fatal("failed initial history read created a workload:", err)
				}
				stateKey := client.ObjectKey{Namespace: f.r.OperatorNamespace,
					Name: w.PodExpectationName(expected.Namespace.Name, expected.Namespace.UID)}
				state := &corev1.ConfigMap{}
				if err := f.kube.Get(ctx, stateKey, state); err != nil {
					t.Fatal(err)
				}
				ticket, found, err := f.r.SandboxNativeCoordination.ReadNative(ctx, f.agent.UID)
				if err != nil {
					t.Fatal(err)
				}
				installer.beforeApply = nil
				if scenario == "submitted" {
					if state.Annotations[sandboxApplyStateAnnotation] != sandboxApplySubmitted ||
						!found || ticket.Reservation.Phase != sandboxcoordination.Stopping {
						t.Fatal("uncertain submission was treated as safe cancellation")
					}
					for range 2 {
						if _, err := f.reconcile(); err == nil {
							t.Fatal("unknown submission resumed without shutdown evidence")
						}
					}
					if err := f.kube.Delete(ctx, f.current(t)); err != nil {
						t.Fatal(err)
					}
					if _, err := f.reconcile(); err == nil {
						t.Fatal("unknown submission authorized deletion without shutdown evidence")
					}
					if err := f.kube.Get(ctx, stateKey, state); err != nil ||
						state.Annotations[sandboxApplyStateAnnotation] != sandboxApplySubmitted {
						t.Fatal("uncertain submission checkpoint was downgraded:", err)
					}
					if f.installer.stages != 0 {
						t.Fatal("unknown submission started another workload")
					}
					return
				}
				if state.Annotations[sandboxApplyStateAnnotation] != sandboxApplyCancelled || found {
					t.Fatal("read-only failure stranded a stopping reservation")
				}
				recorded, err := f.r.sandboxGrantJournal().Read(ctx, f.agent.Name, f.agent.UID)
				if err != nil || len(recorded) != 0 {
					t.Fatal("failed initial apply retained grants:", err)
				}
				if scenario == "history-delete" {
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
						t.Fatal("normal deletion retained its grant journal:", err)
					}
					return
				}
				for range 4 {
					f.reconcileOK(t)
				}
				assertKubernetesFixtureGrants(t, f, true)
				current := reconciledExpectationForTest(t, f)
				if current.Home != expected.Home || current.Agent != expected.Agent || current.Namespace != expected.Namespace ||
					f.installer.stages != 1 {
					t.Fatal("history recovery replaced the lifetime or repeated workload applies")
				}
				if _, found, err := f.r.SandboxNativeCoordination.ReadNative(ctx, f.agent.UID); err != nil || !found {
					t.Fatal("recovered execution did not reacquire its reservation:", err)
				}
			})
		}
	}
}

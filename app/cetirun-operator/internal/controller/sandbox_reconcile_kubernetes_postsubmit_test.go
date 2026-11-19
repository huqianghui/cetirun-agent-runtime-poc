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
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestReconciledSandboxKubernetesPostSubmitRecovery(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, scenario := range []string{"retry", "delete", "previous-submission", "competing-submission", "cancellation-write-failure", "cancellation-ack-loss"} {
			t.Run(kind+"/"+scenario, func(t *testing.T) {
				ctx := context.Background()
				fixture := testfixture.New(kind)
				f, _ := newUnreconciledSandboxRuntimeFixture(t, true, nil, true, kind, true, false,
					withReconciledCoordination(t),
					func(r *AgentReconciler, author client.Client) {
						configureReconciledKubernetesAccess(t, r, author, fixture.Agent.Name, fixture.Policy.Name)
						installKubernetesFixtureCA(t, r, author, fixture.Agent.Name)
					})
				injected := errors.New("synthetic final apply authority read failure")
				cancellationFailure := errors.New("synthetic cancellation write failure")
				var submittedKey client.ObjectKey
				armed, rejected, cancellationAttempted, rejectNextRead := false, false, false, false
				originalClient, originalReader := f.r.Client, f.r.APIReader
				f.r.Client = interceptor.NewClient(originalClient.(client.WithWatch), interceptor.Funcs{
					Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
						state, isState := object.(*corev1.ConfigMap)
						cancelling := isState && client.ObjectKeyFromObject(state) == submittedKey &&
							state.Annotations[sandboxApplyStateAnnotation] == sandboxApplyCancelled
						if cancelling {
							cancellationAttempted = true
							switch scenario {
							case "competing-submission":
								current := &corev1.ConfigMap{}
								if err := c.Get(ctx, submittedKey, current); err != nil {
									return err
								}
								current.Annotations[sandboxApplyStateAnnotation] = sandboxApplySubmitted
								current.Annotations[sandboxApplyAttemptAnnotation] = "synthetic-competing-apply"
								if err := c.Update(ctx, current); err != nil {
									return err
								}
							case "cancellation-write-failure":
								return cancellationFailure
							}
						}
						if err := c.Patch(ctx, object, patch, opts...); err != nil {
							return err
						}
						if cancelling && scenario == "cancellation-ack-loss" {
							return cancellationFailure
						}
						if isState && !rejected &&
							state.Data[w.PodExpectationKey] != "" && state.Annotations[sandboxApplyAttemptAnnotation] != "" {
							submittedKey = client.ObjectKeyFromObject(state)
							rejectNextRead = armed
						}
						return nil
					},
				})
				f.r.APIReader = interceptor.NewClient(f.kube, interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
						if rejectNextRead && key == submittedKey && !rejected {
							rejected = true
							return injected
						}
						return c.Get(ctx, key, object, opts...)
					},
				})
				installer := f.r.HelmClient.(*sharedSandboxInstaller)
				var attempted *helm.PreparedRelease
				installer.beforeApply = func(prepared *helm.PreparedRelease) error {
					attempted = prepared
					if scenario == "previous-submission" {
						var previousClaim string
						for range 2 {
							if err := prepared.AuthorizeSandboxApply(ctx); err != nil {
								return err
							}
							state := &corev1.ConfigMap{}
							if err := f.kube.Get(ctx, submittedKey, state); err != nil {
								return err
							}
							claim := state.Annotations[sandboxApplyAttemptAnnotation]
							if claim == "" || claim == previousClaim {
								t.Fatal("a submitted snapshot was reused without claiming a new version")
							}
							previousClaim = claim
						}
					}
					armed = true
					return nil
				}
				var failed error
				for range 5 {
					_, failed = f.reconcile()
					if failed != nil {
						break
					}
				}
				if !errors.Is(failed, injected) || !rejected || attempted == nil || f.installer.stages != 0 {
					t.Fatal("fixture did not fail at the final pre-write authority read:", failed)
				}
				expected := reconciledExpectationForTest(t, f)
				if err := f.kube.Get(ctx, client.ObjectKey{
					Namespace: expected.Namespace.Name, Name: instanceServiceName(f.agent.Name),
				}, &appsv1.StatefulSet{}); !apierrors.IsNotFound(err) {
					t.Fatal("rejected apply created its workload:", err)
				}
				state := &corev1.ConfigMap{}
				if err := f.kube.Get(ctx, submittedKey, state); err != nil {
					t.Fatal(err)
				}
				f.r.Client, f.r.APIReader = originalClient, originalReader
				installer.beforeApply = nil
				if scenario == "cancellation-write-failure" || scenario == "cancellation-ack-loss" {
					if !cancellationAttempted || !errors.Is(failed, cancellationFailure) {
						t.Fatal("cancellation failure was hidden:", failed)
					}
				}
				if scenario == "previous-submission" || scenario == "competing-submission" {
					if state.Annotations[sandboxApplyStateAnnotation] != sandboxApplySubmitted {
						t.Fatal("uncertain or superseded submission was cancelled")
					}
					if scenario == "previous-submission" && cancellationAttempted {
						t.Fatal("a prior successful authorization was treated as a first submission")
					}
					if scenario == "competing-submission" && (!apierrors.IsConflict(failed) ||
						state.Annotations[sandboxApplyAttemptAnnotation] != "synthetic-competing-apply") {
						t.Fatal("cancellation did not preserve the competing claim:", failed)
					}
					ticket, found, err := f.r.SandboxNativeCoordination.ReadNative(ctx, f.agent.UID)
					if err != nil || !found || ticket.Reservation.Phase != sandboxcoordination.Stopping {
						t.Fatal("uncertain submission released its source reservation:", err)
					}
					for range 2 {
						if _, err := f.reconcile(); err == nil {
							t.Fatal("uncertain submission resumed without shutdown evidence")
						}
					}
					if err := f.kube.Delete(ctx, f.current(t)); err != nil {
						t.Fatal(err)
					}
					if _, err := f.reconcile(); err == nil {
						t.Fatal("uncertain submission allowed deletion without shutdown evidence")
					}
					if err := f.kube.Get(ctx, submittedKey, state); err != nil ||
						state.Annotations[sandboxApplyStateAnnotation] != sandboxApplySubmitted || f.installer.stages != 0 {
						t.Fatal("uncertain submission lost its checkpoint or started a workload:", err)
					}
					return
				}
				wantState := sandboxApplyCancelled
				if scenario == "cancellation-write-failure" {
					wantState = sandboxApplyPending
				}
				if state.Annotations[sandboxApplyStateAnnotation] != wantState {
					t.Fatal("proven no-write failure retained a submitted checkpoint")
				}
				cleanupPending := scenario == "cancellation-write-failure"
				ticket, found, err := f.r.SandboxNativeCoordination.ReadNative(ctx, f.agent.UID)
				if err != nil || found != cleanupPending || (found && ticket.Reservation.Phase != sandboxcoordination.Stopping) {
					t.Fatal("pre-write failure did not preserve the confirmed cleanup state:", err)
				}
				grants, err := f.r.sandboxGrantJournal().Read(ctx, f.agent.Name, f.agent.UID)
				if err != nil {
					t.Fatal(err)
				}
				unconfirmed := unconfirmedKubernetesGrants(grants)
				if (cleanupPending && len(unconfirmed) != 2) || (!cleanupPending && len(grants) != 0) {
					t.Fatal("grant cleanup did not retain intent until cancellation was confirmed")
				}
				if err := attempted.AuthorizeSandboxApply(ctx); err == nil {
					t.Fatal("cancelled preparation still authorized a delayed apply")
				}
				if scenario == "delete" {
					if err := f.kube.Delete(ctx, f.current(t)); err != nil {
						t.Fatal(err)
					}
					for range 3 {
						f.reconcileOK(t)
					}
					if err := f.kube.Get(ctx, client.ObjectKey{Name: f.agent.Name}, &api.Agent{}); !apierrors.IsNotFound(err) {
						t.Fatal("pre-write authority failure blocked normal deletion:", err)
					}
					if err := f.kube.Get(ctx, client.ObjectKey{Namespace: f.r.OperatorNamespace,
						Name: sandboxpolicy.GrantJournalName(f.agent.UID)}, &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
						t.Fatal("normal deletion retained its grant journal:", err)
					}
					return
				}
				for range 6 {
					f.reconcileOK(t)
				}
				assertKubernetesFixtureGrants(t, f, true)
				current := reconciledExpectationForTest(t, f)
				if current.Home != expected.Home || current.Agent != expected.Agent || current.Namespace != expected.Namespace ||
					f.installer.stages != 1 {
					t.Fatal("pre-write recovery replaced the lifetime or repeated workload applies")
				}
			})
		}
	}
}

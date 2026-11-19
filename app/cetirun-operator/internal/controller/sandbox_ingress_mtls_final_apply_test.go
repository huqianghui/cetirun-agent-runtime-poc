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
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestReconciledSandboxIngressMTLSFinalApplyRecovery(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, scenario := range []string{"retry", "delete", "claim-ack-loss", "submission-ack-loss", "concurrent-claim", "previous-submission"} {
			t.Run(kind+"/"+scenario, func(t *testing.T) {
				ctx := context.Background()
				f, _ := newUnreconciledSandboxRuntimeFixture(t, true, nil, true, kind, true, false,
					withReconciledCoordination(t), withIngressMTLS(t, kind))
				injected := errors.New("synthetic final mTLS authority failure")
				originalClient, originalReader := f.r.Client, f.r.APIReader
				armed, intercepted, tlsUnavailable := false, false, false
				var stateKey client.ObjectKey
				tlsKey := client.ObjectKey{Namespace: instanceNamespace(f.agent.Name), Name: "agent-ingress-tls"}
				f.r.Client = interceptor.NewClient(originalClient.(client.WithWatch), interceptor.Funcs{
					Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
						state, ok := object.(*corev1.ConfigMap)
						claim := ok && state.Data[w.PodExpectationKey] != "" && state.Annotations[sandboxApplyAttemptAnnotation] != ""
						if claim {
							stateKey = client.ObjectKeyFromObject(state)
						}
						submitting := claim && state.Annotations[sandboxApplyStateAnnotation] == sandboxApplySubmitted
						if armed && !intercepted && scenario == "concurrent-claim" && submitting {
							current := &corev1.ConfigMap{}
							if err := c.Get(ctx, stateKey, current); err != nil {
								return err
							}
							current.Annotations[sandboxApplyAttemptAnnotation] = "synthetic-competing-attempt"
							if err := c.Update(ctx, current); err != nil {
								return err
							}
							intercepted = true
						}
						if err := c.Patch(ctx, object, patch, options...); err != nil {
							return err
						}
						if armed && !intercepted && claim {
							switch scenario {
							case "claim-ack-loss":
								intercepted = true
								return injected
							case "submission-ack-loss":
								if submitting {
									intercepted = true
									return injected
								}
							case "retry", "delete", "previous-submission":
								intercepted, tlsUnavailable = true, true
							}
						}
						return nil
					},
				})
				f.r.APIReader = interceptor.NewClient(f.kube.(client.WithWatch), interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
						if tlsUnavailable && key == tlsKey {
							return injected
						}
						return c.Get(ctx, key, object, options...)
					},
				})
				installer := f.r.HelmClient.(*sharedSandboxInstaller)
				var attempted *helm.PreparedRelease
				installer.beforeApply = func(prepared *helm.PreparedRelease) error {
					attempted = prepared
					if scenario == "previous-submission" {
						var previous string
						for range 2 {
							if err := prepared.AuthorizeSandboxApply(ctx); err != nil {
								return err
							}
							state := &corev1.ConfigMap{}
							if err := f.kube.Get(ctx, stateKey, state); err != nil {
								return err
							}
							current := state.Annotations[sandboxApplyAttemptAnnotation]
							if current == "" || current == previous {
								t.Fatal("submitted apply reused a resource-version claim")
							}
							previous = current
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
				if failed == nil || !intercepted || attempted == nil || f.installer.stages != 0 {
					t.Fatal("fixture missed the final manifest-delivery boundary:", failed)
				}
				if scenario == "concurrent-claim" && !apierrors.IsConflict(failed) {
					t.Fatal("competing attempt did not invalidate the submission CAS:", failed)
				}
				expected := reconciledExpectationForTest(t, f)
				if err := f.kube.Get(ctx, client.ObjectKey{Namespace: expected.Namespace.Name, Name: instanceServiceName(f.agent.Name)}, &appsv1.StatefulSet{}); !apierrors.IsNotFound(err) {
					t.Fatal("failed final guard delivered a workload:", err)
				}
				installer.beforeApply = nil
				tlsUnavailable = true
				if _, err := f.reconcile(); err == nil {
					t.Fatal("unavailable mTLS prerequisite did not hold the failed attempt")
				}
				f.r.Client, f.r.APIReader = originalClient, originalReader
				state := &corev1.ConfigMap{}
				if err := f.kube.Get(ctx, stateKey, state); err != nil {
					t.Fatal(err)
				}
				ticket, found, err := f.r.SandboxNativeCoordination.ReadNative(ctx, f.agent.UID)
				if err != nil {
					t.Fatal(err)
				}
				uncertain := scenario == "submission-ack-loss" || scenario == "previous-submission"
				if uncertain {
					if state.Annotations[sandboxApplyStateAnnotation] != sandboxApplySubmitted || !found ||
						ticket.Reservation.Phase != sandboxcoordination.Stopping {
						t.Fatal("uncertain submitted attempt was downgraded using workload absence")
					}
					for range 2 {
						if _, err := f.reconcile(); err == nil {
							t.Fatal("uncertain submission retried without shutdown evidence")
						}
					}
					if err := f.kube.Delete(ctx, f.current(t)); err != nil {
						t.Fatal(err)
					}
					if _, err := f.reconcile(); err == nil {
						t.Fatal("uncertain submission allowed normal deletion")
					}
					return
				}
				if state.Annotations[sandboxApplyStateAnnotation] != sandboxApplyCancelled || found {
					t.Fatal("proven pre-delivery failure stranded its reservation")
				}
				if err := attempted.AuthorizeSandboxApply(ctx); err == nil {
					t.Fatal("cancelled or superseded attempt could replay")
				}
				if scenario == "delete" {
					if err := f.kube.Delete(ctx, f.current(t)); err != nil {
						t.Fatal(err)
					}
					for range 3 {
						f.reconcileOK(t)
					}
					if err := f.kube.Get(ctx, client.ObjectKey{Name: f.agent.Name}, &api.Agent{}); !apierrors.IsNotFound(err) {
						t.Fatal("final-guard failure blocked normal deletion:", err)
					}
					return
				}
				for range 4 {
					f.reconcileOK(t)
				}
				current := reconciledExpectationForTest(t, f)
				if current.Agent != expected.Agent || current.Home != expected.Home || current.Namespace != expected.Namespace ||
					f.installer.stages != 1 {
					t.Fatal("retry replaced the retained lifetime or replayed a workload")
				}
			})
		}
	}
}

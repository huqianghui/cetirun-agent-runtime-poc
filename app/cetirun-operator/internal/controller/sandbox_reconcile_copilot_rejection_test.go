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
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload/testfixture"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestReconciledSandboxCopilotFirstWorkloadRejectionRecovery(t *testing.T) {
	for _, backend := range []api.SandboxType{api.SandboxKata, api.SandboxContainer} {
		for _, operation := range []string{"retry", "delete"} {
			t.Run(string(backend)+"/"+operation, func(t *testing.T) {
				ctx := context.Background()
				input := testfixture.New("openclaw")
				setup := []func(*AgentReconciler, client.Client){withReconciledCoordination(t)}
				if backend == api.SandboxContainer {
					setup = append(setup, withContainerRuntime(t))
				}
				setup = append(setup, func(r *AgentReconciler, author client.Client) {
					configureReconciledKubernetesAccess(t, r, author, input.Agent.Name, input.Policy.Name)
					installKubernetesFixtureCA(t, r, author, input.Agent.Name)
				})
				f, _ := newUnreconciledSandboxRuntimeFixture(t, true, nil, true, "openclaw", true, true, setup...)
				installer := f.r.HelmClient.(*sharedSandboxInstaller)
				original := installer.kube
				var attempted *helm.PreparedRelease
				installer.beforeApply = func(prepared *helm.PreparedRelease) error {
					attempted = prepared
					return nil
				}
				auxiliary := &appsv1.Deployment{}
				auxiliaryCreated, rejected := false, false
				installer.kube = interceptor.NewClient(f.kube, interceptor.Funcs{
					Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
						if object.GetObjectKind().GroupVersionKind().Kind == "StatefulSet" {
							if !auxiliaryCreated {
								t.Fatal("fixture did not submit Copilot before the primary workload")
							}
							rejected = true
							return apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "statefulsets"},
								object.GetName(), errors.New("synthetic primary admission rejection"))
						}
						if err := c.Create(ctx, object, opts...); err != nil {
							return err
						}
						if object.GetObjectKind().GroupVersionKind().Kind == "Deployment" {
							auxiliaryCreated = true
							if err := c.Get(ctx, client.ObjectKeyFromObject(object), auxiliary); err != nil {
								return err
							}
							auxiliary.Status.ObservedGeneration = auxiliary.Generation
							auxiliary.Status.Replicas = 1
							return c.Status().Update(ctx, auxiliary)
						}
						return nil
					},
				})
				for range 5 {
					if _, err := f.reconcile(); err != nil {
						if !apierrors.IsForbidden(err) {
							t.Fatal(err)
						}
						break
					}
				}
				if !rejected {
					t.Fatal("primary workload was not rejected")
				}
				expected := reconciledExpectationForTest(t, f)
				state := &corev1.ConfigMap{}
				if err := f.kube.Get(ctx, client.ObjectKey{Namespace: f.r.OperatorNamespace,
					Name: w.PodExpectationName(expected.Namespace.Name, expected.Namespace.UID)}, state); err != nil ||
					state.Annotations[sandboxApplyStateAnnotation] != sandboxApplyCancelled {
					t.Fatal("Copilot consumed the primary non-creation proof:", err)
				}
				ticket, found, err := f.r.SandboxNativeCoordination.ReadNative(ctx, f.agent.UID)
				if err != nil || !found || ticket.Reservation.Phase != sandboxcoordination.Stopping {
					t.Fatal("primary rejection skipped unacknowledged auxiliary shutdown:", err)
				}
				if err := attempted.AuthorizeSandboxApply(ctx); err == nil {
					t.Fatal("rejected primary writer retained authority")
				}
				if err := f.kube.Get(ctx, client.ObjectKeyFromObject(auxiliary), auxiliary); err != nil {
					t.Fatal(err)
				}
				auxiliary.Status.ObservedGeneration, auxiliary.Status.Replicas = auxiliary.Generation, 0
				if err := f.kube.Status().Update(ctx, auxiliary); err != nil {
					t.Fatal(err)
				}
				installer.kube = original
				if operation == "delete" {
					if err := f.kube.Delete(ctx, f.current(t)); err != nil {
						t.Fatal(err)
					}
				}
				for range 6 {
					f.reconcileOK(t)
				}
				if operation == "delete" {
					if err := f.kube.Get(ctx, client.ObjectKey{Name: f.agent.Name}, &api.Agent{}); !apierrors.IsNotFound(err) {
						t.Fatal("ordinary deletion remained blocked:", err)
					}
					return
				}
				current := reconciledExpectationForTest(t, f)
				if current.Agent != expected.Agent || current.Namespace != expected.Namespace || current.Home != expected.Home ||
					f.installer.stages != 1 {
					t.Fatal("retry replaced the retained lifetime or applied more than once")
				}
				assertKubernetesFixtureGrants(t, f, true)
			})
		}
	}
}

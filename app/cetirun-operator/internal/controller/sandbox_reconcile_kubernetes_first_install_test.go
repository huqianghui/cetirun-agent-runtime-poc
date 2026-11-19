// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func firstInstallKubernetesFixture(t *testing.T, kind string, setup ...func(*AgentReconciler, client.Client)) *nativeSandboxFixture {
	t.Helper()
	input := testfixture.New(kind)
	setup = append([]func(*AgentReconciler, client.Client){withReconciledCoordination(t)}, setup...)
	setup = append(setup, func(r *AgentReconciler, author client.Client) {
		configureReconciledKubernetesAccess(t, r, author, input.Agent.Name, input.Policy.Name)
		installKubernetesFixtureCA(t, r, author, input.Agent.Name)
	})
	f, _ := newUnreconciledSandboxRuntimeFixture(t, true, nil, true, kind, true, false, setup...)
	return f
}

func TestReconciledSandboxKubernetesFirstInstallRejection(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, bindingKind := range []string{"RoleBinding", "ClusterRoleBinding"} {
			for _, operation := range []string{"retry", "delete"} {
				t.Run(kind+"/"+bindingKind+"/"+operation, func(t *testing.T) {
					ctx := context.Background()
					f := firstInstallKubernetesFixture(t, kind)
					installer := f.r.HelmClient.(*sharedSandboxInstaller)
					var attempted *helm.PreparedRelease
					installer.beforeApply = func(prepared *helm.PreparedRelease) error {
						attempted = prepared
						return nil
					}
					original := installer.kube
					rejected, workloadWrites := false, 0
					installer.kube = interceptor.NewClient(f.kube, interceptor.Funcs{
						Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
							gvk := object.GetObjectKind().GroupVersionKind()
							if gvk.GroupVersion() == appsv1.SchemeGroupVersion {
								workloadWrites++
							}
							if gvk.Kind == bindingKind {
								rejected = true
								return apierrors.NewForbidden(schema.GroupResource{
									Group: "rbac.authorization.k8s.io", Resource: "bindings",
								}, object.GetName(), fmt.Errorf("synthetic missing bind permission"))
							}
							return c.Create(ctx, object, opts...)
						},
					})
					var failed error
					for range 5 {
						_, failed = f.reconcile()
						if failed != nil {
							break
						}
					}
					if !apierrors.IsForbidden(failed) || !rejected || workloadWrites != 0 {
						t.Fatalf("fixture did not reject the first grant before workloads: rejected=%t writes=%d error=%v",
							rejected, workloadWrites, failed)
					}
					expected := reconciledExpectationForTest(t, f)
					state := &corev1.ConfigMap{}
					if err := f.kube.Get(ctx, client.ObjectKey{
						Namespace: f.r.OperatorNamespace, Name: w.PodExpectationName(expected.Namespace.Name, expected.Namespace.UID),
					}, state); err != nil || state.Annotations[sandboxApplyStateAnnotation] != sandboxApplyCancelled {
						t.Fatal("a rejected first grant stranded an unsubmitted workload:", err)
					}
					if _, found, err := f.r.SandboxNativeCoordination.ReadNative(ctx, f.agent.UID); err != nil || found {
						t.Fatal("first-install rejection stranded its source reservation:", err)
					}
					if refs, err := f.r.sandboxGrantJournal().Read(ctx, f.agent.Name, f.agent.UID); err != nil || len(refs) != 0 {
						t.Fatal("first-install rejection left active grant intents:", err)
					}
					if err := attempted.AuthorizeSandboxApply(ctx); err == nil {
						t.Fatal("a cancelled first install allowed a delayed workload submission")
					}
					installer.kube = original
					if operation == "delete" {
						if err := f.kube.Delete(ctx, f.current(t)); err != nil {
							t.Fatal(err)
						}
					}
					for range 5 {
						f.reconcileOK(t)
					}
					if operation == "delete" {
						if err := f.kube.Get(ctx, client.ObjectKey{Name: f.agent.Name}, &api.Agent{}); !apierrors.IsNotFound(err) {
							t.Fatal("first-install rejection blocked normal deletion:", err)
						}
						if _, err := f.r.sandboxGrantJournal().Read(ctx, f.agent.Name, f.agent.UID); !apierrors.IsNotFound(err) {
							t.Fatal("normal deletion retained the grant journal:", err)
						}
						return
					}
					assertKubernetesFixtureGrants(t, f, true)
					current := reconciledExpectationForTest(t, f)
					if current.Agent != expected.Agent || current.Namespace != expected.Namespace || current.Home != expected.Home ||
						f.installer.stages != 1 {
						t.Fatal("retry replaced the first-install lifetime or repeated the workload apply")
					}
				})
			}
		}
	}
}

func TestReconciledSandboxKubernetesFirstInstallUnknownWrites(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, resourceKind := range []string{"RoleBinding", "StatefulSet"} {
			for _, outcome := range []string{"timeout", "lost-response"} {
				t.Run(kind+"/"+resourceKind+"/"+outcome, func(t *testing.T) {
					ctx := context.Background()
					f := firstInstallKubernetesFixture(t, kind)
					installer := f.r.HelmClient.(*sharedSandboxInstaller)
					original := installer.kube
					var delayed *unstructured.Unstructured
					var injected error = io.ErrUnexpectedEOF
					if outcome == "timeout" {
						injected = apierrors.NewTimeoutError("synthetic pending write", 0)
					}
					installer.kube = interceptor.NewClient(f.kube, interceptor.Funcs{
						Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
							if candidate, ok := object.(*unstructured.Unstructured); ok && candidate.GetKind() == resourceKind {
								delayed = candidate.DeepCopy()
								return injected
							}
							return c.Create(ctx, object, opts...)
						},
					})
					var failed error
					for range 5 {
						_, failed = f.reconcile()
						if failed != nil {
							break
						}
					}
					if !errors.Is(failed, injected) || delayed == nil || f.installer.stages != 0 {
						t.Fatal("fixture did not retain an uncertain first-install write:", failed)
					}
					installer.kube = original
					assertPending := func() {
						t.Helper()
						expected := reconciledExpectationForTest(t, f)
						state := &corev1.ConfigMap{}
						if err := f.kube.Get(ctx, client.ObjectKey{
							Namespace: f.r.OperatorNamespace, Name: w.PodExpectationName(expected.Namespace.Name, expected.Namespace.UID),
						}, state); err != nil {
							t.Fatal(err)
						}
						if resourceKind == "StatefulSet" {
							if state.Annotations[sandboxApplyStateAnnotation] != sandboxApplySubmitted {
								t.Fatal("uncertain workload submission was cancelled")
							}
						} else {
							refs, err := f.r.sandboxGrantJournal().Read(ctx, f.agent.Name, f.agent.UID)
							if err != nil {
								t.Fatal(err)
							}
							submitted, err := submittedReconciledGrants(state, refs)
							if err != nil || !slices.ContainsFunc(submitted, func(ref sandboxpolicy.BindingReference) bool {
								return ref.Kind == delayed.GetKind() && ref.Namespace == delayed.GetNamespace() &&
									ref.Name == delayed.GetName() && ref.UID == ""
							}) {
								t.Fatal("no-workload cancellation discarded an uncertain grant write:", err)
							}
						}
						ticket, found, err := f.r.SandboxNativeCoordination.ReadNative(ctx, f.agent.UID)
						if err != nil || !found || ticket.Reservation.Phase != sandboxcoordination.Stopping || f.installer.stages != 0 {
							t.Fatal("uncertain write released its reservation or retried the workload:", err)
						}
					}
					assertPending()
					for range 2 {
						if _, err := f.reconcile(); err == nil {
							t.Fatal("an unconfirmed write permitted retry")
						}
						assertPending()
					}
					if err := f.kube.Delete(ctx, f.current(t)); err != nil {
						t.Fatal(err)
					}
					if _, err := f.reconcile(); err == nil {
						t.Fatal("an unconfirmed write permitted deletion")
					}
					assertPending()
				})
			}
		}
	}
}

// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload/testfixture"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func stoppedReconciledKubernetesFixture(t *testing.T, kind string) *nativeSandboxFixture {
	t.Helper()
	ctx := context.Background()
	input := testfixture.New(kind)
	f := newSandboxRuntimeFixtureForAgent(t, true, nil, true, kind, true, false,
		withReconciledCoordination(t), func(r *AgentReconciler, author client.Client) {
			configureReconciledKubernetesAccess(t, r, author, input.Agent.Name, input.Policy.Name)
			installKubernetesFixtureCA(t, r, author, input.Agent.Name)
		})
	f.reconcileOK(t)
	agent := f.current(t)
	agent.Spec.Suspended = true
	agent.Generation++
	if err := f.kube.Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		f.reconcileOK(t)
	}
	if f.current(t).Status.Phase != api.PhaseSuspended {
		t.Fatal("fixture must resume an already-stopped Agent")
	}
	assertKubernetesFixtureGrants(t, f, false)
	return f
}

func TestReconciledSandboxKubernetesRejectedGrantRecovery(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, bindingKind := range []string{"RoleBinding", "ClusterRoleBinding"} {
			for _, operation := range []string{"retry", "delete"} {
				t.Run(kind+"/"+bindingKind+"/"+operation, func(t *testing.T) {
					ctx := context.Background()
					f := stoppedReconciledKubernetesFixture(t, kind)
					initial := reconciledExpectationForTest(t, f)
					installer := f.r.HelmClient.(*sharedSandboxInstaller)
					originalClient := installer.kube
					rejected := false
					installer.kube = interceptor.NewClient(f.kube, interceptor.Funcs{
						Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
							if object.GetObjectKind().GroupVersionKind().Kind == bindingKind {
								rejected = true
								return apierrors.NewForbidden(schema.GroupResource{
									Group: "rbac.authorization.k8s.io", Resource: "bindings",
								}, object.GetName(), fmt.Errorf("synthetic missing bind permission"))
							}
							return c.Create(ctx, object, opts...)
						},
					})
					agent := f.current(t)
					agent.Spec.Suspended = false
					agent.Generation++
					if err := f.kube.Update(ctx, agent); err != nil {
						t.Fatal(err)
					}
					if _, err := f.reconcile(); !apierrors.IsForbidden(err) || !rejected {
						t.Fatal("fixture did not reach a definitive binding rejection:", err)
					}
					installer.kube = originalClient
					if operation == "delete" {
						if err := f.kube.Delete(ctx, f.current(t)); err != nil {
							t.Fatal(err)
						}
					}

					for range 5 {
						f.reconcileOK(t)
					}
					if operation == "delete" {
						if err := f.kube.Get(ctx, client.ObjectKey{Name: agent.Name}, &api.Agent{}); !apierrors.IsNotFound(err) {
							t.Fatal("definitive grant rejection blocked normal deletion:", err)
						}
						if _, err := f.r.sandboxGrantJournal().Read(ctx, agent.Name, agent.UID); !apierrors.IsNotFound(err) {
							t.Fatal("normal deletion retained the grant journal:", err)
						}
						return
					}
					assertKubernetesFixtureGrants(t, f, true)
					current := reconciledExpectationForTest(t, f)
					if current.Agent != initial.Agent || current.Namespace != initial.Namespace || current.Home != initial.Home {
						t.Fatal("recovery after restoring bind permission replaced the lifetime")
					}
				})
			}
		}
	}
}

func TestReconciledSandboxKubernetesGrantRejectionFences(t *testing.T) {
	for _, scenario := range []string{"rejected", "duplicate", "new-claim", "new-grant", "write-failure", "lost-ack"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			f := stoppedReconciledKubernetesFixture(t, "openclaw")
			installer := f.r.HelmClient.(*sharedSandboxInstaller)
			originalClient := f.r.Client
			injected := errors.New("synthetic rejection outcome failure")
			var claimed *corev1.ConfigMap
			var outcomeErr error
			recording := false
			f.r.Client = interceptor.NewClient(originalClient.(client.WithWatch), interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if !recording {
						return c.Patch(ctx, object, patch, opts...)
					}
					data, err := patch.Data(object)
					var payload struct {
						Metadata metav1.ObjectMeta `json:"metadata"`
					}
					if err != nil || json.Unmarshal(data, &payload) != nil ||
						payload.Metadata.UID != claimed.UID || payload.Metadata.ResourceVersion != claimed.ResourceVersion {
						t.Fatal("rejection omitted the original submission's atomic UID/version fence:", err)
					}
					if scenario == "write-failure" {
						outcomeErr = injected
						return outcomeErr
					}
					outcomeErr = c.Patch(ctx, object, patch, opts...)
					if outcomeErr == nil && scenario == "lost-ack" {
						outcomeErr = injected
					}
					return outcomeErr
				},
			})
			var attempted *helm.PreparedRelease
			var bindings []sandboxpolicy.BindingReference
			installer.beforeApply = func(prepared *helm.PreparedRelease) error {
				attempted = prepared
				if err := prepared.AuthorizeSandboxApply(ctx); err != nil {
					return err
				}
				expected, err := prepared.ReconcileExpectation()
				if err != nil {
					return err
				}
				var objects []unstructured.Unstructured
				for _, object := range expected.Controls {
					if w.KubernetesBindingResource(object) {
						objects = append(objects, object)
					}
				}
				if len(objects) != 2 {
					t.Fatal("fixture requires two independently submitted grants")
				}
				complete, err := prepared.AuthorizeSandboxBindingCreate(ctx, objects[0])
				if err != nil {
					return err
				}
				claimed = &corev1.ConfigMap{}
				if err := f.kube.Get(ctx, client.ObjectKey{
					Namespace: f.r.OperatorNamespace, Name: w.PodExpectationName(expected.Namespace.Name, expected.Namespace.UID),
				}, claimed); err != nil {
					return err
				}
				recorded, err := w.ReadReconcileExpectation(claimed)
				if err != nil {
					return err
				}
				bindings, err = recorded.KubernetesBindings()
				if err != nil {
					return err
				}
				switch scenario {
				case "new-claim":
					if err := prepared.AuthorizeSandboxApply(ctx); err != nil {
						return err
					}
				case "new-grant":
					if _, err := prepared.AuthorizeSandboxBindingCreate(ctx, objects[1]); err != nil {
						return err
					}
				}
				rejection := apierrors.NewForbidden(schema.GroupResource{
					Group: "rbac.authorization.k8s.io", Resource: "bindings",
				}, objects[0].GetName(), fmt.Errorf("synthetic missing bind permission"))
				recording = true
				err = complete(rejection)
				if scenario == "duplicate" {
					if duplicate := complete(rejection); duplicate == nil || !apierrors.IsConflict(outcomeErr) {
						t.Fatal("a rejection receipt was accepted twice:", duplicate)
					}
				}
				recording = false
				if (scenario == "new-claim" || scenario == "new-grant") && !apierrors.IsConflict(outcomeErr) {
					t.Fatal("a stale rejection cleared a newer claim:", outcomeErr)
				}
				return err
			}
			agent := f.current(t)
			agent.Spec.Suspended = false
			agent.Generation++
			if err := f.kube.Update(ctx, agent); err != nil {
				t.Fatal(err)
			}
			if _, err := f.reconcile(); !apierrors.IsForbidden(err) || attempted == nil || claimed == nil ||
				((scenario == "write-failure" || scenario == "lost-ack") && !errors.Is(err, injected)) {
				t.Fatal("rejection or outcome failure was hidden:", err)
			}
			f.r.Client = originalClient
			installer.beforeApply = nil
			state := &corev1.ConfigMap{}
			if err := f.kube.Get(ctx, client.ObjectKeyFromObject(claimed), state); err != nil {
				t.Fatal(err)
			}
			pending, err := submittedReconciledGrants(state, bindings)
			want := 0
			switch scenario {
			case "new-claim", "write-failure":
				want = 1
			case "new-grant":
				want = 2
			}
			if err != nil || len(pending) != want || state.Annotations[sandboxApplyStateAnnotation] != sandboxApplySubmitted {
				t.Fatalf("rejection changed the wrong authority: pending=%d want=%d error=%v", len(pending), want, err)
			}
			if err := attempted.AuthorizeSandboxApply(ctx); err == nil {
				t.Fatal("a rejected or superseded prepared apply remained usable")
			}
			if want != 0 {
				if _, err := f.reconcile(); !errors.Is(err, sandboxpolicy.ErrRevocationPending) {
					t.Fatal("an unconfirmed outcome bypassed the recovery barrier:", err)
				}
				return
			}
			for range 5 {
				f.reconcileOK(t)
			}
			assertKubernetesFixtureGrants(t, f, true)
		})
	}
}

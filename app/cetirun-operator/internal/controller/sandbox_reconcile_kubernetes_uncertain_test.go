// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"slices"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload/testfixture"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestReconciledSandboxKubernetesUncertainGrantRecovery(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, delayedKind := range []string{"RoleBinding", "ClusterRoleBinding"} {
			t.Run(kind+"/"+delayedKind, func(t *testing.T) {
				ctx := context.Background()
				input := testfixture.New(kind)
				f := newSandboxRuntimeFixtureForAgent(t, true, nil, true, kind, true, false,
					withReconciledCoordination(t), func(r *AgentReconciler, author client.Client) {
						configureReconciledKubernetesAccess(t, r, author, input.Agent.Name, input.Policy.Name)
						installKubernetesFixtureCA(t, r, author, input.Agent.Name)
					})
				f.reconcileOK(t)
				initial := reconciledExpectationForTest(t, f)
				agent := f.current(t)
				keyID := agent.Status.AIGateway.KeyID
				agent.Spec.Suspended = true
				agent.Generation++
				if err := f.kube.Update(ctx, agent); err != nil {
					t.Fatal(err)
				}
				for range 3 {
					f.reconcileOK(t)
				}
				if f.current(t).Status.Phase != api.PhaseSuspended {
					t.Fatal("fixture must start its resume from an already-stopped Agent")
				}
				assertKubernetesFixtureGrants(t, f, false)

				installer := f.r.HelmClient.(*sharedSandboxInstaller)
				injected := errors.New("synthetic grant write completion unavailable")
				var delayed *unstructured.Unstructured
				installer.beforeApply = func(prepared *helm.PreparedRelease) error {
					if err := prepared.AuthorizeSandboxApply(ctx); err != nil {
						return err
					}
					expected, err := prepared.ReconcileExpectation()
					if err != nil {
						return err
					}
					for _, object := range expected.Controls {
						if !w.KubernetesBindingResource(object) {
							continue
						}
						if _, err := prepared.AuthorizeSandboxBindingCreate(ctx, object); err != nil {
							return err
						}
						if object.GetKind() == delayedKind {
							delayed = object.DeepCopy()
							continue
						}
						if err := f.kube.Create(ctx, object.DeepCopy()); err != nil {
							return err
						}
					}
					return injected
				}
				agent = f.current(t)
				agent.Spec.Suspended = false
				agent.Generation++
				if err := f.kube.Update(ctx, agent); err != nil {
					t.Fatal(err)
				}
				stages := f.installer.stages
				if _, err := f.reconcile(); !errors.Is(err, injected) || delayed == nil {
					t.Fatal("fixture did not reach the submitted grant-write boundary:", err)
				}
				installer.beforeApply = nil
				journal := f.r.sandboxGrantJournal()
				pending := func() {
					t.Helper()
					refs, err := journal.Read(ctx, agent.Name, agent.UID)
					if err != nil || !slices.ContainsFunc(refs, func(ref sandboxpolicy.BindingReference) bool {
						return ref.Kind == delayedKind && ref.Namespace == delayed.GetNamespace() &&
							ref.Name == delayed.GetName() && ref.UID == ""
					}) {
						t.Fatal("unconfirmed grant intent was lost during cleanup:", err)
					}
					ticket, found, err := f.r.SandboxNativeCoordination.ReadNative(ctx, agent.UID)
					if err != nil || !found || ticket.Reservation.Phase != sandboxcoordination.Stopping {
						t.Fatal("unconfirmed grant released its source reservation:", err)
					}
					if f.installer.stages != stages {
						t.Fatal("an unconfirmed grant outcome started another apply")
					}
				}
				pending()
				for range 2 {
					_, _ = f.reconcile()
					pending()
				}

				policy := &api.AgentPolicy{}
				if err := f.kube.Get(ctx, client.ObjectKey{Name: input.Policy.Name}, policy); err != nil {
					t.Fatal(err)
				}
				if delayedKind == "RoleBinding" {
					policy.Spec.KubernetesAccess.RoleBindings = nil
				} else {
					policy.Spec.KubernetesAccess.ClusterRoleBindings = nil
				}
				policy.Generation++
				if err := f.kube.Update(ctx, policy); err != nil {
					t.Fatal(err)
				}
				refreshKubernetesFixturePolicy(t, f.r, policy.Name)
				_, _ = f.reconcile()
				pending()

				if err := f.kube.Create(ctx, delayed); err != nil {
					t.Fatal("complete the delayed owned grant:", err)
				}
				if err := f.r.revokeSandboxPreparationGrants(ctx, f.current(t)); err != nil {
					t.Fatal("revoke confirmed late completion:", err)
				}
				for range 5 {
					f.reconcileOK(t)
				}
				current := f.current(t)
				expected := reconciledExpectationForTest(t, f)
				bindings, err := expected.KubernetesBindings()
				if err != nil || len(bindings) != 1 || bindings[0].Kind == delayedKind ||
					expected.Agent != initial.Agent || expected.Namespace != initial.Namespace || expected.Home != initial.Home ||
					current.Status.AIGateway.KeyID != keyID {
					t.Fatal("recovery did not preserve the lifetime with only the reduced grant set:", err)
				}
				if err := f.kube.Get(ctx, client.ObjectKeyFromObject(delayed), delayed); !apierrors.IsNotFound(err) {
					t.Fatal("obsolete grant remained after recovery:", err)
				}
				if err := journal.VerifyKubernetesBindings(ctx, agent.Name, agent.UID, bindings); err != nil {
					t.Fatal("recovered current grants were not confirmed:", err)
				}
			})
		}
	}
}

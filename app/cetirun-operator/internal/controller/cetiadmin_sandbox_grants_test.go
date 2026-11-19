// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"errors"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/cetiadmin"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	"github.com/Azure/CetiRun/app/internal/registryauth"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestCetiAdminRuntimeAccessIsNotASandboxGrant(t *testing.T) {
	t.Run("valid runtime reader and receipt bindings stay outside sandbox inventory", func(t *testing.T) {
		f := newCetiAdminFixture(t)
		agent, namespace := f.target("sandbox-target", "identity-lab", "hermes", true)
		f.converge()
		if err := f.r.ReconcileRuntimeAccess(f.ctx, agent.Name); err != nil {
			t.Fatal(err)
		}

		state := f.state()
		target := cetiadmin.TargetIdentity{Agent: identityOf(agent), Namespace: identityOf(namespace)}
		want := map[client.ObjectKey]bool{}
		for _, object := range f.r.runtimeAccessObjects(state, agent, target) {
			if _, ok := object.(*rbacv1.RoleBinding); ok {
				want[client.ObjectKeyFromObject(object)] = true
			}
		}
		bindings := &rbacv1.RoleBindingList{}
		if err := f.kube.List(f.ctx, bindings); err != nil {
			t.Fatal(err)
		}
		for i := range bindings.Items {
			delete(want, client.ObjectKeyFromObject(&bindings.Items[i]))
		}
		if len(want) != 0 {
			t.Fatalf("runtime access producer did not create its expected bindings: %v", want)
		}

		if _, err := sandboxpolicy.OwnedBindingInventory(f.ctx, f.kube, agent.Name, agent.UID); !errors.Is(err, sandboxpolicy.ErrOwnership) {
			t.Fatal("an unconfigured cleanup inventory accepted CetiAdmin access:", err)
		}
		inventory, err := sandboxpolicy.OwnedBindingInventoryWithPlatformAccess(f.ctx, f.kube, cetiadmin.RuntimeAccessBindingVerifier(f.r.Source), agent.Name, agent.UID)
		if err != nil {
			t.Fatalf("valid CetiAdmin runtime access became an unconfirmed Sandbox grant: %v", err)
		}
		if len(inventory) != 0 {
			t.Fatalf("CetiAdmin runtime access entered the Sandbox grant inventory: %+v", inventory)
		}
	})

	t.Run("tampered CetiAdmin runtime binding does not conceal a grant", func(t *testing.T) {
		f := newCetiAdminFixture(t)
		agent, namespace := f.target("forged-target", "identity-lab", "hermes", true)
		f.converge()
		state := f.state()
		if err := f.r.ReconcileRuntimeAccess(f.ctx, agent.Name); err != nil {
			t.Fatal(err)
		}
		forged := &rbacv1.RoleBinding{}
		if err := f.kube.Get(f.ctx, client.ObjectKey{Namespace: namespace.Name, Name: cetiAdminAccessName(state) + "-runtime"}, forged); err != nil {
			t.Fatal(err)
		}
		forged.Subjects = []rbacv1.Subject{{Kind: "ServiceAccount", Name: "attacker", Namespace: namespace.Name}}
		if err := f.kube.Update(f.ctx, forged); err != nil {
			t.Fatal(err)
		}

		if _, err := sandboxpolicy.OwnedBindingInventoryWithPlatformAccess(f.ctx, f.kube, cetiadmin.RuntimeAccessBindingVerifier(f.r.Source), agent.Name, agent.UID); !errors.Is(err, sandboxpolicy.ErrOwnership) {
			t.Fatal("tampered CetiAdmin runtime binding concealed an unconfirmed Sandbox grant:", err)
		}
	})

	t.Run("sandbox grant remains subject to the normal inventory contract", func(t *testing.T) {
		f := newCetiAdminFixture(t)
		agent, namespace := f.target("sandbox-grant-target", "identity-lab", "hermes", true)
		owner := registryauth.Owner(agent)
		owner.Controller = ptr.To(true)
		grant := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{
			Name:      "sandbox-approved",
			Namespace: namespace.Name,
			Labels: map[string]string{
				sandboxpolicy.AgentUIDLabel: string(agent.UID),
			},
			Annotations: map[string]string{
				sandboxpolicy.GrantRevisionAnnotation: "approved-revision",
			},
			OwnerReferences: []metav1.OwnerReference{owner},
		}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: "sandbox-identity", Namespace: namespace.Name}},
			RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "sandbox-reader"}}
		f.seed(grant)

		inventory, err := sandboxpolicy.OwnedBindingInventory(f.ctx, f.kube, agent.Name, agent.UID)
		if err != nil || len(inventory) != 1 {
			t.Fatalf("normal Sandbox grant was not retained for verification: inventory=%+v err=%v", inventory, err)
		}
		if got := inventory[0]; got.Name != grant.Name || got.Namespace != grant.Namespace || got.RoleRef != grant.RoleRef || got.GrantRevision != "approved-revision" {
			t.Fatalf("normal Sandbox grant inventory drifted: %+v", got)
		}
	})
}

func TestCetiAdminSandboxInventoryRecognizesProtectedAndCustomRuntimeAccess(t *testing.T) {
	t.Run("protected Hermes has an empty receipt", func(t *testing.T) {
		f := newCetiAdminFixture(t)
		agent, _ := f.target("protected-target", "identity-lab", "hermes", true)
		agent.Spec.SandboxRef = "protected-sandbox"
		if err := f.kube.Update(f.ctx, agent); err != nil {
			t.Fatal(err)
		}
		f.converge()
		if err := f.r.ReconcileRuntimeAccess(f.ctx, agent.Name); err != nil {
			t.Fatal(err)
		}
		requireCetiAdminSandboxInventoryEmpty(t, f, agent)
	})

	t.Run("custom runtime has both receipt names", func(t *testing.T) {
		f := newCetiAdminFixture(t)
		agent, namespace := f.target("custom-target", "identity-lab", "custom", false)
		f.customReceipt(agent, namespace)
		f.converge()
		if err := f.r.ReconcileRuntimeAccess(f.ctx, agent.Name); err != nil {
			t.Fatal(err)
		}
		requireCetiAdminSandboxInventoryEmpty(t, f, agent)
	})
}

func TestCetiAdminRuntimeAccessGrantJournalRequiresTrustedAuthority(t *testing.T) {
	f := newCetiAdminFixture(t)
	agent, _ := f.target("journal-target", "identity-lab", "hermes", true)
	f.converge()
	if err := f.r.ReconcileRuntimeAccess(f.ctx, agent.Name); err != nil {
		t.Fatal(err)
	}
	journal := sandboxpolicy.GrantJournalStore{
		Client: f.kube, APIReader: f.kube, Namespace: f.r.Source.Namespace,
		PlatformAccessVerifier: cetiadmin.RuntimeAccessBindingVerifier(f.r.Source),
	}
	if err := journal.Record(f.ctx, agent.Name, agent.UID, nil); err != nil {
		t.Fatal(err)
	}
	for name, verify := range map[string]func(sandboxpolicy.GrantJournalStore) error{
		"hold release": func(store sandboxpolicy.GrantJournalStore) error {
			return store.VerifyKubernetesBindingIntents(f.ctx, agent.Name, agent.UID, nil)
		},
		"pod admission": func(store sandboxpolicy.GrantJournalStore) error {
			return store.VerifyKubernetesBindings(f.ctx, agent.Name, agent.UID, nil)
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := verify(journal); err != nil {
				t.Fatalf("valid platform access blocked an empty Sandbox grant journal: %v", err)
			}
			unconfigured := journal
			unconfigured.PlatformAccessVerifier = nil
			if err := verify(unconfigured); !errors.Is(err, sandboxpolicy.ErrOwnership) {
				t.Fatalf("unconfigured verifier accepted platform access: %v", err)
			}
			binding := cetiAdminRuntimeBinding(t, f, agent)
			original := binding.DeepCopy()
			binding.Subjects[0].Name = "untrusted-service-account"
			if err := f.kube.Update(f.ctx, binding); err != nil {
				t.Fatal(err)
			}
			if err := verify(journal); !errors.Is(err, sandboxpolicy.ErrOwnership) {
				t.Fatalf("tampered platform access escaped grant verification: %v", err)
			}
			binding.Subjects = original.Subjects
			if err := f.kube.Update(f.ctx, binding); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCetiAdminSandboxInventoryRejectsUnavailableAuthority(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		mutate func(*testing.T, *cetiAdminFixture)
	}{
		{"disabled source", func(t *testing.T, f *cetiAdminFixture) {
			f.config.Enabled = false
			f.updateConfig()
		}},
		{"disabled installation", func(t *testing.T, f *cetiAdminFixture) {
			stateObject, state, err := f.r.readState(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			state.Phase = cetiadmin.PhaseDisabled
			if err := f.r.saveState(f.ctx, stateObject, state); err != nil {
				t.Fatal(err)
			}
		}},
		{"terminating source", func(t *testing.T, f *cetiAdminFixture) {
			source := &corev1.ConfigMap{}
			if err := f.kube.Get(f.ctx, f.r.Source, source); err != nil {
				t.Fatal(err)
			}
			markCetiAdminTestObjectTerminating(t, f, source)
		}},
		{"terminating installation", func(t *testing.T, f *cetiAdminFixture) {
			stateObject := &corev1.ConfigMap{}
			if err := f.kube.Get(f.ctx, f.r.stateKey(), stateObject); err != nil {
				t.Fatal(err)
			}
			markCetiAdminTestObjectTerminating(t, f, stateObject)
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f := newCetiAdminFixture(t)
			agent, _ := f.target("authority-target", "identity-lab", "hermes", true)
			f.converge()
			if err := f.r.ReconcileRuntimeAccess(f.ctx, agent.Name); err != nil {
				t.Fatal(err)
			}
			scenario.mutate(t, f)
			requireCetiAdminSandboxInventoryRejected(t, f, agent)
		})
	}
}

func TestCetiAdminSandboxInventoryRejectsRuntimeAuthorityDrift(t *testing.T) {
	for _, scenario := range []struct {
		name      string
		protected bool
		mutate    func(*testing.T, *cetiAdminFixture, *api.Agent)
	}{
		{"runtime ClusterRole extra rule", false, func(t *testing.T, f *cetiAdminFixture, agent *api.Agent) {
			role := cetiAdminRuntimeRole(t, f)
			role.Rules = append(role.Rules, rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"delete"}})
			if err := f.kube.Update(f.ctx, role); err != nil {
				t.Fatal(err)
			}
		}},
		{"runtime ClusterRole aggregation", false, func(t *testing.T, f *cetiAdminFixture, agent *api.Agent) {
			role := cetiAdminRuntimeRole(t, f)
			role.AggregationRule = &rbacv1.AggregationRule{ClusterRoleSelectors: []metav1.LabelSelector{{MatchLabels: map[string]string{"example.com/aggregate": "true"}}}}
			if err := f.kube.Update(f.ctx, role); err != nil {
				t.Fatal(err)
			}
		}},
		{"ordinary runtime cannot use protected role", false, func(t *testing.T, f *cetiAdminFixture, agent *api.Agent) {
			binding := cetiAdminRuntimeBinding(t, f, agent)
			binding.RoleRef.Name = cetiAdminAccessName(f.state()) + "-protected"
			if err := f.kube.Update(f.ctx, binding); err != nil {
				t.Fatal(err)
			}
		}},
		{"protected runtime cannot use ordinary role", true, func(t *testing.T, f *cetiAdminFixture, agent *api.Agent) {
			binding := cetiAdminRuntimeBinding(t, f, agent)
			binding.RoleRef.Name = cetiAdminAccessName(f.state()) + "-runtime"
			if err := f.kube.Update(f.ctx, binding); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f := newCetiAdminFixture(t)
			agent, _ := f.target("drift-target", "identity-lab", "hermes", true)
			if scenario.protected {
				agent.Spec.SandboxRef = "protected-sandbox"
				if err := f.kube.Update(f.ctx, agent); err != nil {
					t.Fatal(err)
				}
			}
			f.converge()
			if err := f.r.ReconcileRuntimeAccess(f.ctx, agent.Name); err != nil {
				t.Fatal(err)
			}
			scenario.mutate(t, f, agent)
			requireCetiAdminSandboxInventoryRejected(t, f, agent)
		})
	}
}

func requireCetiAdminSandboxInventoryEmpty(t *testing.T, f *cetiAdminFixture, agent *api.Agent) {
	t.Helper()
	inventory, err := sandboxpolicy.OwnedBindingInventoryWithPlatformAccess(f.ctx, f.kube, cetiadmin.RuntimeAccessBindingVerifier(f.r.Source), agent.Name, agent.UID)
	if err != nil || len(inventory) != 0 {
		t.Fatalf("valid CetiAdmin access was not excluded from Sandbox inventory: inventory=%+v err=%v", inventory, err)
	}
}

func requireCetiAdminSandboxInventoryRejected(t *testing.T, f *cetiAdminFixture, agent *api.Agent) {
	t.Helper()
	if _, err := sandboxpolicy.OwnedBindingInventoryWithPlatformAccess(f.ctx, f.kube, cetiadmin.RuntimeAccessBindingVerifier(f.r.Source), agent.Name, agent.UID); !errors.Is(err, sandboxpolicy.ErrOwnership) {
		t.Fatal("drifted CetiAdmin access concealed an unconfirmed Sandbox grant:", err)
	}
}

func cetiAdminRuntimeRole(t *testing.T, f *cetiAdminFixture) *rbacv1.ClusterRole {
	t.Helper()
	role := &rbacv1.ClusterRole{}
	if err := f.kube.Get(f.ctx, client.ObjectKey{Name: cetiAdminAccessName(f.state()) + "-runtime"}, role); err != nil {
		t.Fatal(err)
	}
	return role
}

func cetiAdminRuntimeBinding(t *testing.T, f *cetiAdminFixture, agent *api.Agent) *rbacv1.RoleBinding {
	t.Helper()
	binding := &rbacv1.RoleBinding{}
	if err := f.kube.Get(f.ctx, client.ObjectKey{Namespace: instanceNamespace(agent.Name), Name: cetiAdminAccessName(f.state()) + "-runtime"}, binding); err != nil {
		t.Fatal(err)
	}
	return binding
}

func markCetiAdminTestObjectTerminating(t *testing.T, f *cetiAdminFixture, object client.Object) {
	t.Helper()
	object.SetFinalizers([]string{"example.com/hold"})
	if err := f.kube.Update(f.ctx, object); err != nil {
		t.Fatal(err)
	}
	uid, version := object.GetUID(), object.GetResourceVersion()
	if err := f.kube.Delete(f.ctx, object, client.Preconditions{UID: &uid, ResourceVersion: &version}); err != nil {
		t.Fatal(err)
	}
}

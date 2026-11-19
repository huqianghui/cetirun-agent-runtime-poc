// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"encoding/json"
	"reflect"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/cetiadmin"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestCetiAdminUpgradeStopsBoundLegacySourceWithoutRecreatingIdentity(t *testing.T) {
	f := newCetiAdminFixture(t)
	f.converge()
	before := f.state()
	source := &corev1.ConfigMap{}
	if err := f.kube.Get(f.ctx, f.r.Source, source); err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(source.Data[cetiadmin.ConfigKey]), &value); err != nil {
		t.Fatal(err)
	}
	value["enabled"] = false
	value["scope"] = map[string]any{"mode": "all-managed-agents", "teamNames": []string{}, "platformReadNamespaces": []string{}}
	delete(value, "platformReadNamespaces")
	raw, _ := json.Marshal(value)
	source.Data[cetiadmin.ConfigKey] = string(raw)
	if err := f.kube.Update(f.ctx, source); err != nil {
		t.Fatal(err)
	}
	if err := f.run(); err != nil {
		t.Fatal("existing disabled legacy source cannot complete its normal upgrade", err)
	}
	after := f.state()
	if after.Phase != cetiadmin.PhaseDisabled || after.Agent != before.Agent || after.ServiceAccount != before.ServiceAccount || after.Namespace != before.Namespace || after.Source != before.Source {
		t.Fatal("bridge upgrade changed retained installation identities")
	}
	f.noBindings()
}

func TestCetiAdminNewAgentDoesNotInvalidateRuntime(t *testing.T) {
	f := newCetiAdminFixture(t)
	f.target("existing", "first-team", api.AgentTypeHermes, true)
	f.converge()
	admin := f.agent()
	if _, handled, err := f.r.BindingForAgent(f.ctx, admin); err != nil || !handled {
		t.Fatalf("initial Admin runtime binding: handled=%v err=%v", handled, err)
	}
	f.target("new-agent", "other-team", api.AgentTypeOpenClaw, true)
	if _, handled, err := f.r.BindingForAgent(f.ctx, admin); err != nil || !handled {
		t.Fatalf("adding an Agent invalidated the unchanged Admin runtime: handled=%v err=%v", handled, err)
	}
	before := f.state()
	if err := f.r.ReconcileRuntimeAccess(f.ctx, "new-agent"); err != nil {
		t.Fatal(err)
	}
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, f.state()) || !reflect.DeepEqual(admin.Spec, f.agent().Spec) || admin.UID != f.agent().UID {
		t.Fatal("granting ordinary namespace access changed the Admin runtime or installation")
	}
}

func TestCetiAdminRuntimeAccessDoesNotDeleteForeignSubjects(t *testing.T) {
	f := newCetiAdminFixture(t)
	agent, namespace := f.target("ordinary", "team", api.AgentTypeHermes, true)
	f.converge()
	if err := f.r.ReconcileRuntimeAccess(f.ctx, agent.Name); err != nil {
		t.Fatal(err)
	}
	binding := &rbacv1.RoleBinding{}
	key := client.ObjectKey{Name: cetiAdminAccessName(f.state()) + "-runtime", Namespace: namespace.Name}
	if err := f.kube.Get(f.ctx, key, binding); err != nil {
		t.Fatal(err)
	}
	binding.Subjects = []rbacv1.Subject{{Kind: "ServiceAccount", Name: "foreign", Namespace: "other"}}
	if err := f.kube.Update(f.ctx, binding); err != nil {
		t.Fatal(err)
	}
	before := binding.DeepCopy()
	if err := f.r.ReconcileRuntimeAccess(f.ctx, agent.Name); err == nil {
		t.Fatal("foreign subject was adopted")
	}
	if err := f.kube.Get(f.ctx, key, binding); err != nil || !reflect.DeepEqual(before, binding) {
		t.Fatal("foreign binding was deleted or overwritten", err)
	}
}

func TestCetiAdminAgentPermissionsDoNotRequireRegistration(t *testing.T) {
	f := newCetiAdminFixture(t)
	f.converge()
	state := f.state()
	roles := &rbacv1.ClusterRoleList{}
	if err := f.kube.List(f.ctx, roles); err != nil {
		t.Fatal(err)
	}
	var rules []rbacv1.PolicyRule
	bindings := &rbacv1.ClusterRoleBindingList{}
	if err := f.kube.List(f.ctx, bindings); err != nil {
		t.Fatal(err)
	}
	for _, binding := range bindings.Items {
		for _, subject := range binding.Subjects {
			if subject.Kind != "ServiceAccount" || subject.Name != state.ServiceAccount.Name || subject.Namespace != state.ServiceAccount.Namespace {
				continue
			}
			role := &rbacv1.ClusterRole{}
			if err := f.kube.Get(f.ctx, client.ObjectKey{Name: binding.RoleRef.Name}, role); err != nil {
				t.Fatal(err)
			}
			rules = append(rules, role.Rules...)
		}
	}
	for _, verb := range []string{"get", "list", "watch", "create", "update", "patch"} {
		if !controllerRulesAllow(rules, api.GroupVersion.Group, "agents", verb, "not-previously-registered") {
			t.Fatalf("%s Agent requires registration in an authorization list", verb)
		}
	}
	if controllerRulesAllow(rules, "", "secrets", "get", "platform-provider") {
		t.Fatal("all-Agent management acquired cluster-wide Secret reads")
	}
}

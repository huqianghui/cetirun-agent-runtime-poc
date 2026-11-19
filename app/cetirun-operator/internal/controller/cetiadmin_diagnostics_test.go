// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"errors"
	"reflect"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/cetiadmin"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestCetiAdminDiagnosticNamespaceDoesNotFilterAgent(t *testing.T) {
	f := newCetiAdminFixture(t)
	agent, namespace := f.target("ordinary", "engineering", api.AgentTypeHermes, true)
	f.config.PlatformReadNamespaces = []string{namespace.Name}
	f.updateConfig()
	f.converge()
	if _, _, err := cetiadmin.ResolveRuntimeTarget(f.ctx, f.kube, f.config, f.state().Record, agent.Name, namespace.Name); err != nil {
		t.Fatal("diagnostic reads excluded a valid managed Agent:", err)
	}
	if err := f.r.ReconcileRuntimeAccess(f.ctx, agent.Name); err != nil {
		t.Fatal(err)
	}
	bindings := &rbacv1.RoleBindingList{}
	if err := f.kube.List(f.ctx, bindings, client.InNamespace(namespace.Name)); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, binding := range bindings.Items {
		found = found || binding.Annotations[cetiAdminGrantAgent] == agent.Name
	}
	if !found {
		t.Fatal("diagnostic reads prevented ordinary runtime management access")
	}
}

func TestCetiAdminDiagnosticFailureKeepsRuntime(t *testing.T) {
	for _, failure := range []error{
		apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, "observability"),
		errors.New("synthetic diagnostic lookup unavailable"),
	} {
		t.Run(failure.Error(), func(t *testing.T) {
			f := newCetiAdminFixture(t)
			diagnostic := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "observability"}}
			other := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "other-diagnostics"}}
			f.seed(diagnostic)
			f.seed(other)
			f.config.PlatformReadNamespaces = []string{diagnostic.Name, other.Name}
			f.updateConfig()
			f.converge()
			agent, state := f.agent(), f.state()
			workload := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: instanceServiceName(agent.Name), Namespace: state.Namespace.Name,
				OwnerReferences: []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "Agent", Name: agent.Name, UID: agent.UID}}},
				Spec: appsv1.StatefulSetSpec{Replicas: ptr.To(int32(1))}, Status: appsv1.StatefulSetStatus{ObservedGeneration: 1, Replicas: 1, ReadyReplicas: 1}}
			f.seed(workload)
			f.seed(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "runtime-0", Namespace: state.Namespace.Name}, Status: corev1.PodStatus{Phase: corev1.PodRunning}})
			f.getError = func(key client.ObjectKey, object client.Object) error {
				if _, ok := object.(*corev1.Namespace); ok && key.Name == diagnostic.Name {
					return failure
				}
				return nil
			}
			if err := f.run(); err == nil {
				t.Fatal("diagnostic failure was not reported")
			}
			if err := f.kube.Get(f.ctx, client.ObjectKeyFromObject(workload), workload); err != nil {
				t.Fatal(err)
			}
			if !f.state().CanRun() || ptr.Deref(workload.Spec.Replicas, -1) != 1 || !reflect.DeepEqual(state.Record, f.state().Record) {
				t.Fatal("a local diagnostic failure stopped or rebound the Admin")
			}
			if _, handled, err := f.r.BindingForAgent(f.ctx, f.agent()); err != nil || !handled {
				t.Fatalf("diagnostic failure invalidated runtime eligibility: handled=%v error=%v", handled, err)
			}
			if !controllerRulesAllow(f.targetRole(other.Name).Rules, "", "pods", "get", "any") {
				t.Fatal("one unavailable namespace revoked another diagnostic namespace")
			}
			f.getError = nil
			if err := f.run(); err != nil {
				t.Fatal("diagnostic access did not recover:", err)
			}
			role := f.targetRole(diagnostic.Name)
			if !controllerRulesAllow(role.Rules, "", "pods", "get", "any") || controllerRulesAllow(role.Rules, "", "secrets", "get", "any") {
				t.Fatal("recovered platform diagnostics are not read-only without Secrets")
			}
		})
	}
}

func TestCetiAdminDiagnosticGrantFailureIsLocal(t *testing.T) {
	f := newCetiAdminFixture(t)
	diagnostic := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "observability"}}
	f.seed(diagnostic)
	f.config.PlatformReadNamespaces = []string{diagnostic.Name}
	f.updateConfig()
	f.before = func(operation string, object client.Object) error {
		if _, ok := object.(*rbacv1.RoleBinding); ok && operation == "create" && object.GetNamespace() == diagnostic.Name {
			return errors.New("synthetic diagnostic binding rejected")
		}
		return nil
	}
	var observed error
	for range 32 {
		if observed = f.run(); observed != nil {
			break
		}
	}
	if observed == nil || !f.state().CanRun() {
		t.Fatalf("diagnostic binding failure prevented normal installation: %v", observed)
	}
	if _, handled, err := f.r.BindingForAgent(f.ctx, f.agent()); err != nil || !handled {
		t.Fatalf("diagnostic grant failure invalidated runtime eligibility: handled=%v error=%v", handled, err)
	}
	f.before = nil
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	f.targetRole(diagnostic.Name)
}

func TestCetiAdminDiagnosticConflictPreservesForeignBinding(t *testing.T) {
	for _, changed := range []string{"subject", "owner"} {
		t.Run(changed, func(t *testing.T) {
			f := newCetiAdminFixture(t)
			f.config.PlatformReadNamespaces = []string{f.source.Namespace}
			f.updateConfig()
			f.converge()
			state := f.state()
			binding := &rbacv1.RoleBinding{}
			key := client.ObjectKey{Name: cetiAdminAccessName(state) + "-diagnostics", Namespace: f.source.Namespace}
			if err := f.kube.Get(f.ctx, key, binding); err != nil {
				t.Fatal(err)
			}
			if changed == "subject" {
				binding.Subjects = []rbacv1.Subject{{Kind: "ServiceAccount", Name: "foreign", Namespace: "other"}}
			} else {
				binding.OwnerReferences[0].UID = "foreign-owner-uid"
			}
			if err := f.kube.Update(f.ctx, binding); err != nil {
				t.Fatal(err)
			}
			before := binding.DeepCopy()
			if err := f.run(); err == nil || !f.state().CanRun() {
				t.Fatalf("diagnostic conflict was hidden or stopped the Admin: %v", err)
			}
			if err := f.kube.Get(f.ctx, key, binding); err != nil || !reflect.DeepEqual(before, binding) {
				t.Fatal("diagnostic repair adopted or deleted a foreign subject:", err)
			}
			f.config.Enabled = false
			f.updateConfig()
			if err := f.run(); err == nil || f.state().CanRun() {
				t.Fatalf("disable ignored the conflict or retained runtime eligibility: %v", err)
			}
			if err := f.kube.Get(f.ctx, key, binding); err != nil || !reflect.DeepEqual(before, binding) {
				t.Fatal("diagnostic withdrawal deleted a foreign subject:", err)
			}
		})
	}
}

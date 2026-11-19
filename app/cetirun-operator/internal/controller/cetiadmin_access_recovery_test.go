// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"reflect"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/cetiadmin"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func seedCetiAdminExecPolicy(f *cetiAdminFixture, agent *api.Agent, namespace *corev1.Namespace, complete bool) {
	name := instanceServiceName(agent.Name) + "-" + namespace.Name + "-deny-exec"
	metadata := metav1.ObjectMeta{Name: name, Labels: map[string]string{"app.kubernetes.io/managed-by": "Helm"}, Annotations: map[string]string{
		"meta.helm.sh/release-name": helmReleaseName(agent.Name), "meta.helm.sh/release-namespace": namespace.Name,
	}}
	f.seed(&admissionv1.ValidatingAdmissionPolicy{ObjectMeta: metadata, Spec: admissionv1.ValidatingAdmissionPolicySpec{
		FailurePolicy: ptr.To(admissionv1.Fail), MatchConstraints: &admissionv1.MatchResources{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": namespace.Name}},
			ResourceRules: []admissionv1.NamedRuleWithOperations{{RuleWithOperations: admissionv1.RuleWithOperations{
				Operations: []admissionv1.OperationType{admissionv1.Connect}, Rule: admissionv1.Rule{APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"pods/exec"}},
			}}},
		}, Validations: []admissionv1.Validation{{Expression: "false", Message: "kubectl exec is not allowed on openclaw pods"}},
	}})
	if complete {
		f.seed(&admissionv1.ValidatingAdmissionPolicyBinding{ObjectMeta: metadata, Spec: admissionv1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName: name, ValidationActions: []admissionv1.ValidationAction{admissionv1.Deny},
		}})
	}
}

func TestCetiAdminAccessErrorDoesNotBlockOrdinarySuspension(t *testing.T) {
	for _, complete := range []bool{true, false} {
		name := "complete-policy"
		if !complete {
			name = "missing-policy-binding"
		}
		t.Run(name, func(t *testing.T) {
			f := newCetiAdminFixture(t)
			agent, namespace := f.target("ordinary", "engineering", api.AgentTypeHermes, true)
			f.converge()
			seedCetiAdminExecPolicy(f, agent, namespace, complete)
			agent.Spec.Suspended = true
			if err := f.kube.Update(f.ctx, agent); err != nil {
				t.Fatal(err)
			}
			workload := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: instanceServiceName(agent.Name), Namespace: namespace.Name,
				Annotations: map[string]string{"meta.helm.sh/release-name": helmReleaseName(agent.Name), "meta.helm.sh/release-namespace": namespace.Name}},
				Spec: appsv1.StatefulSetSpec{Replicas: ptr.To(int32(1))}}
			f.seed(workload)
			f.r.Runtime.CetiAdmin = f.r
			for range 3 {
				_, err := f.r.Runtime.Reconcile(f.ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: agent.Name}})
				t.Logf("reconcile: %v", err)
			}
			current := &appsv1.StatefulSet{}
			if err := f.kube.Get(f.ctx, client.ObjectKeyFromObject(workload), current); err != nil {
				t.Fatal(err)
			}
			if ptr.Deref(current.Spec.Replicas, -1) != 0 {
				t.Fatal("CetiAdmin access repair prevented an ordinary runtime from stopping")
			}
		})
	}
}

func TestCetiAdminRecordedWithdrawalPreservesForeignSubject(t *testing.T) {
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
	// Retained records from the previous layout contain diagnostic bindings.
	state.Resources = append(state.Resources, cetiadmin.ManagedResource{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "RoleBinding",
		ObjectIdentity: identityOf(binding), CreationID: binding.Annotations[cetiadmin.AnnotationCreationID]})
	binding.Subjects = []rbacv1.Subject{{Kind: "ServiceAccount", Name: "foreign", Namespace: "other"}}
	if err := f.kube.Update(f.ctx, binding); err != nil {
		t.Fatal(err)
	}
	before := binding.DeepCopy()
	if err := f.r.RevokeInstallationAccess(f.ctx, state); err == nil {
		t.Error("withdrawal did not report the conflicting subject")
	}
	if err := f.kube.Get(f.ctx, key, binding); err != nil || !reflect.DeepEqual(before, binding) {
		t.Fatal("recorded withdrawal deleted or changed a foreign subject", err)
	}
}

func TestCetiAdminMissingCheckpointRevokesDynamicReaders(t *testing.T) {
	f := newCetiAdminFixture(t)
	f.config.PlatformReadNamespaces = []string{f.source.Namespace}
	f.updateConfig()
	agent, _ := f.target("ordinary", "engineering", api.AgentTypeHermes, true)
	f.converge()
	if err := f.r.ReconcileRuntimeAccess(f.ctx, agent.Name); err != nil {
		t.Fatal(err)
	}
	checkpoint, state, err := f.r.readState(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.kube.Delete(f.ctx, checkpoint, client.Preconditions{UID: &checkpoint.UID, ResourceVersion: &checkpoint.ResourceVersion}); err != nil {
		t.Fatal(err)
	}
	t.Logf("installer recovery: %v", f.run())
	t.Logf("ordinary access recovery: %v", f.r.ReconcileRuntimeAccess(f.ctx, agent.Name))
	bindings := &rbacv1.RoleBindingList{}
	if err := f.kube.List(f.ctx, bindings, client.InNamespace(f.source.Namespace)); err != nil {
		t.Fatal(err)
	}
	for _, binding := range bindings.Items {
		if binding.Name == cetiAdminAccessName(state)+"-diagnostics" {
			t.Fatal("missing-checkpoint recovery left platform diagnostics access")
		}
		if binding.Annotations[cetiAdminGrantAgent] != agent.Name || binding.RoleRef.Kind != "Role" {
			continue
		}
		role := &rbacv1.Role{}
		if err := f.kube.Get(f.ctx, client.ObjectKey{Name: binding.RoleRef.Name, Namespace: binding.Namespace}, role); err != nil {
			t.Fatal(err)
		}
		if controllerRulesAllow(role.Rules, "", "configmaps", "get", f.r.Runtime.maintenanceKey(agent).Name) {
			t.Fatal("missing-checkpoint recovery left effective admission reader access")
		}
	}
}

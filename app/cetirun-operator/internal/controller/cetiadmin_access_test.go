// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/cetiadmin"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/internal/registryauth"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (f *cetiAdminFixture) target(name, teamName string, kind api.AgentType, owned bool) (*api.Agent, *corev1.Namespace) {
	f.t.Helper()
	if teamName != "" {
		team := &api.Team{}
		if err := f.kube.Get(f.ctx, client.ObjectKey{Name: teamName}, team); apierrors.IsNotFound(err) {
			f.seed(&api.Team{ObjectMeta: metav1.ObjectMeta{Name: teamName}})
		} else if err != nil {
			f.t.Fatal(err)
		}
	}
	agent := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: api.AgentSpec{Agent: api.HarnessSpec{Type: kind}, Team: teamName, User: api.UserSpec{Alias: "target-owner"}, GatewayTokenSecretRef: "native-auth"}}
	f.seed(agent)
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: instanceNamespace(name), Labels: map[string]string{"app.kubernetes.io/managed-by": "cetirun-operator"}}}
	if owned {
		namespace.OwnerReferences = []metav1.OwnerReference{registryauth.Owner(agent)}
	}
	f.seed(namespace)
	return agent, namespace
}

func (f *cetiAdminFixture) customReceipt(agent *api.Agent, namespace *corev1.Namespace) {
	f.t.Helper()
	key := f.r.Runtime.ownershipKey(agent)
	receipt := customOwnership{AgentName: agent.Name, AgentUID: agent.UID, Namespace: namespace.Name, NamespaceUID: namespace.UID, Release: helmReleaseName(agent.Name), Home: "custom-home",
		HomeClaim: &runtimebindings.HomeClaim{StorageClass: "test-storage", StorageSize: "10Gi", AccessMode: "ReadWriteOnce"}}
	raw, _ := json.Marshal(receipt)
	f.seed(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, OwnerReferences: []metav1.OwnerReference{registryauth.Owner(agent)}}, Immutable: ptr.To(true), Data: map[string]string{"ownership": string(raw)}})
}

func controllerRulesAllow(rules []rbacv1.PolicyRule, group, resource, verb, name string) bool {
	for _, rule := range rules {
		if slices.Contains(rule.APIGroups, group) && slices.Contains(rule.Resources, resource) && slices.Contains(rule.Verbs, verb) && (len(rule.ResourceNames) == 0 || slices.Contains(rule.ResourceNames, name)) {
			return true
		}
	}
	return false
}

func (f *cetiAdminFixture) targetRole(namespace string) *rbacv1.Role {
	f.t.Helper()
	state := f.state()
	bindings := &rbacv1.RoleBindingList{}
	if err := f.kube.List(f.ctx, bindings, client.InNamespace(namespace)); err != nil {
		f.t.Fatal(err)
	}
	for _, binding := range bindings.Items {
		if !slices.Contains(binding.Subjects, rbacv1.Subject{Kind: "ServiceAccount", Name: state.ServiceAccount.Name, Namespace: state.ServiceAccount.Namespace}) {
			continue
		}
		if binding.RoleRef.Kind == "Role" {
			role := &rbacv1.Role{}
			if err := f.kube.Get(f.ctx, client.ObjectKey{Name: binding.RoleRef.Name, Namespace: namespace}, role); err != nil {
				f.t.Fatal(err)
			}
			return role
		}
		role := &rbacv1.ClusterRole{}
		if err := f.kube.Get(f.ctx, client.ObjectKey{Name: binding.RoleRef.Name}, role); err != nil {
			f.t.Fatal(err)
		}
		return &rbacv1.Role{Rules: role.Rules}
	}
	f.t.Fatalf("no managed role in %s", namespace)
	return nil
}

func TestCetiAdminNativeCustomAccessUsesNamespaceOwnership(t *testing.T) {
	for _, retainedStatus := range []bool{false, true} {
		name := "sandbox-reference"
		if retainedStatus {
			name = "retained-sandbox-status"
		}
		t.Run(name, func(t *testing.T) {
			f := newCetiAdminFixture(t)
			agent, namespace := f.target("native-custom", "engineering", api.AgentTypeCustom, true)
			if retainedStatus {
				agent.Status.Sandbox = &api.SandboxStatus{
					TeamUID: "historical-team", SandboxUID: "historical-sandbox", AgentPolicyUID: "historical-policy",
					AppliedRevision: "historical-revision",
				}
				if err := f.kube.Status().Update(f.ctx, agent); err != nil {
					t.Fatal(err)
				}
			} else {
				agent.Spec.SandboxRef = "native-sandbox"
				if err := f.kube.Update(f.ctx, agent); err != nil {
					t.Fatal(err)
				}
			}
			namespace.Labels[sandboxworkload.ProtectedNamespaceLabel] = "true"
			namespace.OwnerReferences = []metav1.OwnerReference{sandboxworkload.AgentOwner(agent.Name, agent.UID)}
			namespace.Annotations = map[string]string{
				sandboxworkload.AgentUIDAnnotation:  string(agent.UID),
				sandboxworkload.AgentNameAnnotation: agent.Name,
			}
			if err := f.kube.Update(f.ctx, namespace); err != nil {
				t.Fatal(err)
			}
			f.converge()
			if err := f.r.ReconcileRuntimeAccess(f.ctx, agent.Name); err != nil {
				t.Fatal("native namespace required a legacy Custom receipt:", err)
			}
			role := f.targetRole(namespace.Name)
			if !controllerRulesAllow(role.Rules, "", "pods", "get", "") ||
				controllerRulesAllow(role.Rules, "", "pods/exec", "create", "") {
				t.Fatal("native Custom access is not protected namespace access")
			}
			for _, verb := range []string{"get", "list", "watch"} {
				if controllerRulesAllow(role.Rules, "", "secrets", verb, "") {
					t.Errorf("native Custom access grants Secret %s", verb)
				}
			}
			receipt := &corev1.ConfigMap{}
			if err := f.kube.Get(f.ctx, f.r.Runtime.ownershipKey(agent), receipt); !apierrors.IsNotFound(err) {
				t.Fatal("native access fabricated a legacy ownership receipt:", err)
			}

			for _, mutation := range []string{"owner", "owner-controller", "extra-owner", "agent-uid", "agent-name", "protection"} {
				t.Run(mutation, func(t *testing.T) {
					changed := namespace.DeepCopy()
					if err := f.kube.Get(f.ctx, client.ObjectKeyFromObject(namespace), changed); err != nil {
						t.Fatal(err)
					}
					switch mutation {
					case "owner":
						changed.OwnerReferences[0].UID = "foreign-agent"
					case "owner-controller":
						changed.OwnerReferences[0].Controller = ptr.To(false)
					case "extra-owner":
						changed.OwnerReferences = append(changed.OwnerReferences, metav1.OwnerReference{APIVersion: "v1", Kind: "ConfigMap", Name: "foreign", UID: "foreign"})
					case "agent-uid":
						changed.Annotations[sandboxworkload.AgentUIDAnnotation] = "foreign-agent"
					case "agent-name":
						changed.Annotations[sandboxworkload.AgentNameAnnotation] = "foreign-agent"
					case "protection":
						delete(changed.Labels, sandboxworkload.ProtectedNamespaceLabel)
					}
					if err := f.kube.Update(f.ctx, changed); err != nil {
						t.Fatal(err)
					}
					if err := f.r.ReconcileRuntimeAccess(f.ctx, agent.Name); err == nil {
						t.Fatal("changed native ownership was accepted")
					}
					bindings := &rbacv1.RoleBindingList{}
					if err := f.kube.List(f.ctx, bindings, client.InNamespace(namespace.Name)); err != nil {
						t.Fatal(err)
					}
					for _, binding := range bindings.Items {
						if binding.Annotations[cetiAdminGrantAgent] == agent.Name {
							t.Fatal("changed ownership retained namespace access")
						}
					}
					if err := f.kube.Get(f.ctx, client.ObjectKeyFromObject(namespace), changed); err != nil {
						t.Fatal(err)
					}
					changed.OwnerReferences = namespace.DeepCopy().OwnerReferences
					changed.Labels = namespace.DeepCopy().Labels
					changed.Annotations = namespace.DeepCopy().Annotations
					if err := f.kube.Update(f.ctx, changed); err != nil {
						t.Fatal(err)
					}
					if err := f.r.ReconcileRuntimeAccess(f.ctx, agent.Name); err != nil {
						t.Fatal("restored native ownership did not recover access:", err)
					}
				})
			}
		})
	}
}

func TestCetiAdminAccessFollowsLiveAgentsAndExclusions(t *testing.T) {
	f := newCetiAdminFixture(t)
	f.target("ordinary", "selected-team", api.AgentTypeOpenClaw, true)
	f.target("unowned", "selected-team", api.AgentTypeHermes, false)
	f.target("other-team", "unselected-team", api.AgentTypeHermes, true)
	custom, customNS := f.target("custom", "selected-team", api.AgentTypeCustom, false)
	f.customReceipt(custom, customNS)
	f.target("custom-unbound", "selected-team", api.AgentTypeCustom, true)
	protected, _ := f.target("protected", "selected-team", api.AgentTypeHermes, true)
	protected.Spec.SandboxRef = "protected-sandbox"
	if err := f.kube.Update(f.ctx, protected); err != nil {
		t.Fatal(err)
	}
	platformNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "platform-observability"}}
	f.seed(platformNS)
	_, registryNS := f.target("registry-source", "selected-team", api.AgentTypeHermes, true)
	_, brokerNS := f.target("broker-source", "selected-team", api.AgentTypeHermes, true)
	migration, migrationNS := f.target("previous-admin", "selected-team", api.AgentTypeOpenClaw, true)
	migration.Spec.Suspended = true
	if err := f.kube.Update(f.ctx, migration); err != nil {
		t.Fatal(err)
	}
	migration.Status.Phase = api.PhaseSuspended
	if err := f.kube.Status().Update(f.ctx, migration); err != nil {
		t.Fatal(err)
	}
	f.config.MigrationSource = &cetiadmin.MigrationSource{Agent: identityOf(migration), Namespace: identityOf(migrationNS)}
	f.config.PlatformReadNamespaces = []string{platformNS.Name}
	f.r.Runtime.RegistryCredentialNamespace, f.r.Runtime.ConnectionsMCPBrokerNamespace = registryNS.Name, brokerNS.Name
	f.updateConfig()
	f.converge()
	state := f.state()
	for _, name := range []string{"custom", "ordinary", "protected", "other-team"} {
		if err := f.r.ReconcileRuntimeAccess(f.ctx, name); err != nil {
			t.Fatalf("current Agent %s did not obtain standard namespace access: %v", name, err)
		}
	}
	for _, name := range []string{"unowned", "custom-unbound", "registry-source", "broker-source", "previous-admin"} {
		_ = f.r.ReconcileRuntimeAccess(f.ctx, name)
		bindings := &rbacv1.RoleBindingList{}
		if err := f.kube.List(f.ctx, bindings, client.InNamespace(instanceNamespace(name))); err != nil {
			t.Fatal(err)
		}
		for _, binding := range bindings.Items {
			if binding.Annotations[cetiAdminGrantAgent] == name {
				t.Fatalf("excluded namespace %s received management permissions", name)
			}
		}
	}
	observedCustom := &corev1.Namespace{}
	if err := f.kube.Get(f.ctx, client.ObjectKey{Name: customNS.Name}, observedCustom); err != nil {
		t.Fatal(err)
	}
	if len(observedCustom.OwnerReferences) != 0 {
		t.Fatal("Custom namespace was adopted by adding an owner")
	}
	role := f.targetRole(instanceNamespace("ordinary"))
	if !controllerRulesAllow(role.Rules, "", "pods/exec", "create", "pod") || !controllerRulesAllow(role.Rules, "", "secrets", "list", "") {
		t.Fatal("ordinary native operations were dropped")
	}
	if !controllerRulesAllow(f.targetRole(instanceNamespace("other-team")).Rules, "", "pods/exec", "create", "pod") {
		t.Fatal("another Team was treated as an authorization selection")
	}
	role = f.targetRole(instanceNamespace("protected"))
	if controllerRulesAllow(role.Rules, "", "pods/exec", "create", "pod") || controllerRulesAllow(role.Rules, "", "pods", "delete", "pod") {
		t.Fatal("protected Sandbox acquired exec or Pod deletion")
	}
	for _, verb := range []string{"get", "list", "watch"} {
		if controllerRulesAllow(role.Rules, "", "secrets", verb, "") {
			t.Errorf("protected Sandbox acquired Secret %s", verb)
		}
	}
	role = f.targetRole(platformNS.Name)
	if _, _, err := cetiadmin.ResolveRuntimeTarget(f.ctx, f.kube, f.config, state.Record, "", platformNS.Name); err == nil {
		t.Fatal("non-Agent platform namespace became a management target")
	}
	for _, resource := range []string{"secrets", "pods/exec", "serviceaccounts/token"} {
		for _, verb := range []string{"get", "list", "create", "patch", "delete"} {
			if controllerRulesAllow(role.Rules, "", resource, verb, "anything") {
				t.Fatalf("platform role acquired %s %s", verb, resource)
			}
		}
	}
	document := &corev1.ConfigMap{}
	if err := f.kube.Get(f.ctx, client.ObjectKey{Namespace: state.Namespace.Name, Name: "cetiadmin-targets"}, document); !apierrors.IsNotFound(err) {
		t.Fatal("an authorization inventory was published", err)
	}
	if !reflect.DeepEqual(state, f.state()) {
		t.Fatal("namespace permissions changed the Admin installation checkpoint")
	}
}

func TestCetiAdminAdmissionReadsOnlyTargetMaintenanceRecords(t *testing.T) {
	f := newCetiAdminFixture(t)
	target, _ := f.target("maintenance-target", "team", api.AgentTypeHermes, true)
	protected, _ := f.target("protected-target", "team", api.AgentTypeHermes, true)
	protected.Spec.SandboxRef = "protected-sandbox"
	if err := f.kube.Update(f.ctx, protected); err != nil {
		t.Fatal(err)
	}
	f.converge()
	for _, agent := range []*api.Agent{target, protected} {
		if err := f.r.ReconcileRuntimeAccess(f.ctx, agent.Name); err != nil {
			t.Fatal(err)
		}
	}
	state := f.state()
	bindings := &rbacv1.RoleBindingList{}
	if err := f.kube.List(f.ctx, bindings, client.InNamespace(f.r.Runtime.OperatorNamespace)); err != nil {
		t.Fatal(err)
	}
	var rules []rbacv1.PolicyRule
	for _, binding := range bindings.Items {
		if !slices.Contains(binding.Subjects, rbacv1.Subject{Kind: "ServiceAccount", Name: "cetirun-sandbox-admission", Namespace: state.Source.Namespace}) || binding.RoleRef.Kind != "Role" {
			continue
		}
		role := &rbacv1.Role{}
		if err := f.kube.Get(f.ctx, client.ObjectKey{Name: binding.RoleRef.Name, Namespace: binding.Namespace}, role); err != nil {
			t.Fatal(err)
		}
		rules = append(rules, role.Rules...)
	}
	key := f.r.Runtime.maintenanceKey(target)
	if !controllerRulesAllow(rules, "", "configmaps", "get", key.Name) {
		t.Fatal("admission cannot read the exact bound target maintenance checkpoint")
	}
	for _, name := range []string{"unrelated-config", f.r.Runtime.maintenanceKey(protected).Name} {
		if controllerRulesAllow(rules, "", "configmaps", "get", name) {
			t.Fatalf("admission acquired unrelated checkpoint access: %s", name)
		}
	}
	for _, verb := range []string{"list", "watch", "create", "update", "patch", "delete"} {
		if controllerRulesAllow(rules, "", "configmaps", verb, key.Name) {
			t.Fatalf("admission acquired %s instead of exact read-only access", verb)
		}
	}
}

func TestCetiAdminAccessRequiresCurrentCustomReceipt(t *testing.T) {
	f := newCetiAdminFixture(t)
	agent, namespace := f.target("custom", "team", api.AgentTypeCustom, false)
	f.customReceipt(agent, namespace)
	f.converge()
	if err := f.r.ReconcileRuntimeAccess(f.ctx, agent.Name); err != nil {
		t.Fatal(err)
	}
	record := &corev1.ConfigMap{}
	if err := f.kube.Get(f.ctx, f.r.Runtime.ownershipKey(agent), record); err != nil {
		t.Fatal(err)
	}
	var receipt customOwnership
	_ = json.Unmarshal([]byte(record.Data["ownership"]), &receipt)
	receipt.NamespaceUID = types.UID("different-namespace")
	raw, _ := json.Marshal(receipt)
	record.Data["ownership"] = string(raw)
	if err := f.kube.Update(f.ctx, record); err != nil {
		t.Fatal(err)
	}
	if err := f.r.ReconcileRuntimeAccess(f.ctx, agent.Name); err == nil {
		t.Fatal("changed Custom receipt was treated as current")
	}
	if _, handled, err := f.r.BindingForAgent(f.ctx, f.agent()); err != nil || !handled {
		t.Fatal("one Custom ownership failure invalidated the Admin runtime", err)
	}
	bindings := &rbacv1.RoleBindingList{}
	if err := f.kube.List(f.ctx, bindings, client.InNamespace(namespace.Name)); err != nil || len(bindings.Items) != 0 {
		t.Fatal("invalid Custom ownership retained namespace access", err)
	}
	current := &corev1.Namespace{}
	if err := f.kube.Get(f.ctx, client.ObjectKey{Name: namespace.Name}, current); err != nil {
		t.Fatal(err)
	}
	if len(current.OwnerReferences) != 0 {
		t.Fatal("receipt failure adopted Custom namespace")
	}
}

func TestCetiAdminTeamChangeDoesNotRegrantInstallation(t *testing.T) {
	f := newCetiAdminFixture(t)
	agent, _ := f.target("target", "first-team", api.AgentTypeHermes, true)
	f.converge()
	if err := f.r.ReconcileRuntimeAccess(f.ctx, agent.Name); err != nil {
		t.Fatal(err)
	}
	before := f.state()
	f.seed(&api.Team{ObjectMeta: metav1.ObjectMeta{Name: "second-team"}})
	agent.Spec.Team = "second-team"
	agent.Generation++
	if err := f.kube.Update(f.ctx, agent); err != nil {
		t.Fatal(err)
	}
	changedBindings := 0
	f.before = func(operation string, object client.Object) error {
		switch object.(type) {
		case *rbacv1.RoleBinding, *rbacv1.ClusterRoleBinding:
			changedBindings++
		default:
		}
		return nil
	}
	if err := f.r.ReconcileRuntimeAccess(f.ctx, agent.Name); err != nil {
		t.Fatal(err)
	}
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	if changedBindings != 0 || !reflect.DeepEqual(before, f.state()) {
		t.Fatal("ordinary Team membership changed Admin installation authority")
	}
}

func TestCetiAdminAccessWithdrawsForExcludedTargetNamespace(t *testing.T) {
	for key, value := range map[string]string{"control-plane": "true", "kubernetes.azure.com/managedby": "aks"} {
		t.Run(key, func(t *testing.T) {
			f := newCetiAdminFixture(t)
			agent, namespace := f.target("excluded", "engineering", api.AgentTypeHermes, true)
			f.converge()
			if err := f.r.ReconcileRuntimeAccess(f.ctx, agent.Name); err != nil {
				t.Fatal(err)
			}
			before := f.state()
			namespace.Labels[key] = value
			if err := f.kube.Update(f.ctx, namespace); err != nil {
				t.Fatal(err)
			}
			if err := f.r.ReconcileRuntimeAccess(f.ctx, agent.Name); err != nil {
				t.Fatal(err)
			}
			bindings := &rbacv1.RoleBindingList{}
			if err := f.kube.List(f.ctx, bindings, client.InNamespace(namespace.Name)); err != nil || len(bindings.Items) != 0 {
				t.Fatal("excluded namespace kept management permissions", err)
			}
			if !reflect.DeepEqual(before, f.state()) {
				t.Fatal("one excluded namespace changed the global installation")
			}
			current := &corev1.Namespace{}
			if err := f.kube.Get(f.ctx, client.ObjectKeyFromObject(namespace), current); err != nil || current.Labels[key] != value {
				t.Fatal("controller rewrote an excluded namespace instead of withholding authority", err)
			}
		})
	}
}

func TestCetiAdminAccessNeverOverwritesOrDeletesForeignBindings(t *testing.T) {
	f := newCetiAdminFixture(t)
	f.converge()
	state := f.state()
	var ref cetiadmin.ManagedResource
	for _, candidate := range state.Resources {
		if candidate.Kind == "ClusterRoleBinding" {
			ref = candidate
			break
		}
	}
	foreign := &rbacv1.ClusterRoleBinding{}
	if err := f.kube.Get(f.ctx, client.ObjectKey{Name: ref.Name}, foreign); err != nil {
		t.Fatal(err)
	}
	foreign.Annotations[cetiadmin.AnnotationCreationID] = "someone-elses-binding"
	foreign.Subjects = []rbacv1.Subject{{Kind: "ServiceAccount", Name: "foreign", Namespace: "other"}}
	if err := f.kube.Update(f.ctx, foreign); err != nil {
		t.Fatal(err)
	}
	before := foreign.DeepCopy()
	if err := f.run(); err == nil {
		t.Fatal("foreign binding was accepted")
	}
	if err := f.kube.Get(f.ctx, client.ObjectKeyFromObject(foreign), foreign); err != nil {
		t.Fatal("foreign binding was deleted")
	}
	if !reflect.DeepEqual(before, foreign) {
		t.Fatal("foreign binding was overwritten")
	}
	if _, handled, err := f.r.BindingForAgent(f.ctx, f.agent()); !handled || err == nil {
		t.Fatal("binding conflict did not hold the exact managed Agent")
	}
}

func TestCetiAdminAccessWithdrawsPartialGrantAndRetries(t *testing.T) {
	for _, failKind := range []string{"ClusterRole", "ClusterRoleBinding"} {
		t.Run(failKind, func(t *testing.T) {
			f := newCetiAdminFixture(t)
			f.config.PlatformReadNamespaces = []string{f.source.Namespace}
			f.updateConfig()
			failed := false
			f.before = func(operation string, object client.Object) error {
				if operation == "create" && object.GetObjectKind().GroupVersionKind().Kind == failKind && !failed {
					failed = true
					return errors.New("synthetic rejected RBAC write")
				}
				return nil
			}
			var err error
			for range 15 {
				if err = f.run(); err != nil {
					break
				}
			}
			if err == nil || !failed {
				t.Fatal("RBAC write fault did not execute")
			}
			f.noBindings()
			f.before = nil
			f.converge()
		})
	}
}

type cetiAdminDiscoveryClient struct {
	client.Client
	mapper meta.RESTMapper
}

func (c cetiAdminDiscoveryClient) RESTMapper() meta.RESTMapper { return c.mapper }

type cetiAdminDiscoveryFailure struct {
	meta.RESTMapper
	group string
	err   error
}

func (m cetiAdminDiscoveryFailure) RESTMapping(kind schema.GroupKind, versions ...string) (*meta.RESTMapping, error) {
	if kind.Group == m.group {
		return nil, m.err
	}
	return m.RESTMapper.RESTMapping(kind, versions...)
}

func TestCetiAdminAccessDistinguishesOptionalAbsenceFromDiscoveryFailure(t *testing.T) {
	for _, failure := range []string{"not-installed", "forbidden", "required-missing"} {
		t.Run(failure, func(t *testing.T) {
			f := newCetiAdminFixture(t)
			f.converge()
			group := "acn.azure.com"
			var failureErr error = &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: group, Kind: "NodeNetworkConfig"}}
			if failure == "forbidden" {
				failureErr = apierrors.NewForbidden(schema.GroupResource{Group: group, Resource: "nodenetworkconfigs"}, "", errors.New("synthetic discovery denial"))
			}
			if failure == "required-missing" {
				group = api.GroupVersion.Group
			}
			f.r.Client = cetiAdminDiscoveryClient{Client: f.kube, mapper: cetiAdminDiscoveryFailure{RESTMapper: cetiAdminTestMapper(), group: group, err: failureErr}}
			if failure != "not-installed" {
				err := f.run()
				if err == nil {
					t.Fatal("discovery failure was hidden as an absent optional API")
				}
				f.noBindings()
				return
			}
			cluster, namespaced, err := f.r.diagnosticRules()
			if err != nil {
				t.Fatal(err)
			}
			for _, rules := range [][]rbacv1.PolicyRule{cluster, namespaced} {
				for _, rule := range rules {
					if slices.Contains(rule.APIGroups, group) {
						t.Fatal("absent optional API received diagnostic permissions")
					}
				}
			}
			if !controllerRulesAllow(cluster, api.GroupVersion.Group, "agents", "list", "") ||
				!controllerRulesAllow(namespaced, "", "pods", "get", "") {
				t.Fatal("absent optional API removed unrelated diagnostic permissions")
			}
		})
	}
}

func TestCetiAdminAccessUsesDiscoveredResourceNameForAllowedKind(t *testing.T) {
	f := newCetiAdminFixture(t)
	mapper := cetiAdminTestMapper()
	group := schema.GroupVersion{Group: "acn.azure.com", Version: "v1"}
	mapper.AddSpecific(group.WithKind("NodeInfo"), group.WithResource("nodeinfo"), group.WithResource("nodeinfo"), meta.RESTScopeNamespace)
	f.r.Client = cetiAdminDiscoveryClient{Client: f.kube, mapper: mapper}
	_, rules, err := f.r.diagnosticRules()
	if err != nil {
		t.Fatal(err)
	}
	if !controllerRulesAllow(rules, group.Group, "nodeinfo", "get", "node") {
		t.Fatal("allowed diagnostic kind was lost to guessed pluralization")
	}
	if controllerRulesAllow(rules, group.Group, "nodeinfo", "patch", "node") {
		t.Fatal("discovery expanded beyond diagnostic reads")
	}
	if controllerRulesAllow(rules, group.Group, "nodeinfos", "get", "node") {
		t.Fatal("diagnostic permissions included a guessed resource name")
	}
}

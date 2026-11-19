// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/cetiadmin"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/internal/registryauth"
	"github.com/Azure/CetiRun/app/internal/terminalpolicy"
	"github.com/google/cel-go/cel"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const execUsername = "system:serviceaccount:cetirun-cetiadmin:openclaw-cetiadmin-cetirun"
const execCondition = `request.userInfo.username != "system:serviceaccount:cetirun-cetiadmin:openclaw-cetiadmin-cetirun" || request.userInfo.uid != "admin-account-uid"`

type execFixture struct {
	t           *testing.T
	ctx         context.Context
	r           *CetiAdminReconciler
	kube        client.WithWatch
	binding     cetiadmin.Binding
	target      cetiadmin.TargetIdentity
	source      *corev1.ConfigMap
	attempts    []string
	beforePatch func(client.Object) error
	afterPatch  func(client.Object) error
	readError   func(client.Object) error
}

func newExecFixture(t *testing.T, kind api.AgentType) *execFixture {
	t.Helper()
	f := &execFixture{t: t, ctx: context.Background()}
	config := cetiadmin.DefaultConfig()
	config.Owner = cetiadmin.Owner{Alias: "platform-owner", TenantID: "11111111-1111-1111-1111-111111111111", ObjectID: "22222222-2222-2222-2222-222222222222"}
	config.Model.InitialMonthlyBudgetUSD = "10"
	config.Kubernetes.APIAudience, config.Kubernetes.APIEgressCIDRs = "api://unit-cluster", []string{"192.0.2.10/32"}
	metadata := func(name, namespace, uid string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: name, Namespace: namespace, UID: types.UID(uid), Annotations: map[string]string{
			cetiadmin.AnnotationSourceUID: "source-uid", cetiadmin.AnnotationInstallationUID: "record-uid",
		}}
	}
	admin := &api.Agent{ObjectMeta: metadata("cetiadmin", "", "admin-uid"), Spec: api.AgentSpec{
		Agent: api.HarnessSpec{Type: api.AgentTypeHermes, Image: ptr.To("registry.example.com/hermes:test"), Port: ptr.To(int32(8642))},
		User:  api.UserSpec{Alias: config.Owner.Alias}, Team: "cetiadmin", Model: config.Model.Default,
		ModelAccess: &api.ModelAccessSpec{SharedModels: ptr.To(true), GitHubCopilot: ptr.To(false)},
		Helm:        api.HelmSpec{Version: "0.1.1", Values: map[string]runtime.RawExtension{"skills": {Raw: []byte(`{"shared":false}`)}}},
	}}
	admin.Annotations[api.AnnotationOwnerTenantID], admin.Annotations[api.AnnotationOwnerObjectID] = config.Owner.TenantID, config.Owner.ObjectID
	team := &api.Team{ObjectMeta: metadata("cetiadmin", "", "admin-team-uid")}
	namespace := &corev1.Namespace{ObjectMeta: metadata("cetirun-cetiadmin", "", "admin-namespace-uid")}
	namespace.OwnerReferences = []metav1.OwnerReference{registryauth.Owner(admin)}
	account := &corev1.ServiceAccount{ObjectMeta: metadata("openclaw-cetiadmin-cetirun", namespace.Name, "admin-account-uid"), AutomountServiceAccountToken: ptr.To(false)}
	account.OwnerReferences = []metav1.OwnerReference{registryauth.Owner(admin)}
	rawConfig, _ := json.Marshal(config)
	f.source = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "operations", Namespace: "platform-test", UID: "source-uid"}, Data: map[string]string{cetiadmin.ConfigKey: string(rawConfig)}}
	hash, err := cetiadmin.ConfigHash(config)
	if err != nil {
		t.Fatal(err)
	}
	record := cetiadmin.Record{SchemaVersion: 1, ConfigHash: hash, Source: identityOf(f.source), Agent: identityOf(admin), Team: identityOf(team), Namespace: identityOf(namespace), ServiceAccount: identityOf(account)}
	f.binding, err = cetiadmin.ResolveBinding(config, &record, cetiadmin.ObservedInstallation{Source: f.source, Agent: admin, Team: team, Namespace: namespace, ServiceAccount: account})
	if err != nil {
		t.Fatal(err)
	}
	target := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: "example", UID: "target-uid", Annotations: map[string]string{"example.com/keep": "target-data"}}, Spec: api.AgentSpec{Agent: api.HarnessSpec{Type: kind}}}
	targetNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "cetirun-example", UID: "target-namespace-uid", Annotations: map[string]string{customAgentUID: string(target.UID)}}}
	if kind != api.AgentTypeCustom {
		targetNS.OwnerReferences = []metav1.OwnerReference{registryauth.Owner(target)}
	}
	workload := "openclaw-example-cetirun"
	policyName := workload + "-cetirun-example-deny-exec"
	policyMeta := func(uid string) metav1.ObjectMeta {
		meta := metav1.ObjectMeta{Name: policyName, UID: types.UID(uid), Labels: map[string]string{"app.kubernetes.io/managed-by": "Helm"}, Annotations: map[string]string{
			"meta.helm.sh/release-name": "openclaw-example", "meta.helm.sh/release-namespace": "cetirun-example", "example.com/keep": "policy-data",
		}}
		if kind == api.AgentTypeCustom {
			meta.OwnerReferences = []metav1.OwnerReference{registryauth.Owner(target)}
		}
		return meta
	}
	expression := "false"
	if kind == api.AgentTypeCustom {
		expression = terminalpolicy.AdmissionExpression(targetNS.Name, workload+"-0", "cetirun", terminalpolicy.WorkloadName(workload), string(target.UID))
	}
	policy := &admissionv1.ValidatingAdmissionPolicy{ObjectMeta: policyMeta("policy-uid"), Spec: admissionv1.ValidatingAdmissionPolicySpec{
		FailurePolicy: ptr.To(admissionv1.Fail), MatchConstraints: &admissionv1.MatchResources{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": targetNS.Name}},
			ResourceRules: []admissionv1.NamedRuleWithOperations{{RuleWithOperations: admissionv1.RuleWithOperations{
				Operations: []admissionv1.OperationType{admissionv1.Connect}, Rule: admissionv1.Rule{APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"pods/exec"}},
			}}},
		}, Validations: []admissionv1.Validation{{Expression: expression, Message: "kubectl exec is not allowed on openclaw pods"}},
	}}
	policyBinding := &admissionv1.ValidatingAdmissionPolicyBinding{ObjectMeta: policyMeta("policy-binding-uid"), Spec: admissionv1.ValidatingAdmissionPolicyBindingSpec{PolicyName: policy.Name, ValidationActions: []admissionv1.ValidationAction{admissionv1.Deny}}}
	f.target = cetiadmin.TargetIdentity{Agent: identityOf(target), Namespace: identityOf(targetNS), Runtime: string(kind), Release: "openclaw-example", Resources: []cetiadmin.ManagedResource{
		{APIVersion: admissionv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicy", ObjectIdentity: identityOf(policy)},
		{APIVersion: admissionv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicyBinding", ObjectIdentity: identityOf(policyBinding)},
	}}
	state := cetiadmin.Installation{Record: record, RecordUID: "record-uid", Configuration: config, Phase: cetiadmin.PhaseActive, Bootstrapped: true,
		OperatorImage: "registry.example.com/operator:test", RuntimeVersion: "0.1.1",
		ResolvedRuntime: cetiadmin.RuntimeCoordinates{Version: "0.1.1", Image: *admin.Spec.Agent.Image, Port: admin.Spec.Agent.Port}}
	rawState, _ := json.Marshal(state)
	checkpoint := &corev1.ConfigMap{ObjectMeta: metadata(cetiadmin.InstallationName(f.source.Namespace, f.source.Name), f.source.Namespace, "record-uid"), Data: map[string]string{cetiadmin.BindingKey: string(rawState)}}
	objects := []client.Object{f.source, admin, team, namespace, account, target, targetNS, policy, policyBinding, checkpoint}
	runtimeController := &AgentReconciler{OperatorNamespace: f.source.Namespace, ConnectionsMCPAudience: "api://connections"}
	if kind == api.AgentTypeCustom {
		receipt, _ := json.Marshal(customOwnership{AgentName: target.Name, AgentUID: target.UID, Namespace: targetNS.Name, NamespaceUID: targetNS.UID, Release: "openclaw-example", Home: "home-example",
			HomeClaim: &runtimebindings.HomeClaim{StorageClass: "test-storage", StorageSize: "10Gi", AccessMode: "ReadWriteOnce"}})
		key := runtimeController.ownershipKey(target)
		objects = append(objects, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, UID: "receipt-uid", OwnerReferences: []metav1.OwnerReference{registryauth.Owner(target)}}, Immutable: ptr.To(true), Data: map[string]string{"ownership": string(receipt)}})
	}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	f.kube = fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.Agent{}).WithObjects(objects...).Build()
	tracked := interceptor.NewClient(f.kube, interceptor.Funcs{
		Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			t.Error("unexpected CREATE")
			return errors.New("unexpected CREATE")
		},
		Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
			t.Error("unexpected UPDATE")
			return errors.New("unexpected UPDATE")
		},
		Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
			t.Error("unexpected DELETE")
			return errors.New("unexpected DELETE")
		},
		Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
			data, err := patch.Data(object)
			if err != nil {
				return err
			}
			var ops []struct {
				Op, Path string
				Value    json.RawMessage
			}
			if patch.Type() != types.JSONPatchType || json.Unmarshal(data, &ops) != nil || len(ops) != 3 {
				t.Fatal("expected one UID/RV-conditional field patch")
			}
			var uid, rv string
			_ = json.Unmarshal(ops[0].Value, &uid)
			_ = json.Unmarshal(ops[1].Value, &rv)
			if ops[0].Op != "test" || ops[0].Path != "/metadata/uid" || uid == "" || uid != string(object.GetUID()) ||
				ops[1].Op != "test" || ops[1].Path != "/metadata/resourceVersion" || rv == "" || rv != object.GetResourceVersion() || ops[2].Op != "add" {
				t.Fatal("patch lost its exact UID/RV preconditions")
			}
			switch object.(type) {
			case *api.Agent:
				if ops[2].Path != "/metadata/annotations" {
					t.Fatal("mutated Agent outside annotation")
				}
			case *admissionv1.ValidatingAdmissionPolicy:
				if ops[2].Path != "/spec/matchConditions" && ops[2].Path != "/metadata/annotations" {
					t.Fatal("mutated validation rather than exact condition")
				}
			default:
				t.Fatal("unexpected policy binding, RBAC or workload write")
			}
			f.attempts = append(f.attempts, ops[2].Path)
			if f.beforePatch != nil {
				if err := f.beforePatch(object); err != nil {
					return err
				}
			}
			if err := c.Patch(ctx, object, patch, opts...); err != nil {
				return err
			}
			if f.afterPatch != nil {
				return f.afterPatch(object)
			}
			return nil
		},
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
			if f.readError != nil {
				if err := f.readError(object); err != nil {
					return err
				}
			}
			return c.Get(ctx, key, object, opts...)
		},
	})
	runtimeController.Client, runtimeController.APIReader = tracked, tracked
	f.r = &CetiAdminReconciler{Client: tracked, APIReader: tracked, Runtime: runtimeController, Source: client.ObjectKeyFromObject(f.source),
		OperatorImage: state.OperatorImage, ChartVersion: state.RuntimeVersion, AdmissionReady: func(context.Context) error { return nil }}
	return f
}

func (f *execFixture) policy() *admissionv1.ValidatingAdmissionPolicy {
	f.t.Helper()
	object := &admissionv1.ValidatingAdmissionPolicy{}
	f.get(f.target.Resources[0].ObjectIdentity, object)
	return object
}

func (f *execFixture) agent() *api.Agent {
	f.t.Helper()
	object := &api.Agent{}
	f.get(f.target.Agent, object)
	return object
}

func (f *execFixture) get(ref cetiadmin.ObjectIdentity, object client.Object) {
	f.t.Helper()
	if err := f.kube.Get(f.ctx, client.ObjectKey{Name: ref.Name, Namespace: ref.Namespace}, object); err != nil {
		f.t.Fatal(err)
	}
}

func (f *execFixture) update(object client.Object) {
	f.t.Helper()
	if err := f.kube.Update(f.ctx, object); err != nil {
		f.t.Fatal(err)
	}
}

func (f *execFixture) state(edit func(*cetiadmin.Installation)) {
	f.t.Helper()
	checkpoint := &corev1.ConfigMap{}
	if err := f.kube.Get(f.ctx, f.r.stateKey(), checkpoint); err != nil {
		f.t.Fatal(err)
	}
	var state cetiadmin.Installation
	if err := json.Unmarshal([]byte(checkpoint.Data[cetiadmin.BindingKey]), &state); err != nil {
		f.t.Fatal(err)
	}
	edit(&state)
	raw, err := json.Marshal(state)
	if err != nil {
		f.t.Fatal(err)
	}
	checkpoint.Data[cetiadmin.BindingKey] = string(raw)
	f.update(checkpoint)
}

func (f *execFixture) grant() error { return f.r.GrantExecAccess(f.ctx, f.binding, f.target) }
func (f *execFixture) revoke() error {
	return f.r.RevokeExecAccess(f.ctx, f.binding.Record(), f.target)
}

func TestCetiAdminNativeCustomExecWithdrawalKeepsProtection(t *testing.T) {
	f := newExecFixture(t, api.AgentTypeCustom)
	if err := f.grant(); err != nil {
		t.Fatal(err)
	}
	validations := f.policy().DeepCopy().Spec.Validations
	agent := f.agent()
	agent.Spec.SandboxRef = "native-sandbox"
	f.update(agent)
	namespace := &corev1.Namespace{}
	f.get(f.target.Namespace, namespace)
	namespace.OwnerReferences = []metav1.OwnerReference{sandboxworkload.AgentOwner(agent.Name, agent.UID)}
	namespace.Labels = map[string]string{sandboxworkload.ProtectedNamespaceLabel: "true"}
	namespace.Annotations[sandboxworkload.AgentUIDAnnotation] = string(agent.UID)
	namespace.Annotations[sandboxworkload.AgentNameAnnotation] = agent.Name
	f.update(namespace)
	receipt := &corev1.ConfigMap{}
	if err := f.kube.Get(f.ctx, f.r.Runtime.ownershipKey(agent), receipt); err != nil {
		t.Fatal(err)
	}
	if err := f.kube.Delete(f.ctx, receipt); err != nil {
		t.Fatal(err)
	}
	f.target.Protected = true
	if err := f.revoke(); err != nil {
		t.Fatal("native exec withdrawal required a legacy Custom receipt:", err)
	}
	policy := f.policy()
	if len(policy.Spec.MatchConditions) != 0 || policy.Annotations[cetiAdminExecReceipt] != "" ||
		!reflect.DeepEqual(policy.Spec.Validations, validations) {
		t.Fatal("native withdrawal changed the terminal guard or retained an Admin exception")
	}
	attempts := len(f.attempts)
	if err := f.grant(); err == nil || len(f.attempts) != attempts {
		t.Fatal("protected native Custom runtime received an exec exception")
	}
}

func TestCetiAdminExecUsesCurrentObjectsWithoutInventory(t *testing.T) {
	f := newExecFixture(t, api.AgentTypeHermes)
	policy := f.policy()
	policy.UID = "current-policy-uid"
	f.update(policy)
	f.target.Resources = nil
	if err := f.grant(); err != nil {
		t.Fatal("current owned policy required a registered resource UID", err)
	}
	if err := f.revoke(); err != nil {
		t.Fatal(err)
	}
}

func TestCetiAdminExecWithdrawalAfterAgentDeletion(t *testing.T) {
	f := newExecFixture(t, api.AgentTypeHermes)
	if err := f.grant(); err != nil {
		t.Fatal(err)
	}
	if err := f.kube.Delete(f.ctx, f.agent()); err != nil {
		t.Fatal(err)
	}
	if err := f.r.revokeRuntimeExec(f.ctx, f.binding.Record(), "example"); err != nil {
		t.Fatal("deleted Agent prevented its policy exception withdrawal", err)
	}
	policy := f.policy()
	if len(policy.Spec.MatchConditions) != 0 || policy.Annotations[cetiAdminExecReceipt] != "" {
		t.Fatal("orphaned exec exception survived withdrawal")
	}
}

func TestCetiAdminExecWithdrawalReportsConflictingReceipt(t *testing.T) {
	f := newExecFixture(t, api.AgentTypeHermes)
	if err := f.grant(); err != nil {
		t.Fatal(err)
	}
	policy := f.policy()
	var receipt registryauth.ExecAdminBinding
	if err := json.Unmarshal([]byte(policy.Annotations[cetiAdminExecReceipt]), &receipt); err != nil {
		t.Fatal(err)
	}
	receipt.SourceUID = "different-installation"
	raw, _ := json.Marshal(receipt)
	policy.Annotations[cetiAdminExecReceipt] = string(raw)
	f.update(policy)
	if err := f.r.revokeRuntimeExec(f.ctx, f.binding.Record(), "example"); err == nil {
		t.Fatal("withdrawal reported success while its exact caller exception remained")
	}
}

func TestCetiAdminExecGrantRevokeAndRerun(t *testing.T) {
	for _, kind := range []api.AgentType{api.AgentTypeHermes, api.AgentTypeOpenClaw, api.AgentTypeCustom} {
		t.Run(string(kind), func(t *testing.T) {
			f := newExecFixture(t, kind)
			policy, agent := f.policy(), f.agent()
			if err := f.grant(); err != nil {
				t.Fatal(err)
			}
			actual := f.policy()
			if !reflect.DeepEqual(actual.Spec.MatchConditions, []admissionv1.MatchCondition{{Name: "exclude-cetiadmin", Expression: execCondition}}) {
				t.Fatal("not the exact current username AND UID exception")
			}
			actual.Spec.MatchConditions, policy.Spec.MatchConditions = nil, nil
			if actual.Annotations[cetiAdminExecReceipt] == "" {
				t.Fatal("exec exception has no independent cleanup receipt")
			}
			delete(actual.Annotations, cetiAdminExecReceipt)
			if !reflect.DeepEqual(actual.Spec, policy.Spec) || !reflect.DeepEqual(actual.Annotations, policy.Annotations) || !reflect.DeepEqual(actual.OwnerReferences, policy.OwnerReferences) {
				t.Fatal("changed existing validation or ownership")
			}
			currentAgent := f.agent()
			binding, err := registryauth.ParseExecAdminBinding(currentAgent.Annotations[registryauth.ExecAdminBindingAnnotation], agent.UID, f.target.Namespace.UID)
			if err != nil || binding.Username != execUsername || binding.ServiceAccountUID != "admin-account-uid" || binding.SourceUID != "source-uid" {
				t.Fatal("wrong annotation binding")
			}
			if currentAgent.Annotations["example.com/keep"] != "target-data" || !reflect.DeepEqual(currentAgent.Spec, agent.Spec) {
				t.Fatal("changed unrelated target data")
			}
			writes := len(f.attempts)
			if err := f.grant(); err != nil || len(f.attempts) != writes {
				t.Fatal("repeat grant was not read-only", err)
			}
			if err := f.revoke(); err != nil {
				t.Fatal(err)
			}
			if len(f.policy().Spec.MatchConditions) != 0 {
				t.Fatal("exception survived revocation")
			}
			if _, exists := f.agent().Annotations[registryauth.ExecAdminBindingAnnotation]; exists {
				t.Fatal("binding annotation survived revocation")
			}
			if got := f.attempts[writes:]; !reflect.DeepEqual(got, []string{"/spec/matchConditions", "/metadata/annotations", "/metadata/annotations"}) {
				t.Fatalf("wrong withdrawal order: %v", got)
			}
			writes = len(f.attempts)
			if err := f.revoke(); err != nil || len(f.attempts) != writes {
				t.Fatal("repeat revoke wrote again", err)
			}
		})
	}
}

func TestCetiAdminExecRejectsUnboundOrForgedInputs(t *testing.T) {
	for _, scenario := range []string{"unbound", "source-disabled", "source-replaced", "sa-replaced", "agent-replaced", "namespace-replaced", "namespace-owner",
		"policy-owner", "release-owner", "binding-warn", "binding-policy", "validation", "resource-rule", "selector", "extra-validation",
		"extra-condition", "forged-condition", "foreign-condition", "forged-annotation", "stale-annotation", "protected", "sandbox-spec", "sandbox-status", "team-policy", "missing-team",
		"admission-unavailable", "read-error"} {
		t.Run(scenario, func(t *testing.T) {
			f := newExecFixture(t, api.AgentTypeHermes)
			policy, agent := f.policy(), f.agent()
			switch scenario {
			case "unbound":
				f.binding = cetiadmin.Binding{}
			case "source-disabled":
				f.source.Data[cetiadmin.ConfigKey] = `{"enabled":false}`
				f.update(f.source)
			case "source-replaced":
				f.source.UID = "new-source-uid"
				f.update(f.source)
			case "sa-replaced":
				account := &corev1.ServiceAccount{}
				f.get(f.binding.Record().ServiceAccount, account)
				account.UID = "new-account-uid"
				f.update(account)
			case "agent-replaced":
				agent.UID = "replacement"
				f.update(agent)
			case "namespace-replaced", "namespace-owner":
				ns := &corev1.Namespace{}
				f.get(f.target.Namespace, ns)
				if scenario == "namespace-replaced" {
					ns.UID = "replacement"
				} else {
					ns.OwnerReferences[0].UID = "foreign"
				}
				f.update(ns)
			case "binding-warn", "binding-policy":
				binding := &admissionv1.ValidatingAdmissionPolicyBinding{}
				f.get(f.target.Resources[1].ObjectIdentity, binding)
				if scenario == "binding-warn" {
					binding.Spec.ValidationActions = []admissionv1.ValidationAction{admissionv1.Warn}
				} else {
					binding.Spec.PolicyName = "foreign"
				}
				f.update(binding)
			case "policy-owner":
				policy.OwnerReferences = []metav1.OwnerReference{{Kind: "Agent", UID: "foreign"}}
				f.update(policy)
			case "release-owner":
				policy.Annotations["meta.helm.sh/release-name"] = "foreign"
				f.update(policy)
			case "validation":
				policy.Spec.Validations[0].Expression = "true"
				f.update(policy)
			case "resource-rule":
				policy.Spec.MatchConstraints.ResourceRules[0].Resources = []string{"pods/attach"}
				f.update(policy)
			case "selector":
				policy.Spec.MatchConstraints.NamespaceSelector = nil
				f.update(policy)
			case "extra-validation":
				policy.Spec.Validations = append(policy.Spec.Validations, admissionv1.Validation{Expression: "true"})
				f.update(policy)
			case "extra-condition":
				policy.Spec.MatchConditions = []admissionv1.MatchCondition{{Name: "exclude-cetiadmin", Expression: execCondition}, {Name: "skip", Expression: "false"}}
				f.update(policy)
			case "forged-condition":
				policy.Spec.MatchConditions = []admissionv1.MatchCondition{{Name: "exclude-cetiadmin", Expression: "false"}}
				f.update(policy)
			case "foreign-condition":
				policy.Spec.MatchConditions = []admissionv1.MatchCondition{{Name: "exclude-other-admin", Expression: `request.userInfo.username != "system:serviceaccount:other:admin"`}}
				f.update(policy)
			case "forged-annotation":
				agent.Annotations[registryauth.ExecAdminBindingAnnotation] = `{"username":"system:masters"}`
				f.update(agent)
			case "stale-annotation":
				raw, _ := json.Marshal(registryauth.ExecAdminBinding{TargetAgentUID: agent.UID, NamespaceUID: f.target.Namespace.UID, Username: execUsername, ServiceAccountUID: "old-account-uid", SourceUID: "source-uid"})
				agent.Annotations[registryauth.ExecAdminBindingAnnotation] = string(raw)
				f.update(agent)
			case "protected":
				f.target.Protected = true
			case "sandbox-spec":
				agent.Spec.SandboxRef = "sandbox"
				f.update(agent)
			case "sandbox-status":
				agent.Status.Sandbox = &api.SandboxStatus{}
				if err := f.kube.Status().Update(f.ctx, agent); err != nil {
					t.Fatal(err)
				}
			case "team-policy", "missing-team":
				agent.Spec.Team = "restricted"
				f.update(agent)
				if scenario == "team-policy" {
					if err := f.kube.Create(f.ctx, &api.Team{ObjectMeta: metav1.ObjectMeta{Name: "restricted", UID: "team-uid"}, Spec: api.TeamSpec{AgentPolicyRef: "policy"}}); err != nil {
						t.Fatal(err)
					}
				}
			case "admission-unavailable":
				f.r.AdmissionReady = func(context.Context) error { return errors.New("unavailable") }
			case "read-error":
				f.readError = func(client.Object) error { return errors.New("unavailable") }
			}
			if err := f.grant(); err == nil {
				t.Fatal("untrusted target acquired an exception")
			}
			if len(f.attempts) != 0 {
				t.Fatal("wrote before verifying identities and existing policy")
			}
		})
	}
}

func TestCetiAdminExecRevocationSurvivesDisabledSourceAndSARecreation(t *testing.T) {
	for _, scenario := range []string{"disabled", "source-gone", "sa-replaced", "protected"} {
		t.Run(scenario, func(t *testing.T) {
			f := newExecFixture(t, api.AgentTypeHermes)
			if err := f.grant(); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "disabled":
				f.source.Data[cetiadmin.ConfigKey] = `{"enabled":false}`
				f.update(f.source)
			case "source-gone":
				if err := f.kube.Delete(f.ctx, f.source); err != nil {
					t.Fatal(err)
				}
			case "sa-replaced":
				account := &corev1.ServiceAccount{}
				f.get(f.binding.Record().ServiceAccount, account)
				account.UID = "new-account-uid"
				f.update(account)
			case "protected":
				agent := f.agent()
				agent.Spec.SandboxRef = "sandbox"
				f.update(agent)
			}
			if err := f.revoke(); err != nil {
				t.Fatal(err)
			}
			if len(f.policy().Spec.MatchConditions) != 0 || f.agent().Annotations[registryauth.ExecAdminBindingAnnotation] != "" {
				t.Fatal("old exception was retained")
			}
		})
	}
}

func TestCetiAdminExecRejectsRevokeOfAnotherBinding(t *testing.T) {
	f := newExecFixture(t, api.AgentTypeHermes)
	if err := f.grant(); err != nil {
		t.Fatal(err)
	}
	writes := len(f.attempts)
	record := f.binding.Record()
	record.ServiceAccount.UID = "replacement-account"
	if err := f.r.RevokeExecAccess(f.ctx, record, f.target); err == nil || len(f.attempts) != writes {
		t.Fatal("foreign revocation changed the active binding")
	}
}

func TestCetiAdminExecConditionalWritesAndLostAck(t *testing.T) {
	for _, path := range []string{"agent", "policy"} {
		faults := []string{"version-conflict", "uid-conflict", "lost-ack"}
		if path == "agent" {
			faults = append(faults, "source-disabled")
		}
		for _, fault := range faults {
			t.Run(path+"/"+fault, func(t *testing.T) {
				f := newExecFixture(t, api.AgentTypeHermes)
				selected := func(object client.Object) bool {
					_, isAgent := object.(*api.Agent)
					return isAgent == (path == "agent")
				}
				triggered := false
				if fault == "lost-ack" || fault == "source-disabled" {
					f.afterPatch = func(object client.Object) error {
						if triggered || !selected(object) {
							return nil
						}
						triggered = true
						if fault == "source-disabled" {
							f.source.Data[cetiadmin.ConfigKey] = `{"enabled":false}`
							f.update(f.source)
							return nil
						}
						return errors.New("lost acknowledgement")
					}
				} else {
					f.beforePatch = func(object client.Object) error {
						if triggered || !selected(object) {
							return nil
						}
						triggered = true
						current := object.DeepCopyObject().(client.Object)
						if err := f.kube.Get(f.ctx, client.ObjectKeyFromObject(object), current); err != nil {
							return err
						}
						if fault == "uid-conflict" {
							current.SetUID("replacement-uid")
						}
						annotations := current.GetAnnotations()
						if annotations == nil {
							annotations = map[string]string{}
						}
						annotations["example.com/concurrent"] = "keep"
						current.SetAnnotations(annotations)
						return f.kube.Update(f.ctx, current)
					}
				}
				err := f.grant()
				if !triggered {
					t.Fatal("fault was not exercised")
				}
				if fault == "lost-ack" {
					if err != nil || len(f.attempts) != 3 {
						t.Fatal("did not recognize successful exact readback", err)
					}
				} else {
					if err == nil {
						t.Fatal("grant ignored a changed identity/source")
					}
					if len(f.policy().Spec.MatchConditions) != 0 {
						t.Fatal("exception enabled after an unconfirmed write")
					}
				}
				if fault == "version-conflict" {
					f.beforePatch = nil
					if err := f.grant(); err != nil {
						t.Fatal("could not resume a partial, fail-closed grant", err)
					}
					var current client.Object = f.policy()
					if path == "agent" {
						current = f.agent()
					}
					if current.GetAnnotations()["example.com/concurrent"] != "keep" {
						t.Fatal("overwrote a concurrent annotation")
					}
				}
			})
		}
	}
}

func TestCetiAdminExecNewSARequiresRevokeAndNewRecord(t *testing.T) {
	f := newExecFixture(t, api.AgentTypeHermes)
	if err := f.grant(); err != nil {
		t.Fatal(err)
	}
	record, config := f.binding.Record(), f.binding.Config()
	account := &corev1.ServiceAccount{}
	f.get(record.ServiceAccount, account)
	if err := f.kube.Delete(f.ctx, account); err != nil {
		t.Fatal(err)
	}
	account.UID, account.ResourceVersion = "replacement-account", ""
	if err := f.kube.Create(f.ctx, account); err != nil {
		t.Fatal(err)
	}
	writes := len(f.attempts)
	if err := f.grant(); err == nil || len(f.attempts) != writes {
		t.Fatal("same-name replacement inherited an old grant")
	}
	if err := f.revoke(); err != nil {
		t.Fatal(err)
	}
	record.ServiceAccount = identityOf(account)
	f.state(func(s *cetiadmin.Installation) { s.Record = record })
	agent, team, namespace := &api.Agent{}, &api.Team{}, &corev1.Namespace{}
	f.get(record.Agent, agent)
	f.get(record.Team, team)
	f.get(record.Namespace, namespace)
	var err error
	f.binding, err = cetiadmin.ResolveBinding(config, &record, cetiadmin.ObservedInstallation{Source: f.source, Agent: agent, Team: team, Namespace: namespace, ServiceAccount: account})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.grant(); err != nil {
		t.Fatal("new verified record could not receive its own grant", err)
	}
	if expression := f.policy().Spec.MatchConditions[0].Expression; expression != strings.Replace(execCondition, "admin-account-uid", "replacement-account", 1) {
		t.Fatal("new grant retained the old SA UID")
	}
}

func TestCetiAdminExecPartialRevokePreservesConcurrentChanges(t *testing.T) {
	f := newExecFixture(t, api.AgentTypeCustom)
	if err := f.grant(); err != nil {
		t.Fatal(err)
	}
	f.beforePatch = func(object client.Object) error {
		if _, ok := object.(*api.Agent); !ok {
			return nil
		}
		agent := f.agent()
		agent.Annotations["example.com/concurrent"] = "keep"
		return f.kube.Update(f.ctx, agent)
	}
	if err := f.revoke(); err == nil {
		t.Fatal("annotation RV conflict was ignored")
	}
	if len(f.policy().Spec.MatchConditions) != 0 || f.agent().Annotations[registryauth.ExecAdminBindingAnnotation] == "" {
		t.Fatal("partial revoke did not leave the policy closed")
	}
	f.beforePatch = nil
	if err := f.revoke(); err != nil {
		t.Fatal(err)
	}
	if annotations := f.agent().Annotations; annotations[registryauth.ExecAdminBindingAnnotation] != "" || annotations["example.com/concurrent"] != "keep" {
		t.Fatal("retry lost an unrelated annotation")
	}
}

func TestCetiAdminExecRejectsCustomOwnershipAndCommandDrift(t *testing.T) {
	for _, scenario := range []string{"receipt", "namespace-marker", "policy-owner", "release-owner", "command", "container"} {
		t.Run(scenario, func(t *testing.T) {
			f := newExecFixture(t, api.AgentTypeCustom)
			policy := f.policy()
			switch scenario {
			case "receipt":
				receipt := &corev1.ConfigMap{}
				if err := f.kube.Get(f.ctx, f.r.Runtime.ownershipKey(f.agent()), receipt); err != nil {
					t.Fatal(err)
				}
				receipt.Data["ownership"] = strings.Replace(receipt.Data["ownership"], "target-namespace-uid", "another-namespace", 1)
				f.update(receipt)
			case "namespace-marker":
				ns := &corev1.Namespace{}
				f.get(f.target.Namespace, ns)
				ns.Annotations[customAgentUID] = "another-agent"
				f.update(ns)
			case "policy-owner":
				policy.OwnerReferences[0].UID = "foreign"
				f.update(policy)
			case "release-owner":
				policy.Annotations["meta.helm.sh/release-name"] = "foreign"
				f.update(policy)
			case "command":
				policy.Spec.Validations[0].Expression += " || true"
				f.update(policy)
			case "container":
				policy.Spec.Validations[0].Expression = strings.Replace(policy.Spec.Validations[0].Expression, `object.container == "cetirun"`, `object.container == "istio-proxy"`, 1)
				f.update(policy)
			}
			if err := f.grant(); err == nil || len(f.attempts) != 0 {
				t.Fatal("unverified Custom boundary was modified")
			}
		})
	}
}

func TestCetiAdminExecCELPreservesOtherCallersTerminalRestrictions(t *testing.T) {
	f := newExecFixture(t, api.AgentTypeCustom)
	if err := f.grant(); err != nil {
		t.Fatal(err)
	}
	policy := f.policy()
	env, err := cel.NewEnv(cel.Variable("request", cel.DynType), cel.Variable("object", cel.DynType), cel.Variable("namespaceObject", cel.DynType))
	if err != nil {
		t.Fatal(err)
	}
	compile := func(expression string) cel.Program {
		ast, issues := env.Compile(expression)
		if issues.Err() != nil {
			t.Fatal(issues.Err())
		}
		program, err := env.Program(ast)
		if err != nil {
			t.Fatal(err)
		}
		return program
	}
	condition, validation := compile(policy.Spec.MatchConditions[0].Expression), compile(policy.Spec.Validations[0].Expression)
	for _, test := range []struct {
		name, username, uid, container string
		command                        []string
		skipped, valid                 bool
	}{
		{"current admin", execUsername, "admin-account-uid", "cetirun", []string{"sh", "-c", "inspect"}, true, false},
		{"recreated SA", execUsername, "replacement-uid", "cetirun", []string{"sh", "-i"}, false, false},
		{"different SA", "system:serviceaccount:other:admin", "admin-account-uid", "cetirun", []string{"sh", "-i"}, false, false},
		{"legacy admin", "system:serviceaccount:cetirun-admin:openclaw-admin-cetirun", "legacy-uid", "cetirun", []string{"sh", "-i"}, false, false},
		{"terminal", "system:serviceaccount:cetirun-example:openclaw-example-cetirun-terminal", "terminal-uid", "cetirun", []string{"sh", "-i"}, false, true},
		{"wrong container", "system:serviceaccount:cetirun-example:openclaw-example-cetirun-terminal", "terminal-uid", "istio-proxy", []string{"sh", "-i"}, false, false},
		{"arbitrary command", "system:serviceaccount:cetirun-example:openclaw-example-cetirun-terminal", "terminal-uid", "cetirun", []string{"sh", "-c", "inspect"}, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := map[string]any{"request": map[string]any{"userInfo": map[string]any{"username": test.username, "uid": test.uid}, "namespace": f.target.Namespace.Name, "name": "openclaw-example-cetirun-0"},
				"object":          map[string]any{"container": test.container, "command": test.command, "stdin": true, "stdout": true, "tty": true},
				"namespaceObject": map[string]any{"metadata": map[string]any{"annotations": map[string]any{customAgentUID: string(f.target.Agent.UID)}}}}
			matched, _, err := condition.Eval(input)
			if err != nil {
				t.Fatal(err)
			}
			valid, _, err := validation.Eval(input)
			if err != nil {
				t.Fatal(err)
			}
			if (matched.Value() == false) != test.skipped || (valid.Value() == true) != test.valid {
				t.Fatalf("policy bypass or terminal restriction changed: %v %v", matched, valid)
			}
		})
	}
	if strings.Contains(policy.Spec.Validations[0].Expression, execUsername) {
		t.Fatal("CetiAdmin exception was folded into the terminal's own authority")
	}
}

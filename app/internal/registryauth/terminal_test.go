// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package registryauth

import (
	"context"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/internal/terminalpolicy"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestIsolationAcceptsOnlyUIDBoundTerminalExec(t *testing.T) {
	for _, test := range []string{"bound", "bound-protected-spec", "bound-protected-team", "namespace-owner", "namespace-marker", "account-owner", "deployment-owner", "policy-owner", "binding-owner",
		"fail-open", "different-command", "match-condition", "object-selector", "excluded-rule", "resource-rule", "binding-warn", "binding-selector",
		"secret-access", "admission-write", "token-mint", "foreign-exec", "runtime-identity", "legacy-condition", "unrecorded-admin",
		"admin-bound", "admin-missing-condition", "admin-stale-agent", "admin-stale-namespace", "admin-stale-sa", "admin-missing-source",
		"admin-extra-condition", "admin-protected-spec", "admin-protected-status", "admin-protected-team", "admin-missing-team"} {
		t.Run(test, func(t *testing.T) {
			agent := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: "example", UID: "agent-uid"}, Spec: api.AgentSpec{Agent: api.HarnessSpec{Type: api.AgentTypeCustom}}}
			namespace, workload := "cetirun-example", "openclaw-example-cetirun"
			name := terminalpolicy.WorkloadName(workload)
			metadata := func(name, ns string) metav1.ObjectMeta {
				return metav1.ObjectMeta{Name: name, Namespace: ns, OwnerReferences: []metav1.OwnerReference{Owner(agent)}}
			}
			ns := &corev1.Namespace{ObjectMeta: metadata(namespace, "")}
			ns.UID = "namespace-uid"
			ns.OwnerReferences = nil
			ns.Annotations = map[string]string{"cetirun.microsoft.com/agent-uid": string(agent.UID)}
			account := &corev1.ServiceAccount{ObjectMeta: metadata(name, namespace)}
			deployment := &appsv1.Deployment{ObjectMeta: metadata(name, namespace), Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{ServiceAccountName: name}}}}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name + "-pod", Namespace: namespace}, Spec: corev1.PodSpec{ServiceAccountName: name}}
			policyName := workload + "-" + namespace + "-deny-exec"
			policy := &admissionv1.ValidatingAdmissionPolicy{ObjectMeta: metadata(policyName, ""), Spec: admissionv1.ValidatingAdmissionPolicySpec{
				FailurePolicy: ptr.To(admissionv1.Fail),
				MatchConstraints: &admissionv1.MatchResources{
					NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": namespace}},
					ResourceRules: []admissionv1.NamedRuleWithOperations{{RuleWithOperations: admissionv1.RuleWithOperations{
						Operations: []admissionv1.OperationType{admissionv1.Connect}, Rule: admissionv1.Rule{APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"pods/exec"}},
					}}},
				},
				Validations: []admissionv1.Validation{{Expression: terminalpolicy.AdmissionExpression(namespace, workload+"-0", "cetirun", name, string(agent.UID))}},
			}}
			binding := &admissionv1.ValidatingAdmissionPolicyBinding{ObjectMeta: metadata(policyName, ""), Spec: admissionv1.ValidatingAdmissionPolicyBindingSpec{PolicyName: policyName, ValidationActions: []admissionv1.ValidationAction{admissionv1.Deny}}}
			team := &api.Team{ObjectMeta: metav1.ObjectMeta{Name: "protected", UID: "team-uid"}, Spec: api.TeamSpec{AgentPolicyRef: "policy"}}
			if strings.HasPrefix(test, "admin-") || test == "unrecorded-admin" {
				policy.Spec.MatchConditions = []admissionv1.MatchCondition{{Name: "exclude-cetiadmin", Expression: `request.userInfo.username != "system:serviceaccount:cetirun-operations:managed-admin" || request.userInfo.uid != "account-uid"`}}
				if test != "unrecorded-admin" {
					agent.Annotations = map[string]string{ExecAdminBindingAnnotation: strings.Replace(execBindingJSON, "target-uid", string(agent.UID), 1)}
				}
			}
			desiredSA := workload
			switch test {
			case "namespace-owner":
				ns.OwnerReferences = []metav1.OwnerReference{{Kind: "Agent", UID: "other-uid"}}
			case "namespace-marker":
				ns.Annotations["cetirun.microsoft.com/agent-uid"] = "other-uid"
			case "account-owner":
				account.OwnerReferences[0].UID = "other-uid"
			case "deployment-owner":
				deployment.OwnerReferences[0].UID = "other-uid"
			case "policy-owner":
				policy.OwnerReferences[0].UID = "other-uid"
			case "binding-owner":
				binding.OwnerReferences[0].UID = "other-uid"
			case "fail-open":
				policy.Spec.FailurePolicy = ptr.To(admissionv1.Ignore)
			case "different-command":
				policy.Spec.Validations[0].Expression = "true"
			case "match-condition":
				policy.Spec.MatchConditions = append(policy.Spec.MatchConditions, admissionv1.MatchCondition{Name: "skip", Expression: "false"})
			case "object-selector":
				policy.Spec.MatchConstraints.ObjectSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"skip": "true"}}
			case "excluded-rule":
				policy.Spec.MatchConstraints.ExcludeResourceRules = policy.Spec.MatchConstraints.ResourceRules
			case "resource-rule":
				policy.Spec.MatchConstraints.ResourceRules[0].Resources = []string{"pods/attach"}
			case "binding-warn":
				binding.Spec.ValidationActions = []admissionv1.ValidationAction{admissionv1.Warn}
			case "binding-selector":
				binding.Spec.MatchResources = &admissionv1.MatchResources{}
			case "runtime-identity":
				desiredSA = name
			case "legacy-condition":
				policy.Spec.MatchConditions = []admissionv1.MatchCondition{{Name: "exclude-adminclaw", Expression: `request.userInfo.username != "system:serviceaccount:cetirun-admin:openclaw-admin-cetirun"`}}
			case "admin-missing-condition":
				policy.Spec.MatchConditions = nil
			case "admin-stale-agent":
				agent.Annotations[ExecAdminBindingAnnotation] = execBindingJSON
			case "admin-stale-namespace":
				ns.UID = "new-namespace-uid"
			case "admin-stale-sa":
				policy.Spec.MatchConditions[0].Expression = strings.ReplaceAll(policy.Spec.MatchConditions[0].Expression, "account-uid", "new-account-uid")
			case "admin-missing-source":
				agent.Annotations[ExecAdminBindingAnnotation] = strings.ReplaceAll(agent.Annotations[ExecAdminBindingAnnotation], "source-uid", "")
			case "admin-extra-condition":
				policy.Spec.MatchConditions = append(policy.Spec.MatchConditions, admissionv1.MatchCondition{Name: "skip", Expression: "false"})
			case "admin-protected-spec", "bound-protected-spec":
				agent.Spec.SandboxRef = "sandbox"
			case "admin-protected-status":
				agent.Status.Sandbox = &api.SandboxStatus{}
			case "admin-protected-team", "bound-protected-team":
				agent.Spec.Team = team.Name
			case "admin-missing-team":
				agent.Spec.Team = "missing-team"
			}
			scheme := runtime.NewScheme()
			_ = clientgoscheme.AddToScheme(scheme)
			_ = api.AddToScheme(scheme)
			ruleNamespace := namespace
			if test == "foreign-exec" {
				ruleNamespace = "another-namespace"
			}
			role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "cetirun-terminal-exec"}, Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"pods/exec"}, Verbs: []string{"get", "create"}}}}
			execBinding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ruleNamespace}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Namespace: namespace, Name: name}}, RoleRef: rbacv1.RoleRef{Kind: "ClusterRole", Name: role.Name}}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent, ns, account, deployment, pod, policy, binding, role, execBinding, team).
				WithInterceptorFuncs(interceptor.Funcs{Create: func(_ context.Context, _ client.WithWatch, object client.Object, _ ...client.CreateOption) error {
					review := object.(*authorizationv1.SubjectAccessReview)
					access := review.Spec.ResourceAttributes
					isTerminal := review.Spec.User == "system:serviceaccount:"+namespace+":"+name
					review.Status.Allowed = isTerminal && ((access.Namespace == ruleNamespace && access.Resource == "pods" && access.Subresource == "exec" && (access.Verb == "get" || access.Verb == "create")) ||
						test == "secret-access" && access.Resource == "secrets" && access.Verb == "get" ||
						test == "admission-write" && access.Resource == "validatingadmissionpolicies" && access.Verb == "update" ||
						test == "token-mint" && access.Resource == "serviceaccounts" && access.Subresource == "token" && access.Verb == "create")
					return nil
				}}).Build()
			err := CheckIsolation(context.Background(), c, DefaultNamespace, namespace, desiredSA)
			if (err == nil) != (strings.HasPrefix(test, "bound") || test == "admin-bound") {
				t.Fatalf("terminal isolation result: %v", err)
			}
		})
	}
}

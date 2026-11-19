// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cetiadmin

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"unicode"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const NamespaceAdmissionGuardName = "cetirun-cetiadmin-namespace"

// ExpectedNamespaceAdmissionGuard mirrors the Chart's policy for readback only.
func ExpectedNamespaceAdmissionGuard(platformNamespace, adminNamespace string) (*admissionv1.ValidatingAdmissionPolicy, *admissionv1.ValidatingAdmissionPolicyBinding) {
	expression := fmt.Sprintf(`
!(object.metadata.name in [%q, %q] ||
  (has(object.metadata.labels) && "app.kubernetes.io/managed-by" in object.metadata.labels &&
   object.metadata.labels["app.kubernetes.io/managed-by"] == "cetirun-operator") ||
  (oldObject != null && has(oldObject.metadata.labels) && "app.kubernetes.io/managed-by" in oldObject.metadata.labels &&
   oldObject.metadata.labels["app.kubernetes.io/managed-by"] == "cetirun-operator")) ||
(!has(object.metadata.labels) ||
 ((!("control-plane" in object.metadata.labels) || object.metadata.labels["control-plane"] != "true") &&
  (!("kubernetes.azure.com/managedby" in object.metadata.labels) || object.metadata.labels["kubernetes.azure.com/managedby"] != "aks")))`, platformNamespace, adminNamespace)
	policy := &admissionv1.ValidatingAdmissionPolicy{ObjectMeta: metav1.ObjectMeta{Name: NamespaceAdmissionGuardName}, Spec: admissionv1.ValidatingAdmissionPolicySpec{
		FailurePolicy: ptr.To(admissionv1.Fail),
		MatchConstraints: &admissionv1.MatchResources{MatchPolicy: ptr.To(admissionv1.Equivalent), ResourceRules: []admissionv1.NamedRuleWithOperations{{RuleWithOperations: admissionv1.RuleWithOperations{
			Operations: []admissionv1.OperationType{admissionv1.Create, admissionv1.Update},
			Rule:       admissionv1.Rule{APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"namespaces", "namespaces/status", "namespaces/finalize"}, Scope: ptr.To(admissionv1.ClusterScope)},
		}}}},
		Validations: []admissionv1.Validation{{Expression: expression, Reason: ptr.To(metav1.StatusReasonForbidden),
			Message: "CetiAdmin authority and managed Agent namespaces cannot use labels that bypass admission."}},
	}}
	binding := &admissionv1.ValidatingAdmissionPolicyBinding{ObjectMeta: metav1.ObjectMeta{Name: NamespaceAdmissionGuardName}, Spec: admissionv1.ValidatingAdmissionPolicyBindingSpec{
		PolicyName: NamespaceAdmissionGuardName, ValidationActions: []admissionv1.ValidationAction{admissionv1.Deny},
	}}
	return policy, binding
}

func CheckNamespaceAdmissionGuard(ctx context.Context, reader client.Reader, platformNamespace, adminNamespace string) error {
	policy, binding := &admissionv1.ValidatingAdmissionPolicy{}, &admissionv1.ValidatingAdmissionPolicyBinding{}
	key := client.ObjectKey{Name: NamespaceAdmissionGuardName}
	if err := reader.Get(ctx, key, policy); err != nil {
		return fmt.Errorf("CetiAdmin namespace admission guard is unavailable: %w", err)
	}
	if err := reader.Get(ctx, key, binding); err != nil {
		return fmt.Errorf("CetiAdmin namespace admission guard binding is unavailable: %w", err)
	}
	if err := ValidateNamespaceAdmissionGuard(policy, binding, platformNamespace, adminNamespace); err != nil {
		return err
	}
	for _, name := range []string{platformNamespace, adminNamespace} {
		namespace := &corev1.Namespace{}
		if err := reader.Get(ctx, client.ObjectKey{Name: name}, namespace); err != nil {
			if name == adminNamespace && apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		if NamespaceExcludedFromAdmission(namespace.Labels) {
			return fmt.Errorf("CetiAdmin namespace %s is excluded from admission", name)
		}
	}
	return nil
}

func ValidateNamespaceAdmissionGuard(policy *admissionv1.ValidatingAdmissionPolicy, binding *admissionv1.ValidatingAdmissionPolicyBinding, platformNamespace, adminNamespace string) error {
	wantPolicy, wantBinding := ExpectedNamespaceAdmissionGuard(platformNamespace, adminNamespace)
	if policy.Name != wantPolicy.Name || binding.Name != wantBinding.Name || policy.DeletionTimestamp != nil || binding.DeletionTimestamp != nil {
		return fmt.Errorf("CetiAdmin namespace admission guard identity changed")
	}
	actual := policy.Spec.DeepCopy()
	if actual.MatchConstraints != nil {
		if AdmissionSelectorMatches(actual.MatchConstraints.NamespaceSelector, nil) {
			actual.MatchConstraints.NamespaceSelector = nil
		}
		if AdmissionSelectorMatches(actual.MatchConstraints.ObjectSelector, nil) {
			actual.MatchConstraints.ObjectSelector = nil
		}
	}
	normalize := func(value string) string {
		return strings.Map(func(r rune) rune {
			if unicode.IsSpace(r) {
				return -1
			}
			return r
		}, value)
	}
	for i := range actual.Validations {
		actual.Validations[i].Expression = normalize(actual.Validations[i].Expression)
	}
	for i := range wantPolicy.Spec.Validations {
		wantPolicy.Spec.Validations[i].Expression = normalize(wantPolicy.Spec.Validations[i].Expression)
	}
	if !reflect.DeepEqual(*actual, wantPolicy.Spec) || !reflect.DeepEqual(binding.Spec, wantBinding.Spec) {
		return fmt.Errorf("CetiAdmin namespace admission guard does not enforce the required label protection")
	}
	return nil
}

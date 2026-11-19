// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package registryauth

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	strictjson "sigs.k8s.io/json"
)

const ExecAdminBindingAnnotation = "cetirun.microsoft.com/exec-admin-binding"

var (
	ErrExecAdminBinding = errors.New("invalid or stale exec admin binding")
	execUIDPattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
)

// ExecAdminBinding is an Operator-protected annotation on the target Agent,
// not a tenant option or an independent grant of exec authority.
type ExecAdminBinding struct {
	TargetAgentUID    types.UID `json:"targetAgentUID"`
	NamespaceUID      types.UID `json:"namespaceUID"`
	Username          string    `json:"username"`
	ServiceAccountUID types.UID `json:"serviceAccountUID"`
	SourceUID         types.UID `json:"sourceUID"`
}

func ParseExecAdminBinding(raw string, agentUID, namespaceUID types.UID) (ExecAdminBinding, error) {
	var binding ExecAdminBinding
	if len(raw) == 0 || len(raw) > 4096 {
		return binding, ErrExecAdminBinding
	}
	strictErrors, err := strictjson.UnmarshalStrict([]byte(raw), &binding)
	if err != nil || len(strictErrors) != 0 || binding.TargetAgentUID != agentUID || binding.NamespaceUID != namespaceUID {
		return ExecAdminBinding{}, ErrExecAdminBinding
	}
	for _, uid := range []types.UID{binding.TargetAgentUID, binding.NamespaceUID, binding.ServiceAccountUID, binding.SourceUID} {
		if !execUIDPattern.MatchString(string(uid)) {
			return ExecAdminBinding{}, ErrExecAdminBinding
		}
	}
	parts := strings.Split(binding.Username, ":")
	if len(parts) != 4 || parts[0] != "system" || parts[1] != "serviceaccount" ||
		len(validation.IsDNS1123Label(parts[2])) != 0 || len(validation.IsDNS1123Subdomain(parts[3])) != 0 {
		return ExecAdminBinding{}, ErrExecAdminBinding
	}
	return binding, nil
}

func (b ExecAdminBinding) MatchCondition() admissionv1.MatchCondition {
	return admissionv1.MatchCondition{Name: "exclude-cetiadmin", Expression: fmt.Sprintf(
		`request.userInfo.username != %q || request.userInfo.uid != %q`, b.Username, b.ServiceAccountUID)}
}

// ExecPolicyMatches accepts only the fixed namespace exec rule and either no
// exception or the one expected binding. The validation expression is supplied
// by platform code, never by an annotation or Chart override.
func ExecPolicyMatches(policy *admissionv1.ValidatingAdmissionPolicy, binding *admissionv1.ValidatingAdmissionPolicyBinding, namespace, expression string, admin *ExecAdminBinding) bool {
	spec := policy.Spec
	if admin == nil {
		if len(spec.MatchConditions) != 0 {
			return false
		}
	} else if len(spec.MatchConditions) != 1 || spec.MatchConditions[0] != admin.MatchCondition() {
		return false
	}
	match := spec.MatchConstraints
	if spec.FailurePolicy == nil || *spec.FailurePolicy != admissionv1.Fail || spec.ParamKind != nil ||
		len(spec.Variables) != 0 || len(spec.AuditAnnotations) != 0 || match == nil || len(match.ExcludeResourceRules) != 0 ||
		!reflect.DeepEqual(match.NamespaceSelector, &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": namespace}}) ||
		match.ObjectSelector != nil && (len(match.ObjectSelector.MatchLabels) != 0 || len(match.ObjectSelector.MatchExpressions) != 0) ||
		len(match.ResourceRules) != 1 || len(spec.Validations) != 1 || spec.Validations[0].Expression != expression {
		return false
	}
	rule := match.ResourceRules[0]
	if len(rule.ResourceNames) != 0 || !reflect.DeepEqual(rule.APIGroups, []string{""}) || !reflect.DeepEqual(rule.APIVersions, []string{"v1"}) ||
		!reflect.DeepEqual(rule.Resources, []string{"pods/exec"}) || !reflect.DeepEqual(rule.Operations, []admissionv1.OperationType{admissionv1.Connect}) ||
		rule.Scope != nil && *rule.Scope != admissionv1.AllScopes && *rule.Scope != admissionv1.NamespacedScope {
		return false
	}
	return binding.Spec.PolicyName == policy.Name && binding.Spec.ParamRef == nil && binding.Spec.MatchResources == nil &&
		reflect.DeepEqual(binding.Spec.ValidationActions, []admissionv1.ValidationAction{admissionv1.Deny})
}

// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package terminalpolicy

import (
	"context"
	"fmt"
	"slices"

	"github.com/Azure/CetiRun/app/internal/chataccess"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func ValidateRoleBinding(object unstructured.Unstructured, namespace, account string) error {
	if object.GetAPIVersion() != rbacv1.SchemeGroupVersion.String() || object.GetKind() != "RoleBinding" ||
		object.GetLabels()[WorkloadLabel] != "true" || namespace == "" || account == "" {
		return fmt.Errorf("terminal binding requires its exact namespaced role contract")
	}
	var binding rbacv1.RoleBinding
	if err := runtime.DefaultUnstructuredConverter.FromUnstructuredWithValidation(object.Object, &binding, true); err != nil {
		return err
	}
	if binding.Namespace != namespace || !slices.Equal(binding.Subjects, []rbacv1.Subject{{
		Kind: "ServiceAccount", Name: account, Namespace: namespace,
	}}) {
		return fmt.Errorf("terminal binding subject differs from the platform plan")
	}
	role := rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: account}
	switch binding.Name {
	case account:
	case account + "-exec":
		role = rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: ExecRole}
	default:
		return fmt.Errorf("terminal binding name differs from the platform plan")
	}
	if binding.RoleRef != role {
		return fmt.Errorf("terminal binding role differs from the platform plan")
	}
	return nil
}

// OwnedBinding recognizes only the fixed terminal access roles, not Agent
// Kubernetes grants. A terminal label alone is never sufficient.
func OwnedBinding(ctx context.Context, reader client.Reader, object client.Object, agentName string, agentUID types.UID) (bool, error) {
	namespace, workload := "cetirun-"+agentName, chataccess.ServiceName(agentName)
	role, agentReader := ReadRoles(namespace, workload, agentName, string(agentUID))
	owned := equality.Semantic.DeepEqual(object.GetOwnerReferences(), role.OwnerReferences)
	switch binding := object.(type) {
	case *rbacv1.RoleBinding:
		if binding.Namespace != namespace || (binding.Name != role.Name && binding.Name != role.Name+"-exec") {
			return false, nil
		}
		if !owned || !binding.DeletionTimestamp.IsZero() {
			return true, fmt.Errorf("terminal binding ownership is not current")
		}
		raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(binding)
		if err != nil {
			return true, err
		}
		raw["apiVersion"], raw["kind"] = rbacv1.SchemeGroupVersion.String(), "RoleBinding"
		if err := ValidateRoleBinding(unstructured.Unstructured{Object: raw}, namespace, role.Name); err != nil {
			return true, err
		}
		current := &rbacv1.Role{}
		if err := reader.Get(ctx, client.ObjectKeyFromObject(role), current); err != nil {
			return true, err
		}
		if !terminalRoleOwned(current.ObjectMeta, role.ObjectMeta) || !equality.Semantic.DeepEqual(current.Rules, role.Rules) {
			return true, fmt.Errorf("terminal read Role differs from the fixed own-workload contract")
		}
		return true, nil
	case *rbacv1.ClusterRoleBinding:
		if binding.Name != agentReader.Name {
			return false, nil
		}
		if !owned || binding.Namespace != "" || !binding.DeletionTimestamp.IsZero() ||
			binding.Annotations["cetirun.microsoft.com/agent-uid"] != string(agentUID) ||
			binding.RoleRef != (rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: agentReader.Name}) ||
			!slices.Equal(binding.Subjects, []rbacv1.Subject{{Kind: "ServiceAccount", Namespace: namespace, Name: role.Name}}) {
			return true, fmt.Errorf("terminal Agent reader binding differs from the fixed own-Agent contract")
		}
		current := &rbacv1.ClusterRole{}
		if err := reader.Get(ctx, client.ObjectKeyFromObject(agentReader), current); err != nil {
			return true, err
		}
		if !terminalRoleOwned(current.ObjectMeta, agentReader.ObjectMeta) || !equality.Semantic.DeepEqual(current.Rules, agentReader.Rules) {
			return true, fmt.Errorf("terminal Agent reader Role differs from the fixed own-Agent contract")
		}
		return true, nil
	default:
		return false, nil
	}
}

func terminalRoleOwned(actual, expected metav1.ObjectMeta) bool {
	return actual.UID != "" && actual.DeletionTimestamp.IsZero() &&
		equality.Semantic.DeepEqual(actual.OwnerReferences, expected.OwnerReferences) &&
		actual.Annotations["cetirun.microsoft.com/agent-uid"] == expected.Annotations["cetirun.microsoft.com/agent-uid"]
}

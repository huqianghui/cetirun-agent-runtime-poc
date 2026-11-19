// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxworkload

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

type grantScope struct {
	namespace string
	role      string
}

// This checks the compiler's resource contract, not platform grant approval,
// role contents, identity issuance or the network rules themselves.
func validateObjects(input Input) error {
	c := input.Controls
	access := input.Resolution.AgentPolicy.KubernetesAccess
	grants := map[grantScope]bool{}
	if access != nil {
		if access.ServiceAccountName != c.ServiceAccountName {
			return fmt.Errorf("compiled Kubernetes bindings require the selected dedicated ServiceAccount")
		}
		for _, grant := range access.RoleBindings {
			grants[grantScope{namespace: grant.Namespace, role: grant.ClusterRoleName}] = false
		}
		for _, grant := range access.ClusterRoleBindings {
			grants[grantScope{role: grant.ClusterRoleName}] = false
		}
	}
	dedicated := access != nil || input.Resolution.AgentPolicy.EntraID.Enabled
	if !dedicated && (c.ServiceAccountName != "default" || len(c.Volumes)+len(c.VolumeMounts)+len(c.Environment) != 0) {
		return fmt.Errorf("ordinary workloads cannot acquire a dedicated identity or projections")
	}
	binding, err := json.Marshal(c.Binding)
	if err != nil {
		return err
	}
	owner := []metav1.OwnerReference{AgentOwner(input.AgentName, c.Binding.AgentUID)}
	objects := map[string]bool{}
	network, terminalNetwork, serviceAccounts := 0, 0, 0
	mainSelector := metav1.LabelSelector{MatchLabels: map[string]string{
		"k8s:" + sandboxpolicy.AgentUIDLabel: string(c.Binding.AgentUID), "k8s:io.kubernetes.pod.namespace": input.Namespace.Name,
	}}
	terminalSelector := metav1.LabelSelector{MatchLabels: map[string]string{
		"k8s:cetirun.microsoft.com/terminal": "true",
		"k8s:" + sandboxpolicy.AgentUIDLabel: string(c.Binding.AgentUID), "k8s:io.kubernetes.pod.namespace": input.Namespace.Name,
	}}
	if input.Terminal != nil {
		mainSelector.MatchExpressions = []metav1.LabelSelectorRequirement{{
			Key: "k8s:cetirun.microsoft.com/terminal", Operator: metav1.LabelSelectorOpDoesNotExist,
		}}
	}
	tlsObjects := map[string][]byte{}
	if c.IngressMTLS != nil || c.EgressSNI != nil {
		expected, err := sandboxpolicy.ManagedProxyResources(sandboxpolicy.CompileInput{
			AgentName: input.AgentName, Namespace: input.Namespace.Name, Resolution: *input.Resolution,
		}, c.IngressMTLS, c.EgressSNI, c.Revision)
		if err != nil {
			return err
		}
		for _, object := range expected {
			key := object.GetAPIVersion() + "/" + object.GetKind() + "/" + object.GetNamespace() + "/" + object.GetName()
			tlsObjects[key], _ = json.Marshal(object.Object)
		}
	}
	identity := sandboxpolicy.IdentityPlan{
		ServiceAccountName: c.ServiceAccountName, Volumes: c.Volumes, VolumeMounts: c.VolumeMounts, Env: c.Environment,
	}
	inventory := slices.Clone(c.Objects)
	if account := c.ServiceAccountSource; account != nil {
		if !input.Reconciled || (!input.Resolution.AgentPolicy.EntraID.Enabled && access == nil) ||
			account.UID == "" || account.Annotations["cetirun.microsoft.com/sandbox-bootstrap"] == "" {
			return fmt.Errorf("bootstrap identity source requires a pinned common identity workload")
		}
		raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(account)
		if err != nil {
			return err
		}
		inventory = append(inventory, unstructured.Unstructured{Object: raw})
	}
	for _, object := range inventory {
		kind := object.GetAPIVersion() + "/" + object.GetKind()
		key := kind + "/" + object.GetNamespace() + "/" + object.GetName()
		if objects[key] || object.GetName() == "" || !equality.Semantic.DeepEqual(object.GetOwnerReferences(), owner) ||
			object.GetLabels()[sandboxpolicy.AgentUIDLabel] != string(c.Binding.AgentUID) ||
			object.GetAnnotations()[sandboxpolicy.BindingAnnotation] != string(binding) ||
			object.GetAnnotations()[sandboxpolicy.RevisionAnnotation] != c.Revision {
			return fmt.Errorf("compiler resource identity, ownership or binding revision is invalid")
		}
		for annotation := range object.GetAnnotations() {
			if strings.HasPrefix(annotation, "helm.sh/") {
				return fmt.Errorf("compiler resources cannot contain Helm hooks")
			}
		}
		switch kind {
		case "v1/ConfigMap", "networking.istio.io/v1beta1/DestinationRule":
			encoded, err := json.Marshal(object.Object)
			if err != nil || string(tlsObjects[key]) != string(encoded) {
				return fmt.Errorf("managed proxy resource differs from the exact compiled controls")
			}
			delete(tlsObjects, key)
		case "cilium.io/v2/CiliumNetworkPolicy":
			var policy sandboxpolicy.CiliumNetworkPolicy
			if err := runtime.DefaultUnstructuredConverter.FromUnstructuredWithValidation(object.Object, &policy, true); err != nil {
				return fmt.Errorf("invalid compiler network resource: %w", err)
			}
			if policy.Namespace != input.Namespace.Name {
				return fmt.Errorf("compiler network scope must select only the bound workload")
			}
			switch {
			case equality.Semantic.DeepEqual(policy.Spec.EndpointSelector, mainSelector):
				network++
			case input.Terminal != nil && equality.Semantic.DeepEqual(policy.Spec.EndpointSelector, terminalSelector):
				terminalNetwork++
			default:
				return fmt.Errorf("compiler network scope must select only the bound workload")
			}
		case "v1/ServiceAccount":
			var account corev1.ServiceAccount
			if err := runtime.DefaultUnstructuredConverter.FromUnstructuredWithValidation(object.Object, &account, true); err != nil {
				return fmt.Errorf("invalid compiler ServiceAccount: %w", err)
			}
			if !dedicated || account.Name == "default" || account.Name != c.ServiceAccountName || account.Namespace != input.Namespace.Name ||
				account.AutomountServiceAccountToken == nil || *account.AutomountServiceAccountToken ||
				len(account.Secrets) != 0 || len(account.ImagePullSecrets) != 0 {
				return fmt.Errorf("compiler must emit only the bound dedicated ServiceAccount without implicit token or pull credentials")
			}
			serviceAccounts++
			identity.ServiceAccount = &account
		case "rbac.authorization.k8s.io/v1/RoleBinding":
			var grant rbacv1.RoleBinding
			if err := runtime.DefaultUnstructuredConverter.FromUnstructuredWithValidation(object.Object, &grant, true); err != nil {
				return fmt.Errorf("invalid compiler RoleBinding: %w", err)
			}
			if grant.Namespace == "" {
				return fmt.Errorf("compiled RoleBinding requires its explicit requested namespace")
			}
			if err := validateGrant(input, grant.ObjectMeta, grant.RoleRef, grant.Subjects, grants); err != nil {
				return err
			}
		case "rbac.authorization.k8s.io/v1/ClusterRoleBinding":
			var grant rbacv1.ClusterRoleBinding
			if err := runtime.DefaultUnstructuredConverter.FromUnstructuredWithValidation(object.Object, &grant, true); err != nil {
				return fmt.Errorf("invalid compiler ClusterRoleBinding: %w", err)
			}
			if grant.Namespace != "" {
				return fmt.Errorf("compiled ClusterRoleBinding must be cluster-scoped")
			}
			if err := validateGrant(input, grant.ObjectMeta, grant.RoleRef, grant.Subjects, grants); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported compiler resource kind %s", kind)
		}
		objects[key] = true
	}
	if network != 1 || (input.Terminal != nil && terminalNetwork != 1) || (input.Terminal == nil && terminalNetwork != 0) ||
		(dedicated && serviceAccounts != 1) || len(tlsObjects) != 0 {
		return fmt.Errorf("compiler resource inventory is incomplete or duplicated")
	}
	for _, present := range grants {
		if !present {
			return fmt.Errorf("compiler resource inventory omits a requested Kubernetes binding")
		}
	}
	return sandboxpolicy.ValidateIdentityProjection(input.Resolution.AgentPolicy, identity)
}

func validateGrant(input Input, metadata metav1.ObjectMeta, role rbacv1.RoleRef, subjects []rbacv1.Subject, grants map[grantScope]bool) error {
	expected := []rbacv1.Subject{{Kind: "ServiceAccount", Name: input.Controls.ServiceAccountName, Namespace: input.Namespace.Name}}
	if !equality.Semantic.DeepEqual(subjects, expected) || role.APIGroup != rbacv1.GroupName || role.Kind != "ClusterRole" ||
		metadata.Annotations[sandboxpolicy.GrantRevisionAnnotation] == "" {
		return fmt.Errorf("compiler binding must use the exact workload subject and a pinned ClusterRole")
	}
	scope := grantScope{namespace: metadata.Namespace, role: role.Name}
	if consumed, requested := grants[scope]; !requested || consumed {
		return fmt.Errorf("compiler binding changes or duplicates the requested ClusterRole scope")
	}
	grants[scope] = true
	return nil
}

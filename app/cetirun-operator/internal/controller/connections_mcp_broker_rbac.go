// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/internal/agenttools"
	"github.com/Azure/CetiRun/app/internal/registryauth"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	connectionsMCPBrokerRoleName        = "cetirun-agent-tools-broker"
	connectionsMCPBrokerRoleBindingName = "cetirun-agent-tools-broker"
)

// Only absence of the namespace permits the steady-state path to fall through
// to namespace creation. A missing Role or RoleBinding is not this condition.
var errConnectionsMCPBrokerNamespaceMissing = errors.New("Agent namespace is missing")

// ParseConnectionsMCPBrokerStorageConfig validates the optional external Broker
// identity that is granted access to individual Agent namespaces. The Broker
// gets no access unless both coordinates are explicitly configured.
func ParseConnectionsMCPBrokerStorageConfig(namespace, serviceAccount string) (bool, error) {
	if namespace == "" && serviceAccount == "" {
		return false, nil
	}
	if namespace == "" || serviceAccount == "" {
		return false, fmt.Errorf("Connections MCP Broker namespace and service account must be configured together")
	}
	if strings.TrimSpace(namespace) != namespace || len(validation.IsDNS1123Label(namespace)) != 0 {
		return false, fmt.Errorf("Connections MCP Broker namespace is invalid")
	}
	if strings.TrimSpace(serviceAccount) != serviceAccount || len(validation.IsDNS1123Subdomain(serviceAccount)) != 0 {
		return false, fmt.Errorf("Connections MCP Broker service account is invalid")
	}
	return true, nil
}

func (r *AgentReconciler) connectionsMCPBrokerStorageEnabled() (bool, error) {
	return ParseConnectionsMCPBrokerStorageConfig(r.ConnectionsMCPBrokerNamespace, r.ConnectionsMCPBrokerServiceAccount)
}

func connectionsMCPBrokerOwner(instance *api.Agent) []metav1.OwnerReference {
	return []metav1.OwnerReference{registryauth.Owner(instance)}
}

func connectionsMCPBrokerOwned(obj metav1.Object, instance *api.Agent) bool {
	owners := obj.GetOwnerReferences()
	if len(owners) != 1 {
		return false
	}
	owner := owners[0]
	return owner.APIVersion == api.GroupVersion.String() && owner.Kind == "Agent" &&
		owner.Name == instance.Name && owner.UID == instance.UID
}

func (r *AgentReconciler) connectionsMCPBrokerNamespaceOwned(ctx context.Context, instance *api.Agent) error {
	namespace := &corev1.Namespace{}
	key := types.NamespacedName{Name: instanceNamespace(instance.Name)}
	if err := r.Get(ctx, key, namespace); apierrors.IsNotFound(err) {
		return fmt.Errorf("%w: %w", errConnectionsMCPBrokerNamespaceMissing, err)
	} else if err != nil {
		return fmt.Errorf("get Agent namespace: %w", err)
	}
	if !namespace.DeletionTimestamp.IsZero() {
		return fmt.Errorf("Agent namespace %q is terminating", namespace.Name)
	}
	return r.validateConnectionsMCPBrokerNamespaceOwner(ctx, namespace, instance)
}

func validateConnectionsMCPBrokerNamespaceOwner(namespace *corev1.Namespace, instance *api.Agent) error {
	if !connectionsMCPBrokerOwned(namespace, instance) {
		return fmt.Errorf("Agent namespace %q must be owned by Agent %q (UID %q); automatic namespace adoption is not supported; verify ownership before manual repair", namespace.Name, instance.Name, instance.UID)
	}
	return nil
}

// validateConnectionsMCPBrokerNamespaceOwner accepts the ordinary Custom
// namespace contract only when its immutable operator receipt proves the same
// Agent and namespace UIDs. It deliberately does not add an OwnerReference to
// Custom namespaces, because that would change their deletion ordering.
func (r *AgentReconciler) validateConnectionsMCPBrokerNamespaceOwner(ctx context.Context, namespace *corev1.Namespace, instance *api.Agent) error {
	if connectionsMCPBrokerOwned(namespace, instance) {
		return nil
	}
	if instance.Spec.Agent.Type != api.AgentTypeCustom {
		return validateConnectionsMCPBrokerNamespaceOwner(namespace, instance)
	}
	receipt, err := r.readCustomOwnership(ctx, instance)
	if err != nil {
		return validateConnectionsMCPBrokerNamespaceOwner(namespace, instance)
	}
	if namespace.Annotations[customAgentUID] != string(instance.UID) || namespace.Name != receipt.Namespace || namespace.UID != receipt.NamespaceUID {
		return fmt.Errorf("Custom Agent namespace ownership receipt does not match the namespace")
	}
	return nil
}

func (r *AgentReconciler) customOwnershipReceiptRBACName(instance *api.Agent) string {
	return "cetirun-custom-ownership-" + strings.TrimPrefix(agenttools.CustomOwnershipRecordName(string(instance.UID)), "custom-ownership-")
}

func (r *AgentReconciler) desiredCustomOwnershipReceiptBrokerRBAC(instance *api.Agent) (*rbacv1.Role, *rbacv1.RoleBinding) {
	name := r.customOwnershipReceiptRBACName(instance)
	receipt := agenttools.CustomOwnershipRecordName(string(instance.UID))
	owner := connectionsMCPBrokerOwner(instance)
	namespace := r.OperatorNamespace
	if namespace == "" {
		namespace = "cetirun-system"
	}
	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, OwnerReferences: owner,
		Labels: map[string]string{"app.kubernetes.io/managed-by": "cetirun-operator"}}, Rules: []rbacv1.PolicyRule{{
		APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{receipt}, Verbs: []string{"get"},
	}}}
	binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, OwnerReferences: owner,
		Labels: map[string]string{"app.kubernetes.io/managed-by": "cetirun-operator"}}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name},
		Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Namespace: r.ConnectionsMCPBrokerNamespace, Name: r.ConnectionsMCPBrokerServiceAccount}}}
	return role, binding
}

func (r *AgentReconciler) ensureCustomOwnershipReceiptBrokerRBAC(ctx context.Context, instance *api.Agent) error {
	if instance.Spec.Agent.Type != api.AgentTypeCustom {
		return nil
	}
	// A governed Custom Agent does not have this receipt. An ordinary Custom
	// Agent always does, and must retain the receipt grant even if an unexpected
	// OwnerReference appears on its namespace: the Broker never accepts that as
	// a substitute for the receipt proof.
	if _, err := r.readCustomOwnership(ctx, instance); apierrors.IsNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}
	desiredRole, desiredBinding := r.desiredCustomOwnershipReceiptBrokerRBAC(instance)
	role := &rbacv1.Role{}
	roleKey := client.ObjectKeyFromObject(desiredRole)
	if err := r.Get(ctx, roleKey, role); apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desiredRole); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create Custom ownership receipt Broker Role: %w", err)
		}
		if err := r.Get(ctx, roleKey, role); err != nil {
			return fmt.Errorf("read Custom ownership receipt Broker Role after create: %w", err)
		}
	} else if err != nil {
		return err
	}
	if !connectionsMCPBrokerOwned(role, instance) {
		return fmt.Errorf("Custom ownership receipt Broker Role ownership is invalid")
	}
	if !equality.Semantic.DeepEqual(role.Rules, desiredRole.Rules) || !equality.Semantic.DeepEqual(role.Labels, desiredRole.Labels) {
		role.Rules, role.Labels = desiredRole.Rules, desiredRole.Labels
		if err := r.Update(ctx, role); err != nil {
			return err
		}
	}
	binding := &rbacv1.RoleBinding{}
	bindingKey := client.ObjectKeyFromObject(desiredBinding)
	if err := r.Get(ctx, bindingKey, binding); apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desiredBinding); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create Custom ownership receipt Broker RoleBinding: %w", err)
		}
		if err := r.Get(ctx, bindingKey, binding); err != nil {
			return fmt.Errorf("read Custom ownership receipt Broker RoleBinding after create: %w", err)
		}
	} else if err != nil {
		return err
	}
	if !connectionsMCPBrokerOwned(binding, instance) {
		return fmt.Errorf("Custom ownership receipt Broker RoleBinding ownership is invalid")
	}
	if !equality.Semantic.DeepEqual(binding.RoleRef, desiredBinding.RoleRef) {
		return fmt.Errorf("Custom ownership receipt Broker RoleBinding role reference is invalid")
	}
	if !equality.Semantic.DeepEqual(binding.Subjects, desiredBinding.Subjects) || !equality.Semantic.DeepEqual(binding.Labels, desiredBinding.Labels) {
		binding.Subjects, binding.Labels = desiredBinding.Subjects, desiredBinding.Labels
		if err := r.Update(ctx, binding); err != nil {
			return err
		}
	}
	return nil
}

func (r *AgentReconciler) desiredConnectionsMCPBrokerRBAC(instance *api.Agent) (*rbacv1.Role, *rbacv1.RoleBinding) {
	namespace := instanceNamespace(instance.Name)
	owner := connectionsMCPBrokerOwner(instance)
	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{
		Name:            connectionsMCPBrokerRoleName,
		Namespace:       namespace,
		OwnerReferences: owner,
		Labels:          map[string]string{"app.kubernetes.io/managed-by": "cetirun-operator"},
	}, Rules: []rbacv1.PolicyRule{{
		APIGroups: []string{""}, Resources: []string{"secrets"},
		Verbs: []string{"get", "list", "create", "update", "delete"},
	}}}
	binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{
		Name:            connectionsMCPBrokerRoleBindingName,
		Namespace:       namespace,
		OwnerReferences: owner,
		Labels:          map[string]string{"app.kubernetes.io/managed-by": "cetirun-operator"},
	}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name},
		Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Namespace: r.ConnectionsMCPBrokerNamespace, Name: r.ConnectionsMCPBrokerServiceAccount}}}
	return role, binding
}

func (r *AgentReconciler) ensureConnectionsMCPBrokerRBAC(ctx context.Context, instance *api.Agent) error {
	if err := r.connectionsMCPBrokerNamespaceOwned(ctx, instance); err != nil {
		return err
	}
	if err := r.ensureCustomOwnershipReceiptBrokerRBAC(ctx, instance); err != nil {
		return err
	}
	desiredRole, desiredBinding := r.desiredConnectionsMCPBrokerRBAC(instance)
	role := &rbacv1.Role{}
	roleKey := types.NamespacedName{Namespace: desiredRole.Namespace, Name: desiredRole.Name}
	if err := r.Get(ctx, roleKey, role); apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desiredRole); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create Broker Role: %w", err)
		}
		if err := r.Get(ctx, roleKey, role); err != nil {
			return fmt.Errorf("read Broker Role after create: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("get Broker Role: %w", err)
	}
	if !connectionsMCPBrokerOwned(role, instance) {
		return fmt.Errorf("Broker Role ownership is invalid")
	}
	if !equality.Semantic.DeepEqual(role.Rules, desiredRole.Rules) || !equality.Semantic.DeepEqual(role.Labels, desiredRole.Labels) {
		role.Rules = desiredRole.Rules
		role.Labels = desiredRole.Labels
		if err := r.Update(ctx, role); err != nil {
			return fmt.Errorf("update Broker Role: %w", err)
		}
	}
	binding := &rbacv1.RoleBinding{}
	bindingKey := types.NamespacedName{Namespace: desiredBinding.Namespace, Name: desiredBinding.Name}
	if err := r.Get(ctx, bindingKey, binding); apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desiredBinding); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create Broker RoleBinding: %w", err)
		}
		if err := r.Get(ctx, bindingKey, binding); err != nil {
			return fmt.Errorf("read Broker RoleBinding after create: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("get Broker RoleBinding: %w", err)
	}
	if !connectionsMCPBrokerOwned(binding, instance) {
		return fmt.Errorf("Broker RoleBinding ownership is invalid")
	}
	if !equality.Semantic.DeepEqual(binding.RoleRef, desiredBinding.RoleRef) {
		return fmt.Errorf("Broker RoleBinding role reference is invalid")
	}
	if !equality.Semantic.DeepEqual(binding.Subjects, desiredBinding.Subjects) || !equality.Semantic.DeepEqual(binding.Labels, desiredBinding.Labels) {
		binding.Subjects = desiredBinding.Subjects
		binding.Labels = desiredBinding.Labels
		if err := r.Update(ctx, binding); err != nil {
			return fmt.Errorf("update Broker RoleBinding: %w", err)
		}
	}
	return nil
}

// reconcileConnectionsMCPBrokerRBAC grants the Broker access to Agent-scoped records
// in a normal built-in Agent namespace. This is independent of whether that
// Agent runtime receives a Connections MCP endpoint: the Agent finalizer still relies
// on the Broker to clean existing records before the Agent-owned namespace and
// its RBAC are garbage collected. A later disable is deliberately a no-op.
func (r *AgentReconciler) reconcileConnectionsMCPBrokerRBAC(ctx context.Context, instance *api.Agent, namespaceEligible bool) error {
	configured, err := r.connectionsMCPBrokerStorageEnabled()
	if err != nil {
		return err
	}
	if !configured || !namespaceEligible {
		return nil
	}
	return r.ensureConnectionsMCPBrokerRBAC(ctx, instance)
}

// ensureConnectionsMCPBrokerCleanupRBAC establishes cleanup access without creating
// or adopting a namespace solely for deletion. A live Agent-owned namespace can
// receive the narrowly scoped grant before the Broker is asked to remove its
// records. A namespace already terminating is read-only: existing RBAC must be
// correct, but the finalizer never races namespace garbage collection by
// creating or changing objects there. A missing namespace is an idempotent
// cleanup success because it cannot hold Agent-scoped Secrets.
func (r *AgentReconciler) ensureConnectionsMCPBrokerCleanupRBAC(ctx context.Context, instance *api.Agent) error {
	configured, err := r.connectionsMCPBrokerStorageEnabled()
	if err != nil || !configured {
		return err
	}
	namespace := &corev1.Namespace{}
	key := types.NamespacedName{Name: instanceNamespace(instance.Name)}
	if err := r.Get(ctx, key, namespace); apierrors.IsNotFound(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("get Agent namespace for Broker cleanup: %w", err)
	}
	if err := r.validateConnectionsMCPBrokerNamespaceOwner(ctx, namespace, instance); err != nil {
		return err
	}
	if namespace.DeletionTimestamp.IsZero() {
		return r.ensureConnectionsMCPBrokerRBAC(ctx, instance)
	}
	if err := r.verifyConnectionsMCPBrokerRBAC(ctx, instance); err != nil {
		return err
	}
	if instance.Spec.Agent.Type == api.AgentTypeCustom {
		if _, err := r.readCustomOwnership(ctx, instance); err == nil {
			return r.verifyCustomOwnershipReceiptBrokerRBAC(ctx, instance)
		} else if !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (r *AgentReconciler) verifyCustomOwnershipReceiptBrokerRBAC(ctx context.Context, instance *api.Agent) error {
	desiredRole, desiredBinding := r.desiredCustomOwnershipReceiptBrokerRBAC(instance)
	role := &rbacv1.Role{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(desiredRole), role); err != nil {
		return fmt.Errorf("get Custom ownership receipt Broker Role during namespace deletion: %w", err)
	}
	if !connectionsMCPBrokerOwned(role, instance) || !equality.Semantic.DeepEqual(role.Rules, desiredRole.Rules) {
		return fmt.Errorf("Custom ownership receipt Broker Role is invalid during namespace deletion")
	}
	binding := &rbacv1.RoleBinding{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(desiredBinding), binding); err != nil {
		return fmt.Errorf("get Custom ownership receipt Broker RoleBinding during namespace deletion: %w", err)
	}
	if !connectionsMCPBrokerOwned(binding, instance) || !equality.Semantic.DeepEqual(binding.RoleRef, desiredBinding.RoleRef) ||
		!equality.Semantic.DeepEqual(binding.Subjects, desiredBinding.Subjects) {
		return fmt.Errorf("Custom ownership receipt Broker RoleBinding is invalid during namespace deletion")
	}
	return nil
}

func (r *AgentReconciler) verifyConnectionsMCPBrokerRBAC(ctx context.Context, instance *api.Agent) error {
	desiredRole, desiredBinding := r.desiredConnectionsMCPBrokerRBAC(instance)
	role := &rbacv1.Role{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: desiredRole.Namespace, Name: desiredRole.Name}, role); err != nil {
		return fmt.Errorf("get Broker Role during namespace deletion: %w", err)
	}
	if !connectionsMCPBrokerOwned(role, instance) || !equality.Semantic.DeepEqual(role.Rules, desiredRole.Rules) {
		return fmt.Errorf("Broker Role is invalid during namespace deletion")
	}
	binding := &rbacv1.RoleBinding{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: desiredBinding.Namespace, Name: desiredBinding.Name}, binding); err != nil {
		return fmt.Errorf("get Broker RoleBinding during namespace deletion: %w", err)
	}
	if !connectionsMCPBrokerOwned(binding, instance) ||
		!equality.Semantic.DeepEqual(binding.RoleRef, desiredBinding.RoleRef) ||
		!equality.Semantic.DeepEqual(binding.Subjects, desiredBinding.Subjects) {
		return fmt.Errorf("Broker RoleBinding is invalid during namespace deletion")
	}
	return nil
}

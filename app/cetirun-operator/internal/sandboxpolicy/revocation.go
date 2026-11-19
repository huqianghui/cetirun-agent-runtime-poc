// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxpolicy

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/internal/terminalpolicy"
)

// RevokeBindings deletes only the exact owned per-Agent bindings in inventory.
// Deletion requests are not completion: terminating bindings keep returning an
// error until absence is observed. The caller retains inventory and the existing
// operator finalizer on every error, including Custom and Sandbox-held paths.
// Shared Roles, ClusterRoles, ServiceAccounts and cloud identities are untouched.
// The client must read directly from the API server. Cached controller clients
// must instead use RevokeBindingsWithReader with the manager's APIReader.
func RevokeBindings(ctx context.Context, c client.Client, inventory []BindingReference) error {
	return RevokeBindingsWithReader(ctx, c, c, inventory)
}

// RevokeBindingsWithReader separates authoritative absence/ownership checks
// from writes. A cached NotFound must never authorize finalizer removal.
func RevokeBindingsWithReader(ctx context.Context, reader client.Reader, writer client.Client, inventory []BindingReference) error {
	if reader == nil || writer == nil {
		return fmt.Errorf("%w: authoritative binding reader and writer are required", ErrUnavailable)
	}
	var failures []error
	for _, ref := range canonicalBindingReferences(inventory) {
		if err := revokeBinding(ctx, reader, writer, ref); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func revokeBinding(ctx context.Context, reader client.Reader, writer client.Client, ref BindingReference) error {
	object, err := readOwnedBinding(ctx, reader, ref)
	if err != nil || object == nil {
		return err
	}
	if object.GetDeletionTimestamp().IsZero() {
		uid, version := object.GetUID(), object.GetResourceVersion()
		if err := writer.Delete(ctx, object, client.Preconditions{UID: &uid, ResourceVersion: &version}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("revoke sandbox %s: %w", ref.Kind, err)
		}
	}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
		return client.IgnoreNotFound(err)
	}
	return fmt.Errorf("%w: %s still exists", ErrRevocationPending, ref.Kind)
}

func readOwnedBinding(ctx context.Context, reader client.Reader, ref BindingReference) (client.Object, error) {
	if ref.AgentUID == "" || len(validation.IsDNS1123Subdomain(ref.AgentName)) != 0 ||
		len(validation.IsDNS1123Subdomain(ref.Name)) != 0 ||
		len(validation.IsDNS1123Subdomain(ref.ServiceAccountName)) != 0 ||
		len(validation.IsDNS1123Label(ref.ServiceAccountNamespace)) != 0 ||
		ref.RoleRef.APIGroup != rbacv1.GroupName || ref.RoleRef.Name == "" ||
		(ref.RoleRef.Kind != "Role" && ref.RoleRef.Kind != "ClusterRole") {
		return nil, fmt.Errorf("%w: incomplete binding inventory", ErrOwnership)
	}
	var object client.Object
	switch ref.Kind {
	case "RoleBinding":
		if len(validation.IsDNS1123Label(ref.Namespace)) != 0 {
			return nil, fmt.Errorf("%w: RoleBinding namespace is required", ErrOwnership)
		}
		object = &rbacv1.RoleBinding{}
	case "ClusterRoleBinding":
		if ref.Namespace != "" || ref.RoleRef.Kind != "ClusterRole" {
			return nil, fmt.Errorf("%w: invalid cluster-scoped binding inventory", ErrOwnership)
		}
		object = &rbacv1.ClusterRoleBinding{}
	default:
		return nil, fmt.Errorf("%w: inventory is restricted to RBAC bindings", ErrOwnership)
	}
	key := types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}
	if err := reader.Get(ctx, key, object); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	if !bindingOwned(object, ref.AgentName, ref.AgentUID) ||
		(ref.UID != "" && ref.UID != object.GetUID()) ||
		(ref.GrantRevision != "" && ref.GrantRevision != object.GetAnnotations()[GrantRevisionAnnotation]) {
		return nil, fmt.Errorf("%w: refusing a foreign or replaced %s", ErrOwnership, ref.Kind)
	}
	var role rbacv1.RoleRef
	var subjects []rbacv1.Subject
	switch binding := object.(type) {
	case *rbacv1.RoleBinding:
		role, subjects = binding.RoleRef, binding.Subjects
	case *rbacv1.ClusterRoleBinding:
		role, subjects = binding.RoleRef, binding.Subjects
	}
	expected := rbacv1.Subject{Kind: "ServiceAccount", Name: ref.ServiceAccountName, Namespace: ref.ServiceAccountNamespace}
	if role != ref.RoleRef || len(subjects) != 1 || subjects[0] != expected {
		return nil, fmt.Errorf("%w: binding contents differ from recorded inventory", ErrOwnership)
	}
	if object.GetUID() == "" || object.GetResourceVersion() == "" {
		return nil, fmt.Errorf("%w: persisted binding identity is required", ErrOwnership)
	}
	return object, nil
}

func bindingOwned(object client.Object, name string, uid types.UID) bool {
	if object.GetLabels()[AgentUIDLabel] != string(uid) {
		return false
	}
	owners := object.GetOwnerReferences()
	if len(owners) != 1 {
		return false
	}
	owner := owners[0]
	return owner.APIVersion == api.GroupVersion.String() && owner.Kind == "Agent" &&
		owner.Name == name && owner.UID == uid && owner.Controller != nil && *owner.Controller
}

// ObsoleteBindings is the inventory to revoke before staging a changed identity.
// Binding object UIDs are deliberately ignored when comparing desired entries.
func ObsoleteBindings(previous, desired []BindingReference) []BindingReference {
	var obsolete []BindingReference
	for _, old := range previous {
		expected := old
		expected.UID = ""
		if !slices.ContainsFunc(desired, func(next BindingReference) bool {
			next.UID = ""
			return next == expected
		}) {
			obsolete = append(obsolete, old)
		}
	}
	return canonicalBindingReferences(obsolete)
}

// BindingReference is entirely comparable; sorting it does not need JSON or an
// error-returning change to the durable revocation inventory API.
func canonicalBindingReferences(values []BindingReference) []BindingReference {
	result := slices.Clone(values)
	slices.SortFunc(result, func(a, b BindingReference) int {
		return cmp.Or(
			cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Namespace, b.Namespace),
			cmp.Compare(a.Name, b.Name), cmp.Compare(a.UID, b.UID),
			cmp.Compare(a.AgentName, b.AgentName), cmp.Compare(a.AgentUID, b.AgentUID),
			cmp.Compare(a.ServiceAccountName, b.ServiceAccountName),
			cmp.Compare(a.ServiceAccountNamespace, b.ServiceAccountNamespace),
			cmp.Compare(a.RoleRef.APIGroup, b.RoleRef.APIGroup), cmp.Compare(a.RoleRef.Kind, b.RoleRef.Kind),
			cmp.Compare(a.RoleRef.Name, b.RoleRef.Name), cmp.Compare(a.GrantRevision, b.GrantRevision),
		)
	})
	return slices.Compact(result)
}

// OwnedBindingInventory supports restart/deletion recovery across namespaces.
// It searches owner references as well as labels so label removal cannot silently
// hide a grant. An inconsistent owner/label/subject blocks cleanup rather than
// authorizing adoption. Persisted inventory is still needed before writes and
// for change/rebind revocation, including resources not currently listable.
// Pass the manager's APIReader, not an eventually consistent cache.
func OwnedBindingInventory(ctx context.Context, c client.Reader, agentName string, agentUID types.UID) ([]BindingReference, error) {
	return ownedBindingInventory(ctx, c, nil, agentName, agentUID, false)
}

// OwnedBindingInventoryWithPlatformAccess recognizes a separate, strictly
// validated platform access contract. The verifier must be bound to deployment
// configuration; callers must not derive its authority from a candidate binding.
func OwnedBindingInventoryWithPlatformAccess(ctx context.Context, c client.Reader, verify PlatformAccessVerifier, agentName string, agentUID types.UID) ([]BindingReference, error) {
	return ownedBindingInventory(ctx, c, verify, agentName, agentUID, false)
}

func ownedBindingInventory(ctx context.Context, c client.Reader, verify PlatformAccessVerifier, agentName string, agentUID types.UID, clusterRolesOnly bool) ([]BindingReference, error) {
	if c == nil || agentUID == "" || len(validation.IsDNS1123Subdomain(agentName)) != 0 {
		return nil, fmt.Errorf("%w: an Agent identity and binding client are required", ErrOwnership)
	}
	var roleBindings rbacv1.RoleBindingList
	var clusterBindings rbacv1.ClusterRoleBindingList
	if err := c.List(ctx, &roleBindings); err != nil {
		return nil, fmt.Errorf("list namespaced sandbox grants: %w", err)
	}
	if err := c.List(ctx, &clusterBindings); err != nil {
		return nil, fmt.Errorf("list cluster sandbox grants: %w", err)
	}
	var inventory []BindingReference
	add := func(object client.Object, kind string, role rbacv1.RoleRef, subjects []rbacv1.Subject) error {
		if clusterRolesOnly && role.Kind != "ClusterRole" {
			return nil
		}
		relevant := object.GetLabels()[AgentUIDLabel] == string(agentUID)
		for _, owner := range object.GetOwnerReferences() {
			relevant = relevant || owner.UID == agentUID
		}
		if !relevant {
			return nil
		}
		if verify != nil {
			if owned, err := verify(ctx, c, object, agentName, agentUID); err != nil {
				return fmt.Errorf("%w: platform access binding is not confirmed: %w", ErrOwnership, err)
			} else if owned {
				return nil
			}
		}
		if terminal, err := terminalpolicy.OwnedBinding(ctx, c, object, agentName, agentUID); err != nil {
			return fmt.Errorf("%w: terminal access binding is not confirmed: %w", ErrOwnership, err)
		} else if terminal {
			return nil
		}
		if !bindingOwned(object, agentName, agentUID) || len(subjects) != 1 ||
			subjects[0].Kind != "ServiceAccount" || subjects[0].APIGroup != "" {
			return fmt.Errorf("%w: inconsistent owned binding during inventory recovery", ErrOwnership)
		}
		inventory = append(inventory, BindingReference{
			Kind: kind, Namespace: object.GetNamespace(), Name: object.GetName(), UID: object.GetUID(),
			AgentName: agentName, AgentUID: agentUID, ServiceAccountName: subjects[0].Name,
			ServiceAccountNamespace: subjects[0].Namespace, RoleRef: role,
			GrantRevision: object.GetAnnotations()[GrantRevisionAnnotation],
		})
		return nil
	}
	for i := range roleBindings.Items {
		object := &roleBindings.Items[i]
		if err := add(object, "RoleBinding", object.RoleRef, object.Subjects); err != nil {
			return nil, err
		}
	}
	for i := range clusterBindings.Items {
		object := &clusterBindings.Items[i]
		if err := add(object, "ClusterRoleBinding", object.RoleRef, object.Subjects); err != nil {
			return nil, err
		}
	}
	return canonicalBindingReferences(inventory), nil
}

// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxpolicy

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/internal/chataccess"
	"github.com/Azure/CetiRun/app/internal/terminalpolicy"
)

func revocationFixture(t *testing.T) (*Plan, client.Client) {
	t.Helper()
	plan := mustCompile(t, adminFixture())
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{api.AddToScheme, corev1.AddToScheme, rbacv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	objects := []client.Object{
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "view", UID: "shared-view"}},
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "read-nodes", UID: "shared-nodes"}},
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "shared-role", Namespace: "operations", UID: "shared-role"}},
		plan.Identity.ServiceAccount.DeepCopy(),
	}
	for i := range plan.Identity.RoleBindings {
		object := plan.Identity.RoleBindings[i].DeepCopy()
		object.UID = types.UID("persisted-role-binding")
		objects = append(objects, object)
	}
	for i := range plan.Identity.ClusterRoleBindings {
		object := plan.Identity.ClusterRoleBindings[i].DeepCopy()
		object.UID = types.UID("persisted-cluster-binding")
		objects = append(objects, object)
	}
	return plan, fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func TestOwnedBindingInventoryKeepsTerminalAuthoritySeparate(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(map[bool]string{false: "fixed terminal", true: "broadened terminal"}[invalid], func(t *testing.T) {
			plan, kube := revocationFixture(t)
			ctx := context.Background()
			role, reader := terminalpolicy.ReadRoles("cetirun-"+plan.AgentName,
				chataccess.ServiceName(plan.AgentName), plan.AgentName, string(plan.Binding.AgentUID))
			role.UID, reader.UID = "terminal-role-uid", "terminal-reader-uid"
			for _, object := range []client.Object{role, reader} {
				if err := kube.Create(ctx, object); err != nil {
					t.Fatal(err)
				}
			}
			meta := *role.ObjectMeta.DeepCopy()
			meta.UID, meta.ResourceVersion = "terminal-binding-uid", ""
			meta.Name += "-exec"
			meta.Labels[terminalpolicy.WorkloadLabel] = "true"
			exec := &rbacv1.RoleBinding{ObjectMeta: meta,
				Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Namespace: role.Namespace, Name: role.Name}},
				RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: terminalpolicy.ExecRole}}
			readerMeta := *reader.ObjectMeta.DeepCopy()
			readerMeta.UID, readerMeta.ResourceVersion = "terminal-reader-binding-uid", ""
			readerBinding := &rbacv1.ClusterRoleBinding{ObjectMeta: readerMeta, Subjects: exec.Subjects,
				RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: reader.Name}}
			if invalid {
				exec.RoleRef.Name = "cluster-admin"
			}
			for _, object := range []client.Object{exec, readerBinding} {
				if err := kube.Create(ctx, object); err != nil {
					t.Fatal(err)
				}
			}
			inventory, err := OwnedBindingInventory(ctx, kube, plan.AgentName, plan.Binding.AgentUID)
			if invalid {
				if !errors.Is(err, ErrOwnership) {
					t.Fatal("terminal marker concealed an unapproved grant:", err)
				}
				return
			}
			if err != nil || len(inventory) != 2 {
				t.Fatal("terminal access entered the Agent grant cleanup inventory:", err)
			}
			if err := RevokeBindings(ctx, kube, inventory); err != nil {
				t.Fatal(err)
			}
			for _, object := range []client.Object{exec, readerBinding} {
				if err := kube.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
					t.Fatal("Agent grant revocation deleted the separate terminal authority:", err)
				}
			}
		})
	}
}

func TestRevokeOwnedBindingsIsIdempotentAndPreservesSharedResources(t *testing.T) {
	plan, c := revocationFixture(t)
	ctx := context.Background()
	inventory, err := OwnedBindingInventory(ctx, c, plan.AgentName, plan.Binding.AgentUID)
	if err != nil || len(inventory) != 2 {
		t.Fatalf("expected cross-namespace and cluster inventory: %+v, %v", inventory, err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := RevokeBindings(ctx, c, inventory); err != nil {
			t.Fatal(err)
		}
	}
	if remaining, err := OwnedBindingInventory(ctx, c, plan.AgentName, plan.Binding.AgentUID); err != nil || len(remaining) != 0 {
		t.Fatalf("revocation did not observe absence: %v, %v", remaining, err)
	}
	for _, object := range []client.Object{
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "view"}},
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "read-nodes"}},
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "shared-role", Namespace: "operations"}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: plan.Identity.ServiceAccountName, Namespace: plan.Namespace}},
	} {
		if err := c.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
			t.Fatalf("revocation removed a shared role or identity resource: %v", err)
		}
	}
}

func TestRevokeBindingsRejectsForeignOwnershipAndReplacement(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*rbacv1.RoleBinding)
	}{
		{"wrong owner UID", func(b *rbacv1.RoleBinding) { b.OwnerReferences[0].UID = "foreign-agent" }},
		{"label alone", func(b *rbacv1.RoleBinding) { b.OwnerReferences = nil }},
		{"missing protected label", func(b *rbacv1.RoleBinding) { b.Labels = nil }},
		{"extra owner", func(b *rbacv1.RoleBinding) {
			b.OwnerReferences = append(b.OwnerReferences, metav1.OwnerReference{UID: "foreign-owner"})
		}},
		{"changed role", func(b *rbacv1.RoleBinding) { b.RoleRef.Name = "unrecorded-role" }},
		{"new grant approval", func(b *rbacv1.RoleBinding) { b.Annotations[GrantRevisionAnnotation] = "replacement-approval" }},
		{"changed subject", func(b *rbacv1.RoleBinding) { b.Subjects[0].Namespace = "foreign-namespace" }},
		{"additional subject", func(b *rbacv1.RoleBinding) {
			b.Subjects = append(b.Subjects, rbacv1.Subject{Kind: "User", Name: "someone"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, c := revocationFixture(t)
			ref := plan.Identity.Bindings[0]
			for _, candidate := range plan.Identity.Bindings {
				if candidate.Kind == "RoleBinding" {
					ref = candidate
				}
			}
			key := types.NamespacedName{Name: ref.Name, Namespace: ref.Namespace}
			binding := &rbacv1.RoleBinding{}
			if err := c.Get(context.Background(), key, binding); err != nil {
				t.Fatal(err)
			}
			tc.mutate(binding)
			if err := c.Update(context.Background(), binding); err != nil {
				t.Fatal(err)
			}
			if err := RevokeBindings(context.Background(), c, []BindingReference{ref}); !errors.Is(err, ErrOwnership) {
				t.Fatal("foreign binding was not rejected", err)
			}
			if err := c.Get(context.Background(), key, &rbacv1.RoleBinding{}); err != nil {
				t.Fatal("foreign binding was removed", err)
			}
		})
	}
	plan, c := revocationFixture(t)
	inventory, err := OwnedBindingInventory(context.Background(), c, plan.AgentName, plan.Binding.AgentUID)
	if err != nil {
		t.Fatal(err)
	}
	inventory[0].UID = "previous-binding-object"
	if err := RevokeBindings(context.Background(), c, inventory[:1]); !errors.Is(err, ErrOwnership) {
		t.Fatal("same-name replacement must not be revoked using stale UID inventory", err)
	}
}

func TestRevocationFailuresRemainRetriable(t *testing.T) {
	plan, c := revocationFixture(t)
	inventory, err := OwnedBindingInventory(context.Background(), c, plan.AgentName, plan.Binding.AgentUID)
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("synthetic deletion failure")
	failing := &revocationErrorClient{Client: c, deleteError: failure}
	if err := RevokeBindings(context.Background(), failing, inventory); !errors.Is(err, failure) || failing.deletes != 2 {
		t.Fatalf("deletion failure was lost or remaining cleanup not attempted: %v, deletes=%d", err, failing.deletes)
	}
	failing.deleteError = nil
	if err := RevokeBindings(context.Background(), failing, inventory); err != nil {
		t.Fatal(err)
	}
	failing.listError = failure
	if _, err := OwnedBindingInventory(context.Background(), failing, plan.AgentName, plan.Binding.AgentUID); !errors.Is(err, failure) {
		t.Fatal("failed inventory read became empty success", err)
	}
}

func TestRevocationWaitsForObservedAbsence(t *testing.T) {
	plan, c := revocationFixture(t)
	ref := plan.Identity.Bindings[0]
	for _, candidate := range plan.Identity.Bindings {
		if candidate.Kind == "RoleBinding" {
			ref = candidate
		}
	}
	key := types.NamespacedName{Name: ref.Name, Namespace: ref.Namespace}
	binding := &rbacv1.RoleBinding{}
	if err := c.Get(context.Background(), key, binding); err != nil {
		t.Fatal(err)
	}
	binding.Finalizers = []string{"example.com/pending-cleanup"}
	if err := c.Update(context.Background(), binding); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := RevokeBindings(context.Background(), c, []BindingReference{ref}); !errors.Is(err, ErrRevocationPending) {
			t.Fatal("a deletion request was mistaken for completed revocation", err)
		}
	}
	if err := c.Get(context.Background(), key, binding); err != nil {
		t.Fatal(err)
	}
	binding.Finalizers = nil
	if err := c.Update(context.Background(), binding); err != nil {
		t.Fatal(err)
	}
	if err := RevokeBindings(context.Background(), c, []BindingReference{ref}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), key, binding); !apierrors.IsNotFound(err) {
		t.Fatal("binding remains after confirmed revocation", err)
	}
}

func TestInventoryRejectsHiddenOrSpoofedOwnership(t *testing.T) {
	plan, c := revocationFixture(t)
	role := &rbacv1.RoleBinding{}
	key := client.ObjectKeyFromObject(&plan.Identity.RoleBindings[0])
	if err := c.Get(context.Background(), key, role); err != nil {
		t.Fatal(err)
	}
	role.Labels = nil
	if err := c.Update(context.Background(), role); err != nil {
		t.Fatal(err)
	}
	if _, err := OwnedBindingInventory(context.Background(), c, plan.AgentName, plan.Binding.AgentUID); !errors.Is(err, ErrOwnership) {
		t.Fatal("label removal silently hid an owned grant from cleanup", err)
	}
}

func TestObsoleteBindingsIncludesDowngradeRebindAndRoleChange(t *testing.T) {
	plan := mustCompile(t, adminFixture())
	old := append([]BindingReference(nil), plan.Identity.Bindings...)
	for i := range old {
		old[i].UID = "observed-binding"
	}

	if got := ObsoleteBindings(old, plan.Identity.Bindings); len(got) != 0 {
		t.Fatal("recorded object UIDs do not make an unchanged desired grant obsolete")
	}
	if got := ObsoleteBindings(old, nil); len(got) != 2 {
		t.Fatal("downgrade must revoke both namespaced and cluster grants")
	}
	changed := append([]BindingReference(nil), plan.Identity.Bindings...)
	changed[0].ServiceAccountName = "replacement-identity"
	if got := ObsoleteBindings(old, changed); len(got) != 1 || got[0].UID != old[0].UID {
		t.Fatal("identity rebind must retain old grant UID for revocation")
	}
	changed = append([]BindingReference(nil), plan.Identity.Bindings...)
	changed[0].RoleRef.Name = "replacement-role"
	if got := ObsoleteBindings(old, changed); len(got) != 1 {
		t.Fatal("role replacement must revoke the immutable previous roleRef")
	}
}

func TestRevocationRoleApprovalChangesInvalidateExistingBindings(t *testing.T) {
	input := adminFixture()
	previous := mustCompile(t, input)
	input.Platform.Grants[0].Rules[0].Resources = []string{"configmaps"}
	current := mustCompile(t, input)
	obsolete := ObsoleteBindings(previous.Identity.Bindings, current.Identity.Bindings)
	if len(obsolete) != 1 || obsolete[0].Kind != "RoleBinding" {
		t.Fatal("role-content drift must revoke the old grant before restaging even when roleRef and name are unchanged")
	}
	if current.Identity.RoleBindings[0].Name != previous.Identity.RoleBindings[0].Name ||
		current.Identity.RoleBindings[0].Annotations[GrantRevisionAnnotation] == previous.Identity.RoleBindings[0].Annotations[GrantRevisionAnnotation] {
		t.Fatal("stable role binding names must carry changing approved grant contents")
	}
}

func TestRevocationUsesAuthoritativeReadsDespiteCachedAbsence(t *testing.T) {
	plan, authoritative := revocationFixture(t)
	failure := errors.New("synthetic write failure")
	cached := &revocationErrorClient{Client: authoritative, deleteError: failure, cachedAbsent: true}
	if err := RevokeBindingsWithReader(context.Background(), authoritative, cached, plan.Identity.Bindings); !errors.Is(err, failure) {
		t.Fatal("cached absence was mistaken for authoritative revocation", err)
	}
	cached.deleteError = nil
	if err := RevokeBindingsWithReader(context.Background(), authoritative, cached, plan.Identity.Bindings); err != nil {
		t.Fatal(err)
	}
}

type revocationErrorClient struct {
	client.Client
	deleteError  error
	listError    error
	deletes      int
	cachedAbsent bool
}

func (c *revocationErrorClient) Get(ctx context.Context, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
	if c.cachedAbsent {
		return apierrors.NewNotFound(rbacv1.Resource("rolebindings"), key.Name)
	}
	return c.Client.Get(ctx, key, object, opts...)
}

func (c *revocationErrorClient) Delete(ctx context.Context, object client.Object, opts ...client.DeleteOption) error {
	c.deletes++
	if c.deleteError != nil {
		return c.deleteError
	}
	return c.Client.Delete(ctx, object, opts...)
}

func (c *revocationErrorClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if c.listError != nil {
		return c.listError
	}
	return c.Client.List(ctx, list, opts...)
}

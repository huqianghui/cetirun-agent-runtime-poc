// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func prepareWorkloadGrants(t *testing.T, f *preparationFixture) (*sandboxpolicy.GrantJournalStore, []sandboxpolicy.BindingReference) {
	t.Helper()
	ctx := context.Background()
	role := f.makeAdmin(t)
	policy := &api.AgentPolicy{}
	if err := f.client.Get(ctx, client.ObjectKey{Name: "example-admin"}, policy); err != nil {
		t.Fatal(err)
	}
	policy.Spec.KubernetesAccess.ClusterRoleBindings = []api.AgentPolicyClusterRoleBinding{{ClusterRoleName: role.Name}}
	if err := f.client.Update(ctx, policy); err != nil {
		t.Fatal(err)
	}
	platform := f.config.Platforms[api.SandboxKata]
	clusterGrant := platform.Grants[0]
	clusterGrant.Namespace = ""
	platform.Grants = append(platform.Grants, clusterGrant)
	f.config.Platforms[api.SandboxKata] = platform
	if err := f.reconcile(); err != nil {
		t.Fatal(err)
	}
	agent := f.current(t)
	store := &sandboxpolicy.GrantJournalStore{Client: f.client, APIReader: f.client, Namespace: "cetirun-system"}
	refs, err := store.Read(ctx, agent.Name, agent.UID)
	if err != nil || len(refs) != 3 {
		t.Fatalf("expected admission, namespaced and cluster grants: %+v %v", refs, err)
	}
	for _, ref := range refs {
		if ref.UID == "" {
			t.Fatal("initial workload grant has no confirmed UID")
		}
	}
	return store, refs
}

func workloadBindingObject(ref sandboxpolicy.BindingReference) client.Object {
	if ref.Kind == "ClusterRoleBinding" {
		return &rbacv1.ClusterRoleBinding{}
	}
	return &rbacv1.RoleBinding{}
}

func isWorkloadBinding(object client.Object, ref sandboxpolicy.BindingReference) bool {
	kind := ""
	switch object := object.(type) {
	case *rbacv1.RoleBinding:
		kind = "RoleBinding"
	case *rbacv1.ClusterRoleBinding:
		kind = "ClusterRoleBinding"
	case *unstructured.Unstructured:
		kind = object.GetKind()
	}
	return kind == ref.Kind && object.GetName() == ref.Name && object.GetNamespace() == ref.Namespace
}

func TestSandboxPreparationRecoversDeletedWorkloadGrants(t *testing.T) {
	for _, typ := range []string{"openclaw", "hermes"} {
		for _, kind := range []string{"RoleBinding", "ClusterRoleBinding"} {
			for _, late := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/late=%v", typ, kind, late), func(t *testing.T) {
					ctx := context.Background()
					f := newPreparationFixture(t, typ)
					store, refs := prepareWorkloadGrants(t, f)
					agent := f.current(t)
					var target sandboxpolicy.BindingReference
					for _, ref := range refs {
						if ref.Kind == kind && ref.ServiceAccountNamespace == f.fixture.Namespace.Name {
							target = ref
						}
					}
					if target.UID == "" {
						t.Fatal("workload grant to delete was not found")
					}
					key := client.ObjectKey{Namespace: target.Namespace, Name: target.Name}
					old := workloadBindingObject(target)
					if err := f.client.Get(ctx, key, old); err != nil {
						t.Fatal(err)
					}
					deleted := false
					remove := func() {
						if !deleted {
							if err := f.client.Delete(ctx, old); err != nil {
								t.Fatal(err)
							}
							deleted = true
						}
					}
					if late {
						f.installer.stageDigest = "synthetic-release-drift"
						f.source.onFence = remove
					} else {
						remove()
					}
					var events []string
					wrapped := interceptor.NewClient(f.client.(client.WithWatch), interceptor.Funcs{
						Update: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.UpdateOption) error {
							if cm, ok := object.(*corev1.ConfigMap); ok && cm.Name == sandboxpolicy.GrantJournalName(agent.UID) {
								var record struct {
									Bindings []sandboxpolicy.BindingReference
								}
								if err := json.Unmarshal([]byte(cm.Data[sandboxpolicy.GrantJournalDataKey]), &record); err != nil {
									return err
								}
								index := slices.IndexFunc(record.Bindings, func(ref sandboxpolicy.BindingReference) bool {
									return withoutBindingUID(ref) == withoutBindingUID(target)
								})
								switch {
								case index == -1:
									events = append(events, "retire")
								case record.Bindings[index].UID == "":
									events = append(events, "intent")
								case record.Bindings[index].UID != target.UID:
									events = append(events, "confirm")
								default:
									return fmt.Errorf("workload replacement kept the old UID")
								}
							}
							return c.Update(ctx, object, options...)
						},
						Create: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.CreateOption) error {
							if isWorkloadBinding(object, target) {
								current, err := store.Read(ctx, agent.Name, agent.UID)
								if err != nil || !slices.Contains(current, withoutBindingUID(target)) {
									return fmt.Errorf("workload creation lacks fresh journal intent: %v", err)
								}
								events = append(events, "create")
							}
							return c.Create(ctx, object, options...)
						},
						Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
							return fmt.Errorf("recovery must not delete any still-present grant")
						},
					})
					f.reconciler.Client, f.reconciler.APIReader, f.installer.client = wrapped, wrapped, wrapped
					if err := f.reconcile(); err != nil {
						t.Fatal("deleted workload grant failed to recover:", err)
					}
					if !deleted || !slices.Equal(events, []string{"retire", "intent", "create", "confirm"}) {
						t.Fatalf("unsafe workload replacement sequence: %v", events)
					}
					current, err := store.Read(ctx, agent.Name, agent.UID)
					if err != nil || len(current) != len(refs) {
						t.Fatalf("replacement lost grant inventory: %+v %v", current, err)
					}
					for _, ref := range refs {
						if ref != target && !slices.Contains(current, ref) {
							t.Fatal("recovery changed an unrelated live grant identity")
						}
					}
					newBinding := workloadBindingObject(target)
					if err := f.client.Get(ctx, key, newBinding); err != nil || newBinding.GetUID() == target.UID ||
						newBinding.GetUID() == "" {
						t.Fatal("replacement did not have a new API identity:", err)
					}
					replacement := target
					replacement.UID = newBinding.GetUID()
					if !slices.Contains(current, replacement) {
						t.Fatal("replacement UID was not durably recorded")
					}
					if !f.expectation(t).ExecutionHeld || f.current(t).Status.Sandbox != nil {
						t.Fatal("grant recovery advanced execution or applied status")
					}
					f.reconciler.Client, f.reconciler.APIReader, f.installer.client = f.client, f.client, f.client
					f.source.onFence = nil
					stages := f.installer.stages
					if err := f.reconcile(); err != nil || f.installer.stages != stages {
						t.Fatal("recovered workload grant prevented unchanged reuse:", err)
					}
					agent = f.current(t)
					agent.Spec.Suspended = true
					if err := f.client.Update(ctx, agent); err != nil {
						t.Fatal(err)
					}
					if err := f.reconcile(); err != nil {
						t.Fatal("recovered workload grant blocked suspension:", err)
					}
					for _, ref := range current {
						if err := f.client.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, workloadBindingObject(ref)); !apierrors.IsNotFound(err) {
							t.Fatal("suspension left a recorded grant:", err)
						}
					}
					agent = f.current(t)
					agent.Spec.Suspended = false
					if err := f.client.Update(ctx, agent); err != nil {
						t.Fatal(err)
					}
					if err := f.reconcile(); err != nil {
						t.Fatal("grant recreation after suspension failed:", err)
					}
					if err := f.client.Delete(ctx, f.current(t)); err != nil {
						t.Fatal(err)
					}
					for range 2 {
						if err := f.reconcile(); err != nil {
							t.Fatal("workload grant replacement blocked deletion:", err)
						}
					}
					if err := f.client.Get(ctx, client.ObjectKeyFromObject(agent), &api.Agent{}); !apierrors.IsNotFound(err) {
						t.Fatal("workload grant recovery retained the Agent after cleanup:", err)
					}
				})
			}
		}
	}
}

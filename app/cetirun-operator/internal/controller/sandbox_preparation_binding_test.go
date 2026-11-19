// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestSandboxPreparationRecreatesAbsentSourceBinding(t *testing.T) {
	for _, typ := range []string{"openclaw", "hermes"} {
		for _, late := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/late=%v", typ, late), func(t *testing.T) {
				ctx := context.Background()
				f := newPreparationFixture(t, typ)
				if err := f.reconcile(); err != nil {
					t.Fatal(err)
				}
				agent := f.current(t)
				store := &sandboxpolicy.GrantJournalStore{Client: f.client, APIReader: f.client, Namespace: "cetirun-system"}
				refs, err := store.Read(ctx, agent.Name, agent.UID)
				if err != nil || len(refs) != 1 || refs[0].UID == "" {
					t.Fatalf("missing confirmed source grant: %+v %v", refs, err)
				}
				old := refs[0]
				key := client.ObjectKey{Namespace: old.Namespace, Name: old.Name}
				binding := &rbacv1.RoleBinding{}
				if err := f.client.Get(ctx, key, binding); err != nil {
					t.Fatal(err)
				}
				if !late {
					if err := f.client.Delete(ctx, binding); err != nil {
						t.Fatal(err)
					}
				} else {
					f.installer.stageDigest = "synthetic-release-drift"
				}
				var events []string
				deleted := !late
				wrapped := interceptor.NewClient(f.client.(client.WithWatch), interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, k client.ObjectKey, object client.Object, options ...client.GetOption) error {
						if _, ok := object.(*rbacv1.RoleBinding); ok && k == key && !deleted {
							if err := c.Delete(ctx, binding); err != nil {
								return err
							}
							deleted = true
						}
						return c.Get(ctx, k, object, options...)
					},
					Update: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.UpdateOption) error {
						if cm, ok := object.(*corev1.ConfigMap); ok && cm.Name == sandboxpolicy.GrantJournalName(agent.UID) {
							var record struct {
								Bindings []sandboxpolicy.BindingReference
							}
							if err := json.Unmarshal([]byte(cm.Data[sandboxpolicy.GrantJournalDataKey]), &record); err != nil {
								return err
							}
							switch {
							case len(record.Bindings) == 0:
								events = append(events, "retire")
							case len(record.Bindings) == 1 && record.Bindings[0].UID == "":
								events = append(events, "intent")
							case len(record.Bindings) == 1 && record.Bindings[0].UID != old.UID:
								events = append(events, "confirm")
							default:
								return errors.New("unexpected journal identity during replacement")
							}
						}
						return c.Update(ctx, object, options...)
					},
					Create: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.CreateOption) error {
						if _, ok := object.(*rbacv1.RoleBinding); ok && client.ObjectKeyFromObject(object) == key {
							current, err := store.Read(ctx, agent.Name, agent.UID)
							if err != nil || len(current) != 1 || current[0] != withoutBindingUID(old) {
								return fmt.Errorf("replacement lacks fresh write-ahead intent: %+v %v", current, err)
							}
							events = append(events, "create")
						}
						return c.Create(ctx, object, options...)
					},
				})
				f.reconciler.Client, f.reconciler.APIReader = wrapped, wrapped
				if err := f.reconcile(); err != nil {
					t.Fatal("missing source grant did not recover:", err)
				}
				if !deleted || !slices.Equal(events, []string{"retire", "intent", "create", "confirm"}) {
					t.Fatalf("unsafe replacement sequence: %v", events)
				}
				assertSandboxPrepared(t, f)
				current, err := store.Read(ctx, agent.Name, agent.UID)
				if err != nil || len(current) != 1 || current[0].UID == "" || current[0].UID == old.UID {
					t.Fatalf("replacement UID was not durably confirmed: %+v %v", current, err)
				}
				f.reconciler.Client, f.reconciler.APIReader = f.client, f.client
				stages := f.installer.stages
				if err := f.reconcile(); err != nil || f.installer.stages != stages {
					t.Fatal("replacement could not reuse preparation:", err)
				}
				agent = f.current(t)
				agent.Spec.Suspended = true
				if err := f.client.Update(ctx, agent); err != nil {
					t.Fatal(err)
				}
				if err := f.reconcile(); err != nil {
					t.Fatal("replacement blocked suspension:", err)
				}
				if err := f.client.Get(ctx, key, &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
					t.Fatal("suspension retained replacement grant:", err)
				}
				agent = f.current(t)
				agent.Spec.Suspended = false
				if err := f.client.Update(ctx, agent); err != nil {
					t.Fatal(err)
				}
				if err := f.reconcile(); err != nil {
					t.Fatal("replacement prevented preparation after suspension:", err)
				}
				assertSandboxPrepared(t, f)
				if err := f.client.Delete(ctx, f.current(t)); err != nil {
					t.Fatal(err)
				}
				for range 2 {
					if err := f.reconcile(); err != nil {
						t.Fatal("replacement blocked deletion:", err)
					}
				}
				if err := f.client.Get(ctx, client.ObjectKeyFromObject(agent), &api.Agent{}); !apierrors.IsNotFound(err) {
					t.Fatal("replacement retained Agent after cleanup:", err)
				}
			})
		}
	}
}

func withoutBindingUID(ref sandboxpolicy.BindingReference) sandboxpolicy.BindingReference {
	ref.UID = ""
	return ref
}

func TestSandboxPreparationSourceBindingRetirementFailures(t *testing.T) {
	for _, failure := range []string{
		"authoritative read", "journal write", "journal conflict", "replacement intent",
		"foreign replacement", "same-owner replacement", "replacement during retirement", "create", "stage",
	} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			f := newPreparationFixture(t, "openclaw")
			if err := f.reconcile(); err != nil {
				t.Fatal(err)
			}
			agent := f.current(t)
			store := &sandboxpolicy.GrantJournalStore{Client: f.client, APIReader: f.client, Namespace: "cetirun-system"}
			refs, err := store.Read(ctx, agent.Name, agent.UID)
			if err != nil || len(refs) != 1 {
				t.Fatalf("missing initial grant: %+v %v", refs, err)
			}
			old := refs[0]
			key := client.ObjectKey{Namespace: old.Namespace, Name: old.Name}
			binding := &rbacv1.RoleBinding{}
			if err := f.client.Get(ctx, key, binding); err != nil {
				t.Fatal(err)
			}
			if err := f.client.Delete(ctx, binding); err != nil {
				t.Fatal(err)
			}
			replacement := binding.DeepCopy()
			replacement.UID, replacement.ResourceVersion = "unrelated-binding-uid", ""
			if failure != "same-owner replacement" {
				replacement.OwnerReferences[0].UID = "unrelated-agent-uid"
			}
			if failure == "foreign replacement" || failure == "same-owner replacement" {
				if err := f.client.Create(ctx, replacement); err != nil {
					t.Fatal(err)
				}
			}
			creates, injected := 0, errors.New("synthetic replacement failure")
			raced, retired := false, false
			if failure == "stage" {
				f.installer.fail = injected
			}
			wrapped := interceptor.NewClient(f.client.(client.WithWatch), interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, k client.ObjectKey, object client.Object, options ...client.GetOption) error {
					if _, ok := object.(*rbacv1.RoleBinding); ok && k == key && failure == "authoritative read" {
						return injected
					}
					err := c.Get(ctx, k, object, options...)
					if _, ok := object.(*rbacv1.RoleBinding); ok && k == key && apierrors.IsNotFound(err) &&
						failure == "replacement during retirement" && !raced {
						raced = true
						if createErr := c.Create(ctx, replacement); createErr != nil {
							return createErr
						}
					}
					return err
				},
				Update: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.UpdateOption) error {
					if cm, ok := object.(*corev1.ConfigMap); ok && cm.Name == sandboxpolicy.GrantJournalName(agent.UID) {
						switch failure {
						case "journal write":
							return injected
						case "journal conflict":
							return apierrors.NewConflict(corev1.Resource("configmaps"), cm.Name, injected)
						case "replacement intent":
							var record struct {
								Bindings []sandboxpolicy.BindingReference
							}
							if err := json.Unmarshal([]byte(cm.Data[sandboxpolicy.GrantJournalDataKey]), &record); err != nil {
								return err
							}
							if retired && len(record.Bindings) == 1 && record.Bindings[0].UID == "" {
								return injected
							}
							retired = len(record.Bindings) == 0
						}
					}
					return c.Update(ctx, object, options...)
				},
				Create: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.CreateOption) error {
					if _, ok := object.(*rbacv1.RoleBinding); ok && client.ObjectKeyFromObject(object) == key {
						creates++
						if failure == "create" {
							return injected
						}
					}
					return c.Create(ctx, object, options...)
				},
			})
			f.reconciler.Client, f.reconciler.APIReader = wrapped, wrapped
			err = f.reconcile()
			transient := slices.Contains([]string{"authoritative read", "journal write", "replacement intent", "create", "stage"}, failure)
			if err == nil || (transient && !errors.Is(err, injected)) || (failure == "journal conflict" && !apierrors.IsConflict(err)) {
				t.Fatal("replacement failure was not surfaced:", err)
			}
			if failure != "create" && failure != "stage" && creates != 0 {
				t.Fatal("replacement created before safe journal retirement")
			}
			current, err := store.Read(ctx, agent.Name, agent.UID)
			if err != nil {
				t.Fatal(err)
			}
			if failure == "create" || failure == "stage" || failure == "replacement intent" {
				if len(current) != 0 {
					t.Fatal("rollback retained replacement intent")
				}
				if err := f.client.Get(ctx, key, &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
					t.Fatal("rollback retained replacement grant:", err)
				}
			} else if !slices.Equal(current, refs) {
				t.Fatal("failed retirement lost old inventory")
			}
			if failure == "replacement during retirement" && !raced {
				t.Fatal("replacement race was not exercised")
			}
			if failure == "foreign replacement" || failure == "same-owner replacement" || failure == "replacement during retirement" {
				observed := &rbacv1.RoleBinding{}
				if err := f.client.Get(ctx, key, observed); err != nil || observed.UID != replacement.UID ||
					observed.ResourceVersion != replacement.ResourceVersion {
					t.Fatal("unrelated replacement was changed:", err)
				}
			}
			if !slices.Contains(f.current(t).Finalizers, sandbox.PreparationFinalizer) {
				t.Fatal("failed replacement released cleanup protection")
			}
			if transient || failure == "journal conflict" {
				f.reconciler.Client, f.reconciler.APIReader = f.client, f.client
				f.installer.fail = nil
				if err := f.reconcile(); err != nil {
					t.Fatal("transient replacement failure prevented retry:", err)
				}
				assertSandboxPrepared(t, f)
			}
		})
	}
}

// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxpolicy

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func confirmedJournalFixture(t *testing.T, kind string) (*Plan, GrantJournalStore, *journalTestClient, []BindingReference, BindingReference, client.Object) {
	t.Helper()
	plan, store, writer := journalFixture(t)
	refs, err := OwnedBindingInventory(context.Background(), store.APIReader, plan.AgentName, plan.Binding.AgentUID)
	if err != nil || len(refs) != 2 {
		t.Fatalf("missing confirmed workload inventory: %+v %v", refs, err)
	}
	recordJournal(t, store, plan, refs)
	for _, ref := range refs {
		if ref.Kind == kind {
			object, err := readOwnedBinding(context.Background(), store.APIReader, ref)
			if err != nil || object == nil {
				t.Fatalf("missing initial binding: %v", err)
			}
			return plan, store, writer, refs, ref, object
		}
	}
	t.Fatal("missing requested binding kind")
	return nil, GrantJournalStore{}, nil, nil, BindingReference{}, nil
}

func TestGrantJournalRefreshRetiresOnlyAbsentConfirmedBindings(t *testing.T) {
	for _, kind := range []string{"RoleBinding", "ClusterRoleBinding"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			plan, store, writer, refs, old, object := confirmedJournalFixture(t, kind)
			updates, deletes := writer.updates, writer.deletes
			if err := store.RefreshBindingIntents(ctx, plan.AgentName, plan.Binding.AgentUID, plan.Identity.Bindings); err != nil {
				t.Fatal(err)
			}
			if writer.updates != updates || writer.deletes != deletes {
				t.Fatal("healthy grant refresh performed writes")
			}
			if err := writer.Client.Delete(ctx, object); err != nil {
				t.Fatal(err)
			}
			if err := store.RefreshBindingIntents(ctx, plan.AgentName, plan.Binding.AgentUID, plan.Identity.Bindings); err != nil {
				t.Fatal(err)
			}
			want := slices.Clone(refs)
			for i := range want {
				if want[i] == old {
					want[i].UID = ""
				}
			}
			assertJournalInventory(t, store, plan, want)
			if writer.updates != updates+2 || writer.deletes != deletes || writer.reads != 0 {
				t.Fatal("replacement must persist retirement then intent, without deleting live grants or using cached reads")
			}
			updates = writer.updates
			if err := store.RefreshBindingIntents(ctx, plan.AgentName, plan.Binding.AgentUID, plan.Identity.Bindings); err != nil ||
				writer.updates != updates {
				t.Fatal("fresh write-ahead intent was not reusable:", err)
			}
		})
	}
}

func TestGrantJournalRefreshRetainsRecoveryAcrossFailures(t *testing.T) {
	for _, kind := range []string{"RoleBinding", "ClusterRoleBinding"} {
		for _, failure := range []string{
			"read", "foreign replacement", "same-owner replacement", "replacement race",
			"retirement conflict", "retirement readback", "intent write",
		} {
			t.Run(kind+"/"+failure, func(t *testing.T) {
				ctx := context.Background()
				plan, store, writer, refs, old, object := confirmedJournalFixture(t, kind)
				base := writer.Client
				if err := base.Delete(ctx, object); err != nil {
					t.Fatal(err)
				}
				replacement := object.DeepCopyObject().(client.Object)
				replacement.SetUID("replacement-binding")
				replacement.SetResourceVersion("")
				if failure == "foreign replacement" || failure == "replacement race" {
					owners := replacement.GetOwnerReferences()
					owners[0].UID = "unrelated-agent"
					replacement.SetOwnerReferences(owners)
				}
				if failure == "foreign replacement" || failure == "same-owner replacement" {
					if err := base.Create(ctx, replacement); err != nil {
						t.Fatal(err)
					}
				}
				injected := errors.New("synthetic journal refresh failure")
				persisted, attempted, raced := 0, 0, false
				writer.beforeUpdate = func(object client.Object) error {
					if _, ok := object.(*corev1.ConfigMap); !ok {
						return fmt.Errorf("refresh wrote an unexpected resource")
					}
					attempted++
					if failure == "retirement conflict" {
						return apierrors.NewConflict(corev1.Resource("configmaps"), object.GetName(), injected)
					}
					if failure == "intent write" && attempted == 2 {
						return injected
					}
					return nil
				}
				writer.afterUpdate = func(client.Object) error {
					persisted++
					return nil
				}
				store.APIReader = journalTestReader{Reader: base, get: func(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
					if key == (client.ObjectKey{Namespace: old.Namespace, Name: old.Name}) {
						if failure == "read" {
							return injected
						}
						err := base.Get(ctx, key, object, options...)
						if failure == "replacement race" && apierrors.IsNotFound(err) && !raced {
							raced = true
							if err := base.Create(ctx, replacement); err != nil {
								return err
							}
						}
						return err
					}
					if failure == "retirement readback" && persisted == 1 {
						return injected
					}
					return base.Get(ctx, key, object, options...)
				}}
				deletes := writer.deletes
				err := store.RefreshBindingIntents(ctx, plan.AgentName, plan.Binding.AgentUID, plan.Identity.Bindings)
				if err == nil {
					t.Fatal("refresh hid a retirement failure")
				}
				switch failure {
				case "read", "retirement readback", "intent write":
					if !errors.Is(err, injected) {
						t.Fatal("refresh discarded the original failure:", err)
					}
				case "retirement conflict":
					if !apierrors.IsConflict(err) {
						t.Fatal("refresh discarded the journal CAS conflict:", err)
					}
				default:
					if !errors.Is(err, ErrOwnership) {
						t.Fatal("replacement ownership was not rejected:", err)
					}
				}
				if writer.deletes != deletes || writer.reads != 0 {
					t.Fatal("failed refresh deleted a binding or read from the cache")
				}
				if failure == "replacement race" && !raced {
					t.Fatal("replacement race was not reached")
				}
				store.APIReader = base
				want := refs
				if failure == "intent write" || failure == "retirement readback" {
					want = slices.DeleteFunc(slices.Clone(refs), func(ref BindingReference) bool { return ref == old })
					if persisted != 1 {
						t.Fatal("unconfirmed retirement permitted replacement intent")
					}
				} else if persisted != 0 {
					t.Fatal("failed retirement changed durable inventory")
				}
				assertJournalInventory(t, store, plan, want)
				if failure == "foreign replacement" || failure == "same-owner replacement" || failure == "replacement race" {
					current := object.DeepCopyObject().(client.Object)
					if err := base.Get(ctx, client.ObjectKeyFromObject(replacement), current); err != nil ||
						current.GetUID() != replacement.GetUID() || current.GetResourceVersion() != replacement.GetResourceVersion() {
						t.Fatal("refresh changed an unrelated replacement:", err)
					}
					return
				}
				writer.beforeUpdate, writer.afterUpdate = nil, nil
				recordJournal(t, store, plan, plan.Identity.Bindings)
				if err := store.RefreshBindingIntents(ctx, plan.AgentName, plan.Binding.AgentUID, plan.Identity.Bindings); err != nil {
					t.Fatal("normal write-ahead retry could not recover:", err)
				}
			})
		}
	}
}

func TestGrantJournalRefreshRequiresRecordedIntents(t *testing.T) {
	for _, missingJournal := range []bool{false, true} {
		t.Run(fmt.Sprint(missingJournal), func(t *testing.T) {
			plan, store, writer := journalFixture(t)
			if !missingJournal {
				recordJournal(t, store, plan, nil)
			}
			creates, updates, deletes := writer.creates, writer.updates, writer.deletes
			err := store.RefreshBindingIntents(context.Background(), plan.AgentName, plan.Binding.AgentUID, plan.Identity.Bindings)
			if missingJournal && !apierrors.IsNotFound(err) || !missingJournal && !errors.Is(err, ErrOwnership) {
				t.Fatal("missing recorded authority was not rejected:", err)
			}
			if writer.creates != creates || writer.updates != updates || writer.deletes != deletes {
				t.Fatal("refresh introduced unrecorded grant intent")
			}
		})
	}
}

func TestGrantJournalRefreshKeepsTerminatingBindingsRecorded(t *testing.T) {
	for _, kind := range []string{"RoleBinding", "ClusterRoleBinding"} {
		t.Run(kind, func(t *testing.T) {
			plan, store, writer, refs, _, object := confirmedJournalFixture(t, kind)
			object.SetFinalizers([]string{"example.com/cleanup"})
			ctx := context.Background()
			if err := writer.Client.Update(ctx, object); err != nil {
				t.Fatal(err)
			}
			if err := writer.Client.Delete(ctx, object); err != nil {
				t.Fatal(err)
			}
			if err := store.RefreshBindingIntents(ctx, plan.AgentName, plan.Binding.AgentUID, plan.Identity.Bindings); !errors.Is(err, ErrRevocationPending) {
				t.Fatal("terminating binding was treated as absent:", err)
			}
			assertJournalInventory(t, store, plan, refs)
		})
	}
}

func TestGrantJournalRefreshRejectsTerminatingJournalAndCanceledContext(t *testing.T) {
	for _, terminating := range []bool{false, true} {
		t.Run(fmt.Sprint(terminating), func(t *testing.T) {
			plan, store, writer, _, _, _ := confirmedJournalFixture(t, "RoleBinding")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if terminating {
				cm := readJournalCM(t, store, writer.Client, plan)
				if err := writer.Client.Delete(ctx, cm); err != nil {
					t.Fatal(err)
				}
			} else {
				cancel()
			}
			updates, deletes := writer.updates, writer.deletes
			err := store.RefreshBindingIntents(ctx, plan.AgentName, plan.Binding.AgentUID, plan.Identity.Bindings)
			if err == nil || !terminating && !errors.Is(err, context.Canceled) {
				t.Fatal("unavailable grant authority was accepted:", err)
			}
			if writer.updates != updates || writer.deletes != deletes {
				t.Fatal("unavailable grant authority permitted a write")
			}
		})
	}
}

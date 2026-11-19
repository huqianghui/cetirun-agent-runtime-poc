// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package configurationcoordination

import (
	"context"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestClientDeletionRecoversSupersededResourceVersions(t *testing.T) {
	for _, phase := range []sandboxcoordination.Phase{sandboxcoordination.Reserved, sandboxcoordination.Changing} {
		t.Run(string(phase), func(t *testing.T) {
			ctx := context.Background()
			storage, store, authority, before := coordinationFixture(t)
			ticket, err := store.AcquireConfigurationDeletion(ctx, before)
			if err != nil {
				t.Fatal(err)
			}
			if phase == sandboxcoordination.Changing {
				ticket, err = store.BeginChange(ctx, ticket)
				if err != nil {
					t.Fatal(err)
				}
			}
			current := before.DeepCopy()
			current.Status.Conditions = []metav1.Condition{{
				Type: "Accepted", Status: metav1.ConditionTrue, Reason: "PolicyAccepted",
				Message: "Synthetic policy accepted", LastTransitionTime: metav1.Now(),
			}}
			if err := storage.Status().Update(ctx, current); err != nil {
				t.Fatal(err)
			}
			if current.ResourceVersion == before.ResourceVersion {
				t.Fatal("status update did not supersede the original delete precondition")
			}
			deletes := 0
			writer := interceptor.NewClient(storage, interceptor.Funcs{
				Delete: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.DeleteOption) error {
					deletes++
					options := (&client.DeleteOptions{}).ApplyOptions(opts).AsDeleteOptions()
					if options.Preconditions == nil || options.Preconditions.ResourceVersion == nil ||
						*options.Preconditions.ResourceVersion != current.ResourceVersion {
						t.Fatal("a superseded conditional Delete was replayed")
					}
					if err := store.AuthorizeConfigurationDeletion(ctx, current, *options); err != nil {
						t.Fatal("fresh delete was not independently authorized:", err)
					}
					return c.Delete(ctx, object, opts...)
				},
			})
			coordinator, err := New(writer, storage, authority)
			if err != nil {
				t.Fatal(err)
			}
			if err := coordinator.Delete(ctx, current); err != nil {
				t.Fatal("a fresh version-fenced delete could not recover the superseded reservation:", err)
			}
			if deletes != 1 {
				t.Fatalf("got %d Delete calls, want only the fresh authorized request", deletes)
			}
			if err := storage.Get(ctx, client.ObjectKeyFromObject(before), &api.AgentPolicy{}); !apierrors.IsNotFound(err) {
				t.Fatal("fresh delete did not remove the requested policy:", err)
			}
			assertNoChanges(t, store)
			if err := store.CheckChange(ctx, ticket); err == nil {
				t.Fatal("the superseded ticket remained authorized")
			}
		})
	}
}

func TestClientDeletionKeepsUnchangedChangingReservation(t *testing.T) {
	ctx := context.Background()
	storage, store, authority, before := coordinationFixture(t)
	ticket, err := store.AcquireConfigurationDeletion(ctx, before)
	if err != nil {
		t.Fatal(err)
	}
	ticket, err = store.BeginChange(ctx, ticket)
	if err != nil {
		t.Fatal(err)
	}
	deletes := 0
	writer := interceptor.NewClient(storage, interceptor.Funcs{
		Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
			deletes++
			return nil
		},
	})
	coordinator, err := New(writer, storage, authority)
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Delete(ctx, before); !IsUnavailable(err) {
		t.Fatal("an unchanged uncertain deletion was not held:", err)
	}
	if deletes != 0 || store.CheckChange(ctx, ticket) != nil {
		t.Fatal("an unchanged deletion was replayed or its reservation released")
	}
}

func TestClientDeletionRecoveryDoesNotRefreshCallerPreconditions(t *testing.T) {
	for _, phase := range []sandboxcoordination.Phase{sandboxcoordination.Reserved, sandboxcoordination.Changing} {
		t.Run(string(phase), func(t *testing.T) {
			ctx := context.Background()
			storage, store, authority, before := coordinationFixture(t)
			ticket, err := store.AcquireConfigurationDeletion(ctx, before)
			if err != nil {
				t.Fatal(err)
			}
			if phase == sandboxcoordination.Changing {
				if _, err := store.BeginChange(ctx, ticket); err != nil {
					t.Fatal(err)
				}
			}
			current := before.DeepCopy()
			current.Status.ObservedGeneration = 1
			if err := storage.Status().Update(ctx, current); err != nil {
				t.Fatal(err)
			}
			deletes := 0
			writer := interceptor.NewClient(storage, interceptor.Funcs{
				Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
					deletes++
					return nil
				},
			})
			coordinator, err := New(writer, storage, authority)
			if err != nil {
				t.Fatal(err)
			}
			if err := coordinator.Delete(ctx, before); !IsConflict(err) {
				t.Fatal("recovery silently refreshed a stale caller's delete precondition:", err)
			}
			if deletes != 0 {
				t.Fatal("the stale caller submitted a fresh deletion")
			}
			assertNoChanges(t, store)
		})
	}
}

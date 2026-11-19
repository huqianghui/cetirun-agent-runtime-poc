// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package configurationcoordination

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestClientUsesChangingReservationAndBackgroundDeletion(t *testing.T) {
	ctx := context.Background()
	storage, store, authority, before := coordinationFixture(t)
	updateAuthorized, deleteAuthorized := false, false
	writer := interceptor.NewClient(storage, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.UpdateOption) error {
			if after, ok := object.(*api.AgentPolicy); ok {
				current := &api.AgentPolicy{}
				if err := storage.Get(ctx, client.ObjectKeyFromObject(after), current); err != nil {
					return err
				}
				if err := store.AuthorizeConfigurationUpdate(ctx, current, after); err != nil {
					t.Fatalf("update reached admission without its exact Changing reservation: %v", err)
				}
				updateAuthorized = true
			}
			return c.Update(ctx, object, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.DeleteOption) error {
			current := &api.AgentPolicy{}
			if err := storage.Get(ctx, client.ObjectKeyFromObject(object), current); err != nil {
				return err
			}
			options := (&client.DeleteOptions{}).ApplyOptions(opts).AsDeleteOptions()
			if err := store.AuthorizeConfigurationDeletion(ctx, current, *options); err != nil {
				t.Fatalf("delete reached admission without an exact Changing reservation and background propagation: %v", err)
			}
			deleteAuthorized = true
			return c.Delete(ctx, object, opts...)
		},
	})
	coordinator, err := New(writer, storage, authority)
	if err != nil {
		t.Fatal(err)
	}
	after := before.DeepCopy()
	after.Spec.Harnesses.Allowed = []api.AgentType{api.AgentTypeHermes}
	if err := coordinator.Update(ctx, before, after); err != nil {
		t.Fatal(err)
	}
	if !updateAuthorized {
		t.Fatal("coordinated update did not reach its admission boundary")
	}
	updated := &api.AgentPolicy{}
	if err := storage.Get(ctx, client.ObjectKeyFromObject(before), updated); err != nil {
		t.Fatal(err)
	}
	if len(updated.Spec.Harnesses.Allowed) != 1 || updated.Spec.Harnesses.Allowed[0] != api.AgentTypeHermes {
		t.Fatalf("unexpected updated policy: %+v", updated.Spec)
	}
	if err := coordinator.Delete(ctx, updated); err != nil {
		t.Fatal(err)
	}
	if !deleteAuthorized {
		t.Fatal("coordinated delete did not reach its admission boundary")
	}
	if err := storage.Get(ctx, client.ObjectKeyFromObject(before), &api.AgentPolicy{}); !apierrors.IsNotFound(err) {
		t.Fatalf("policy still exists after coordinated deletion: %v", err)
	}
	assertNoChanges(t, store)
}

func TestClientRetainsReservationWhenUpdateOutcomeIsUnknown(t *testing.T) {
	ctx := context.Background()
	storage, store, authority, before := coordinationFixture(t)
	writer := interceptor.NewClient(storage, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.UpdateOption) error {
			if _, ok := object.(*api.AgentPolicy); ok {
				return errors.New("synthetic unavailable writer")
			}
			return c.Update(ctx, object, opts...)
		},
	})
	coordinator, err := New(writer, storage, authority)
	if err != nil {
		t.Fatal(err)
	}
	after := before.DeepCopy()
	after.Spec.Harnesses.Allowed = []api.AgentType{api.AgentTypeHermes}
	if err := coordinator.Update(ctx, before, after); err == nil {
		t.Fatal("unknown Update outcome was reported as successful")
	}
	current := &api.AgentPolicy{}
	if err := storage.Get(ctx, client.ObjectKeyFromObject(before), current); err != nil {
		t.Fatal(err)
	}
	if current.Spec.Harnesses.Allowed[0] != api.AgentTypeOpenClaw {
		t.Fatal("failed coordinated submission changed the policy")
	}
	ticket, err := store.ReadConfigurationChange(ctx, before, after)
	if err != nil || ticket.Change.Phase != sandboxcoordination.Changing {
		t.Fatalf("unknown outcome did not retain its Changing reservation: ticket=%+v err=%v", ticket, err)
	}
}

func TestClientRecoversReservedStatusOnlySupersession(t *testing.T) {
	ctx := context.Background()
	storage, store, authority, before := coordinationFixture(t)
	after := before.DeepCopy()
	after.Spec.Harnesses.Allowed = []api.AgentType{api.AgentTypeHermes}
	ticket, err := store.AcquireConfigurationChange(ctx, before, after)
	if err != nil {
		t.Fatal(err)
	}
	current := &api.AgentPolicy{}
	if err := storage.Get(ctx, client.ObjectKeyFromObject(before), current); err != nil {
		t.Fatal(err)
	}
	current.Status.ObservedGeneration = current.Generation
	if err := storage.Status().Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	if current.ResourceVersion == before.ResourceVersion {
		t.Fatal("status update did not advance the resource version")
	}
	coordinator, err := New(storage, storage, authority)
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.RecoverUpdate(ctx, current, after); err != nil {
		t.Fatal("status-only Reserved update did not recover:", err)
	}
	assertNoChanges(t, store)
	if err := storage.Get(ctx, client.ObjectKeyFromObject(current), current); err != nil {
		t.Fatal(err)
	}
	after = current.DeepCopy()
	after.Spec.Harnesses.Allowed = []api.AgentType{api.AgentTypeHermes}
	if err := coordinator.Update(ctx, current, after); err != nil {
		t.Fatal("fresh update after status-only recovery failed:", err)
	}
	if err := storage.Get(ctx, client.ObjectKeyFromObject(current), current); err != nil {
		t.Fatal(err)
	}
	if len(current.Spec.Harnesses.Allowed) != 1 || current.Spec.Harnesses.Allowed[0] != api.AgentTypeHermes {
		t.Fatal("fresh update did not commit its desired configuration")
	}
	if ticket.Change.Phase != sandboxcoordination.Reserved {
		t.Fatal("fixture did not retain an unsubmitted reservation")
	}
}

func TestClientUpdateReportsConflictForChangingStatusOnlySupersession(t *testing.T) {
	ctx := context.Background()
	storage, store, authority, before := coordinationFixture(t)
	after := before.DeepCopy()
	after.Spec.Harnesses.Allowed = []api.AgentType{api.AgentTypeHermes}
	ticket, err := store.AcquireConfigurationChange(ctx, before, after)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginChange(ctx, ticket); err != nil {
		t.Fatal(err)
	}
	current := &api.AgentPolicy{}
	if err := storage.Get(ctx, client.ObjectKeyFromObject(before), current); err != nil {
		t.Fatal(err)
	}
	current.Status.ObservedGeneration = current.Generation
	if err := storage.Status().Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	coordinator, err := New(storage, storage, authority)
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Update(ctx, before, after); !IsConflict(err) {
		t.Fatalf("superseded Update did not report a conflict: %v", err)
	}
	assertNoChanges(t, store)
	if err := storage.Get(ctx, client.ObjectKeyFromObject(current), current); err != nil {
		t.Fatal(err)
	}
	if len(current.Spec.Harnesses.Allowed) != 1 || current.Spec.Harnesses.Allowed[0] != api.AgentTypeOpenClaw {
		t.Fatal("superseded Update changed the desired configuration")
	}
}

func TestClientReservedStatusRecoveryRetainsChangingRecordWhenContentDrifts(t *testing.T) {
	ctx := context.Background()
	storage, store, authority, before := coordinationFixture(t)
	after := before.DeepCopy()
	after.Spec.Harnesses.Allowed = []api.AgentType{api.AgentTypeHermes}
	if _, err := store.AcquireConfigurationChange(ctx, before, after); err != nil {
		t.Fatal(err)
	}
	current := &api.AgentPolicy{}
	if err := storage.Get(ctx, client.ObjectKeyFromObject(before), current); err != nil {
		t.Fatal(err)
	}
	current.Status.ObservedGeneration = current.Generation
	if err := storage.Status().Update(ctx, current); err != nil {
		t.Fatal(err)
	}

	var policyReads atomic.Int32
	policyKey := client.ObjectKeyFromObject(before)
	reader := interceptor.NewClient(storage, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
			if key == policyKey && policyReads.Add(1) == 2 {
				drift := &api.AgentPolicy{}
				if err := storage.Get(ctx, key, drift); err != nil {
					return err
				}
				drift.Annotations = map[string]string{"example.com/drift": "after-first-observation"}
				if err := storage.Update(ctx, drift); err != nil {
					return err
				}
			}
			return c.Get(ctx, key, object, opts...)
		},
	})
	coordinator, err := New(storage, reader, authority)
	if err != nil {
		t.Fatal(err)
	}
	err = coordinator.RecoverUpdate(ctx, current, after)
	if !IsUnavailable(err) {
		observed := &api.AgentPolicy{}
		if getErr := storage.Get(ctx, client.ObjectKeyFromObject(before), observed); getErr != nil {
			t.Fatal(getErr)
		}
		t.Fatalf("second observation accepted content drift: %v (policy reads=%d annotations=%v)", err, policyReads.Load(), observed.Annotations)
	}
	if policyReads.Load() != 2 {
		t.Fatalf("recovery made %d policy observations, want two", policyReads.Load())
	}
	recorded, found, err := store.ReadConfigurationUpdate(ctx, current)
	if err != nil || !found || recorded.Change.Phase != sandboxcoordination.Changing {
		t.Fatalf("content drift did not retain the Changing reservation: ticket=%+v found=%v err=%v", recorded, found, err)
	}
}

func TestClientReservedStatusRecoveryAndOriginalUpdateHaveOneBeginWinner(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	storage, store, authority, before := coordinationFixture(t)
	after := before.DeepCopy()
	after.Spec.Harnesses.Allowed = []api.AgentType{api.AgentTypeHermes}
	if _, err := store.AcquireConfigurationChange(ctx, before, after); err != nil {
		t.Fatal(err)
	}
	current := &api.AgentPolicy{}
	if err := storage.Get(ctx, client.ObjectKeyFromObject(before), current); err != nil {
		t.Fatal(err)
	}
	current.Status.ObservedGeneration = current.Generation
	if err := storage.Status().Update(ctx, current); err != nil {
		t.Fatal(err)
	}

	firstObservation := make(chan struct{})
	allowRecovery := make(chan struct{})
	policyKey := client.ObjectKeyFromObject(before)
	reader := interceptor.NewClient(storage, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
			if key == policyKey {
				if err := c.Get(ctx, key, object, opts...); err != nil {
					return err
				}
				select {
				case <-firstObservation:
				default:
					close(firstObservation)
					select {
					case <-allowRecovery:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				return nil
			}
			return c.Get(ctx, key, object, opts...)
		},
	})
	var changingWrites, successfulChangingWrites atomic.Int32
	firstChangingWrite := make(chan struct{})
	secondChangingWrite := make(chan struct{})
	writer := interceptor.NewClient(storage, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.UpdateOption) error {
			if authorityObject, ok := object.(*corev1.ConfigMap); ok {
				var state struct {
					Changes []sandboxcoordination.Change `json:"changes"`
				}
				if err := json.Unmarshal([]byte(authorityObject.Data[sandboxcoordination.DataKey]), &state); err != nil {
					return err
				}
				if len(state.Changes) == 1 && state.Changes[0].Phase == sandboxcoordination.Changing {
					switch changingWrites.Add(1) {
					case 1:
						close(firstChangingWrite)
						select {
						case <-secondChangingWrite:
						case <-ctx.Done():
							return ctx.Err()
						}
					case 2:
						close(secondChangingWrite)
					}
				}
			}
			err := c.Update(ctx, object, opts...)
			if err == nil {
				if authorityObject, ok := object.(*corev1.ConfigMap); ok {
					var state struct {
						Changes []sandboxcoordination.Change `json:"changes"`
					}
					if json.Unmarshal([]byte(authorityObject.Data[sandboxcoordination.DataKey]), &state) == nil &&
						len(state.Changes) == 1 && state.Changes[0].Phase == sandboxcoordination.Changing {
						successfulChangingWrites.Add(1)
					}
				}
			}
			return err
		},
	})
	coordinator, err := New(writer, reader, authority)
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	var group sync.WaitGroup
	group.Go(func() { results <- coordinator.RecoverUpdate(ctx, current, after) })
	select {
	case <-firstObservation:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	group.Go(func() { results <- coordinator.Update(ctx, before, after) })
	select {
	case <-firstChangingWrite:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	close(allowRecovery)
	group.Wait()
	close(results)
	for err := range results {
		if err == nil {
			continue
		}
		if !IsConflict(err) && !IsUnavailable(err) {
			t.Fatalf("concurrent recovery returned an unexpected error: %v", err)
		}
	}
	if changingWrites.Load() != 2 || successfulChangingWrites.Load() != 1 {
		t.Fatalf("concurrent BeginChange calls=%d successful=%d, want exactly two calls and one winner", changingWrites.Load(), successfulChangingWrites.Load())
	}
}

func TestClientRetainsReservationWhenSubmissionIntentReplyIsLost(testContext *testing.T) {
	for _, action := range []string{"update", "delete"} {
		testContext.Run(action, func(testContext *testing.T) {
			ctx := context.Background()
			storage, store, authority, before := coordinationFixture(testContext)
			mutations := 0
			writer := interceptor.NewClient(storage, interceptor.Funcs{
				Update: func(ctx context.Context, storage client.WithWatch, object client.Object, opts ...client.UpdateOption) error {
					if authority, ok := object.(*corev1.ConfigMap); ok {
						var state struct {
							Changes []sandboxcoordination.Change `json:"changes"`
						}
						if err := json.Unmarshal([]byte(authority.Data[sandboxcoordination.DataKey]), &state); err != nil {
							testContext.Fatal(err)
						}
						if len(state.Changes) == 1 && state.Changes[0].Phase == sandboxcoordination.Changing {
							if err := storage.Update(ctx, object, opts...); err != nil {
								return err
							}
							return errors.New("synthetic lost submission intent reply")
						}
					} else {
						mutations++
					}
					return storage.Update(ctx, object, opts...)
				},
				Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
					mutations++
					return errors.New("unconfirmed submission intent must not delete")
				},
			})
			coordinator, err := New(writer, storage, authority)
			if err != nil {
				testContext.Fatal(err)
			}
			var ticket sandboxcoordination.ChangeTicket
			if action == "update" {
				after := before.DeepCopy()
				after.Spec.Harnesses.Allowed = []api.AgentType{api.AgentTypeHermes}
				err = coordinator.Update(ctx, before, after)
				ticket, _ = store.ReadConfigurationChange(ctx, before, after)
			} else {
				err = coordinator.Delete(ctx, before)
				ticket, _ = store.ReadConfigurationDeletion(ctx, before)
			}
			if !IsUnavailable(err) || mutations != 0 || ticket.Change.Phase != sandboxcoordination.Changing {
				testContext.Fatalf("lost intent reply did not preserve an unavailable, unsubmitted Changing operation: %v %+v writes=%d", err, ticket, mutations)
			}
		})
	}
}

func TestClientRequiresRecoveryWhenReleaseFails(testContext *testing.T) {
	resource := schema.GroupResource{Resource: "configmaps"}
	for _, action := range []string{"update", "delete"} {
		for _, cause := range []error{
			apierrors.NewNotFound(resource, "configuration-coordination"),
			apierrors.NewForbidden(resource, "configuration-coordination", errors.New("synthetic denied release")),
			apierrors.NewConflict(resource, "configuration-coordination", errors.New("synthetic conflicting release")),
		} {
			testContext.Run(action+"/"+cause.Error(), func(testContext *testing.T) {
				ctx := context.Background()
				storage, store, authority, before := coordinationFixture(testContext)
				deletes := 0
				writer := interceptor.NewClient(storage, interceptor.Funcs{
					Update: func(ctx context.Context, storage client.WithWatch, object client.Object, opts ...client.UpdateOption) error {
						if authority, ok := object.(*corev1.ConfigMap); ok {
							var state struct {
								Changes []sandboxcoordination.Change `json:"changes"`
							}
							if err := json.Unmarshal([]byte(authority.Data[sandboxcoordination.DataKey]), &state); err != nil {
								testContext.Fatal(err)
							}
							if len(state.Changes) == 0 {
								return cause
							}
						}
						return storage.Update(ctx, object, opts...)
					},
					Delete: func(ctx context.Context, storage client.WithWatch, object client.Object, opts ...client.DeleteOption) error {
						deletes++
						return storage.Delete(ctx, object, opts...)
					},
				})
				coordinator, err := New(writer, storage, authority)
				if err != nil {
					testContext.Fatal(err)
				}
				if action == "update" {
					after := before.DeepCopy()
					after.Spec.Harnesses.Allowed = []api.AgentType{api.AgentTypeHermes}
					err = coordinator.Update(ctx, before, after)
				} else {
					err = coordinator.Delete(ctx, before)
				}
				if !IsUnavailable(err) || !errors.Is(err, cause) {
					testContext.Fatalf("release failure did not require recovery while retaining its API status: %v", err)
				}
				journal := &corev1.ConfigMap{}
				if err := storage.Get(ctx, client.ObjectKey{Namespace: authority.Namespace, Name: authority.Name}, journal); err != nil {
					testContext.Fatal(err)
				}
				var state struct {
					Changes []sandboxcoordination.Change `json:"changes"`
				}
				if err := json.Unmarshal([]byte(journal.Data[sandboxcoordination.DataKey]), &state); err != nil || len(state.Changes) != 1 ||
					state.Changes[0].Phase != sandboxcoordination.Changing {
					testContext.Fatalf("release failure did not retain its Changing reservation: %+v %v", state, err)
				}
				if action != "delete" {
					return
				}
				if err := storage.Get(ctx, client.ObjectKeyFromObject(before), &api.AgentPolicy{}); !apierrors.IsNotFound(err) {
					testContext.Fatal("deletion did not commit before its release failure:", err)
				}
				ticket, err := store.ReadConfigurationDeletionOperation(ctx, state.Changes[0].ID)
				if err != nil {
					testContext.Fatal("Portal deletion was not recoverable without its original body:", err)
				}
				adapter, err := sandboxcoordination.NewKubernetesDeletionRecoveryAdapter(storage, ticket)
				if err != nil {
					testContext.Fatal(err)
				}
				if _, err := store.ReconcileChange(ctx, ticket, adapter); err != nil || deletes != 1 {
					testContext.Fatal("recovery failed or replayed the committed Delete:", err)
				}
				assertNoChanges(testContext, store)
			})
		}
	}
}

func coordinationFixture(t *testing.T) (client.WithWatch, sandboxcoordination.Store, Authority, *api.AgentPolicy) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	authorityObject, err := sandboxcoordination.NewState("cetirun-system", "configuration-coordination")
	if err != nil {
		t.Fatal(err)
	}
	authorityObject.UID = types.UID("authority-uid")
	policy := &api.AgentPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "standard", UID: types.UID("policy-uid")},
		Spec:       api.AgentPolicySpec{Harnesses: api.AgentPolicyHarnesses{Allowed: []api.AgentType{api.AgentTypeOpenClaw}}},
	}
	storage := fake.NewClientBuilder().WithScheme(scheme).WithGlobalResourceVersionCounter().WithStatusSubresource(&api.AgentPolicy{}).WithObjects(authorityObject, policy).Build()
	if err := storage.Get(context.Background(), client.ObjectKeyFromObject(policy), policy); err != nil {
		t.Fatal(err)
	}
	authority := Authority{Namespace: authorityObject.Namespace, Name: authorityObject.Name, UID: authorityObject.UID}
	store := sandboxcoordination.Store{Client: storage, APIReader: storage, Namespace: authority.Namespace,
		Identity: w.ObjectIdentity{Name: authority.Name, UID: authority.UID}}
	return storage, store, authority, policy
}

func assertNoChanges(t *testing.T, store sandboxcoordination.Store) {
	t.Helper()
	authority := &corev1.ConfigMap{}
	if err := store.APIReader.Get(context.Background(), client.ObjectKey{Namespace: store.Namespace, Name: store.Identity.Name}, authority); err != nil {
		t.Fatal(err)
	}
	state := struct {
		Changes []json.RawMessage `json:"changes"`
	}{}
	if err := json.Unmarshal([]byte(authority.Data[sandboxcoordination.DataKey]), &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Changes) != 0 {
		t.Fatalf("successful operations left %d reservations", len(state.Changes))
	}
}

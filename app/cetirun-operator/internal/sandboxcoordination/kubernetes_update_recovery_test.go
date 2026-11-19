// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxcoordination

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestUpdateRecoveryRequiresExactCommittedContent(t *testing.T) {
	for _, kind := range []string{"Team", "Sandbox", "AgentPolicy"} {
		for _, scenario := range []string{"committed", "unchanged", "same-version", "replaced", "spec-drift", "metadata-drift", "terminating", "unknown-field", "read-error", "wrong-operation"} {
			t.Run(kind+"/"+scenario, func(t *testing.T) {
				ctx := context.Background()
				store, storage, before, after := configurationFixture(t, kind)
				ticket, err := store.AcquireConfigurationChange(ctx, before, after)
				if err != nil {
					t.Fatal(err)
				}
				ticket, err = store.BeginChange(ctx, ticket)
				if err != nil {
					t.Fatal(err)
				}
				if scenario != "unchanged" {
					if err := storage.Update(ctx, after); err != nil {
						t.Fatal(err)
					}
				}
				reader := interceptor.NewClient(storage, interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
						if err := c.Get(ctx, key, object, opts...); err != nil {
							return err
						}
						observed, ok := object.(*unstructured.Unstructured)
						if !ok {
							return nil
						}
						switch scenario {
						case "same-version":
							observed.SetResourceVersion(before.GetResourceVersion())
						case "replaced":
							observed.SetUID("replacement-uid")
						case "spec-drift":
							_ = unstructured.SetNestedMap(observed.Object, map[string]interface{}{}, "spec")
						case "metadata-drift":
							observed.SetAnnotations(map[string]string{"example.com/drift": "different"})
						case "terminating":
							_ = unstructured.SetNestedField(observed.Object, "2026-01-01T00:00:00Z", "metadata", "deletionTimestamp")
						case "unknown-field":
							observed.Object["unknown"] = "unrecognized"
						case "read-error":
							return errors.New("synthetic read failure")
						}
						return nil
					},
				})
				// A new Store and adapter retain only the durable journal and current
				// source key/UID; neither original request body participates.
				restarted := store
				restarted.APIReader = reader
				current := before.DeepCopyObject().(client.Object)
				if err := storage.Get(ctx, client.ObjectKeyFromObject(current), current); err != nil {
					t.Fatal(err)
				}
				recovered, found, err := restarted.ReadConfigurationUpdate(ctx, current)
				if err != nil || !found || recovered.Change.ID != ticket.Change.ID {
					t.Fatalf("durable lookup: %v found=%v", err, found)
				}
				adapter, err := NewKubernetesUpdateRecoveryAdapter(reader, recovered, current)
				if err != nil {
					t.Fatal(err)
				}
				if err := adapter.Submit(ctx, recovered, func(context.Context) error { t.Fatal("recovery attempted submission"); return nil }); !errors.Is(err, ErrConflict) {
					t.Fatal("recovery allowed submission")
				}
				if scenario == "wrong-operation" {
					forged := recovered.DeepCopy()
					forged.Change.ID = "another-operation"
					if _, err := adapter.ObserveChange(ctx, forged); !errors.Is(err, ErrConflict) {
						t.Fatal("observer accepted another operation")
					}
					return
				}
				_, err = restarted.ReconcileChange(ctx, recovered, adapter)
				if scenario == "committed" {
					if err != nil {
						t.Fatal("committed update was not recovered:", err)
					}
					if _, found, err := restarted.ReadConfigurationUpdate(ctx, current); err != nil || found {
						t.Fatal("successful recovery retained the reservation")
					}
				} else {
					if err == nil {
						t.Fatal("uncertain update was released")
					}
					if err := store.CheckChange(ctx, ticket); err != nil {
						t.Fatal("failed observation lost the reservation:", err)
					}
				}
			})
		}
	}
}

func TestUpdateRecoveryReleasesStatusOnlySupersession(t *testing.T) {
	ctx := context.Background()
	store, storage, before, after := configurationFixture(t, "Team")
	ticket, err := store.AcquireConfigurationChange(ctx, before, after)
	if err != nil {
		t.Fatal(err)
	}
	ticket, err = store.BeginChange(ctx, ticket)
	if err != nil {
		t.Fatal(err)
	}
	current := &api.Team{}
	if err := storage.Get(ctx, client.ObjectKeyFromObject(before), current); err != nil {
		t.Fatal(err)
	}
	current.Status.ObservedGeneration = current.Generation
	if err := storage.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	if current.ResourceVersion == before.GetResourceVersion() {
		t.Fatal("status refresh did not advance the resource version")
	}
	restarted := store
	recovered, found, err := restarted.ReadConfigurationUpdate(ctx, current)
	if err != nil || !found || recovered.Change.ID != ticket.Change.ID || recovered.Change.BeforeContentDigest == "" {
		t.Fatalf("did not find the concrete Changing update: ticket=%+v found=%v err=%v", recovered, found, err)
	}
	adapter, err := NewKubernetesUpdateRecoveryAdapter(storage, recovered, current)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.ReconcileChange(ctx, recovered, adapter); err != nil {
		t.Fatal("status-only supersession did not release its reservation:", err)
	}
	if _, found, err := restarted.ReadConfigurationUpdate(ctx, current); err != nil || found {
		t.Fatal("status-only supersession retained the reservation")
	}
	if err := storage.Update(ctx, after); !apierrors.IsConflict(err) {
		t.Fatalf("delayed stale update was not excluded: %v", err)
	}
	if err := storage.Get(ctx, client.ObjectKeyFromObject(current), current); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(current.Spec, before.(*api.Team).Spec) || !reflect.DeepEqual(current.Annotations, before.GetAnnotations()) {
		t.Fatal("stale update changed the configuration")
	}
}

func TestUpdateRecoveryCannotAdoptDeletionOrReservedOperation(t *testing.T) {
	for _, scenario := range []string{"deletion", "reserved", "replacement"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			store, storage, before, after := configurationFixture(t, "Team")
			var ticket ChangeTicket
			var err error
			if scenario == "deletion" {
				ticket, err = store.AcquireConfigurationDeletion(ctx, before)
			} else {
				ticket, err = store.AcquireConfigurationChange(ctx, before, after)
			}
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "reserved" {
				ticket, err = store.BeginChange(ctx, ticket)
				if err != nil {
					t.Fatal(err)
				}
			}
			current := before.DeepCopyObject().(client.Object)
			if scenario == "replacement" {
				current.SetUID("replacement-uid")
			}
			if recovered, found, err := store.ReadConfigurationUpdate(ctx, current); scenario == "reserved" {
				if err != nil || !found || recovered.Change.ID != ticket.Change.ID || recovered.Change.Phase != Reserved {
					t.Fatalf("reserved update lookup: ticket=%+v found=%v err=%v", recovered, found, err)
				}
			} else if !errors.Is(err, ErrConflict) {
				t.Fatal("invalid recovery lookup accepted:", err)
			}
			adapter, adapterErr := NewKubernetesUpdateRecoveryAdapter(storage, ticket, current)
			if scenario == "reserved" {
				if adapterErr != nil {
					t.Fatal("reserved recovery adapter rejected:", adapterErr)
				}
				if _, err := adapter.ObserveChange(ctx, ticket); !errors.Is(err, ErrUnavailable) {
					t.Fatal("unchanged Reserved update was observed as terminal:", err)
				}
			} else if !errors.Is(adapterErr, ErrConflict) {
				t.Fatal("invalid recovery adapter accepted:", adapterErr)
			}
			if err := store.CheckChange(ctx, ticket); err != nil {
				t.Fatal("invalid recovery changed the reservation:", err)
			}
		})
	}
}

func TestConcreteUpdateRecoveryRejectsTamperedBeforeDigest(t *testing.T) {
	for _, scenario := range []string{"missing", "malformed", "changed"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			store, storage, before, after := configurationFixture(t, "AgentPolicy")
			ticket := changingConfiguration(t, store, before, after)
			authority, state, err := store.read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "missing":
				state.Changes[0].BeforeContentDigest = ""
			case "malformed":
				state.Changes[0].BeforeContentDigest = "not-a-digest"
			case "changed":
				state.Changes[0].BeforeContentDigest = strings.Repeat("a", 64)
				if state.Changes[0].BeforeContentDigest == state.Changes[0].DesiredDigest {
					t.Fatal("fixture digest unexpectedly collided with tampered digest")
				}
			}
			// Simulate a persisted journal fault. encode intentionally refuses the
			// malformed case, so write the raw test payload as an untrusted reader
			// would encounter it.
			raw, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			authority.Data = map[string]string{DataKey: string(raw)}
			if err := storage.Update(ctx, authority); err != nil {
				t.Fatal(err)
			}

			forged := ticket.DeepCopy()
			forged.Change.BeforeContentDigest = state.Changes[0].BeforeContentDigest
			if scenario == "malformed" {
				if _, _, err := store.ReadConfigurationUpdate(ctx, before); !errors.Is(err, ErrUnavailable) {
					t.Fatalf("malformed journal record was readable: %v", err)
				}
				if _, err := NewKubernetesUpdateRecoveryAdapter(storage, forged, before); !errors.Is(err, ErrConflict) {
					t.Fatalf("malformed before digest constructed a recovery adapter: %v", err)
				}
				return
			}
			if _, err := store.ReadConfigurationChange(ctx, before, after); !errors.Is(err, ErrConflict) {
				t.Fatalf("concrete configuration path adopted a %s before digest: %v", scenario, err)
			}
			if scenario == "missing" {
				if _, found, err := store.ReadConfigurationUpdate(ctx, before); !errors.Is(err, ErrConflict) || found {
					t.Fatalf("recovery adopted a record without a before digest: found=%v err=%v", found, err)
				}
				if _, err := NewKubernetesUpdateRecoveryAdapter(storage, forged, before); !errors.Is(err, ErrConflict) {
					t.Fatalf("missing before digest constructed a recovery adapter: %v", err)
				}
				return
			}
			adapter, err := NewKubernetesUpdateRecoveryAdapter(storage, forged, before)
			if err != nil {
				t.Fatal(err)
			}
			current := before.DeepCopyObject().(client.Object)
			if err := storage.Get(ctx, client.ObjectKeyFromObject(current), current); err != nil {
				t.Fatal(err)
			}
			if policy, ok := current.(*api.AgentPolicy); ok {
				policy.Status.ObservedGeneration = policy.Generation
			}
			if err := storage.Update(ctx, current); err != nil {
				t.Fatal(err)
			}
			if current.GetResourceVersion() == before.GetResourceVersion() {
				t.Fatal("status-only refresh did not advance the resource version")
			}
			if _, err := adapter.ObserveChange(ctx, forged); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("changed before digest was released as a terminal outcome: %v", err)
			}
		})
	}
}

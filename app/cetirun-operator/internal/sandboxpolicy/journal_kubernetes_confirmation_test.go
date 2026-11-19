// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxpolicy

import (
	"context"
	"errors"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestConfirmKubernetesBindingIntentsPreservesObservedUIDs(t *testing.T) {
	for _, kind := range []string{"RoleBinding", "ClusterRoleBinding"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			plan, store, writer := journalFixture(t)
			recordJournal(t, store, plan, plan.Identity.Bindings)
			created, deleted := writer.creates, writer.deletes
			if err := store.ConfirmBindingIntents(ctx, plan.AgentName, plan.Binding.AgentUID, plan.Identity.Bindings); err != nil {
				t.Fatal(err)
			}
			refs, err := store.Read(ctx, plan.AgentName, plan.Binding.AgentUID)
			if err != nil || len(refs) != 2 || slices.ContainsFunc(refs, func(ref BindingReference) bool { return ref.UID == "" }) {
				t.Fatal("observed binding UIDs were not persisted:", err)
			}
			if writer.creates != created || writer.deletes != deleted {
				t.Fatal("confirmation created or deleted a grant")
			}
			selected := refs[slices.IndexFunc(refs, func(ref BindingReference) bool { return ref.Kind == kind })]
			lostReply := errors.New("synthetic deletion response loss")
			writer.beforeDelete = func(object client.Object, options client.DeleteOptions) error {
				if err := writer.Client.Delete(ctx, object, &options); err != nil {
					return err
				}
				return lostReply
			}
			if err := store.Revoke(ctx, plan.AgentName, plan.Binding.AgentUID, []BindingReference{selected}); !errors.Is(err, lostReply) {
				t.Fatal("deletion response loss was hidden:", err)
			}
			assertJournalInventory(t, store, plan, refs)
			writer.beforeDelete = nil
			if err := store.Revoke(ctx, plan.AgentName, plan.Binding.AgentUID, []BindingReference{selected}); err != nil {
				t.Fatal("confirmed UID did not recover an already-completed deletion:", err)
			}
		})
	}
}

func TestConfirmKubernetesBindingIntentsRetainsUncertainAndFailedProgress(t *testing.T) {
	for _, scenario := range []string{"missing", "write-failure", "write-ack-loss", "terminating-journal", "mixed-read-failure"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			plan, store, writer := journalFixture(t)
			recordJournal(t, store, plan, plan.Identity.Bindings)
			refs := plan.Identity.Bindings
			failure := errors.New("synthetic confirmation storage failure")
			journal := &corev1.ConfigMap{}
			key := client.ObjectKey{Namespace: store.Namespace, Name: GrantJournalName(plan.Binding.AgentUID)}
			if err := store.APIReader.Get(ctx, key, journal); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "missing", "mixed-read-failure":
				object, err := readOwnedBinding(ctx, store.APIReader, refs[0])
				if err != nil || object == nil {
					t.Fatal(err)
				}
				if err := writer.Client.Delete(ctx, object); err != nil {
					t.Fatal(err)
				}
				if scenario == "mixed-read-failure" {
					store.APIReader = journalTestReader{Reader: writer.Client, get: func(ctx context.Context, objectKey client.ObjectKey, object client.Object, options ...client.GetOption) error {
						if objectKey.Name == refs[1].Name {
							return failure
						}
						return writer.Client.Get(ctx, objectKey, object, options...)
					}}
				}
			case "write-failure":
				writer.beforeUpdate = func(client.Object) error { return failure }
			case "write-ack-loss":
				writer.afterUpdate = func(client.Object) error { return failure }
			case "terminating-journal":
				if err := writer.Client.Delete(ctx, journal); err != nil {
					t.Fatal(err)
				}
			}
			created, deleted := writer.creates, writer.deletes
			err := store.ConfirmBindingIntents(ctx, plan.AgentName, plan.Binding.AgentUID, refs)
			if scenario == "missing" {
				if !errors.Is(err, ErrRevocationPending) {
					t.Fatal("missing outcome was treated as confirmation:", err)
				}
			} else if scenario == "terminating-journal" {
				if err != nil {
					t.Fatal("terminating journal could not preserve cleanup UIDs:", err)
				}
			} else if !errors.Is(err, failure) || errors.Is(err, ErrRevocationPending) {
				t.Fatal("non-absence failure was weakened to cancellable uncertainty:", err)
			}
			writer.beforeUpdate, writer.afterUpdate = nil, nil
			store.APIReader = writer.Client
			recorded, err := store.Read(ctx, plan.AgentName, plan.Binding.AgentUID)
			if err != nil || len(recorded) != len(refs) {
				t.Fatal("confirmation lost journal inventory:", err)
			}
			if scenario == "write-failure" && slices.ContainsFunc(recorded, func(ref BindingReference) bool { return ref.UID != "" }) {
				t.Fatal("failed confirmation claimed durable UIDs")
			}
			if (scenario == "write-ack-loss" || scenario == "terminating-journal") &&
				slices.ContainsFunc(recorded, func(ref BindingReference) bool { return ref.UID == "" }) {
				t.Fatal("durable confirmation was lost after response loss or termination")
			}
			if writer.creates != created || writer.deletes != deleted {
				t.Fatal("confirmation changed binding existence")
			}
		})
	}
}

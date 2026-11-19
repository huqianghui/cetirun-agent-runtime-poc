// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxpolicy

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestVerifyKubernetesBindingsRequiresConfirmedExactInventory(t *testing.T) {
	ctx := context.Background()
	plan, store, writer := journalFixture(t)
	recordJournal(t, store, plan, plan.Identity.Bindings)
	if err := store.VerifyKubernetesBindings(ctx, plan.AgentName, plan.Binding.AgentUID, plan.Identity.Bindings); !errors.Is(err, ErrOwnership) {
		t.Fatal("unconfirmed write-ahead intent authorized execution:", err)
	}
	if err := store.VerifyKubernetesBindingIntents(ctx, plan.AgentName, plan.Binding.AgentUID, plan.Identity.Bindings); err != nil {
		t.Fatal("valid write-ahead intents could not release a metadata-only reconciliation hold:", err)
	}
	refs, err := OwnedBindingInventory(ctx, store.APIReader, plan.AgentName, plan.Binding.AgentUID)
	if err != nil {
		t.Fatal(err)
	}
	recordJournal(t, store, plan, refs)
	creates, updates, deletes := writer.creates, writer.updates, writer.deletes
	store.Client = nil
	if err := store.VerifyKubernetesBindings(ctx, plan.AgentName, plan.Binding.AgentUID, plan.Identity.Bindings); err != nil {
		t.Fatal("confirmed exact grants did not authorize a read-only check:", err)
	}
	if err := store.VerifyKubernetesBindings(ctx, plan.AgentName, plan.Binding.AgentUID, plan.Identity.Bindings[:1]); !errors.Is(err, ErrOwnership) {
		t.Fatal("obsolete Kubernetes grants authorized a reduced grant set:", err)
	}
	if creates != writer.creates || updates != writer.updates || deletes != writer.deletes {
		t.Fatal("grant verification mutated its authority")
	}
}

func TestVerifyKubernetesBindingsRejectsDriftAndUnavailableAuthority(t *testing.T) {
	for _, failure := range []string{"missing", "replacement", "shared", "foreign", "approval", "terminating", "journal-race"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			plan, store, writer, _, _, object := confirmedJournalFixture(t, "RoleBinding")
			binding := object.(*rbacv1.RoleBinding)
			switch failure {
			case "missing", "replacement":
				if err := writer.Client.Delete(ctx, binding); err != nil {
					t.Fatal(err)
				}
				if failure == "replacement" {
					binding.UID, binding.ResourceVersion = "replacement", ""
					if err := writer.Client.Create(ctx, binding); err != nil {
						t.Fatal(err)
					}
				}
			case "shared":
				binding.Subjects = append(binding.Subjects, rbacv1.Subject{Kind: "ServiceAccount", Name: "other", Namespace: plan.Namespace})
			case "foreign":
				binding.OwnerReferences[0].UID = "other-agent"
			case "approval":
				binding.Annotations[GrantRevisionAnnotation] = "another-approval"
			case "terminating":
				binding.Finalizers = []string{"example.com/cleanup"}
			case "journal-race":
				reads := 0
				store.APIReader = journalTestReader{Reader: writer.Client, get: func(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
					if key.Name == GrantJournalName(plan.Binding.AgentUID) {
						reads++
						if reads == 2 {
							current := &corev1.ConfigMap{}
							if err := writer.Client.Get(ctx, key, current); err != nil {
								return err
							}
							current.Annotations["example.com/change"] = "raced"
							if err := writer.Client.Update(ctx, current); err != nil {
								return err
							}
						}
					}
					return writer.Client.Get(ctx, key, object, options...)
				}}
			}
			if failure == "shared" || failure == "foreign" || failure == "approval" || failure == "terminating" {
				if err := writer.Client.Update(ctx, binding); err != nil {
					t.Fatal(err)
				}
			}
			if failure == "terminating" {
				if err := writer.Client.Delete(ctx, binding); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.VerifyKubernetesBindings(ctx, plan.AgentName, plan.Binding.AgentUID, plan.Identity.Bindings); err == nil {
				t.Fatal("changed or unavailable grant authority authorized Kubernetes access")
			}
		})
	}
}

func TestGrantJournalRefreshChecksUnconfirmedExistingBinding(t *testing.T) {
	ctx := context.Background()
	plan, store, writer := journalFixture(t)
	recordJournal(t, store, plan, plan.Identity.Bindings)
	binding := plan.Identity.RoleBindings[0].DeepCopy()
	if err := writer.Client.Get(ctx, client.ObjectKeyFromObject(binding), binding); err != nil {
		t.Fatal(err)
	}
	binding.Subjects = append(binding.Subjects, rbacv1.Subject{Kind: "ServiceAccount", Name: "other", Namespace: plan.Namespace})
	if err := writer.Client.Update(ctx, binding); err != nil {
		t.Fatal(err)
	}
	if err := store.RefreshBindingIntents(ctx, plan.AgentName, plan.Binding.AgentUID, plan.Identity.Bindings); !errors.Is(err, ErrOwnership) {
		t.Fatal("unconfirmed outcome adopted a shared binding:", err)
	}
}

// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxpolicy

import (
	"context"
	"errors"
	"testing"
)

func TestVerifyKubernetesBindingsRejectsUnrecordedOwnedGrants(t *testing.T) {
	for _, scenario := range []struct {
		name      string
		remaining int
	}{{"none", 0}, {"one", 1}} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx := context.Background()
			plan, store, writer := journalFixture(t)
			refs, err := OwnedBindingInventory(ctx, store.APIReader, plan.AgentName, plan.Binding.AgentUID)
			if err != nil || len(refs) != 2 {
				t.Fatal("fixture requires two existing owned workload grants:", err)
			}
			recordJournal(t, store, plan, refs[:scenario.remaining])
			creates, updates, deletes := writer.creates, writer.updates, writer.deletes
			for _, confirmed := range []bool{false, true} {
				verify := store.VerifyKubernetesBindingIntents
				if confirmed {
					verify = store.VerifyKubernetesBindings
				}
				if err := verify(ctx, plan.AgentName, plan.Binding.AgentUID, refs[:scenario.remaining]); !errors.Is(err, ErrOwnership) {
					t.Fatal("an unrecorded owned grant authorized the reduced inventory:", err)
				}
			}
			if creates != writer.creates || updates != writer.updates || deletes != writer.deletes {
				t.Fatal("owned-inventory verification mutated its authority")
			}
		})
	}
}

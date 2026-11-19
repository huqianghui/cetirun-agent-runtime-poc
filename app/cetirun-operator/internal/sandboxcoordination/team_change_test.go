// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxcoordination

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func teamChangeFixture(t *testing.T) (Store, client.WithWatch, *api.Team) {
	t.Helper()
	store, storage := fixture(t)
	if err := api.AddToScheme(storage.Scheme()); err != nil {
		t.Fatal(err)
	}
	team := &api.Team{ObjectMeta: metav1.ObjectMeta{Name: "team", UID: "team-uid"},
		Spec: api.TeamSpec{AgentPolicyRef: "policy",
			SandboxRefs: []string{"sandbox", "spare"}, DefaultSandboxRef: "sandbox"}}
	if err := storage.Create(context.Background(), team); err != nil {
		t.Fatal(err)
	}
	return store, storage, team
}

func teamIntent(t *testing.T, team *api.Team, suffix string) Intent {
	t.Helper()
	intent := testIntent(suffix)
	intent.Expected.Binding.TeamUID = team.UID
	inputs, err := w.ExecutionTeamInputs(team)
	if err != nil {
		t.Fatal(err)
	}
	intent.TeamSharedDigest = inputs.SharedDigest
	for i := range intent.Sources {
		source := &intent.Sources[i]
		if source.Kind == "Team" {
			delete(intent.SourceDigests, w.SourceKey(*source))
			source.Name, source.UID = team.Name, team.UID
			source.ResourceVersion = team.ResourceVersion
			intent.SourceDigests[w.SourceKey(*source)], err = inputs.DigestFor(intent.SelectedSandbox())
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	return intent
}

func TestTeamChangesOnlyConflictWithAffectedExecutions(t *testing.T) {
	for _, change := range []string{"creation default", "add unselected Sandbox", "policy", "remove selected Sandbox", "runtime context", "ownership"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			store, _, before := teamChangeFixture(t)
			intent := teamIntent(t, before, "one")
			ticket := acquire(t, store, intent)
			after := before.DeepCopy()
			allowed := change == "creation default" || change == "add unselected Sandbox"
			switch change {
			case "creation default":
				after.Spec.DefaultSandboxRef = "spare"
			case "add unselected Sandbox":
				after.Spec.SandboxRefs = append(after.Spec.SandboxRefs, "another-spare")
			case "policy":
				after.Spec.AgentPolicyRef = "replacement-policy"
			case "remove selected Sandbox":
				after.Spec.SandboxRefs, after.Spec.DefaultSandboxRef = []string{"spare"}, "spare"
			case "runtime context":
				after.Spec.MemberContext.Role = "changed-role"
			case "ownership":
				after.OwnerReferences = []metav1.OwnerReference{w.AgentOwner("other", "other-uid")}
			}
			mutation, err := store.AcquireTeamChange(ctx, before, after)
			if allowed {
				if err != nil || mutation.Change.Team == nil {
					t.Fatal("unaffected execution blocked Team mutation:", err)
				}
				if err := store.Check(ctx, ticket); err != nil {
					t.Fatal("scoped change invalidated existing execution:", err)
				}
				second := teamIntent(t, before, "two")
				if _, err := store.AcquireExecution(ctx, second); err != nil {
					t.Fatal("unaffected new execution was blocked by pending Team change:", err)
				}
				mutation.Change.Team.After.Sandboxes[0] = "caller-mutation"
				if err := store.CheckChange(ctx, mutation); !errors.Is(err, ErrConflict) {
					t.Fatal("caller modified persisted Team mutation authority:", err)
				}
			} else if !errors.Is(err, ErrConflict) {
				t.Fatal("effective Team change did not conflict:", err)
			}
		})
	}
}

func TestTeamPolicyChangeBlocksExistingAndFutureStarts(t *testing.T) {
	ctx := context.Background()
	store, _, before := teamChangeFixture(t)
	acquire(t, store, teamIntent(t, before, "existing"))
	after := before.DeepCopy()
	after.Spec.AgentPolicyRef = "replacement-policy"
	if _, err := store.AcquireTeamChange(ctx, before, after); !errors.Is(err, ErrConflict) {
		t.Fatal("running execution did not protect the Team policy:", err)
	}
	store, _, before = teamChangeFixture(t)
	after = before.DeepCopy()
	after.Spec.AgentPolicyRef = "replacement-policy"
	change, err := store.AcquireTeamChange(ctx, before, after)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcquireExecution(ctx, teamIntent(t, before, "future")); !errors.Is(err, ErrConflict) {
		t.Fatal("pending policy mutation allowed an affected execution:", err)
	}
	if err := store.CheckChange(ctx, change); err != nil {
		t.Fatal(err)
	}
}

func TestTeamChangeRejectsStaleOrMisrepresentedBeforeImage(t *testing.T) {
	for _, scenario := range []string{"UID", "RV", "spec", "after UID", "after RV", "terminating"} {
		t.Run(scenario, func(t *testing.T) {
			store, _, before := teamChangeFixture(t)
			after := before.DeepCopy()
			after.Spec.DefaultSandboxRef = "spare"
			switch scenario {
			case "UID":
				before.UID, after.UID = "foreign", "foreign"
			case "RV":
				before.ResourceVersion, after.ResourceVersion = "stale", "stale"
			case "spec":
				before.Spec.MemberContext.Role = "not-current"
			case "after UID":
				after.UID = "replacement"
			case "after RV":
				after.ResourceVersion = "replacement"
			case "terminating":
				now := metav1.Now()
				after.DeletionTimestamp = &now
			}

			if _, err := store.AcquireTeamChange(context.Background(), before, after); err == nil {
				t.Fatal("misrepresented or stale Team change acquired authority")
			}
		})
	}
}

func TestTeamChangeRejectsNoopBeforeCreatingReservation(t *testing.T) {
	ctx := context.Background()
	store, storage, before := teamChangeFixture(t)
	key := client.ObjectKey{Namespace: store.Namespace, Name: store.Identity.Name}
	original := &corev1.ConfigMap{}
	if err := storage.Get(ctx, key, original); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcquireTeamChange(ctx, before, before.DeepCopy()); !errors.Is(err, ErrConflict) {
		t.Fatalf("no-op Team change acquired a reservation: %v", err)
	}
	current := &corev1.ConfigMap{}
	if err := storage.Get(ctx, key, current); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, current) {
		t.Fatal("no-op Team change wrote the coordination authority")
	}
}

func TestScopedTeamChangeAndAffectedStartHaveOneCASWinner(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store, storage, before := teamChangeFixture(t)
	after := before.DeepCopy()
	after.Spec.AgentPolicyRef = "replacement-policy"
	var writes atomic.Int32
	barrier := make(chan struct{})
	store.Client = interceptor.NewClient(storage, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.UpdateOption) error {
			switch writes.Add(1) {
			case 1:
				select {
				case <-barrier:
				case <-ctx.Done():
					return ctx.Err()
				}
			case 2:
				close(barrier)
			}
			return c.Update(ctx, object, opts...)
		},
	})
	intent := teamIntent(t, before, "normal")
	results := make(chan error, 2)
	var group sync.WaitGroup
	group.Go(func() {
		_, err := store.AcquireTeamChange(ctx, before, after)
		results <- err
	})
	group.Go(func() {
		_, err := store.AcquireExecution(ctx, intent)
		results <- err
	})
	group.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 || writes.Load() != 2 {
		t.Fatalf("scoped mutation and affected start had %d winners across %d writes", successes, writes.Load())
	}
}

func TestScopedTeamChangeRetainsUnknownOutcomes(t *testing.T) {
	ctx := context.Background()
	store, _, before := teamChangeFixture(t)
	acquire(t, store, teamIntent(t, before, "normal"))
	after := before.DeepCopy()
	after.Spec.DefaultSandboxRef = "spare"
	ticket, err := store.AcquireTeamChange(ctx, before, after)
	if err != nil {
		t.Fatal(err)
	}

	adapter := &syntheticChangeAdapter{submitErr: errors.New("synthetic lost reply"), outcome: completeChange}
	ticket, err = store.ReconcileChange(ctx, ticket, adapter)
	if err == nil || ticket.Change.Phase != Changing {
		t.Fatal("unknown scoped mutation lost its durable intent:", err)
	}
	recovered, err := store.AcquireTeamChange(ctx, before, after)
	if err != nil || !reflect.DeepEqual(recovered, ticket) {
		t.Fatal("scoped mutation could not recover its exact intent:", err)
	}
	if _, err := store.ReconcileChange(ctx, recovered, adapter); err != nil || adapter.submissions != 1 {
		t.Fatal("scoped mutation replayed after restart:", err)
	}
}

func TestScopedTeamChangeRecoversCommittedWriteWithLostReply(t *testing.T) {
	ctx := context.Background()
	store, storage, before := teamChangeFixture(t)
	acquire(t, store, teamIntent(t, before, "normal"))
	after := before.DeepCopy()
	after.Spec.DefaultSandboxRef = "spare"
	ticket, err := store.AcquireTeamChange(ctx, before, after)
	if err != nil {
		t.Fatal(err)
	}
	var committed *api.Team
	adapter := &syntheticChangeAdapter{
		submit: func(ctx context.Context, _ ChangeTicket) error {
			committed = after.DeepCopy()
			if err := storage.Update(ctx, committed); err != nil {
				return err
			}
			return errors.New("synthetic lost reply after committed Team write")
		},
		outcome: func(ticket ChangeTicket) *ChangeOutcome {
			outcome := completeChange(ticket)
			outcome.After.ResourceVersion = committed.ResourceVersion
			return outcome
		},
	}
	_, err = store.ReconcileChange(ctx, ticket, adapter)
	if err == nil || committed == nil || committed.ResourceVersion == before.ResourceVersion {
		t.Fatal("fixture did not commit the Team update before losing its reply")
	}
	restarted := Store{Client: storage, APIReader: storage, Namespace: store.Namespace, Identity: store.Identity}
	recovered, err := restarted.AcquireTeamChange(ctx, before, after)
	if err != nil || recovered.Change.ID != ticket.Change.ID || recovered.Change.Phase != Changing {
		t.Fatal("committed Team update prevented recovery of persisted mutation:", err)
	}
	if _, err := restarted.ReconcileChange(ctx, recovered, adapter); err != nil || adapter.submissions != 1 {
		t.Fatal("recovery resubmitted a committed Team change:", err)
	}
}

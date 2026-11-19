// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxcoordination

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func testNativeIntent(suffix string) NativeIntent {
	intent := NativeIntent{
		Agent: w.ObjectIdentity{Name: "agent-" + suffix, UID: types.UID("agent-uid-" + suffix)}, AgentGeneration: 1,
		Namespace: w.ObjectIdentity{Name: "cetirun-agent-" + suffix, UID: types.UID("namespace-uid-" + suffix)},
		Home:      w.ObjectIdentity{Name: "home", UID: types.UID("home-uid-" + suffix)},
		Workload:  w.ObjectIdentity{Name: "agent-" + suffix, UID: types.UID("workload-uid-" + suffix)},
		Binding: sandbox.Binding{AgentUID: types.UID("agent-uid-" + suffix), TeamUID: "team-uid",
			SandboxUID: "sandbox-uid", AgentPolicyUID: "policy-uid"},
		Revision: "native-revision",
		Sources: []w.SourceReference{
			source(api.GroupVersion.String(), "Team", "", "team", "team-uid"),
			source(api.GroupVersion.String(), "Sandbox", "", "sandbox", "sandbox-uid"),
			source(api.GroupVersion.String(), "AgentPolicy", "", "policy", "policy-uid"),
		},
		SourceDigests: map[string]string{},
	}
	for _, reference := range intent.Sources {
		intent.SourceDigests[w.SourceKey(reference)] = strings.Repeat("e", 64)
	}
	return intent
}

func acquireNative(t *testing.T, store Store, intent NativeIntent) NativeTicket {
	t.Helper()
	ticket, err := store.AcquireNative(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	return ticket
}

func TestNativeStatePreservesDefaultSerialization(t *testing.T) {
	ctx := context.Background()
	store, _ := fixture(t)
	object, _, err := store.read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const baseline = `{"version":1,"executions":[],"changes":[]}`
	if object.Data[DataKey] != baseline {
		t.Fatalf("default coordination wire format changed: %s", object.Data[DataKey])
	}
	ticket := acquireNative(t, store, testNativeIntent("one"))
	object, state, err := store.read(ctx)
	if err != nil || len(state.Executions) != 0 || len(state.NativeReservations) != 1 {
		t.Fatalf("native reservation fabricated an enrolled execution: %+v, %v", state, err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(object.Data[DataKey]), &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 4 || len(fields["nativeReservations"]) == 0 {
		t.Fatal("native state did not use the optional bounded field")
	}
	stopping, err := store.BeginNativeStop(ctx, ticket)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReleaseNative(ctx, stopping); err != nil {
		t.Fatal(err)
	}
	object, _, err = store.read(ctx)
	if err != nil || object.Data[DataKey] != baseline {
		t.Fatalf("last release did not restore old default serialization: %v", err)
	}
}

func TestNativeSelectedChangesRemainExcludedUntilRelease(t *testing.T) {
	ctx := context.Background()
	store, _ := fixture(t)
	intent := testNativeIntent("one")
	ticket := acquireNative(t, store, intent)
	for _, stopping := range []bool{false, true} {
		if stopping {
			var err error
			ticket, err = store.BeginNativeStop(ctx, ticket)
			if err != nil {
				t.Fatal(err)
			}
		}
		for _, reference := range intent.Sources {
			if _, err := store.AcquireChange(ctx, reference, strings.Repeat("a", 64)); !errors.Is(err, ErrConflict) {
				t.Fatalf("selected %s mutation accepted (stopping=%v): %v", reference.Kind, stopping, err)
			}
		}
		err := store.AuthorizeNativeStart(ctx, intent)
		if (err != nil) != stopping {
			t.Fatalf("native admission changed incorrectly (stopping=%v): %v", stopping, err)
		}
		if !stopping {
			if err := store.ReleaseNative(ctx, ticket); !errors.Is(err, ErrConflict) {
				t.Fatal("active reservation released:", err)
			}
		} else if _, err := store.AcquireNative(ctx, intent); !errors.Is(err, ErrConflict) {
			t.Fatal("stopping reservation allowed replay:", err)
		}
	}
	if err := store.ReleaseNative(ctx, ticket); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcquireChange(ctx, intent.Sources[2], strings.Repeat("a", 64)); err != nil {
		t.Fatal("released selection still blocked its writer:", err)
	}
	if err := store.AuthorizeNativeStart(ctx, intent); !errors.Is(err, ErrConflict) {
		t.Fatal("missing reservation authorized Pod admission:", err)
	}
}

func TestNativePendingChangeExcludesStart(t *testing.T) {
	for _, phase := range []Phase{Reserved, Changing} {
		t.Run(string(phase), func(t *testing.T) {
			ctx := context.Background()
			store, _ := fixture(t)
			intent := testNativeIntent("one")
			change, err := store.AcquireChange(ctx, intent.Sources[1], strings.Repeat("a", 64))
			if err != nil {
				t.Fatal(err)
			}
			if phase == Changing {
				if _, err := store.BeginChange(ctx, change); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.AcquireNative(ctx, intent); !errors.Is(err, ErrConflict) {
				t.Fatal("pending selected mutation allowed native reservation:", err)
			}
			if err := store.AuthorizeNativeStart(ctx, intent); !errors.Is(err, ErrConflict) {
				t.Fatal("pending selected mutation allowed Pod admission:", err)
			}
			intent.Sources[1] = source(api.GroupVersion.String(), "Sandbox", "", "unrelated", "unrelated-uid")
			delete(intent.SourceDigests, w.SourceKey(testNativeIntent("one").Sources[1]))
			intent.SourceDigests[w.SourceKey(intent.Sources[1])] = strings.Repeat("b", 64)
			intent.Binding.SandboxUID = intent.Sources[1].UID
			acquireNative(t, store, intent)
		})
	}
}

func testNativeCASRace(t *testing.T, store Store, storage client.WithWatch) {
	t.Helper()
	ctx := context.Background()
	var reads atomic.Int32
	barrier := make(chan struct{})
	store.APIReader = interceptor.NewClient(storage, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
			if err := c.Get(ctx, key, object, opts...); err != nil {
				return err
			}
			switch reads.Add(1) {
			case 1:
				<-barrier
			case 2:
				close(barrier)
			}
			return nil
		},
	})
	results := make(chan error, 2)
	var group sync.WaitGroup
	group.Go(func() {
		_, err := store.AcquireNative(ctx, testNativeIntent("one"))
		results <- err
	})
	group.Go(func() {
		_, err := store.AcquireChange(ctx, testNativeIntent("one").Sources[2], strings.Repeat("a", 64))
		results <- err
	})
	group.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !apierrors.IsConflict(err) && !errors.Is(err, ErrConflict) {
			t.Fatal("unexpected acquisition failure:", err)
		}
	}
	if successes != 1 {
		t.Fatalf("start/change race had %d CAS winners, want exactly one", successes)
	}
	_, state, err := store.read(ctx)
	if err != nil || len(state.NativeReservations)+len(state.Changes) != 1 {
		t.Fatalf("race persisted overlapping operations: %+v %v", state, err)
	}
}

func TestNativeStartAndChangeHaveOneCASWinner(t *testing.T) {
	store, storage := fixture(t)
	testNativeCASRace(t, store, storage)
}

func TestNativeReservationIdentityAndReadOnlyChecks(t *testing.T) {
	ctx := context.Background()
	store, _ := fixture(t)
	intent := testNativeIntent("one")
	first := acquireNative(t, store, intent)
	again := acquireNative(t, store, intent)
	if first.Reservation.ID != again.Reservation.ID {
		t.Fatal("retry created a second operation")
	}
	store.Client = nil
	if err := store.AuthorizeNativeStart(ctx, intent); err != nil {
		t.Fatal("read-only admission required a writer:", err)
	}
	for name, mutate := range map[string]func(*NativeIntent){
		"generation": func(i *NativeIntent) { i.AgentGeneration++ },
		"namespace":  func(i *NativeIntent) { i.Namespace.UID = "replaced-namespace" },
		"HOME":       func(i *NativeIntent) { i.Home.UID = "replaced-home" },
		"workload":   func(i *NativeIntent) { i.Workload.UID = "replaced-workload" },
		"revision":   func(i *NativeIntent) { i.Revision = "changed-revision" },
		"selection": func(i *NativeIntent) {
			i.Binding.AgentPolicyUID, i.Sources[2].UID = "replacement-policy", "replacement-policy"
		},
		"digest": func(i *NativeIntent) { i.SourceDigests[w.SourceKey(i.Sources[1])] = strings.Repeat("a", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			changed := first.DeepCopy().Reservation.Intent
			mutate(&changed)
			if err := store.AuthorizeNativeStart(ctx, changed); !errors.Is(err, ErrConflict) {
				t.Fatal("stale reservation authorized a different lifetime:", err)
			}
		})
	}
	first.Reservation.Intent.Sources[0].Name = "caller-mutation"
	again.Reservation.Intent.SourceDigests[w.SourceKey(intent.Sources[0])] = strings.Repeat("a", 64)
	if err := store.AuthorizeNativeStart(ctx, intent); err != nil {
		t.Fatal("caller mutation changed durable authority:", err)
	}
	store.Identity.UID = "replacement-authority"
	if err := store.AuthorizeNativeStart(ctx, intent); !errors.Is(err, ErrUnavailable) {
		t.Fatal("replaced authority authorized native admission:", err)
	}
	store.Identity.Name = "missing-authority"
	if err := store.AuthorizeNativeStart(ctx, intent); !errors.Is(err, ErrUnavailable) {
		t.Fatal("missing authority authorized native admission:", err)
	}
}

func TestNativeIntentSelectsBoundSourcesOnly(t *testing.T) {
	intent := testNativeIntent("one")
	expected := w.Expectation{
		Version: w.ExpectationVersion, WorkloadKind: w.AgentWorkload,
		Agent: intent.Agent, AgentGeneration: intent.AgentGeneration, Namespace: intent.Namespace,
		Home: intent.Home, Workload: intent.Workload, Binding: intent.Binding, Revision: intent.Revision,
		Sources: slices.Clone(intent.Sources), SourceDigests: maps.Clone(intent.SourceDigests),
	}
	expected.Sources = append(expected.Sources,
		source(api.GroupVersion.String(), "Sandbox", "", "unused-sandbox", "unused-sandbox-uid"),
		source(api.GroupVersion.String(), "AgentPolicy", "", "unused-policy", "unused-policy-uid"))
	selected, err := NativeIntentFor(expected)
	if err != nil || len(selected.Sources) != 3 || len(selected.SourceDigests) != 3 {
		t.Fatalf("unused Team slots were required by the reservation: %+v %v", selected, err)
	}
	expected.Sources = append(expected.Sources, intent.Sources[2])
	if _, err := NativeIntentFor(expected); err == nil {
		t.Fatal("ambiguous selected identity was accepted")
	}
}

func TestNativeRecordBoundsAndMalformedReservations(t *testing.T) {
	for name, mutate := range map[string]func(*record){
		"missing selected source": func(s *record) { s.NativeReservations[0].Intent.Sources = s.NativeReservations[0].Intent.Sources[:2] },
		"missing digest": func(s *record) {
			delete(s.NativeReservations[0].Intent.SourceDigests, w.SourceKey(s.NativeReservations[0].Intent.Sources[0]))
		},
		"duplicate operation": func(s *record) { s.NativeReservations = append(s.NativeReservations, s.NativeReservations[0]) },
		"qualified phase":     func(s *record) { s.NativeReservations[0].Phase = Running },
		"overlap": func(s *record) {
			s.Changes = append(s.Changes, Change{ID: "mutation", Before: s.NativeReservations[0].Intent.Sources[0],
				DesiredDigest: strings.Repeat("a", 64), Phase: Changing})
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store, storage := fixture(t)
			acquireNative(t, store, testNativeIntent("one"))
			object, state, err := store.read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			mutate(&state)
			raw, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			object.Data[DataKey] = string(raw)
			if err := storage.Update(ctx, object); err != nil {
				t.Fatal(err)
			}
			if err := store.AuthorizeNativeStart(ctx, testNativeIntent("one")); err == nil {
				t.Fatal("corrupt native authority authorized admission")
			}
		})
	}
	store, storage := fixture(t)
	object, state, err := store.read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for i := range MaxEntries {
		state.NativeReservations = append(state.NativeReservations, NativeReservation{
			ID: fmt.Sprint("native-", i), Phase: Reserved, Intent: testNativeIntent(fmt.Sprint(i)),
		})
	}
	if err := encode(object, state); err != nil {
		t.Fatal("maximum supported native inventory rejected:", err)
	}
	if err := storage.Update(context.Background(), object); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcquireNative(context.Background(), testNativeIntent("extra")); !errors.Is(err, ErrUnavailable) {
		t.Fatal("native reservations did not share the entry bound:", err)
	}
}

func TestNativeAndQualifiedReservationsCannotShareALifetime(t *testing.T) {
	for _, nativeFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(nativeFirst), func(t *testing.T) {
			ctx := context.Background()
			store, _ := fixture(t)
			native, qualified := testNativeIntent("one"), testIntent("one")
			if nativeFirst {
				acquireNative(t, store, native)
				if _, err := store.AcquireExecution(ctx, qualified); !errors.Is(err, ErrConflict) {
					t.Fatal("qualified execution reused a native lifetime:", err)
				}
				acquire(t, store, testIntent("other"))
			} else {
				acquire(t, store, qualified)
				if _, err := store.AcquireNative(ctx, native); !errors.Is(err, ErrConflict) {
					t.Fatal("native execution reused a qualified lifetime:", err)
				}
				acquireNative(t, store, testNativeIntent("other"))
			}
		})
	}
}

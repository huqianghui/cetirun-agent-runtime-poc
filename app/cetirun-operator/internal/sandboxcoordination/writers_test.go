// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxcoordination

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxwriter"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

type syntheticWriterSource struct {
	writer sandboxwriter.Writer
	calls  int
	err    error
}

func (s *syntheticWriterSource) ResolveWriter(context.Context, Ticket) (sandboxwriter.Writer, error) {
	s.calls++
	return s.writer, s.err
}

type syntheticExclusion struct {
	now               time.Time
	revokes, observes int
	revokeErr         error
	pendingStorage    bool
}

func (s *syntheticExclusion) Revoke(context.Context, sandboxwriter.Request) error {
	s.revokes++
	return s.revokeErr
}

func (s *syntheticExclusion) Observe(_ context.Context, request sandboxwriter.Request) (sandboxwriter.Observation, error) {
	s.observes++
	observation := sandboxwriter.Observation{
		Request: request, ObservedAt: s.now, Coverage: sandboxwriter.CompleteHistory,
		Barrier: sandboxwriter.BarrierEvidence{ID: "synthetic-barrier", Method: sandboxwriter.DurableStartBarrier},
		Home: sandboxwriter.HomeRelease{Home: request.Lifetime.Home, State: sandboxwriter.Released,
			EvidenceID: "synthetic-home-release", Volumes: []sandboxwriter.StorageRelease{}},
		Writers: []sandboxwriter.WriterEvidence{},
	}
	seen := map[sandboxwriter.StorageIdentity]bool{}
	for _, writer := range request.Writers {
		observation.Writers = append(observation.Writers, sandboxwriter.WriterEvidence{
			Writer: writer, Method: sandboxwriter.RuntimeRevocation, EvidenceID: "synthetic-revocation-" + writer.Epoch,
		})
		if !seen[writer.Storage] {
			seen[writer.Storage] = true
			observation.Home.Volumes = append(observation.Home.Volumes, sandboxwriter.StorageRelease{
				Storage: writer.Storage, State: sandboxwriter.Released, EvidenceID: "synthetic-volume-release",
			})
		}
	}
	if s.pendingStorage {
		observation.Home.State = "pending"
	}
	return observation, nil
}

func journalWriterFixture(t *testing.T) (Lifecycle, Ticket, *syntheticAdapter, *JournalWriters, *syntheticExclusion) {
	t.Helper()
	store, storage := fixture(t)
	intent := testIntent("one")
	runtime := &syntheticAdapter{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	provider := &syntheticExclusion{now: runtime.now}
	creates := 0
	writerStore := sandboxwriter.Store{
		Client: interceptor.NewClient(storage, interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
				creates++
				object.SetUID(types.UID(fmt.Sprintf("synthetic-writer-journal-%d", creates)))
				return c.Create(ctx, object, opts...)
			},
		}),
		APIReader: storage, Namespace: store.Namespace,
	}
	manager := &sandboxwriter.Manager{Store: writerStore, Provider: provider, Now: func() time.Time { return provider.now }, MaxAge: time.Minute}
	source := &syntheticWriterSource{writer: intent.Writer}
	enrolled, err := manager.Store.Enroll(context.Background(),
		sandboxwriter.Lifetime{Agent: intent.Agent, Namespace: intent.Namespace, Home: intent.Home}, "synthetic-birth-coverage")
	if err != nil {
		t.Fatal(err)
	}
	intent.WriterJournal = w.ObjectIdentity{Name: sandboxwriter.JournalName(enrolled.Lifetime), UID: enrolled.UID}
	ticket := acquire(t, store, intent)
	writers := &JournalWriters{Manager: manager, Source: source}
	return Lifecycle{Store: store, Adapter: runtime, Writers: writers, Now: func() time.Time { return runtime.now }, MaxAge: 10 * time.Second},
		ticket, runtime, writers, provider
}

func TestLifecycleRecordsWriterBeforeStartingAndRetainsHistory(t *testing.T) {
	lifecycle, ticket, runtime, writers, provider := journalWriterFixture(t)
	runtime.beforeStart = func() {
		snapshot, err := writers.Manager.Store.Read(context.Background(), writerLifetime(ticket))
		if err != nil || snapshot.Phase != sandboxwriter.PhaseReserved || len(snapshot.Writers) != 1 ||
			snapshot.Writers[0].Pod.UID != ticket.Execution.Intent.Expected.PodUID {
			t.Fatal("runtime start preceded durable writer reservation:", err)
		}
	}
	result, err := lifecycle.Reconcile(context.Background(), ticket)
	if err != nil || !result.Verified || provider.revokes != 1 {
		t.Fatalf("synthetic integrated start did not complete: %+v %v", result, err)
	}
	if err := lifecycle.Stop(context.Background(), result.Ticket); err != nil {
		t.Fatal(err)
	}
	history, err := writers.Manager.Store.Read(context.Background(), writerLifetime(ticket))
	if err != nil || history.Phase != sandboxwriter.PhaseQuiescent || len(history.Writers) != 1 || provider.revokes != 2 {
		t.Fatal("stop discarded history or failed to revoke the writer:", err)
	}
}

func TestWriterReservationRetryDoesNotRevokeOrAppendAgain(t *testing.T) {
	lifecycle, ticket, _, writers, provider := journalWriterFixture(t)
	guard := func(ctx context.Context) error { return lifecycle.Store.Check(ctx, ticket) }
	if err := writers.Reserve(context.Background(), ticket, guard); err != nil {
		t.Fatal(err)
	}
	revokes, observes := provider.revokes, provider.observes
	restarted := *writers
	if err := restarted.Reserve(context.Background(), ticket, guard); err != nil {
		t.Fatal(err)
	}
	history, err := writers.Manager.Store.Read(context.Background(), writerLifetime(ticket))
	if err != nil || len(history.Writers) != 1 || provider.revokes != revokes || provider.observes != observes {
		t.Fatal("retry repeated revocation or history append:", err)
	}
}

func TestWriterCleanupDoesNotResolveCurrentInputs(t *testing.T) {
	lifecycle, ticket, _, writers, provider := journalWriterFixture(t)
	result, err := lifecycle.Reconcile(context.Background(), ticket)
	if err != nil {
		t.Fatal(err)
	}
	source := writers.Source.(*syntheticWriterSource)
	calls := source.calls
	source.err = errors.New("synthetic source removed during suspension")
	provider.pendingStorage = true
	if err := lifecycle.Stop(context.Background(), result.Ticket); err == nil {
		t.Fatal("pending storage release was ignored")
	}
	stopping, err := lifecycle.Store.ReadExecution(context.Background(), ticket.Execution.Intent.Agent.UID)
	if err != nil || stopping.Execution.Phase != Stopping || source.calls != calls {
		t.Fatal("historical cleanup depended on current source or released authority:", err)
	}
	revokes := provider.revokes
	provider.pendingStorage = false
	if err := lifecycle.Stop(context.Background(), stopping); err != nil || provider.revokes != revokes || source.calls != calls {
		t.Fatal("cleanup recovery rediscovered or re-revoked the writer:", err)
	}
}

func TestDifferentActiveWriterEpochCannotBeRetargeted(t *testing.T) {
	lifecycle, ticket, _, writers, provider := journalWriterFixture(t)
	guard := func(ctx context.Context) error { return lifecycle.Store.Check(ctx, ticket) }
	if err := writers.Reserve(context.Background(), ticket, guard); err != nil {
		t.Fatal(err)
	}
	foreign := ticket
	foreign.Execution.Intent.Expected.EnforcementEpoch = "different-enrolled-epoch"
	foreign.Execution.Intent.Writer.Epoch = foreign.Execution.Intent.Expected.EnforcementEpoch
	foreign.Execution.Phase = Stopping
	calls := provider.revokes
	// Even a caller-provided successful guard cannot turn a historical request
	// into a request to revoke a different active epoch.
	if err := writers.Quiesce(context.Background(), foreign, func(context.Context) error { return nil }); !errors.Is(err, ErrConflict) ||
		provider.revokes != calls {
		t.Fatal("cleanup retargeted a different active epoch:", err)
	}
}

func TestWriterCleanupMatchesPhysicalAndJournalIncarnations(t *testing.T) {
	for _, scenario := range []string{"machine", "volume", "journal"} {
		t.Run(scenario, func(t *testing.T) {
			lifecycle, ticket, _, writers, provider := journalWriterFixture(t)
			if err := writers.Reserve(context.Background(), ticket, func(ctx context.Context) error {
				return lifecycle.Store.Check(ctx, ticket)
			}); err != nil {
				t.Fatal(err)
			}
			stopping := ticket.DeepCopy()
			stopping.Execution.Phase = Stopping
			switch scenario {
			case "machine":
				stopping.Execution.Intent.Writer.MachineID = "different-physical-incarnation"
			case "volume":
				stopping.Execution.Intent.Writer.Storage.Handle = "different-volume-incarnation"
			case "journal":
				stopping.Execution.Intent.WriterJournal.UID = "different-journal-incarnation"
			}
			revokes, observes := provider.revokes, provider.observes
			if err := writers.Quiesce(context.Background(), stopping, func(context.Context) error { return nil }); !errors.Is(err, ErrConflict) ||
				provider.revokes != revokes || provider.observes != observes {
				t.Fatal("cleanup adopted a different physical or journal incarnation:", err)
			}
		})
	}
}

func TestWriterSourceMustMatchEnrolledTarget(t *testing.T) {
	for _, scenario := range []string{"Pod", "Node", "machine", "volume", "epoch", "machine replacement", "volume replacement"} {
		t.Run(scenario, func(t *testing.T) {
			lifecycle, ticket, runtime, writers, provider := journalWriterFixture(t)
			source := writers.Source.(*syntheticWriterSource)
			switch scenario {
			case "Pod":
				source.writer.Pod.UID = "foreign-pod"
			case "Node":
				source.writer.Node.ProviderID = "example://foreign-node"
			case "machine":
				source.writer.MachineID = ""
			case "machine replacement":
				source.writer.MachineID = "different-physical-incarnation"
			case "volume":
				source.writer.Storage.Volume.UID = ""
			case "volume replacement":
				source.writer.Storage.Handle = "different-volume-incarnation"
			case "epoch":
				source.writer.Epoch = "foreign-epoch"
			}
			if err := writers.Reserve(context.Background(), ticket, func(ctx context.Context) error {
				return lifecycle.Store.Check(ctx, ticket)
			}); err == nil || provider.revokes != 0 || runtime.starts != 0 {
				t.Fatal("mismatched target caused provider or runtime side effects:", err)
			}
		})
	}
}

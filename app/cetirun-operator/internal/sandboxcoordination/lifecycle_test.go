// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxcoordination

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
)

type syntheticAdapter struct {
	now                  time.Time
	events               []string
	sourceErr            error
	startErr             error
	publishErr           error
	withdrawErr          error
	reserveErr           error
	quiesceErr           error
	commitErr            error
	clearErr             error
	ready                bool
	beforeCommit         func()
	ack                  func(*PreStartObservation)
	observation          func(int, *sandbox.Observation) error
	checkPrepared        func()
	mutateInput          bool
	beforeStart          func()
	beforePublish        func()
	afterPublish         func()
	starts, observations int
}

func (a *syntheticAdapter) CheckPrepared(ctx context.Context, ticket Ticket) error {
	if a.mutateInput {
		ticket.Execution.Intent.Sources[0].IntentDigest = strings.Repeat("c", 64)
		ticket.Execution.Intent.Placement.Nodes[0].ProviderID = "example://adapter-copy"
	}
	if a.checkPrepared != nil {
		a.checkPrepared()
	}
	return errors.Join(ctx.Err(), a.sourceErr)
}

func (a *syntheticAdapter) PrepareStart(_ context.Context, ticket Ticket) (*PreStartObservation, error) {
	a.events = append(a.events, "pre-start")
	ack := &PreStartObservation{
		AuthorityUID: string(ticket.AuthorityUID), OperationID: ticket.Execution.ID,
		Expected: ticket.Execution.Intent.Expected, ObservedAt: a.now,
		ControlsReady: true, StartGateArmed: true,
	}
	if a.ack != nil {
		a.ack(ack)
	}
	return ack, nil
}

func (a *syntheticAdapter) Start(ctx context.Context, _ Ticket, guard func(context.Context) error) error {
	if a.beforeStart != nil {
		a.beforeStart()
	}
	if err := guard(ctx); err != nil {
		return err
	}
	a.starts++
	a.events = append(a.events, "start")
	return a.startErr
}

func (a *syntheticAdapter) Observe(_ context.Context, ticket Ticket) (*sandbox.Observation, error) {
	a.observations++
	a.events = append(a.events, "observe")
	observation := &sandbox.Observation{ExecutionIdentity: ticket.Execution.Intent.Expected,
		ObservedAt: a.now, RuntimeVerified: true, PodVerified: true, ControlsVerified: true}
	if a.observation != nil {
		if err := a.observation(a.observations, observation); err != nil {
			return nil, err
		}
	}
	return observation, nil
}

func (a *syntheticAdapter) PublishRoutes(ctx context.Context, _ Ticket, guard func(context.Context) error) error {
	if a.beforePublish != nil {
		a.beforePublish()
	}
	if err := guard(ctx); err != nil {
		return err
	}
	a.events = append(a.events, "publish")
	if a.afterPublish != nil {
		a.afterPublish()
	}
	return a.publishErr
}

func (a *syntheticAdapter) WithdrawRoutes(ctx context.Context, _ Ticket, guard func(context.Context) error) error {
	if err := guard(ctx); err != nil {
		return err
	}
	a.events = append(a.events, "withdraw")
	return a.withdrawErr
}

func (a *syntheticAdapter) CommitReadiness(ctx context.Context, _ Ticket, _ sandbox.Observation, guard func(context.Context) error) error {
	if a.beforeCommit != nil {
		a.beforeCommit()
	}
	if err := guard(ctx); err != nil {
		return err
	}
	a.events = append(a.events, "commit-readiness")
	a.ready = true
	return a.commitErr
}

func (a *syntheticAdapter) ClearReadiness(ctx context.Context, _ Ticket, guard func(context.Context) error) error {
	if err := guard(ctx); err != nil {
		return err
	}
	a.events = append(a.events, "clear-readiness")
	if a.clearErr != nil {
		return a.clearErr
	}
	a.ready = false
	return nil
}

func (a *syntheticAdapter) Reserve(ctx context.Context, _ Ticket, guard func(context.Context) error) error {
	if err := guard(ctx); err != nil {
		return err
	}
	a.events = append(a.events, "reserve-writer")
	return a.reserveErr
}

func (a *syntheticAdapter) Quiesce(ctx context.Context, _ Ticket, guard func(context.Context) error) error {
	if err := guard(ctx); err != nil {
		return err
	}
	a.events = append(a.events, "exclude-writers")
	return a.quiesceErr
}

func lifecycleFixture(t *testing.T) (Lifecycle, Ticket, *syntheticAdapter) {
	t.Helper()
	store, _ := fixture(t)
	ticket := acquire(t, store, testIntent("one"))
	adapter := &syntheticAdapter{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	return Lifecycle{Store: store, Adapter: adapter, Writers: adapter,
		Now: func() time.Time { return adapter.now }, MaxAge: 10 * time.Second}, ticket, adapter
}

func TestOrderedStartAndCurrentObservation(t *testing.T) {
	lifecycle, ticket, adapter := lifecycleFixture(t)
	result, err := lifecycle.Reconcile(context.Background(), ticket)
	if err != nil || result.Hold || !result.Verified || result.Observation == nil || result.Ticket.Execution.Phase != Running {
		t.Fatalf("synthetic complete integration did not verify: %+v %v", result, err)
	}
	want := []string{"reserve-writer", "pre-start", "start", "observe", "publish", "observe", "commit-readiness"}
	if !slices.Equal(adapter.events, want) {
		t.Fatalf("lifecycle ordering changed: %v", adapter.events)
	}
	// A new coordinator instance must re-observe but never re-start this writer.
	restarted := lifecycle
	result, err = restarted.Reconcile(context.Background(), result.Ticket)
	if err != nil || !result.Verified || adapter.starts != 1 || adapter.observations != 4 {
		t.Fatal("restart replayed execution or reused old evidence:", err)
	}
}

func TestAdapterCannotMutateRetainedIntent(t *testing.T) {
	lifecycle, ticket, adapter := lifecycleFixture(t)
	adapter.mutateInput = true
	result, err := lifecycle.Reconcile(context.Background(), ticket)
	if err != nil || !result.Verified || result.Ticket.Execution.Intent.Sources[0] != ticket.Execution.Intent.Sources[0] ||
		result.Ticket.Execution.Intent.Placement.Nodes[0] != ticket.Execution.Intent.Placement.Nodes[0] {
		t.Fatalf("adapter mutated retained authority or cleanup input: %+v %v", result, err)
	}
}

func TestPreStartRequiresFreshExactAcknowledgement(t *testing.T) {
	for name, mutate := range map[string]func(*PreStartObservation){
		"authority":    func(a *PreStartObservation) { a.AuthorityUID = "other-authority" },
		"operation":    func(a *PreStartObservation) { a.OperationID = "old-operation" },
		"Pod":          func(a *PreStartObservation) { a.Expected.PodUID = "old-pod" },
		"Node":         func(a *PreStartObservation) { a.Expected.NodeUID = "old-node" },
		"epoch":        func(a *PreStartObservation) { a.Expected.EnforcementEpoch = "old-epoch" },
		"policy":       func(a *PreStartObservation) { a.Expected.Binding.AgentPolicyUID = "old-policy" },
		"revision":     func(a *PreStartObservation) { a.Expected.Revision = "old-revision" },
		"missing time": func(a *PreStartObservation) { a.ObservedAt = time.Time{} },
		"stale":        func(a *PreStartObservation) { a.ObservedAt = a.ObservedAt.Add(-11 * time.Second) },
		"future":       func(a *PreStartObservation) { a.ObservedAt = a.ObservedAt.Add(time.Second) },
		"no controls":  func(a *PreStartObservation) { a.ControlsReady = false },
		"no gate":      func(a *PreStartObservation) { a.StartGateArmed = false },
	} {
		t.Run(name, func(t *testing.T) {
			lifecycle, ticket, adapter := lifecycleFixture(t)
			adapter.ack = mutate
			result, err := lifecycle.Reconcile(context.Background(), ticket)
			if err == nil || !result.Hold || result.Verified || adapter.starts != 0 ||
				slices.Contains(adapter.events, "publish") || !slices.Contains(adapter.events, "exclude-writers") {
				t.Fatalf("invalid acknowledgement enabled execution: %+v %v %v", result, adapter.events, err)
			}
		})
	}
}

func TestPreStartAgesAtTheActualStartBoundary(t *testing.T) {
	lifecycle, ticket, adapter := lifecycleFixture(t)
	adapter.beforeStart = func() { adapter.now = adapter.now.Add(11 * time.Second) }
	result, err := lifecycle.Reconcile(context.Background(), ticket)
	if err == nil || !result.Hold || result.Verified || adapter.starts != 0 {
		t.Fatalf("expired acknowledgement authorized a delayed start: %+v %v", result, err)
	}
}

func TestRecoveredStartIsObservationOnly(t *testing.T) {
	lifecycle, ticket, adapter := lifecycleFixture(t)
	starting, err := lifecycle.Store.BeginStart(context.Background(), ticket)
	if err != nil {
		t.Fatal(err)
	}
	result, err := lifecycle.Reconcile(context.Background(), starting)
	if err != nil || !result.Verified || adapter.starts != 0 || slices.Contains(adapter.events, "reserve-writer") {
		t.Fatalf("recovered uncertain intent was resubmitted: %+v %v %v", result, adapter.events, err)
	}
}

func TestInitialPendingNeverPublishesOrRepeatsStart(t *testing.T) {
	lifecycle, ticket, adapter := lifecycleFixture(t)
	adapter.observation = func(call int, observation *sandbox.Observation) error {
		if call == 1 {
			observation.ControlsVerified = false
		}
		return nil
	}
	result, err := lifecycle.Reconcile(context.Background(), ticket)
	if !errors.Is(err, sandbox.ErrEnforcementPending) || !result.Hold || result.Verified ||
		result.Ticket.Execution.Phase != Starting || slices.Contains(adapter.events, "publish") {
		t.Fatalf("pending initial enforcement became ready: %+v %v", result, err)
	}
	result, err = lifecycle.Reconcile(context.Background(), result.Ticket)
	if err != nil || !result.Verified || adapter.starts != 1 {
		t.Fatal("pending start was not observed to completion:", err)
	}
}

func TestObservationLossWithdrawsAndKeepsReservationUntilFenced(t *testing.T) {
	lifecycle, ticket, adapter := lifecycleFixture(t)
	result, err := lifecycle.Reconcile(context.Background(), ticket)
	if err != nil {
		t.Fatal(err)
	}
	adapter.events = nil
	adapter.observation = func(int, *sandbox.Observation) error { return sandbox.ErrObservationUnavailable }
	adapter.quiesceErr = errors.New("synthetic unreachable writer: durable exclusion pending")
	result, err = lifecycle.Reconcile(context.Background(), result.Ticket)
	if err == nil || !result.Hold || result.Verified || result.Observation != nil ||
		adapter.ready || !slices.Equal(adapter.events, []string{"observe", "clear-readiness", "withdraw", "exclude-writers"}) {
		t.Fatalf("observation loss retained readiness or skipped cleanup: %+v %v %v", result, adapter.events, err)
	}
	pending, err := lifecycle.Store.ReadExecution(context.Background(), ticket.Execution.Intent.Agent.UID)
	if err != nil || pending.Execution.Phase != Stopping {
		t.Fatal("unknown writer was forgotten:", err)
	}
	if _, err := lifecycle.Store.AcquireChange(context.Background(), ticket.Execution.Intent.Sources[4], strings.Repeat("b", 64)); !errors.Is(err, ErrConflict) {
		t.Fatal("shared change bypassed pending writer exclusion:", err)
	}
	adapter.sourceErr = errors.New("synthetic deleted policy")
	adapter.quiesceErr = nil
	if err := lifecycle.Stop(context.Background(), pending); err != nil {
		t.Fatal("historical cleanup depended on current policy availability:", err)
	}
	if adapter.starts != 1 {
		t.Fatal("cleanup restarted the writer")
	}
}

func TestPublicationBoundaryRequiresFreshEvidence(t *testing.T) {
	for _, test := range []struct {
		name  string
		delay time.Duration
		valid bool
	}{
		{name: "exact freshness bound", delay: 10 * time.Second, valid: true},
		{name: "past freshness bound", delay: 10*time.Second + time.Nanosecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			lifecycle, ticket, adapter := lifecycleFixture(t)
			adapter.beforePublish = func() { adapter.now = adapter.now.Add(test.delay) }
			result, err := lifecycle.Reconcile(context.Background(), ticket)
			if test.valid {
				if err != nil || !result.Verified || !slices.Contains(adapter.events, "publish") {
					t.Fatalf("fresh boundary observation was rejected: %+v %v", result, err)
				}
				return
			}
			if !errors.Is(err, sandbox.ErrObservationUnavailable) || !result.Hold || result.Verified ||
				slices.Contains(adapter.events, "publish") || slices.Contains(adapter.events, "commit-readiness") ||
				!slices.Contains(adapter.events, "withdraw") || !slices.Contains(adapter.events, "exclude-writers") {
				t.Fatalf("expired publication evidence was accepted: %+v %v %v", result, adapter.events, err)
			}
		})
	}
}

func TestPostPublicationEvidenceIsRequired(t *testing.T) {
	for _, failure := range []string{"observation", "source", "publish"} {
		t.Run(failure, func(t *testing.T) {
			lifecycle, ticket, adapter := lifecycleFixture(t)
			switch failure {
			case "observation":
				adapter.observation = func(call int, observation *sandbox.Observation) error {
					if call == 2 {
						observation.PodUID = "replacement-pod"
					}
					return nil
				}
			case "source":
				adapter.afterPublish = func() { adapter.sourceErr = errors.New("synthetic source changed") }
			case "publish":
				adapter.publishErr = errors.New("synthetic ambiguous route write")
			}
			result, err := lifecycle.Reconcile(context.Background(), ticket)
			if err == nil || !result.Hold || result.Verified || result.Observation != nil ||
				!slices.Contains(adapter.events, "withdraw") || !slices.Contains(adapter.events, "exclude-writers") {
				t.Fatalf("post-start uncertainty advertised success: %+v %v %v", result, adapter.events, err)
			}
		})
	}
}

func TestWithdrawalFailureStillAttemptsFenceButDoesNotRelease(t *testing.T) {
	lifecycle, ticket, adapter := lifecycleFixture(t)
	adapter.withdrawErr = errors.New("synthetic route withdrawal failed")
	if err := lifecycle.Stop(context.Background(), ticket); err == nil {
		t.Fatal("withdrawal failure ignored")
	}
	if !slices.Equal(adapter.events, []string{"clear-readiness", "withdraw", "exclude-writers"}) {
		t.Fatal("route failure prevented old-writer exclusion:", adapter.events)
	}
	current, err := lifecycle.Store.ReadExecution(context.Background(), ticket.Execution.Intent.Agent.UID)
	if err != nil || current.Execution.Phase != Stopping {
		t.Fatal("failed withdrawal released source reservations:", err)
	}
}

func TestLostStartReplyCannotAuthorizeAnotherAttempt(t *testing.T) {
	lifecycle, ticket, adapter := lifecycleFixture(t)
	adapter.startErr = errors.New("synthetic lost start reply")
	adapter.quiesceErr = errors.New("synthetic fence pending")
	if result, err := lifecycle.Reconcile(context.Background(), ticket); err == nil || !result.Hold || result.Verified {
		t.Fatal("uncertain start reported ready:", err)
	}
	current, err := lifecycle.Store.ReadExecution(context.Background(), ticket.Execution.Intent.Agent.UID)
	if err != nil || current.Execution.Phase != Stopping {
		t.Fatal("start uncertainty lost its cleanup obligation:", err)
	}
	if _, err := lifecycle.Reconcile(context.Background(), current); err == nil || adapter.starts != 1 {
		t.Fatal("restart resubmitted the start or ignored pending exclusion:", err)
	}
}

func TestReadinessCommitUncertaintyFailsClosed(t *testing.T) {
	for _, scenario := range []string{"lost reply", "source drift", "aged observation"} {
		t.Run(scenario, func(t *testing.T) {
			lifecycle, ticket, adapter := lifecycleFixture(t)
			switch scenario {
			case "lost reply":
				adapter.commitErr = errors.New("synthetic lost status commit reply")
			case "source drift":
				adapter.beforeCommit = func() { adapter.sourceErr = errors.New("synthetic changed source") }
			case "aged observation":
				adapter.beforeCommit = func() { adapter.now = adapter.now.Add(11 * time.Second) }
			}
			result, err := lifecycle.Reconcile(context.Background(), ticket)
			if err == nil || result.Verified || !result.Hold || adapter.ready ||
				!slices.Contains(adapter.events, "clear-readiness") ||
				!slices.Contains(adapter.events, "withdraw") || !slices.Contains(adapter.events, "exclude-writers") {
				t.Fatalf("uncertain status retained readiness: %+v %v %v", result, adapter.events, err)
			}
		})
	}
}

func TestReadinessClearFailureDoesNotSkipWriterExclusion(t *testing.T) {
	lifecycle, ticket, adapter := lifecycleFixture(t)
	adapter.clearErr = errors.New("synthetic status clear failure")
	if err := lifecycle.Stop(context.Background(), ticket); err == nil {
		t.Fatal("status failure was ignored")
	}
	if !slices.Equal(adapter.events, []string{"clear-readiness", "withdraw", "exclude-writers"}) {
		t.Fatal("status failure prevented writer exclusion:", adapter.events)
	}
	current, err := lifecycle.Store.ReadExecution(context.Background(), ticket.Execution.Intent.Agent.UID)
	if err != nil || current.Execution.Phase != Stopping {
		t.Fatal("failed status clearing released reservations:", err)
	}
}

func TestMissingIntegrationNeverEnablesExecution(t *testing.T) {
	for _, missing := range []string{"adapter", "writer", "clock", "freshness", "excessive freshness"} {
		t.Run(missing, func(t *testing.T) {
			lifecycle, ticket, adapter := lifecycleFixture(t)
			switch missing {
			case "adapter":
				lifecycle.Adapter = nil
			case "writer":
				lifecycle.Writers = nil
			case "clock":
				lifecycle.Now = nil
			case "freshness":
				lifecycle.MaxAge = 0
			case "excessive freshness":
				lifecycle.MaxAge = 2 * time.Minute
			}
			result, err := lifecycle.Reconcile(context.Background(), ticket)
			if err == nil || !result.Hold || result.Verified || len(adapter.events) != 0 {
				t.Fatal("missing integration enabled a side effect:", err)
			}
		})
	}
}

type syntheticChangeAdapter struct {
	submissions int
	submitErr   error
	submit      func(context.Context, ChangeTicket) error
	outcome     func(ChangeTicket) *ChangeOutcome
}

func (a *syntheticChangeAdapter) Submit(ctx context.Context, ticket ChangeTicket, guard func(context.Context) error) error {
	if err := guard(ctx); err != nil {
		return err
	}
	a.submissions++
	if a.submit != nil {
		return a.submit(ctx, ticket)
	}
	return a.submitErr
}

func (a *syntheticChangeAdapter) ObserveChange(_ context.Context, ticket ChangeTicket) (*ChangeOutcome, error) {
	return a.outcome(ticket), nil
}

func completeChange(ticket ChangeTicket) *ChangeOutcome {
	after := ticket.Change.Before
	after.ResourceVersion = "2"
	return &ChangeOutcome{AuthorityUID: string(ticket.AuthorityUID), OperationID: ticket.Change.ID,
		Before: ticket.Change.Before, After: after, DesiredDigest: ticket.Change.DesiredDigest,
		Applied: true, NoPendingSubmissions: true}
}

func TestSharedChangeRecoversOnlyByObservingTerminalOutcome(t *testing.T) {
	ctx := context.Background()
	store, _ := fixture(t)
	intent := testIntent("one")
	change, err := store.AcquireChange(ctx, intent.Sources[4], strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	adapter := &syntheticChangeAdapter{submitErr: errors.New("synthetic lost mutation reply"), outcome: completeChange}
	change, err = store.ReconcileChange(ctx, change, adapter)
	if err == nil || change.Change.Phase != Changing || adapter.submissions != 1 {
		t.Fatal("uncertain change did not retain submission intent:", err)
	}
	if _, err := store.AcquireExecution(ctx, intent); !errors.Is(err, ErrConflict) {
		t.Fatal("uncertain shared change allowed a start:", err)
	}
	recovered, err := store.AcquireChange(ctx, intent.Sources[4], strings.Repeat("b", 64))
	if err != nil || recovered != change {
		t.Fatal("restarted caller could not recover its exact change:", err)
	}
	if _, err := store.ReconcileChange(ctx, recovered, adapter); err != nil || adapter.submissions != 1 {
		t.Fatal("change recovery resubmitted or failed to observe:", err)
	}
	acquire(t, store, intent)
}

func TestSharedChangeCannotReleaseOnPointInTimeOrForeignOutcome(t *testing.T) {
	for name, mutate := range map[string]func(*ChangeOutcome){
		"in flight":    func(o *ChangeOutcome) { o.NoPendingSubmissions = false },
		"authority":    func(o *ChangeOutcome) { o.AuthorityUID = "foreign" },
		"operation":    func(o *ChangeOutcome) { o.OperationID = "foreign" },
		"old object":   func(o *ChangeOutcome) { o.After = o.Before },
		"replacement":  func(o *ChangeOutcome) { o.After.UID = "replacement" },
		"digest":       func(o *ChangeOutcome) { o.DesiredDigest = strings.Repeat("c", 64) },
		"ambiguous":    func(o *ChangeOutcome) { o.Aborted = true },
		"not terminal": func(o *ChangeOutcome) { o.Applied = false },
	} {
		t.Run(name, func(t *testing.T) {
			store, _ := fixture(t)
			change, err := store.AcquireChange(context.Background(), testIntent("one").Sources[4], strings.Repeat("b", 64))
			if err != nil {
				t.Fatal(err)
			}
			adapter := &syntheticChangeAdapter{outcome: func(ticket ChangeTicket) *ChangeOutcome {
				outcome := completeChange(ticket)
				mutate(outcome)
				return outcome
			}}
			if _, err := store.ReconcileChange(context.Background(), change, adapter); err == nil {
				t.Fatal("incomplete outcome released a shared change")
			}
			if _, err := store.AcquireExecution(context.Background(), testIntent("one")); !errors.Is(err, ErrConflict) {
				t.Fatal("unknown change stopped blocking execution:", err)
			}
		})
	}
}

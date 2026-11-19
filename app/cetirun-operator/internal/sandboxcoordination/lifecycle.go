// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxcoordination

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
)

// PreStartObservation acknowledges an armed guest-external gate, not execution.
// A runtime must check the same ticket at the actual start boundary and remain
// restrictive when the coordinator or current control observations are lost.
type PreStartObservation struct {
	AuthorityUID   string
	OperationID    string
	Expected       sandbox.ExecutionIdentity
	ObservedAt     time.Time
	ControlsReady  bool
	StartGateArmed bool
}

// Adapter is an independently qualified platform integration. None of these
// operations can treat a Helm release, a held expectation or Pod Ready as
// enforcement evidence. Side effects must enforce guard at their commit/start
// boundary; invoking it once before an asynchronous submission is insufficient.
// Route operations are idempotent and must also enforce the ticket's ingress
// mode, including verified route absence for disabled ingress.
type Adapter interface {
	CheckPrepared(context.Context, Ticket) error
	PrepareStart(context.Context, Ticket) (*PreStartObservation, error)
	Start(context.Context, Ticket, func(context.Context) error) error
	Observe(context.Context, Ticket) (*sandbox.Observation, error)
	PublishRoutes(context.Context, Ticket, func(context.Context) error) error
	WithdrawRoutes(context.Context, Ticket, func(context.Context) error) error
	// CommitReadiness atomically advances the applied binding and readiness
	// for the exact current Agent intent, with an API resource-version check.
	CommitReadiness(context.Context, Ticket, sandbox.Observation, func(context.Context) error) error
	ClearReadiness(context.Context, Ticket, func(context.Context) error) error
}

// Writers supplies write-ahead registration and verified durable exclusion of
// the full old-writer history, including storage release and unreachable nodes.
// Reserve is idempotent only for this exact ticket/epoch; it must never adopt a
// different active writer. Quiesce must reject a later epoch before side effects.
type Writers interface {
	Reserve(context.Context, Ticket, func(context.Context) error) error
	Quiesce(context.Context, Ticket, func(context.Context) error) error
}

type Lifecycle struct {
	Store   Store
	Adapter Adapter
	Writers Writers
	Now     func() time.Time
	MaxAge  time.Duration
}

type Result struct {
	Ticket      Ticket
	Hold        bool
	Verified    bool
	Observation *sandbox.Observation
}

// Reconcile does not install adapters or change the shipped activation hold.
// It is the common ordered path for an explicitly integrated platform. Source
// changes retain their exclusive tickets until their actual outcome is known.
func (l Lifecycle) Reconcile(ctx context.Context, ticket Ticket) (Result, error) {
	ticket = ticket.DeepCopy()
	result := Result{Ticket: ticket.DeepCopy(), Hold: true}
	if err := l.validate(ctx); err != nil {
		return result, err
	}
	if err := l.Store.Check(ctx, ticket); err != nil {
		return result, err
	}
	if ticket.Execution.Phase == Stopping {
		return result, l.stop(ctx, ticket)
	}
	if err := l.Adapter.CheckPrepared(ctx, ticket.DeepCopy()); err != nil {
		return result, l.fail(ctx, ticket, err)
	}
	if ticket.Execution.Phase == Reserved {
		guard := l.guard(ticket)
		if err := l.Writers.Reserve(ctx, ticket.DeepCopy(), guard); err != nil {
			return result, l.fail(ctx, ticket, err)
		}
		ack, err := l.Adapter.PrepareStart(ctx, ticket.DeepCopy())
		if err != nil {
			return result, l.fail(ctx, ticket, err)
		}
		if err := l.verifyPreStart(ticket, ack); err != nil {
			return result, l.fail(ctx, ticket, err)
		}
		ackCopy := *ack
		ack = &ackCopy
		if err := guard(ctx); err != nil {
			return result, l.fail(ctx, ticket, err)
		}
		// Recheck freshness after all source reads, not just when the adapter
		// returned. Intent persists before the only allowed start submission.
		if err := l.verifyPreStart(ticket, ack); err != nil {
			return result, l.fail(ctx, ticket, err)
		}
		ticket, err = l.Store.BeginStart(ctx, ticket)
		if err != nil {
			return result, err
		}
		result.Ticket = ticket.DeepCopy()
		startGuard := func(ctx context.Context) error {
			if err := l.guard(ticket)(ctx); err != nil {
				return err
			}
			return l.verifyPreStart(ticket, ack)
		}
		if err := startGuard(ctx); err != nil {
			return result, l.fail(ctx, ticket, err)
		}
		if err := l.Adapter.Start(ctx, ticket.DeepCopy(), startGuard); err != nil {
			return result, l.fail(ctx, ticket, err)
		}
	}
	observation, err := l.Adapter.Observe(ctx, ticket.DeepCopy())
	if err == nil {
		err = sandbox.VerifyObservation(ticket.Execution.Intent.Expected, observation, l.Now(), l.MaxAge)
	}
	if errors.Is(err, sandbox.ErrEnforcementPending) && ticket.Execution.Phase == Starting {
		// Initial pending enforcement never publishes a route or applied
		// revision. The armed adapter gate remains responsible for exclusion.
		return result, err
	}
	if err != nil {
		return result, l.fail(ctx, ticket, err)
	}
	if err := l.guard(ticket)(ctx); err != nil {
		return result, l.fail(ctx, ticket, err)
	}
	if err := sandbox.VerifyObservation(ticket.Execution.Intent.Expected, observation, l.Now(), l.MaxAge); err != nil {
		return result, l.fail(ctx, ticket, err)
	}
	publicationEvidence := *observation
	if ticket.Execution.Phase == Starting {
		ticket, err = l.Store.MarkRunning(ctx, ticket)
		if err != nil {
			return result, err
		}
		result.Ticket = ticket.DeepCopy()
	}
	if ticket.Execution.Phase != Running {
		return result, fmt.Errorf("%w: execution is not eligible for publication", ErrConflict)
	}
	publicationGuard := l.enforcementGuard(ticket, publicationEvidence)
	if err := publicationGuard(ctx); err != nil {
		return result, l.fail(ctx, ticket, err)
	}
	if err := l.Adapter.PublishRoutes(ctx, ticket.DeepCopy(), publicationGuard); err != nil {
		return result, l.fail(ctx, ticket, err)
	}
	// Re-observe after route publication. A successful API write is not the
	// post-start observation used for readiness or an applied binding.
	observation, err = l.Adapter.Observe(ctx, ticket.DeepCopy())
	if err == nil {
		err = l.guard(ticket)(ctx)
	}
	if err == nil {
		err = sandbox.VerifyObservation(ticket.Execution.Intent.Expected, observation, l.Now(), l.MaxAge)
	}
	if err != nil {
		return result, l.fail(ctx, ticket, err)
	}
	copy := *observation
	commitGuard := l.enforcementGuard(ticket, copy)
	if err := l.Adapter.CommitReadiness(ctx, ticket.DeepCopy(), copy, commitGuard); err != nil {
		return result, l.fail(ctx, ticket, err)
	}
	if err := commitGuard(ctx); err != nil {
		return result, l.fail(ctx, ticket, err)
	}
	result.Ticket, result.Observation = ticket.DeepCopy(), &copy
	result.Hold, result.Verified = false, true
	return result, nil
}

// Stop uses the durable ticket even when current Team/policy inputs are gone,
// suspended or rebound. The shared-source reservations survive every failure.
func (l Lifecycle) Stop(ctx context.Context, ticket Ticket) error {
	if err := l.validate(ctx); err != nil {
		return err
	}
	return l.stop(ctx, ticket.DeepCopy())
}

func (l Lifecycle) stop(ctx context.Context, ticket Ticket) error {
	stopping, err := l.Store.BeginStop(ctx, ticket)
	if err != nil {
		return err
	}
	guard := func(ctx context.Context) error { return l.Store.Check(ctx, stopping) }
	// Withdrawal failure must not prevent an attempted writer stop, but neither
	// failure permits releasing the shared-change reservation.
	statusErr := l.Adapter.ClearReadiness(ctx, stopping.DeepCopy(), guard)
	withdrawErr := l.Adapter.WithdrawRoutes(ctx, stopping.DeepCopy(), guard)
	excludeErr := l.Writers.Quiesce(ctx, stopping.DeepCopy(), guard)
	if err := errors.Join(statusErr, withdrawErr, excludeErr); err != nil {
		return err
	}
	if err := guard(ctx); err != nil {
		return err
	}
	return l.Store.ReleaseExecution(ctx, stopping)
}

func (l Lifecycle) fail(ctx context.Context, ticket Ticket, cause error) error {
	return errors.Join(cause, l.stop(ctx, ticket))
}

func (l Lifecycle) guard(ticket Ticket) func(context.Context) error {
	return func(ctx context.Context) error {
		if err := l.Store.Check(ctx, ticket); err != nil {
			return err
		}
		if err := l.Adapter.CheckPrepared(ctx, ticket.DeepCopy()); err != nil {
			return err
		}
		return l.Store.Check(ctx, ticket)
	}
}

func (l Lifecycle) enforcementGuard(ticket Ticket, observation sandbox.Observation) func(context.Context) error {
	return func(ctx context.Context) error {
		if err := l.guard(ticket)(ctx); err != nil {
			return err
		}
		return sandbox.VerifyObservation(ticket.Execution.Intent.Expected, &observation, l.Now(), l.MaxAge)
	}
}

func (l Lifecycle) verifyPreStart(ticket Ticket, observed *PreStartObservation) error {
	now := l.Now()
	if observed == nil || observed.AuthorityUID != string(ticket.AuthorityUID) ||
		observed.OperationID != ticket.Execution.ID || observed.Expected != ticket.Execution.Intent.Expected ||
		observed.ObservedAt.IsZero() || now.IsZero() || observed.ObservedAt.After(now) ||
		now.Sub(observed.ObservedAt) > l.MaxAge {
		return fmt.Errorf("%w: pre-start acknowledgement is missing, stale or mismatched", sandbox.ErrObservationUnavailable)
	}
	if !observed.ControlsReady || !observed.StartGateArmed {
		return fmt.Errorf("%w: pre-start controls and durable start gate are not ready", sandbox.ErrEnforcementPending)
	}
	return nil
}

func (l Lifecycle) validate(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if l.Adapter == nil || l.Writers == nil || l.Now == nil || l.MaxAge <= 0 || l.MaxAge > time.Minute || l.Now().IsZero() {
		return fmt.Errorf("%w: explicit writer, enforcement, routing and freshness integrations are required", ErrUnavailable)
	}
	return nil
}

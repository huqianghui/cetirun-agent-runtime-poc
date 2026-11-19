// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxcoordination

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxwriter"
)

// WriterSource resolves the independently enrolled physical machine and volume
// identities. A Node provider path, Pod status or PVC access mode is not enough.
// ResolveWriter is read-only and must not create or start the proposed writer.
type WriterSource interface {
	ResolveWriter(context.Context, Ticket) (sandboxwriter.Writer, error)
}

type JournalWriters struct {
	Manager *sandboxwriter.Manager
	Source  WriterSource
}

var _ Writers = (*JournalWriters)(nil)

func (w *JournalWriters) Reserve(ctx context.Context, ticket Ticket, guard func(context.Context) error) error {
	if err := w.validate(ctx, ticket, guard); err != nil {
		return err
	}
	if w.Source == nil {
		return fmt.Errorf("%w: independently enrolled writer identities are required", ErrUnavailable)
	}
	if ticket.Execution.Phase != Reserved {
		return ErrConflict
	}
	writer, err := w.Source.ResolveWriter(ctx, ticket.DeepCopy())
	if err != nil {
		return err
	}
	if !matchesWriter(ticket, writer) {
		return fmt.Errorf("%w: physical writer does not match the enrolled execution target", ErrConflict)
	}
	manager := *w.Manager
	history, err := manager.Store.Read(ctx, writerLifetime(ticket))
	if err != nil {
		return err
	}
	if err := checkHistoryPlacement(ticket, history); err != nil {
		return err
	}
	if history.Phase == sandboxwriter.PhaseReserved {
		if history.Writers[len(history.Writers)-1] != writer {
			return fmt.Errorf("%w: another writer is already reserved", ErrConflict)
		}
		if err := guard(ctx); err != nil {
			return err
		}
		return unchangedHistory(ctx, manager.Store, history)
	}
	if err := guard(ctx); err != nil {
		return err
	}
	quiescent, err := manager.Quiesce(ctx, history)
	if err != nil {
		return err
	}
	if err := guard(ctx); err != nil {
		return err
	}
	recorded, err := manager.Store.Reserve(ctx, quiescent, writer)
	if err != nil {
		return err
	}
	if err := guard(ctx); err != nil {
		return err
	}
	return unchangedHistory(ctx, manager.Store, recorded)
}

// Quiesce uses only recorded history. It never calls the current WriterSource
// during suspension, deletion, observation loss or configuration replacement.
func (w *JournalWriters) Quiesce(ctx context.Context, ticket Ticket, guard func(context.Context) error) error {
	if err := w.validate(ctx, ticket, guard); err != nil {
		return err
	}
	if ticket.Execution.Phase != Stopping {
		return ErrConflict
	}
	manager := *w.Manager
	history, err := manager.Store.Read(ctx, writerLifetime(ticket))
	if err != nil {
		return err
	}
	if err := checkHistoryPlacement(ticket, history); err != nil {
		return err
	}
	if (history.Phase == sandboxwriter.PhaseReserved || history.Phase == sandboxwriter.PhaseRevoking) &&
		len(history.Writers) > 0 && !matchesWriter(ticket, history.Writers[len(history.Writers)-1]) {
		return fmt.Errorf("%w: cleanup cannot revoke a different active writer epoch", ErrConflict)
	}
	if err := guard(ctx); err != nil {
		return err
	}
	quiescent, err := manager.Quiesce(ctx, history)
	if err != nil {
		return err
	}
	if err := guard(ctx); err != nil {
		return err
	}
	return unchangedHistory(ctx, manager.Store, quiescent)
}

func (w *JournalWriters) validate(ctx context.Context, ticket Ticket, guard func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if w == nil || w.Manager == nil || w.Manager.Provider == nil || w.Manager.Now == nil ||
		w.Manager.MaxAge <= 0 || w.Manager.MaxAge > sandboxwriter.MaxObservationAge ||
		!w.Manager.Now().After(time.Unix(0, 0)) || guard == nil {
		return fmt.Errorf("%w: writer history, qualified provider and coordination guard are required", ErrUnavailable)
	}
	if err := ticket.Execution.Intent.Validate(); err != nil {
		return err
	}
	return guard(ctx)
}

func writerLifetime(ticket Ticket) sandboxwriter.Lifetime {
	intent := ticket.Execution.Intent
	return sandboxwriter.Lifetime{Agent: intent.Agent, Namespace: intent.Namespace, Home: intent.Home}
}

func matchesWriter(ticket Ticket, writer sandboxwriter.Writer) bool {
	return writer == ticket.Execution.Intent.Writer
}

func checkHistoryPlacement(ticket Ticket, history sandboxwriter.Snapshot) error {
	intent := ticket.Execution.Intent
	if history.UID != intent.WriterJournal.UID ||
		sandboxwriter.JournalName(history.Lifetime) != intent.WriterJournal.Name {
		return fmt.Errorf("%w: enrolled writer journal identity changed", ErrConflict)
	}
	for _, writer := range history.Writers {
		if writer.Workload != intent.Workload || !slices.Contains(intent.Placement.Nodes, writer.Node) {
			return fmt.Errorf("%w: historical writer is outside lifetime placement", ErrConflict)
		}
	}
	return nil
}

func unchangedHistory(ctx context.Context, store sandboxwriter.Store, pinned sandboxwriter.Snapshot) error {
	current, err := store.Read(ctx, pinned.Lifetime)
	if err != nil {
		return err
	}
	if current.UID != pinned.UID || current.ResourceVersion != pinned.ResourceVersion {
		return fmt.Errorf("%w: writer history changed during coordination", ErrConflict)
	}
	return ctx.Err()
}

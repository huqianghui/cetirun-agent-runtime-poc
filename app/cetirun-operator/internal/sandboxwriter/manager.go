// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxwriter

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

type Manager struct {
	Store    Store
	Provider Provider
	Now      func() time.Time
	MaxAge   time.Duration
}

func (m *Manager) CheckQuiesced(ctx context.Context, lifetime Lifetime) error {
	if err := m.validate(ctx); err != nil {
		return err
	}
	snapshot, err := m.Store.Read(ctx, lifetime)
	if err != nil {
		return err
	}
	_, err = m.Quiesce(ctx, snapshot)
	return err
}

// Quiesce accepts a pinned snapshot so adapters can validate historical
// placement before any side effect. Changed history is a conflict, not a reason
// to retarget the request. A later successful check always reobserves evidence.
func (m *Manager) Quiesce(ctx context.Context, pinned Snapshot) (Snapshot, error) {
	if err := m.validate(ctx); err != nil {
		return Snapshot{}, err
	}
	manager := *m
	pinned = pinned.DeepCopy()
	if err := manager.Store.validate(ctx, pinned.Lifetime, true); err != nil {
		return Snapshot{}, err
	}
	object, err := manager.Store.pinned(ctx, pinned)
	if err != nil {
		return Snapshot{}, err
	}
	if pinned.Phase == PhaseEnrolled || pinned.Phase == PhaseReserved {
		var operation [16]byte
		if _, err := rand.Read(operation[:]); err != nil {
			return Snapshot{}, fmt.Errorf("create Sandbox revocation operation ID: %w", err)
		}
		next := pinned.DeepCopy()
		next.Phase, next.OperationID = PhaseRevoking, hex.EncodeToString(operation[:])
		pinned, err = manager.Store.update(ctx, object, pinned, next, true)
		if err != nil {
			return Snapshot{}, err
		}
		// Only this acknowledged and read-back transition submits. Neither
		// an intent found on restart nor an ambiguous write may submit again.
		if err := ctx.Err(); err != nil {
			return Snapshot{}, err
		}
		err = manager.Provider.Revoke(ctx, requestOf(pinned))
		if canceled := ctx.Err(); canceled != nil {
			return Snapshot{}, canceled
		}
		if err != nil {
			return Snapshot{}, fmt.Errorf("submit Sandbox writer revocation; recovery is observe-only: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	request := requestOf(pinned)
	observation, err := manager.Provider.Observe(ctx, request.DeepCopy())
	if canceled := ctx.Err(); canceled != nil {
		return Snapshot{}, canceled
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("observe Sandbox writer revocation: %w", err)
	}
	if err := validateObservation(request, observation, manager.Now(), manager.MaxAge); err != nil {
		return Snapshot{}, err
	}
	// Recheck after Observe, including already-quiescent checks: a reservation
	// concurrent with observation invalidates this result even if proof is valid.
	object, err = manager.Store.pinned(ctx, pinned)
	if err != nil {
		return Snapshot{}, err
	}
	if err := validateObservationTime(observation.ObservedAt, manager.Now(), manager.MaxAge); err != nil {
		return Snapshot{}, err
	}
	if pinned.Phase != PhaseQuiescent {
		next := pinned.DeepCopy()
		next.Phase = PhaseQuiescent
		pinned, err = manager.Store.update(ctx, object, pinned, next, true)
		if err != nil {
			return Snapshot{}, err
		}
	}
	if err := validateObservationTime(observation.ObservedAt, manager.Now(), manager.MaxAge); err != nil {
		return Snapshot{}, err
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	return pinned, nil
}

func (m *Manager) validate(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m == nil || m.Provider == nil || m.Now == nil || m.MaxAge <= 0 || m.MaxAge > MaxObservationAge {
		return fmt.Errorf("%w: a qualified provider, explicit clock and positive bounded MaxAge are required", ErrUnavailable)
	}
	if !m.Now().After(time.Unix(0, 0)) {
		return fmt.Errorf("%w: a positive observation clock is required", ErrUnavailable)
	}
	return nil
}

func requestOf(snapshot Snapshot) Request {
	return Request{
		JournalUID: snapshot.UID, OperationID: snapshot.OperationID,
		EnrollmentID: snapshot.EnrollmentID, Lifetime: snapshot.Lifetime,
		Writers: snapshot.DeepCopy().Writers,
	}
}

// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package configurationcoordination exposes the supported client integration
// for changing configuration resources protected by Sandbox coordination.
package configurationcoordination

import (
	"context"
	"errors"
	"fmt"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// IsConflict reports a competing or mismatched coordination operation.
func IsConflict(err error) bool {
	return errors.Is(err, sandboxcoordination.ErrConflict)
}

// IsUnavailable reports an unavailable authority or an uncertain mutation
// outcome whose reservation must be retained for recovery.
func IsUnavailable(err error) bool {
	return errors.Is(err, sandboxcoordination.ErrUnavailable)
}

// Authority pins the coordination ConfigMap used by admission and all
// participating configuration writers.
type Authority struct {
	Namespace string
	Name      string
	UID       types.UID
}

// Client submits configuration updates and deletions through the existing
// durable reservation protocol. It does not create or discover an authority.
type Client struct {
	writer client.Client
	reader client.Reader
	store  sandboxcoordination.Store
}

// New constructs a coordinated configuration writer. reader must be an
// authoritative, uncached API reader; a controller-runtime direct client can
// be supplied as both writer and reader.
func New(writer client.Client, reader client.Reader, authority Authority) (*Client, error) {
	if writer == nil || reader == nil || len(validation.IsDNS1123Label(authority.Namespace)) != 0 ||
		len(validation.IsDNS1123Subdomain(authority.Name)) != 0 || !boundedIdentity(string(authority.UID), 128) {
		return nil, fmt.Errorf("configuration coordination requires a writer, authoritative reader, namespace, name, and exact authority UID")
	}
	return &Client{
		writer: writer,
		reader: reader,
		store: sandboxcoordination.Store{
			Client: writer, APIReader: reader, Namespace: authority.Namespace,
			Identity: w.ObjectIdentity{Name: authority.Name, UID: authority.UID},
		},
	}, nil
}

// Update reserves and submits one exact resource-version-conditional Update,
// then releases the reservation only after a definitive applied or superseded
// outcome. A superseded operation returns a conflict rather than claiming the
// desired content committed; an uncertain outcome retains its reservation.
func (c *Client) Update(ctx context.Context, before, after client.Object) error {
	adapter, err := sandboxcoordination.NewKubernetesChangeAdapter(c.writer, c.reader, before, after)
	if err != nil {
		return err
	}
	ticket, err := c.store.AcquireConfigurationChange(ctx, before, after)
	if err != nil {
		return err
	}
	observed := &observedChangeAdapter{ChangeAdapter: adapter}
	if err := c.reconcileChange(ctx, ticket, observed); err != nil {
		return err
	}
	if observed.outcome != nil && observed.outcome.Superseded {
		return fmt.Errorf("%w: configuration update was superseded before it committed", sandboxcoordination.ErrConflict)
	}
	return nil
}

// RecoverUpdate permits an exact Reserved retry to proceed through Update. A
// status-only resource-version change may safely release Reserved or Changing
// work only when an authoritative read proves the exact recorded before content
// on the same UID. Desired content at a superseding version proves the Changing
// update committed. No other outcome is released, submitted, or rebased.
func (c *Client) RecoverUpdate(ctx context.Context, before, after client.Object) error {
	ticket, found, err := c.store.ReadConfigurationUpdate(ctx, before)
	if err != nil || !found {
		return err
	}
	if ticket.Change.Phase == sandboxcoordination.Reserved {
		// Only the original request may resume an unsubmitted operation. This
		// lookup neither acquires nor advances a reservation; the caller must
		// still validate its version preconditions before invoking Update.
		if ticket.Change.Before.UID == before.GetUID() && ticket.Change.Before.ResourceVersion == before.GetResourceVersion() &&
			ticket.Change.Before.Name == before.GetName() && ticket.Change.Before.Namespace == before.GetNamespace() {
			_, err := c.store.ReadConfigurationChange(ctx, before, after)
			return err
		}
		// A status writer can advance only the resourceVersion after Reserve and
		// before BeginChange. Observe the exact before content first, then use
		// the journal's CAS transition to exclude a concurrent original submit.
		adapter, err := sandboxcoordination.NewKubernetesUpdateRecoveryAdapter(c.reader, ticket, before)
		if err != nil {
			return err
		}
		outcome, err := adapter.ObserveChange(ctx, ticket)
		if err != nil || !outcome.Superseded {
			if err != nil {
				return err
			}
			return fmt.Errorf("%w: reserved configuration update did not remain at its exact before content", sandboxcoordination.ErrConflict)
		}
		ticket, err = c.store.BeginChange(ctx, ticket)
		if err != nil {
			return err
		}
		adapter, err = sandboxcoordination.NewKubernetesUpdateRecoveryAdapter(c.reader, ticket, before)
		if err != nil {
			return err
		}
		return c.reconcileChange(ctx, ticket, adapter)
	}
	adapter, err := sandboxcoordination.NewKubernetesUpdateRecoveryAdapter(c.reader, ticket, before)
	if err != nil {
		return err
	}
	return c.reconcileChange(ctx, ticket, adapter)
}

// Delete reserves and submits one exact UID/resourceVersion-conditional
// background Delete. An uncertain outcome deliberately retains the reservation.
func (c *Client) Delete(ctx context.Context, before client.Object) error {
	completed, err := c.store.RecoverConfigurationDeletion(ctx, before)
	if err != nil || completed {
		return err
	}
	adapter, err := sandboxcoordination.NewKubernetesDeleteAdapter(c.writer, c.reader, before)
	if err != nil {
		return err
	}
	ticket, err := c.store.AcquireConfigurationDeletion(ctx, before)
	if err != nil {
		return err
	}
	return c.reconcileChange(ctx, ticket, adapter)
}

func (coordinator *Client) reconcileChange(ctx context.Context, ticket sandboxcoordination.ChangeTicket, adapter sandboxcoordination.ChangeAdapter) error {
	ticket, err := coordinator.store.ReconcileChange(ctx, ticket, adapter)
	if err != nil && ticket.Change.Phase == sandboxcoordination.Changing {
		return fmt.Errorf("%w: recorded configuration operation requires recovery: %w", sandboxcoordination.ErrUnavailable, err)
	}
	return err
}

type observedChangeAdapter struct {
	sandboxcoordination.ChangeAdapter
	outcome *sandboxcoordination.ChangeOutcome
}

func (a *observedChangeAdapter) ObserveChange(ctx context.Context, ticket sandboxcoordination.ChangeTicket) (*sandboxcoordination.ChangeOutcome, error) {
	outcome, err := a.ChangeAdapter.ObserveChange(ctx, ticket)
	if err == nil {
		a.outcome = outcome
	}
	return outcome, err
}

func boundedIdentity(value string, limit int) bool {
	if len(value) == 0 || len(value) > limit {
		return false
	}
	for _, c := range value {
		if c < '!' || c > '~' {
			return false
		}
	}
	return true
}

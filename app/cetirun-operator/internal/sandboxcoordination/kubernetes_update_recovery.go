// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxcoordination

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/equality"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ReadConfigurationUpdate finds an outstanding update of this exact source UID.
// It needs no original request images and never acquires a new reservation.
func (s Store) ReadConfigurationUpdate(ctx context.Context, object client.Object) (ChangeTicket, bool, error) {
	image, err := newConfigurationImage(object)
	if err != nil {
		return ChangeTicket{}, false, err
	}
	return s.matchConfigurationTicket(ctx, image.reference, func(change Change) bool {
		return change.Before.UID == image.reference.UID &&
			(change.Phase == Reserved || change.Phase == Changing) && change.DeletionContentDigest == "" &&
			change.BeforeContentDigest != ""
	})
}

// KubernetesUpdateRecoveryAdapter observes a recorded update without retaining
// or replaying its original request. The journal pins complete writable before
// and desired content, including metadata, for all configuration kinds.
type KubernetesUpdateRecoveryAdapter struct {
	reader client.Reader
	ticket ChangeTicket
	source *configurationChange
}

var _ ChangeAdapter = (*KubernetesUpdateRecoveryAdapter)(nil)

func NewKubernetesUpdateRecoveryAdapter(reader client.Reader, ticket ChangeTicket, object client.Object) (*KubernetesUpdateRecoveryAdapter, error) {
	image, err := newConfigurationImage(object)
	if err != nil {
		return nil, err
	}
	if reader == nil || !literal(string(ticket.AuthorityUID), 128) || !literal(ticket.Change.ID, 128) ||
		(ticket.Change.Phase != Reserved && ticket.Change.Phase != Changing) || ticket.Change.DeletionContentDigest != "" ||
		!validSource(ticket.Change.Before) || !digest(ticket.Change.DesiredDigest) || !digest(ticket.Change.BeforeContentDigest) ||
		ticket.Change.BeforeContentDigest == ticket.Change.DesiredDigest ||
		sourceKey(image.reference) != sourceKey(ticket.Change.Before) || image.reference.UID != ticket.Change.Before.UID {
		return nil, fmt.Errorf("%w: an authoritative reader and matching concrete Reserved or Changing update are required", ErrConflict)
	}
	return &KubernetesUpdateRecoveryAdapter{reader: reader, ticket: ticket.DeepCopy(), source: &configurationChange{before: image}}, nil
}

func (a *KubernetesUpdateRecoveryAdapter) Submit(context.Context, ChangeTicket, func(context.Context) error) error {
	return fmt.Errorf("%w: update recovery is observation-only", ErrConflict)
}

func (a *KubernetesUpdateRecoveryAdapter) ObserveChange(ctx context.Context, ticket ChangeTicket) (*ChangeOutcome, error) {
	if a == nil || a.reader == nil || a.source == nil {
		return nil, fmt.Errorf("%w: a configured update observer is required", ErrUnavailable)
	}
	ticket = ticket.DeepCopy()
	if !equality.Semantic.DeepEqual(a.ticket, ticket) {
		return nil, ErrConflict
	}
	current, err := a.source.readCurrent(ctx, a.reader)
	if err != nil {
		return nil, err
	}
	before := ticket.Change.Before
	if current.reference.UID != before.UID || current.reference.ResourceVersion == before.ResourceVersion ||
		!current.object.GetDeletionTimestamp().IsZero() ||
		current.object.GetDeletionGracePeriodSeconds() != nil {
		return nil, fmt.Errorf("%w: recorded update is not observed at a superseding version of the same UID", ErrUnavailable)
	}
	// The authoritative read proves the old resourceVersion cannot commit.
	// It may release the operation only for the exact desired content, or for
	// the exact before content after a non-configuration status refresh.
	outcome := &ChangeOutcome{
		AuthorityUID: string(ticket.AuthorityUID), OperationID: ticket.Change.ID,
		Before: before, After: current.reference, DesiredDigest: ticket.Change.DesiredDigest,
		NoPendingSubmissions: true,
	}
	switch current.digest {
	case ticket.Change.DesiredDigest:
		outcome.Applied = true
	case ticket.Change.BeforeContentDigest:
		outcome.Aborted, outcome.Superseded = true, true
	default:
		return nil, fmt.Errorf("%w: recorded update is not observed at an exact desired or before content version", ErrUnavailable)
	}
	return outcome, ctx.Err()
}

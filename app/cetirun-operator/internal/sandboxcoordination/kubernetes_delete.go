// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxcoordination

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync/atomic"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type configurationDeletion struct {
	before configurationImage
	digest string
}

// AcquireConfigurationDeletion reserves the whole source, including every Team
// class. Recovery uses the original before image even after its UID is gone.
func (s Store) AcquireConfigurationDeletion(ctx context.Context, before client.Object) (ChangeTicket, error) {
	deletion, err := newConfigurationDeletion(before)
	if err != nil {
		return ChangeTicket{}, err
	}
	ticket, found, err := s.matchConfigurationTicket(ctx, deletion.before.reference, deletion.matches)
	if err != nil || found {
		return ticket, err
	}
	if err := deletion.source().checkBefore(ctx, s.APIReader); err != nil {
		return ChangeTicket{}, err
	}
	return s.acquireChange(ctx, deletion.before.reference, deletion.digest, "", nil, "", deletion.before.digest)
}

// ReadConfigurationDeletion only finds the exact recorded operation.
func (s Store) ReadConfigurationDeletion(ctx context.Context, before client.Object) (ChangeTicket, error) {
	deletion, err := newConfigurationDeletion(before)
	if err != nil {
		return ChangeTicket{}, err
	}
	ticket, found, err := s.matchConfigurationTicket(ctx, deletion.before.reference, deletion.matches)
	if err != nil {
		return ChangeTicket{}, err
	}
	if !found {
		return ChangeTicket{}, fmt.Errorf("%w: configuration deletion is not recorded", ErrUnavailable)
	}
	return ticket, nil
}

func (store Store) ReadConfigurationDeletionOperation(ctx context.Context, operationID string) (ChangeTicket, error) {
	if !literal(operationID, 128) {
		return ChangeTicket{}, fmt.Errorf("%w: an exact deletion operation ID is required", ErrUnavailable)
	}
	object, state, err := store.read(ctx)
	if err != nil {
		return ChangeTicket{}, err
	}
	for _, change := range state.Changes {
		if change.ID != operationID {
			continue
		}
		if change.Phase != Changing || change.DeletionContentDigest == "" {
			return ChangeTicket{}, fmt.Errorf("%w: operation-only recovery requires a recorded Changing deletion", ErrConflict)
		}
		if err := validateConfigurationDeletion(change); err != nil {
			return ChangeTicket{}, err
		}
		return ChangeTicket{object.UID, cloneChange(change)}, nil
	}
	return ChangeTicket{}, fmt.Errorf("%w: configuration deletion operation is not recorded", ErrUnavailable)
}

// RecoverConfigurationDeletion observes an earlier deletion of this source.
// It never submits a Delete. A superseded precondition permits a new request;
// an unchanged unsubmitted reservation remains available to its exact caller.
func (s Store) RecoverConfigurationDeletion(ctx context.Context, current client.Object) (bool, error) {
	image, err := newConfigurationImage(current)
	if err != nil {
		return false, err
	}
	ticket, found, err := s.matchConfigurationTicket(ctx, image.reference, func(change Change) bool {
		return change.DeletionContentDigest != ""
	})
	if err != nil || !found {
		return false, err
	}
	if err := s.CheckChange(ctx, ticket); err != nil {
		return false, err
	}
	if ticket.Change.Phase == Reserved && ticket.Change.Before == image.reference {
		if err := (&configurationChange{before: image}).checkBefore(ctx, s.APIReader); err == nil {
			return false, nil
		}
	}
	adapter, err := NewKubernetesDeletionRecoveryAdapter(s.APIReader, ticket)
	if err != nil {
		return false, err
	}
	if ticket.Change.Phase == Reserved {
		if _, err := adapter.ObserveChange(ctx, ticket); err != nil {
			return false, err
		}
		// Advance only after metadata proves the old conditional write cannot
		// affect a live object. Competing submission intent remains CAS-fenced.
		ticket, err = s.BeginChange(ctx, ticket)
		if err != nil {
			return false, err
		}
	}
	outcome, err := s.observeAndReleaseChange(ctx, ticket, adapter)
	if err != nil {
		return false, fmt.Errorf("%w: recorded configuration deletion requires recovery: %w", ErrUnavailable, err)
	}
	return outcome.Applied && ticket.Change.Before.UID == image.reference.UID, nil
}

// AuthorizeConfigurationDeletion is read-only and supplements ordinary deletion
// authorization. The API must enforce these exact UID/resourceVersion conditions.
func (s Store) AuthorizeConfigurationDeletion(ctx context.Context, before client.Object, options metav1.DeleteOptions) error {
	deletion, err := newConfigurationDeletion(before)
	if err != nil {
		return err
	}
	p := options.Preconditions
	if p == nil || p.UID == nil || p.ResourceVersion == nil ||
		*p.UID != deletion.before.reference.UID || *p.ResourceVersion != deletion.before.reference.ResourceVersion ||
		options.PropagationPolicy == nil || *options.PropagationPolicy != metav1.DeletePropagationBackground ||
		options.OrphanDependents != nil || (options.GracePeriodSeconds != nil && *options.GracePeriodSeconds != 0) ||
		ptr.Deref(options.IgnoreStoreReadErrorWithClusterBreakingPotential, false) ||
		(len(options.DryRun) != 0 && (len(options.DryRun) != 1 || options.DryRun[0] != metav1.DryRunAll)) {
		return fmt.Errorf("%w: configuration deletion requires exact preconditions and background propagation", ErrConflict)
	}
	ticket, found, err := s.matchConfigurationTicket(ctx, deletion.before.reference, deletion.matches)
	if err != nil {
		return err
	}
	if !found || ticket.Change.Phase != Changing {
		return fmt.Errorf("%w: the exact changing deletion ticket is required", ErrConflict)
	}
	if err := deletion.source().checkBefore(ctx, s.APIReader); err != nil {
		return err
	}
	return s.CheckChange(ctx, ticket)
}

// KubernetesDeleteAdapter submits a single background Delete. Clients must use
// authoritative reads and must not retry or weaken the Delete preconditions.
type KubernetesDeleteAdapter struct {
	writer    client.Client
	reader    client.Reader
	deletion  *configurationDeletion
	attempted atomic.Bool
}

var _ ChangeAdapter = (*KubernetesDeleteAdapter)(nil)

func NewKubernetesDeleteAdapter(writer client.Client, reader client.Reader, before client.Object) (*KubernetesDeleteAdapter, error) {
	deletion, err := newConfigurationDeletion(before)
	if err != nil {
		return nil, err
	}
	if reader == nil {
		return nil, fmt.Errorf("%w: an authoritative configuration reader is required", ErrUnavailable)
	}
	return &KubernetesDeleteAdapter{writer: writer, reader: reader, deletion: deletion}, nil
}

func NewKubernetesDeletionRecoveryAdapter(reader client.Reader, ticket ChangeTicket) (*KubernetesDeleteAdapter, error) {
	if reader == nil || !literal(string(ticket.AuthorityUID), 128) || !literal(ticket.Change.ID, 128) ||
		(ticket.Change.Phase != Changing && ticket.Change.Phase != Reserved) {
		return nil, fmt.Errorf("%w: an authoritative reader and recorded deletion ticket are required", ErrUnavailable)
	}
	if err := validateConfigurationDeletion(ticket.Change); err != nil {
		return nil, err
	}
	return &KubernetesDeleteAdapter{
		reader: reader,
		deletion: &configurationDeletion{
			before: configurationImage{reference: ticket.Change.Before, digest: ticket.Change.DeletionContentDigest},
			digest: ticket.Change.DesiredDigest,
		},
	}, nil
}

func (a *KubernetesDeleteAdapter) Submit(ctx context.Context, ticket ChangeTicket, guard func(context.Context) error) error {
	ticket = ticket.DeepCopy()
	if err := a.validate(ctx, ticket); err != nil {
		return err
	}
	if a.writer == nil || guard == nil {
		return fmt.Errorf("%w: configuration writer and coordination guard are required", ErrUnavailable)
	}
	if !a.attempted.CompareAndSwap(false, true) {
		return fmt.Errorf("%w: configuration deletion cannot be retried; observe with the same input", ErrConflict)
	}
	if err := guard(ctx); err != nil {
		return err
	}
	if err := a.deletion.source().checkBefore(ctx, a.reader); err != nil {
		return err
	}
	if err := guard(ctx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	before := a.deletion.before
	if err := a.writer.Delete(ctx, before.object.DeepCopyObject().(client.Object),
		client.Preconditions{UID: ptr.To(before.reference.UID), ResourceVersion: ptr.To(before.reference.ResourceVersion)},
		client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
		return fmt.Errorf("%w: configuration Delete outcome is uncertain; observe with the same input: %w", ErrUnavailable, err)
	}
	return nil
}

// ObserveChange confirms deletion acceptance or an irreversibly superseded
// resourceVersion precondition. It never removes finalizers or resubmits.
func (a *KubernetesDeleteAdapter) ObserveChange(ctx context.Context, ticket ChangeTicket) (*ChangeOutcome, error) {
	ticket = ticket.DeepCopy()
	if err := a.validate(ctx, ticket); err != nil {
		return nil, err
	}
	before := a.deletion.before.reference
	// Deletion evidence needs only identity and termination metadata. A
	// replacement's size or schema must not prevent observing the old UID gone.
	metadata := &metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: before.APIVersion, Kind: before.Kind}}
	err := a.reader.Get(ctx, client.ObjectKey{Namespace: before.Namespace, Name: before.Name}, metadata)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("%w: configuration deletion observation failed: %w", ErrUnavailable, err)
	}
	current := w.SourceReference{APIVersion: before.APIVersion, Kind: before.Kind,
		Namespace: metadata.Namespace, Name: metadata.Name, UID: metadata.UID, ResourceVersion: metadata.ResourceVersion}
	if err == nil && (!validSource(current) || sourceKey(current) != sourceKey(before)) {
		return nil, fmt.Errorf("%w: observed configuration identity is incomplete or mismatched", ErrUnavailable)
	}
	outcome := &ChangeOutcome{
		AuthorityUID: string(ticket.AuthorityUID), OperationID: ticket.Change.ID,
		Before: before, DesiredDigest: a.deletion.digest, Applied: true, NoPendingSubmissions: true,
	}
	if apierrors.IsNotFound(err) || current.UID != before.UID {
		// The old UID is absent. A delayed conditional Delete cannot affect a
		// replacement; its contents are neither adopted nor returned as After.
		outcome.Removed = true
	} else if !metadata.DeletionTimestamp.IsZero() && current.ResourceVersion != before.ResourceVersion {
		// Release the change reservation so ordinary finalizer cleanup can
		// finish. This is deletion acceptance, not proof of physical cleanup.
		outcome.After = current
	} else if metadata.DeletionTimestamp.IsZero() && current.ResourceVersion != before.ResourceVersion {
		outcome.Applied = false
		outcome.Aborted = true
		outcome.Superseded = true
		outcome.After = current
	} else {
		return nil, fmt.Errorf("%w: configuration deletion outcome remains unknown", ErrUnavailable)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return outcome, nil
}

func (a *KubernetesDeleteAdapter) validate(ctx context.Context, ticket ChangeTicket) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if a == nil || a.reader == nil || a.deletion == nil {
		return fmt.Errorf("%w: a configured Kubernetes deletion adapter is required", ErrUnavailable)
	}
	if !literal(string(ticket.AuthorityUID), 128) || !literal(ticket.Change.ID, 128) ||
		(ticket.Change.Phase != Changing && (ticket.Change.Phase != Reserved || a.writer != nil)) ||
		!a.deletion.matches(ticket.Change) {
		return ErrConflict
	}
	return nil
}

func newConfigurationDeletion(before client.Object) (*configurationDeletion, error) {
	image, err := newConfigurationImage(before)
	if err != nil {
		return nil, err
	}
	if !image.object.GetDeletionTimestamp().IsZero() || image.object.GetDeletionGracePeriodSeconds() != nil {
		return nil, fmt.Errorf("%w: deletion requires the original nonterminating configuration", ErrConflict)
	}
	operationDigest, err := configurationDeletionDigest(image.reference, image.digest)
	if err != nil {
		return nil, err
	}
	return &configurationDeletion{before: image, digest: operationDigest}, nil
}

func configurationDeletionDigest(before w.SourceReference, contentDigest string) (string, error) {
	raw, err := json.Marshal(struct {
		Operation string
		Before    w.SourceReference
		Content   string
	}{"configuration-background-delete-v1", before, contentDigest})
	if err != nil {
		return "", fmt.Errorf("%w: configuration deletion encoding failed", ErrUnavailable)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func validateConfigurationDeletion(change Change) error {
	before := change.Before
	if !validSource(before) || before.APIVersion != api.GroupVersion.String() || before.Namespace != "" ||
		(before.Kind != "Team" && before.Kind != "Sandbox" && before.Kind != "AgentPolicy") ||
		!digest(change.DeletionContentDigest) || change.BeforeContentDigest != "" || change.Team != nil || change.UnchangedDigest != "" {
		return fmt.Errorf("%w: invalid configuration deletion recovery metadata", ErrUnavailable)
	}
	expected, err := configurationDeletionDigest(before, change.DeletionContentDigest)
	if err != nil {
		return err
	}
	if change.DesiredDigest != expected {
		return fmt.Errorf("%w: configuration deletion recovery digest does not match its operation", ErrUnavailable)
	}
	return nil
}

func (d *configurationDeletion) source() *configurationChange {
	return &configurationChange{before: d.before}
}

func (d *configurationDeletion) matches(recorded Change) bool {
	return recorded.Before == d.before.reference && recorded.DesiredDigest == d.digest &&
		recorded.BeforeContentDigest == "" && recorded.Team == nil && recorded.UnchangedDigest == "" &&
		(recorded.DeletionContentDigest == "" || recorded.DeletionContentDigest == d.before.digest)
}

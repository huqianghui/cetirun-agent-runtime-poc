// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxpolicy

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/types"
)

// VerifyKubernetesBindings reads the existing journal and exact live grants.
// Unconfirmed write intents cannot authorize Pods, and obsolete Kubernetes
// grants must be revoked before a replacement set can authorize execution.
// The separate admission source-read RoleBinding is not a workload grant.
func (s GrantJournalStore) VerifyKubernetesBindings(ctx context.Context, agentName string, agentUID types.UID, desired []BindingReference) error {
	return s.verifyKubernetesBindings(ctx, agentName, agentUID, desired, true)
}

// VerifyKubernetesBindingIntents permits only the metadata-only release of a
// common reconciliation hold. Unconfirmed, not-yet-created bindings may be
// applied afterward, but still cannot authorize Pod admission.
func (s GrantJournalStore) VerifyKubernetesBindingIntents(ctx context.Context, agentName string, agentUID types.UID, desired []BindingReference) error {
	return s.verifyKubernetesBindings(ctx, agentName, agentUID, desired, false)
}

func (s GrantJournalStore) verifyKubernetesBindings(ctx context.Context, agentName string, agentUID types.UID, desired []BindingReference, confirmed bool) error {
	if err := s.validate(agentName, agentUID, false); err != nil {
		return err
	}
	if err := validateJournalReferences(agentName, agentUID, desired); err != nil {
		return err
	}
	wanted := make(map[journalBindingKey]BindingReference, len(desired))
	for _, ref := range desired {
		if ref.RoleRef.Kind != "ClusterRole" {
			return fmt.Errorf("%w: Kubernetes access requires exact ClusterRole bindings", ErrOwnership)
		}
		wanted[journalKey(ref)] = ref
	}
	cm, recorded, err := s.read(ctx, agentName, agentUID, journalRequireProtection)
	if err != nil {
		return err
	}
	if !cm.DeletionTimestamp.IsZero() {
		return fmt.Errorf("%w: the grant journal is terminating", ErrUnavailable)
	}
	owned, err := ownedBindingInventory(ctx, s.APIReader, s.PlatformAccessVerifier, agentName, agentUID, true)
	if err != nil {
		return err
	}
	inventory := make(map[journalBindingKey]BindingReference, len(recorded))
	for _, ref := range recorded {
		inventory[journalKey(ref)] = ref
	}
	for _, ref := range owned {
		if ref.RoleRef.Kind != "ClusterRole" {
			continue
		}
		expected, wantedGrant := wanted[journalKey(ref)]
		record, recordedGrant := inventory[journalKey(ref)]
		if !wantedGrant || !recordedGrant || journalIdentity(ref) != journalIdentity(expected) ||
			journalIdentity(ref) != journalIdentity(record) || (record.UID != "" && ref.UID != record.UID) {
			return fmt.Errorf("%w: an owned Kubernetes binding is outside current grant authority", ErrOwnership)
		}
	}
	for _, ref := range recorded {
		if ref.RoleRef.Kind != "ClusterRole" {
			if ref.ServiceAccountNamespace != s.Namespace {
				return fmt.Errorf("%w: a workload cannot acquire an unapproved Role grant", ErrOwnership)
			}
			continue
		}
		expected, found := wanted[journalKey(ref)]
		if !found || journalIdentity(ref) != journalIdentity(expected) || (confirmed && ref.UID == "") ||
			(expected.UID != "" && expected.UID != ref.UID) {
			return fmt.Errorf("%w: Kubernetes grants differ from confirmed execution intent", ErrOwnership)
		}
		object, err := readOwnedBinding(ctx, s.APIReader, ref)
		if err != nil {
			return err
		}
		if (object == nil && (confirmed || ref.UID != "")) || (object != nil && !object.GetDeletionTimestamp().IsZero()) {
			return fmt.Errorf("%w: a confirmed Kubernetes binding is missing or terminating", ErrUnavailable)
		}
		delete(wanted, journalKey(ref))
	}
	if len(wanted) != 0 {
		return fmt.Errorf("%w: Kubernetes bindings are missing from the grant journal", ErrOwnership)
	}
	current, _, err := s.read(ctx, agentName, agentUID, journalRequireProtection)
	if err != nil {
		return err
	}
	if current.UID != cm.UID || current.ResourceVersion != cm.ResourceVersion || !current.DeletionTimestamp.IsZero() {
		return fmt.Errorf("%w: Kubernetes grant inventory changed during observation", ErrConflict)
	}
	return nil
}

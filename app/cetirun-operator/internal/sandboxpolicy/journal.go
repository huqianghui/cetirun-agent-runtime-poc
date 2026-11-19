// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxpolicy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	pathvalidation "k8s.io/apimachinery/pkg/api/validation/path"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	strictjson "sigs.k8s.io/json"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
)

const (
	GrantJournalFinalizer = "sandbox.cetirun.microsoft.com/grant-journal"
	GrantJournalDataKey   = "grants.json"

	grantJournalReleasedAnnotation = "sandbox.cetirun.microsoft.com/grant-journal-released"

	// Limits include obsolete entries and the entire serialized ConfigMap,
	// including foreign metadata. Excess is rejected, never truncated. UIDs
	// must fit a label value; role names must be valid UTF-8 path segments and
	// grant revisions are the compiler's lowercase SHA-256 digests.
	MaxGrantJournalReferences = 1024
	MaxGrantJournalBytes      = 512 * 1024
)

// GrantJournalStore persists only typed binding identities, not approval
// sources, credentials or tokens. Namespace must be an explicit controller
// state namespace. APIReader must be authoritative; Client may have a cache.
// RoleBinding Roles are scoped to the binding namespace; ServiceAccount
// subjects may be in a different namespace, including the operator namespace.
// Call Record successfully before any grant write. Serialize grant application
// and cleanup for an Agent; this journal is not a lock around external writes.
// Conflicts are returned without retrying against a newer inventory.
type GrantJournalStore struct {
	Client    client.Client
	APIReader client.Reader
	Namespace string
	// PlatformAccessVerifier confirms separately managed platform bindings.
	// Callers bind it to trusted deployment configuration. Nil never exempts
	// a binding from the Sandbox grant contract.
	PlatformAccessVerifier PlatformAccessVerifier
}

// PlatformAccessVerifier keeps platform-specific ownership checks outside the
// policy package. Errors reject the inventory; only a confirmed match excludes
// a binding from Sandbox grant management.
type PlatformAccessVerifier func(context.Context, client.Reader, client.Object, string, types.UID) (bool, error)

// GrantJournalSnapshot binds the validated inventory to one persisted journal.
// It is not an execution permit or a lock around later grant mutations.
type GrantJournalSnapshot struct {
	UID             types.UID
	ResourceVersion string
	Bindings        []BindingReference
}

type grantJournalRecord struct {
	Version   int                `json:"version"`
	AgentName string             `json:"agentName"`
	AgentUID  types.UID          `json:"agentUID"`
	Bindings  []BindingReference `json:"bindings"`
}

type journalBindingKey struct {
	Kind, Namespace, Name string
}

type journalReadMode uint8

const (
	journalRequireProtection journalReadMode = iota
	journalAllowReleased
	journalAllowLegacyCleanup
)

// GrantJournalName is stable across restarts and distinct for replaced Agents.
// Ownership is checked independently of the name on every read.
func GrantJournalName(agentUID types.UID) string {
	sum := sha256.Sum256([]byte(agentUID))
	return "sandbox-grants-" + hex.EncodeToString(sum[:24])
}

// Read returns validated durable inventory, including obsolete entries and
// entries in a terminating journal. A UID-bound release receipt keeps empty
// inventory readable until deletion completes. A missing journal returns
// NotFound, not an empty inventory: discovery cannot prove that a write-ahead
// record was empty.
func (s GrantJournalStore) Read(ctx context.Context, agentName string, agentUID types.UID) ([]BindingReference, error) {
	snapshot, err := s.ReadSnapshot(ctx, agentName, agentUID)
	return snapshot.Bindings, err
}

// ReadSnapshot returns the identity and inventory from the same authoritative
// read and validation used by Read. Released inventory is never start authority.
func (s GrantJournalStore) ReadSnapshot(ctx context.Context, agentName string, agentUID types.UID) (GrantJournalSnapshot, error) {
	if err := s.validate(agentName, agentUID, false); err != nil {
		return GrantJournalSnapshot{}, err
	}
	object, refs, err := s.read(ctx, agentName, agentUID, journalAllowReleased)
	if err != nil {
		return GrantJournalSnapshot{}, err
	}
	return GrantJournalSnapshot{UID: object.UID, ResourceVersion: object.ResourceVersion, Bindings: refs}, nil
}

// ReadForCleanup is read-only and additionally accepts fully validated, owned,
// empty legacy journals whose finalizer was removed without a release receipt.
// Use only for cleanup, not grant application. Skip Revoke for an empty result;
// Delete revalidates the journal and CAS-persists its release receipt. Missing,
// corrupt, foreign and unprotected grant-bearing journals still fail closed.
func (s GrantJournalStore) ReadForCleanup(ctx context.Context, agentName string, agentUID types.UID) ([]BindingReference, error) {
	if err := s.validate(agentName, agentUID, false); err != nil {
		return nil, err
	}
	_, refs, err := s.read(ctx, agentName, agentUID, journalAllowLegacyCleanup)
	return refs, err
}

// Record durably unions desired with prior inventory, including when desired is
// empty. A known binding UID can be added, but never erased or replaced. A
// changed immutable identity at the same binding key must first be revoked.
// Released journals cannot accept new inventory.
func (s GrantJournalStore) Record(ctx context.Context, agentName string, agentUID types.UID, desired []BindingReference) error {
	if err := s.validate(agentName, agentUID, true); err != nil {
		return err
	}
	if err := validateJournalReferences(agentName, agentUID, desired); err != nil {
		return err
	}
	cm, previous, err := s.read(ctx, agentName, agentUID, journalAllowReleased)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	creating := apierrors.IsNotFound(err)
	if creating {
		controller, block := true, true
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: GrantJournalName(agentUID), Namespace: s.Namespace,
				Labels:      map[string]string{AgentUIDLabel: string(agentUID)},
				Annotations: map[string]string{AgentUIDLabel: string(agentUID)},
				Finalizers:  []string{GrantJournalFinalizer},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: api.GroupVersion.String(), Kind: "Agent", Name: agentName,
					UID: agentUID, Controller: &controller, BlockOwnerDeletion: &block,
				}},
			},
		}
	} else if !cm.DeletionTimestamp.IsZero() || !slices.Contains(cm.Finalizers, GrantJournalFinalizer) {
		return journalConflict(cm.Name, "cannot record grants in a terminating or released journal")
	}
	merged := make(map[journalBindingKey]BindingReference, len(previous)+len(desired))
	for _, ref := range previous {
		merged[journalKey(ref)] = ref
	}
	for _, ref := range desired {
		key := journalKey(ref)
		if old, exists := merged[key]; exists {
			if journalIdentity(old) != journalIdentity(ref) ||
				(old.UID != "" && ref.UID != "" && old.UID != ref.UID) {
				return fmt.Errorf("%w: journal binding identity changed before revocation", ErrConflict)
			}
			if old.UID != "" {
				ref.UID = old.UID
			}
		}
		merged[key] = ref
	}
	refs := make([]BindingReference, 0, len(merged))
	for _, ref := range merged {
		refs = append(refs, ref)
	}
	previousData := cm.Data[GrantJournalDataKey]
	if err := setJournalReferences(cm, agentName, agentUID, refs); err != nil {
		return err
	}
	if !creating && cm.Data[GrantJournalDataKey] == previousData {
		// The authoritative read already validated this protected inventory.
		return nil
	}
	originalUID := cm.UID
	if creating {
		err = s.Client.Create(ctx, cm)
		if apierrors.IsAlreadyExists(err) {
			return journalConflict(cm.Name, "journal was created concurrently")
		}
	} else {
		err = s.Client.Update(ctx, cm)
	}
	if err != nil {
		return fmt.Errorf("persist sandbox grant journal: %w", err)
	}
	if originalUID != "" && cm.UID != originalUID {
		return journalConflict(cm.Name, "journal was replaced during persistence")
	}
	return s.confirm(ctx, cm, agentName, agentUID, refs, false)
}

// ConfirmBindingIntents preserves observed UIDs before cleanup can delete their
// objects. Missing outcomes remain recorded, including in terminating journals.
// This confirms existing inventory only; it never creates or removes a grant.
func (s GrantJournalStore) ConfirmBindingIntents(ctx context.Context, agentName string, agentUID types.UID, desired []BindingReference) error {
	if err := s.validate(agentName, agentUID, true); err != nil {
		return err
	}
	if err := validateJournalReferences(agentName, agentUID, desired); err != nil {
		return err
	}
	cm, recorded, err := s.read(ctx, agentName, agentUID, journalRequireProtection)
	if err != nil {
		return err
	}
	indices := make(map[journalBindingKey]int, len(recorded))
	for index, ref := range recorded {
		indices[journalKey(ref)] = index
	}
	for _, requested := range desired {
		index, found := indices[journalKey(requested)]
		if !found || journalIdentity(recorded[index]) != journalIdentity(requested) ||
			(requested.UID != "" && requested.UID != recorded[index].UID) {
			return fmt.Errorf("%w: confirmation is not in the grant journal", ErrOwnership)
		}
	}
	changed, pending := false, false
	var failures []error
	for _, requested := range desired {
		ref := &recorded[indices[journalKey(requested)]]
		if ref.UID != "" {
			continue
		}
		object, err := readOwnedBinding(ctx, s.APIReader, *ref)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if object == nil {
			pending = true
			continue
		}
		ref.UID, changed = object.GetUID(), true
	}
	if changed {
		if err := setJournalReferences(cm, agentName, agentUID, recorded); err != nil {
			return errors.Join(append(failures, err)...)
		}
		originalUID := cm.UID
		if err := s.Client.Update(ctx, cm); err != nil {
			return errors.Join(append(failures, fmt.Errorf("confirm sandbox grant outcomes: %w", err))...)
		}
		if cm.UID != originalUID {
			return errors.Join(append(failures, journalConflict(cm.Name, "journal was replaced during confirmation"))...)
		}
		if err := s.confirm(ctx, cm, agentName, agentUID, recorded, true); err != nil {
			failures = append(failures, err)
		}
	}
	if len(failures) != 0 {
		return errors.Join(failures...)
	}
	if pending {
		return fmt.Errorf("%w: binding write outcome is unconfirmed", ErrRevocationPending)
	}
	return nil
}

// RefreshBindingIntents checks already-recorded desired grants before writes.
// Confirmed bindings that are authoritatively absent are retired durably before
// fresh UID-less intent is recorded. Live replacements are rejected, and healthy
// bindings are not revoked. This cannot introduce an unrecorded desired grant.
func (s GrantJournalStore) RefreshBindingIntents(ctx context.Context, agentName string, agentUID types.UID, desired []BindingReference) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.validate(agentName, agentUID, true); err != nil {
		return err
	}
	if err := validateJournalReferences(agentName, agentUID, desired); err != nil {
		return err
	}
	cm, previous, err := s.read(ctx, agentName, agentUID, journalRequireProtection)
	if err != nil {
		return err
	}
	if !cm.DeletionTimestamp.IsZero() {
		return journalConflict(cm.Name, "cannot refresh grants in a terminating journal")
	}
	recorded := make(map[journalBindingKey]BindingReference, len(previous))
	for _, ref := range previous {
		recorded[journalKey(ref)] = ref
	}
	var absent, replacement []BindingReference
	for _, requested := range desired {
		old, exists := recorded[journalKey(requested)]
		if !exists || journalIdentity(old) != journalIdentity(requested) ||
			(requested.UID != "" && requested.UID != old.UID) {
			return fmt.Errorf("%w: desired binding is not in the grant journal", ErrOwnership)
		}
		object, err := readOwnedBinding(ctx, s.APIReader, old)
		if err != nil {
			return err
		}
		if object != nil {
			if !object.GetDeletionTimestamp().IsZero() {
				return fmt.Errorf("%w: desired %s is terminating", ErrRevocationPending, old.Kind)
			}
			continue
		}
		if old.UID == "" {
			continue
		}
		absent = append(absent, old)
		old.UID = ""
		replacement = append(replacement, old)
	}
	if len(absent) == 0 {
		return nil
	}
	// Revoke rechecks the old identities, so a replacement racing the first
	// absence read cannot be silently retired or adopted.
	if err := s.Revoke(ctx, agentName, agentUID, absent); err != nil {
		return err
	}
	return s.Record(ctx, agentName, agentUID, replacement)
}

// Revoke acts only on recorded entries, using the stored UID even when remove
// omits it. All requests and the entire journal are validated before any binding
// effect. Partial revocation and failed CAS updates retain the original durable
// inventory; retry using Read. Unrecorded requests are rejected, not discovered
// or deleted. Already-absent recorded bindings are safe to remove.
func (s GrantJournalStore) Revoke(ctx context.Context, agentName string, agentUID types.UID, remove []BindingReference) error {
	if err := s.validate(agentName, agentUID, true); err != nil {
		return err
	}
	if err := validateJournalReferences(agentName, agentUID, remove); err != nil {
		return err
	}
	cm, previous, err := s.read(ctx, agentName, agentUID, journalAllowReleased)
	if err != nil {
		return err
	}
	recorded := make(map[journalBindingKey]BindingReference, len(previous))
	for _, ref := range previous {
		recorded[journalKey(ref)] = ref
	}
	effective := make([]BindingReference, 0, len(remove))
	for _, requested := range remove {
		old, exists := recorded[journalKey(requested)]
		if !exists || journalIdentity(old) != journalIdentity(requested) ||
			(requested.UID != "" && requested.UID != old.UID) {
			return fmt.Errorf("%w: revocation request is not in the grant journal", ErrOwnership)
		}
		effective = append(effective, old)
		delete(recorded, journalKey(requested))
	}
	if len(effective) == 0 {
		return nil
	}
	remaining := make([]BindingReference, 0, len(recorded))
	for _, ref := range recorded {
		remaining = append(remaining, ref)
	}
	if err := setJournalReferences(cm, agentName, agentUID, remaining); err != nil {
		return err
	}
	if err := RevokeBindingsWithReader(ctx, s.APIReader, s.Client, effective); err != nil {
		return err
	}
	originalUID := cm.UID
	if err := s.Client.Update(ctx, cm); err != nil {
		return fmt.Errorf("persist sandbox grant revocation: %w", err)
	}
	if cm.UID != originalUID {
		return journalConflict(cm.Name, "journal was replaced during revocation")
	}
	return s.confirm(ctx, cm, agentName, agentUID, remaining, true)
}

// Delete releases only this journal's finalizer, and only for a validated empty
// inventory, then deletes with UID and resourceVersion preconditions. Foreign
// metadata/finalizers and the Agent itself are never removed. NotFound makes
// journal deletion idempotent; it is not evidence that grants were revoked.
// Finalizer release atomically records a UID-bound receipt, so a retry can
// resume even if deletion and best-effort CAS restoration both fail. A released
// journal must remain empty and cannot authorize new grants. Validated empty
// legacy journals are upgraded by the same CAS write before deletion.
func (s GrantJournalStore) Delete(ctx context.Context, agentName string, agentUID types.UID) error {
	if err := s.validate(agentName, agentUID, true); err != nil {
		return err
	}
	cm, refs, err := s.read(ctx, agentName, agentUID, journalAllowLegacyCleanup)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(refs) != 0 {
		return fmt.Errorf("%w: grant journal is not empty", ErrRevocationPending)
	}
	originalUID := cm.UID
	if _, released := cm.Annotations[grantJournalReleasedAnnotation]; !released {
		cm.Finalizers = slices.DeleteFunc(cm.Finalizers, func(value string) bool { return value == GrantJournalFinalizer })
		cm.Annotations[grantJournalReleasedAnnotation] = string(originalUID)
		if err := journalSize(cm); err != nil {
			return err
		}
		if err := s.Client.Update(ctx, cm); err != nil {
			return fmt.Errorf("release sandbox grant journal: %w", err)
		}
		if cm.UID != originalUID {
			return journalConflict(cm.Name, "journal was replaced during finalizer release")
		}
	}
	releasedRefs, err := s.decode(cm, agentName, agentUID, journalAllowReleased)
	if err != nil {
		return err
	}
	if len(releasedRefs) != 0 || slices.Contains(cm.Finalizers, GrantJournalFinalizer) {
		return errors.Join(journalConflict(cm.Name, "journal changed during finalizer release"), s.restoreFinalizer(ctx, cm))
	}
	observed, observedRefs, err := s.read(ctx, agentName, agentUID, journalAllowReleased)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err == nil && (observed.UID != cm.UID || observed.ResourceVersion != cm.ResourceVersion ||
		len(observedRefs) != 0 || slices.Contains(observed.Finalizers, GrantJournalFinalizer)) {
		err = journalConflict(cm.Name, "journal changed after finalizer release")
	}
	if err == nil && observed.DeletionTimestamp.IsZero() {
		uid, version := observed.UID, observed.ResourceVersion
		err = s.Client.Delete(ctx, observed, client.Preconditions{UID: &uid, ResourceVersion: &version})
		if apierrors.IsNotFound(err) {
			return nil
		}
	}
	if err != nil {
		return errors.Join(fmt.Errorf("delete sandbox grant journal: %w", err), s.restoreFinalizer(ctx, cm))
	}
	return nil
}

func (s GrantJournalStore) validate(agentName string, agentUID types.UID, writing bool) error {
	if s.APIReader == nil || (writing && s.Client == nil) {
		return fmt.Errorf("%w: an authoritative journal reader and writer are required", ErrUnavailable)
	}
	if len(s.Namespace) > 63 || len(validation.IsDNS1123Label(s.Namespace)) != 0 {
		return fmt.Errorf("%w: an explicit valid journal namespace is required", ErrOwnership)
	}
	if len(agentName) > 253 || len(validation.IsDNS1123Subdomain(agentName)) != 0 || !validJournalUID(agentUID) {
		return fmt.Errorf("%w: a valid Agent name and UID are required", ErrOwnership)
	}
	return nil
}

func (s GrantJournalStore) read(ctx context.Context, agentName string, agentUID types.UID, mode journalReadMode) (*corev1.ConfigMap, []BindingReference, error) {
	cm := &corev1.ConfigMap{}
	key := client.ObjectKey{Namespace: s.Namespace, Name: GrantJournalName(agentUID)}
	if err := s.APIReader.Get(ctx, key, cm); err != nil {
		return nil, nil, fmt.Errorf("read sandbox grant journal: %w", err)
	}
	refs, err := s.decode(cm, agentName, agentUID, mode)
	if err != nil {
		return nil, nil, err
	}
	return cm, refs, nil
}

func (s GrantJournalStore) decode(cm *corev1.ConfigMap, agentName string, agentUID types.UID, mode journalReadMode) ([]BindingReference, error) {
	if cm.Name != GrantJournalName(agentUID) || cm.Namespace != s.Namespace ||
		!validJournalUID(cm.UID) || cm.ResourceVersion == "" ||
		(cm.Kind != "" && cm.Kind != "ConfigMap") || (cm.APIVersion != "" && cm.APIVersion != "v1") ||
		!bindingOwned(cm, agentName, agentUID) || cm.Annotations[AgentUIDLabel] != string(agentUID) {
		return nil, fmt.Errorf("%w: foreign or incomplete grant journal metadata", ErrOwnership)
	}
	owner := cm.OwnerReferences[0]
	if owner.BlockOwnerDeletion == nil || !*owner.BlockOwnerDeletion {
		return nil, fmt.Errorf("%w: grant journal must block foreground owner deletion", ErrOwnership)
	}
	finalizers := 0
	for _, value := range cm.Finalizers {
		if value == GrantJournalFinalizer {
			finalizers++
		}
	}
	if finalizers > 1 || (mode == journalRequireProtection && finalizers != 1) {
		return nil, fmt.Errorf("%w: grant journal finalizer is not intact", ErrOwnership)
	}
	if len(cm.Data) != 1 || len(cm.BinaryData) != 0 || (cm.Immutable != nil && *cm.Immutable) {
		return nil, fmt.Errorf("%w: unsupported grant journal ConfigMap contents", ErrOwnership)
	}
	raw, present := cm.Data[GrantJournalDataKey]
	if !present || len(raw) > MaxGrantJournalBytes || !utf8.ValidString(raw) {
		return nil, fmt.Errorf("%w: invalid grant journal data", ErrOwnership)
	}
	if err := journalSize(cm); err != nil {
		return nil, err
	}
	if err := validateJournalJSON(raw); err != nil {
		return nil, err
	}
	var record grantJournalRecord
	problems, err := strictjson.UnmarshalStrict([]byte(raw), &record, strictjson.DisallowDuplicateFields, strictjson.DisallowUnknownFields)
	if err != nil || len(problems) != 0 {
		return nil, fmt.Errorf("%w: malformed or ambiguous grant journal JSON", ErrOwnership)
	}
	if record.Version != 1 || record.AgentName != agentName || record.AgentUID != agentUID || record.Bindings == nil {
		return nil, fmt.Errorf("%w: unsupported or incomplete grant journal record", ErrOwnership)
	}
	if err := validateJournalReferences(agentName, agentUID, record.Bindings); err != nil {
		return nil, err
	}
	if releasedUID, released := cm.Annotations[grantJournalReleasedAnnotation]; released {
		if releasedUID != string(cm.UID) || finalizers != 0 || len(record.Bindings) != 0 {
			return nil, fmt.Errorf("%w: invalid grant journal release receipt", ErrOwnership)
		}
	} else if finalizers != 1 && (mode != journalAllowLegacyCleanup || len(record.Bindings) != 0) {
		return nil, fmt.Errorf("%w: grant journal finalizer is not intact", ErrOwnership)
	}
	return canonicalBindingReferences(record.Bindings), nil
}

func (s GrantJournalStore) confirm(ctx context.Context, written *corev1.ConfigMap, agentName string, agentUID types.UID, expected []BindingReference, allowTerminating bool) error {
	refs, err := s.decode(written, agentName, agentUID, journalRequireProtection)
	if err != nil {
		return err
	}
	expected = canonicalBindingReferences(expected)
	if !slices.Equal(refs, expected) || (!allowTerminating && !written.DeletionTimestamp.IsZero()) {
		return journalConflict(written.Name, "persisted journal differs from the requested inventory")
	}
	observed, refs, err := s.read(ctx, agentName, agentUID, journalRequireProtection)
	if err != nil {
		return err
	}
	if observed.UID != written.UID || observed.ResourceVersion != written.ResourceVersion ||
		!slices.Equal(refs, expected) || (!allowTerminating && !observed.DeletionTimestamp.IsZero()) {
		return journalConflict(written.Name, "journal changed before persistence was confirmed")
	}
	return nil
}

func (s GrantJournalStore) restoreFinalizer(ctx context.Context, released *corev1.ConfigMap) error {
	if !released.DeletionTimestamp.IsZero() || slices.Contains(released.Finalizers, GrantJournalFinalizer) {
		return nil
	}
	protected := released.DeepCopy()
	protected.Finalizers = append(protected.Finalizers, GrantJournalFinalizer)
	delete(protected.Annotations, grantJournalReleasedAnnotation)
	if err := s.Client.Update(ctx, protected); err != nil {
		return fmt.Errorf("restore empty grant journal protection: %w", err)
	}
	return nil
}

func setJournalReferences(cm *corev1.ConfigMap, agentName string, agentUID types.UID, refs []BindingReference) error {
	if err := validateJournalReferences(agentName, agentUID, refs); err != nil {
		return err
	}
	refs = canonicalBindingReferences(refs)
	if refs == nil {
		refs = []BindingReference{}
	}
	raw, err := json.Marshal(grantJournalRecord{Version: 1, AgentName: agentName, AgentUID: agentUID, Bindings: refs})
	if err != nil {
		return fmt.Errorf("encode sandbox grant journal: %w", err)
	}
	cm.Data = map[string]string{GrantJournalDataKey: string(raw)}
	return journalSize(cm)
}

func journalSize(cm *corev1.ConfigMap) error {
	raw, err := json.Marshal(cm)
	if err != nil {
		return fmt.Errorf("measure sandbox grant journal: %w", err)
	}
	if len(raw) > MaxGrantJournalBytes {
		return fmt.Errorf("%w: grant journal exceeds %d bytes", ErrConflict, MaxGrantJournalBytes)
	}
	return nil
}

func validateJournalReferences(agentName string, agentUID types.UID, refs []BindingReference) error {
	if len(refs) > MaxGrantJournalReferences {
		return fmt.Errorf("%w: grant journal exceeds %d references", ErrConflict, MaxGrantJournalReferences)
	}
	seen := make(map[journalBindingKey]bool, len(refs))
	for _, ref := range refs {
		if ref.AgentName != agentName || ref.AgentUID != agentUID ||
			len(ref.Name) > 253 || len(ref.ServiceAccountName) > 253 || len(ref.ServiceAccountNamespace) > 63 ||
			len(validation.IsDNS1123Subdomain(ref.Name)) != 0 ||
			len(validation.IsDNS1123Subdomain(ref.ServiceAccountName)) != 0 ||
			len(validation.IsDNS1123Label(ref.ServiceAccountNamespace)) != 0 ||
			(ref.UID != "" && !validJournalUID(ref.UID)) ||
			ref.RoleRef.APIGroup != rbacv1.GroupName || (ref.RoleRef.Kind != "Role" && ref.RoleRef.Kind != "ClusterRole") ||
			ref.RoleRef.Name == "" || !utf8.ValidString(ref.RoleRef.Name) ||
			len(pathvalidation.IsValidPathSegmentName(ref.RoleRef.Name)) != 0 {
			return fmt.Errorf("%w: malformed or foreign journal binding reference", ErrOwnership)
		}
		switch ref.Kind {
		case "RoleBinding":
			if len(ref.Namespace) > 63 || len(validation.IsDNS1123Label(ref.Namespace)) != 0 {
				return fmt.Errorf("%w: journal RoleBinding requires a valid namespace", ErrOwnership)
			}
		case "ClusterRoleBinding":
			if ref.Namespace != "" || ref.RoleRef.Kind != "ClusterRole" {
				return fmt.Errorf("%w: invalid journal ClusterRoleBinding scope", ErrOwnership)
			}
		default:
			return fmt.Errorf("%w: grant journal accepts only typed RBAC bindings", ErrOwnership)
		}
		if len(ref.GrantRevision) != sha256.Size*2 || strings.ToLower(ref.GrantRevision) != ref.GrantRevision {
			return fmt.Errorf("%w: journal binding requires a canonical grant revision", ErrOwnership)
		}
		if _, err := hex.DecodeString(ref.GrantRevision); err != nil {
			return fmt.Errorf("%w: malformed journal grant revision", ErrOwnership)
		}
		key := journalKey(ref)
		if seen[key] {
			return fmt.Errorf("%w: duplicate journal binding identity", ErrConflict)
		}
		seen[key] = true
	}
	return nil
}

// Count array entries before allocating typed references. The schema's maximum
// depth is four (record, bindings, reference, roleRef); null is never meaningful.
func validateJournalJSON(raw string) error {
	decoder := json.NewDecoder(strings.NewReader(raw))
	var containers []json.Delim
	entries := 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil || token == nil {
			return fmt.Errorf("%w: invalid or null grant journal JSON", ErrOwnership)
		}
		delim, isDelim := token.(json.Delim)
		if len(containers) == 2 && containers[1] == '[' && token != json.Delim(']') {
			entries++
			if entries > MaxGrantJournalReferences {
				return fmt.Errorf("%w: excessive journal JSON references", ErrConflict)
			}
		}
		if isDelim {
			switch delim {
			case '{', '[':
				containers = append(containers, delim)
				if len(containers) > 4 {
					return fmt.Errorf("%w: excessive journal JSON nesting", ErrOwnership)
				}
			case '}', ']':
				containers = containers[:len(containers)-1]
			}
		}
	}
}

func validJournalUID(uid types.UID) bool {
	return uid != "" && len(uid) <= 63 && len(validation.IsValidLabelValue(string(uid))) == 0
}

func journalKey(ref BindingReference) journalBindingKey {
	return journalBindingKey{Kind: ref.Kind, Namespace: ref.Namespace, Name: ref.Name}
}

func journalIdentity(ref BindingReference) BindingReference {
	ref.UID = ""
	return ref
}

func journalConflict(name, reason string) error {
	return apierrors.NewConflict(corev1.Resource("configmaps"), name, errors.New(reason))
}

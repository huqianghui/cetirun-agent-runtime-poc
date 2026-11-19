// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxwriter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	strictjson "sigs.k8s.io/json"
)

const lifetimeAnnotation = "sandbox.cetirun.microsoft.com/writer-lifetime"

// Store uses Client only for writes and APIReader for authoritative reads.
// Namespace must be an explicit, separately protected controller namespace,
// never the disposable workload namespace. No operation lists resources.
// All updates are UID/resource-version CAS without conflict retries.
type Store struct {
	Client    client.Client
	APIReader client.Reader
	Namespace string
}

type journalRecord struct {
	Version      string   `json:"version"`
	Lifetime     Lifetime `json:"lifetime"`
	EnrollmentID string   `json:"enrollmentID"`
	Writers      []Writer `json:"writers"`
	Phase        Phase    `json:"phase"`
	OperationID  string   `json:"operationID"`
}

// JournalName addresses one exact Agent/namespace/HOME lifetime, including all
// UIDs. Ownership and the complete tuple are checked independently on each read.
func JournalName(lifetime Lifetime) string {
	key := lifetime.Agent.Name + "\x00" + string(lifetime.Agent.UID) + "\x00" +
		lifetime.Namespace.Name + "\x00" + string(lifetime.Namespace.UID) + "\x00" +
		lifetime.Home.Name + "\x00" + string(lifetime.Home.UID)
	sum := sha256.Sum256([]byte(key))
	return "sandbox-writers-" + hex.EncodeToString(sum[:24])
}

func journalOwner(lifetime Lifetime) metav1.OwnerReference {
	owner := sandboxworkload.AgentOwner(lifetime.Agent.Name, lifetime.Agent.UID)
	// History outlives cleanup; its retained finalizer must not block the
	// Agent's foreground deletion after the Agent's own cleanup completes.
	owner.BlockOwnerDeletion = ptr.To(false)
	return owner
}

// Enroll is an explicit, create-only assertion by trusted enrollment authority
// that this journal covers the complete lifetime from birth, before any possible
// writer. It never adopts or resets an existing journal. After an ambiguous
// create, use Read and verify the enrollment ID; do not recreate history.
func (s Store) Enroll(ctx context.Context, lifetime Lifetime, enrollmentID string) (Snapshot, error) {
	if err := s.validate(ctx, lifetime, true); err != nil {
		return Snapshot{}, err
	}
	record := journalRecord{
		Version: JournalVersion, Lifetime: lifetime, EnrollmentID: enrollmentID,
		Writers: []Writer{}, Phase: PhaseEnrolled,
	}
	object := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: JournalName(lifetime), Namespace: s.Namespace,
		Annotations: map[string]string{
			sandboxworkload.AgentNameAnnotation: lifetime.Agent.Name,
			sandboxworkload.AgentUIDAnnotation:  string(lifetime.Agent.UID),
			lifetimeAnnotation:                  JournalName(lifetime),
		},
		OwnerReferences: []metav1.OwnerReference{journalOwner(lifetime)},
		Finalizers:      []string{JournalFinalizer},
	}}
	if err := encodeRecord(object, record); err != nil {
		return Snapshot{}, err
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	if err := s.Client.Create(ctx, object); err != nil {
		return Snapshot{}, fmt.Errorf("enroll Sandbox writer lifetime: %w", err)
	}
	return s.confirm(ctx, object, record, "", "", false)
}

// Read never treats NotFound as an empty history. A terminating journal remains
// readable for exclusion while its protection is intact, but cannot reserve.
func (s Store) Read(ctx context.Context, lifetime Lifetime) (Snapshot, error) {
	_, snapshot, err := s.read(ctx, lifetime)
	return snapshot, err
}

// Reserve appends exactly one possible writer after a verified quiescent phase.
// It invalidates the prior operation/proof and never changes an existing entry.
// This is write-ahead bookkeeping, not permission to lift the start barrier.
func (s Store) Reserve(ctx context.Context, previous Snapshot, writer Writer) (Snapshot, error) {
	previous = previous.DeepCopy()
	object, err := s.pinned(ctx, previous)
	if err != nil {
		return Snapshot{}, err
	}
	if previous.Phase != PhaseQuiescent || !object.DeletionTimestamp.IsZero() {
		return Snapshot{}, conflict(object.Name, "reservation requires a protected, nonterminating quiescent journal")
	}
	if len(previous.Writers) >= MaxWriters {
		return Snapshot{}, fmt.Errorf("%w: writer history is full; it cannot be truncated", ErrInvalidHistory)
	}
	next := previous.DeepCopy()
	next.Writers = append(next.Writers, writer)
	next.Phase, next.OperationID = PhaseReserved, ""
	return s.update(ctx, object, previous, next, false)
}

func (s Store) validate(ctx context.Context, lifetime Lifetime, writing bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.APIReader == nil || (writing && s.Client == nil) {
		return fmt.Errorf("%w: an authoritative APIReader and journal writer are required", ErrUnavailable)
	}
	if len(validation.IsDNS1123Label(s.Namespace)) != 0 || s.Namespace == lifetime.Namespace.Name {
		return fmt.Errorf("%w: an explicit, separate protected journal namespace is required", ErrUnavailable)
	}
	return validateLifetime(lifetime)
}

func (s Store) read(ctx context.Context, lifetime Lifetime) (*corev1.ConfigMap, Snapshot, error) {
	if err := s.validate(ctx, lifetime, false); err != nil {
		return nil, Snapshot{}, err
	}
	object := &corev1.ConfigMap{}
	if err := s.APIReader.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: JournalName(lifetime)}, object); err != nil {
		return nil, Snapshot{}, fmt.Errorf("read Sandbox writer history: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, Snapshot{}, err
	}
	snapshot, err := s.decode(object, lifetime)
	if err != nil {
		return nil, Snapshot{}, err
	}
	if err := ctx.Err(); err != nil {
		return nil, Snapshot{}, err
	}
	return object, snapshot, nil
}

func (s Store) pinned(ctx context.Context, previous Snapshot) (*corev1.ConfigMap, error) {
	object, observed, err := s.read(ctx, previous.Lifetime)
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(observed, previous) {
		return nil, conflict(object.Name, "journal UID, resource version or pinned history changed")
	}
	return object, nil
}

func (s Store) decode(object *corev1.ConfigMap, lifetime Lifetime) (Snapshot, error) {
	if object.Name != JournalName(lifetime) || object.Namespace != s.Namespace ||
		!boundedValue(string(object.UID), 128) || !boundedValue(object.ResourceVersion, 128) ||
		(object.Kind != "" && object.Kind != "ConfigMap") || (object.APIVersion != "" && object.APIVersion != "v1") ||
		!reflect.DeepEqual(object.OwnerReferences, []metav1.OwnerReference{journalOwner(lifetime)}) ||
		object.Annotations[sandboxworkload.AgentNameAnnotation] != lifetime.Agent.Name ||
		object.Annotations[sandboxworkload.AgentUIDAnnotation] != string(lifetime.Agent.UID) ||
		object.Annotations[lifetimeAnnotation] != JournalName(lifetime) {
		return Snapshot{}, fmt.Errorf("%w: foreign or incomplete writer journal metadata", ErrInvalidHistory)
	}
	finalizers := 0
	for _, finalizer := range object.Finalizers {
		if finalizer == JournalFinalizer {
			finalizers++
		}
	}
	_, released := object.Annotations[JournalReleasedAnnotation]
	if finalizers != 1 || released || (object.Immutable != nil && *object.Immutable) ||
		len(object.Data) != 1 || len(object.BinaryData) != 0 {
		return Snapshot{}, fmt.Errorf("%w: writer journal is released, unprotected or has unsupported contents", ErrInvalidHistory)
	}
	raw, present := object.Data[JournalDataKey]
	if !present || len(raw) > MaxJournalBytes || !utf8.ValidString(raw) {
		return Snapshot{}, fmt.Errorf("%w: missing or oversized writer journal data", ErrInvalidHistory)
	}
	if err := journalSize(object); err != nil {
		return Snapshot{}, err
	}
	if err := boundedJSON(raw); err != nil {
		return Snapshot{}, err
	}
	var record journalRecord
	problems, err := strictjson.UnmarshalStrict([]byte(raw), &record)
	if err != nil || len(problems) != 0 {
		return Snapshot{}, fmt.Errorf("%w: malformed, unknown or duplicate journal fields", ErrInvalidHistory)
	}
	if err := validateRecord(record); err != nil {
		return Snapshot{}, err
	}
	if record.Lifetime != lifetime {
		return Snapshot{}, fmt.Errorf("%w: journal belongs to another lifetime", ErrInvalidHistory)
	}
	return Snapshot{
		UID: object.UID, ResourceVersion: object.ResourceVersion, Version: record.Version,
		Lifetime: record.Lifetime, EnrollmentID: record.EnrollmentID, Writers: record.Writers,
		Phase: record.Phase, OperationID: record.OperationID,
	}, nil
}

func recordOf(snapshot Snapshot) journalRecord {
	return journalRecord{
		Version: snapshot.Version, Lifetime: snapshot.Lifetime, EnrollmentID: snapshot.EnrollmentID,
		Writers: snapshot.Writers, Phase: snapshot.Phase, OperationID: snapshot.OperationID,
	}
}

func (s Store) update(ctx context.Context, object *corev1.ConfigMap, previous, next Snapshot, allowTerminating bool) (Snapshot, error) {
	if err := s.validate(ctx, previous.Lifetime, true); err != nil {
		return Snapshot{}, err
	}
	if object.UID != previous.UID || object.ResourceVersion != previous.ResourceVersion ||
		(!allowTerminating && !object.DeletionTimestamp.IsZero()) {
		return Snapshot{}, conflict(object.Name, "journal changed before update")
	}
	if err := validateTransition(previous, next); err != nil {
		return Snapshot{}, err
	}
	record := recordOf(next)
	if err := encodeRecord(object, record); err != nil {
		return Snapshot{}, err
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	// Update sends both UID and resourceVersion. Never retry a conflict or an
	// ambiguous result against newer state, even if its history looks equal.
	if err := s.Client.Update(ctx, object); err != nil {
		return Snapshot{}, fmt.Errorf("update Sandbox writer journal: %w", err)
	}
	return s.confirm(ctx, object, record, previous.UID, previous.ResourceVersion, allowTerminating)
}

func (s Store) confirm(ctx context.Context, written *corev1.ConfigMap, expected journalRecord, oldUID types.UID, oldRV string, allowTerminating bool) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	ack, err := s.decode(written, expected.Lifetime)
	if err != nil {
		return Snapshot{}, err
	}
	if (oldUID != "" && ack.UID != oldUID) || ack.ResourceVersion == oldRV ||
		!reflect.DeepEqual(recordOf(ack), expected) || (!allowTerminating && !written.DeletionTimestamp.IsZero()) {
		return Snapshot{}, conflict(written.Name, "journal write was not acknowledged with the exact new identity and state")
	}
	object, observed, err := s.read(ctx, expected.Lifetime)
	if err != nil {
		return Snapshot{}, err
	}
	if !reflect.DeepEqual(observed, ack) || (!allowTerminating && !object.DeletionTimestamp.IsZero()) {
		return Snapshot{}, conflict(written.Name, "journal changed during authoritative write read-back")
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	return observed, nil
}

func encodeRecord(object *corev1.ConfigMap, record journalRecord) error {
	if err := validateRecord(record); err != nil {
		return err
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode Sandbox writer history: %w", err)
	}
	object.Data = map[string]string{JournalDataKey: string(raw)}
	return journalSize(object)
}

func journalSize(object *corev1.ConfigMap) error {
	raw, err := json.Marshal(object)
	if err != nil {
		return fmt.Errorf("encode Sandbox writer journal: %w", err)
	}
	if len(raw) > MaxJournalBytes {
		return fmt.Errorf("%w: complete journal exceeds its byte bound", ErrInvalidHistory)
	}
	return nil
}

func validateLifetime(lifetime Lifetime) error {
	if !validIdentity(lifetime.Agent) || !validIdentity(lifetime.Namespace) || !validIdentity(lifetime.Home) ||
		len(validation.IsDNS1123Label(lifetime.Namespace.Name)) != 0 {
		return fmt.Errorf("%w: complete Agent, namespace and HOME identities are required", ErrInvalidHistory)
	}
	return nil
}

func validateRecord(record journalRecord) error {
	if err := validateLifetime(record.Lifetime); err != nil {
		return err
	}
	if record.Version != JournalVersion || !boundedValue(record.EnrollmentID, 256) ||
		record.Writers == nil || len(record.Writers) > MaxWriters {
		return fmt.Errorf("%w: unsupported version, enrollment or writer inventory", ErrInvalidHistory)
	}
	switch record.Phase {
	case PhaseEnrolled:
		if len(record.Writers) != 0 || record.OperationID != "" {
			return fmt.Errorf("%w: enrollment cannot contain writers or an operation", ErrInvalidHistory)
		}
	case PhaseReserved:
		if len(record.Writers) == 0 || record.OperationID != "" {
			return fmt.Errorf("%w: reservation requires a writer and no prior proof", ErrInvalidHistory)
		}
	case PhaseRevoking, PhaseQuiescent:
		operation, err := hex.DecodeString(record.OperationID)
		if err != nil || len(operation) != 16 || hex.EncodeToString(operation) != record.OperationID {
			return fmt.Errorf("%w: revocation requires a persisted operation ID", ErrInvalidHistory)
		}
	default:
		return fmt.Errorf("%w: unknown or released journal phase", ErrInvalidHistory)
	}
	return validateWriters(record.Writers)
}

func validateWriters(writers []Writer) error {
	pods, epochs := map[types.UID]bool{}, map[string]bool{}
	workloads := map[types.UID]sandboxworkload.ObjectIdentity{}
	nodes, machines := map[types.UID]Writer{}, map[string]Writer{}
	volumes, handles := map[types.UID]StorageIdentity{}, map[[2]string]StorageIdentity{}
	for _, writer := range writers {
		if !validIdentity(writer.Workload) || !validIdentity(writer.Pod) ||
			len(validation.IsDNS1123Subdomain(writer.Node.Name)) != 0 ||
			!boundedValue(string(writer.Node.UID), 128) || !boundedValue(writer.Node.ProviderID, 2048) ||
			!boundedValue(writer.MachineID, 256) || writer.MachineID == writer.Node.ProviderID ||
			!boundedValue(writer.Epoch, 128) || !validIdentity(writer.Storage.Volume) ||
			!boundedValue(writer.Storage.Driver, 253) || !boundedValue(writer.Storage.Handle, 2048) ||
			pods[writer.Pod.UID] || epochs[writer.Epoch] {
			return fmt.Errorf("%w: writers require complete, nonreused Pod/epoch and physical/storage identities", ErrInvalidHistory)
		}
		if previous, ok := workloads[writer.Workload.UID]; ok && previous != writer.Workload {
			return fmt.Errorf("%w: workload UID identity drift", ErrInvalidHistory)
		}
		if previous, ok := nodes[writer.Node.UID]; ok && (previous.Node != writer.Node || previous.MachineID != writer.MachineID) {
			return fmt.Errorf("%w: Node UID or physical incarnation drift", ErrInvalidHistory)
		}
		if previous, ok := machines[writer.MachineID]; ok && previous.Node != writer.Node {
			return fmt.Errorf("%w: physical incarnation assigned to a different Node identity", ErrInvalidHistory)
		}
		handle := [2]string{writer.Storage.Driver, writer.Storage.Handle}
		if previous, ok := volumes[writer.Storage.Volume.UID]; ok && previous != writer.Storage {
			return fmt.Errorf("%w: historical volume UID identity drift", ErrInvalidHistory)
		}
		if previous, ok := handles[handle]; ok && previous != writer.Storage {
			return fmt.Errorf("%w: historical storage handle identity drift", ErrInvalidHistory)
		}
		pods[writer.Pod.UID], epochs[writer.Epoch] = true, true
		workloads[writer.Workload.UID] = writer.Workload
		nodes[writer.Node.UID], machines[writer.MachineID] = writer, writer
		volumes[writer.Storage.Volume.UID], handles[handle] = writer.Storage, writer.Storage
	}
	return nil
}

func validateTransition(previous, next Snapshot) error {
	if previous.UID != next.UID || previous.ResourceVersion != next.ResourceVersion ||
		previous.Version != next.Version || previous.Lifetime != next.Lifetime || previous.EnrollmentID != next.EnrollmentID {
		return fmt.Errorf("%w: journal identity and enrollment are immutable", ErrInvalidHistory)
	}
	switch {
	case (previous.Phase == PhaseEnrolled || previous.Phase == PhaseReserved) && next.Phase == PhaseRevoking:
		if slices.Equal(previous.Writers, next.Writers) {
			return nil
		}
	case previous.Phase == PhaseRevoking && next.Phase == PhaseQuiescent:
		if slices.Equal(previous.Writers, next.Writers) && previous.OperationID == next.OperationID {
			return nil
		}
	case previous.Phase == PhaseQuiescent && next.Phase == PhaseReserved:
		if len(next.Writers) == len(previous.Writers)+1 && slices.Equal(previous.Writers, next.Writers[:len(previous.Writers)]) {
			return nil
		}
	}
	return fmt.Errorf("%w: invalid phase transition or non-append-only history", ErrInvalidHistory)
}

// Bound depth and array entries before allocating typed history; null cannot
// stand in for an absent immutable identity or an empty complete inventory.
func boundedJSON(raw string) error {
	decoder := json.NewDecoder(strings.NewReader(raw))
	var containers []json.Delim
	entries := 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil || token == nil {
			return fmt.Errorf("%w: invalid or null writer journal JSON", ErrInvalidHistory)
		}
		if len(containers) == 2 && containers[1] == '[' && token != json.Delim(']') {
			entries++
			if entries > MaxWriters {
				return fmt.Errorf("%w: excessive writer history entries", ErrInvalidHistory)
			}
		}
		if delim, ok := token.(json.Delim); ok {
			switch delim {
			case '{', '[':
				containers = append(containers, delim)
				if len(containers) > 5 {
					return fmt.Errorf("%w: excessive writer journal nesting", ErrInvalidHistory)
				}
			case '}', ']':
				containers = containers[:len(containers)-1]
			}
		}
	}
}

func validIdentity(identity sandboxworkload.ObjectIdentity) bool {
	return len(validation.IsDNS1123Subdomain(identity.Name)) == 0 && boundedValue(string(identity.UID), 128)
}

func boundedValue(value string, limit int) bool {
	if len(value) == 0 || len(value) > limit {
		return false
	}
	for _, char := range value {
		if char < '!' || char > '~' {
			return false
		}
	}
	return true
}

func conflict(name, message string) error {
	return apierrors.NewConflict(corev1.Resource("configmaps"), name, errors.New(message))
}

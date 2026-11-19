// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package sandboxcoordination serializes participating execution and source
// changes. Its records are not runtime permits or evidence of writer exclusion.
package sandboxcoordination

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxwriter"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	pathvalidation "k8s.io/apimachinery/pkg/api/validation/path"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	strictjson "sigs.k8s.io/json"
)

const (
	DataKey    = "coordination.json"
	Finalizer  = "sandbox.cetirun.microsoft.com/coordination"
	MaxBytes   = 512 * 1024
	MaxEntries = 128
	// Access dependencies are appended after the preparation envelope itself.
	// Keep their budget separate so a full preparation remains enrollable.
	MaxPreparedExecutionSources = w.MaxPreparationSources + 1
	MaxIngressNamespaces        = 128
	MaxAccessSources            = 2 + MaxIngressNamespaces // Service, Gateway and distinct consumer namespaces.
	MaxSources                  = MaxPreparedExecutionSources + MaxAccessSources
	Reserved                    = Phase("reserved")
	Starting                    = Phase("starting")
	Running                     = Phase("running")
	Stopping                    = Phase("stopping")
	Changing                    = Phase("changing")
)

var (
	ErrConflict    = errors.New("Sandbox coordination conflicts with an outstanding operation")
	ErrUnavailable = errors.New("Sandbox coordination authority is unavailable")
)

type Phase string

type EnrolledTarget struct {
	sandbox.ExecutionIdentity
	Writer        sandboxwriter.Writer
	WriterJournal w.ObjectIdentity
}

// Intent contains identities and digests only. Expected must be independently
// enrolled before consulting an enforcement observation, not copied from it.
type Intent struct {
	Agent                    w.ObjectIdentity           `json:"agent"`
	AgentGeneration          int64                      `json:"agentGeneration"`
	Namespace                w.ObjectIdentity           `json:"namespace"`
	Home                     w.ObjectIdentity           `json:"home"`
	Workload                 w.ObjectIdentity           `json:"workload"`
	WorkloadDigest           string                     `json:"workloadDigest"`
	Expected                 sandbox.ExecutionIdentity  `json:"expected"`
	Writer                   sandboxwriter.Writer       `json:"writer"`
	WriterJournal            w.ObjectIdentity           `json:"writerJournal"`
	Preparation              w.SourceReference          `json:"preparation"`
	RunningPreparationDigest string                     `json:"runningPreparationDigest,omitempty"`
	Placement                *w.PlacementBinding        `json:"placement"`
	Sources                  []w.SourceReference        `json:"sources"`
	SourceDigests            map[string]string          `json:"sourceDigests"`
	TeamSharedDigest         string                     `json:"teamSharedDigest"`
	IngressMode              api.AgentPolicyIngressMode `json:"ingressMode"`
	Access                   *AccessBinding             `json:"access,omitempty"`
}

// AccessBinding pins an already-created, nonforwarding route slot. Runtime
// publication may update this UID, never create or adopt a replacement.
type AccessBinding struct {
	Route             w.ObjectIdentity    `json:"route"`
	Service           w.SourceReference   `json:"service"`
	Gateway           w.SourceReference   `json:"gateway"`
	IngressNamespaces []w.SourceReference `json:"ingressNamespaces,omitempty"`
	Host              string              `json:"host"`
	Port              int32               `json:"port"`
}

func (intent Intent) Validate() error {
	return validateIntent(intent)
}

func (intent Intent) SelectedSandbox() string {
	for _, source := range intent.Sources {
		if source.APIVersion == api.GroupVersion.String() && source.Kind == "Sandbox" &&
			source.UID == intent.Expected.Binding.SandboxUID {
			return source.Name
		}
	}
	return ""
}

type Execution struct {
	ID     string `json:"id"`
	Intent Intent `json:"intent"`
	Phase  Phase  `json:"phase"`
}

// Change reserves one exact API mutation. The caller must retain it across an
// uncertain outcome until the API write is definitively observed or excluded.
type Change struct {
	ID            string            `json:"id"`
	Before        w.SourceReference `json:"before"`
	DesiredDigest string            `json:"desiredDigest"`
	// BeforeContentDigest pins the complete writable content before a concrete
	// configuration update. It lets recovery distinguish a superseded
	// resourceVersion from an unrelated configuration change.
	BeforeContentDigest string      `json:"beforeContentDigest,omitempty"`
	Phase               Phase       `json:"phase"`
	Team                *TeamChange `json:"team,omitempty"`
	// UnchangedDigest pins equal before/after effective Sandbox or AgentPolicy inputs.
	UnchangedDigest       string `json:"unchangedDigest,omitempty"`
	DeletionContentDigest string `json:"deletionContentDigest,omitempty"`
}

type TeamChange struct {
	Before w.TeamInputs `json:"before"`
	After  w.TeamInputs `json:"after"`
}

type Ticket struct {
	AuthorityUID types.UID
	Execution    Execution
}

func (ticket Ticket) DeepCopy() Ticket {
	ticket.Execution = cloneExecution(ticket.Execution)
	return ticket
}

type ChangeTicket struct {
	AuthorityUID types.UID
	Change       Change
}

func (ticket ChangeTicket) DeepCopy() ChangeTicket {
	ticket.Change = cloneChange(ticket.Change)
	return ticket
}

type record struct {
	Version            int                 `json:"version"`
	Executions         []Execution         `json:"executions"`
	Changes            []Change            `json:"changes"`
	NativeReservations []NativeReservation `json:"nativeReservations,omitempty"`
}

// Store uses one UID-pinned, protected object so acquisition is atomic across
// shared sources. There is deliberately no TTL, stealing, or create-on-missing.
// All source writers must participate before a deployment can use this store
// for execution. Read-only source rechecks alone do not establish that boundary.
type Store struct {
	Client    client.Client
	APIReader client.Reader
	Namespace string
	Identity  w.ObjectIdentity
}

// NewState is for explicit trusted installation, before any execution is
// possible. Pin the server-issued UID in Store.Identity after creating it.
func NewState(namespace, name string) (*corev1.ConfigMap, error) {
	if len(validation.IsDNS1123Label(namespace)) != 0 || len(validation.IsDNS1123Subdomain(name)) != 0 {
		return nil, fmt.Errorf("%w: valid state namespace and name are required", ErrUnavailable)
	}
	object := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Namespace: namespace, Name: name, Finalizers: []string{Finalizer},
	}}
	if err := encode(object, record{Version: 1, Executions: []Execution{}, Changes: []Change{}}); err != nil {
		return nil, err
	}
	return object, nil
}

func (s Store) AcquireExecution(ctx context.Context, intent Intent) (Ticket, error) {
	if err := validateIntent(intent); err != nil {
		return Ticket{}, err
	}
	if intent.Namespace.Name == s.Namespace {
		return Ticket{}, fmt.Errorf("%w: execution cannot use the authority namespace", ErrConflict)
	}
	object, state, err := s.read(ctx)
	if err != nil {
		return Ticket{}, err
	}
	for _, existing := range state.Executions {
		if existing.Intent.Agent.UID == intent.Agent.UID {
			if equality.Semantic.DeepEqual(existing.Intent, intent) {
				return Ticket{object.UID, cloneExecution(existing)}, nil
			}
			return Ticket{}, ErrConflict
		}
		if err := compatibleExecutions(existing.Intent, intent); err != nil {
			return Ticket{}, err
		}
	}
	for _, native := range state.NativeReservations {
		if err := compatibleNativeExecution(native.Intent, intent); err != nil {
			return Ticket{}, err
		}
	}
	for _, change := range state.Changes {
		if changeAffects(intent, change) {
			return Ticket{}, ErrConflict
		}
	}
	execution := Execution{ID: rand.Text(), Intent: intent, Phase: Reserved}
	state.Executions = append(state.Executions, execution)
	if err := s.write(ctx, object, state); err != nil {
		return Ticket{}, err
	}
	ticket := Ticket{object.UID, cloneExecution(execution)}
	if err := s.Check(ctx, ticket); err != nil {
		return Ticket{}, err
	}
	return ticket, nil
}

func (s Store) AcquireChange(ctx context.Context, before w.SourceReference, desiredDigest string) (ChangeTicket, error) {
	return s.acquireChange(ctx, before, desiredDigest, "", nil, "", "")
}

// AcquireTeamChange derives the affected execution selections from an exact
// authoritative before image and the full proposed mutation. It does not bypass
// configuration admission. Submit must enforce Before and DesiredDigest at its
// actual write boundary; callers cannot supply a partial field-mask assertion.
func (s Store) AcquireTeamChange(ctx context.Context, before, after *api.Team) (ChangeTicket, error) {
	if s.APIReader == nil || before == nil || after == nil || before.Namespace != "" ||
		after.Namespace != "" || before.Name != after.Name || before.UID != after.UID ||
		before.ResourceVersion != after.ResourceVersion || before.ResourceVersion == "" {
		return ChangeTicket{}, fmt.Errorf("%w: exact before and proposed Team identities are required", ErrUnavailable)
	}
	oldInputs, err := w.ExecutionTeamInputs(before)
	if err != nil {
		return ChangeTicket{}, err
	}
	newInputs, err := w.ExecutionTeamInputs(after)
	if err != nil {
		return ChangeTicket{}, err
	}
	desiredDigest, err := w.TeamMutationDigest(after)
	if err != nil {
		return ChangeTicket{}, err
	}
	beforeContentDigest, err := w.TeamMutationDigest(before)
	if err != nil {
		return ChangeTicket{}, err
	}
	if beforeContentDigest == desiredDigest {
		return ChangeTicket{}, fmt.Errorf("%w: Team configuration updates require a writable-content change", ErrConflict)
	}
	reference := w.SourceReference{APIVersion: api.GroupVersion.String(), Kind: "Team",
		Name: before.Name, UID: before.UID, ResourceVersion: before.ResourceVersion}
	scope := &TeamChange{Before: oldInputs, After: newInputs}
	object, state, err := s.read(ctx)
	if err != nil {
		return ChangeTicket{}, err
	}
	for _, existing := range state.Changes {
		if sourceKey(existing.Before) != sourceKey(reference) {
			continue
		}
		// A committed API mutation can advance the live Team before its reply
		// arrives. Recover the exact journaled operation, never a new write.
		if existing.Before == reference && existing.DesiredDigest == desiredDigest && existing.BeforeContentDigest == beforeContentDigest &&
			equality.Semantic.DeepEqual(existing.Team, scope) {
			return ChangeTicket{object.UID, cloneChange(existing)}, nil
		}
		return ChangeTicket{}, ErrConflict
	}
	current := &api.Team{}
	if err := s.APIReader.Get(ctx, client.ObjectKeyFromObject(before), current); err != nil {
		return ChangeTicket{}, err
	}
	currentDigest, err := w.TeamMutationDigest(current)
	if err != nil {
		return ChangeTicket{}, err
	}
	if current.UID != before.UID || current.ResourceVersion != before.ResourceVersion ||
		!current.DeletionTimestamp.IsZero() || beforeContentDigest != currentDigest {
		return ChangeTicket{}, ErrConflict
	}
	return s.acquireChange(ctx, reference, desiredDigest, beforeContentDigest, scope, "", "")
}

func (s Store) acquireChange(ctx context.Context, before w.SourceReference, desiredDigest, beforeContentDigest string, team *TeamChange, unchangedDigest, deletionContentDigest string) (ChangeTicket, error) {
	if !validSource(before) || !digest(desiredDigest) || (beforeContentDigest != "" &&
		(!digest(beforeContentDigest) || beforeContentDigest == desiredDigest)) {
		return ChangeTicket{}, fmt.Errorf("%w: change requires an exact source and desired-content digest", ErrUnavailable)
	}
	object, state, err := s.read(ctx)
	if err != nil {
		return ChangeTicket{}, err
	}
	for _, execution := range state.Executions {
		if changeAffects(execution.Intent, Change{Before: before, Team: team, UnchangedDigest: unchangedDigest}) {
			return ChangeTicket{}, ErrConflict
		}
	}
	for _, native := range state.NativeReservations {
		if nativeChangeAffects(native.Intent, Change{Before: before, Team: team, UnchangedDigest: unchangedDigest}) {
			return ChangeTicket{}, ErrConflict
		}
	}
	for _, existing := range state.Changes {
		if sourceKey(existing.Before) == sourceKey(before) {
			if existing.Before == before && existing.DesiredDigest == desiredDigest && existing.BeforeContentDigest == beforeContentDigest &&
				existing.UnchangedDigest == unchangedDigest && existing.DeletionContentDigest == deletionContentDigest &&
				equality.Semantic.DeepEqual(existing.Team, team) {
				return ChangeTicket{object.UID, cloneChange(existing)}, nil
			}
			return ChangeTicket{}, ErrConflict
		}
	}
	change := Change{ID: rand.Text(), Before: before, DesiredDigest: desiredDigest, BeforeContentDigest: beforeContentDigest,
		Phase: Reserved, Team: team, UnchangedDigest: unchangedDigest, DeletionContentDigest: deletionContentDigest}
	state.Changes = append(state.Changes, change)
	if err := s.write(ctx, object, state); err != nil {
		return ChangeTicket{}, err
	}
	ticket := ChangeTicket{object.UID, cloneChange(change)}
	if err := s.CheckChange(ctx, ticket); err != nil {
		return ChangeTicket{}, err
	}
	return ticket, nil
}

func (s Store) ReadExecution(ctx context.Context, agentUID types.UID) (Ticket, error) {
	if agentUID == "" {
		return Ticket{}, fmt.Errorf("%w: Agent UID is required", ErrUnavailable)
	}
	object, state, err := s.read(ctx)
	if err != nil {
		return Ticket{}, err
	}
	for _, execution := range state.Executions {
		if execution.Intent.Agent.UID == agentUID {
			return Ticket{object.UID, cloneExecution(execution)}, nil
		}
	}
	return Ticket{}, fmt.Errorf("%w: execution is not recorded", ErrUnavailable)
}

func (s Store) Check(ctx context.Context, ticket Ticket) error {
	object, state, err := s.read(ctx)
	if err != nil {
		return err
	}
	if ticket.AuthorityUID == object.UID {
		for _, execution := range state.Executions {
			if equality.Semantic.DeepEqual(execution, ticket.Execution) {
				return ctx.Err()
			}
		}
	}
	return ErrConflict
}

func (s Store) CheckChange(ctx context.Context, ticket ChangeTicket) error {
	object, state, err := s.read(ctx)
	if err != nil {
		return err
	}
	if ticket.AuthorityUID == object.UID && slices.ContainsFunc(state.Changes, func(change Change) bool {
		return equality.Semantic.DeepEqual(change, ticket.Change)
	}) {
		return ctx.Err()
	}
	return ErrConflict
}

// BeginChange records submission intent before the participating API writer
// acts. After an uncertain write, only outcome observation is allowed.
func (s Store) BeginChange(ctx context.Context, ticket ChangeTicket) (ChangeTicket, error) {
	if ticket.Change.Phase != Reserved {
		return ChangeTicket{}, ErrConflict
	}
	object, state, err := s.read(ctx)
	if err != nil {
		return ChangeTicket{}, err
	}
	if object.UID != ticket.AuthorityUID {
		return ChangeTicket{}, ErrConflict
	}
	for i, change := range state.Changes {
		if equality.Semantic.DeepEqual(change, ticket.Change) {
			state.Changes[i].Phase = Changing
			if err := s.write(ctx, object, state); err != nil {
				return ChangeTicket{}, fmt.Errorf("%w: configuration submission intent outcome is uncertain: %w", ErrUnavailable, err)
			}
			ticket.Change.Phase = Changing
			if err := s.CheckChange(ctx, ticket); err != nil {
				return ChangeTicket{}, fmt.Errorf("%w: configuration submission intent could not be confirmed: %w", ErrUnavailable, err)
			}
			return ticket, nil
		}
	}
	return ChangeTicket{}, ErrConflict
}

// BeginStart is not retryable. Only the caller whose durable transition and
// read-back succeeded may submit Start once. A recovered Starting record must
// be observed or stopped, never submitted again, even if no Pod is visible.
func (s Store) BeginStart(ctx context.Context, ticket Ticket) (Ticket, error) {
	if ticket.Execution.Phase != Reserved {
		return Ticket{}, ErrConflict
	}
	return s.transition(ctx, ticket, Starting)
}

func (s Store) MarkRunning(ctx context.Context, ticket Ticket) (Ticket, error) {
	if ticket.Execution.Phase != Starting {
		return Ticket{}, ErrConflict
	}
	return s.transition(ctx, ticket, Running)
}

func (s Store) BeginStop(ctx context.Context, ticket Ticket) (Ticket, error) {
	if ticket.Execution.Phase == Stopping {
		return ticket, s.Check(ctx, ticket)
	}
	return s.transition(ctx, ticket, Stopping)
}

func (s Store) transition(ctx context.Context, ticket Ticket, next Phase) (Ticket, error) {
	object, state, err := s.read(ctx)
	if err != nil {
		return Ticket{}, err
	}
	if object.UID != ticket.AuthorityUID {
		return Ticket{}, ErrConflict
	}
	for i, execution := range state.Executions {
		if equality.Semantic.DeepEqual(execution, ticket.Execution) {
			state.Executions[i].Phase = next
			if err := s.write(ctx, object, state); err != nil {
				return Ticket{}, err
			}
			ticket.Execution.Phase = next
			if err := s.Check(ctx, ticket); err != nil {
				return Ticket{}, err
			}
			return ticket, nil
		}
	}
	return Ticket{}, ErrConflict
}

// ReleaseExecution must follow route withdrawal AND independently verified
// durable writer exclusion/storage release. Entering Stopping is not evidence.
func (s Store) ReleaseExecution(ctx context.Context, ticket Ticket) error {
	if ticket.Execution.Phase != Stopping {
		return ErrConflict
	}
	object, state, err := s.read(ctx)
	if err != nil {
		return err
	}
	if object.UID != ticket.AuthorityUID {
		return ErrConflict
	}
	for i, execution := range state.Executions {
		if equality.Semantic.DeepEqual(execution, ticket.Execution) {
			state.Executions = slices.Delete(state.Executions, i, i+1)
			return s.write(ctx, object, state)
		}
	}
	return ErrConflict
}

// ReleaseChange requires a definitive outcome, including exclusion of delayed
// submissions under this ticket. A timeout or a source GET is not such an
// outcome. The trusted mutation integration, not this store, supplies it.
func (s Store) ReleaseChange(ctx context.Context, ticket ChangeTicket) error {
	if ticket.Change.Phase != Changing {
		return ErrConflict
	}
	object, state, err := s.read(ctx)
	if err != nil {
		return err
	}
	if object.UID != ticket.AuthorityUID {
		return ErrConflict
	}
	for i, change := range state.Changes {
		if equality.Semantic.DeepEqual(change, ticket.Change) {
			state.Changes = slices.Delete(state.Changes, i, i+1)
			return s.write(ctx, object, state)
		}
	}
	return ErrConflict
}

func (s Store) read(ctx context.Context) (*corev1.ConfigMap, record, error) {
	var state record
	if err := ctx.Err(); err != nil {
		return nil, state, err
	}
	if s.APIReader == nil || len(validation.IsDNS1123Label(s.Namespace)) != 0 ||
		!identity(s.Identity) {
		return nil, state, fmt.Errorf("%w: authoritative reader and state identity are required", ErrUnavailable)
	}
	object := &corev1.ConfigMap{}
	if err := s.APIReader.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: s.Identity.Name}, object); err != nil {
		return nil, state, fmt.Errorf("%w: read authority: %w", ErrUnavailable, err)
	}
	if object.Name != s.Identity.Name || object.Namespace != s.Namespace ||
		object.UID != s.Identity.UID || object.ResourceVersion == "" || !object.DeletionTimestamp.IsZero() ||
		len(object.OwnerReferences) != 0 || !slices.Contains(object.Finalizers, Finalizer) ||
		len(object.Data) != 1 || len(object.BinaryData) != 0 {
		return nil, state, fmt.Errorf("%w: authority identity or protection changed", ErrUnavailable)
	}
	raw := object.Data[DataKey]
	if len(raw) == 0 || len(raw) > MaxBytes {
		return nil, state, fmt.Errorf("%w: authority exceeds its content bound", ErrUnavailable)
	}
	problems, err := strictjson.UnmarshalStrict([]byte(raw), &state)
	if err != nil || len(problems) != 0 {
		return nil, state, fmt.Errorf("%w: malformed, duplicate or unknown authority fields", ErrUnavailable)
	}
	if err := validateRecord(state); err != nil {
		return nil, state, err
	}
	whole, err := json.Marshal(object)
	if err != nil || len(whole) > MaxBytes {
		return nil, state, fmt.Errorf("%w: authority exceeds its object bound", ErrUnavailable)
	}
	return object, state, ctx.Err()
}

func (s Store) write(ctx context.Context, object *corev1.ConfigMap, state record) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.Client == nil {
		return fmt.Errorf("%w: a configured authority writer is required", ErrUnavailable)
	}
	if err := encode(object, state); err != nil {
		return err
	}
	// Never retry a conflict against a newer set of owners.
	return s.Client.Update(ctx, object)
}

func encode(object *corev1.ConfigMap, state record) error {
	if err := validateRecord(state); err != nil {
		return err
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	object.Data = map[string]string{DataKey: string(raw)}
	whole, err := json.Marshal(object)
	if err != nil || len(whole) > MaxBytes {
		return fmt.Errorf("%w: authority exceeds its object bound", ErrUnavailable)
	}
	return nil
}

func validateRecord(state record) error {
	if state.Version != 1 || state.Executions == nil || state.Changes == nil ||
		len(state.Executions)+len(state.Changes)+len(state.NativeReservations) > MaxEntries {
		return fmt.Errorf("%w: invalid version, inventory or entry bound", ErrUnavailable)
	}
	ids := map[string]bool{}
	for i, execution := range state.Executions {
		if !literal(execution.ID, 128) || ids[execution.ID] ||
			!slices.Contains([]Phase{Reserved, Starting, Running, Stopping}, execution.Phase) {
			return fmt.Errorf("%w: invalid execution identity or phase", ErrUnavailable)
		}
		ids[execution.ID] = true
		if err := validateIntent(execution.Intent); err != nil {
			return err
		}
		for _, other := range state.Executions[:i] {
			if other.Intent.Agent.UID == execution.Intent.Agent.UID {
				return fmt.Errorf("%w: duplicate Agent execution", ErrUnavailable)
			}
			if err := compatibleExecutions(other.Intent, execution.Intent); err != nil {
				return err
			}
		}
	}
	for i, native := range state.NativeReservations {
		if !literal(native.ID, 128) || ids[native.ID] || (native.Phase != Reserved && native.Phase != Stopping) {
			return fmt.Errorf("%w: invalid native reservation identity or phase", ErrUnavailable)
		}
		ids[native.ID] = true
		if err := native.Intent.Validate(); err != nil {
			return err
		}
		for _, other := range state.NativeReservations[:i] {
			if err := compatibleNativeIntents(other.Intent, native.Intent); err != nil {
				return err
			}
		}
		for _, execution := range state.Executions {
			if err := compatibleNativeExecution(native.Intent, execution.Intent); err != nil {
				return err
			}
		}
	}
	keys := map[string]bool{}
	for _, change := range state.Changes {
		key := sourceKey(change.Before)
		if !literal(change.ID, 128) || ids[change.ID] || !validSource(change.Before) ||
			!digest(change.DesiredDigest) || (change.BeforeContentDigest != "" && !digest(change.BeforeContentDigest)) || keys[key] ||
			(change.Phase != Reserved && change.Phase != Changing) {
			return fmt.Errorf("%w: invalid or duplicate source change", ErrUnavailable)
		}
		ids[change.ID], keys[key] = true, true
		if change.Team != nil {
			if change.Before.APIVersion != api.GroupVersion.String() || change.Before.Kind != "Team" ||
				change.Before.Namespace != "" || !digest(change.BeforeContentDigest) ||
				change.BeforeContentDigest == change.DesiredDigest || change.Team.Before.Validate() != nil || change.Team.After.Validate() != nil {
				return fmt.Errorf("%w: invalid scoped Team change", ErrUnavailable)
			}
		}
		if change.UnchangedDigest != "" &&
			(!digest(change.UnchangedDigest) || change.Team != nil ||
				change.Before.APIVersion != api.GroupVersion.String() || change.Before.Namespace != "" ||
				(change.Before.Kind != "Sandbox" && change.Before.Kind != "AgentPolicy")) {
			return fmt.Errorf("%w: invalid unchanged configuration digest", ErrUnavailable)
		}
		if change.DeletionContentDigest != "" {
			if err := validateConfigurationDeletion(change); err != nil {
				return err
			}
		} else if change.BeforeContentDigest != "" &&
			(change.Before.APIVersion != api.GroupVersion.String() || change.Before.Namespace != "" ||
				(change.Before.Kind != "Team" && change.Before.Kind != "Sandbox" && change.Before.Kind != "AgentPolicy") ||
				change.BeforeContentDigest == change.DesiredDigest) {
			return fmt.Errorf("%w: invalid configuration update recovery metadata", ErrUnavailable)
		}
		for _, execution := range state.Executions {
			if changeAffects(execution.Intent, change) {
				return fmt.Errorf("%w: source change overlaps an execution", ErrUnavailable)
			}
		}
		for _, native := range state.NativeReservations {
			if nativeChangeAffects(native.Intent, change) {
				return fmt.Errorf("%w: source change overlaps a native reservation", ErrUnavailable)
			}
		}
	}
	return nil
}

func validateIntent(intent Intent) error {
	if intent.RunningPreparationDigest != "" && !digest(intent.RunningPreparationDigest) {
		return fmt.Errorf("%w: running preparation digest is invalid", ErrUnavailable)
	}
	if !identity(intent.Agent) || !identity(intent.Namespace) || !identity(intent.Home) ||
		!identity(intent.Workload) || !digest(intent.WorkloadDigest) || !digest(intent.TeamSharedDigest) ||
		len(validation.IsDNS1123Label(intent.Namespace.Name)) != 0 ||
		intent.AgentGeneration < 1 || intent.Expected.Binding.AgentUID != intent.Agent.UID ||
		intent.Expected.Binding.TeamUID == "" || intent.Expected.Binding.SandboxUID == "" ||
		intent.Expected.Binding.AgentPolicyUID == "" ||
		!literal(intent.Expected.Revision, 256) || !literal(string(intent.Expected.PodUID), 128) ||
		!literal(string(intent.Expected.NodeUID), 128) || !literal(intent.Expected.EnforcementEpoch, 128) ||
		(intent.IngressMode != api.AgentPolicyIngressAuthenticated && intent.IngressMode != api.AgentPolicyIngressDisabled) {
		return fmt.Errorf("%w: complete independently enrolled execution identity is required", ErrUnavailable)
	}
	expected := w.Expectation{Agent: intent.Agent, Namespace: intent.Namespace, Home: intent.Home, Workload: intent.Workload}
	if err := intent.Placement.MatchesExpectation(expected); err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	if !slices.ContainsFunc(intent.Placement.Nodes, func(node w.PlacementNode) bool { return node.UID == intent.Expected.NodeUID }) {
		return fmt.Errorf("%w: expected Node is outside the lifetime placement", ErrUnavailable)
	}
	if !identity(intent.WriterJournal) || intent.WriterJournal.Name != sandboxwriter.JournalName(sandboxwriter.Lifetime{
		Agent: intent.Agent, Namespace: intent.Namespace, Home: intent.Home,
	}) || intent.Writer.Workload != intent.Workload ||
		intent.Writer.Pod.Name != intent.Workload.Name+"-0" || intent.Writer.Pod.UID != intent.Expected.PodUID ||
		intent.Writer.Node.UID != intent.Expected.NodeUID || !slices.Contains(intent.Placement.Nodes, intent.Writer.Node) ||
		intent.Writer.Epoch != intent.Expected.EnforcementEpoch || !literal(intent.Writer.MachineID, 256) ||
		!identity(intent.Writer.Storage.Volume) || !literal(intent.Writer.Storage.Driver, 253) ||
		!literal(intent.Writer.Storage.Handle, 2048) {
		return fmt.Errorf("%w: exact physical writer and enrolled journal identities are required", ErrUnavailable)
	}
	if intent.Preparation.APIVersion != "v1" || intent.Preparation.Kind != "ConfigMap" ||
		intent.Preparation.Namespace == "" || intent.Preparation.Namespace == intent.Namespace.Name ||
		!validSource(intent.Preparation) || len(intent.Sources) == 0 || len(intent.Sources) > MaxSources {
		return fmt.Errorf("%w: exact preparation state and bounded sources are required", ErrUnavailable)
	}
	keys := map[string]bool{}
	required := map[struct {
		version, kind, namespace string
		uid                      types.UID
	}]bool{
		{api.GroupVersion.String(), "Team", "", intent.Expected.Binding.TeamUID}:               false,
		{api.GroupVersion.String(), "Sandbox", "", intent.Expected.Binding.SandboxUID}:         false,
		{api.GroupVersion.String(), "AgentPolicy", "", intent.Expected.Binding.AgentPolicyUID}: false,
		{"v1", "Namespace", "", intent.Namespace.UID}:                                          false,
		{"v1", "PersistentVolumeClaim", intent.Namespace.Name, intent.Home.UID}:                false,
		{"apps/v1", "StatefulSet", intent.Namespace.Name, intent.Workload.UID}:                 false,
		{"v1", "ConfigMap", intent.Preparation.Namespace, intent.Placement.Registry.UID}:       false,
	}
	exactNames := map[struct {
		version, kind, namespace string
		uid                      types.UID
	}]string{
		{"v1", "Namespace", "", intent.Namespace.UID}:                                    intent.Namespace.Name,
		{"v1", "PersistentVolumeClaim", intent.Namespace.Name, intent.Home.UID}:          intent.Home.Name,
		{"apps/v1", "StatefulSet", intent.Namespace.Name, intent.Workload.UID}:           intent.Workload.Name,
		{"v1", "ConfigMap", intent.Preparation.Namespace, intent.Placement.Registry.UID}: intent.Placement.Registry.Name,
	}
	agentPinned, preparationPinned := false, false
	effectiveSources := 0
	for _, source := range intent.Sources {
		key := sourceKey(source)
		if !validSource(source) || keys[key] {
			return fmt.Errorf("%w: source identities must be complete and unique", ErrUnavailable)
		}
		keys[key] = true
		if w.EffectiveSource(source) {
			effectiveSources++
			if !digest(intent.SourceDigests[w.SourceKey(source)]) {
				return fmt.Errorf("%w: effective source digest is missing", ErrUnavailable)
			}
		}
		identity := struct {
			version, kind, namespace string
			uid                      types.UID
		}{source.APIVersion, source.Kind, source.Namespace, source.UID}
		if name, found := exactNames[identity]; found && source.Name != name {
			return fmt.Errorf("%w: source name differs from its enrolled identity", ErrUnavailable)
		}
		if _, found := required[identity]; found {
			required[identity] = true
		}
		preparationPinned = preparationPinned || source == intent.Preparation
		if source.IsAgent() {
			if source.Name != intent.Agent.Name || source.UID != intent.Agent.UID || source.Generation != intent.AgentGeneration {
				return fmt.Errorf("%w: Agent source differs from the execution", ErrUnavailable)
			}
			agentPinned = true
		}
	}
	if !agentPinned || !preparationPinned {
		return fmt.Errorf("%w: execution must pin Agent intent and preparation state", ErrUnavailable)
	}
	if len(intent.SourceDigests) != effectiveSources {
		return fmt.Errorf("%w: effective source digest inventory differs from sources", ErrUnavailable)
	}
	for _, present := range required {
		if !present {
			return fmt.Errorf("%w: required binding, placement or storage source is missing", ErrUnavailable)
		}
	}
	if access := intent.Access; access != nil {
		if intent.IngressMode != api.AgentPolicyIngressAuthenticated ||
			!identity(access.Route) || access.Route.Name != "cetirun-"+intent.Agent.Name ||
			len(validation.IsDNS1123Subdomain(access.Host)) != 0 || access.Host == "" ||
			access.Port < 1 || access.Port > 65535 ||
			access.Service.APIVersion != "v1" || access.Service.Kind != "Service" ||
			access.Service.Namespace != intent.Namespace.Name || access.Service.Name != intent.Workload.Name ||
			access.Gateway.APIVersion != "networking.istio.io/v1beta1" || access.Gateway.Kind != "Gateway" ||
			access.Gateway.Namespace == "" ||
			!slices.Contains(intent.Sources, access.Service) || !slices.Contains(intent.Sources, access.Gateway) {
			return fmt.Errorf("%w: access requires exact owned route and pinned Service/Gateway inputs", ErrUnavailable)
		}
		if len(access.IngressNamespaces) > MaxIngressNamespaces {
			return fmt.Errorf("%w: ingress namespace inventory exceeds its bound", ErrUnavailable)
		}
		for i, namespace := range access.IngressNamespaces {
			if namespace.APIVersion != "v1" || namespace.Kind != "Namespace" || namespace.Namespace != "" ||
				len(validation.IsDNS1123Label(namespace.Name)) != 0 ||
				!slices.Contains(intent.Sources, namespace) ||
				(i > 0 && access.IngressNamespaces[i-1].Name >= namespace.Name) {
				return fmt.Errorf("%w: access ingress namespaces must be uniquely ordered and pinned sources", ErrUnavailable)
			}
		}
	}
	return nil
}

func compatibleExecutions(a, b Intent) error {
	if a.Agent.Name == b.Agent.Name || a.Namespace.Name == b.Namespace.Name ||
		a.Namespace.UID == b.Namespace.UID || a.Home.UID == b.Home.UID ||
		a.Workload.UID == b.Workload.UID || a.Expected.PodUID == b.Expected.PodUID ||
		a.WriterJournal.UID == b.WriterJournal.UID || a.Writer.MachineID == b.Writer.MachineID ||
		a.Writer.Storage.Volume.UID == b.Writer.Storage.Volume.UID ||
		(a.Writer.Storage.Driver == b.Writer.Storage.Driver && a.Writer.Storage.Handle == b.Writer.Storage.Handle) {
		return ErrConflict
	}
	for _, left := range a.Placement.Nodes {
		for _, right := range b.Placement.Nodes {
			if left.Name == right.Name || left.UID == right.UID || left.ProviderID == right.ProviderID {
				return ErrConflict
			}
		}
	}
	pinned := make(map[string]w.SourceReference, len(a.Sources))
	for _, source := range a.Sources {
		pinned[sourceKey(source)] = source
	}
	for _, source := range b.Sources {
		old, found := pinned[sourceKey(source)]
		if !found {
			continue
		}
		if old.UID != source.UID {
			return ErrConflict
		}
		if source.APIVersion == api.GroupVersion.String() && source.Kind == "Team" {
			if a.TeamSharedDigest != b.TeamSharedDigest || a.Expected.Binding.AgentPolicyUID != b.Expected.Binding.AgentPolicyUID {
				return ErrConflict
			}
		} else if w.EffectiveSource(source) {
			if a.SourceDigests[w.SourceKey(old)] != b.SourceDigests[w.SourceKey(source)] {
				return ErrConflict
			}
		} else if old != source {
			return ErrConflict
		}
	}
	return nil
}

func cloneExecution(execution Execution) Execution {
	execution.Intent.Sources = slices.Clone(execution.Intent.Sources)
	execution.Intent.Placement = execution.Intent.Placement.DeepCopy()
	execution.Intent.SourceDigests = maps.Clone(execution.Intent.SourceDigests)
	if execution.Intent.Access != nil {
		access := *execution.Intent.Access
		access.IngressNamespaces = slices.Clone(access.IngressNamespaces)
		execution.Intent.Access = &access
	}
	return execution
}

func cloneChange(change Change) Change {
	if change.Team != nil {
		change.Team = &TeamChange{Before: change.Team.Before.DeepCopy(), After: change.Team.After.DeepCopy()}
	}
	return change
}

func changeAffects(intent Intent, change Change) bool {
	return selectedChangeAffects(intent.Sources, intent.SourceDigests, intent.SelectedSandbox(), intent.Expected.Binding, change)
}

func selectedChangeAffects(sources []w.SourceReference, digests map[string]string, box string, binding sandbox.Binding, change Change) bool {
	if !uses(sources, change.Before) {
		return false
	}
	if change.UnchangedDigest != "" {
		return digests[w.SourceKey(change.Before)] != change.UnchangedDigest ||
			!slices.ContainsFunc(sources, func(reference w.SourceReference) bool {
				return sourceKey(reference) == sourceKey(change.Before) && reference.UID == change.Before.UID
			})
	}
	if change.Team == nil || change.Before.UID != binding.TeamUID {
		return true
	}
	pinned := digests[w.SourceKey(change.Before)]
	for _, inputs := range []w.TeamInputs{change.Team.Before, change.Team.After} {
		digest, err := inputs.DigestFor(box)
		if err != nil || digest != pinned {
			return true
		}
	}
	return false
}
func sourceKey(source w.SourceReference) string {
	return strings.Join([]string{source.APIVersion, source.Kind, source.Namespace, source.Name}, "\x00")
}

func validSource(source w.SourceReference) bool {
	version, err := schema.ParseGroupVersion(source.APIVersion)
	return err == nil && version.Version != "" && literal(source.APIVersion, 253) &&
		literal(source.Kind, 128) && literal(source.Name, 253) &&
		len(pathvalidation.IsValidPathSegmentName(source.Name)) == 0 &&
		(source.Namespace == "" || len(validation.IsDNS1123Label(source.Namespace)) == 0) &&
		literal(string(source.UID), 128) && source.ValidIdentity() &&
		(source.IsAgent() || literal(source.ResourceVersion, 256))
}

func uses(sources []w.SourceReference, target w.SourceReference) bool {
	return slices.ContainsFunc(sources, func(source w.SourceReference) bool { return sourceKey(source) == sourceKey(target) })
}

func identity(value w.ObjectIdentity) bool {
	return len(validation.IsDNS1123Subdomain(value.Name)) == 0 && literal(string(value.UID), 128)
}

// ValidateAuthorityIdentity validates deployment configuration without reading
// or creating the authority. Its server-issued UID must be supplied explicitly.
func ValidateAuthorityIdentity(value w.ObjectIdentity) error {
	if !identity(value) {
		return fmt.Errorf("%w: a valid coordination ConfigMap name and exact bounded UID are required", ErrUnavailable)
	}
	return nil
}

func literal(value string, limit int) bool {
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

func digest(value string) bool {
	raw, err := hex.DecodeString(value)
	return err == nil && len(raw) == 32 && hex.EncodeToString(raw) == value
}

// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxcoordination

import (
	"context"
	"crypto/rand"
	"fmt"
	"maps"
	"slices"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/util/retry"
)

// NativeIntent pins selected configuration for a Kubernetes lifetime.
// Reconciled lifetimes name their logical workload without claiming a Pod or
// physical writer identity. Neither form is an enrolled execution or fence.
type NativeIntent struct {
	Reconciled      bool                `json:"reconciled,omitempty"`
	Agent           w.ObjectIdentity    `json:"agent"`
	AgentGeneration int64               `json:"agentGeneration"`
	Namespace       w.ObjectIdentity    `json:"namespace"`
	Home            w.ObjectIdentity    `json:"home"`
	Workload        w.ObjectIdentity    `json:"workload"`
	Binding         sandbox.Binding     `json:"binding"`
	Revision        string              `json:"revision"`
	Sources         []w.SourceReference `json:"sources"`
	SourceDigests   map[string]string   `json:"sourceDigests"`
}

type NativeReservation struct {
	ID     string       `json:"id"`
	Intent NativeIntent `json:"intent"`
	Phase  Phase        `json:"phase"`
}

type NativeTicket struct {
	AuthorityUID types.UID
	Reservation  NativeReservation
}

func (ticket NativeTicket) DeepCopy() NativeTicket {
	ticket.Reservation.Intent.Sources = slices.Clone(ticket.Reservation.Intent.Sources)
	ticket.Reservation.Intent.SourceDigests = maps.Clone(ticket.Reservation.Intent.SourceDigests)
	return ticket
}

// NativeIntentFor selects only the bound Team, Sandbox and effective policy,
// even if held preparation also inspected unused Team slots.
func NativeIntentFor(expected w.Expectation) (NativeIntent, error) {
	return nativeIntentFor(expected, false)
}

// ReconciledIntentFor reserves shared configuration for an owned HOME lifetime,
// not for one Pod or writer. Normal template updates retain that reservation.
func ReconciledIntentFor(expected w.ReconcileExpectation) (NativeIntent, error) {
	if err := expected.Validate(); err != nil {
		return NativeIntent{}, err
	}
	var workload w.ObjectIdentity
	for _, component := range expected.Workloads {
		if component.Kind == "StatefulSet" {
			workload.Name = component.Identity.Name
		}
	}
	return nativeIntentFor(w.Expectation{
		Version: w.ExpectationVersion, Agent: expected.Agent, AgentGeneration: expected.AgentGeneration,
		Namespace: expected.Namespace, Home: expected.Home, Workload: workload, WorkloadKind: w.AgentWorkload,
		Binding: expected.Binding, Revision: expected.Revision, Sources: expected.Sources, SourceDigests: expected.SourceDigests,
	}, true)
}

func nativeIntentFor(expected w.Expectation, reconciled bool) (NativeIntent, error) {
	intent := NativeIntent{
		Reconciled: reconciled,
		Agent:      expected.Agent, AgentGeneration: expected.AgentGeneration,
		Namespace: expected.Namespace, Home: expected.Home, Workload: expected.Workload,
		Binding: expected.Binding, Revision: expected.Revision, SourceDigests: map[string]string{},
	}
	if expected.Version != w.ExpectationVersion || expected.WorkloadKind != w.AgentWorkload ||
		len(expected.Sources) > w.MaxPreparationSources {
		return NativeIntent{}, fmt.Errorf("%w: a bounded native workload expectation is required", ErrUnavailable)
	}
	for _, selected := range []struct {
		kind string
		uid  types.UID
	}{{"Team", expected.Binding.TeamUID}, {"Sandbox", expected.Binding.SandboxUID}, {"AgentPolicy", expected.Binding.AgentPolicyUID}} {
		found := false
		for _, source := range expected.Sources {
			if source.APIVersion != api.GroupVersion.String() || source.Kind != selected.kind ||
				source.Namespace != "" || source.UID != selected.uid {
				continue
			}
			if found {
				return NativeIntent{}, fmt.Errorf("%w: duplicate selected native configuration", ErrUnavailable)
			}
			found = true
			intent.Sources = append(intent.Sources, source)
			intent.SourceDigests[w.SourceKey(source)] = expected.SourceDigests[w.SourceKey(source)]
		}
		if !found {
			return NativeIntent{}, fmt.Errorf("%w: selected native configuration is missing", ErrUnavailable)
		}
	}
	return intent, intent.Validate()
}

func (intent NativeIntent) Validate() error {
	workloadIdentity := identity(intent.Workload)
	if intent.Reconciled {
		workloadIdentity = intent.Workload.UID == "" && len(validation.IsDNS1123Subdomain(intent.Workload.Name)) == 0
	}
	if !identity(intent.Agent) || intent.AgentGeneration < 1 || !identity(intent.Namespace) ||
		len(validation.IsDNS1123Label(intent.Namespace.Name)) != 0 ||
		!identity(intent.Home) || !workloadIdentity || !literal(intent.Revision, 256) ||
		intent.Binding.AgentUID != intent.Agent.UID ||
		len(intent.Sources) != 3 || len(intent.SourceDigests) != 3 {
		return fmt.Errorf("%w: exact native lifetime and three selected configurations are required", ErrUnavailable)
	}
	for i, selected := range []struct {
		kind string
		uid  types.UID
	}{{"Team", intent.Binding.TeamUID}, {"Sandbox", intent.Binding.SandboxUID}, {"AgentPolicy", intent.Binding.AgentPolicyUID}} {
		source := intent.Sources[i]
		if !validSource(source) || source.APIVersion != api.GroupVersion.String() ||
			source.Kind != selected.kind || source.Namespace != "" || source.UID != selected.uid ||
			!digest(intent.SourceDigests[w.SourceKey(source)]) {
			return fmt.Errorf("%w: native configuration selection or digest is invalid", ErrUnavailable)
		}
	}
	return nil
}

func (intent NativeIntent) SelectedSandbox() string {
	if len(intent.Sources) != 3 {
		return ""
	}
	return intent.Sources[1].Name
}

// AcquireNative reserves before any start submission or admission release.
// The controller must recheck current selected inputs after acquiring: a
// configuration writer may have completed since preparation captured them.
func (s Store) AcquireNative(ctx context.Context, intent NativeIntent) (NativeTicket, error) {
	var ticket NativeTicket
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var err error
		ticket, err = s.acquireNative(ctx, intent)
		return err
	})
	return ticket, err
}

func (s Store) acquireNative(ctx context.Context, intent NativeIntent) (NativeTicket, error) {
	if err := intent.Validate(); err != nil {
		return NativeTicket{}, err
	}
	if intent.Namespace.Name == s.Namespace {
		return NativeTicket{}, ErrConflict
	}
	object, state, err := s.read(ctx)
	if err != nil {
		return NativeTicket{}, err
	}
	for index, native := range state.NativeReservations {
		if native.Intent.Agent.UID == intent.Agent.UID {
			if native.Phase == Reserved && equality.Semantic.DeepEqual(native.Intent, intent) {
				return (NativeTicket{object.UID, native}).DeepCopy(), nil
			}
			if native.Phase == Reserved && sameReconciledSources(native.Intent, intent) {
				state.NativeReservations[index].Intent = intent
				if err := s.write(ctx, object, state); err != nil {
					return NativeTicket{}, err
				}
				ticket := (NativeTicket{object.UID, state.NativeReservations[index]}).DeepCopy()
				return ticket, s.CheckNative(ctx, ticket)
			}
			return NativeTicket{}, ErrConflict
		}
		if err := compatibleNativeIntents(native.Intent, intent); err != nil {
			return NativeTicket{}, err
		}
	}
	for _, execution := range state.Executions {
		if err := compatibleNativeExecution(intent, execution.Intent); err != nil {
			return NativeTicket{}, err
		}
	}
	for _, change := range state.Changes {
		if nativeChangeAffects(intent, change) {
			return NativeTicket{}, ErrConflict
		}
	}
	native := NativeReservation{ID: rand.Text(), Intent: intent, Phase: Reserved}
	state.NativeReservations = append(state.NativeReservations, native)
	if err := s.write(ctx, object, state); err != nil {
		return NativeTicket{}, err
	}
	ticket := (NativeTicket{object.UID, native}).DeepCopy()
	return ticket, s.CheckNative(ctx, ticket)
}

func sameReconciledSources(previous, desired NativeIntent) bool {
	if !previous.Reconciled || !desired.Reconciled || desired.AgentGeneration < previous.AgentGeneration ||
		previous.Agent != desired.Agent || previous.Namespace != desired.Namespace || previous.Home != desired.Home ||
		previous.Workload != desired.Workload || previous.Binding != desired.Binding ||
		!maps.Equal(previous.SourceDigests, desired.SourceDigests) || len(previous.Sources) != len(desired.Sources) {
		return false
	}
	for index, source := range previous.Sources {
		current := desired.Sources[index]
		if w.SourceKey(source) != w.SourceKey(current) || source.UID != current.UID {
			return false
		}
	}
	return true
}

func (s Store) ReadNative(ctx context.Context, agentUID types.UID) (NativeTicket, bool, error) {
	if !literal(string(agentUID), 128) {
		return NativeTicket{}, false, fmt.Errorf("%w: native Agent UID is required", ErrUnavailable)
	}
	object, state, err := s.read(ctx)
	if err != nil {
		return NativeTicket{}, false, err
	}
	for _, native := range state.NativeReservations {
		if native.Intent.Agent.UID == agentUID {
			return (NativeTicket{object.UID, native}).DeepCopy(), true, nil
		}
	}
	return NativeTicket{}, false, nil
}

// AuthorizeNativeStart is read-only and also applies at final Pod admission.
// Missing, stale and stopping reservations never authorize a start.
func (s Store) AuthorizeNativeStart(ctx context.Context, intent NativeIntent) error {
	if err := intent.Validate(); err != nil {
		return err
	}
	ticket, found, err := s.ReadNative(ctx, intent.Agent.UID)
	if err != nil {
		return err
	}
	if !found || ticket.Reservation.Phase != Reserved || !equality.Semantic.DeepEqual(ticket.Reservation.Intent, intent) {
		return fmt.Errorf("%w: current native source reservation is required", ErrConflict)
	}
	return ctx.Err()
}

func (s Store) CheckNative(ctx context.Context, ticket NativeTicket) error {
	current, found, err := s.ReadNative(ctx, ticket.Reservation.Intent.Agent.UID)
	if err != nil {
		return err
	}
	if !found || !equality.Semantic.DeepEqual(current, ticket) {
		return ErrConflict
	}
	return ctx.Err()
}

func (s Store) BeginNativeStop(ctx context.Context, ticket NativeTicket) (NativeTicket, error) {
	ticket = ticket.DeepCopy()
	if ticket.Reservation.Phase == Stopping {
		return ticket, s.CheckNative(ctx, ticket)
	}
	if ticket.Reservation.Phase != Reserved {
		return NativeTicket{}, ErrConflict
	}
	object, state, err := s.read(ctx)
	if err != nil {
		return NativeTicket{}, err
	}
	if object.UID != ticket.AuthorityUID {
		return NativeTicket{}, ErrConflict
	}
	for i, native := range state.NativeReservations {
		if equality.Semantic.DeepEqual(native, ticket.Reservation) {
			state.NativeReservations[i].Phase = Stopping
			if err := s.write(ctx, object, state); err != nil {
				return NativeTicket{}, err
			}
			ticket.Reservation.Phase = Stopping
			return ticket, s.CheckNative(ctx, ticket)
		}
	}
	return NativeTicket{}, ErrConflict
}

// ReleaseNative follows disabled startup/Pod admission and observed ordinary
// Kubernetes quiescence. Stopping alone is insufficient. This does not supply
// physical fencing, permit native replay, or release a qualified execution.
func (s Store) ReleaseNative(ctx context.Context, ticket NativeTicket) error {
	if ticket.Reservation.Phase != Stopping {
		return ErrConflict
	}
	object, state, err := s.read(ctx)
	if err != nil {
		return err
	}
	if object.UID != ticket.AuthorityUID {
		return ErrConflict
	}
	for i, native := range state.NativeReservations {
		if equality.Semantic.DeepEqual(native, ticket.Reservation) {
			state.NativeReservations = slices.Delete(state.NativeReservations, i, i+1)
			return s.write(ctx, object, state)
		}
	}
	return ErrConflict
}

func nativeChangeAffects(intent NativeIntent, change Change) bool {
	return selectedChangeAffects(intent.Sources, intent.SourceDigests, intent.SelectedSandbox(), intent.Binding, change)
}

func compatibleNativeIntents(a, b NativeIntent) error {
	if a.Agent.Name == b.Agent.Name || a.Agent.UID == b.Agent.UID ||
		a.Namespace.Name == b.Namespace.Name || a.Namespace.UID == b.Namespace.UID ||
		a.Home.UID == b.Home.UID || (a.Workload.UID != "" && a.Workload.UID == b.Workload.UID) {
		return ErrConflict
	}
	return compatibleNativeSources(a, b.Sources, b.SourceDigests, b.SelectedSandbox())
}

func compatibleNativeExecution(native NativeIntent, execution Intent) error {
	if native.Agent.Name == execution.Agent.Name || native.Agent.UID == execution.Agent.UID ||
		native.Namespace.Name == execution.Namespace.Name || native.Namespace.UID == execution.Namespace.UID ||
		native.Home.UID == execution.Home.UID || (native.Workload.UID != "" && native.Workload.UID == execution.Workload.UID) {
		return ErrConflict
	}
	return compatibleNativeSources(native, execution.Sources, execution.SourceDigests, execution.SelectedSandbox())
}

func compatibleNativeSources(native NativeIntent, sources []w.SourceReference, digests map[string]string, box string) error {
	for _, selected := range native.Sources {
		for _, source := range sources {
			if sourceKey(selected) != sourceKey(source) {
				continue
			}
			if selected.UID != source.UID {
				return ErrConflict
			}
			if source.Kind == "Team" && native.SelectedSandbox() != box {
				continue
			}
			if native.SourceDigests[w.SourceKey(selected)] != digests[w.SourceKey(source)] {
				return ErrConflict
			}
		}
	}
	return nil
}

// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package sandboxwriter journals possible writers before execution and
// orchestrates independently qualified, guest-external durable exclusion.
//
// Enrollment asserts complete birth coverage of a lifetime, not discovered
// inventory. It must precede every possible writer, including helpers. Neither
// enrollment, reservation nor a quiescent result grants permission to start.
// A separate serialized start/change authority must hold the start barrier,
// reserve before any possible execution, and coordinate all subsequent starts,
// placement/storage changes and cleanup with this journal.
//
// The journal lives in a separately protected controller namespace. Its write
// access, enrollment authority and the provider must be trusted independently;
// this package cannot detect privileged rewriting or qualify physical fencing.
// History is never reconstructed, truncated, released or automatically adopted.
package sandboxwriter

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"k8s.io/apimachinery/pkg/types"
)

const (
	JournalVersion            = "v1"
	JournalFinalizer          = "sandbox.cetirun.microsoft.com/writer-journal"
	JournalDataKey            = "writers.json"
	JournalReleasedAnnotation = "sandbox.cetirun.microsoft.com/writer-journal-released"
	MaxWriters                = 128
	MaxJournalBytes           = 512 * 1024
	MaxObservationAge         = 5 * time.Minute
)

var (
	ErrUnavailable    = errors.New("Sandbox writer-exclusion dependency unavailable")
	ErrInvalidHistory = errors.New("invalid Sandbox writer history")
	ErrUnproven       = errors.New("durable Sandbox writer exclusion is unproven")
)

type Lifetime struct {
	Agent     sandboxworkload.ObjectIdentity `json:"agent"`
	Namespace sandboxworkload.ObjectIdentity `json:"namespace"`
	Home      sandboxworkload.ObjectIdentity `json:"home"`
}

type StorageIdentity struct {
	Volume sandboxworkload.ObjectIdentity `json:"volume"`
	Driver string                         `json:"driver"`
	Handle string                         `json:"handle"`
}

type Writer struct {
	Workload sandboxworkload.ObjectIdentity `json:"workload"`
	Pod      sandboxworkload.ObjectIdentity `json:"pod"`
	Node     sandboxworkload.PlacementNode  `json:"node"`
	// MachineID identifies an immutable physical incarnation from trusted
	// enrollment, never a reusable Node provider resource name.
	MachineID string          `json:"machineID"`
	Epoch     string          `json:"epoch"`
	Storage   StorageIdentity `json:"storage"`
}

type Phase string

const (
	PhaseEnrolled  Phase = "enrolled"
	PhaseReserved  Phase = "reserved"
	PhaseRevoking  Phase = "revoking"
	PhaseQuiescent Phase = "quiescent"
)

// Snapshot pins the full history and phase to one authoritative journal read.
// In PhaseReserved the last entry is the sole newly reserved possible writer.
// All earlier entries remain in every revocation and observation request.
type Snapshot struct {
	UID             types.UID
	ResourceVersion string
	Version         string
	Lifetime        Lifetime
	EnrollmentID    string
	Writers         []Writer
	Phase           Phase
	OperationID     string
}

func (s Snapshot) DeepCopy() Snapshot {
	s.Writers = slices.Clone(s.Writers)
	return s
}

// Request stays identical across the revoking/quiescent phase change. The
// operation ID is durable before submission; a resource version is not part of
// the provider request because that phase change advances it.
type Request struct {
	JournalUID   types.UID
	OperationID  string
	EnrollmentID string
	Lifetime     Lifetime
	Writers      []Writer
}

func (r Request) DeepCopy() Request {
	r.Writers = slices.Clone(r.Writers)
	return r
}

// Provider must support unreachable nodes and verify immutable machine and
// storage incarnations. Revoke installs a durable guest-external start barrier
// for the entire enrolled lifetime and revokes every recorded writer. Observe
// is read-only: it must never resubmit an operation or infer exclusion from
// resource absence, CR status, node readiness, or point-in-time CRI stops.
//
// An ambiguous submission is only observed thereafter, even across restarts.
// A crash after intent persistence but before submission can therefore require
// independently authorized recovery; automatic destructive replay is forbidden.
type Provider interface {
	Revoke(context.Context, Request) error
	Observe(context.Context, Request) (Observation, error)
}

type HistoryCoverage string
type BarrierMethod string
type ExclusionMethod string
type ReleaseState string

const (
	CompleteHistory           HistoryCoverage = "complete-enrolled-history"
	DurableStartBarrier       BarrierMethod   = "durable-guest-external-start-barrier"
	RuntimeRevocation         ExclusionMethod = "restart-proof-runtime-revocation"
	InfrastructureDestruction ExclusionMethod = "verified-infrastructure-destruction"
	Released                  ReleaseState    = "released"
)

type BarrierEvidence struct {
	ID     string
	Method BarrierMethod
}

type WriterEvidence struct {
	Writer     Writer
	Method     ExclusionMethod
	EvidenceID string
}

type StorageRelease struct {
	Storage    StorageIdentity
	State      ReleaseState
	EvidenceID string
}

type HomeRelease struct {
	Home       sandboxworkload.ObjectIdentity
	State      ReleaseState
	EvidenceID string
	Volumes    []StorageRelease
}

// Observation is an assertion from the qualified provider, not Kubernetes
// status. ObservedAt is the fresh verification time of the entire proof,
// including the still-installed barrier, every writer and every storage
// release. Home is required even for an enrolled lifetime with no writers.
type Observation struct {
	Request    Request
	ObservedAt time.Time
	Coverage   HistoryCoverage
	Barrier    BarrierEvidence
	Writers    []WriterEvidence
	Home       HomeRelease
}

// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package sandboxguard implements a standalone, trusted-node CRI prototype.
// Its observations are not writer exclusion or authorization to start a workload.
package sandboxguard

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/util/validation"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

const (
	DefaultTimeout  = 15 * time.Second
	MaxTimeout      = time.Minute
	MaxReceiptBytes = 2048
	maxResponseSize = 4 << 20
)

type Action string

const (
	ActionInspect Action = "inspect"
	ActionStop    Action = "stop"
)

type Status string

const (
	StatusObservedReady   Status = "observed_ready"
	StatusObservedStopped Status = "observed_stopped"
	StatusUnknown         Status = "unknown"
	StatusRejected        Status = "rejected"
	StatusError           Status = "error"
)

type Reason string

const (
	ReasonObservedReady      Reason = "observed_ready"
	ReasonObservedStopped    Reason = "observed_stopped_at_that_moment"
	ReasonInvalidInput       Reason = "invalid_input"
	ReasonConnectionFailed   Reason = "connection_failed"
	ReasonListFailed         Reason = "sandbox_list_failed"
	ReasonStatusFailed       Reason = "sandbox_status_failed"
	ReasonStopFailed         Reason = "sandbox_stop_failed"
	ReasonContainersFailed   Reason = "container_list_failed"
	ReasonCancelled          Reason = "cancelled"
	ReasonDeadline           Reason = "deadline_exceeded"
	ReasonInvalidResponse    Reason = "invalid_cri_response"
	ReasonSandboxAbsent      Reason = "sandbox_absent"
	ReasonIdentityMismatch   Reason = "identity_mismatch"
	ReasonHandlerMismatch    Reason = "runtime_handler_mismatch"
	ReasonAmbiguousSandbox   Reason = "ambiguous_sandbox"
	ReasonReplacementSandbox Reason = "replacement_sandbox"
	ReasonSandboxChanged     Reason = "sandbox_changed"
	ReasonSandboxState       Reason = "sandbox_state_unknown"
	ReasonStillReady         Reason = "sandbox_still_ready"
	ReasonContainerMismatch  Reason = "container_sandbox_mismatch"
	ReasonActiveContainers   Reason = "active_containers"
	ReasonContainerState     Reason = "container_state_unknown"
)

type Target struct {
	Namespace      string `json:"namespace"`
	PodName        string `json:"podName"`
	PodUID         string `json:"podUID"`
	SandboxID      string `json:"sandboxID"`
	RuntimeHandler string `json:"runtimeHandler"`
}

type Options struct {
	SocketPath string
	Target     Target
	Action     Action
	Timeout    time.Duration
}

type Receipt struct {
	Version                int     `json:"version"`
	Action                 Action  `json:"action"`
	Status                 Status  `json:"status"`
	Reason                 Reason  `json:"reason"`
	Target                 *Target `json:"target,omitempty"`
	ObservedAt             string  `json:"observedAt,omitempty"`
	StopAttempted          bool    `json:"stopAttempted"`
	ObservationScope       string  `json:"observationScope"`
	DurableWriterExclusion bool    `json:"durableWriterExclusion"`
	PreStartAuthorization  bool    `json:"preStartAuthorization"`
	CompleteEnforcement    bool    `json:"completeEnforcement"`
	ProductionEligible     bool    `json:"productionEligible"`
}

type Failure struct {
	Status Status
	Reason Reason
}

func (f *Failure) Error() string {
	return "sandbox node guard: " + string(f.Reason)
}

// InvalidInputReceipt never includes unvalidated command-line values.
func InvalidInputReceipt() Receipt {
	return Receipt{
		Version: 1, Action: ActionInspect, Status: StatusRejected,
		Reason: ReasonInvalidInput, ObservationScope: "point_in_time_only",
	}
}

// Run uses one deadline for all connection and CRI operations and invokes stop
// at most once. A stopped receipt records only this call's observations.
func Run(ctx context.Context, opts Options) (Receipt, error) {
	receipt := InvalidInputReceipt()
	if opts.Action == ActionStop {
		receipt.Action = ActionStop
	}
	fail := func(f *Failure) (Receipt, error) {
		receipt.Status, receipt.Reason = f.Status, f.Reason
		return receipt, f
	}
	if !validOptions(opts) {
		return fail(&Failure{StatusRejected, ReasonInvalidInput})
	}
	target := opts.Target
	receipt.Target = &opts.Target
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	if f := callFailure(ctx, nil, ReasonConnectionFailed); f != nil {
		return fail(f)
	}

	// The target never goes to a DNS resolver or proxy. Even if gRPC supplies a
	// different address, the dialer can connect only to this local Unix socket.
	conn, err := grpc.NewClient("passthrough:///sandbox-node-guard",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(dialCtx context.Context, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(dialCtx, "unix", opts.SocketPath)
		}),
		grpc.WithNoProxy(),
		grpc.WithDisableServiceConfig(),
		grpc.WithDisableRetry(),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(maxResponseSize),
			grpc.MaxCallSendMsgSize(MaxReceiptBytes),
			grpc.MaxRetryRPCBufferSize(0),
		),
	)
	if err != nil {
		return fail(&Failure{StatusError, ReasonConnectionFailed})
	}
	defer conn.Close()
	client := runtimeapi.NewRuntimeServiceClient(conn)

	first, f := readSnapshot(ctx, client, target, nil)
	if f != nil {
		return fail(f)
	}
	target.SandboxID = first.sandboxID
	current, f := readSnapshot(ctx, client, target, &first)
	if f != nil {
		return fail(f)
	}
	if opts.Action == ActionStop {
		if f := callFailure(ctx, nil, ReasonStopFailed); f != nil {
			return fail(f)
		}
		receipt.StopAttempted = true
		_, err := client.StopPodSandbox(ctx, &runtimeapi.StopPodSandboxRequest{PodSandboxId: target.SandboxID})
		if f := callFailure(ctx, err, ReasonStopFailed); f != nil {
			return fail(f)
		}
	} else if current.state == runtimeapi.PodSandboxState_SANDBOX_READY {
		if f := callFailure(ctx, nil, ReasonStatusFailed); f != nil {
			return fail(f)
		}
		receipt.Status, receipt.Reason = StatusObservedReady, ReasonObservedReady
		receipt.Target = &target
		receipt.ObservedAt = time.Now().UTC().Format(time.RFC3339Nano)
		return receipt, nil
	}

	// A successful StopPodSandbox RPC is not evidence that the sandbox stopped.
	// Bracket the container observation with fresh complete sandbox inventories
	// and exact status checks, including after an inspect of a NOTREADY sandbox.
	for pass := 0; pass < 2; pass++ {
		current, f = readSnapshot(ctx, client, target, &first)
		if f != nil {
			return fail(f)
		}
		if current.state != runtimeapi.PodSandboxState_SANDBOX_NOTREADY {
			return fail(&Failure{StatusRejected, ReasonStillReady})
		}
		if pass == 0 {
			if f := checkContainers(ctx, client, target); f != nil {
				return fail(f)
			}
		}
	}
	if f := callFailure(ctx, nil, ReasonStatusFailed); f != nil {
		return fail(f)
	}
	receipt.Status, receipt.Reason = StatusObservedStopped, ReasonObservedStopped
	receipt.Target = &target
	receipt.ObservedAt = time.Now().UTC().Format(time.RFC3339Nano)
	return receipt, nil
}

type snapshot struct {
	sandboxID string
	attempt   uint32
	createdAt int64
	state     runtimeapi.PodSandboxState
}

func readSnapshot(ctx context.Context, client runtimeapi.RuntimeServiceClient, target Target, previous *snapshot) (snapshot, *Failure) {
	list, err := client.ListPodSandbox(ctx, &runtimeapi.ListPodSandboxRequest{})
	if f := callFailure(ctx, err, ReasonListFailed); f != nil {
		return snapshot{}, f
	}
	if list == nil {
		return snapshot{}, &Failure{StatusUnknown, ReasonInvalidResponse}
	}
	var match *runtimeapi.PodSandbox
	for _, item := range list.Items {
		if item == nil || item.Metadata == nil || item.Id == "" ||
			item.Metadata.Namespace == "" || item.Metadata.Name == "" || item.Metadata.Uid == "" {
			return snapshot{}, &Failure{StatusUnknown, ReasonInvalidResponse}
		}
		metadata := item.Metadata
		related := item.Id == target.SandboxID || metadata.Uid == target.PodUID ||
			(metadata.Namespace == target.Namespace && metadata.Name == target.PodName)
		if !related {
			continue
		}
		if !matchesMetadata(metadata, target) {
			return snapshot{}, &Failure{StatusRejected, ReasonIdentityMismatch}
		}
		if target.SandboxID != "" && item.Id != target.SandboxID {
			return snapshot{}, &Failure{StatusRejected, ReasonReplacementSandbox}
		}
		if item.RuntimeHandler != target.RuntimeHandler {
			return snapshot{}, &Failure{StatusRejected, ReasonHandlerMismatch}
		}
		if match != nil {
			return snapshot{}, &Failure{StatusRejected, ReasonAmbiguousSandbox}
		}
		match = item
	}
	if match == nil {
		return snapshot{}, &Failure{StatusUnknown, ReasonSandboxAbsent}
	}
	if !validID(match.Id) || match.CreatedAt <= 0 {
		return snapshot{}, &Failure{StatusUnknown, ReasonInvalidResponse}
	}
	if !knownSandboxState(match.State) {
		return snapshot{}, &Failure{StatusUnknown, ReasonSandboxState}
	}
	if previous != nil && (match.CreatedAt != previous.createdAt || match.Metadata.Attempt != previous.attempt) {
		return snapshot{}, &Failure{StatusRejected, ReasonSandboxChanged}
	}

	target.SandboxID = match.Id
	response, err := client.PodSandboxStatus(ctx, &runtimeapi.PodSandboxStatusRequest{
		PodSandboxId: target.SandboxID, Verbose: false,
	})
	if f := callFailure(ctx, err, ReasonStatusFailed); f != nil {
		return snapshot{}, f
	}
	if response == nil || response.Status == nil || response.Status.Metadata == nil {
		return snapshot{}, &Failure{StatusUnknown, ReasonInvalidResponse}
	}
	observed := response.Status
	if observed.Id != target.SandboxID || !matchesMetadata(observed.Metadata, target) {
		return snapshot{}, &Failure{StatusRejected, ReasonIdentityMismatch}
	}
	if observed.RuntimeHandler != target.RuntimeHandler {
		return snapshot{}, &Failure{StatusRejected, ReasonHandlerMismatch}
	}
	if !knownSandboxState(observed.State) {
		return snapshot{}, &Failure{StatusUnknown, ReasonSandboxState}
	}
	if observed.CreatedAt != match.CreatedAt || observed.Metadata.Attempt != match.Metadata.Attempt ||
		observed.State != match.State {
		return snapshot{}, &Failure{StatusRejected, ReasonSandboxChanged}
	}
	return snapshot{sandboxID: match.Id, attempt: match.Metadata.Attempt, createdAt: match.CreatedAt, state: observed.State}, nil
}

func checkContainers(ctx context.Context, client runtimeapi.RuntimeServiceClient, target Target) *Failure {
	response, err := client.ListContainers(ctx, &runtimeapi.ListContainersRequest{
		Filter: &runtimeapi.ContainerFilter{PodSandboxId: target.SandboxID},
	})
	if f := callFailure(ctx, err, ReasonContainersFailed); f != nil {
		return f
	}
	if response == nil {
		return &Failure{StatusUnknown, ReasonInvalidResponse}
	}
	seen := make(map[string]bool, len(response.Containers))
	for _, container := range response.Containers {
		if container == nil || container.Id == "" || seen[container.Id] {
			return &Failure{StatusUnknown, ReasonInvalidResponse}
		}
		seen[container.Id] = true
		if container.PodSandboxId != target.SandboxID {
			return &Failure{StatusRejected, ReasonContainerMismatch}
		}
		switch container.State {
		case runtimeapi.ContainerState_CONTAINER_EXITED:
		case runtimeapi.ContainerState_CONTAINER_CREATED, runtimeapi.ContainerState_CONTAINER_RUNNING:
			return &Failure{StatusRejected, ReasonActiveContainers}
		default:
			return &Failure{StatusUnknown, ReasonContainerState}
		}
	}
	return nil
}

func callFailure(ctx context.Context, err error, reason Reason) *Failure {
	// Runtime errors may contain credentials or arbitrary workload payloads.
	// Only the fixed reason and selected gRPC status codes leave this package.
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || status.Code(err) == codes.DeadlineExceeded {
		return &Failure{StatusError, ReasonDeadline}
	}
	if errors.Is(ctx.Err(), context.Canceled) || status.Code(err) == codes.Canceled {
		return &Failure{StatusError, ReasonCancelled}
	}
	if err == nil {
		return nil
	}
	if status.Code(err) == codes.NotFound && (reason == ReasonStatusFailed || reason == ReasonStopFailed) {
		return &Failure{StatusUnknown, ReasonSandboxAbsent}
	}
	return &Failure{StatusError, reason}
}

func matchesMetadata(metadata *runtimeapi.PodSandboxMetadata, target Target) bool {
	return metadata != nil && metadata.Namespace == target.Namespace &&
		metadata.Name == target.PodName && metadata.Uid == target.PodUID
}

func knownSandboxState(state runtimeapi.PodSandboxState) bool {
	return state == runtimeapi.PodSandboxState_SANDBOX_READY || state == runtimeapi.PodSandboxState_SANDBOX_NOTREADY
}

func validOptions(opts Options) bool {
	if (opts.Action != ActionInspect && opts.Action != ActionStop) || opts.Timeout <= 0 || opts.Timeout > MaxTimeout {
		return false
	}
	path := opts.SocketPath
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" || len(path) > 107 {
		return false
	}
	for _, c := range path {
		if c < 33 || c > 126 {
			return false
		}
	}
	target := opts.Target
	return len(validation.IsDNS1123Label(target.Namespace)) == 0 &&
		len(validation.IsDNS1123Subdomain(target.PodName)) == 0 &&
		validID(target.PodUID) &&
		(validID(target.SandboxID) || opts.Action == ActionInspect && target.SandboxID == "") &&
		(target.RuntimeHandler == "" || len(validation.IsDNS1123Label(target.RuntimeHandler)) == 0)
}

func validID(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for i, c := range value {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			continue
		}
		if i == 0 || c != '-' && c != '_' && c != '.' {
			return false
		}
	}
	return true
}

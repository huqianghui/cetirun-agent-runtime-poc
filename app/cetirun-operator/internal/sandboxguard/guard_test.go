// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxguard

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

const opaquePayload = "fixture-payload-sk-dummy-do-not-echo"

type runtimeFixture struct {
	runtimeapi.UnimplementedRuntimeServiceServer
	mu             sync.Mutex
	sandboxes      []*runtimeapi.PodSandbox
	sandboxStatus  *runtimeapi.PodSandboxStatus
	containers     []*runtimeapi.Container
	calls          map[string]int
	hooks          map[string]func(context.Context, *runtimeFixture, int) error
	stops          []string
	keepReady      bool
	keepContainers bool
	emptyStatus    bool
	badRequest     bool
}

func fixtureTarget() Target {
	return Target{
		Namespace: "sandbox-example", PodName: "guard-example",
		PodUID:    "00000000-0000-4000-8000-000000000001",
		SandboxID: strings.Repeat("a", 64), RuntimeHandler: "runc",
	}
}

func newRuntimeFixture() *runtimeFixture {
	target := fixtureTarget()
	sandbox := &runtimeapi.PodSandbox{
		Id: target.SandboxID,
		Metadata: &runtimeapi.PodSandboxMetadata{
			Namespace: target.Namespace, Name: target.PodName, Uid: target.PodUID, Attempt: 1,
		},
		RuntimeHandler: target.RuntimeHandler,
		CreatedAt:      1_700_000_000_000_000_000,
		State:          runtimeapi.PodSandboxState_SANDBOX_READY,
		Labels:         map[string]string{"example.com/fixture": opaquePayload},
	}
	return &runtimeFixture{
		sandboxes: []*runtimeapi.PodSandbox{sandbox},
		sandboxStatus: &runtimeapi.PodSandboxStatus{
			Id: sandbox.Id, Metadata: proto.Clone(sandbox.Metadata).(*runtimeapi.PodSandboxMetadata),
			RuntimeHandler: sandbox.RuntimeHandler, CreatedAt: sandbox.CreatedAt, State: sandbox.State,
			Annotations: map[string]string{"example.com/fixture": opaquePayload},
		},
		containers: []*runtimeapi.Container{{
			Id: "container-example", PodSandboxId: target.SandboxID,
			State: runtimeapi.ContainerState_CONTAINER_RUNNING,
		}},
		calls: map[string]int{},
		hooks: map[string]func(context.Context, *runtimeFixture, int) error{},
	}
}

func (f *runtimeFixture) before(ctx context.Context, method string) error {
	f.calls[method]++
	if hook := f.hooks[method]; hook != nil {
		return hook(ctx, f, f.calls[method])
	}
	return nil
}

func (f *runtimeFixture) ListPodSandbox(ctx context.Context, req *runtimeapi.ListPodSandboxRequest) (*runtimeapi.ListPodSandboxResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if req.Filter != nil {
		f.badRequest = true
	}
	if err := f.before(ctx, "list"); err != nil {
		return nil, err
	}
	response := &runtimeapi.ListPodSandboxResponse{Items: f.sandboxes}
	return proto.Clone(response).(*runtimeapi.ListPodSandboxResponse), nil
}

func (f *runtimeFixture) PodSandboxStatus(ctx context.Context, req *runtimeapi.PodSandboxStatusRequest) (*runtimeapi.PodSandboxStatusResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if req.PodSandboxId != fixtureTarget().SandboxID || req.Verbose {
		f.badRequest = true
	}
	if err := f.before(ctx, "status"); err != nil {
		return nil, err
	}
	if f.emptyStatus {
		return &runtimeapi.PodSandboxStatusResponse{}, nil
	}
	if f.sandboxStatus == nil {
		return nil, status.Error(codes.NotFound, opaquePayload)
	}
	response := &runtimeapi.PodSandboxStatusResponse{
		Status: f.sandboxStatus, Info: map[string]string{"fixture": opaquePayload},
	}
	return proto.Clone(response).(*runtimeapi.PodSandboxStatusResponse), nil
}

func (f *runtimeFixture) StopPodSandbox(ctx context.Context, req *runtimeapi.StopPodSandboxRequest) (*runtimeapi.StopPodSandboxResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops = append(f.stops, req.PodSandboxId)
	if err := f.before(ctx, "stop"); err != nil {
		return nil, err
	}
	if !f.keepReady {
		for _, sandbox := range f.sandboxes {
			if sandbox.Id == req.PodSandboxId {
				sandbox.State = runtimeapi.PodSandboxState_SANDBOX_NOTREADY
			}
		}
		f.sandboxStatus.State = runtimeapi.PodSandboxState_SANDBOX_NOTREADY
	}
	if !f.keepContainers {
		for _, container := range f.containers {
			if container != nil && container.PodSandboxId == req.PodSandboxId {
				container.State = runtimeapi.ContainerState_CONTAINER_EXITED
			}
		}
	}
	return &runtimeapi.StopPodSandboxResponse{}, nil
}

func (f *runtimeFixture) ListContainers(ctx context.Context, req *runtimeapi.ListContainersRequest) (*runtimeapi.ListContainersResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if req.Filter == nil || req.Filter.PodSandboxId != fixtureTarget().SandboxID || req.Filter.State != nil {
		f.badRequest = true
	}
	if err := f.before(ctx, "containers"); err != nil {
		return nil, err
	}
	return proto.Clone(&runtimeapi.ListContainersResponse{Containers: f.containers}).(*runtimeapi.ListContainersResponse), nil
}

func serveFixture(t *testing.T, f *runtimeFixture) string {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "cri.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	runtimeapi.RegisterRuntimeServiceServer(server, f)
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		if err := <-done; err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			t.Errorf("fixture server: %v", err)
		}
	})
	return socket
}

func fixtureOptions(socket string) Options {
	return Options{SocketPath: socket, Target: fixtureTarget(), Action: ActionStop, Timeout: 5 * time.Second}
}

func afterList(number int, mutate func(*runtimeFixture)) func(context.Context, *runtimeFixture, int) error {
	return func(_ context.Context, f *runtimeFixture, call int) error {
		if call == number {
			mutate(f)
		}
		return nil
	}
}

func addReplacement(f *runtimeFixture) {
	replacement := proto.Clone(f.sandboxes[0]).(*runtimeapi.PodSandbox)
	replacement.Id = strings.Repeat("b", 64)
	replacement.State = runtimeapi.PodSandboxState_SANDBOX_READY
	f.sandboxes = append(f.sandboxes, replacement)
}

func assertReceipt(t *testing.T, receipt Receipt, err error, wantStatus Status, wantReason Reason) {
	t.Helper()
	if receipt.Status != wantStatus || receipt.Reason != wantReason {
		t.Fatalf("receipt = %+v, error = %v; want %s / %s", receipt, err, wantStatus, wantReason)
	}
	success := wantStatus == StatusObservedStopped || wantStatus == StatusObservedReady
	if (err == nil) != success {
		t.Fatalf("error = %v for status %s", err, receipt.Status)
	}
	if success {
		if _, err := time.Parse(time.RFC3339Nano, receipt.ObservedAt); err != nil {
			t.Fatalf("missing observation timestamp: %v", err)
		}
	} else if receipt.ObservedAt != "" {
		t.Fatal("failed operation has an observation timestamp")
	}
	if receipt.Version != 1 || receipt.ObservationScope != "point_in_time_only" ||
		receipt.DurableWriterExclusion || receipt.PreStartAuthorization ||
		receipt.CompleteEnforcement || receipt.ProductionEligible {
		t.Fatalf("overstated receipt: %+v", receipt)
	}
	data, marshalErr := json.Marshal(receipt)
	if marshalErr != nil || len(data)+1 > MaxReceiptBytes {
		t.Fatalf("unbounded or invalid receipt: %d bytes, %v", len(data), marshalErr)
	}
	if strings.Contains(string(data), opaquePayload) || err != nil && strings.Contains(err.Error(), opaquePayload) {
		t.Fatal("runtime payload escaped into output")
	}
}

func TestRun(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(*runtimeFixture, *Options)
		status Status
		reason Reason
		stops  int
	}{
		{"stop", func(*runtimeFixture, *Options) {}, StatusObservedStopped, ReasonObservedStopped, 1},
		{"inspect ready", func(_ *runtimeFixture, o *Options) { o.Action = ActionInspect }, StatusObservedReady, ReasonObservedReady, 0},
		{"inspect stopped", func(f *runtimeFixture, o *Options) {
			o.Action = ActionInspect
			f.sandboxes[0].State = runtimeapi.PodSandboxState_SANDBOX_NOTREADY
			f.sandboxStatus.State = runtimeapi.PodSandboxState_SANDBOX_NOTREADY
			f.containers[0].State = runtimeapi.ContainerState_CONTAINER_EXITED
		}, StatusObservedStopped, ReasonObservedStopped, 0},
		{"empty containers", func(f *runtimeFixture, _ *Options) { f.containers = nil }, StatusObservedStopped, ReasonObservedStopped, 1},
		{"empty expected handler", func(f *runtimeFixture, o *Options) {
			o.Target.RuntimeHandler = ""
			f.sandboxes[0].RuntimeHandler = ""
			f.sandboxStatus.RuntimeHandler = ""
		}, StatusObservedStopped, ReasonObservedStopped, 1},
		{"already absent", func(f *runtimeFixture, _ *Options) { f.sandboxes = nil }, StatusUnknown, ReasonSandboxAbsent, 0},
		{"missing status", func(f *runtimeFixture, _ *Options) { f.sandboxStatus = nil }, StatusUnknown, ReasonSandboxAbsent, 0},
		{"empty status", func(f *runtimeFixture, _ *Options) { f.emptyStatus = true }, StatusUnknown, ReasonInvalidResponse, 0},
		{"absent after stop", func(f *runtimeFixture, _ *Options) {
			f.hooks["list"] = afterList(3, func(f *runtimeFixture) { f.sandboxes = nil })
		}, StatusUnknown, ReasonSandboxAbsent, 1},
		{"initial replacement", func(f *runtimeFixture, _ *Options) { addReplacement(f) }, StatusRejected, ReasonReplacementSandbox, 0},
		{"old nonready match", func(f *runtimeFixture, _ *Options) {
			addReplacement(f)
			f.sandboxes[1].State = runtimeapi.PodSandboxState_SANDBOX_NOTREADY
		}, StatusRejected, ReasonReplacementSandbox, 0},
		{"replacement before stop", func(f *runtimeFixture, _ *Options) {
			f.hooks["list"] = afterList(2, addReplacement)
		}, StatusRejected, ReasonReplacementSandbox, 0},
		{"replacement after stop", func(f *runtimeFixture, _ *Options) {
			f.hooks["list"] = afterList(3, addReplacement)
		}, StatusRejected, ReasonReplacementSandbox, 1},
		{"final replacement", func(f *runtimeFixture, _ *Options) {
			f.hooks["list"] = afterList(4, addReplacement)
		}, StatusRejected, ReasonReplacementSandbox, 1},
		{"new pod same name", func(f *runtimeFixture, _ *Options) {
			f.sandboxes[0].Metadata.Uid = "different-uid"
		}, StatusRejected, ReasonIdentityMismatch, 0},
		{"uid different namespace", func(f *runtimeFixture, _ *Options) {
			f.sandboxes[0].Metadata.Namespace = "different-namespace"
		}, StatusRejected, ReasonIdentityMismatch, 0},
		{"uid different name", func(f *runtimeFixture, _ *Options) {
			f.sandboxes[0].Metadata.Name = "different-name"
		}, StatusRejected, ReasonIdentityMismatch, 0},
		{"duplicate id", func(f *runtimeFixture, _ *Options) {
			f.sandboxes = append(f.sandboxes, proto.Clone(f.sandboxes[0]).(*runtimeapi.PodSandbox))
		}, StatusRejected, ReasonAmbiguousSandbox, 0},
		{"id prefix", func(_ *runtimeFixture, o *Options) {
			o.Target.SandboxID = o.Target.SandboxID[:12]
		}, StatusRejected, ReasonReplacementSandbox, 0},
		{"handler mismatch", func(f *runtimeFixture, _ *Options) {
			f.sandboxes[0].RuntimeHandler = "other"
		}, StatusRejected, ReasonHandlerMismatch, 0},
		{"handler missing", func(f *runtimeFixture, _ *Options) {
			f.sandboxes[0].RuntimeHandler = ""
		}, StatusRejected, ReasonHandlerMismatch, 0},
		{"status handler mismatch", func(f *runtimeFixture, _ *Options) {
			f.sandboxStatus.RuntimeHandler = "other"
		}, StatusRejected, ReasonHandlerMismatch, 0},
		{"status id mismatch", func(f *runtimeFixture, _ *Options) {
			f.sandboxStatus.Id = strings.Repeat("b", 64)
		}, StatusRejected, ReasonIdentityMismatch, 0},
		{"status namespace mismatch", func(f *runtimeFixture, _ *Options) {
			f.sandboxStatus.Metadata.Namespace = "different-namespace"
		}, StatusRejected, ReasonIdentityMismatch, 0},
		{"status name mismatch", func(f *runtimeFixture, _ *Options) {
			f.sandboxStatus.Metadata.Name = "different-name"
		}, StatusRejected, ReasonIdentityMismatch, 0},
		{"status uid mismatch", func(f *runtimeFixture, _ *Options) {
			f.sandboxStatus.Metadata.Uid = "different-uid"
		}, StatusRejected, ReasonIdentityMismatch, 0},
		{"attempt changed", func(f *runtimeFixture, _ *Options) {
			f.hooks["list"] = afterList(2, func(f *runtimeFixture) {
				f.sandboxes[0].Metadata.Attempt++
				f.sandboxStatus.Metadata.Attempt++
			})
		}, StatusRejected, ReasonSandboxChanged, 0},
		{"creation changed", func(f *runtimeFixture, _ *Options) {
			f.hooks["list"] = afterList(2, func(f *runtimeFixture) {
				f.sandboxes[0].CreatedAt++
				f.sandboxStatus.CreatedAt++
			})
		}, StatusRejected, ReasonSandboxChanged, 0},
		{"status attempt mismatch", func(f *runtimeFixture, _ *Options) {
			f.sandboxStatus.Metadata.Attempt++
		}, StatusRejected, ReasonSandboxChanged, 0},
		{"status creation mismatch", func(f *runtimeFixture, _ *Options) {
			f.sandboxStatus.CreatedAt++
		}, StatusRejected, ReasonSandboxChanged, 0},
		{"status state mismatch", func(f *runtimeFixture, _ *Options) {
			f.sandboxStatus.State = runtimeapi.PodSandboxState_SANDBOX_NOTREADY
		}, StatusRejected, ReasonSandboxChanged, 0},
		{"zero creation time", func(f *runtimeFixture, _ *Options) {
			f.sandboxes[0].CreatedAt = 0
		}, StatusUnknown, ReasonInvalidResponse, 0},
		{"missing metadata", func(f *runtimeFixture, _ *Options) {
			f.sandboxes[0].Metadata = nil
		}, StatusUnknown, ReasonInvalidResponse, 0},
		{"missing status metadata", func(f *runtimeFixture, _ *Options) {
			f.sandboxStatus.Metadata = nil
		}, StatusUnknown, ReasonInvalidResponse, 0},
		{"nil inventory record", func(f *runtimeFixture, _ *Options) {
			f.sandboxes = append(f.sandboxes, nil)
		}, StatusUnknown, ReasonInvalidResponse, 0},
		{"unknown sandbox state", func(f *runtimeFixture, _ *Options) {
			f.sandboxes[0].State = runtimeapi.PodSandboxState(99)
		}, StatusUnknown, ReasonSandboxState, 0},
		{"unknown status state", func(f *runtimeFixture, _ *Options) {
			f.sandboxStatus.State = runtimeapi.PodSandboxState(99)
		}, StatusUnknown, ReasonSandboxState, 0},
		{"false success stop", func(f *runtimeFixture, _ *Options) {
			f.keepReady = true
		}, StatusRejected, ReasonStillReady, 1},
		{"running container", func(f *runtimeFixture, _ *Options) {
			f.keepContainers = true
		}, StatusRejected, ReasonActiveContainers, 1},
		{"created container", func(f *runtimeFixture, _ *Options) {
			f.keepContainers = true
			f.containers[0].State = runtimeapi.ContainerState_CONTAINER_CREATED
		}, StatusRejected, ReasonActiveContainers, 1},
		{"unknown container", func(f *runtimeFixture, _ *Options) {
			f.keepContainers = true
			f.containers[0].State = runtimeapi.ContainerState_CONTAINER_UNKNOWN
		}, StatusUnknown, ReasonContainerState, 1},
		{"invalid container state", func(f *runtimeFixture, _ *Options) {
			f.keepContainers = true
			f.containers[0].State = runtimeapi.ContainerState(99)
		}, StatusUnknown, ReasonContainerState, 1},
		{"wrong container sandbox", func(f *runtimeFixture, _ *Options) {
			f.containers[0].PodSandboxId = "different-sandbox"
		}, StatusRejected, ReasonContainerMismatch, 1},
		{"duplicate container", func(f *runtimeFixture, _ *Options) {
			f.containers = append(f.containers, proto.Clone(f.containers[0]).(*runtimeapi.Container))
		}, StatusUnknown, ReasonInvalidResponse, 1},
		{"nil container", func(f *runtimeFixture, _ *Options) {
			f.containers = append(f.containers, nil)
		}, StatusUnknown, ReasonInvalidResponse, 1},
		{"final ready state", func(f *runtimeFixture, _ *Options) {
			f.hooks["list"] = afterList(4, func(f *runtimeFixture) {
				f.sandboxes[0].State = runtimeapi.PodSandboxState_SANDBOX_READY
				f.sandboxStatus.State = runtimeapi.PodSandboxState_SANDBOX_READY
			})
		}, StatusRejected, ReasonStillReady, 1},
		{"final changed handler", func(f *runtimeFixture, _ *Options) {
			f.hooks["list"] = afterList(4, func(f *runtimeFixture) { f.sandboxes[0].RuntimeHandler = "other" })
		}, StatusRejected, ReasonHandlerMismatch, 1},
		{"unrelated sandbox", func(f *runtimeFixture, _ *Options) {
			unrelated := proto.Clone(f.sandboxes[0]).(*runtimeapi.PodSandbox)
			unrelated.Id, unrelated.Metadata.Namespace, unrelated.Metadata.Uid = "other", "other-namespace", "other-uid"
			f.sandboxes = append(f.sandboxes, unrelated)
		}, StatusObservedStopped, ReasonObservedStopped, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newRuntimeFixture()
			opts := fixtureOptions("")
			tc.setup(f, &opts)
			opts.SocketPath = serveFixture(t, f)
			receipt, err := Run(context.Background(), opts)
			assertReceipt(t, receipt, err, tc.status, tc.reason)
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.badRequest {
				t.Fatal("request was broad, verbose, or not an exact target request")
			}
			if len(f.stops) != tc.stops || receipt.StopAttempted != (tc.stops != 0) {
				t.Fatalf("stop requests = %v, attempted = %v", f.stops, receipt.StopAttempted)
			}
			for _, id := range f.stops {
				if id != opts.Target.SandboxID {
					t.Fatalf("stopped unexpected id %q", id)
				}
			}
		})
	}
}

func TestInspectDiscovery(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(*runtimeFixture)
		status Status
		reason Reason
	}{
		{"unique ready", func(*runtimeFixture) {}, StatusObservedReady, ReasonObservedReady},
		{"unique stopped", func(f *runtimeFixture) {
			f.sandboxes[0].State = runtimeapi.PodSandboxState_SANDBOX_NOTREADY
			f.sandboxStatus.State = runtimeapi.PodSandboxState_SANDBOX_NOTREADY
			f.containers[0].State = runtimeapi.ContainerState_CONTAINER_EXITED
		}, StatusObservedStopped, ReasonObservedStopped},
		{"absent", func(f *runtimeFixture) { f.sandboxes = nil }, StatusUnknown, ReasonSandboxAbsent},
		{"multiple ids", addReplacement, StatusRejected, ReasonAmbiguousSandbox},
		{"duplicate id", func(f *runtimeFixture) {
			f.sandboxes = append(f.sandboxes, proto.Clone(f.sandboxes[0]).(*runtimeapi.PodSandbox))
		}, StatusRejected, ReasonAmbiguousSandbox},
		{"wrong uid", func(f *runtimeFixture) {
			f.sandboxes[0].Metadata.Uid = "other-uid"
		}, StatusRejected, ReasonIdentityMismatch},
		{"wrong namespace", func(f *runtimeFixture) {
			f.sandboxes[0].Metadata.Namespace = "other-namespace"
		}, StatusRejected, ReasonIdentityMismatch},
		{"wrong name", func(f *runtimeFixture) {
			f.sandboxes[0].Metadata.Name = "other-name"
		}, StatusRejected, ReasonIdentityMismatch},
		{"wrong handler", func(f *runtimeFixture) {
			f.sandboxes[0].RuntimeHandler = "other"
		}, StatusRejected, ReasonHandlerMismatch},
		{"wrong status", func(f *runtimeFixture) {
			f.sandboxStatus.Metadata.Uid = "other-uid"
		}, StatusRejected, ReasonIdentityMismatch},
		{"invalid discovered id", func(f *runtimeFixture) {
			f.sandboxes[0].Id = opaquePayload + "\n"
		}, StatusUnknown, ReasonInvalidResponse},
		{"oversized discovered id", func(f *runtimeFixture) {
			f.sandboxes[0].Id = strings.Repeat("a", 129)
		}, StatusUnknown, ReasonInvalidResponse},
		{"replacement during discovery", func(f *runtimeFixture) {
			f.hooks["list"] = afterList(2, func(f *runtimeFixture) {
				f.sandboxes[0].Id = strings.Repeat("b", 64)
				f.sandboxStatus.Id = f.sandboxes[0].Id
			})
		}, StatusRejected, ReasonReplacementSandbox},
		{"id alias", func(f *runtimeFixture) {
			alias := proto.Clone(f.sandboxes[0]).(*runtimeapi.PodSandbox)
			alias.Metadata.Uid, alias.Metadata.Name = "other-uid", "other-name"
			f.sandboxes = append(f.sandboxes, alias)
		}, StatusRejected, ReasonIdentityMismatch},
		{"status error", func(f *runtimeFixture) {
			f.hooks["status"] = func(context.Context, *runtimeFixture, int) error {
				return status.Error(codes.Unavailable, opaquePayload)
			}
		}, StatusError, ReasonStatusFailed},
		{"recheck error", func(f *runtimeFixture) {
			f.hooks["list"] = func(_ context.Context, _ *runtimeFixture, count int) error {
				if count == 2 {
					return status.Error(codes.Unavailable, opaquePayload)
				}
				return nil
			}
		}, StatusError, ReasonListFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRuntimeFixture()
			tc.setup(f)
			opts := fixtureOptions(serveFixture(t, f))
			opts.Action, opts.Target.SandboxID = ActionInspect, ""
			receipt, err := Run(context.Background(), opts)
			assertReceipt(t, receipt, err, tc.status, tc.reason)
			wantID := ""
			if err == nil {
				wantID = fixtureTarget().SandboxID
			}
			if receipt.Target == nil || receipt.Target.SandboxID != wantID {
				t.Fatalf("discovered target = %+v, want id %q", receipt.Target, wantID)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.stops) != 0 || receipt.StopAttempted || f.badRequest {
				t.Fatalf("unsafe discovery requests: stops=%v, badRequest=%v", f.stops, f.badRequest)
			}
		})
	}
}

func TestRPCFailures(t *testing.T) {
	for _, method := range []string{"list", "status", "stop", "containers"} {
		for _, code := range []codes.Code{codes.Internal, codes.PermissionDenied, codes.Unavailable, codes.NotFound, codes.Canceled, codes.DeadlineExceeded} {
			t.Run(method+"/"+code.String(), func(t *testing.T) {
				f := newRuntimeFixture()
				f.hooks[method] = func(context.Context, *runtimeFixture, int) error {
					return status.Error(code, opaquePayload)
				}
				wantStatus := StatusError
				wantReason := map[string]Reason{
					"list": ReasonListFailed, "status": ReasonStatusFailed,
					"stop": ReasonStopFailed, "containers": ReasonContainersFailed,
				}[method]
				if code == codes.NotFound && (method == "status" || method == "stop") {
					wantStatus, wantReason = StatusUnknown, ReasonSandboxAbsent
				}
				if code == codes.Canceled {
					wantReason = ReasonCancelled
				}
				if code == codes.DeadlineExceeded {
					wantReason = ReasonDeadline
				}
				receipt, err := Run(context.Background(), fixtureOptions(serveFixture(t, f)))
				assertReceipt(t, receipt, err, wantStatus, wantReason)
				f.mu.Lock()
				defer f.mu.Unlock()
				if f.calls[method] != 1 {
					t.Fatalf("failed RPC was retried: %v", f.calls)
				}
				wantStops := 0
				if method == "stop" || method == "containers" {
					wantStops = 1
				}
				if len(f.stops) != wantStops {
					t.Fatalf("stop count = %d, want %d", len(f.stops), wantStops)
				}
			})
		}
	}
}

func TestPostStopReadFailures(t *testing.T) {
	for _, method := range []string{"list", "status"} {
		t.Run(method, func(t *testing.T) {
			f := newRuntimeFixture()
			f.hooks[method] = func(_ context.Context, _ *runtimeFixture, count int) error {
				if count == 3 {
					return status.Error(codes.Unavailable, opaquePayload)
				}
				return nil
			}
			want := ReasonListFailed
			if method == "status" {
				want = ReasonStatusFailed
			}
			receipt, err := Run(context.Background(), fixtureOptions(serveFixture(t, f)))
			assertReceipt(t, receipt, err, StatusError, want)
			if !receipt.StopAttempted {
				t.Fatal("post-stop error lost stop attempt")
			}
		})
	}
}

func TestCancellation(t *testing.T) {
	for _, stage := range []string{"before", "list", "stop", "containers"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f := newRuntimeFixture()
			if stage == "before" {
				cancel()
			} else {
				f.hooks[stage] = func(context.Context, *runtimeFixture, int) error {
					cancel()
					return nil
				}
			}
			receipt, err := Run(ctx, fixtureOptions(serveFixture(t, f)))
			assertReceipt(t, receipt, err, StatusError, ReasonCancelled)
			wantAttempt := stage == "stop" || stage == "containers"
			if receipt.StopAttempted != wantAttempt {
				t.Fatalf("stop attempted = %v, want %v", receipt.StopAttempted, wantAttempt)
			}
		})
	}
}

func TestTotalDeadline(t *testing.T) {
	f := newRuntimeFixture()
	delay := func(ctx context.Context, _ *runtimeFixture, _ int) error {
		timer := time.NewTimer(40 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		case <-timer.C:
			return nil
		}
	}
	f.hooks["list"], f.hooks["status"] = delay, delay
	opts := fixtureOptions(serveFixture(t, f))
	opts.Timeout = 100 * time.Millisecond
	started := time.Now()
	receipt, err := Run(context.Background(), opts)
	assertReceipt(t, receipt, err, StatusError, ReasonDeadline)
	if receipt.StopAttempted {
		t.Fatal("per-call deadlines allowed a stop beyond the total deadline")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("operation exceeded bounded deadline tolerance: %v", elapsed)
	}
}

func TestOversizedRuntimeResponse(t *testing.T) {
	f := newRuntimeFixture()
	f.sandboxes[0].Labels["example.com/fixture"] = strings.Repeat("x", maxResponseSize)
	receipt, err := Run(context.Background(), fixtureOptions(serveFixture(t, f)))
	assertReceipt(t, receipt, err, StatusError, ReasonListFailed)
	if receipt.StopAttempted {
		t.Fatal("stopped after a truncated inventory")
	}
}

func TestInvalidOptions(t *testing.T) {
	tests := map[string]func(*Options){
		"remote tcp":      func(o *Options) { o.SocketPath = "tcp://example.com:1234" },
		"remote http":     func(o *Options) { o.SocketPath = "http://example.com:1234" },
		"unix uri":        func(o *Options) { o.SocketPath = "unix:///run/containerd/containerd.sock" },
		"relative path":   func(o *Options) { o.SocketPath = "cri.sock" },
		"abstract socket": func(o *Options) { o.SocketPath = "\x00cri" },
		"unclean path":    func(o *Options) { o.SocketPath = "/run/../cri.sock" },
		"empty path":      func(o *Options) { o.SocketPath = "" },
		"root path":       func(o *Options) { o.SocketPath = "/" },
		"long path":       func(o *Options) { o.SocketPath = "/" + strings.Repeat("a", 107) },
		"non ascii path":  func(o *Options) { o.SocketPath = "/tmp/\u00e9.sock" },
		"missing name":    func(o *Options) { o.Target.PodName = "" },
		"bad namespace":   func(o *Options) { o.Target.Namespace = "NameSpace" },
		"missing uid":     func(o *Options) { o.Target.PodUID = "" },
		"missing id":      func(o *Options) { o.Target.SandboxID = "" },
		"long id":         func(o *Options) { o.Target.SandboxID = strings.Repeat("a", 129) },
		"bad uid":         func(o *Options) { o.Target.PodUID = opaquePayload + "\n" },
		"bad id":          func(o *Options) { o.Target.SandboxID = "../sandbox" },
		"bad handler":     func(o *Options) { o.Target.RuntimeHandler = "not/a/handler" },
		"zero timeout":    func(o *Options) { o.Timeout = 0 },
		"negative time":   func(o *Options) { o.Timeout = -time.Second },
		"long timeout":    func(o *Options) { o.Timeout = MaxTimeout + time.Nanosecond },
		"empty action":    func(o *Options) { o.Action = "" },
		"bad action":      func(o *Options) { o.Action = "Stop" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			f := newRuntimeFixture()
			opts := fixtureOptions(serveFixture(t, f))
			mutate(&opts)
			receipt, err := Run(context.Background(), opts)
			assertReceipt(t, receipt, err, StatusRejected, ReasonInvalidInput)
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.calls) != 0 || receipt.Target != nil {
				t.Fatal("invalid input reached CRI or was echoed")
			}
		})
	}
}

func TestMissingSocket(t *testing.T) {
	opts := fixtureOptions(filepath.Join(t.TempDir(), "missing.sock"))
	opts.Timeout = 100 * time.Millisecond
	receipt, err := Run(context.Background(), opts)
	if err == nil || receipt.Status != StatusError || receipt.StopAttempted {
		t.Fatalf("unreachable local runtime = %+v, %v", receipt, err)
	}
	if receipt.ObservedAt != "" {
		t.Fatal("unreachable runtime returned an observation")
	}
}

func TestMaximumReceiptSize(t *testing.T) {
	opts := fixtureOptions("/run/containerd/containerd.sock")
	opts.Target.Namespace = strings.Repeat("n", 63)
	opts.Target.PodName = strings.Repeat("p", 63) + "." + strings.Repeat("p", 63) + "." +
		strings.Repeat("p", 63) + "." + strings.Repeat("p", 61)
	opts.Target.PodUID = strings.Repeat("u", 128)
	opts.Target.SandboxID = strings.Repeat("i", 128)
	opts.Target.RuntimeHandler = strings.Repeat("h", 63)
	if !validOptions(opts) {
		t.Fatal("maximum-sized fixture should be valid")
	}
	receipt := InvalidInputReceipt()
	receipt.Action, receipt.Status, receipt.Reason = ActionInspect, StatusObservedStopped, ReasonObservedStopped
	receipt.Target = &opts.Target
	receipt.ObservedAt = "2026-01-01T00:00:00.123456789Z"
	assertReceipt(t, receipt, nil, StatusObservedStopped, ReasonObservedStopped)
}

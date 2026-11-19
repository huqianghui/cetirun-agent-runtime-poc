// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxguard"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

const cliSandboxID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type cliRuntime struct {
	runtimeapi.UnimplementedRuntimeServiceServer
	mu      sync.Mutex
	stopped bool
	calls   int
	stops   []string
	absent  bool
	fail    bool
	handler string
}

func (f *cliRuntime) sandbox() *runtimeapi.PodSandbox {
	state := runtimeapi.PodSandboxState_SANDBOX_READY
	if f.stopped {
		state = runtimeapi.PodSandboxState_SANDBOX_NOTREADY
	}
	return &runtimeapi.PodSandbox{
		Id: cliSandboxID, RuntimeHandler: f.handler, State: state, CreatedAt: 123,
		Metadata: &runtimeapi.PodSandboxMetadata{
			Namespace: "sandbox-example", Name: "guard-example", Uid: "example-pod-uid",
		},
	}
}

func (f *cliRuntime) ListPodSandbox(context.Context, *runtimeapi.ListPodSandboxRequest) (*runtimeapi.ListPodSandboxResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.fail {
		return nil, status.Error(codes.Internal, "do-not-echo-fixture-sk-dummy")
	}
	if f.absent {
		return &runtimeapi.ListPodSandboxResponse{}, nil
	}
	return &runtimeapi.ListPodSandboxResponse{Items: []*runtimeapi.PodSandbox{f.sandbox()}}, nil
}

func (f *cliRuntime) PodSandboxStatus(_ context.Context, req *runtimeapi.PodSandboxStatusRequest) (*runtimeapi.PodSandboxStatusResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if req.PodSandboxId != cliSandboxID || req.Verbose {
		return nil, status.Error(codes.InvalidArgument, "unexpected request")
	}
	sandbox := f.sandbox()
	return &runtimeapi.PodSandboxStatusResponse{
		Status: &runtimeapi.PodSandboxStatus{
			Id: sandbox.Id, Metadata: sandbox.Metadata, RuntimeHandler: sandbox.RuntimeHandler,
			State: sandbox.State, CreatedAt: sandbox.CreatedAt,
		},
		Info: map[string]string{"fixture": "do-not-echo-fixture-sk-dummy"},
	}, nil
}

func (f *cliRuntime) StopPodSandbox(_ context.Context, req *runtimeapi.StopPodSandboxRequest) (*runtimeapi.StopPodSandboxResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.stops = append(f.stops, req.PodSandboxId)
	f.stopped = true
	return &runtimeapi.StopPodSandboxResponse{}, nil
}

func (f *cliRuntime) ListContainers(_ context.Context, req *runtimeapi.ListContainersRequest) (*runtimeapi.ListContainersResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if req.Filter == nil || req.Filter.PodSandboxId != cliSandboxID {
		return nil, status.Error(codes.InvalidArgument, "unexpected request")
	}
	return &runtimeapi.ListContainersResponse{}, nil
}

func cliFixture(t *testing.T, f *cliRuntime) []string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cri.sock")
	listener, err := net.Listen("unix", path)
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
	return []string{
		"--socket=" + path, "--namespace=sandbox-example", "--pod-name=guard-example",
		"--pod-uid=example-pod-uid", "--sandbox-id=" + cliSandboxID, "--runtime-handler=" + f.handler,
	}
}

func readReceipt(t *testing.T, stdout, stderr *bytes.Buffer) sandboxguard.Receipt {
	t.Helper()
	if stderr.Len() != 0 || stdout.Len() > sandboxguard.MaxReceiptBytes || bytes.Count(stdout.Bytes(), []byte("\n")) != 1 {
		t.Fatalf("unexpected output: stdout = %q, stderr = %q", stdout, stderr)
	}
	if strings.Contains(stdout.String(), "sk-dummy") || strings.Contains(stdout.String(), "cri.sock") {
		t.Fatal("diagnostic payload or socket path escaped into output")
	}
	decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	decoder.DisallowUnknownFields()
	var receipt sandboxguard.Receipt
	if err := decoder.Decode(&receipt); err != nil {
		t.Fatal(err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatalf("extra output after receipt: %v", err)
	}
	if receipt.Version != 1 || receipt.ObservationScope != "point_in_time_only" ||
		receipt.DurableWriterExclusion || receipt.PreStartAuthorization ||
		receipt.CompleteEnforcement || receipt.ProductionEligible {
		t.Fatalf("overstated receipt: %+v", receipt)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(stdout.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"version", "action", "status", "reason", "stopAttempted", "observationScope",
		"durableWriterExclusion", "preStartAuthorization", "completeEnforcement", "productionEligible",
	}
	if receipt.Target != nil {
		want = append(want, "target")
	}
	if receipt.ObservedAt != "" {
		want = append(want, "observedAt")
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	slices.Sort(want)
	slices.Sort(keys)
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("receipt keys = %v, want %v", keys, want)
	}
	return receipt
}

func TestCLI(t *testing.T) {
	for _, tc := range []struct {
		name   string
		server *cliRuntime
		action string
		code   int
		status sandboxguard.Status
	}{
		{name: "inspect default", server: &cliRuntime{handler: "runc"}, status: sandboxguard.StatusObservedReady},
		{name: "stop", server: &cliRuntime{handler: "runc"}, action: "stop", status: sandboxguard.StatusObservedStopped},
		{name: "kata handler", server: &cliRuntime{handler: "kata"}, action: "stop", status: sandboxguard.StatusObservedStopped},
		{name: "empty handler", server: &cliRuntime{}, action: "stop", status: sandboxguard.StatusObservedStopped},
		{name: "already absent", server: &cliRuntime{absent: true}, action: "stop", code: 1, status: sandboxguard.StatusUnknown},
		{name: "runtime error", server: &cliRuntime{fail: true}, action: "stop", code: 1, status: sandboxguard.StatusError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := cliFixture(t, tc.server)
			if tc.action != "" {
				args = append(args, "--action="+tc.action)
			}
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), args, &stdout, &stderr)
			receipt := readReceipt(t, &stdout, &stderr)
			if code != tc.code || receipt.Status != tc.status {
				t.Fatalf("exit = %d, receipt = %+v; want exit %d, %s", code, receipt, tc.code, tc.status)
			}
			tc.server.mu.Lock()
			defer tc.server.mu.Unlock()
			if receipt.Status == sandboxguard.StatusObservedStopped {
				if receipt.Reason != sandboxguard.ReasonObservedStopped || !reflect.DeepEqual(tc.server.stops, []string{cliSandboxID}) {
					t.Fatalf("incorrect stopped receipt or target: %+v, %v", receipt, tc.server.stops)
				}
			} else if len(tc.server.stops) != 0 {
				t.Fatalf("unexpected stop: %v", tc.server.stops)
			}
		})
	}
}

func TestCLIInvalidArguments(t *testing.T) {
	for _, name := range []string{"missing handler", "unknown flag", "bad timeout", "bad action", "positional", "missing identity", "stop without id", "no args"} {
		t.Run(name, func(t *testing.T) {
			f := &cliRuntime{handler: "runc"}
			args := cliFixture(t, f)
			switch name {
			case "missing handler":
				args = args[:len(args)-1]
			case "unknown flag":
				args = append(args, "--do-not-echo=sk-dummy")
			case "bad timeout":
				args = append(args, "--timeout=sk-dummy")
			case "bad action":
				args = append(args, "--action=sk-dummy")
			case "positional":
				args = append(args, "sk-dummy")
			case "missing identity":
				args = append(args, "--pod-uid=")
			case "stop without id":
				args = slices.DeleteFunc(args, func(arg string) bool { return strings.HasPrefix(arg, "--sandbox-id=") })
				args = append(args, "--action=stop")
			case "no args":
				args = nil
			}
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), args, &stdout, &stderr)
			receipt := readReceipt(t, &stdout, &stderr)
			if code != 2 || receipt.Status != sandboxguard.StatusRejected ||
				receipt.Reason != sandboxguard.ReasonInvalidInput || receipt.Target != nil {
				t.Fatalf("invalid arguments: exit %d, %+v", code, receipt)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.calls != 0 {
				t.Fatal("invalid arguments reached CRI")
			}
		})
	}
}

func TestCLIDiscovery(t *testing.T) {
	f := &cliRuntime{handler: "kata"}
	args := cliFixture(t, f)
	args = slices.DeleteFunc(args, func(arg string) bool { return strings.HasPrefix(arg, "--sandbox-id=") })
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), args, &stdout, &stderr)
	receipt := readReceipt(t, &stdout, &stderr)
	if code != 0 || receipt.Status != sandboxguard.StatusObservedReady ||
		receipt.Target == nil || receipt.Target.SandboxID != cliSandboxID || receipt.StopAttempted {
		t.Fatalf("discovery exit = %d, receipt = %+v", code, receipt)
	}
	f.mu.Lock()
	if len(f.stops) != 0 {
		t.Fatal("discovery issued a stop")
	}
	f.mu.Unlock()

	stdout.Reset()
	stderr.Reset()
	args = append(args, "--action=stop", "--sandbox-id="+receipt.Target.SandboxID)
	code = run(context.Background(), args, &stdout, &stderr)
	receipt = readReceipt(t, &stdout, &stderr)
	if code != 0 || receipt.Status != sandboxguard.StatusObservedStopped || !receipt.StopAttempted {
		t.Fatalf("pinned stop exit = %d, receipt = %+v", code, receipt)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !reflect.DeepEqual(f.stops, []string{cliSandboxID}) {
		t.Fatalf("pinned stop target = %v", f.stops)
	}
}

func TestCLIHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("help exit = %d", code)
	}
	if stdout.String() != usage || stderr.Len() != 0 ||
		!strings.Contains(stdout.String(), "unreachable-node fencing") {
		t.Fatal("help omitted usage or safety boundary")
	}
}

type failingWriter struct {
	short bool
}

func (w failingWriter) Write([]byte) (int, error) {
	if w.short {
		return 0, nil
	}
	return 0, errors.New("do-not-echo-fixture-sk-dummy")
}

func TestOutputFailure(t *testing.T) {
	for _, short := range []bool{false, true} {
		var stderr bytes.Buffer
		if code := run(context.Background(), nil, failingWriter{short: short}, &stderr); code != 1 {
			t.Fatalf("output error exit = %d", code)
		}
		if stderr.String() != "sandbox-node-guard: receipt output failed\n" {
			t.Fatalf("unexpected error output: %q", stderr.String())
		}
	}
}

func TestReceiptBound(t *testing.T) {
	receipt := sandboxguard.InvalidInputReceipt()
	receipt.Target = &sandboxguard.Target{Namespace: strings.Repeat("x", sandboxguard.MaxReceiptBytes)}
	var stdout, stderr bytes.Buffer
	if code := emit(&stdout, &stderr, receipt, 0); code != 1 || stdout.Len() != 0 {
		t.Fatal("oversized receipt was emitted or returned success")
	}
	if stderr.String() != "sandbox-node-guard: receipt encoding failed\n" {
		t.Fatalf("unexpected error output: %q", stderr.String())
	}
}

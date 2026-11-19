// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxguard"
	"google.golang.org/grpc/grpclog"
)

const usage = `Usage: sandbox-node-guard --socket /run/containerd/containerd.sock \
  --namespace NAMESPACE --pod-name NAME --pod-uid UID [--sandbox-id FULL_ID] \
  --runtime-handler HANDLER [--action inspect|stop] [--timeout 15s]

Standalone trusted-node prototype. inspect is the default; --action stop is
the explicit destructive opt-in. Namespace, name, UID, handler and socket are
required. Inspect may omit --sandbox-id to discover a unique exact-identity
match; only a successful inspection returns its ID in target.sandboxID.
Stop always requires --sandbox-id and revalidates that exact ID.
Use --runtime-handler="" only when an empty CRI runtime handler is expected.
Only a clean absolute local Unix socket path is accepted. Timeout must be
positive and at most 1m, and covers all connection and CRI calls together.

Except for --help, stdout is one JSON receipt of at most 2048 bytes.
Exit codes: 0 = observation, 1 = unknown/rejected/failed operation,
2 = invalid arguments. observed_ready is not a stopped observation.

observed_stopped means observed stopped at that moment only. It is NOT durable
writer exclusion, pre-start authorization, or complete enforcement. Kubelet
may recreate the PodUID immediately afterward. Production enablement remains
blocked pending independent review and unreachable-node fencing.
`

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("sandbox-node-guard", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Usage = func() {}
	var opts sandboxguard.Options
	var action string
	flags.StringVar(&opts.SocketPath, "socket", "", "absolute local Unix CRI socket path")
	flags.StringVar(&opts.Target.Namespace, "namespace", "", "exact Pod namespace")
	flags.StringVar(&opts.Target.PodName, "pod-name", "", "exact Pod name")
	flags.StringVar(&opts.Target.PodUID, "pod-uid", "", "exact Pod UID")
	flags.StringVar(&opts.Target.SandboxID, "sandbox-id", "", "full exact CRI sandbox ID (required for stop)")
	flags.StringVar(&opts.Target.RuntimeHandler, "runtime-handler", "", "exact expected CRI runtime handler")
	flags.StringVar(&action, "action", string(sandboxguard.ActionInspect), "inspect or stop")
	flags.DurationVar(&opts.Timeout, "timeout", sandboxguard.DefaultTimeout, "total operation deadline")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			if _, err := io.WriteString(stdout, usage); err != nil {
				fmt.Fprintln(stderr, "sandbox-node-guard: output failed")
				return 1
			}
			return 0
		}
		return emit(stdout, stderr, sandboxguard.InvalidInputReceipt(), 2)
	}
	handlerSet := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "runtime-handler" {
			handlerSet = true
		}
	})
	if flags.NArg() != 0 || !handlerSet {
		return emit(stdout, stderr, sandboxguard.InvalidInputReceipt(), 2)
	}
	opts.Action = sandboxguard.Action(action)
	receipt, err := sandboxguard.Run(ctx, opts)
	exitCode := 0
	if err != nil {
		exitCode = 1
		if receipt.Reason == sandboxguard.ReasonInvalidInput {
			exitCode = 2
		}
	}
	return emit(stdout, stderr, receipt, exitCode)
}

func emit(stdout, stderr io.Writer, receipt sandboxguard.Receipt, exitCode int) int {
	data, err := json.Marshal(receipt)
	if err != nil || len(data)+1 > sandboxguard.MaxReceiptBytes {
		fmt.Fprintln(stderr, "sandbox-node-guard: receipt encoding failed")
		return 1
	}
	data = append(data, '\n')
	written, err := stdout.Write(data)
	if err != nil || written != len(data) {
		fmt.Fprintln(stderr, "sandbox-node-guard: receipt output failed")
		return 1
	}
	return exitCode
}

func main() {
	// All operation failures are surfaced by the bounded receipt, not gRPC logs.
	grpclog.SetLoggerV2(grpclog.NewLoggerV2(io.Discard, io.Discard, io.Discard))
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	exitCode := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	cancel()
	os.Exit(exitCode)
}

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
	"time"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxfence"
)

const usage = `Usage: sandbox-pool-fence --journal-dir ABSOLUTE_PRIVATE_DIRECTORY
  [--action inspect|reserve|register|test-condition|delete|observe] [--timeout 30s]

reserve additionally requires --pool-id ARM_ID --tenant-id UUID
  --cluster-resource-uid UID --run-id UUID.
Reserve BEFORE pool creation: an existing pool can never be adopted.
The parent creates ONE User-pool VM with autoscaling disabled and the pool tag
  cetirun-fence-run=RUN_ID, then register pins its exact ETag and Compute vmId.
test-condition and delete both require --confirm-delete-pool EXACT_POOL_ARM_ID.
test-condition itself sends DELETE with a nonmatching ETag; a broken service
could delete the entire disposable pool. Only a 412 and unchanged identities
permit the subsequent, single matching conditional DELETE.

inspect is the default. Azure Public ARM API 2026-05-01 is fixed.
Authentication is an explicit, noninteractive az account get-access-token
argument array with the registered subscription and exact ARM audience.
Returned tenant and subscription must both match the registration. --tenant-id
is the expected token tenant, not a second CLI token-request selector.
No login, credential fallback, provider registration, or DELETE replay occurs.

The Linux journal directory must be private (0700); files are 0600, locked,
fsynced and atomically replaced. Keep it permanently. Do not copy, reset or
remove pending intent to retry a deletion. Re-run observe, never delete.
delete returning pending/unknown is NOT permission to resubmit.

Exit 0 = successful read/local transition or completed observation;
exit 1 = pending, unknown, rejected or failed; exit 2 = invalid CLI syntax.
stdout contains a bounded JSON journal and an explicit error on failure.
No Agent workloads are permitted in this API-only experiment. Completion
proves neither pre-start/data-plane enforcement nor runtime writer exclusion.
This command is not a CheckQuiesced provider and grants no execution permit.
`

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("sandbox-pool-fence", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var options sandboxfence.Options
	var action string
	var timeout time.Duration
	flags.StringVar(&action, "action", string(sandboxfence.Inspect), "explicit action")
	flags.StringVar(&options.JournalDirectory, "journal-dir", "", "private journal directory")
	flags.StringVar(&options.Reservation.PoolID, "pool-id", "", "pool ARM ID for reservation")
	flags.StringVar(&options.Reservation.TenantID, "tenant-id", "", "expected token tenant UUID")
	flags.StringVar(&options.Reservation.ClusterResourceUID, "cluster-resource-uid", "", "cluster resourceUID")
	flags.StringVar(&options.Reservation.RunID, "run-id", "", "task-owned qualification run UUID")
	flags.StringVar(&options.ConfirmDeletePool, "confirm-delete-pool", "", "exact pool ARM ID destruction confirmation")
	flags.DurationVar(&timeout, "timeout", 30*time.Second, "overall request deadline, at most 5m")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			if _, err := io.WriteString(stdout, usage); err != nil {
				fmt.Fprintln(stderr, "sandbox-pool-fence: help output failed")
				return 1
			}
			return 0
		}
		fmt.Fprintln(stderr, "sandbox-pool-fence: invalid arguments; see --help")
		return 2
	}
	if flags.NArg() != 0 || timeout <= 0 || timeout > 5*time.Minute {
		fmt.Fprintln(stderr, "sandbox-pool-fence: no positional arguments; timeout must be positive and at most 5m")
		return 2
	}
	options.Action = sandboxfence.Action(action)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	journal, err := sandboxfence.Run(ctx, options)
	result := struct {
		Journal *sandboxfence.Journal `json:"journal,omitempty"`
		Error   string                `json:"error,omitempty"`
	}{Journal: journal}
	exitCode := 0
	if err != nil {
		result.Error = err.Error()
		exitCode = 1
	}
	data, marshalErr := json.Marshal(result)
	if marshalErr != nil || len(data)+1 > sandboxfence.MaxJournalBytes+4096 {
		fmt.Fprintln(stderr, "sandbox-pool-fence: bounded result encoding failed")
		return 1
	}
	if count, err := stdout.Write(append(data, '\n')); err != nil || count != len(data)+1 {
		fmt.Fprintln(stderr, "sandbox-pool-fence: result output failed; use the durable journal to observe, never resubmit")
		return 1
	}
	return exitCode
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	exitCode := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	cancel()
	os.Exit(exitCode)
}

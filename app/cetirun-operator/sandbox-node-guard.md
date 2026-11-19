# Standalone sandbox node guard prototype

**Production enablement is blocked pending independent review and
unreachable-node fencing.** This is a one-shot, trusted-node CRI experiment,
not a shipped controller integration, pre-start gate, or writer-exclusion
provider. It does not implement `CheckQuiesced`, release an execution hold, or
authorize a replacement writer.

The executable talks directly to the CRI v1 runtime service over a local Unix
socket. Its default action is inspection. Only an explicit `--action=stop` can
issue `StopPodSandbox`, and only for the full sandbox ID. Before the stop, the
guard validates all these values for that ID against current CRI data:
namespace, Pod name, Pod UID, and runtime handler. There is no name-only stop,
remote endpoint, runtime endpoint discovery, or batch mode.

## Dependency boundary

The operator did not previously declare `k8s.io/cri-api`. To call the real
runtime service, this prototype must have the official CRI v1 protobuf messages
and the generated gRPC client from that module. For the local wire-protocol
tests, it also must have the server bindings from that module. It adds exactly
`k8s.io/cri-api v0.37.1`, which aligns with the Kubernetes module versions that
the operator already uses. It imports the generated CRI bindings from that
module. It does not copy or keep them in this repository.

The client declares `google.golang.org/grpc v1.83.2` directly. This version
includes the fixes for
[HTTP/2 frame memory exhaustion](https://github.com/advisories/GHSA-vp52-pcj8-j9qc)
and the [xDS header crash](https://github.com/advisories/GHSA-2v4p-qf9q-27wj).
Dependency review reported these problems for the previously selected version.
The protobuf version that the operator already uses and the other dependency
versions stay the same. Dependency setup uses Go's versioned module commands
and checksum verification. The guard does not invoke an external runtime CLI
or substitute an unpinned library.

## Build and offline tests

Use the repository's pinned Go toolchain and dependencies that are already
prepared. Run these commands from the repository root:

```bash
GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local GOFLAGS=-mod=readonly \
  CGO_ENABLED=0 make -C app/cetirun-operator build-node-guard

GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local GOFLAGS=-mod=readonly \
  go -C app/cetirun-operator test ./internal/sandboxguard ./cmd/sandbox-node-guard -count=1

GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local GOFLAGS=-mod=readonly \
  go -C app/cetirun-operator vet ./internal/sandboxguard ./cmd/sandbox-node-guard
```

The build produces `bin/sandbox-node-guard` in the operator directory.
It does not build a container image, deploy a Pod, start the operator, or
contact a cluster. The tests use synthetic CRI gRPC services on temporary
local Unix sockets, not a host runtime.

## Explicit invocation

Get the expected identity from a separately authorized, trusted source.
The caller must select the correct node and runtime socket. This process does
not query Kubernetes. It also does not examine node scheduling or API-server
identity. These values are synthetic, and you must not use them to guess a
real target:

```bash
app/cetirun-operator/bin/sandbox-node-guard \
  --socket=/run/containerd/containerd.sock \
  --namespace=sandbox-example \
  --pod-name=guard-example \
  --pod-uid=00000000-0000-4000-8000-000000000001 \
  --sandbox-id=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
  --runtime-handler=runc \
  --timeout=15s
```

That command only inspects. For an explicitly approved stop experiment,
append **`--action=stop`** to the same exact-identity command. The guard does
not retry a failed stop RPC, substitute a target, or stop a replacement sandbox.

For safe sandbox ID discovery, omit `--sandbox-id` **only in inspect mode**:

```bash
app/cetirun-operator/bin/sandbox-node-guard --action=inspect \
  --socket=/run/containerd/containerd.sock \
  --namespace=sandbox-example --pod-name=guard-example \
  --pod-uid=00000000-0000-4000-8000-000000000001 \
  --runtime-handler=kata --timeout=15s
```

For discovery, the exact namespace, Pod name, Pod UID, and expected handler
are still necessary. Discovery selects only one complete match. It pins the
full sandbox ID of that match for the status and inventory rechecks. It
returns `target.sandboxID` only on success. If there is no match, or if a
match is ambiguous, mismatched, or changes, discovery fails without a
discovered ID.

Discovery never stops a sandbox. For a later approved stop, supply that
returned ID explicitly with `--sandbox-id=FULL_ID --action=stop`. The guard
then validates the full identity again. It does not trust the earlier receipt.
The guard rejects a stop without an ID before any CRI call.

All other identity flags and `--socket` are necessary. You must explicitly
supply the runtime handler flag. If an empty handler is expected, you must
also supply it, as `--runtime-handler=""`. An empty value is compared
literally. It does not mean "any handler", and the guard does not infer a
default. A handler that is absent from a runtime response cannot satisfy a
nonempty expected handler.

For a Kubernetes RuntimeClass, pass its top-level `handler`, not its
`metadata.name`. The guard does not fetch RuntimeClass names and does not
translate them.

`--socket` accepts only a clean, absolute, printable-ASCII path of at most 107
bytes to a local Unix socket. It does not accept `unix://`, TCP, HTTP, DNS, or
abstract-socket endpoints. The namespace and the Pod name follow the
Kubernetes DNS rules for names. The UID and the sandbox ID have at most 128
ASCII characters and start with an alphanumeric character. After the first
character, they allow alphanumerics, `.`, `_`, and `-`. The comparison is
exact and case-sensitive, and the guard never accepts a prefix as an ID.

`--timeout` must be positive and no greater than one minute. The default is 15
seconds. One deadline covers the connection setup and all CRI calls together.
Cancellation, deadline expiry, and any unsuccessful CRI call cause the
operation to fail. In particular, a stop error or timeout can mean that a stop
occurred without a confirmed outcome. Do not treat it as a no-op, and do not
retry automatically.

## What is checked

1. List **all** CRI sandboxes, and include the non-ready ones. Find exactly
   the supplied ID and complete Pod identity (or the unique complete match
   during inspect-only discovery). Reject extra records with that ID,
   the same Pod UID, or the same namespace and Pod name. The guard
   deliberately treats historical records that match as ambiguous, even if
   they are non-ready.
2. Read the non-verbose status for the full ID. Make sure that Pod metadata,
   runtime handler, creation time, attempt, and state are identical across
   list and status. Before any stop, repeat the inventory and status checks.
   Keep the original creation time and attempt, and use them to reject a
   changed sandbox.
3. In stop mode, issue one exact-ID `StopPodSandbox`. A successful RPC alone
   is never a successful stopped observation.
4. To report stopped, make sure that fresh inventories and exact status checks
   report `NOTREADY`. Between them, get a filtered container inventory.
   Only exited containers or an empty container list are acceptable.
   Created, running, unknown-state, duplicate, malformed, or wrong-sandbox
   container records cause the observation to fail.
5. After the container inspection, recheck the complete sandbox inventory and
   exact status. A replacement ID, an active sandbox that matches, or a changed
   identity, handler, or attempt rejects the result. No replacement is stopped.

An inspection of a ready sandbox reports `observed_ready` and never issues a
stop. An inspection of a non-ready sandbox uses the same stopped-observation
checks. An already absent sandbox, an absent status, an unsupported response,
or `NotFound` from status or stop is inconclusive, not successful fencing.
Unexpected malformed inventory data also fails closed.

## Receipt and exit codes

Except for `--help`, stdout is a single JSON object followed by a newline,
with a limit of 2,048 bytes. CRI response messages have a limit of 4 MiB. A
response that is larger than that limit causes a failure. The guard does not
accept a partial inventory. There is no verbose output, runtime information
dump, container metadata dump, environment dump, log retrieval, or credential
output. Errors use fixed reason codes, and the guard never prints raw CRI
error messages.

| `status` | Exit | Meaning |
| --- | --- | --- |
| `observed_ready` | 0 | Exact identity was observed ready (inspect only) |
| `observed_stopped` | 0 | Observed stopped **at that moment only** |
| `unknown` | 1 | Insufficient evidence, for example an absent sandbox |
| `rejected` | 1 | Identity, replacement, state or container checks failed |
| `error` | 1 | CRI call, cancellation or deadline failure |
| `rejected` with `invalid_input` | 2 | Invalid arguments (no CRI call) |

The version-1 receipt includes `action`, typed `status` and `reason`,
`stopAttempted`, `observationScope: "point_in_time_only"`, and the validated
input `target` when available. `observedAt` is the local UTC completion time
of a successful observation, **not an atomic runtime snapshot timestamp**.
The stopped reason is `observed_stopped_at_that_moment`. These fields are
always false: `durableWriterExclusion`, `preStartAuthorization`,
`completeEnforcement`, and `productionEligible`.

`stopAttempted` means that the client invoked the exact-ID stop RPC. If a
transport failure occurs, the field does not establish that the runtime
received the RPC. The receipt contains only the validated Pod identity that
the caller supplied and, after a successful discovery, the rechecked sandbox
ID. It never contains the socket path or diagnostic payloads from the runtime.
A failed discovery leaves `target.sandboxID` empty. Treat identity receipts as
operational data, and do not publish real workload identifiers.

## Privilege and remaining integration

Access to the node's root-owned CRI socket is effectively host-runtime
privilege. This tool belongs only in a separately authorized, trusted test
process on the selected disposable node, never in an agent workload. For
socket access, host networking and other host filesystem mounts are not
necessary. The tool does not add those privileges and does not supply a
deployment manifest.

The observations span sequential CRI calls and are not atomic. Kubelet can
restart a container or recreate a sandbox with the **same Pod UID**, even
immediately after the final recheck. A runtime can also remove a sandbox
before the guard can observe its stopped status. That outcome stays unknown.
No number of finite rechecks makes the receipt durable writer exclusion.
Trust in the local runtime and node is a prerequisite, not something this
prototype establishes.

Independent integration and validation are still necessary for the operator
hold, termination order, fallback behavior, and the prevention of replacement
starts. This local process cannot safely exclude an unreachable or compromised
node. Independent unreachable-node fencing is still necessary. Successful
offline tests do not allow production enablement or the release of the
operator's execution hold. A disposable-cluster experiment also does not allow
them.

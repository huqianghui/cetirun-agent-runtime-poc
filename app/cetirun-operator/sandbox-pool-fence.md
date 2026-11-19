# Disposable User-pool deletion qualification

This Linux-only command sends a real, conditional AKS **whole-pool DELETE**.
It is restricted to a newly reserved, single-use User pool that contains one VM
and has autoscaling disabled. The cluster must have exactly one untouched
system pool and no other User pools.

**No Agent workloads are allowed in this API-only qualification.** The operator
must keep the task-owned cluster isolated from unrelated users and workloads
for the entire experiment. A run tag is ownership correlation, not admission
isolation. This command does not establish that isolation or supply a
`CheckQuiesced` provider, pre-start/data-plane validation, an execution permit,
or trusted post-start observation. Production startup stays held. Any pilot
with Agent workloads must have separately qualified admission/start
coordination and writer-history registration, and this receipt cannot
substitute for them.

## Public API and dependency boundary

The fixed Azure Public ARM operation is:

```http
GET    {poolARMID}?api-version=2026-05-01
DELETE {poolARMID}?api-version=2026-05-01
If-Match: <exact properties.eTag from GET>
```

The [public AKS specification](https://github.com/Azure/azure-rest-api-specs/blob/497a43f48abaaefc756655cc703832e7d2cb7754/specification/containerservice/resource-manager/Microsoft.ContainerService/aks/stable/2026-05-01/managedClusters.json)
defines the ETag field and conditional DELETE. ManagedCluster `resourceUID`
distinguishes cluster incarnations. AgentPool has no published immutable UID.
As a result, the helper pins an ETag and refuses any refresh or adoption after
drift. A pool name must never be reused for this registration.

The command uses the already-pinned `azcore` ARM transport. It does not add a
generated ContainerService SDK or change dependency versions. ARM provider
auto-registration and retries are disabled. A transport guard also prevents
DELETE replay through authentication handling. Redirects are forbidden.

The transport applies the 1 MiB response-body cap before azcore can buffer the
body. This cap also applies to error responses and chunked transfers.
An oversized DELETE reply leaves the durable intent unknown, never completed
or eligible for resubmission.

The Azure CLI credential invokes `az account get-access-token` with a strict
argument array, explicit `--subscription`, and the exact
`--resource https://management.azure.com/` audience. It does this without a
shell, login, default-credential chain, or alternate credential discovery.

Azure CLI rejects the combination of `--subscription` and `--tenant` on this
command. Instead, the helper's `--tenant-id` gives the tenant that the returned
token must have. Before any ARM request, both the returned `tenant` and the
returned `subscription` must match the registration. If this metadata is
absent or mismatched, the command fails without a fallback or context switch.
Tokens are memory-only, not journal or output fields.

Polling supports the documented `Azure-AsyncOperation` and `Location` response
headers, confined to the HTTPS AKS operation endpoints of the same
subscription. The monitor API version can differ from the pool API version.
A single valid dated version is necessary. The helper keeps the bounded opaque
query of the provider verbatim and does not rewrite it. Origin, subscription,
path, and redirect restrictions still apply.

Location completion follows the pinned SDK's
[Location polling semantics](https://github.com/Azure/azure-sdk-for-go/blob/sdk/azcore/v1.23.2/sdk/azcore/internal/pollers/loc/loc.go).
Compute GET uses `2025-04-01` and the read-only `properties.vmId`. VM resource
names and VMSS instance IDs alone are insufficient because
[instance IDs can be reused](https://learn.microsoft.com/en-us/azure/virtual-machine-scale-sets/virtual-machine-scale-sets-instance-ids).

## Build and offline validation

Use the pinned Go toolchain, and run these commands from the repository root:

```bash
GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local GOFLAGS=-mod=readonly \
  go -C app/cetirun-operator test ./internal/sandboxfence ./cmd/sandbox-pool-fence -count=1

GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local GOFLAGS=-mod=readonly \
  go -C app/cetirun-operator vet ./internal/sandboxfence ./cmd/sandbox-pool-fence

GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local GOFLAGS=-mod=readonly \
  go -C app/cetirun-operator build -o /tmp/sandbox-pool-fence ./cmd/sandbox-pool-fence
```

The tests use synthetic identities, a local TLS HTTP server, and a synthetic
SDK credential. They never invoke Azure CLI or access Azure. These tests do not
qualify the live provider. Separately authorized resources and authenticated
access are necessary for the live commands below. These commands are not
development checks.

### Test-only managed-disk observation probe

The [disk probe](internal/sandboxfence/disk_probe_test.go) exists only in Go test
code. It reuses the bounded ARM transport and synthetic TLS fixtures. The
command and the Operator do not gain a disk reader or writer-exclusion
provider. The probe makes no cloud requests and does not change the empty-pool
experiment scope.

The probe exercises the public
[Disk GET 2025-01-02 contract](https://learn.microsoft.com/en-us/rest/api/compute/disks/get?view=rest-compute-2025-01-02):

- The resource ID and `properties.uniqueId` must match the separately pinned
  disk incarnation.
- Provisioning must be `Succeeded`.
- The disk state must be `Unattached`.

Top-level `managedBy` and `managedByExtended`, and also `properties.shareInfo`,
must contain no owners. For this deliberately narrow prototype, an explicit
`maxShares: 1` is necessary. Omitted, null, or shared capacity is still
inconclusive and does not default to a supported storage tuple.

Regressions reject a recreated disk at the same path and `Reserved` disks that
belong to deallocated VMs. They also reject attachment contradictions,
duplicate fields, read errors, and disk absence. A completed pool receipt does
not substitute for a disk read. Conversely, an unattached disk and absent old
resources do not repair a lost DELETE reply. The journal stays observe-only
when it is opened again, with no destructive replay or fabricated completion.

These are offline protocol checks, not positive physical-fencing evidence.
Even an `Unattached` response that matches is only a point-in-time
control-plane observation. It does not prevent reattachment or prove that all
historical writers are excluded. Pre-writer enrollment, a durable start
barrier, independent current exclusion/storage evidence, and real
no-overlap/data-continuity checks stay outside this probe. No execution
permission or production integration is introduced.

### Test-only writer-recovery composition

The [recovery probe](internal/sandboxfence/disk_recovery_test.go) composes that
disk reader with the `sandboxwriter.Provider` methods, the `Store` JSON
journal, and the `Manager.Quiesce` / `CheckQuiesced` order that already exist.
A fake ConfigMap API keeps the serialized intent across fresh Manager/Provider
instances. Disk requests use the loopback ARM transport that already exists.
This adds no production provider, source activation, or schema. The synthetic
storage driver's opaque handle carries the fixture disk incarnation. This is
**not** a claim that an Azure CSI handle pins a managed disk's `uniqueId`.

Another boundary is evidence composition. A later disk read cannot refresh
stale exclusion evidence. A delayed exclusion read also cannot refresh an
earlier disk sample. The composite observation keeps the oldest component time
for the freshness checks that the Manager already has, also after journal reads
and writes. Future observations and a backward clock are rejected, not hidden
by timestamp selection. Even a restamped proof from the previous operation
cannot cover a newly reserved writer epoch.

The tests lose a Revoke reply and, separately, a persisted quiescent-update
reply. Then they recover through fresh managers without another Revoke. They
keep the journal, operation, and enrollment IDs and complete immutable
writer/storage history. Every recovery check reads the disk again, even for an
already quiescent journal. These conditions cannot pass:

- Reattached or recreated disks
- Replaced machine/PV incarnations
- No independent storage-release evidence
- Accepted stops
- NotReady nodes
- Pod absence
- No durable barriers

```bash
GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local GOFLAGS=-mod=readonly \
  go -C app/cetirun-operator test ./internal/sandboxfence -run '^TestDiskWriterRecovery' -count=1

GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local GOFLAGS=-mod=readonly \
  go -C app/cetirun-operator test -race ./internal/sandboxfence ./internal/sandboxwriter ./cmd/sandbox-pool-fence -count=1
```

**Production facility is still NO-GO.** In these tests, the positive barrier,
machine-exclusion, and HOME/volume-release assertions are independently
supplied synthetic fixtures. They are never inferred from `Unattached` or an
empty-pool receipt. The fake API does not prove API-server or power-loss
durability. Before any live recovery claim, a separately authorized provider
experiment must establish all of these:

- Complete pre-writer enrollment
- A still-installed guest-external start barrier
- Immutable machine/disk binding
- Independent current storage release
- No-overlap/data-continuity results across response loss and controller
  restart

If this offline slice passes, that is not full M4 qualification or permission
to start workloads.

## Explicit workflow

These placeholders must come from the authorized provisioning run:
`POOL_ARM_ID`, `TENANT_ID`, `CLUSTER_RESOURCE_UID`, and a new UUID `RUN_ID`.
`TENANT_ID` is the expected tenant for the selected subscription's Azure CLI
account, not an instruction to switch accounts or tenants.
Use a private, durable directory outside the checkout as `PRIVATE_DIRECTORY`.

1. **Before creating the User pool**, reserve the absent ARM ID:

   ```text
   sandbox-pool-fence --action reserve --journal-dir PRIVATE_DIRECTORY \
     --pool-id POOL_ARM_ID --tenant-id TENANT_ID \
     --cluster-resource-uid CLUSTER_RESOURCE_UID --run-id RUN_ID
   ```

   The cluster must already exist. Pools that already exist are rejected, even
   if they have the expected tag. A failed or unauthorized read is not an
   absence result.

2. The authorized operator creates the new pool, not this command. The pool
   has one **User** VM, autoscaling explicitly disabled, and the pool tag
   `cetirun-fence-run=RUN_ID`. Do not create Agent workloads or give another
   actor access to the pool. Do not change the system pool.

3. Register the pool ETag and the exact Machines-to-Compute `vmId` binding:

   ```text
   sandbox-pool-fence --action register --journal-dir PRIVATE_DIRECTORY
   sandbox-pool-fence --journal-dir PRIVATE_DIRECTORY
   ```

   The second command is the default inspect action. Registration is one-shot.
   Registration fails for an absent ETag, identity changes, extra pools, or
   ambiguous machines.

4. Explicitly authorize the destructive conditional-API probe:

   ```text
   sandbox-pool-fence --action test-condition --journal-dir PRIVATE_DIRECTORY \
     --confirm-delete-pool POOL_ARM_ID
   ```

   **This sends DELETE with a nonmatching ETag. A service that fails to enforce
   the condition could destroy the disposable pool.** Intent is persisted
   first. Only an HTTP 412, followed by unchanged pool and VM identities,
   qualifies the next step. An unexpected acceptance, a lost response, or an
   ambiguous failure stops the experiment. Never repeat the probe or discard
   its journal.

5. Submit the matching DELETE one time:

   ```text
   sandbox-pool-fence --action delete --journal-dir PRIVATE_DIRECTORY \
     --confirm-delete-pool POOL_ARM_ID
   ```

   For an asynchronous or uncertain submission, exit 1 with a `deleting` or
   unknown phase is expected. **Never repeat DELETE.** Observe instead:

   ```text
   sandbox-pool-fence --action observe --journal-dir PRIVATE_DIRECTORY
   ```

   Each observe is one bounded read pass. Repeat it after pending or read
   errors. The default `--timeout` is 30 seconds, and it cannot be more than
   five minutes. Cancellation does not cancel the cloud operation or authorize
   a new DELETE.

For completion, all of these are necessary:

- A terminal successful operation
- Authoritative pool absence
- Authoritative absence at every recorded old Compute VM resource
- The same cluster incarnation, still present

A replacement at an old resource path is left untouched and does not count as
absence. An initial 204 does not establish operation success. An absent
operation monitor also does not establish it. Running, failed, or canceled
operations cannot produce completion. VM deallocation, Node readiness, and Pod
absence also cannot produce it. A completed journal is a historical receipt for
this exact empty-pool experiment, never permission to delete a later pool.

A pending journal can already contain the durable HTTP 202, a recognized poll
kind, and an operation handle that the current validator accepts. If it does,
`observe` can resume polling after it validates the cluster incarnation. It
never resubmits DELETE. Absent response evidence, or an invalid or foreign
handle, stays unknown.

For these fixed, identity-checked GETs, a 404 absence response must contain
`NotFound`, `ResourceNotFound`, or `ParentResourceNotFound`. This code must be
in either the ARM `error` envelope or the bare AKS error-detail object
(`code`, `message`, `details`). The
[public error-detail schema](https://github.com/Azure/azure-rest-api-specs/blob/497a43f48abaaefc756655cc703832e7d2cb7754/specification/common-types/resource-management/v6/types.json)
defines those detail fields. Synthetic HTTP fixtures cover the observed bare
AKS `NotFound` shape. These cases stay unknown and do not establish absence:

- Empty, HTML, or malformed replies
- Authorization errors
- Wrong HTTP status
- Duplicate fields
- Simultaneous bare and enveloped codes

Cluster-incarnation checks still surround reservation. No error response
allows target adoption.

## Journal and recovery

The private directory is mode 0700, and `lock` and `journal.json` are mode 0600.
The journal binds the fixed API/scope, tenant, subscription (through the pool
ARM ID), cluster resourceUID, run UUID, node resource group, ETag, and Compute
identities. It records separate condition-probe and final DELETE intents,
request IDs, HTTP outcomes, operation handle/status, and final destruction
observations. It contains resource identities and operation handles, but no
Azure CLI bearer token or workload credentials/contents. Operation handles can
include opaque or signed query values. Treat the whole journal and command
output as sensitive: keep real receipts private and out of Git.

A nonblocking exclusive lock covers each invocation. Each transition writes a
bounded temporary file, fsyncs it, atomically renames it, and fsyncs the
directory **before** a destructive side effect or completion output. Symlinks,
unsafe permissions, malformed state, and phase rollback are rejected.

Every journal open also fsyncs its parent directory. This also applies to
retries with a directory that already exists and to resumed operations. The
directory entry must be persisted before a command can succeed or contact ARM.
Parent-sync failures propagate, and the helper does not create a success
record or send a destructive request.

An authentication failure before the first successful reservation can leave a
directory and lock file without `journal.json`. Retry the reservation only
after you make sure that the actual `journal.json` is absent. To recover from
an error, do not delete or reset a journal that already exists. Keep stdout
captures outside the helper-owned journal files. A stdout error document is
not a reservation or mutation receipt. It must never be written over
`journal.json`.

Keep the journal permanently on durable local storage. Do not copy it to
another host, run multiple copies, or reset its phase. Do not remove an intent,
refresh its ETag, or reuse the pool name.

A crash after persisted intent but before a confirmed response is
intentionally ambiguous: there is no blind resubmission or unsupported
operation-discovery fallback. If operation handles are absent, a separately
authorized provider investigation is necessary. Absence alone does not repair
that evidence gap. This local journal does not support networked or shared
filesystems, or multi-host coordination.

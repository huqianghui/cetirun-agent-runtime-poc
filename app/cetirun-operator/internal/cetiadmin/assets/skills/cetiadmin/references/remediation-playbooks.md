# Remediation playbooks

Diagnose a failure before changing its target. Use the current Agent UID,
namespace UID, runtime, release, and resource binding. Apply
[operational constraints](operational-constraints.md) to every attempt.
Forbidden, conflict, changed identity, and unknown write outcomes stop automatic
retries. Record evidence without complete Secrets, user logs, or credentials.

## CrashLoopBackOff

Detect `status.containerStatuses[].state.waiting.reason=CrashLoopBackOff` in
the actual failing container. Read bounded current and previous logs and its
last termination reason:

```sh
kubectl --context cetiadmin -n "$CETIADMIN_TARGET_NAMESPACE" logs "$CETIADMIN_TARGET_POD" -c "$CETIADMIN_TARGET_CONTAINER" --since=15m --tail=200
kubectl --context cetiadmin -n "$CETIADMIN_TARGET_NAMESPACE" logs "$CETIADMIN_TARGET_POD" -c "$CETIADMIN_TARGET_CONTAINER" --previous --tail=200
kubectl --context cetiadmin -n "$CETIADMIN_TARGET_NAMESPACE" get pod "$CETIADMIN_TARGET_POD" -o json
```

- For `OOMKilled`, follow the OOM branch below.
- For invalid configuration, correct only already-authorized intent. A new
  configuration/resource decision requires confirmation. Missing managed
  authentication belongs to the Operator's reconstruction path below; do not
  create a substitute Secret.
- For a transient failure with no persistent configuration cause, an ordinary
  bound Pod on a healthy source can use a normal, graceful Pod deletion and
  controller recreation. Verify the exact Pod UID immediately before the
  operation, its controller ownership, current target binding, node health,
  HOME writer identity, and absence of suspension, migration, or maintenance.
  Use the installed UID-aware admission/deletion contract. Do not force-delete
  or set a zero grace period.
- If the source/writer is unknown, or the target is protected, use its existing
  supported recovery path or escalate. Do not weaken exec or admission rules.

After an eligible restart, observe the replacement identity and require Running
and Ready continuously for 60 seconds. If the crash recurs, escalate instead
of restarting indefinitely. The normal Ready-only retained restart API is not
assumed to work for CrashLoopBackOff.

## OOMKilled

Read the named container's memory limits, recent termination reasons, restart
counts, and workload Events. Do not assume a counter from another container
or a replaced Pod belongs to the same failure series. An eligible ordinary Pod
can receive one autonomous restart using the checks above.

If OOM occurs more than three times in one hour, escalate. Propose a justified
limit increase, obtain confirmation, and update permitted Agent intent rather
than patching an arbitrary live workload. Verify the effective Pod limits and
stability after reconciliation. Do not silently change global resource defaults.

## ImagePullBackOff

Inspect the exact image reference, image pull Events, and the current
registry-binding status. Distinguish unavailable images, denied pulls,
bad coordinates, and scheduling problems. Report the failing image and
sanitized error to the administrator. Do not read registry credential stores,
copy another imagePullSecret, log in to a registry, or change repository access.

## Helm maintenance handoff

Use native read commands for the exact bound release:

```sh
helm status "$CETIADMIN_TARGET_RELEASE" --namespace "$CETIADMIN_TARGET_NAMESPACE" --kube-context cetiadmin
helm history "$CETIADMIN_TARGET_RELEASE" --namespace "$CETIADMIN_TARGET_NAMESPACE" --kube-context cetiadmin
```

`helm get`, including values, manifest, and hooks, can contain sensitive data.
Inspect only necessary fields locally and keep recovery material private.
Never include complete release Secrets or values in an Event or report.

For direct Helm writes, complete this sequence for one ordinary target:

1. Determine the current Agent UID/RV, latest release revision, desired recovery
   Chart/image pair, and supported object set. Exclude CetiAdmin, platform
   releases, protected Sandbox, deleting targets, and active migration. Prove
   that an old Helm writer or hook is no longer running; a fixed delay is not
   that proof.
2. Conditionally submit `spec.suspended=true`, the agreed target Chart/image
   intent, and `spec.helm.maintenance` containing a stable `requestID` and
   `expectedRevision`. Save the previous suspension state. Suspension alone
   is not permission to run Helm writes.
3. Wait for matching `status.helmMaintenance` phase `Held`, request ID, observed
   generation, and revision. Verify that the Operator stopped normal release
   writes, confirmed all HOME writers and old hooks stopped, and installed
   startup blocking for every workload creator. Do not rely only on an old
   `HelmMaintenanceReady=True` condition.
4. Preflight both current and target manifests. Read all supported unchanged
   objects that Helm needs, including SA, RBAC, network, admission, and storage
   objects. Only bound runtime ConfigMaps, permitted existing workloads/Services,
   and this release's revision Secrets can be written. A historical Chart that
   changes forbidden GVKs, identity, storage ownership, or another namespace
   requires administrator recovery, not wider CetiAdmin permission.
5. Run one native maintenance action with hooks disabled, no Pod Ready wait,
   and history pruning disabled. Keep the target stopped. Do not create new
   workload types or replay historical hooks.
6. After the command has a known outcome, read the actual revision. Clear only
   this maintenance request conditionally while preserving `suspended=true`.
   The Operator enters `Reapplying`, rejects further CetiAdmin Helm writes,
   invalidates stale configuration, and reapplies authoritative Agent intent
   with `DisableHooks=true`. `Wait=false` is not a substitute for no hooks.
7. Wait for this request's `Released` state, current intended configuration,
   and zero runtime replicas. Startup remains blocked. Only then submit a
   separate authorized resume and verify this generation's Ready state. Do not
   resume a previously user-suspended target or clear an external key block.

If the handoff is absent or fails, do not issue native writes. A timeout,
disconnect, unknown command result, incomplete old writer, revision conflict,
or incompatible immutable field leaves the target stopped for inspection.
`Blocked` records missing or conflicting completion evidence. It does not permit
aborting the handoff. The Operator retries a failed preflight only when no write
was submitted. Do not clear the request or resume merely because a timeout
elapsed. Another administrator can continue the same request after it reaches
`Held`. No running model is required. While a released target stays suspended,
submit configuration changes with a fresh maintenance request, or resume first.

The handoff does not add a human approval to an already-authorized autonomous
fault rollback. Planned upgrades, proactive rollback, and new choices still
require confirmation. Never reuse one request ID for a different recovery target.

## Release stuck in pending-upgrade or pending-install

Read native status, history, revision metadata, and old-operation evidence.
Do not read every Secret by prefix. If successful compatible history exists,
choose the confirmed last known-good revision and use the maintenance handoff.

Within matching `Held`, a fault rollback uses:

```sh
helm rollback "$CETIADMIN_TARGET_RELEASE" "$CETIADMIN_TARGET_REVISION" \
  --namespace "$CETIADMIN_TARGET_NAMESPACE" --kube-context cetiadmin \
  --no-hooks --wait=false --history-max=0
```

Helm 3.22 rollback otherwise prunes history at the default limit. The explicit
`--history-max=0` preserves revision Secrets and does not require Secret delete
permission. A native maintenance `helm upgrade` may reapply only the held
revision's stored chart, values, manifest, and hooks, with the same no-hook,
no-Ready-wait, and no-pruning rules. A rollback must retain those fields from
one existing historical revision. Make configuration or version changes through
the Agent fields after maintenance. The Operator remains the chart writer.
Normal Operator history retention remains separate from maintenance and prior
recovery archival.

For `pending-install` with no successful history, use the shipped
`/opt/cetirun/cetiadmin/scripts/repair_helm_release.py` integration after the same
handoff. Its job is to verify the exact latest revision Secret UID/RV, fixed
Helm 3.22 encoding, pending status, and ended old operation, then mark both
label and encoded status `failed`. It preserves values and history and never
fabricates `deployed`. Save the original object only as a private, sensitive
recovery artifact through that helper's supported interface.

The helper is supplied by the runtime integration, not by this Markdown skill.
Read its shipped `--help` for finalized flags and supported input files. Do not
invent a shell replacement, delete revision Secrets, or use `helm uninstall`
when the helper is absent, incompatible, or cannot prove its preconditions.
Finish with the handoff's Reapplying, Released, and separate resume checks.

## Release failed

Record the failed revision, sanitized Helm error, relevant Pod/Events, and
Agent conditions. Report the cause and recovery choices. A failed revision is
not permission to retry indefinitely or uninstall. Use a separately justified
fault recovery through the same handoff when the stuck-release branch applies;
otherwise escalate, as in the original failed-release procedure.

## Standard route failure

Inspect `status.accessUrl`, the bound Service and endpoints, actual runtime
profile, current routes, and ingress conditions. A missing route, failed
upstream, authentication denial, and mTLS failure are different causes.
Read the current endpoint and TLS binding rather than assuming a 302 response,
an alias subdomain, or port 18789.

For a standard route that drifted, correct only permitted authoritative Agent
intent and let the Operator regenerate the route. Observe a current
reconciliation and the resulting Service/destination/port. A same-value patch
does not prove a new reconciliation occurred. If no supported reconciliation
path is available, report the gap; do not manufacture spec drift or patch an
arbitrary VirtualService. Nonstandard routes, Gateway, Istio control-plane,
NetworkPolicy, CA, or external DNS changes belong to the administrator.

## DNS failure

Check the actual published host, Service endpoints, DNS Events, and CoreDNS
status only in authorized read namespaces. Do not assume every alias uses a
separate subdomain. Report missing discovery permissions distinctly from a DNS
failure. DNS infrastructure repair is always escalated; do not modify CoreDNS,
Istio, node networking, or cluster policies.

## Disk usage above 85 percent

Use authorized exec only on a confirmed ordinary target, with the real image
user's HOME. Read `df` for the mounted filesystem, then a bounded `du` listing
to identify where space is used. The usual `/home/vibe` is not universal.
Never infer fullness from PVC capacity.

Autonomous cleanup is limited to exact, verified disposable temporary files
or logs in an approved temporary directory. Inspect paths, owners, use, and
size before removal; retain recovery copies when needed. Do not delete a HOME
tree, user sessions, native output/notepad, a PVC, or all files under `/tmp`.
Do not follow symlinks or use recursive wildcards. Age alone does not make
every `*.log` under HOME disposable. If no safe files exist, skip cleanup.

Measure again after cleanup. If use remains above 85% or is above 95%, recommend
expansion and obtain confirmation. Check the actual StorageClass's expansion
support and current PVC ownership. Increase both the permitted Agent capacity
intent (`spec.helm.values.persistence.size` for ordinary targets) and the bound
PVC request, using observed identities and conditional writes. Never shrink or
change reclaim policy, existingClaim, mount ownership, or volume binding.

Observe the PVC's resize conditions and the mounted filesystem capacity.
Record a partial or failed expansion rather than assuming the second write or
filesystem resize succeeded. Protected targets without a supported file/resize
interface remain protected; escalate without an exec exception.

## PVC or workload stuck Pending

Read the exact PVC and Pod Events, StorageClass binding mode, capacity,
selected-node/scheduling observations, and applicable CSI/attachment/snapshot
status. A WaitForFirstConsumer PVC can legitimately be Pending before its first
consumer. A StatefulSet or Pod Pending for more than five minutes needs
diagnosis, not blind restart.

Report provisioning errors, exhausted capacity, scheduling constraints,
unavailable nodes, or missing read permissions. Do not alter StorageClass,
PV, node scheduling, volume ownership, or storage infrastructure. Escalate an
unresolved storage/capacity cause to the administrator.

## Native authentication expired, missing, or inconsistent

First distinguish native UI/API authentication from model access, ingress mTLS,
MCP, Copilot, and provider authorization. Read only the required status and
native-auth metadata for the exact target. Do not print key values or put them
in command arguments. An authentication symptom is not proof that rotating
every key will fix it.

If the referenced native-auth Secret or a managed required field is absent,
let the Operator's authentication reconciliation reconstruct it. Observe its
current result and target restart/readiness. CetiAdmin does not create an
arbitrary Secret or adopt a same-name foreign object.

For an existing Secret that needs an allowed autonomous native-auth rotation,
use `/opt/cetirun/cetiadmin/scripts/rotate_native_auth.py`. The runtime-owned
helper verifies target type, Secret UID/RV, and admitted fields. Hermes uses
`API_SERVER_KEY` and consistent `hermes.env`; ordinary OpenClaw uses
`OPENCLAW_GATEWAY_TOKEN` and its native configuration. Handle the separate
`CETIRUN_AUTH_KEY` according to that helper's current contract, while preserving
all other Secret fields and ownership metadata. Never copy the native key into
the auth key or model key slot as a shortcut.

Read the helper's shipped `--help`; its flags are an integration dependency.
Do not replace it with `openssl` output, `--from-literal`, a whole-Secret apply,
or credentials in a pipeline. On a conflict or unknown rotation result, stop
and inspect before retrying. Finish with the target's supported coordinated
restart, native config/readiness verification, and appropriately authorized
access check. A new Secret resourceVersion alone is not successful recovery.

Do not rotate `CETIRUN_GATEWAY_API_KEY`, another Agent's Virtual Key, Gateway
master/provider keys, Copilot tokens, or Azure/MCP workload identity. Escalate
unavailable model authorization without switching providers or resetting spend.

## Namespace stuck Terminating

After five minutes of unexplained termination, inspect the exact namespace
UID, deletion conditions, finalizers, remaining admitted resource kinds, and
the responsible controllers. Use discovery to bound the list; do not run an
unfiltered all-API/all-namespace loop or dump Secrets. Identify unreadable
resource domains as gaps, not absent blockers.

Report each blocker, who owns it, the affected data, and recovery/backup status.
Another authorized administrator decides the controller-specific cleanup or
finalization procedure. CetiAdmin cannot patch/finalize namespaces, remove
Agent/PVC finalizers, force-delete resources, or supply blanket empty-finalizer
instructions. Observe the administrator's outcome without claiming execution
or data retention that was not verified.

## Platform component failure

If Operator replicas are unavailable, Portal is unhealthy, or Istio ingress
Pods are not Ready, collect bounded status/Events/logs within configured read
namespaces and alert the administrator. Include relevant node, API health,
route, certificate, and DNS observations. Do not restart, scale, or edit the
platform, its permissions, or CetiAdmin itself.

## Failure branch index

| Failure | Autonomous scope | Escalation |
| --- | --- | --- |
| CrashLoopBackOff | Diagnose; one eligible ordinary-target restart | Persistent failure, config decision, unknown source, protected target |
| OOMKilled | Diagnose; eligible restart | More than 3 in 1 hour or any limit change |
| ImagePullBackOff | Read image and Events | Image or registry repair |
| pending-upgrade | Known-good fault rollback after matching Held | Unknown writer, conflict, unsupported history, or failed rollback |
| pending-install | Held, fixed-format status repair without history deletion | No compatible helper or uncertain preconditions |
| failed release | Inspect and report | Recovery decision outside the stuck-release branch |
| Standard route drift | Recover through Operator intent/reconciliation | Nonstandard routing or infrastructure fault |
| DNS failure | Read authorized DNS and endpoint state | Infrastructure repair |
| Disk above 85% | Exact disposable-file cleanup | Still above 85%, above 95%, or no safe files; expansion needs confirmation |
| PVC/workload Pending | Diagnose binding, CSI, capacity, and scheduling | Persistent provisioning or scheduling failure |
| Native auth failure | Operator reconstruction or allowed helper rotation | Wrong auth domain, conflict, unsupported/protected target |
| Namespace Terminating | Diagnose exact blockers | All destructive cleanup/finalization |
| Operator, Portal, or Istio failure | Read and alert | All platform mutations |

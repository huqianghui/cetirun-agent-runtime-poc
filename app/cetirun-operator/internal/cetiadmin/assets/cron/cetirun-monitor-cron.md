# CetiRun monitor task

Run a platform health check as CetiAdmin. Use the `cetiadmin` skill and its
remediation playbooks. Retain the autonomous repair tiers, exact target scope,
batch limits, and audit rules. A scheduled run does not grant new permissions.

The installation appends a **Bound monitor input** JSON object to this prompt.
It contains this instance's name, namespace, and UID, the six `monitor` fields,
and `stateDirectory`. Read those values, not a previous conversation's channel
or a guessed namespace. Do not print environment variables or credentials.
If the input is absent, conflicts with this instance, or has `enabled=false`,
stop without repairs or external delivery and record the reason locally.

## 1. Check current Agents

1. List current Agent CRs through the Kubernetes API. For each candidate, verify the
   Agent UID, namespace UID, ownership, runtime, and release. Exclude CetiAdmin
   itself, retained migration sources, platform write targets, and unrelated
   namespaces. A `cetirun-` prefix is not evidence of ownership.
2. Read Agent phase, generation, conditions, suspension, restart, and maintenance
   state. Check the bound workload's replicas, named Pods and containers,
   restart counts, termination reasons, and Events. Use bounded current and
   `--previous` logs when a failure needs them.
3. Check PVC binding, storage Events, Service endpoints, EndpointSlices, and
   the generated routes. Measure disk use with `df` only where ordinary-target
   exec is authorized. PVC capacity is not a disk-use measurement.
4. Read Operator, Portal, Istio, CoreDNS, and other platform component status
   only in the configured `platformReadNamespaces`. Include node, scheduling,
   storage, API health, and optional diagnostic APIs when relevant.

For example, after resolving the exact variables from current API objects:

```sh
kubectl --context cetiadmin get agents.cetirun.microsoft.com -o wide
kubectl --context cetiadmin get agents.cetirun.microsoft.com "$CETIADMIN_TARGET_NAME" -o json
kubectl --context cetiadmin -n "$CETIADMIN_TARGET_NAMESPACE" get pods,statefulsets,deployments,pvc,services,endpointslices -o wide
kubectl --context cetiadmin -n "$CETIADMIN_TARGET_NAMESPACE" get events --sort-by=.lastTimestamp
kubectl --context cetiadmin -n "$CETIADMIN_TARGET_NAMESPACE" logs "$CETIADMIN_TARGET_POD" -c "$CETIADMIN_TARGET_CONTAINER" --since=15m --tail=200
kubectl --context cetiadmin -n "$CETIADMIN_TARGET_NAMESPACE" logs "$CETIADMIN_TARGET_POD" -c "$CETIADMIN_TARGET_CONTAINER" --previous --tail=200
```

Do not suppress errors with `2>/dev/null`. Distinguish an optional API that is
not installed from an installed API that returns Forbidden. Failed reads, empty
responses that lack expected objects, and stale conditions are unknown, not
healthy. A suspended instance or a WaitForFirstConsumer PVC waiting for its
first consumer is not, by itself, a repair target.

## 2. Load the private issue state

Use `stateDirectory/monitor-state.json`. Its default location is
`HERMES_HOME/cetiadmin/monitor-state.json`. This is separate from Hermes' native
job store and from owner-private Memory. Do not modify `cron-binding.json`.

If the state file does not exist, start with an empty `knownIssues` object.
Check that the parent is this instance's private state directory and that the
file is not a symlink before writing. Create private files with mode `0600`.
Never overwrite a foreign-instance or unrecognized-format file.

Keep state at or below 50 KB, defined here as 51,200 UTF-8 bytes. If a recognized
state file exceeds that limit, record a local warning, reset the issue cache,
and rebuild it from bounded findings. Evict records whose `firstSeen` is more
than seven days old. If an evicted problem is still present, treat it as new
with a new `firstSeen`. Before a reset or eviction, retain unresolved write
outcomes and maintenance request IDs in the operation audit. Cache eviction
does not authorize another write whose earlier result is unknown.

Use this shape, with actual bounded findings rather than the example values:

```json
{
  "version": 1,
  "instanceUID": "monitor-agent-uid",
  "lastCheck": "2030-01-01T09:00:00Z",
  "pendingDeliveries": {},
  "knownIssues": {
    "target-agent-uid/target-namespace/Pod/target-pod/CrashLoopBackOff": {
      "firstSeen": "2030-01-01T09:00:00Z",
      "lastSeen": "2030-01-01T09:00:00Z",
      "description": "Runtime container repeatedly exits",
      "slackSent": false,
      "deliveryStatus": "not_sent",
      "deliveryChannel": "",
      "deliveryExecutionID": "",
      "lastAttempt": {"requestID": "repair-example-0001", "result": "skipped"}
    }
  }
}
```

Build issue keys as `<Agent UID>/<namespace>/<resource kind>/<resource name>/<fault>`.
Never include timestamps, restart counts, or fluctuating measurements in a key.
For a platform resource without an Agent, use its actual resource UID in an
explicit `platform/<resource UID>/...` key. A same-name replacement is a new
identity. Do not copy old `.openclaw` state into this file. Migration conversion
or archival is a separate, approved operation.

## 3. Separate new, unchanged, and recovered problems

First, reconcile `pendingDeliveries`, keyed by native execution ID. Each record
contains the native job ID, selected channel, included issue keys, and report
digest. Read each exact execution through `cron.executions.get_execution` and
the job through `cron.jobs.get_job`, using this same `HERMES_HOME`. Do not read
or edit the native database files yourself.

Set `slackSent=true` only after the matching execution completed and its native
delivery result confirms delivery to the recorded channel. The pinned native
ledger uses `delivery_outcome=delivered`, but a bare adapter acknowledgement
can still be unverified. Check delivery evidence and the corresponding
`last_delivery_unverified`, `last_delivery_error`, and `last_delivery_queued`
observations. Do not attribute a later run's job-level fields to an older
pending execution. If the receipt cannot be matched, is queued, lacks evidence,
or timed out, keep `slackSent=false` and mark the result `unknown`. Inspect before
resending. Only a confirmed non-delivery can authorize a bounded later retry.
`suppressed`, `not_configured`, and a tool result with `skipped=true` are not
delivery. Retain pending unknown outcomes until resolved or explicitly handed
to an administrator; TTL eviction does not authorize a duplicate send.
After confirmed delivery, update the included issues' delivery status, channel,
and execution ID, then remove that resolved pending record. An unresolved send
prevents repeating its report, not notifying unrelated new failures. Keep its
issue keys out of a new outbound report and retain its correlation record.

- A new issue is present now and absent from the unexpired cache.
- An unchanged issue is still present with the same identity and fault. Do not
  repeat a repair solely because another cron tick occurred.
- A recovered issue has a successful fresh observation of the formerly failing
  condition. An unreadable target is not recovered. Remove recovered records
  after retaining a small local recovery summary.
- A changed failure, repair attempt, permission gap, or confirmed notification
  failure needs review. An unknown write or send result needs inspection, not
  an automatic repeat.

If the configured Slack channel changes, a receipt for the old channel does
not prove delivery to the new one. Reset delivery bookkeeping for the selected
channel without resetting repair history or resending to the previous channel.

## 4. Repair eligible failures

For each new or materially changed failure, follow the matching branch in
`references/remediation-playbooks.md`. Keep no more than three simultaneous
mutations. Count all targets against 30 Kubernetes writes/min, 10 Pod execs/min,
and 5 Helm operations/min. Wait for the whole current batch. Stop subsequent
batches after any failed or unknown result.

Autonomous work includes bounded diagnosis, a normal restart of an eligible
ordinary Pod on a healthy source, allowed native-auth rotation, exact temporary
file cleanup, and standard route recovery through Operator reconciliation.
Stuck Helm fault recovery to a known-good revision remains autonomous after
the matching maintenance `Held` handoff. `pending-install` without successful
history uses the version-matched repair helper inside that handoff, never an
uninstall or revision deletion.

Do not blindly delete every non-Running Pod. Pending scheduling, missing
dependencies, storage waiting for a consumer, an unavailable source node,
protected Sandbox, and an active maintenance hold need their own branches.
Wait for Running and Ready to remain true for 60 seconds after a supported
restart. Escalate more than three OOMs in one hour. Resource changes, expansion,
planned upgrades, and batch creation still require confirmation.

On Forbidden, conflict, or an unknown write result, stop that operation. Record
the verb, API resource, target identity, sanitized error, and the missing
permission. Do not retry, widen RBAC, disable admission, or use another identity.
Record every attempted mutation with the audit fields in
`references/operational-constraints.md`, including `actor=cetiadmin` and an
honest `success`, `failed`, `skipped`, `dry-run`, or `unknown` result.

## 5. Prepare delivery to the configured destination

Prepare a short report when there are new problems, repair attempts, permission
gaps, or a notification failure that is confirmed safe to retry. Include the
new failures, attempted actions and outcomes, permission gaps, unchanged
unresolved problems, recovered problems when part of that report, and a platform
summary. Keep healthy, degraded, suspended, and unknown counts separate.
Exclude tokens, complete Secrets, complete logs, and user conversation content.

For `deliver=local`, retain that report locally as the native cron result. Do
not send Slack messages and do not set `slackSent=true`.

For `deliver=slack`, require `slackChannel`, `slackSecretName`, and the injected
native Slack credentials. Do not read the Secret or a retained credential store
to fill a missing binding. If configuration or authorization is missing,
record notification unavailable locally. Never fall back to `origin`, a home
channel, a previous channel, local delivery, or Teams as a replacement target.
Local error evidence is not successful delivery.

Use the native cron final-response delivery, already bound to
`slack:<configured channel>` with the same failure destination. Do not also call
`send_message` for that destination. The pinned tool returns
`skipped=true, reason=cron_auto_delivery_duplicate_target` for that call; its
`success=true` means no send was needed, not that this report was delivered.
Do not bypass that guard or change the job's configured destination.

Before returning a report, find this run's exact native execution ID through
the native execution interface. `cron.executions.list_executions(job_id=..., limit=20)`
can identify the current running execution. Require a unique match to this
job and run; do not guess from the newest unrelated row. Save a pending record
with that ID, the selected channel, included issue keys, and a digest of the
report. Keep `slackSent=false` until step 3 of a later run confirms the send.
If execution identity is uncertain, do not send a report that cannot be
correlated; record the issue locally and request inspection.

Return the report as the final response only after step 6 saves that pending
record. Hermes sends the final response after this task ends, so this run
cannot truthfully record its own future delivery as successful. Native send
failure, timeout, or an unverified acknowledgement is handled by step 3.

Return exactly `[SILENT]` on an unchanged healthy tick, known unchanged issues
already delivered to the selected channel, a report consisting only of pending
unknown sends, or unavailable notification configuration. This suppresses
delivery without changing the job to `local`. The pinned scheduler recognizes
the marker and retains local output for inspection.

Use the following report sections when they contain information:

```text
CetiRun health report: <timestamp>

NEW ISSUES
<target identity, resource, and bounded symptom>

REMEDIATION ACTIONS
<request ID, attempted action, and confirmed or unknown result>

PERMISSION GAPS
<verb, API resource, namespace, and sanitized error>

UNRESOLVED
<known unchanged problems>

RECOVERED
<problems with fresh recovery evidence>

PLATFORM STATUS
<healthy, degraded, suspended, and unknown counts>
```

## 6. Save the outcome

Update `lastCheck`, add new bounded records, preserve unchanged unresolved
records, and remove positively recovered or TTL-expired records. Persist only
delivery outcomes already confirmed in step 3. A new pending report is still
unsent. Keep both byte size and serialized structure valid. Replace only
`monitor-state.json`, using a temporary file in the same private directory and
an atomic rename. Do not touch native jobs, output, notepad, user files, or
owner-private Memory. If state persistence fails, return `[SILENT]` and report
the local failure rather than sending an untracked report.

If there are no new issues, repair attempts, or permission gaps, save the fresh
check time and recovery changes. Unless a confirmed non-delivery or newly
configured channel needs a bounded notification, return exactly `[SILENT]`.
A healthy tick does not produce an external notification.

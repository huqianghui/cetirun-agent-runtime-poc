# Instance lifecycle

Manage ordinary CetiRun Agents through their current CRs. The Operator owns
normal runtime Helm writes, namespaces, credentials, and routing. Use native
kubectl for allowed intent changes and native Helm for inspection; direct
release writes require the [maintenance handoff](remediation-playbooks.md#helm-maintenance-handoff).
Read [operational constraints](operational-constraints.md) before mutations.

## Create one instance or a confirmed batch

1. Obtain the requested aliases, display names, approved Team assignments, real
   owner input, model preferences, and permitted configuration. Use Hermes
   unless another supported runtime is explicitly selected. Do not infer owner
   claims from email or create an owner, Team, identity, or external connection.
2. Validate alias syntax `^[a-z][a-z0-9-]{0,62}$`. Check the exact Agent name,
   intended namespace, and installation binding for collisions. An existing
   name is not permission to adopt it; report conflicts without deletion.
3. Check Operator readiness, current policy, node capacity, storage availability,
   and the selected image/Chart coordinates. Distinguish an unsupported runtime
   or unavailable artifact from a permission problem. Do not change registry
   credentials, perform a cloud login, or disable admission to pass preflight.
4. Construct only the allowed Agent fields from the
   [CRD reference](crd-reference.md). Submit trusted owner information through
   the installed admission contract. Preview the manifest, then create the CR.
   Do not pre-create its namespace, native-auth Secret, or Virtual Key.
5. For more than one instance, obtain batch confirmation and create no more
   than three concurrently. Poll every 10 seconds with a 10-minute timeout for
   each. Finish observing the entire current batch before another starts.
   Stop subsequent batches after any failure or unknown outcome.
6. Verify the requested generation, Ready conditions, actual Pod identity,
   Running and Ready state, matched image/profile, storage binding, and generated
   Service/route. Use `status.accessUrl`, not an inferred alias subdomain.
   A 302 login redirect proves only that part of routing, not owner access,
   model availability, or a successful native operation.
7. Report confirmed creations, pre-existing objects, failures, pending/unknown
   work, and unattempted targets. A timeout does not mean cleanup or rollback.

For a reviewed manifest and an exact target name:

```sh
kubectl --context cetiadmin create --dry-run=client -f "$CETIADMIN_AGENT_MANIFEST"
kubectl --context cetiadmin create -f "$CETIADMIN_AGENT_MANIFEST"
kubectl --context cetiadmin get agents.cetirun.microsoft.com "$CETIADMIN_TARGET_NAME" -o json
```

A client dry run checks construction only. When available, a server dry run
adds schema/admission checks, not an execution guarantee. WaitForFirstConsumer
storage need not be Bound before the first legitimate consumer is created.

## Check platform and instance health

Read Operator, Portal, Istio ingress/control-plane, and CoreDNS status only in
the installation's explicit platform read namespaces. Compare desired and
available replicas, Pod conditions, recent Events, and bounded logs. An absent
optional component can be not applicable; an unreadable component is unknown.
Never modify these platform deployments to repair an Agent.

For each managed Agent, resolve its current UID, namespace UID, actual release,
runtime, generation, and applicable conditions. Inspect its exact Pods,
StatefulSet or Deployment, Services, endpoints, route, and PVC. Check current
and previous container failures rather than assuming the first listed container
is the native runtime. Read disk usage through allowed ordinary-target exec;
PVC requested or reported capacity is not actual free space.

Report platform components and individual targets with their evidence. Separate
healthy, degraded, failed, deliberately suspended, and unknown instances. Include
permission gaps and the actions allowed by the matching remediation branch.

## Restart, suspend, and resume without deleting data

A normal retained restart uses `spec.restartRequestID`. Before a new request,
require current Ready evidence, `status.observedGeneration == metadata.generation`,
and no unfinished restart. Match `spec.restartRequestID` to
`status.observedRestartRequestID`. Check the current runtime's lifecycle support,
policy, and migration/maintenance state.

Use a 16–128 character request ID with ASCII letters, digits, `_`, or `-`, and
conditional JSON Patch tests for the exact UID and resourceVersion. Keep the
same request ID when observing a lost response. Do not generate a new ID to
retry uncertain execution. Read back that request's completion, the current
generation, and Running/Ready stability for 60 seconds.

For example, a reviewed patch file can contain:

```json
[
  {"op": "test", "path": "/metadata/uid", "value": "observed-agent-uid"},
  {"op": "test", "path": "/metadata/resourceVersion", "value": "observed-version"},
  {"op": "add", "path": "/spec/restartRequestID", "value": "restart-example-0001"}
]
```

```sh
kubectl --context cetiadmin patch agents.cetirun.microsoft.com "$CETIADMIN_TARGET_NAME" --type=json --patch-file "$CETIADMIN_PATCH_FILE"
```

Replace the synthetic values with current observations before submission.
Do not patch `status`, finalizers, ownership, or an unrelated spec field.

`spec.suspended=true` requests retained suspension. Verify zero running writers,
route withdrawal, and the applicable model-key block. Preserve Agent, namespace,
and HOME identities. Resume only an authorized suspension whose other start
conditions hold. Do not clear a user pause, external key block, or incomplete
maintenance as a side effect of a configuration change.

The Ready-only retained restart path is not a repair for CrashLoopBackOff.
Use its [failure-specific branch](remediation-playbooks.md#crashloopbackoff),
including ordinary-target healthy-source checks. A protected Sandbox continues
to use its existing supported lifecycle path; no exec/force-delete exception
is introduced.

## Upgrade and roll back paired Chart/image coordinates

1. Confirm this operation's instance list and desired version. Select instances
   from the current Agent API response by Team or explicit names, not namespace
   prefixes. These filters select this batch; they do not define management authority.
   Exclude non-Ready, deleting, migrating, maintained, and user-suspended targets
   unless a separate approved procedure covers their state.
2. Verify the target Chart and matching runtime image/profile. Use the existing
   configured read-only registry access; do not borrow the Operator's Docker
   config, log in to Azure, or publish artifacts. Save each target's prior
   version/image intent and retention evidence privately.
3. Preview the effect with native Helm inspection/rendering. Protect any
   credential-bearing values or manifests. Do not stream them into chat or
   diagnostic Events.
4. Select at least two approved non-critical canaries. Use conditional Agent
   patches to update `spec.helm.version` and matching `spec.agent.image` together.
   Preserve resolved port/username contracts and unrelated configuration. Let
   the Operator perform the runtime upgrade.
5. Check each canary's current generation, actual image, appropriate conditions,
   route, and runtime behavior. Observe the agreed stability window, including
   at least the 60-second Running/Ready recovery check. Pod Ready alone does not
   qualify model calls, owner login, or full operations.
6. After both pass, continue in batches of at most three, waiting for the whole
   batch. Halt immediately on failure or uncertainty. With fewer than two
   targets, report controlled single-instance validation, not a canary-qualified
   batch rollout.

For an already pinned Agent, the update shape is:

```json
[
  {"op": "test", "path": "/metadata/uid", "value": "observed-agent-uid"},
  {"op": "test", "path": "/metadata/resourceVersion", "value": "observed-version"},
  {"op": "replace", "path": "/spec/helm/version", "value": "1.2.3"},
  {"op": "replace", "path": "/spec/agent/image", "value": "registry.example.com/cetirun-hermes:1.2.3"}
]
```

The version and registry in this example are synthetic, not qualified artifacts.
If a parent object or pin is absent, construct an appropriate conditional patch
from the actual CR; do not replace the whole Helm values map and discard other
configuration.

After a failed upgrade, restore both prior Chart and image intent conditionally,
then observe Operator recovery. A proactive non-fault rollback requires
confirmation. If a stuck release needs native Helm rollback for fault recovery,
use the exact `Held` handoff and no-hook/history-preserving flags in the
playbook. Do not run Helm writes concurrently with normal reconciliation.

## Change allowed runtime configuration

Confirm resource limit, storage growth, and other disruptive changes first.
Check the installed CetiAdmin admission allowlist rather than assuming the
untyped Helm map permits every setting. Preserve all unrelated fields and use
UID/RV-conditional patches. Observe the resulting generation and actual running
configuration before reporting success.

| Requested change | Ordinary Agent intent | Boundary |
| --- | --- | --- |
| Memory or CPU limits | `spec.helm.values.resources.limits.memory` or `.cpu` | Confirm; check policy and resulting Pod resources |
| Runtime release | `spec.helm.version` and matching `spec.agent.image` | Confirm planned changes and use the canary process |
| HOME capacity | Growth of `spec.helm.values.persistence.size` plus the bound PVC request | Confirm; never shrink, delete, or change storage ownership |
| Preferred model | `spec.model` within existing authorization | Preserve spend and external blocks; do not inject provider keys |
| Tailscale or registry credentials | Existing typed references | Refer changes to the responsible administrator; do not copy secrets |
| Shared skills, Team, Sandbox, identity, RBAC, mounts | Platform-governed configuration | Do not turn a normal config update into new operations authority |

For a policy-bound or Custom target, use its applicable typed contract. If that
contract does not allow the desired change, report the limitation without
clearing its protection or inventing overrides.

## Plan decommissioning with verified recovery

Confirm the exact Agent and namespace UIDs, affected HOME/PVC/PV bindings, and
retention requirement. Record unfinished maintenance, native sessions, model
spend history, and external connections. Verify recoverable backups before any
destructive action; do not call an untested copy a recovery plan.

Use retained suspension for "stop but keep the instance." Deleting an Agent can
trigger namespace cleanup and remove PVCs. Helm keep annotations do not make
Agent deletion a data-retention guarantee. CetiAdmin cannot execute Agent,
namespace, PVC, or PV deletion.

Another authorized principal performs a separately approved retirement, at most
five instances per batch. Observe resource and route removal, report blockers,
and verify the requested retention or recovery evidence. Do not strip finalizers
or forcibly uninstall releases when cleanup stalls. Use the
[namespace blocker procedure](remediation-playbooks.md#namespace-stuck-terminating).

## Bind the native monitor during runtime startup

The runtime invokes `scripts/cron_sync.py` from the read-only asset bundle before
starting the Hermes scheduler. Use Hermes' installed Python interpreter. The
CLI reads `HERMES_INSTALL_DIR` and `HERMES_HOME`; it does not choose or change
those locations. `--state-dir` optionally chooses a private absolute directory,
defaulting to `HERMES_HOME/cetiadmin`.

```sh
"$HERMES_PYTHON" /opt/cetirun/cetiadmin/scripts/cron_sync.py --config "$CETIADMIN_MONITOR_CONFIG"
```

The file contains no credential values:

```json
{
  "instance": {"name": "cetiadmin", "namespace": "cetirun-cetiadmin", "uid": "observed-agent-uid"},
  "monitor": {
    "enabled": true,
    "schedule": "every 30m",
    "timezone": "UTC",
    "deliver": "local",
    "slackChannel": "",
    "slackSecretName": ""
  }
}
```

`enabled`, `timezone`, and `deliver` are required. A recurring native interval
or cron schedule is required when enabled; one-shot schedules are not monitor
jobs. Empty Slack fields can be omitted for local delivery. Disabled monitoring
can omit schedule and Slack fields. Slack requires an explicit channel ID,
a same-instance Secret reference, and injected `SLACK_BOT_TOKEN` plus
`SLACK_APP_TOKEN`. The helper checks presence, not live authorization.

The runtime must stop startup on a nonzero exit. On success it must export the
returned validated `timezone` as `HERMES_TIMEZONE` before starting the scheduler.
A child CLI cannot modify its parent's environment. Do not rewrite system
timezone, a global Hermes installation, retained `.env`, or user `config.yaml`.
Make sure the scheduler retains that timezone after native configuration loading.
Do not leave an old scheduler running after a rejected configuration.

`cron-binding.json` stores this instance identity, canonical HOME, stable native
job ID, creation phase, and last synchronized timezone. Initial create is
paused until the binding and native readback succeed. Ordinary changes edit
that ID; disabled monitoring only pauses it. Missing jobs, unknown same-name
jobs, foreign binding files, and uncertain creates require inspection. Never
clear the record and retry creation blindly. Preserve output and notepad;
native remove is a separate, explicitly approved retirement after archival.

The `commit` field in the runtime image's `install-stamp.json` must match
`b200f5bc60e31588ba2cb60e288029c4a54ba5fa`. The bridge calls that version's
`cron.jobs` API; it never writes native `jobs.json`. Its JSON response includes
safe status, ID, timezone, and next-run metadata only. Offline fixtures validate
the bridge contract, not the native scheduler or Slack. Use the separate
[monitor prompt](../../../cron/cetirun-monitor-cron.md) for issue deduplication
and delivery receipts.

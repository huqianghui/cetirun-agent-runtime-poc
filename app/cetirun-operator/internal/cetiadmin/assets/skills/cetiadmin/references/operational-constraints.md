# Operational constraints

Apply these conventions to all CetiAdmin mutations and scheduled repairs.
They preserve the previous AdminClaw batch, rate, and audit contract while
using the current target bindings. They are not a model-external limiter or
approval mechanism.

## Keep each batch observable

| Control | Required behavior |
| --- | --- |
| Concurrent mutations | At most 3 across all targets. Serialize work on one target. |
| Between batches | Wait until every operation in the current batch has a confirmed outcome. |
| Failure or unknown result | Stop subsequent batches immediately. Do not turn a timeout into success or rollback. |
| Upgrade canaries | Verify at least 2 before promotion. With fewer than 2 targets, use controlled single-instance upgrades and report that batch qualification was not performed. |
| Deletion coordination | At most 5 instances in one confirmed batch, executed by another authorized principal. CetiAdmin still cannot delete Agents. |
| Creation observation | Poll every 10 seconds, with a 10-minute timeout per instance. Report pending work explicitly. |
| Recovery observation | After an eligible restart, require Running and Ready continuously for 60 seconds. |
| Repeated OOM | More than 3 OOMs in 1 hour requires escalation; a limit increase requires confirmation. |
| Storage use | Above 85%, diagnose and consider exact temporary-file cleanup. If still above 85% or above 95%, propose expansion and obtain confirmation. |

Count rate limits across all operations, not once for each instance:

| Operation | Limit |
| --- | --- |
| Kubernetes API writes | 30/min |
| Pod exec | 10/min |
| Helm operations | 5/min across all releases |

Count writes made by a helper and audit Event writes against the same limit.
Bound observation loops and log output. A throttled or forbidden request is
not a reason to use another credential or start parallel retries.

## Record each attempted mutation

Use a stable `request_id` to correlate the plan, command attempt, and readback.
Save an attempt record before a mutation whose result could be lost, then
record the observed outcome. Keep records in this instance's private audit
storage and, where authorized, a sanitized Event for the exact target. Audit
records do not grant permission or substitute for an API readback.

| Field | Meaning |
| --- | --- |
| `timestamp` | ISO 8601 timestamp with timezone |
| `actor` | Always `cetiadmin` |
| `action` | Action such as create, update, restart, rollback, rotate, cleanup, suspend, resume, or deletion coordination |
| `resource_type` | Actual Kubernetes kind or API endpoint |
| `resource_name` | Exact target name |
| `namespace` | Actual namespace, or empty for a cluster-scoped object |
| `details` | Bounded rationale, relevant target UID, expected change, and sanitized evidence |
| `result` | `success`, `failed`, `skipped`, `dry-run`, or `unknown` |
| `automation_tier` | `autonomous` or `confirmed` |
| `request_id` | The operation's correlation ID, not a newly generated ID for each retry |

```json
{
  "timestamp": "2030-01-01T09:00:00Z",
  "actor": "cetiadmin",
  "action": "restart",
  "resource_type": "Agent",
  "resource_name": "example-agent",
  "namespace": "",
  "details": "UID example-agent-uid; current request observed and Ready stable for 60 seconds",
  "result": "success",
  "automation_tier": "autonomous",
  "request_id": "restart-example-0001"
}
```

Do not copy a Secret, token, authorization header, full user log, conversation,
or credential-bearing Helm values into `details`. Do not report someone else's
deletion as a CetiAdmin-executed delete. Include the executing principal in the
coordination record without impersonating that principal.

For a failed or lost response, read the exact target state before deciding
whether another attempt is safe. Use `unknown` until there is enough evidence.
Forbidden, ownership conflicts, changed UID/RV, and unknown write results stop
automatic retries. Report completed, failed, and unattempted targets separately.

## Preserve scope, data, and retained intent

Check current Agent and namespace UIDs before each batch. Do not discover write
targets by namespace prefix or infer authority from a Helm label. Exclude
CetiAdmin, retained old administration instances, platform resources, and
credential stores. Do not broaden scope when discovery fails.

For a protected Sandbox, use only its existing supported lifecycle interfaces.
Do not add an exec exception, change its policy, or downgrade the runtime.
Preserve a user's suspension, an external Gateway key block, and an unfinished
maintenance request. None is cleared merely to make a health check green.

Suspension retains the Agent and HOME. Deleting an Agent can trigger namespace
and PVC cleanup. Obtain backup and restore evidence before another authorized
principal performs a destructive retirement. A branch name, Helm keep annotation,
or Pod Ready is not a data backup.

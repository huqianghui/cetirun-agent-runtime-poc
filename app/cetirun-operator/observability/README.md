# Opt-in Sandbox reported-status alerts

These standalone Prometheus rule examples use only the public
[reported-status metrics contract](../README.md#sandbox-reported-status-metrics),
the [collector](../internal/controller/sandbox_metrics.go), and the
scrape-generated `up` metric from Prometheus. Nothing loads or deploys them
automatically. They add no scraper, listener, Service, CRD, `PrometheusRule`,
chart integration, or default CI check. Configure authorized scrapes and rule
loads separately. The operator's metrics endpoint is plaintext and
unauthenticated.

**Cached reports are not live enforcement signals.** Each operator replica
reports an independently cached, non-atomic view of the same population. A
successful collection, `up=1`, a `True` condition, or an alert that resolves
does not prove fresh observations, runtime qualification, execution safety, or
containment. There is no observation-age metric in this contract.

## Adapt before loading

Review [the rules](sandbox-alerts.rules.yml) before you explicitly load them
into an authorized Prometheus installation that already exists:

- Replace the synthetic `job="example-cetirun-operator"` selector everywhere.
  Select the **operator**, not the separate admission process.
- At the rule evaluator, supply nonempty `cluster`, `fleet`, `job`, and
  `instance` labels on `up` and on both Sandbox metrics. The full tuple must
  identify one scrape target in one population. `cluster` and `fleet` are
  examples of identity labels that the scraper supplies. They are not labels
  that the operator exports. Prometheus `external_labels` alone do not add
  labels to local query inputs.
- Keep those identity labels identical across all three metrics. Other labels
  can differ. The joins explicitly use the identity tuple. But the alerts keep
  the other labels of their source series. For reported loss, these other
  labels include `runtime`, `class`, `status`, and `reason`. Do not add
  per-Agent or per-object labels.
- Extra labels can distinguish independent populations or monitoring sources.
  If they do, add them to the checks for necessary labels and to **every join
  and expected-target selector**. Make sure that there is at most one `up` and
  one collection gauge for each identity tuple. Multiple population groups for
  each tuple are expected, and their metric dimensions stay separate. Resolve
  duplicate monitoring sources in the monitoring configuration. These examples
  do not infer topology or deduplicate remote-write or HA monitoring replicas.
- Replace the explicit inventory in `CetiRunSandboxTargetMissing`. It expects
  two synthetic replicas in each of two populations:

  | `cluster` | `fleet` | Expected `instance` values |
  | --- | --- | --- |
  | `example-a` | `example-blue` | `operator-0.example.com:8080`, `operator-1.example.com:8080` |
  | `example-b` | `example-green` | `operator-0.example.com:8080`, `operator-1.example.com:8080` |

  All use the example job above. The overlap of job and instance labels across
  populations is intentional. The inventory is illustrative, not an assumption
  about a deployment. The rules monitor extra targets in the selected job while
  these targets are present. But if you do not add expectations, the rules
  cannot detect the complete disappearance of these targets. If you remove a
  target deliberately, update the expectations.
- Keep both metric families when you scrape or relabel. A successful
  collection with no population series means an empty or legacy-only
  population **only under that contract**. With these signals, a filter that
  removes only `cetirun_sandbox_agents` looks the same as an empty population.
- Review the routing, severity, and time settings with the owners of the
  deployment. Every example uses `severity: warning`, a one-minute evaluation
  interval, and a continuous `for: 5m` debounce. Five minutes is illustrative.
  It is not an operational SLO, an observation-freshness limit, or an
  authorized response deadline.

These examples do not sum or average population snapshots, or select the
snapshot of the best replica. A healthy replica or fleet must not mask a failed
one. Route or group notifications as applicable, but do not drop the source
identity from the alert data. The identity-label rule uses
`group without (__name__)` only to show that malformed label sets are present.
It never combines population counts.

## Meaning and response guidance

Inspect only the selected target or population through authorized monitoring,
current status, and trusted operator logs. Do not publish raw logs or object
identifiers.

| Alert or observation | Meaning and conservative response |
| --- | --- |
| `CetiRunSandboxTargetIdentityMissing` | A selected `up` or Sandbox series does not have a necessary identity label. Fix the labels before you interpret joins. Status series that cannot be identified are excluded from the joined alerts. They are not treated as healthy. |
| `CetiRunSandboxTargetMissing` | An explicitly expected target has no `up` series. Examine target discovery, relabel rules, ingestion, and intended removal. This does not show if the operator runs. |
| `CetiRunSandboxScrapeFailed` | `up=0`: the scrape failed. Inspect the scrape error of the target and the approved connectivity or access path. Do not interpret the status samples that are left as current. |
| `CetiRunSandboxCollectionMetricMissing` | `up=1` but the collection gauge is absent for that identity. Examine the target selection, the endpoint and version, disabled metrics, and relabel or filter rules. Population counts are unavailable, not zero. |
| `CetiRunSandboxCollectionUnavailable` | `up=1` and collection success is `0`. Startup or cache initialization, synchronization, or cached reads are unavailable or failed. The counts are omitted. Inspect the operator health, trusted logs, and authorized API connectivity and RBAC. |
| `up=1`, collection success `1`, no population series | An empty or legacy-only reported population, if there is no metric filter. No failure alert. This is not evidence that every intended Agent was discovered or that enforcement is current. |
| Held, suspended or deleting reports | `SandboxPrepared`, `SandboxPreparationPending`, and `EnforcementPending` are not automatically failures. Prepared still means a held preparation with zero replicas. `Suspended`, `Deleting`, and `InstanceDeleting` are intentional lifecycle categories. These examples do not alert on them. |
| Other non-ready or unknown reports | Inspect the current status and the response guidance in the public contract. Absent, stale, or malformed conditions and other reasons are not automatically enforcement loss. No generic non-ready ratio alert or failure alert is supplied. |
| `CetiRunSandboxReportedEnforcementLost` | A populated group has `status=False` or `Unknown`, `reason=EnforcementLost`, and a count **greater than zero**. That same identity also has `up=1` and collection success `1`. Use trusted status, trusted logs, and the qualified response procedure to investigate the affected control. A `True` report with that reason does not meet this rule. |

The loss alert stays per target and per bounded status group, not per object.
Its threshold is exactly `> 0`, not a fleet ratio. One reported Agent is
enough. Collection problems suppress that interpretation and produce the
applicable availability alert instead.

A false or absent expression resets the five-minute pending timer. It also
clears a firing alert on the next evaluation. There is no `keep_firing_for`.
Resolution can mean absent evidence or a changed lifecycle, not restored
enforcement. Availability alerts also have the five-minute debounce. As a
result, a newly unavailable target can temporarily have no firing alert.

Before `absent` or `unless` can detect series that are not present, Prometheus
applies its staleness and lookback behavior to them. This is not a guarantee
of detection within five minutes of a real-world failure. If the rule
evaluator or monitoring system itself is unavailable, these rules cannot
notify. To monitor that system and to keep an expected-target inventory are
separate responsibilities.

Never remove holds or finalizers, edit binding identities, bypass admission,
revoke grants, or restart workloads only because of these alerts. These alerts
do not stop execution and do not establish containment. Follow the qualified
recovery procedure. Keep the access controls in place. For suspected
vulnerabilities, use
[the instructions to report security problems](../../../SECURITY.md), not
public issues.

## Offline validation

Use **Prometheus 3.5.0 `promtool`**. If you do not have it, prepare the public,
checksum-verified release tool separately. No installation or download is part
of these checks. Make sure that this binary is on `PATH`. Then run these
commands from the repository root:

```bash
cd app/cetirun-operator/observability
promtool --version
promtool check rules sandbox-alerts.rules.yml
promtool test rules sandbox-alerts.test.yml
```

The [real rule tests](sandbox-alerts.test.yml) exercise PromQL and alert state
for these cases:

- Zero/one thresholds
- The instant before the five-minute firing boundary and the instant at it
- Pending reset
- Recovery
- Duplicate snapshots from two operator replicas
- Two populations with job and instance labels that overlap
- Unavailable or absent collectors
- Absent targets
- Empty and intentional lifecycle populations
- Heterogeneous labels

Synthetic samples that linger validate that both successful scrape and
collection signals are necessary before the rules interpret reported loss.

These offline tests do not validate scrapes, authentication, deployment,
notification delivery, workloads that run, or enforcement. They do not qualify
these illustrative thresholds for production.

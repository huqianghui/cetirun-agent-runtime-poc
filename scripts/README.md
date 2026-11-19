# CetiRun scripts

The [script reference](../site/content/en/docs/reference/scripts.md) covers
local checks, container builds, cloud deployment, and publication tools. These
include the platform umbrella and the independent Istio helpers.

To validate the charts locally, run these commands from the repository root:

```bash
bash scripts/cetirun-helm-lint.sh
bash scripts/cetirun-helm-test.sh
```

Documentation validation is `make -C site check`. This command does not run
deployment, publication, or end-to-end scripts.

## Default platform deployment and release

`cetirun-platform-deploy.sh --config /path/to/environment.yaml` manages the
separate Istio, Broker, and platform releases. The `--plan` option validates
the inputs and renders the charts locally. The root `make deploy` and
`make deploy-plan` targets pass `DEPLOY_CONFIG` to this helper.
Refer to [the platform guide](../app/cetirun-platform/README.md#deploy-the-platform)
for the input shape, the certificate ownership modes, and staged admission.
This guide also shows which identity and runtime qualification is still
necessary.

The default model routing mode requires the JEV `auto` router and a real
Decision-1 deployment. Set `modelRouting.mode: direct` to deploy callable
shared models while classifier quota is unavailable. Omit router/classifier
entries and give enabled CetiAdmin a configured direct model as its default;
all other deployment checks remain in effect.

`cetirun-release.py` drives the root build and publish targets and the MCR
pipeline from `release-artifacts.json`. Its `list`, `check`, and `plan`
commands do not connect to a registry. `REGISTRY` defaults to the public
`mcr.microsoft.com/aks/ai-runtime/cetirun` prefix; `VERSION` defaults to the
Runtime Chart version, and the source SHA defaults to HEAD. Environment or
CLI overrides affect temporary build/package inputs, not the source checkout.
Selections containing VS Code require an
explicit license acceptance. Publication refuses a tag that already exists
unless `--force` is explicit. It distinguishes registry errors from artifacts
that do not exist, and makes sure that chart pulls work. You can still use each
component build script independently.

The unified release driver and `cetirun-helm-release.sh` normally package the
source Runtime Chart unchanged.
For a new immutable private chart prefix, set both `OPENCLAW_IMAGE_DIGEST` and
`HERMES_IMAGE_DIGEST` to `sha256:` digests. The helper copies the chart to a
temporary staging directory, pins only its built-in native image defaults, and
adds exact `REGISTRY/<runtime>-gateway@digest` entries to the matching runtime
profiles before packaging. The source chart and `VALUES_FILE` behavior remain
unchanged; the values file is used only for linting. Partial, invalid, or
replacement digest inputs fail before publication. The unified publisher also
rejects `--force` when runtime image locking is enabled.

MCR is only a pull endpoint. Local builds and chart packaging may use its
default coordinates, but publishing requires an explicit writable `REGISTRY`.
All publish entry points reject a direct MCR push. Official MCR releases use
the ADO pipeline's backing ACR and automatic synchronization, not `make publish`
against MCR.

Chart publishers share the same packager, registry preflight and mandatory
pull comparison. Packages carry the selected image defaults and the Operator's
downstream Runtime Chart URL/version. MCR publication keeps public pull
coordinates separate from its backing ACR and staging push destinations:
the pipeline's MCR `REGISTRY` assignment is for `package-charts` only, while
all image and chart writes go to ACR.

`cetirun-vscode` is currently the only declared optional image. To defer its
license acceptance, pass `--exclude-image cetirun-vscode` to the release CLI
for every selected build or publication command, and set the matching
`release.excludeImages` list in the deployment configuration. Exclusions of
required, unknown, duplicate, or still-referenced images fail before a build
or deployment preflight.

```bash
python3 -B scripts/cetirun-release.py list images
python3 -B scripts/cetirun-release.py list charts
make artifact-check
python3 -B -m unittest scripts.tests.test_release_inventory scripts.tests.test_full_deployment -v
```

The tests use command doubles and local Helm. They do not deploy or publish.

## Public DNS and TLS preparation

`cetirun_platform_deploy.py` handles optional public DNS and HTTP-01 TLS as a
phase of the normal deployment. Set `publicTLS.enabled: true` in the same
absolute `DEPLOY_CONFIG` file used by `make deploy`. `make deploy-plan` renders
the certificate-only umbrella values without external calls, `make deploy`
prepares DNS/cert-manager/certificates before the normal releases, and
`make deploy-verify` reads the finished public endpoint. The upstream
cert-manager controller remains a separately managed Helm release; its pinned
support assets are in [cert-manager](../infra/cert-manager/). See
[public DNS and TLS](../site/content/en/docs/operations/public-ingress.md) for
the configuration and connected-operation boundary.

## CetiRun Connections MCP artifacts

`cetirun-connections-mcp-docker-build.sh` builds the standalone MCP image from
the repository root. Override `REGISTRY` to use a test registry or repository
prefix instead of public MCR. The default value of `VERSION` comes from
the central release metadata. `PUSH=false` loads a local image, and `PUSH=true`
publishes the image.

`cetirun-connections-mcp-helm-release.sh` lints and packages the independent
chart. It pushes the chart under `oci://$REGISTRY/helm`.
`CHART_NAMESPACE` must be `helm`; put repository prefixes in `REGISTRY` so
the Operator can derive the same image repositories from its Runtime Chart URL.
Set `REGISTRY` to an authorized writable registry; the default MCR address
is valid for packaging, not publication.
Then it compares the pulled package with the published package. Pull verification
cannot be disabled with `VERIFY_PULL=false`.
Neither helper logs in or deploys resources. Image builds can download pinned
dependencies and base images. For publication, explicit authorization and
registry authentication are necessary.

The root build and publish targets include these artifacts. These helpers do
not add the artifacts to the platform umbrella or to the Agent runtime. To
validate offline without Docker or registry access, use these commands:

```bash
make -C app/cetirun-connections-mcp test vet chart-lint
python3 -B -m unittest scripts.tests.test_connections_mcp_chart -v
```

## Explicit-target application E2E lifecycle

`e2e-test.sh` is a connected application smoke test, not an offline check.
Run it only after you get authorization for the exact environment and its test
resources. It now must have an absolute `E2E_KUBECONFIG`, an explicit
`E2E_CONTEXT`, and an `E2E_CLUSTER_UID` that you get independently (the UID of
the `kube-system` namespace).

Use a trusted, pre-provisioned HTTPS kubeconfig without exec or auth-provider
login plugins. Do not change that file or context during a run. Every kubectl
and Helm invocation carries the explicit target. The script does not use the
ambient current context.

```bash
E2E_KUBECONFIG="$TEST_KUBECONFIG" \
E2E_CONTEXT="$TEST_CONTEXT" \
E2E_CLUSTER_UID="$TEST_CLUSTER_UID" \
AGENT_TYPE=openclaw bash scripts/e2e-test.sh
```

The script generates the default alias for each invocation. If you supply
`TEST_ALIAS`, both the alias and its derived namespace must be absent.
Resources that already exist, forbidden reads, and failed observations stop the
run. There is no leftover adoption, pre-run deletion, `apply` takeover, or
forced namespace cleanup.

Creation records the API-issued Agent UID and a run label. Suspend and resume
use JSON Patch tests for the current UID and resourceVersion. The script does
not automatically rebase or retry conflicts.

Cleanup examines the cluster and Agent ownership and any namespace owner
reference again. It submits at most one conditional Agent Delete. Then it
observes the absence of both the Agent and the namespace through normal
controller finalization. The script reports a replacement identity, an unknown
response, a read error, or a timeout as an incomplete cleanup, and exits with a
failure. It never removes finalizers or deletes a namespace directly.

`READY_TIMEOUT` (default 600 seconds) and `CLEANUP_TIMEOUT` (default 180
seconds) accept positive values up to 9999. Individual Kubernetes requests use
a 15-second request timeout, which can extend the final poll iteration beyond
its deadline. The script keeps an ambiguous creation without a returned UID for
explicit recovery, and never adopts it by its name or label.
`SKIP_CLEANUP` keeps resources only after successful application checks. The
script reports this explicit choice, which can incur charges. Keep the alias
and the private run output for an authorized cleanup.

The application checks keep their scope. This does not implement governed
creation, prove physical writer fencing, qualify enforcement, or complete a
real model or tool acceptance run. The Hermes check still uses Pod exec and is
not a workaround for governed-Pod admission. All six live workflow jobs stay
disabled. Before you enable the test job independently, provision its
`E2E_CONTEXT` and `E2E_CLUSTER_UID` secrets together with its kubeconfig,
which already exists. The job uses a task-local file and does not overwrite
the runner's default kubeconfig.

The offline lifecycle regressions replace kubectl and Helm with synthetic
processes:

```bash
python3 -B -m unittest discover -s scripts/tests -p test_e2e_lifecycle.py -v
```

### Offline lifecycle evidence assertions

`sandbox-platform-qualify.py acceptance-check --receipt FILE` examines a local,
sanitized JSON receipt. It uses the same bounded, strict JSON parser and fixed
reason codes as the qualification helper. It does **not** collect evidence, run
another lifecycle runner, read Kubernetes configuration, sign in, call a model,
or change resources. The receipt fields are validation inputs. They are not new
Agent status fields, annotations, or API requirements.
For a narrow collector of current resources, refer to
[read-only lifecycle snapshots](#read-only-lifecycle-snapshots) below.

Run the offline regression entry point from the repository root:

```bash
PYTHONPATH=scripts/tests python3 -B -m unittest \
  test_lifecycle_acceptance test_lifecycle_snapshot test_e2e_lifecycle \
  test_sandbox_platform_qualify test_sandbox_deployment_preflight -v
make python-check license-check
```

The `make ci-test` entry point also discovers these tests. The
[synthetic fixture](tests/fixtures/lifecycle_acceptance.json) intentionally has
no authorization or observations:

```bash
python3 -B scripts/sandbox-platform-qualify.py acceptance-check \
  --receipt scripts/tests/fixtures/lifecycle_acceptance.json
```

**Expected result: exit 1, `status: blocked`.** The executable
[test receipt builder](tests/test_lifecycle_acceptance.py) adds synthetic
measurements for positive and negative assertions. These measurements are never
live evidence. No test sends fabricated identity headers to a service.

#### Coverage and support boundary

This is an **assertion coverage matrix**, not a new runtime support declaration.
Before you collect a receipt, you must examine the product availability. Examine
it on the exact selected source, images, charts, feature configuration, and
platform tuple. In particular, retained native operations and native
Copilot/helper combinations must have an integrated common-execution candidate.
This main-based helper does not deliver that candidate or enable its features.

| Dimension | Fixture coverage | Connected acceptance |
| --- | --- | --- |
| OpenClaw / Hermes, ordinary / retained `sandbox-native`, and shared / Copilot-only / mixed | All 12 core tuples | Blocked until the exact supported artifact, target, owners, and providers are authorized |
| Each core tuple with no optional identity / Entra OBO / Azure app-only, and with each of skills / Memory / doctor / logger / desktop | 180 single-helper assertion tuples, plus a combined-helper case | Conditional on actual runtime integration and helper/identity availability. No native parity is inferred from ordinary support |
| Owner create, restart, suspend/resume, and delete. Native HTTP and WebSocket | Necessary owner-path and distinct-owner negative assertions | Blocked without real OIDC owner and peer sessions and available owner controls |
| Healthy-source native migration | At least two administrator-only moves, each with source termination and CSI release/attach observations | A separate bounded authorization is necessary. There is no owner migration action |
| Custom images, gVisor, failed/unreachable-source recovery, physical fencing | Not covered. Unknown runtime/execution values are blocked | Outside this checker. No support is implied |

Each tuple must have its own receipt. Adjacent rows do not qualify these items:

- Helper combinations beyond the tested single-helper rows and combined case.
- Other platforms.
- Optional identity integrations.

The [Harness contract](../site/content/en/docs/reference/agent-harness.md),
[identity availability](../site/content/en/docs/guides/agent-identity.md), and
[protected renderer](../app/cetirun-chart/README.md#protected-sandbox-rendering)
are still the product support sources.

#### Required observations

The fixture supplies the v1 envelope. The test builder shows the complete
snapshot, operation, request, and cleanup shapes. The reusable
`assess_lifecycle_receipt` function returns ten independent check results:

| Check | Necessary evidence and rejected substitutes |
| --- | --- |
| `artifacts` | Full source commit **and tree**, digest-pinned component images and charts, and a digest of the reviewed non-secret configuration/tuple manifest. Every snapshot, request case, and cleanup must carry the same recomputed fingerprint of the artifacts **and case**. Tags, a relabeled tuple, a shared run label, or receipts from different builds are not sufficient. |
| `authorization` | Explicit target, owner, peer, provider, bounded spend and fault authorization, and owner-control availability. Prerequisites that are absent stay blocked. `true` in a file does not grant permission. |
| `owner-access` | Real OIDC owner REST/Portal access, stable distinct owners, denied peer reads/mutations, and native HTTP 200 and WebSocket 101. Administrator patches, unauthenticated redirects, and synthetic identity headers cannot replace these observations. |
| `home` | Retained Agent/namespace/StatefulSet/PVC/PV identities, a nonempty known synthetic file checksum manifest, contents that match, and continued read/write through an allowed owner/application path after every operation. PVC identity alone, or protected Pod exec, is not sufficient. |
| `accounting` | The same key and budget. Strictly positive spend before the journey. Cumulative spend that does not decrease across operations. More spend from each later model call within the budget. Decimal strings avoid floating-point comparisons. `0 -> 0`, a key replacement, or a budget reset causes a failure. Copilot-only must not silently use a shared priced model. The absence of an authorized priced path is a blocker. |
| `recovery` | Separate apply/request acceptance, stop request, observed shutdown/source termination and actual current-workload Ready witnesses bound to the retained Agent UID, current generation and Pod UID. Every operation identifies the previous source Pod/node. After every operation, each selected model source must have a reply and a fresh, independently examined tool challenge. The reuse of an earlier checksum causes a failure. Native moves also must have a healthy source, distinct destination and CSI witnesses. Generation or a status label alone cannot prove completion. |
| `peers` | Another owner's independent Agent, Pod, HOME PVC/PV, key, and budget stay unchanged and Ready through cleanup. Peer spend cannot decrease. No recovered owner Pod or retained HOME volume can be the peer's. An unused peer can have zero spend. |
| `requests` | Duplicate, stale UID/version, unknown reply, same-version takeover, suspend/delete precedence, and native in-flight replacement/destination-failure cases. Keep the original request fingerprint and accepted operation's ID/Agent UID/generation/source Pod, bound time and apply counts. Compare the reported state with authoritative, current-workload observations. Completion must have a nonzero apply count and a fresh Ready Pod. Pending/retryable and terminal failure stay distinct. |
| `cleanup` | Ordinary owner deletion of the same retained Agent UID, independently observed absence of Agent/namespace/HOME volumes/attachments/control records/reservations/key, and retained peer health. A retained suspended Agent, an unobserved timeout, or a forced finalizer/detach operation is not cleanup. |
| `extensions` | For selected helpers: read-only unchanged skills, private usable Memory, ready doctor/logger, and owner-only desktop. Optional identity adds authorized use, peer denial, and revocation observations. Azure app-only also must have the MCP path. A connection configuration alone does not prove these facts. |

For object/owner/key comparisons, use local consistent pseudonym digests, never
raw identities, credential hashes, tokens, cookies, endpoints, conversations, or
diagnostic bundles. HOME/tool entries are bounded synthetic witness labels and
checksums, not user files. Artifact fingerprints are SHA256 over
`{"artifacts": artifacts, "case": case}` serialized as sorted-key compact JSON,
with `case.helpers` sorted. Use `lifecycle_artifact_fingerprint` instead of an
invented label. At every observation, examine the deployed pins again. The
checker cannot authenticate the claims of a collector.

These fields bind the measurements. They are receipt projections, not new
product API fields:

| Record | Binding |
| --- | --- |
| Snapshot `resources` | `agent`, `namespace`, `statefulSet`, `homePvc`, and `homePv` are consistent pseudonym digests of the retained Kubernetes UIDs, not names. |
| Snapshot `workload` | `agent` equals `resources.agent`. `pod` and `node` identify the current execution. `generation` is its selected Agent generation, not a local step counter. `observedGeneration` must equal it, and `observedPod` must equal `pod`. `ready` must be `true`. Successful recovery snapshots must have fresh Pod UIDs and generations that increase. |
| Operation | `sourcePod` and `sourceNode` must equal the workload of the previous snapshot. All observation flags are JSON booleans. This also applies to CSI flags on non-move operations. When those observations are not applicable, use `false`. |
| Model witness | `expectedToolChecksum` pins a new synthetic challenge for that source and observation. `observedToolChecksum` is read independently of the model reply. Both must match. The checksum must not be in any earlier model witness. |
| Peer | `home` is the UID digest of the peer HOME PVC. `homePv` is the UID digest of the PV that backs it. Both stay unchanged and are different from the owner's retained volumes. |
| Request `operationBefore` / `operationAfter` | Each has exactly `id`, `agent`, `generation`, and `sourcePod`. The ID, Agent UID, and source Pod UID are pseudonym digests. The generation is a positive integer. Both records must be identical and must belong to the retained Agent. For rejected retries, observe the original protected operation, not an invented replacement for the rejected request. |
| Request `workload` | Exactly `agent`, `generation`, `observedGeneration`, `pod`, `observedPod`, and `ready`. These fields are bound to the original operation's Agent/generation. Pod fields must match. When no current Pod is observed, both are `null`. Ready must have the current generation and a Pod. Completion also must have a Pod that is different from the original source. Suspended/deleted outcomes must have no current Pod. Pending/failed outcomes cannot report Ready. |
| Request failure | `retryable` is `true` only for a pending outcome. `failureObserved` is `true` only for the destination-failure case. This is also true when that case stays pending/retryable. These are independent observations, not an inference from submission acceptance or timeout. |
| Cleanup | `agent` identifies the retained Agent that is deleted. All `remaining` values are observed integer zeroes, never booleans or an absence inferred after an unsuccessful read. |

To print a complete, machine-readable **synthetic** shape without evidence
collection or a lifecycle run, use this command:

```bash
PYTHONPATH=scripts/tests python3 -B -c \
  'import json; from test_lifecycle_acceptance import receipt; print(json.dumps(receipt(), sort_keys=True))'
```

The builder accepts `runtime`, `execution`, `model`, `identity`, and `helpers`
arguments for the matrix above. Never relabel this example as live evidence.
In older receipts, binding fields that are absent stay blocked. Do not fill
them with invented identities or observations to get a match.

An unknown submission must keep its original intent until an authoritative
observation shows pending or completed work. Absence is not rejection or
cancellation. Confirmed stale requests stay rejected, not silently rebased. A
pending failure can match its bounded negative-case assertions, but it is not a
successful migration. `retryable` describes internal operation recovery, not
permission to replay an unknown submission.

A retained stop is not destruction and not physical writer exclusion. For fault
injection, reuse the controller/unit/envtest cases that already exist. Do not
add a second implementation here.

The output contains only validated case enums, the artifact fingerprint, fixed
check names/reasons, and the supplied evidence kind. It never echoes input
paths, observed resource identifiers, or exception text. `status: matched` and
exit 0 mean only that the **reported scoped measurements** meet these
assertions. `qualifiesExecution` is always `false` and `liveAcceptance` is
always `not-established`, also for receipts that are labeled `live`.
Independent evidence review and the complete supported matrix are still
necessary.

Absent evidence returns `blocked`, and contradictory evidence returns `failed`.
Both exit 1. Malformed command lines exit 2. The input must be an explicit local
regular file (not a pipe or device). The input is capped at 1 MiB. Duplicate
JSON fields and nonfinite numbers are rejected.

Keep live receipts and private validation reports outside public PRs. Never
merge historical versions into a new pass.

### Read-only lifecycle snapshots

`sandbox-platform-qualify.py lifecycle-snapshot` collects a small set of current
Kubernetes facts for **one explicitly selected Agent and run**. It is not a
lifecycle runner, an acceptance receipt, or authorization to use an environment.
Run it only after you separately authorize read access to that exact target:

```bash
python3 -B scripts/sandbox-platform-qualify.py lifecycle-snapshot \
  --kubeconfig "$TEST_KUBECONFIG" --context "$TEST_CONTEXT" \
  --cluster-uid "$TEST_CLUSTER_UID" --scope "$REVIEWED_SNAPSHOT_SCOPE"
```

The default is `--state ready`, with the ready input and output contract that
already exists. To collect an **already suspended** instance, add
`--state suspended`. This selector never requests suspension. Deleted states and
states during a migration are not supported.

Supply a trusted, absolute, static-credential kubeconfig and pins that you get
independently. Never adopt discovered resources or the ambient current context.
Both local inputs must be regular files, not symlinks, directories, devices, or
pipes. Keep real inputs private. The scope JSON is capped at 64 KiB and has
**exactly** these fields:

| Field | Required input |
| --- | --- |
| `version` | `"v1"` |
| `run` | One explicit, nonempty DNS-style run-label value, at most 63 characters |
| `case`, `artifacts` | The acceptance receipt shapes above, with full source commit/tree, configuration digest, and all necessary image/chart digests. No tags |
| `resources` | The seven entries shown below. No other discovery inputs |

This example is a **synthetic `resources` fragment**, not a runnable live
scope:

```json
{
  "agent": {
    "name": "selected-agent", "uid": "agent-uid",
    "resourceVersion": "1", "generation": 3
  },
  "namespace": {"name": "snapshot-example", "uid": "namespace-uid"},
  "statefulSet": {"name": "selected-runtime", "uid": "statefulset-uid"},
  "pod": {"name": "selected-runtime-0", "uid": "pod-uid"},
  "homePvc": {"name": "selected-home", "uid": "pvc-uid"},
  "homePv": {"name": "selected-pv", "uid": "pv-uid"},
  "attachment": {"name": "selected-attachment", "uid": "attachment-uid"}
}
```

When no attachment identity is pinned, use `attachment: null` explicitly. The
CSI attachment then stays unknown, and the collector exits 1. It never discovers
attachments cluster-wide or interprets the absent pin as absence/release.

With an explicit `--state suspended`, `pod: null` is also allowed. A supplied
Pod pin stays an input identity, not an observed Pod. In both cases, the
collector must observe zero Pods in the entire owned namespace.

Ready mode must still have the Pod name and UID. All seven resource entries are
still mandatory, and every other entry must have its name and UID. The Agent
generation and resourceVersion are also mandatory. The collector does not
refresh replaced or stale pins automatically.

The supported collection footprint is deliberately strict:

- The Agent and its controller-owned namespace must both carry the exact
  `qualification.cetirun.microsoft.com/run` label. Workload/storage objects are
  scoped by the pinned identities and ownership/binding chain. Any run label
  that they carry must agree. The namespace must be Active, and the Agent must
  report the selected runtime and exact pinned `status.observedGeneration`.

  Ready mode must have an unsuspended intent and phase `Ready`. Suspended mode
  must have `spec.suspended: true`, phase `Suspended`, and exactly one each of
  the controller's completed-suspension conditions. These conditions are
  `Ready=False` with reason `InstanceSuspended`, plus `PodReady=False` and
  `IngressReady=False`, both with reason `Suspended`. A suspension request, a
  stale generation, or the intermediate Sandbox hold's `Ready=False/Suspended`
  condition is not sufficient.

  The completed controller path stamps the top-level generation, not each
  condition's `observedGeneration`. These status reports are not independent
  shutdown proof. No owner authentication is inferred from the Agent's user
  information or from administrative access.
- There must be exactly one StatefulSet in that namespace, controller-owned by
  the Agent, with current `status.observedGeneration`. In ready mode, it must
  have one current Ready replica. The main Pod must have the pinned UID, that
  StatefulSet controller owner, a selector/revision that matches, Running/Ready
  status, and one ready `cetirun` container. Both modes must have precisely the
  chart's `app.kubernetes.io/name` and `app.kubernetes.io/instance` selector
  labels. The namespace-scoped StatefulSet list and ready mode's
  selector-scoped Pod list each use `limit=2`. Empty, multiple, paginated, or
  truncated results stop ready collection.
- Suspended mode must have explicit integer zero `spec.replicas` and
  `status.replicas`. When the optional
  current/ready/updated/available/terminating replica counters are present,
  they must also be integer zero. Kubernetes can omit optional counters that
  are zero.

  In suspended mode, the Pod list is **namespace-wide**, without a label
  selector, and still uses `limit=2`. It must return a complete empty list with
  a resourceVersion, no continuation, and no items that are left, both initially
  and on recheck. Any Pod blocks collection. This includes a
  terminating/completed Pod or a Pod with absent/stale labels. An absent,
  forbidden, malformed, or truncated list never proves absence. The collector
  does not make a named Pod GET and does not adopt a discovered identity.
- The template must declare the digest-pinned main image that matches
  `artifacts.images.runtime`. Ready mode must also have the same Pod
  declaration and a main-container `imageID` digest that matches. A different
  platform-image digest is **unknown**, not silently adopted from a
  multi-architecture tag/index. Suspended mode examines only the declaration.
  In this mode, old/new workload revisions do not have to match, and the mode
  does not claim a current runtime `imageID`. No registry lookup occurs.
- The template, and in ready mode the Pod, must explicitly mount volume `data`
  from the pinned HOME PVC at `/home/vibe`. This mount must not use subpaths or
  a read-only declaration. Implicit claim-template layouts are outside this
  slice. PVC and PV must be Bound, filesystem-mode, and linked by the exact
  claim UID/name/namespace.

  Only the explicitly named CSI VolumeAttachment is examined. This examination
  includes its UID, PV, driver, reported attached flag, and absence of reported
  errors. Ready mode must also have the node name of the independently read
  Pod. Without a Pod, suspended mode only validates the shape of the
  attachment's node name, and never adopts that name as a current runtime node.
  An examined attachment record still does not inventory other attachments or
  prove physical attachment, detachment, writer exclusion, or actual HOME
  read/write. **Zero replicas and zero Pods are only Kubernetes observations**,
  never physical writer exclusion, CSI release, or cleanup.

This mode reuses the deployed-preflight POSIX runner. The runner has a
5-second request timeout, a 10-second command deadline, a 90-second total
deadline, and a 64-command ceiling. It also has a 1 MiB stdout cap per command
and a separate one-second process-cleanup wait.

Only redacted local `kubectl config view --minify` and allowlisted API GETs run.
These GETs are for the pinned `kube-system` namespace, Agent, namespace,
StatefulSet, ready-mode Pod, HOME PVC/PV, optional VolumeAttachment, and the two
bounded lists above. The mode never reads Secrets, Nodes, application logs, or
HOME contents. No exec, attach, port-forward, proxy, watch, mutation, login,
model call, or retry is available. Credential plugins, token-file/impersonation
indirection, and external client-key/certificate files are rejected before API
reads. Do not replace the trusted kubeconfig or `kubectl` during collection.

The collector rechecks every selected object and candidate set that it reads
successfully. It also rechecks the redacted context selection and kubeconfig
file identity. Drift in the UID, resourceVersion, content, run label, or
configuration blocks the snapshot. In suspended mode, an unavailable/uncertain
attachment read or recheck can leave the other stable projections intact. In
that case, the attachment is unknown and the overall result is blocked. The
collector does not retry failed initial reads.

Replacement, binding mismatch, terminating resources, or observed content drift
still block the whole snapshot. This also applies to attachments. A
process-cleanup failure, exhausted budgets, and cancellation cannot produce a
snapshot. These checks detect observed changes. They are not an atomic API
snapshot or a promise of future state. Failed/forbidden GETs never prove absence
or cleanup.

The collector emits one sanitized JSON object below 8 KiB. Its `checks` use the
`observed` / `unknown` / `blocked` statuses that already exist, and fixed reason
codes:

- Exit 0 and `status: observed` mean only that the selected current-object
  checks matched. `qualifiesExecution` stays `false`, and `liveAcceptance` stays
  `not-established`. Exit 1 reports a blocker/unknown. Malformed arguments exit
  2, and cancellation exits 130.
- Only an explicit suspended selection adds the top-level `state: suspended`.
  Ready output stays unchanged.
- Suspension blockers use `agent-not-current-suspended`,
  `agent-suspension-not-observed`, `statefulset-not-current-zero`, and
  `pod-candidates-not-empty`. They also use the shared fixed reason codes that
  already exist.
- After stable reads, `snapshot` reuses the receipt's `artifactSet`,
  `resources`, and `workload` projections. Resource UIDs, the Pod UID, node
  **name**, and cluster/context/run scope are pseudonymized without raw names,
  identifiers, endpoints, paths, credentials, or diagnostics. Each pseudonym is
  the SHA256 of the compact JSON `[clusterUID, role, value]`. When you assemble
  later observations, keep the same convention. Node-name equality does not
  prove node/provider identity.
- In ready mode, `workload.observedPod` identifies the independently read Pod,
  but `observedGeneration` and `ready` stay `null`. The controller's
  `reported.agentObservedGeneration` and the independently read
  `reported.podReady` do not prove generation-bound runtime readiness.
- In suspended mode, `workload.pod` is either the pseudonym of the supplied pin
  or `null`. `observedPod`, `node`, `observedGeneration`, and `ready` are all
  `null`. `reported` adds `agentSuspended: true`, `agentPhase: Suspended`,
  `desiredReplicas: 0`, `currentReplicas: 0`, and `podCandidates: 0`.
  `reported.podReady` and `reported.runtimeImageID` are `null`. No current Pod,
  node, or runtime image identity is inferred from the stopped template.
- `storage` reports only the examined bindings/attachment, and `csiReleased` is
  always `null`. With no attachment pin, the other stable projections are kept,
  the attachment check is unknown, and the overall result is blocked. Suspended
  mode also keeps that partial shape for unavailable/malformed attachment
  responses, an absent/false/uncertain attached flag, or reported attach/detach
  errors. `storage.attachment` and `csiAttached` then stay `null`. An
  unattached or absent record is not a successful release. Other failures leave
  `snapshot: null`, and earlier checks are only partial reads.
- `owner`, `home`, `accounting`, `models`, `peer`, and `extensions` stay `null`.
  No owner/provider authorization, model/tool witness, HOME checksum, nonzero
  spend, peer continuity, source shutdown, or cleanup proof is manufactured.

The artifact fingerprint binds the **supplied** case/pins. It is not proof that
the source, charts, other component images, or effective case are deployed.
Those facts and collector authenticity stay explicitly `unverified`, and so do
the acceptance observations above. An edited JSON file or a successful
administrator snapshot is not real-owner acceptance, billing evidence, a signed
attestation, or live qualification. Independently authorized collection and
review of the other evidence are still necessary.

The offline regressions replace every kubectl subprocess and use synthetic JSON:

```bash
PYTHONPATH=scripts/tests python3 -B -m unittest \
  test_lifecycle_snapshot test_lifecycle_acceptance \
  test_sandbox_deployment_preflight test_sandbox_platform_qualify test_sandbox_udp_probe
make python-check license-check
```

### Native ingress sign-in challenge

The application runner uses `e2e-ingress-check.py` to request the canonical
`https://<domain>/ui/<agent>/` route, not an instance subdomain. `DOMAIN` is an
explicit override of the operator release's `domainSuffix`. Absent/unreadable
values cause a failure, and the runner does not fall back to an example domain.
`E2E_OIDC_CLIENT_ID` can supply the expected public application identifier.
Otherwise, the runner reads `clientId` from the Istio release of the selected
cluster.

The helper sends one unauthenticated GET with certificate and hostname
verification. It does not read an Agent Secret, send a token/cookie, follow a
redirect, or contact the identity provider. It also does not use ambient HTTP
proxies or enable ambient TLS key logging. System CA trust is the default. When
the approved target must have a private test CA, `E2E_CA_FILE` selects an
absolute PEM CA bundle. There is no insecure verification option.

For a separately authorized HTTPS target, the helper can also run directly:

```bash
python3 -B scripts/e2e-ingress-check.py \
  --origin "$TEST_HTTPS_ORIGIN" --agent-name "$TEST_AGENT_NAME" \
  --client-id "$EXPECTED_OIDC_CLIENT_ID"
```

It accepts only HTTP 302 with a single Location. This Location must name the
exact public `https://login.microsoftonline.com/<tenant>/oauth2/v2.0/authorize`
endpoint, the expected client ID, `response_type=code`, and the exact
`<origin>/oauth2/callback` redirect URI. Absent/duplicate/mismatched query
parameters cause a failure. Other statuses, for example 200, 401, and 403, are
not evidence that the expected sign-in challenge is wired correctly.
Sovereign-cloud issuers, intermediate redirects, and other layouts are outside
this narrow check.

Accepted response headers are limited to 16 KiB, and the Location is limited to
8 KiB. Python's HTTP parser also applies its native line/count limits. The
helper does not read the body. Socket operations have a ten-second timeout. The
POSIX command has a 30-second worker deadline, and this deadline also covers
DNS/TLS/header collection.

A separate supervisor enforces that deadline. It does not rely on Python signal
delivery inside the resolver. On timeout or SIGINT/SIGTERM, it kills the worker.
Before it returns, it waits up to one more second to confirm that the worker is
reaped. Unavailable process isolation, an abnormal worker exit, or unconfirmed
cleanup makes the check fail.

The helper must have POSIX `fork` support. The output is one small JSON object
with fixed reason codes. It never contains the origin, response headers,
cookies, redirect state, identifiers, or raw transport diagnostics.

Exit zero means only `status: observed` for that unauthenticated challenge.
`authenticatedAccess` stays `not-tested`, and `qualifiesExecution` stays
`false`. Owner sign-in, authorized native HTTP/WebSocket access, downstream
application readiness, and model/tool execution must have separate acceptance.
A transport/validation failure exits 1, invalid inputs exit 2, and cancellation
exits 130/143. On failure, the runner keeps its ordinary conditional cleanup.

The offline tests use response doubles and ephemeral local TLS
servers/certificates:

```bash
PYTHONPATH=scripts/tests python3 -B -m unittest \
  test_e2e_ingress_check test_e2e_lifecycle -v
```

On Linux/glibc, a local C compiler (`cc`) also enables native `getaddrinfo`
regressions for the deadline and SIGINT/SIGTERM cleanup. A temporary fixture
directs only the resolver of the worker to a loopback DNS socket that does not
respond, with 30-second retries. These tests do not change the host resolver
configuration or contact external DNS. Without Linux/glibc or `cc`, these tests
are explicitly skipped.

## Read-only deployed Sandbox preflight

`sandbox-platform-qualify.py deployment-preflight` inspects a small, explicitly
selected deployment of the Operator chart that already exists. It is separate
from the older RuntimeClass/storage/Cilium `preflight` and the mutating
`network-smoke`. It does not start an Agent, qualify a provider, or authorize
execution. Stock startup leaves `WriterExclusion` unavailable and execution
held. If you enable Kubernetes sources, this state does not change.

Run this connected command only after authorization for the named environment.
Supply a trusted, static-credential kubeconfig, an explicit context, and the
expected `kube-system` namespace UID. Also get the Deployment and
ServiceAccount UIDs and SHA256-pinned image references independently, and
supply them. Do not implicitly adopt anything that the preflight discovers as a
pin. Keep real inputs local. The helper does not log in or invoke credential
plugins.

```bash
python3 -B scripts/sandbox-platform-qualify.py deployment-preflight \
  --kubeconfig "$TEST_KUBECONFIG" --context "$TEST_CONTEXT" \
  --cluster-uid "$TEST_CLUSTER_UID" --namespace cetirun-system \
  --operator-deployment cetirun-operator --operator-uid "$OPERATOR_UID" \
  --operator-image "$PINNED_OPERATOR_IMAGE" \
  --operator-service-account cetirun-operator-controller-manager \
  --operator-service-account-uid "$OPERATOR_SA_UID" \
  --admission-deployment cetirun-sandbox-admission --admission-uid "$ADMISSION_UID" \
  --admission-image "$PINNED_ADMISSION_IMAGE" \
  --admission-service-account cetirun-sandbox-admission \
  --admission-service-account-uid "$ADMISSION_SA_UID"
```

The supported footprint is deliberately narrow:

- A stable Kubernetes version of at least 1.37 (prereleases are blocked).
- The selected Deployments in one explicit namespace, distinct pinned service
  accounts, and exact digest-pinned images for the native
  `manager`/`admission` containers and their process modes. Desired and
  observed rollout generations and replica counts must agree. Operator replicas
  are bounded to 1-4, and admission replicas are bounded to 2-4. This helper
  does not wait for rollouts.
- Inline `CETIRUN_SANDBOX_SOURCES` and `CETIRUN_SANDBOX_ADMISSION`,
  controller/admission identities that match, the controller state namespace,
  strict admission, explicit administrator lists, and distinct HOME/skills
  claim names. Platform and placement ConfigMap references must agree between
  the two processes. The helper examines only the identities of the named
  ConfigMaps and the bounded `platform.json` / `placement.json` v1 document
  envelopes. Their declarations, capabilities, and references are **not**
  validated as runtime evidence.
- The chart's fixed `cetirun-sandbox-admission` Service, its selector and
  443-to-9443 listener, readiness probe, and TLS Secret **reference**, plus its
  exact-name core/v1 Endpoints object. The helper does not fetch a Secret. Two
  to four ready endpoint Pods must keep their UIDs. They must match the
  selected admission image, service account, and relevant process
  configuration, and trace through named ReplicaSets to the selected Deployment
  UID. Their reported node names must span at least two nodes. The helper does
  not read or qualify any Node.
- The chart's fixed validating/mutating `cetirun-sandbox-admission` webhooks
  and validating `cetirun-sandbox-pods` / `cetirun-sandbox-placement` webhooks.
  This footprint covers their service/path/CA-encoding wiring, CA bundles that
  match, resource rules and selectors, fail-closed settings, and bounded
  request timeouts. The explicit `ms` or `s` request budget of the admission
  process must be shorter than each webhook timeout.

When `sandboxSources.plannedMigration` is absent or `false`, the exact legacy
two-rule Pod webhook stays supported. The current three-rule form adds `CREATE`
for both `pods/binding` and `bindings`. This form must also have the
garbage-collector cleanup predicate of the public chart. For
`plannedMigration: true`, native execution, that complete Binding rule, and the
chart's controller and Pod cleanup guards are necessary. The flag must be a
JSON boolean.

Nonempty match-condition lists must contain exactly the applicable public
`_sandbox-admission.tpl` predicate, with the pinned controller identity where
necessary. Unknown, duplicate, or altered conditions cause a failure. Only
legacy, non-migration wiring can omit guards. Predicate text that matches does
not prove CEL execution, namespace policy, or migration readiness.

Unsupported layouts, indirect environment configuration, absent/forbidden
resources, identity/version drift, malformed responses, and timeouts stop the
inspection with a blocked or unknown check. There is no fallback to Secret
reads, EndpointSlice discovery, alternative workloads, or the current context.
Before the helper accepts the observations, it rechecks object
UID/resourceVersion pairs, the API version, and the redacted kubeconfig
selection. This detects changes during collection. It is not an atomic snapshot
or a promise about future state. The trusted local kubeconfig and `kubectl` must
not be replaced during a run.

### Requests, bounds and output

This mode uses only local, redacted `kubectl config view --minify` and exact
`kubectl get --raw` GETs. The allowlist contains `/version`, the `kube-system`
Namespace, the two selected Deployments/ServiceAccounts, and the two referenced
ConfigMaps. It also contains the fixed Service/Endpoints and four webhook
configurations, and only the endpoint Pods and their ReplicaSets. It does not
use API discovery, resource lists, watches, logs, exec, attach, port-forward,
service proxies, mutations, or test resources. This mode rejects mutation-mode
arguments.

The POSIX subprocess runner has a 5-second Kubernetes request timeout, a
10-second command deadline, and a 90-second collection deadline. It also has a
64-command ceiling and a 1 MiB stdout limit per command. Cleanup has a separate
one-second wait. Inline JSON is capped at 16 KiB. Platform and placement
documents are capped at 512 KiB and 256 KiB.

The runner discards stderr and closes stdin. It terminates and reaps failed,
oversized, timed-out, or cancelled processes. SIGINT/SIGTERM cancellation stops
further requests. `kuberc` preferences are disabled. Exec/auth-provider
credentials, token-file indirection, impersonation, and insecure/non-HTTPS
targets are rejected before cluster requests.

The mode prints one JSON object below 4 KiB, with no observed names, UIDs,
versions, images, addresses, administrator identities, kubeconfig data, or raw
diagnostics:

- `version: "v1"`, `mode: "deployment-preflight"`.
- `status: "completed"` means that the **scoped observations** were collected
  and matched. It does not mean that all deployment prerequisites or admission
  behavior passed. Otherwise, `status` is `"blocked"`.
- `checks` is the fixed 11-entry checklist, in execution order. Each entry has
  `check`, `status` (`observed`, `blocked`, or `unknown`), and a fixed `reason`
  code. After the first failure, the entries that follow stay
  `unknown/not-checked`.
- `unverified` always contains five `unknown` entries. Three entries are for
  TLS and actual admission invocation, for effective CEL evaluation and
  namespace policy, and for source semantics and namespace write protection.
  The other two are for running Operator Pods, and for runtime/network/storage
  enforcement and provider evidence.
- `qualifiesExecution` is always `false`, `executionReadiness` is always
  `unknown`, and `stockExecutionHold` is always `writer-exclusion-unavailable`.
  The last field describes stock source wiring, **not** an observation of
  deployed execution.

The exit codes are 0 for a completed scoped inspection and 1 for a
blocker/unknown mandatory observation. They are 2 for command-line syntax errors
and 130 for cancellation. Syntax errors produce a minimal sanitized
`version/status/qualifiesExecution/error` object instead of a checklist.
`--help` prints usage without any requests.

Do not use exit 0, rollout status, endpoint readiness, or source booleans as an
execution gate. For certificate validity, effective webhook
matching/authorization, protected placement, and provider enforcement,
independently authorized checks are still necessary. There is no execution
override.

The offline tests replace every `kubectl` subprocess with a synthetic Python
process. They also keep the helper regressions that already exist:

```bash
PYTHONPATH=scripts/tests python3 -B -m unittest \
  test_sandbox_deployment_preflight test_sandbox_platform_qualify -v
make python-check license-check
```

## Protocol-specific network smoke

Synthetic resource creation uses the versioned API collections with strict
server-side field validation. It does not download the cluster-wide OpenAPI
schema before each create. Target checks, command deadlines, returned UID
checks, and cleanup preconditions still apply.

The `network-smoke` mode that already exists uses TCP by default. On a
separately authorized disposable target, select `--network-protocol UDP`. This
runs the same destination port-denial and overlapping-allow sequence with UDP
datagrams:

```bash
python3 -B scripts/sandbox-platform-qualify.py network-smoke \
  --kubeconfig "$TEST_KUBECONFIG" --context "$TEST_CONTEXT" \
  --cluster-uid "$TEST_CLUSTER_UID" --storage-class managed-csi \
  --probe-image "$PINNED_PYTHON_IMAGE" --allow-test-resources \
  --network-protocol UDP
```

Each invocation creates its own synthetic server/probe Pods and policies. Then
it deletes only its UID-pinned, run-labelled namespace. Before client setup,
both read-only preflight modes reject the protocol flag. No new workload
credentials, host access, capability registration, or execution override is
added.

UDP uses connected sockets to the primary IPv4/IPv6 Pod IP of the selected
server. Every datagram carries a fresh 16-byte random suffix and must receive
the exact echo. Mismatched, empty, stale, or oversized responses make the probe
fail. Only a receive timeout counts as no response. Connect/send failures,
short sends, and connection refusals are errors, not successful policy denials.

Each receive has a two-second timeout. For server readiness, an echo from all
three listeners is necessary. Each listener has one second inside the
five-second readiness probe that already exists.

In the baseline, all ports must respond. The Cilium policy then allows 8081,
denies 8080, and leaves 8082 blocked. After an overlapping allow, 8082 must
respond while 8080 still times out. Each sample examines 8082 before 8080, so
an earlier denial cannot be reused across that allow transition.

Results include `networkProtocol` (`TCP` or `UDP`) and keep
`qualifiesExecution: false`. UDP timeouts are observed loss of replies. They are
not an independent proof of which device or policy dropped a datagram. Neither
protocol qualifies full network isolation, DNS semantics, other IP families,
physical fencing, or a running governed Agent. These are synthetic primitive
probes. They are not permission to exec into governed Pods or to enable a live
E2E workflow.

Offline validation covers both the TCP behavior that already exists and UDP
policy wiring, strict response handling, and actual echo traffic over IPv4/IPv6
loopback:

```bash
PYTHONPATH=scripts/tests python3 -B -m unittest \
  test_sandbox_udp_probe test_sandbox_platform_qualify \
  test_sandbox_deployment_preflight -v
```

## Pinned test tools

`ci/install_tools.py` is a separate, networked setup command. It is not part of
the offline validation targets. Its manifest pins Linux amd64/arm64 downloads by
SHA256. The `envtest` bundle installs Kubernetes 1.37
`kube-apiserver`/`kubectl` and its bundled etcd into a new explicit directory.

The installer verifies the archive before it reads only its named regular
members. It refuses outputs that already exist and symlink destinations. If a
write fails, it removes only the files that it created. It does not start the
binaries or contact a cluster.

Downloads are capped at 128 MiB, and this cap also applies to raw binaries.
Each selected archive member or raw binary is capped at 128 MiB. Across one
invocation, the total selected payloads are capped at 384 MiB.

Downloads retry transient HTTP errors (408, 429, 500, 502, 503, and 504),
connection errors, and timeouts at most three times, waiting 5, 15, and 30
seconds. Each attempt keeps the 45-second socket timeout. Permanent HTTP and
TLS errors, invalid payloads, and checksum mismatches fail without retrying.

Oversized advertised lengths cause a failure before the installer reads the
response. Absent or understated lengths still use a bounded read. The installer
examines selected member sizes before it reads any member payload. Verified
archives and selected binaries stay in memory within these limits. Streaming to
disk is not implemented.

Operator CI uses this same manifest and installer for both `envtest` and Helm 3.
It uses an explicit run/attempt-specific directory under `RUNNER_TEMP`. It sets
`KUBEBUILDER_ASSETS` to that binary directory and adds it to `PATH`. It does not
duplicate pins or rely on the Helm version of the runner. Release provenance for
both Linux architectures is recorded in `ci/tools.json`.

Refer to [envtest setup](../DEVELOPMENT.md#generated-files-and-dependencies) for
the separate provisioning and isolated API-server test commands.

## License headers

Run the same offline check that CI uses:

```bash
make license-check
```

The checks include supported source files and Makefiles across the repository.
They also include `site/`, release, and E2E sources. Keep upstream and generated
exceptions aligned with [NOTICE](../NOTICE).

Before you add a notice, follow the [header review and addition procedure](../site/content/en/docs/reference/scripts.md#license-headers).
The tools do not replace notices that already exist, follow symlinks, deploy
resources, or access the network.

## Agent tool authorization metadata inventory

`agent-tools-inspect.py` is a read-only command-line inventory for the
authorization records of Agent tools. It separates the delegated personal
Connections in the central namespace from Agent-local independent Connections
and ConnectionBindings. An Agent-local `connection-reference` is shown in its
own table. It contains no credential and is not a duplicate Connection. It links
an Agent-local Binding to an owner-matched central delegated Connection.

Record identity is the Secret namespace plus the record ID. The command joins
the immutable Agent UID of each record with the live cluster-scoped Agent
resource. As a result, a deleted or recreated Agent is shown as
`deleted/unresolved`, and is not matched by name.

Several Agent-local references and bindings can share a delegated personal
connection. An unreferenced connection is shown as `unassigned/unknown`.
Binding validity is always reported as `unknown/not evaluated`. The reason is
that this command does not decrypt its encrypted payload, and record presence
does not prove an active permission.

Use an explicit kubeconfig. `--context` is optional, and the command does not
change either the kubeconfig or its current context. The central credential
namespace is `cetirun-credentials` by default. If the Broker was configured
differently, pass `--namespace`.

Without an Agent filter, the command lists each live Agent's `status.namespace`
individually. It never uses `kubectl get secrets -A`. A Pending Agent can have
no `status.namespace`. In that case, the output lists the Agent in
`scope.unqueriedAgents` with its UID, name, and reason. The command does not
run an Agent-local Secret query for that Agent.

With `--agent`, the command queries only the selected live Agent namespace plus
the central namespace. As a result, a selected Pending Agent produces a clearly
incomplete central-only view. This intentionally does not claim visibility of
references in other Agent namespaces. A deleted UID has no safely discoverable
namespace, so `--agent-uid` then reports central records only.

An Agent-filtered view includes only its independent Connections, Connections
resolved by its Bindings, and central delegated Connections reached through its
own local references. Other owners' central accounts are not shown.

```bash
python3 -B scripts/agent-tools-inspect.py \
  --kubeconfig "$TEST_KUBECONFIG" --context "$TEST_CONTEXT"

python3 -B scripts/agent-tools-inspect.py \
  --kubeconfig "$TEST_KUBECONFIG" --context "$TEST_CONTEXT" \
  --agent my-agent --output json
```

The command receives a fixed JSONPath projection of selected Secret labels and
display annotations from `kubectl`. Kubectl can fetch complete Secret objects
for that client-side projection. For this reason, normal Secret list permission
is necessary. The script does not receive, decrypt, or print the payload, and
does not contact Key Vault or provider APIs.

Each query has a 15-second request timeout. More than 8 MiB of projected output
is rejected. An absent `status.namespace` is reported as unqueried. A namespace
that does exist but is absent or forbidden when queried stops the inspection.
It is never reported as an empty record set. The inventory is not an atomic
snapshot across the separate metadata queries.

Old central Connections and Bindings stay visible as legacy central relations.
Connections without a mode label show an unknown mode, not an inferred delegated
identity. New display labels and annotations are only display hints. UID and
owner matches stay authoritative.

Offline coverage uses synthetic kubectl subprocess results only:

```bash
python3 -B -m unittest scripts/tests/test_agent_tools_inspect.py -v
```

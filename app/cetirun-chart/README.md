# CetiRun agent chart

The chart deploys an OpenClaw, Hermes, or approved Custom instance. For
settings and defaults, read the
[Helm configuration reference](../../site/content/en/docs/reference/helm.md).
To render the chart without credentials, read the
[local checks guide](../../site/content/en/docs/getting-started/local-checks.md).
We recommend that maintainers who update the bundled Hermes release also follow
[the Hermes runtime upgrade procedure](agents/hermes/README.md).

`values.yaml`, `values.schema.json`, and `templates/` are the source of truth.
Operator-resolved values differ from standalone chart defaults. A local render
does not prove storage, RuntimeClass, image access, or cloud readiness.

From the repository root:

```bash
bash scripts/cetirun-helm-lint.sh
bash scripts/cetirun-helm-test.sh
make -C site serve
```

For public npm/PyPI defaults and alternate mirrors, see
[package registry overrides](../../site/content/en/docs/reference/scripts.md#container-build-helpers).
For runtime package and Git settings, see
[configuration and skills](../../site/content/en/docs/concepts/configuration.md).

## Native npm initialization

The ordinary native runtime installs `skills.npm` packages into its private
HOME. The initializer copies each package into a temporary directory and
examines the completion marker before it replaces an installed package. A
failed copy does not publish a completed installation.

The `.stage.<six-alphanumeric-characters>` names in the npm skills directory are
for the initializer only. Each run of the initializer removes the temporary
copies with these names before it downloads or copies a package. As a result,
if SIGKILL or a node loss stops a copy, that copy does not use the space for
the next run. An unexpected link or a cleanup failure stops initialization with
an error. Other hidden entries are not temporary copies, and the initializer
does not change them.

This does not enable runtime package installation for protected or Custom
workloads. Those workloads must use approved, published skill material.

## Memory and shared Skills on native Agents

With Memory enabled, OpenClaw and Hermes Agents keep the shared Skills
PVC mounted read-only at `skills.mountPath`. Publishing or editing a shared
skill updates that mount without a Pod restart. Native discovery loads the
shared path, personal Skills, and `/opt/cetirun-memory/skills` separately.
The name `cetirun-memory` is reserved for the installed platform skill. Native
loaders ignore other skills with that name, including conflicts introduced by
live shared updates. Other skills keep their native discovery rules. The
compatibility patch checks the pinned native source hashes during image builds.
With shared Skills enabled, OpenClaw polls its watched files every five seconds.
This adds file metadata reads so remote storage updates reach native discovery
even when the filesystem does not deliver change events.

The Agents of a Skill Marketplace Team read a copy of the Team's release at
`skills.mountPath` instead, and Custom Agents in `$HOME/.agents/skills`. Refer
to [Protected Sandbox rendering](#protected-sandbox-rendering).

Memory no longer mounts a read-only directory over the whole `$HOME/.agents`.
Personal files and Skills there remain visible and writable. The platform still
mounts its instructions over `$HOME/.agents/AGENTS.md`; any underlying HOME copy
stays on disk. A Team that explicitly sets `skills.mountPath` to
`$HOME/.agents/skills` still uses that directory for its read-only shared mount.
The platform Memory skill comes from `/opt/cetirun-memory`, so it needs no
directory or nested mount inside the shared source.

The native `context-view` contains only platform material. Ordinary instances
use a `40Mi` emptyDir limit; reconciled Sandboxes keep their configured scratch
limit. Custom context composition and held Sandbox preparation keep their
existing contracts. Use native images that include the reserved-skill patch.
Upgrade existing native instances through
the supported retained restart procedure to install the changed mounts. The
upgrade does not move or delete HOME files or change shared Skills content.
For damaged Memory files or interrupted deletion, use the
[Memory recovery procedure](../cetirun-memory-context/assets/skills/cetirun-memory/references/requests.md#recover-a-store-after-an-interruption-or-file-edit).

## Protected Sandbox rendering

The Operator's `sandboxworkload` adapter uses the shared Sandbox resolution and
the desired outputs of the policy compiler. `helm.PrepareSandboxChart`
validates the original per-Agent overrides **before** it merges them. Then it
applies the complete protected bundle last. `sandboxWorkload` is an internal
chart contract. It is not a tenant override or a way to enable Sandbox
execution.

For a common-executor OpenClaw Sandbox with registered ingress mTLS,
`openclaw.portalSignIn.enabled: true` lets the owner open the Control UI from
the Portal without a gateway-token paste or device approval. OpenClaw trusts
only the managed loopback proxy. Plain Sandbox ingress and retained
single-start lifetimes keep token authentication. See the
[mTLS Portal sign-in contract](../../site/content/en/docs/guides/agent-ingress-mtls.md#openclaw-control-ui-portal-sign-in)
for prerequisites, CLI credentials, and upgrade behavior.

Common reconciliation supports OpenClaw, Hermes, and Gateway-qualified Custom.
The retained preparation-only path still renders zero replicas and does not
authorize execution. Custom must use the common executor. It never uses an
ungoverned runtime as a fallback.

Custom keeps its approved image entrypoint and its inherited numeric non-root
identity. It reuses the HOME claim that already exists and projects immutable
canonical context at its actual HOME. HTTP modes use the static probe that is
packaged in the approved terminal image. Headless mode has no probes. The
optional isolated terminal is rendered separately, is resource-bounded, and is
policy-controlled.

Common Custom terminals also keep the exact Agent-UID-bound exec admission gate
that registry credential isolation uses. With this gate, private-image
credentials can be repaired and rotated after the terminal starts. The repair
and the rotation do not weaken the registry checks or replace the running Pod.
Before Registry synchronization, common Custom reconciliation repairs this gate,
even if a pending credential operation does not match the source Secret yet.
Gate repair does not complete that operation or grant terminal exec permissions.
The common Sandbox admission checks continue to apply independently.

Changes to pull references use the sealed desired manifest of the common
executor. After the manifest is sealed, the legacy deferred-rollout flag must
not change those values. For registration, artifact pins, and unsupported
combinations, see the
[governed Custom contract](../../site/content/en/docs/reference/agent-harness.md#policy-governed-custom-execution).

The protected inventory includes:

- The main container, the HOME initializer, and the selected chart-owned
  doctor, logger, and desktop helpers. Each gets CPU, memory, and
  ephemeral-storage requests and limits. Pod-level CPU/memory declarations use
  effective init/helper accounting. RuntimeClass overhead is accounted
  separately and validated after the API applies defaults.
- Non-root security contexts, dropped capabilities, disabled privilege
  escalation, and RuntimeDefault seccomp. An explicit policy
  `readOnlyRootFilesystem: false` changes only that control.
- A private HOME claim that already exists, a distinct read-only skills claim,
  and a size-limited writable scratch volume. On the native path,
  `skills.existingClaim` prevents PV/PVC creation by the chart. For the
  adapter, HOME UID ownership must be confirmed, and the class, access mode,
  and size must be unchanged. When the effective selection is empty and no
  catalog is configured, the opt-in native OpenClaw profile can use an explicit
  internal empty-skills source instead.
- Disabled Istio injection and Sidecar discovery, enabled compiler-owned
  network controls, and independent explicit identity projections that are
  mounted only in the main container. This chart emits compiler resources
  unchanged.
- Only compiler-owned CiliumNetworkPolicy, dedicated ServiceAccount,
  RoleBinding, and ClusterRoleBinding resources are accepted. Explicitly
  requested cross-namespace RoleBindings and cluster-wide ClusterRoleBindings
  keep their scopes. They must target only the exact ServiceAccount of the
  workload. Shared Role and ClusterRole objects are never emitted or adopted.
  Ordinary mode uses the default ServiceAccount of the namespace. It does not
  create that ServiceAccount or project identity.
- `dnsPolicy: None` with exactly the compiler's one to three trusted DNS server
  IPs. Node/cluster resolver defaults, fallback servers, search suffixes, and
  DNS options are not inherited, and users cannot configure them. No
  cluster-domain suffix is invented. Where necessary, use fully qualified
  names. The compiler's bound ingress mode must also match the shared
  resolution.

For a Skill Marketplace Team, the operator sets two values. `skills.subPath`
is the Team subtree of the skills claim (`teams/<team-uid>`).
`skills.marketplace.selectionDigest` is the Team selection digest. Only the
HOME initializer mounts that subtree, read-only.

The initializer receives the digest as `CETIRUN_SKILLS_RELEASE_SELECTION`,
waits for the matching Team release, and copies it into the context view. The
Agent container mounts this copy read-only at `skills.mountPath` instead of
the claim, so a repository change reaches the Agent at its next start. A
Custom Agent reads the copy in `$HOME/.agents/skills`, with the other skills
of its context view. No other container mounts the claim.

The render fails unless both values are set, `skills.shared` is true, the
Agent context is composed, and the workload is a reconciled Sandbox. When both
values are empty, the render does not change. The chart annotation
`cetirun.microsoft.com/custom-skill-marketplace: v1` tells the operator that
the chart gives Custom Agents their Team's release.

The initializer writes the copied release ID into its termination message as
`cetirun.skills.release=<id>`. The Portal compares it with the Team's current
release to show the owner that a restart picks up newer skills. A failed write
does not stop the initializer.

The legacy pre-upgrade orphan/re-adoption hook is absent from protected
renders. If a StatefulSet that already exists uses `volumeClaimTemplates`, a
separately approved stopped-workload transition is necessary. A render never
migrates that StatefulSet, changes PVC ownership, or claims writer exclusion.
Scratch limits are controls for accounting and eviction, not an independent
hard disk quota.

Only model provider/name, the authorized selection of a Secret that already
exists, and non-reserved typed environment inputs are accepted as governed user
overrides. Final-Pod validation compares all containers, volumes, identity,
scheduling, and other PodSpec fields with the rendered expectation. It allows
only sanctioned Kubernetes/StatefulSet defaults. For admission, the current
controller-owned persisted UID/revision state is also necessary. A rendered
template alone cannot authorize a Pod.

The admission contract also validates the live Namespace's
`cetirun.microsoft.com/sandbox-protected: "true"` label, Agent name/UID
annotations, and canonical Agent owner reference against that state.
Installation must make this namespace identity immutable. It must also
restrict initial marking to the exact controller identity. Labels alone never
authorize a workload.

Admission must read the protected state ConfigMap. It must also do name-scoped
reads of referenced workload ConfigMaps and authorized Secrets in their
namespaces. When Kubernetes access is projected, this includes the
platform-managed `kube-root-ca.crt`. It is not copied into an Agent-owned CA
resource. For metadata-only reads, ordinary resource `get` permission is still
necessary. As a result, do not grant cluster-wide Secret access.

For common-executor Kata lifetimes, the controller records workload binding
intents before this chart is applied. Afterward, it confirms their live UIDs.
For current Pod admission, those confirmed grants and exact ClusterRole source
pins are necessary. A suspended common render validates the compiler output,
but it omits workload RoleBindings/ClusterRoleBindings. Every component in that
render must have zero replicas. A suspended render cannot recreate revoked
access during suspension.

Kubernetes identity volumes and mounts must be present and unchanged on the
main container. They cannot be added to helpers. See the
[administrator setup](../../examples/native-sandbox/README.md#kubernetes-api-access).
This chart does not approve roles or enable capabilities.

Offline regression checks include:

```bash
go -C app/cetirun-operator test ./internal/sandboxworkload/... ./internal/helm ./internal/admission -count=1
# Requires separately prepared local Kubernetes test binaries:
go -C app/cetirun-operator test -tags=envtest ./internal/admission -run '^TestPodAdmissionAPIServerBoundaries$' -count=1
```

These checks do not qualify runtime isolation, host resource enforcement, CSI
writer exclusion, network enforcement, or identity federation. When the
protected contract is absent, legacy chart defaults stay unchanged.

### Native OpenClaw Gateway packaging

The native OpenClaw MVP reuses the protected renderer and the OpenClaw image
that already exists. It does not add an alternative image, an Istio sidecar, or
a Helm value that authorizes execution. The controller owns Sandbox/AgentPolicy
selection, owner access, activation, and deletion. The chart still prepares a
held workload. A render alone does not test any of those live operations.

The Operator supplies this contract:

| Values | Runtime use |
| --- | --- |
| `agentType: openclaw`, `sandboxWorkload.contractVersion: v1` | Explicit protected OpenClaw profile. Preparation keeps `executionHeld: true` and `replicaCount: 0` |
| `runtimeClassName`, `sandboxWorkload` resources, DNS, security and enforcement objects | The compiled runtime controls of the selected Sandbox, never legacy network defaults |
| `persistence.existingClaim`, `skills.existingClaim` | Separately owned HOME and read-only skills storage |
| `sandboxWorkload.skillsMode: empty` (internal, otherwise omitted) | Replaces only the skills claim with an unpopulated `emptyDir`, mounted read-only at `/home/vibe/.aks/skills` in the main container |
| `openclaw.image.repository`, `tag`, optional `digest` | The Agent and its initializers use the same OpenClaw image that already exists. Shared registry/version overrides stay supported |
| `aiGateway.enabled: true`, `keySecretName`, `keySecretKey` | The Agent's virtual key, passed as `OPENAI_API_KEY` through a Secret reference only in the main container |
| `aiGateway.models`, `secrets.provider: openai`, `secrets.model`, `secrets.apiBase` | The authorized model list, default, and Gateway endpoint. If an endpoint is not set, or a default is outside that list, the render fails |
| `aiGateway.visionModels` | The IDs of the models that accept images. OpenClaw gets `input: ["text", "image"]` and Hermes gets `supports_vision: true` for these models only |
| `openclaw.config.gateway` | Controller-selected ingress/auth configuration, copied without persisted auth overrides on each native start |

Protected Gateway traffic uses compiler-owned network policy. As a result,
`sidecarScope.enabled` stays false. For a legacy Gateway render, the Istio
Sidecar scope that already exists is still necessary.

Only the controller's `sandboxSources.nativeExecution` opt-in, which already
exists, can select the empty-skills mode. This selection occurs after Team
skills and Agent additions and removals are resolved. No new public flag is
necessary. HOME stays a PVC, the chart stays held, and the initial Pod lifetime
finalizer is kept.

The schema and renderer reject the empty mode in combination with any of these
items:

- a shared claim
- package requests
- another mount path
- a non-OpenClaw runtime
- native Gateway/lifetime inputs that are not set

No helper can populate the empty skills volume. Requested skills and configured
catalogs continue to use the validated PVC/publication path that already
exists. Errors never select an empty fallback.

Protected OpenClaw startup, readiness, and liveness probes use the Node runtime
that is already in the image. They request `/startupz`, `/readyz`, and
`/healthz` over `127.0.0.1` inside the main container. The helper is packaged
in the read-only ConfigMap mount at `/etc/cetirun/health-probe.cjs`, and no new
image is necessary. It accepts HTTP 200-399 but does not follow redirects, and
it sends no model credentials. It exits nonzero on HTTP errors, connection
failures, or its bounded deadline. Explicit probe timing, thresholds, and
enablement settings are kept.

With `startupProbe.failureThreshold: null` (the default), common-executor
OpenClaw Sandboxes allow 60 failed startup probes at the current 10-second
interval. They do not restart a gateway after 30 failures while its
initialization continues. Other built-in paths keep 30 failures, for example
retained single-start OpenClaw and Hermes. An explicit positive integer
overrides either default. This also applies to `30`.

Liveness/readiness checks and their timings stay unchanged. Legacy OpenClaw and
held Hermes renders keep their HTTP probes.

The startup probe starts after init containers finish. It is not a deadline for
doctor or credential retirement. It does not extend the acceptance bound of a
lifecycle operation. Qualify the complete startup and retained operations on
the target runtime/storage, not only the probe window. This default does not
change pinned charts that already exist or explicit overrides. To adopt the new
chart, use the normal retained update flow.

For these exec probes, no host/remote-node ingress exception or network
compiler change is necessary. They report Kubernetes container health only,
not guest-external observations, runtime qualification, or authorization to
lift an execution hold.

The chart adds `CETIRUN_NATIVE_SANDBOX=true` to OpenClaw's HOME initializer only
on this protected Gateway path. The initializer replaces saved model/provider,
model fallback, and ingress-auth settings with the current platform
configuration. Invalid source configuration fails initialization instead of a
start with a different model source. Named-agent configurations are outside
this single-agent MVP.

Native OpenClaw doctor and gateway processes also disable Node's optional
on-disk compile cache. If that cache is created again in ephemeral Pod scratch,
this adds startup I/O on virtualized filesystems. This does not change the
JavaScript that is executed, plugin selection, resource limits, health checks,
or lifecycle deadlines. Legacy OpenClaw and Hermes keep their settings.

The Gateway deployment must enable `operatorIntegration` and configure shared
models in its chart. This lets the Operator bind AgentPolicy grants to the
[chart-derived model identities](../cetirun-ai-gateway/chart/README.md#cetirun-agentpolicy-model-bindings).
Provider credentials stay with the Gateway. The controller must reject
arbitrary direct provider routes, unimplemented policy restrictions, identity
extensions, and other deferred profiles. It must not forward them as ordinary
runtime values.

Focused offline checks (with the documented dependencies, Node and Helm 3 or 4):

```bash
python3 -B -m unittest discover -s scripts/tests -p test_native_gateway_chart.py -v
python3 -B -m pytest -p no:cacheprovider -q \
  app/cetirun-chart/tests/test_init_home.py \
  app/cetirun-chart/tests/test_retire_copilot_sign_in.py
```

`make ci-test` includes the render tests. `make python-test` includes the
initialization tests with the offline guards of the repository. The CLI
fixtures are synthetic. They do not qualify a built image, Kata, Cilium,
storage, a live owner sign-in, or an actual model request.

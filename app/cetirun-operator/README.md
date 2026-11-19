# CetiRun operator

The operator reconciles `Agent`, `Team`, `Sandbox`, and `AgentPolicy` resources
in `cetirun.microsoft.com/v1alpha1`. Policy assignment and AI Gateway
projection are active control-plane contracts. Native Sandbox execution stays
held at zero replicas until a separately delivered execution adapter is enabled.

- [Custom-resource reference](../../site/content/en/docs/reference/crds.md)
- [Agent Harness contract](../../site/content/en/docs/reference/agent-harness.md)
- [Operator chart configuration](../../site/content/en/docs/reference/helm.md#operator-chart)
- [Architecture and resource names](../../site/content/en/docs/concepts/architecture.md)
- [Platform installation prerequisites](../../site/content/en/docs/getting-started/platform.md)
- [Lifecycle and deletion boundaries](../../site/content/en/docs/guides/lifecycle.md)

## API contract

| Kind | Resource | CRD name | Short name | Scope |
| --- | --- | --- | --- | --- |
| `Agent` | `agents` | `agents.cetirun.microsoft.com` | `cra` | Cluster |
| `Team` | `teams` | `teams.cetirun.microsoft.com` | `crt` | Cluster |
| `Sandbox` | `sandboxes` | `sandboxes.cetirun.microsoft.com` | `crs` | Cluster |
| `AgentPolicy` | `agentpolicies` | `agentpolicies.cetirun.microsoft.com` | `crp` | Cluster |

An Agent is a long-lived environment that a user owns. A Team supplies shared
configuration, not access to private agent data.

Agents can keep the legacy deployment path. It is not necessary to create a
Sandbox or an AgentPolicy for this path. To use this path, omit `sandboxRef`.
Use no Team, or use a Team that has no policy or Sandbox fields. The chart,
runtime, Gateway, ownership, and admission rules that already exist still
apply. Compatibility is not a bypass for a requested policy.

A policy-bound Agent names one Team and one Sandbox that the Team allows. The
Team's single `agentPolicyRef` selects its policy. This selection does not come
from the Portal role of the human caller. The effective policy is the
intersection of that administrator-selected `AgentPolicy` and the Agent's
explicit requests for models, budget, skills, and identity.

For a policy-bound Agent, the Operator resolves the selected Team, Sandbox, and
policy before it can keep compute or model access. It projects model names and
a native monthly USD budget to the Agent's own AI Gateway virtual key. It
compiles the Sandbox, security, and network revision, and records the expected
Gateway, Sandbox, and credential revisions in the Agent status. Absent,
rejected, stale, or unreadable evidence fails closed. There is no approval
resource or separate governance workflow.

The API replaces `OpenClawInstance` and `OpenClawTeam` with the same group and
version. The installation of these CRDs does not convert, adopt, or delete
legacy objects. Do not delete legacy CRs or CRDs as an upgrade shortcut.
Finalizers can remove workloads and namespaces, and the deletion of a CRD
removes its stored objects. Installations that already exist must have a
reviewed plan for data retention and ownership handover. See the
[compatibility guidance](../../site/content/en/docs/reference/crds.md#existing-installations).

Source layout: `api/` defines the types, and `config/crd/bases/` contains
schemas. `internal/` contains reconciliation and the Helm and Azure
integration. `chart/` deploys the operator.

The chart packages CRDs under `chart/templates/crds/`. Helm installs and
upgrades them with the release. Their `helm.sh/resource-policy: keep`
annotation keeps them when the release is uninstalled. Follow
[DEVELOPMENT.md](../../DEVELOPMENT.md#generated-files-and-dependencies) for the
pinned generation commands.

`deploy/deploy.sh` is a connected helper that makes changes. It is not a local
smoke test. If the Gateway is in a separate namespace, configure the
[Gateway reference and workload namespace](../../site/content/en/docs/reference/helm.md#operator-chart)
independently.

## Policy control plane and native Sandbox execution

`Sandbox` describes a reusable runtime, container resource declarations,
private home storage, and scratch. `AgentPolicy` describes the selection,
security, network, ingress, and identity intent. Both configuration resources
are cluster-scoped.

A policy-enabled Team binds one policy, the allowed Sandboxes, and a creation
default. An Agent binds a Sandbox. The role of a human administrator does not
select the workload policy. These bindings exist together with the mandatory
`spec.agent` Harness configuration.

The [sandbox package](internal/sandbox/) now supplies these features:

- Pure functions that apply defaults, validate, and normalize deterministically.
  They do not mutate source objects.
- Cross-resource validation of every Sandbox and policy pair that a Team
  configures.
- Single-policy resolution with explicit authorization of the Agent, Team,
  Sandbox, and AgentPolicy UIDs. A replacement that has the same name must get
  new authorization.
- A desired-configuration revision. It excludes metadata, policies that are not
  selected, changes to the creation default, and unrelated model-access
  settings.
- Qualified-capability checks, and a freshness and identity check for trusted
  observations. Zero-value registrations, absent observations, stale evidence,
  and incomplete validation fail explicitly.

These helpers do not write workloads, authorize RBAC grants, compile a complete
network policy, or prove enforcement. There is no packaged guest-external
observer, writer-fencing adapter, or default capability registration. Explicit
platform declarations can be read through the Kubernetes sources below. Their
registration revision must represent the qualified runtime, chart, topology,
and grant inventory, not only the existence of a RuntimeClass. An observation
validator examines the input of an adapter, but it does not generate evidence.

### Credential policy evidence

The credential projection for Agent Identity uses two fields that already
exist: `status.policy.enforcement.credential.desiredRevision` and
`status.agentIdentity.credentialPolicyRevision`. The second field is written
only after a typed Broker readback of an actual owned enforcement record
matches the current references. These are the policy, owner, and canonical
identity references. The `CredentialPolicyReady` condition separates that
evidence from the readiness of the directory identity. These actions alone
cannot complete the aggregate `PolicyApplied`: to request Agent Identity, to
copy a desired revision, or to get a bearer token.

The controller reads before it applies, and it reads again after a successful
write. Lost replies, invalid readback, and status conflicts do not become
evidence. Policy or identity drift clears the applied revision. The removal of
Agent Identity does not satisfy the identity-free policy until the identity
cleanup finishes. This cleanup includes Broker revocation before directory
deletion.

With `connectionsMCP.nativeEnabled`, the common OpenClaw/Hermes executor and
explicitly allowlisted Custom images can project a purpose-bound Connections
proof. The proof goes on the current or default ServiceAccount, which is
already pinned. The registered MCP Service, the public CA, and the exactly
Agent-owned Broker storage Role/RoleBinding are added to the Sources that
already exist. Broker and MCP each validate the live workload independently.
They use current API reads and the served Pod-admission/source validator. They
never trust the enable flag as proof.

Configuration readback can occur before the aggregate Ready. But to serve
credentials, a current Running/Ready Pod and a Ready Agent are necessary. Only
validated readback can complete the credential part of `PolicyApplied`. Inputs
stay nonready if they are absent, unsupported, stale, held, suspended, or in
deletion. A validated bootstrap publishes the namespace locator before Ready.
It does not claim execution readiness.

This path does not add a dedicated ServiceAccount lifecycle. Generic Entra
workload projections do not establish a Connections MCP identity. Live
runtime, networking, and provider/federation qualification are still
prerequisites. For the boundaries of installation, readback, current-source
matches, and cleanup, see
[native installation](../cetirun-connections-mcp/README.md#governed-native-workloads)
and the
[Broker contract](../cetirun-agent-broker/README.md#credential-policy-execution-and-readback).

### Common Sandbox reconciliation (explicit opt-in)

With `sandboxSources.nativeExecution: true`, new OpenClaw and Hermes Agents use
the ordinary controller flow: feature assembly, Harness adapters, Helm apply,
readiness, model integration, and deletion. The selected Sandbox adds runtime,
resource, HOME, network, and credential constraints to that flow. There is no
fallback deployment mode. The default value of the opt-in is `false`.

Use the same release version for the Operator, Sandbox admission, Portal, and
configuration-change tools. This contract does not cover the mixed-version
rollout order or pre-upgrade bootstrap-only recovery. There is no other
reader-first switch. Remove the retired `sandboxSources.reconciledExecution`
setting. This setting is rejected, not silently ignored.

The persisted lifetime mode and the owned namespace and HOME identities still
govern recovery and cleanup. Configuration changes do not migrate or adopt
resources that already exist. Absent ownership evidence still holds execution.

Kata and container HOME support `ReadWriteOncePod` (RWOP) and `ReadWriteOnce`
(RWO) when the platform's `homeAccessModes` explicitly lists the selected mode.
Both modes use the same common-executor paths for restart, retained
suspension/resume, and cleanup. Before coordination releases a source
reservation, it still waits for an observed workload shutdown and for the
termination of every namespace Pod. RWO is not a shared HOME grant or a
single-Pod storage lock. Private ownership, one main StatefulSet replica, and
current Pod admission are still necessary. A change to a Sandbox does not
convert the access mode of a HOME claim that already exists.

Shared-only, Copilot-only, and mixed model configurations use the Gateway and
the per-Agent Copilot proxy that already exist. The proxy is a separate
Deployment, not a sidecar. It cannot mount HOME or use the Agent's runtime or
model credentials. The proxy's optional, controller-owned
`aiGateway.copilotProxy.replicaCount` allows authentication preparation
independently of the Agent replica count. Suspension and policy revocation stop
both components. Creation before login does not grant Copilot model access, and
the Portal continues to report sign-in required.

Suspension does three actions before the chart or bootstrap prerequisites are
examined. It blocks a Gateway key that already exists, stops the owned main and
auxiliary workloads, and withdraws their route. If the Gateway, a component
stop, or a status write fails, suspension still tries the other shutdown
operations. A shutdown that is not acknowledged still keeps its source
reservation. Normal Kubernetes stop requests are not physical fencing.

The Portal may also accept a narrow pre-bootstrap pause for a governed common
Sandbox Agent when it can authoritatively prove that no execution lifetime
exists: there is no Agent namespace, bootstrap/preparation/grant receipt,
bootstrap/preparation finalizer, or status evidence of an enforced or ready
runtime. A hold left by a failed pre-bootstrap prerequisite is permitted when
all of those no-lifetime proofs still hold. This does not retain HOME or a
Gateway key because neither exists yet. Any missing, conflicting, or unreadable
evidence instead leaves the requested suspension `Pending` and never starts
bootstrap; resume is the explicit intent that lets the controller try
provisioning again.

The current Gateway reconciliation also removes the per-Agent Copilot token
and proxy-key Secrets during explicit suspension. Suspension keeps HOME and the
Gateway spend, but it does not keep the Copilot sign-in. The owner must sign in
again after resume. If the Copilot sign-in must stay available, do not use
explicit suspension for an ordinary chart or image upgrade. Use the normal Agent
update and reconciliation path. If a coordinated configuration change makes a
suspension necessary, plan for owner reauthorization.

If policy denial already requested the deletion of the Gateway key, suspension
retries that revocation. It does not try to block the deleted key. It reports
`Suspended` only after revocation and shutdown succeed. The policy stays denied,
and the recorded key identity stays. A resume does not recreate a missing key or
restore access under a denied policy.

Loss of bootstrap or runtime prerequisites also holds a reconciled Agent that
already runs and is not suspended. The hold blocks its key, stops the owned
components, and withdraws ingress, but it keeps HOME and spend. After a status
error, the hold still tries these independent cleanup operations.

The same protected expectation ConfigMap now has a version-2 representation for
these lifetimes. It records the current chart-owned workloads, the complete
source identities, and the compiled controls. Pod admission validates the
StatefulSet or Deployment/ReplicaSet ownership and the current authorized
template. It does not permanently consume one Pod UID or fix helper names.
Common helpers and chart components are still subject to runtime, privilege,
storage, and credential constraints.

Configuration, key, and model updates use normal Helm reconciliation. They keep
HOME and the current Gateway spend identity. Absent or foreign ownership is not
repaired through resource adoption or the removal of protection markers. Ready
and suspended Agents still recheck the current sources and the deployed
controls. Unchanged passes keep their phase and reconcile timestamp between
periodic refreshes, and they do not generate status-watch loops.

Private-image Agents complete the owned namespace bootstrap before registry
credentials are synchronized. Absent runtime pins are filled first. Namespaces
that already exist without the protected bootstrap receipt are not adopted.
Credential rotation still synchronizes the owned pull Secret before chart
preparation. The protected expectation pins the identity of that Secret as a
kubelet image-pull binding. It does not pin it as a Secret that Agent containers
or the Copilot proxy can use.

When Memory Context is enabled, reconciled OpenClaw and Hermes use their
ordinary native plugin registration and composed skill paths together with the
installed assets. OpenClaw keeps its conversation hook. Hermes explicitly
includes `cetirun-memory-context` in `plugins.enabled`, also after HOME
initialization. Retained single-start templates stay disabled.

Every disk-backed `emptyDir` must declare a positive `sizeLimit` that is not
greater than the selected Sandbox scratch limit. This rule also applies to
extension and auxiliary volumes. The common chart also bounds its context,
empty-skills, and Tailscale temporary storage. Helpers with fixed
temporary-volume sizes must have a sufficiently large selected limit, and
validation does not silently increase it. Memory-backed volumes keep their
separate memory accounting. These are checks for each volume, not a replacement
for the storage accounting of containers and Pods.

Portal runtime profiles are optional only when the Portal deployment explicitly
enables `sandboxRuntimeDefaultsFromController` after this common executor is
enabled. Without that explicit deployment setting, creation is rejected when
profiles are absent. When it is enabled, the controller pins the chart, image,
and port from its selected trusted chart, with narrowly authorized admission
updates. Disable the Portal opt-in before you disable native execution. Explicit
requests and configured profiles are still validated.

The protected path disables the ordinary immutable-StatefulSet replacement hook.
Unsupported storage or type transitions must use a deliberate handoff. They must
not leave the old HOME writer as an orphan.

Bootstrap receipts that already exist select their recorded lifetime mode. This
includes checkpoints that were created before the preparation finalizer or the
Gateway projection. These receipts keep their HOME identity, chart pins, and
cleanup, even after the new switch is enabled. Version-1 preparation and start
records also keep their recorded lifecycle. They are not a second
feature-development path or an automatic conversion of a lifetime that runs.

The historical `native-single-start` capability-profile identifier is still
accepted for configuration compatibility. New version-2 lifetimes have the
common lifecycle. Supported policy capabilities are still necessary for
FQDN/mTLS and identity grants. Kubernetes shutdown, RWOP, and successful rolling
updates do **not** establish physical writer fencing, failed-node recovery,
durable restart requests, or cross-node migration guarantees.

When the optional configuration-coordination authority is configured, these
lifetimes also reserve their selected Team, Sandbox, and policy. A normal
desired-template generation can refresh that reservation only while the selected
source identities and effective inputs do not change. Shared configuration
changes stay excluded until suspension or deletion acknowledges controller
shutdown and ordinary Kubernetes quiescence. This is a logical configuration
reservation. It is not a physical workload identity or a writer-fencing
receipt. Unknown shutdown outcomes stay held.

If a resume finds a recorded stop, it restores the hold and finishes that
guarded shutdown before it gets new authority. An absent hold marker alone does
not discard the pending stop or establish quiescence. For removed or
never-created auxiliary Deployments, their ReplicaSets and namespace Pods must
still be absent. Their absence does not replace the acknowledged shutdown of the
main HOME workload.

A fresh, durably recorded pre-submit checkpoint can be canceled without a
shutdown acknowledgment for the main StatefulSet. This is possible only before
the primary workload write is submitted, or after a fenced, definitive
rejection of the first primary CREATE. Also, cancellation is possible only after
the ordinary namespace and Pod cleanup checks.

Its expectation is first persisted as initializing, before the grant journal.
It becomes pending only after the empty journal is confirmed. As a result,
publication retries keep the pre-submit evidence, also when acknowledgments are
lost. The retries do not infer that evidence from an absent workload.

No grants or workload apply are issued during initialization. Cancellation
competes with submission through a conditional state update. Submitted,
uncertain, or older unmarked checkpoints still must have an acknowledged
shutdown. An absent StatefulSet alone is not evidence that no workload was
submitted.

Helm configuration and history reads recheck the current authority, but they do
not mark submission. The post-renderer claims the expectation resource version
and rechecks authority, but it leaves a first primary workload pending. Helm's
guarded writer records primary submission immediately before a StatefulSet
CREATE or update. It also records primary submission before an opaque apply
that can contain the primary workload.

An approved auxiliary Deployment CREATE or update takes its own conditional
render claim. The primary workload keeps its non-submission proof. As a result,
a first-install binding rejection before the primary boundary can cancel the
pending workload. Recovery can then occur through a normal retry or deletion.

Grant submissions are still tracked independently. An uncertain binding CREATE
still blocks cleanup, even when no workload was submitted. The bindings or the
auxiliary creation for Copilot can succeed while the first primary CREATE gets
a definitive API rejection. In this condition, the writer records non-creation
against the UID/resourceVersion of the exact expectation that was submitted.
A normal retry or deletion can then revoke the grants and release the
reservation. It does not wait for a StatefulSet that does not exist.

An auxiliary that exists or is uncertain still must have ordinary controller,
ReplicaSet, and Pod quiescence. Primary cancellation cannot skip those checks.
The same CREATE boundary applies when Helm uses its update path. This includes
retries whose failed release history declares a workload that was never
created.

The durable controller checkpoint decides if this is still the first
unobserved submission. A historical manifest or the current absence of a
workload does not decide it. Earlier workload submissions stay opaque.
Submitted checkpoints that already exist are not reclassified because a
workload is absent. Guarded CREATEs do not follow HTTP Retry-After responses.

Primary timeouts, lost responses, earlier submissions, and opaque update
batches stay uncertain. A later rejection cannot cancel them. If a competing
claim or a replacement expectation exists, a stale rejection recorder cannot
cancel current work.

Cancellation and workload submission compete through conditional state writes.
No observation of an absent resource can reset a submitted or older uncertain
record. When claims compete, they fence stale writers. But after a lost
acknowledgment of workload submission, or for work that was submitted before, a
shutdown is still necessary.

The Operator lists auxiliary ReplicaSets in the affected namespace. The chart
keeps this read/list permission when new native starts are disabled. This lets
lifetimes that already exist finish cleanup.

Normal reconciled deletion revokes its recorded preparation grants and
releases/deletes the Operator-namespace grant journal. Then it removes the Agent
finalizer. This cleanup stays available when new starts are disabled. During
foreground deletion, it retries journals that are in deletion. It does not
adopt foreign ownership or discard grant history.

### Scoped internal egress

Common-executor Kata and container OpenClaw/Hermes lifetimes also support
`AgentPolicy.spec.network.allowedInternalCIDRs` with explicit TCP/UDP ports, for
either registered HOME mode. An omitted protocol means TCP. They use the Cilium
compiler that already exists, the owned per-Agent policy, and current-source Pod
admission. No other network controller is added.

Grants must stay within private or registered internal networks. They cannot
include node, management, metadata, loopback, or other mandatory exclusions.
Explicit CIDR/port denies and the approved-DNS-only restriction still have
precedence over grants. If grants overlap, they combine ports only within their
common address range. Unspecified ports and other managed peers stay denied.

Cilium-managed destinations also must have the platform's complete
`topology.peers` inventory, exact namespace and protected peer-UID labels. All
registered addresses for one peer must have identical effective permissions.
Inconsistent address/port permissions are rejected, and the endpoint selector of
the peer does not become wider. A CIDR grant does not discover Pods, translate
Service ports, or grant arbitrary cluster access.

These network edits still use the configuration-coordination rules above. These
rules include an acknowledged suspension before a change to reserved sources.
Retained version-1 single-start lifetimes continue to reject internal grants.
See the
[internal-egress example](../../examples/native-sandbox/README.md#scoped-internal-egress)
for the policy shape and the separate rules for live validation.

### Planned healthy-source migration (separate opt-in)

`sandboxSources.plannedMigration: true` enables a platform-administrator
Kubernetes API for cold moves of common-executor OpenClaw/Hermes Agents. This
setting must have `nativeExecution`, which selects common reconciliation. The
default is off. For reader-first rollout, the false setting is omitted from
the controller configuration.

Upgrade the CRDs, all Operator and admission replicas, and Portal readers before
you enable new requests. Do not roll back readers or schemas while migration
records still exist. If you disable new requests, accepted operations and their
cleanup are kept, and a destination pin that already exists is not removed.

This scope must have one bound filesystem CSI `ReadWriteOncePod` or
`ReadWriteOnce` HOME. There must be no other PVC mounts. The scope also must
have a CSI driver for which attachment is necessary and a healthy source on a
different Node. The migration keeps the Agent, namespace, StatefulSet, HOME
PVC/PV, chart pin, Gateway key, and spend identities. Governed Custom uses the
same common migration path.

This feature does not support retained single-start lifetimes, shared-volume
migration, cross-cluster moves, forced deletion/detach, or unavailable-source
recovery. Before you enable this feature, use an explicitly authorized
environment and qualify the actual runtime/CSI artifact set.

Set `spec.plannedMigration` to a fresh `requestID` and a `target` that contains
the current `name` and `uid` of the destination Node. IDs use the
retained-restart 16-128 character format. Submit the change with Agent
UID/resourceVersion preconditions.

The same ID cannot select a different destination. An accepted request cannot
be replaced until it is completed. A rejected, unaccepted request can be
corrected. If the activation gate rejects a request that is not captured, the
request still rechecks source authorization. If that request stays in place,
it cannot keep revoked access.

After a recorded migration, keep the destination. Do not remove the field.
Portal has no migration action in this increment. When a pending migration
exists, Portal rejects new owner restarts.

An obsolete reconciliation cannot turn a newer migration request into a
dependency hold. A detected intent/checkpoint change is retried from a fresh
state, and the source of that request is not stopped. Actual authorization
outages still cause a hold. The detected changes include intent changes after
the migration check but before the later Agent read of the common Sandbox guard.
A changed current binding is not exempted.

Other common preparation failures recheck the Agent revision before they hold
the Agent. As a result, a later stale source read cannot stop the source of a
newer request. An unavailable current-version read still follows the dependency
hold and reports the read error.

The controller persists `status.plannedMigration` before side effects:

1. `Protecting` captures exact source/storage/Node identities. It protects the
   source Pod and VolumeAttachment with separate finalizers. After both are
   protected, a failure to write the stop checkpoint does not skip independent
   authorization checks. It also does not skip the shutdown of an unauthorized
   source.

   A transient dependency failure keeps the source observations and continues
   shutdown from `Stopping`. It does not wait for readiness that its own hold
   cleared. Destination start still must have current authorization through
   common reconciliation. No desired generation is reported as applied only to
   pass a migration check. An accepted checkpoint is handled before
   bootstrap-receipt lookups can enter an independent hold.

   During `Protecting`, failed source reads or protection writes delay
   scale-down, even when suspension is requested. Retries must keep both
   observations and must not destroy an unprotected source. These retries still
   recheck the current authorization.

   While incomplete protection prevents scale-down, suspension, withdrawn
   access, changed projections, or unavailable authorization independently
   block the Gateway key. They also withdraw ingress and apply an all-traffic
   deny to the exact owned Cilium policy. The Agent/namespace selector of that
   policy and the captured source/storage identities stay the same. Ordinary
   reconciliation restores the policy before the destination is used.

   Network write/read-back failures stay explicit and retryable. They are not
   evidence of successful dataplane isolation. The temporary block keeps the
   key/spend identity. It does not apply a new grant or claim an
   administrator's block. Access failures are returned with the protection
   failure for retry. The preservation of source observations does not allow
   continued use of an obsolete grant.
2. `Stopping` blocks the key, withdraws routing, and stops the main/auxiliary
   workloads. `SourceTerminated` must have a terminal Pod status and a
   completed status for every named container. At the same time, the original
   source Node/boot and CSI identity must stay healthy. An absent Pod is not a
   termination acknowledgment.
3. For `Detached`, the original VolumeAttachment that the migration keeps must
   be in deletion. The CSI attacher finalizer that was observed during capture
   must also be released. The attacher removes that finalizer after a
   successful detach. It does this before it updates `attached` and clears
   `detachError`. These later status writes can be lost without a retry. As a
   result, the finalizer transition on this kept attachment is the completion
   evidence, even when the status is stale.

   The attachment UID, volume, driver, and source Node must still match. The
   attachment finalizer of the migration is removed only after `Detached` is
   recorded. These do not establish detach: an absent attachment, or
   `attached: false` while the CSI finalizer is still present. Ordinary
   deletion uses the same evidence to release the captured protection. See the
   upstream
   [external-attacher completion order](https://github.com/kubernetes-csi/external-attacher/blob/e19b215493af4f130a8a82ef5911b5f052edd81c/pkg/controller/util.go#L82).
4. `Starting` occurs after the removal of source Pods/attachments and the
   release of any configuration reservation. The destination must satisfy the
   bound PV topology, workload/RuntimeClass scheduling, and supported
   Equal/Exists tolerations. Required node-name affinity intersects the
   constraints that already exist. It never replaces them or sets
   `spec.nodeName` directly.
5. `Completed` must have the current Ready replacement Pod on the selected
   Node. It also must have an attached destination VolumeAttachment for the
   original HOME.

Both Pod binding APIs read the destination Node UID and provider identity
again. Replacement Nodes that have the same name are rejected. For Node health
checks, a Ready condition is necessary. The UID-owned lease of the Node also
must be renewed within the lease duration, with a maximum of 60 seconds. Lease
timestamps that are more than five seconds ahead are rejected.

Source boot/CSI identity changes or uncertain observations block progress, and
another destination is not selected. These API observations are **not physical
writer fencing** and do not qualify failed-node recovery.

Suspension keeps the checkpoint and prevents destination start. Resume
continues the same request. If suspension arrives before the first checkpoint,
a source that is still ready is captured and protected before it is stopped. A
rejected migration preflight does not block a suspension that was requested
independently. An unaccepted request stays available for administrator
correction or withdrawal. It is not silently cleared.

If an authorization outage holds the source before capture, ordinary
reconciliation can restore its running projection after the dependency
recovers. Only a projection that is authorized at that time is restored. The
unaccepted destination is not rendered or applied. Any previous completed
destination pin stays in effect.

Both binding APIs allow that source recovery. Capture is retried only after
readiness returns. Current authorization, suspension, and the migration opt-in
still apply. A newly persisted checkpoint invalidates an older recovery apply.

Pending migration/shutdown work reports `Pending`, not completed suspension. A
scale-down to zero replicas alone does not acknowledge source termination.
Deletion never creates a replacement. It releases only the captured
protections after their acknowledgments. Do not remove finalizers or edit
status to bypass a wait. Source protection writes, acknowledgments, and
replacement apply stay retryable after conflicts or lost responses.

Even when new requests are disabled, the Operator keeps `get` on
Nodes/CSINodes/CSIDrivers and `get/list/patch` on VolumeAttachments for
observation and finalizer cleanup. Admission keeps read-only Node access and
validates only the checkpoint-authorized Pod finalizer delta. It does not
mutate lifecycle state.

Offline tests use synthetic kubelet/CSI observations and isolated API servers.
They do not test a live AKS driver, a model, storage durability, or physical
fencing. See the
[lifecycle guide](../../site/content/en/docs/guides/lifecycle.md#planned-node-migration).

### Retained single-start profile and capability registration

These single-start lifecycle notes describe retained version-1 records. New
native lifetimes use the version-2 common flow above.

For the explicit native option, these items are necessary: enabled, enforced
configuration/Pod admission, `sandboxSources.enabled`,
`sandboxSources.bootstrapOwnedPrerequisites`, and an enabled AI Gateway. The
controller receives the same option through `CETIRUN_SANDBOX_SOURCES`. The
option adds Node `get` permission for readiness.

The manager always keeps Pod `patch` permission for recorded lifetime
finalization. This also applies after new native starts are disabled.
Admission still limits protected Pod updates. The native profile uses the
scheduling constraints of the registered RuntimeClass. The separate durable
placement registry is not supported, and it is rejected.

These earlier execution records support one OpenClaw Agent, a Kata Sandbox with
ReadWriteOncePod HOME, and shared Gateway model access. This execution uses the
resources/storage of the selected Sandbox. It also uses the model entitlement
and budget of the selected AgentPolicy. Per-Agent Gateway keys are projected
through owned Secrets, not embedded provider credentials. CIDR denies and
supported TCP/UDP destination-port denies are compiled into the owned Cilium
policy.

These items are rejected before start: FQDN rules, internal-network exceptions,
mandatory mTLS, Kubernetes/identity grants, Copilot sidecars, desktop,
Tailscale, and arbitrary Agent Helm overrides. The baseline network controls
that already exist are kept.

Platform registration must state its actual guarantee. A bounded registration
uses `registrations.kata.Capabilities.profile: "native-single-start"` in
`platform.json`. It also sets `platforms.kata.adapter` to
`{"name":"cilium","revision":"<probed-version>","qualified":false,"available":true}`.
This generic Cilium identifier does not assert Azure CNI or Azure topology.

The profile must have actual Kata resource limits. It also must have an
explicit, nonempty `homeAccessModes` list. This list must contain only
`ReadWriteOncePod`, `ReadWriteOnce`, or both. New common-executor lifetimes can
use either registered mode. Retained single-start lifetimes still must have
`ReadWriteOncePod`. Both lifetime types keep the guest-external network,
deny-precedence, node-traffic, and DNS IP restrictions that already exist.

Declare `hostResourceLimits`, `guestExternalNetwork`, `denyPrecedence`,
`nodeTraffic`, and `dnsIPRestrictions` only after you establish those controls.
These items are still necessary: TCP and UDP, the actual IP families, the
revisioned complete topology, DNS/Gateway peers, and pinned
RuntimeClass/StorageClass identities. Read-only root and ingress support must
match the selected policy. The configuration harness allow-list supports
`openclaw` and `hermes`. The earlier single-start executor itself is still
OpenClaw-only.

For native Kata, `enforcementObservation`, `restrictiveFailure`, and
per-container guarantee flags must be `false`. Kubernetes access can be
explicitly registered for new Kata common-executor lifetimes with exact
policy-role and per-Agent identity approvals. See the
[Kubernetes API access example](../../examples/native-sandbox/README.md#kubernetes-api-access).
Retained single-start lifetimes continue to reject Kubernetes access.

Upgrade all Operator/admission readers before you activate a shared
registration. Older readers cannot use or remove the new RBAC controls. A
disabled capability alone does not make a rollback safe. Before you downgrade,
follow the cleanup and old-compatible-state prerequisites in the example.

Generic Entra projection can be explicitly enabled for common-executor
lifetimes with the dedicated bootstrap/source contract below. Observed-SNI
egress must have its separate explicit registration. These are not physical
fencing or guest-external runtime-observation guarantees.

Common-executor Kata and container OpenClaw/Hermes lifetimes can opt in to
managed ingress mTLS. This opt-in uses `mutualTLS: true`, per-Agent external
certificate approvals, and the registered Envoy endpoint. Retained
single-start lifetimes still reject it. See
[managed Agent ingress mTLS](../../site/content/en/docs/guides/agent-ingress-mtls.md)
for certificate ownership, supported surfaces, source pins, and
stop/change/resume rotation. Managed ingress mTLS alone does not enable
outbound inspection. Observed-SNI egress must have the separate registration
that is described below.

### Ordinary-container backend

An explicit `Sandbox.spec.type: container` runs OpenClaw, Hermes, or approved
Custom images in ordinary containers (for example, runc). These containers
share the node kernel and do not use a Kata VM. Omitted/null types still
default to `kata`. The current scope of gVisor does not change.

Container must have the same opt-in common executor, AgentPolicy/Team binding,
current Pod admission, Gateway key/budget/spend process, Cilium compiler, and
owned HOME lifecycle. It cannot use the held or retained single-start executor.
There is no separate ungoverned runc deployment path.

Register it under `registrations.container` and `platforms.container` in the
trusted `platform.json`. Select `Capabilities.profile: native-single-start`.
Declare `hostResourceLimits: true` and `perContainerResourceLimits: true`. Name
an explicit RuntimeClass whose UID and ordinary-container handler are pinned in
`runtimeClasses`. `requirePerContainerResourceLimits` can also be `true`.

Do not register a renamed Kata/gVisor class. Do not infer availability from a
RuntimeClass object. Before the platform owner declares support, the owner must
validate the configured handler, eligible nodes, resource enforcement, CSI, and
Cilium. Absent, unavailable, or changed registrations/UIDs/handlers prevent
execution. There is no fallback to the default runtime of the node.

Agent and same-Pod helper requests/limits keep their Kubernetes semantics.
Effective Pod CPU/memory includes regular and init containers. Only the actual
approved RuntimeClass overhead is included, and there is no automatic VM
allowance. A normal runc class has no VM overhead.

Separate platform auxiliary Deployments keep their current role-specific chart
and admission contracts. `guestExternalNetwork` keeps its
platform-network-control meaning (outside the workload). It is not a claim that
a container has a guest VM.

Both registered RWO and RWOP filesystem HOME modes use the same retained
restart, suspend/resume, and opt-in healthy-source CSI migration. RWO alone is
not writer exclusion, and neither backend gets physical failover fencing. This
feature does not add runtime conversion. For a different backend, select a
separate Sandbox and a new Agent. Active configuration reservations still reject
runtime edits.

Container uses the same supported CIDR/port/ingress policy subset as native
Kata. Managed ingress mTLS uses the same approved endpoint and certificate
contract that is described above. A runtime or capability flag alone does not
authorize it. Generic Entra projection must have the separately approved
bootstrap identity contract below.

Observed-SNI blocking and Kubernetes grants use the common adapters. Each of
them has its own explicit capability declarations and source approvals. A
container selection alone does not enable either of them. Kubernetes grants
keep the exact per-policy role and per-Agent identity checks. Keep
`enforcementObservation` and `restrictiveFailure` false.

See the
[ordinary-container configuration example](../../examples/native-sandbox/README.md#ordinary-container-backend).
Offline tests use the same executor and served admission. They do not test a
live container runtime, Cilium dataplane, CSI driver, or model provider.

### Shared native network controls

The Kata registration examples that follow also apply to container
registrations under the `container` key, with independently validated
runtime/network support.

`network.blockedCIDRs` uses the registered address families and the current
guest-external deny baseline. `network.blockedPorts` also must have
`registrations.kata.Capabilities.blockedPorts: true`. This capability is never
enabled by default or inferred from TCP/UDP connectivity. Declare that support
only after you validate the actual deny behavior on the selected Cilium/runtime
combination. Native port requests default to TCP. The example/probe protocol
lists stay TCP/UDP.

When `registrations.kata.Capabilities.protocols` explicitly includes `SCTP`, new
common-executor Kata lifetimes also accept SCTP port denies and internal CIDR
grants. This registration, which already exists, is the opt-in. It is not a new
capability flag or an inference from API syntax. Retained version-1
single-start lifetimes still reject SCTP port requests.

Native SCTP internal grants target direct endpoints, not registered Service
CIDRs or Kubernetes API destinations. Node, metadata, and management exclusions
are still mandatory. See the
[SCTP usage and qualification boundary](../../examples/native-sandbox/README.md#governed-sctp-egress).
No Cilium/Kata SCTP dataplane is qualified by offline compilation.

Rules that block mandatory DNS/Gateway destinations or ports are rejected. They
are not silently removed. Admission examines these conflicts against the
current topology when a policy is bound to a native Sandbox. It also examines
them when a referenced policy changes and when an Agent is authorized. An
unreferenced policy definition alone does not select a platform.
Capability-only injected registrations cannot authorize native deny rules
without the topology that matches.

Configure port support before you create consumers. A change to the platform
inventory is not a live capability upgrade. Public destinations outside the
deny rules keep the current egress posture.

These controls do not enable live policy rollout. For reserved policy or
platform changes, use the ordinary stop/change/resume lifecycle. Registration
and policy revisions are still inputs to current Pod admission. Source drift
cannot silently authorize the old workload that runs.

Custom observed-SNI capture also must have the numeric UID/GID of the approved
image in `customRuntime.imageUsers`. Before you approve those values, make sure
that they are the actual values of the immutable image. Both must be positive
32-bit integers. The Agent UID must be different from UID 1337, which is
reserved for the capture proxy. No numeric identity is inferred from an Agent
request or from Gateway image approval. Without capture, Custom continues to
use its image-owned user.

This applies to Custom HTTP Chat, Web UI, and headless execution on both Kata
and container. Pin the platform `ttyd.image` for the static probe. This is
also necessary for headless capture, because headless readiness examines the
managed proxy. It does not invent an application HTTP endpoint. HTTP readiness
examines both the application and the proxy.

If you use capture together with ingress mTLS, the terminal keeps its
independently approved configuration and its own TLS digest. The terminal gets
no Agent egress authority.

The empty/omitted capabilities profile keeps the qualified contract that
already exists. This contract includes mandatory `enforcementObservation`,
`restrictiveFailure`, and the qualified `azure-cni-overlay-cilium` adapter.
Unknown profiles fail closed. Native-only registrations must have the explicit
native option in both controller and configuration admission. They cannot
authorize the default held/external execution path.

Admission examines the option for the selected Sandbox of an Agent, and for all
referenced Sandboxes when it validates Team policy. An unused native
registration does not block requests that use qualified Sandboxes. If you
disable the option, new native authorization/start is blocked. But recorded
lifetime cleanup is not disabled. No profile qualifies a platform only through a
declaration of its capabilities.

When this native profile has no `sandboxSources.skillsCatalogConfigMapName` and
the effective skills `(Team skills + Agent additions) - Agent removals` are
empty, no skills PVC or published catalog is necessary. The internal
empty-skills source mounts a fresh `emptyDir` read-only at
`/home/vibe/.aks/skills`, only in the main container. It creates no publication
receipt, PVC, PV, package installer, or other grant. `skillsClaimName` is not
used for that selection and can be empty. HOME and the other native
prerequisites are still necessary.

If any selected skill is left, the configured skills PVC is still necessary. A
configured catalog keeps its current validated publication path, even for an
empty selection. Catalog errors or a recorded publication cannot fall back to
empty skills. Default held and non-native behavior does not change.

The controller stages the protected chart, records a one-shot start marker,
releases admission, and starts one StatefulSet replica. The first Pod has a
lifetime finalizer. As a result, an early deletion cannot free its name before
the controller records its UID. The finalizer can be removed only after that
durable record. Admission rejects later Pod creation for that lifetime.
Finalizer-only cleanup stays available after a hold or Agent deletion, and
admission stays read-only.

The chart gives the same native opt-in to Agent admission. Before the controller
removes the hold of an Agent, it persists the selected source snapshot while Pod
creation is still held. Admission allows only the exact hold-finalizer removal
by the configured controller. It allows this removal after it rechecks the
current native receipt/start marker, the owned stopped workload, source inputs,
and the configured coordination reservation. Other finalizer changes keep their
current rules. Neither a controller identity alone nor a status value grants
native startup.

The recorded expectation is also kept through foreground garbage collection. It
is released only after owned namespace cleanup. As a result, early Pod deletion
cannot lose the record that is necessary for finalization.

Ready must have the current application readiness probe, Node readiness, the
current Gateway grant, and workload and effective source inputs that match.
Native HOME and PVC-backed skills checks pin ownership and storage inputs, not
PVC resourceVersion. Ordinary delayed binding, status updates, and
PVC-protection finalization do not block first creation. Changed
ownership/profile or lost claims are still rejected.

Native owner access uses the current `/ui/<agent>/` route and authentication
deployment. An unauthenticated sign-in challenge alone is not an access test.
For authenticated native ingress, the protected renderer replaces OpenClaw's
`gateway.trustedProxies` with the sorted approved ingress addresses from the
same platform snapshot. By default, these are exact peer IPs that are selected
with the protected peer-UID label. The native single-start profile can
explicitly register stable Pod labels and bounded proxy Pod CIDRs instead. See
the
[managed ingress example](../../examples/native-sandbox/README.md#rollout-stable-managed-ingress).

The compiler revision binds the selector, namespace, and trust ranges through
the canonical topology. The rendered configuration must match the derived list.
No wildcard or loopback fallback is added. The ingress proxy must overwrite or
safely rebuild forwarded-client headers. Proxy trust does not bypass Gateway
authentication, owner authorization, or device authentication.

The `sandboxWorkload.trustedProxyIPs` hook that already exists carries either
exact IPs or the explicitly registered Pod CIDRs. This hook is supplied only for
authenticated native execution. Held and legacy render paths without that hook
keep their current proxy configuration.

Native Agents that wait for preparation can keep chart pins that are older than
this hook. For those charts, the protected renderer supplies the exact proxy
list through the current `openclaw.config.gateway.trustedProxies` value and
omits the unsupported hook. It keeps the original chart schema and final
proxy-list validation. It does not rewrite the pin or authorization of the
Agent. Within the single-policy API contract, the addition of this render
projection does not change the current `sandbox-controls-v1` digest for inputs
that are otherwise unchanged.

A native lifetime that already runs keeps its old runtime configuration,
workload, route, expectation, and consumed start marker across an Operator
upgrade. It is not automatically re-rendered or restarted to adopt new proxy
settings. Actual policy, topology, or execution-source changes still fail
closed. If a workload that already exists must have the new rendered
configuration, use normal deletion and a new Agent lifetime.

When coordinated configuration admission is enabled, native startup reserves the
bound Team, selected Sandbox, and effective policy in the same coordination
store. It does this before its one-shot start marker. Pending configuration
changes prevent that reservation. Relevant configuration writers are blocked
while the reservation is active or in shutdown. Pod admission and readiness
recheck the reservation read-only. Ordinary cleanup releases it only after it
observes quiescence.

Unused Sandbox and unrelated AgentPolicy edits do not become native execution
inputs. After first-Pod admission is released, an unobserved creation outcome
keeps the reservation held. This is true even if replicas are zero and no Pod is
listed now. The initial Pod must be observed and must stop through ordinary
termination. An uncertain start does not give permission to unlock
configuration or force-remove lifecycle state.

The authority name/UID comes from the current admission configuration. Keep
that authority configured until native cleanup finishes. This reservation does
not enroll a qualified writer or claim physical-fencing evidence.

After native start, HOME and skills prerequisite rechecks use the same
binding-tolerant claim digest as Pod admission. Ordinary delayed binding, claim
status updates, and the Kubernetes PVC-protection finalizer do not stop the
single-start lifetime. This is true even when they occur between reads. Claim
UID, ownership, storage requests, and protected metadata still must match.
Claims that are lost, absent, in deletion, or unreadable fail closed. Held
preparation keeps its strict snapshot checks, and this does not authorize
replacement or start replay.

This is **Kubernetes workload readiness, not guest-external enforcement proof or
physical failover fencing**. ReadWriteOncePod and ordinary Pod termination do
not establish the stronger writer-exclusion contract below. Do not force-delete
Pods. Do not use this mode for node failover, old-HOME handoff, or production
recovery.

After a hold, source change, suspension, or uncertain start, delete and recreate
the Agent. Do not try a resume or replay. In-Pod restarts are still Kubernetes
behavior. This profile does not qualify failure recovery.

Normal deletion keeps the current ownership checks and finalizers. It withdraws
the route and independently revokes the Gateway grant. It also waits for
ordinary workload termination before it removes owned storage/namespace
resources. A failed cleanup stays visible. If you disable native execution, the
cleanup obligations that already exist still apply.

Cleanup selects ordinary native quiescence from the recorded execution mode of
the protected preparation receipt, not from the current new-start option.
Earlier started native receipts keep their controller-written start marker.
Non-native or unknown lifetimes still must have their qualified cleanup
provider.

Keep the operator state namespace, enforced admission, Gateway cleanup
credentials, and any configured coordination authority until the recorded
lifetimes finish. When you disable native starts, an empty `skillsClaimName`
without a catalog is still accepted. As a result, that configuration does not
prevent a restart of the controller. Component tests and API observations do
not replace an authorized end-to-end create/connect/model/tool/delete run on the
selected runtime. See the
[native Sandbox example](../../examples/native-sandbox/) for the minimal
configuration, explicit-target run steps, and UID-conditional cleanup.

### Durable writer-exclusion orchestration

The [writer lifecycle package](internal/sandboxwriter/) records an explicitly
enrolled Agent/namespace/HOME lifetime and its immutable historical writers in
protected controller state. Enrollment must occur before every possible writer.
Neither an absent journal nor a list of Pods that are visible now proves an
empty history. Each writer records the workload, Pod, Node, physical machine
incarnation, epoch, and storage identity. Conflicts, absent identities, and
history bounds fail, and they do not adopt or discard old entries.

The journal keeps its own finalizer and exact Agent UID ownership, with
`blockOwnerDeletion: false`. When garbage collection requests its deletion, the
journal stays available for historical cleanup. After the independent cleanup
obligations of the Agent are complete, the journal does not block foreground
Agent deletion. A journal that is in deletion cannot reserve another writer.

Revocation intent is persisted before the call to the external provider. An
uncertain reply or a controller restart allows only observation, not another
destructive submission. Completion must have current evidence for the exact
recorded operation and every historical writer, a durable restart barrier, and
storage release. A one-shot CRI stop, Pod absence, zero replicas, Node
readiness, or a PVC access mode cannot supply that evidence. Cleanup uses the
recorded historical tuple even when current policy references are unavailable.

When a trusted platform integration explicitly configures the
[controller bridge](internal/controller/sandbox_writer_exclusion.go), the bridge
can supply the `SandboxWriterExclusionSource` interface that already exists. It
does not install a provider, adopt a workload that already exists, weaken
placement checks, issue a start permit, or change the default activation hold.
An independently qualified provider must implement the guest-external barrier,
the procedure for unreachable nodes, and storage release. The standalone CRI and
empty-pool experiments are not such providers.

Unit, race, and isolated API-server tests use synthetic provider responses. For
the API persistence tests, run `make test-sandbox-lifecycle` with preinstalled
`KUBEBUILDER_ASSETS`. These checks are not live runtime qualification.

### Start and shared-change coordination

The [coordination package](internal/sandboxcoordination/) supplies the ordered
protocol for a platform integration that participates. The
[native preparation bridge](internal/controller/sandbox_execution_intent.go)
must have current held preparation, a pinned ControllerRevision and grant
journal, and complete lifetime placement. It also must have current
authorization and compiled policy, and an independently enrolled
Pod/Node/epoch, physical machine, volume, and writer-journal UID. The complete
writer tuple is persisted with the coordination ticket and must still match
before reservation or cleanup. It rechecks sources, but it does not read Secret
contents or treat Agent status as authorization.

Preparation still validates every allowed Team Sandbox against the Team policy.
An execution reserves its selected Sandbox and the Team policy. As a result,
valid changes to an unused Sandbox or to the policy of another Team do not make
it necessary to stop the execution.

Non-secret sources use immutable UIDs and effective-input digests. They do not
treat the original observation resource version as lifetime configuration.
Digests keep desired content, owner references, finalizers, and recognized
runtime, ownership, network, and admission-control metadata. Unrelated labels,
annotations, managed fields, and status do not invalidate execution.

StatefulSet zero/one-replica transitions are still controlled separately.
Replacement, termination, lost storage, changed content, or more than one
desired replica still fail. These checks never read Secret contents. As a
result, Secret versions stay conservative pins. Explicit grant-approval rules
do not change.

Every execution recheck also refreshes whole-generation prerequisite approval
and compares it with the protected preparation receipt. Kubernetes prerequisite
revisions cover effective inputs, not incidental observation versions.
Publication-source content and identities are still examined, even when their
observation versions change. Withdrawal, absent providers, changed approvals,
and stale observations fail. The failure does not restage Helm or quiesce a
writer that runs.

Preparation and final-Pod admission keep the 128-source expectation limit.
Execution and coordination reserve room for all 128 prepared inputs, the
preparation-state ConfigMap, and a maximum of 130 access inputs. The access
inputs are one Service, one Gateway, and 128 distinct ingress workload
namespaces. The combined limit is 259 source identities. Duplicate dependencies
use a single slot.

Oversized inventories fail without truncation. The 512-KiB coordination-object
and compiler budgets that already exist still apply. Older readers that enforce
the 129-source execution limit must be upgraded before larger execution records
are written.

One UID-pinned controller-state object serializes shared-source readers and
exclusive changes with API resource-version comparisons. Executions that run or
are in shutdown keep their source reservations. There is no expiry, lock theft,
automatic state recreation, or adoption of a same-name replacement.

A start or source mutation records intent before its only submission. A restart
or a lost reply allows only observation. Unknown mutation outcomes keep their
reservation. This also applies when the API still shows the old object.

`AcquireTeamChange` derives the affected execution selections from an
authoritative before image and the complete proposed Team mutation. A change to
only the creation default can coexist with unaffected executions. A change to
the Team policy or the removal of their Sandbox membership cannot coexist with
them. A replacement of the Team or a change to shared runtime inputs also cannot
coexist with them.

Team mutations still serialize with one another. They pin the exact before
UID/resource version and the full desired mutation digest, and they keep unknown
outcomes. Normal configuration admission and whole-Team validation still apply.
The mutation adapter must enforce the full digest at its actual write boundary.

The lifecycle reserves the physical writer through the durable journal. Then it
must have a fresh, exact pre-start acknowledgment of an armed guest-external
gate. Current runtime, Pod, control, and epoch evidence comes before route
publication. After coordination and source reads, the route-publication boundary
guard rechecks the freshness of the same evidence. As a result, delayed
publication cannot use an expired observation. Another post-publication
observation comes before the atomic applied-binding/readiness commit of the
adapter.

Source, freshness, or observation loss clears readiness, withdraws routes, and
tries to get durable writer exclusion. A failure in one cleanup step does not
skip the other steps. Reservations are released only after all cleanup succeeds.
Historical cleanup does not rediscover targets from current policy or storage
configuration.

**This protocol does not activate the shipped controller.** The adapter must
enforce the same operation at the actual runtime, mutation, and publication
boundaries. This includes delayed submissions, restart, and observation loss. A
call to a read-only guard before an asynchronous write is not sufficient. Every
shared-source writer must participate. Direct writes must be excluded before
enablement.

No concrete production start, observation, routing, or status-commit adapter is
installed here. Neither a coordination record nor a successful callback
qualifies one. The controller and admission holds that already exist do not
change. Live governed Agent execution, model traffic, and platform qualification
are still separate integration work, and this work is not complete.

The focused lifecycle tests cover synthetic order, identity/freshness failures,
writer-history integration, and real isolated API persistence/CAS. They do not
start agent containers, contact a model service, or qualify a provider.

### Coordinated configuration updates

The [Kubernetes configuration adapter](internal/sandboxcoordination/kubernetes_change.go)
and the [explicit command](cmd/sandbox-config-change/) submit real Team, Sandbox
and AgentPolicy updates and deletions through the coordination protocol. They
are not a runtime adapter or an execution switch. Creation, Agent changes and
other execution sources stay outside this command.
The public [configuration coordination client](configurationcoordination/) is
the narrow in-process facade for trusted platform writers, for example the
Portal. It reuses these adapters and does not define a second protocol.

The before object and the proposed object must both keep the original name, UID
and resource version. Before one Update that is conditional on the resource
version, the adapter does these steps:

- It makes sure that the before image is authoritative.
- It pins the complete writable content before and after the update.
- It reserves the affected scope.
- It records submission intent.

Team changes from governed to governed keep the scope limited to the selected
Sandboxes. Legacy updates, and transitions into or out of governance, use the
conservative scope and reserve the entire Team.

Schema defaults (for example, omitted port protocols) are applied to independent
copies of the inputs before the write is pinned and submitted. Normalization
does not remove explicit values, list order or duplicates. No-op updates are
rejected before reservation, because it is not necessary for Kubernetes to
advance their resource version. Changes that only omit defaulted values are
also no-op updates. Configuration admission and Kubernetes authorization still
apply.

Sandbox and AgentPolicy updates make a separate comparison of the effective
execution digest before and after the proposed write. Typed and unstructured
AgentPolicy inputs share the same canonical representation of schema-defaulted
false booleans and empty network arrays. Optional empty grant lists use their
omitted form. Unknown authoritative fields and explicit control values are still
part of the digest.

If this digest does not change, the reservation records it. As a result,
harmless metadata updates can coexist with executions that have the same source
UID and digest. This applies to executions that already exist and to new
executions. Spec, ownership, finalizer and control-metadata changes still
exclude affected executions. The complete mutation digest still authorizes every
field. Updates of the same source that compete still serialize.

Recovery uses the complete writable-content digests. These digests contain
labels, annotations, owner references and finalizers. Status is excluded from
those digests. The effective execution digest cannot substitute for them because
it deliberately ignores fields that still belong to the configuration update.

After a conditional Update fails or its reply is lost, use the same input files
to observe the original operation. An authoritative read of the same live UID
at a different resourceVersion proves that the old conditional request cannot
commit. Recovery then examines the writable content:

- Content that matches the proposed write confirms that it applied.
- Content that matches the original configuration establishes a superseded
  update. For example, a status refresh can invalidate the original
  resourceVersion before the configuration write. The desired configuration is
  not applied.
- Other content changes, an unchanged resourceVersion, a missing or replaced
  object, termination, or a failed read keep the outcome uncertain. In these
  cases, the reservation stays.

Observation never releases a reservation. For the two confirmed outcomes,
`--action observe` reports `applied-reservation-retained` or
`update-superseded-reservation-retained`. An explicit `--action update` with the
same files then releases that operation and does not submit it again. The
CLI reports `completed` for applied content or `update-superseded` for the
original content. Read the current configuration before you prepare a new
update. Do not edit the journal, refresh the original files, or replay the old
request.

The Portal uses the same recovery rules before a save. After a restart, it can
recover a superseded update without the original request body. Its normal
UID/resourceVersion checks still reject a stale edit. After the old reservation
is released, a save with the freshly read version can continue.

For deletion, use `--action delete --before FILE` without `--after`.
Before one background Delete with exact UID and resourceVersion preconditions,
the [deletion adapter](internal/sandboxcoordination/kubernetes_delete.go)
reserves the entire source and persists submission intent. The reference checks
that already exist and Kubernetes authorization still apply. The adapter does
not remove finalizers and does not adopt a replacement object.

After an uncertain Delete result, keep the original before file and use
`--action observe-delete --before FILE`. Portal deletion recovery can instead
use the recorded operation ID with `--action observe-delete --operation-id ID`.
With this option, it is not necessary to keep the body. The UID-pinned journal
supplies only the recorded source identity and the deletion-content hash. An
unchanged object or a failed read keeps the outcome unknown and keeps its
reservation.

An authoritative observation of one of these states establishes deletion
acceptance:

- The original UID is marked for deletion at a different resourceVersion.
- That UID is absent.

In these states, a delayed old conditional Delete cannot change that object or
a replacement with the same name. Observation alone does not release the
reservation. A repeated explicit `--action delete` with the same `--before` or
`--operation-id` then finishes the same recorded operation without a second
submission. This does not prove physical cleanup. It also does not prove the
completion of finalizers or dependent cleanup.

The CLI reports `deletion-accepted`. It does not report the completion of
resource cleanup or dependent cleanup. If the original UID is still live at a
newer resourceVersion, the old conditional Delete can no longer apply.
Observation reports `deletion-superseded-reservation-retained`. Explicit
recovery reports `deletion-superseded` and releases only the old reservation,
without a Delete.

Portal deletion with a newly read UID/resourceVersion observes and clears such
an older reservation before it authorizes the new conditional request. An
unchanged uncertain request is never replayed or declared superseded. A release
after the object is marked for deletion lets normal finalizer cleanup finish.
An absent authority is still an error. It is never permission to delete.

Deletion observation reads only authoritative metadata. As a result, a larger or
newer specification in a replacement cannot block recovery for the original UID.
Reservation, admission and submission still validate the complete original
before image and enforce the same conditional Delete.

Deletion records contain a content hash for metadata-only recovery, not a body.
For records that already exist without that optional metadata, `--before` is
necessary. Update tickets cannot authorize deletion. Upgrade all strict journal
readers before you issue deletion records with the new metadata. Keep a client
that supports deletion available until its pending operations complete.

Build locally:

```bash
make -C app/cetirun-operator build-sandbox-config-change
```

To use the command, you must have separate authorization for the exact cluster
and mutation. The command must have an explicit kubeconfig/context and the
authority namespace/name/UID. An update must have the original before file and
the after input. A deletion must have the original before file or a recorded
operation ID. There is no implicit current-cluster fallback.

The default action observes an update. For deletion observation,
`--action observe-delete` is necessary. A mutation must have an explicit
`--action update` or `--action delete`. Inputs are bounded, strict YAML/JSON
configuration objects, not arbitrary Kubernetes resources (256 KiB per image).
Verified HTTPS is necessary. Redirects, exec/auth-provider login plugins and
automatic mutation retries are refused.

The output identifies the operation and phase, not the configuration contents.
For the exact argument list, run `sandbox-config-change --help`.

Whitespace and object-key order inside `managedFields.fieldsV1` do not change
field ownership. They are normalized before configuration images are compared.
Malformed or duplicate-key field sets are rejected. Other server metadata must
still stay the same, for example managers, timestamps, UID and resourceVersion.
Normalization does not allow a refresh of the inputs of an original operation.

Before execution, the UID-pinned authority must already be installed and
protected. It must use the coordination state format that already exists. The
command does not create state that does not exist. It also does not grant
privileges, bypass admission, qualify fencing or enable a workload. Before
governed execution can be enabled, all other source writers and the physical
runtime must still have their own integrations.

If coordinated configuration admission is enabled, finalizer-only cleanup uses
normal authorized API updates, not `sandbox-config-change`. This applies to a
Team, Sandbox or AgentPolicy that is already marked for deletion. Admission does
a read-only check of the current source and the UID-pinned authority. It refuses
cleanup while any recorded execution or change uses that source.

Only finalizers that already exist can be removed. Other content and all pending
reservations stay unchanged. An absent authority is not a cleanup bypass.
Ordinary configuration updates still must have inputs that are not marked for
deletion.

With this opt-in, every primary Team/Sandbox/AgentPolicy Delete must also carry
the exact submitted deletion reservation and the conditional background options.
This also applies to unreferenced configurations and legacy Teams. Upgrade
operational deletion clients before you enable it. Default-off behavior, Agent
deletion, status writes and the narrow finalizer exclusion for the Kubernetes
garbage collector are unchanged. This is not a complete all-source execution
barrier.

### Running-input verification

`CheckSandboxExecutionSources` is still the held-preparation check. After its
controlled handoff, a qualified runtime integration can use
`SandboxRunningStatusInput`. These conditions apply:

- The Agent and the persisted expectation must both be unheld.
- The workload must still match its enrolled UID/template with exactly one
  desired replica.
- The actual Pod and Node must match the separately enrolled execution.

The returned current resolution and plan contain no observation or start permit.
Pod readiness and Node health are not enforcement evidence.

Held intent capture now pins a separate `runningPreparationDigest`. It allows
only these items:

- The expected hold transition.
- The current Agent intent, after an independent recheck.
- Refreshed observation versions for sources that have a separate check of
  their effective content.

Ownership, namespace/HOME identity, template, revision, source identities and
non-effective version pins stay protected. Held tickets that already exist
without the optional field stay readable. But they cannot use the running-input
path without a newly validated, stopped enrollment. Older strict readers cannot
read new records that contain the field. Keep compatible readers until these
records complete cleanup.

The concrete Kubernetes prerequisite source has an explicit `ResolveRunning`
path. It keeps metadata-only Secret reads and exact input/approval checks. It
does not depend on a preparation hold, and it does not run Helm again or call
writer quiescence. Other prerequisite adapters must explicitly support running
reads. The controller does not silently reuse their held-only resolver.
Placement checks share the scheduled-Pod topology validation that already
exists.

The controller still dispatches governed Agents to held preparation. No code
here changes finalizers, expectations or replicas to start an Agent. Real
runtime gating, independent observations, all-source mutation coordination and
model/tool validation are still prerequisites to enable execution.

### Guarded Sandbox status persistence

`SandboxExecutionStatusWriter` supplies the status component for a runtime
adapter that is qualified separately. `CommitReadiness` must have the exact
current Running ticket, running-input verification and independent, complete,
fresh enforcement evidence. It records Sandbox enforcement and applied history
with the status helper that already exists and one UID/resource-version-pinned
merge patch. The observation age has an explicit limit of at most one minute,
and it is rechecked before and after the write. Unchanged status is validated
again and is not rewritten.

This component does not create routes, assign a new access URL, set global Agent
`Ready=True` or change the phase to Ready. If ingress is disabled, the URL stays
empty. If a status write is acknowledged but the revalidation after it fails,
an error is returned. The write is not reported as a validated transition.
Status persistence is not an atomic transaction with the coordination object or
the physical runtime. If the result is uncertain, the caller must run
coordinated cleanup.

`ClearReadiness` must have the exact Stopping ticket, but current policy or
prerequisite availability is not necessary. It clears the reported
readiness/access, follows the current suspension/deletion generation and keeps
applied history.

An opaque stop marker forces the first clear to advance the Agent resource
version. This occurs even if the Agent was already nonready. As a result,
pending positive writes that use the old version cannot overwrite the clear.
Repeated identical clears are no-ops. This is status invalidation, not route
withdrawal, process termination or writer fencing.

Stock startup does not install this component as an execution adapter. Its unit
tests and local API-server tests use synthetic runtime inputs and observations.
They do not establish live enforcement or an end-to-end governed Agent run.

### Governed native access lifecycle

`SandboxExecutionAccess` supplies concrete Istio API operations for the first
OpenClaw bring-up. Stock startup does not install it. `Enroll` accepts only
validated held preparation and creates an Agent-owned route slot that does not
forward traffic. The returned coordination intent pins that route UID and the
current Service, HTTPS Gateway, host and native port. As a separate step, it
resolves the ingress workload namespaces from the trusted platform ingress
topology. It reads their exact UIDs and includes them in the same
effective-source digests and reservations.

The VirtualService refers to the Gateway resource by its namespace/name, but it
exports only to these enrolled consumer namespaces. It is not necessary that
these namespaces contain the Gateway resource. Multiple approved consumers are
sorted and deduplicated. More than 128 distinct ingress namespaces are rejected
before namespace reads or route creation. Absent or replaced namespaces fail
closed, and the VirtualService does not export mesh-wide. A foreign, changed or
used route that already exists is never adopted.

`Publish` must have the exact Running ticket, current running-input checks and
separate fresh guest-external enforcement evidence. It updates only the enrolled
route UID and keeps the native `/ui/<agent>` redirect, HTTP and WebSocket
behavior. It does not expose terminal, desktop, sign-in helper or webhook
routes. No new access URL, global Ready condition or runtime permission is
written. Shared gateway authentication and its continued enforcement must
already be qualified independently. An HTTPS Gateway object is not
authentication evidence.

`Withdraw` uses the recorded Stopping ticket and route ownership, even when
current policy, gateway or ingress namespaces are gone. It uses the enrolled
consumer namespace names. First, it makes the exact route stop forwarding
traffic, which advances its resource version. Then it conditionally deletes
that UID and makes sure that the UID is absent.

This invalidates delayed publication updates. It does not create routes again
or touch a replacement with the same name. Unknown results keep the
coordination reservation. Route removal is not writer fencing or a reason to
release the reservation. If ingress is disabled, route absence is necessary and
no slot is created.

While such records exist, updated readers are necessary for the optional
`access` intent field and its `ingressNamespaces` inventory. Legacy intents
without access or consumer pins cannot publish authenticated access. Routes
that were enrolled before without consumer pins stay eligible only for a
cleanup that stops forwarding, followed by conditional deletion. That cleanup
uses their original Gateway-namespace export scope.

This component does not install a physical start/fencing provider or replace
the controller's held dispatch. This first native access slice does not include
support for Hermes or helper routes. Offline tests exercise API behavior with
synthetic runtime evidence, not a running proxy or an end-to-end model request.

### Current activation boundary

Legacy Agents and Teams are still supported while Sandbox execution is opt-in.
The CRD makes Agent `team`/`sandboxRef` and Team policy/Sandbox fields optional.
Without policy intent, the controller still uses the built-in or Custom runtime
path that already exists. This path includes provisioning, suspension,
readiness, and deletion.

Legacy Gateway model sources are on by default, unless they are explicitly
disabled. New legacy keys get the deployment default budget. Later
administrator budget changes, spend, reset periods, and emergency blocks are
kept. Legacy shared catalog entries and current per-deployment shared-model
groups are still usable.

If any Team policy/Sandbox field is set, that Team opts into the full binding
contract: one policy, allowed Sandboxes, and a creation default. Partial
configuration is not a legacy fallback. Policy-bound Agents must explicitly
request model sources. They must have an allowed Sandbox and current
authorization. Custom must have the common executor and an explicitly
Gateway-qualified image. See the [Custom contract](../../site/content/en/docs/reference/agent-harness.md#policy-governed-custom-execution).

Portal creation accepts legacy Teams without a default Sandbox. If the Gateway
is disabled, it also accepts no Team, as before. If the Gateway is enabled,
supported model routing and a Team that exists are still necessary. Portal
support to manage policy configuration is delivered separately. Admission still
defaults governed creation and protects exact Team/Sandbox/policy UIDs. For
later governed assignment or budget changes, an authorized administrator and a
suspended Agent are necessary.

This compatibility path does not clear hold/preparation finalizers,
authorization records, or recorded policy/Sandbox status that already exist.
These lifetimes stay governed, even after spec fields are removed. If an earlier
mandatory-policy version already held an old Agent, review its state and the
data to keep before recovery. Do not remove authority markers or delete its
namespace as an upgrade shortcut. Legacy mode does not claim AgentPolicy
enforcement.

An administrative Team is a separate Team with its own context and skills. It
does not create a privileged policy lane for other Teams. A person's permission
to manage Teams or AgentPolicies is a control-plane authorization that is
different from workload authorization. Workload authorization comes only from
the Agent's selected Team, that Team's `agentPolicyRef`, and the Agent's
explicit requests. Before a changed binding can be explicitly reauthorized,
policy-enabled resources that already exist must have a controlled stop and
cleanup. No lossless rolling migration between policies is supported.

The pre-release `normalAgentPolicyRef`, `adminAgentPolicyRef` and `policyClass`
fields are removed from the API and CRD schemas. New resources use only the
Team's `agentPolicyRef`. There are no compatibility fields, policy-class
branches, or migration readers for the former dual-policy design. This contract
targets a new system, not an upgrade of a governed deployment that already
exists.

Authorization snapshots and Pod expectations use `v1`. Coordination records
use version `1`. Policy foundation and compiled-control revisions also use `v1`
domains. These are the initial single-policy formats, not version upgrades of
the undeployed dual-policy design. Strict decoding, ownership checks and
explicit reauthorization stay fail-closed. No migration readers are added.

The controller holds these Agents:

- Agents that request Sandbox policy.
- Agents that belong to a Team with policy settings.
- Agents that already have Sandbox status or a hold finalizer.

The controller never passes these Agents to legacy built-in or Custom runtime
preparation or application. Before workload changes, it persists a dedicated
hold finalizer. This finalizer also keeps newly held Agents available for
cleanup. A change that clears spec fields cannot release that recorded hold,
even if a later status write fails. If the initial finalizer write fails, the
controller does not try workload changes. A request that is not persisted is
not a durable hold.

For resources with validated ownership, the controller removes the route and
requests zero replicas, but it keeps home storage. It reports
`SandboxReady=False` and keeps any last-applied binding. Explicit suspension
stays `Suspended`. Shutdown/removal/completion failures are reported as failures
and retried.

Built-in resources must have an owner reference with the Agent UID and
ownership/release metadata with no conflicts. Older resources without that
evidence are not automatically adopted or stopped. The hold reports an
ownership error that makes a reviewed recovery necessary. Route and namespace
deletes use UID/resourceVersion preconditions. Custom shutdown uses the
ownership checks that already exist and keeps external home storage. It retries
earlier apply-completion records and does not prepare a new workload.

Held built-in deletion removes only a validated Agent-owned namespace. It waits
until the namespace disappears before it removes finalizers, and it never runs
Helm uninstall hooks. Unknown ownership leaves deletion pending for recovery.
Deletion removes namespace contents. Home retention during a hold does not
promise retention after an explicit Agent deletion. Custom deletion keeps the
receipt and completion checks that it already has.

For held built-in Agents, Agent Identity authorization and directory cleanup
must also succeed before namespace deletion or finalizer removal. This applies
even if the runtime namespace is already absent or the identity feature was
deselected.

Portal Team edits keep policy fields that the Portal does not expose. Portal
creation registers the cleanup finalizer and binds only namespaces that it
newly created to the persisted Agent UID. Namespaces that already exist are not
adopted. For this ownership write, the Portal chart grants namespace `patch`.
This permission is in addition to the `get`/`create` permissions that the chart
already grants. When you deploy the Portal binary, deploy the RBAC that matches
it.

If that ownership write fails, creation reports an error, and the Agent stays
tracked for recovery. It must not be retried as an unrelated new Agent.

This hold is a development activation guard. It is not an enforcement adapter or
a fence for an unreachable node. Do not adopt live workloads that already exist
until these items are implemented and qualified:

- Admission.
- Approved rendering.
- Identity revocation.
- Actual enforcement observation/fencing.
- The ordered rollout path.

The guard is the temporary controller-owned stop path. Normal runtime
reconciliation cannot recreate a held workload at the same time.

### Optional configuration admission

The operator image also supports `--mode=sandbox-admission`. This mode starts an
independent HTTPS admission process, not runtime controllers or leader election.
It uses uncached API reads and a separate ServiceAccount. By default, this
ServiceAccount has only `get`/`list` access to the four configuration kinds.
Optional Pod admission adds read access to workload prerequisites and
namespace-scoped controller state, not cluster-wide Secret or ConfigMap access.
It does not mount operator cloud credentials, registry credentials, or workload
Secrets.

Agent validation fetches its Team and only that Team's named Sandbox/policy
references. If [Kubernetes-backed sources](#kubernetes-backed-sources-opt-in)
are enabled, policy configuration checks also read the selected platform
declarations and validate RuntimeClass/StorageClass pins. Shared-configuration
checks scan consumers in pages of 200. They reuse named reference reads only in
the current request. They fail on incomplete or expired page reads. There is no
cross-request reference cache.

The chart sets `sandboxAdmission.enabled`, `sandboxAdmission.enforce` and
`sandboxAdmission.podsEnabled` to `false` by default. Default installations keep
the runtime behavior that they already have. When you enable the server, webhook
rules are not installed. `enforce` is a separate bootstrap step. After
admission is enforced, even legacy Agent writes depend on webhook availability.
But healthy admission does not change their Sandbox selection.

This increment implements:

- Explicit administrator usernames/groups for shared policy configuration,
  and trusted Team assignments for policy-bound member Agents. Kubernetes RBAC
  must also authorize the operation.
  Portal administrators do not automatically get this independent authority.
- Agent selection defaults that apply only at creation, followed by validation
  against the current Team. Defaults are never silently applied again to Agents
  that already exist. Portal writes use its administrator identity and its
  service account (see
  [Availability and bootstrap](#availability-and-bootstrap)). They can keep the
  Team that the Agent owner selected.

  Direct Kubernetes writes from non-administrators must have an exact
  authenticated identity-to-Team assignment that is configured on the webhook.
  That cluster access rule is separate from the product's Team selection model.

  Mutation and validation are installed together. Kubernetes runs mutation
  first, also for dry runs. Validation rejects an absent selection. It does not
  act as if a default was persisted. Agent creation with
  `metadata.generateName` is accepted before the API server assigns a name.
  After the name is assigned, the object name and the admission request name
  must match.
- Durable admission-owned Team/Sandbox/selected-policy UID authorization on the
  Agent, independent of last-applied status. A replacement with the same name
  never grants authority to the new UID. The mutator also handles explicit
  administrator reauthorization on UPDATE, and does not apply creation defaults
  again. Mutation uses `reinvocationPolicy: Never`. Both webhooks share the
  native cleanup exclusion.
- Shared normalization and compatibility validation, and reference checks
  before shared deletion. On Sandbox/policy edits, the validation includes every
  Team that refers to the edited object. Ordinary edits to the Team context in
  the Portal can keep unchanged policy.

  Deletion of a policy-free Team keeps the RBAC boundary that already exists,
  even with member Agents. Before policy is removed from a policy-configured
  Team, or the Team is deleted, its members must be bound to another
  policy-configured Team or deleted. Members inherit policy, even without an
  explicit Agent `sandboxRef`. A change that clears the Team fields must not
  release those Agents into legacy execution.
- Protected lifecycle finalizers. Only the configured controller identity can
  change known lifecycle finalizers in a metadata-only update. It has no blanket
  exemption for spec changes. For removal, the Agent must also be already marked
  for deletion. Kubernetes' exact authenticated controller-manager or generic
  garbage-collector username can remove only the built-in `foregroundDeletion`
  finalizer. The removal must be from an object that is already marked for
  deletion, in an update that is otherwise metadata-only.

  The legacy controller's writes that pin the chart version and the Harness
  image/port stay behind the execution hold. Before a future policy-bound path
  releases the hold, it must authorize those specific writes or move them into
  authorized creation.

There are **no production capability registrations**. Standalone Sandbox and
AgentPolicy definitions can be validated. But without a qualified registration,
policy-bound Team/Agent bindings are rejected. The tests inject synthetic
registrations. When this process is enabled, those registrations do not become
production evidence, and the controller hold is not released.

An opt-in **held controller preparation** path, described below, connects
protected rendering, final-Pod checks and policy compilation. It does not
serialize concurrent shared-configuration changes, coordinate rollout/start
barriers, produce enforcement observations, or allow policy-bound execution.
Reference reads are checks against current objects, not an atomic
cross-resource transaction. Those boundaries must be completed before
production adoption.

#### Direct Kubernetes writer assignments and explicit reauthorization

For non-administrator callers that write Agent CRs directly, configure
assignments. Use the Kubernetes authenticated identity, not an Agent field or a
Portal role:

```yaml
sandboxAdmission:
  teamAssignments:
    - team: research
      users: ["member@example.com"]
      groups: ["research-members"]
```

Every user/group that matches must identify the same Team. Memberships that
conflict cause a rejection, not a precedence order. Assignments do not grant
Kubernetes RBAC permissions or independently approved workload grants. An
unassigned caller cannot create or update a policy-bound member Agent. Do not
map a shared Portal ServiceAccount as if it identified the individual human
caller.

`TeamAssignmentConflict` means that the effective memberships of the
authenticated user match more than one configured Team. Administrators must
review both the assignment configuration and identity-provider memberships.
There is no user-over-group or first-match precedence. Static configuration
validation cannot find live memberships that overlap. The diagnostic does not
show users, groups or matched Team names.

To recover after reference deletion or replacement, restore the necessary
configuration or explicitly select valid replacement references. Then authorize
the exact current UIDs with a fresh administrator request. The Agent UID and
execution hold are kept, also when several references change together. Absent
references are not a reason to clear authorization or to use legacy execution
as a fallback. Consumed reauthorization requests stay as replay state. Do not
remove either protected annotation as ordinary metadata cleanup.

Server-side apply owns individual annotation keys. We recommend that you omit
the admission-owned authorization and reauthorization keys from desired
manifests. We recommend that you do not copy these keys from a live-object
export. Repeated applies and removal of ordinary, application-owned annotations
keep the policy.

If a field manager already claimed protected keys, an administrator can make
another manager the owner of their exact unchanged values. The administrator
can do this before the original manager omits the keys. This is an ownership
handoff, not authorization removal. A forced apply cannot bypass admission. A
binding change still must use the explicit reauthorization protocol below.

The controller persists hold and cleanup finalizers with optimistic,
metadata-only patches. These patches keep sparse server-side-applied
specifications without a typed full-object rewrite. Finalization still must
have the exact controller and the narrow lifecycle transition. It does not
exempt specification, status or protected-annotation changes.

Admission stamps `cetirun.microsoft.com/sandbox-authorization` during creation.
Callers cannot supply, copy, remove or directly rewrite that record. For
policy-bound Agents that already exist without a record, and for changed
reference UIDs, an administrator must update
`cetirun.microsoft.com/sandbox-reauthorize`. The update must contain a fresh
`requestID` and the exact current `binding` tuple: Agent, Team, Sandbox and
AgentPolicy UIDs. The mutator validates that request and replaces the snapshot
atomically with the Agent update.

Reauthorization changes metadata, but not always the generation. Consumers must
not rely only on generation changes. `sandboxbinding.Read` reads the persisted
authorization through an uncached reader. Then the shared resolver must compare
it with fresh references.

#### Protected preparation, controls and status

The preparation chain is `sandboxbinding.Read` -> `sandbox.Resolve` ->
`sandboxpolicy.Compile` -> `sandboxworkload.ControlsFromPlan`/`Build` ->
`helm.PrepareSandboxChart`. The chart/renderer that already exists prepares
OpenClaw and Hermes and does not introduce another runtime renderer. Generic
user values are validated before they are merged, and the protected bundle is
applied last. The `sandboxWorkload` chart values are compiler-owned outputs,
not a user activation switch. Custom joins the common reconciled renderer. The
held-only preparation path is kept, but it does not support Custom.

Prepared manifests protect resource accounting, runtime/security settings,
owned HOME, read-only skills, scratch mounts and identity projections. Resource
accounting also covers init/helper containers and RuntimeClass overhead. HOME
and skills claims that already exist must match their trusted bindings. The
native-only empty-skills source has no skills claim identity. An incompatible
storage or StatefulSet transition fails explicitly. No live writer-exclusion or
data-adoption guarantee is inferred from the render.

Prepared Sandbox releases stay at zero replicas. The generic Helm
install/upgrade path still refuses them. The separate guarded `StageSandbox`
operation can apply only the sealed, held manifest through Helm. It never
starts Pods, runs source templates again, runs hooks or creates claims. Native
ConfigMap/Service ownership is included before the manifest is sealed, together
with the StatefulSet and compiler resources.

Retired resources are excluded from implicit removal by Helm. Grant removal
stays an identity-checked, journalled operation. Live updates must enforce
UID/resourceVersion conditions and repair custom-resource drift. A comparison
of only the old and new release manifests is not enough. Before staging accepts
a deployed release as prepared, it validates the live resources against the
sealed manifest. It allows only supported API defaults.

##### Held controller integration

[`AgentReconciler.SandboxPreparation`](internal/controller/sandbox_preparation.go)
connects the components, but it does not treat a successful render as
enforcement. This is an internal platform integration seam,
**not a tenant activation flag**. By default, the operator installs neither a
preparation source nor qualified production registrations. With no source,
policy-bound Agents keep the execution hold that they already have.

The opt-in Kubernetes sources below add authoritative reads of platform
configuration and of owned resources that already exist to the shipped binary.
Without the separate native execution opt-in above, they do not supply a
qualified writer-exclusion implementation. As a result, held staging cannot
complete through stock startup. Tests inject that separate dependency. Their
results are not platform qualification or a permit to start.

An operator namespace alone does not enable preparation or routine state reads
for untracked Agents. When a provider is disabled, cleanup is still necessary
for preparation markers that already exist. Deletion also probes state while
the hold finalizer stays. As a result, if the preparation marker is already
released, interrupted finalization is not lost.

A trusted integration must supply these items:

- The same fresh `SandboxConfigurationSource` that the Sandbox and AgentPolicy
  configuration reconcilers use.
- `SandboxPreparationSource` approvals for the complete Agent generation, chart
  version, helpers and protected namespace UID. The approvals also cover the
  HOME/skills PVC UIDs and the runtime Secret UID that already exist.
  Namespace/storage/model provisioning, controller write permissions and
  serving admission are platform prerequisites. The controller does not find
  or adopt them by name alone.
- A qualified `CheckQuiesced` implementation. It must confirm that previous
  writers stopped or are durably fenced, also on unreachable nodes. It must
  support cleanup with the persisted earlier namespace/HOME tuple after
  suspension, reference deletion and restart. A Kubernetes scale request or an
  empty Pod list is not this evidence.
- A held Helm installer with authoritative, read-only release-store inspection,
  an explicit protected state namespace, and the dedicated admission
  ServiceAccount. There is no trusted-source fallback to Agent status or
  arbitrary tenant ConfigMaps.

The controller resolves every referenced Team slot and validates current
runtime/storage/grant approvals. New or changed preparation first confirms
qualified quiescence and Pod absence. Then it compiles controls and renders the
native chart. A failed preflight creates no preparation receipt, grant journal
or Agent preparation finalizer. Preparation state that already exists still
must have its qualified cleanup fence.

The controller persists ownership and write-ahead grant inventory before any
grants or workloads are applied. Old grants are revoked and observed absent.
Fresh source checks, qualified quiescence and Pod absence still guard held
staging. Source-read RBAC gives admission only `get` on the named workload
Secrets and ConfigMaps, not blanket namespace Secret reads.

After staging, the controller records binding-object UIDs and an
`ExecutionHeld=true` Pod expectation in the operator namespace. Source
UID/resourceVersion snapshots include the Team, referenced configurations,
RuntimeClass/StorageClass, namespace, claims, Pod sources and compiled
controls. The Agent instead pins UID, generation, and a digest of its spec,
protected authorization/reauthorization metadata and lifecycle holds. Status
writes and unrelated labels/annotations do not invalidate it. But
metadata-only reauthorization still invalidates it.

This is expectation format `v1`, separate from the render contract. Controller
and admission use the same initial format. Malformed or unsupported
expectations fail closed. Do not edit stored JSON or remove holds to bypass
these checks.

Successful attempts refresh every 15 seconds. As a result, source changes,
prerequisite expiry and explicit reauthorization are not missed. An unchanged
attempt uses a controller-owned digest of approved inputs, render values,
confirmed grant inventory, the expectation, native Service identity and the
deployed Helm record. It rechecks source identities, qualified fencing and Pod
absence. But it does not render, stage, rewrite journals or publish
expectations again.

A change to any of these inputs returns to the guarded preparation path.
Absent or corrupt durable inventory, or unavailable authority, stays an error.
All mutation guards in the controller and the Helm action stay in place.

The ordinary/admin fake-client regression fixtures enforce a bounded read-only
reuse path. This path has zero writes, one release-store inspection and one
fence check. These are regression budgets, not production throughput
measurements. Providers and release storage can send more requests. Current
grant-journal and observed ControllerRevision identities are included in the
source reads.

Inventories stay bounded to 128 sources and 512 KiB per expectation, with a
one-Pod existence query. No production fleet size is qualified. Activation
review must establish a measured capacity limit. It must also evaluate more
dependency watches together with the Agent/Team/Sandbox/AgentPolicy watches and
expiry polls that already exist.

`AgentConfigReady=True` with reason `SandboxPrepared` means
**held preparation only**: `SandboxReady` and `Ready` stay false, the access
URL is empty, and applied-binding history is not advanced. Success is
published only after the held expectation persists and its source versions are
rechecked.

##### Current held start-input pins

The [expectation publisher](internal/controller/sandbox_preparation_sources.go)
adds only the identities that are necessary to validate a prospective start.
These identities are the UID and resourceVersion of the validated grant
journal, and the owned ControllerRevision. The publisher adds the
ControllerRevision after the StatefulSet controller observes the current
generation.

The actual recorded template of the revision must match the sealed native
template. This match also covers the Kubernetes template-replacement patch
form. A stale generation does not get a current revision pin. Both the
operator and admission get read-only `get` access to ControllerRevisions.

These references use the `v1` expectation source format. They are not another
intent journal or an execution permit. Held expectations without the current
pins are rebuilt through guarded preparation. A start action must explicitly
make sure that the complete current identities are present. It must not infer
qualification from the format version alone.

No preparation-receipt upgrade, attempt history, new finalizer or GC lifecycle
is introduced. The `v1` cleanup receipt that already exists keeps its 64 KiB
bound and historical namespace/HOME tuple. Unsupported receipt formats still
fail closed. The current expectation keeps the 128-source/512-KiB bounds and
the Agent owner reference that already exist. It is subject to the fenced
cleanup and GC that already exist.

**Controlled start is not implemented.** Both execution holds and zero-replica
staging stay in place. Stock startup cannot reach successful preparation,
because its writer-exclusion provider is unavailable. Synthetic API tests do
not demonstrate production reachability, runtime enforcement or a qualified
fence. An actual start must have separately approved start/change exclusion,
lifetime pool isolation, qualified old-writer fencing, a distinct pre-start
enforcement acknowledgement and trusted current Pod/node/epoch observations.
Neither a held expectation nor pool deletion alone supplies those authorities.

This native path still does not support Tailscale or workload identity. Custom
must use common reconciliation, not the single-start execution that is kept.
Unsupported requests keep an explanatory `UnsupportedControl` hold and a
bounded requeue, not a permanent preparation-error loop. If a previously
prepared Agent changes to one of these requests, the grants that already exist
are revoked.

Unqualified platform controls also keep the bounded `UnsupportedControl` hold.
Unavailable adapters, absent inventory and API read failures stay preparation
errors. They are not reported as unsupported. An unsupported result does not
suppress source, cleanup or status-write failures.

If a recorded binding disappears after preparation, the controller
authoritatively confirms its absence. It durably retires its old journal entry
before it records fresh write-ahead intent and creates a replacement. The same
check covers admission source-read bindings, namespaced workload RoleBindings
and workload ClusterRoleBindings. It also covers deletion during Helm
preflight.

Staging guards recheck confirmed grant identities. Unrelated bindings with the
same name are not adopted, overwritten or revoked, and healthy bindings are not
retired. Read errors, journal conflicts and unconfirmed retirement prevent
replacement creation. New UIDs are confirmed for reuse and later revocation.
Interrupted retirement or creation resumes through the normal write-ahead
preparation path.

Partial writes, stale sources, suspension and provider disablement revoke
recorded grants, but the hold and recovery state stay.

Deletion revokes access first. Then it waits for qualified writer fencing and
for all Pods, also Pods that are marked for deletion. It removes the owned
namespace only after that. The controller releases preparation state and
finalizers after the namespace is absent. An absent or corrupt journal, foreign
ownership, an unavailable fence, or a state update that conflicts blocks
completion. Such a condition does not imply successful cleanup.

During the deletion of Sandbox-held native Agents and legacy Custom Agents, a
grant-journal read or revocation failure does not skip these operations:

- Independent workload stop requests.
- Custom terminal shutdown.
- Owned-route removal.
- The operations that try to clear readiness and the access URL.

Status-write failures also do not skip revocation or shutdown. Errors from
these independent operations are joined. Until a retry succeeds, they block
registry credential cleanup, namespace/storage cleanup and finalizer release.

Both deletion paths also remove the managed registry credentials that were
kept. They do this after revocation/shutdown requests and before namespace
cleanup or finalizer release. Registry cleanup failures keep the deletion state
and a bounded retry. They do not skip shutdown or bypass the necessary Sandbox
writer fencing.

After an interrupted journal-finalizer release, cleanup can resume only when
the record that is left is valid, owned and empty. This cleanup-only recovery
does not authorize new grants. It also does not accept unprotected inventory
that is not empty. Foreign finalizers are kept, also when they hold an empty
journal that is marked for deletion.

Local tests exercise this chain with synthetic platform providers and an
isolated API server:

```bash
go test ./internal/controller -run TestSandboxPreparation -count=1
KUBEBUILDER_ASSETS=/path/to/preinstalled/envtest make test-sandbox-integration
```

The held-input cases exercise real API-assigned identities, expectation CAS
conflicts, grant-journal races and restart recovery. They also exercise both
native runtimes, default/explicit qualified images and HTTPS Pod admission
denial. The ControllerRevision producer and platform approvals are synthetic.
These tests do not run agent containers or establish runtime, Cilium, CSI,
cloud identity or writer-fence qualification.

Storage/identity handoffs, retired non-binding controls, shared-configuration
reservations, authorized starts, Pod/node/epoch observations and route
publication still must have their qualified lifecycle integrations. Pending or
incompatible Helm releases fail closed for coordinated recovery. They are not
adopted or force-unlocked.

##### Kubernetes-backed sources (opt-in)

In the operator chart, `sandboxSources.enabled` selects capability declarations
for configuration admission. It also selects sources for held native
preparation. It serializes `CETIRUN_SANDBOX_SOURCES` for the controller. It
also derives `platformNamespace` and `platformConfigMapName` in the
`CETIRUN_SANDBOX_ADMISSION` of the admission process. It derives them from the
same chart `namespace` and `sandboxSources.platformConfigMapName`.

There are no separate admission-source Helm values. When sources are disabled,
the chart does not set the two admission fields or the source setting of the
controller.

For this option, configuration and Pod admission must be
enabled and enforced (`sandboxAdmission.enabled`, `enforce`, and `podsEnabled`).
The admission ServiceAccount is always the separately deployed
`cetirun-sandbox-admission`. It is not the controller ServiceAccount.
Admission uses the same operator-namespace ConfigMap `get` Role and
Pod-admission RuntimeClass/StorageClass `get` permissions that it already has.

Enabled sources also grant the controller `get`, `list`, `watch`, `create`,
`update`, `patch`, and `delete` on namespaced `ciliumnetworkpolicies.cilium.io`.
Guarded Helm staging must have these permissions for the policy resources of the
compiler. The ClusterRole of the controller grants these permissions
cluster-wide. But the staging path still examines the exact Agent ownership and
namespace identities. Admission stays read-only, and no cluster-wide Cilium
policy or endpoint write permissions are granted.

Sources also grant the controller `get` on Istio Gateways for exact
access-source pinning. This path does not create, update, or delete the shared
Gateway. The optional skills publication catalog described below adds only
PersistentVolume `get` for its metadata dependencies. It does not add write
permissions, Secret lists, or cluster-wide ConfigMap access. When this option
has its default value (off), the chart output does not change.

```yaml
sandboxSources:
  enabled: true
  bootstrapOwnedPrerequisites: false
  platformConfigMapName: sandbox-platform
  homeClaimName: home
  skillsClaimName: skills
  skillsCatalogConfigMapName: ""
  helpers:
    doctor: false
    logger: false
    desktop: false
```

These are operator settings. They are never Agent or Team Helm overrides. The
constructor uses the selected operator namespace and chart version. The
[configuration source](internal/controller/sandbox_kubernetes_configuration.go)
and configuration admission read the same `platform.json` from the named
ConfigMap that already exists in that namespace.

Its `v1` document contains `registrations`, `platforms`, `runtimeClasses`, and
`storageClasses`. It uses the same inner contract types as before, with their
case-sensitive JSON fields. Registrations use `Capabilities`/`Available`.
RuntimeClass pins use `UID`/`Handler`, and StorageClass pins use
`UID`/`Provisioner`. Capability and platform objects keep their lower-camel
field names.

RuntimeClass and StorageClass declarations pin their UIDs and
handler/provisioner. Role approvals pin the role identity, version, and rules.
Live role checks apply only to the grants that each policy requests during
assessment and workload preparation. This includes mutation guards.

A missing, stale, unavailable, or ambiguously approved role rejects that
request. It does not make the shared snapshot invalid. It also does not make
unrelated policies invalid, for example policies without Kubernetes access.
Different policies can have different approval pins for the same role. Each
request must match its own pins.

Replacement, concurrent change, malformed configuration, and API failures are
errors. They do not cause automatic reapproval or a fallback to cached tenant
data. The limits for the document are 512 KiB, 16,384 JSON tokens, 12 nesting
levels, and 128 distinct API objects. The 128 objects include the ConfigMap.
Unknown/duplicate fields, nulls, and incomplete object pins are rejected.

Each relevant configuration-admission request uses current authoritative reads.
There is no cross-request inventory cache, and no fallback to earlier
declarations. Missing, inaccessible, malformed, or drifted platform inventory
blocks policy-bound configuration writes that must have capability checks.

This source dependency does not apply to these operations: configuration
`/status`, Agent deletion, narrowly authorized controller cleanup, authorized
suspension-only updates, and unrelated legacy writes. A suspension must change
only `spec.suspended` from false (or omitted) to true. All other spec fields,
authorization records, and caller-controlled metadata must stay the same. Only
server-managed generation and field ownership changes are ignored.

The current caller-authority and lifecycle checks still apply. For creation,
resume, binding changes, and reauthorization, valid inventory is still
necessary. This does not change the webhook-outage behavior. When admission
itself is unavailable, other matched writes still fail closed. This includes
legacy writes.

The platform owner must protect this ConfigMap from tenant writes. The platform
owner must also keep its qualification records, complete topology, and
explicit approvals current. The chart does not create that inventory or set
capabilities to true. You can write an empty inventory that claims no platform
support:

```json
{
  "version": "v1",
  "registrations": {},
  "platforms": {},
  "runtimeClasses": {},
  "storageClasses": {}
}
```

The [prerequisite source](internal/controller/sandbox_kubernetes_prerequisites.go)
selects the protected Agent namespace and the configured HOME/skills claims that
exist (or the native-only empty-skills source described above). It examines
the ownership and current identities. It reads only the metadata for the
`gatewayTokenSecretRef` of the Agent. It never provisions or adopts storage by
name. It never generates model credentials, and it never reads Secret contents.

Supply these resources through the trusted platform, or opt in to the limited
owned bootstrap described below. The reader itself is not a storage migration
or credential-issuance mechanism.

The namespace must already have its protected label. It must also have
Agent-name/UID annotations that match, and the exact controller owner
reference. A retained HOME without an owner reference is supported only with
the current Agent UID annotation. It must also have the exact filesystem
storage profile of the selected Sandbox. Skills can keep their supported
shared/RWX shape, with no owner or with the current Agent/Team owner.

The runtime Secret must be Agent-owned. Its contents, usability, and
immutability are not derived from metadata. Pending claims are allowed for
delayed-binding storage classes. But a pending claim is not writer-exclusion
evidence.

Both sources return revisions that are bound to the inputs that they examined.
They reject changes that occur during capture. Each configuration observation
has the date of the start of the read. After the read completes, consumers
examine freshness. They reject observations that expire during capture.

Configuration admission and the Agent, Sandbox, and AgentPolicy reconcilers use
the same platform declarations. These declarations do not give Portal callers
Kubernetes admission authority. They also do not replace trusted Team
assignments. A configured source does not give runtime, network, or CSI
enforcement. In particular, the default prerequisite source has
no writer-exclusion adapter. It returns an explicit error before preparation
creates its qualified-preparation cleanup state.

Bootstrap-only ownership state is separate. It does not claim a writer fence.
You can delete an Agent that this preflight rejects. This is also true after
sources are disabled. Preparation state that already exists never bypasses
fencing. Kubernetes Pod absence, PVC access modes, and zero replicas cannot
replace fencing.

Qualified fencing, successful held staging, controlled starts, and running
enforcement observations are still separate lifecycle work. Do not remove holds
or finalizers to bypass these missing guarantees. The separately enabled native
profile uses ordinary Kubernetes quiescence. It never uses this qualified
fence.

##### Owned bootstrap (separate opt-in)

With `sandboxSources.bootstrapOwnedPrerequisites: true`, new native policy-bound
Agents can create the prerequisites that they own while they stay held. For
this flag, Kubernetes sources must be enabled. The same enforced
configuration/Pod admission prerequisites also apply. Its default value is
false. Namespace inputs that already exist stay on the read-only path. They are
not adopted or mutated.

The Agent must explicitly set the configured approved `spec.helm.version`,
the native `spec.agent.image`, and the `spec.agent.port`. Bootstrap does not
write these fields or add a controller admission exemption. After the current
binding/platform checks, it records the ownership intent. Then it creates the
protected Agent namespace and the existing operator Role/RoleBinding. It also
creates a HOME that matches the selected Sandbox, and the native authentication
Secret that `gatewayTokenSecretRef` names.

It uses the ordinary OpenClaw/Hermes key initialization. Bootstrap does not
read or enrich credentials that already exist. No cluster-wide RBAC grants are
added.

Bootstrap does **not** create an empty replacement for published skills,
model-provider credentials, Pods, routes, or execution permits. By default, the
platform must supply the configured, distinct skills claim in the new
namespace. The claim must have the supported Agent/Team or retained ownership.
Missing selected skills are still an explicit prerequisite error.

As an alternative, the separately selected publication catalog below can
create static bindings to content that is already published. The only
exception is the explicitly enabled native profile with an empty effective
selection, no Team shared skills storage, and no recorded publication. A
globally configured catalog does not require an empty Team to publish. It uses the read-only in-Pod directory described
above. It does not use an empty replacement for published content. Without
native execution, the stock binary still stops because qualified writer
exclusion is not available. The native authentication Secret is not evidence
that a model provider issued usable credentials.

A bounded controller-owned receipt records each created UID. Each object is
marked with the UID of that receipt, so that an interrupted create can be
recognized. For confirmed missing/replaced objects, storage resize, foreign
objects, and changed ownership, recovery is necessary. Automatic recreation
does not occur. Similarly, if you change the native type, Secret name, or
approved chart, an explicit handoff is necessary.

The operator can migrate one known older bootstrap Role in place: the complete
rule set from before mTLS support, whose Istio rule lacks `destinationrules`.
Before and after an optimistic patch, it examines the recorded Namespace, Role,
and RoleBinding identities, ownership, protection, and current bootstrap inputs.
Other rule differences are still errors. This is not a general RBAC repair.

If an upgrade left an Agent held with `Sandbox bootstrap operator Role changed`,
install an operator that includes this migration. Then let normal reconciliation
retry. When the recorded Role matches, the operator updates it and does not
delete the Agent, namespace, HOME, or Gateway key. Agents already held can
recover with the same budget and spend identity. Do not delete and recreate the
Agent, clear finalizers, or replace its storage to work around this error. For
unrecognized rules, missing objects, or changed identities, a reviewed recovery
is still necessary.

Every bootstrap-owned resource has its own finalizer. The reason is that Agent
finalizers and `blockOwnerDeletion` do not stop the deletion of dependents by
foreground garbage collection. Also, a namespace finalizer alone does not keep
the contents of the namespace.

Dependent DELETEs can mark HOME, native authentication, and operator RBAC as
terminating. But their bootstrap finalizers keep the objects until these items
succeed: independent shutdown, receipt/ownership validation, and any preparation
fencing obligation that exists. Cleanup removes only its own finalizer, with
resourceVersion checks. It releases HOME/credentials before RBAC. It releases
namespace protection last.

Interrupted cleanup retries, but it does not recreate resources or clear the
finalizers of another controller. You can delete bootstrap-only Agents without
a fence that was never used to authorize preparation. If you disable bootstrap,
its inventory stays. Recorded inputs also cannot bypass ownership checks.
Deletion is still available.

Tests cover cold bootstrap, lost API responses, source/UID changes, immutable
HOME, retries that keep credentials, and cleanup. The tests use synthetic
fixtures and an isolated API server. Foreground-GC regressions explicitly send
dependent DELETEs, because these test environments do not run a garbage
collector. They do not qualify runtime/storage/network behavior.

###### Published Blob NFS skills catalog (separate opt-in)

Set `sandboxSources.skillsCatalogConfigMapName` to a **separate, pre-existing
ConfigMap in the operator namespace**. Also enable both the sources and the
owned bootstrap. The default is empty. This setting does not change
`platform.json` or the configuration-admission wire contract.

The operator never creates or updates the catalog. It never publishes source
content or contacts Azure. It never imports caller PVs/Secrets or resolves
storage credentials. `Team.spec.skillStorage`, Agent Helm values, and
namespace-local ConfigMaps are not publication authority. Use
operator-namespace RBAC to protect catalog writes.

For example, this synthetic catalog binds a publication to one exact Team
name and UID. Replace the example UID with the UID of the intended Team object:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: sandbox-published-skills
  namespace: cetirun-system
data:
  skills.json: |
    {
      "version": "v1",
      "teams": {
        "example-team": {
          "teamUID": "00000000-0000-4000-8000-000000000001",
          "revision": "published-v1",
          "source": {
            "resourceGroup": "skills-example",
            "storageAccount": "skillsexample123",
            "containerName": "skills-v1",
            "storageClassName": "azureblob-nfs-premium",
            "storageSize": "1Gi"
          }
        }
      }
    }
```

The ConfigMap must contain only `data["skills.json"]`, with no binary data.
The case-sensitive `v1` document must have `teams`, which can be empty. Each
entry must have all the fields that the example shows. The limits for the
document are 64 KiB, 128 Teams, 8,192 JSON tokens, and five nesting levels.
Unknown/duplicate fields, nulls, malformed descriptors, Secret references,
protocol overrides, and mount options are rejected.

If there is no approval for the current Team name **and UID**, a prerequisite
error occurs before cold-create effects. A same-name replacement Team does not
inherit approval. The publisher must keep the content stable for the selected
revision. The publisher must publish changes under a new approval/source. The
operator does not examine or hash NFS contents. The revision is an approval
identifier, not storage qualification.

New catalog-backed bootstrap uses a `v2` write-ahead receipt. The receipt
contains the catalog UID, the selected Team/publication, and the two binding
names. It creates the Agent-owned skills PVC with an explicit `volumeName`, and
records its API UID. Then it creates a deterministic per-Agent PV with that
exact PVC UID in `claimRef`. This is static Blob NFS binding, never an empty
dynamically provisioned claim.

It follows the shared-skills RWX shape that exists. It also enforces CSI
`readOnly`, the `ro` mount option, read-only native workload mounts, and the PV
reclaim policy `Retain`. HOME stays a different claim with its own authorized
profile. No shared source object or backing data changes.

Each returned object UID is recorded before the next effect. Interrupted
creates can resume only for the exact receipt-marked intent. For foreign
objects, replacement UIDs, and immutable spec drift, recovery is necessary. The
selected catalog UID, Team UID, revision, and source cannot change in place.
Unrelated catalog edits are allowed. But every attempt guards the newly read
resourceVersion.

If you remove/change the selected approval or disable its catalog, further
preparation is held. Then recovery is necessary. Automatic migration or
fallback does not occur. Namespaces that already exist and older `v1`
bootstrap receipts keep their externally supplied skills mode. If you enable a
catalog, it does not adopt or migrate them.
New native empty-skills bootstrap records also carry an explicit empty-mode
annotation on their operator-owned receipt. This preserves that choice if a
catalog is enabled later. The receipt JSON versions and existing object
identities remain unchanged. Older controllers ignore the annotation and may
hold these new Agents until their original controller is restored; stop new
candidate fixtures before rolling back the controller.

Held preparation includes the catalog, bootstrap receipt, and PV identities in
the admission source inventory that it examines. With the catalog enabled, the
admission chart adds only cluster-scoped PersistentVolume `get`. It adds no
list, write, or Secret permissions. Catalog/receipt metadata use the
operator-namespace ConfigMap `get` Role that admission already has.
Namespace-local runtime source grants stay name-scoped.

Deletion uses the durable receipt. The live catalog is not necessary. The
shutdown and ownership gates still apply. Any qualified-preparation fencing
gates also still apply. Skills bindings carry the bootstrap finalizer through
foreground deletion.

The controller releases the PVC with namespace cleanup. When the namespace and
the PVC are absent, it deletes only the recorded `Retain` PV. For this deletion,
it uses UID/resourceVersion preconditions. It removes only its own finalizer.
It keeps the Agent/receipt protection until it finds that the PV is absent.
Other controller finalizers and the published backing data stay.

This is not a source-content deletion or a cloud fencing implementation.
Launch image/chart/port inputs stay explicit. There is no new controller
spec-write exemption. Portal namespace precreation and identity delegation are
still separate work. Catalog bindings, pending PVCs, and successful held
rendering do not qualify CSI/NFS behavior, provider credentials, execution
starts, or running enforcement.

**Manual recovery after forced finalizer removal.** Do not force-remove Agent
or state finalizers as routine cleanup. If an Agent disappears outside the
normal lifecycle, its journal, bootstrap, and preparation receipts keep their
finalizers by design. They can block state-namespace deletion. There is no
automatic orphan reaper. A separately authorized recovery must:

1. Keep and strictly validate the original Agent UID and the journal/receipt
   identities. Also keep and validate the complete grant inventory and the
   recorded namespace/HOME tuple. Missing or corrupt evidence does not let you
   replace it with empty state.
2. Compare the UID of any current same-name Agent. A replacement is unrelated.
   Do not adopt it, and do not remove its resources. For a terminating Agent
   that matches, we recommend normal controller cleanup.
3. Quiesce in-flight grant writers. Get the qualified fence of the platform for
   the original writers. Agent absence, operator shutdown, scale-to-zero, and an
   empty Pod list are not sufficient evidence.
4. Examine the owner UID, role, subject, and revision. Then revoke every
   recorded binding, also the obsolete entries. If an inventory entry is older
   than the UID confirmation, pin the actual object UID/version. Use deletion
   preconditions and authoritative absence checks. Terminating or replaced
   bindings stay unresolved. Discovery adds to durable inventory, but never
   replaces it.
5. Persist only confirmed removals, with resourceVersion checks. First, confirm
   the empty inventory, the continued fencing, and the original
   namespace/lifecycle cleanup. Then release only the finalizer of the journal
   itself, and confirm the deletion. Handle the receipt separately under its
   original identity. Keep foreign finalizers. Do not finalize the full
   operator namespace to discard state.

This procedure is not a tested live recovery tool. Before activation in a
shared environment, a reviewed and qualified orphan-recovery mechanism is
necessary.

When the desktop helper is enabled, it uses a separate `tmp-desktop` emptyDir
for `/tmp`, and the Agent keeps `tmp`. Both volumes have the Sandbox scratch
size limit. The container/Pod resource accounting that exists does not change.
This keeps their Supervisor configuration, socket, and PID files independent.
The protected inventory rejects shared, missing, unbounded, or changed scratch
mounts/volumes. If you disable desktop, its scratch volume is not included.

#### Trusted Sandbox policy transition logs

The operator's `sandbox-policy` logger emits structured
`sandbox.policy.accepted`, `sandbox.policy.rejected`,
`sandbox.policy.applied`, `sandbox.enforcement.failed`, and
`sandbox.enforcement.lost` events. It emits them after optimistic status
persistence succeeds. These are operator logs, not Agent stdout or Kubernetes
Event mirrors. Configuration acceptance does not establish Agent enforcement.
A rejection can include unavailable prerequisites. The value
`conditionStatus=Unknown` identifies these rejections.

Preparation errors go through the shared typed failure classifier before status
persistence. As a result, an approved role that is absent or that drifted emits
a committed failure, not an ordinary pending hold. Unavailable trusted
dependencies set `SandboxReady=Unknown` with the reason
`ObservationUnavailable`, and `Ready` and `AgentConfigReady` stay false.
Ordinary pending/prepared holds stay silent. Error propagation, grant
revocation, and the conditions that make explicit reapproval necessary do not
change.

Events include these fields:

- UTC timestamp
- Bounded reason
- Resource UID and generation
- Condition status
- Stable `transitionID`
- Separate `attemptedBinding`/`attemptedRevision` and
  `appliedBinding`/`appliedRevision` fields

The attempted Agent identities come from admission-owned authorization
metadata, not from applied history. If a field is not resolved, its value is
explicitly `unresolved`. At these hooks, configuration assessments and failed
status paths have no confirmed attempt revision. They report this revision as
unresolved and do not reuse the previous applied revision. The loss event
reports the previous applied history separately from the current attempt.

Events keep canonical Kubernetes UUIDs and compiled SHA-256 revisions. SHA-256
fingerprints represent other opaque identity or revision values. Event fields
do not contain raw specifications, Secret data, free-form status messages,
provider errors, or conversation content.

Application events occur only when the applied status is committed, confirmed,
and for the current generation, and the authorization matches. Held
preparation is not application. Intentional suspension and deletion are not
enforcement failures. This is also true when a previous failure condition
still exists. These logs do not add observation, fencing, or permission to start
execution.

An unchanged semantic status does not emit events repeatedly. Correlation IDs
do not contain timestamps, resource versions, or free-form content. They are
not unique delivery receipts. Status persistence and log delivery are not
atomic. As a result, exactly-once delivery across crashes is not guaranteed.

Protected Pods use `DNSPolicy=None` with exactly the one to three distinct
resolver IPs from the compiler. No fallback resolvers, search suffixes, or DNS
options are added. User overrides and final-Pod DNS changes are rejected. They
do not silently change the trusted network binding.

The candidate Azure CNI Overlay/Cilium compiler emits explicit network controls
and per-Agent identity resources from trusted topology and grant inventories.
Gateway endpoints and ports come from that inventory, not from hardcoded
defaults or policy-supplied destinations. Necessary destinations that conflict
cause a rejection. FQDN blocking and qualified-adapter mTLS are still not
supported. They cause a rejection and do not become no-ops. The native common
executor has the separately registered
[managed mTLS endpoint](../../site/content/en/docs/guides/agent-ingress-mtls.md).

The effective compiled revision includes topology, identity, and role-content
changes. Use this revision, not only the base resolver revision.

The qualified candidate adapter still rejects FQDN capability declarations.
The common native Kata executor accepts `Capabilities.blockedFQDNs=true` only
with an operator-owned `platforms.kata.egressSNI` image registration.
Ordinary-container registrations must keep `blockedFQDNs=false`. This enables
the bounded observed-SNI implementation below, not full hostname enforcement.
`blockedFQDNs` accepts exact lowercase ASCII hostnames, not suffixes,
wildcards, URLs, or IP literals. DNS-derived IP sets are not equivalent to
exact observed-SNI denial, and no DNS-to-IP conversion is used.

The trusted topology must explicitly supply `DNSResolverIPs`, the one to three
distinct Pod resolver addresses, separately from the `DNS` peer membership.
Resolvers must belong to approved DNS peers and use qualified IP families.
Absent or unapproved resolvers cause a rejection. The compiler does not infer
resolvers from backend addresses or truncate the inventory. Legacy DNS peers
keep all Service and backend IPs for network allowances and DNS deny
exemptions. In that mode, new backend replicas change the enforcement
revision, not the Pod's nameservers.

Native Cilium DNS and AI Gateway peers can instead register protected,
namespace-scoped `podSelector` labels with only stable Service IPs. Pod
replacement and scaling then require no manual peer labels, backend IP
updates, or network-policy revision change. Exact ports and complementary
denies remain enforced. This opt-in rejects blocked CIDRs that overlap Pod
networks, because changing selected backend IPs cannot be represented by
stable endpoint denies. See the [service selector contract](../../examples/native-sandbox/README.md#rollout-stable-dns-and-ai-gateway)
for namespace protection, migration and validation limits.

The current egress posture allows public IP destinations, except explicit
exclusions. It restricts private, cluster, node, metadata, and management
ranges, and it qualifies the necessary DNS/Gateway destinations. It is not a
public-destination allow-list. Cilium default-deny establishes isolation for
traffic that no rule matches. It does not remove the compiler's explicit
public-egress rules.

An exclusion of the whole address space causes a compilation failure. It does
not produce an empty allow rule. Cilium selectors use source-qualified keys,
for example `k8s:`. Their JSON shape is not a Kubernetes API-machinery
label-selector contract.

Network compilation uses these fixed budgets:

- 1,024 input entries
- 32,768 partition work units
- 2,048 rules
- 16,384 expanded entries
- 512 KiB for the complete serialized Cilium policy, which includes the
  ownership/revision metadata

The [budget accounting](internal/sandboxpolicy/limits.go) limits the work
before expansion, and it also limits the final output size. If a plan exceeds
a limit, the plan is rejected. Entries are never silently truncated to get a
successful result.

Independently of those budgets, emitted allow and deny groups contain at most
40 entries in each `toPorts[].ports` array. This limit agrees with the
validation limits of Cilium. The compiler groups entries after it combines
port sets and computes complementary deny ranges. This computation also
includes internal CIDRs that overlap and merged managed-peer destinations.
When the compiler groups entries, it keeps the complete allowed and denied
port/protocol sets, and also fragmented ranges. No entries are truncated.

Ordinary workloads keep the default ServiceAccount without token automount.
Approved Kubernetes API and Entra identities use separate audience-bound token
projections. An Entra-only identity does not implicitly authorize Kubernetes
access.

#### Entra runtime projection core

`AgentPolicy.spec.entraID.enabled` selects the generic workload token
projection. It does not select `Agent.spec.agentIdentity` or the Broker's
credential-service authorization. The compiler must have a current platform
approval for these exact items:

- Agent UID
- Workload namespace
- Dedicated non-default ServiceAccount
- Approval revision
- Client ID
- Tenant ID

An approval inventory cannot assign one ServiceAccount to two Agents in the
same namespace. A disabled identity projects nothing. A Kubernetes-only policy
does not inherit an Entra approval that it did not request.

The current compiler and common chart emit a read-only `0440` projected token.
The token has the audience `api://AzureADTokenExchange` and a 600–3600 second
lifetime. The compiler and chart also emit the three explicit
`AZURE_CLIENT_ID`, `AZURE_TENANT_ID`, and `AZURE_FEDERATED_TOKEN_FILE`
settings.

Structural validation shares the compiler's projection builder. It confines
identity volumes, mounts, and environment to the main `cetirun` container. It
excludes init containers, helpers, and ephemeral containers. No injection from
the workload identity webhook is enabled.

Entra-only output creates no Kubernetes RoleBinding or ClusterRoleBinding.
Neither Kubernetes RBAC grants nor ingress mTLS are necessary for it. The
separately declared Kubernetes API audience must be different from the Entra
exchange audience. That declaration is not an API permission or a network
grant. When Kubernetes access is also supported and requested, both
capabilities must be explicitly approved on the same ServiceAccount. An unused
approval never adds a token projection or grants.

Common-executor OpenClaw, Hermes, and approved Custom images support this
projection when all of these conditions are true:

- The native registration explicitly declares `entraID: true`.
- The policy enables the projection.
- The platform contains the exact per-Agent approval.

The configuration coordination authority and the owned bootstrap must be
enabled. Retained single-start lifetimes still reject Entra. No generic-profile
fallback is used.

Set `registrations.kata.Capabilities.entraID` to `true`. Add an approval like
this one to `platforms.kata.identities`, with the actual persisted Agent UID:

```json
{
  "agentUID": "example-agent-uid",
  "namespace": "cetirun-example-agent",
  "serviceAccountName": "example-agent-entra",
  "revision": "approved-identity-1",
  "kubernetes": false,
  "entra": {
    "clientID": "00000000-0000-0000-0000-000000000001",
    "tenantID": "00000000-0000-0000-0000-000000000002"
  }
}
```

In that platform entry, also declare the distinct Kubernetes API audience and
the optional token lifetime. The current `AgentPolicy.spec.entraID.enabled`
switch requests use. Neither the capability nor an unused approval grants
access.

If necessary, create the Agent first to get its UID. If the approval is
absent, the Agent stays held. The operator creates the owned ServiceAccount.
Do not create it in advance. Do not borrow an operator/Broker identity. Cloud
federation and Azure permissions must be configured and qualified separately
for that exact namespace/account subject.

Bootstrap is the **single owner** of the dedicated ServiceAccount. The optional
receipt versions `v3`/`v4` extend the current `v1`/`v2` inventory with one
account that is kept. First, the creation intent is persisted. Then API
readback records the account UID with the UID/resource-version CAS that the
receipt already uses. No workload that carries a token can be submitted until
that pin is read back.

If a create or pin response is lost, the same protected intent is used again.
If a recorded account is absent or replaced, it is never re-created or adopted.
Older readers reject the new receipt versions. They do not ignore the cleanup
obligations of these versions.

The ServiceAccount is a source, **not a Helm resource**. Its recorded UID and
bootstrap authority are part of the compiled revision and the protected
expectation. Source digests include Azure identity annotations. Bootstrap
reconciles the desired metadata with UID/resource-version preconditions, and
only after the owned workloads are stopped. Source readback occurs before
workload publication.

[`ValidateIdentityBinding`](internal/sandboxworkload/identity_binding.go)
operates on a `Bundle` or on a `ReconcileExpectation` that exists. It compares
the authoritative namespace/ServiceAccount readback with the current compiled
approval and with an `ObjectIdentity` UID pin that exists. It rejects
replacements, foreign owners, stale approval metadata, and implicit
token/credential inheritance. It never adopts or records a replacement itself.

Apply guards examine the pinned source and digest. Observation and Pod
admission also examine the exact identity binding. An expectation cannot omit
the identity source or transfer ownership to the chart.

Suspension, retained restart, and approval refresh keep the account UID, HOME,
and Gateway key. When Entra is disabled, its projected token/environment is
removed. But the account and pin stay for the Agent lifetime. If Kubernetes
access stays enabled, bootstrap still owns that account. Helm must not adopt
it or erase its creation record. Retired Azure identity annotations are
removed only after quiescence with UID/resource-version checks.

If Kubernetes access is not requested, the kept account grants no Kubernetes
RBAC. If a new projection is enabled when the Agent is stopped, the confirmed
suspension and blocked key of the Agent stay intact. Pending identity
preparation stays visible in policy conditions. It does not create an account
or move the lifecycle back to Pending. An explicit resume starts the approved
bootstrap.

Fresh Entra lifetimes that use controller runtime defaults pin the omitted
chart, image, and port values from the trusted chart. They do this before the
owned identity bootstrap. Explicit coordinates do not change. For execution,
the recorded ServiceAccount UID and current approved sources are still
necessary.

The account name cannot change after it is recorded. Final Agent/namespace
deletion releases the account after workload shutdown, through the current
bootstrap cleanup order. Helm cannot delete or recreate it.

Offline tests cover these items:

- Compiler approvals
- Both built-in chart renderers
- Current binding readback
- Negative Kubernetes RBAC checks on an isolated API server
- Positive native Pod admission
- Interrupted-create/replacement/removal behavior

They do not exchange tokens with Entra or provision federated credentials.
They do not confirm Azure permissions, run containers, or establish cloud
token revocation. A stop of new projections cannot revoke Azure access tokens
that were already issued. See the
[focused commands](../../DEVELOPMENT.md#sandbox-entra-projection-core).

The compiler does not provision or revoke cloud identities. For RBAC revocation
helpers, authoritative UID/ownership checks and confirmed absence are
necessary. These helpers keep shared roles/accounts. Held and common
controllers use [`GrantJournalStore`](internal/sandboxpolicy/journal.go) for
these tasks:

- To persist both workload and admission-source bindings before writes
- To record the UIDs of these bindings
- To integrate retryable revocation into hold, suspension, and deletion paths

Its protected inventory is separate from the platform terminal's fixed own-Pod
and own-Agent read/exec bindings. Only exact terminal names, subjects,
ownership, and current read-role rules qualify for that separation. A terminal
label alone does not qualify. Terminal shutdown and namespace/owner cleanup
keep their current lifecycle.

The journal's protected ConfigMap survives foreground owner deletion and uses
resourceVersion CAS. It is bounded to 1,024 references and 512 KiB. Discovered
visible bindings are never treated as complete cleanup inventory.

For current common Pod admission, confirmed workload binding UIDs and exact
live binding contents are also necessary. A lost apply acknowledgement keeps
the write-ahead cleanup obligation. It does not authorize a Pod from an
unconfirmed intent. Common suspension renders zero replicas and does not
recreate revoked workload bindings. It keeps the requested identity for a
resume that is authorized later.

Each binding CREATE has its own submission checkpoint. A definitive API
rejection records a non-creation outcome against the exact expectation UID and
resource version of that submission. This allows ordinary grant cleanup and
retry. It cannot clear a newer submission or replace the workload's shutdown
evidence.

Binding CREATEs do not follow HTTP Retry-After responses. A later rejection
must not hide an uncertain first write. Timeouts, lost responses, and failed
outcome writes keep the confirmation barrier. The absence of a binding alone is
never proof of non-creation. This does not supply cloud-identity revocation or
authorize execution.

When an unchanged canonical inventory is recorded, one authoritative read is
used. No write and no confirmation read occur. Real changes keep the
write-and-confirm path. They never discard a binding UID that is already
recorded.

Each approved grant must carry the live ClusterRole UID, resource version, and
normalized rules. The approval's `Revision` is a separate administrative
revision. It is not a substitute for an API observation. Configuration
assessment calls [`VerifyPolicyGrants`](internal/sandboxpolicy/grants.go)
through the authoritative reader, only for the grants of the policy that makes
the request.

Held preparation and common reconciliation, their apply guards, and current Pod
admission also do this check immediately before they consume requested grants.
They also invalidate changed observations and coordinate revocation on drift.
The compiler is still a pure transformation. It cannot detect a later role edit.
Explicitly approved wildcard or Secret access is not silently rewritten.

For the approval check itself, only `get` access to ClusterRoles is necessary.
It adds no authority for role edits, blanket binding, or escalation.
Kubernetes still enforces one of these conditions before the operator creates
the bindings of a ClusterRole:

- The operator holds the permissions of the role.
- The operator holds a `bind` permission from an administrator for that exact
  ClusterRole.

The operator registers Sandbox and AgentPolicy configuration reconcilers.
Sandbox `Ready` describes prerequisites. AgentPolicy `Accepted` describes
configuration validity/support. Without a qualified platform snapshot source,
these conditions stay `Unknown`, not ready. Indexed configuration watches
requeue the Agents that refer to a configuration. This also applies to pending
bindings.

For per-Agent status helpers, the current binding/revision/Pod/node/epoch must
match, and the guest-external observation must be fresh. These helpers keep
the applied history on failure and do not release the hold. `Accepted` does
not establish compatibility with a particular Sandbox or a ready-to-run
workload.

Reconciliation uses the cache for its primary configuration object, but
authoritative reads for prerequisites and grants. Successful refreshes are
jittered between 50% and 62.5% of the snapshot freshness window that is left.
Failures keep the controller backoff. These helpers do not install a
production snapshot source.

#### Optional final-Pod admission

With `sandboxAdmission.podsEnabled=true`, the server registers
`/validate-sandbox-pods` and reads expectation ConfigMaps in the operator
namespace. Deploy this server setting before you enable enforcement. When
`enforce=true`, the chart also installs final-Pod match rules and native
namespace protection. Configuration admission must stay enforced so that the
UID records that Pod admission uses are authoritative.

Only the exact configured controller can initialize a protected namespace with
these items:

- `cetirun.microsoft.com/sandbox-protected: "true"`
- The `agent-name` and `agent-uid` annotations under `cetirun.microsoft.com/`
  that match the Agent
- One canonical Agent owner reference

The marker, identity, and ownership become immutable. This also applies to
that controller and to namespace status/finalize updates. The operator/state
namespace cannot receive the marker. Native validation does not depend on the
availability of the webhook.

The match rules use the presence of the marker, not mutable Pod labels. Each
request examines these items again:

- Live namespace/Agent ownership
- Durable authorization
- Exact controller-owned expectation
- Source UIDs/resource versions
- StatefulSet revision/template
- Storage
- RuntimeClass

Absent/stale state, suspension, or either execution hold denies the request.
All container, init-container, ephemeral-container, and resize paths are
examined. Pod DELETE and `/status` stay outside the webhook. Unmarked
platform/repair namespaces stay outside its match rules.

The exact Kubernetes garbage-collector identities
`system:kube-controller-manager` and
`system:serviceaccount:kube-system:generic-garbage-collector` can remove only
`foregroundDeletion` from a Pod that is already in termination. The webhook
match rules exclude this narrow update, and it is handled without
controller-state reads. As a result, absent/stale expectations, execution
holds, and webhook outages do not prevent finalization. All other Pod fields
and finalizers must not change, except server-managed `managedFields`.
This does not grant either identity a general admission exemption.

In protected namespaces, CONNECT requests for `pods/exec`, `pods/attach`, and
`pods/portforward` are denied. These denials are independent of expectations,
and they occur without an administrator or controller exemption. The protected
runtime chart does not install the legacy per-release exec policy. The
namespace-scoped admission guard replaces it. Kubernetes RBAC must also
restrict access to these subresources. This guard does not change access to
unmarked workloads.

Protected namespaces must be excluded from unrelated Pod mutators, for example
sidecar/identity injectors and policies that rewrite metadata. Validation
compares the complete specification and protected metadata after supported API
defaults. Unknown annotations and labels are not harmless by definition. For a
mutating webhook, combine its current namespace match rules with this
exclusion:

```yaml
namespaceSelector:
  matchExpressions:
    - key: cetirun.microsoft.com/sandbox-protected
      operator: DoesNotExist
```

Before you enable enforcement, examine the actual match rules, namespace/object
selectors, and match conditions of every relevant mutator. Include native
mutation policies. Test the supported templates through the API-server request
path in the target environment.

The presence of an unrelated webhook alone is not a conflict. The Istio opt-out
is not a general mutation exemption. API-server round-trip tests cover current
native templates and explicit DownwardAPI/field-selector defaults. This is not
cross-version qualification.

Denials include stable reason codes (for example `SourceChanged`,
`ExpectationMissing`, `TemplateMismatch`, or `RequestDeadlineExceeded`).
Structured logs and `cetirun_sandbox_pod_admission_denials_total{reason=...}`
use bounded reasons. Template diagnostics show at most eight field paths. They
do not show environment values, Secret contents, map keys, or raw API errors.
Correct the trusted source/expectation or the incompatible mutator. Do not
disable validation.

Source GETs are uncached and cancellation-aware. They are limited to four
concurrent reads for each request. The chart supplies a total validation
budget of 80% of `timeoutSeconds`. The final expectation revision check is
kept.

Controller state must be writable only by the trusted controller. The chart
grants admission ConfigMap reads only in that state namespace. The necessary
metadata reads of workload Secrets/ConfigMaps must have separately scoped,
name-restricted RBAC in the protected workload namespaces. The chart
intentionally does not grant blanket cluster-wide access. Absent source
permissions fail closed. No deployment is qualified by these package defaults.

The separate template/ownership contract for the platform Copilot helper is
not defined yet. There is no helper exemption that uses only a label, a
namespace, or a ServiceAccount. Protected preparation and expectation creation
stay held until the separate lifecycle/start-barrier and
enforcement-observation integration is qualified. There is no production
expectation producer in this increment.

`podsEnabled` alone only registers the endpoint. `enforce` installs match rules
for marked namespaces, not for every Pod in the cluster. Do not mark namespaces
or enable a policy-bound start path until these items are qualified together:

- The producer
- Scoped read permissions
- Source refresh/revocation
- Workload lifecycle

A chart version, a readiness probe, or a manually authored expectation is not
that evidence.

#### Dedicated-node placement guard (separate opt-in)

`sandboxAdmission.placementConfigMapName` selects a `placement.json` ConfigMap
that already exists in the controller state namespace. Empty is the default.
Configuration and final-Pod admission must be enabled for this setting. The
placement webhook is installed only when `sandboxAdmission.enforce` is true.

This read-only guard examines placement across **all namespaces**. For example,
it examines explicit `Pod.spec.nodeName`, scheduler `pods/binding`, Pod
updates, and identity changes and deletion of reserved Nodes. Labels and taints
do not establish ownership. The deprecated core `bindings` endpoint is matched
and denied. It is not allowed to bypass the guard. Binding annotation overrides
are rejected, because the API server can apply them to the Pod without a
separate Pod UPDATE request.

An explicit, complete Node inventory pins Node names, UIDs, and provider IDs.
Unknown nodes and same-name replacement nodes fail closed. They do not become
ordinary capacity. Known unreserved nodes keep their ordinary placement path.

The registry uses this shape. Every identity in the example is synthetic:

```json
{
  "version": "v1",
  "nodes": [
    {
      "name": "ordinary-worker",
      "uid": "00000000-0000-4000-8000-000000000001",
      "providerID": "example://ordinary-worker"
    },
    {
      "name": "reserved-worker",
      "uid": "00000000-0000-4000-8000-000000000002",
      "providerID": "example://reserved-worker",
      "reservation": {
        "agent": {
          "name": "example-agent",
          "uid": "00000000-0000-4000-8000-000000000003"
        },
        "namespace": {
          "name": "example-workload",
          "uid": "00000000-0000-4000-8000-000000000004"
        },
        "statefulSet": {
          "name": "example-agent",
          "uid": "00000000-0000-4000-8000-000000000005"
        }
      }
    }
  ],
  "platformDaemonSets": []
}
```

Store this JSON as `data["placement.json"]` in the selected ConfigMap, with no
binary data. Registrations must come from persisted API objects, not from
guessed names or labels. The inventory is bounded to 256 KiB, 256 Nodes, and 64
platform DaemonSets. Each DaemonSet can enroll at most 256 persisted Pod
identities. Unknown fields, duplicate keys, nulls, and inconsistent identities
are rejected. An empty platform inventory gives no platform-workload
exemptions.

A reserved node is assigned to an exact Agent, namespace, and StatefulSet
identity. Agent placement must also match the current protected workload
expectation. This does not relax final-Pod validation, release an execution
hold, or issue a start grant. Ordinary unrelated workloads cannot get reserved
placement if they set a node name, select a node, add tolerations, or forge a
namespace label.

When placement is configured, both binding and protected-Pod validation accept
the region/zone labels that Kubernetes adds. They accept these labels only
with the freshly confirmed values of the registered Node. Explicit template
labels are never overwritten. Other additions are still not allowed. Without
this opt-in, the current exact-template comparison is unchanged.

Platform workloads are not exempt because of their namespace or
ServiceAccount. Only explicitly registered DaemonSets with pinned identities
and templates can use the platform path. Each `platformDaemonSets` entry
contains `name`, `uid`, `namespace: {name, uid}`, `templateDigest`,
`trustedCreator`, and a `bootstrapPods` array of `{name, uid}`.
[`CanonicalDaemonSetDigest`](internal/sandboxplacement/daemonset.go) computes
the digest from the current API-defaulted selector and Pod template. It does
not use Pod labels that the caller supplies or the status of the DaemonSet.

Registered controller identities are examined at creation. A reserved-node
binding/update also must have the exact persisted Pod name and UID in the
protected registration. The trusted controller of a replacement can create it.
But the replacement still cannot bind to reserved capacity until its new
UID is enrolled. This is an intentional fail-closed pilot limitation, not
automatic platform recovery.

Explicit platform `priorityClassName` selections are resolved from the current
PriorityClass with read-only access. Its numeric priority and preemption policy
are examined, not copied from the Pod in the request. For long DaemonSet names,
generated Pod names use the bounded prefix/suffix rules of Kubernetes. The full
registered controller name, UID, and generation prefix still identify the
owner.

Pod-only resource request defaults come from the registered limits of each
regular or init container. This also applies to restartable init containers.
Explicit requests do not change. The Pod in the request and the stored
DaemonSet template are not rewritten. Unrelated resource requests that
admission adds are still rejected.

The platform owner must protect registry writes. The owner must also establish
the registry on an empty, isolated dedicated pool before the owner grants
workload access. The guard does not discover or adopt nodes, mutate scheduling
inputs, install DaemonSets, write reservations, or prove historical pool
isolation. Registry changes must have their own authorized, stopped lifecycle.
The removal of a record is not permission to reuse a pool or skip old-writer
fencing.

First, bootstrap TLS and the replicas that serve requests on unreserved nodes.
Then publish the registry. Then run the direct/binding request-path tests.
Enable enforcement only after these steps.

This opt-in expands the failure domain. A registry failure or a failure to
serve requests blocks matched requests cluster-wide. Pod status and deletion
stay independent. The current narrow completion of garbage-collector
finalizers also stays independent.

When the replicas can serve requests, ordinary Pods in termination can remove
only finalizers. They cannot change placement or other object fields.
Unrelated changes are not cleanup exemptions. ConfigMap updates and
webhook-registration repair stay available.

A new Pod that serves requests is still a matched CREATE. Plan recovery with
the replicas that already serve requests or with an authorized, stopped
maintenance procedure. Do not plan for an admission bypass.

This is one placement boundary that is necessary for a future dedicated-pool
Agent pilot. It is not a `CheckQuiesced` provider, an AKS deletion
authorization, a complete start/change barrier, or proof of pre-start and
continuous runtime enforcement. Those gates and the actual end-to-end
demonstration of a policy-bound Agent are separate items that are still
necessary.

#### Durable placement binding during held preparation

`sandboxSources.placementConfigMapName` is a separate controller opt-in, and
it is empty by default. For this opt-in, sources must be enabled, and it must
select the same registry as `sandboxAdmission.placementConfigMapName`. It adds
only Node `get` permission to the controller. It does not write the placement
registry.

After preparation validates the owned zero-replica StatefulSet, it reads the
registered reservation of the StatefulSet and the current Node identities.
Before preparation publishes the held Pod expectation, it records these exact
items in the preparation receipt that already exists:

- Agent
- Namespace
- StatefulSet
- HOME
- Registry UID
- Node UID/provider-ID set

The expectation pins both that receipt and the current UID/version of the
registry. The platform must enroll the actual staged workload UID. If the
reservation is absent, the workload stays held and preparation stays
incomplete. The workload is not implicitly assigned.

The recorded identity is immutable for this namespace/HOME lifetime. A changed
Node set, a replaced registry or workload, or a disabled reader cannot silently
reassign it. Node status and unrelated metadata updates do not append history
or rewrite the binding. Cleanup passes the recorded tuple to the separately
qualified writer-exclusion provider. It does this also if the current registry,
Node, or configuration is unavailable. It does not discover old writers again
by name.

Opted-in receipts use the internal `v2` preparation format. This format must
have the placement payload. Unselected preparation that already exists keeps
`v1`. The loss of a `v2` payload is an error, not an unassigned fallback.
Upgrade readers before you select this option. Do not downgrade binaries while
cleanup of these receipts is necessary.

This is durable identity infrastructure, not proof of lifetime exclusivity or
Compute VM destruction. No automatic enrollment, qualified fence adapter, cloud
deletion, execution permit, or hold release is added. Those necessary items
and a real policy-bound Agent run are not complete.

#### Schema and binary upgrades

Helm upgrades all four CRDs as ordinary templates. For compatible schema
changes, an external CRD apply step or upgrade Job is not necessary. The CRD
retention annotation prevents deletion. It does not make schema changes or
rollback reversible.

The default value of `sandboxAdmission.validationMode` is `compatible`. When CR
objects are decoded, fields that a newer schema introduces are tolerated. As a
result, an added status field does not block ordinary writes through an older
admission replica. Known field types, authorization, binding checks, deletion
protections, and dependency failures still apply. Server configuration, Pod
admission, and namespace protection stay strict. `failurePolicy` is still
`Fail`.

Metadata-only cleanup compares the raw objects. This comparison includes
unknown fields. Non-administrators cannot use legacy Agent/Team authority to
add, change, or remove unknown top-level spec fields. They can continue
ordinary updates when those unknown fields are unchanged.

Administrator authority is not proof that an older binary implements a new
policy. Additions in the same served API must keep the old authorization and
execution semantics. New security semantics must have a qualified
version/capability transition. An unknown field alone, or a default that
silently activates them, is not enough.

The optional `strict` mode rejects unknown CR object fields. In that mode,
schema additions can cause rejected requests until all binaries that serve
requests understand them. This also applies to additions that come from
API-server defaults. Use this mode only with an explicitly ordered
schema/binary upgrade procedure.

Operator finalizer changes, spec backfills, and status updates use merge
patches with resource-version checks. They do not replace the whole object or
status. Writes that conflict must be retried from a fresh object. If other
clients write objects whose schema can outlive their compiled types, these
clients must follow the same rule.

The template-managed installation is the new baseline. It does not adopt
unmanaged CRDs from an earlier installation or CRDs that another release owns.
Installations that already exist must have a separately reviewed ownership
handover. Never delete and recreate CRDs to bypass ownership checks.
Incompatible schema changes and downgrades must have a reviewed migration. Do
not expect that Helm rollback or automatic rollback can safely restore an older
schema and binary.

API-server tests cover these items:

- A newer status schema with strict and compatible handlers
- Continuous writes during Helm upgrade
- Retention on uninstall
- Status patches that keep unknown fields

They do not qualify arbitrary new policy semantics or prove a production
rolling-upgrade availability guarantee.

#### Availability and bootstrap

Use an explicitly authorized environment and platform certificate issuer.
Admission is now gated to Kubernetes 1.37+, which matches the tested
minor version. Native match-condition support alone does not qualify older
versions. The fixed cluster-scoped resource names support only one
operator/admission installation for each cluster.

1. Provision a `kubernetes.io/tls` Secret that contains `tls.crt` and
   `tls.key` for `cetirun-sandbox-admission.<operator-namespace>.svc`. Supply
   its name through `sandboxAdmission.tlsSecretName`. This chart does not
   install an issuer and does not generate a serving certificate.
2. Configure exact `sandboxAdmission.adminUsers`/`adminGroups`. Then deploy
   `enabled=true, enforce=false`. Blanket groups, for example
   `system:authenticated` or `system:serviceaccounts:<namespace>`, are refused.

   The Portal's service account must also be an administrator. The Portal
   writes Agents, Teams, Sandboxes, and AgentPolicies with this account. If
   the account is not an administrator, the webhook refuses its writes to
   policy-bound Agents. For example, the webhook refuses the write that saves
   the GitHub Copilot models of an owner after a sign-in.

   `sandboxAdmission.portalServiceAccount` adds
   `system:serviceaccount:<release namespace>:<name>` to the administrators one
   time. This entry does not count as the one necessary administrator. The
   platform chart sets it to the service account of the Portal, which is
   `cetirun-portal` by default. If a Portal is in another namespace, list it in
   `adminUsers`.

   Examine TLS, readiness, API read permissions, two available replicas, and
   their placement. The chart supplies a disruption budget and spread
   constraints. A chart render does not establish availability. Before the
   next step, use request-path checks and a rollout that waits. An initial
   install with both switches enabled is not a supported bootstrap.
3. Supply the base64 PEM CA bundle of the issuer as
   `sandboxAdmission.caBundle`. After you make sure that the request path
   operates correctly, enable `enforce=true`. The configurations use
   `failurePolicy: Fail` and `sandboxAdmission.timeoutSeconds` (default 10,
   integer range 1-30). Measure request latency at the intended
   configuration/Agent count. A larger deadline limits the wait time, but it
   does not establish a capacity guarantee. Neither flag enables runtime
   execution.
4. During CA rotation, first trust both the old and the new CAs. Then rotate
   the mounted pair. Make sure that every replica serves the new certificate.
   Then retire the old CA.

   Each new TLS handshake reads the current pair. It uses the parsed
   certificate again only when its contents match. Validity is examined every
   time. Invalid, absent, or expired material makes the handshake and
   readiness fail. Neither a filesystem watcher nor unchanged modification
   times can authorize stale cached material. TLS connections that already
   exist can outlive the replacement.

Scheduling uses independent `sandboxAdmission.nodeSelector`, `affinity`, and
`tolerations`, not the placement of the controller. An optional
`priorityClassName` must name a platform-approved PriorityClass that exists.
The chart does not automatically assign system-critical priority.

Spread constraints do not guarantee distinct hosts when only one eligible
topology domain exists. As a result, for availability, make sure that the
actual placement is on at least two eligible hosts. Spread is scoped to each
`pod-template-hash`. Old ReplicaSets must not make a replacement revision
appear balanced before their Pods disappear. The deployment preflight still
examines the actual current endpoints that serve requests.

The disruption budget allows voluntary disruptions only while at least
`replicaCount - 1` replicas stay available. A native 10-second preStop sleep
lets endpoint changes propagate before shutdown. The process has a 35-second
manager shutdown budget in a 50-second Pod termination grace period. This
covers the maximum webhook deadline. These settings decrease rollout races.
Live endpoint-drain and replica-loss tests are still necessary.

The rules do not intercept their own Deployment, Service, RBAC, or certificate
Secret. They exclude configuration `/status` and Agent DELETE. Optional
protected Pod rules are installed only with `podsEnabled=true` and
`enforce=true`. They always exclude Pod deletion and `/status`. For status
writes, the applicable Kubernetes RBAC is still necessary.

An API-server-side match condition also excludes **only** the removal of known
lifecycle finalizers by the exact controller. This exclusion applies only to an
Agent that is already in deletion, with unchanged spec, status, and protected
metadata. It keeps foreign finalizers. It cannot exempt additions,
annotations, ownership changes, or spec updates.

As a result, authorized status, deletion, and narrow cleanup are still possible
while the webhook is unavailable. Shared-configuration deletion and other
matched writes fail closed. Repair the service or certificate. Do not weaken
the rules or manually remove finalizers. Any exceptional rollback must have a
separately reviewed recovery procedure while starts stay held.

#### Monitoring and platform boundaries

The controller-runtime server exports
`controller_runtime_webhook_latency_seconds`,
`controller_runtime_webhook_requests_total`, and
`controller_runtime_webhook_requests_in_flight`. Admission denials usually
come in HTTP 200 responses. As a result, use
`cetirun_sandbox_admission_decisions_total` for bounded `allowed`, `denied`,
`invalid_request`, and `dependency_error` outcomes by handler.

`cetirun_sandbox_admission_certificate_not_after_seconds` reports the Unix
expiry time of the current serving certificate (zero for
unreadable/unparseable material). On admission pods, alert on these
conditions:

- Zero
- An expiry time that is near
- Dependency errors
- Sustained request latency that is near the configured deadline

No object names or arbitrary error strings become metric labels.

`cetirun_sandbox_admission_request_duration_seconds` is a histogram of the
time in each completed admission handler. This time includes the dependency
reads of the handler. Its `_count` supplies completed-call counts for each
operation. The current
`cetirun_sandbox_admission_decisions_total{handler,outcome}` schema is
unchanged.

| Label | Values |
| --- | --- |
| `handler` | `validate`, `default`, `pod`, `placement`, or defensive `unknown` |
| `operation` | `CREATE`, `UPDATE`, `DELETE`, `CONNECT`, or `unknown` |
| `outcome` | `allowed`, `denied`, `invalid_request`, `dependency_error` |

Other operation strings, for example empty strings, collapse to `unknown` in
metrics. This does not change the request that goes to admission. Request
identities, resource names, user information, patches, warnings, and error
messages are not labels or exemplars. Finite duration buckets are 0.005, 0.01,
0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10 and 30 seconds. The implicit
`+Inf` bucket follows them. Only observed label combinations are exported.

This histogram does not include TLS, the request decode step before dispatch,
response serialization, or API-server transport time. A handler that panics or
never returns has no completed-call sample. The behavior for panics and
cancellation is unchanged. For transport failures, in-flight calls, and full
request latency, use the controller-runtime metrics and API-server
observations with this histogram.

A denial that is sent with HTTP 200 is still `outcome="denied"`, not an
allowed call. The metric adds no API reads, scrape dependency, request
deadline, listener, or readiness gate. The current metrics-disable setting
stays in effect.

For example, configure an authorized admission-process scraper separately.
After that, examine the p95 handler time and request rate for each target,
handler, and operation. Replace these synthetic scrape labels with your labels.
When you select a target, keep any cluster/fleet labels:

```promql
histogram_quantile(0.95,
  sum without (outcome) (
    rate(cetirun_sandbox_admission_request_duration_seconds_bucket{
      job="sandbox-admission-example", instance="admission.example.invalid:8080"
    }[5m])
  )
)

sum without (outcome) (
  rate(cetirun_sandbox_admission_request_duration_seconds_count{
    job="sandbox-admission-example", instance="admission.example.invalid:8080"
  }[5m])
)
```

These queries combine outcomes, not replicas or fleets. Examine the rate and
scrape health together with the quantile. If there are no completed calls or
the series are absent, that does not establish zero latency or a healthy
service. No latency target is qualified by these examples.

The serving certificate metric does **not** confirm the CA bundle that is
configured in the API server, its expiry, or hostname/trust alignment. Before
enforcement is enabled, and during every rotation, the platform must examine
those items separately. The platform must also test an actual API-server
request through every replica that serves requests. CA injection is not
installed or expected. The issuer and trusted CA value stay
platform-managed.

The TLS listener authenticates the server, not the API-server client. A direct
request cannot apply an object, but it can cause validation and use API reads.
Metrics are also served on a plaintext, unauthenticated Pod port.

Before you enable production enforcement, qualify one of these items:

- Cluster-specific network restrictions for API-server access, node health
  probes, and authorized metrics scrapers
- An explicitly configured API-server client-certificate arrangement

No generic NetworkPolicy is installed here. If you guess control-plane source
addresses, you can block the fail-closed webhook itself. Keep these production
gates separate from local loopback and chart tests.

### Sandbox reported-status metrics

The operator also exports these gauges from its current `/metrics` endpoint,
which is bound to `:8080` by default. These are operator metrics, not
admission-process metrics. No new listener, Service, scrape configuration,
RBAC grant, or policy-enforcement behavior is installed. If you set the
`--metrics-bind-address=0` flag that already exists, the operator also skips
this collector and its metrics-only informer initialization.

| Metric | Meaning |
| --- | --- |
| `cetirun_sandbox_agents{runtime,status,reason}` | Counts explicit or Team policy-bound Sandbox intent and retained-held/history Agents by their reported status. Ordinary policy-free Agents are excluded. The obsolete `class` label is removed. |
| `cetirun_sandbox_metrics_collection_success` | `1` when the complete cached snapshot was collected. `0` when collection is unavailable or fails. Failed collection emits no partial fleet counts. |

Startup initializes the shared Agent, Team, and Sandbox informers on every
operator replica, regardless of leadership. Scrapes do not initialize
informers. Incomplete startup reports collection health `0` with no fleet
counts. Operational initialization failures are logged and retried. This does
not stop the operator. The manager cache keeps its normal background
list/watch and reconnect traffic.

Each scrape calculates the counts again from that cache, with a shared
five-second read context. It does not keep per-Agent metric series or query a
live enforcement provider. The cache is eventually consistent. Lists of
different resource kinds are not an atomic snapshot. Later scrapes show cache
updates and deletions. These reads do not add API calls to reconciliation or
status writes.

An empty or legacy-only fleet has successful collection and no fleet series.
This is different from failed collection or an unavailable scrape target.

Labels are bounded:

- `runtime` is `kata`, `gvisor`, `container`, or `unknown`.
- `status` is `True`, `False`, or `Unknown`.
- `reason` uses an allowlist and bounded fallback categories.

Names, UIDs, namespaces, revisions, messages, provider errors, and arbitrary
values never become metric labels. For runtime classification, the current
cached Sandbox must not be in termination and must match the admission-owned
UID binding. A same-name replacement is not adopted for attribution.

Absent, stale, or malformed conditions report `Unknown`. They do not repeat an
old success. In other cases, the valid current-generation condition is
reported. The collector does not independently confirm it again.

Intentional suspension and deletion take precedence over old conditions. They
stay distinguishable from failures. Applied-binding history alone is not
current readiness. Cached Team policy fields count only as intent. They never
select a Sandbox or establish an authorization binding.

**These are cached status reports, not fresh enforcement evidence.** A declared
runtime label does not qualify its adapter. `SandboxPrepared` still means
zero-replica held preparation. Scrapes do not examine running Pod/node/epoch
observations, and they do not authorize execution. Condition transition
timestamps are not observation-freshness measurements. Collection failure is
not evidence that there are zero affected Agents.

#### Inspection queries and response guidance

Configure an authorized scraper separately. The current metrics port is
plaintext and unauthenticated. Keep the deployment's network/access controls.

Before aggregation, select one operator target for one cluster/fleet. Multiple
operator replicas report the same cluster-wide population. Do not add their
snapshots together. The target labels that follow are synthetic examples.
Replace them with the actual labels of the scraper.

```promql
cetirun_sandbox_metrics_collection_success{
  job="example-cetirun-operator", instance="operator.example.com:8080"
}
```

Interpret an empty count as zero only after you confirm both collection
success and the selected target's `up` signal:

```promql
sum(cetirun_sandbox_agents{
  job="example-cetirun-operator", instance="operator.example.com:8080"
}) or vector(0)
```

Examine non-ready reports, but not reports of explicit suspension/deletion:

```promql
sum by (runtime, class, reason) (cetirun_sandbox_agents{
  job="example-cetirun-operator", instance="operator.example.com:8080",
  status!="True", reason!~"Suspended|Deleting|InstanceDeleting"
})
```

This includes expected preparation holds. It is not an automatic failure
alert. Select alert thresholds and durations only after you qualify the
expected lifecycle and collection behavior of the deployment.

| Observation | Operator response |
| --- | --- |
| Collection failure, absent gauge, or target down | Examine target selection/version, operator health, cache synchronization, and API connectivity/authorization. Treat fleet counts as unavailable, not healthy or empty. |
| Absent references or unauthorized bindings | Examine the current status and trusted operator logs. Restore valid references or get explicit administrator reapproval of the current identities. Never adopt same-name replacements or edit journal UIDs to bypass checks. |
| Unknown evidence or unavailable runtime | Examine the trusted platform inventory and qualified providers. Restore the necessary evidence. Do not reinterpret Pod absence, scale-zero, or a PVC access mode as writer fencing. |
| Apply/conflict failures or reported enforcement loss | Examine the affected control through trusted status/logs and the qualified containment procedure. Metrics and alerts do not stop execution, revoke grants, or establish containment. |

Keep holds and recovery finalizers intact during the investigation. This
increment does not add observation-age, rollout, or fencing-duration metrics,
durable failure counters, alert rules, dashboards, or production
qualification.

### Enforcement feasibility baseline

Source inspection identifies Azure CNI Overlay powered by Cilium and Kata node
pools in the [cluster template](../../infra/cluster/modules/aks-cluster.bicep).
The [current chart network template](../cetirun-chart/templates/networkpolicy.yaml)
uses standard NetworkPolicy, namespace-wide DNS, and an IPv4 egress rule. It is
not a qualified implementation of the new contract.

Public documentation that was reviewed on 2026-09-25 narrows the next trial:

- [Cilium deny policies](https://docs.cilium.io/en/stable/security/policy/deny/)
  have precedence over allow policies, but they do not support `toFQDNs`
  denial. As a result, native deny rules alone cannot implement full hostname
  blocking.
- [Cilium endpoint lifecycle](https://docs.cilium.io/en/stable/security/policy/lifecycle/)
  exposes endpoint regeneration state. This is a candidate observation input.
  It is not proof of all necessary controls or of protection during
  initialization/failure.
- [Managed Azure CNI capabilities](https://learn.microsoft.com/en-us/azure/aks/azure-cni-powered-by-cilium)
  have version/feature restrictions and node/pod `ipBlock` limitations.
  Upstream Cilium support must not be expected to exist in a managed
  deployment.
- [gVisor compatibility](https://gvisor.dev/docs/user_guide/compatibility/)
  distinguishes host-enforced sandbox resource bounds from unenforced
  in-sandbox competing-process limits. Container declarations alone do not
  request independent hard quotas.

The initial candidate is the configured managed Cilium path. A separately
qualified upstream Cilium installation serves as a comparison only where
authorized. Full hostname blocking, qualified-adapter gateway-to-workload mTLS,
and any unqualified protocols are still not supported. Native common-executor
mTLS and observed-SNI capture are separate opt-ins. They are not qualification
of this baseline.

Network isolation, port blocking, runtime accounting, and observation/fencing
are necessary, and they still must have actual target-version tests. No
cluster was examined or changed to establish this baseline.

#### Exact-hostname deny projection experiment

[`TestHostnameBlockDNSProjectionFeasibility`](internal/sandboxpolicy/hostname_feasibility_test.go)
tests one proposed backend reduction. The reduction resolves a blocked hostname
and denies its IPv4/IPv6 host CIDRs. Go's real resolver uses synthetic A, AAAA,
and CNAME responses over in-memory connections. Then separate CIDR-only inputs
go through the current compiler. Assertions use the serialized Cilium policy
and the current synthetic L3/L4 evaluator, not a running Cilium dataplane.

The result is **NO-GO for exact-hostname blocking through DNS-to-CIDR denies**:

- The exact blocked name is denied. But an unlisted name, a CNAME alias, and a
  child name with the same addresses are also denied. An unrelated address
  is still allowed. The [API](api/v1alpha1/agentpolicy_types.go) uses exact
  names, not address equivalence or suffix rules. Wildcard policy input still
  rejects.
- For literal IPv4/IPv6 access, no DNS query is necessary. At this layer, this
  access cannot be distinguished from named access. This observation does not
  assign a new API permission to direct-IP access. It also does not establish
  a backend that filters DNS queries.
- After an answer changes, a stale projection allows the new addresses. When
  the projection is replaced, the old addresses are released. When both are
  kept, both are denied. This also applies to unlisted names on shared
  addresses. Recompilation changes the control revision, but these tests do
  not cover TTL scheduling, policy propagation, connections that already
  exist, or failure recovery.

This is not Cilium [`toFQDNs`](https://docs.cilium.io/en/stable/security/policy/layer3/#dns-based).
Those rules generate additive IP **allows**, not hostname denies. They do not
remove the compiler's public-egress allowance.

This is also not the shared Istio sidecar SNI blacklist. That blacklist was
retired, and the scoped AgentPolicy controls are used instead. That filter was
not a guest-external control that covers arbitrary traffic. This experiment
does not enable the separately registered observed-SNI path below. It also
does not establish full hostname enforcement. A successful DNS-projection
experiment is not M3 completion.

Run this bounded offline check with the documented pinned Go toolchain:

```bash
go -C app/cetirun-operator test ./internal/sandboxpolicy -run '^TestHostnameBlockDNSProjectionFeasibility$' -count=1
go -C app/cetirun-operator test -race ./internal/sandboxpolicy -run '^TestHostnameBlockDNSProjectionFeasibility$' -count=1
```

The smallest separately authorized packet trial is a disposable protected
client, controlled DNS, and a dual-stack test endpoint. The endpoint serves a
blocked and an unlisted name on the same addresses. On the explicitly selected
Cilium version, compare the baseline with CIDR denies. For this comparison, use
new named and direct-IP TCP/UDP connections. Then change one DNS answer and
observe the update.

Record both denied and allowed controls from outside the guest. This can
examine the predicted counterexample. It cannot qualify hostname support. A
different hostname-aware backend still must have a separately agreed design
and qualification.

#### Managed observed-SNI egress

The common executor connects `AgentPolicy.spec.network.blockedFQDNs` through
the compiler, controlled Envoy, chart, final-Pod admission, and lifecycle. It
must have `registrations.<runtime>.Capabilities.blockedFQDNs: true` and an
explicit operator-owned registration in `platform.json`:

```json
{
  "egressSNI": {
    "envoyImage": "registry.example.com/envoy@sha256:<approved-envoy-digest>",
    "initImage": "registry.example.com/cetirun-base@sha256:<approved-helper-digest>"
  }
}
```

This is a fragment of `platforms.<runtime>`, not a complete platform
configuration. Replace the placeholders with approved artifact digests. The
tested Envoy version is 1.36.3.

The trusted init image must supply `sh`, `curl`, `iptables`, and `ip6tables`.
The current [CetiRun base-image source](../cetirun-chart/Dockerfile.base)
already supplies them. At Pod startup, host binaries are not copied and
packages are not installed. The init image is selected by the platform,
**never** from an Agent image override. When mTLS is also requested, both
registrations must approve the same Envoy image.

An empty list returns no resources or commands. In other cases, a dedicated
proxy UID 1337 is exempt from Pod-local TCP capture to port 15001. Before
ordinary initialization, the same controlled Envoy starts as a restartable init
sidecar, and then the exact one-shot `egress-capture-init` starts. Only that
initializer is root with NET_ADMIN. The Agent and ordinary helpers stay
nonroot/cap-drop. They cannot use the proxy UID or private mounts.

Capture commands never flush other rules. Init retries validate the reserved
chain, complete only its canonical prefix, and deduplicate only the exact owned
OUTPUT hook. Foreign contents and netfilter errors fail closed. They are not
ignored. Namespace/cluster admission must allow this narrow initializer. No
privileged container, host namespace, or host mount is requested.

FQDN-only uses no ingress certificates. With both controls, there is one Envoy.
Ingress keeps the current mTLS ports, certificate source approvals, and private
Copilot routing.

Envoy admin handlers that make changes are available only on a proxy-private
Unix socket. Loopback port 15440 exposes GET `/ready`, `/certs`, and `/stats`.
It does not expose shutdown, logging changes, or config dumps. For native
readiness, the managed proxy is necessary. When mTLS is enabled, current
certificates are also necessary.

The implementation uses `original_dst`, stock `tls_inspector`, an exact
`requested_server_name` DENY match, and an ORIGINAL_DST TCP proxy. Names are
validated as lowercase ASCII hostnames. Before the exact comparison, Envoy
changes the observed server name to lowercase. As a result, wire-case variants
of a listed hostname are also denied. This is not a suffix match or DNS
resolution.

UDP is not affected. A separate terminal filter chain rejects the loopback
capture destination **before** an upstream connection is created. This also
applies to silent direct application sockets. The same port at a remote
destination is not excluded.

The inspection window is explicitly **1 second**. Continuation occurs after
this window. As a result, server-first TCP has that wait. A delayed/incomplete
ClientHello can continue without SNI inspection. No-SNI traffic or traffic
that does not match keeps its L3/L4 treatment. This is observed-SNI filtering,
**not full hostname enforcement**.

The exported `IPv4TimeoutCounter` and `IPv6TimeoutCounter` identify
uninspected listener timeouts. The SNI/TLS counters of the TLS inspector give
separate observations, but they can be process-aggregate in a shared Envoy.
RBAC `allowed` means only “not denied,” never “hostname inspected and allowed.”
These counters must stay observable through the read-only stats endpoint.

Changes use the current source UID/version, desired-control, final-apply, and
configuration-coordination guards. Capture contents and images change the
execution revision and Pod checksum. Removal replaces the Pod's network
namespace through the normal stop/change/reconcile flow. It does not rewrite
live rules or declare success from a ConfigMap alone. When FQDN capture is
removed, independent mTLS stays. Restart, suspend/resume, and ordinary
deletion keep their current storage, key, and grant-journal contracts.

Offline controller/API tests and isolated transport checks do not prove
equivalence with old deployed proxies. They also do not qualify live Cilium,
Service translation, Kata, kubelet, or CSI behavior. The backend-neutral
capture contract does not enable unmerged ordinary-container or
Kubernetes-access features.

```bash
go -C app/cetirun-operator test ./internal/egresscapture -count=1
go -C app/cetirun-operator vet ./internal/egresscapture
go -C app/cetirun-operator test ./internal/sandboxpolicy ./internal/controller -run 'TestEgressSNI|TestReconciledSandboxObservedSNI' -count=1
KUBEBUILDER_ASSETS=/path/to/local/envtest go -C app/cetirun-operator test -tags=envtest ./internal/controller -run '^TestReconciledSandboxObservedSNIAPIServer$' -count=1
```

### Offline checks

When the documented pinned tools and dependencies are already installed, use
these commands:

```bash
GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local \
  make -C app/cetirun-operator generate manifests
make -C app/cetirun-operator check-crds
python3 -B scripts/ci/validate.py go-test --component operator
python3 -B scripts/ci/validate.py go-lint --component operator
bash scripts/cetirun-platform-helm-test.sh
```

Run these commands from the repository root. Unit/schema/render checks are not
API-server admission tests or live CNI/CSI/runtime qualification.

`make -C app/cetirun-operator test-envtest` starts isolated Kubernetes
API-server and etcd processes, with RBAC-enabled admission fixtures and the
rendered webhook rules. It covers schema/default persistence, compatible and
strict decode behavior, authority, reference deletion, outage/recovery, and the
native cleanup predicate. A separate loopback serving test runs the actual
admission process and a certificate rotation with overlap.

For these checks, pinned envtest assets and Helm 3 or 4 are necessary. Refer
to [development setup](../../DEVELOPMENT.md#generated-files-and-dependencies).
These checks never use a cluster that already exists or run an Agent. They
also never qualify a production issuer, replica-loss recovery, runtime, CNI, or
CSI.

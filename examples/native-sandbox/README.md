# Native Sandbox first run

This example tests one OpenClaw Agent with a Kata Sandbox and a shared AI
Gateway model. New Agents use the explicitly enabled common executor, not a
production failover or complete AgentPolicy conformance test.

For the scoped Kubernetes API checks that can be run on an authorized AKS
target, read [AKS workflow](aks/README.md). The local `platform-probe.py`
documented below has a Kind/Docker/QEMU and hostpath-CSI contract; it is not an
AKS qualification tool.

## Ordinary-container backend

For a deliberately non-VM backend, [container.yaml](container.yaml) contains an
explicit `container` Sandbox and an illustrative runc RuntimeClass. The Agent
and its helpers in the same Pod share the node kernel. Kata is still the
default when the runtime type is omitted or null. The current gVisor scope does
not change.

This is configuration, **not** runtime installation or qualification. Before
you use the example on a separately authorized target, do these steps:

1. Make sure that the target CRI has the named `runc` handler and that the
   selected nodes support it. Replace the example node selector with confirmed
   scheduling constraints for eligible nodes. Do not rename a Kata or gVisor
   class. Do not rely on whichever node runtime is the default.
2. Register `container` in both `registrations` and `platforms` in the current
   trusted `platform.json`. Set `Capabilities.profile` to `native-single-start`
   and `Capabilities.runtimeClassName` to the actual ordinary-container class.
   Pin that class's current UID and handler in `runtimeClasses`. An absent
   class, a handler/UID mismatch, an absent registration, or `Available: false`
   prevents execution. Set `Available: true` only after actual runtime
   validation.
3. Declare `hostResourceLimits` and `perContainerResourceLimits` only after you
   make sure that per-container CPU, memory, and ephemeral-storage behavior is
   normal. Both are necessary for Container.
   `requirePerContainerResourceLimits` can also be true. Do not copy Kata VM
   overhead. Only the actual overhead that the approved RuntimeClass declares
   is part of the Kubernetes effective Pod accounting. The example
   intentionally has no overhead.
4. Under `platforms.container`, supply the same complete, revisioned
   Cilium/DNS/Gateway/ingress topology and confirmed network capabilities that
   the common executor uses. Register the actual CSI StorageClass
   UID/provisioner. Register only the tested `homeAccessModes`
   (`ReadWriteOncePod`, `ReadWriteOnce`, or both). RWO alone is not a
   single-writer guarantee.
5. Use the matching CRDs, Portal, Operator/admission, and runtime chart. Enable
   the common executor/admission settings below. Then create a Team that binds
   the new Sandbox to its AgentPolicy. Create a new OpenClaw or Hermes Agent
   with that selection. This does not add online runtime conversion for an
   Agent or HOME that already exists.

The common pipeline still owns authorization, final-Pod checks, HOME retention,
restart/suspend/resume, Gateway key/budget/spend, and Cilium policies. Opt-in
healthy-source CSI migration uses the same acknowledged shutdown/detach path,
not physical failover fencing. Separate platform auxiliary Deployments keep
their current role-specific contracts.

Container supports the same opt-in
[managed ingress mTLS](../../site/content/en/docs/guides/agent-ingress-mtls.md)
as Kata. To use it, register `Capabilities.mutualTLS: true` and the approved
endpoint, Gateway, and per-Agent certificate identities under
`platforms.container.ingressMTLS`. Approvals that are absent or changed fail
closed. Observed-SNI blocking, Entra projection, and Kubernetes grants must have
their separate common-adapter registrations and approvals under the `container`
key. mTLS does not grant them. Keep `enforcementObservation` and
`restrictiveFailure` false.

The [offline checks](../../DEVELOPMENT.md) use synthetic platform observations.
Live Kata and ordinary-container acceptance, CSI file retention, actual Gateway
spend, and Cilium traffic tests stay a separate target-specific qualification.
The `local/` Kata qualification tools below are not a container runtime
installer.

## Prepare one authorized target

Start with the [development checks](../../DEVELOPMENT.md). Installation,
image/chart publication, and live requests must have authorization for the
actual target. These examples do not provision a cluster or grant credentials.

Use matching CRDs, Operator/admission binaries, runtime chart, and Gateway chart
from the same integration build. The platform must already have:

- The selected Kata runtime, compatible CSI storage for the explicitly
  registered HOME access mode, and baseline Cilium
  networking. A RuntimeClass object alone is not runtime qualification.
- A Sandbox platform configuration that an administrator keeps accurate.
  Do not invent qualification flags, object UIDs, or topology to pass
  admission.
- Enforced Sandbox configuration and Pod admission with TLS that works.
- A configured AI Gateway model, its real provider credentials in the approved
  secret mechanism, and `operatorIntegration: true`. Model IDs and access
  groups must match the chart-derived policy bindings.
- The current native ingress/authentication deployment and an authorized
  owner who can sign in. An HTTPS redirect alone does not prove owner access.

For the bounded native profile, the platform registration explicitly selects
`capabilities.profile: native-single-start` (the JSON field is
`registrations.kata.Capabilities.profile`). It does not claim an independent
enforcement observer or physical failover fencing. The runtime, host resource
limits, baseline network controls, storage, and selected ingress mode must
still have their actual deployment checks. Do not enable this profile to bypass
these checks.

For this profile, `platforms.kata.adapter.name` is `cilium`, with an explicit
revision and `qualified: false`. `azure-cni-overlay-cilium` is the different
adapter name that the fully qualified profile uses. It is not interchangeable
with the native profile. These fields describe an observed deployment, not
proof that its controls work.

For a Custom terminal, also register the actual Kubernetes API Service and
backend IP destinations and ports in `platforms.kata.topology.kubernetesAPI`.
This is necessary even when the Agent has no `kubernetesAccess` grant. The
separate terminal helper must have its restricted API connection. Do not enable
Agent Kubernetes access or allow a broad internal CIDR to satisfy this helper
dependency.

For common-executor Custom, configure HOME through
`Sandbox.spec.storage.home`. Do not copy the ordinary runtime chart's
`persistence` values into the Operator's shared `cetirun.helmValues`. Custom
rejects that override surface. The non-Sandbox Custom storage settings do not
replace the selected Sandbox storage contract.

Native OpenClaw runs without an Istio proxy in the same Pod. As a result, its
proxy trust comes from the registered ingress peers, not from the legacy
`127.0.0.6` sidecar address. Exact peer IPs stay the default. Managed gateways
can select [rollout-stable ingress](#rollout-stable-managed-ingress) below.
Do not use a wildcard proxy range or a per-Agent Helm override.

The packaged native OpenClaw init containers use `/usr/bin/tini -g --` to
supervise the current HOME, doctor, and credential-retirement commands.
Use the matching OpenClaw image, which includes this init process. An older
image without this process cannot run the updated initializer commands.

Helper failures keep their exit status, and resource limits, authentication
cleanup, and the execution hold are unchanged. Standalone OpenClaw entrypoints
are unchanged. Prepared templates that already exist keep their exact recorded
command expectations. This does not replace or restart a native lifetime that
already exists.

This minimal example selects no skills. If no skills catalog is configured, the
native profile supplies an empty read-only skills directory. In that case, a
shared skills PVC or NFS publication is not necessary. To expose
already-published shared files, configure the validated publication/catalog.
Requested skills are never silently dropped. A configured catalog still uses
its normal validation and storage path.

A published-skills PVC that already exists does not prove that an npm or
ClawHub package request was materialized. Protected rendering rejects those
requests with `SkillsMaterializationPending`. It does not clear the package
list, and the Agent does not start with unrelated published content. This
applies to OpenClaw, Hermes, and Custom. Publish the necessary content through
the approved source contract. Do not bypass the hold with a raw chart value.

For a Custom request that the policy authorizes, unavailable materialization
does not revoke the Gateway key that exists. It blocks that key and holds
compute. When you remove the unmaterialized request, recovery can use the same
key and spend record. A request that is outside the policy still revokes
access. This does not automatically replace a key that was already revoked or
lost.

When Memory Context is enabled, reconciled OpenClaw and Hermes lifetimes
mount the common instructions read-only at `$HOME/.agents/AGENTS.md` and load
the platform Memory skill from `/opt/cetirun-memory/skills`. Personal files
elsewhere in `$HOME/.agents` remain writable. Shared skills use a direct
read-only PVC mount, so published updates remain visible during the Pod's
lifetime. The temporary context volume keeps the Sandbox scratch size bound.
Legacy single-start lifetimes do not enable this context path.

In the Operator values, explicitly select:

```yaml
sandboxSources:
  enabled: true
  nativeExecution: true
  bootstrapOwnedPrerequisites: true
  platformConfigMapName: sandbox-platform
  homeClaimName: home
  skillsClaimName: skills
aiGateway:
  enabled: true
sandboxAdmission:
  enabled: true
  enforce: true
  podsEnabled: true
```

The legacy `sandboxSources.helpers` flags do not control reconciled (`v2`)
native lifetimes. Those lifetimes use the helper settings of the runtime chart.
By default, OpenClaw doctor runs on each Pod initialization, even if
`sandboxSources.helpers.doctor` is `false`.

This is an **overlay**, not a complete installation configuration. Supply the
target's admission TLS/administrator/coordination settings, domain, registry,
Gateway configuration, and source registries through your normal private
deployment configuration. Do not commit that configuration.

On AKS, the control plane can append its system-namespace exclusions to
admission webhooks. By default, deployment preflight stays strict. An
administrator can explicitly pass `--allow-aks-system-exclusions` to
`deployment-preflight`. With this option, preflight accepts only the two exact
`NotIn` clauses for `control-plane: ["true"]` and
`kubernetes.azure.com/managedby: ["aks"]`, in addition to the original
protected namespace selector. Absent, changed, duplicated, or other clauses
stay blocked.

This option does not change webhooks or their failure policy. Agent namespaces
must not carry those system labels. Preflight still does not qualify the
runtime, test actual network enforcement, or establish application readiness.

Copy [configuration.yaml](configuration.yaml) and [agent.yaml](agent.yaml) to a
private run directory. Replace these items:

- `native-home` with the registered CSI storage class.
- `example-model` with the exact authorized Gateway catalog ID.
- The image and chart pins with the candidate artifacts.
- The synthetic owner fields.

Use a fresh Agent name and a matching user alias applicable to the
authenticated owner. The example registry is deliberately nonfunctional.

An administrator creates the reusable configuration through supported APIs.
The Agent creator uses an authorized owner context. Do not supply internal
authorization records, lifecycle finalizers, or manually edited status.
Do not create the Agent namespace in advance. Do not substitute unowned
HOME/Secret objects.

## Helper memory

The Portal's new-Sandbox form and [configuration.yaml](configuration.yaml)
recommend `spec.resources.helpers.limits.memory: 2Gi`. The memory request stays
`128Mi`. An observation showed that the OpenClaw 2026.9.6 doctor was OOMKilled
at the previous `512Mi` limit. With `2Gi`, initialization completed. This is an
initial recommendation, not a measured minimum or a guarantee for every runtime
and workload.

All init containers and any Agent sidecars share the per-helper resource
settings of the Sandbox. The init containers include `doctor`. When you plan
the aggregate Pod resource budget, include these containers and the
RuntimeClass overhead.

Sandboxes that already exist keep their stored limits. To increase these
limits, use the supported
[coordinated configuration update](../../app/cetirun-operator/README.md#coordinated-configuration-updates)
flow. A change to the Portal default does not resize Sandboxes that already
exist. It also does not bypass lifecycle and admission checks.

## Default startup window

With the matching runtime chart, common-executor OpenClaw uses 60 failed startup
probes at 10-second intervals by default. This prevents a restart that is too
early, before the gateway completes its initialization on Kata. Startup,
readiness, and liveness checks all stay enabled.
`startupProbe.failureThreshold: null` selects this default, but an explicit
positive value is kept. Retained single-start OpenClaw, Hermes, and ordinary
non-Sandbox paths keep their previous default of 30.

Probe time starts after initialization, not when a restart or resume is
requested. Doctor and credential retirement still run. On each start,
retirement examines saved auth, verifies any logout, and compacts the
databases. Test the complete operation on the selected storage and helper
resources within the originally declared bound. Include independent HOME and
model witnesses in this test. The larger probe window and offline regressions
do not establish default-profile Kata acceptance.

## HOME access mode

This example and Portal use `ReadWriteOncePod` (RWOP) by default. New
common-executor Kata lifetimes also support `ReadWriteOnce` (RWO). To use RWO,
select it in `Sandbox.spec.storage.home.accessMode`. Also, explicitly register
it in `registrations.kata.Capabilities.homeAccessModes`.

To convert an Agent that is in operation, do not change the mode of a current
HOME claim. Also, do not remove the ownership records of that claim. Retained
single-start lifetimes stay RWOP-only.

Both modes use the same retained restart, stop/resume, and separately enabled
healthy-source migration flow. RWO allows multiple mounts on one node. It does
not replace private HOME ownership, single-replica workload admission, or
acknowledged source termination and CSI detach before migration. The local
platform probe tests RWOP contention, not RWO lifecycle behavior. Before you
declare RWO support, qualify the selected Kata/CSI combination separately. An
RWOP probe result alone is not a sufficient reason to add RWO to the
registration.

## Kubernetes API access

New OpenClaw and Hermes Agents that use the common executor on Kata or on
container support `AgentPolicy.spec.kubernetesAccess`. This support applies to
either registered HOME access mode. The examples below use `kata`. For an
independently qualified ordinary-container backend, use the applicable
`container` registration and platform keys. Retained single-start (`v1`)
lifetimes still reject Kubernetes access. This is Kubernetes ServiceAccount
authorization, not Entra ID, Agent Identity, Broker, or Connections support.

Upgrade **all Operator and admission replicas before enabling** Kubernetes
access in a shared platform registration. The expectation/journal versions stay
`v2`/`1`. But older readers reject the new RBAC control kinds, also during
cleanup. They also reject the enabled platform, even for nil-access consumers.
Do not roll back while these declarations or execution records are still
present.

It is not sufficient to only disable the capability. Use current binaries for
normal revocation/cleanup and restore old-compatible state. As an alternative,
finish normal consumer deletion and make sure that grant/journal cleanup is
complete before you downgrade. Never remove journals, expectations, or
finalizers to bypass this rule.

Only platform administrators manage this policy field and its separate
approvals. The Portal shows it as read-only and keeps it when other fields are
saved. The Portal administrator role does not authorize changes to Kubernetes
grants. A policy request alone grants nothing. For example, this **policy
fragment** requests a namespaced binding to a ClusterRole that already exists:

```yaml
kubernetesAccess:
  serviceAccountName: kubernetes-reader
  roleBindings:
    - namespace: tools
      clusterRoleName: agent-pod-reader
```

Before the policy is bound to a Team, the administrator must:

1. Explicitly register `registrations.kata.Capabilities.kubernetesAccess: true`
   in the protected platform configuration. By default, it stays disabled.
2. Approve each requested role and binding scope in `platforms.kata.grants`.
   Every entry pins the policy UID and the UID of the ClusterRole that already
   exists. Every entry also pins the resource version, the complete rules, and
   a separate approval revision. An empty grant namespace authorizes a
   ClusterRoleBinding, not a namespaced RoleBinding.
3. Under `platforms.kata.topology.kubernetesAPI`, register the actual
   Kubernetes API Service and backend IP destinations and ports. Do not
   substitute a broad internal CIDR grant. The necessary API destinations cannot
   overlap blocked ports/CIDRs or protected node, management, or metadata
   ranges.
4. Set `platforms.kata.kubernetesAudience` to an audience that is accepted by
   that API server. By default, `tokenExpirationSeconds` is 600. If you set
   this value, it must be 600–3600.

This synthetic **grant fragment** shows the case-sensitive platform JSON
fields. Replace every identity and rule with the observed values of the
approved target. These placeholders are not deployment approvals:

```json
{
  "policyUID": "example-policy-uid",
  "namespace": "tools",
  "roleRef": {
    "apiGroup": "rbac.authorization.k8s.io",
    "kind": "ClusterRole",
    "name": "agent-pod-reader"
  },
  "roleUID": "example-role-uid",
  "roleResourceVersion": "123",
  "revision": "review-1",
  "rules": [{"apiGroups": [""], "resources": ["pods"], "verbs": ["get", "list"]}]
}
```

After the Agent has its API-assigned UID, register its dedicated identity in
`platforms.kata.identities`. Use that exact UID, the Agent's `cetirun-<agent>`
namespace, the requested ServiceAccount name, `kubernetes: true`, and an
approval revision. Startup stays held until this per-Agent approval exists.
Do not create a shared ServiceAccount in advance or adopt one. The operator
creates the owned account and per-Agent bindings. It never creates, edits, or
deletes the referenced shared ClusterRoles.

When the operator creates bindings, Kubernetes also examines the operator's
RBAC. If the operator does not already have the permissions of a role, an
administrator must separately grant the operator `bind` on **that named
ClusterRole**. When possible, the administrator must scope this grant to the
intended namespace. This feature does not add a blanket `bind`, `escalate`, or
Portal RBAC permission.

Only the main Agent container receives a read-only projected token that
rotates. This token is at `/var/run/secrets/kubernetes.io/serviceaccount`,
together with the namespace and the Kubernetes-published `kube-root-ca.crt` of
the namespace. Token automount stays off. Init containers and helpers do not
receive the projection. The CA must exist and is pinned as a normal admission
input. It is not replaced with an Agent-owned trust bundle.

Current Pod admission must have these items:

- The exact rendered projection.
- Live owned RBAC bindings.
- Confirmed UIDs in the grant journal that already exists.
- Unchanged role UID/version/rules.

Policy `Accepted` is configuration acceptance, not proof of execution. Examine
the Agent's current `SandboxReady`, applied revision, aggregate `PolicyApplied`,
and workload readiness.

Revoked or changed approvals hold compute and withdraw the recorded grants.
Suspension and deletion revoke both namespaced and cluster-wide bindings, but
they do not delete shared roles. For a resume, current approvals are necessary.
The resume also confirms newly created binding UIDs.

A nil `kubernetesAccess` removes the token projection and implicit API network
exception. After acknowledged shutdown, use the normal coordinated configuration
update. Do not edit grant journals or expectations to change access.

Unconfirmed binding writes keep their journal entries and the source
reservation held. An absent object alone does not complete revocation. The
controller records per-grant submission fingerprints and fences grants that
were never submitted. It also persists observed binding UIDs before it deletes
completed writes. This distinguishes a retained workload's earlier submission
from its next set of grants. Outcomes that are still uncertain stay held and do
not authorize another apply.

Only binding creates that are tried receive submission records. Binding updates
use UID/version preconditions, and Helm does not prune grants by name from
release history. Older records without per-grant evidence are treated
conservatively.

Admission also examines the owned ClusterRole-binding inventory, even when the
new policy removes all Kubernetes grants. An obsolete or unrecorded owned
workload grant blocks admission. Platform source-reader Roles are separate.
As a result, read-only `list` access to RoleBindings and ClusterRoleBindings is
necessary for the admission reader. This access is in addition to the
exact-object reads that it already has.

The served-API check in [DEVELOPMENT.md](../../DEVELOPMENT.md) tests these
contracts without a cluster or containers that run. Before you register a target
as supported, separately authorize live positive/negative API requests, token
rotation, and Cilium dataplane checks. For these requests and checks, use only
task-owned synthetic resources. Do not print the projected token, copy it to
HOME, or send it to a test witness.

## Scoped internal egress

New common-executor Kata OpenClaw/Hermes lifetimes can use
`AgentPolicy.spec.network.allowedInternalCIDRs` with either registered HOME
mode. For example, this synthetic **policy fragment** grants TCP `443`
throughout one private subnet. It adds UDP `8080` only in a subrange that
overlaps this subnet:

```yaml
network:
  allowedInternalCIDRs:
    - cidr: 10.20.0.0/16
      ports:
        - {port: 443, protocol: TCP}
    - cidr: 10.20.1.0/24
      ports:
        - {port: 8080, protocol: UDP}
```

Replace these ranges with approved internal destinations. These ranges are not
deployed test endpoints. When a protocol is not set, the default is TCP. The
requested protocol and IP family must be explicitly registered. For SCTP, the
[direct-endpoint limits](#governed-sctp-egress) below also apply.

Node, management, metadata, and mandatory excluded ranges cannot be granted.
Explicit CIDR/port denies still win. TCP/UDP `53` stays limited to approved DNS
peers, even if an internal rule includes that port.

For Cilium-managed Pods, a CIDR match alone is not sufficient. The platform
must register the exact namespace, protected peer-UID label, and addresses in
`topology.peers`, with a complete managed-peer inventory. All addresses of a
registered peer must receive the same effective port permissions. This does not
discover Pod IPs that change, or translate a Service's port to its backend
port. Do not grant node ranges, broaden selectors, or change the Cilium
deployment as a workaround for an incomplete registration.

When configuration coordination is enabled, suspend all consumers before you
update reserved policy or platform sources. Also complete their normal shutdown
before the update. After the coordinated configuration change, resume through
the ordinary lifecycle. Do not remove reservations or edit status. Retained
version-1 single-start lifetimes still reject internal grants.

Offline compiler, chart, and API-server checks do not qualify real traffic.
On an explicitly authorized target, use owner-authenticated Agent requests and
independent task-owned witnesses to examine these items:

- The allowed address/port/protocol
- Wrong ports/protocols
- Outside ranges
- Grants that overlap
- Explicit denies
- Managed-peer identities
- DNS/node/metadata isolation
- Unchanged peer Agents

After grant removal through the supported lifecycle, repeat the witness. Then
make sure that access is denied. Record the exact artifacts and registration.
This field does not complete the Istio handover or enable FQDN/mTLS controls.

## Governed SCTP egress

New common-executor Kata OpenClaw/Hermes lifetimes can request SCTP in both
`network.blockedPorts` and `network.allowedInternalCIDRs`. They use the current
Cilium compiler and owned CiliumNetworkPolicy, not a new proxy or dataplane.
Retained version-1 single-start lifetimes still reject SCTP port requests.

The current administrator-controlled
`registrations.kata.Capabilities.protocols` list is the protocol opt-in.
Registration examples and the local platform probe still use TCP/UDP by
default. Neither an API value of `SCTP` nor a successful TCP/UDP probe
qualifies SCTP. Only after a separately authorized qualification of the exact
Kata, kernel, Cilium, and address-family combination, add SCTP to that list.
For port denies, `blockedPorts: true` is also necessary. This **capability
fragment** is not evidence or a complete platform configuration:

```json
{
  "protocols": ["TCP", "UDP", "SCTP"],
  "blockedPorts": true
}
```

Keep the native adapter's `qualified: false`. This increment does not supply
independent enforcement observation or physical fencing. Do not enable SCTP in
Cilium or change host/kernel settings as part of an offline check.

This synthetic **policy fragment** grants SCTP `3868` to approved direct
internal endpoints and denies SCTP `3869` at every destination:

```yaml
network:
  allowedInternalCIDRs:
    - cidr: 10.20.0.0/16
      ports:
        - {port: 3868, protocol: SCTP}
  blockedPorts:
    - {port: 3869, protocol: SCTP}
```

The internal grant does not allow TCP/UDP `3868` or other SCTP ports. Grants
that overlap combine exact protocol/port pairs. Explicit CIDR and port denies
still take precedence. TCP/UDP DNS `53` and the registered Gateway ports are
still necessary. A configuration that blocks those pairs is rejected. When SCTP
is blocked on a numeric port, TCP/UDP on the same port is not blocked.

Ports must be `1`–`65535`. A protocol that is not set still means TCP, not
SCTP. For IPv6, an explicit `IPv6` registration and the applicable topology
are necessary. The current public-egress baseline is unchanged. Public addresses
stay allowed, except for exclusions and explicit denies. Protocol registration
is not a public-egress allow-list.

SCTP internal grants cannot overlap registered Service CIDRs or Kubernetes API
destinations. They also cannot overlap the current node, management, metadata,
and mandatory exclusions. For managed Pods, use direct endpoints and the
current protected namespace/peer-UID registration. Do not treat CIDRs as
Service discovery. All addresses of a peer must have identical effective port
permissions.

Cilium's [SCTP limitations](https://docs.cilium.io/en/stable/configuration/sctp/)
include unsupported pod-to-VIP policies, multihoming, port rewriting, BPF
masquerading, and egress gateway. This feature does not add those capabilities.
The administrator must qualify the actual direct path and any NAT in it.

The current [compiler budgets](../../app/cetirun-operator/internal/sandboxpolicy/limits.go)
and 40-entry port groups apply and do not truncate controls. An unregistered
protocol, unsupported family, malformed rule, protected destination, or
excessive expansion rejects the request. Admission validates a policy against
its selected Sandbox/platform. The storage of an unused AgentPolicy alone is
not authorization.

For policy or registration changes, use the ordinary stop/change/resume
lifecycle. Active policy reservations stay enforced. Administrator changes to
platform inventory invalidate old Pod authority. These changes are not an
in-place capability upgrade for Pods that are in operation. The compiler and
controller bind both the requested rules and registration into the current
revisions and source checks.

The served-API and compiler tests examine these contracts with synthetic
inventory. They do not run SCTP traffic through Kata/Cilium or qualify a
target. On a separately authorized target, examine these items:

- Allowed SCTP
- Wrong ports and TCP/UDP on the same port
- Grants that overlap
- Explicit denies
- Necessary DNS and Gateway access
- Protected destinations
- Peer isolation
- Grant removal

Record direct endpoint paths and each registered address family, not only
application success.

Observed-SNI filtering has a separate opt-in
[managed egress contract](../../app/cetirun-operator/README.md#managed-observed-sni-egress).
SCTP registration does not enable it. Ingress mTLS has a separate opt-in
[managed endpoint contract](../../site/content/en/docs/guides/agent-ingress-mtls.md).
SCTP registration does not enable it.

## Rollout-stable DNS and AI Gateway

The native Cilium profile supports namespace-scoped Pod selectors for DNS and
AI Gateway peers on both Kata and ordinary-container backends. Register the
stable Service IPs and the labels already supplied by their workload
controllers. This **topology fragment** illustrates the fields; it is not a
complete platform registration:

```json
{
  "dnsResolverIPs": ["10.96.0.10"],
  "dns": [
    {
      "name": "coredns",
      "namespace": "kube-system",
      "uid": "example-dns-uid",
      "ips": ["10.96.0.10"],
      "podSelector": {"k8s-app": "kube-dns"}
    }
  ],
  "gateway": [
    {
      "peer": {
        "name": "cetirun-ai-gateway",
        "namespace": "cetirun-system",
        "uid": "example-gateway-uid",
        "ips": ["10.96.1.50"],
        "podSelector": {
          "app.kubernetes.io/name": "cetirun-ai-gateway",
          "app.kubernetes.io/instance": "example",
          "app.kubernetes.io/component": "gateway"
        }
      },
      "ports": [{"port": 4000, "protocol": "TCP"}]
    }
  ]
}
```

Replace these synthetic values with the approved Service addresses and exact
selectors for the target installation. Keep the peer UID stable; do not use a
Pod UID. A selector must contain valid Pod labels and an exact namespace.
Tenants must not be able to create or relabel matching Pods in that namespace.
The platform owns that protection and the Service-to-Pod association.

Selector-bound DNS/Gateway entries must contain only Service IPs within the
registered `serviceCIDRs`, outside `podCIDRs`. Do not pin backend Pod IPs.
`dnsResolverIPs` still selects at most three approved DNS Service addresses.
The generated endpoint rules follow replacement Pods and keep the exact DNS
TCP/UDP and Gateway port restrictions. Complementary denies still exclude
unselected Pods, wrong namespaces and ports, including when another policy
adds an allow. Kubernetes API and arbitrary inventory peers retain their
existing registration contract.

Explicit Service IP and port denies still reject conflicting required access.
Blocked CIDRs that overlap registered Pod networks are rejected with this mode:
changing backend IPs cannot be mapped to stable endpoint denies. It does not
claim support for node-local DNS or host-networked backends.

Upgrade all Operator/admission readers before adopting these selectors. Change
the protected platform registration through the normal configuration-change
and lifecycle workflow; do not rewrite active expectations or reservations.
After that transition, a Pod rollout or scale-out does not change the
registration or the Agent's network-policy revision. Changing Service IPs,
selectors, namespaces, or peer UIDs still requires a coordinated update.
Existing IP/peer-UID registrations remain supported and still need current
backend IPs and protected peer labels.

Offline compiler tests cover replacement addresses, namespace/label and port
denies, mixed legacy peers, resolver stability, terminal isolation and bounded
selector expansion. Controller tests cover repeated DNS/Gateway Pod replacement
without changing Agent workloads on both backends. These tests do not replace
separately authorized AKS/Cilium rollout, scaling and dataplane acceptance.

## Rollout-stable managed ingress

For an AKS-managed Istio gateway, register the stable labels that the gateway
Deployment already supplies. Use these labels instead of manual labels on the
individual gateway Pods. The native single-start profile accepts this
**one-entry fragment** under `platforms.kata.topology.ingress` in the
administrator's `platform.json`:

```json
[
  {
    "peer": {
      "name": "aks-istio-ingressgateway-external",
      "namespace": "aks-istio-ingress",
      "uid": "example-ingress-deployment-uid",
      "podSelector": {
        "istio": "aks-istio-ingressgateway-external"
      }
    },
    "trustedProxyCIDRs": ["10.244.0.0/16"],
    "ports": [{"port": 18789, "protocol": "TCP"}]
  }
]
```

Replace the synthetic UID, namespace, labels, Pod CIDRs, and workload ports
with the approved target inventory. Use a stable gateway identity, for example
its Deployment UID, not a Pod UID. The registration declares that identity. It
does not discover or independently examine the Deployment. For native Hermes,
register its API port (`8642`) and Webapp port (`9120`) instead of the
OpenClaw port above. Refer to
[Create, connect and use](#create-connect-and-use).

This mode must have matching Operator and runtime chart support and these
items:

- At least one valid Pod label. All labels are matched within the exact
  registered namespace. Do not include `peer.ips`. Selection and proxy trust
  must not depend on the current Pods. The `peer-uid` Pod label is not
  necessary for this ingress entry. DNS and AI Gateway peers can separately
  opt in to [stable service selectors](#rollout-stable-dns-and-ai-gateway);
  other peers keep their current IP/peer-UID contract.
- Explicit `trustedProxyCIDRs` that are in the registered `topology.podCIDRs`.
  The compiler rejects wildcard ranges. It also rejects ranges that overlap
  Service, Node, management, metadata, loopback, or link-local networks.
  Register the actual source Pod networks of the gateway for each necessary IP
  family. This mode does not support host-networked gateways or paths that SNAT
  to Node addresses.
- A platform-controlled namespace and labels, and a gateway that does
  authentication and still enforces owner access. That gateway must overwrite
  or safely rebuild the forwarded-client headers. It must not be possible for
  tenants to create or relabel Pods in the selected gateway namespace.

Cilium still admits only the selected gateway Pods on the registered workload
ports. It explicitly denies other sources and ports. Trust of the bounded Pod
CIDRs in OpenClaw does **not** admit arbitrary Pods from those networks. If a
gateway Pod is replaced within these networks, it is not necessary to relabel,
update the registration, or restart the native Agent. OpenClaw's token and
device authentication stay enabled. This change does not enable the legacy
`openclaw.portalSignIn` mode for native execution.

This is an opt-in registration change, not an automatic migration of Agents
that are in operation. Lifetimes that already exist keep their recorded
configuration. Before you adopt a changed platform registration, arrange the
normal lifecycle transition. Do not edit the expectations, authorization, or
start markers of these lifetimes. Exact-IP registrations are still supported.
But each time the gateway Pods change, their protected labels and proxy IP list
must still be kept current.

Deployment preflight and offline rendering do not qualify gateway rollouts.
With separate authorization for the target, do these checks:

- Make sure that owner UI access works before and after a gateway Pod
  replacement.
- Make sure that the native Agent Pod and registration stay unchanged.
- Make sure that unauthenticated, non-owner, and non-gateway access is still
  denied.

Record any source-IP translation or network enforcement failure. Do not widen
proxy trust as an alternative.

## Create, connect and use

Set an absolute `KUBECONFIG`, explicit `OWNER_CONTEXT` and `ADMIN_CONTEXT`, and
the expected `CLUSTER_UID` from the approved target inventory. Before any write,
make sure that both contexts refer to that target:

```bash
set -euo pipefail
for context in "$OWNER_CONTEXT" "$ADMIN_CONTEXT"; do
  test "$(kubectl --kubeconfig "$KUBECONFIG" --context "$context" \
    --request-timeout=15s get namespace kube-system -o jsonpath='{.metadata.uid}')" \
    = "$CLUSTER_UID"
done
```

Create the edited configuration with the administrator context. Use
`kubectl create -f configuration.yaml` with the same explicit kubeconfig and
context arguments. If any named configuration already exists, stop and select
new names or arrange deliberate reuse. Do not overwrite it.

For the Agent, use the owner context. Keep the creation receipt outside the
repository:

```bash
k() {
  kubectl --kubeconfig "$KUBECONFIG" --context "$OWNER_CONTEXT" \
    --request-timeout=15s "$@"
}
AGENT_NAME=native-example
STATE_DIR="$(mktemp -d -t cetirun-native.XXXXXXXX)"
test -z "$(k get agent "$AGENT_NAME" --ignore-not-found -o name)"
test -z "$(k get namespace "cetirun-$AGENT_NAME" --ignore-not-found -o name)"
k create -f agent.yaml -o json > "$STATE_DIR/created.json"
jq -e --arg name "$AGENT_NAME" \
  '.metadata.name == $name and (.metadata.uid | type == "string" and length > 0)' \
  "$STATE_DIR/created.json" >/dev/null
k wait --for=condition=Ready "agent/$AGENT_NAME" --timeout=10m
k get agent "$AGENT_NAME" -o jsonpath='{.status.accessUrl}'
```

If the creation outcome is uncertain, examine it through authoritative reads
before you continue. Do not retry creation or delete by name.

1. Make sure that the actual Pod runs the selected image and RuntimeClass, with
   the expected volumes and current application readiness. Record failures. Do
   not change replicas, expectations, or finalizers as an alternative.
2. Open the returned native URL as the owner and sign in normally. After the
   controller publishes the confirmed workload namespace, the current Portal
   Overview supplies **Copy credential** for the Gateway token. Paste that
   token into the native UI, or use OpenClaw's browser-only `#token=` handoff.
   Never put the token in a query string, log, example file, or shared receipt.

   Normal device pairing is still necessary for a new browser. If OpenClaw asks
   for approval, examine the request. After you examine it, run
   `openclaw devices approve <exact-current-request-id>` in the current
   owner-authenticated web terminal. The registered ingress destinations must
   include TCP `7681` for that terminal and the actual OpenClaw listener. These
   destinations must accept only the approved ingress peers.

   For a native Hermes Agent, the destinations must include its API port,
   `8642`, and its Webapp port, `9120`. The Portal routes `/ui/<agent>/` to
   this Webapp port. Without these ports, the Hermes UI times out at the
   ingress.

   This is an explicit owner approval, not Kubernetes Pod exec or disabled
   device authentication. Make sure that an authenticated native WebSocket
   session works. A handshake challenge alone is not sufficient. Make sure that
   unauthenticated and nonowner access cannot use the Agent.
3. Select the allowed real model. Then get a response through the Gateway.
4. Select a task-owned synthetic witness endpoint that baseline networking
   allows. Ask the Agent to make a harmless request with a fresh random nonce
   to that endpoint. Independently examine the receipt at that endpoint. Model
   prose that claims success is not evidence that the tool ran. Do not send
   credentials or user data to the witness. Do not use protected-Pod
   exec/attach/port-forward as an access bypass.

The legacy full `scripts/e2e-test.sh` flow is not the acceptance run for this
profile. Its suspend/resume and legacy integration checks are outside this
profile. Reuse its explicit-target/UID ownership principles, not its broader
assertions. FQDN rules, unregistered SCTP port requests, Copilot sidecars,
Entra/Agent Identity grants, and live reconfiguration are not supported here.
They must not be silently ignored.

### Optional CIDR and TCP/UDP denies

The policy can include `network.blockedCIDRs` for registered address families
and `network.blockedPorts` for TCP/UDP. For example, these synthetic values deny
one documentation subnet and two destination ports:

```yaml
network:
  blockedCIDRs:
    - 198.51.100.0/24
  blockedPorts:
    - {port: 18080, protocol: TCP}
    - {port: 18081, protocol: UDP}
  ingress:
    mode: authenticated
```

This is a policy fragment, not a complete AgentPolicy or a reachable test
endpoint. For port denies, the platform support must be confirmed and recorded
as `registrations.kata.Capabilities.blockedPorts: true`. The baseline example
does not set that flag. Rules cannot block necessary DNS or Gateway traffic.
The current public-egress baseline continues to apply. These fields are not a
public destination allowlist or FQDN policy.

On an authorized target, use separate Team/Policy bindings for a restricted
Agent and a control Agent. Compare both Agents against the same independently
observed task-owned endpoint. In this comparison, include an allowed port that
is known to work for each protocol. Make sure that DNS and a real allowed model
request still work. Service failures and generic transport errors must not
count as policy denials. Do not use the documentation addresses above as if a
listener exists there.

Apply Team/Sandbox/AgentPolicy changes only through the supported coordination
path after their consumers complete cleanup. Keep the approved platform
inventory fixed during the run. A fresh Agent can test a changed policy. This
is not resume, live rollout, or transfer of a HOME that already exists.

### Repeat the network comparison

[native-network-check.py](../../scripts/native-network-check.py) examines the
network part of this first run. It makes only explicit-target Kubernetes reads
and credential-free requests to your witnesses. It does not create or delete
Agents, change policies, execute in protected Pods, or establish owner
authentication, real model/tool use, or runtime qualification.

Use three fresh OpenClaw Agents from this example, with different Team and
AgentPolicy objects and the same approved Sandbox/artifact configuration:

| Role | Policy controls |
| --- | --- |
| `control` | No `blockedCIDRs` or `blockedPorts` |
| `cidr` | One CIDR that covers the restricted witness, but not the outside witness. No denied ports. |
| `ports` | TCP `18080` and UDP `18081` are denied. No denied CIDRs. |

The Agents must be Ready before you prepare a run. Supply the exact names. The
resource names of the Agents can be different from these role names.

Deploy [network-witness.py](network-witness.py) on **two separate task-owned
IP addresses outside the platform's internal/management ranges**. It must have
Python 3, TCP `18080`/`18082`, and UDP `18081`/`18083`. For each protocol, the
second port is the allowed control. Restrict access to the validation
participants. By default, it uses loopback. For a non-loopback binding,
explicit permission is necessary:

```bash
python3 network-witness.py --listen-address 0.0.0.0 --allow-public-listen \
  --allow-peer "$VALIDATION_CLIENT_CIDR" --allow-peer "$CLUSTER_EGRESS_CIDR"
```

Include every confirmed egress address that the test Agents use. Also include
the validation host. Peer ranges are necessary beyond loopback. Unrestricted
`/0` ranges are rejected. No credential is used for witness access.

This fixture stores only generated synthetic run/nonce pairs in memory. Do not
send credentials or other data to it. If a witness is replaced or its bounded
event log overflows, a prepared run becomes invalid. Do not use a customer
service, an arbitrary public endpoint, or the documentation IP ranges as a live
witness.

After you examine the target and the current configuration of the Agents,
create a private plan on the validation host:

```bash
python3 -B scripts/native-network-check.py prepare \
  --kubeconfig "$KUBECONFIG" --context "$ADMIN_CONTEXT" --cluster-uid "$CLUSTER_UID" \
  --control-agent "$CONTROL_AGENT" --cidr-agent "$CIDR_AGENT" --ports-agent "$PORT_AGENT" \
  --restricted-witness "$RESTRICTED_WITNESS_IP" --outside-witness "$OUTSIDE_WITNESS_IP" \
  --dns-name "$APPROVED_GATEWAY_DNS_NAME" --output "$STATE_DIR/network-plan.json"
```

The file is created exclusively with owner-only permissions. It contains
private resource identities and endpoints, not credentials. Do not commit or
publish it. The checker pins Agent/configuration/namespace/Pod/runtime/image
identities and its own source. Kubernetes client authentication must use the
explicit verified-HTTPS context without plugins or impersonation.

Generate the readable probe command for each phase:

```bash
python3 -B scripts/native-network-check.py command \
  --plan "$STATE_DIR/network-plan.json" --phase control-before
```

Run that command through the **normal owner-authenticated terminal of the
control Agent**, not Kubernetes exec/attach/port-forward. Save its single
`CETIRUN_NETWORK_RESULT=` output line privately as `control-before.txt`.
Repeat these steps for `cidr` in the CIDR Agent and for `ports` in the
port-restricted Agent. Then repeat them for `control-after` in the control
Agent. Do not replay a phase. Duplicate nonces fail validation.

Probes have a 60-second deadline. They examine the recorded hostname, DNS
resolution, and all eight destination/protocol/port combinations. The command
supervises the complete probe in a separate process. The probe includes native
DNS resolution. On timeout or cancellation, the command terminates and reaps
the worker before it fails without a result line. Partial observations are not
accepted.

Within 30 minutes, examine the results. Use the same checker source and
unchanged Agents:

```bash
python3 -B scripts/native-network-check.py verify \
  --kubeconfig "$KUBECONFIG" --context "$ADMIN_CONTEXT" --cluster-uid "$CLUSTER_UID" \
  --plan "$STATE_DIR/network-plan.json" \
  --result "$STATE_DIR/control-before.txt" --result "$STATE_DIR/cidr.txt" \
  --result "$STATE_DIR/ports.txt" --result "$STATE_DIR/control-after.txt"
```

To examine the results, the checker independently reads both witnesses. It
also examines the source pins again before and after it collects evidence.
Positive controls must bracket the restricted probes. An allowed packet must
have a witness receipt that matches it. A denied packet must have no receipt
and the expected timeout type. Refused connections, malformed replies,
post-connect TCP timeouts, absent controls, stale/replayed results, source
drift, and unknown observations fail the check.

Each witness request that `prepare` or `verify` makes has a five-second total
deadline. This deadline applies to the connection, the response headers, and
the body collection. Continued partial responses do not reset that deadline.
An overrun closes the connection and fails with `witness-deadline-exceeded`.
No polling-to-success or request retry turns those failures into a pass.

A summary that passes covers 32 network checks. These checks include eight
expected denials. The summary deliberately leaves `qualifiesExecution` false.
It also leaves owner access, model/tool use, and cleanup as `not-tested`.

Continue the separate first-run steps above and below on the same build. Do
not promote this network result to an E2E pass. Keep initial failures. After
validation, remove the two task witnesses.

## Delete and verify cleanup

Examine the target UID again. Compare the current Agent with the creation
receipt. Then issue a UID/resourceVersion-conditional deletion:

```bash
test "$(k get namespace kube-system -o jsonpath='{.metadata.uid}')" = "$CLUSTER_UID"
expected_uid="$(jq -er '.metadata.uid' "$STATE_DIR/created.json")"
k get agent "$AGENT_NAME" -o json > "$STATE_DIR/current.json"
jq -e --arg uid "$expected_uid" '.metadata.uid == $uid' \
  "$STATE_DIR/current.json" >/dev/null
jq '{apiVersion:"v1",kind:"DeleteOptions",
  preconditions:{uid:.metadata.uid,resourceVersion:.metadata.resourceVersion},
  propagationPolicy:"Background"}' "$STATE_DIR/current.json" |
  k delete --raw "/apis/cetirun.microsoft.com/v1alpha1/agents/$AGENT_NAME" \
    -f - > "$STATE_DIR/delete.json"
k wait --for=delete "agent/$AGENT_NAME" --timeout=10m
k wait --for=delete "namespace/cetirun-$AGENT_NAME" --timeout=10m
```

For a conflict or an unknown delete outcome, observation is necessary, not
automatic replay. Never force-delete the namespace, Pod, storage, or lifecycle
finalizers. Make sure that the route is withdrawn, the Gateway grant is
revoked, and the owned resources are cleaned up. Follow the storage reclaim
policy. Report any PVs that are kept and any continued cost.

When a skills publication is configured, its read-only shared source is not
owned by this Agent and must survive. The no-skills example has no shared
skills storage to keep or delete.

Reusable Sandbox/AgentPolicy/Team configuration is separate from Agent cleanup.
If this configuration was created only for this run, make sure that no
consumers are left. After that, remove it with the current
[coordinated configuration deletion procedure](../../app/cetirun-operator/README.md).
Report deliberately kept configuration and any target infrastructure. Unknown
cleanup is not a pass.

Keep a sanitized receipt that ties the source/image/chart pins and the five
results to the same run. The results are **provisioned, Ready, owner connected,
real model/tool action, cleaned up**. Keep private target IDs and credentials
out of PRs. When the task-local receipt files are no longer necessary, remove
them.

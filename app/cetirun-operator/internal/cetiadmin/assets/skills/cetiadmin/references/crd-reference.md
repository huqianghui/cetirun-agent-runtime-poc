# CRD and platform topology reference

`Agent` is a cluster-scoped, long-lived runtime resource in
`cetirun.microsoft.com/v1alpha1`. The Operator creates its namespace, normal
runtime release, authentication, routing, and storage bindings. Names that
start with `openclaw-` are also used by Hermes and do not identify the runtime.

These fields follow the current `api/v1alpha1/agent_types.go` and
`harness_types.go` contracts. The installed CRD, bound target, and resolved
runtime profile are authoritative. An optional API that is absent is not the
same as a Forbidden response or a failed observation.

## Agent input fields

This minimal example shows the shape for a requested ordinary Hermes instance.
`example-agent` and `example-team` are synthetic names, not approved identities.
Creation requires deployment-admin supplied, trusted owner input through the
current admission contract. Do not invent or copy another user's owner claims.

```yaml
apiVersion: cetirun.microsoft.com/v1alpha1
kind: Agent
metadata:
  name: example-agent
spec:
  agent:
    type: hermes
  user:
    alias: example-agent
    displayName: Example agent
  team: example-team
  model: auto
  modelAccess:
    sharedModels: true
    githubCopilot: false
  gatewayTokenSecretRef: gateway-token
```

| Field | Contract |
| --- | --- |
| `metadata.name`, `spec.user.alias` | Use the approved instance name and alias consistently. Alias follows `^[a-z][a-z0-9-]{0,62}$`; check collisions before creation. |
| `spec.user.displayName`, `spec.user.email` | Required display name and optional contact email. Email is not a verified owner identity. |
| `spec.team` | The actual `Team` assignment used for configuration, policy, skills, and model accounting. |
| `spec.user.team` | Deprecated display context, not a Team assignment. Do not use it to select policy or authority. |
| `spec.agent.type` | `hermes` by default; `openclaw` and `custom` are explicit choices. Immutable after creation. |
| `spec.agent.image` | Native runtime image pin. Required for Custom; built-ins resolve and pin their defaults. Once pinned, it cannot be removed. |
| `spec.agent.username` | Existing non-root image user, default `vibe`, with HOME `/home/<username>`. Immutable; Kubernetes does not create that user. |
| `spec.agent.port` | Resolved native HTTP port for built-ins. For Custom, an optional Chat HTTP port, or a Web UI port when `webUI=true`; omission means headless. |
| `spec.agent.webUI` | Custom-only browser application mode; requires `port`. |
| `spec.agent.registrySecretRef.name` | A Portal-managed, UID-bound credential reference. Not an inline credential or permission to read the credential store. |
| `spec.gatewayTokenSecretRef` | Required native-auth Secret name for Hermes and OpenClaw; forbidden for Custom. Not the model-access key. |
| `spec.model` | Preferred model already authorized by the AI Gateway. An existing selection is preserved during migration. |
| `spec.modelAccess` | Requested `sharedModels`, `githubCopilot`, and optional `monthlyBudgetUSD` within current policy. Does not create models or grant owner OAuth consent. |
| `spec.sandboxRef` | A Team-permitted Sandbox selection. CetiAdmin must not remove it to bypass protected execution. |
| `spec.skillOverrides.add`, `.remove` | Per-instance deltas against Team skills. Not an operations permission or an instruction to overwrite the built-in CetiAdmin skill. |
| `spec.agentIdentity.enabled` | Existing Entra Agent Identity integration. It is separate from optional Azure Workload Identity. Identity values and owner claims are platform-managed. |
| `spec.tailscale.authKeySecretRef` | Optional reference with key `TAILSCALE_AUTH_KEY`. Identity or credential changes require the responsible administrator. |
| `spec.helm.version` | Pinned runtime Chart version. A normal upgrade pairs this with the matching `spec.agent.image` and profile. |
| `spec.helm.values` | Runtime configuration accepted by the platform. The API map does not grant CetiAdmin arbitrary overrides, mounts, identity, RBAC, or model credentials. |
| `spec.suspended` | Retained suspension intent; stops the workload and routes through the existing lifecycle. Does not authorize direct Helm writes. |
| `spec.restartRequestID` | A retained restart ID, 16–128 ASCII letters, digits, `_`, or `-`. Reusing the current ID does not restart again. A suspended Agent cannot request a new restart. |
| `spec.plannedMigration` | Existing healthy-source cold-move contract. Not force deletion, failover, or a way around an unfinished move. |

The trusted owner annotations are
`agentidentity.cetirun.microsoft.com/owner-tenant-id` and
`agentidentity.cetirun.microsoft.com/owner-object-id`. Trusted platform code
writes them from verified owner claims. CetiAdmin cannot change an existing
owner or Team assignment; it cannot infer those claims from a display name.

`cetiadmin.model.initialMonthlyBudgetUSD` is installation input for the first
Virtual Key of that CetiAdmin UID. It is not a replacement for Agent policy
budget fields, a global Gateway default, or a per-restart budget reset. Keep
existing spend, budget periods, administrator adjustments, and key blocks.

The CetiAdmin Helm-maintenance extension uses
`spec.helm.maintenance.requestID` and `.expectedRevision`.
`status.helmMaintenance` reports the matching request, observed generation,
revision, and phase. Native writes require current `Held` evidence, not an old
`HelmMaintenanceReady` condition. If the installed schema or controller does
not provide the handoff, native Helm writes are unavailable. See
[the maintenance procedure](remediation-playbooks.md#helm-maintenance-handoff).

## Observed status and readiness

| Status | Interpretation |
| --- | --- |
| `status.phase` | `Pending`, `Provisioning`, `Ready`, `Failed`, `Deleting`, or `Suspended`. Do not restart a deliberately suspended instance. |
| `status.namespace`, `status.helmReleaseName` | Actual runtime locations. Check their ownership and UIDs rather than deriving authorization from names. |
| `status.accessUrl` | Actual published access URL, commonly `https://example.com/ui/<alias>/`. Do not guess a per-alias subdomain. |
| `status.observedGeneration` | Generation the Operator observed. A stale Ready phase does not qualify a new configuration. |
| `status.observedRestartRequestID` | Restart request observed on the current Ready Pod; match the request and generation before claiming restart completion. |
| `status.observedOperatorHash`, `status.lastReconcileTime` | Reconciliation observations, not standalone runtime health evidence. |
| `status.aiGateway` | This instance's key identity, models, budget, spend, reset time, block state, and readiness. Not permission to manage keys globally. |
| `status.policy`, `status.sandbox`, `status.registryCredentials`, `status.plannedMigration` | Applicable policy, execution, credential-binding, and migration observations. Missing or stale evidence is not success. |

Core conditions include `Ready`, `HelmDeployed`, `IngressReady`, `PodReady`,
`AgentConfigReady`, and `AIGatewayReady`. Read applicable
`TailscaleReady`, `AgentIdentityReady`, `RegistryCredentialsReady`,
`SandboxReady`, `PolicyResolved`, `PolicyApplied`, `DesktopConfigured`, and
`TerminalConfigured` conditions too. A deliberately disabled optional feature
need not be True. Judge readiness against the current requested configuration
and generation, not "every stored condition is True."

Normal creation progresses from Pending through Provisioning to Ready or
Failed. Retained suspension reports Suspended. Deleting is a separate,
potentially destructive lifecycle; it is not suspension or guaranteed retention.

## Runtime resources, ports, and credentials

Ordinary names often include namespace `cetirun-<alias>`, release
`openclaw-<alias>`, and Service or StatefulSet
`openclaw-<alias>-cetirun`. Keep these names even when the runtime is Hermes.
The Operator also manages the namespace role/binding, native authentication
Secret, configuration, NetworkPolicy, PVC, and standard routes. Read the
actual bound resources, selectors, owner references, and UIDs before acting.

| Runtime endpoint | Current native port | Meaning |
| --- | --- | --- |
| Hermes API | 8642 | Native API; health path `/health` |
| Hermes Webapp | 9120 | Native browser UI; not a public unauthenticated endpoint |
| OpenClaw Gateway | 18789 | Ordinary OpenClaw only; `/healthz`, `/readyz`, `/startupz` |
| ttyd | 7681 | Optional native web terminal |
| noVNC | 6080 | Optional desktop browser endpoint |
| x11vnc | 5900 | Desktop-internal VNC listener, not a public route |
| Chrome CDP | 18800 | Desktop browser control, not a public route |
| Managed ingress mTLS | 15443 / 15444 / 15445 / 15446 | API / Webapp / terminal / desktop proxy listeners when that target has the approved mTLS binding |

Use the selected profile's resolved port and the actual Service/route. Do not
replace a bound TLS destination with the table's plaintext backend, or route
every runtime to 18789. Current profiles are Hermes 0.22.7 and OpenClaw 2026.9.6;
the paired image/Chart qualification determines their use in an installed
release. Custom ports and endpoint modes come from that target's explicit spec.

| Purpose | Source and boundary |
| --- | --- |
| CetiAdmin Kubernetes CLI | `/var/run/cetirun/kubernetes/config`, context `cetiadmin`, API-audience `tokenFile` at `/var/run/cetirun/kubernetes/token`, and `ca.crt` beside it |
| Connections MCP | Existing projection under `/var/run/cetirun/connections` and its CA binding; different audience, never substituted for the API token |
| Hermes native authentication | `API_SERVER_KEY`, matching `hermes.env`, and the separate `CETIRUN_AUTH_KEY` in the referenced native-auth Secret |
| Ordinary OpenClaw authentication | `OPENCLAW_GATEWAY_TOKEN`, corresponding native configuration, and separate `CETIRUN_AUTH_KEY` |
| Model access | Operator-managed per-Agent Virtual Key supplied as `CETIRUN_GATEWAY_API_KEY`; not the native UI key or a Gateway master key |
| Optional Azure Workload Identity | Existing managed identity, dedicated SA federation, and separate Azure projection supplied by deployment administrators |
| Optional Slack | `monitor.slackSecretName` in this instance's namespace, injecting `SLACK_BOT_TOKEN` and `SLACK_APP_TOKEN` into native Hermes; contents never enter monitor JSON |

## Diagnostic coverage and write boundaries

The bound installation selects managed Agent namespaces and explicit
`platformReadNamespaces`. Platform components can live in differently named
namespaces; examples such as `cetirun-system`, `cetirun-portal`,
`aks-istio-system`, `aks-istio-ingress`, and `kube-system` are not implicit grants.

| Diagnostic purpose | Read-only resource domains |
| --- | --- |
| Workload health | Explicit core, apps, batch, autoscaling, and policy resources; Pod logs and Events in approved namespaces |
| Routes and networking | Services, EndpointSlices, NetworkPolicy, Istio networking/security/telemetry, Gateway API, and relevant Cilium state |
| Storage | PV, PVC, StorageClass, CSI resources, and volume snapshot status |
| Security/configuration audit | Role/Binding, ClusterRole/Binding, CRD, APIService, admission configuration, certificate-request status, and relevant Gatekeeper state |
| AKS and monitoring | Installed diagnostic resources in `acn.azure.com`, `aks.clusterconfig.azure.com`, `clusterconfig.azure.com`, and `azmonitoring.coreos.com` |
| Scheduling and API health | Node, Namespace, RuntimeClass, PriorityClass, Lease, API flow-control resources, and GET `/healthz`, `/readyz`, `/livez`, `/metrics`, `/version` |
| CetiRun ownership and intent | Agent, Team, Sandbox, and AgentPolicy observations; reading them does not grant mutation authority |

Bindings grant specific resources, not the old wildcard API rules. Optional
CRD absence is not applicable; an installed but unreadable API is a permission
gap. Do not describe that gap as a healthy empty result.

Allowed writes are limited to admitted Agent creation/updates, eligible ordinary
Pod restart/exec, exact native-auth fields, confirmed PVC growth, target Events,
and the selected release's supported objects during `Held`. Standard routes
are recovered by the Operator, not arbitrary VirtualService edits. Team,
Sandbox, AgentPolicy, RBAC, SA, NetworkPolicy, admission, Node, namespace/PVC
deletion, finalizers, protected bindings, platform, and self-modification are
outside CetiAdmin's write authority.

Helm reads include unchanged supported manifest objects such as SA, Role,
RoleBinding, NetworkPolicy, Sidecar, admission policies/bindings, PVC, PV,
Ingress, HPA, and PDB. Read access does not allow writing them. Namespace-level
Secret list access can expose more than the release's Secrets; never interpret
Helm labels as an RBAC-enforced content filter.

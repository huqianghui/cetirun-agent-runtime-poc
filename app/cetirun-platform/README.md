# CetiRun Platform chart

Deploy with `make deploy DEPLOY_CONFIG=/path/to/environment.yaml`.
This command manages the CetiRun Istio/OAuth2 Proxy release, the Broker and
Auth SDK in a private credential namespace, and the platform release. The
platform includes the Operator, admission server, Portal, AI Gateway,
PostgreSQL, Valkey, and Connections MCP. The Operator installs CetiAdmin through
its Hermes runtime and asset export path. The same configuration enables Portal
Chat and supplies Custom runtime, terminal, Memory, and shared Skills/HOME
references. The optional [Skill Marketplace](#skill-marketplace) is installed
only when the environment file configures it.

The `cetirun-platform` Helm chart contains the platform components. Its
switches stay available to chart consumers. The full set is necessary for the
deployment script without a `--full` option. CetiAdmin is enabled by default,
and it must have explicit owner, budget, and Kubernetes API inputs. Cloud
identities, certificates, Sandbox enrollment, and arbitrary Agent creation
are not part of this entry point. Operator removal has a separate, guarded
configuration path described below.

## Deploy the platform

### Choose the deployment path first

Azure infrastructure creation, platform installation, Agent creation, and model
provider authorization are separate operations. The entry point below does not
provision the cluster or qualify a Sandbox. Do not interpret its workload waits
as an end-to-end Agent or model-readiness check.

The full `make deploy` path must have the published artifact inventory, identity
inputs, and callable shared models. Its default routing mode also requires
shared model tiers and the `auto`/`decision-1` configuration described below.
Explicit `modelRouting.mode: direct` omits that classifier dependency. Before
you start, make sure that you have the prerequisites for the selected mode.
A provider quota or account restriction is not a slow Pod startup.

A Copilot-only installation instead uses the
[component chart path](#use-the-component-chart-directly). Shared models are
optional there. Keep the AI Gateway and
`cetirun-operator.aiGateway.copilotProxy.enabled` enabled. The owner must still
complete GitHub sign-in before they use a Copilot model. This is a
deliberately selected installation mode, not a way to make the full helper
accept missing prerequisites. Neither path authorizes you to copy a user's
credentials or to bypass provider restrictions.

### Prepare and apply the full deployment

Prepare an environment file outside the repository. The
[synthetic example](ci/deployment-example.yaml) shows the platform input shape.
The example enables CetiAdmin with a synthetic owner, budget, and API access.
Replace them as [Configure CetiAdmin](#configure-cetiadmin) describes, or set
`cetiadmin.enabled: false`. The certificate placeholder, image digests,
identities, prices, and owner of the example are test data, not deployable
credentials or qualification.

The file contains these inputs:

For a test release, `REGISTRY` and `VERSION` environment variables or Make
command-line assignments override only the release coordinates in memory.
They do not rewrite the environment file. Without explicit overrides, the
file's coordinates take precedence over defaults. The Python entry point also
accepts `--registry` and `--version`. Use the same values for `deploy-plan`
and the separately authorized `deploy` invocation. Existing explicit image
and digest pins are not cleared; conflicting coordinated values fail validation.

```bash
REGISTRY=registry.example.com/team/test VERSION=0.1.1-dev.1 \
  make deploy-plan DEPLOY_CONFIG=/path/to/environment.yaml
```

| Input | Meaning |
| --- | --- |
| `target` | Explicit kubeconfig context, its cluster name and HTTPS API server exactly as in the kubeconfig, and the `kube-system` namespace UID that you get independently |
| `release` | Registry with any repository prefix, release version, full source Git SHA, and optionally `excludeImages` for declared optional images. Omitted registry/version use the public MCR prefix and source Runtime Chart version |
| `domain`, `identityRealm`, `ownerAlias` | Public hostname, Portal Chat identity realm, and the Portal administrator alias of the real platform owner |
| `publicTLS` | Optional Azure Public IP DNS or custom-DNS HTTP-01 certificate configuration. It derives the Helm release from `platform`, and the ingress namespace from `istio` |
| `identity` | Entra tenant, Portal, blueprint, authorization, and encryption client IDs, and the Key Vault key URL. Use the URL without a key version, for example `https://<vault>.vault.azure.net/keys/<key>`, so that new records use a rotated version |
| `istio` | Separate release/namespace, the managed control plane and ingress identities that exist, mesh ConfigMap, and Istio Chart values |
| `broker` | Separate credential namespace/release and Broker Chart values, for example, references to TLS Secrets that exist and references to provider credential Secrets. The helper stops if a provider Secret that the file names does not exist. Remove the providers that you do not register |
| `platform` | Platform release/namespace and umbrella values, for example, admission, provider credentials, model deployments, and qualified Custom images |
| `platform.values.cetirun-operator.cetiadmin` | CetiAdmin owner assignment, initial budget, exact API access, optional migration source, and runtime options |
| `sharedSkills` | The shared Skills storage fields that exist in the runtime chart, for example, storage class, resource group, account, and container |
| `skillMarketplace` | Optional. The Git repository of the [Skill Marketplace](#skill-marketplace), its credentials Secret, and its Blob NFS container |

The managed Istio control plane, ingress workload, DNS, cloud identity
federation/permissions, storage classes, and referenced Secrets are
prerequisites unless the optional public DNS/TLS preparation below supplies
the public hostname and certificate. Provision the other requirements through
the environment's approved workflow. Azure DNS mode uses `az` with the configured
subscription; the helper does not get Key Vault secrets or change
`current-context`. Every cluster operation uses `target.context`. Before a
write, the helper examines the cluster name, API origin, and cluster UID.

For one exact public hostname, add `publicTLS` to this same environment file.
`make deploy` then configures the Azure Public IP DNS label or verifies custom
DNS, installs or checks the separately managed cert-manager controller, and
uses the platform chart's certificate-only mode to issue staging and production
HTTP-01 certificates before it installs the normal CetiRun releases. See
[Public DNS and TLS](../../site/content/en/docs/operations/public-ingress.md).
The normal full platform upgrade retains the same public TLS resources. A full
release is never replaced with certificate-only values. Existing externally
supplied certificates and Key Vault workflows remain supported when
`publicTLS` is absent.

When `publicTLS` is enabled, `target.kubeconfig` is required and every command
uses that file with `target.context`; the shell `KUBECONFIG` is ignored. For
other deployments, read the target values from the context after making sure it
selects the intended cluster. The server must be the same string as in the
kubeconfig, with its port if it has one:

```bash
kubectl config view --minify --context <context> \
  -o jsonpath='{.contexts[0].context.cluster}{"\n"}{.clusters[0].cluster.server}{"\n"}'
kubectl get namespace kube-system --context <context> -o jsonpath='{.metadata.uid}{"\n"}'
```

`release.excludeImages` can omit a declared optional artifact, currently only
`cetirun-vscode`. It must be the same selection used during publication. Remove
every reference to an excluded image, including a Custom runtime
`approvedImages` digest; the helper rejects a configuration that still
references it. When the Agent CRD already exists, it also stops before a Helm
write if a retained Custom Agent still uses that image.

If the Operator must have registry credentials to pull the runtime chart, set
`platform.values.cetirun-operator.acrDockerSecret` to a
`kubernetes.io/dockerconfigjson` Secret in the platform namespace. The helper
then stops if that Secret does not exist.

AKS managed Istio adds the revision to the names of the control plane
Deployment, the ingress gateway Deployment, and the shared mesh ConfigMap. An
example is `aks-istio-ingressgateway-external-asm-1-30`. The ingress
ServiceAccount has no revision in its name. Read the names from the cluster.
Before a write, the helper stops if a configured Deployment does not exist. It
also stops if `istio.meshConfigMap` does not exist but
`istio-shared-configmap-<revision>` exists.

Use the available Helm values under the `values` field of each release. The
helper connects component URLs, namespaces, image coordinates, and identity
fields from the common inputs. Custom images must have digest-pinned
`approvedImages` and the storage settings whose UID/GID behavior you qualified.
Each approved image uses the AI Gateway binding. The publication of an image
does not establish that qualification.

The `customRuntime.agentManagedModel` and `customRuntime.gatewayImages` fields
are no longer supported. The helper rejects them before it runs external commands
and does not convert their values. Before you remove them from an older
environment file, confirm the intended model access against the running Operator
configuration.

By default, `modelRouting.mode` is `auto`. Configure the logical model `auto` with `auto_router/complexity_router`,
`classifier_type: jev`, and `jev_classifier_config.deployment_name: decision-1`.
The separate `decision-1` entry refers to the real `azure_ai/<deployment>` and
uses `model_info.base_model: azure_ai/Microsoft-Decision-1` with
`mode: evaluation`. The endpoint and the Secret environment reference are
necessary. Its `api_version` is optional, and LiteLLM 1.105.6 does not send it.
LiteLLM sends each decision request to
`<endpoint>/providers/microsoft/v1/systemone`, without a query. Before that, it
removes a `/models`, `/openai`, `/openai/v1`, or `/v1` suffix from the
endpoint.

Do not remove the `azure_ai/` prefix from the base model. LiteLLM has no price
for the name without the prefix. With the default `blockUnpricedModels: true`,
the Operator does not offer an unpriced model, and then `auto` does not work.

Put all four tiers and the real provider deployments in the environment file.
The router's price fields are necessary guard metadata, not an average of route
prices. Use the highest real tier rates. Billing stays on the selected backend
and classifier. Do not copy the synthetic fixture prices into service.

For an environment without Decision-1 quota, explicitly set:

```yaml
modelRouting:
  mode: direct
```

Keep at least one callable shared model in
`platform.values.cetirun-ai-gateway.models`. The model mode must be omitted,
empty, `chat`, `completion`, or `responses`; embedding and evaluation models
cannot serve Agent conversations. Omit `auto`, `decision-1`, and
classifier or `auto_router/` entries. If CetiAdmin is enabled, set
`platform.values.cetirun-operator.cetiadmin.model.default` to one of these
configured model names. Direct mode does not provide automatic model routing.
All identity, certificate, Secret, artifact, storage, admission, and pricing
checks still apply. After quota is available, provision and qualify Decision-1,
restore the router entries, and select `mode: auto`.

Run the offline plan first:

```bash
make deploy-plan DEPLOY_CONFIG=/path/to/environment.yaml
```

The plan validates the inputs and renders local charts in a temporary
directory. Its JSON lists the target, releases, workloads, the selected images
and all 9 charts, and `referenceCounts` by resource kind. The JSON does not contain
reference names, Secret keys, or extra input fields. YAML errors report their
position but do not show the contents. The plan does not connect to a cluster
or registry. Python, PyYAML, and Helm must already be installed.

After the target deployment is authorized, run:

```bash
make deploy DEPLOY_CONFIG=/path/to/environment.yaml
```

After the Portal is available, `make deploy-verify` performs the read-only
public DNS, Gateway, certificate, redirect, Portal readiness, and OAuth redirect
checks when `publicTLS` is enabled. It does not complete an interactive sign-in.

This command must also have kubectl, OpenSSL, and Docker buildx for read-only
artifact inspection. Their normal authentication must be configured before you
run the command. Before its first persistent write, the command examines
release ownership, the necessary Secret key names, storage references, and
image/chart availability. It refuses to adopt unmanaged objects or resources
from another Helm release.

The kubectl client must be within one minor version of the API server (which
must be Kubernetes 1.37 or newer). Preflight reports an incompatible client
before deployment writes. Select a compatible binary on `PATH`; the helper
does not install tools or change the current context. Repeated preflights in
one invocation reuse successful inspections only for exact image digest
references. Image tags, chart versions, target identity, ownership, Secrets,
and other live prerequisites are still checked again.

For an existing bundled PostgreSQL StatefulSet whose claim template omitted
`storageClassName`, preserve that omission. The Bound PVC can have the
defaulted StorageClass even though the immutable StatefulSet template has no
class. Opt into the following upgrade-only mode, using the actual PVC class
and the original requested size:

```yaml
platform:
  postgresqlStorage:
    mode: retain-default
    storageClassName: example-default
  values:
    cetirun-ai-gateway:
      postgresql:
        bundled:
          persistence:
            storageClass: ""
            size: 10Gi
```

This mode requires the existing release-owned StatefulSet, Bound PVC, bound
PV, StorageClass, and password Secret. It compares the complete rendered
claim template with the existing template (allowing only Kubernetes defaults)
and checks the actual PVC class, request, access mode, and PV binding. Their
UIDs and storage fields are retained for rechecks before and after each
platform upgrade. A missing or replaced object, changed size or template, or
foreign owner stops the deployment. This is not a storage migration or a
fresh-install mode; it never replaces the StatefulSet or PVC. Concurrent
administrative storage changes must be excluded during deployment, since
these checks and a Helm upgrade are not a single Kubernetes transaction.
New installations continue to require an explicit nonempty
`persistence.storageClass` without `platform.postgresqlStorage`.

Helm's client OpenAPI validation is enabled by default. If a diagnosed schema
download failure prevents deployment, explicitly set
`release.clientOpenAPIValidation: false` to pass Helm's
`--disable-openapi-validation` option. This skips only the client schema check;
the API server still validates resources and runs admission. It does not make
Helm's requests use strict unknown-field validation. Review the exact rendered
manifests before choosing this mode. All other preflight, ownership, TLS,
artifact, rollout, and admission checks remain active. The final Team probe
uses the fixed API collection with `dryRun=All&fieldValidation=Strict`, so it
still checks strict server validation and admission without a schema download.
The offline plan reports this choice. There is no automatic fallback after a
validation error.

Helm 3 validates hook manifests independently, even with this option. A failed
hook schema download still stops deployment. Resolve schema access or populate
the client's genuine discovery cache before retrying; do not skip the
CetiAdmin hooks. The option never suppresses their installation checks.

Hook Jobs explicitly declare Helm release ownership because Helm does not add
it automatically for hooks. A failed hook retained from an older chart may
need a metadata-only ownership repair before retrying. Verify its UID and spec
against a trusted prior receipt and use UID/resourceVersion preconditions for
that repair. If its provenance cannot be established, stop for investigation.

Primary runtime images are governed by the published runtime chart's defaults
and qualified image list. Do not put `image`, `hermes.image`, or
`openclaw.image` in `cetirun.helmValues`; the Operator rejects those overrides.
Publish trusted runtime image pins in the chart, then select an exact approved
reference with `spec.agent.image` or `cetiadmin.runtime.image`. Helper image
pins such as `desktop.image.digest` remain separate settings.

Before a write, the command also examines these items:

- The configured admission CA and service DNS name
- The MCP/Broker server chains
- The Broker client's CA, client purpose, and exact MCP URI identity contract
  that already exists

The ingress certificate must be valid now and must cover `domain`. This test
is not a public-browser trust test. The helper extracts only the referenced
`tls.crt` and CA fields into temporary public certificate files. Kubernetes
still returns the whole Secret for a JSONPath request. The `get Secret`
permission is necessary for this request. The helper does not parse, save, or
log private-key, cookie, or provider-key values.

If an OpenSSL test fails, the command stops before any persistent cluster
write. It does not issue or rotate certificates.

The helper installs or upgrades CetiRun Istio and waits for OAuth2 Proxy. It
reconciles only the named `oauth2-proxy` mesh provider through the
resource-version-checked helper that already exists. A provider for another
service or a replaced ConfigMap stops the deployment. Set
`istio.mode: external` only when the named release and provider are already
managed elsewhere. That mode examines them and does not upgrade the release or
patch the mesh.

Next, the Broker and its Auth SDK start in the credential namespace. Then the
helper installs the platform. It waits for every Deployment and StatefulSet,
which includes PostgreSQL, Valkey, MCP, the admission server, and the
configured Skill Marketplace.

On a new installation, the helper starts the admission server before it
activates global Sandbox enforcement. CetiAdmin's dedicated fail-closed webhooks
are present during that bootstrap. The Operator withholds management grants
until its admission checks pass. An upgrade never disables the admission
enforcement that is already in effect.

AKS adds two system-namespace exclusions to webhook namespace selectors.
CetiAdmin accepts only those exact additions, and only with an independent
namespace admission policy. This policy does not let CetiAdmin's authority
namespaces and managed Agent namespaces use the excluded labels. The Operator
reads back that policy and confirms a real server-side dry-run denial before it
grants access.

Unrelated AKS namespaces keep their exclusions. Other namespace selectors or
object selectors are not accepted. The deployment does not disable the AKS
admission enforcer or rewrite labels on an excluded namespace.

At the end, a server-side dry-run of a Team creation examines the served
admission path. This dry-run does not store a Team. If a read, render, upgrade,
rollout, or dry-run fails, the helper returns a nonzero exit status. There is
no automatic rollback or ownership transfer.

An upgrade must keep the current admission administrators, Team assignments,
Pod and strict validation settings, coordination authority, and active native
execution controls. If a control value is unreadable or has an external
reference, the upgrade stops. Changes to these protections must use a separate
approved procedure. The helper does not migrate arbitrary stored Helm values.

The Operator's `connectionsMCP.runtimeTypes`, the Portal's
`agentTools.runtimeTypes`, and the Broker's `toolService.runtimeTypes` must name
the same runtimes. A consistent explicit `[hermes]` selection is still
supported. `custom` is an explicit opt-in and requires the native Connections
prerequisites: native execution and Pod admission, an approved Custom image,
the matching Policy and model access, and a registered protected Connections
MCP peer. Adding `custom` does not enable native Connections by itself.

For existing Hermes and OpenClaw Agents in a selected runtime, the helper extracts the
name, the runtime, the Team/Sandbox markers, and the necessary ServiceAccount
settings. It does not output owner details, unrelated Helm values, or
conversation data. The helper rejects an ordinary Agent if it depends on a
custom ServiceAccount, an API role, or an API token. The rejection occurs
before the MCP enablement can override those settings.

Ordinary Custom Agents do not activate Connections MCP. Their ServiceAccount,
API role, and token settings are not treated as an MCP migration conflict.

An explicitly pinned CetiAdmin migration source is exempt only after the helper
confirms four conditions. The conditions are its retained suspension, blocked
model key, stopped writers, and the removal of its direct ServiceAccount grants.

Policy-bound Sandbox Agents use the separate native path. This check does not
treat them as ordinary Agents. Kubernetes still returns complete objects for
these projections. The helper only parses the selected fields. The caller must
have permission to read these Agent, Team, Deployment, and webhook resources.
The helper does not grant itself more permissions.

### Configure CetiAdmin

Merge this subtree into the platform values of the environment file. These
identities, budget, and API address are synthetic examples. Replace them with
the environment's approved inputs before deployment.

```yaml
platform:
  values:
    cetirun-operator:
      cetiadmin:
        enabled: true
        instanceName: cetiadmin
        owner:
          alias: example-owner
          tenantID: "11111111-1111-1111-1111-111111111111"
          objectID: "88888888-8888-8888-8888-888888888888"
        platformReadNamespaces: [cetirun-system]
        kubernetes:
          apiServer: https://kubernetes.default.svc
          apiAudience: https://kubernetes.default.svc.cluster.local
          caConfigMapName: kube-root-ca.crt
          apiEgressCIDRs: [192.0.2.10/32]
        model:
          default: auto
          initialMonthlyBudgetUSD: "100"
          githubCopilot: false
        azureWorkloadIdentity:
          enabled: false
        sharedSkills:
          enabled: false
        monitor:
          enabled: false
          schedule: every 30m
          timezone: UTC
          deliver: local
```

`owner.alias` must equal the file's real `ownerAlias`, and `owner.tenantID` must
equal `identity.tenantId`. The owner account must already exist. Supply its
Entra object ID explicitly. The helper examines format and consistency. It does
not query the directory, create an account, or treat an email address as proof
of identity. Owner access still uses SSO claims that match the assigned
identity.

CetiAdmin automatically manages all ordinary Agents owned by this Operator,
regardless of their Team. There is no Agent or Team allowlist. The optional
`platformReadNamespaces` list adds read-only platform diagnostics. It does not
select managed Agents and is not passed to the runtime. If you do not want
diagnostics for other platform namespaces, omit it or use `[]`.

Native Custom Sandbox namespaces use their exact Agent owner reference and
Agent UID/name protection markers. They do not require a legacy Custom
ownership receipt. Legacy Custom namespaces still require their immutable
receipt. Both paths reject foreign ownership. Protected Sandbox namespace
access does not grant a CetiAdmin exec exception or Secret reads or writes.
Pod, log, and Event diagnostics remain available. Ordinary runtime namespace
Secret permissions are unchanged.

When Copilot is enabled, the configured Portal authoring identity can record
the owner's Copilot model list, sign-in generation, and sign-in timestamp on
the installed CetiAdmin Agent. This exception changes only those annotations.
The installation configuration, owner bindings, other metadata, and the
Agent specification remain protected. CetiAdmin cannot write its own sign-in
metadata through this exception.

New installation inputs and the deployment helper reject the removed `scope`
field. A disabled installation that already exists has a separate, bounded
upgrade path described below.

A diagnostic namespace that is missing, is in the `Terminating` phase, or is
temporarily unavailable does not stop CetiAdmin. The Operator reports the error
and retries the read-only binding of that namespace. It does not revoke
unrelated management access. Installation identity failures still stop
CetiAdmin. Disable and recovery paths also withdraw the diagnostic bindings, and
never adopt or delete a binding with foreign subjects.

CetiAdmin follows Hermes when an optional `skills.external_dirs` path does not
exist: it skips that path and keeps the available built-in skill. If the
built-in registration is missing or skill names conflict, the startup check
still fails.

The Operator and admission server resolve each target from current API objects,
and they examine its UID and ownership. They do not use a saved target inventory
or an access hash. Standard namespace RBAC still controls access. A new Agent
receives its namespace bindings, and CetiAdmin does not stop or restart.

CetiAdmin itself, platform components, credential stores, and the retained
migration source keep their protections. Protected Sandbox rules also still
apply. The maintenance handoff is still necessary for native Helm writes.

The budget is an explicit nonnegative decimal string with at most six fractional
digits. It initializes the model key for the new Agent UID. It does not reset
keys or historical consumption on an upgrade.

Supply the API audience that this cluster accepts. It must be different from the
Connections MCP audience. Also supply the single-host `/32` or `/128` API egress
addresses, and make sure that they are correct. Loopback, link-local, multicast,
and unspecified addresses are rejected. The API origin must be the configured
target or the in-cluster Kubernetes Service. Format checks do not prove that the
audience, network path, or owner is usable.

CetiAdmin uses the Operator image and the runtime chart of the same release. It
does not add an image, a chart, or an entry point for publication. Optional
`runtime.version`, `runtime.image`, and `runtime.port` must select that release
and the qualified Hermes profile, whose port is 8642. An explicit image is also
included in the normal artifact inspection. The Operator examines the profile
before it grants access.

Keep the current admission TLS Secret and CA inputs. CetiAdmin does not relax
them or enable native Sandbox execution.

#### Upgrade an existing scope-format installation

Use the images and charts of the same new release for both upgrades. The old
admission server cannot accept `platformReadNamespaces` before its version
changes. The old runtime chart also must have the old projection, so keep
CetiAdmin stopped during this configuration transition.

1. Keep the same owner, instance name, retained identities, and storage. Set
   `cetiadmin.enabled=false` and `cetiadmin.legacyDisabledSource=true`. Then run
   a normal upgrade.

   This chart-only flag emits the old wire shape with an explicit disabled
   value. The new Operator and hook accept it only for the same recorded source
   and Agent. Wait for Disabled, revoked access, and stopped writers. Also wait
   for the admission Deployment rollout and for old admission Pods and service
   endpoints to leave.
2. Set `legacyDisabledSource=false` and restore the intended `enabled` value.
   Run a second normal upgrade with the runtime chart and Hermes image of the
   new release. The final configuration uses the new shape. Wait for the
   post-upgrade check. Then make sure that the retained Agent UID and HOME did
   not change.

The compatibility flag defaults to false and cannot be used with
`enabled=true`. It is not a first-install option or an Agent selection mode.
Keep admission and hooks enabled throughout. Do not edit Helm release history.
If the installation was user-paused, keep it paused. Do not resume it.

Enable `sharedSkills.enabled` to use the top-level `sharedSkills` storage
configuration. The built-in operations skill stays available when shared skills
are disabled. For optional Workload Identity, set
`azureWorkloadIdentity.clientId` to a managed identity that already exists. That
identity must have a federation for the new recorded ServiceAccount. CetiAdmin
does not reuse the Operator identity automatically.

`monitor.deliver` accepts only `local` or `slack`. For Slack, set an exact
channel in `monitor.slackChannel`. Also set `monitor.slackSecretName` to a
Secret in `cetirun-<instanceName>` with the `SLACK_BOT_TOKEN` and
`SLACK_APP_TOKEN` keys. The helper examines key names, not values.

For a new namespace, install with `monitor.enabled: false`. After the Operator
creates the namespace, provision the Secret through the approved workflow. Then
set `monitor.enabled: true`. No Teams settings or credentials are accepted or
created.

If an OpenClaw administrator already exists, keep its original Agent and HOME.
Before the migration, complete the approved backup and restoration checks. Then
stop its monitor and writers, resolve maintenance, and revoke its old
administrative bindings. Use a distinct new instance name and add both exact
source identities:

```yaml
instanceName: cetiadmin-hermes
migrationSource:
  agent:
    name: old-operations
    uid: REPLACE_WITH_OBSERVED_AGENT_UID
  namespace:
    name: cetirun-old-operations
    uid: REPLACE_WITH_OBSERVED_NAMESPACE_UID
```

These fields belong inside `cetiadmin`. Neither a familiar name nor a label is
an adoption receipt. Preflight examines the supplied UIDs and the stopped
source. The Operator keeps the migration identities and excludes the old
namespace from automatic administration.

No OpenClaw database, native credentials, HOME, or RBAC is copied. If the pinned
source Agent or namespace is replaced, deployment stops. The helper does not
create backups or establish that a backup can be restored.

### Disable CetiAdmin before removing the Operator

For normal CetiAdmin shutdown, keep `cetirun-operator.enabled: true` and change
only `cetiadmin.enabled` to `false` in the same environment file. Run the normal
plan and authorized deployment. The retained source and protection stay in
place while the Operator revokes access and stops the Agent. The Helm check
waits for the current disabled configuration to be confirmed. Keep the other
CetiAdmin fields and its original UID records for recovery. A fresh installation
can use only `cetiadmin: {enabled: false}`, so that the Agent is not created.

If a separately approved change removes the Operator subchart, first complete
that shutdown. Then explicitly set `cetirun-operator.enabled: false` and
`global.portalChat.enabled: false` under `platform.values`. Keep the same
CetiAdmin configuration and release coordinates. Do not combine removal with
changes to the owner, `platformReadNamespaces`, runtime coordinates, or migration
source.

The helper examines the stopped installation before any write and again
immediately before the platform Helm upgrade. It must have the exact retained
source UID and the current configuration hash. The installation checkpoint must
be in `Disabled`, with no pending creation or access grant. The helper also
examines the current Operator image, source selection, and rollout.

It also examines the bound Agent, namespace, and ServiceAccount identities.
Direct RBAC grants must not exist. The helper examines the current Agent
maintenance markers and namespace startup-block markers. Pods, StatefulSets,
Deployments, ReplicaSets, DaemonSets, Jobs, and CronJobs must be stopped. An
unavailable Operator, missing record, changed source, unresolved maintenance, or
failed read prevents removal. The helper does not accept an umbrella upgrade as
a pre-delete hook receipt.

The deployer must have read access to those metadata, ConfigMap, RBAC, and
workload fields, and to the current Agent and namespace lists. Maintenance
checks do not depend on a retained target inventory, and they include orphaned
namespace markers. Legacy authorization inventories are not accepted as stop
evidence. The Operator must first reconcile its checkpoint to the current
contract. No new permissions are granted.

Independent admission, configuration coordination, and native execution controls
still block Operator removal until their separately approved handover is
complete. A normal full installation has such admission protection. CetiAdmin
shutdown alone does not authorize its removal. The helper never disables that
protection automatically.

An offline plan reports removal intent with `stopConfirmed: false`. Only a live
preflight in an authorized deployment can establish the current stop state.

### Preserve certificates and retained data

`istio.tlsMode: external` uses the `cetirun-tls` Secret that is already in the
ingress namespace. If the Istio release owns that Secret, this mode fails
before writes. The reason is that when `tls.enabled` is disabled, the release's
certificate is deleted.

If a chart-owned certificate already exists, set `istio.tlsMode: retain`. For
this mode, the release must already exist, and the Secret ownership must match.
The helper omits `tls` from the actual override file. It uses `--reuse-values`
for that Istio upgrade. Helm keeps the prior TLS values, and private-key
material does not go into the helper's override file. To change the ownership
mode, you must do a separate migration.

The offline render uses synthetic cookie/TLS values only in a separate
temporary render file. The helper never applies that manifest. Actual upgrades
use the normal charts and their server-side Secret lookup. If the upgrade
cannot find a cookie or database Secret to keep, it fails before writes. It
does not replace that Secret with new keys.

Agents, HOME/PVCs, and user keys that already exist stay in place. If you run
the normal deployment again, it reconciles the same releases. It does not disable
admission enforcement. Use a separately authorized environment test to examine
the actual cookie, certificate, and database continuity.

### Deployment readiness is not runtime qualification

Broker/MCP readiness endpoints do not show that the provider or Auth SDK APIs
are available. The helper examines the workload rollouts, the necessary
containers and Istio sidecars, the wired references, and the admission
dry-run. It does not run an OAuth sign-in, a provider token exchange, a real
model call, or a terminal session. The Gateway's own health probes use its
configured model settings. After startup, these probes can make model calls.

Native Sandbox execution and physical writer fencing are separate
qualification work. The default does not enable `sandboxSources`, and it does
not create capability registrations. A native configuration that already
exists must keep its real qualification inputs.

CetiAdmin's post-install and post-upgrade checks cover
its current installation state. They do not prove end-to-end operations,
model access, Slack delivery, backup restoration, or native runtime behavior.
Other ordinary Agents with a ServiceAccount conflict still stop deployment
before writes. The helper does not silently remove OpenClaw from the allowlist.
Arbitrary instance creation requests in this file are rejected.

## Use the component chart directly

Direct chart consumers can optionally enable CetiRun Connections MCP. Install
the Agent Broker separately in its private credential namespace. The umbrella
rejects an enabled Broker that is colocated with the Tool service. Provision
TLS and OAuth separately. Then configure these items:

- The MCP Broker HTTPS origin
- The Portal and Operator management HTTP origins
- The Operator Secret that contains only the MCP URL and CA

The sample `app/cetirun-connections-mcp/chart/ci/render-values.yaml` contains
synthetic render-only configuration. It does not create certificates or
authorize deployment.

The bundled Operator chart manages the four CetiRun CRDs as Helm templates. It
also manages their schema upgrades. Use values that explicitly confirm the
Istio prerequisites that are installed separately. Supply `cetirun-operator.cetiadmin`
and the shared admission TLS inputs in an umbrella values file, not the
deployment environment file. For a fresh installation without CetiAdmin, set
`cetirun-operator.cetiadmin.enabled: false` explicitly in that values file.

After deployment is authorized, use that file with the chart:

```bash
ISTIO_REVISION="$(kubectl get pods -n aks-istio-system -l app=istiod \
  -o jsonpath='{.items[0].metadata.labels.istio\.io/rev}')"
test -n "${ISTIO_REVISION}"
kubectl get namespace cetirun-system >/dev/null 2>&1 ||
  kubectl create namespace cetirun-system
kubectl label namespace cetirun-system "istio.io/rev=${ISTIO_REVISION}" --overwrite

helm dependency build app/cetirun-platform
helm upgrade --install cetirun-platform app/cetirun-platform \
  --namespace cetirun-system \
  --timeout 15m \
  --values /path/to/platform-values.yaml \
  --set istio.enabled=true \
  --set-string cetirun-operator.domainSuffix=cetirun.example.com \
  --set-string "cetirun-operator.istioRevision=${ISTIO_REVISION}" \
  --set-string cetirun-operator.istioGateway=aks-istio-ingress/cetirun-gateway \
  --set-string cetirun-operator.cetirun.helmValues.networkPolicy.istioIngressNamespace=aks-istio-ingress \
  --set cetirun-portal.istio.host=cetirun.example.com \
  --set cetirun-portal.istio.gateway=aks-istio-ingress/cetirun-gateway \
  --set cetirun-portal.istio.gatewayNamespace=aks-istio-ingress \
  --set cetirun-portal.istio.gatewayServiceAccount=aks-istio-ingressgateway-external

kubectl rollout status deployment/cetirun-portal -n cetirun-system --timeout=180s
kubectl get pods -n cetirun-system -l app.kubernetes.io/name=cetirun-portal \
  -o jsonpath='{range .items[*]}{.spec.containers[*].name} {.spec.initContainers[*].name}{"\n"}{end}' | grep -qw istio-proxy
```

The final command makes sure that the Portal has the Istio sidecar that is
necessary for its authorization policies. The command also lists init
containers because Istio can inject the sidecar as a native sidecar. A native
sidecar is an init container that continues to run. The AKS Istio add-on
injects the sidecar in this way.

If you use a custom gateway, set `cetirun-operator.istioGateway` to the same
`<namespace>/<name>` as `cetirun-portal.istio.gateway`.
`cetirun-portal.istio.gatewayNamespace` and `gatewayServiceAccount` identify
the selected gateway workload. This workload can run in a namespace other than
the namespace of its Gateway resource. Also set
`cetirun-operator.cetirun.helmValues.networkPolicy.istioIngressNamespace` to
the namespace of that gateway workload. If the value is not that namespace,
the instance NetworkPolicies block ingress traffic.

Helm upgrades the CRD templates with this release. For compatible changes, a
separate CRD apply script is not necessary. The
`helm.sh/resource-policy: keep` annotation keeps CRDs and their objects during
an uninstall. It does not protect against an incompatible schema change or
downgrade.

By default, the Operator chart sets `sandboxAdmission.validationMode` to
`compatible`. This mode keeps known validation and authorization checks and
accepts newer CR fields. Refer to the
[schema and binary upgrade contract](../cetirun-operator/README.md#schema-and-binary-upgrades).
For CRDs that were previously unmanaged, and for ownership transfers across
releases, a separately reviewed handover is still necessary. This chart does
not forcibly adopt them.

For a different Gateway resource, set `cetirun-operator.istioGateway` and
`cetirun-portal.istio.gateway`. For its workload identity, set
`cetirun-portal.istio.gatewayNamespace` and `gatewayServiceAccount`, as the
command above shows. `make deploy` sets these values from its `istio` inputs:
the Gateway `cetirun-gateway` in `istio.ingressNamespace`, and
`istio.ingressServiceAccount`.

To install in a namespace other than `cetirun-system`, set both
`cetirun-operator.namespace` and `cetirun-portal.aiGateway.catalogNamespace` to
that namespace. The operator publishes the shared models in its own namespace,
and the Portal reads them from there. `make deploy` sets both values from
`platform.namespace`. The chart refuses a Portal that looks for the models in a
different namespace.

When you use the umbrella, configure the separate Istio release to reach the
co-located Portal before you install it:

```bash
PORTAL_AUTH_NAMESPACE=cetirun-system ./infra/istio/deploy.sh
```

You can still install each component chart independently. A separately planned
handover is necessary to move standalone releases that already exist into this
umbrella. The chart never adopts resources that another release owns.

## Sandbox creation profiles

Configure `cetirun-portal.sandboxRuntimeProfiles` for each supported built-in
Sandbox runtime. Each entry contains `chartVersion`, a digest-pinned `image`,
and `port` (18789 for OpenClaw, 8642 for Hermes). The chart version must match
the Operator's configured runtime chart. That chart's profile must approve the
image and port. Configure only the harnesses that the selected platform
supports. The common native executor supports OpenClaw and Hermes, but retained
single-start records are for OpenClaw only.

The Portal records these inputs in the initial Agent creation request. As a
result, users do not enter deployment coordinates in the creation form.
Admission still applies to explicit image or port requests. By default,
deployment profiles that do not exist or are not valid reject creation before
any Agent, namespace or Secret is written.

With the Operator's `sandboxSources.nativeExecution` enabled, the deployment
can set `cetirun-portal.sandboxRuntimeDefaultsFromController: true`. This lets
profiles that do not exist use trusted chart, image, and port defaults in the
common executor. Configured profiles are still validated. The platform
chart rejects this Portal opt-in unless the Operator and its native sources are
enabled.

For standalone Portal releases, make sure that the Operator configuration
matches before you enable the opt-in. For these releases, disable the opt-in
before you disable native execution. These defaults do not authorize a runtime
or change the selected AgentPolicy/Sandbox checks.

### Sandbox HOME StorageClass choices

The Portal's Sandbox form obtains HOME StorageClass choices from the exact
operator-namespace ConfigMap selected by
`cetirun-operator.sandboxSources.platformConfigMapName`. The platform deploy
command wires this reference automatically when Sandbox sources are enabled.
It also grants the Portal read access to that one ConfigMap and to individual
StorageClasses; it does not grant list, watch, or storage mutation access.

For a standalone Portal chart, configure the same source explicitly:

```yaml
sandboxStorage:
  platformNamespace: cetirun-system
  platformConfigMapName: sandbox-platform
  # Optional UI preference. It must still be approved and current.
  defaultStorageClassName: managed-csi
```

For a direct `cetirun-platform` Helm release, set the same values beneath
`cetirun-portal.sandboxStorage` and keep them equal to the bundled Operator's
namespace and `sandboxSources.platformConfigMapName`. The umbrella chart
rejects a partial or mismatched reference. `make deploy` derives those two
fields from the Operator source, so its environment file needs only an optional
`defaultStorageClassName` preference.

The form shows only classes whose current UID and provisioner still match the
platform's approved pin. It shows a retryable error instead of accepting an
unknown list when the source cannot be read. An existing Sandbox whose class is
no longer selectable keeps its recorded value but requires an administrator to
choose a current approved class before saving. HOME access-mode choices come
from the selected runtime registration, not from assumptions about a CSI
driver. This selection aid does not qualify CSI behavior or replace Sandbox
admission and controller checks at save and execution time.

When `cetirun-operator.sandboxAdmission` is enabled, the chart adds the Portal's
service account, `system:serviceaccount:<release namespace>:cetirun-portal`, to
the admission administrators. The Portal writes Agents, Teams, Sandboxes and
AgentPolicies with this account. If the account is not an admission
administrator, the enforced admission webhook refuses its writes to
policy-bound Agents. For example, the webhook refuses the write that saves an
owner's GitHub Copilot models after a sign-in.

`cetirun-operator.sandboxAdmission.portalServiceAccount` identifies the
account. By default, the account is `cetirun-portal`. An installation that
already lists the account in `adminUsers` keeps the same admission
configuration. The Portal service account does not count as the administrator
that `adminUsers` or `adminGroups` must name. For example, reauthorization has
no Portal page.

- If you use a custom `cetirun-portal.serviceAccount.name`, set
  `portalServiceAccount` to the same name. The chart refuses different names.
- If you disable the bundled Portal, clear `portalServiceAccount`. If you do
  not clear it, the chart refuses it. List a separately installed Portal's
  service account in `adminUsers`.
- An upgrade with `--reuse-values` from an older release does not set
  `portalServiceAccount`. Set it, or list the Portal's service account in
  `adminUsers`. If you do neither, the chart refuses enforced admission.

Administrator groups cannot be blanket groups, for example
`system:authenticated` or `system:serviceaccounts:<namespace>`. The admission
server refuses to start with these groups. As a result, the chart also refuses
them.

## Skill Marketplace

The Operator runs the optional Skill Marketplace. A worker syncs one Git
repository of skills, and publishes the skills that each policy-bound Team
selects to a Blob NFS container. The Operator gives each Sandbox Agent of
these Teams that the common executor runs, built-in or Custom, a read-only
claim of the container. The marketplace is off by
default. To enable it, set `cetirun-operator.skillMarketplace` and enable
`cetirun-portal.skillMarketplace.enabled`. The Operator chart accepts the
marketplace only with `sandboxSources.enabled` and `nativeExecution`, with a
`sandboxSources.skillsClaimName`, and without
`sandboxSources.skillsCatalogConfigMapName`.

The worker is the Deployment `cetirun-skill-marketplace` in the operator
namespace. It runs the operator image, and it can only read the Teams document
of the Operator and update the catalog. The platform chart refuses these
settings:

- The marketplace of the bundled Operator is on, and the setting of the bundled
  Portal is off.
- The Portal setting is on, and the bundled Operator or its marketplace is off.
- The Portal's `skillMarketplace.catalogNamespace` is not
  `cetirun-operator.namespace`.

`make deploy` sets these values from the optional `skillMarketplace` block of
the environment file:

```yaml
skillMarketplace:
  repository:
    url: https://git.example.com/example/skills.git
    ref: main                # Optional. Empty uses the remote HEAD.
    path: skills             # Optional. Empty is the repository root.
    auth: token              # none, token, or ssh
    existingSecret: skills-repository
    syncInterval: 5m         # Optional. At least 30s.
  storage:
    resourceGroup: example-rg
    storageAccount: examplemarketplace
    containerName: marketplace
    storageClassName: azureblob-nfs-premium
```

With `auth: ssh`, use an `ssh://` URL or the `user@host:path` form. The URL
must not contain credentials. For `auth: token`, the Secret in
`platform.namespace` has the key `token`. For `auth: ssh`, it has the keys
`ssh-privatekey` and `known_hosts`. Before its first write, the command makes
sure that these keys and the StorageClass exist.

The command refuses the block if the native Sandbox sources are off. It also
refuses it if the Operator uses a published skills catalog, or if the storage
is the `sharedSkills` container. Set other values of the marketplace, for
example `repository.tokenUsername` or `limits`, under
`platform.values.cetirun-operator.skillMarketplace`.

On a new installation, the bootstrap upgrade does not install the marketplace.
The command waits for the rollout of the worker after the final platform
upgrade. A ready worker has not necessarily synchronized the repository. The
catalog ConfigMap `cetirun-skill-marketplace` in the operator namespace shows
the synchronized commit and any sync error.

NFS 3.0 authorizes clients only by network. Use a storage account that only
private endpoints can reach, and turn off its public network access. Refer to
[Storage](../../site/content/en/docs/operations/storage.md) and
[Skill Marketplace](../../site/content/en/docs/operations/skill-marketplace.md).

## AI Gateway

Because OpenClaw and Hermes Agents take their models from the AI Gateway, the
AI Gateway is necessary for the platform. The chart refuses to install with
`cetirun-operator.aiGateway.enabled` or
`cetirun-portal.features.aiGatewayEnabled` disabled. Hermes Agents pinned to a
chart version without Hermes' Gateway provider keep their own model provider
and sign-in until their chart is upgraded.

By default, the release also installs the Gateway with its PostgreSQL and
Valkey in the release namespace. The name of the Gateway is
`cetirun-ai-gateway`. The release namespace must be the operator's namespace
because the operator reads the Gateway master key from there. Refer to the
[Gateway chart](../cetirun-ai-gateway/chart/README.md) for its settings, which
are under `cetirun-ai-gateway` in this chart.

Shared models are the models set in `cetirun-ai-gateway.models`. The Gateway
chart gives each model a deployment ID and an access group derived from its
name. The operator offers each model through its deployment ID and access
group. It offers every model to Agents without a policy, and it offers
policy-bound Agents the models that their AgentPolicy grants. If the Gateway
blocks models that do not have a price, as it does by default, each model must
also have a price.

In this example, the provider key is in a Secret that you create in the release
namespace. With `make deploy`, put the same entries under
`platform.values.cetirun-ai-gateway` in the environment file. `make deploy`
also requires the `auto` and `decision-1` entries in its default routing mode.
With explicit `modelRouting.mode: direct`, supply callable models without
those entries. Refer to [Deploy the platform](#deploy-the-platform):

```yaml
cetirun-ai-gateway:
  providerCredentials:
    existingSecret: gateway-provider-keys  # holds AZURE_OPENAI_KEY
  models:
    - model_name: gpt-4o
      litellm_params:
        model: azure/my-gpt-4o-deployment
        api_base: https://example.openai.azure.com/
        api_key: os.environ/AZURE_OPENAI_KEY
        # Must support the Responses API: agents' tool calls are sent there.
        api_version: "2025-04-01-preview"
      model_info:
        # The price; or set input_cost_per_token and output_cost_per_token.
        base_model: azure/gpt-4o
```

The access group `cetirun-shared` is no longer necessary for these models. A
model that has it continues to work. When the settings of a model change, for
example its endpoint, provider parameters or price, the model keeps its access
group. As a result, Agents continue to use the model and are not restarted for
this change.

Also, an upgrade of the Gateway does not interrupt the Agents. The Gateway pods
keep the configuration that they started with until they are replaced. The
operator applies a changed model list after the rollout ends.

Shared models are optional for a direct install of this chart. `make deploy`
must have them. Its default routing mode requires `auto`; explicit
`modelRouting.mode: direct` requires callable shared models without a classifier. Without shared models, OpenClaw
Agents use their owners' GitHub Copilot. With
`cetirun-operator.aiGateway.copilotProxy.enabled` disabled, the Portal creates
no OpenClaw Agent until a shared model is offered.
The Gateway's health checks do not probe the Agents' GitHub Copilot routes
because each probe is a real call on the owner's Copilot allowance.
`cetirun-operator.aiGateway.copilotProxy.healthChecks.enabled` enables their
probes.

A model added in the console is shared only if it has the access group
`cetirun-shared` and a price. It is shared only with Agents without a policy.
To open the console, use these commands:

```bash
kubectl get secret cetirun-ai-gateway-auth -n cetirun-system \
  -o jsonpath='{.data.ui-password}' | base64 -d; echo
kubectl port-forward -n cetirun-system svc/cetirun-ai-gateway 4000:4000
```

Then open <http://localhost:4000/ui> and sign in as `admin`.

You can use a separately installed Gateway instead, but not with `make deploy`,
which installs the bundled one. To do this, set
`cetirun-ai-gateway.enabled=false`. Install the Gateway in the operator
namespace under the name `cetirun-ai-gateway`, with
`operatorIntegration: true`. If you install the Gateway elsewhere, set
`cetirun-operator.aiGateway.url` to the URL of that Gateway. In this case, also
copy its master key into the operator namespace as the
`cetirun-ai-gateway-auth` Secret.

A Team is optional for a new Agent. Without a Team, the Agent can use every
shared model. Policy-bound Teams are created in the Portal's administration
pages. A Team without a policy can also be created with a manifest:

```bash
kubectl apply -f - <<'EOF'
apiVersion: cetirun.microsoft.com/v1alpha1
kind: Team
metadata:
  name: example-team
spec: {}
EOF
```

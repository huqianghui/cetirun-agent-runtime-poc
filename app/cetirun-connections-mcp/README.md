# CetiRun Connections MCP

CetiRun Connections MCP is a stateless Streamable HTTP MCP service. It executes
JSON REST requests through a workload-bound Connection. The service validates
the Agent workload that calls it. It asks the Agent Identity Broker to examine
the Agent's enabled connection binding. It gets a short-lived token, executes
the fixed-service REST request, and returns the provider result. The credential
never enters Agent context, logs, files, command arguments, or tool results.

Services include Azure Resource Manager, GitHub, Microsoft Graph, Gmail and
Google Calendar. Provider RBAC, OAuth permissions, and GitHub App installation
permissions control resource access. GitHub App bindings include explicit
repositories and permissions. The Broker limits each installation token to that
grant. MCP examines this metadata across authorization and issuance. It does
not keep a permission catalog for REST operations.

## Tools and request contract

- `connections_status`: enabled bindings, account or principal, and status,
  but never credentials.
- `connections_start_link`: configured Portal URL for account and Agent
  connection management.
- `services_list`: enabled services and their fixed origin/base path.
- `service_request`: execute one JSON REST request.

```json
{
  "connection_id": "example-connection",
  "service": "azure_arm",
  "method": "POST",
  "path": "/providers/Microsoft.ResourceGraph/resources",
  "query": {"api-version": "2022-10-01"},
  "body": {"query": "Resources | project id, name | take 10"}
}
```

`azure_arm` always targets `https://management.azure.com/`. Paths begin with
`/` and are relative to that base path. Query values are strings or arrays of
strings. The optional headers are `If-Match`, `If-None-Match`, `Prefer`,
`ConsistencyLevel`, and `X-GitHub-Api-Version`.

The JSON body is limited to 256 KiB. GET and HEAD bodies are rejected. The
methods are GET, HEAD, POST, PUT, PATCH, DELETE, and OPTIONS. The HTTP method
does not classify permission.

Results contain the provider `status`, `body`, filtered `headers`,
`request_ids`, and a CetiRun `correlation_id`. Provider 401/403 responses stay
provider results. Workload and binding failures are separate tool errors.
External data is untrusted.

## Execution and trust boundaries

The service rejects traversal, absolute or scheme-relative URLs, redirects,
proxy headers, cookies, authentication overrides, unsafe DNS answers and
oversized requests or responses. Its transport validates public DNS results and
connects to the validated address. It verifies TLS and disables proxies,
HTTP/2, and connection reuse. It never retries automatically. Every provider
call has a 20-second timeout. Each MCP call has a 30-second deadline.

`execution_unknown` means that an effect possibly occurred already. This also
applies when the response cannot be read safely. Before another call, examine
the provider state. A correlation ID supports tracing only. It is not an
idempotency key.

MCP runs over HTTPS `/mcp`. Native clients present a Pod-bound proof that
rotates. Its audience is `cetirun-connections-mcp`. MCP validates the live Pod,
ServiceAccount, namespace Agent owner reference and Agent state. The Broker
repeats this validation and independently examines the owner-scoped connection
and Agent binding. Before MCP sends a request, it makes sure that the Broker
check and issuance responses agree on connection and binding revisions.

For governed Agent Identity acquisition, these responses must also agree on
these items:

- the current credential-policy revision that is read from the authenticated
  Agent
- the enforcement record UID/resource version

A recreated identical record does not reuse an earlier check response. The
Broker independently validates an owned enforcement record against the live
Agent, Team and AgentPolicy before and after credential acquisition. If the
evidence is absent, changed, or unreadable, execution stops. An ARM-only policy
cannot be used for Graph OBO. This does not create a Connection or authorize a
provider operation.

For governed native workloads, the current common-executor proof described
below is also necessary. A stored policy alone is not sufficient. See the
[credential policy contract](../cetirun-agent-broker/README.md#credential-policy-execution-and-readback).

When the proof is invalid, or its live binding is absent, changed, or inactive,
workload authentication failures return HTTP 401 with `unauthenticated`.
Kubernetes API timeouts, failed API requests, invalid responses, and
unavailable server credentials return HTTP 503 with `dependency_unavailable`.
These failures stop the request before Broker or provider execution. For these
failures, it is not necessary to reconnect the Agent account. Failed
notifications use the same HTTP status without a JSON-RPC body.

Authenticated `ping` requests return `result: {}` with the same request ID.
They use the same workload proof and protocol-version validation. They do not
contact the Broker or Azure.

For the chart, an MCP server TLS Secret (`tls.crt`, `tls.key`) and a dedicated
Broker client TLS Secret (`tls.crt`, `tls.key`, `ca.crt`) are necessary. The
server certificate must cover the MCP service hostname that clients use. The
Broker client certificate must carry the URI
`spiffe://cluster.local/ns/<mcp-namespace>/sa/<mcp-serviceaccount>`. The Broker
must trust the CA of this certificate, and the `ca.crt` of this certificate
must trust the Broker server. Configure the Broker's `toolService` identity and
workload audience to match this chart. For this configuration, follow the
[Broker prerequisites](../cetirun-agent-broker/README.md#internal-credential-issuance).

Mount whole Secrets, without `subPath`, so that valid certificate and CA
replacements can reload for new connections. Invalid replacements keep the last
valid material. Established connections and issued provider tokens are not
revoked immediately. MCP can create TokenReviews and read named workload
identity objects. It has no Secret API access. Its release namespace must be
different from the Broker namespace.

Audit records identify the Agent, connection and binding revisions, Azure
principal, request method, target path, duration, outcome and Azure request
IDs. They exclude query strings, request and response bodies, supplied headers
and credentials. The injected bearer token is never returned. This also applies
when a provider echoes it.

Azure APIs can still return other sensitive resource data, such as access keys.
This can occur when Azure RBAC allows that action. When you assign Azure roles,
consider this possibility.

## Deployment configuration

Before you enable this service, follow the
[Agent Identity setup guide](../../site/content/en/docs/operations/agent-identity.md).
It gives the complete Broker/MCP certificate and trust layout, platform values,
and provider preparation.

The image, standalone Helm chart, default ServiceAccount, and workload token
audience use `cetirun-connections-mcp`. The platform umbrella chart enables this
component through `cetirun-connections-mcp.enabled`.

Configure the Operator and runtime charts through `connectionsMCP`. The Operator
reads `CONNECTIONS_MCP_*` environment variables. Native OpenClaw and Hermes
clients use `CETIRUN_CONNECTIONS_MCP_URL` and the `cetirun-connections` MCP
server entry. Workloads mount their proof at
`/var/run/cetirun/connections/token` and their MCP CA at
`/var/run/cetirun/connections-ca/ca.crt`. The proof rotates.

Deploy the renamed MCP service, Broker configuration, Operator, and runtime
assets together. Configure the server certificate for the new service DNS name.
Configure the Broker client certificate for the SPIFFE URI that matches the
ServiceAccount. Recreate Agent workloads with the new configuration, projected
token audience, and mount paths. Previous component and configuration names
have no aliases. It is not necessary to migrate connection records and provider
authorization grants.

### Governed native workloads

The common OpenClaw/Hermes executor has an explicit Connections opt-in:

- Operator: enable `sandboxSources.nativeExecution` and
  `connectionsMCP.nativeEnabled` together with the current Connections URL,
  purpose audience, CA and Broker namespace/ServiceAccount configuration.
- Connections chart: enable `nativeSandbox.enabled`, set
  `nativeSandbox.operatorNamespace`, and supply `nativeSandbox.peerUID`.
  Register the same protected managed-peer identity, namespace, Service name
  and Service IP inventory in the Operator's current Sandbox platform source.
- Keep the fixed Sandbox Pod-admission webhook and the current CA of its server
  certificate available. Broker and MCP receive only named read access to
  `cetirun-sandbox-pods`. For MCP, its normal workload reads and TokenReview
  permission are also necessary. They receive no Pod write permission.
  Admission TLS verifies `cetirun-sandbox-admission.<operatorNamespace>.svc`
  against that CA. A certificate that contains only this documented Service SAN
  is sufficient.

The Operator derives the necessary TCP destination from the current registered
MCP Service. The destination includes the numeric backend port of the Service.
Deny rules still have priority.

The current/default workload ServiceAccount already has UID/version pins in
common-executor Sources. This path does not create a dedicated ServiceAccount,
and it does not depend on generic Entra projection approval. Only the primary
container mounts the 600-second, purpose-bound Connections proof. It is not an
`api://AzureADTokenExchange` token.

Both MCP and Broker independently do these steps:

- They TokenReview that proof.
- They reread the current Running/Ready Pod, namespace, ServiceAccount and
  Ready Agent.
- They ask the Operator Pod-admission validator that already exists to
  validate an identical-object, dry-run UPDATE of the API-read Pod.

The request cannot select a webhook, namespace or alternate validation path.
Discovery uses the fixed Kubernetes webhook configuration and its current CA.
No Pod is written. This validator examines current source
UIDs/versions/digests, Agent binding, rendered workloads/controls and
reservation. Then the consumer rereads identities and webhook trust. If the
authority is recreated, suspended, held, changed, or unavailable, or if its
deletion is in progress, it fails closed.

Native Pods do not carry an Istio sidecar. With this opt-in, port 8443 on MCP
uses its current **application HTTPS and Pod-proof authentication**. It does not
add an outer Istio mTLS layer. The chart limits ingress by Agent namespace and
registers the protected peer selector.

A Service-scoped DestinationRule also makes sure that ordinary meshed Agents do
not add an incompatible outer mTLS layer. Their application HTTPS and proof are
still necessary. MCP-to-Broker direct mTLS is still necessary. Other MCP ports
keep the current mesh restrictions. The default non-native chart configuration
stays strict.

Credential-policy configuration readback can run before aggregate Agent Ready.
It never allows credentials to be served before workload readiness. Current
policy evidence can then complete `PolicyApplied`.

For independent GitHub/Google metadata and credentials, it is not necessary to
have Broker Agent Identity or ARM policy records. But unavailable *workload
authority* rejects all workload-authenticated requests. This also applies to
metadata. For owner-side inventory and exact owned cleanup, a native Pod that
still runs is not necessary.

### Governed Custom workloads

Custom uses the same native validation, not a type-only bypass. It stays off by
default. To enable it, add `custom` to the Operator
`connectionsMCP.runtimeTypes`, Broker `toolService.runtimeTypes`, and Portal
`agentTools.runtimeTypes` lists. Also enable the native settings above. Custom
without a Sandbox stays unsupported. Source or workload outages still reject
execution.

The Custom image implements the MCP client contract with
`CETIRUN_CONNECTIONS_MCP_URL`, `CETIRUN_CONNECTIONS_MCP_TOKEN_FILE` and
`CETIRUN_CONNECTIONS_MCP_CA_FILE`. The last two refer to the mounted proof and
CA paths above. Read both files on each new request. Keep certificate
verification. Do not retry an unknown effect.

Do not make application startup/readiness depend on a successful provider call.
Before credentials are served, the Agent and current Pod must first be ready.
The [Pi adapter](../../examples/pi-agent/connections-runtime.mjs) initializes
lazily and exposes the four fixed tools to the real SDK.

For Custom Agent Identity, the directory configuration that already exists,
policy permission, and current credential-policy readback are also necessary.
Generic Entra token projection is independent. For GitHub/Google connections,
neither form of Entra identity is necessary. Explicit bindings and provider
permissions are still mandatory. No provider token is projected into any Agent.

## Offline checks

```bash
go test ./... -count=1
go test -race ./...
go vet ./...
make chart-lint
```

These checks use synthetic Azure and Kubernetes fixtures. They do not qualify a
live Azure tenant, cluster, Portal UI or dataplane. The Operator also has a
local API-server/common-executor/served-admission test that is documented in
[DEVELOPMENT.md](../../DEVELOPMENT.md). Scheduler, kubelet, directory and
provider observations stay synthetic. Live Kata/Cilium, Service routing, TLS
delivery and provider/federation qualification are separate prerequisites.

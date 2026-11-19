# CetiRun Agent Connection Broker

The Broker manages owner-scoped Connections and explicit per-Agent connection
bindings. Azure RBAC and provider permissions are the authorities for resource
authorization. GitHub App bindings limit installation tokens to selected
repositories and permissions. There are no business-level AgentGrants and no
per-tool permissions. There is also no prerequisite check of Azure RBAC.

We recommend that platform administrators start with the
[Agent Identity setup guide](../../site/content/en/docs/operations/agent-identity.md)
for encryption, federation, certificates, provider registrations, and
deployment order. This README describes the component contracts after those
prerequisites.

A binding associates an immutable Agent UID with a Connection and selected
services. New associations are always enabled. Agents with the same owner do
not share connections automatically. Every Agent must have an explicit binding.
Services are defined with fixed origins and audiences in
`app/internal/agenttools`, which is shared with MCP.

## Management API

For Portal APIs, the verified Portal mesh identity and the owner tenant/object
headers are necessary. Agent-scoped operations also must have
`X-CetiRun-Instance-UID` and must match the live Agent owner. API views do not
contain provider credentials.

- `GET /v1/connections`
- `POST /v1/connections/start`
- `GET /v1/connections/github-app/config`
- `GET /v1/connections/{id}/github/installations`
- `GET /v1/connections/{id}/github/repositories?installationId=123&page=1`
- `POST /v1/connections/{id}/github/installation`
- `GET|DELETE /v1/connections/{id}`
- `POST /v1/connections/{id}/reconnect`
- `GET /v1/connection-authorizations/{id}` (owner-scoped OAuth result)
- `GET /v1/connections/{id}/usage` (associated Agents and `canRemove`)
- `POST /v1/instances/{name}/tool-identities` (independent Azure connection)
- `GET|POST /v1/instances/{name}/connection-bindings`
- `PUT|DELETE /v1/instances/{name}/connection-bindings/{id}`

Binding input is `connectionId`, `services`, and `enabled: true`. PUT includes
`revision`, and DELETE must have `If-Match`. A deterministic Agent/Connection
record key and a Kubernetes resourceVersion CAS prevent duplicate binding races
and lost updates.

Revocation keeps a tombstone, so that re-creation cannot reuse an issued
revision. Disconnect revokes the binding and blocks future issuance. New
disabled writes are rejected. For historical disabled records, an explicit
reconnection is necessary. Requests that were already sent to providers cannot
be recalled. Provider-side revocation stays external.

For a GitHub App connection, the start request also supplies
`mode: installation` with `provider: github`. After GitHub user authorization,
the owner selects an installation with `{ "installationId": 123 }`. A selected
installation is immutable. To use a different installation, connect again.

For a GitHub App connection, binding input must include
`github: { "repositoryIds": [456], "permissions": { "contents": "read" } }`.
The supported permission keys are `contents`, `pull_requests`, and `issues`.
Each key accepts `read` or `write`. To give no access, omit the permission.
Metadata read is implicit. The Broker rejects an empty repository list,
duplicates, more than 500 IDs, unknown permissions, and grants that exceed the
current installation authority.

## Internal credential issuance

The three internal routes are registered only on the direct mTLS listener:

- `POST /internal/v1/service-authorizations/list`
- `POST /internal/v1/service-authorizations/check`
- `POST /internal/v1/service-credentials/issue`

The input contains `callerProof`, `connectionId`, `service`, and
`contractVersion`. For issuance, it also contains the connection/binding
revisions and the governed `credentialPolicy` reference. This reference
contains the revision, record UID, and resource version. The input contains no
REST arguments and no operation policy. The Broker independently does these
checks:

- It validates the Pod-bound proof with a TokenReview.
- It examines the live Pod and ServiceAccount.
- It follows the Agent owner reference of the namespace to a GET of the named
  Agent.
- It validates the immutable UID, owner, and binding of that Agent.
- It resolves the service from the trusted profile.

After OAuth refresh or app-only/OBO acquisition, the Broker examines the
identity, connection, and binding again before it returns a credential.

Independent Azure uses the platform-derived tenant, child Agent client ID, and
principal ID. Its two-step autonomous OAuth exchange gets an ARM app-only
token. It never falls back to a personal account. Personal Azure and Microsoft
Graph keep the Entra Auth SDK OBO flow, and they must have a ready Agent
Identity.

GitHub supports personal OAuth or App installation credentials. Google uses
personal OAuth. For these connections, only a supported ready workload is
necessary. Returned tokens exist only in the trusted MCP process for
execution.

HTTP `:8080` serves the Portal/Operator mesh APIs. HTTPS `:8443` serves the
MCP internal APIs. The HTTPS listener validates a client certificate that has
exactly the configured MCP ServiceAccount SPIFFE URI SAN. It never validates an
XFCC header instead.

Configure `tls.enabled`, the server certificate, and the client CA. Also
configure the client certificate and the Broker CA for MCP. Set the Broker
chart's `workloadTokenAudience` to the same value as the MCP chart's
`workloadTokenAudience` and the Operator chart's `connectionsMCP.audience`. All
three default to `cetirun-connections-mcp`. A custom audience must match across
all three releases, because both MCP and the Broker independently verify the
projected workload token of the Agent.

## Credential policy execution and readback

The Operator already has an authenticated mesh API. This API also exposes
`PUT`, `GET`, and `DELETE /operator/v1/instances/{uid}/credential-policy`. The
credential-policy endpoint is not a token endpoint. The versioned
`credential-policy-v3` projection binds these items:

- The Agent, Team, and AgentPolicy UIDs
- The trusted owner tenant/object IDs
- The canonical Agent client and principal IDs
- The requested/allowed/required identity controls

`azure-resource-manager` maps only to the fixed `azure_arm` profile.

PUT independently reads the live Agent, its selected Team and accepted
AgentPolicy. Then it conditionally writes a credential-free Secret in the
Broker namespace. This Secret is owned exactly by the Agent. For this write,
the Operator uses the Secret permissions that it already has. No Secret access
or token mount in the Agent namespace is necessary. PUT does not provision an
identity, and it does not create a Connection or ConnectionBinding.

The 204 acknowledgment of PUT is **not** evidence that the policy is applied.
GET reads the actual record and examines the current authorization again. Then
it returns the UID, resource version, and projection revision of the record.
Desired status, legacy bearer issuance, and provider token acquisition cannot
supply this readback. Application and readback make no provider calls.

For readback, an available supported executor is also necessary. Opted-in
common native OpenClaw/Hermes workloads qualify only after reads of the current
namespace, Pod, Agent, and ServiceAccount. An independent check through the
Pod-admission validator that the Operator already has is also necessary. The
validator uses its protected common-executor expectation to validate the source
UID/version pins, the current workload/control shapes, and the active native
reservation. A stored restriction, a generic Entra projection, a runtime
capability flag, or a desired policy cannot give that authority. Native inputs
that are absent, stale, held, suspended, or unqualified stay
unavailable/nonready.

The native path uses the already-pinned current/default ServiceAccount, not a
new dedicated identity lifecycle. The Operator already installs an exactly
Agent-owned Broker storage Role/RoleBinding in the Agent namespace. It includes
this Role/RoleBinding, the public MCP CA, and the registered MCP Service in
common-executor Sources. Provider credentials stay outside Agent workloads. For
the opt-in and transport prerequisites, see
[native Connections configuration](../cetirun-connections-mcp/README.md#governed-native-workloads).
Generic `AzureADTokenExchange` federation is separate.

Configuration readback can occur before the aggregate Agent Ready. This
prevents a readiness cycle. For actual workload authentication, a Running/Ready
current Pod and a Ready Agent are also necessary. The Broker does not grant
provider permissions or live runtime qualification through configuration
readback.

For governed Azure app-only and Azure/Microsoft delegated acquisition, the
stored projection must match the current sources on the real check/issue path.
Unaccepted, removed, changed, or unreadable inputs fail before credential
acquisition. The policy revision and the record UID/resource version are pinned
together with the Connection and Binding revisions, and are examined again
after acquisition. Before execution, Connections also compares the policy
revision with the status of the authenticated Agent. A governed ARM-only policy
does not authorize Microsoft Graph. Non-governed provider integrations that
already exist keep their Connection/Binding authorization.

Connection discovery lists owned metadata. It does not read ARM
credential-policy evidence, and it does not certify credential authorization.
ARM evidence that is absent, revoked, stale, or unavailable is not a list-wide
dependency for GitHub/Google. The owner, binding, and runtime filters that
already exist do not change. For identity-backed service check/issue, current
credential-policy evidence is still necessary.

After an uncertain PUT, read before you retry. An identical stored projection
is not rewritten. DELETE uses exact Agent ownership and Secret
UID/resource-version preconditions. Then it confirms that the record is absent.

The authorization cleanup route that already exists removes this record before
its other Agent-scoped records. The Operator still waits for Broker cleanup
before directory deletion. A failed cleanup stays retryable. Source checks
reject suspended Agents and Agents in deletion, even if an old record is
present. None of this revokes provider tokens that were already issued. None of
this retries a provider operation that has an unknown execution result.

The internal Broker/Connections wire version is `2026-10-28`. Deploy Broker,
Connections, and Operator builds that match. There is no legacy policy receipt
adapter and no migration reader.

## Availability and identity modes

Both chart switches default to `true`:

```yaml
identityModes:
  azureAgent:
    enabled: true
  microsoftDelegated:
    enabled: true
```

They map to `AZURE_AGENT_ENABLED` and `MICROSOFT_DELEGATED_ENABLED`. When a
mode is disabled, new connections and credential issuance for that mode are
blocked. This does not delete saved Connections or bindings. When either Entra
mode is enabled, the blueprint client ID is necessary. The authorization client
ID is necessary only for Microsoft delegated mode. The common tenant and Key
Vault encryption configuration is still necessary, also for GitHub/Google-only
deployments.

If Microsoft delegated mode is disabled, the Auth SDK container and its scratch
volumes are removed from the chart. Broker `/readyz` reports local readiness
and does not call the SDK. SDK request failures affect delegated
Azure/Microsoft Graph issuance, but not independent Azure or GitHub/Google
flows. When the SDK is enabled, it still shares the Broker Pod. As a result,
scheduling, resource exhaustion, and container/Pod failure can affect the
deployment. This change is not Pod-level isolation.

Broker TLS and MCP serving/client TLS reload mounted material every five
seconds without a restart. Invalid updates keep the previous valid snapshot.
Expired leaf material causes new TLS handshakes or outbound requests to fail.

To rotate CAs, first distribute the old and new trust together. Then replace
the leaf certificates. Then remove the old trust. Allow time for Kubernetes
projection delays and poll delays. In-flight requests and established server
connections can finish. For mount and rollover details, see the
[MCP trust boundary](../cetirun-connections-mcp/README.md#execution-and-trust-boundaries).

Broker shutdown cancels the reload/maintenance loops. It gives HTTP requests up
to ten seconds to drain.

## Shared GitHub App

Configure `providers.github.app` with `id`, `slug`, `clientId`, and
`credentialsSecretName`. The Secret contains `client-secret` and
`private-key.pem`. Only the Broker mounts the private key. A partial App
configuration is rejected. If the App configuration is entirely absent,
personal OAuth stays available. For exact callback URLs, App permissions,
rotation, and trust boundaries, see the
[registration and user guide](../../site/content/en/docs/guides/github-app.md).

App connections store the refreshable authorization of the verified GitHub user
in the encrypted credential store that already exists. The Broker uses that
credential to validate installation access and repository ownership/Admin
authority, but never returns it to MCP. Each execution issuance examines live
GitHub authority again and signs a new installation token with explicit
repository IDs and permission limits. No installation token cache or webhook is
used. Binding revisions are examined again after acquisition. There is no
fallback to personal OAuth.

The App key of the platform can mint tokens up to the full installation grant.
Per-Agent limits protect normal execution but do not remove trust in platform
administrators. GitHub sees the App bot. Internal audit distinguishes Agents.

When the owner selects an installation, the Broker reuses the Connection for
the same CetiRun owner, verified GitHub user ID, and stable installation ID. An
App rename does not create a new connection. GitHub identifies which App owns
the installation. A conditional index serializes concurrent selections.

Healthy credentials and Agent grants that already exist are kept. Each Agent
still has its own repository and permission selection. Personal OAuth
connections, different users, and different installations stay separate. A
temporary sign-in record is consumed only after the reuse succeeds. A retry of
its selection returns the completed result.

Historical duplicates keep their IDs and bindings. `canonicalConnectionId`
identifies the connection that is offered for new access. Connections labels
these historical duplicates and keeps their usage and removal controls. The
removal of an unused duplicate does not revoke the provider authorization or
uninstall the App.

## Personal OAuth

The callback is `/api/v1/connections/callback`. OAuth transactions bind the
owner, state, nonce, and PKCE. For GitHub/Google, a Portal-generated
same-browser proof is also necessary. This proof is stored in a state-specific
Secure HttpOnly host cookie. Reconnect must keep the original provider account.
For an account change, Connect is necessary.

Single-use state and storage CAS protect concurrent callbacks. Transactions are
separate encrypted records, and start responses include an `authorizationId`.
The popup includes this ID. The Portal validates the origin, window source, and
ID of the popup. Then it fetches the owner-scoped result before an account is
selected. Only explicit confirmation creates an Agent binding.

The key of a conditional account index contains the owner tenant/object IDs,
the provider, and the provider-verified stable account ID. Concurrent logins
select one Connection ID, and healthy repeat logins keep credentials and
bindings. Reauthorization keeps the old credentials until a successful sign-in
to the same account. An explicit refresh `invalid_grant` marks sign-in as
required, but transport failures do not. Old duplicate Connections keep their
IDs, and `canonicalConnectionId` is projected for selection.

After one hour elapses from the transaction expiry, expired transactions are
removed at startup, every five minutes, and on the owner's next sign-in.
Cleanup scans paginated authorization Secrets in the Broker credential
namespace, examines the encrypted record identity, and uses UID/resourceVersion
delete preconditions. Corrupt records are skipped with a
warning, and concurrent cleanup conflicts are harmless. Connections, account
indexes, references, and revocation tombstones are outside this sweep.

GitHub uses an OAuth App with `repo` scope, configured by
`providers.github.credentialsSecretName` (`client-id`, `client-secret`). Calls
run as the connected user. OAuth and repository/organization permissions apply
in full. CetiRun does not limit them to repositories or operations. The
personal OAuth mode does not use App installations. GitHub tokens that do not
expire are explicitly marked, but internal issuance still has a finite
five-minute validity.

Google uses a Web OAuth client configured by
`providers.google.credentialsSecretName` with the same keys. Enable the Gmail
and Calendar APIs. Register the exact Portal callback URI.

Authorization asks for openid/email, Gmail read/send, and Calendar, with
offline access and explicit consent. For the initial/reconnect exchange, a
refresh token is necessary. If the provider omits replacements, refresh keeps
the current refresh token or scope set. The actual returned scopes are kept and
shown. They are not translated into a fixed operation catalog. Google's
testing-mode token expiry still applies.

## Storage and lifecycle

| Record | Storage | Owner |
| --- | --- | --- |
| ConnectionBinding | Agent workload namespace | Agent UID |
| Credential policy enforcement (no credentials) | Broker namespace | Agent UID |
| Independent Azure Connection | Agent workload namespace | Agent UID |
| Credential-free delegated connection reference | Agent workload namespace | Agent UID |
| Personal OAuth Connection | Broker credential namespace | Portal owner |
| OAuth client configuration / Broker TLS | Broker namespace | Platform |

Storage in the per-Agent workload namespace is still mandatory for
ConnectionBindings and independent Connections. The credential-free policy
record uses the Broker namespace instead. It still has exact Agent ownership.
There is no migration command or compatibility reader for the previous grant
model.

The Broker resolves the workload namespace from the live Agent state and
examines its Agent owner reference. Ordinary Custom Agents retain their
separate namespace lifecycle: their Broker records require the matching
immutable Custom ownership receipt in the Operator namespace, the exact
Agent/namespace UIDs, and the namespace UID annotation. The Broker receives a
per-Agent `get` grant for that receipt only; it cannot list Operator
ConfigMaps. Encrypted records bind the owner, immutable IDs, and revisions.
Labels and display snapshots support only inventory, and they cannot establish
authority. Workload authentication uses namespace ownership and named Agent
reads. It does not fall back to a cluster-wide Agent list.

Management inventory scans use pages of 50 Agents, with a 1 MiB response limit
for each page. A failed, oversized, or repeated page causes the entire scan to
fail. The scan does not return partial results.

The Operator supplies Broker Secret RBAC in ordinary managed and ordinary
Custom Agent namespaces. This is namespace-wide Secret access, not label-scoped
isolation. Records are never mounted in Agent workloads. Custom receipt proof
permits record storage and finalizer cleanup only; it does not enable Custom
workload credential issuance.

The Operator cleanup endpoint deletes validated records with
UID/resourceVersion preconditions before the Agent finalizer is released.
Namespace/Agent garbage collection also owns their lifetime. Personal
credentials are kept when any individual Agent is deleted. Unsupported
runtimes, for example Sandbox, do not receive Broker Secret access.

## Audit

Broker emits structured started/completed events for OAuth start/reauthorize/
completion, independent identity connection, binding connect/update/disconnect,
account removal, Operator cleanup, and internal credential operations. Events
include the correlation ID, duration, HTTP status, outcome, and verified
owner/target IDs when available. A confirmed storage commit is recorded as
succeeded. Ambiguous storage failures after a mutation starts are
`execution_unknown`.

An OAuth error page is not a successful account connection. An interrupted
request can have only a started event. As a result, logs are not a durable
transaction ledger. Queries, credentials, request/response bodies, and headers
are not logged.

MCP audit adds the validated provider method/path, the stable executor account
ID, the Agent and connection/binding revisions, and the upstream results. MCP
uses the same correlation for its Broker calls and provider request. These IDs
correlate observations. They do not deduplicate execution.

## Offline checks

```bash
go test ./... -count=1
go test -race ./...
go vet ./...
bash ../../scripts/cetirun-agent-broker-helm-test.sh
```

No live provider or Kubernetes access is necessary for these checks.

For account removal, there must be no active binding and no pending binding
registration. Each binding write first reserves its own token on the Connection
with Kubernetes resourceVersion CAS. After the namespace Binding commits,
superseded tokens are released. Disconnect persists a tombstone before it
releases its token. Usage checks include Agents in deletion and unsupported
Agents.

Interrupted registrations are reclaimed only after a Binding revision fence
proves that a delayed write cannot commit. Personal account deletion uses the
same Connection CAS. This prevents a check-then-delete race. Local removal
never revokes provider-wide OAuth consent.

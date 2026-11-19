# CetiRun Istio authentication

Read [Istio authentication and egress](../../site/content/en/docs/operations/istio.md)
for identity, DNS/TLS, helper inputs, external authorization, ownership checks,
webhook exceptions, egress configuration, and warnings about removal.

`values.yaml` configures the shared chart. `templates/ingress/` contains the
gateway and authentication resources. `templates/egress/` contains the
destination and logging controls for all of the mesh. The operator manages the
VirtualServices for each instance.

`deploy.sh` changes both the Helm resources and the AKS Istio shared ConfigMap.
For a manual Helm install, a separate configuration for external authorization
is necessary. The chart and the helper do not issue or renew TLS certificates.

For a single public hostname backed by Azure Public IP DNS or a user-managed
DNS record, configure `publicTLS` in the normal environment file as
[Public DNS and TLS](../../site/content/en/docs/operations/public-ingress.md)
describes. `make deploy` can issue and renew the externally managed
`cetirun-tls` Secret with cert-manager HTTP-01. The umbrella chart owns the
port-80 ACME listener only. This chart continues to own the HTTPS Gateway and
must use `tls.enabled: false` for that ownership model.

Before you run the deployment helper, install its pinned Python dependency:

```bash
python3 -m pip install --requirement infra/istio/requirements.txt
```

## Portal ownership endpoint

For the platform umbrella, set `PORTAL_AUTH_NAMESPACE=cetirun-system` before
you run `deploy.sh`. The default value for the standalone helper is
`cetirun-portal`. `PORTAL_AUTH_SERVICE` and `PORTAL_AUTH_CLUSTER_HOST` let you
set custom service locations. Refer to the
[ownership endpoint configuration](../../site/content/en/docs/operations/istio.md#portal-ownership-endpoint).

## Ingress configuration

The Gateway serves HTTPS on port 443. Every authentication policy rule and all
three ingress HTTP filters match only that port. As a result, they do not
intercept an HTTP-01 challenge on a separate port-80 listener. The chart does
not create that listener or its challenge route.

| Value | Default | Behavior |
| --- | --- | --- |
| `gateway.includeWildcardHost` | `true` | Includes both `domain` and `*.domain` in the Gateway. `false` includes only the exact `domain`. |
| `oauth2Proxy.skipJwtBearerTokens` | `null` | Keeps the default of the mode: validated JWT authentication is on for `entra-id` and off for `oidc` and `local-oidc`. With `false`, a session cookie is necessary. `true` is rejected in both OIDC modes. |
| `oauth2Proxy.logging.request` | `true` | Enables OAuth2 Proxy request logs. |
| `oauth2Proxy.logging.auth` | `true` | Enables OAuth2 Proxy authentication logs. |
| `oauth2Proxy.logging.standard` | `true` | Enables OAuth2 Proxy standard logs. These logs also contain diagnostic errors. |
| `cookieSecretLegacyNamespace` | `istio-system` | Reads the legacy cookie Secret only if the release namespace does not have one. An empty string disables this fallback. |

The host and logging switches accept YAML booleans, not strings. The legacy
namespace must be a valid Kubernetes namespace or an empty string. If an older
release does not have an option, that option keeps the default above. This is
also true with `--reuse-values`.

For an apex-only deployment that makes session cookies necessary and disables
OAuth2 Proxy logs, the values file of the environment can contain:

```yaml
gateway:
  includeWildcardHost: false
cookieSecretLegacyNamespace: ""
oauth2Proxy:
  skipJwtBearerTokens: false
  logging:
    request: false
    auth: false
    standard: false
```

These values are deployment choices, not new global defaults. If you disable
standard logs, you also remove the diagnostic output of OAuth2 Proxy. The
removal of the wildcard host does not remove authentication rules for legacy
subdomains or add anonymous routes. The exempt paths, callback method guards,
and Portal ownership checks stay in place.

OAuth2 Proxy always uses PKCE with `S256`, verifies the OIDC nonce, and sets
Secure and HttpOnly session cookies. The CSRF cookies for each request, their
configured limit, and the 401 behavior of API and Agent asset requests stay
enabled. TLS verification for OIDC is not disabled. The ingress filters
continue to remove Portal identity tokens and session cookies before the
requests get to Agent workloads.

## Cloud and OIDC parameters

The existing `entra-id` mode keeps its Azure global cloud and federated-token
authentication defaults. For Azure China, select `oauth2Proxy.mode=oidc`
explicitly. This uses OAuth2 Proxy's generic OIDC provider with a client secret
from a Secret in the release namespace. The bundled `entra-id` provider rejects
China issuers, so the chart rejects that combination before installation.

| Parameter | Default | Use |
| --- | --- | --- |
| `cloud` | `AzureCloud` | `AzureCloud` or `AzureChinaCloud`; selects the authority for a derived issuer, not the Azure CLI cloud. |
| `oauth2Proxy.mode` | `entra-id` | Select `oidc` for HTTPS OIDC with a client secret. `local-oidc` remains the separate local-development mode. |
| `oauth2Proxy.oidc.issuerURL` | Empty | Explicit HTTPS issuer. If empty, derive it from `cloud` and `tenantId`. |
| `oauth2Proxy.oidc.clientID` | Empty | Client ID of the Portal login application. |
| `oauth2Proxy.oidc.existingSecret` | Empty | Existing Secret containing that application's client secret. |
| `oauth2Proxy.oidc.clientSecretKey` | `client-secret` | Key in the referenced Secret. |

Derived issuers are `https://login.microsoftonline.com/<tenantId>/v2.0` for
`AzureCloud` and `https://login.partner.microsoftonline.cn/<tenantId>/v2.0` for
`AzureChinaCloud`; `tenantId` is the directory's GUID, not its domain name.
An explicit issuer takes precedence and does not require
`tenantId`. Use the exact issuer advertised by the identity provider's discovery
document. This mode does not allow HTTP or skip TLS, signature, issuer, or nonce
verification. PKCE, secure cookies, session storage and Portal ownership checks
remain enabled. Bearer-token session bypass remains disabled in OIDC modes.

Use only a provider whose administrators control the login claims. With Agent
Identity and Tools disabled, Portal API ownership and administrator checks use
the lowercased local part of `preferred_username`, falling back to the email
header if it is absent. The ingress ownership check uses the email header, which
comes from `oauth2Proxy.emailClaim` (`preferred_username` by default).

The provider must assign these claims, prevent users from changing them, and
keep their local parts unique across all users allowed into this installation,
including users from different email domains. If `emailClaim: email` is selected,
the email and preferred username must identify the same owner. Do not configure
a provider that lets users choose another user's alias. A valid signature and
group membership establish who issued the claims and who may sign in; they do
not establish that an alias is administrator-controlled. This mode does not add
the Portal's separate Entra tenant/object-ID verification.

`allowedGroups` must name at least one group, and the identity provider must put
the applicable memberships in the `groups` claim. Generic OIDC does not fetch
Entra group-overage memberships from Microsoft Graph. A user whose token omits
the allowed membership cannot sign in; do not remove the group restriction to
work around an overage.

All customizations can be supplied on the Helm command line without editing
`values.yaml`. This synthetic example renders locally and uses no credentials:

```bash
helm template cetirun-istio ./infra/istio --namespace cetirun-istio \
  --set-string cloud=AzureChinaCloud \
  --set-string oauth2Proxy.mode=oidc \
  --set-string tenantId=11111111-1111-1111-1111-111111111111 \
  --set-string oauth2Proxy.oidc.clientID=22222222-2222-2222-2222-222222222222 \
  --set-string oauth2Proxy.oidc.existingSecret=portal-oidc \
  --set-string oauth2Proxy.oidc.clientSecretKey=client-secret \
  --set-string allowedGroups=33333333-3333-3333-3333-333333333333 \
  --set-string domain=portal.example.com \
  --set-string portalAuth.clusterHost=cetirun-portal.cetirun-system.svc.cluster.local \
  --set-json 'egress.externalServices=["login.partner.microsoftonline.cn"]' \
  >/dev/null
```

To override the issuer, also pass
`--set-string oauth2Proxy.oidc.issuerURL=https://identity.example.com/tenant`.
`egress.externalServices` remains an independent SNI logging configuration, not
an allowlist. Supply the complete list of hosts needed by your environment; the
example replaces it with just the login host. No cloud changes its default list.

For deployment, replace the synthetic coordinates and use the reviewed flags
with `helm upgrade --install`, an explicit `--kube-context`, and prepared TLS,
mesh authorization and Secret resources. Do not pass the client secret itself
with `--set`. Rotate it before expiry and restart OAuth2 Proxy after updating
the Secret, because the process reads it from its environment.

Use the [component platform chart](../../app/cetirun-platform/README.md#use-the-component-chart-directly)
with `cetirun-portal.features.agentIdentityEnabled=false`,
`cetirun-portal.features.agentToolsEnabled=false`,
`cetirun-operator.agentIdentity.enabled=false`, and
`cetirun-connections-mcp.enabled=false`. These are umbrella values; all four
already default to false. Do not install the Broker. The full `make deploy` and
the legacy `infra/istio/deploy.sh` helper still expect the Entra federated-token
configuration; they do not select
this OIDC path. These settings do not establish live China login, image access,
or runtime qualification.

## Preserve sessions and TLS during upgrades

If `cookieSecret` is empty, Helm first reads `oauth2-proxy-credentials` in the
release namespace. Helm uses the `cookie-secret` bytes of that Secret again and
does not change them. Helm tries `cookieSecretLegacyNamespace` only if that
Secret does not exist. If you disable the legacy lookup, you do not disable the
reuse in the current namespace.

If a Secret exists but does not contain `cookie-secret`, the chart render
fails. If neither reusable Secret exists, an upgrade also fails and does not
generate a new session key. If neither Secret exists, a first install generates
a 32-character key. An explicit `cookieSecret` overrides the lookup and can
rotate the key. A rotated key makes the sessions invalid. Do not change it
during a normal upgrade.

A client-only `helm template --is-upgrade` cannot see cluster Secrets. Without
an explicit `cookieSecret`, a failure of this command does not show that the
live Secret does not exist. Offline tests supply a synthetic key only to the
render command. Never copy that key, or a manifest that contains it, into
deployment values or a real upgrade.

A real Helm upgrade must use server lookup or an explicitly supplied key that
already exists. Render-only deployment tools cannot keep a generated key
through lookup. These tools must have a stable, separately protected
`cookieSecret`.

The Gateway still refers to `cetirun-tls` in `gateway.namespace`. With
`tls.enabled: false`, the chart does not render or update that externally
managed Secret. With `tls.enabled: true`, both `tls.cert` and `tls.key` are
necessary, and the chart uses them as they are supplied. During an upgrade, keep
the same TLS ownership mode and inputs. If you disable `tls.enabled` after Helm
managed the Secret, the upgrade removes the Secret from the release and can
delete it. No ingress option above migrates TLS ownership, issues a
certificate, or rotates a key.

## Offline checks

```bash
bash scripts/cetirun-istio-helm-test.sh
python3 -B -m unittest discover -s scripts/tests -p 'test_istio_*.py' -v
```

The Python checks must have the pinned PyYAML dependency in `requirements.txt`.
The Secret-reuse tests run Helm against a loopback-only API fixture that has
synthetic objects and no credentials. That fixture supports discovery and GET
requests. It does not support Kubernetes writes or OpenAPI validation. These
checks cover the rendered chart and the lookup decisions. They do not cover a
live upgrade, OIDC sign-in, or Istio traffic.

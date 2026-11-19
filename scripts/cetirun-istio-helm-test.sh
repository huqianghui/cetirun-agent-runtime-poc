#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

script_dir="$(cd "$(dirname "$0")" && pwd)"
repo_root="$(cd "$script_dir/.." && pwd)"
chart_dir="${CHART_DIR:-${repo_root}/infra/istio}"

if [ ! -f "${chart_dir}/Chart.yaml" ]; then
  echo "ERROR: Chart.yaml not found in ${chart_dir}" >&2
  exit 1
fi

base_args=(
  --set-string clientId=lint-client
  --set-string tenantId=lint-tenant
  --set-string allowedGroups=lint-group
  --set-string cookieSecret=lint-cookie-secret
  --set-string oauth2Proxy.sessionStore.type=cookie
)

assert_ownership_target() {
  local output="$1"
  local host="$2"
  grep -Fq "address: \"${host}\"" <<< "${output}" || {
    echo "FAIL: ownership cluster did not target ${host}" >&2
    exit 1
  }
  grep -Fq "[\":authority\"] = \"${host}\"" <<< "${output}" || {
    echo "FAIL: ownership request authority did not target ${host}" >&2
    exit 1
  }
}

# Concurrent sign-ins keep separate CSRF cookies, and API requests answer 401
# instead of starting a sign-in, in every session store mode.
assert_sign_in_flags() {
  local output="$1"
  local limit="$2"
  local flag
  for flag in \
    '- --cookie-secure=true' \
    '- --cookie-httponly=true' \
    '- --code-challenge-method=S256' \
    '- --insecure-oidc-skip-nonce=false' \
    '- --cookie-csrf-per-request=true' \
    "- --cookie-csrf-per-request-limit=${limit}" \
    '- --api-route=^/api/' \
    '- --api-route=^/ui/[^/]+/api/' \
    '- --api-route=^/ui/[^/]+/assets/' \
    '- --api-route=^/[^/]+/cetirun-auth/'; do
    grep -Fxq -- "            ${flag}" <<< "${output}" || {
      echo "FAIL: oauth2-proxy did not render ${flag}" >&2
      exit 1
    }
  done
}

echo "==> helm template cetirun-istio (standalone portal default)"
output=$(helm template cetirun-istio "${chart_dir}" "${base_args[@]}")
assert_ownership_target "${output}" "cetirun-portal.cetirun-portal.svc.cluster.local"
grep -Fq 'local PORTAL_AUTH_MTLS_ENABLED = false' <<< "${output}" || {
  echo "FAIL: plaintext ownership mode was not the default" >&2
  exit 1
}
if grep -Fq 'name: portal_auth_mtls_cluster' <<< "${output}"; then
  echo "FAIL: plaintext ownership mode rendered the token transport cluster" >&2
  exit 1
fi
grep -Fq 'local EXEMPT_EXACT_PATHS = {"/healthz", "/readyz", "/favicon.ico", "/connections", "/connections/"}' <<< "${output}" || {
  echo "FAIL: health and readiness exemptions must be exact paths" >&2
  exit 1
}
grep -Fq 'local EXEMPT_PATH_PREFIXES = {"/oauth2", "/api", "/internal", "/assets", "/instances", "/admin"}' <<< "${output}" || {
  echo "FAIL: Portal subtree exemptions are missing" >&2
  exit 1
}
grep -Fq 'return path == prefix or path:sub(1, #prefix + 1) == prefix .. "/"' <<< "${output}" || {
  echo "FAIL: Portal subtree exemptions do not enforce a path-segment boundary" >&2
  exit 1
}
if grep -Fq 'local EXEMPT_PREFIXES =' <<< "${output}"; then
  echo "FAIL: legacy string-prefix exemptions can shadow valid instance names" >&2
  exit 1
fi
grep -Fq 'local PORTAL_COOKIE_NAME = "_cetirun_oauth2"' <<< "${output}" || {
  echo "FAIL: ownership filter did not identify the Portal session cookie" >&2
  exit 1
}
grep -Fq 'name:match("^" .. PORTAL_COOKIE_NAME .. "_%d+$")' <<< "${output}" || {
  echo "FAIL: ownership filter did not handle chunked Portal session cookies" >&2
  exit 1
}
grep -Fq 'name:match("^" .. PORTAL_COOKIE_NAME .. "_[%w_%-]*csrf$")' <<< "${output}" || {
  echo "FAIL: ownership filter did not remove OAuth2 Proxy sign-in cookies" >&2
  exit 1
}
[[ "$(grep -Ec '^[[:space:]]+strip_portal_session_cookie\(request_handle\)$' <<< "${output}")" == 1 ]] || {
  echo "FAIL: Portal cookie stripping must run on owned Agent routes" >&2
  exit 1
}
grep -Fq 'headers:replace("cookie", table.concat(retained, "; "))' <<< "${output}" || {
  echo "FAIL: ownership filter did not preserve Agent-owned cookies" >&2
  exit 1
}
grep -Fq 'local AGENT_API_KEY_HEADER = "x-cetirun-agent-api-key"' <<< "${output}" || {
  echo "FAIL: ownership filter did not define the native Agent API transport header" >&2
  exit 1
}
grep -Fq 'headers:replace("authorization", "Bearer " .. api_key)' <<< "${output}" || {
  echo "FAIL: ownership filter did not restore the native Agent API bearer credential" >&2
  exit 1
}
grep -Fq 'path:match("^/ui/[^/]+/v1/chat/completions$") ~= nil' <<< "${output}" || {
  echo "FAIL: native Agent API credential is not restricted to retained API routes" >&2
  exit 1
}
grep -Fq 'handle:streamInfo():dynamicMetadata():set("cetirun", "client_authorization", authorization)' <<< "${output}" || {
  echo "FAIL: security headers filter did not record the client's Authorization before ext-auth" >&2
  exit 1
}
grep -Fq 'if client and not same_credential(client, identity_token) then' <<< "${output}" || {
  echo "FAIL: ownership filter could forward the credential OAuth2 Proxy accepted as the user's identity" >&2
  exit 1
}
grep -Fq 'path:match("^/ui/[^/]+/") ~= nil' <<< "${output}" || {
  echo "FAIL: the browser's Authorization is not restricted to native Agent UI routes" >&2
  exit 1
}
grep -Fq 'key:match("^[A-Za-z0-9_+/=-]+$") ~= nil' <<< "${output}" || {
  echo "FAIL: ownership filter did not accept existing standard Base64 auth keys" >&2
  exit 1
}
[[ "$(grep -Fc 'request_handle:headers():remove(AGENT_API_KEY_HEADER)' <<< "${output}")" == 3 ]] || {
  echo "FAIL: fallback, Portal, and shared routes must remove the native Agent API header" >&2
  exit 1
}
grep -Fq -- '- --scope=openid email profile' <<< "${output}" || {
  echo "FAIL: cookie-only oauth2-proxy requested an unexpected scope" >&2
  exit 1
}
grep -Fq -- '- --oidc-email-claim=preferred_username' <<< "${output}" || {
  echo "FAIL: oauth2-proxy did not use the tenant-facing username as its email claim" >&2
  exit 1
}
grep -Fq -- '- --cookie-expire=45m' <<< "${output}" || {
  echo "FAIL: cookie-only oauth2-proxy did not expire before the ID token" >&2
  exit 1
}
# Container Insights copies a container's environment, Secret values included,
# into its ContainerInventory table unless collection is off for the container.
grep -A1 'name: AZMON_COLLECT_ENV$' <<< "${output}" | grep -Fq 'value: "False"' || {
  echo "FAIL: oauth2-proxy does not keep its cookie secret out of Container Insights" >&2
  exit 1
}
assert_sign_in_flags "${output}" 20
if grep -Fq -- 'offline_access' <<< "${output}" ||
  grep -Fq -- '--session-store-type=redis' <<< "${output}" ||
  grep -Fq -- 'OAUTH2_PROXY_REDIS_PASSWORD' <<< "${output}" ||
  grep -Fq -- 'checksum/redis-session-credentials:' <<< "${output}"; then
  echo "FAIL: cookie-only oauth2-proxy rendered Redis or refresh-token configuration" >&2
  exit 1
fi

echo "==> helm template cetirun-istio (Redis session store)"
redis_output=$(helm template cetirun-istio "${chart_dir}" "${base_args[@]}" \
  --set-string oauth2Proxy.sessionStore.type=redis \
  --set-string oauth2Proxy.sessionStore.redis.connectionURL=redis://oauth2-proxy-redis.example.svc.cluster.local:6379/0 \
  --set-string oauth2Proxy.sessionStore.redis.existingSecret=oauth2-proxy-redis-credentials)
grep -Fq -- '- --scope=openid email profile offline_access' <<< "${redis_output}" || {
  echo "FAIL: Redis oauth2-proxy did not request a refresh token" >&2
  exit 1
}
grep -Fq -- '- --session-store-type=redis' <<< "${redis_output}" || {
  echo "FAIL: Redis oauth2-proxy did not enable the Redis session store" >&2
  exit 1
}
grep -Fq -- '- --redis-connection-url=redis://oauth2-proxy-redis.example.svc.cluster.local:6379/0' <<< "${redis_output}" || {
  echo "FAIL: Redis oauth2-proxy did not render the configured connection URL" >&2
  exit 1
}
grep -Fq -- 'name: OAUTH2_PROXY_REDIS_PASSWORD' <<< "${redis_output}" || {
  echo "FAIL: Redis oauth2-proxy did not load its password from a Secret" >&2
  exit 1
}
grep -Fq -- 'checksum/redis-session-credentials:' <<< "${redis_output}" || {
  echo "FAIL: Redis Secret changes cannot trigger an oauth2-proxy rollout" >&2
  exit 1
}
grep -Fq -- '- --cookie-expire=8h' <<< "${redis_output}" || {
  echo "FAIL: Redis oauth2-proxy did not render the configured cookie lifetime" >&2
  exit 1
}
grep -Fq -- '- --cookie-refresh=30m' <<< "${redis_output}" || {
  echo "FAIL: Redis oauth2-proxy did not refresh the Portal session before ID-token expiry" >&2
  exit 1
}
assert_sign_in_flags "${redis_output}" 20

echo "==> helm template cetirun-istio (CSRF cookie limit)"
limit_output=$(helm template cetirun-istio "${chart_dir}" "${base_args[@]}" \
  --set oauth2Proxy.cookie.csrfPerRequestLimit=50)
assert_sign_in_flags "${limit_output}" 50
policy_output=$(helm template cetirun-istio "${chart_dir}" "${base_args[@]}" \
  --show-only templates/ingress/authorization-policy.yaml)
callback_count="$(grep -Fc '/api/v1/connections/callback' <<< "${policy_output}")"
[[ "${callback_count}" == 2 ]] || {
  echo "FAIL: apex callback bypass leaked into a legacy instance-subdomain rule" >&2
  exit 1
}
if grep -Fq '/api/messages' <<< "${output}"; then
  echo "FAIL: removed Teams webhook still has special ingress handling" >&2
  exit 1
fi
grep -Fq 'instance_name = legacy_instance_name(authority)' <<< "${output}" || {
  echo "FAIL: legacy subdomain routes did not derive ownership exclusively from authority" >&2
  exit 1
}
grep -Fq 'if not host then host = value:match("^([^:]+):%d+$") end' <<< "${output}" || {
  echo "FAIL: ownership filter did not normalize the request host" >&2
  exit 1
}
grep -Fq 'if host:sub(-1) == "." then host = host:sub(1, -2) end' <<< "${output}" || {
  echo "FAIL: ownership filter did not normalize a DNS root label" >&2
  exit 1
}
grep -Fq 'request_handle:respond({[":status"] = "403"}, "Forbidden: invalid instance host")' <<< "${output}" || {
  echo "FAIL: invalid legacy hosts did not fail closed" >&2
  exit 1
}
grep -Fq 'if not is_anonymous_apex_bypass(path_only, method, authority) then' <<< "${output}" || {
  echo "FAIL: anonymous apex bypasses could be promoted to a Portal identity" >&2
  exit 1
}
grep -Fq 'if request_host(authority) ~= PORTAL_DOMAIN then' <<< "${output}" || {
  echo "FAIL: Portal path exemptions leaked to instance subdomains" >&2
  exit 1
}
echo "==> helm template cetirun-istio (no callback bypasses)"
no_callbacks_policy=$(helm template cetirun-istio "${chart_dir}" "${base_args[@]}" \
  --show-only templates/ingress/authorization-policy.yaml \
  --set-json 'authPolicy.apexPostBypassPaths=[]' \
  --set-string 'authPolicy.oauthCallbackPath=')
if grep -Eq '^[[:space:]]+(paths|notMethods):' <<< "${no_callbacks_policy}"; then
  echo "FAIL: disabled callbacks rendered an unused method guard" >&2
  exit 1
fi

echo "==> helm template cetirun-istio (umbrella portal override)"
output=$(helm template cetirun-istio "${chart_dir}" "${base_args[@]}" \
  --set-string portalAuth.clusterHost=cetirun-portal.cetirun-system.svc.cluster.local)
assert_ownership_target "${output}" "cetirun-portal.cetirun-system.svc.cluster.local"

echo "==> helm template cetirun-istio (mTLS Portal ownership)"
mtls_output=$(helm template cetirun-istio "${chart_dir}" "${base_args[@]}" \
  --set portalAuth.mtls.enabled=true \
  --set-string portalAuth.mtls.serverIdentity=spiffe://cluster.local/ns/synthetic/sa/portal)
grep -Fq 'name: portal_auth_mtls_cluster' <<< "${mtls_output}" || {
  echo "FAIL: mTLS ownership mode did not add its distinct cluster" >&2
  exit 1
}
grep -Fq 'local PORTAL_AUTH_CLUSTER = "portal_auth_mtls_cluster"' <<< "${mtls_output}" || {
  echo "FAIL: mTLS ownership mode did not select its distinct cluster" >&2
  exit 1
}
grep -Fq 'type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.UpstreamTlsContext' <<< "${mtls_output}" || {
  echo "FAIL: mTLS ownership cluster did not render an upstream TLS context" >&2
  exit 1
}
grep -Fq 'exact: "spiffe://cluster.local/ns/synthetic/sa/portal"' <<< "${mtls_output}" || {
  echo "FAIL: mTLS ownership cluster did not pin the configured SPIFFE URI" >&2
  exit 1
}
if grep -Fq 'name: portal_auth_mtls_cluster' <<< "${mtls_output}" && grep -A70 'name: portal_auth_mtls_cluster' <<< "${mtls_output}" | grep -Fq 'raw_buffer'; then
  echo "FAIL: mTLS ownership cluster rendered a plaintext fallback" >&2
  exit 1
fi

echo "==> reject incomplete and invalid Redis session configuration"
if helm template cetirun-istio "${chart_dir}" "${base_args[@]}" \
  --set-string oauth2Proxy.sessionStore.type=redis \
  --set oauth2Proxy.sessionStore.redis.bundled.enabled=false >/dev/null 2>&1; then
  echo "FAIL: Redis session store rendered without a connection URL and Secret" >&2
  exit 1
fi
if helm template cetirun-istio "${chart_dir}" "${base_args[@]}" \
  --set-string oauth2Proxy.sessionStore.type=invalid >/dev/null 2>&1; then
  echo "FAIL: invalid session store type rendered successfully" >&2
  exit 1
fi
if helm template cetirun-istio "${chart_dir}" "${base_args[@]}" \
  --set-string oauth2Proxy.emailClaim=unsupported >/dev/null 2>&1; then
  echo "FAIL: invalid OIDC email claim rendered successfully" >&2
  exit 1
fi
if helm template cetirun-istio "${chart_dir}" "${base_args[@]}" \
  --set portalAuth.mtls.enabled=true >/dev/null 2>&1; then
  echo "FAIL: mTLS ownership rendered without a server SPIFFE URI" >&2
  exit 1
fi
if helm template cetirun-istio "${chart_dir}" "${base_args[@]}" \
  --set portalAuth.mtls.enabled=true \
  --set-string portalAuth.mtls.serverIdentity=spiffe://cluster.local/not-an-identity >/dev/null 2>&1; then
  echo "FAIL: mTLS ownership rendered with a malformed server SPIFFE URI" >&2
  exit 1
fi
for limit in 0 -1 1.5 many; do
  if helm template cetirun-istio "${chart_dir}" "${base_args[@]}" \
    --set "oauth2Proxy.cookie.csrfPerRequestLimit=${limit}" >/dev/null 2>&1; then
    echo "FAIL: CSRF cookie limit ${limit} rendered successfully" >&2
    exit 1
  fi
done

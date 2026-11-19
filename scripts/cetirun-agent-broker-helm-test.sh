#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

script_dir="$(cd "$(dirname "$0")" && pwd)"
repo_root="$(cd "${script_dir}/.." && pwd)"
chart_dir="${CHART_DIR:-${repo_root}/app/cetirun-agent-broker/chart}"
app_version="$(awk -F': *' '$1=="appVersion" {gsub(/"/, "", $2); print $2; exit}' "${chart_dir}/Chart.yaml")"
if [ -z "${app_version}" ]; then
  echo "FAIL: Broker Chart.yaml has no appVersion" >&2
  exit 1
fi
values=(
  --set-string agentIdentity.tenantId=11111111-1111-1111-1111-111111111111
  --set-string agentIdentity.blueprintClientId=22222222-2222-2222-2222-222222222222
  --set-string agentIdentity.authorizationClientId=33333333-3333-3333-3333-333333333333
  --set-string agentIdentity.encryptionClientId=44444444-4444-4444-4444-444444444444
  --set-string agentIdentity.keyVaultKeyUrl=https://example.vault.azure.net/keys/cache/version
  --set-string agentIdentity.redirectUri=https://portal.example.invalid/api/v1/connections/callback
  --set-string agentIdentity.portalOrigin=https://portal.example.invalid
  --set-string agentIdentity.portalBaseUrl=https://portal.example.invalid
  --set-string istio.revision=asm-example
  --set istio.sidecarInjection=true
  --set-string portal.namespace=portal-example
  --set-string portal.serviceAccount=portal-example
)

output="$(helm template broker-test "${chart_dir}" "${values[@]}")"
# Exercise the Portal's OAuth flow against the rendered mesh allowlist, not
# just the HTTP handlers. Istio trailing wildcards match literal prefixes.
"${PYTHON:-python3}" -c '
import sys
import yaml

documents = [doc for doc in yaml.safe_load_all(sys.stdin) if doc]
policy = next(doc for doc in documents if doc.get("kind") == "AuthorizationPolicy")
principal = "cluster.local/ns/portal-example/sa/portal-example"
paths = [path
         for rule in policy["spec"]["rules"]
         if any(principal in source["source"].get("principals", [])
                for source in rule.get("from", []))
         for target in rule.get("to", [])
         if "8080" in target["operation"].get("ports", [])
         for path in target["operation"].get("paths", [])]
for route in ("/v1/connections/start", "/api/v1/connections/callback",
              "/v1/connection-authorizations/oauth-example"):
    assert any(route.startswith(path[:-1]) if path.endswith("*") else route == path
               for path in paths), f"Portal OAuth route blocked by Istio: {route}"
operator = "cluster.local/ns/cetirun-system/sa/cetirun-operator-controller-manager"
operations = [target["operation"]
              for rule in policy["spec"]["rules"]
              if any(operator in source["source"].get("principals", [])
                     for source in rule.get("from", []))
              for target in rule.get("to", [])]
assert operations == [{"ports": ["8080"], "methods": ["GET", "PUT", "DELETE"],
                       "paths": ["/operator/v1/instances/*"]}], operations
role = next(doc for doc in documents if doc.get("kind") == "ClusterRole")
configuration = [rule for rule in role["rules"]
                 if set(rule.get("resources", [])) & {"teams", "agentpolicies"}]
assert configuration == [{"apiGroups": ["cetirun.microsoft.com"],
                          "resources": ["teams", "agentpolicies"], "verbs": ["get"]}]
assert not any("secrets" in rule.get("resources", []) for rule in role["rules"])
for resource, name in (("validatingwebhookconfigurations", "cetirun-sandbox-pods"),
                       ("rolebindings", "cetirun-agent-tools-broker")):
    rules = [rule for rule in role["rules"] if resource in rule.get("resources", [])]
    assert len(rules) == 1 and rules[0]["resourceNames"] == [name] and rules[0]["verbs"] == ["get"], rules
' <<<"${output}"
grep -Fq 'automountServiceAccountToken: false' <<<"${output}"
grep -Fq 'istio.io/rev: "asm-example"' <<<"${output}"
grep -Fq "image: \"mcr.microsoft.com/aks/ai-runtime/cetirun/cetirun-agent-broker:${app_version}\"" <<<"${output}"
grep -Fq 'image: "mcr.microsoft.com/entra-sdk/auth-sidecar:1.1.2-azurelinux3.0-distroless@sha256:73a8763100c93c3da1a3ed1ba3af2c6122c0517687354dceded30537a1aee97f"' <<<"${output}"
grep -Fq 'name: AzureAd__ClientCredentials__0__SignedAssertionFileDiskPath' <<<"${output}"
grep -Fq 'value: /var/run/secrets/azure/tokens/azure-identity-token' <<<"${output}"
if grep -Fq 'value: $(AZURE_FEDERATED_TOKEN_FILE)' <<<"${output}"; then
  echo 'SDK assertion path must not depend on a later webhook-injected variable' >&2
  exit 1
fi
grep -Fq 'name: Sidecar__AllowOverrides__GetAuthorizationHeader' <<<"${output}"
oauth_output="$(helm template broker-test "${chart_dir}" "${values[@]}" --set-string providers.github.credentialsSecretName=github-oauth-example)"
grep -Fq 'name: GITHUB_OAUTH_CLIENT_ID' <<<"${oauth_output}"
grep -Fq 'name: GITHUB_OAUTH_CLIENT_SECRET' <<<"${oauth_output}"
grep -Fq 'name: "github-oauth-example"' <<<"${oauth_output}"
if grep -Fq 'name: GITHUB_APP_' <<<"${oauth_output}"; then
  echo 'FAIL: personal GitHub OAuth must not configure GitHub App credentials' >&2
  exit 1
fi
app_values=(--set-string providers.github.app.id=12345 --set-string providers.github.app.slug=cetirun-example --set-string providers.github.app.clientId=Iv1.example --set-string providers.github.app.credentialsSecretName=github-app-example)
app_output="$(helm template broker-test "${chart_dir}" "${values[@]}" "${app_values[@]}" --set-string providers.github.credentialsSecretName=github-oauth-example)"
"${PYTHON:-python3}" -c '
import sys
import yaml

deployment = next(doc for doc in yaml.safe_load_all(sys.stdin) if doc and doc.get("kind") == "Deployment")
spec = deployment["spec"]["template"]["spec"]
broker = next(c for c in spec["containers"] if c["name"] == "broker")
env = {e["name"]: e for e in broker["env"]}
assert env["GITHUB_APP_ID"]["value"] == "12345"
assert env["GITHUB_APP_CLIENT_SECRET"]["valueFrom"]["secretKeyRef"] == {"name": "github-app-example", "key": "client-secret"}
assert env["GITHUB_APP_PRIVATE_KEY_FILE"]["value"] == "/var/run/cetirun/github-app/private-key.pem"
assert "GITHUB_OAUTH_CLIENT_ID" in env
assert {"name": "github-app-key", "mountPath": "/var/run/cetirun/github-app", "readOnly": True} in broker["volumeMounts"]
for container in spec["containers"]:
    if container["name"] != "broker":
        assert not any(m["name"] == "github-app-key" for m in container.get("volumeMounts", []))
volume = next(v for v in spec["volumes"] if v["name"] == "github-app-key")
assert volume["secret"]["items"] == [{"key": "private-key.pem", "path": "private-key.pem"}]
' <<<"${app_output}"
for field in id slug clientId credentialsSecretName clientSecretKey privateKeyKey; do
  if helm template broker-test "${chart_dir}" "${values[@]}" "${app_values[@]}" --set-string "providers.github.app.${field}=" >/dev/null 2>&1; then
    echo "FAIL: GitHub App accepted missing ${field}" >&2
    exit 1
  fi
done
google_output="$(helm template broker-test "${chart_dir}" "${values[@]}" --set-string providers.google.credentialsSecretName=google-oauth-example --set-string providers.google.clientIDKey=google-client-id --set-string providers.google.clientSecretKey=google-client-secret)"
grep -A4 -F 'name: GOOGLE_OAUTH_CLIENT_ID' <<<"${google_output}" | grep -Fq 'name: "google-oauth-example"'
grep -A4 -F 'name: GOOGLE_OAUTH_CLIENT_ID' <<<"${google_output}" | grep -Fq 'key: "google-client-id"'
grep -A4 -F 'name: GOOGLE_OAUTH_CLIENT_SECRET' <<<"${google_output}" | grep -Fq 'name: "google-oauth-example"'
grep -A4 -F 'name: GOOGLE_OAUTH_CLIENT_SECRET' <<<"${google_output}" | grep -Fq 'key: "google-client-secret"'
if grep -Fq 'name: GOOGLE_OAUTH_' <<<"${output}"; then
  echo 'FAIL: Google OAuth credentials must require an explicit Secret reference' >&2
  exit 1
fi
if grep -Fq ':latest' <<<"${output}"; then
  echo "FAIL: chart rendered a latest image tag" >&2
  exit 1
fi

digest_output="$(helm template broker-test "${chart_dir}" "${values[@]}" --set-string image.tag=test --set-string image.digest=sha256:1111111111111111111111111111111111111111111111111111111111111111 --set-string authSDK.tag=test --set-string authSDK.digest=sha256:2222222222222222222222222222222222222222222222222222222222222222)"
grep -Fq 'image: "mcr.microsoft.com/aks/ai-runtime/cetirun/cetirun-agent-broker:test@sha256:1111111111111111111111111111111111111111111111111111111111111111"' <<<"${digest_output}"
grep -Fq 'image: "mcr.microsoft.com/entra-sdk/auth-sidecar:test@sha256:2222222222222222222222222222222222222222222222222222222222222222"' <<<"${digest_output}"

# Only the Broker receives the short-lived Kubernetes API credential. The
# third-party Auth SDK container must not receive this mount.
deployment="$(awk '/^kind: Deployment$/{in_deployment=1} in_deployment{print} /^---$/{if(in_deployment) exit}' <<<"${output}")"
[[ "$(grep -Fc 'mountPath: /var/run/cetirun/kubernetes' <<<"${deployment}")" == 1 ]]
[[ "$(grep -Fc 'name: kubernetes-api-credential' <<<"${deployment}")" == 2 ]]
[[ "$(grep -Fc 'mountPath: /app/keys' <<<"${deployment}")" == 1 ]]
[[ "$(grep -Fc 'name: auth-sdk-keys' <<<"${deployment}")" == 2 ]]
grep -A4 -F 'name: auth-sdk-keys' <<<"${deployment}" | grep -Fq 'medium: Memory'
grep -A4 -F 'name: auth-sdk-keys' <<<"${deployment}" | grep -Fq 'sizeLimit: 16Mi'
[[ "$(grep -Fc 'mountPath: /tmp' <<<"${deployment}")" == 1 ]]
[[ "$(grep -Fc 'name: auth-sdk-tmp' <<<"${deployment}")" == 2 ]]
grep -A4 -F 'name: auth-sdk-tmp' <<<"${deployment}" | grep -Fq 'medium: Memory'
grep -A4 -F 'name: auth-sdk-tmp' <<<"${deployment}" | grep -Fq 'sizeLimit: 16Mi'

# The Auth SDK image declares the named user `app`. Kubernetes cannot prove a
# named image user is non-root, so the chart must pin the image's numeric UID
# and GID to satisfy runAsNonRoot before kubelet starts the container.
auth_sdk_security_context="$(awk '/- name: auth-sdk/{in_auth_sdk=1} in_auth_sdk && /securityContext:/{in_security_context=1} in_security_context{print} in_security_context && /resources:/{exit}' <<<"${deployment}")"
grep -Fq 'runAsNonRoot: true' <<<"${auth_sdk_security_context}"
grep -Fq 'runAsUser: 1654' <<<"${auth_sdk_security_context}"
grep -Fq 'runAsGroup: 1654' <<<"${auth_sdk_security_context}"

if helm template broker-test "${chart_dir}" "${values[@]}" --set istio.enabled=false >/dev/null 2>&1; then
  echo "FAIL: chart allowed Agent Identity Broker without Istio authorization" >&2
  exit 1
fi
if helm template broker-test "${chart_dir}" "${values[@]}" --set istio.sidecarInjection=false >/dev/null 2>&1; then
  echo "FAIL: chart allowed unconfirmed sidecar injection" >&2
  exit 1
fi

# The credential listener is exposed only with complete client TLS material.
if grep -Fq 'containerPort: 8443' <<<"${output}"; then
  echo "FAIL: credential listener enabled without client TLS" >&2
  exit 1
fi
for field in secretName certKey keyKey clientCAKey; do
  if helm template broker-test "${chart_dir}" "${values[@]}" --set tls.enabled=true --set-string tls.secretName=broker-client-trust --set-string "tls.${field}=" >/dev/null 2>&1; then
    echo "FAIL: TLS enabled without ${field}" >&2
    exit 1
  fi
done
connection_output="$(helm template broker-test "${chart_dir}" "${values[@]}" --set tls.enabled=true --set-string tls.secretName=broker-client-trust)"
grep -Fq 'containerPort: 8443' <<<"${connection_output}"
grep -Fq 'resources: ["tokenreviews"]' <<<"${connection_output}"
grep -Fq 'ports: ["8443"]' <<<"${connection_output}"
[[ "$(grep -Fc 'mountPath: /var/run/cetirun/tls' <<<"${connection_output}")" == 1 ]]
grep -Fq '/v1/connections*' <<<"${connection_output}"
grep -Fq '/operator/v1/instances/*' <<<"${connection_output}"

# The independent Broker TokenReview must accept the same configured audience
# used by the Operator's projected token and the MCP verifier.
for audience in cetirun-connections-mcp tools.example.com; do
  audience_output="$(helm template broker-test "${chart_dir}" "${values[@]}" --set-string "workloadTokenAudience=${audience}")"
  "${PYTHON:-python3}" -c '
import sys
import yaml

deployment = next(doc for doc in yaml.safe_load_all(sys.stdin)
                  if doc and doc.get("kind") == "Deployment")
values = [entry["value"] for container in deployment["spec"]["template"]["spec"]["containers"]
          for entry in container.get("env", []) if entry["name"] == "WORKLOAD_TOKEN_AUDIENCE"]
assert values == [sys.argv[1]], f"Unexpected Broker workload audience: {values}"
' "${audience}" <<<"${audience_output}"
done
if helm template broker-test "${chart_dir}" "${values[@]}" --set-string workloadTokenAudience= >/dev/null 2>&1; then
  echo 'FAIL: Broker chart allowed an empty workload token audience' >&2
  exit 1
fi

#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

script_dir="$(cd "$(dirname "$0")" && pwd)"
repo_root="$(cd "$script_dir/.." && pwd)"
chart_dir="${CHART_DIR:-${repo_root}/app/cetirun-chart}"
release_name="${RELEASE_NAME:-cetirun}"

if [ ! -f "${chart_dir}/Chart.yaml" ]; then
  echo "ERROR: Chart.yaml not found in ${chart_dir}" >&2
  exit 1
fi

# The chart deliberately has no image-embedded model default. Every rendering
# scenario, including expected-failure cases, must therefore supply a model so
# that each test exercises its intended validation path.
helm_template() {
  helm template --set aiGateway.keySecretName=lint-model-key \
    --set-string secrets.apiBase=http://gateway.example.com/v1 \
    --set-string hermes.env.HERMES_WEBAPP_PUBLIC_URL=https://agents.example.com/ui/cetirun \
    "$@" --set-string secrets.model=lint-model
}

values_files=(
  "values.yaml"
)

for values_file in "${values_files[@]}"; do
  if [ -f "${chart_dir}/${values_file}" ]; then
    echo "==> helm template ${release_name} ${chart_dir} -f ${values_file} (agentType=openclaw)"
    helm_template "${release_name}" "${chart_dir}" -f "${chart_dir}/${values_file}" \
      --set agentType=openclaw \
      --set secrets.openclawGatewayToken=lint-token > /dev/null

    echo "==> helm template ${release_name} ${chart_dir} -f ${values_file} (agentType=hermes)"
    helm_template "${release_name}" "${chart_dir}" -f "${chart_dir}/${values_file}" \
      --set agentType=hermes > /dev/null
  fi
done

# Defaults and explicit selections must agree across config, image and ports.
echo "==> helm template ${release_name} ${chart_dir} (default Hermes)"
output=$(helm_template "${release_name}" "${chart_dir}")
grep -q 'hermes-config.yaml' <<< "$output" || { echo "FAIL: default is not Hermes" >&2; exit 1; }
grep -q 'containerPort: 8642' <<< "$output" || { echo "FAIL: default Hermes API port missing" >&2; exit 1; }
if grep -q '^  openclaw.json: |' <<< "$output"; then
  echo "FAIL: default must not render OpenClaw config" >&2; exit 1
fi
if grep -q '^kind: PersistentVolume' <<< "$output"; then
  echo "FAIL: shared skills storage must remain disabled by default" >&2; exit 1
fi

for agent_type in hermes openclaw; do
  echo "==> ${agent_type}: default HOME storage"
  output=$(helm_template "$release_name" "$chart_dir" --set "agentType=$agent_type" \
    --set secrets.existingSecret=lint-secret --show-only templates/statefulset.yaml)
  for expected in 'storageClassName: default' 'storage: 100Gi' '- ReadWriteOnce' \
    'whenDeleted: Retain' 'whenScaled: Retain'; do
    grep -Fq -- "$expected" <<< "$output" || {
      echo "FAIL: HOME default missing: $expected" >&2; exit 1
    }
  done
  echo "==> ${agent_type}: standalone HOME PVC defaults"
  output=$(helm_template "$release_name" "$chart_dir" --set "agentType=$agent_type" \
    --set secrets.existingSecret=lint-secret --set persistence.useStatefulSetVolumeClaim=false \
    --show-only templates/pvc.yaml)
  for expected in 'storageClassName: default' 'storage: 100Gi' '- ReadWriteOnce'; do
    grep -Fq -- "$expected" <<< "$output" || {
      echo "FAIL: standalone HOME default missing: $expected" >&2; exit 1
    }
  done
done

echo "==> helm template ${release_name} ${chart_dir} (agentType omitted)"
output=$(helm_template "${release_name}" "${chart_dir}" --set agentType=null)
grep -q 'hermes-config.yaml' <<< "$output" || { echo "FAIL: omitted agentType must use Hermes" >&2; exit 1; }

echo "==> helm template ${release_name} ${chart_dir} (shared registry and version)"
output=$(helm_template "${release_name}" "${chart_dir}" \
  --set-string global.registry=registry.example.com \
  --set-string global.version=9.8.7)
grep -q 'image: "registry.example.com/hermes-gateway:9.8.7"' <<< "$output" || {
  echo "FAIL: Hermes image did not use global.registry and global.version" >&2
  exit 1
}
grep -q 'image: "registry.example.com/cetirun-desktop:9.8.7"' <<< "$output" || {
  echo "FAIL: desktop image did not use global.registry and global.version" >&2
  exit 1
}

echo "==> helm template ${release_name} ${chart_dir} (explicit OpenClaw)"
output=$(helm_template "${release_name}" "${chart_dir}" --set agentType=openclaw \
  --set secrets.openclawGatewayToken=lint-token)
grep -q 'openclaw.json' <<< "$output" || { echo "FAIL: explicit OpenClaw config missing" >&2; exit 1; }
grep -q 'containerPort: 18789' <<< "$output" || { echo "FAIL: explicit OpenClaw port missing" >&2; exit 1; }
grep -q 'path: /healthz' <<< "$output" || { echo "FAIL: OpenClaw liveness probe path missing" >&2; exit 1; }
grep -q 'path: /readyz' <<< "$output" || { echo "FAIL: OpenClaw readiness probe path missing" >&2; exit 1; }
grep -q 'path: /startupz' <<< "$output" || { echo "FAIL: OpenClaw startup probe path missing" >&2; exit 1; }

echo "==> helm template ${release_name} ${chart_dir} (OpenClaw shared registry and version)"
output=$(helm_template "${release_name}" "${chart_dir}" \
  --set agentType=openclaw \
  --set secrets.openclawGatewayToken=lint-token \
  --set-string global.registry=registry.example.com \
  --set-string global.version=9.8.7)
grep -q 'image: "registry.example.com/openclaw-gateway:9.8.7"' <<< "$output" || {
  echo "FAIL: OpenClaw image did not use global.registry and global.version" >&2
  exit 1
}

# Test variant: Hermes renders API port, native config, and no OpenClaw doctor init.
echo "==> helm template ${release_name} ${chart_dir} (agentType=hermes, rendered surface)"
output=$(helm_template "${release_name}" "${chart_dir}" -f "${chart_dir}/values.yaml" \
  --set agentType=hermes)
grep -q 'hermes-config.yaml' <<< "$output" || { echo "FAIL: hermes-config.yaml not rendered" >&2; exit 1; }
grep -q 'name: api' <<< "$output" || { echo "FAIL: Hermes API service/container port not found" >&2; exit 1; }
grep -q 'API_SERVER_KEY' <<< "$output" || { echo "FAIL: API_SERVER_KEY not rendered" >&2; exit 1; }
if grep -q '^  hermes-copilot-auth.py: |' <<< "$output"; then
  echo "FAIL: Hermes Copilot Python helper should be packaged in the image, not rendered in ConfigMap" >&2; exit 1
fi
if grep -q 'command: \["openclaw", "doctor"' <<< "$output"; then
  echo "FAIL: OpenClaw doctor init should not render for Hermes" >&2; exit 1
fi

# Connections MCP uses projected workload identity and never exposes provider credentials.
for agent_type in openclaw hermes; do
  echo "==> helm template ${release_name} ${chart_dir} (agentType=${agent_type}, agentIdentity.enabled=true)"
  agent_identity_output=$(helm_template "${release_name}" "${chart_dir}" -f "${chart_dir}/values.yaml" \
    --set agentType="${agent_type}" \
    --set secrets.openclawGatewayToken=lint-token \
    --set serviceAccount.create=true \
    --set serviceAccount.name=agent-workload \
    --set serviceAccount.automountToken=false \
    --set agentIdentity.enabled=true \
    --set connectionsMCP.enabled=true \
    --set connectionsMCP.caSecretName=connections-ca \
    --set connectionsMCP.url=https://cetirun-connections-mcp.cetirun-tools.svc.cluster.local/mcp)
  grep -q 'name: CETIRUN_CONNECTIONS_MCP_URL' <<< "$agent_identity_output" || { echo "FAIL: CetiRun Connections MCP environment variable missing for ${agent_type}" >&2; exit 1; }
  grep -q 'value: "https://cetirun-connections-mcp.cetirun-tools.svc.cluster.local/mcp"' <<< "$agent_identity_output" || { echo "FAIL: CetiRun Connections MCP URL missing for ${agent_type}" >&2; exit 1; }
  grep -q 'kubernetes.io/metadata.name: cetirun-tools' <<< "$agent_identity_output" || { echo "FAIL: CetiRun Connections MCP network policy missing for ${agent_type}" >&2; exit 1; }
  grep -q 'app.kubernetes.io/name: cetirun-connections-mcp' <<< "$agent_identity_output" || { echo "FAIL: CetiRun Connections MCP pod selector missing for ${agent_type}" >&2; exit 1; }
  grep -q 'automountServiceAccountToken: false' <<< "$agent_identity_output" || { echo "FAIL: default Kubernetes token must be disabled for ${agent_type}" >&2; exit 1; }
  if grep -q 'AGENT_IDENTITY_CLIENT_ID' <<< "$agent_identity_output"; then
    echo "FAIL: Agent Identity client ID must not be exposed to ${agent_type}" >&2; exit 1
  fi
done

echo "==> helm template ${release_name} ${chart_dir} (long BYO ServiceAccount without Agent Identity)"
long_instance_name="$(printf 'a%.0s' {1..47})"
long_service_account="openclaw-${long_instance_name}-cetirun"
service_account_output=$(helm_template "${release_name}" "${chart_dir}" \
  --show-only templates/statefulset.yaml \
  --set serviceAccount.create=false \
  --set-string serviceAccount.name="${long_service_account}")
grep -Fq "serviceAccountName: ${long_service_account}" <<< "${service_account_output}" || {
  echo "FAIL: explicit BYO ServiceAccount name was not preserved" >&2
  exit 1
}

echo "==> helm template ${release_name} ${chart_dir} (long Agent Identity ServiceAccount, expect failure)"
if helm_template "${release_name}" "${chart_dir}" \
  --set serviceAccount.create=true \
  --set-string serviceAccount.name="${long_service_account}" \
  --set agentIdentity.enabled=true \
  --set connectionsMCP.enabled=true \
    --set connectionsMCP.caSecretName=connections-ca \
    --set connectionsMCP.url=https://cetirun-connections-mcp.cetirun-tools.svc.cluster.local/mcp 2>/dev/null; then
  echo "FAIL: Agent Identity accepted a ServiceAccount name longer than 63 characters" >&2
  exit 1
fi

echo "==> helm template ${release_name} ${chart_dir} (invalid Agent Identity Broker URL, expect failure)"
if helm_template "${release_name}" "${chart_dir}" -f "${chart_dir}/values.yaml" \
  --set agentType=hermes \
  --set agentIdentity.enabled=true \
  --set connectionsMCP.enabled=true \
  --set connectionsMCP.url=https://broker.example.invalid/mcp 2>/dev/null; then
  echo "FAIL: external Agent Identity Broker URL should have been rejected" >&2; exit 1
fi

for required_setting in sidecarScope.enabled networkPolicy.enabled; do
  echo "==> helm template ${release_name} ${chart_dir} (Agent Identity with ${required_setting}=false, expect failure)"
  if helm_template "${release_name}" "${chart_dir}" -f "${chart_dir}/values.yaml" \
    --set agentType=hermes \
    --set agentIdentity.enabled=true \
    --set connectionsMCP.enabled=true \
    --set connectionsMCP.caSecretName=connections-ca \
    --set connectionsMCP.url=https://cetirun-connections-mcp.cetirun-tools.svc.cluster.local/mcp \
    --set "${required_setting}=false" 2>/dev/null; then
    echo "FAIL: Agent Identity accepted ${required_setting}=false" >&2
    exit 1
  fi
done

echo "==> helm template ${release_name} ${chart_dir} (reserved Agent Identity environment variable, expect failure)"
if helm_template "${release_name}" "${chart_dir}" -f "${chart_dir}/values.yaml" \
  --set agentType=hermes \
  --set agentIdentity.enabled=true \
  --set connectionsMCP.enabled=true \
    --set connectionsMCP.caSecretName=connections-ca \
    --set connectionsMCP.url=https://cetirun-connections-mcp.cetirun-tools.svc.cluster.local/mcp \
  --set extraEnv[0].name=CETIRUN_CONNECTIONS_MCP_URL \
  --set extraEnv[0].value=http://evil.default.svc.cluster.local:8080 2>/dev/null; then
  echo "FAIL: reserved Agent Identity environment variable should have been rejected" >&2; exit 1
fi

# Test variant: existing secret
echo "==> helm template ${release_name} ${chart_dir} (secrets.existingSecret=my-secret)"
helm_template "${release_name}" "${chart_dir}" -f "${chart_dir}/values.yaml" \
  --set secrets.existingSecret=my-secret > /dev/null

# Test variant: ttyd enabled
echo "==> helm template ${release_name} ${chart_dir} (ttyd.enabled=true)"
output=$(helm_template "${release_name}" "${chart_dir}" -f "${chart_dir}/values.yaml" \
  --set secrets.openclawGatewayToken=lint-token \
  --set ttyd.enabled=true)
# Use here-strings instead of "echo | grep" to avoid SIGPIPE/broken-pipe errors
# under "set -eo pipefail". grep -q exits immediately after the first match,
# closing the pipe while echo is still writing, causing a non-zero exit code
# that pipefail treats as a failure.
grep -q 'name: ttyd' <<< "$output" || { echo "FAIL: ttyd port not found when enabled" >&2; exit 1; }
grep -q 'TTYD_ENABLED' <<< "$output" || { echo "FAIL: TTYD_ENABLED env var not found" >&2; exit 1; }

# Test variant: ttyd disabled (default)
echo "==> helm template ${release_name} ${chart_dir} (ttyd.enabled=false)"
output=$(helm_template "${release_name}" "${chart_dir}" -f "${chart_dir}/values.yaml" \
  --set secrets.openclawGatewayToken=lint-token \
  --set ttyd.enabled=false)
if grep -q 'containerPort: 7681' <<< "$output"; then
  echo "FAIL: ttyd containerPort should not appear when disabled" >&2; exit 1
fi

# Test variant: ttyd port collision with noVNC (should fail)
echo "==> helm template ${release_name} ${chart_dir} (ttyd.port=6080, expect failure)"
if helm_template "${release_name}" "${chart_dir}" -f "${chart_dir}/values.yaml" \
  --set secrets.openclawGatewayToken=lint-token \
  --set ttyd.enabled=true \
  --set ttyd.port=6080 2>/dev/null; then
  echo "FAIL: ttyd.port=6080 should have been rejected (noVNC collision)" >&2; exit 1
fi

# Test variant: ttyd port collision with gateway (should fail)
echo "==> helm template ${release_name} ${chart_dir} (ttyd.port=18789, expect failure)"
if helm_template "${release_name}" "${chart_dir}" -f "${chart_dir}/values.yaml" \
  --set agentType=openclaw \
  --set secrets.openclawGatewayToken=lint-token \
  --set ttyd.enabled=true \
  --set ttyd.port=18789 2>/dev/null; then
  echo "FAIL: ttyd.port=18789 should have been rejected (gateway collision)" >&2; exit 1
fi

# Test variant: Hermes ttyd port collision with API (should fail)
echo "==> helm template ${release_name} ${chart_dir} (agentType=hermes, ttyd.port=8642, expect failure)"
if helm_template "${release_name}" "${chart_dir}" -f "${chart_dir}/values.yaml" \
  --set agentType=hermes \
  --set ttyd.enabled=true \
  --set ttyd.port=8642 2>/dev/null; then
  echo "FAIL: Hermes ttyd.port=8642 should have been rejected (API collision)" >&2; exit 1
fi


# ---------------------------------------------------------------------------
# Desktop sidecar tests
# ---------------------------------------------------------------------------

for agent_type in openclaw hermes; do
  echo "==> ${agent_type}: desktop VNC port collision and disabled-desktop control"
  port_args=(--set hermes.env.API_SERVER_PORT=5900)
  if [ "$agent_type" = openclaw ]; then
    port_args=(--set service.port=5900 --set service.targetPort=5900 --set openclaw.config.gateway.port=5900)
  fi
  if helm_template "$release_name" "$chart_dir" --set "agentType=$agent_type" \
    --set secrets.existingSecret=lint-secret "${port_args[@]}" >/dev/null 2>&1; then
    echo "FAIL: Agent port 5900 should conflict with desktop VNC" >&2; exit 1
  fi
  helm_template "$release_name" "$chart_dir" --set "agentType=$agent_type" \
    --set secrets.existingSecret=lint-secret "${port_args[@]}" \
    --set desktop.enabled=false >/dev/null
  if helm_template "$release_name" "$chart_dir" --set "agentType=$agent_type" \
    --set secrets.existingSecret=lint-secret --set ttyd.enabled=true --set ttyd.port=5900 >/dev/null 2>&1; then
    echo "FAIL: ttyd port 5900 should conflict with desktop VNC" >&2; exit 1
  fi
  helm_template "$release_name" "$chart_dir" --set "agentType=$agent_type" \
    --set secrets.existingSecret=lint-secret --set ttyd.enabled=true --set ttyd.port=5900 \
    --set desktop.enabled=false >/dev/null
done

# Test variant: desktop enabled (default)
echo "==> helm template ${release_name} ${chart_dir} (desktop.enabled=true)"
output=$(helm_template "${release_name}" "${chart_dir}" -f "${chart_dir}/values.yaml" \
  --set agentType=openclaw \
  --set secrets.openclawGatewayToken=lint-token \
  --set desktop.enabled=true)
grep -q '"attachOnly": true' <<< "$output" || { echo "FAIL: desktop browser must default to attach-only" >&2; exit 1; }
grep -q '"headless": false' <<< "$output" || { echo "FAIL: desktop browser must default to headed mode" >&2; exit 1; }
grep -q 'name: desktop' <<< "$output" || { echo "FAIL: desktop container not found when enabled" >&2; exit 1; }
grep -q 'containerPort: 6080' <<< "$output" || { echo "FAIL: noVNC port not found on desktop container" >&2; exit 1; }
grep -q 'containerPort: 18800' <<< "$output" || { echo "FAIL: Chrome CDP port not found on desktop container" >&2; exit 1; }
grep -q 'tmp-desktop' <<< "$output" || { echo "FAIL: tmp-desktop volume not found when desktop enabled" >&2; exit 1; }

# Test variant: desktop disabled
echo "==> helm template ${release_name} ${chart_dir} (desktop.enabled=false)"
output=$(helm_template "${release_name}" "${chart_dir}" -f "${chart_dir}/values.yaml" \
  --set agentType=openclaw \
  --set secrets.openclawGatewayToken=lint-token \
  --set desktop.enabled=false)
grep -q '"attachOnly": false' <<< "$output" || { echo "FAIL: browser must launch without a desktop" >&2; exit 1; }
grep -q '"headless": true' <<< "$output" || { echo "FAIL: browser must default to headless without a desktop" >&2; exit 1; }
if grep -q 'name: desktop' <<< "$output"; then
  echo "FAIL: desktop container should not appear when disabled" >&2; exit 1
fi
if grep -q 'tmp-desktop' <<< "$output"; then
  echo "FAIL: tmp-desktop volume should not appear when desktop disabled" >&2; exit 1
fi
# When desktop is disabled, noVNC ports should not appear anywhere
if grep -q 'containerPort: 6080' <<< "$output"; then
  echo "FAIL: noVNC container port should not appear when desktop disabled" >&2; exit 1
fi

# ---------------------------------------------------------------------------
# Tailscale enablement tests
# ---------------------------------------------------------------------------

# Test variant: tailscale enabled via secrets.tailscaleAuthKey (no existingSecret)
echo "==> helm template ${release_name} ${chart_dir} (tailscaleAuthKey set, no existingSecret)"
output=$(helm_template "${release_name}" "${chart_dir}" -f "${chart_dir}/values.yaml" \
  --set secrets.openclawGatewayToken=lint-token \
  --set secrets.tailscaleAuthKey=tskey-test-key)
grep -q 'TAILSCALE_ENABLED' <<< "$output" || { echo "FAIL: TAILSCALE_ENABLED not found when tailscaleAuthKey is set" >&2; exit 1; }
grep -q 'TS_AUTHKEY' <<< "$output" || { echo "FAIL: TS_AUTHKEY env var not found when tailscaleAuthKey is set" >&2; exit 1; }
grep -q 'TAILSCALE_HOSTNAME' <<< "$output" || { echo "FAIL: TAILSCALE_HOSTNAME not found when tailscaleAuthKey is set" >&2; exit 1; }
grep -q 'tailscale-state' <<< "$output" || { echo "FAIL: tailscale-state volume not found when tailscaleAuthKey is set" >&2; exit 1; }

# Test variant: tailscale disabled (no tailscaleAuthKey, no existingSecret)
echo "==> helm template ${release_name} ${chart_dir} (no tailscaleAuthKey, no existingSecret)"
output=$(helm_template "${release_name}" "${chart_dir}" -f "${chart_dir}/values.yaml" \
  --set secrets.openclawGatewayToken=lint-token)
if grep -q 'TAILSCALE_ENABLED' <<< "$output"; then
  echo "FAIL: TAILSCALE_ENABLED should not appear when tailscaleAuthKey is not set" >&2; exit 1
fi
if grep -q 'tailscale-state' <<< "$output"; then
  echo "FAIL: tailscale-state volume should not appear when tailscale is disabled" >&2; exit 1
fi

# Test variant: tailscale enabled via secrets.tailscaleAuthKey + existingSecret
echo "==> helm template ${release_name} ${chart_dir} (tailscaleAuthKey set + existingSecret)"
output=$(helm_template "${release_name}" "${chart_dir}" -f "${chart_dir}/values.yaml" \
  --set secrets.existingSecret=gateway-token \
  --set secrets.tailscaleAuthKey=tskey-test-key)
grep -q 'TAILSCALE_ENABLED' <<< "$output" || { echo "FAIL: TAILSCALE_ENABLED not found with existingSecret + tailscaleAuthKey" >&2; exit 1; }
grep -q 'TS_AUTHKEY' <<< "$output" || { echo "FAIL: TS_AUTHKEY not found with existingSecret + tailscaleAuthKey" >&2; exit 1; }

# Test variant: existingSecret set, no tailscaleAuthKey (tailscale depends on Secret lookup at deploy time)
# helm template cannot do live lookup, so tailscale should NOT be enabled in dry-run
echo "==> helm template ${release_name} ${chart_dir} (existingSecret, no tailscaleAuthKey — dry-run)"
output=$(helm_template "${release_name}" "${chart_dir}" -f "${chart_dir}/values.yaml" \
  --set secrets.existingSecret=gateway-token)
if grep -q 'TAILSCALE_ENABLED' <<< "$output"; then
  echo "FAIL: TAILSCALE_ENABLED should not appear in dry-run without tailscaleAuthKey (lookup unavailable)" >&2; exit 1
fi

# ---------------------------------------------------------------------------
# Shared skills Azure Blob CSI tests
# ---------------------------------------------------------------------------

echo "==> shared skills retain their Blob NFS defaults"
output=$(helm_template "$release_name" "$chart_dir" --set skills.shared=true \
  --set skills.resourceGroup=lint-rg --set skills.storageAccount=lintstorage \
  --show-only templates/shared-skills-pv.yaml --show-only templates/shared-skills-pvc.yaml)
for expected in 'storageClassName: "azureblob-nfs-premium"' 'storage: "100Gi"' \
  '- ReadWriteMany' 'persistentVolumeReclaimPolicy: Retain' 'protocol: "nfs"'; do
  grep -Fq -- "$expected" <<< "$output" || {
    echo "FAIL: shared skills default changed: $expected" >&2; exit 1
  }
done

# Test variant: Blobfuse2 shared skills render the CSI protocol, staging Secret,
# and static mount options.
echo "==> helm template ${release_name} ${chart_dir} (skills.shared=true, protocol=fuse2)"
output=$(helm_template "${release_name}" "${chart_dir}" -f "${chart_dir}/values.yaml" \
  --set secrets.openclawGatewayToken=lint-token \
  --set skills.shared=true \
  --set skills.resourceGroup=lint-rg \
  --set skills.storageAccount=lintstorage \
  --set skills.containerName=lint-skills \
  --set skills.storageSize=10Gi \
  --set skills.protocol=fuse2 \
  --set skills.secretName=blobfuse-staging \
  --set 'skills.mountOptions[0]=-o allow_other')
grep -Eq 'protocol: "?fuse2"?$' <<< "$output" || { echo "FAIL: Fuse2 protocol not rendered for shared skills" >&2; exit 1; }
grep -Eq 'name: "?blobfuse-staging"?$' <<< "$output" || { echo "FAIL: Blobfuse staging Secret not rendered" >&2; exit 1; }
grep -q -- '- -o allow_other' <<< "$output" || { echo "FAIL: Blobfuse mount option not rendered" >&2; exit 1; }

# Test variant: Blobfuse shared skills require their node-stage Secret.
echo "==> helm template ${release_name} ${chart_dir} (skills.protocol=fuse2 without secretName, expect failure)"
if helm_template "${release_name}" "${chart_dir}" -f "${chart_dir}/values.yaml" \
  --set secrets.openclawGatewayToken=lint-token \
  --set skills.shared=true \
  --set skills.resourceGroup=lint-rg \
  --set skills.storageAccount=lintstorage \
  --set skills.containerName=lint-skills \
  --set skills.storageSize=10Gi \
  --set skills.protocol=fuse2 2>/dev/null; then
  echo "FAIL: Blobfuse shared skills without secretName should have been rejected" >&2; exit 1
fi

# ---------------------------------------------------------------------------
# RuntimeClass (Kata pod sandboxing) tests
# ---------------------------------------------------------------------------

# Test variant: runtimeClassName renders by default (kata-vm-isolation)
echo "==> helm template ${release_name} ${chart_dir} (runtimeClassName=kata-vm-isolation, default)"
output=$(helm_template "${release_name}" "${chart_dir}" -f "${chart_dir}/values.yaml" \
  --set secrets.openclawGatewayToken=lint-token)
grep -Eq 'runtimeClassName: "?kata-vm-isolation"?$' <<< "$output" || { echo "FAIL: runtimeClassName not found in default render" >&2; exit 1; }

# Test variant: runtimeClassName disabled (empty string)
echo "==> helm template ${release_name} ${chart_dir} (runtimeClassName empty)"
output=$(helm_template "${release_name}" "${chart_dir}" -f "${chart_dir}/values.yaml" \
  --set secrets.openclawGatewayToken=lint-token \
  --set runtimeClassName="")
if grep -q 'runtimeClassName' <<< "$output"; then
  echo "FAIL: runtimeClassName should not appear when set to empty string" >&2; exit 1
fi

# Both built-in harnesses render without a Teams listener or channel.
for agent_type in openclaw hermes; do
  output=$(helm_template "${release_name}" "${chart_dir}" -f "${chart_dir}/values.yaml" \
    --set "agentType=${agent_type}" --set secrets.openclawGatewayToken=test-token)
  # HOME initializers retire saved Teams settings; inspect the remaining
  # configuration and workload wiring independently of those script bodies.
  runtime_output=$(awk '
    /^  (init-home.sh|hermes-apply-config.py): \|/ { skip = 1; next }
    /^  [^[:space:]]|^---/ { skip = 0 }
    !skip { print }
  ' <<< "$output")
  if grep -Ei 'msteams|cetirun_teams|MSTEAMS_|containerPort: 3978' <<< "$runtime_output"; then
    echo "FAIL: ${agent_type} rendered removed Teams integration" >&2; exit 1
  fi
done

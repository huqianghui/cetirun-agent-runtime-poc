#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# deploy-instance.sh — Interactive script to deploy an Agent CR
# to the AKS cluster.
#
# Usage:
#   ./deploy-instance.sh
#   ALIAS=alice DISPLAY_NAME="Alice Smith" EMAIL=alice@example.com TEAM=example-team ./deploy-instance.sh
set -euo pipefail

SUBSCRIPTION="${AZURE_SUBSCRIPTION_ID:?AZURE_SUBSCRIPTION_ID is required}"
RESOURCE_GROUP="${RESOURCE_GROUP:?RESOURCE_GROUP is required}"
CLUSTER_NAME="${AKS_CLUSTER_NAME:?AKS_CLUSTER_NAME is required}"

#-----------------------------------------------------------------------
# Step 1: Obtain admin kubeconfig
#-----------------------------------------------------------------------
echo "=== Step 1: Obtain admin kubeconfig ==="
echo "Cluster: ${CLUSTER_NAME} (RG: ${RESOURCE_GROUP}, Sub: ${SUBSCRIPTION})"

az account set --subscription "${SUBSCRIPTION}"
az aks get-credentials \
  --resource-group "${RESOURCE_GROUP}" \
  --name "${CLUSTER_NAME}" \
  --admin \
  --overwrite-existing

echo "Kubeconfig configured. Current context:"
kubectl config current-context
echo ""

#-----------------------------------------------------------------------
# Step 2: Prompt for instance details (skip if env vars are set)
#-----------------------------------------------------------------------
echo "=== Step 2: Instance details ==="

if [[ -z "${ALIAS:-}" ]]; then
  read -rp "Alias (lowercase, e.g. alice): " ALIAS
fi
if [[ -z "${DISPLAY_NAME:-}" ]]; then
  read -rp "Display name (e.g. Alice Smith): " DISPLAY_NAME
fi
if [[ -z "${EMAIL:-}" ]]; then
  read -rp "Email (e.g. alice@example.com): " EMAIL
fi
if [[ -z "${DOMAIN_SUFFIX:-}" ]]; then
  read -rp "Domain suffix (e.g. example.com): " DOMAIN_SUFFIX
fi
TEAM="${TEAM:-cetirun}"

# Maximum alias length to ensure derived Kubernetes resource names stay within
# the 63-character DNS-label limit. For example:
#   - Namespace: cetirun-${ALIAS}
#   - Pod:       openclaw-${ALIAS}-cetirun-0
# The strictest constraint is from the pod name:
#   len("openclaw-") + len(ALIAS) + len("-cetirun-0") <= 63
#   9 + len(ALIAS) + 11 <= 63  => len(ALIAS) <= 43
MAX_ALIAS_LEN=43

# Validate alias format
if [[ ! "${ALIAS}" =~ ^[a-z][a-z0-9-]{0,62}$ ]]; then
  echo "ERROR: Alias must match ^[a-z][a-z0-9-]{0,62}$"
  exit 1
fi

# Validate alias length against Kubernetes DNS-label constraints for derived names
alias_len=${#ALIAS}
if (( alias_len > MAX_ALIAS_LEN )); then
  echo "ERROR: Alias is too long (${alias_len} characters). With derived names such as 'cetirun-\${ALIAS}' and 'openclaw-\${ALIAS}-cetirun-0', the alias must be at most ${MAX_ALIAS_LEN} characters to satisfy Kubernetes 63-character DNS-label limits."
  exit 1
fi
echo ""
echo "  Alias:        ${ALIAS}"
echo "  Display Name: ${DISPLAY_NAME}"
echo "  Team:         ${TEAM}"
echo "  Email:        ${EMAIL}"
echo ""

#-----------------------------------------------------------------------
# Step 3: Verify operator installation
#-----------------------------------------------------------------------
echo "=== Step 3: Verify Agent CRD is installed ==="
if ! kubectl get crd agents.cetirun.microsoft.com &>/dev/null; then
  echo "ERROR: Agent CRD is not installed. Install the CetiRun Operator chart first." >&2
  exit 1
fi
echo ""

#-----------------------------------------------------------------------
# Step 4: Generate and apply Agent CR
#-----------------------------------------------------------------------
echo "=== Step 4: Deploy Agent '${ALIAS}' ==="

cat <<EOF | kubectl apply -f -
apiVersion: cetirun.microsoft.com/v1alpha1
kind: Agent
metadata:
  name: ${ALIAS}
spec:
  agent:
    type: hermes
  gatewayTokenSecretRef: "gateway-token"
  user:
    alias: "${ALIAS}"
    displayName: "${DISPLAY_NAME}"
    team: "${TEAM}"
    email: "${EMAIL}"
EOF

#-----------------------------------------------------------------------
# Step 5: Wait for Agent to become Ready
#-----------------------------------------------------------------------
echo ""
echo "=== Step 5: Waiting for Agent '${ALIAS}' to become Ready ==="
echo "(This may take several minutes while Helm installs the chart...)"

TIMEOUT=600
INTERVAL=10
ELAPSED=0
while true; do
  PHASE=$(kubectl get agents "${ALIAS}" -o jsonpath='{.status.phase}' 2>/dev/null || true)
  if [[ "${PHASE}" == "Ready" ]]; then
    echo "Agent '${ALIAS}' is Ready!"
    break
  fi
  if [[ "${PHASE}" == "Failed" ]]; then
    echo "ERROR: Agent '${ALIAS}' entered Failed phase."
    kubectl get agents "${ALIAS}" -o yaml
    exit 1
  fi
  if (( ELAPSED >= TIMEOUT )); then
    echo "ERROR: Timed out after ${TIMEOUT}s waiting for Ready (current phase: ${PHASE:-<none>})."
    kubectl get agents "${ALIAS}" -o yaml
    exit 1
  fi
  echo "  Phase: ${PHASE:-<none>} ... (${ELAPSED}s/${TIMEOUT}s)"
  sleep "${INTERVAL}"
  (( ELAPSED += INTERVAL ))
done

INSTANCE_NS="cetirun-${ALIAS}"

#-----------------------------------------------------------------------
# Step 6: Access instructions
#-----------------------------------------------------------------------
GATEWAY_TOKEN=$(kubectl get secret gateway-token -n "${INSTANCE_NS}" \
  -o jsonpath='{.data.OPENCLAW_GATEWAY_TOKEN}' 2>/dev/null | base64 --decode || true)
# URL-encode the token (contains +, = which are special in query strings)
GATEWAY_TOKEN_ENCODED=$(python3 -c "import urllib.parse, sys; print(urllib.parse.quote(sys.stdin.read().strip(), safe=''))" <<< "${GATEWAY_TOKEN}" 2>/dev/null || echo "${GATEWAY_TOKEN}")

# Primary domain using the configured domain suffix.
CETIRUN_HOST="${ALIAS}.${DOMAIN_SUFFIX}"

echo "==========================================="
echo " Deployment Complete!"
echo "==========================================="
echo ""
echo "Agent '${ALIAS}' is Ready."
echo ""
echo "Access your instance at:"
echo ""
if [[ -n "${GATEWAY_TOKEN}" ]]; then
  echo "  https://${CETIRUN_HOST}/?token=${GATEWAY_TOKEN_ENCODED}"
else
  echo "  https://${CETIRUN_HOST}/"
  echo "  (gateway token not found — you will need to paste it manually in Control UI settings)"
fi
echo ""
echo "  NOTE: SSL is not yet configured for ${CETIRUN_HOST}."
echo "        You will see a browser security warning — this is expected."
echo "        Click 'Advanced' → 'Proceed' (Chrome) or 'Accept the Risk' (Firefox)"
echo "        to continue."
echo ""

#-----------------------------------------------------------------------
# Step 7: Approve device pairing
#-----------------------------------------------------------------------
echo "=== Step 7: Approve device pairing ==="
echo "Open the URL above in your browser, then come back here."
echo "OpenClaw requires each new browser/device to be approved before"
echo "the WebSocket connection can be established."
echo ""
echo "Waiting for a pairing request..."

POD_NAME="openclaw-${ALIAS}-cetirun-0"
PAIR_TIMEOUT=120
PAIR_INTERVAL=5
PAIR_ELAPSED=0
REQUEST_ID=""
# The Agent chart's exec restriction (execRestriction.enabled, on by default)
# denies kubectl exec, cluster administrators included; the portal's Terminal
# runs in the Agent and can approve the browser instead.
EXEC_DENIED=""
if ! EXEC_ERROR=$(kubectl exec -n "${INSTANCE_NS}" "${POD_NAME}" -c cetirun -- true 2>&1 >/dev/null) &&
  grep -qE 'denied request|is not allowed' <<<"${EXEC_ERROR}"; then
  EXEC_DENIED=1
  echo "kubectl exec is denied on this instance (execRestriction.enabled)."
  echo "Approve the browser from the instance's Terminal in the portal:"
  echo "  openclaw devices list"
  echo "  openclaw devices approve <requestId>"
fi
while [[ -z "${EXEC_DENIED}" ]]; do
  # Extract UUID-format request IDs from the Pending section only.
  # The Pending table has UUIDs (8-4-4-4-12), the Paired table has plain hex device hashes.
  DEVICES_OUTPUT=$(kubectl exec -n "${INSTANCE_NS}" "${POD_NAME}" -c cetirun -- \
    openclaw devices list 2>/dev/null || true)
  # Get only the Pending section (between "Pending" and "Paired" headers)
  PENDING_SECTION=$(echo "${DEVICES_OUTPUT}" | sed -n '/^Pending/,/^Paired/p')
  REQUEST_ID=$(echo "${PENDING_SECTION}" \
    | grep -oE '[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}' \
    | head -1 || true)
  if [[ -n "${REQUEST_ID}" ]]; then
    echo "  Found pending request: ${REQUEST_ID}"
    break
  fi
  if (( PAIR_ELAPSED >= PAIR_TIMEOUT )); then
    echo "No pairing request detected within ${PAIR_TIMEOUT}s."
    echo "You can approve manually later, from the instance's Terminal in the portal:"
    echo "  openclaw devices list"
    echo "  openclaw devices approve <requestId>"
    echo "or, where kubectl exec is allowed:"
    echo "  kubectl exec -n ${INSTANCE_NS} ${POD_NAME} -c cetirun -- openclaw devices list"
    echo "  kubectl exec -n ${INSTANCE_NS} ${POD_NAME} -c cetirun -- openclaw devices approve <requestId>"
    break
  fi
  echo "  No pending requests yet ... (${PAIR_ELAPSED}s/${PAIR_TIMEOUT}s)"
  sleep "${PAIR_INTERVAL}"
  (( PAIR_ELAPSED += PAIR_INTERVAL ))
done

if [[ -n "${REQUEST_ID}" ]]; then
  kubectl exec -n "${INSTANCE_NS}" "${POD_NAME}" -c cetirun -- \
    openclaw devices approve "${REQUEST_ID}" 2>/dev/null
  echo "Device approved! Refresh your browser — the UI should connect."
fi
echo ""

echo "Check status:"
echo "  kubectl get agents ${ALIAS}"
echo "  kubectl get agents ${ALIAS} -o yaml"

#-----------------------------------------------------------------------
# Step 8: Generate delete-instance-<alias>.sh
#-----------------------------------------------------------------------
DELETE_SCRIPT="${SCRIPT_DIR}/delete-instance-${ALIAS}.sh"
echo "=== Step 8: Generating cleanup script ==="

cat > "${DELETE_SCRIPT}" <<'DELEOF'
#!/usr/bin/env bash
# Auto-generated by deploy-instance.sh — deletes all resources for this instance.
set -euo pipefail
DELEOF

cat >> "${DELETE_SCRIPT}" <<DELEOF
ALIAS="${ALIAS}"
INSTANCE_NS="${INSTANCE_NS}"
SUBSCRIPTION="${SUBSCRIPTION}"
RESOURCE_GROUP="${RESOURCE_GROUP}"
CLUSTER_NAME="${CLUSTER_NAME}"
DELEOF

cat >> "${DELETE_SCRIPT}" <<'DELEOF'
az account set --subscription "${SUBSCRIPTION}"
az aks get-credentials \
  --resource-group "${RESOURCE_GROUP}" \
  --name "${CLUSTER_NAME}" \
  --admin \
  --overwrite-existing

# 1. Delete Agent CR (operator finalizer cleans up namespace,
#    secrets, Helm release, VirtualService)
echo "=== Delete Agent '${ALIAS}' ==="
if kubectl get agent "${ALIAS}" &>/dev/null; then
  kubectl delete agent "${ALIAS}"
else
  echo "Agent '${ALIAS}' not found — skipping."
fi

# 2. Wait for namespace cleanup
echo "=== Waiting for namespace '${INSTANCE_NS}' to be deleted ==="
TIMEOUT=120; INTERVAL=5; ELAPSED=0
while kubectl get namespace "${INSTANCE_NS}" &>/dev/null; do
  if (( ELAPSED >= TIMEOUT )); then
    echo "WARNING: Namespace still exists after ${TIMEOUT}s. Check operator logs."
    break
  fi
  echo "  Waiting... (${ELAPSED}s/${TIMEOUT}s)"
  sleep "${INTERVAL}"
  (( ELAPSED += INTERVAL ))
done
if ! kubectl get namespace "${INSTANCE_NS}" &>/dev/null; then
  echo "Namespace '${INSTANCE_NS}' deleted."
fi

echo ""
echo "Cleanup complete for '${ALIAS}'."
# Self-delete
rm -f "$0"
DELEOF

chmod +x "${DELETE_SCRIPT}"
echo "Generated: ${DELETE_SCRIPT}"
echo ""
echo "To tear down this instance later, run:"
echo "  ${DELETE_SCRIPT}"

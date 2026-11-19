#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BICEP_FILE="${SCRIPT_DIR}/main.bicep"
PARAM_FILE="${PARAM_FILE:-${SCRIPT_DIR}/main.bicepparam}"
DEPLOYMENT_NAME="cetirun-cluster-$(date +%Y%m%d%H%M%S)"
# Set to true only for an explicitly authorized bootstrap environment where
# local AKS admin accounts remain enabled. Default user credentials preserve
# the normal Azure RBAC path.
AKS_ADMIN_CREDENTIALS="${AKS_ADMIN_CREDENTIALS:-false}"

RG_NAME="${RESOURCE_GROUP:?RESOURCE_GROUP is required}"
LOCATION="${LOCATION:?LOCATION is required}"

# Check the effective version before extension, feature, or resource writes.
# Deploy these same compiled inputs even if the source changes while waiting
# for Azure feature registration. The private directory can contain secrets.
DEPLOYMENT_INPUT_DIR="$(mktemp -d)"
trap 'rm -rf -- "$DEPLOYMENT_INPUT_DIR"' EXIT
python3 "${SCRIPT_DIR}/../../scripts/validate-cluster-kubernetes.py" \
  --template "$BICEP_FILE" --parameters "$PARAM_FILE" --output-dir "$DEPLOYMENT_INPUT_DIR"

AGENT_SETTINGS_NAME="container-azm-ms-agentconfig"
AGENT_SETTINGS_FILE="${SCRIPT_DIR}/${AGENT_SETTINGS_NAME}.yaml"

# agent_setting_is SECTION VALUE: succeeds when the Container Insights agent
# settings on stdin set "enabled = VALUE" in [SECTION].
agent_setting_is() {
  awk -v want_section="$1" -v want_value="$2" '
    /^[[:space:]]*\[/ { section = $0; gsub(/[][[:space:]]/, "", section); next }
    section == want_section && /^[[:space:]]*enabled[[:space:]]*=/ {
      value = $0
      sub(/^[^=]*=[[:space:]]*/, "", value)
      sub(/[[:space:]]*(#.*)?$/, "", value)
      found = (value == want_value)
    }
    END { exit found ? 0 : 1 }
  '
}

# Pre-flight: ensure aks-preview extension is installed and up-to-date
# --enable-gateway-api requires aks-preview >= 19.0.0b4
az extension add --name aks-preview --upgrade --yes 2>/dev/null \
  || { echo "Error: failed to install/update aks-preview extension"; exit 1; }

# Pre-flight: register Gateway API feature flag if needed
GATEWAY_STATE="$(az feature show --namespace Microsoft.ContainerService \
  --name ManagedGatewayAPIPreview --query 'properties.state' -o tsv 2>/dev/null || echo 'NotRegistered')"
if [[ "$GATEWAY_STATE" != "Registered" ]]; then
  echo "Registering ManagedGatewayAPIPreview feature flag..."
  az feature register --namespace Microsoft.ContainerService --name ManagedGatewayAPIPreview
  az provider register --namespace Microsoft.ContainerService
  echo "Waiting for feature registration..."
  while [[ "$(az feature show --namespace Microsoft.ContainerService \
    --name ManagedGatewayAPIPreview --query 'properties.state' -o tsv)" != "Registered" ]]; do
    sleep 30
  done
fi

# Create resource group (use existing location if RG already exists)
RG_LOCATION=$(az group show --name "$RG_NAME" --query location -o tsv 2>/dev/null || echo "$LOCATION")
az group create --name "$RG_NAME" --location "$RG_LOCATION" --tags project=cetirun

# Validate
az deployment group validate \
  --resource-group "$RG_NAME" \
  --template-file "${DEPLOYMENT_INPUT_DIR}/template.json" \
  --parameters "@${DEPLOYMENT_INPUT_DIR}/parameters.json" \
  --name "${DEPLOYMENT_NAME}-validate"

# Deploy
az deployment group create \
  --resource-group "$RG_NAME" \
  --template-file "${DEPLOYMENT_INPUT_DIR}/template.json" \
  --parameters "@${DEPLOYMENT_INPUT_DIR}/parameters.json" \
  --name "$DEPLOYMENT_NAME"

# Read cluster name from outputs
CLUSTER_NAME="$(az deployment group show \
  --resource-group "$RG_NAME" \
  --name "$DEPLOYMENT_NAME" \
  --query 'properties.outputs.clusterName.value' -o tsv)"

# Fetch kubeconfig
credential_args=()
if [[ "${AKS_ADMIN_CREDENTIALS}" == "true" ]]; then
  credential_args+=(--admin)
fi
az aks get-credentials \
  --resource-group "$RG_NAME" \
  --name "$CLUSTER_NAME" \
  "${credential_args[@]}" \
  --overwrite-existing

# Container Insights agent settings, in place before anything else runs on the
# cluster: the agent stops collecting containers' environment variables, which
# hold credentials, and joins log lines the container runtime split. Set up
# whether or not Container Insights is on, so turning it on later is safe too.
#
# A cluster that already has agent settings keeps them. They are checked, not
# replaced: replacing them would drop the cluster's own collection settings,
# such as namespaces it excludes, for every workload.
if ! existing_settings="$(kubectl get configmap "${AGENT_SETTINGS_NAME}" -n kube-system --ignore-not-found -o name)"; then
  echo "Error: could not read the Container Insights agent settings in kube-system." >&2
  exit 1
fi
if [[ -z "${existing_settings}" ]]; then
  if ! kubectl create -f "${AGENT_SETTINGS_FILE}"; then
    echo "Error: could not create the Container Insights agent settings in kube-system." >&2
    echo "Until they exist, the agent collects every container's environment, credentials included." >&2
    echo "Create them from ${AGENT_SETTINGS_FILE} with an identity that can write to kube-system before installing CetiRun." >&2
    exit 1
  fi
else
  if ! log_settings="$(kubectl get configmap "${AGENT_SETTINGS_NAME}" -n kube-system \
    -o 'jsonpath={.data.log-data-collection-settings}')"; then
    echo "Error: could not read kube-system/${AGENT_SETTINGS_NAME}." >&2
    exit 1
  fi
  if ! agent_setting_is log_collection_settings.env_var false <<< "${log_settings}"; then
    echo "Error: kube-system/${AGENT_SETTINGS_NAME} already exists and does not turn off environment variable collection." >&2
    echo "It was left unchanged, since replacing it would drop the cluster's own collection settings." >&2
    echo "Add these settings to its log-data-collection-settings, as in ${AGENT_SETTINGS_FILE}, then run this script again:" >&2
    echo "  [log_collection_settings.env_var]" >&2
    echo "     enabled = false" >&2
    echo "  [log_collection_settings.enable_multiline_logs]" >&2
    echo "     enabled = true" >&2
    exit 1
  fi
  echo "kube-system/${AGENT_SETTINGS_NAME} already exists and turns off environment variable collection; left unchanged."
  if ! agent_setting_is log_collection_settings.enable_multiline_logs true <<< "${log_settings}"; then
    echo "Note: it does not turn on multiline logs, so lines longer than 16 KiB are stored in pieces." >&2
  fi
fi

# Enable Gateway API
az aks update \
  --resource-group "$RG_NAME" \
  --name "$CLUSTER_NAME" \
  --enable-gateway-api \
  --yes

# Verify
kubectl get nodes -o wide
kubectl get pods -n aks-istio-system
kubectl get pods -n aks-istio-ingress
# The Azure CLI spells the property issuerUrl; the ARM name in the Bicep
# template is issuerURL. JMESPath is case-sensitive, so the ARM spelling
# prints an empty line.
az aks show --resource-group "$RG_NAME" --name "$CLUSTER_NAME" \
  --query 'oidcIssuerProfile.issuerUrl' -o tsv

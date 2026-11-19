#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BICEP_FILE="${SCRIPT_DIR}/main.bicep"
PARAM_FILE="${PARAM_FILE:-${SCRIPT_DIR}/main.bicepparam}"
DEPLOYMENT_NAME="cetirun-storage-$(date +%Y%m%d%H%M%S)"

RG_NAME="${RESOURCE_GROUP:?RESOURCE_GROUP is required}"
AKS_CLUSTER_NAME="${AKS_CLUSTER_NAME:?AKS_CLUSTER_NAME is required}"

# Optional private endpoint for the blob service. With one, the account takes no
# subnet service-endpoint rule from this helper.
PRIVATE_ENDPOINT_SUBNET_ID="${PRIVATE_ENDPOINT_SUBNET_ID:-}"
PRIVATE_DNS_ZONE_ID="${PRIVATE_DNS_ZONE_ID:-}"
PUBLIC_NETWORK_ACCESS="${PUBLIC_NETWORK_ACCESS:-Enabled}"
case "$PUBLIC_NETWORK_ACCESS" in
  Enabled|Disabled) ;;
  *)
    echo "ERROR: PUBLIC_NETWORK_ACCESS must be Enabled or Disabled." >&2
    exit 1
    ;;
esac
if [[ -n "$PRIVATE_DNS_ZONE_ID" && -z "$PRIVATE_ENDPOINT_SUBNET_ID" ]]; then
  echo "ERROR: PRIVATE_DNS_ZONE_ID requires PRIVATE_ENDPOINT_SUBNET_ID." >&2
  exit 1
fi
if [[ "$PUBLIC_NETWORK_ACCESS" == Disabled && -z "$PRIVATE_ENDPOINT_SUBNET_ID" ]]; then
  echo "ERROR: PUBLIC_NETWORK_ACCESS=Disabled requires PRIVATE_ENDPOINT_SUBNET_ID: without a private endpoint, no client can reach the account." >&2
  exit 1
fi

PARAMETERS=(
  --parameters "$PARAM_FILE"
  --parameters publicNetworkAccess="$PUBLIC_NETWORK_ACCESS"
)

if [[ -n "$PRIVATE_ENDPOINT_SUBNET_ID" ]]; then
  echo "Using a private endpoint in subnet: $PRIVATE_ENDPOINT_SUBNET_ID"
  PARAMETERS+=(--parameters privateEndpointSubnetId="$PRIVATE_ENDPOINT_SUBNET_ID")
  if [[ -n "$PRIVATE_DNS_ZONE_ID" ]]; then
    PARAMETERS+=(--parameters privateDnsZoneId="$PRIVATE_DNS_ZONE_ID")
  fi
else
  # Resolve AKS node subnet ID
  if [[ -z "${AKS_SUBNET_ID:-}" ]]; then
    echo "AKS_SUBNET_ID not set, detecting from cluster '$AKS_CLUSTER_NAME'..."

    # Try agentPoolProfiles first (works for custom VNet clusters)
    AKS_SUBNET_ID="$(az aks show \
      --resource-group "$RG_NAME" \
      --name "$AKS_CLUSTER_NAME" \
      --query 'agentPoolProfiles[0].vnetSubnetId' -o tsv 2>/dev/null)"

    # Fallback for CNI Overlay: find subnet in node resource group
    if [[ -z "$AKS_SUBNET_ID" || "$AKS_SUBNET_ID" == "None" || "$AKS_SUBNET_ID" == "null" ]]; then
      echo "vnetSubnetId not set (CNI Overlay), looking up from node resource group..."
      NODE_RG="$(az aks show -g "$RG_NAME" -n "$AKS_CLUSTER_NAME" --query nodeResourceGroup -o tsv 2>/dev/null)"
      AKS_SUBNET_ID="$(az network vnet list -g "$NODE_RG" \
        --query '[0].subnets[?name==`aks-subnet`].id | [0]' -o tsv 2>/dev/null)"
    fi

    if [[ -z "$AKS_SUBNET_ID" || "$AKS_SUBNET_ID" == "None" ]]; then
      echo "ERROR: Could not detect subnet ID. Set AKS_SUBNET_ID manually." >&2
      exit 1
    fi
    echo "Detected subnet: $AKS_SUBNET_ID"
  fi

  # Ensure Microsoft.Storage service endpoint is enabled on the subnet (required for NFS v3)
  echo "Ensuring Microsoft.Storage service endpoint on subnet..."
  SUBNET_NAME="$(basename "$AKS_SUBNET_ID")"
  VNET_ID="$(echo "$AKS_SUBNET_ID" | sed 's|/subnets/.*||')"
  VNET_NAME="$(basename "$VNET_ID")"
  SUBNET_RG="$(echo "$AKS_SUBNET_ID" | grep -oP 'resourceGroups/\K[^/]+')"
  az network vnet subnet update \
    --resource-group "$SUBNET_RG" \
    --vnet-name "$VNET_NAME" \
    --name "$SUBNET_NAME" \
    --service-endpoints Microsoft.Storage \
    -o none

  PARAMETERS+=(--parameters allowedSubnetIds="[\"${AKS_SUBNET_ID}\"]")
fi

# Validate
az deployment group validate \
  --resource-group "$RG_NAME" \
  --template-file "$BICEP_FILE" \
  "${PARAMETERS[@]}" \
  --name "${DEPLOYMENT_NAME}-validate"

# Deploy
az deployment group create \
  --resource-group "$RG_NAME" \
  --template-file "$BICEP_FILE" \
  "${PARAMETERS[@]}" \
  --name "$DEPLOYMENT_NAME"

# Read outputs
STORAGE_ACCOUNT="$(az deployment group show \
  --resource-group "$RG_NAME" \
  --name "$DEPLOYMENT_NAME" \
  --query 'properties.outputs.storageAccountName.value' -o tsv)"

CONTAINER_NAME="$(az deployment group show \
  --resource-group "$RG_NAME" \
  --name "$DEPLOYMENT_NAME" \
  --query 'properties.outputs.containerName.value' -o tsv)"

BLOB_ENDPOINT="$(az deployment group show \
  --resource-group "$RG_NAME" \
  --name "$DEPLOYMENT_NAME" \
  --query 'properties.outputs.primaryBlobEndpoint.value' -o tsv)"

# Verify
az storage account show --name "$STORAGE_ACCOUNT" --resource-group "$RG_NAME" -o table
if [[ "$PUBLIC_NETWORK_ACCESS" == Disabled ]]; then
  # Only the private endpoint reaches the data plane: read the container through Azure Resource Manager.
  az storage container-rm show --name "$CONTAINER_NAME" --storage-account "$STORAGE_ACCOUNT" --resource-group "$RG_NAME" -o table
else
  az storage container show --name "$CONTAINER_NAME" --account-name "$STORAGE_ACCOUNT" --auth-mode login -o table
fi

echo ""
echo "=== Deployment Summary ==="
echo "Storage Account: $STORAGE_ACCOUNT"
echo "Container:       $CONTAINER_NAME"
echo "Blob Endpoint:   $BLOB_ENDPOINT"
echo "Resource Group:  $RG_NAME"

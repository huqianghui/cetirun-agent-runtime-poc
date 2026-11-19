#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# CetiRun Istio Authentication Infrastructure — Helm-based deployment.
# Installs the cetirun-istio Helm chart and patches the Istio mesh config
# with the ext_authz extension provider (which cannot be Helm-templated
# because it merges into a configmap managed by the AKS Istio addon).
#
# Prerequisites:
#   - AKS cluster with Istio addon enabled and external ingress gateway running
#   - kubectl context pointing to the target cluster
#   - helm, az, jq, kubectl, and Python with PyYAML installed
#   - Entra ID app registration created (see README.md)
#
# Required environment variables:
#   AZURE_SUBSCRIPTION_ID  — Azure subscription ID containing the AKS cluster
#   CLIENT_ID              — Entra ID app registration client/application ID
#   TENANT_ID              — Azure AD tenant ID
#   ALLOWED_GROUPS         — Comma-separated Entra ID group object IDs (e.g. "oid1,oid2")
#
# Optional environment variables:
#   COOKIE_SECRET          — Pre-generated cookie secret (auto-generated if not set)
#   ADMIN_ALIASES           — Comma-separated list of admin aliases (can access portal /admin)
#   TLS_CERT_PATH          — Path to TLS certificate file (PEM; skips PFX parsing entirely)
#   TLS_KEY_PATH           — Path to TLS private key file (PEM; skips PFX parsing entirely)
#   PFX_BASE64             — Base64-encoded PFX certificate content (decoded and parsed into cert/key)
#   PFX_PATH               — Path to PFX file (skips Key Vault download, still parses into cert/key)
#   KEY_VAULT_NAME         — Key Vault name for TLS cert (required)
#   KEY_VAULT_CERT_NAME    — Certificate name in Key Vault (default: sslcert)
#   AKS_CLUSTER_NAME       — Cluster name (required)
#   RESOURCE_GROUP         — Resource group (required)
#   RELEASE_NAME           — Helm release name (default: cetirun-istio)
#   DOMAIN                 — Wildcard domain for CetiRun (required)
#   PORTAL_AUTH_NAMESPACE  — Namespace of the portal auth service (default: cetirun-portal)
#   PORTAL_AUTH_SERVICE    — Name of the portal auth service (default: cetirun-portal)
#   PORTAL_AUTH_CLUSTER_HOST — Explicit portal auth service FQDN (overrides namespace/service)
#   SKIP_AZ_VALIDATION     — If non-empty, skip az CLI checks (for CI with pre-provided certs)
#
# Usage:
#   export AZURE_SUBSCRIPTION_ID="<subscription-id>"
#   export CLIENT_ID="<app-client-id>"
#   export TENANT_ID="<tenant-id>"
#   export ALLOWED_GROUPS="<group-oid-1>,<group-oid-2>"
#   ./deploy.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

#-----------------------------------------------------------------------
# Validate required environment variables
#-----------------------------------------------------------------------
if [[ -z "${SKIP_AZ_VALIDATION:-}" ]]; then
  : "${AZURE_SUBSCRIPTION_ID:?AZURE_SUBSCRIPTION_ID must be set to the Azure subscription ID}"
fi
: "${CLIENT_ID:?CLIENT_ID must be set to the Entra app client ID}"
: "${TENANT_ID:?TENANT_ID must be set to the Azure AD tenant ID}"
: "${ALLOWED_GROUPS:?ALLOWED_GROUPS must be set to comma-separated Entra group object IDs}"

REQUIRED_CMDS=(helm kubectl jq openssl python3)
if [[ -z "${SKIP_AZ_VALIDATION:-}" ]]; then
  REQUIRED_CMDS+=(az)
fi
for CMD in "${REQUIRED_CMDS[@]}"; do
  if ! command -v "$CMD" &>/dev/null; then
    echo "ERROR: ${CMD} is required but not installed." >&2
    exit 1
  fi
done
if ! python3 -c "import yaml" 2>/dev/null; then
  echo "ERROR: PyYAML is required. Install infra/istio/requirements.txt." >&2
  exit 1
fi

AKS_CLUSTER_NAME="${AKS_CLUSTER_NAME:?AKS_CLUSTER_NAME is required}"
RESOURCE_GROUP="${RESOURCE_GROUP:?RESOURCE_GROUP is required}"
RELEASE_NAME="${RELEASE_NAME:-cetirun-istio}"
DOMAIN="${DOMAIN:?DOMAIN is required}"
KEY_VAULT_NAME="${KEY_VAULT_NAME:?KEY_VAULT_NAME is required}"
KEY_VAULT_CERT_NAME="${KEY_VAULT_CERT_NAME:-sslcert}"
PORTAL_AUTH_NAMESPACE="${PORTAL_AUTH_NAMESPACE:-cetirun-portal}"
PORTAL_AUTH_SERVICE="${PORTAL_AUTH_SERVICE:-cetirun-portal}"
PORTAL_AUTH_CLUSTER_HOST="${PORTAL_AUTH_CLUSTER_HOST:-${PORTAL_AUTH_SERVICE}.${PORTAL_AUTH_NAMESPACE}.svc.cluster.local}"
if [[ -z "${PORTAL_AUTH_CLUSTER_HOST}" ]]; then
  echo "ERROR: PORTAL_AUTH_CLUSTER_HOST must not be empty." >&2
  exit 1
fi

#-----------------------------------------------------------------------
# Set subscription and validate cluster context
#-----------------------------------------------------------------------
if [[ -z "${SKIP_AZ_VALIDATION:-}" ]]; then
  echo "Setting Azure subscription: ${AZURE_SUBSCRIPTION_ID}"
  if ! az account set --subscription "${AZURE_SUBSCRIPTION_ID}" 2>/dev/null; then
    echo "ERROR: Failed to set subscription '${AZURE_SUBSCRIPTION_ID}'." >&2
    echo "       Ensure you are logged in with 'az login' and have access to this subscription." >&2
    exit 1
  fi

  if ! az aks show -g "${RESOURCE_GROUP}" -n "${AKS_CLUSTER_NAME}" &>/dev/null; then
    echo "ERROR: AKS cluster '${AKS_CLUSTER_NAME}' in resource group '${RESOURCE_GROUP}' not found." >&2
    echo "       Ensure AKS_CLUSTER_NAME and RESOURCE_GROUP are set correctly." >&2
    exit 1
  fi

  CURRENT_CONTEXT="$(kubectl config current-context 2>/dev/null || true)"
  if [[ -z "${CURRENT_CONTEXT}" ]]; then
    echo "ERROR: No current kubectl context is set." >&2
    exit 1
  fi

  if [[ "${CURRENT_CONTEXT}" != *"${AKS_CLUSTER_NAME}"* ]]; then
    echo "ERROR: kubectl context ('${CURRENT_CONTEXT}') does not appear to target '${AKS_CLUSTER_NAME}'." >&2
    echo "       Update with: az aks get-credentials -g '${RESOURCE_GROUP}' -n '${AKS_CLUSTER_NAME}' --overwrite-existing" >&2
    exit 1
  fi
else
  echo "SKIP_AZ_VALIDATION set — skipping Azure CLI and context checks."
  CURRENT_CONTEXT="$(kubectl config current-context 2>/dev/null || echo 'ci')"
fi

echo "==========================================="
echo " CetiRun Istio (Auth + Egress) — Helm Deploy"
echo "==========================================="
echo ""
if [[ -z "${SKIP_AZ_VALIDATION:-}" ]]; then
  echo "Cluster:        ${AKS_CLUSTER_NAME}"
  echo "Resource Group: ${RESOURCE_GROUP}"
fi
echo "kubectl ctx:    ${CURRENT_CONTEXT}"
echo "Release:        ${RELEASE_NAME}"
echo "Portal auth:    ${PORTAL_AUTH_CLUSTER_HOST}:8081"
echo ""

#-----------------------------------------------------------------------
# TLS certificate resolution
#
# Priority:
#   1. TLS_CERT_PATH + TLS_KEY_PATH already set → use as-is (PEM files)
#   2. PFX_BASE64 set                           → decode base64, parse PFX into cert/key
#   3. PFX_PATH set                             → parse PFX into cert/key
#   4. Neither                                  → download PFX from Key Vault, then parse
#-----------------------------------------------------------------------
TLS_TMPDIR=""
cleanup_tls_tmp() {
  if [[ -n "${TLS_TMPDIR}" && -d "${TLS_TMPDIR}" ]]; then
    rm -rf "${TLS_TMPDIR}"
  fi
}
trap cleanup_tls_tmp EXIT

# parse_pfx <pfx_file> — extracts leaf cert + ordered CA chain + private key
# Reuses TLS_TMPDIR if already set; otherwise creates a new temp dir.
# Sets TLS_CERT_PATH and TLS_KEY_PATH on success.
parse_pfx() {
  local pfx="$1"
  if [[ -z "${TLS_TMPDIR}" ]]; then
    TLS_TMPDIR=$(mktemp -d)
  fi
  TLS_CERT_PATH="${TLS_TMPDIR}/tls.crt"
  TLS_KEY_PATH="${TLS_TMPDIR}/tls.key"

  # Extract leaf certificate (strip Bag Attributes for clean PEM).
  openssl pkcs12 -in "${pfx}" -clcerts -nokeys -passin pass: 2>/dev/null \
    | sed -n '/-----BEGIN CERTIFICATE-----/,/-----END CERTIFICATE-----/p' > "${TLS_CERT_PATH}"
  # Extract private key.
  openssl pkcs12 -in "${pfx}" -nocerts -nodes -passin pass: 2>/dev/null \
    | sed -n '/-----BEGIN PRIVATE KEY-----/,/-----END PRIVATE KEY-----/p' > "${TLS_KEY_PATH}"

  # Extract CA certs and append them in chain order (issuer → subject walk).
  openssl pkcs12 -in "${pfx}" -cacerts -nokeys -passin pass: 2>/dev/null \
    | sed -n '/-----BEGIN CERTIFICATE-----/,/-----END CERTIFICATE-----/p' > "${TLS_TMPDIR}/ca.pem"
  if [[ -s "${TLS_TMPDIR}/ca.pem" ]]; then
    awk '/-----BEGIN CERTIFICATE-----/{n++; f="'"${TLS_TMPDIR}"'/ca-"n".pem"} f{print > f}' "${TLS_TMPDIR}/ca.pem"
    local NEXT FOUND SUBJ ISSUER
    NEXT=$(openssl x509 -in "${TLS_CERT_PATH}" -noout -issuer -nameopt RFC2253 2>/dev/null | sed 's/^issuer=//')
    for _ in 1 2 3 4 5; do
      [[ -z "$NEXT" ]] && break
      FOUND=""
      for ca_file in "${TLS_TMPDIR}"/ca-*.pem; do
        [[ -f "$ca_file" ]] || continue
        SUBJ=$(openssl x509 -in "$ca_file" -noout -subject -nameopt RFC2253 2>/dev/null | sed 's/^subject=//')
        if [[ "$SUBJ" == "$NEXT" ]]; then
          cat "$ca_file" >> "${TLS_CERT_PATH}"
          ISSUER=$(openssl x509 -in "$ca_file" -noout -issuer -nameopt RFC2253 2>/dev/null | sed 's/^issuer=//')
          rm -f "$ca_file"
          if [[ "$ISSUER" == "$SUBJ" ]]; then NEXT=""; else NEXT="$ISSUER"; fi
          FOUND=1; break
        fi
      done
      [[ -z "$FOUND" ]] && break
    done
  fi

  if [[ ! -s "${TLS_CERT_PATH}" || ! -s "${TLS_KEY_PATH}" ]]; then
    echo "ERROR: Failed to extract certificate or key from PFX '${pfx}'." >&2
    exit 1
  fi
}

if [[ -n "${TLS_CERT_PATH:-}" && -n "${TLS_KEY_PATH:-}" ]]; then
  echo "Using TLS certificate from provided paths."

elif [[ -n "${TLS_CERT_PATH:-}" || -n "${TLS_KEY_PATH:-}" ]]; then
  echo "ERROR: Both TLS_CERT_PATH and TLS_KEY_PATH must be set together (got only one)." >&2
  exit 1

elif [[ -n "${PFX_BASE64:-}" ]]; then
  echo "--- Decoding PFX from base64 content ---"
  TLS_TMPDIR=$(mktemp -d)
  PFX_PATH="${TLS_TMPDIR}/cert.pfx"
  echo "${PFX_BASE64}" | base64 -d > "${PFX_PATH}"
  parse_pfx "${PFX_PATH}"
  echo "Certificate and key extracted from PFX content successfully."

elif [[ -n "${PFX_PATH:-}" ]]; then
  echo "--- Parsing PFX from provided path: ${PFX_PATH} ---"
  parse_pfx "${PFX_PATH}"
  echo "Certificate and key extracted from PFX successfully."

else
  if [[ -n "${SKIP_AZ_VALIDATION:-}" ]]; then
    echo "ERROR: SKIP_AZ_VALIDATION is set but no certificate material provided." >&2
    echo "       Set TLS_CERT_PATH+TLS_KEY_PATH, PFX_BASE64, or PFX_PATH when skipping az validation." >&2
    exit 1
  fi
  echo "--- Downloading TLS certificate from Key Vault ---"
  echo "Key Vault:   ${KEY_VAULT_NAME}"
  echo "Certificate: ${KEY_VAULT_CERT_NAME}"

  TLS_TMPDIR=$(mktemp -d)
  PFX_PATH="${TLS_TMPDIR}/cert.pfx"

  az keyvault secret download \
    --vault-name "${KEY_VAULT_NAME}" \
    --name "${KEY_VAULT_CERT_NAME}" \
    --file "${PFX_PATH}" \
    --encoding base64

  parse_pfx "${PFX_PATH}"
  echo "Certificate and key extracted successfully."
fi

#-----------------------------------------------------------------------
# Helm install / upgrade
#-----------------------------------------------------------------------
HELM_ARGS=(
  upgrade --install "${RELEASE_NAME}" "${SCRIPT_DIR}"
  --namespace cetirun-istio --create-namespace
  --set "clientId=${CLIENT_ID}"
  --set "tenantId=${TENANT_ID}"
  --set "allowedGroups=${ALLOWED_GROUPS//,/\\,}"
  --set "domain=${DOMAIN}"
  --set-string "portalAuth.clusterHost=${PORTAL_AUTH_CLUSTER_HOST}"
)

# Only pass cookieSecret when explicitly provided via environment variable.
# This allows the chart to reuse the existing secret on upgrades, avoiding
# session invalidation from secret rotation.
if [[ -n "${COOKIE_SECRET:-}" ]]; then
  echo "Using explicitly provided COOKIE_SECRET."
  HELM_ARGS+=(--set "cookieSecret=${COOKIE_SECRET}")
else
  echo "COOKIE_SECRET not set; chart will reuse existing secret or generate one."
fi

if [[ -n "${TLS_CERT_PATH:-}" && -n "${TLS_KEY_PATH:-}" ]]; then
  echo "TLS: cert=${TLS_CERT_PATH} key=${TLS_KEY_PATH}"
  HELM_ARGS+=(
    --set "tls.enabled=true"
    --set-file "tls.cert=${TLS_CERT_PATH}"
    --set-file "tls.key=${TLS_KEY_PATH}"
  )
else
  echo "WARNING: No TLS certificate available. Gateway will not have HTTPS." >&2
fi

# Pass admin aliases as individual Helm --set-string entries.
if [[ -n "${ADMIN_ALIASES:-}" ]]; then
  IFS=',' read -ra _ADMIN_LIST <<< "${ADMIN_ALIASES}"
  _IDX=0
  for _ALIAS in "${_ADMIN_LIST[@]}"; do
    _ALIAS="$(echo "${_ALIAS}" | xargs)"  # trim whitespace
    [[ -z "${_ALIAS}" ]] && continue
    HELM_ARGS+=(--set-string "authPolicy.adminAliases[$_IDX]=${_ALIAS}")
    _IDX=$((_IDX + 1))
  done
  echo "Admin aliases configured: ${_IDX} entry/entries."
fi

echo "--- Helm install/upgrade ---"
helm "${HELM_ARGS[@]}"
echo ""

echo "Waiting for OAuth2 Proxy rollout..."
kubectl rollout status deployment/oauth2-proxy -n cetirun-istio --timeout=120s
echo "OAuth2 Proxy ready."

#-----------------------------------------------------------------------
# Patch ext_authz extension provider in Istio mesh config
# (Cannot be Helm-templated — merges into AKS Istio addon managed configmap)
#-----------------------------------------------------------------------
echo ""
echo "--- ext_authz extension provider ---"

REVISION=$(kubectl get pods -n aks-istio-system -l app=istiod \
  -o jsonpath='{.items[0].metadata.labels.istio\.io/rev}' 2>/dev/null || echo "")

if [[ -z "$REVISION" ]]; then
  echo "ERROR: Could not detect Istio revision. Is the Istio addon enabled?" >&2
  exit 1
fi

CONFIGMAP="istio-shared-configmap-${REVISION}"
echo "Istio revision: ${REVISION}"
echo "ConfigMap: ${CONFIGMAP} (aks-istio-system)"

# Provider creation and header reconciliation share one resourceVersion CAS.
# On a conflict the helper reloads the ConfigMap and recomputes the complete
# change, so an AKS addon or administrator update cannot be overwritten by a
# stale mesh snapshot.
echo "Reconciling the oauth2-proxy extension provider..."
python3 "${SCRIPT_DIR}/../../scripts/ensure-oauth2-proxy-id-token-header.py" \
  --namespace aks-istio-system \
  --name "${CONFIGMAP}"
echo "ext_authz provider configured."

#-----------------------------------------------------------------------
# Summary
#-----------------------------------------------------------------------
echo ""
echo "==========================================="
echo " Deployment Complete!"
echo "==========================================="
echo ""
echo "Helm release: ${RELEASE_NAME} (namespace: cetirun-istio)"
echo ""
echo "Deployed (ingress auth):"
echo "  - OAuth2 Proxy (cetirun-istio)"
echo "  - Istio Gateway (aks-istio-ingress): *.${DOMAIN}"
echo "  - OAuth2 Proxy VirtualService: oauth2.${DOMAIN}"
echo "  - AuthorizationPolicy: wildcard for *.${DOMAIN}"
echo "  - ext_authz provider: ${CONFIGMAP}"
echo "  - EnvoyFilters: redirect-header, alias-check, security-headers"
echo ""
echo "Deployed (egress audit):"
echo "  - ServiceEntry: external services (aks-istio-system)"
echo "  - EnvoyFilter: JSON access logging on all sidecars"
echo "  - Agent egress enforcement is configured through AgentPolicy, not a shared blacklist"
echo ""
echo "Exempt from OAuth2 Proxy (no auth):"
echo "  - oauth2.${DOMAIN}"
echo "  - llmapi.${DOMAIN}"
echo ""
ADMIN_COUNT=$(helm get values "${RELEASE_NAME}" -n cetirun-istio -o json 2>/dev/null \
  | jq -r '.authPolicy.adminAliases // [] | length' 2>/dev/null || echo "0")
if [[ "${ADMIN_COUNT}" -gt 0 ]]; then
  echo "Admin aliases (portal /admin access): ${ADMIN_COUNT} configured"
  echo ""
fi
echo "Manage with Helm:"
echo "  helm status ${RELEASE_NAME} -n cetirun-istio"
echo "  helm upgrade ${RELEASE_NAME} ${SCRIPT_DIR} -n cetirun-istio --reset-values -f <reviewed-values.yaml>"
echo "  helm uninstall ${RELEASE_NAME} -n cetirun-istio"
echo "Older kept blacklist filters require explicit ownership-checked cleanup."
echo "See scripts/cetirun-istio-cleanup.py --help and the Istio operations guide."

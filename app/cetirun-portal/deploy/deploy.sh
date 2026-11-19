#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# deploy.sh — One-command deploy for CetiRun Portal.
#
# Installs the portal from the OCI chart published in ACR.
# Every step is idempotent and safe to re-run.
#
# Requires: kubectl and helm configured to target the correct cluster,
#           with pull access to the ACR registry.
#
# Environment variables:
#   REGISTRY             — OCI registry prefix   (default: public CetiRun MCR prefix)
#   VERSION              — Shared chart/image version (default: source release)
#   CHART_VERSION        — Chart version          (default: ...)
#   NAMESPACE            — Target namespace       (default: cetirun-portal)
#   RELEASE_NAME         — Helm release name      (default: cetirun-portal)
#   LOCAL_CHART          — If non-empty, use local chart at ../chart instead of OCI
#   IMAGE_REPO           — Override image.repository Helm value
#   IMAGE_TAG            — Override image.tag Helm value
#   IMAGE_DIGEST         — Override image.digest Helm value (e.g. "sha256:abc123...")
#   ISTIO_HOST           — Override istio.host Helm value
#   ISTIO_LEGACY_HOST    — Optional legacy hostname to 301-redirect to ISTIO_HOST (e.g. portal.<domain>)
#   NOTICE_BANNER        — Optional notice banner message shown in the portal UI
#   MAX_INSTANCES_PER_USER — Max Agents per user (default: 1, 0 = unlimited)

set -euo pipefail

for cmd in kubectl helm; do
  if ! command -v "$cmd" >/dev/null 2>&1; then
    echo "ERROR: ${cmd} is not installed or not found in PATH." >&2
    exit 1
  fi
done

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# OCI chart / deploy defaults (override via environment variables)
REGISTRY="${REGISTRY:-$("${SCRIPT_DIR}/../../../scripts/cetirun-release-metadata.sh" registry)}"
CHART_REF="oci://${REGISTRY}/helm/cetirun-portal"
CHART_VERSION="${CHART_VERSION:-${VERSION:-0.1.1-src.20261111}}"
NAMESPACE="${NAMESPACE:-cetirun-portal}"
RELEASE_NAME="${RELEASE_NAME:-cetirun-portal}"
IMAGE_REPO="${IMAGE_REPO:-${REGISTRY}/cetirun-portal}"

echo "==> Checking cluster connectivity..."
kubectl cluster-info --request-timeout=5s > /dev/null

# ── Istio prerequisite check ──────────────────────────────────────────────────
echo ""
echo "==> Checking Istio prerequisites..."
if ! kubectl get crd virtualservices.networking.istio.io &>/dev/null; then
  echo "ERROR: Istio CRDs not found. Install Istio first:"
  echo ""
  echo "    az aks mesh enable --name <cluster> --resource-group <rg> --revision asm-1-27"
  echo "    az aks mesh enable-ingress-gateway --name <cluster> --resource-group <rg> --ingress-gateway-type External"
  echo ""
  exit 1
fi
if ! kubectl get gateway.networking.istio.io cetirun-gateway -n aks-istio-ingress &>/dev/null; then
  echo "ERROR: cetirun-gateway not found in aks-istio-ingress namespace."
  echo "Deploy shared Istio infrastructure first:"
  echo ""
  echo "    cd infra/istio && ./deploy.sh"
  echo ""
  exit 1
fi
echo "    Istio CRDs and cetirun-gateway found."

# ── Helm install ─────────────────────────────────────────────────────────────
echo ""
echo "==> Creating namespace ${NAMESPACE} (if not exists)..."
kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -

# Enable Istio sidecar injection so AuthorizationPolicy/PeerAuthentication
# are enforced. The revision must match the AKS Istio addon revision.
ISTIO_REV="${ISTIO_REV:-asm-1-27}"
echo "==> Labeling namespace ${NAMESPACE} for Istio sidecar injection (rev=${ISTIO_REV})..."
kubectl label namespace "$NAMESPACE" "istio.io/rev=${ISTIO_REV}" --overwrite

echo ""
HELM_ARGS=()
if [ -n "${LOCAL_CHART:-}" ]; then
  CHART_SOURCE="$SCRIPT_DIR/../chart"
  echo "==> Installing/upgrading Helm release '${RELEASE_NAME}' from local chart ${CHART_SOURCE}..."
  HELM_ARGS+=("$CHART_SOURCE")
else
  echo "==> Installing/upgrading Helm release '${RELEASE_NAME}' from ${CHART_REF}:${CHART_VERSION}..."
  HELM_ARGS+=("$CHART_REF" --version "$CHART_VERSION")
fi

# Image repository override (always set to ensure correct registry)
HELM_ARGS+=(--set "image.repository=${IMAGE_REPO}")

# Optional overrides
HELM_ARGS+=(--set-string "image.tag=${IMAGE_TAG:-${VERSION:-${CHART_VERSION}}}")
[ -n "${IMAGE_DIGEST:-}" ]   && HELM_ARGS+=(--set "image.digest=${IMAGE_DIGEST}")
[ -n "${ISTIO_HOST:-}" ]      && HELM_ARGS+=(--set "istio.host=${ISTIO_HOST}")
[ -n "${ISTIO_LEGACY_HOST:-}" ] && HELM_ARGS+=(--set "istio.legacyHost=${ISTIO_LEGACY_HOST}")
[ "${NOTICE_BANNER+x}" = x ]     && HELM_ARGS+=(--set-literal "noticeBanner=${NOTICE_BANNER}")
[ -n "${MAX_INSTANCES_PER_USER:-}" ] && HELM_ARGS+=(--set "features.maxInstancesPerUser=${MAX_INSTANCES_PER_USER}")

# Admin aliases — same source as the istio chart's authPolicy.adminAliases.
# Accept ADMIN_ALIASES as comma-separated and project into admins.aliases[*].
if [ -n "${ADMIN_ALIASES:-}" ]; then
  IFS=',' read -ra _ADMIN_LIST <<< "${ADMIN_ALIASES}"
  _IDX=0
  for _ALIAS in "${_ADMIN_LIST[@]}"; do
    _ALIAS_TRIMMED=$(echo "${_ALIAS}" | xargs)
    [ -z "${_ALIAS_TRIMMED}" ] && continue
    HELM_ARGS+=(--set-string "admins.aliases[${_IDX}]=${_ALIAS_TRIMMED}")
    _IDX=$((_IDX + 1))
  done
  echo "Admin aliases configured: ${_IDX} entry/entries."
fi

helm upgrade --install "$RELEASE_NAME" "${HELM_ARGS[@]}" \
  --namespace "$NAMESPACE"

echo ""
echo "==> Deploy complete."
echo ""
echo "Portal should be accessible at: https://${ISTIO_HOST}"

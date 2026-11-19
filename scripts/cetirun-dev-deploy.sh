#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# Quick dev deploy: build images, push to test ACR, package/push Helm chart,
# and create/patch an Agent CR.
#
# Usage:
#   scripts/cetirun-dev-deploy.sh <alias>
#   scripts/cetirun-dev-deploy.sh <alias> --skip-build   # reuse existing images
#   scripts/cetirun-dev-deploy.sh <alias> --apply-only    # only apply the CR (no build/push)
#
# Environment overrides:
#   REGISTRY         ACR hostname         (required)
#   IMAGE_TAG        image tag override    (default: dev-<branch>-<sha>)
#   CHART_VERSION    chart version override(default: 0.0.0-dev-<branch>-<sha>)
set -euo pipefail

usage() {
  echo "Usage: REGISTRY=<registry> $0 <alias> [--skip-build|--apply-only]"
}
if [ "${1:-}" = "--help" ] || [ "${1:-}" = "-h" ]; then
  usage
  exit 0
fi

script_dir="$(cd "$(dirname "$0")" && pwd)"
repo_root="$(cd "$script_dir/.." && pwd)"
context_dir="${repo_root}/app/cetirun-chart"

# ── Args ──────────────────────────────────────────────────────────────────
ALIAS="${1:?Usage: cetirun-dev-deploy.sh <alias> [--skip-build|--apply-only]}"
MODE="${2:-full}"  # full | --skip-build | --apply-only
if [ "$#" -gt 2 ]; then
  usage >&2
  exit 1
fi
case "$MODE" in
  full|--skip-build|--apply-only) ;;
  *) usage >&2; exit 1 ;;
esac
if [[ ! "$ALIAS" =~ ^[a-z][a-z0-9-]{0,62}$ ]]; then
  echo "ERROR: alias must match ^[a-z][a-z0-9-]{0,62}$" >&2
  exit 1
fi

# When skipping build, IMAGE_TAG must be set explicitly to reuse existing images.
if [ "$MODE" = "--skip-build" ] && [ -z "${IMAGE_TAG:-}" ]; then
  echo "ERROR: IMAGE_TAG must be set when using --skip-build (to reuse existing images)" >&2
  echo "  Example: IMAGE_TAG=dev-feat-branch-abc1234 $0 $ALIAS --skip-build" >&2
  exit 1
fi

registry="${REGISTRY:?Set REGISTRY to your container registry hostname}"

# ── Tag generation ────────────────────────────────────────────────────────
branch=$(git -C "$repo_root" rev-parse --abbrev-ref HEAD | sed 's|[/:]|-|g' | head -c 40)
sha=$(git -C "$repo_root" rev-parse --short=7 HEAD)
default_tag="dev-${branch}-${sha}"
image_tag="${IMAGE_TAG:-$default_tag}"
chart_version="${CHART_VERSION:-0.0.0-${default_tag}}"

echo "──────────────────────────────────────────────"
echo "  Registry:       ${registry}"
echo "  Image tag:      ${image_tag}"
echo "  Chart version:  ${chart_version}"
echo "  Alias:          ${ALIAS}"
echo "  Mode:           ${MODE}"
echo "──────────────────────────────────────────────"

# ── Build & push images ──────────────────────────────────────────────────
if [ "$MODE" != "--skip-build" ] && [ "$MODE" != "--apply-only" ]; then
  echo ""
  echo "==> Logging in to ${registry}"
  az acr login --name "${registry%%.*}" 2>/dev/null || true

  echo ""
  echo "==> Building and pushing CetiRun agent images:${image_tag}"
  REGISTRY="$registry" IMAGE_NAME=openclaw-gateway IMAGE_TAG="$image_tag" PUSH=true BUILD_HERMES=true \
    "$script_dir/cetirun-docker-build.sh"
fi

# ── Package & push Helm chart ────────────────────────────────────────────
if [ "$MODE" != "--apply-only" ]; then
  echo ""
  echo "==> Packaging Helm chart (version ${chart_version})"
  tmp_chart=$(mktemp -d)
  helm package "$context_dir" \
    --version "$chart_version" \
    --app-version "$image_tag" \
    -d "$tmp_chart"

  echo "==> Pushing Helm chart to oci://${registry}/helm/"
  helm push "${tmp_chart}"/cetirun-*.tgz "oci://${registry}/helm/"
  rm -rf "$tmp_chart"
fi

# ── Apply Agent CR ─────────────────────────────────────────────
echo ""
echo "==> Applying Agent for ${ALIAS}"
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
    displayName: "${ALIAS}"
  helm:
    version: "${chart_version}"
    values:
      openclaw:
        image:
          repository: "${registry}/openclaw-gateway"
          tag: "${image_tag}"
      hermes:
        image:
          repository: "${registry}/hermes-gateway"
          tag: "${image_tag}"
      desktop:
        image:
          repository: "${registry}/cetirun-desktop"
          tag: "${image_tag}"
EOF

echo ""
echo "──────────────────────────────────────────────"
echo "  Deployed!"
echo ""
echo "  Gateway image:  ${registry}/openclaw-gateway:${image_tag}"
echo "  Hermes image:   ${registry}/hermes-gateway:${image_tag}"
echo "  Desktop image:  ${registry}/cetirun-desktop:${image_tag}"
echo "  Helm chart:     oci://${registry}/helm/cetirun:${chart_version}"
echo "  CR:             Agent/${ALIAS}"
echo ""
echo "  Watch progress:"
echo "    kubectl get agent ${ALIAS} -w"
echo "    kubectl -n cetirun-${ALIAS} get pods -w"
echo "──────────────────────────────────────────────"

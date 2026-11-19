#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail
if [[ "${1:-}" == --help || "${1:-}" == -h ]]; then
  echo "Usage: REGISTRY=registry.example.com VERSION=X.Y.Z ACCEPT_VSCODE_SERVER_LICENSE_TERMS=true [PUSH=false] vscode-docker-build.sh"
  exit 0
fi
[[ $# -eq 0 ]] || { echo "Use --help" >&2; exit 2; }
repo_root="$(cd "$(dirname "$0")/.." && pwd)"
REGISTRY="${REGISTRY:-$("${repo_root}/scripts/cetirun-release-metadata.sh" registry)}"
VERSION="${VERSION:-$("${repo_root}/scripts/cetirun-release-metadata.sh" version)}"
[[ "${ACCEPT_VSCODE_SERVER_LICENSE_TERMS:-}" == true ]] || { echo "Explicit VS Code Server license acceptance is required" >&2; exit 2; }
[[ "${REGISTRY:-}" =~ ^[a-z0-9][a-z0-9.:/_-]*$ && "${REGISTRY}" != *://* ]] || { echo "REGISTRY is required" >&2; exit 2; }
[[ "${VERSION:-}" =~ ^[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,127}$ ]] || { echo "VERSION is required" >&2; exit 2; }
[[ "${PUSH:-false}" == true || "${PUSH:-false}" == false ]] || { echo "PUSH must be true or false" >&2; exit 2; }
args=(--platform linux/amd64 --tag "${REGISTRY}/cetirun-vscode:${VERSION}"
  --build-arg "BASE_IMAGE=${BASE_IMAGE:-${REGISTRY}/cetirun-base:${VERSION}}"
  --build-arg ACCEPT_VSCODE_SERVER_LICENSE_TERMS=true
  --label "org.opencontainers.image.version=${VERSION}"
  --label "org.opencontainers.image.revision=${GIT_SHA:-$(git -C "${repo_root}" rev-parse HEAD)}")
if [[ "${PUSH:-false}" == true ]]; then
  "${repo_root}/scripts/cetirun-release-metadata.sh" check-push-registry "${REGISTRY}"
  args+=(--push)
else
  args+=(--load)
fi
docker buildx build "${args[@]}" "${repo_root}/examples/vscode-server"

#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail
if [[ "${1:-}" == "--help" ]]; then
  echo "Build the Memory Context asset image: REGISTRY=registry.example.com [VERSION=...] [PUSH=false] [PLATFORMS=linux/amd64,linux/arm64] $0"
  exit 0
fi
[[ $# -eq 0 ]] || { echo "Use --help" >&2; exit 2; }
script_dir="$(cd "$(dirname "$0")" && pwd)"
REGISTRY="${REGISTRY:-$("${script_dir}/cetirun-release-metadata.sh" registry)}"
[[ -n "${REGISTRY:-}" && "${REGISTRY}" != *://* && "${REGISTRY}" =~ ^[a-zA-Z0-9][a-zA-Z0-9.:/-]*$ ]] || { echo "REGISTRY is required" >&2; exit 2; }
[[ "${PUSH:-false}" == "true" || "${PUSH:-false}" == "false" ]] || { echo "PUSH must be true or false" >&2; exit 2; }
command -v docker >/dev/null || { echo "docker is required" >&2; exit 1; }
repo_root="$(cd "${script_dir}/.." && pwd)"
release_version="${VERSION:-$("${script_dir}/cetirun-release-metadata.sh" version)}"
[[ "${release_version}" =~ ^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$ ]] || { echo "VERSION must be a valid image tag" >&2; exit 2; }
if [[ -n "${PLATFORMS:-}" && ! "${PLATFORMS}" =~ ^linux/(amd64|arm64)(,linux/(amd64|arm64))*$ ]]; then
  echo "PLATFORMS must select linux/amd64 or linux/arm64" >&2
  exit 2
fi
git_sha="${GIT_SHA:-$(git -C "${repo_root}" rev-parse HEAD)}"
args=(buildx build -f "${repo_root}/app/cetirun-memory-context/Dockerfile"
  -t "${REGISTRY}/cetirun-memory-context:${release_version}"
  --build-arg "VERSION=${release_version}" --build-arg "GIT_SHA=${git_sha}")
if [[ "${PUSH:-false}" == "true" ]]; then
  "${script_dir}/cetirun-release-metadata.sh" check-push-registry "${REGISTRY}"
  args+=(--platform "${PLATFORMS:-linux/amd64,linux/arm64}" --push)
else
  args+=(--load)
  if [[ -n "${PLATFORMS:-}" ]]; then
    [[ "${PLATFORMS}" != *,* ]] || { echo "Loading requires one platform" >&2; exit 2; }
    args+=(--platform "${PLATFORMS}")
  fi
fi
docker "${args[@]}" "${repo_root}"

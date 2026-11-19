#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

case "${1:-}" in
  -h|--help)
    printf '%s\n' \
      "Usage: terminal-docker-build.sh" \
      "Build the dedicated cetirun-terminal image; no registry login is performed." \
      "Inputs: REGISTRY, VERSION, IMAGE_TAG, GIT_SHA, PLATFORM (linux/amd64 or linux/arm64)." \
      "PUSH defaults to false. Set PUSH=true only for an authorized publication."
    exit 0
    ;;
  "") ;;
  *) printf '%s\n' "Unknown argument; use --help." >&2; exit 2 ;;
esac

script_dir="$(cd "$(dirname "$0")" && pwd)"
repo_root="$(cd "${script_dir}/.." && pwd)"
registry="${REGISTRY:-$("${script_dir}/cetirun-release-metadata.sh" registry)}"
release_version="${VERSION:-$("${script_dir}/cetirun-release-metadata.sh" version)}"
image_tag="${IMAGE_TAG:-${release_version}}"
git_sha="${GIT_SHA:-$(git -C "${repo_root}" rev-parse HEAD)}"
platform="${PLATFORM:-linux/amd64}"
push="${PUSH:-false}"
dockerfile="${repo_root}/app/cetirun-operator/Dockerfile.terminal"

[[ "${registry}" != *"://"* && "${registry}" =~ ^[a-zA-Z0-9][a-zA-Z0-9./:_-]*$ ]] || { printf '%s\n' "Invalid REGISTRY." >&2; exit 2; }
[[ "${image_tag}" =~ ^[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,127}$ ]] || { printf '%s\n' "Invalid IMAGE_TAG." >&2; exit 2; }
[[ "${push}" == "true" || "${push}" == "false" ]] || { printf '%s\n' "PUSH must be true or false." >&2; exit 2; }
[[ "${platform}" == "linux/amd64" || "${platform}" == "linux/arm64" ]] || { printf '%s\n' "Unsupported PLATFORM." >&2; exit 2; }
test -f "${dockerfile}" || { printf '%s\n' "Terminal Dockerfile is missing." >&2; exit 1; }
command -v docker >/dev/null || { printf '%s\n' "Docker is required." >&2; exit 1; }
docker buildx version >/dev/null

args=(
  -f "${dockerfile}"
  -t "${registry%/}/cetirun-terminal:${image_tag}"
  --platform "${platform}"
  --build-arg "VERSION=${image_tag}"
  --build-arg "GIT_SHA=${git_sha}"
)
if [[ "${push}" == "true" ]]; then
  "${script_dir}/cetirun-release-metadata.sh" check-push-registry "${registry}"
  args+=(--push)
else
  args+=(--load)
fi
docker buildx build "${args[@]}" "${repo_root}"

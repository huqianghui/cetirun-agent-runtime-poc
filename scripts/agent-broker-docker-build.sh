#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail
script_dir="$(cd "$(dirname "$0")" && pwd)"
repo_root="$(cd "${script_dir}/.." && pwd)"
source "${script_dir}/cetirun-build-attestations.sh"
cetirun_validate_sbom_generator "${SBOM_GENERATOR:-}"
push="${PUSH:-false}"
cetirun_require_sbom_generator_push "${push}"
command -v docker >/dev/null 2>&1 || { echo "ERROR: docker is required" >&2; exit 1; }
docker buildx version >/dev/null 2>&1 || { echo "ERROR: docker buildx is required" >&2; exit 1; }
registry="${REGISTRY:-$("${script_dir}/cetirun-release-metadata.sh" registry)}"
release_version="${VERSION:-$("${script_dir}/cetirun-release-metadata.sh" version)}"
image_tag="${IMAGE_TAG:-${release_version}}"
image_ref="${registry}/cetirun-agent-broker:${image_tag}"
git_sha="${GIT_SHA:-$(git -C "${repo_root}" rev-parse HEAD)}"
args=(-f "${repo_root}/app/cetirun-agent-broker/Dockerfile" -t "${image_ref}"
  --label "org.opencontainers.image.version=${image_tag}"
  --label "org.opencontainers.image.revision=${git_sha}")
if [[ "${push}" == true ]]; then
  "${script_dir}/cetirun-release-metadata.sh" check-push-registry "${registry}"
  args+=(--platform linux/amd64 --push)
  buildx_driver=""
  if buildx_driver="$(docker buildx inspect --bootstrap 2>/dev/null | awk '/^Driver:/ { print $2; exit }')"; then :; fi
  cetirun_configure_sbom_attestation "${buildx_driver}"
  args+=("${CETIRUN_SBOM_ATTESTATION_ARGS[@]}")
else
  args+=(--load)
fi
docker buildx build "${args[@]}" "${repo_root}"

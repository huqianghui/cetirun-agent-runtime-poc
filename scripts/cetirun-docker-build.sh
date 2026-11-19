#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

script_dir="$(cd "$(dirname "$0")" && pwd)"
repo_root="$(cd "$script_dir/.." && pwd)"
source "${script_dir}/cetirun-build-attestations.sh"
cetirun_validate_sbom_generator "${SBOM_GENERATOR:-}"

registry="${REGISTRY:-$("${script_dir}/cetirun-release-metadata.sh" registry)}"
image_name="${IMAGE_NAME:-openclaw-gateway}"
release_version="${VERSION:-$("${script_dir}/cetirun-release-metadata.sh" version)}"
image_tag="${IMAGE_TAG:-${release_version}}"
git_sha="${GIT_SHA:-$(git -C "${repo_root}" rev-parse HEAD 2>/dev/null || echo unknown)}"
push="${PUSH:-false}"
cetirun_require_sbom_generator_push "${push}"
context_dir="${repo_root}/app/cetirun-chart"
dockerfile="${DOCKERFILE:-agents/openclaw/Dockerfile}"
build_base="${BUILD_BASE:-true}"
build_image="${BUILD_IMAGE:-true}"
build_desktop="${BUILD_DESKTOP:-true}"
build_hermes="${BUILD_HERMES:-false}"
build_network="${BUILD_NETWORK:-}"

image_ref="${registry}/${image_name}:${image_tag}"
base_image_ref="${BASE_IMAGE:-${registry}/cetirun-base:${image_tag}}"
if [[ "${push}" == true ]]; then
  "${script_dir}/cetirun-release-metadata.sh" check-push-registry "${registry}"
  if [[ "${build_base}" == true ]]; then
    "${script_dir}/cetirun-release-metadata.sh" check-push-registry "${base_image_ref}"
  fi
fi

# Package registries remain independently configurable for each build path.
npm_registry="${NPM_REGISTRY:-https://registry.npmjs.org/}"
npm_registry_args=(--build-arg "NPM_REGISTRY=${npm_registry}")
# Match the public Dockerfile default; private deployment environments can
# explicitly select their own package proxy with PYPI_INDEX_URL.
pypi_index_url="${PYPI_INDEX_URL:-https://pypi.org/simple/}"
pypi_index_args=(--build-arg "PYPI_INDEX_URL=${pypi_index_url}")
release_build_args=(
  --build-arg "VERSION=${image_tag}"
  --build-arg "GIT_SHA=${git_sha}"
)

# Preserve the separate OpenClaw override for deployments using different mirrors.
openclaw_npm_registry="${OPENCLAW_NPM_REGISTRY:-https://registry.npmjs.org/}"
openclaw_npm_registry_args=(--build-arg "NPM_REGISTRY=${openclaw_npm_registry}")

common_output_args=()
if [ -n "${build_network}" ]; then
  case "${build_network}" in
    default|host|none) common_output_args+=(--network "${build_network}") ;;
    *) echo "BUILD_NETWORK must be default, host, or none" >&2; exit 1 ;;
  esac
fi
buildx_driver=""
if buildx_driver="$(docker buildx inspect --bootstrap 2>/dev/null | awk '/^Driver:/ { print $2; exit }')"; then :; fi
cetirun_configure_sbom_attestation "${buildx_driver}"
common_output_args+=("${CETIRUN_SBOM_ATTESTATION_ARGS[@]}")
if [ "${push}" = "true" ]; then
  common_output_args+=(--push)
else
  common_output_args+=(--load)
fi

if [ "${build_base}" = "true" ]; then
  base_build_args=(
    --platform linux/amd64
    -f "${context_dir}/Dockerfile.base"
    -t "${base_image_ref}"
    "${release_build_args[@]}"
    "${npm_registry_args[@]}"
    "${pypi_index_args[@]}"
    "${common_output_args[@]}"
  )
  if [ "${push}" = "true" ]; then
    echo "==> Building and pushing ${base_image_ref} from ${context_dir} (Dockerfile.base)"
  else
    echo "==> Building ${base_image_ref} from ${context_dir} (Dockerfile.base)"
  fi
  docker buildx build "${base_build_args[@]}" "${context_dir}"
fi

image_npm_registry_args=("${npm_registry_args[@]}")
if [ "${dockerfile}" = "agents/openclaw/Dockerfile" ]; then
  image_npm_registry_args=("${openclaw_npm_registry_args[@]}")
fi

if [ "${build_image}" = "true" ]; then
  build_args=(
    --platform linux/amd64
    -f "${context_dir}/${dockerfile}"
    -t "${image_ref}"
    --build-arg "BASE_IMAGE=${base_image_ref}"
    "${release_build_args[@]}"
    "${image_npm_registry_args[@]}"
    "${pypi_index_args[@]}"
    "${common_output_args[@]}"
  )
  if [ "${push}" = "true" ]; then
    echo "==> Building and pushing ${image_ref} from ${context_dir} (${dockerfile})"
  else
    echo "==> Building ${image_ref} from ${context_dir} (${dockerfile})"
  fi

  docker buildx build "${build_args[@]}" "${context_dir}"
fi

if [ "${build_hermes}" = "true" ]; then
  hermes_image_ref="${registry}/hermes-gateway:${image_tag}"
  hermes_build_args=(
    --platform linux/amd64
    -f "${context_dir}/agents/hermes/Dockerfile"
    -t "${hermes_image_ref}"
    --build-arg "BASE_IMAGE=${base_image_ref}"
    "${release_build_args[@]}"
    "${npm_registry_args[@]}"
    "${pypi_index_args[@]}"
    "${common_output_args[@]}"
  )
  if [ "${push}" = "true" ]; then
    echo "==> Building and pushing ${hermes_image_ref} from ${context_dir} (agents/hermes/Dockerfile)"
  else
    echo "==> Building ${hermes_image_ref} from ${context_dir} (agents/hermes/Dockerfile)"
  fi
  docker buildx build "${hermes_build_args[@]}" "${context_dir}"
fi

if [ "${build_desktop}" = "true" ]; then
  desktop_image_ref="${registry}/cetirun-desktop:${image_tag}"
  desktop_build_args=(
    --platform linux/amd64
    -f "${context_dir}/Dockerfile.desktop"
    -t "${desktop_image_ref}"
    "${release_build_args[@]}"
    "${common_output_args[@]}"
  )
  if [ "${push}" = "true" ]; then
    echo "==> Building and pushing ${desktop_image_ref} from ${context_dir} (Dockerfile.desktop)"
  else
    echo "==> Building ${desktop_image_ref} from ${context_dir} (Dockerfile.desktop)"
  fi
  docker buildx build "${desktop_build_args[@]}" "${context_dir}"
fi

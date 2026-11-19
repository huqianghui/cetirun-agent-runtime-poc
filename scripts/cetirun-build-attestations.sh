#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# Shared buildx attestation inputs. Source this file from an image build helper.

set -euo pipefail

cetirun_validate_sbom_generator() {
  local generator="${1:-}"
  if [[ -z "${generator}" ]]; then
    return 0
  fi
  # A generator is executable supply-chain input. Require an OCI digest rather
  # than a mutable tag, and keep the accepted repository grammar deliberately
  # narrow so it can be passed as one buildx argument without reinterpretation.
  if [[ ! "${generator}" =~ ^([a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]+)?/)?[a-z0-9]+([._-][a-z0-9]+)*(\/[a-z0-9]+([._-][a-z0-9]+)*)*@sha256:[a-f0-9]{64}$ ]]; then
    printf '%s\n' "SBOM_GENERATOR must be a lowercase OCI image reference pinned with @sha256:<64 lowercase hexadecimal characters>." >&2
    return 2
  fi
}

cetirun_require_sbom_generator_push() {
  local push="${1:-}"
  if [[ -n "${SBOM_GENERATOR:-}" && "${push}" != "true" ]]; then
    printf '%s\n' "SBOM_GENERATOR requires PUSH=true because loaded images do not retain Buildx attestations." >&2
    return 2
  fi
}

cetirun_configure_sbom_attestation() {
  local driver="${1:-}"
  local generator="${SBOM_GENERATOR:-}"

  cetirun_validate_sbom_generator "${generator}"
  CETIRUN_SBOM_ATTESTATION_ARGS=()
  if [[ "${driver}" != "docker-container" ]]; then
    if [[ -n "${generator}" ]]; then
      printf '%s\n' "SBOM_GENERATOR requires a docker-container Buildx driver; current driver is ${driver:-unavailable}." >&2
      return 2
    fi
    return 0
  fi
  CETIRUN_SBOM_ATTESTATION_ARGS=(--provenance=true)
  if [[ -n "${generator}" ]]; then
    CETIRUN_SBOM_ATTESTATION_ARGS+=("--sbom=generator=${generator}")
  else
    CETIRUN_SBOM_ATTESTATION_ARGS+=(--sbom=true)
  fi
}

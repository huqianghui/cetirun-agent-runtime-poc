#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail
script_dir="$(cd "$(dirname "$0")" && pwd)"
if [[ "${1:-}" == --help || "${1:-}" == -h ]]; then
  echo "Usage: [REGISTRY=host/prefix] [VERSION=X.Y.Z] $0 [chart-name]"
  echo "Optional: CHART_VERSION, APP_VERSION, CHART_DIR, VALUES_FILE, FORCE_PUBLISH=true."
  echo "Publishes one chart using the shared packager and verifies its pull."
  echo "Publication requires a writable REGISTRY; the default MCR pull endpoint cannot receive pushes."
  exit 0
fi
chart="${1:-cetirun}"
if [[ $# -gt 0 ]]; then shift; fi
version="${VERSION:-$("${script_dir}/cetirun-release-metadata.sh" version)}"
args=(publish-charts --chart "${chart}" --version "${CHART_VERSION:-${version}}"
  --app-version "${APP_VERSION:-${version}}")
[[ -z "${CHART_DIR:-}" ]] || args+=(--chart-dir "${CHART_DIR}")
[[ -z "${VALUES_FILE:-}" ]] || args+=(--values-file "${VALUES_FILE}")
[[ "${FORCE_PUBLISH:-false}" == true || "${FORCE_PUBLISH:-false}" == false ]] || { echo "FORCE_PUBLISH must be true or false" >&2; exit 2; }
if [[ "${FORCE_PUBLISH:-false}" == true ]]; then args+=(--force); fi
exec python3 -B "${script_dir}/cetirun-release.py" "${args[@]}" "$@"

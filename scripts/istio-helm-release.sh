#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
set -euo pipefail
script_dir="$(cd "$(dirname "$0")" && pwd)"
exec bash "${script_dir}/cetirun-helm-release.sh" cetirun-istio "$@"

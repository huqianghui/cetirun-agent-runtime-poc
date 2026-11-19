#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail
script_dir="$(cd "$(dirname "$0")" && pwd)"
exec python3 -B "${script_dir}/cetirun_platform_deploy.py" "$@"

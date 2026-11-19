#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

script_dir="$(cd "$(dirname "$0")" && pwd)"
repo_root="$(cd "$script_dir/.." && pwd)"
chart_dir="${CHART_DIR:-${repo_root}/app/cetirun-chart}"

if [ ! -f "${chart_dir}/Chart.yaml" ]; then
  echo "ERROR: Chart.yaml not found in ${chart_dir}" >&2
  exit 1
fi

values_files=(
  "values.yaml"
)

for values_file in "${values_files[@]}"; do
  if [ -f "${chart_dir}/${values_file}" ]; then
    echo "==> helm lint ${chart_dir} -f ${values_file} (agentType=openclaw)"
    helm lint "${chart_dir}" -f "${chart_dir}/${values_file}" \
      --set agentType=openclaw \
      --set aiGateway.keySecretName=lint-model-key \
      --set-string secrets.apiBase=http://gateway.example.com/v1 \
      --set-string secrets.model=lint-model \
      --set secrets.openclawGatewayToken=lint-token

    echo "==> helm lint ${chart_dir} -f ${values_file} (agentType=hermes)"
    helm lint "${chart_dir}" -f "${chart_dir}/${values_file}" \
      --set agentType=hermes \
      --set aiGateway.keySecretName=lint-model-key \
      --set-string secrets.apiBase=http://gateway.example.com/v1 \
      --set-string hermes.env.HERMES_WEBAPP_PUBLIC_URL=https://agents.example.com/ui/cetirun \
      --set-string secrets.model=lint-model
  fi
done

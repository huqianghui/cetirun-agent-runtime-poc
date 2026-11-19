#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# Render assertions for the cetirun-observability chart. Offline: helm template
# only, no cluster and no install. scripts/tests/test_observability_chart.py
# checks the rendered objects in more detail.

set -euo pipefail

script_dir="$(cd "$(dirname "$0")" && pwd)"
repo_root="$(cd "$script_dir/.." && pwd)"
chart_dir="${CHART_DIR:-${repo_root}/app/cetirun-observability/chart}"

if [ ! -f "${chart_dir}/Chart.yaml" ]; then
  echo "ERROR: Chart.yaml not found in ${chart_dir}" >&2
  exit 1
fi

base_args=(
  --namespace cetirun-system
  --set-string "clusterName=example"
  --set-string "adx.endpoint=https://example.westus.kusto.windows.net"
  --set-string "ingestor.clientId=66666666-6666-6666-6666-666666666666"
)

render() {
  helm template observability "${chart_dir}" "${base_args[@]}" "$@"
}

assert_contains() {
  local needle="$1" description="$2" output="$3"
  if ! grep -qF -- "${needle}" <<< "${output}"; then
    echo "FAIL: ${description}" >&2
    exit 1
  fi
}

assert_absent() {
  local needle="$1" description="$2" output="$3"
  if grep -qF -- "${needle}" <<< "${output}"; then
    echo "FAIL: ${description}" >&2
    exit 1
  fi
}

assert_rejected() {
  local message="$1" description="$2"
  shift 2
  local output
  if output="$(helm template observability "${chart_dir}" --namespace cetirun-system "$@" 2>&1)"; then
    echo "FAIL: ${description} should have been rejected" >&2
    exit 1
  fi
  if ! grep -qF -- "${message}" <<< "${output}"; then
    echo "FAIL: ${description} was rejected without naming the value: ${output}" >&2
    exit 1
  fi
}

default="$(render)"

# The collector serves nothing that others can write to: no OTLP or remote
# write receivers, and its listener binds the Pod's loopback address.
assert_contains 'listen-addr = "127.0.0.1:8080"' "collector listens on loopback only" "${default}"
assert_absent '[otel-log]' "collector has no OTLP log receiver" "${default}"
assert_absent '[[prometheus-remote-write]]' "collector has no remote write receiver" "${default}"
assert_absent '[[otel-metric]]' "collector has no OTLP metric receiver" "${default}"
assert_absent 'hostPort' "no Pod takes a node port" "${default}"
# The collector's role has only what pod discovery and the kubelet's metrics
# endpoints need.
assert_absent 'nodes/proxy' "collector role has no nodes/proxy" "${default}"
assert_contains '"nodes/metrics"' "collector role reads kubelet metrics" "${default}"
assert_absent '"secrets"' "no role reads Secrets" "${default}"
# Transfers are verified TLS, and the ingestor uses its workload identity only.
assert_contains 'endpoint = "https://cetirun-adx-mon-ingestor.cetirun-system.svc:9090"' "collector sends to the ingestor over HTTPS" "${default}"
assert_absent 'insecure-skip-verify' "collector verifies the ingestor certificate" "${default}"
assert_contains 'SSL_CERT_FILE' "collector trusts the chart CA" "${default}"
assert_contains 'value: WorkloadIdentityCredential' "ingestor uses workload identity only" "${default}"
assert_contains 'azure.workload.identity/client-id: "66666666-6666-6666-6666-666666666666"' "ingestor service account names the identity" "${default}"
assert_contains '--disable-peer-transfer' "ingestors upload what they receive" "${default}"
# Agent logs go to their own database before any namespace route.
assert_contains 'destination = "CetiRunAgents:AgentLogs"' "Agent logs go to the Agent logs database" "${default}"
assert_contains 'destination = "CetiRunLogs:Gateway"' "Gateway logs have their own table" "${default}"
assert_contains 'cluster = "example"' "rows carry the cluster name" "${default}"
assert_contains 'kind: CustomResourceDefinition' "ADX-Mon CRDs are installed by default" "${default}"

without_optional="$(render --set kubeStateMetrics.enabled=false --set telemetry.enabled=false --set crds.install=false \
  --set logs.agents.enabled=false)"
assert_absent 'cetirun-kube-state-metrics' "kube-state-metrics can be left out" "${without_optional}"
assert_absent 'name: cetirun-telemetry' "the telemetry program can be left out" "${without_optional}"
assert_absent 'kind: CustomResourceDefinition' "the CRDs can be left out" "${without_optional}"
assert_absent 'CetiRunAgents:AgentLogs' "Agent logs can be left out" "${without_optional}"

registry="$(render --set-string global.registry=registry.example.com/team --set-string global.version=9.9.9)"
assert_contains 'image: "registry.example.com/team/cetirun-observability:9.9.9"' "global coordinates set the collector image" "${registry}"
assert_contains 'image: "registry.example.com/team/cetirun-operator:9.9.9"' "global coordinates set the telemetry image" "${registry}"
assert_contains 'mcr.microsoft.com/oss/kubernetes/kube-state-metrics:v2.14.0@sha256:' "kube-state-metrics stays pinned by digest" "${registry}"

existing_tls="$(render --set-string ingestor.tls.secretName=adx-mon-tls)"
assert_contains 'secretName: adx-mon-tls' "an existing TLS Secret is used" "${existing_tls}"
assert_absent 'kind: Secret' "no TLS Secret is generated when one is supplied" "${existing_tls}"

assert_rejected 'clusterName must name this CetiRun cluster' "missing cluster name" \
  --set-string "adx.endpoint=https://example.westus.kusto.windows.net" --set-string "ingestor.clientId=66666666-6666-6666-6666-666666666666"
assert_rejected 'adx.endpoint must be the query URI' "Kusto endpoint with a path" "${base_args[@]:2}" \
  --set-string "adx.endpoint=https://example.westus.kusto.windows.net/CetiRunLogs"
assert_rejected 'adx.endpoint must be the query URI' "plain HTTP Kusto endpoint" "${base_args[@]:2}" \
  --set-string "adx.endpoint=http://example.westus.kusto.windows.net"
assert_rejected 'ingestor.clientId must be the client ID' "missing ingestor identity" \
  --set-string "clusterName=example" --set-string "adx.endpoint=https://example.westus.kusto.windows.net"
assert_rejected 'adx.databases.logs must be a database name' "database name with a hyphen" "${base_args[@]:2}" \
  --set-string "adx.databases.logs=cetirun-logs"
assert_rejected 'telemetry.events must be' "unknown event filter" "${base_args[@]:2}" --set-string "telemetry.events=everything"
assert_rejected 'is not supported by this chart' "kube-state-metrics reading Secrets" "${base_args[@]:2}" \
  --set-string "kubeStateMetrics.objects=pods\,secrets"

echo "cetirun-observability chart render checks passed"

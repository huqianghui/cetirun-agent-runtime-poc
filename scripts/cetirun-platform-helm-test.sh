#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

script_dir="$(cd "$(dirname "$0")" && pwd)"
repo_root="$(cd "$script_dir/.." && pwd)"
chart_dir="${CHART_DIR:-${repo_root}/app/cetirun-platform}"
operator_chart_dir="${repo_root}/app/cetirun-operator/chart"
crd_source_dir="${repo_root}/app/cetirun-operator/config/crd/bases"
crd_chart_dir="${operator_chart_dir}/templates/crds"

if [ ! -f "${chart_dir}/Chart.yaml" ]; then
  echo "ERROR: Chart.yaml not found in ${chart_dir}" >&2
  exit 1
fi

helm dependency build --skip-refresh "${chart_dir}" > /dev/null

for crd_file in \
  cetirun.microsoft.com_agents.yaml \
  cetirun.microsoft.com_teams.yaml \
  cetirun.microsoft.com_sandboxes.yaml \
  cetirun.microsoft.com_agentpolicies.yaml; do
  if ! cmp -s "${crd_source_dir}/${crd_file}" "${crd_chart_dir}/${crd_file}"; then
    echo "FAIL: chart CRD ${crd_file} does not match its generated source" >&2
    exit 1
  fi
done

echo "==> helm template operator CRDs"
output=$(helm template operator "${operator_chart_dir}" --set cetiadmin.enabled=false)
for crd_name in agents.cetirun.microsoft.com teams.cetirun.microsoft.com \
  sandboxes.cetirun.microsoft.com agentpolicies.cetirun.microsoft.com; do
  grep -q "name: ${crd_name}" <<< "${output}" || {
    echo "FAIL: operator chart did not render CRD ${crd_name}" >&2
    exit 1
  }
done

# The deployment helper installs the platform into the operator's namespace,
# where the bundled AI Gateway must run too.
base_args=(
  --set cetirun-operator.cetiadmin.enabled=false
  -f "${chart_dir}/values.yaml"
  --namespace cetirun-system
  --set istio.enabled=true
  --set-string cetirun-operator.domainSuffix=platform.example.invalid
  --set-string cetirun-portal.istio.host=portal.example.invalid
)

echo "==> helm lint ${chart_dir} (default platform)"
helm lint --strict "${chart_dir}" "${base_args[@]}"

echo "==> helm template platform (default platform)"
output=$(helm template platform "${chart_dir}" "${base_args[@]}")
grep -q 'name: cetirun-operator' <<< "${output}" || {
  echo "FAIL: operator was not rendered" >&2
  exit 1
}
grep -q 'name: cetirun-portal' <<< "${output}" || {
  echo "FAIL: portal was not rendered" >&2
  exit 1
}
grep -q 'name: cetirun-ai-gateway$' <<< "${output}" || {
  echo "FAIL: the AI Gateway was not rendered" >&2
  exit 1
}
grep -q 'name: cetirun-ai-gateway-auth$' <<< "${output}" || {
  echo "FAIL: the AI Gateway master key Secret does not have the name the operator reads" >&2
  exit 1
}
grep -A1 'name: AI_GATEWAY_URL' <<< "${output}" | grep -q 'http://cetirun-ai-gateway.cetirun-system.svc.cluster.local:4000' || {
  echo "FAIL: the operator does not use the bundled AI Gateway" >&2
  exit 1
}
# The bundled Gateway serves CetiRun Agents: a router fallback must not reach
# a model the caller's key may not use, such as another Agent's Copilot route.
grep -q 'enforce_fallback_model_access: true' <<< "${output}" || {
  echo "FAIL: the bundled AI Gateway does not check fallback targets against the caller's key" >&2
  exit 1
}
for flag in AI_GATEWAY_ENABLED FEATURE_AI_GATEWAY_ENABLED; do
  grep -A1 "name: ${flag}$" <<< "${output}" | grep -q 'value: "true"' || {
    echo "FAIL: ${flag} is not on" >&2
    exit 1
  }
done
# Each probe of an Agent's GitHub Copilot route is a real call on the owner's
# Copilot allowance, so the Gateway probes none of them by default.
grep -A1 'name: AI_GATEWAY_COPILOT_HEALTH_CHECKS$' <<< "${output}" | grep -q 'value: "false"' || {
  echo "FAIL: the operator does not turn the probes of the Agents' GitHub Copilot routes off by default" >&2
  exit 1
}
# The platform writes the Gateway's request log and keeps LiteLLM's spend log
# for seven days, so the console's request log pages work, and serves the
# Gateway's metrics for managed Prometheus.
grep -A1 'name: CETIRUN_REQUEST_LOG$' <<< "${output}" | grep -q 'value: "true"' || {
  echo "FAIL: the bundled AI Gateway does not write its request log" >&2
  exit 1
}
if grep -q 'name: CETIRUN_SPEND_LOGS$' <<< "${output}"; then
  echo "FAIL: the bundled AI Gateway turns LiteLLM's spend log off" >&2
  exit 1
fi
grep -q 'maximum_spend_logs_retention_period: "7d"' <<< "${output}" || {
  echo "FAIL: the bundled AI Gateway keeps LiteLLM's spend log without a retention period" >&2
  exit 1
}
grep -q -- '- --prometheus_metrics_port$' <<< "${output}" || {
  echo "FAIL: the bundled AI Gateway does not serve its metrics" >&2
  exit 1
}
if grep -q 'kind: PodMonitor' <<< "${output}"; then
  echo "FAIL: the platform rendered a PodMonitor for a cluster without its API" >&2
  exit 1
fi
monitored=$(helm template platform "${chart_dir}" "${base_args[@]}" --api-versions azmonitoring.coreos.com/v1/PodMonitor)
grep -q 'kind: PodMonitor' <<< "${monitored}" || {
  echo "FAIL: the platform rendered no PodMonitor on a cluster with managed Prometheus" >&2
  exit 1
}
for crd_name in agents.cetirun.microsoft.com teams.cetirun.microsoft.com \
  sandboxes.cetirun.microsoft.com agentpolicies.cetirun.microsoft.com; do
  grep -q "name: ${crd_name}" <<< "${output}" || {
    echo "FAIL: platform chart did not render CRD ${crd_name}" >&2
    exit 1
  }
done

echo "==> helm template platform (probes of the Agents' GitHub Copilot routes)"
output=$(helm template platform "${chart_dir}" "${base_args[@]}" \
  --set cetirun-operator.aiGateway.copilotProxy.healthChecks.enabled=true)
grep -A1 'name: AI_GATEWAY_COPILOT_HEALTH_CHECKS$' <<< "${output}" | grep -q 'value: "true"' || {
  echo "FAIL: the operator does not let the Gateway probe the Agents' GitHub Copilot routes when asked" >&2
  exit 1
}
# helm upgrade --reuse-values keeps the values of the chart version a release
# was installed with, which may not have the setting. Setting it to null
# removes it the same way: such a release keeps the probes off.
output=$(helm template platform "${chart_dir}" "${base_args[@]}" \
  --set cetirun-operator.aiGateway.copilotProxy.healthChecks=null)
grep -A1 'name: AI_GATEWAY_COPILOT_HEALTH_CHECKS$' <<< "${output}" | grep -q 'value: "false"' || {
  echo "FAIL: a release from before the setting does not keep the probes of GitHub Copilot routes off" >&2
  exit 1
}

echo "==> helm template platform (shared registry and version)"
output=$(helm template platform "${chart_dir}" "${base_args[@]}" \
  --set-string global.registry=registry.example.com \
  --set-string global.version=9.8.7)
grep -q 'image: "registry.example.com/cetirun-operator:9.8.7"' <<< "${output}" || {
  echo "FAIL: operator image did not inherit global release coordinates" >&2
  exit 1
}
grep -q 'image: "registry.example.com/cetirun-portal:9.8.7"' <<< "${output}" || {
  echo "FAIL: portal image did not inherit global release coordinates" >&2
  exit 1
}
grep -q 'image: registry.example.com/cetirun-ai-gateway:9.8.7' <<< "${output}" || {
  echo "FAIL: AI Gateway image did not inherit global release coordinates" >&2
  exit 1
}
grep -q -- '--chart-repo-url=oci://registry.example.com/helm/cetirun' <<< "${output}" || {
  echo "FAIL: operator Runtime Chart URL did not inherit global.registry" >&2
  exit 1
}
grep -q -- '--chart-version=9.8.7' <<< "${output}" || {
  echo "FAIL: operator Runtime Chart version did not inherit global.version" >&2
  exit 1
}

echo "==> helm template platform (custom gateway)"
output=$(helm template platform "${chart_dir}" "${base_args[@]}" \
  --set-string cetirun-operator.istioGateway=gateway-resources/custom-gateway \
  --set-string cetirun-operator.cetirun.helmValues.networkPolicy.istioIngressNamespace=gateway-workloads \
  --set-string cetirun-portal.istio.gateway=gateway-resources/custom-gateway \
  --set-string cetirun-portal.istio.gatewayNamespace=gateway-workloads \
  --set-string cetirun-portal.istio.gatewayServiceAccount=custom-gateway-sa)
grep -q -- '- gateway-resources/custom-gateway' <<< "${output}" || {
  echo "FAIL: portal VirtualService did not use the configured gateway" >&2
  exit 1
}
grep -q 'kubernetes.io/metadata.name: gateway-workloads' <<< "${output}" || {
  echo "FAIL: portal policies did not use the configured gateway workload namespace" >&2
  exit 1
}
grep -q 'cluster.local/ns/gateway-workloads/sa/custom-gateway-sa' <<< "${output}" || {
  echo "FAIL: portal AuthorizationPolicy did not use the configured gateway service account" >&2
  exit 1
}
grep -q -- '--istio-gateway=gateway-resources/custom-gateway' <<< "${output}" || {
  echo "FAIL: operator did not use the configured gateway" >&2
  exit 1
}
grep -q 'istioIngressNamespace\\":\\"gateway-workloads' <<< "${output}" || {
  echo "FAIL: operator did not pass the custom ingress namespace to instance NetworkPolicies" >&2
  exit 1
}

echo "==> helm template platform (operator only)"
output=$(helm template platform "${chart_dir}" "${base_args[@]}" --set cetirun-portal.enabled=false)
if grep -q 'name: cetirun-portal' <<< "${output}"; then
  echo "FAIL: portal rendered while disabled" >&2
  exit 1
fi

admission_args=(
  --kube-version 1.37.0
  --set cetirun-operator.sandboxAdmission.enabled=true
  --set cetirun-operator.sandboxAdmission.enforce=true
  --set cetirun-operator.sandboxAdmission.podsEnabled=true
  --set-string cetirun-operator.sandboxAdmission.validationMode=strict
  --set-string cetirun-operator.sandboxAdmission.tlsSecretName=sandbox-admission-tls
  --set-string cetirun-operator.sandboxAdmission.caBundle=LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0tClFRPT0KLS0tLS1FTkQgQ0VSVElGSUNBVEUtLS0tLQ==
  --set-string 'cetirun-operator.sandboxAdmission.adminUsers[0]=platform-admin@example.com'
  --set-string cetirun-operator.sandboxAdmission.coordinationConfigMapName=configuration-coordination
  --set-string cetirun-operator.sandboxAdmission.coordinationConfigMapUID=11111111-1111-1111-1111-111111111111
)
portal_coordination_args=(
  --set-string cetirun-portal.configurationCoordination.namespace=cetirun-system
  --set-string cetirun-portal.configurationCoordination.name=configuration-coordination
  --set-string cetirun-portal.configurationCoordination.uid=11111111-1111-1111-1111-111111111111
)
# Without the bundled Portal, the release names no Portal service account.
no_portal_args=(
  --set cetirun-portal.enabled=false
  --set-string cetirun-operator.sandboxAdmission.portalServiceAccount=
)
portal_account=system:serviceaccount:cetirun-system:cetirun-portal

# Prints the Sandbox admission configuration: JSON in a quoted YAML string.
admission_config() {
  grep -A1 'name: CETIRUN_SANDBOX_ADMISSION$' <<< "$1" | tail -n 1
}

# Prints how many times that configuration lists a user as an administrator.
admission_admin_count() {
  { admission_config "$1" | grep -o 'adminUsers\\":\[[^]]*\]' | grep -oF "\\\"$2\\\"" || true; } | wc -l
}

echo "==> helm template platform (operator-only admission coordination)"
output=$(helm template platform "${chart_dir}" "${base_args[@]}" "${admission_args[@]}" \
  "${no_portal_args[@]}")
grep -q 'name: cetirun-sandbox-admission' <<< "${output}" || {
  echo "FAIL: operator admission was not rendered" >&2
  exit 1
}
grep -q 'coordinationConfigMapName\\":\\"configuration-coordination' <<< "${output}" || {
  echo "FAIL: operator admission did not render the coordination ConfigMap name" >&2
  exit 1
}
grep -q 'coordinationConfigMapUID\\":\\"11111111-1111-1111-1111-111111111111' <<< "${output}" || {
  echo "FAIL: operator admission did not render the coordination ConfigMap UID" >&2
  exit 1
}
[ "$(admission_admin_count "${output}" "${portal_account}")" -eq 0 ] || {
  echo "FAIL: operator admission made the service account of a Portal it does not install an administrator" >&2
  exit 1
}

echo "==> helm template platform (Portal service account without the Portal, expect failure)"
if output=$(helm template platform "${chart_dir}" "${base_args[@]}" "${admission_args[@]}" \
  --set cetirun-portal.enabled=false 2>&1); then
  echo "FAIL: platform made the Portal service account an administrator without installing the Portal" >&2
  exit 1
fi
grep -Fq 'clear cetirun-operator.sandboxAdmission.portalServiceAccount when the bundled Portal is disabled' <<< "${output}" || {
  echo "FAIL: platform did not ask to clear the Portal service account without the Portal" >&2
  exit 1
}

echo "==> helm template platform (operator coordination without Portal authority, expect failure)"
if output=$(helm template platform "${chart_dir}" "${base_args[@]}" "${admission_args[@]}" 2>&1); then
  echo "FAIL: platform rendered Operator coordination without matching Portal authority" >&2
  exit 1
fi
grep -Fq 'cetirun-portal.configurationCoordination must exactly match' <<< "${output}" || {
  echo "FAIL: platform did not reject the missing Portal authority with a coordination mismatch" >&2
  exit 1
}

echo "==> helm template platform (Portal disabled with inherited coordination values)"
output=$(helm template platform "${chart_dir}" "${base_args[@]}" "${admission_args[@]}" \
  "${portal_coordination_args[@]}" \
  "${no_portal_args[@]}" \
  --set-string cetirun-portal.configurationCoordination.namespace=stale-namespace \
  --set-string cetirun-portal.configurationCoordination.name=stale-coordination \
  --set-string cetirun-portal.configurationCoordination.uid=22222222-2222-2222-2222-222222222222)
if grep -q 'name: cetirun-portal' <<< "${output}"; then
  echo "FAIL: portal rendered while disabled with inherited coordination values" >&2
  exit 1
fi

echo "==> helm template platform (matched Operator and Portal coordination)"
output=$(helm template platform "${chart_dir}" "${base_args[@]}" "${admission_args[@]}" \
  "${portal_coordination_args[@]}")
grep -A1 'name: CONFIGURATION_COORDINATION_UID$' <<< "${output}" | grep -q 'value: "11111111-1111-1111-1111-111111111111"' || {
  echo "FAIL: Portal did not render the matched coordination ConfigMap UID" >&2
  exit 1
}
# The Portal writes with its service account, which the operator adds to the
# admission administrators.
[ "$(admission_admin_count "${output}" "${portal_account}")" -eq 1 ] || {
  echo "FAIL: admission does not list the Portal service account as an administrator exactly once" >&2
  exit 1
}
[ "$(admission_admin_count "${output}" platform-admin@example.com)" -eq 1 ] || {
  echo "FAIL: admission does not keep the configured administrator" >&2
  exit 1
}
default_admission=$(admission_config "${output}")

echo "==> helm template platform (Portal service account also listed in adminUsers)"
# Releases that list it already keep the same configuration, so an upgrade does
# not roll the admission server.
output=$(helm template platform "${chart_dir}" "${base_args[@]}" "${admission_args[@]}" \
  "${portal_coordination_args[@]}" \
  --set-string "cetirun-operator.sandboxAdmission.adminUsers[1]=${portal_account}")
[ "$(admission_admin_count "${output}" "${portal_account}")" -eq 1 ] || {
  echo "FAIL: admission lists the Portal service account more than once" >&2
  exit 1
}
[ "$(admission_config "${output}")" = "${default_admission}" ] || {
  echo "FAIL: listing the Portal service account changed the admission configuration" >&2
  exit 1
}

echo "==> helm template platform (custom Portal service account)"
output=$(helm template platform "${chart_dir}" "${base_args[@]}" "${admission_args[@]}" \
  "${portal_coordination_args[@]}" \
  --set-string cetirun-portal.serviceAccount.name=custom-portal \
  --set-string cetirun-operator.sandboxAdmission.portalServiceAccount=custom-portal)
grep -q 'serviceAccountName: custom-portal$' <<< "${output}" || {
  echo "FAIL: the Portal does not run as its custom service account" >&2
  exit 1
}
[ "$(admission_admin_count "${output}" system:serviceaccount:cetirun-system:custom-portal)" -eq 1 ] || {
  echo "FAIL: admission does not list the custom Portal service account exactly once" >&2
  exit 1
}
[ "$(admission_admin_count "${output}" "${portal_account}")" -eq 0 ] || {
  echo "FAIL: admission lists the default Portal service account instead of the custom one" >&2
  exit 1
}

echo "==> helm template platform (mismatched Portal service account, expect failure)"
if output=$(helm template platform "${chart_dir}" "${base_args[@]}" "${admission_args[@]}" \
  "${portal_coordination_args[@]}" \
  --set-string cetirun-portal.serviceAccount.name=custom-portal 2>&1); then
  echo "FAIL: platform made a service account other than the Portal's an administrator" >&2
  exit 1
fi
grep -Fq 'portalServiceAccount is cetirun-portal, but the Portal runs as cetirun-portal.serviceAccount.name custom-portal: set both to the same name' <<< "${output}" || {
  echo "FAIL: platform did not reject a Portal service account that differs from the Portal's" >&2
  exit 1
}

for field in namespace name uid; do
  echo "==> helm template platform (mismatched Portal coordination ${field}, expect failure)"
  if output=$(helm template platform "${chart_dir}" "${base_args[@]}" "${admission_args[@]}" \
    "${portal_coordination_args[@]}" \
    --set-string "cetirun-portal.configurationCoordination.${field}=mismatched" 2>&1); then
    echo "FAIL: platform rendered mismatched Portal coordination ${field}" >&2
    exit 1
  fi
  grep -Fq 'cetirun-portal.configurationCoordination must exactly match' <<< "${output}" || {
    echo "FAIL: platform did not reject mismatched Portal coordination ${field} with a coordination mismatch" >&2
    exit 1
  }
done

echo "==> helm template platform (Portal coordination without Operator, expect failure)"
if output=$(helm template platform "${chart_dir}" "${base_args[@]}" "${admission_args[@]}" \
  "${portal_coordination_args[@]}" --set cetirun-operator.enabled=false 2>&1); then
  echo "FAIL: platform rendered Portal coordination without Operator" >&2
  exit 1
fi
grep -Fq 'configuration coordination requires both Operator and Portal' <<< "${output}" || {
  echo "FAIL: platform did not reject Portal coordination without Operator with the required dependency" >&2
  exit 1
}

echo "==> helm template platform (Portal not an admission administrator, expect failure)"
# As after an upgrade with --reuse-values from a release without the value.
if output=$(helm template platform "${chart_dir}" "${base_args[@]}" "${admission_args[@]}" \
  "${portal_coordination_args[@]}" \
  --set-string cetirun-operator.sandboxAdmission.portalServiceAccount= 2>&1); then
  echo "FAIL: platform rendered enforced admission that refuses the Portal's writes" >&2
  exit 1
fi
grep -Fq "sandboxAdmission.adminUsers must include ${portal_account}, or set cetirun-operator.sandboxAdmission.portalServiceAccount to cetirun-portal" <<< "${output}" || {
  echo "FAIL: platform did not name the Portal service account the admission webhook must accept" >&2
  exit 1
}
output=$(helm template platform "${chart_dir}" "${base_args[@]}" "${admission_args[@]}" \
  "${portal_coordination_args[@]}" \
  --set-string cetirun-operator.sandboxAdmission.portalServiceAccount= \
  --set-string "cetirun-operator.sandboxAdmission.adminUsers[1]=${portal_account}")
[ "$(admission_config "${output}")" = "${default_admission}" ] || {
  echo "FAIL: listing the Portal service account instead of setting portalServiceAccount changed the admission configuration" >&2
  exit 1
}

# The admission server refuses to start with a blanket group, so the chart
# refuses one too; it cannot stand in for the Portal's service account.
for group in system:serviceaccounts:cetirun-system system:serviceaccounts; do
  echo "==> helm template platform (administrator group ${group}, expect failure)"
  if output=$(helm template platform "${chart_dir}" "${base_args[@]}" "${admission_args[@]}" \
    "${portal_coordination_args[@]}" \
    --set-string "cetirun-operator.sandboxAdmission.adminGroups[0]=${group}" 2>&1); then
    echo "FAIL: platform rendered the blanket administrator group ${group}" >&2
    exit 1
  fi
  grep -Fq 'sandboxAdmission.adminGroups cannot use system:authenticated, system:unauthenticated or the system:serviceaccounts groups' <<< "${output}" || {
    echo "FAIL: platform did not reject the blanket administrator group ${group}" >&2
    exit 1
  }
done

echo "==> helm template platform (portal only)"
output=$(helm template platform "${chart_dir}" "${base_args[@]}" --set cetirun-operator.enabled=false)
if grep -q 'name: cetirun-operator' <<< "${output}"; then
  echo "FAIL: operator rendered while disabled" >&2
  exit 1
fi

echo "==> helm template platform (AI Gateway installed separately)"
# Without the bundled Gateway the release namespace is free, and the Portal
# still follows it while the operator keeps its own namespace.
output=$(helm template platform "${chart_dir}" --set cetirun-operator.cetiadmin.enabled=false -f "${chart_dir}/values.yaml" --namespace portal-ns \
  --set istio.enabled=true \
  --set-string cetirun-operator.domainSuffix=platform.example.invalid \
  --set-string cetirun-portal.istio.host=portal.example.invalid \
  --set cetirun-ai-gateway.enabled=false)
if grep -q 'cetirun-ai-gateway-postgresql' <<< "${output}"; then
  echo "FAIL: the AI Gateway rendered while disabled" >&2
  exit 1
fi
grep -q 'namespace: portal-ns' <<< "${output}" || {
  echo "FAIL: portal was not rendered into the release namespace" >&2
  exit 1
}
grep -A1 'name: AI_GATEWAY_ENABLED' <<< "${output}" | grep -q 'value: "true"' || {
  echo "FAIL: the operator must use a separately installed AI Gateway" >&2
  exit 1
}

echo "==> helm template platform (admission with the Portal outside the operator namespace)"
# The Portal's service account is in the release namespace, where the Portal runs.
output=$(helm template platform "${chart_dir}" --set cetirun-operator.cetiadmin.enabled=false -f "${chart_dir}/values.yaml" --namespace portal-ns \
  --set istio.enabled=true \
  --set-string cetirun-operator.domainSuffix=platform.example.invalid \
  --set-string cetirun-portal.istio.host=portal.example.invalid \
  --set cetirun-ai-gateway.enabled=false \
  "${admission_args[@]}" "${portal_coordination_args[@]}")
[ "$(admission_admin_count "${output}" system:serviceaccount:portal-ns:cetirun-portal)" -eq 1 ] || {
  echo "FAIL: admission does not list the Portal service account in the release namespace" >&2
  exit 1
}
[ "$(admission_admin_count "${output}" "${portal_account}")" -eq 0 ] || {
  echo "FAIL: admission lists a Portal service account in the operator namespace" >&2
  exit 1
}

for flag in cetirun-operator.aiGateway.enabled cetirun-portal.features.aiGatewayEnabled cetirun-ai-gateway.operatorIntegration; do
  echo "==> helm template platform (${flag}=false, expect failure)"
  if helm template platform "${chart_dir}" "${base_args[@]}" --set "${flag}=false" 2>/dev/null; then
    echo "FAIL: platform rendered with ${flag}=false" >&2
    exit 1
  fi
done

echo "==> helm template platform (AI Gateway outside the operator namespace, expect failure)"
if helm template platform "${chart_dir}" --set cetirun-operator.cetiadmin.enabled=false -f "${chart_dir}/values.yaml" --namespace other-ns \
  --set istio.enabled=true \
  --set-string cetirun-operator.domainSuffix=platform.example.invalid \
  --set-string cetirun-portal.istio.host=portal.example.invalid 2>/dev/null; then
  echo "FAIL: platform rendered the AI Gateway outside the operator namespace" >&2
  exit 1
fi

# As the deployment helper does for a custom NAMESPACE: the Portal reads the
# shared models where the operator publishes them.
echo "==> helm template platform (custom namespace)"
output=$(helm template platform "${chart_dir}" --set cetirun-operator.cetiadmin.enabled=false -f "${chart_dir}/values.yaml" --namespace custom-ns \
  --set istio.enabled=true \
  --set-string cetirun-operator.domainSuffix=platform.example.invalid \
  --set-string cetirun-portal.istio.host=portal.example.invalid \
  --set-string cetirun-operator.namespace=custom-ns \
  --set-string cetirun-portal.aiGateway.catalogNamespace=custom-ns)
grep -A1 'name: AI_GATEWAY_CATALOG_NAMESPACE' <<< "${output}" | grep -q 'value: "custom-ns"' || {
  echo "FAIL: the Portal does not read the catalog in the operator namespace" >&2
  exit 1
}

echo "==> helm template platform (Portal catalog outside the operator namespace, expect failure)"
if helm template platform "${chart_dir}" --set cetirun-operator.cetiadmin.enabled=false -f "${chart_dir}/values.yaml" --namespace custom-ns \
  --set istio.enabled=true \
  --set-string cetirun-operator.domainSuffix=platform.example.invalid \
  --set-string cetirun-portal.istio.host=portal.example.invalid \
  --set-string cetirun-operator.namespace=custom-ns 2>/dev/null; then
  echo "FAIL: platform rendered a Portal that reads the catalog outside the operator namespace" >&2
  exit 1
fi

echo "==> helm template platform (missing Istio confirmation, expect failure)"
if helm template platform "${chart_dir}" --set cetirun-operator.cetiadmin.enabled=false --namespace cetirun-system \
  --set-string cetirun-operator.domainSuffix=platform.example.invalid \
  --set-string cetirun-portal.istio.host=portal.example.invalid 2>/dev/null; then
  echo "FAIL: platform rendered without Istio confirmation" >&2
  exit 1
fi

echo "==> helm template platform (missing portal hostname, expect failure)"
if helm template platform "${chart_dir}" --set cetirun-operator.cetiadmin.enabled=false --namespace cetirun-system \
  --set istio.enabled=true \
  --set-string cetirun-operator.domainSuffix=platform.example.invalid 2>/dev/null; then
  echo "FAIL: platform rendered without portal hostname" >&2
  exit 1
fi

echo "==> helm template platform (missing gateway service account, expect failure)"
if helm template platform "${chart_dir}" "${base_args[@]}" \
  --set-string cetirun-portal.istio.gatewayServiceAccount= 2>/dev/null; then
  echo "FAIL: platform rendered without gateway service account" >&2
  exit 1
fi

echo "==> helm template platform (missing operator domain suffix, expect failure)"
if helm template platform "${chart_dir}" --set cetirun-operator.cetiadmin.enabled=false --namespace cetirun-system \
  --set istio.enabled=true \
  --set-string cetirun-portal.istio.host=portal.example.invalid 2>/dev/null; then
  echo "FAIL: platform rendered without operator domain suffix" >&2
  exit 1
fi

echo "==> helm template platform (missing operator gateway, expect failure)"
if helm template platform "${chart_dir}" --set cetirun-operator.cetiadmin.enabled=false --namespace cetirun-system \
  --set istio.enabled=true \
  --set-string cetirun-operator.domainSuffix=platform.example.invalid \
  --set-string cetirun-operator.istioGateway= \
  --set-string cetirun-portal.istio.host=portal.example.invalid 2>/dev/null; then
  echo "FAIL: platform rendered without operator gateway" >&2
  exit 1
fi

echo "==> helm template platform (malformed operator gateway, expect failure)"
if helm template platform "${chart_dir}" --set cetirun-operator.cetiadmin.enabled=false --namespace cetirun-system \
  --set istio.enabled=true \
  --set-string cetirun-operator.domainSuffix=platform.example.invalid \
  --set-string cetirun-operator.istioGateway=not-a-qualified-gateway \
  --set-string cetirun-portal.istio.host=portal.example.invalid 2>/dev/null; then
  echo "FAIL: platform rendered with a malformed operator gateway" >&2
  exit 1
fi

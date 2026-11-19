#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# e2e-test.sh — End-to-end test for CetiRun.
#
# Deploys a test Agent, waits for it to become Ready,
# verifies key resources (namespace, secret, StatefulSet, pod, VirtualService),
# validates gateway routing and OAuth2 redirect to Entra ID, optionally checks
# the openclaw image tag, and cleans up.
#
# Prerequisites:
#   - Explicitly authorized cluster and the target pins below
#   - Agent CRD installed
#   - Operator, portal, and OAuth2 Proxy pods running
#
# Environment variables:
#   E2E_KUBECONFIG        — Explicit absolute kubeconfig path (required)
#   E2E_CONTEXT           — Explicit context name (required)
#   E2E_CLUSTER_UID       — Expected kube-system namespace UID (required)
#   TEST_ALIAS            — New instance alias (default: generated)
#   AGENT_TYPE            — Agent type               (default: hermes)
#   TEST_NAMESPACE        — Operator namespace        (default: cetirun-system)
#   PORTAL_NAMESPACE      — Portal namespace          (default: cetirun-portal; set to TEST_NAMESPACE for umbrella)
#   READY_TIMEOUT         — Seconds to wait for Ready (default: 600)
#   CLEANUP_TIMEOUT       — Seconds to observe controller cleanup (default: 180)
#   DOMAIN                — Base domain for routing   (auto-detected from operator Helm values)
#   E2E_OIDC_CLIENT_ID    — Expected public OIDC client ID (otherwise read from Istio release values)
#   E2E_CA_FILE           — Optional absolute PEM CA bundle for ingress verification
#   EXPECTED_IMAGE_TAG    — If set, verify agent pod uses this image tag
#   SKIP_CLEANUP          — If non-empty, skip cleanup on success

set -euo pipefail

E2E_KUBECONFIG="${E2E_KUBECONFIG:-}"
E2E_CONTEXT="${E2E_CONTEXT:-}"
E2E_CLUSTER_UID="${E2E_CLUSTER_UID:-}"
TEST_ALIAS="${TEST_ALIAS:-e2e-${RANDOM}-${RANDOM}}"
AGENT_TYPE="${AGENT_TYPE:-hermes}"
TEST_NAMESPACE="${TEST_NAMESPACE:-cetirun-system}"
PORTAL_NAMESPACE="${PORTAL_NAMESPACE:-cetirun-portal}"
READY_TIMEOUT="${READY_TIMEOUT:-600}"
CLEANUP_TIMEOUT="${CLEANUP_TIMEOUT:-180}"
INSTANCE_NS="cetirun-${TEST_ALIAS}"
INSTANCE_UID=""
INSTANCE_NS_UID=""
CREATE_ATTEMPTED=false
RUN_LABEL="qualification.cetirun.microsoft.com/e2e-run"
RUN_ID="run-$$-${RANDOM}-${RANDOM}"
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"

pass() { echo "  ✓ $1"; }
fail() { echo "  ✗ $1" >&2; FAILURES=$((FAILURES + 1)); }
FAILURES=0

kubectl() {
  KUBECTL_KUBERC=false command kubectl --kubeconfig "${E2E_KUBECONFIG}" \
    --context "${E2E_CONTEXT}" --request-timeout=15s "$@"
}

helm() {
  command helm --kubeconfig "${E2E_KUBECONFIG}" --kube-context "${E2E_CONTEXT}" "$@"
}

valid_label() {
  [[ ${#1} -ge 1 && ${#1} -le 63 && "$1" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]]
}

validate_inputs() {
  if [[ "${E2E_KUBECONFIG}" != /* || ! -f "${E2E_KUBECONFIG}" ||
        -z "${E2E_CONTEXT}" || -z "${E2E_CLUSTER_UID}" ]] ||
     ! valid_label "${TEST_ALIAS}" || ! valid_label "${INSTANCE_NS}" ||
     ! valid_label "${TEST_NAMESPACE}" || ! valid_label "${PORTAL_NAMESPACE}" ||
     [[ ! "${READY_TIMEOUT}" =~ ^[1-9][0-9]{0,3}$ || ! "${CLEANUP_TIMEOUT}" =~ ^[1-9][0-9]{0,3}$ ]]; then
    echo "ERROR: Explicit kubeconfig/context/cluster UID, valid names and bounded positive timeouts are required." >&2
    return 1
  fi
}

check_target() {
  local value
  if ! value="$(kubectl get namespace kube-system -o json 2>/dev/null)" ||
     ! jq -e --arg uid "${E2E_CLUSTER_UID}" \
       '.metadata.uid == $uid and .metadata.deletionTimestamp == null' <<< "${value}" >/dev/null 2>&1; then
    echo "ERROR: Expected cluster identity could not be verified; no mutation authorized." >&2
    return 1
  fi
}

check_client_config() {
  local value
  if ! value="$(kubectl config view --minify -o json 2>/dev/null)" ||
     ! jq -e --arg context "${E2E_CONTEXT}" '
       .["current-context"] == $context and
       (.contexts | length) == 1 and (.clusters | length) == 1 and (.users | length) == 1 and
       .contexts[0].name == $context and
       .contexts[0].context.cluster == .clusters[0].name and
       .contexts[0].context.user == .users[0].name and
       .users[0].user.exec == null and .users[0].user["auth-provider"] == null and
       .clusters[0].cluster["insecure-skip-tls-verify"] != true and
       (.clusters[0].cluster.server | test("^https://[^/@?#]+(:[0-9]+)?(/[^?#]*)?$"))
       ' <<< "${value}" >/dev/null 2>&1; then
    echo "ERROR: An explicit HTTPS context with non-plugin authentication is required." >&2
    return 1
  fi
}

read_owned_agent() {
  local value
  if ! value="$(kubectl get agent "${TEST_ALIAS}" --ignore-not-found -o json 2>/dev/null)"; then
    echo "ERROR: Agent observation failed; absence is not established." >&2
    return 1
  fi
  if [[ -n "${value}" ]] && ! jq -e --arg name "${TEST_ALIAS}" --arg uid "${INSTANCE_UID}" \
    --arg label "${RUN_LABEL}" --arg run "${RUN_ID}" \
    '.metadata.name == $name and .metadata.uid == $uid and $uid != "" and
     .metadata.labels[$label] == $run and
     (.metadata.resourceVersion | type == "string" and length > 0)' <<< "${value}" >/dev/null 2>&1; then
    echo "ERROR: Agent ownership changed or is incomplete; replacement retained." >&2
    return 1
  fi
  printf '%s' "${value}"
}

owned_agent_phase() {
  local value
  value="$(read_owned_agent)" || return 1
  if [[ -z "${value}" ]]; then
    echo "ERROR: Run-owned Agent disappeared before its lifecycle check." >&2
    return 1
  fi
  jq -r '.status.phase // "Pending"' <<< "${value}"
}

observe_owned_namespace() {
  local value uid
  if ! value="$(kubectl get namespace "${INSTANCE_NS}" --ignore-not-found -o json 2>/dev/null)"; then
    echo "ERROR: Namespace observation failed; cleanup is unconfirmed." >&2
    return 1
  fi
  if [[ -z "${value}" ]]; then
    return 0
  fi
  if ! uid="$(jq -er --arg name "${INSTANCE_NS}" --arg agent "${TEST_ALIAS}" --arg uid "${INSTANCE_UID}" \
    'select(.metadata.name == $name and $uid != "" and
      any(.metadata.ownerReferences[]?;
        .apiVersion == "cetirun.microsoft.com/v1alpha1" and .kind == "Agent" and
        .name == $agent and .uid == $uid)) |
      .metadata.uid | select(type == "string" and length > 0)' <<< "${value}" 2>/dev/null)" ||
     [[ -n "${INSTANCE_NS_UID}" && "${INSTANCE_NS_UID}" != "${uid}" ]]; then
    echo "ERROR: Namespace ownership changed or is unconfirmed; namespace retained." >&2
    return 1
  fi
  INSTANCE_NS_UID="${uid}"
  NAMESPACE_PRESENT=true
}

create_instance() {
  local existing created resource name
  check_target || return 1
  for resource in agent namespace; do
    name="${TEST_ALIAS}"
    [[ "${resource}" == "namespace" ]] && name="${INSTANCE_NS}"
    if ! existing="$(kubectl get "${resource}" "${name}" --ignore-not-found -o json 2>/dev/null)"; then
      echo "ERROR: Initial resource absence could not be established; refusing creation." >&2
      return 1
    fi
    if [[ -n "${existing}" ]]; then
      echo "ERROR: Test Agent or namespace already exists; choose a fresh alias. Nothing was adopted or deleted." >&2
      return 1
    fi
  done
  CREATE_ATTEMPTED=true
  if ! created="$(jq -n --arg name "${TEST_ALIAS}" --arg agentType "${AGENT_TYPE}" \
    --arg label "${RUN_LABEL}" --arg run "${RUN_ID}" \
    '{apiVersion:"cetirun.microsoft.com/v1alpha1",kind:"Agent",
      metadata:{name:$name,labels:{($label):$run}},
      spec:{agent:{type:$agentType},gatewayTokenSecretRef:"gateway-token",
        user:{alias:$name,displayName:"E2E Test User",team:"e2e-testing",email:"e2e-test@example.com"},
        helm:{values:{skills:{shared:false}}}}}' |
      kubectl create -f - -o json 2>/dev/null)"; then
    echo "ERROR: Agent create outcome is unknown; do not retry or delete by name. Inspect the test alias explicitly." >&2
    return 1
  fi
  if ! INSTANCE_UID="$(jq -er --arg name "${TEST_ALIAS}" --arg label "${RUN_LABEL}" --arg run "${RUN_ID}" \
    'select(.apiVersion == "cetirun.microsoft.com/v1alpha1" and .kind == "Agent" and
       .metadata.name == $name and .metadata.labels[$label] == $run) |
     .metadata.uid | select(type == "string" and length > 0)' <<< "${created}" 2>/dev/null)"; then
    INSTANCE_UID=""
    echo "ERROR: Create did not return confirmed ownership; resource retained for explicit recovery." >&2
    return 1
  fi
}

set_suspended() {
  local value patch
  check_target || return 1
  value="$(read_owned_agent)" || return 1
  if [[ -z "${value}" ]] ||
     ! jq -e '.metadata.deletionTimestamp == null' <<< "${value}" >/dev/null 2>&1; then
    echo "ERROR: Owned Agent is absent or terminating; lifecycle mutation refused." >&2
    return 1
  fi
  patch="$(jq -c --argjson suspended "$1" \
    '[{op:"test",path:"/metadata/uid",value:.metadata.uid},
      {op:"test",path:"/metadata/resourceVersion",value:.metadata.resourceVersion},
      {op:"add",path:"/spec/suspended",value:$suspended}]' <<< "${value}")" || return 1
  if ! kubectl patch agent "${TEST_ALIAS}" --type=json -p "${patch}" >/dev/null 2>&1; then
    echo "ERROR: Conditional lifecycle mutation failed; no automatic retry was attempted." >&2
    return 1
  fi
}

cleanup() {
  local value options deadline
  if [[ -z "${INSTANCE_UID}" ]]; then
    if [[ "${CREATE_ATTEMPTED}" == "true" ]]; then
      echo "ERROR: Creation ownership is unconfirmed; no cleanup mutation attempted." >&2
      return 1
    fi
    return 0
  fi
  echo ""
  echo "==> Cleanup: observing the run-owned Agent and namespace..."
  check_target || return 1
  value="$(read_owned_agent)" || return 1
  NAMESPACE_PRESENT=false
  observe_owned_namespace || return 1
  if [[ -n "${value}" ]] && jq -e '.metadata.deletionTimestamp == null' <<< "${value}" >/dev/null; then
    options="$(jq -c '{apiVersion:"v1",kind:"DeleteOptions",
      preconditions:{uid:.metadata.uid,resourceVersion:.metadata.resourceVersion},
      propagationPolicy:"Background"}' <<< "${value}")" || return 1
    if ! kubectl delete --raw "/apis/cetirun.microsoft.com/v1alpha1/agents/${TEST_ALIAS}" \
      -f - <<< "${options}" >/dev/null 2>&1; then
      echo "ERROR: Conditional Agent deletion is unconfirmed; retained for recovery without replay." >&2
      return 1
    fi
  fi
  deadline=$((SECONDS + CLEANUP_TIMEOUT))
  while true; do
    value="$(read_owned_agent)" || return 1
    NAMESPACE_PRESENT=false
    observe_owned_namespace || return 1
    if [[ -z "${value}" && "${NAMESPACE_PRESENT}" == "false" ]]; then
      echo "    Cleanup verified: owned Agent and namespace are absent."
      return 0
    fi
    if (( SECONDS >= deadline )); then
      echo "ERROR: Controller cleanup timed out; resources retained. No namespace or finalizer was forced." >&2
      return 1
    fi
    sleep 1
  done
}

finish() {
  local result=$?
  trap - EXIT
  if ! cleanup && (( result == 0 )); then
    result=1
  fi
  if (( result == 0 )); then
    echo " E2E PASSED (controller cleanup verified)"
  fi
  exit "${result}"
}

check_ingress() {
  local domain="${DOMAIN:-}" client_id="${E2E_OIDC_CLIENT_ID:-}" values
  local -a arguments
  if [[ -z "${domain}" ]]; then
    if ! values="$(helm get values cetirun-operator -n "${TEST_NAMESPACE}" -o json 2>/dev/null)" ||
       ! domain="$(jq -er '.domainSuffix | select(type == "string" and length > 0)' <<< "${values}" 2>/dev/null)"; then
      echo "ERROR: An explicit ingress domain or readable operator domain value is required." >&2
      return 1
    fi
  fi
  if [[ -z "${client_id}" ]]; then
    if ! values="$(helm get values cetirun-istio -n cetirun-istio -o json 2>/dev/null)" ||
       ! client_id="$(jq -er '.clientId | select(type == "string" and length > 0)' <<< "${values}" 2>/dev/null)"; then
      echo "ERROR: An expected OIDC client identifier is required for the ingress check." >&2
      return 1
    fi
  fi
  arguments=(--origin "https://${domain}" --agent-name "${TEST_ALIAS}" --client-id "${client_id}")
  if [[ -n "${E2E_CA_FILE:-}" ]]; then
    arguments+=(--ca-file "${E2E_CA_FILE}")
  fi
  python3 -B "${SCRIPT_DIR}/e2e-ingress-check.py" "${arguments[@]}"
}

main() {
validate_inputs

echo "==========================================="
echo " CetiRun E2E Test"
echo "==========================================="
echo ""
echo "Test alias:    ${TEST_ALIAS}"
echo "Agent type:    ${AGENT_TYPE}"
echo "Operator NS:   ${TEST_NAMESPACE}"
echo "Portal NS:     ${PORTAL_NAMESPACE}"
echo "Instance NS:   ${INSTANCE_NS}"
echo "Ready timeout: ${READY_TIMEOUT}s"
echo ""

if [[ "${AGENT_TYPE}" != "openclaw" && "${AGENT_TYPE}" != "hermes" ]]; then
  echo "ERROR: AGENT_TYPE must be 'openclaw' or 'hermes', got '${AGENT_TYPE}'." >&2
  exit 1
fi

# ── Prerequisite checks ──────────────────────────────────────────────────────
echo "==> Checking prerequisites..."
for cmd in kubectl helm jq python3 base64; do
  if ! type -P "$cmd" &>/dev/null; then
    echo "ERROR: ${cmd} is required but not installed." >&2
    exit 1
  fi
done

check_client_config
check_target
if ! kubectl get crd agents.cetirun.microsoft.com &>/dev/null; then
  echo "ERROR: Agent CRD not found. Is the operator deployed?" >&2
  exit 1
fi

# Check operator is running
OPERATOR_PODS=$(kubectl get pods -n "${TEST_NAMESPACE}" -l app.kubernetes.io/name=cetirun-operator --field-selector=status.phase=Running -o name 2>/dev/null | wc -l || echo 0)
if (( OPERATOR_PODS == 0 )); then
  echo "ERROR: No running operator pods found in ${TEST_NAMESPACE}." >&2
  exit 1
fi
echo "    Operator running (${OPERATOR_PODS} pod(s))."

# Check portal is running
PORTAL_PODS=$(kubectl get pods -n "${PORTAL_NAMESPACE}" \
  -l app.kubernetes.io/name=cetirun-portal \
  --field-selector=status.phase=Running -o name 2>/dev/null | wc -l || echo 0)
if (( PORTAL_PODS == 0 )); then
  echo "ERROR: No running portal pods found in ${PORTAL_NAMESPACE}." >&2
  exit 1
fi
echo "    Portal running (${PORTAL_PODS} pod(s))."

# Check OAuth2 Proxy is running
OAUTH2_PODS=$(kubectl get pods -n cetirun-istio -l app=oauth2-proxy --field-selector=status.phase=Running -o name 2>/dev/null | wc -l || echo 0)
if (( OAUTH2_PODS == 0 )); then
  echo "ERROR: No running oauth2-proxy pods found in cetirun-istio namespace." >&2
  exit 1
fi
echo "    OAuth2 Proxy running (${OAUTH2_PODS} pod(s))."

echo "    Prerequisites OK."

# ── Deploy test instance ─────────────────────────────────────────────────────
echo ""
echo "==> Creating test Agent '${TEST_ALIAS}'..."
trap finish EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
create_instance
echo "    New Agent identity recorded."

# ── Wait for Ready ────────────────────────────────────────────────────────────
echo ""
echo "==> Waiting for instance to reach Ready phase (timeout: ${READY_TIMEOUT}s)..."
DEADLINE=$((SECONDS + READY_TIMEOUT))
LAST_PHASE=""
while true; do
  PHASE="$(owned_agent_phase)"

  if [[ "${PHASE}" != "${LAST_PHASE}" ]]; then
    echo "    Phase: ${PHASE}"
    LAST_PHASE="${PHASE}"
  fi

  if [[ "${PHASE}" == "Ready" ]]; then
    echo "    Instance is Ready!"
    break
  fi

  if [[ "${PHASE}" == "Failed" ]]; then
    echo "ERROR: Instance entered Failed phase." >&2
    MSG=$(kubectl get agent "${TEST_ALIAS}" -n "${TEST_NAMESPACE}" \
      -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}' 2>/dev/null || echo "")
    [[ -n "$MSG" ]] && echo "  Message: ${MSG}" >&2
    exit 1
  fi

  if (( SECONDS > DEADLINE )); then
    echo "ERROR: Timed out waiting for Ready phase after ${READY_TIMEOUT}s." >&2
    echo "  Last phase: ${PHASE}" >&2
    kubectl describe agent "${TEST_ALIAS}" -n "${TEST_NAMESPACE}" 2>/dev/null || true
    exit 1
  fi

  sleep 10
done

# ── Verify resources ─────────────────────────────────────────────────────────
echo ""
echo "==> Verifying resources..."

# Namespace
NAMESPACE_PRESENT=false
observe_owned_namespace
if [[ "${NAMESPACE_PRESENT}" == "true" ]]; then
  pass "Namespace ${INSTANCE_NS} exists"
else
  fail "Namespace ${INSTANCE_NS} not found"
fi

# Gateway token secret
if kubectl get secret gateway-token -n "${INSTANCE_NS}" &>/dev/null; then
  pass "Gateway token secret exists"
else
  fail "Gateway token secret not found"
fi

# Helm release (StatefulSet)
STS_COUNT=$(kubectl get statefulset -n "${INSTANCE_NS}" -o name 2>/dev/null | wc -l || echo 0)
if (( STS_COUNT > 0 )); then
  pass "StatefulSet found (${STS_COUNT})"
else
  fail "No StatefulSet found in ${INSTANCE_NS}"
fi

# Pod running
RUNNING_PODS=$(kubectl get pods -n "${INSTANCE_NS}" --field-selector=status.phase=Running -o name 2>/dev/null | wc -l || echo 0)
if (( RUNNING_PODS > 0 )); then
  pass "Running pod(s) found (${RUNNING_PODS})"
else
  fail "No running pods in ${INSTANCE_NS}"
fi

POD_NAME=$(kubectl get pods -n "${INSTANCE_NS}" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")

if [[ "${AGENT_TYPE}" == "hermes" && -n "${POD_NAME}" ]]; then
  echo ""
  echo "==> Verifying Hermes services inside the pod..."

  API_SERVER_KEY=$(kubectl get secret gateway-token -n "${INSTANCE_NS}" \
    -o jsonpath='{.data.API_SERVER_KEY}' 2>/dev/null | base64 -d 2>/dev/null || echo "")
  if [[ -n "${API_SERVER_KEY}" ]]; then
    pass "Hermes API_SERVER_KEY secret exists"
  else
    fail "Hermes API_SERVER_KEY secret not found"
  fi

  HERMES_ENV=$(kubectl get secret gateway-token -n "${INSTANCE_NS}" \
    -o jsonpath='{.data.hermes\.env}' 2>/dev/null | base64 -d 2>/dev/null || echo "")
  if echo "${HERMES_ENV}" | grep -q '^API_SERVER_KEY='; then
    pass "Hermes hermes.env secret contains API_SERVER_KEY"
  else
    fail "Hermes hermes.env secret missing API_SERVER_KEY"
  fi

  wait_for_pod_http() {
    local label="$1"
    local url="$2"
    local deadline=$((SECONDS + 120))
    while true; do
      if kubectl exec -n "${INSTANCE_NS}" "${POD_NAME}" -- curl -fsS --max-time 5 "${url}" >/dev/null 2>&1; then
        pass "${label} reachable"
        return
      fi
      if (( SECONDS > deadline )); then
        fail "${label} not reachable at ${url}"
        return
      fi
      sleep 5
    done
  }

  wait_for_pod_http "Hermes API health" "http://127.0.0.1:8642/health"
fi

# VirtualService
if kubectl get virtualservice -n "${INSTANCE_NS}" -o name 2>/dev/null | grep -q .; then
  pass "VirtualService exists"
else
  fail "VirtualService not found"
fi

# ── Suspend / Unsuspend cycle ─────────────────────────────────────────────────
echo ""
echo "==> Testing suspend / unsuspend cycle..."

# Determine PVC name (StatefulSet volumeClaimTemplate "data" + pod ordinal 0)
mapfile -t STS_NAMES < <(kubectl get statefulset -n "${INSTANCE_NS}" -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null || true)

if (( ${#STS_NAMES[@]} == 1 )) && [[ -n "${STS_NAMES[0]}" ]]; then
  STS_NAME="${STS_NAMES[0]}"
elif (( ${#STS_NAMES[@]} == 0 )); then
  fail "No StatefulSets found in namespace ${INSTANCE_NS}"
  STS_NAME=""
else
  fail "Expected 1 StatefulSet in namespace ${INSTANCE_NS}, found ${#STS_NAMES[@]}"
  STS_NAME=""
fi
PVC_NAME="data-${STS_NAME}-0"

if [[ -n "${STS_NAME}" ]]; then
  # Verify PVC exists and is Bound before suspend
  PVC_PHASE=$(kubectl get pvc "${PVC_NAME}" -n "${INSTANCE_NS}" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
  if [[ "${PVC_PHASE}" == "Bound" ]]; then
    pass "PVC ${PVC_NAME} exists and is Bound (before suspend)"
  else
    fail "PVC ${PVC_NAME} phase is '${PVC_PHASE}', expected 'Bound' (before suspend)"
  fi
fi

# Suspend the instance
echo "    Patching suspended=true..."
set_suspended true

# Wait for Suspended phase
SUSPEND_TIMEOUT=300
SUSPEND_DEADLINE=$((SECONDS + SUSPEND_TIMEOUT))
while true; do
  PHASE="$(owned_agent_phase)"
  if [[ "${PHASE}" == "Suspended" ]]; then
    pass "Phase transitioned to Suspended"
    break
  fi
  if (( SECONDS > SUSPEND_DEADLINE )); then
    fail "Timed out waiting for Suspended phase (last: ${PHASE})"
    break
  fi
  sleep 5
done

# Verify: StatefulSet replicas=0
STS_REPLICAS=$(kubectl get statefulset -n "${INSTANCE_NS}" -o jsonpath='{.items[0].spec.replicas}' 2>/dev/null || echo "?")
if [[ "${STS_REPLICAS}" == "0" ]]; then
  pass "StatefulSet replicas=0 when suspended"
else
  fail "StatefulSet replicas=${STS_REPLICAS}, expected 0"
fi

# Verify: no running pods (wait for termination — pods may still be shutting down)
POD_GONE_DEADLINE=$((SECONDS + 120))
while true; do
  RUNNING_PODS_SUSPENDED=$(kubectl get pods -n "${INSTANCE_NS}" --field-selector=status.phase=Running -o name 2>/dev/null | wc -l || echo 0)
  if (( RUNNING_PODS_SUSPENDED == 0 )); then
    pass "No running pods when suspended"
    break
  fi
  if (( SECONDS > POD_GONE_DEADLINE )); then
    fail "Found ${RUNNING_PODS_SUSPENDED} running pod(s) when suspended after 120s, expected 0"
    break
  fi
  sleep 5
done

# Verify: VirtualService deleted
if kubectl get virtualservice -n "${INSTANCE_NS}" -o name 2>/dev/null | grep -q .; then
  fail "VirtualService still exists when suspended"
else
  pass "VirtualService deleted when suspended"
fi

# Verify: PVC preserved during suspend
PVC_PHASE_SUSPENDED=$(kubectl get pvc "${PVC_NAME}" -n "${INSTANCE_NS}" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
if [[ "${PVC_PHASE_SUSPENDED}" == "Bound" ]]; then
  pass "PVC ${PVC_NAME} preserved and Bound during suspend"
else
  fail "PVC ${PVC_NAME} phase is '${PVC_PHASE_SUSPENDED}' during suspend, expected 'Bound'"
fi

# Unsuspend the instance
echo "    Patching suspended=false..."
set_suspended false

# Wait for Ready phase
UNSUSPEND_DEADLINE=$((SECONDS + READY_TIMEOUT))
LAST_PHASE_UNSUSPEND=""
while true; do
  PHASE="$(owned_agent_phase)"
  if [[ "${PHASE}" != "${LAST_PHASE_UNSUSPEND}" ]]; then
    echo "    Phase: ${PHASE}"
    LAST_PHASE_UNSUSPEND="${PHASE}"
  fi
  if [[ "${PHASE}" == "Ready" ]]; then
    pass "Phase transitioned back to Ready after unsuspend"
    break
  fi
  if [[ "${PHASE}" == "Failed" ]]; then
    fail "Instance entered Failed phase during unsuspend"
    break
  fi
  if (( SECONDS > UNSUSPEND_DEADLINE )); then
    fail "Timed out waiting for Ready after unsuspend (last: ${PHASE})"
    break
  fi
  sleep 10
done

# Verify: StatefulSet replicas=1
STS_REPLICAS_AFTER=$(kubectl get statefulset -n "${INSTANCE_NS}" -o jsonpath='{.items[0].spec.replicas}' 2>/dev/null || echo "?")
if [[ "${STS_REPLICAS_AFTER}" == "1" ]]; then
  pass "StatefulSet replicas=1 after unsuspend"
else
  fail "StatefulSet replicas=${STS_REPLICAS_AFTER} after unsuspend, expected 1"
fi

# Verify: pod running again
RUNNING_PODS_AFTER=$(kubectl get pods -n "${INSTANCE_NS}" --field-selector=status.phase=Running -o name 2>/dev/null | wc -l || echo 0)
if (( RUNNING_PODS_AFTER > 0 )); then
  pass "Running pod(s) restored after unsuspend (${RUNNING_PODS_AFTER})"
else
  fail "No running pods after unsuspend"
fi

# Verify: VirtualService restored
if kubectl get virtualservice -n "${INSTANCE_NS}" -o name 2>/dev/null | grep -q .; then
  pass "VirtualService restored after unsuspend"
else
  fail "VirtualService not found after unsuspend"
fi

# Verify: PVC still exists after full cycle
PVC_PHASE_AFTER=$(kubectl get pvc "${PVC_NAME}" -n "${INSTANCE_NS}" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
if [[ "${PVC_PHASE_AFTER}" == "Bound" ]]; then
  pass "PVC ${PVC_NAME} preserved through full suspend/unsuspend cycle"
else
  fail "PVC ${PVC_NAME} phase is '${PVC_PHASE_AFTER}' after unsuspend, expected 'Bound'"
fi

# ── Verify routing ────────────────────────────────────────────────────────────
echo ""
echo "==> Verifying gateway routing..."

if check_ingress; then
  pass "Native UI sign-in challenge verified over HTTPS"
else
  fail "Native UI sign-in challenge could not be verified"
fi
echo "    Not tested: owner sign-in, authorized native UI/WebSocket access and model/tool traffic."

# ── Verify image tags ───────────────────────────────────────────────────────────────
if [[ -n "${EXPECTED_IMAGE_TAG:-}" ]]; then
  echo ""
  echo "==> Verifying ${AGENT_TYPE} image tag..."
  image_name="${AGENT_TYPE}-gateway"
  ACTUAL_IMAGE=$(kubectl get pods -n "${INSTANCE_NS}" \
    -o jsonpath='{.items[0].spec.containers[*].image}' 2>/dev/null | tr ' ' '\n' | grep "${image_name}" || echo "")

  if echo "${ACTUAL_IMAGE}" | grep -q ":${EXPECTED_IMAGE_TAG}"; then
    pass "${AGENT_TYPE} image uses expected tag (${EXPECTED_IMAGE_TAG})"
  else
    fail "${AGENT_TYPE} image tag mismatch: expected '${EXPECTED_IMAGE_TAG}', got '${ACTUAL_IMAGE}'"
  fi
fi

# ── Verify Istio EnvoyFilter alias check ─────────────────────────────────────
echo ""
echo "==> Verifying EnvoyFilter alias-check configuration..."

ENVOY_FILTER_LUA=$(kubectl get envoyfilter cetirun-alias-check -n aks-istio-ingress \
  -o jsonpath='{.spec.configPatches[0].patch.value.typed_config.inline_code}' 2>/dev/null || echo "")

if [[ -n "${ENVOY_FILTER_LUA}" ]]; then
  pass "EnvoyFilter cetirun-alias-check exists with Lua code"
else
  fail "EnvoyFilter cetirun-alias-check not found or has no Lua code"
fi

# Verify aliasExemptPrefixes are rendered
for prefix in oauth2 portal llmapi; do
  if echo "${ENVOY_FILTER_LUA}" | grep -Fq "^${prefix}%."; then
    pass "Alias exempt prefix '${prefix}' rendered in EnvoyFilter"
  else
    fail "Alias exempt prefix '${prefix}' not found in EnvoyFilter"
  fi
done

# Verify adminAliases are rendered into the portal admins ConfigMap
# (admin authorization moved from a per-host Envoy Lua check to the portal
# /admin route guard backed by the cetirun-portal-admins ConfigMap).
ADMIN_ALIASES=$(helm get values cetirun-istio -n cetirun-istio -o json 2>/dev/null \
  | jq -r '.authPolicy.adminAliases // [] | .[]' 2>/dev/null || echo "")
if [[ -n "${ADMIN_ALIASES}" ]]; then
  PORTAL_ADMINS_CM=$(kubectl get configmap cetirun-portal-admins -n "${PORTAL_NAMESPACE}" \
    -o jsonpath='{.data.aliases}' 2>/dev/null || echo "")
  if [[ -n "${PORTAL_ADMINS_CM}" ]]; then
    pass "cetirun-portal-admins ConfigMap exists"
  else
    fail "cetirun-portal-admins ConfigMap missing or empty (expected for /admin authorization)"
  fi
  while IFS= read -r admin; do
    if echo "${PORTAL_ADMINS_CM}" | grep -Fxqi "${admin}"; then
      pass "Admin alias '${admin}' rendered in portal admins ConfigMap"
    else
      fail "Admin alias '${admin}' not found in portal admins ConfigMap"
    fi
  done <<< "${ADMIN_ALIASES}"
else
  echo "    SKIP: No adminAliases configured — skipping admin check."
fi

# ── Result ────────────────────────────────────────────────────────────────────
echo ""
echo "==========================================="
if (( FAILURES > 0 )); then
  echo " E2E FAILED (${FAILURES} failure(s))"
  echo "==========================================="
  # cleanup runs via trap
  exit 1
else
  echo " E2E checks passed; cleanup is still required."
  echo "==========================================="
  if [[ -n "${SKIP_CLEANUP:-}" ]]; then
    echo "SKIP_CLEANUP set — retaining the recorded test Agent for explicit cleanup; charges may continue."
    trap - EXIT
  fi
fi
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi

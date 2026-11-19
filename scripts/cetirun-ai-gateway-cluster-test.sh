#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# Install-and-upgrade test for the CetiRun AI Gateway chart, against a real
# cluster, with the chart's defaults: the CetiRun image and the bundled
# PostgreSQL and Valkey. Needs kubectl pointed at a cluster that already has
# the image (IMAGE_REPOSITORY:IMAGE_TAG), plus helm, curl and jq.
#
# The test creates NAMESPACE and deletes it when it finishes, unless KEEP=true.
# It refuses a namespace that already exists and leaves it untouched.
#
# This exists because the offline render suite cannot see ordering or runtime
# behaviour. Bugs got past it that all rendered cleanly: a migration hook that
# could never start, a migration that exited zero without creating any of
# LiteLLM's tables, Redis wiring that answered identical requests from cache
# across callers, and an uninstall that deleted the salt key while the database
# kept everything encrypted with it. Each check below is tied to something that
# has actually gone wrong, or to a claim the chart makes that only a running
# cluster can confirm.
#
# Checks have been verified by putting the original problem back and
# confirming the test fails at the check meant to catch it.
#
# Everything runs against a deterministic mock provider, so no provider account
# or credential is involved. The mock runs in the Gateway image, which has
# Python, so the test pulls nothing beyond the chart's own images.

set -euo pipefail

script_dir="$(cd "$(dirname "$0")" && pwd)"
repo_root="$(cd "$script_dir/.." && pwd)"
chart_dir="${CHART_DIR:-${repo_root}/app/cetirun-ai-gateway/chart}"
namespace="${NAMESPACE:-gateway-cluster-test}"
release_name="${RELEASE_NAME:-gateway}"
# The image under test, built and loaded into the cluster beforehand. The
# workflow builds it with scripts/ai-gateway-docker-build.sh.
image_repository="${IMAGE_REPOSITORY:-local.cetirun.invalid/cetirun-ai-gateway}"
image_tag="${IMAGE_TAG:-cluster-test}"
# Optional: an image built before the request log existed, also loaded into the
# cluster. With it set, the test checks that the chart still starts that image.
previous_image_repository="${PREVIOUS_IMAGE_REPOSITORY:-${image_repository}}"
previous_image_tag="${PREVIOUS_IMAGE_TAG:-}"
keep="${KEEP:-false}"

for tool in kubectl helm curl jq; do
  if ! command -v "${tool}" >/dev/null 2>&1; then
    echo "ERROR: Missing required command: ${tool}" >&2
    exit 1
  fi
done

if [ ! -f "${chart_dir}/Chart.yaml" ]; then
  echo "ERROR: Chart.yaml not found in ${chart_dir}" >&2
  exit 1
fi

work_dir="$(mktemp -d)"
forward_pids=()
# Resource names are read from the cluster once the release exists, by
# discover_names below, rather than rebuilt here: the chart shortens long names
# and uses the release name alone when it already contains the chart's, and a
# copy of those rules in the test would drift from the chart's.
fullname=""
postgresql_name=""
postgresql_pod=""
valkey_name=""
# Set only once this run has created the namespace. Cleanup deletes the
# namespace and the release in it, so it needs that ownership first: an
# existing namespace, and everything in it, must never be touched.
namespace_created=false

stop_forwards() {
  for pid in "${forward_pids[@]}"; do
    kill "${pid}" 2>/dev/null || true
  done
  forward_pids=()
}

cleanup() {
  stop_forwards
  if [ "${keep}" != "true" ] && [ "${namespace_created}" = "true" ]; then
    helm uninstall "${release_name}" -n "${namespace}" >/dev/null 2>&1 || true
    kubectl delete namespace "${namespace}" --wait=false >/dev/null 2>&1 || true
  fi
  rm -rf "${work_dir}"
}
trap cleanup EXIT

fail() {
  echo "FAIL: $*" >&2
  echo "--- pods ---" >&2
  kubectl get pods -n "${namespace}" >&2 || true
  exit 1
}

pass() {
  echo "  ok: $*"
}

# ----- Fixtures -------------------------------------------------------------

echo "==> Deploying the mock provider into ${namespace}"
if ! kubectl create namespace "${namespace}" >/dev/null; then
  echo "ERROR: could not create namespace ${namespace}. The test deletes its namespace when it finishes, so it only runs in one it creates; set NAMESPACE to an unused name." >&2
  exit 1
fi
namespace_created=true

kubectl apply -n "${namespace}" -f - >/dev/null <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: gateway-provider-keys
type: Opaque
stringData:
  MOCK_PROVIDER_KEY: sk-mock-not-a-real-key
---
# OpenAI-compatible backend that always reports 1000 prompt and 1000
# completion tokens, so spend per request is exact. A request whose content
# contains "fail429" is answered with a 429, and GET /stats reports how many
# such requests arrived, so the test can count upstream attempts. One whose
# content contains "sleepN" is answered after N seconds. Any request under
# /broken is answered with a 500, and counted, to stand in for a failing
# deployment. Every response has an
# id of its own, as a real provider's does: LiteLLM keys its per-request spend
# log on that id and skips a row whose id it has already stored, so a mock that
# repeated one id kept a single success in the log however many it served.
apiVersion: v1
kind: ConfigMap
metadata:
  name: mock-provider
data:
  mock_provider.py: |
    import json, re, time, uuid
    from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
    requests = {"failed": 0, "total": 0, "broken": 0}
    class H(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"
        def _send(self, body, code=200):
            self.send_response(code)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        def do_POST(self):
            requests["total"] += 1
            n = int(self.headers.get("Content-Length", 0))
            raw = self.rfile.read(n) if n else b""
            if self.path.startswith("/broken"):
                requests["broken"] += 1
                self._send(b'{"error":{"message":"server error (mock)","code":"500"}}', 500)
                return
            if b"fail429" in raw:
                requests["failed"] += 1
                self._send(b'{"error":{"message":"rate limited (mock)","code":"429"}}', 429)
                return
            delay = re.search(rb"sleep(\d+)", raw)
            if delay:
                time.sleep(int(delay.group(1)))
            self._send(json.dumps({
                "id": f"chatcmpl-{uuid.uuid4().hex}", "object": "chat.completion",
                "created": int(time.time()), "model": "mock-model",
                "choices": [{"index": 0, "finish_reason": "stop",
                             "message": {"role": "assistant", "content": "mock-ok"}}],
                "usage": {"prompt_tokens": 1000, "completion_tokens": 1000,
                          "total_tokens": 2000},
            }).encode())
        def do_GET(self):
            if self.path.startswith("/stats"):
                self._send(json.dumps(requests).encode())
                return
            self._send(b'{"status":"ok"}')
        def log_message(self, *a):
            pass
    ThreadingHTTPServer(("0.0.0.0", 8080), H).serve_forever()
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: mock-provider
spec:
  replicas: 1
  selector:
    matchLabels: {app: mock-provider}
  template:
    metadata:
      labels: {app: mock-provider}
    spec:
      containers:
        - name: mock
          image: ${image_repository}:${image_tag}
          imagePullPolicy: IfNotPresent
          command: ["python", "/mock/mock_provider.py"]
          volumeMounts:
            - {name: src, mountPath: /mock}
      volumes:
        - name: src
          configMap: {name: mock-provider}
---
apiVersion: v1
kind: Service
metadata:
  name: mock-provider
spec:
  selector: {app: mock-provider}
  ports:
    - port: 8080
EOF

kubectl wait --for=condition=available deployment/mock-provider -n "${namespace}" --timeout=300s >/dev/null \
  || fail "the mock provider did not start; is ${image_repository}:${image_tag} loaded into the cluster?"
mock_pod="$(kubectl get pods -n "${namespace}" -l app=mock-provider -o jsonpath='{.items[0].metadata.name}')"
pass "mock provider running"

# The chart's defaults, plus the image under test and the models. PostgreSQL
# and Valkey are the bundled ones.
values_file="${work_dir}/values.yaml"
cat > "${values_file}" <<EOF
image:
  repository: ${image_repository}
  tag: ${image_tag}
models:
  # Priced, so it is served and budgets apply to it.
  - model_name: mock-priced
    litellm_params:
      model: openai/mock-model
      api_base: http://mock-provider:8080
      api_key: os.environ/MOCK_PROVIDER_KEY
    model_info:
      input_cost_per_token: 0.00001
      output_cost_per_token: 0.00001
  # Deliberately unpriced, to check the chart refuses it.
  - model_name: mock-unpriced
    litellm_params:
      model: openai/mock-model
      api_base: http://mock-provider:8080
      api_key: os.environ/MOCK_PROVIDER_KEY
  # Two deployments behind one name, the second always failing and nine times
  # as likely to be picked, to check that a request failing on one deployment
  # is sent to the other.
  - model_name: mock-failover
    litellm_params:
      model: openai/mock-model
      api_base: http://mock-provider:8080
      api_key: os.environ/MOCK_PROVIDER_KEY
      weight: 1
    model_info:
      input_cost_per_token: 0.00001
      output_cost_per_token: 0.00001
  - model_name: mock-failover
    litellm_params:
      model: openai/mock-model
      api_base: http://mock-provider:8080/broken
      api_key: os.environ/MOCK_PROVIDER_KEY
      weight: 9
    model_info:
      input_cost_per_token: 0.00001
      output_cost_per_token: 0.00001
  # A LiteLLM auto router over the priced model, its own name priced at twice
  # the model's.
  - model_name: mock-auto
    litellm_params:
      model: auto_router/complexity_router
      input_cost_per_token: 0.00002
      output_cost_per_token: 0.00002
      complexity_router_config:
        tiers: {SIMPLE: mock-priced, MEDIUM: mock-priced, COMPLEX: mock-priced, REASONING: mock-priced}
      complexity_router_default_model: mock-priced
providerCredentials:
  existingSecret: gateway-provider-keys
# The checks below count calls and spend exactly and need the failing
# deployment of mock-failover to be tried: no background probes, which would
# add calls and keep requests away from it, and no key kept on one deployment.
router:
  deploymentAffinity: false
  healthChecks:
    enabled: false
EOF

# ----- Helpers against the running Gateway ----------------------------------

secret_value() {  # secret_value KEY [SECRET]
  kubectl get secret "${2:-${fullname}-auth}" -n "${namespace}" -o "jsonpath={.data.$1}" | base64 -d
}

# Counts LiteLLM's own tables, not every table. `prisma migrate deploy` creates
# its bookkeeping table even when it finds no migrations to apply, so counting
# all tables passes on exactly the empty database this check exists to catch.
# That was found by putting the original bug back and watching the check pass.
litellm_table_count() {
  kubectl exec -n "${namespace}" "${postgresql_pod}" -- \
    psql -U litellm -d litellm -tAc \
    "SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name LIKE 'LiteLLM\_%'"
}

# Runs Python in the mock pod, which uses the Gateway image and so has the
# PostgreSQL and Redis clients: a second pod on the cluster network, the way a
# Gateway replica reaches the bundled services.
in_mock_pod() {
  kubectl exec -n "${namespace}" "${mock_pod}" -- python -c "$1" "${@:2}"
}

gateway_pods() {
  kubectl get pods -n "${namespace}" \
    -l "app.kubernetes.io/instance=${release_name},app.kubernetes.io/component=gateway" \
    -o json | jq -r '.items[]
      | select(.metadata.deletionTimestamp == null)
      | select(any(.status.conditions[]?; .type == "Ready" and .status == "True"))
      | .metadata.name'
}

# Forward each Gateway pod on its own local port. A port-forward to the Service
# lands on a single pod, which would make every multi-replica check below
# indistinguishable from a single replica.
#
# Pods are chosen by readiness rather than phase: after an upgrade the old pods
# are still in phase Running while they terminate.
forward_pods() {
  stop_forwards
  local pods port=18101
  mapfile -t pods < <(gateway_pods)
  if [ "${#pods[@]}" -lt 2 ]; then
    fail "expected two ready Gateway pods, found ${#pods[@]}"
  fi
  pod_ports=()
  pod_names=("${pods[@]:0:2}")
  for pod in "${pod_names[@]}"; do
    kubectl port-forward -n "${namespace}" "pod/${pod}" "${port}:4000" >"${work_dir}/port-forward-${port}.log" 2>&1 &
    forward_pids+=("$!")
    pod_ports+=("${port}")
    port=$((port + 1))
  done
  for p in "${pod_ports[@]}"; do
    local ready=false
    for _ in $(seq 1 30); do
      if curl -fs -m 5 "http://127.0.0.1:${p}/health/readiness" >/dev/null 2>&1; then
        ready=true
        break
      fi
      sleep 2
    done
    [ "${ready}" = "true" ] || fail "pod on local port ${p} never became reachable"
  done
}

# POST a chat completion; prints the HTTP status. The content is the same every
# time on purpose: identical requests are what a response cache would answer,
# so a test built from varied requests cannot notice one being switched on.
chat() {
  local port="$1" key="$2" model="$3" content="${4:-gateway-body-canary}"
  curl -sS -m 60 -o "${work_dir}/last.json" -D "${work_dir}/last.headers" -w '%{http_code}' \
    -X POST "http://127.0.0.1:${port}/v1/chat/completions" \
    -H "Authorization: Bearer ${key}" -H 'Content-Type: application/json' \
    -d "{\"model\":\"${model}\",\"messages\":[{\"role\":\"user\",\"content\":\"${content}\"}]}"
}

served_from_cache() {
  tr -d '\r' < "${work_dir}/last.headers" | grep -qi '^x-litellm-cache-key:'
}

# The start of the last response, for failure messages.
last_body() {
  head -c 300 "${work_dir}/last.json" 2>/dev/null | tr -d '\n'
}

api_post() {
  local port="$1" path="$2" body="$3"
  curl -fs -m 30 -X POST "http://127.0.0.1:${port}${path}" \
    -H "Authorization: Bearer ${master_key}" -H 'Content-Type: application/json' -d "${body}"
}

# Successful calls in LiteLLM's per-request spend log.
spend_log_successes() {
  kubectl exec -n "${namespace}" "${postgresql_pod}" -- psql -U litellm -d litellm -tAc \
    "SELECT count(*) FROM \"LiteLLM_SpendLogs\" WHERE status = 'success'"
}

# The image's request log lines a Gateway pod has written to stdout.
request_log() {  # request_log POD
  kubectl logs -n "${namespace}" "$1" -c gateway | grep '^{"log_type":"cetirun.ai_gateway.request"' || true
}

# The Gateway Deployment carries the chart's fullname, which also prefixes the
# auth Secret; the bundled services are found by their component label.
discover_names() {
  local selector="app.kubernetes.io/instance=${release_name}"
  fullname="$(kubectl get deployments -n "${namespace}" -l "${selector}" -o json | jq -r '.items[]
    | select(.spec.selector.matchLabels["app.kubernetes.io/component"] == "gateway") | .metadata.name')"
  postgresql_name="$(kubectl get statefulsets -n "${namespace}" -l "${selector},app.kubernetes.io/component=postgresql" \
    -o jsonpath='{.items[0].metadata.name}')"
  valkey_name="$(kubectl get services -n "${namespace}" -l "${selector},app.kubernetes.io/component=valkey" \
    -o jsonpath='{.items[0].metadata.name}')"
  [ -n "${fullname}" ] && [ -n "${postgresql_name}" ] && [ -n "${valkey_name}" ] \
    || fail "could not find the release's Gateway, PostgreSQL and Valkey by their labels"
  postgresql_pod="${postgresql_name}-0"
  echo "  names: ${fullname} (${#fullname}), ${postgresql_name} (${#postgresql_name}), ${valkey_name} (${#valkey_name})"
}

# ----- Install ---------------------------------------------------------------

# One command, no prior setup beyond the provider key: the chart brings its own
# PostgreSQL and Valkey. The Gateway pods migrate in an init container, waiting
# for the database, which starts in the same release.
echo "==> Installing ${release_name}"
helm install "${release_name}" "${chart_dir}" -n "${namespace}" -f "${values_file}" \
  --wait --timeout 10m >/dev/null || fail "install did not complete"
pass "install completed with the bundled PostgreSQL and Valkey"
discover_names

# The migration once exited zero against a schema copy with no migrations beside
# it, leaving no LiteLLM tables behind a successful install. The proxies still
# started, passed readiness, and served models from the config file. Only the
# tables tell them apart.
tables="$(litellm_table_count)"
[ "${tables}" -gt 0 ] || fail "install reported success but none of LiteLLM's tables exist"
pass "migrations applied (${tables} LiteLLM tables)"

# Two replicas start together and both run the migrate init container. If they
# migrated at the same time, one could be killed by PostgreSQL's deadlock
# detection partway through and leave a failed migration behind, which blocks
# every later deploy. They take turns.
failed_migrations="$(kubectl exec -n "${namespace}" "${postgresql_pod}" -- psql -U litellm -d litellm -tAc \
  'SELECT count(*) FROM _prisma_migrations WHERE finished_at IS NULL OR rolled_back_at IS NOT NULL')"
[ "${failed_migrations}" = "0" ] || fail "${failed_migrations} migrations failed or were rolled back"
pass "replicas migrated one after the other, with no failed migration"

gateway_pod="$(gateway_pods | head -n 1 || true)"
[ "$(kubectl exec -n "${namespace}" "${gateway_pod}" -c gateway -- id -u)" = "65532" ] \
  || fail "the Gateway does not run as the image's nonroot user"
pass "the Gateway runs as a non-root user"

master_key="$(secret_value master-key)"
salt_before="$(secret_value salt-key)"
[ -n "${salt_before}" ] || fail "no salt key was generated"
db_password_before="$(secret_value password "${postgresql_name}")"

forward_pods
port_a="${pod_ports[0]}"
port_b="${pod_ports[1]}"
pass "both replicas reachable individually"

# The bundled services are reachable across the cluster network. Local
# connections inside the PostgreSQL container are trusted, so its password is
# only meaningful for the connections a Gateway pod makes, and that is what is
# checked, from another pod.
echo "==> The bundled PostgreSQL and Valkey require their passwords"
pg_wrong="$(in_mock_pod '
import sys, psycopg
try:
    psycopg.connect(f"postgresql://litellm:wrong-password@{sys.argv[1]}:5432/litellm", connect_timeout=10).close()
    print("accepted")
except psycopg.OperationalError as error:
    print("refused" if "password authentication failed" in str(error) else f"error: {error}")
' "${postgresql_name}")"
[ "${pg_wrong}" = "refused" ] || fail "the bundled PostgreSQL did not refuse a wrong password from another pod (${pg_wrong})"
valkey_open="$(in_mock_pod '
import sys, redis
try:
    redis.Redis(host=sys.argv[1], port=6379, socket_timeout=10).ping()
    print("accepted")
except redis.exceptions.AuthenticationError:
    print("refused")
except redis.exceptions.ResponseError as error:
    print("refused" if "NOAUTH" in str(error) else f"error: {error}")
' "${valkey_name}")"
[ "${valkey_open}" = "refused" ] || fail "the bundled Valkey answered without a password (${valkey_open})"
pass "a wrong PostgreSQL password and a password-less Valkey client are refused"

# ----- Behaviour -------------------------------------------------------------

echo "==> End-to-end request"
[ "$(chat "${port_a}" "${master_key}" mock-priced)" = "200" ] || fail "request to a priced model failed: $(last_body)"
grep -q 'mock-ok' "${work_dir}/last.json" || fail "response did not come from the backend"
pass "request reached the backend and returned"

# Redis is configured through LiteLLM's cache so that budgets are shared across
# replicas. Left at its default, that cache also answers identical requests from
# Redis: recorded at zero spend, not counted against any budget, and shared
# across every caller who sends the same content. An agent retrying after a bad
# answer would get the same answer back. The chart sets it to default_off.
echo "==> Responses are not cached"
chat "${port_a}" "${master_key}" mock-priced >/dev/null
if served_from_cache; then
  fail "an identical request was answered from the response cache"
fi
pass "identical repeat reached the backend rather than the cache"

# A model missing from the price table costs zero, and budget checks are skipped
# for zero-cost models, so a budget would silently not apply. The chart refuses
# such requests by default.
echo "==> Unpriced models are refused"
[ "$(chat "${port_a}" "${master_key}" mock-unpriced)" = "403" ] || fail "an unpriced model was served"
pass "unpriced model refused with 403"

# LiteLLM retries a failed upstream call twice by default, and agents retry on
# their own, so the two multiply: one failing request was measured at three
# upstream calls. The chart turns LiteLLM's retries off; this checks that the
# running version honours it, which rendering alone cannot show.
echo "==> Upstream failures are not retried"
status="$(chat "${port_a}" "${master_key}" mock-priced fail429)"
[ "${status}" = "429" ] || fail "an upstream 429 reached the client as ${status}"
attempts="$(in_mock_pod "import urllib.request; print(urllib.request.urlopen('http://127.0.0.1:8080/stats').read().decode())" \
  | jq -r .failed)"
[ "${attempts}" = "1" ] || fail "one failing request produced ${attempts} upstream attempts; the Gateway is retrying"
pass "an upstream 429 went straight back to the client, one attempt"

# A caller could ask LiteLLM for retries of its own, with num_retries in the
# body or the x-litellm-num-retries header: one failing request then made four
# upstream calls. The image's callback drops those fields.
echo "==> A caller cannot change the retries"
retry_request() {  # retry_request BODY [CURL_ARGS...]
  curl -s -m 60 -o /dev/null -X POST "http://127.0.0.1:${port_a}/v1/chat/completions" "${@:2}" \
    -H "Authorization: Bearer ${master_key}" -H 'Content-Type: application/json' -d "$1" || true
}
retry_request '{"model":"mock-priced","messages":[{"role":"user","content":"fail429"}]}' -H 'x-litellm-num-retries: 3'
retry_request '{"model":"mock-priced","num_retries":3,"messages":[{"role":"user","content":"fail429"}]}'
attempts="$(in_mock_pod "import urllib.request; print(urllib.request.urlopen('http://127.0.0.1:8080/stats').read().decode())" \
  | jq -r .failed)"
[ "${attempts}" = "3" ] || fail "two failing requests asking for retries made $((attempts - 1)) upstream attempts; a caller changed the retries"
pass "a caller's num_retries, in the body or a header, added no upstream attempt"

# Two deployments behind one name, one always failing: a request that fails on
# one deployment is sent once more, to the other. Without that, about half the
# requests failed.
echo "==> A request failing on one deployment is sent to another"
for i in $(seq 1 10); do
  if [ $((i % 2)) -eq 1 ]; then port="${port_a}"; else port="${port_b}"; fi
  status="$(chat "${port}" "${master_key}" mock-failover)"
  [ "${status}" = "200" ] || fail "request ${i} to a model with a failing deployment got ${status}: $(last_body)"
done
broken="$(in_mock_pod "import urllib.request; print(urllib.request.urlopen('http://127.0.0.1:8080/stats').read().decode())" \
  | jq -r .broken)"
[ "${broken}" -ge 1 ] || fail "no request reached the failing deployment, so the failover was not exercised"
pass "10 of 10 requests answered, ${broken} of them after their first deployment failed"

# Without shared state each replica enforces its own copy of a budget, so two
# replicas allow twice the configured spend. Alternating between the two pods
# directly is what makes this a test of coordination rather than of one pod.
# Here the shared state is the bundled Valkey, with its password.
echo "==> A budget holds across replicas"
virtual_key="$(api_post "${port_a}" /key/generate '{"models":["mock-priced"],"max_budget":0.10}' | jq -r .key)" \
  || fail "could not create a virtual key through the management API"
[ -n "${virtual_key}" ] && [ "${virtual_key}" != "null" ] || fail "could not create a virtual key"
allowed=0
for i in $(seq 1 16); do
  if [ $((i % 2)) -eq 1 ]; then port="${port_a}"; else port="${port_b}"; fi
  if [ "$(chat "${port}" "${virtual_key}" mock-priced)" = "200" ]; then
    allowed=$((allowed + 1))
  fi
  sleep 0.3
done
# $0.10 at $0.02 a request is five. Allow one request of slack for a request in
# flight while spend was being reserved. Missing coordination shows as about
# ten; response caching shows as all sixteen, because cache hits cost nothing.
[ "${allowed}" -ge 5 ] && [ "${allowed}" -le 6 ] \
  || fail "a \$0.10 budget allowed ${allowed} requests across two replicas; expected 5"
pass "\$0.10 budget allowed ${allowed} requests across both replicas"

# Every answered call is recorded twice: in LiteLLM's spend log, which the
# console's request view reads, and in the image's request log on stdout, for
# the cluster's log collection. The answered calls so far are the two with the
# master key, the ten to the model with a failing deployment, each recorded
# once, and those the budget allowed, spread over both replicas. LiteLLM
# writes the spend log in batches, a few seconds behind.
echo "==> Every answered call is recorded, in the spend log and the request log"
answered=$((12 + allowed))
recorded=""
for _ in $(seq 1 30); do
  recorded="$(spend_log_successes)"
  [ "${recorded}" = "${answered}" ] && break
  sleep 2
done
[ "${recorded}" = "${answered}" ] \
  || fail "LiteLLM_SpendLogs holds ${recorded} successful calls; ${answered} were answered"
# This namespace and database contain only this run's synthetic requests.
# Return a count, not stored request or response bodies.
with_content="$(kubectl exec -n "${namespace}" "${postgresql_pod}" -- psql -U litellm -d litellm -tAc \
  "SELECT count(*) FROM \"LiteLLM_SpendLogs\" WHERE status = 'success'
   AND coalesce(\"proxy_server_request\"::text, '') LIKE '%gateway-body-canary%'
   AND coalesce(\"response\"::text, '') LIKE '%mock-ok%'")"
[ "${with_content}" = "${answered}" ] \
  || fail "${with_content} of ${answered} successful spend-log rows contain the synthetic prompt and response"
logged=0
all_lines=""
for pod in "${pod_names[@]}"; do
  lines="$(request_log "${pod}")"
  all_lines+="${lines}"$'\n'
  logged=$((logged + $(grep -c '"status":"success"' <<< "${lines}" || true)))
done
[ "${logged}" = "${answered}" ] || fail "the request log holds ${logged} successful calls; ${answered} were answered"
grep -q "\"key_hash\":\"[0-9a-f]\{64\}\".*\"user_agent\":\"curl" <<< "${all_lines}" \
  || fail "the request log lines do not carry the caller"
# Stdout must not contain the synthetic prompt or answer stored in the database.
if grep -Eq 'gateway-body-canary|mock-ok' <<< "${all_lines}"; then
  fail "the stdout request log carries prompt or response content"
fi
pass "${answered} spend-log rows contain prompts and responses; stdout request logs remain metadata-only"

# A model stored in the database is encrypted with the salt key. It is what an
# upgrade has to preserve. LiteLLM 1.105 keeps a price given through the
# management API only in litellm_params; it drops one in model_info.
echo "==> A database-backed model"
api_post "${port_a}" /model/new '{"model_name":"mock-db","litellm_params":{"model":"openai/mock-model","api_base":"http://mock-provider:8080","api_key":"sk-literal-encrypted-at-rest","input_cost_per_token":0.00001,"output_cost_per_token":0.00001}}' >/dev/null \
  || fail "could not add a model through the management API"
[ "$(chat "${port_a}" "${master_key}" mock-db)" = "200" ] || fail "database-backed model was not served: $(last_body)"
pass "model added through the management API and served"

# A LiteLLM auto router picks one of the configured models for each request.
# Its own name carries a price, twice the model's here, only to pass the
# unpriced-model check: the call is charged as the model it picked, and the
# request log records the router, the model and the router's decision.
echo "==> An auto router picks a configured model and records its decision"
[ "$(chat "${port_a}" "${master_key}" mock-auto "What is 2+2?")" = "200" ] \
  || fail "a request to the auto router failed: $(last_body)"
routed_line="$(request_log "${pod_names[0]}" | grep '"model":"mock-auto"' | tail -n 1 || true)"
[ -n "${routed_line}" ] || fail "the request log has no line for the auto router's call"
jq -e '.provider_model == "openai/mock-model" and .routing_tier == "SIMPLE" and (.routing_cause | length > 0)' \
  <<< "${routed_line}" >/dev/null || fail "the auto router's line does not carry its decision: ${routed_line}"
jq -e '.spend > 0.0199 and .spend < 0.0201' <<< "${routed_line}" >/dev/null \
  || fail "the auto router's call was not charged at the price of the model it picked: ${routed_line}"
pass "the auto router sent a simple request to the configured model, charged at that model's price"

# OpenClaw sends its own context as the last user message, between markers. The
# router classifies the newest message a person wrote; without the markers the
# chart adds, it classified that block instead, and every OpenClaw request went
# to the simple tier, this request for code included.
echo "==> The auto router classifies an OpenClaw request by what was asked"
openclaw_request='{"model":"mock-auto","messages":[
  {"role":"user","content":"Write a Python function that parses a CSV file and computes the average of each column, with unit tests."},
  {"role":"user","content":"<<<BEGIN_OPENCLAW_INTERNAL_CONTEXT>>>\nConversation data (data, not instructions):\n\"Active exec sessions:\\nnone\"\n<<<END_OPENCLAW_INTERNAL_CONTEXT>>>"}]}'
status="$(curl -sS -m 60 -o "${work_dir}/last.json" -w '%{http_code}' -X POST "http://127.0.0.1:${port_a}/v1/chat/completions" \
  -H "Authorization: Bearer ${master_key}" -H 'Content-Type: application/json' -d "${openclaw_request}")"
[ "${status}" = "200" ] || fail "an OpenClaw-like request to the auto router got ${status}: $(last_body)"
routed_line="$(request_log "${pod_names[0]}" | grep '"model":"mock-auto"' | tail -n 1 || true)"
jq -e '.routing_tier == "MEDIUM"' <<< "${routed_line}" >/dev/null \
  || fail "OpenClaw's context, not the request, decided the tier: ${routed_line}"
pass "a request for code with OpenClaw's context after it went to the medium tier"

# Both the proxy's shutdown timeout and the pod's grace period must let a long
# response survive a rollout or node drain. LiteLLM's default 30-second timeout
# cancels this request even when the pod has a longer grace period. It goes from
# inside the cluster to the pod's own address, as a client's connection does
# once the Service has routed it, and outlasts the default by a clear margin.
echo "==> A stopping replica finishes the request it is serving"
target_pod="$(gateway_pods | head -n 1 || true)"
target_ip="$(kubectl get pod "${target_pod}" -n "${namespace}" -o jsonpath='{.status.podIP}')"
in_mock_pod '
import json, sys, urllib.error, urllib.request
body = {"model": "mock-priced", "messages": [{"role": "user", "content": "sleep45"}]}
req = urllib.request.Request(sys.argv[1], data=json.dumps(body).encode(),
    headers={"Authorization": "Bearer " + sys.argv[2], "Content-Type": "application/json"})
try:
    print(urllib.request.urlopen(req, timeout=120).status)
except urllib.error.HTTPError as e:
    print(e.code)
except Exception as e:
    print(type(e).__name__)
' "http://${target_ip}:4000/v1/chat/completions" "${master_key}" > "${work_dir}/slow.out" 2>&1 &
slow_pid=$!
sleep 5
kubectl delete pod "${target_pod}" -n "${namespace}" --wait=false >/dev/null
wait "${slow_pid}" || true
slow_status="$(tail -n 1 "${work_dir}/slow.out")"
[ "${slow_status}" = "200" ] \
  || fail "a request in flight was cut off when its replica stopped (${slow_status}); the grace period is too short"
# A long grace period is free only if a replica exits once its work is done.
# An idle replica was measured exiting four seconds after SIGTERM.
gone=false
for _ in $(seq 1 30); do
  if ! kubectl get pod "${target_pod}" -n "${namespace}" >/dev/null 2>&1; then
    gone=true
    break
  fi
  sleep 2
done
[ "${gone}" = "true" ] || fail "the replica was still running a minute after its last request finished"
pass "a 45-second request completed while its replica shut down, and the replica then exited"

# ----- Upgrade ---------------------------------------------------------------

# LiteLLM has no rotation path for the salt key. When it changes the proxy
# starts, reports healthy, and silently stops serving every database-backed
# model. Offline tests cannot check preservation, because lookup returns
# nothing under helm template.
#
# This upgrade explicitly opts out of database logging while enabling metrics.
# The normal Chart and platform defaults keep the spend log and its content.
echo "==> Upgrading ${release_name}"
spend_logs_before_upgrade="$(spend_log_successes)"
helm upgrade "${release_name}" "${chart_dir}" -n "${namespace}" -f "${values_file}" \
  --set-string podAnnotations.cluster-test/revision=2 --set metrics.enabled=true --set spendLogs.enabled=false \
  --set spendLogs.storePrompts=false \
  --wait --timeout 10m >/dev/null \
  || fail "upgrade did not complete"
pass "upgrade completed"

[ "$(secret_value salt-key)" = "${salt_before}" ] || fail "the salt key changed across upgrade"
[ "$(secret_value master-key)" = "${master_key}" ] || fail "the master key changed across upgrade"
[ "$(secret_value password "${postgresql_name}")" = "${db_password_before}" ] \
  || fail "the bundled PostgreSQL's password changed across upgrade"
pass "salt key, master key and database password unchanged"

forward_pods
port_a="${pod_ports[0]}"
[ "$(chat "${port_a}" "${master_key}" mock-db)" = "200" ] \
  || fail "database-backed model stopped working after upgrade; stored credentials may be unreadable: $(last_body)"
pass "database-backed model still decrypts and serves after upgrade"

# With LiteLLM's spend log off, a call adds no row to LiteLLM_SpendLogs. It is
# still charged to the key, and the request log still records it, one call per
# replica.
echo "==> With the spend log off, calls are charged and recorded in the request log only"
spend_key="$(api_post "${port_a}" /key/generate '{"models":["mock-priced"],"max_budget":10}' | jq -r .key)" \
  || fail "could not create a virtual key"
for port in "${pod_ports[@]}"; do
  [ "$(chat "${port}" "${spend_key}" mock-priced)" = "200" ] || fail "a call with the spend log off failed: $(last_body)"
done
spend_key_spend=""
for _ in $(seq 1 30); do
  spend_key_spend="$(curl -fs -m 30 "http://127.0.0.1:${port_a}/key/info?key=${spend_key}" \
    -H "Authorization: Bearer ${master_key}" | jq -r '.info.spend')"
  [ "${spend_key_spend}" = "0.04" ] && break
  sleep 2
done
[ "${spend_key_spend}" = "0.04" ] || fail "two \$0.02 calls charged the key ${spend_key_spend}"
sleep 15
spend_logs_after="$(spend_log_successes)"
# The database-backed model's call right after the upgrade can be the only one
# the old replicas had not written yet.
[ "${spend_logs_after}" -le $((spend_logs_before_upgrade + 1)) ] \
  || fail "LiteLLM_SpendLogs grew from ${spend_logs_before_upgrade} to ${spend_logs_after} with the spend log off"
key_hash_of() {
  in_mock_pod 'import hashlib, sys; print(hashlib.sha256(sys.argv[1].encode()).hexdigest())' "$1"
}
logged_for() {  # logged_for KEY: request log lines for that key across both pods
  local hash count=0
  hash="$(key_hash_of "$1")"
  for pod in "${pod_names[@]}"; do
    count=$((count + $(request_log "${pod}" | grep -c "\"key_hash\":\"${hash}\"" || true)))
  done
  echo "${count}"
}
spend_logged="$(logged_for "${spend_key}")"
[ "${spend_logged}" = "2" ] || fail "the request log holds ${spend_logged} of the 2 calls made with the spend log off"
pass "2 calls charged \$0.04 and in the request log, none in LiteLLM_SpendLogs"

# LiteLLM lets a caller skip the logging callbacks for its own call by sending
# "no-log": true. The chart has LiteLLM ignore that field, so a caller cannot
# keep a call out of the request log.
echo "==> A caller cannot keep a call out of the request log"
nolog_key="$(api_post "${port_a}" /key/generate '{"models":["mock-priced"],"max_budget":10}' | jq -r .key)" \
  || fail "could not create a virtual key"
nolog_status="$(curl -s -m 60 -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:${port_a}/v1/chat/completions" \
  -H "Authorization: Bearer ${nolog_key}" -H 'Content-Type: application/json' \
  -d '{"model":"mock-priced","messages":[{"role":"user","content":"test"}],"no-log":true}')"
[ "${nolog_status}" = "200" ] || fail "a call sent with no-log failed (${nolog_status})"
sleep 2
[ "$(logged_for "${nolog_key}")" = "1" ] || fail "a call sent with no-log is missing from the request log"
pass "a call sent with no-log is in the request log"

# Clients choose some of what the record carries: LiteLLM records a request's
# metadata.session_id as its trace_id. A line longer than 16 KiB is split by the
# container runtime into pieces that are not valid JSON, so every string is
# capped and a line never exceeds 15 KiB. An uncapped trace_id once made a
# 100 KB session ID a 100 KB line.
echo "==> A request log line stays within 15 KiB whatever the caller sends"
long_key="$(api_post "${port_a}" /key/generate '{"models":["mock-priced"],"max_budget":10}' | jq -r .key)" \
  || fail "could not create a virtual key"
head -c 100000 /dev/zero | tr '\0' 's' > "${work_dir}/session-id"
jq -n --rawfile session "${work_dir}/session-id" \
  '{model: "mock-priced", messages: [{role: "user", content: "test"}], metadata: {session_id: $session}}' \
  > "${work_dir}/long-session.json"
long_status="$(curl -s -m 60 -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:${port_a}/v1/chat/completions" \
  -H "Authorization: Bearer ${long_key}" -H 'Content-Type: application/json' --data-binary "@${work_dir}/long-session.json")"
[ "${long_status}" = "200" ] || fail "a call with a 100 KB session ID failed (${long_status})"
sleep 2
long_hash="$(key_hash_of "${long_key}")"
long_lines="$(for pod in "${pod_names[@]}"; do request_log "${pod}" | grep "\"key_hash\":\"${long_hash}\"" || true; done)"
[ "$(grep -c . <<< "${long_lines}")" = "1" ] \
  || fail "expected one request log line for the call with a 100 KB session ID, found $(grep -c . <<< "${long_lines}")"
long_bytes="$(head -n 1 <<< "${long_lines}" | tr -d '\n' | wc -c)"
[ "${long_bytes}" -le 15360 ] || fail "the request log line for a 100 KB session ID is ${long_bytes} bytes; the limit is 15 KiB"
jq -e '(.trace_id | length) == 256' <<< "${long_lines}" >/dev/null \
  || fail "the session ID did not reach the request log line as a capped trace_id"
pass "a call with a 100 KB session ID wrote one line of ${long_bytes} bytes, its trace_id capped at 256 characters"

# LiteLLM serves the metrics from a separate process, on a port of their own,
# from samples the proxy leaves in a directory they share. Only a running pod
# shows the two meet. On the Service port, /metrics stays behind the master key.
echo "==> Metrics are served on their own port"
metrics_ip="$(kubectl get pod "${pod_names[0]}" -n "${namespace}" -o jsonpath='{.status.podIP}')"
metrics_text="$(in_mock_pod '
import sys, urllib.request
print(urllib.request.urlopen(sys.argv[1], timeout=30).read().decode())
' "http://${metrics_ip}:9090/metrics")" || fail "the metrics port did not answer"
grep -q '^litellm_proxy_total_requests_metric_total{.*requested_model="mock-db"' <<< "${metrics_text}" \
  || fail "the metrics port does not report the request the pod just served"
if grep -q 'client_ip=' <<< "${metrics_text}"; then
  fail "the metrics carry the client address, which the chart leaves out"
fi
status="$(curl -s -m 30 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${port_a}/metrics")"
[ "${status}" = "401" ] || fail "/metrics on the Service port answered ${status} without a key"
pass "the metrics port reports the pod's requests; the Service port still requires a key"

# LiteLLM replaces a configured callbacks list with one saved in its database
# when it reads its configuration with the database connected. LiteLLM 1.101.0
# reads the file before it connects, so a configured callback survived a saved
# list when tested, but that rests on LiteLLM's startup order. The image's
# entrypoint registers the request log outside the list, so a saved list leaves
# it, and the spend log setting that goes with it, in place in any case.
echo "==> A callbacks list saved in LiteLLM's database leaves the request log in place"
kubectl exec -i -n "${namespace}" "${postgresql_pod}" -- psql -U litellm -d litellm -v ON_ERROR_STOP=1 -q \
  >/dev/null <<'SQL' || fail "could not save a callbacks list in LiteLLM_Config"
INSERT INTO "LiteLLM_Config" (param_name, param_value)
VALUES ('litellm_settings', '{"callbacks": ["prometheus"]}')
ON CONFLICT (param_name) DO UPDATE SET param_value = EXCLUDED.param_value;
SQL
stop_forwards
kubectl rollout restart "deployment/${fullname}" -n "${namespace}" >/dev/null
kubectl rollout status "deployment/${fullname}" -n "${namespace}" --timeout=600s >/dev/null \
  || fail "the Gateway did not restart with a callbacks list saved in its database"
forward_pods
port_a="${pod_ports[0]}"
saved_key="$(api_post "${port_a}" /key/generate '{"models":["mock-priced"],"max_budget":10}' | jq -r .key)" \
  || fail "could not create a virtual key"
saved_spend_logs_before="$(spend_log_successes)"
for port in "${pod_ports[@]}"; do
  [ "$(chat "${port}" "${saved_key}" mock-priced)" = "200" ] \
    || fail "a call with a callbacks list saved in the database failed: $(last_body)"
done
sleep 15
saved_logged="$(logged_for "${saved_key}")"
[ "${saved_logged}" = "2" ] \
  || fail "with a callbacks list saved in LiteLLM's database, the request log holds ${saved_logged} of the 2 calls"
saved_spend_logs_after="$(spend_log_successes)"
[ "${saved_spend_logs_after}" = "${saved_spend_logs_before}" ] \
  || fail "with a callbacks list saved in LiteLLM's database, LiteLLM_SpendLogs grew from ${saved_spend_logs_before} to ${saved_spend_logs_after}"
kubectl exec -n "${namespace}" "${postgresql_pod}" -- psql -U litellm -d litellm -tAc \
  "DELETE FROM \"LiteLLM_Config\" WHERE param_name = 'litellm_settings'" >/dev/null \
  || fail "could not remove the saved callbacks list"
pass "with a callbacks list saved in the database, 2 calls are in the request log and none in LiteLLM_SpendLogs"

# ----- Managed key authorization --------------------------------------------

provider_requests() {
  in_mock_pod 'import json, urllib.request
print(json.load(urllib.request.urlopen("http://mock-provider:8080/stats", timeout=10))["total"])'
}

key_requests() {  # key_requests KEY MODEL EXPECTED_STATUS
  local key="$1" model="$2" expected="$3" port status
  for port in "${pod_ports[@]}"; do
    if ! status="$(chat "${port}" "${key}" "${model}")"; then
      tail -n 20 "${work_dir}/port-forward-${port}.log" >&2
      fail "key request transport failed on ${port}"
    fi
    if [ "${expected}" = denied ]; then
      case "${status}" in
        401|403) ;;
        *) fail "new request on ${port} used outdated authorization: expected 401/403, got ${status}" ;;
      esac
    else
      [ "${status}" = "${expected}" ] || fail "key request on ${port}: expected ${expected}, got ${status}"
      jq -e '.choices[0].message.content == "mock-ok"' "${work_dir}/last.json" >/dev/null \
        || fail "the allowed request did not return the provider response"
    fi
  done
}

managed_key_freshness() {
  local phase="$1" control_key target_key target_hash before alias budget_key allowed i port status
  echo "==> Current key authorization on both replicas (${phase})"
  control_key="$(api_post "${port_a}" /key/generate '{"models":["mock-priced"],"max_budget":10}' | jq -er .key)" \
    || fail "could not create the independent key control"
  target_key="$(api_post "${port_a}" /key/generate '{"models":["mock-priced"],"max_budget":10}' | jq -er .key)" \
    || fail "could not create the deletion target"
  target_hash="$(key_hash_of "${target_key}")"
  key_requests "${control_key}" mock-priced 200
  key_requests "${target_key}" mock-priced 200
  before="$(provider_requests)"
  api_post "${port_a}" /key/delete "$(jq -nc --arg key "${target_hash}" '{keys:[$key]}')" >/dev/null \
    || fail "key deletion by hash failed"
  # No polling, cache flush, restart or sleep after management success: check
  # the very next new request on each separately addressed, prewarmed replica.
  key_requests "${target_key}" mock-priced denied
  [ "$(provider_requests)" = "${before}" ] || fail "a deleted key reached the provider"
  key_requests "${control_key}" mock-priced 200
  pass "deletion by hash denies new requests on both replicas; the control still serves"

  alias="cluster-test-current-${phase}"
  target_key="$(api_post "${port_a}" /key/generate \
    "$(jq -nc --arg alias "${alias}" '{key_alias:$alias,models:["mock-priced","mock-db"],max_budget:10}')" | jq -er .key)" \
    || fail "could not create the key-change target"
  key_requests "${target_key}" mock-priced 200
  key_requests "${target_key}" mock-db 200
  before="$(provider_requests)"
  api_post "${port_a}" /key/update \
    "$(jq -nc --arg key "${target_key}" '{key:$key,models:["mock-db"]}')" >/dev/null \
    || fail "model entitlement update failed"
  key_requests "${target_key}" mock-priced denied
  [ "$(provider_requests)" = "${before}" ] || fail "a removed model entitlement reached the provider"
  key_requests "${target_key}" mock-db 200
  key_requests "${control_key}" mock-priced 200
  pass "removed model entitlement is denied on both replicas; retained access still serves"

  before="$(provider_requests)"
  api_post "${port_a}" /key/block "$(jq -nc --arg key "${target_key}" '{key:$key}')" >/dev/null \
    || fail "key block failed"
  key_requests "${target_key}" mock-db denied
  [ "$(provider_requests)" = "${before}" ] || fail "a blocked key reached the provider"
  key_requests "${control_key}" mock-priced 200
  api_post "${port_a}" /key/unblock "$(jq -nc --arg key "${target_key}" '{key:$key}')" >/dev/null \
    || fail "key unblock failed"
  key_requests "${target_key}" mock-db 200
  key_requests "${target_key}" mock-priced denied
  pass "block and unblock are current on both replicas without restoring removed entitlements"

  before="$(provider_requests)"
  api_post "${port_a}" /key/delete "$(jq -nc --arg alias "${alias}" '{key_aliases:[$alias]}')" >/dev/null \
    || fail "key deletion by alias failed"
  key_requests "${target_key}" mock-db denied
  [ "$(provider_requests)" = "${before}" ] || fail "a key deleted by alias reached the provider"
  key_requests "${control_key}" mock-priced 200
  pass "deletion by alias denies new requests on both replicas; the control still serves"

  budget_key="$(api_post "${port_a}" /key/generate '{"models":["mock-priced"],"max_budget":0.10}' | jq -er .key)" \
    || fail "could not create the managed budget control"
  allowed=0
  for i in $(seq 0 15); do
    port="${pod_ports[$((i % 2))]}"
    status="$(chat "${port}" "${budget_key}" mock-priced)" || fail "managed budget request transport failed"
    if [ "${status}" = 200 ]; then
      allowed=$((allowed + 1))
      served_from_cache && fail "managed budget request was answered from the response cache"
    else
      # The chart refuses an exhausted budget with 402 (budgetExceededStatusCode;
      # LiteLLM's own default is 422), which agents report as exhausted credit
      # rather than retrying it as a rate limit.
      [ "${status}" = 402 ] || fail "managed budget returned ${status}, not a budget rejection"
      jq -e '.error.message | test("budget"; "i")' "${work_dir}/last.json" >/dev/null \
        || fail "managed budget request was rejected for a different reason"
    fi
    sleep 0.3
  done
  [ "${allowed}" -ge 5 ] && [ "${allowed}" -le 6 ] \
    || fail "managed budget allowed ${allowed} requests, expected 5-6 across both replicas"
  pass "managed budget remains coordinated across replicas, without response-cache hits"
}

# Exercise a standalone-to-managed upgrade with existing keys and saved DB
# settings. The rendered value alone cannot establish its effective precedence
# in the pinned LiteLLM runtime.
echo "==> Upgrading to managed authentication with stored settings"
kubectl exec -i -n "${namespace}" "${postgresql_pod}" -- psql -U litellm -d litellm -v ON_ERROR_STOP=1 -q \
  >/dev/null <<'SQL' || fail "could not save general settings in LiteLLM_Config"
INSERT INTO "LiteLLM_Config" (param_name, param_value)
VALUES ('general_settings', '{"user_api_key_cache_ttl": 120}')
ON CONFLICT (param_name) DO UPDATE SET param_value = EXCLUDED.param_value;
SQL
stop_forwards
helm upgrade "${release_name}" "${chart_dir}" -n "${namespace}" -f "${values_file}" \
  --set operatorIntegration=true --set metrics.enabled=true --set spendLogs.enabled=false \
  --set spendLogs.storePrompts=false \
  --wait --timeout 10m >/dev/null || fail "managed authentication upgrade did not complete"
forward_pods
port_a="${pod_ports[0]}"
[ "$(secret_value salt-key)" = "${salt_before}" ] || fail "managed upgrade changed the salt key"
[ "$(secret_value master-key)" = "${master_key}" ] || fail "managed upgrade changed the master key"
key_requests "${saved_key}" mock-priced 200
managed_key_freshness upgrade

stop_forwards
kubectl rollout restart "deployment/${fullname}" -n "${namespace}" >/dev/null
kubectl rollout status "deployment/${fullname}" -n "${namespace}" --timeout=600s >/dev/null \
  || fail "managed Gateway restart did not complete"
forward_pods
port_a="${pod_ports[0]}"
key_requests "${saved_key}" mock-priced 200
managed_key_freshness restart
kubectl exec -n "${namespace}" "${postgresql_pod}" -- psql -U litellm -d litellm -v ON_ERROR_STOP=1 -tAc \
  "DELETE FROM \"LiteLLM_Config\" WHERE param_name = 'general_settings'" >/dev/null \
  || fail "could not remove the saved general settings"

# ----- Uninstall and install again -------------------------------------------

# The database outlives the release: the bundled PostgreSQL's volume is kept on
# uninstall, like an external database would be. When helm uninstall removed
# the auth Secret, the next install generated a new salt key against the old
# data: every database-backed model vanished from the model list, the previous
# master key stopped working, and only a log warning said why. The PostgreSQL
# password has the same property, since the kept data was initialised with it.
# Both Secrets are kept, so the reinstall's lookups find and reuse them.
echo "==> Uninstalling and installing again"
stop_forwards
pvc_before="$(kubectl get pvc "data-${postgresql_pod}" -n "${namespace}" -o jsonpath='{.metadata.uid}')"
helm uninstall "${release_name}" -n "${namespace}" --wait >/dev/null || fail "uninstall did not complete"
kubectl get secret "${fullname}-auth" -n "${namespace}" >/dev/null 2>&1 \
  || fail "helm uninstall deleted the auth Secret; the next install would generate a new salt key"
kubectl get secret "${postgresql_name}" -n "${namespace}" >/dev/null 2>&1 \
  || fail "helm uninstall deleted the bundled PostgreSQL's Secret; the next install would be locked out of the kept data"
[ "$(kubectl get pvc "data-${postgresql_pod}" -n "${namespace}" -o jsonpath='{.metadata.uid}' 2>/dev/null)" = "${pvc_before}" ] \
  || fail "helm uninstall deleted the bundled PostgreSQL's volume"
pass "auth Secret, database Secret and database volume kept on uninstall"

helm install "${release_name}" "${chart_dir}" -n "${namespace}" -f "${values_file}" \
  --set operatorIntegration=true \
  --wait --timeout 10m >/dev/null || fail "install after uninstall did not complete"
[ "$(secret_value salt-key)" = "${salt_before}" ] || fail "the salt key changed across uninstall and install"
[ "$(secret_value master-key)" = "${master_key}" ] || fail "the master key changed across uninstall and install"
pass "salt key and master key reused by the new install"

forward_pods
port_a="${pod_ports[0]}"
[ "$(chat "${port_a}" "${master_key}" mock-db)" = "200" ] \
  || fail "database-backed model stopped working after uninstall and install: $(last_body)"
pass "the kept database was found again, and its model decrypts and serves"
key_requests "${saved_key}" mock-priced 200
managed_key_freshness reinstall

# ----- An image built before the request log ---------------------------------

# A release can keep an image built before the request log existed, for example
# one pinned by digest across a chart upgrade. That image does not have the
# callback: with the callback named in LiteLLM's configuration, it exited at
# startup with an ImportError. The chart must start it, and LiteLLM's spend log
# must stay on with it: nothing else would record each call.
if [ -n "${previous_image_tag}" ]; then
  echo "==> An image built before the request log starts and keeps LiteLLM's spend log"
  stop_forwards
  helm upgrade "${release_name}" "${chart_dir}" -n "${namespace}" -f "${values_file}" \
    --set-string image.repository="${previous_image_repository}" --set-string image.tag="${previous_image_tag}" \
    --set metrics.enabled=true --set spendLogs.enabled=false --set spendLogs.storePrompts=false \
    --wait --timeout 10m >/dev/null \
    || fail "the chart did not start ${previous_image_repository}:${previous_image_tag}"
  forward_pods
  port_a="${pod_ports[0]}"
  previous_key="$(api_post "${port_a}" /key/generate '{"models":["mock-priced"],"max_budget":10}' | jq -r .key)" \
    || fail "could not create a virtual key"
  previous_spend_logs_before="$(spend_log_successes)"
  for port in "${pod_ports[@]}"; do
    [ "$(chat "${port}" "${previous_key}" mock-priced)" = "200" ] || fail "a call to the previous image failed: $(last_body)"
  done
  sleep 15
  previous_spend_logs_after="$(spend_log_successes)"
  [ "${previous_spend_logs_after}" -ge $((previous_spend_logs_before + 2)) ] \
    || fail "with the previous image, LiteLLM_SpendLogs grew from ${previous_spend_logs_before} to ${previous_spend_logs_after} for 2 calls"
  pass "the previous image serves, and LiteLLM's spend log recorded its 2 calls"
fi

echo "All Gateway cluster checks passed."

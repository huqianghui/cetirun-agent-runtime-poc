#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# Render assertions for the CetiRun AI Gateway chart. Offline: helm template
# only, no cluster and no install.
#
# Several of these guard behaviour that was measured against LiteLLM rather than
# assumed, and where the failure mode is silent. Those cases carry a comment
# explaining what goes wrong without the guard.

set -euo pipefail

script_dir="$(cd "$(dirname "$0")" && pwd)"
repo_root="$(cd "$script_dir/.." && pwd)"
chart_dir="${CHART_DIR:-${repo_root}/app/cetirun-ai-gateway/chart}"
release_name="${RELEASE_NAME:-gateway}"

if [ ! -f "${chart_dir}/Chart.yaml" ]; then
  echo "ERROR: Chart.yaml not found in ${chart_dir}" >&2
  exit 1
fi

app_version="$(awk -F': *' '$1=="appVersion" {gsub(/"/, "", $2); print $2; exit}' "${chart_dir}/Chart.yaml")"

# A usable baseline: the defaults, which bundle PostgreSQL and Valkey, plus one
# model configured at install.
base_args=(
  --set-string "models[0].model_name=lint-model"
  --set-string "models[0].litellm_params.model=openai/lint-model"
)
# The production path: an external database and Redis.
external_args=(
  --set "postgresql.bundled.enabled=false"
  --set-string "postgresql.urlSecret.existingSecret=lint-database"
  --set "redis.bundled.enabled=false"
  --set-string "redis.host=lint-redis.example.invalid"
)

render() {
  helm template "${release_name}" "${chart_dir}" "${base_args[@]}" "$@"
}

# Assert a needle is present. Uses a here-string rather than a pipe so that
# grep exiting early cannot trip pipefail.
assert_contains() {
  local needle="$1" description="$2" output="$3"
  if ! grep -q -- "${needle}" <<< "${output}"; then
    echo "FAIL: ${description}" >&2
    exit 1
  fi
}

assert_absent() {
  local needle="$1" description="$2" output="$3"
  if grep -q -- "${needle}" <<< "${output}"; then
    echo "FAIL: ${description}" >&2
    exit 1
  fi
}

assert_rejected() {
  local description="$1"
  shift
  if render "$@" > /dev/null 2>&1; then
    echo "FAIL: ${description} should have been rejected" >&2
    exit 1
  fi
}

# The initContainers block of the Gateway Deployment.
init_block() {
  awk '/^      initContainers:/{f=1} /^      containers:/{f=0} f' <<< "$1"
}

# The containers block of the Gateway Deployment, without its init containers.
gateway_block() {
  awk '/^      containers:/{f=1} f' <<< "$1"
}

# object_name KIND COMPONENT RENDER: the name of the rendered object of that
# kind whose own labels carry app.kubernetes.io/component=COMPONENT. Prints
# one line: awk runs END even after exit, so END checks whether it printed.
object_name() {
  awk -v kind="$1" -v component="$2" '
    /^---/ { if (k == kind && c) { print n; found = 1; exit } k = ""; n = ""; c = 0; meta = 0; next }
    /^kind: / { k = $2 }
    /^metadata:/ { meta = 1; next }
    /^[^ ]/ { meta = 0 }
    meta && /^  name: / && n == "" { n = $2 }
    meta && /^    app\.kubernetes\.io\/component: / && $2 == component { c = 1 }
    END { if (!found && k == kind && c) print n }' <<< "$3"
}

# A name read from the render must be a single word. grep reads a pattern with
# a newline as two alternatives, so a doubled name would still match.
single_name() {  # single_name NAME DESCRIPTION
  if [ -z "$1" ] || [[ "$1" =~ [[:space:]] ]]; then
    echo "FAIL: ${2} is not a single name: '${1}'" >&2
    exit 1
  fi
}

# ----- Baseline render -----

echo "==> helm template ${release_name} ${chart_dir} (baseline)"
output="$(render)"
external="$(render "${external_args[@]}")"
assert_contains "kind: Deployment" "Deployment is rendered" "${output}"
assert_contains "kind: Service" "Service is rendered" "${output}"
assert_contains "kind: ConfigMap" "ConfigMap is rendered" "${output}"
assert_contains "model_name: lint-model" "configured model reaches the LiteLLM config" "${output}"
assert_contains "\- \-\-config" "the config path is passed explicitly" "${output}"
# Mounted as a directory, the file changed inside running pods after an
# upgrade, and LiteLLM's periodic re-read removed an edited model from the old
# pods until they were replaced. subPath keeps each pod's file as it started.
gateway_deployment="$(render -s templates/deployment.yaml)"
config_mount="$(awk '/^            - name: config$/ { n = 4 } n > 0 { print; n-- }' <<< "${gateway_deployment}")"
assert_contains "mountPath: /etc/litellm/config.yaml$" "the configuration is mounted as a file" "${config_mount}"
assert_contains "subPath: config.yaml$" "the configuration file is mounted with subPath" "${config_mount}"
assert_absent "mountPath: /etc/litellm$" "the configuration directory is not mounted" "${gateway_deployment}"

# Names are read from the rendered objects rather than rebuilt here: the chart
# shortens long names, and uses the release name alone when it already
# contains the chart's, so RELEASE_NAME can be any valid release name.
postgresql_name="$(object_name StatefulSet postgresql "${output}")"
valkey_name="$(object_name Service valkey "${output}")"
single_name "${postgresql_name}" "the bundled PostgreSQL in the default render"
single_name "${valkey_name}" "the bundled Valkey in the default render"

# ----- Required inputs -----

# ----- No models at install -----

# A Gateway can start without models: administrators add them through the
# console later, and a CetiRun deployment may use only the GitHub Copilot
# models the operator registers for each Agent.
echo "==> a Gateway without models renders"
no_models="$(helm template "${release_name}" "${chart_dir}")"
assert_contains "model_list:" "the configuration still lists models" "${no_models}"
assert_absent "model_name:" "no model is configured" "${no_models}"

echo "==> expected rejections for missing or conflicting input"

assert_rejected "missing litellm_params.model" --set-string "models[0].litellm_params.model="
assert_rejected "no database at all" --set "postgresql.bundled.enabled=false"
assert_rejected "an external database while the bundled one is on" \
  --set-string "postgresql.urlSecret.existingSecret=lint-database"
assert_rejected "an external Redis while the bundled one is on" --set-string "redis.host=r.example.invalid"
assert_rejected "both provider credential sources" \
  --set-string "providerCredentials.existingSecret=a" --set-string "providerCredentials.inline.B=c"

# ----- Reserved model-name prefix -----

# A model added later under a name that already exists does not replace the
# original: both are served under one name and traffic splits between two
# different backends, invisibly to the client. The prefix keeps operator
# generated names out of the administrator's namespace.
echo "==> reserved model-name prefix"
assert_rejected "reserved cetirun- prefix" --set-string "models[0].model_name=cetirun-gpt4"
assert_rejected "reserved prefix in another case" --set-string "models[0].model_name=CetiRun-gpt4"
# A standalone Gateway keeps serving models under copilot/; serving CetiRun
# Agents reserves that prefix for their GitHub Copilot routes.
standalone_copilot="$(render --set-string "models[0].model_name=copilot/gpt-5")"
assert_contains "model_name: copilot/gpt-5" "a standalone Gateway accepts a copilot/ model name" "${standalone_copilot}"
assert_rejected "reserved copilot/ prefix with the operator integration" \
  --set operatorIntegration=true --set-string "models[0].model_name=copilot/gpt-5"
assert_rejected "reserved copilot/ prefix in another case with the operator integration" \
  --set operatorIntegration=true --set-string "models[0].model_name=Copilot/gpt-5"

# ----- Operator integration -----

# Router fallbacks are chosen after the key check. Serving CetiRun Agents, the
# Gateway checks each fallback target against the caller's key, so a fallback
# cannot reach another Agent's GitHub Copilot route.
echo "==> fallback targets are checked against the caller's key for CetiRun"
assert_absent "enforce_fallback_model_access" "a standalone Gateway keeps LiteLLM's fallback default" "${output}"
integration="$(render --set operatorIntegration=true)"
assert_contains "enforce_fallback_model_access: true" \
  "the operator integration checks fallback targets against the caller's key" "${integration}"
assert_absent "user_api_key_cache_ttl" "standalone authentication-cache defaults are unchanged" "${output}"
assert_contains "user_api_key_cache_ttl: 0" \
  "managed keys use current authorization instead of cached grants" "${integration}"

# ----- Image -----

# The Gateway runs our own image, built from app/cetirun-ai-gateway, and follows
# the other CetiRun charts in taking its registry and version from the umbrella
# chart's global values.
echo "==> the Gateway runs the CetiRun image"
assert_contains "image: mcr.microsoft.com/aks/ai-runtime/cetirun/cetirun-ai-gateway:${app_version}$" \
  "the default image is cetirun-ai-gateway at the chart's appVersion" "${output}"
assert_absent "berriai" "no upstream LiteLLM image is referenced" "${output}"
umbrella="$(render --set-string global.registry=registry.example.invalid/ --set-string global.version=9.9.9)"
assert_contains "image: registry.example.invalid/cetirun-ai-gateway:9.9.9$" \
  "global.registry and global.version select the image" "${umbrella}"
pinned="$(render --set-string image.digest=sha256:0000000000000000000000000000000000000000000000000000000000000000)"
assert_contains "cetirun-ai-gateway:${app_version}@sha256:0000" "a digest pins the image" "${pinned}"

echo "==> the Gateway runs as a non-root user"
gateway_pod="$(render -s templates/deployment.yaml)"
assert_contains "runAsNonRoot: true" "the Gateway pods refuse to run as root" "${gateway_pod}"
assert_contains "runAsUser: 65532" "the Gateway pods run as the image's nonroot user" "${gateway_pod}"

# ----- Redis and replicas -----

# Without shared state each replica enforces its own copy of every budget and
# rate limit, so a limit is multiplied by the replica count. Measured: a $0.10
# budget permitted $0.20 across two replicas.
echo "==> Redis is required above one replica"
assert_rejected "two replicas without Redis" --set "redis.bundled.enabled=false" --set replicaCount=2

echo "==> a single replica does not require Redis"
single="$(render --set "redis.bundled.enabled=false" --set replicaCount=1)"
assert_contains "replicas: 1" "single replica renders" "${single}"
assert_absent "redis_host" "no Redis configuration without Redis" "${single}"
assert_absent "component: valkey" "no Valkey without Redis" "${single}"

# Redis declared in only one block is inert: it stays unused while budgets keep
# doubling, and the deployment still looks correctly configured. Both blocks
# have to be present, and both have to carry the password.
echo "==> Redis is wired into both configuration blocks"
assert_contains "redis_host: \"${valkey_name}\"" "router_settings points at the bundled Valkey" "${output}"
assert_contains "type: redis" "litellm_settings carries the Redis cache block" "${output}"
assert_contains "host: \"${valkey_name}\"" "the cache block points at the bundled Valkey" "${output}"
assert_contains "redis_password: os.environ/REDIS_PASSWORD" "router_settings carries the password" "${output}"
assert_contains "password: os.environ/REDIS_PASSWORD" "the cache block carries the password" "${output}"
assert_contains "name: REDIS_PASSWORD" "the proxy receives the Valkey password" "${output}"
assert_contains "redis_host: \"lint-redis.example.invalid\"" "an external Redis is used when configured" "${external}"

# The Redis-backed cache is what makes budgets coordinate across replicas, but
# at its default it also answers identical requests from Redis, at zero spend,
# outside any budget, and across callers. Measured: with it on, a $0.10 budget
# allowed every one of sixteen identical requests.
echo "==> response caching is off unless a request asks for it"
assert_contains "mode: default_off" "the Redis cache does not answer requests by default" "${output}"

# ----- Retries -----

# LiteLLM retries a failed upstream call twice by default. Agents retry too, so
# the two multiply: one failing request was measured at three upstream calls.
echo "==> the Gateway does not retry upstream failures"
assert_contains "num_retries: 0" "LiteLLM's default retries are turned off" "${output}"

# ----- Routing -----

# Only the strategies LiteLLM selects with on its asynchronous path, which runs
# the auto routers and the pre-call checks, are accepted.
echo "==> the routing strategy is the administrator's, among those that run the pre-call checks"
assert_contains 'routing_strategy: "simple-shuffle"' "deployments are chosen at random by default" "${output}"
assert_contains 'routing_strategy: "simple-shuffle"' "the strategy does not depend on Redis" "${single}"
for strategy in least-busy latency-based-routing usage-based-routing-v2 cost-based-routing; do
  assert_contains "routing_strategy: \"${strategy}\"" "${strategy} can be chosen" \
    "$(render --set "router.routingStrategy=${strategy}")"
done
assert_rejected "usage-based-routing, which skips the pre-call checks" --set router.routingStrategy=usage-based-routing
assert_rejected "an unknown routing strategy" --set router.routingStrategy=round-robin

# simple-shuffle weighs by the first of weight, rpm and tpm with a positive
# value and gives a deployment without it no requests; a deployment without
# order is left out whenever another has one.
echo "==> weight, order, rpm and tpm are set on every deployment of a model or on none"
second=(--set-string 'models[1].model_name=lint-model' --set-string 'models[1].litellm_params.model=openai/lint-model-2')
for field in weight order rpm tpm; do
  assert_rejected "${field} on one of two deployments" "${second[@]}" --set "models[0].litellm_params.${field}=1"
  render "${second[@]}" --set "models[0].litellm_params.${field}=1" --set "models[1].litellm_params.${field}=2" > /dev/null || {
    echo "FAIL: ${field} on both deployments of a model should render" >&2
    exit 1
  }
done
render "${second[@]}" --set-string 'models[2].model_name=other-model' --set-string 'models[2].litellm_params.model=openai/other' \
  --set 'models[2].litellm_params.weight=1' > /dev/null || {
  echo "FAIL: a model with one deployment may set weight" >&2
  exit 1
}

# Several deployments behind one model name: a request that fails on one is sent
# once more, to another. It is not a retry, so num_retries stays 0. Measured with
# one of two deployments failing: 13 of 30 requests failed without it, none with
# it.
echo "==> a failed request is sent to another deployment of the model"
assert_contains "enable_weighted_failover: true" "failover is on by default" "${output}"
assert_contains "enable_weighted_failover: true" "failover does not depend on Redis" "${single}"
assert_absent "enable_weighted_failover" "failover can be turned off" "$(render --set router.failover=false)"

# A Responses API continuation (previous_response_id) works only on the
# deployment that produced the earlier response. Measured across two
# deployments: 6 of 20 continuations failed without the check. Only the
# resource that encrypted a model's reasoning can read it; LiteLLM 1.105.1
# moves a conversation off its deployment, without that reasoning, only while
# the deployment is in cooldown or marked unhealthy by the health checks.
# A key kept on one deployment keeps the provider's prompt cache.
echo "==> continuations, encrypted reasoning and keys stay on their deployment"
assert_contains 'optional_pre_call_checks: \["responses_api_deployment_check","encrypted_content_affinity","deployment_affinity"\]' \
  "the three affinity checks are on by default" "${output}"
assert_contains 'optional_pre_call_checks: \["responses_api_deployment_check","deployment_affinity"\]' \
  "encrypted reasoning affinity can be turned off" "$(render --set router.encryptedContentAffinity=false)"
assert_contains 'optional_pre_call_checks: \["responses_api_deployment_check","encrypted_content_affinity"\]' \
  "deployment affinity can be turned off" "$(render --set router.deploymentAffinity=false)"
assert_absent "optional_pre_call_checks" "no pre-call checks when all are off" \
  "$(render --set router.continuationAffinity=false --set router.encryptedContentAffinity=false --set router.deploymentAffinity=false)"
assert_absent "model_group_affinity_config" "no per-model affinity without standby deployments" "${output}"

# Deployment affinity pins a key before LiteLLM keeps only the lowest order, so
# a key pinned to a standby would stay there after the primary is back. It is
# off for such a model; the other checks stay on for it.
echo "==> deployment affinity is off for a model with standby deployments"
ordered="$(render "${second[@]}" --set 'models[0].litellm_params.order=1' --set 'models[1].litellm_params.order=2' \
  --set-string 'models[2].model_name=other-model' --set-string 'models[2].litellm_params.model=openai/other')"
assert_contains 'model_group_affinity_config: {"lint-model":\["responses_api_deployment_check","encrypted_content_affinity"\]}' \
  "only the ordered model leaves deployment affinity out, keeping the other checks" "${ordered}"
assert_contains 'model_group_affinity_config: {"lint-model":\[\]}' "a model with standby deployments can have no checks at all" \
  "$(render "${second[@]}" --set 'models[0].litellm_params.order=1' --set 'models[1].litellm_params.order=2' \
    --set router.continuationAffinity=false --set router.encryptedContentAffinity=false)"
assert_absent "model_group_affinity_config" "nothing to turn off without deployment affinity" \
  "$(render "${second[@]}" --set 'models[0].litellm_params.order=1' --set 'models[1].litellm_params.order=2' \
    --set router.deploymentAffinity=false)"

echo "==> fallbacks are the administrator's"
assert_absent "fallbacks:" "no fallbacks by default" "${output}"
assert_contains 'fallbacks: \[{"lint-model":\["other-model"\]}\]' "configured fallbacks reach the router" \
  "$(render --set 'router.fallbacks[0].lint-model[0]=other-model')"
assert_contains 'context_window_fallbacks: \[{"lint-model":\["long-model"\]}\]' \
  "configured context window fallbacks reach the router" \
  "$(render --set 'router.contextWindowFallbacks[0].lint-model[0]=long-model')"
assert_rejected "a fallback target that is not a list" --set-string 'router.fallbacks[0].lint-model=other-model'

# Each probe is a real call, to every deployment: those configured here and
# those added later, but not one that sets disable_background_health_check, as
# the operator's per-Agent GitHub Copilot routes do by default. LiteLLM never
# probes an auto router itself. With Redis, one replica probes under a lock and
# the others reuse its results for one interval.
echo "==> background health checks probe every deployment, once for all replicas"
assert_contains "background_health_checks: true" "health checks are on by default" "${output}"
assert_absent "background_health_check_model_groups" "every deployment is probed, not a chosen list" "${output}"
assert_contains "enable_health_check_routing: true" "requests go only to deployments that answered" "${output}"
assert_contains "health_check_interval: 300$" "probes every five minutes by default" "${output}"
assert_contains "health_check_staleness_threshold: 600$" "a result counts for two intervals" "${output}"
assert_contains "health_check_ignore_transient_errors: true" "a probe answered with 429 does not exclude a deployment" "${output}"
assert_contains "use_shared_health_check: true" "with Redis, one replica probes for all" "${output}"
grep -A1 'name: DEFAULT_SHARED_HEALTH_CHECK_TTL$' <<< "${output}" | grep -q 'value: "300"' || {
  echo "FAIL: shared probe results are not reused past the next probe" >&2
  exit 1
}
grep -A1 'name: DEFAULT_SHARED_HEALTH_CHECK_TTL$' <<< "$(render --set router.healthChecks.intervalSeconds=120)" \
  | grep -q 'value: "120"' || {
  echo "FAIL: the shared result lifetime does not follow the probe interval" >&2
  exit 1
}
assert_contains "health_check_interval: 120$" "the probe interval can be changed" "$(render --set router.healthChecks.intervalSeconds=120)"
assert_absent "use_shared_health_check" "a single replica without Redis probes on its own" "${single}"
assert_absent "DEFAULT_SHARED_HEALTH_CHECK_TTL" "no shared results without Redis" "${single}"
assert_contains "background_health_checks: true" "a single replica without Redis still probes" "${single}"
off="$(render --set router.healthChecks.enabled=false)"
assert_absent "background_health_checks" "health checks can be turned off" "${off}"
assert_absent "use_shared_health_check" "no shared health checks when off" "${off}"
assert_absent "DEFAULT_SHARED_HEALTH_CHECK_TTL" "no shared result lifetime when off" "${off}"
assert_rejected "a probe interval under 30 seconds" --set router.healthChecks.intervalSeconds=10

# Without a limit LiteLLM starts every probe at once: one GitHub account
# answered 11 to 43 of a cycle's Copilot probes with 429. LiteLLM reads the
# limit from general_settings, and treats 0 as no limit.
echo "==> probes run a few at a time, under one lock per cycle"
assert_contains "health_check_concurrency: 8$" "at most eight probes run at once by default" "${output}"
assert_contains "health_check_concurrency: 3$" "the probe concurrency can be changed" \
  "$(render --set router.healthChecks.concurrency=3)"
assert_contains "health_check_concurrency: 8$" "a single replica limits its probes too" "${single}"
assert_absent "health_check_concurrency" "no probe concurrency when probes are off" "${off}"
assert_rejected "a probe concurrency of 0, which LiteLLM reads as no limit" --set router.healthChecks.concurrency=0
assert_rejected "a negative probe concurrency" --set router.healthChecks.concurrency=-1
# The probing replica's lock expires after 60 seconds unless set. A longer cycle
# would let another replica take it and probe every deployment again.
grep -A1 'name: DEFAULT_SHARED_HEALTH_CHECK_LOCK_TTL$' <<< "${output}" | grep -q 'value: "300"' || {
  echo "FAIL: the shared probe lock does not last one interval" >&2
  exit 1
}
grep -A1 'name: DEFAULT_SHARED_HEALTH_CHECK_LOCK_TTL$' <<< "$(render --set router.healthChecks.intervalSeconds=120)" \
  | grep -q 'value: "120"' || {
  echo "FAIL: the shared probe lock does not follow the probe interval" >&2
  exit 1
}
assert_absent "DEFAULT_SHARED_HEALTH_CHECK_LOCK_TTL" "no shared probe lock without Redis" "${single}"
assert_absent "DEFAULT_SHARED_HEALTH_CHECK_LOCK_TTL" "no shared probe lock when off" "${off}"

# A LiteLLM auto router is an entry in models whose model is
# auto_router/complexity_router. Its own name is checked against the unpriced
# model block like any model; measured, each call is charged at the price of the
# model it picks and budgets hold. A price of zero would make LiteLLM's notion
# of a free model the only thing between it and the budget checks, so the
# chart wants one above zero. Its tiers have to be models served here, and
# without a LiteLLM licence the proxy refuses to start with more than one
# router that writes its own classifier prompt.
echo "==> an auto router is priced, routes to models configured here, and fits the licence"
router_dir="$(mktemp -d)"
trap 'rm -rf "${router_dir}"' EXIT
default_router_config='tiers: {SIMPLE: lint-model, MEDIUM: [lint-model, big-model], COMPLEX: big-model, REASONING: big-model}'
router_values() {  # router_values FILE [ROUTER_CONFIG] [INPUT_PRICE]: a router named auto over lint-model and big-model
  local file="$1" config="${2:-${default_router_config}}" price="${3:-0.00001}"
  cat > "${file}" <<EOF
models:
  - model_name: lint-model
    litellm_params: {model: openai/lint-model}
  - model_name: big-model
    litellm_params: {model: openai/big-model}
  - model_name: auto
    litellm_params:
      model: auto_router/complexity_router
      input_cost_per_token: ${price}
      output_cost_per_token: 0.00004
      complexity_router_config:
        ${config}
      complexity_router_default_model: big-model
EOF
}
router_values "${router_dir}/valid.yaml"
routed="$(helm template "${release_name}" "${chart_dir}" -f "${router_dir}/valid.yaml" --set operatorIntegration=true)"
assert_contains "model: auto_router/complexity_router" "a router configured in models reaches LiteLLM" "${routed}"
assert_absent "background_health_check_model_groups" \
  "every deployment is probed; LiteLLM never probes a router itself" "${routed}"

# LiteLLM 1.105.1 compacts a conversation near the model's input limit with
# extra model calls, replaces the caller's max_tokens with the tier model's
# ceiling, and refuses a request too long for the decided tier's models. The
# chart changes those defaults, unless a router sets them itself.
echo "==> a router leaves the conversation and max_tokens to the agent, and moves a request that does not fit"
assert_contains "context_compaction: false" "no compaction by the Gateway" "${routed}"
assert_contains "max_tokens_from_tier_model: false" "the caller's max_tokens is forwarded" "${routed}"
assert_contains "enable_context_window_escalation: true" "a request too long for its tier moves to one that fits" "${routed}"
router_values "${router_dir}/own-defaults.yaml" "${default_router_config}
        context_compaction: {trigger_ratio: 0.9}
        max_tokens_from_tier_model: true
        enable_context_window_escalation: false"
own_defaults="$(helm template "${release_name}" "${chart_dir}" -f "${router_dir}/own-defaults.yaml")"
assert_contains "trigger_ratio: 0.9" "a router's own compaction is kept" "${own_defaults}"
assert_contains "max_tokens_from_tier_model: true" "a router's own max_tokens setting is kept" "${own_defaults}"
assert_contains "enable_context_window_escalation: false" "a router's own escalation setting is kept" "${own_defaults}"

# A jev classifier that fails, as Microsoft-Decision-1 did for a request over
# its context, leaves the request to LiteLLM's heuristic scorer by default,
# which placed 11 of 40 Chinese and 15 of 40 English prompts on the expected
# tier in our tests. The chart sends it to the router's default model instead,
# unless the router sets classifier_fallback, or defines its own tiers, whose
# fallback_tier LiteLLM uses and next to which it refuses default_model.
echo "==> a jev router sends a request it could not classify to its default model"
assert_absent "classifier_fallback" "a heuristic router keeps LiteLLM's fallback" "${routed}"
jev_router_config="${default_router_config}
        classifier_type: jev
        jev_classifier_config: {deployment_name: lint-model}"
router_values "${router_dir}/jev.yaml" "${jev_router_config}"
assert_contains "classifier_fallback: default_model" "a jev router falls back to its default model" \
  "$(helm template "${release_name}" "${chart_dir}" -f "${router_dir}/jev.yaml" --set operatorIntegration=true)"
assert_contains "classifier_fallback: default_model" "also without the operator integration" \
  "$(helm template "${release_name}" "${chart_dir}" -f "${router_dir}/jev.yaml" --set operatorIntegration=false)"
router_values "${router_dir}/jev-own.yaml" "${jev_router_config}
        classifier_fallback: heuristic"
jev_own="$(helm template "${release_name}" "${chart_dir}" -f "${router_dir}/jev-own.yaml")"
assert_contains "classifier_fallback: heuristic" "a jev router's own fallback is kept" "${jev_own}"
assert_absent "classifier_fallback: default_model" "and not replaced" "${jev_own}"
router_values "${router_dir}/llm.yaml" "${default_router_config}
        classifier_type: llm
        classifier_llm_config: {model: lint-model}"
assert_absent "classifier_fallback" "an llm router keeps LiteLLM's fallback" \
  "$(helm template "${release_name}" "${chart_dir}" -f "${router_dir}/llm.yaml")"
router_values "${router_dir}/jev-tiers.yaml" "tiers: {LIGHT: lint-model, HEAVY: big-model}
        classifier_type: jev
        jev_classifier_config: {deployment_name: lint-model}
        tier_definitions: [{name: LIGHT, description: short asks}, {name: HEAVY, description: long work}]
        fallback_tier: HEAVY"
assert_absent "classifier_fallback" "a jev router with its own tiers keeps its fallback_tier" \
  "$(helm template "${release_name}" "${chart_dir}" -f "${router_dir}/jev-tiers.yaml")"

# OpenClaw sends its own context as the last user message, between markers. A
# router classifies the newest message a person wrote, so without the markers
# every OpenClaw request was classified by that block: measured, all of them
# went to the simple tier, a request for code included.
echo "==> a router skips OpenClaw's context when it classifies"
assert_contains "open: <<<BEGIN_OPENCLAW_INTERNAL_CONTEXT>>>" "a router reads past OpenClaw's context" "${routed}"
assert_contains "open: <system-reminder>" "and keeps LiteLLM's own markers" "${routed}"
# OpenClaw passes on a tool result's images in a user message of their own,
# labelled "Image(s) from tool result #1 (computer):". Measured, the label put
# a request for a design in the simple tier.
assert_contains "open: 'Image(s) from tool result #'" "and the label of OpenClaw's tool-result images" "${routed}"
standalone="$(helm template "${release_name}" "${chart_dir}" -f "${router_dir}/valid.yaml" --set operatorIntegration=false)"
assert_contains "open: <<<BEGIN_OPENCLAW_INTERNAL_CONTEXT>>>" "also without the operator integration" "${standalone}"
router_values "${router_dir}/own-markers.yaml" "${default_router_config}
        reminder_markers: [{open: '<ctx>', close: '</ctx>'}]"
own="$(helm template "${release_name}" "${chart_dir}" -f "${router_dir}/own-markers.yaml")"
assert_contains "open: <ctx>" "a router's own markers are kept" "${own}"
assert_absent "OPENCLAW_INTERNAL_CONTEXT" "and not extended" "${own}"
router_rejected() {  # router_rejected DESCRIPTION FILE
  if helm template "${release_name}" "${chart_dir}" -f "$2" > /dev/null 2>&1; then
    echo "FAIL: $1 should have been rejected" >&2
    exit 1
  fi
}
# LiteLLM 1.105.1 takes only {open, close} pairs, two different strings that
# are not blank, and refuses to start otherwise.
for markers in "['<system-reminder>']" "[]" "[{open: '<a>'}]" "[{open: '##', close: '##'}]" "[{open: '<a>', close: ' '}]"; do
  router_values "${router_dir}/bad-markers.yaml" "${default_router_config}
        reminder_markers: ${markers}"
  router_rejected "reminder_markers ${markers}" "${router_dir}/bad-markers.yaml"
done
router_values "${router_dir}/zero.yaml" "" 0
router_rejected "a router priced at zero" "${router_dir}/zero.yaml"
grep -v 'cost_per_token' "${router_dir}/valid.yaml" > "${router_dir}/unpriced.yaml"
router_rejected "an unpriced router" "${router_dir}/unpriced.yaml"
helm template "${release_name}" "${chart_dir}" -f "${router_dir}/unpriced.yaml" --set blockUnpricedModels=false > /dev/null \
  || { echo "FAIL: an unpriced router was rejected with blockUnpricedModels off" >&2; exit 1; }
router_values "${router_dir}/unknown.yaml" 'tiers: {SIMPLE: lint-model, COMPLEX: elsewhere}'
router_rejected "a router tier outside models" "${router_dir}/unknown.yaml"
router_values "${router_dir}/itself.yaml" 'tiers: {SIMPLE: lint-model, COMPLEX: auto}'
router_rejected "a router tier that is a router" "${router_dir}/itself.yaml"
# LiteLLM does not refuse a jev classifier whose decision model is not served, and
# the chart's default_model fallback then sends every request to the default model.
router_values "${router_dir}/jev-unknown.yaml" "${default_router_config}
        classifier_type: jev
        jev_classifier_config: {deployment_name: decision-typo}"
router_rejected "a jev classifier outside models" "${router_dir}/jev-unknown.yaml"
router_values "${router_dir}/jev-router.yaml" "${default_router_config}
        classifier_type: jev
        jev_classifier_config: {deployment_name: auto}"
router_rejected "a jev classifier that is a router" "${router_dir}/jev-router.yaml"
router_values "${router_dir}/jev-unnamed.yaml" "${default_router_config}
        classifier_type: jev"
router_rejected "a jev classifier without a decision model" "${router_dir}/jev-unnamed.yaml"
router_values "${router_dir}/no-tiers.yaml" 'classifier_type: heuristic'
router_rejected "a router without tiers" "${router_dir}/no-tiers.yaml"
router_values "${router_dir}/one-prompt.yaml" \
  'tiers: {SIMPLE: lint-model, COMPLEX: big-model}
        classifier_type: llm
        classifier_llm_config: {model: lint-model, system_prompt: rubric}'
helm template "${release_name}" "${chart_dir}" -f "${router_dir}/one-prompt.yaml" > /dev/null \
  || { echo "FAIL: one router with its own classifier prompt was rejected" >&2; exit 1; }
sed -n '/model_name: auto$/,$p' "${router_dir}/one-prompt.yaml" | sed 's/model_name: auto$/model_name: auto-two/' \
  > "${router_dir}/second.yaml"
cat "${router_dir}/one-prompt.yaml" "${router_dir}/second.yaml" > "${router_dir}/two-prompts.yaml"
router_rejected "two routers with their own classifier prompt" "${router_dir}/two-prompts.yaml"
# Without a licence, LiteLLM 1.105.1 lets one router claim each licensed
# capability: heuristic_v2, capability and llm_v2 classifiers, and its own
# tiers or classifier prompt. One of each is fine; a model classifier with
# LiteLLM's own prompt claims none.
two_routers() {  # two_routers FILE FIRST_SETTINGS SECOND_SETTINGS: routers auto and auto-two over lint-model
  cat > "$1" <<EOF
models:
  - model_name: lint-model
    litellm_params: {model: openai/lint-model}
  - model_name: auto
    litellm_params:
      model: auto_router/complexity_router
      input_cost_per_token: 0.00001
      output_cost_per_token: 0.00001
      complexity_router_config: {tiers: {SIMPLE: lint-model}, $2}
  - model_name: auto-two
    litellm_params:
      model: auto_router/complexity_router
      input_cost_per_token: 0.00001
      output_cost_per_token: 0.00001
      complexity_router_config: {tiers: {SIMPLE: lint-model}, $3}
EOF
}
for type in heuristic_v2 capability llm_v2; do
  two_routers "${router_dir}/two-${type}.yaml" "classifier_type: ${type}" "classifier_type: ${type}"
  router_rejected "two routers with classifier_type ${type}" "${router_dir}/two-${type}.yaml"
done
two_routers "${router_dir}/mixed.yaml" "classifier_type: heuristic_v2" \
  "classifier_type: llm, classifier_llm_config: {model: lint-model, system_prompt: rubric}"
helm template "${release_name}" "${chart_dir}" -f "${router_dir}/mixed.yaml" > /dev/null \
  || { echo "FAIL: one heuristic_v2 router and one with its own classifier prompt were rejected" >&2; exit 1; }
two_routers "${router_dir}/shipped-prompts.yaml" "classifier_type: llm, classifier_llm_config: {model: lint-model}" \
  "classifier_type: llm, classifier_llm_config: {model: lint-model}"
helm template "${release_name}" "${chart_dir}" -f "${router_dir}/shipped-prompts.yaml" > /dev/null \
  || { echo "FAIL: two model classifiers with LiteLLM's own prompt were counted" >&2; exit 1; }
# A jev classifier claims the classifier prompt only with instructions of its own.
two_routers "${router_dir}/shipped-jev.yaml" "classifier_type: jev, jev_classifier_config: {deployment_name: lint-model}" \
  "classifier_type: llm, classifier_llm_config: {model: lint-model, system_prompt: rubric}"
helm template "${release_name}" "${chart_dir}" -f "${router_dir}/shipped-jev.yaml" > /dev/null \
  || { echo "FAIL: a jev classifier with LiteLLM's own instructions was counted" >&2; exit 1; }
two_routers "${router_dir}/two-jev-instructions.yaml" \
  "classifier_type: jev, jev_classifier_config: {deployment_name: lint-model, instructions: rubric}" \
  "classifier_type: jev, jev_classifier_config: {deployment_name: lint-model, instructions: rubric}"
router_rejected "two jev classifiers with their own instructions" "${router_dir}/two-jev-instructions.yaml"

# ----- Budgets -----

# LiteLLM 1.105 refuses a request over budget with 422 unless told otherwise.
# Measured with OpenClaw 2026.9.6 and Hermes v2026.9.24: 422 reads to both as a
# malformed request, 429 as a rate limit they retry, 402 as exhausted credits.
echo "==> a request over budget is refused with 402"
assert_contains "budget_exceeded_status_code: 402$" "a budget refusal is 402 by default" "${output}"
assert_contains "budget_exceeded_status_code: 402$" "a budget refusal does not depend on Redis" "${single}"
for code in 422 429; do
  assert_contains "budget_exceeded_status_code: ${code}$" "${code} can be chosen for a budget refusal" \
    "$(render --set budgetExceededStatusCode=${code})"
done
assert_rejected "a budget refusal status that agents were not measured with" --set budgetExceededStatusCode=500
assert_rejected "a budget refusal status given as text" --set-string budgetExceededStatusCode=402

# ----- Disruptions -----

# A node drain evicts one Gateway replica at a time. maxUnavailable rather than
# minAvailable, and only above one replica, so the budget never blocks a drain.
echo "==> the Gateway replicas are drained one at a time"
pdb="$(render -s templates/poddisruptionbudget.yaml)"
assert_contains "kind: PodDisruptionBudget" "two replicas get a PodDisruptionBudget" "${pdb}"
assert_contains "apiVersion: policy/v1$" "the PodDisruptionBudget uses policy/v1" "${pdb}"
assert_contains "maxUnavailable: 1$" "one replica at a time may be evicted" "${pdb}"
assert_absent "minAvailable" "the budget never requires a replica to stay up" "${pdb}"
assert_contains "app.kubernetes.io/component: gateway$" "the budget selects the Gateway pods" \
  "$(awk '/^  selector:/{f=1} f' <<< "${pdb}")"
[ "$(awk '/^  selector:/{f=1} f' <<< "${pdb}")" \
  = "$(awk '/^  selector:/{f=1} /^  template:/{f=0} f' <<< "$(render -s templates/deployment.yaml)")" ] || {
  echo "FAIL: the PodDisruptionBudget does not select exactly the Deployment's pods" >&2
  exit 1
}
assert_absent "kind: PodDisruptionBudget" "a single replica gets no PodDisruptionBudget" "${single}"
assert_absent "kind: PodDisruptionBudget" "the PodDisruptionBudget can be turned off" \
  "$(render --set podDisruptionBudget.enabled=false)"
assert_contains "kind: PodDisruptionBudget" "three replicas get a PodDisruptionBudget" \
  "$(render --set replicaCount=3)"

# ----- Shutdown -----

echo "==> a stopping replica has time to finish its requests"
assert_contains "terminationGracePeriodSeconds: 600$" \
  "the grace period is longer than the Kubernetes default" "${output}"
assert_contains 'value: "540"$' "the proxy waits for long requests with cleanup headroom" \
  "$(gateway_block "${gateway_deployment}" | grep -A1 'name: GRACEFUL_SHUTDOWN_TIMEOUT$')"
assert_absent 'name: GRACEFUL_SHUTDOWN_TIMEOUT$' "migration does not inherit the proxy shutdown budget" \
  "$(init_block "${gateway_deployment}")"
for setting in 1200:1080 30:27 1:0.9 0:0; do
  pod_grace="${setting%%:*}"
  proxy_grace="${setting#*:}"
  shutdown_pod="$(render -s templates/deployment.yaml --set "terminationGracePeriodSeconds=${pod_grace}")"
  assert_contains "terminationGracePeriodSeconds: ${pod_grace}$" "the Pod keeps its configured budget" "${shutdown_pod}"
  assert_contains "value: \"${proxy_grace}\"$" "the proxy budget follows the Pod budget with cleanup headroom" \
    "$(gateway_block "${shutdown_pod}" | grep -A1 'name: GRACEFUL_SHUTDOWN_TIMEOUT$')"
done

# ----- Unpriced models -----

# A model missing from LiteLLM's price table costs zero, and budget checks are
# skipped entirely for zero-cost models, so a budget on a key silently does
# nothing. Azure OpenAI deployment names are not in that table, so this is the
# ordinary case. Measured against a real deployment: a $0.0001 budget allowed
# every request until this setting was turned on.
echo "==> unpriced models are blocked by default"
assert_contains "block_requests_for_models_without_pricing: true" \
  "unpriced models are refused unless the operator opts out" "${output}"

unpriced_allowed="$(render --set blockUnpricedModels=false)"
assert_absent "block_requests_for_models_without_pricing" \
  "the block can be turned off deliberately" "${unpriced_allowed}"

# ----- Price table -----

# LiteLLM downloads its price table from GitHub whenever it is imported, which
# the proxy does at start and the migrate step does to build its indexes, and
# falls back to the copy in the image when that fails. priceTable: bundled keeps
# both to that copy, for a cluster without internet access or prices pinned to
# the image.
echo "==> the price table is downloaded unless it is pinned to the image"
assert_absent "LITELLM_LOCAL_MODEL_COST_MAP" "LiteLLM downloads the current price table by default" "${output}"
assert_absent "LITELLM_LOCAL_MODEL_COST_MAP" "priceTable=remote downloads the current price table" \
  "$(render --set-string priceTable=remote)"
bundled_pod="$(render --set-string priceTable=bundled -s templates/deployment.yaml)"
assert_contains 'value: "True"' "priceTable=bundled keeps the proxy to the image's price table" \
  "$(gateway_block "${bundled_pod}" | grep -A1 'name: LITELLM_LOCAL_MODEL_COST_MAP$')"
assert_contains 'value: "True"' "priceTable=bundled keeps the migrate step to the image's price table" \
  "$(init_block "${bundled_pod}" | grep -A1 'name: LITELLM_LOCAL_MODEL_COST_MAP$')"
assert_rejected "an unknown price table source" --set-string priceTable=local

# ----- Token limits -----

# LiteLLM reserves a request's input plus its maximum output tokens against a
# tokens-per-minute limit unless told not to. OpenClaw asks for the model's
# maximum output with every request, so under a small limit a long conversation
# was refused in every minute. The chart counts finished requests instead.
echo "==> token limits count finished requests unless reservation is on"
assert_contains 'value: "false"' "tokens per minute count finished requests by default" \
  "$(gateway_block "$(render -s templates/deployment.yaml)" | grep -A1 'name: LITELLM_TPM_TOKEN_RESERVATION_ENABLED$')"
assert_contains 'value: "true"' "tpmReservation=true reserves tokens before calling the model" \
  "$(gateway_block "$(render --set tpmReservation=true -s templates/deployment.yaml)" | grep -A1 'name: LITELLM_TPM_TOKEN_RESERVATION_ENABLED$')"
assert_rejected "a token limit mode that is not a boolean" --set-string tpmReservation=yes

# ----- Migrations -----

# Migrations run in an init container of every Gateway pod, and the image makes
# pods take turns. A pre-install hook, the earlier design, runs before the
# release's own resources exist, so it cannot reach the bundled PostgreSQL; and
# when it referenced the chart's ServiceAccount or Secret the install hung. No
# hook is rendered at all.
echo "==> migrations run in an init container, not a hook"
init="$(init_block "${gateway_pod}")"
assert_contains "name: migrate" "the Gateway pods migrate before starting" "${init}"
assert_contains "\- migrate$" "the init container runs the image's migrate command" "${init}"
assert_contains "name: DATABASE_URL" "the migrate init container gets the database connection" "${init}"
assert_absent "LITELLM_SALT_KEY" "the migrate init container gets no credential it does not need" "${init}"
assert_absent "LITELLM_MASTER_KEY" "the migrate init container gets no credential it does not need" "${init}"
assert_contains "DISABLE_SCHEMA_UPDATE" "proxies do not migrate on startup" "${output}"
assert_absent "helm.sh/hook" "no Helm hook is rendered" "${output}"
assert_absent "helm.sh/hook" "no Helm hook is rendered with external dependencies" "${external}"

migrations_off="$(render --set migrations.enabled=false)"
assert_absent "DISABLE_SCHEMA_UPDATE" "proxies migrate when the init container is disabled" "${migrations_off}"
assert_absent "initContainers:" "no migrate init container when disabled" \
  "$(render --set migrations.enabled=false -s templates/deployment.yaml)"

# ----- Bundled PostgreSQL and Valkey -----

echo "==> PostgreSQL and Valkey are bundled by default"
assert_contains "kind: StatefulSet" "the bundled PostgreSQL is a StatefulSet" "${output}"
assert_contains "volumeClaimTemplates:" "the bundled PostgreSQL keeps its data on a volume" "${output}"
# The PostgreSQL image declares its data directory a volume. Mounted anywhere
# else, the claim sat unused while the runtime gave that path an anonymous
# volume, discarded with each container: every restart of the pod began from an
# empty database. Only an install-level test showed it; this keeps the mount
# where the data actually goes, and on a subdirectory that initdb accepts.
postgresql="$(render -s templates/postgresql.yaml)"
assert_contains "mountPath: /var/lib/postgresql/data$" \
  "the claim is mounted at the image's declared data directory" "${postgresql}"
assert_contains "subPath: data$" \
  "the data sits in a subdirectory, so a formatted disk's lost+found does not stop initdb" "${postgresql}"
assert_contains "image: mcr.microsoft.com/azurelinux/base/postgres:" "PostgreSQL comes from MCR" "${output}"
assert_contains "image: mcr.microsoft.com/azurelinux/base/valkey:" "Valkey comes from MCR" "${output}"
assert_contains "\- --requirepass" "the bundled Valkey requires a password" "${output}"
db_ref="$(grep -A4 'name: DATABASE_URL' <<< "${output}")"
assert_contains "name: ${postgresql_name}$" "the proxy connects to the bundled PostgreSQL" "${db_ref}"
assert_contains "key: database-url" "the connection URL comes from the bundled PostgreSQL's Secret" "${db_ref}"

echo "==> an external database and Redis replace the bundled ones"
assert_absent "kind: StatefulSet" "no bundled PostgreSQL with an external database" "${external}"
assert_absent "component: valkey" "no bundled Valkey with an external Redis" "${external}"
assert_contains "name: lint-database" "the administrator's database Secret is referenced" "${external}"

# The bundled pods share the release's name and instance labels. Selecting the
# Gateway by those alone put the PostgreSQL and Valkey pods behind the Gateway
# Service, so a share of API traffic went to them. Each pod template's labels
# are compared with the Service selector.
echo "==> the Gateway Service selects only the Gateway pods"
selector="$(render -s templates/service.yaml | awk '/^  selector:/{f=1; next} f && /^    /{sub(/^ +/, ""); print; next} f{f=0}')"
[ -n "${selector}" ] || { echo "FAIL: the Gateway Service has no selector" >&2; exit 1; }
matching=0
while IFS= read -r template; do
  [ -n "${template}" ] || continue
  labels="$(render -s "templates/${template}" | awk '
    /^  template:/{t=1} t && /^      labels:/{l=1; next}
    l && /^        [^ ]/{sub(/^ +/, ""); print; next} l{l=0; t=0}')"
  all=true
  while IFS= read -r pair; do
    grep -qxF -- "${pair}" <<< "${labels}" || all=false
  done <<< "${selector}"
  if [ "${all}" = "true" ]; then
    [ "${template}" = "deployment.yaml" ] || { echo "FAIL: the Gateway Service also selects the pods of ${template}" >&2; exit 1; }
    matching=$((matching + 1))
  fi
done < <(printf '%s\n' deployment.yaml postgresql.yaml valkey.yaml)
[ "${matching}" -eq 1 ] || { echo "FAIL: the Gateway Service does not select the Gateway pods" >&2; exit 1; }

# The bundled PostgreSQL's volume outlives the release, and PostgreSQL keeps the
# password it was initialised with. A password regenerated on upgrade or
# reinstall would lock the Gateway out of its own data, so the Secret is read
# back and kept, like the salt key. lookup returns nothing under helm template,
# so this checks the source; the cluster test checks the behaviour.
echo "==> the bundled dependencies' passwords are read back and kept"
dependency_secrets="$(render -s templates/secret-dependencies.yaml)"
assert_contains "helm.sh/resource-policy: keep" "the dependency Secrets are kept on uninstall" "${dependency_secrets}"
dependency_template="${chart_dir}/templates/secret-dependencies.yaml"
if [ "$(grep -c 'lookup "v1" "Secret"' "${dependency_template}")" -lt 2 ]; then
  echo "FAIL: a dependency Secret is regenerated instead of read back; the next upgrade would change its password" >&2
  exit 1
fi

# The bundled PostgreSQL is a StatefulSet, whose pods carry the label
# controller-revision-hash=<name>-<hash>. The hash has up to ten characters and
# a label value at most 63, so above 52 characters Kubernetes creates no pod:
# measured on 1.37, none from 54 characters, and 53 only while the hash had
# nine. The Gateway then waits for its database indefinitely. Long release
# names are shortened to fit, with a hash that keeps two of them apart.
echo "==> long release names still fit the bundled PostgreSQL and Valkey"
long_release="release-name-long-enough-to-need-shortening"
long_one="$(helm template "${long_release}-one" "${chart_dir}" "${base_args[@]}")"
long_two="$(helm template "${long_release}-two" "${chart_dir}" "${base_args[@]}")"
pg_one="$(object_name StatefulSet postgresql "${long_one}")"
pg_two="$(object_name StatefulSet postgresql "${long_two}")"
valkey_one="$(object_name Service valkey "${long_one}")"
valkey_two="$(object_name Service valkey "${long_two}")"
for name in "${pg_one}" "${pg_two}" "${valkey_one}" "${valkey_two}"; do
  single_name "${name}" "a bundled resource of a long release name"
done
for name in "${pg_one}" "${pg_two}"; do
  [ -n "${name}" ] && [ "${#name}" -le 52 ] || {
    echo "FAIL: the bundled PostgreSQL is named '${name}' (${#name} characters); above 52 Kubernetes creates no pod for it" >&2
    exit 1
  }
done
for name in "${valkey_one}" "${valkey_two}"; do
  [ -n "${name}" ] && [ "${#name}" -le 63 ] || {
    echo "FAIL: the bundled Valkey is named '${name}' (${#name} characters), more than a Service name allows" >&2
    exit 1
  }
done
if [ "${pg_one}" = "${pg_two}" ] || [ "${valkey_one}" = "${valkey_two}" ]; then
  echo "FAIL: two long release names sharing a prefix get the same bundled resource names" >&2
  exit 1
fi
assert_contains "name: ${pg_one}$" "the Gateway connects to the shortened PostgreSQL name" \
  "$(grep -A4 'name: DATABASE_URL' <<< "${long_one}")"
assert_contains "redis_host: \"${valkey_one}\"" "the Gateway connects to the shortened Valkey name" "${long_one}"

# Names that fit are never shortened. The data volume's claim is named after
# the StatefulSet, so renaming it in an upgrade would start an empty database
# and leave the data behind on the old claim.
default_names="$(helm template gateway "${chart_dir}" "${base_args[@]}")"
if [ "$(object_name StatefulSet postgresql "${default_names}")" != "gateway-cetirun-ai-gateway-postgresql" ] \
  || [ "$(object_name Service valkey "${default_names}")" != "gateway-cetirun-ai-gateway-valkey" ]; then
  echo "FAIL: the default release's bundled resources were renamed; an upgrade would leave the data on the old volume" >&2
  exit 1
fi

# Under helm template, which GitOps tools such as Argo CD use, lookup returns
# nothing, so a generated Valkey password changed with every render. Neither
# Valkey nor the Gateway restarts when only that Secret changes, so the next
# restart of just one of them left the two with different passwords.
# redis.passwordSecret serves the bundled Valkey too, and the documented GitOps
# configuration renders the same way every time.
echo "==> the bundled Valkey can take its password from an existing Secret"
valkey_env="$(render -s templates/valkey.yaml | grep -A4 'name: VALKEY_PASSWORD')"
assert_contains "name: ${valkey_name}$" "by default the bundled Valkey uses the generated password" "${valkey_env}"
supplied=(--set-string "redis.passwordSecret.existingSecret=lint-redis")
valkey_env="$(render "${supplied[@]}" -s templates/valkey.yaml | grep -A4 'name: VALKEY_PASSWORD')"
assert_contains "name: lint-redis$" "the bundled Valkey reads a supplied password Secret" "${valkey_env}"
assert_contains "key: redis-password$" "the bundled Valkey reads the supplied Secret's key" "${valkey_env}"
redis_env="$(render "${supplied[@]}" -s templates/deployment.yaml | grep -A4 'name: REDIS_PASSWORD')"
assert_contains "name: lint-redis$" "the Gateway reads the same supplied Secret as Valkey" "${redis_env}"
assert_absent "component: valkey" "no Valkey password is generated when one is supplied" \
  "$(render "${supplied[@]}" -s templates/secret-dependencies.yaml)"
gitops_args=(
  "${supplied[@]}"
  --set-string "auth.masterKey.existingSecret=lint-auth"
  --set-string "auth.saltKey.existingSecret=lint-auth"
  --set-string "auth.adminUI.passwordSecret.existingSecret=lint-auth"
  --set "postgresql.bundled.enabled=false"
  --set-string "postgresql.urlSecret.existingSecret=lint-database"
)
if [ "$(render "${gitops_args[@]}")" != "$(render "${gitops_args[@]}")" ]; then
  echo "FAIL: the documented GitOps configuration renders differently each time; every sync would change a credential" >&2
  exit 1
fi

# ----- Credentials -----

echo "==> platform credentials"
assert_contains "LITELLM_MASTER_KEY" "master key is passed" "${output}"
assert_contains "LITELLM_SALT_KEY" "salt key is passed" "${output}"
assert_contains "salt-key:" "a salt key is generated when none is supplied" "${output}"

# LiteLLM accepts the master key as the console password when no UI password is
# set, which turns a console sign-in into full management API access.
assert_contains "UI_PASSWORD" "the console has its own password" "${output}"

ui_off="$(render --set auth.adminUI.enabled=false)"
assert_contains "DISABLE_ADMIN_UI" "the console is marked disabled when turned off" "${ui_off}"
assert_absent "UI_PASSWORD" "no console password when the console is off" "${ui_off}"

# An administrator-supplied Secret must be referenced, not copied.
byo="$(render --set-string "auth.saltKey.existingSecret=my-salt" --set-string "auth.saltKey.key=custom")"
assert_contains "name: my-salt" "an existing salt Secret is referenced" "${byo}"
assert_absent "salt-key:" "the chart does not generate a salt key when one is supplied" "${byo}"

# Provider keys reach the proxy as environment variables, read once at startup.
# Changing an inline value with helm upgrade updated the chart's Secret while
# both pods went on sending the old key, so the pods must roll with it.
echo "==> changing an inline provider key rolls the pods"
inline_one="$(render --set-string "providerCredentials.inline.LINT_KEY=one" | grep 'checksum/providers:' || true)"
inline_two="$(render --set-string "providerCredentials.inline.LINT_KEY=two" | grep 'checksum/providers:' || true)"
if [ -z "${inline_one}" ] || [ "${inline_one}" = "${inline_two}" ]; then
  echo "FAIL: a changed inline provider key does not change the pod template" >&2
  exit 1
fi
assert_absent "checksum/providers" "no provider checksum without inline credentials" "${output}"

# ----- Salt key preservation -----

# LiteLLM has no rotation path for the salt key. When it changes, the proxy
# starts normally and reports healthy while every model stored in the database
# silently disappears; restoring the original value brings them back. So a chart
# that regenerates it on upgrade breaks the install unrecoverably, and readiness
# probes do not notice.
#
# This is a source assertion, not a behaviour test: `lookup` returns nothing
# under `helm template`, so preservation across a real upgrade can only be
# verified against a cluster. What this catches is the generation branch being
# reached without first reading the existing value back.
echo "==> the salt key is read back rather than regenerated"
secret_template="${chart_dir}/templates/secret.yaml"
if ! grep -q 'lookup "v1" "Secret"' "${secret_template}"; then
  echo "FAIL: secret.yaml no longer looks up the existing Secret; generated values would change on every upgrade" >&2
  exit 1
fi
if ! grep -A4 'salt-key' "${secret_template}" | grep -q 'b64dec'; then
  if ! grep -B6 'salt-key:' "${secret_template}" | grep -q 'b64dec'; then
    echo "FAIL: the salt key branch does not read an existing value back" >&2
    exit 1
  fi
fi

# PostgreSQL outlives the release. If helm uninstall removed the auth Secret,
# the next install generated a new salt key against the old database, and every
# model added through the console silently disappeared. The cluster test checks
# the behaviour; this catches the annotation going missing before it gets there.
echo "==> the auth Secret survives helm uninstall"
auth_secret="$(render -s templates/secret.yaml)"
assert_contains "helm.sh/resource-policy: keep" "the auth Secret is kept on uninstall" "${auth_secret}"

# ----- Request log and metrics -----

# LiteLLM writes no per-request record of its own. The image's callback writes
# one JSON line of metadata per call to stdout, for the cluster's log
# collection. It has no setting that adds request or response content. The
# image's entrypoint registers it, not LiteLLM's configuration: a configured
# callback depends on how LiteLLM combines its configuration with settings
# saved in its database, and an image without it cannot start with a
# configuration that names it.
echo "==> the stdout request log records each call without content"
assert_contains 'value: "true"' "the request log is turned on by default" \
  "$(grep -A1 'name: CETIRUN_REQUEST_LOG$' <<< "${gateway_pod}")"
assert_absent "cetirun_ai_gateway" "the request log callback stays out of LiteLLM's configuration" "${output}"
assert_absent "callbacks:" "LiteLLM's configuration lists no callback without metrics" "${output}"
assert_absent "CETIRUN_REQUEST_LOG_CONTENT" "no setting adds request or response content to stdout" "${output}"
log_off="$(render --set requestLog.enabled=false)"
assert_absent "CETIRUN_REQUEST_LOG" "the request log can be turned off" "${log_off}"

# A client sending "no-log": true turned every logging callback off for its own
# call, so the call never reached the request log or the metrics. The setting
# that ignores the field holds whatever is turned on.
echo "==> callers cannot turn recording off"
assert_contains "global_disable_no_log_param: true" "a caller cannot turn recording off" "${output}"
assert_contains "global_disable_no_log_param: true" "a caller cannot turn recording off with the request log off" \
  "${log_off}"

# With the database spend log disabled, the request log keeps the per-call
# metadata outside the database.
# The entrypoint turns it off only together with the request log, so an image
# without the request log keeps it, and never in LiteLLM's configuration, where
# a saved settings block would keep it off after a rollback.
echo "==> LiteLLM's spend log is turned off only together with the request log"
assert_absent "CETIRUN_SPEND_LOGS" "LiteLLM keeps its spend log by default" "${output}"
spend_off="$(render --set spendLogs.enabled=false --set spendLogs.storePrompts=false)"
assert_contains 'value: "false"' "spendLogs.enabled=false has the entrypoint stop the spend log" \
  "$(grep -A1 'name: CETIRUN_SPEND_LOGS$' <<< "${spend_off}")"
assert_absent "disable_spend_logs" "the spend log setting stays out of LiteLLM's configuration" "${spend_off}"
assert_rejected "the spend log off without the request log" \
  --set spendLogs.enabled=false --set spendLogs.storePrompts=false --set requestLog.enabled=false

# The spend log grows by a row per call. LiteLLM deletes older rows, and the
# stored health check results with them, on each replica one interval after it
# starts and every interval after that. At LiteLLM's default interval, a day,
# pods replaced sooner, as every configuration change replaces them, never
# deleted a row, so the chart sets an hour whenever a retention is set. With
# the spend log off there are still health check results and older rows to
# delete. Stored prompts and responses follow the same row retention.
echo "==> LiteLLM's spend log is kept for seven days, and older rows are deleted every hour"
assert_contains 'maximum_spend_logs_retention_period: "7d"' "spend log rows are kept for seven days by default" "${output}"
assert_contains 'maximum_health_check_retention_period: "7d"' "health check results are kept as long" "${output}"
assert_contains 'maximum_spend_logs_retention_interval: "1h"$' "older rows are deleted every hour, not once a day" "${output}"
assert_contains 'maximum_spend_logs_retention_interval: "1h"$' "a single replica deletes every hour too" "${single}"
assert_contains 'maximum_spend_logs_retention_interval: "1h"$' \
  "with the spend log off, health check results and older rows are still deleted every hour" "${spend_off}"
retention_30d="$(render --set-string spendLogs.retention=30d)"
assert_contains 'maximum_spend_logs_retention_period: "30d"' "the retention can be changed" "${retention_30d}"
assert_contains 'maximum_spend_logs_retention_interval: "1h"$' "a longer retention is still applied every hour" \
  "${retention_30d}"
no_retention="$(render --set-string spendLogs.retention=)"
assert_absent "retention_period" "an empty retention keeps every row" "${no_retention}"
assert_absent "retention_interval" "nothing is scheduled for deletion without a retention" "${no_retention}"
assert_rejected "a retention LiteLLM cannot read" --set-string spendLogs.retention=week
assert_contains '^      store_prompts_in_spend_logs: true$' \
  "prompts and responses are stored by default" "${output}"

echo "==> prompt and response storage defaults on and can be disabled"
prompt_config="$(render -s templates/configmap.yaml)"
assert_contains '^      store_prompts_in_spend_logs: true$' \
  "the default reaches LiteLLM's general settings as a boolean" "${prompt_config}"
assert_contains '^      store_prompts_in_spend_logs: true$' "explicit true keeps content storage enabled" \
  "$(render --set spendLogs.storePrompts=true)"
assert_absent "disable_spend_logs" "prompt storage does not change spend-log enablement" "${prompt_config}"
assert_contains 'maximum_spend_logs_retention_period: "7d"' \
  "prompt storage keeps the configured retention" "${prompt_config}"
assert_contains '^      store_prompts_in_spend_logs: false$' "false explicitly disables content storage" \
  "$(render --set spendLogs.storePrompts=false)"
assert_contains '^      store_prompts_in_spend_logs: true$' "operator integration and metrics keep the default" \
  "$(render --set operatorIntegration=true --set metrics.enabled=true)"
assert_contains '^      store_prompts_in_spend_logs: false$' "operator integration and metrics preserve an explicit opt-out" \
  "$(render --set operatorIntegration=true --set metrics.enabled=true --set spendLogs.storePrompts=false)"
assert_contains '^      store_prompts_in_spend_logs: false$' "disabling spend logs also disables content storage" \
  "${spend_off}"
assert_contains '^      store_prompts_in_spend_logs: true$' "content storage does not require the stdout request log" \
  "$(render --set spendLogs.storePrompts=true --set requestLog.enabled=false)"
prompt_no_retention="$(render --set spendLogs.storePrompts=true --set-string spendLogs.retention=)"
assert_contains '^      store_prompts_in_spend_logs: true$' "empty retention keeps stored content indefinitely" \
  "${prompt_no_retention}"
assert_absent 'retention_period' "prompt storage does not force a retention period" "${prompt_no_retention}"
assert_rejected "prompt storage while the spend log is disabled" \
  --set spendLogs.enabled=false --set spendLogs.storePrompts=true
assert_rejected "default prompt storage while the spend log is disabled" --set spendLogs.enabled=false
for value in true false ''; do
  assert_rejected "a string instead of the prompt-storage boolean" --set-string "spendLogs.storePrompts=${value}"
done
for value in 1 '[]' '{}'; do
  assert_rejected "a non-boolean prompt-storage value" --set-json "spendLogs.storePrompts=${value}"
done
prompt_pod="$(render --set spendLogs.storePrompts=false -s templates/deployment.yaml)"
if [ "$(grep 'checksum/config:' <<< "${prompt_pod}")" = "$(grep 'checksum/config:' <<< "${gateway_pod}")" ]; then
  echo "FAIL: changing prompt storage does not roll the Gateway pods" >&2
  exit 1
fi
assert_absent "CETIRUN_REQUEST_LOG_CONTENT" "prompt storage never enables stdout body logging" "${prompt_pod}"
for value in true false; do
  if [ "$(render "${gitops_args[@]}" --set "spendLogs.storePrompts=${value}")" \
    != "$(render "${gitops_args[@]}" --set "spendLogs.storePrompts=${value}" --is-upgrade)" ]; then
    echo "FAIL: prompt storage is not stable across repeated install and upgrade renders" >&2
    exit 1
  fi
done
for template in configmap.yaml deployment.yaml; do
  [ "$(render --set spendLogs.storePrompts=null --is-upgrade -s "templates/${template}")" \
    = "$(render -s "templates/${template}")" ] || {
    echo "FAIL: missing spendLogs.storePrompts does not keep the default ${template}" >&2
    exit 1
  }
done

# Container Insights copies each container's environment, Secret values
# included, into its ContainerInventory table once a minute unless collection
# is turned off for the cluster or for the container. Every container here
# that holds a credential in its environment turns it off for itself.
echo "==> containers with credentials opt out of environment collection"
azmon_count="$(grep -c 'name: AZMON_COLLECT_ENV$' <<< "${output}" || true)"
[ "${azmon_count}" = "4" ] || {
  echo "FAIL: AZMON_COLLECT_ENV is set on ${azmon_count} containers; expected the gateway, migrate, PostgreSQL and Valkey containers" >&2
  exit 1
}
assert_contains "name: AZMON_COLLECT_ENV$" "the migrate init container opts out" "${init}"
for template in deployment.yaml postgresql.yaml valkey.yaml; do
  assert_contains 'value: "False"' "${template} opts out of environment collection" \
    "$(render -s "templates/${template}" | grep -A1 'name: AZMON_COLLECT_ENV$')"
done

# The metrics are LiteLLM's. Served on the Service port they need a key, which
# a scraper would have to hold; LiteLLM serves them on a port of their own,
# from a separate process that reads the samples the proxy leaves in a shared
# directory.
echo "==> metrics are served on their own port when turned on"
assert_absent "prometheus" "no metrics by default" "${output}"
assert_absent "name: metrics" "no metrics port by default" "${gateway_pod}"
metrics="$(render --set metrics.enabled=true)"
metrics_pod="$(render --set metrics.enabled=true -s templates/deployment.yaml)"
assert_contains 'callbacks: \["prometheus"\]' "LiteLLM's Prometheus callback is registered" "${metrics}"
assert_contains 'prometheus_exclude_labels: \["client_ip","user_agent","user_email"\]' \
  "labels that change per client or carry email addresses are left out" "${metrics}"
assert_contains "\- --prometheus_metrics_port$" "LiteLLM serves the metrics on a port of their own" "${metrics_pod}"
assert_contains "containerPort: 9090$" "the metrics port is declared" "${metrics_pod}"
assert_contains "name: PROMETHEUS_MULTIPROC_DIR$" "the proxy records samples where the metrics process reads them" "${metrics_pod}"
assert_contains "emptyDir: {}" "the sample directory is the pod's own" "${metrics_pod}"
assert_absent "9090" "the metrics port stays off the Service" "$(render --set metrics.enabled=true -s templates/service.yaml)"
assert_absent "kind: PodMonitor" "no PodMonitor unless asked for" "${metrics}"

# A PodMonitor on a cluster without its API fails the whole install, and the
# platform asks for one by default. It is created only where the API exists:
# Azure Monitor managed Prometheus serves azmonitoring.coreos.com.
echo "==> the PodMonitor is created only where the cluster serves its API"
monitor_args=(--set metrics.enabled=true --set metrics.podMonitor.enabled=true)
assert_absent "kind: PodMonitor" "no PodMonitor on a cluster without the API" "$(render "${monitor_args[@]}")"
monitor="$(render "${monitor_args[@]}" --api-versions azmonitoring.coreos.com/v1/PodMonitor -s templates/podmonitor.yaml)"
assert_contains "apiVersion: azmonitoring.coreos.com/v1$" "the PodMonitor targets Azure Monitor managed Prometheus by default" "${monitor}"
assert_contains "port: metrics$" "the PodMonitor scrapes the metrics port" "${monitor}"
assert_contains "app.kubernetes.io/component: gateway$" "the PodMonitor selects the Gateway pods only" "${monitor}"
assert_absent "kind: PodMonitor" "the Prometheus Operator's API does not satisfy the Azure one" \
  "$(render "${monitor_args[@]}" --api-versions monitoring.coreos.com/v1/PodMonitor)"
assert_contains "apiVersion: monitoring.coreos.com/v1$" "the Prometheus Operator's PodMonitor can be chosen" \
  "$(render "${monitor_args[@]}" --set-string metrics.podMonitor.apiVersion=monitoring.coreos.com/v1 \
    --api-versions monitoring.coreos.com/v1/PodMonitor -s templates/podmonitor.yaml)"
assert_rejected "a PodMonitor without metrics" --set metrics.podMonitor.enabled=true

# The proxy listens on service.port. A Gateway that already listened on 9090
# kept failing its upgrade once the platform turned metrics on at 9090; the
# metrics take 9091 then, and only a port set by hand can collide.
echo "==> the metrics port never takes the proxy's"
moved="$(render --set metrics.enabled=true --set service.port=9090 -s templates/deployment.yaml)"
assert_contains "containerPort: 9091$" "the metrics move to 9091 when the proxy listens on 9090" "${moved}"
assert_contains '"9091"' "LiteLLM serves the metrics on 9091 then" \
  "$(grep -A1 -- '- --prometheus_metrics_port$' <<< "${moved}")"
assert_contains "containerPort: 9100$" "a metrics port set by hand is used" \
  "$(render --set metrics.enabled=true --set metrics.port=9100 -s templates/deployment.yaml)"
assert_rejected "the metrics port on the Service port" --set metrics.enabled=true --set metrics.port=4000

# helm upgrade --reuse-values keeps the values of the chart version a release
# was installed with, so a release from before the request log, metrics,
# router and price table settings has none of them; reading them directly
# failed the upgrade with a nil pointer. Without them, the chart renders as
# with their defaults. Setting a map to null removes it, as --reuse-values
# leaves it.
echo "==> a release from before these settings upgrades with --reuse-values"
reuse_args=(--set metrics=null --set requestLog=null --set spendLogs=null --set router=null
  --set budgetExceededStatusCode=null --set podDisruptionBudget=null --set priceTable=null)
for template in deployment.yaml configmap.yaml poddisruptionbudget.yaml; do
  [ "$(render "${reuse_args[@]}" -s "templates/${template}")" = "$(render -s "templates/${template}")" ] || {
    echo "FAIL: without the request log, metrics, router, budget status, disruption budget and price table settings, ${template} differs from its default render" >&2
    exit 1
  }
done
for template in configmap.yaml deployment.yaml; do
  for setting in intervalSeconds concurrency; do
    [ "$(render --set "router.healthChecks.${setting}=null" -s "templates/${template}")" \
      = "$(render -s "templates/${template}")" ] || {
      echo "FAIL: without router.healthChecks.${setting}, ${template} differs from its default render" >&2
      exit 1
    }
  done
done
[ "$(render --set metrics.enabled=true --set metrics.podMonitor=null -s templates/deployment.yaml)" \
  = "$(render --set metrics.enabled=true -s templates/deployment.yaml)" ] || {
  echo "FAIL: without the PodMonitor settings, the Deployment differs from its default render" >&2
  exit 1
}

# ----- Exposure -----

# The console is reached through a port-forward. A chart that ships an Ingress
# for an admin console eventually ships one by accident.
echo "==> the chart creates no Ingress"
assert_absent "kind: Ingress" "no Ingress is rendered" "${output}"

echo "All Gateway chart render assertions passed."

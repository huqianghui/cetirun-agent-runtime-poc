# CetiRun AI Gateway

The CetiRun AI Gateway is based on LiteLLM. It gives AI agents a single endpoint
for model access.

You can install this chart on its own. It has no dependency on the CetiRun
operator, portal, or agent chart. As a result, an administrator who already has
their own agents can install it and configure provider keys. The administrator
can then configure those agents to use the installed Gateway.

## Install

The installation is one command. With the default values, the chart includes
its own PostgreSQL and Valkey. As a result, the only inputs are the models and
their keys:

```bash
kubectl create secret generic gateway-provider-keys \
  --from-literal=AZURE_PROD_KEY='...' \
  --from-literal=ANTHROPIC_KEY='...'

helm install gateway oci://<registry>/helm/cetirun-ai-gateway --version <version> \
  --set global.registry=<registry> -f my-values.yaml
```

This is a minimal `my-values.yaml`:

```yaml
models:
  - model_name: gpt-4o
    litellm_params:
      model: azure/my-gpt-4o-deployment
      api_base: https://example.openai.azure.com/
      api_key: os.environ/AZURE_PROD_KEY
      api_version: "2025-04-01-preview"
    model_info:
      base_model: azure/gpt-4o

providerCredentials:
  existingSecret: gateway-provider-keys
```

`<registry>` is the registry that you publish the chart and its image to.
`<version>` is their version. Use a clone of the repository on a machine that
has Docker Buildx, Helm, and Python 3. After you sign in to the registry with
`docker login` and `helm registry login`, build and push both:

```bash
REGISTRY=<registry> PUSH=true bash scripts/ai-gateway-docker-build.sh
REGISTRY=<registry> bash scripts/ai-gateway-helm-release.sh
```

Both scripts take `<version>` from the release version of the repository,
unless `VERSION` is set. The chart runs the image of its own version.
`make publish REGISTRY=<registry> ACCEPT_VSCODE_SERVER_LICENSE_TERMS=true`
publishes them together with every other CetiRun image and chart, the VS Code
Server image included. The master key, the salt key, and the console password
are generated. Refer to [Credentials](#credentials). To call the Gateway, refer
to [Calling the Gateway](#calling-the-gateway).

The model list can be empty. Standalone Gateways can add models through the
console later. The models for shared-model grants in a CetiRun AgentPolicy must
be configured in the chart, with `operatorIntegration` enabled. The section that
follows describes this.

The cluster must meet two conditions before the installation:

- The bundled PostgreSQL claims a 100Gi `ReadWriteOnce` volume from the
  default StorageClass. If the cluster has no default StorageClass, set
  `postgresql.bundled.persistence.storageClass`. Without the volume, neither
  the database nor the Gateway starts.
- The cluster can pull `<registry>/cetirun-ai-gateway`. On AKS, attach the
  registry to the cluster. For other registries, set `imagePullSecrets`.

The commands install into the namespace of the current context. To use another
namespace, add `-n <namespace>` to each command.

In practice, the last two lines of the model entry are not optional. Without a
price, the model is refused. For newer Azure models, an older `api_version`
serves plain chat but fails every tool call that an agent makes. Refer to
[Budgets and unpriced models](#budgets-and-unpriced-models) and
[Azure OpenAI](#azure-openai).

### CetiRun AgentPolicy model bindings

With `operatorIntegration: true`, each model that is configured in the chart
gets a `cetirun-model-<name hash>` access group and a
`cetirun-shared-<name hash>-<configuration hash>` deployment ID. The name hash
is the first 16 hexadecimal characters of the SHA-256 hash of `model_name`. The
configuration hash uses the same method on the canonical JSON of the complete
model object, before those generated fields are added. Entries that have the
same `model_name`, for example load-balanced deployments of one model, share
the group and keep different IDs. Access groups that already exist and are not
reserved stay unchanged, and provider parameters that already exist also stay
unchanged. The chart rejects a supplied `model_info.id` or access group that
uses the reserved `cetirun-model-` prefix.

Agent keys are granted the group. The group follows the model name, which is
the identity that an AgentPolicy grants. If you change the endpoint, provider
parameters, price, or other metadata of a configured model, only its deployment
ID changes. Agents keep their grant. The Operator does not update their keys,
and it does not hold policy-bound Agents for this change. Changes that reach an
Agent's own configuration still update it as before:

- A change to the token limits of a model.
- The removal of a model without a price from the list, while the Gateway
  blocks unpriced models.

If you rotate the value of a referenced Secret, neither the ID nor the group
changes. If you add or remove a model name, this adds or removes a group. When
the Operator's catalog refresh sees this change, the Operator updates the keys
of Agents with shared models. It re-projects only the policy-bound Agents whose
AgentPolicy allows that name.

A name that a policy does not allow changes nothing for the Agents of that
policy. Before, any new name re-projected every policy-bound Agent. This held a
running native Sandbox Agent with a single-start record until the Agent was
recreated (measured on a Kubernetes 1.37 cluster).

The Operator trusts a group only while every deployment that carries the group
meets these conditions:

- It is configured in this chart and not added through the console.
- It has a unique deployment ID of the form above.
- It serves the model name that the group is derived from.

A console deployment under the same name is not automatically trusted or
granted. If a console deployment copies the group, the Operator distrusts it.
Agents then lose that model until the copy is removed. Two identical entries
render the same deployment ID. If the Gateway lists this ID twice, the Operator
does not trust the group.

Earlier versions of this chart derived the group from the complete model
configuration. As a result, a change to a model changed its group. The model
was not available to Agents until the Operator granted the new group. An
upgrade changes every group one time. Upgrade this chart together with the
Operator, as the platform chart does. The reason is that the Operator trusts
only groups of the current form.

When the Operator's catalog refresh sees the new groups, the Operator updates
the keys of Agents with shared models. It also re-projects policy-bound Agents
one time. The effect of this re-projection on a running native Sandbox Agent
depends on the record that the Agent was started with. If the Operator's common
executor started the Agent, the Agent restarts one time. If the Agent still has
a single-start record, it stays held until it is recreated.

The results that follow were measured on a Kubernetes 1.37 cluster. An OpenClaw
Agent that was started before the common executor stayed held after the
upgrade. Agents that the common executor started, OpenClaw and Hermes,
restarted one time for each re-projection. Hermes restarted in about two to
three minutes. OpenClaw restarted in about eight to ten minutes.

This behavior is opt-in. When `operatorIntegration` is false, standalone
Gateways keep their model list and custom deployment IDs unchanged. The
platform umbrella chart already enables this integration. No other platform
value or image is necessary.

## Requirements

**PostgreSQL** holds virtual keys, spend records, and any model that is added
through the admin console.

**Redis or Valkey** is necessary when `replicaCount` is above 1, and the chart
rejects that combination without it. This is not an option for speed.
Otherwise, each replica keeps its own copy of every key budget and rate limit.
As a result, the effective limit is multiplied by the replica count. In a
measurement against LiteLLM, a `$0.10` budget allowed `$0.20` of spend across
two replicas. When Redis was added, the figure was correct again.

A configuration without Redis does not affect the records. In a measurement
without Redis, spend totals, spend logs, and daily usage aggregates were
complete across replicas. Only enforcement failed. When Redis is configured,
the records have their own gaps. Refer to [Known gaps](#known-gaps).

Redis contains no durable data. The record of spend is in PostgreSQL. Redis
holds only in-flight coordination. As a result, persistence, backup, and
replication are not necessary for Redis.

[Availability](#availability) describes what the Gateway does when PostgreSQL
or Redis is not available.

### Bundled or your own

By default, both are bundled: a single PostgreSQL on a persistent volume, and a
single password-protected Valkey. Both come from the Azure Linux images on MCR.
This is enough for evaluation, demos, and small installations.

If you must have backups or high availability, use managed services instead:

```yaml
postgresql:
  bundled:
    enabled: false
  urlSecret:
    existingSecret: gateway-database   # key database-url: postgresql://...

redis:
  bundled:
    enabled: false
  host: my-redis.example.com
  passwordSecret:
    existingSecret: gateway-redis      # key redis-password
```

The chart cannot connect to Redis with TLS yet. As a result, it cannot use
Azure Cache for Redis with its default settings, which accept only TLS
connections. Refer to [Known gaps](#known-gaps).

`make deploy` of the CetiRun platform does not accept this configuration. It
installs the Gateway with the bundled PostgreSQL and Valkey, and it refuses
`postgresql.bundled.enabled: false` or `redis.bundled.enabled: false`. To use
managed services with CetiRun, install the platform chart directly. Refer to
[Use the component chart directly](../../cetirun-platform/README.md#use-the-component-chart-directly).

The bundled PostgreSQL behaves like an external database across the lifecycle
of the release. `helm uninstall` keeps its volume and its password Secret. As a
result, an installation under the same release name finds the data again. In a
measurement, a model that was added through the console before the
uninstallation was served after it.

To discard the data, manually delete the resources that `helm uninstall` left.
These are the volume claim, the `-auth` and `-postgresql` Secrets, and the
`-valkey` Secret when the chart generated one. All of them carry the instance
label of the release:

```bash
kubectl get pvc,secret -l app.kubernetes.io/instance=<release>
```

For a release with the name `gateway`, the claim is
`data-gateway-cetirun-ai-gateway-postgresql-0`. With a long release name, the
names of the bundled resources are shortened and contain a hash instead.

The size and the storage class of the volume are fixed at installation. The
default size is 100Gi because the database keeps the spend log of every agent
that uses the Gateway, for `spendLogs.retention`. As a result, the database
grows with the number of agents and with how much they run.

The volume claim template of a StatefulSet cannot be changed. As a result,
Kubernetes refuses an upgrade that changes `postgresql.bundled.persistence.size`
or `storageClass`. In a measurement, a change from 8Gi to 16Gi failed with
`spec.volumeClaimTemplates`. The release stayed marked as failed until an
upgrade with the original values restored it.

To make the database larger, expand the claim itself. Keep `persistence.size`
as it was. This works when the storage class of the claim allows expansion
(`allowVolumeExpansion: true`), as the AKS built-in classes do:

```bash
kubectl patch pvc data-gateway-cetirun-ai-gateway-postgresql-0 \
  -p '{"spec":{"resources":{"requests":{"storage":"200Gi"}}}}'
```

Local connections inside the PostgreSQL container are trusted, as in the
upstream image. Connections from other pods must supply the password.
Measurements show that a wrong password from another pod is refused, and that
Valkey refuses a client without one.

### Resources

The chart sets no resource requests or limits: `resources` for the Gateway,
`postgresql.bundled.resources`, and `redis.bundled.resources` are empty. As a
result, Kubernetes reserves no CPU or memory for these containers. When a node
runs out of memory, the kubelet first evicts the pods that use more memory
than they request. Set these values before you serve users.

Each Gateway pod runs one LiteLLM worker, and the image refuses more workers.
To scale, use `replicaCount`. For a pod with one worker, LiteLLM's
[production guide](https://docs.litellm.ai/docs/proxy/prod#machine-specifications)
gives 1 CPU and 4Gi of memory, as requests and as limits:

```yaml
resources:
  requests:
    cpu: "1"
    memory: 4Gi
  limits:
    cpu: "1"
    memory: 4Gi
```

The guide treats 4Gi as a minimum when the spend log stores prompts and
responses, as this chart does by default. In the guide, the memory of a pod
grows to fit the largest database write that the pod made, and it does not
decrease. The spend log rows with prompts and responses are the largest writes.
With the default `replicaCount: 2`, these values reserve 2 CPU and 8Gi of
memory.

A test installation had four Agents and little traffic. After 15 hours, each
Gateway container used about 800Mi of memory and less than 0.01 CPU. The
bundled PostgreSQL used about 200Mi, and Valkey used less than 10Mi. These
figures show the use at rest. They are not a recommendation.

Measure the use under your own traffic with `kubectl top pods --containers`.
Set each memory limit above the highest value that you see.

For larger installations, LiteLLM also gives
[Database Sizing](https://docs.litellm.ai/docs/proxy/db_sizing) and
[Redis Sizing](https://docs.litellm.ai/docs/proxy/redis_sizing). In the
database guide, a spend log row with the prompt and the response uses about
ten times more storage.

### Response caching is off

Budgets are shared across replicas only when the LiteLLM cache is backed by
Redis. As a result, the chart configures that cache. With its default setting,
the same cache also answers identical requests itself. These results were
observed on this chart before the fix:

- An identical request was served from Redis and not from the provider.
- The cache key is built from the content of the request. As a result, two
  callers that sent the same content got one shared answer. This answer was
  generated for the caller that asked first.
- If an agent retries after a bad answer, it gets the same answer back.

A cache hit costs nothing at the provider, and it is recorded as zero spend.
This record is correct, and it is not a leak. But a cache hit breaks the
expectation that each request reaches the model. It also breaks the isolation
between callers.

The chart sets `cache_params.mode: default_off`. This setting keeps the
coordination that is backed by Redis. It also makes response caching something
that a request must ask for. Both effects were confirmed: identical requests
reach the provider, and a budget still holds exactly across two replicas.

The `x-litellm-response-cost` header reports the full cost, also on a cache
hit. As a result, you cannot use this header to tell if a cache hit occurred.

## Models

The `models` value is LiteLLM's own `model_list`. It is passed through
unchanged, so the upstream provider documentation applies directly.

`model_name` is the name that a client asks for. `litellm_params.model` is the
value that the Gateway sends upstream. This value has a prefix that selects the
provider handler.

Do not put credentials in the model list. Use `os.environ/NAME` to refer to
them. Supply the value through `providerCredentials`. As a result, keys never
appear in rendered templates or in the `helm get values` output.

### Reserved names

Model names that start with `cetirun-` are rejected, because that prefix
belongs to models that the CetiRun operator registers automatically. With
`operatorIntegration` on, names that start with `copilot/` are also rejected.
In a tier, a CetiRun Team's router names an Agent's GitHub Copilot model as
`copilot/<model>`. A shared model with such a name is ambiguous there. A
standalone Gateway can continue to serve models under `copilot/`. Both prefixes
are matched in any letter case.

The console examines neither prefix. As a result, do not add models under
these prefixes in the console. The operator does not include such shared models
in the model list that it configures for Agents. It does the same for names
under `openai/`, which the agent chart refuses as a provider prefix. It also
does the same for names with whitespace or with more than 256 characters.
Shared models under other provider prefixes are listed like any other.

The reservation exists because a model that you add under a name that already
exists does not replace the original. Both are served under the same name, and
traffic is split between two different backends. Nothing that is visible to the
client shows this.

### Reasoning models

A request can set a reasoning effort, with `reasoning_effort` on Chat
Completions or `reasoning.effort` on the Responses API. If LiteLLM does not
identify the deployment as a reasoning model, it refuses such a request with a
400 (`UnsupportedParamsError`). This occurs before anything reaches the
provider. LiteLLM decides by name and by its price table. As a result, one more
setting can be necessary for a reasoning model under a name of your own. These
results were measured on LiteLLM 1.105.1, with made-up deployment names in
`litellm_params.model` and a mock provider:

| Deployment | Chat Completions | Responses API |
| --- | --- | --- |
| `azure/<deployment name>` | 400 | passed on |
| the same with `model_info.base_model: azure/gpt-5.4-nano` | passed on | passed on |
| the same with `model_info.supports_reasoning: true` | 400 | passed on |
| `openai/<name>` | passed on | 400 |
| the same with `model_info.base_model: gpt-5.4-nano` | passed on | 400 |
| the same with `model_info.supports_reasoning: true` | passed on | passed on |

For Azure OpenAI, set `base_model`. This setting also prices the deployment. On
Chat Completions, LiteLLM accepts a reasoning effort for an `azure/` deployment
only when its name or `base_model` names an o-series, GPT-5, or GPT-6 model.
`supports_reasoning` does not change that. `base_model` does not decide if a
chat request with tools goes to the Responses API. Refer to
[Azure OpenAI](#azure-openai).

For a reasoning model under `openai/` whose name is not in LiteLLM's price
table, set `model_info.supports_reasoning: true`. An example is a model behind
another OpenAI-compatible endpoint. `base_model` is not a substitute for this
setting. Neither setting is necessary for a deployment that has the name of its
model, for example `azure/gpt-5.4-nano` or `openai/gpt-5.4-nano`.

The chart does not set LiteLLM's `drop_params`. If it is set, each 400 becomes
a request that is sent without the reasoning effort, and nothing tells the
client.

### Images

LiteLLM reports for each deployment if its model accepts images, as
`supports_vision` in `/model/info`. It takes the value from its price table,
for the deployment's `model_info.base_model` or its `litellm_params.model`. A
value in the deployment's own `model_info` replaces the value from the table.
For a model that accepts images under a name that is not in the table, set
`model_info.supports_vision: true`. An example is a model behind another
OpenAI-compatible endpoint.

With `operatorIntegration`, the CetiRun operator reads this value. OpenClaw and
Hermes then send images with a request to the models that accept them. For the
other models, OpenClaw uses its own model catalog, and Hermes has a model
describe each image first. Refer to
[Serving CetiRun Agents](#serving-cetirun-agents).

## Budgets and unpriced models

LiteLLM calculates spend from a built-in price table. It multiplies the price
by the tokens that a request used. A model that is not in that table costs
zero, and **budget checks are skipped entirely for zero-cost models**. As a
result, a budget set on a virtual key silently does nothing.

This is not an edge case. An Azure OpenAI deployment name, for example
`my-gpt4-prod`, is not in the price table. The usual way to configure Azure is
to give the deployment a name. In a measurement against a real deployment, a
`$0.0001` budget allowed every request.

As a result, the chart sets `blockUnpricedModels: true`. This setting refuses
such a request with a message that names the model and tells you to declare
pricing. It is better to fail at the first request than to never enforce a
budget that somebody believes is in place.

The console can change the setting later. LiteLLM keeps that change in its
database, where it takes precedence over the value of the chart. Refer to
[Editing settings in the console](#editing-settings-in-the-console-takes-them-out-of-chart-control).
With CetiRun, the operator reads the setting that is in effect. It gives Agents
only the shared models that the Gateway serves.

### Pricing a deployment

You can price a deployment in two ways. Tests showed that both ways price the
requests that use a virtual key and enforce a budget on them.

**Name the underlying model**, when the deployment runs a model that LiteLLM
knows. The price comes from LiteLLM's own price table. As a result, you have no
figures to keep current:

```yaml
models:
  - model_name: gpt-4o
    litellm_params:
      model: azure/my-gpt4-prod
      api_base: https://example.openai.azure.com/
      api_key: os.environ/AZURE_KEY
      api_version: "2025-04-01-preview"
    model_info:
      base_model: azure/gpt-4o
```

**Declare prices explicitly**, when LiteLLM does not know the model, or when
your contract price differs from the list price. It is your task to keep these
figures current:

```yaml
    model_info:
      input_cost_per_token: 0.0000025
      output_cost_per_token: 0.00001
```

If you do neither, the model is refused.

### Check what LiteLLM actually holds

A model's price has more parts than the two prices that you set. Cached input
is billed at a discount, and cache writes and context size are tracked
separately. Explicit prices override only the fields that you set. When the
deployment name matches a model family in LiteLLM's price table, the other
fields come from that table. When the name matches nothing, LiteLLM logs a
warning that the cache prices default to zero. If the cache prices are zero,
cached input is free, and LiteLLM counts too little spend.

As a result, after you add a model, do not guess the values that LiteLLM uses.
Examine them with this command. `MASTER_KEY` holds the master key, as
[Calling the Gateway](#calling-the-gateway) shows:

```bash
curl -s http://localhost:4000/model/info -H "Authorization: Bearer $MASTER_KEY" \
  | jq '.data[] | {model_name, price: (.model_info | {input_cost_per_token,
         output_cost_per_token, cache_read_input_token_cost,
         cache_creation_input_token_cost, max_input_tokens})}'
```

If `cache_read_input_token_cost` is not set or is zero, set it to the
provider's cached-input price. For Azure OpenAI and OpenAI, set
`cache_creation_input_token_cost` equal to the input price. The reason is that
these providers do not charge separately for writes to the cache.

A measurement used a real Azure deployment with only its input and output
prices set. The cache-read price and a 922k-token context size came from
LiteLLM's price table. Cache writes were charged at the input price. The charge
for each request matched those rates exactly.

### When a budget runs out

Budgets are in US dollars. There is no token allowance. The Gateway refuses a
request over budget with HTTP 402 and `Budget has been exceeded!` by default
(`budgetExceededStatusCode`). LiteLLM's own default is 422 from version 1.105,
and 429 before that. The status decides what an agent does next. A measurement
with OpenClaw 2026.9.6 and Hermes v2026.9.24 against an exhausted key gave these
results:

| Status | OpenClaw | Hermes |
| --- | --- | --- |
| 402 | stops after one request: a billing error, the API key has no credits left | stops after one request: billing or credits exhausted |
| 422 | stops after one request: the provider rejected the request schema or tool payload | stops after one request: the request was rejected as malformed |
| 429 | 10 tries over two to two and a half minutes, then "API rate limit reached" | 3 tries, then asks the user to wait a minute and retry |

With 402, both agents stop immediately and say that no credit is left, which is
closest to the cause. With 429, the agents continue to retry, but the budget is
available again only at its reset. The message for 402 still refers to the
provider's billing or API key, and not to the Gateway's budget. As a result, a
user can look for the problem in the wrong place. To select another status, set
`budgetExceededStatusCode` to 422 or 429, because the chart refuses any other
value. Other agents and other versions were not measured.

### Key request limits

An AgentPolicy's `modelAccess.rateLimits` makes the CetiRun operator set
`max_parallel_requests`, `rpm_limit`, and `tpm_limit` on the key of every Agent
bound to the policy. It also makes the operator set the per-model
`model_rpm_limit` and `model_tpm_limit` on these keys. The operator updates
these keys in place and does not change other keys. A request over a limit gets
HTTP 429 with a `retry-after` header. Replicas share the counters through Redis.

In the default mode, `tpmReservation: false`, the token limit counts the tokens
of each finished request. It does not count cached input tokens. When the count
for a minute reaches the limit, requests are refused until the next minute.
LiteLLM's other mode, `tpmReservation: true`, reserves the input tokens and the
maximum output tokens of a request before LiteLLM calls the model.

With every request, OpenClaw asks for the maximum output of the model, which is
128,000 tokens for current GPT models. As a result, in that mode, a limit of
200,000 refused every request of an OpenClaw conversation after about 55,000
tokens of input. The reason is that the reservation alone was larger than the
limit. OpenClaw stopped after nine one-minute retries. These results were
measured on a test cluster. Refer to the
[AgentPolicy reference](../../../site/content/en/docs/reference/crds.md#agentpolicy-rate-limits).

A `budget_duration` of `30d` or `1mo` resets on the first day of each month,
UTC, and not thirty days after the key was created. A background job that runs
about every ten minutes applies the resets. As a result, a key can stay refused
for up to this interval after its reset time. In a measurement with a
one-minute budget, the spend was cleared about twelve minutes after the period
ended. At that time, the key was usable again, and the next reset time moved
later.

## Providers

Tests against a live Azure OpenAI deployment used OpenCode as the agent. They
showed that these functions work: the model list, streaming, multi-round tool
calls, spend records, and budget enforcement through a virtual key.

All other providers in LiteLLM's provider catalog are expected to work, but
they were not tried here. The list will grow when providers are tried in real
use, and not because they are expected to work.

### Azure OpenAI

**`api_version` must support the Responses API**, not only Chat Completions.

For newer reasoning models, Azure refuses function tools on
`/chat/completions`:

> Function tools with reasoning_effort are not supported for this model in
> /v1/chat/completions.

For a deployment with the name of such a model, LiteLLM knows this and sends
chat requests with tools to `/responses` instead. For other names, refer to the
text below. The requests of an agent almost always carry tools. As a result, in
practice every agent turn goes through the Responses API, and `api_version`
decides if that endpoint exists. A measurement directly against one deployment
gave these results:

| `api_version` | Chat | Chat with tools | Responses | Responses with tools |
| --- | --- | --- | --- | --- |
| `2024-10-21` | 200 | 400 | **404** | - |
| `2025-04-01-preview` | 200 | 400 | 200 | 200 |

With `2024-10-21`, the failure is easy to miss. A plain chat request succeeds.
As a result, a smoke test passes. An agent then gets replies to requests
without tools, for example OpenCode's session title generation. But every real
turn fails with this error:

```
litellm.NotFoundError: AzureException NotFoundError - {"error":{"code":"404","message": "Resource not found"}}
```

The Gateway log also shows the request that it made:
`404 Not Found for url '.../openai/responses?api-version=2024-10-21'`.

Use `2025-04-01-preview` or later. The "Chat with tools" column stays at 400
for all versions. This refusal is the reason for the bridge. It is not a
problem with the bridge.

LiteLLM decides by the deployment's own name. In 1.105.1, this name must begin
with `gpt-5.4` or a later version, for example `gpt-5.4-nano`. In a measurement
on LiteLLM 1.105.1 with a mock provider, a chat request with tools to
`azure/gpt-5.4-nano` went to `/responses`. But a chat request with tools to a
deployment with its own name and `base_model: azure/gpt-5.4-nano` stayed on
`/chat/completions`. There, such a model refuses the request. As a result,
give the deployment the name of the model that it runs.

Otherwise, `model_info.mode: responses` makes LiteLLM send every chat request
of that deployment to `/responses`. In the same test, plain and streamed chat
requests got ordinary Chat Completions answers at the same price. Requests with
tools also went to that endpoint. The health probes of that deployment then
also call the Responses API, without the 16-token limit. A moved request
carries `store` only when the client sends it. If the client does not send it,
the provider's default applies.

### Upstream errors are not retried

By default, LiteLLM retries a failed upstream call twice. Agents also retry on
their own, and as a result the two multiply. In a measurement, one failed
request became three upstream calls, and the client waited four seconds for
the error. If an agent also retries twice, one failure becomes nine. A retry of
a 429 only makes the rate limiting worse.

The chart sets `litellm_settings.num_retries: 0`. A test on two replicas showed
that a 429 or a 500 from the provider reached the client immediately, after one
upstream call. The cluster test examines this behavior on the version that
runs.

A save of the Router Settings page in the console stores the value that the
Gateway runs with. As a result, the value stays at zero. A replay of that save
stored `num_retries: 0`, and failures were still not retried.

A caller cannot enable retries again for its own request. LiteLLM accepts
`num_retries` in the request body or the `x-litellm-num-retries` header, and
`model_group_retry_policy`. In a measurement, each one changed one failed
request into four upstream calls. The image's guard callback drops those
fields. The entrypoint always registers this callback, and the cluster test
examines this behavior. Refer to [The guard callback](#the-guard-callback).

The guard also drops the same fields from `router_settings_override`. LiteLLM
adds these router settings to the request after the guard runs. They are the
router settings of the key or of its team. If neither has router settings,
LiteLLM takes them from the request body.

On LiteLLM 1.105.6, a request body with
`router_settings_override: {"num_retries": 2}` made one failed request three
upstream calls. With the guard, it made one call. As a result, the retry
settings of a key or a team also do not apply.

When a failed request goes to another deployment of the same model, this is not
a retry. Refer to [Routing](#routing).

## Routing

Several entries in `models` with the same `model_name` are deployments of one
model, for example two Azure OpenAI resources, or Azure OpenAI and OpenAI.
`router.routingStrategy` selects one of them for each request. Refer to
[Choosing a deployment](#choosing-a-deployment). An entry with a higher
`litellm_params.order` stays in reserve until the entries with a lower order
are unavailable. The administrator decides which deployments serve a model and
which model falls back to which. The `router` values set the rest.

The routing measurements used two Gateway replicas and the bundled Valkey, and
ran against mock deployments in place of separate Azure OpenAI resources. The
clients were an OpenClaw-like client and OpenClaw itself. Where noted, the
tests also ran against Azure OpenAI. The figures in tables were measured on
LiteLLM 1.101.0, and the text gives the changes in 1.105.1 where they apply.
The image now runs 1.108.3. Measurements that name an earlier release were not
repeated for this upgrade, unless the text says so.

### Choosing a deployment

`router.routingStrategy` sets LiteLLM's `routing_strategy`:

- `simple-shuffle`, the default, selects a deployment at random, or in
  proportion to the `weight`, `rpm`, or `tpm` in each entry's `litellm_params`.
  It uses the first of the three that any entry sets to a positive value. Only
  this strategy sends a failed request to another deployment of the same model
  (`router.failover`).
- `least-busy` selects the deployment with the fewest requests in flight. Each
  replica counts only its own requests.
- `latency-based-routing` selects the deployment with the fastest recent
  responses. Each replica measures its own response times.
- `usage-based-routing-v2` selects the deployment with the fewest tokens in
  this minute. It skips the deployments that are over their `rpm` or `tpm`.
  LiteLLM keeps the counts in Redis. As a result, every replica sees them. Each
  request adds Redis calls.
- `cost-based-routing` selects the cheapest deployment by the
  `input_cost_per_token` and `output_cost_per_token` in its `litellm_params`,
  or else by LiteLLM's price table entry for its `litellm_params.model`. It
  does not read `base_model` or `model_info`. A deployment with neither price
  counts as the most expensive, which is the usual case for Azure OpenAI
  deployment names. As a result, set the prices in `litellm_params`. With
  health checks on, LiteLLM 1.105.1 logs an error with a traceback for every
  probe under this strategy, but requests are not affected. The reason for the
  error is that its cost tracker looks for a deployment ID that probes do not
  carry.

LiteLLM's other strategies are refused. `usage-based-routing` selects a
deployment, but it does not run LiteLLM's auto routers or pre-call checks. The
other strategies must have settings that the chart does not supply.

Each of `weight`, `order`, `rpm`, and `tpm` must be set on every entry of a
model or on no entry. The chart refuses a model that sets one of them on only
some entries. The reason is that LiteLLM gives no requests to an entry without
`weight`, `rpm`, or `tpm` if another entry has it. LiteLLM also does not use an
entry without `order` if another entry has one.

### Primary and standby deployments

`litellm_params.order` keeps deployments in reserve. In this example, a
discounted provider serves the model first, and Azure OpenAI is the standby
deployment:

```yaml
models:
  - model_name: gpt-4o
    litellm_params:
      model: openai/gpt-4o
      api_base: https://discount.example.com/v1
      api_key: os.environ/DISCOUNT_KEY
      order: 1
      input_cost_per_token: 0.000002
      output_cost_per_token: 0.000008
  - model_name: gpt-4o
    litellm_params:
      model: azure/my-gpt-4o-deployment
      api_base: https://example.openai.azure.com/
      api_key: os.environ/AZURE_PROD_KEY
      api_version: "2025-04-01-preview"
      order: 2
    model_info:
      base_model: azure/gpt-4o
```

Requests go to order 1. With health checks on, LiteLLM skips a deployment that
failed its last probe, before it sends anything. If a request fails on order 1,
LiteLLM sends it to order 2 in the same request, with every routing strategy.
Requests return to order 1 when its probe passes again, or, without health
checks, when its cooldown ends. Deployment affinity is off for such a model.
Refer to [A key stays on its deployment](#a-key-stays-on-its-deployment).

In a measurement on LiteLLM 1.105.1 with mock deployments, a key's requests
went to order 2 while order 1 failed its probes. After order 1 passed its
probes again, the requests returned to it.

### A failed request goes to another deployment

`router.failover` is on by default. It sends a request that failed on one
deployment one more time, to another deployment of the same model (LiteLLM's
`enable_weighted_failover`). It does this only with the `simple-shuffle`
strategy. It does not try the failed deployment again. A model with a single
deployment gets no second try. As a result, `num_retries` stays 0.

| One of two deployments failing | Requests failed, of 30 |
| --- | --- |
| With 500, `router.failover: false` | 13 |
| With 429, `router.failover: false` | 2 |
| With 500 or 429, `router.failover: true` | 0 |

Without failover, LiteLLM sends no more requests to a deployment that fails,
but only after several failures, and only for a few seconds. Each replica keeps
its own count of the failures that it sees. The replicas share the cooldown
that follows through Redis. Failover has a cost. A request that the provider
refuses as invalid, with a 400, also goes to the other deployment before the
error reaches the client.

The CetiRun operator registers GitHub Copilot routes for each Agent. These
routes have the same names as the shared models and as the routes of the other
Agents. The access group identifies each route. With failover on, no Agent's
request reached another Agent's route.

With `router.deploymentAffinity` on, a key stays on the deployment that served
it last. While that deployment is still in rotation, failover cannot move the
key. The key moves when LiteLLM puts that deployment in cooldown. LiteLLM does
this immediately after a 429 from a model with other deployments. For other
errors, it does this only after repeated failures. LiteLLM compares these
failures with the successes of the deployment in the current minute.

In a measurement, a key stayed on a deployment that returned 500. The first 2
of 10 requests failed, and the other 8 went to another deployment. Immediately
after 30 successful requests in the same minute, 8 of 8 failed. With 429, none
of 10 failed.

### A continuation stays on its deployment

A Responses API request that continues an earlier request with
`previous_response_id` works only on the deployment that stored the earlier
response. `router.continuationAffinity` is on by default, and it sends the
continuation to that deployment. Across two deployments, 6 of 20 continuations
failed without it, and none failed with it. When that deployment is not
available, the continuation fails with or without it.

### Encrypted reasoning stays with its resource

A reasoning model can return its reasoning in encrypted form. Only the Azure
OpenAI resource or OpenAI organization that made it can read it. Agents that
keep no state on the provider, for example OpenClaw, ask for it and send it
back with every turn.

These results were measured on Azure OpenAI. Another model on the same
resource accepted the encrypted reasoning. A made-up or changed item was
refused with a 400, `invalid_encrypted_content`. Only a measurement against
mock resources shows that another resource refuses it in the same way.

When the item is refused, OpenClaw retries the turn one time without the
encrypted reasoning. More than one resource can serve one model. In that case,
a conversation that was on both resources pays for an extra call on every
later turn. On these turns, it also loses the reasoning that it had. In the
test, this occurred on 61 of 84 turns.

`router.encryptedContentAffinity` is on by default. It keeps each conversation
on the deployment that encrypted the earliest reasoning in the conversation,
or on a deployment with the same endpoint and key. In LiteLLM, this check is
`encrypted_content_affinity`. The check marks the items that it returns, so no
state in Redis is necessary. It is safe to turn the check off later, because
the marks are removed from every request in both cases.

If a deployment authenticates without an API key, it never counts as a
deployment with the same endpoint and key as another deployment. The check
applies to every model. LiteLLM cannot turn it off for one model.

LiteLLM forwards an item without the mark as it is. Such an item can be from
before the check was turned on. An example is an item in a conversation that
an agent started before an upgrade. The item can also come from elsewhere.
When such an item gets to a deployment of another resource or account, that
deployment refuses all of the request with a 400, `invalid_encrypted_content`.
A router fallback that is configured for the model fails in the same way.

Because of this, while the check is on, the guard callback changes such an item
before the request is routed. It removes the encrypted content and the ID of
the item. It keeps the readable summary of the item, as LiteLLM does when it
moves a marked item. An item with no readable part is removed from the
request. Such a conversation loses the encrypted reasoning of its earlier
turns, but only one time. After that, all that the agent sends is marked.

These results were measured on a test installation. An Azure OpenAI item
without its mark was sent to a GitHub Copilot route of the same model, and was
refused. A Copilot item that was sent to Azure OpenAI was refused because of
its ID. With their marks, both items got an answer. In that test, OpenClaw
retried such turns without the encrypted reasoning. The cost of each retry was
the failed call and its fallback.

A GitHub Copilot model returns its reasoning items with a `status`. Azure
OpenAI refuses this field on a reasoning item that a request sends back, with
a 400: `Unknown parameter: 'input[n].status'`. Azure OpenAI also refuses an
item ID longer than 64 characters. Copilot's IDs for the messages of a Grok
model had 396 characters.

On the e2e cluster, a stateless Responses API conversation moved from a
Copilot model to the Azure tier of a router. The check removed the encrypted
content of the Copilot item, but Azure OpenAI refused the request because of
these fields. The router's fallback answered it.

For a request to an Azure OpenAI deployment, the guard callback now removes the
`status` of each reasoning item. It also removes the ID of any other item that
is longer than 64 characters. Azure OpenAI accepts the `status` of a message,
so it does not change. An ID that LiteLLM marked (`encitem_`) also does not
change, because LiteLLM restores it before it sends the request. The Gateway
logs a warning for each request that the guard changes. On the e2e cluster,
Azure OpenAI answered the same conversation after these changes.

These results were measured with mock resources. With the guard, every such
replay got an answer, streamed or not streamed. An item with the mark was
routed as before. With the check off, nothing is removed.

The mark is written so that clients keep it. LiteLLM writes it as
`litellm_enc:<base64>;<encrypted content>`. OpenClaw keeps encrypted reasoning
only when it looks like an opaque token (letters, digits, `+ / _ -`, and `=`
signs at the end). All other content goes through the secret redaction of its
transcript. This redaction shortens a `gAAAA…` value to its first six and
last four characters.

On a test installation, this sequence occurred on every OpenClaw turn after a
turn with reasoning. The turn sent such a shortened item back and was refused
with `invalid_encrypted_content`. Then it went through the fallbacks of the
model, which failed in the same way. Then OpenClaw retried the turn without its
reasoning. This also occurred in new conversations, on Azure OpenAI and on
GitHub Copilot.

As a result, the guard callback makes LiteLLM write the mark as
`ctrnenc<deployment ID in hex>x<encrypted content>`. The guard callback also
makes LiteLLM read both forms, so items marked before an upgrade still route.
Encrypted content that a client shortened (it contains `…`) is removed in
the same way as an item without the mark.

These results were measured with mock resources on LiteLLM 1.105.1. Every item
that a response carries is in the new form, streamed or not streamed. Items in
the old form and in the new form are routed back to their deployment, and get
to it without the mark.

A deployment can refuse a request because of its encrypted reasoning (400,
`invalid_encrypted_content`). Such a request does not go on to the
`router.fallbacks` of the model. The deployments of a fallback also cannot read
that reasoning, so each of them fails in the same way. The guard ends the
fallback, and the client gets the 400 of the deployment. OpenClaw then sends
the turn again without the encrypted reasoning. Other errors fall back as
before.

These results were measured with mock resources. The refused request got to no
fallback deployment, and no deployment went into cooldown. A 500 still fell
back.

When that deployment fails, LiteLLM 1.105.1 keeps the conversation on it while
it still counts as available. As a result, the requests of the conversation
fail with the error of the deployment. `router.failover` does not prevent these
failures. The failover sends the request a second time, but the check pins it
to the same deployment. The failover must skip that deployment.

The deployment does not count as available while it is in cooldown, or while
the health checks mark it unhealthy. The conversation moves only at that time.
Its turns go to another deployment without encrypted reasoning, but with the
reasoning summaries. This continues until the deployment counts as available
again.

These results were measured with two replicas and mock deployments. No
deployment received the encrypted reasoning of another deployment, and no
deployment refused a turn with a 400. LiteLLM 1.101.0 failed every such turn
until the deployment was available again. For this reason, the check was off
by default in earlier versions.

A cooldown starts when more than half of at least five calls to the deployment
in a minute failed on one replica. A 429, 401 or 404 starts a cooldown
immediately. A cooldown lasts five seconds. Only the request whose failure
starts the cooldown goes on to another deployment through `router.failover`.
As a result, without health checks, a conversation leaves a deployment that
fails only for a short time.

Health checks are on by default. With health checks on, the requests of the
conversation fail only until a probe marks the deployment unhealthy. On the
replica that probes, this occurs in one `router.healthChecks.intervalSeconds`
or less (300 seconds by default). The other replicas use its result, so on
them this can occur up to one interval later. After that, the conversation
stays on another deployment while the probes continue to fail.

Keep health checks on. A shorter interval makes this time shorter, but it
causes more probes. The CetiRun operator registers GitHub Copilot routes.
These routes are not probed unless the operator turns on their probes (see
[Health checks](#health-checks)). A conversation can be kept on the Copilot
route of an Agent. If that route fails, the conversation leaves it only for a
short time, as without health checks.

### A key stays on its deployment

`router.deploymentAffinity` is on by default. For one hour, it sends the
requests of each virtual key for a model to the deployment that served the
last request of that key. In LiteLLM, this check is `deployment_affinity`.
Providers cache the start of a prompt on the deployment that saw it. As a
result, an agent that keeps its conversation on one deployment pays less and
waits less for each turn.

With Redis, every replica sees the same choice. While that deployment is
unavailable, the requests of the key go to the other deployments.

These results were measured on LiteLLM 1.105.1 with two replicas and two
deployments. Four keys sent 10 requests each, to the two replicas in turn. The
10 requests of each key went to one deployment.

A key can be kept on a deployment. If a request fails on that deployment, it
fails over to another deployment (`router.failover`), as any other request
does. But LiteLLM 1.105.1 applies the choice of the key before it removes the
deployments that a failover already tried. As a result, the failover found no
deployment and was not tried.

This was measured on the e2e cluster. A key was kept on a deployment that
answered 500. Before that deployment went into cooldown, the key got three
500s ("No fallback was attempted"). The guard of the Gateway gives the
affinity check of LiteLLM only the deployments that the failover can still
use. After this change, another deployment answered the requests of the same
key from the first failure.

This check is off for a model whose deployments set `litellm_params.order`.
LiteLLM applies the choice before it keeps only the lowest order. It also
keeps the first choice for the hour. As a result, a key can go to the standby
while the primary is down. That key then stays on the standby after the
primary is available again.

For such a model, the chart turns this check off in
`model_group_affinity_config`. This setting keeps the other checks on for that
model. These results were measured on LiteLLM 1.105.1. The first requests of a
key came while the primary failed its probes, and these requests went to the
standby. After the primary passed again, with affinity on for that model, all
6 of the requests of the key stayed on the standby. With the setting of the
chart, all 6 went back to the primary.

### Fallbacks

For a model, `router.fallbacks` lists the models to try, in order, when a
request to that model still fails:

```yaml
router:
  fallbacks:
    - gpt-4o: [gpt-4o-mini]
  contextWindowFallbacks:
    - gpt-4o-mini: [gpt-4o]
```

With `operatorIntegration`, a fallback target is used only if the key of the
caller has access to it. `contextWindowFallbacks` applies to requests that are
too long for the context window of the model. An earlier measurement used a
model with two deployments, and one of them was unreachable. Without a fallback
group, 6 of 30 requests failed. With a fallback group, no request failed.

Without `operatorIntegration`, as on a standalone Gateway, LiteLLM does not
examine the key for a fallback target. A key then reaches a fallback target
that it was not given. Name as fallback targets only models that every key of
the first model can also use.

A request to a router goes to its other models when the model of its tier
fails, without this setting. Fallbacks in `router.fallbacks` for a model of
the router, or for the router's name, apply instead. Refer to
[Smart routing](#smart-routing).

### Health checks

`router.healthChecks.enabled` is on by default. It probes every deployment at
an interval of `router.healthChecks.intervalSeconds` (300 by default). It
sends requests only to the deployments that answered their last probe. This
was measured with two deployments, and one of them failed. Every request went
to the other deployment. Without health checks, 10 of 30 requests failed.

When every deployment of a model fails its probe, requests are still sent.
Every deployment means the models in `models` and the models that were added
through the console. Each probe is a real call, with `max_tokens` 16. A router
from the auto routing of LiteLLM is not probed, but its models are probed.

Set `model_info.mode` on a model that is not a chat model, for example an
embedding or image model. Then the model is probed in the correct way. To stop
the probes of a model, set `model_info.disable_background_health_check: true`
on that model.

For each Agent, the CetiRun operator registers GitHub Copilot routes. By
default, these routes are not probed, because the operator registers them with
`model_info.disable_background_health_check: true`. A probe of the Copilot
route of an Agent uses the personal GitHub Copilot allowance of the Agent
owner. At the default interval, this is approximately 288 small calls each day
for each route. An Agent has a route for each of its Copilot models.

`aiGateway.copilotProxy.healthChecks.enabled` in the operator chart turns on
the probes of these routes. In the `cetirun-platform` chart, this setting is
`cetirun-operator.aiGateway.copilotProxy.healthChecks.enabled`. Then these
routes are probed as every other deployment is. This changes the routing only
for a route under the name of a shared model. If only the route of the Agent
serves a name, the requests for that name go to that route in both cases.

Without probes, nothing removes such a route from rotation when its proxy is
unreachable. See [Serving CetiRun Agents](#serving-cetirun-agents).

At most `router.healthChecks.concurrency` probes start at the same time (8 by
default). In LiteLLM, this setting is `health_check_concurrency`. Without a
limit, LiteLLM starts all probes of a cycle at the same time. This was
measured on LiteLLM 1.105.1. Every deployment was probed in approximately
100 ms. One GitHub account answered 11 to 43 of the Copilot probes of a cycle
with 429.

If the deployment does not set
`model_info.health_check_params: {max_retries: 0}`, LiteLLM retries a failed
probe at the SDK level. These retries add calls. The CetiRun operator
registers every GitHub Copilot route with this setting. As a result, when the
probes of these routes are on, LiteLLM does not retry them.

This was measured on LiteLLM 1.105.1 against an upstream that answered 429. A
route registered with this setting got one call for each probe. A route
without it got three calls.

Routing ignores a probe that got a 429 or 408 answer. The deployment keeps the
state of its previous probe for up to two intervals, and no cooldown starts.
`/health` and the stored results still list the deployment as unhealthy. The
results are stored in PostgreSQL.

For more than one replica, Redis is necessary. Without Redis, the single
replica probes alone. With Redis, only one replica probes at a time, under a
lock in Redis. The other replicas use its results
(`use_shared_health_check`).

The chart sets `DEFAULT_SHARED_HEALTH_CHECK_TTL` to the interval, so a shared
result is not used again after the next probe. The chart also sets
`DEFAULT_SHARED_HEALTH_CHECK_LOCK_TTL` to the interval. Without this setting,
the lock expires after 60 seconds. Then a longer cycle lets another replica
probe every deployment again.

These results were measured on LiteLLM 1.105.1 with two replicas, a 30-second
interval, and six mock deployments. Two of the deployments were added through
the management API, with Copilot access groups for each Agent. Each deployment
was probed twice in 65 seconds. One of the two deployments of a model failed
its probes. 12 of 12 requests went to the other deployment, and none got to
the deployment that failed.

Each probe writes a line to the request log with `"call_origin":
"health_check"` and the key alias `litellm-internal-health-check`. Do not
include these lines in usage figures.

The results are kept in PostgreSQL for the time that `spendLogs.retention`
sets, as the spend log is. `/health/latest` and the console show the latest
result of each deployment. `/health` also returns the last results. With
health checks off, it probes every deployment live when it is called with the
master key. It refuses the keys of Agents.

### Smart routing

If the `litellm_params.model` of an entry in `models` is
`auto_router/complexity_router`, the entry is a router. This is the auto
routing of LiteLLM. For each request, the router selects one of four tiers.
Then it sends the request to the model that is configured for that tier.

```yaml
models:
  - model_name: auto
    litellm_params:
      model: auto_router/complexity_router
      # Only for the unpriced-model check; see below.
      input_cost_per_token: 0.00001
      output_cost_per_token: 0.00004
      complexity_router_config:
        tiers:
          SIMPLE: gpt-4o-mini
          MEDIUM: gpt-4o
          COMPLEX: gpt-4o
          REASONING: o3
      complexity_router_default_model: gpt-4o
```

The chart supplies the mechanism. The tiers, their models, and the keyword
rules, if there are any, are the responsibility of the administrator.

The example sends requests to `gpt-4o-mini`, `gpt-4o`, and `o3`. Each of them
must also be an entry in `models`, because the chart refuses a router whose
tier is not configured there. Helm replaces a list as a whole. As a result, a
second values file with `models` replaces the list of the first file. Put all
entries of `models` in one file.

These results were measured with the default classifier of LiteLLM. This
classifier scores the latest message without a model call. "What is 2+2?" went
to the simple tier. A request for code went to the medium tier.

A classifier that asks a model (`classifier_type: llm`) makes that call for
every request. This call is charged to the key of the caller. The request log
records the call as a separate line, with
`"call_origin": "autorouter_classifier"`. Anyone can move a request one tier
up. To do this, they write the escalation keyword of LiteLLM,
`LITELLM ESCALATE`, into the request.

**The router's name needs a price.** The unpriced-model block applies to the
router as it applies to any model. As a result, the chart does not accept the
router unless its `litellm_params` contain `input_cost_per_token` and
`output_cost_per_token` above zero. A price in `model_info` is not sufficient.

A measurement showed these results. Each call was charged at the price of the
model that the router selected, not at the price of the router. The budget of
a key continued to apply. With a $0.0001 budget, the fourth call was refused.

**OpenClaw's own context is skipped.** The classifier reads the newest message
that a person wrote. OpenClaw 2026.9.6 sends its own context as a last user
message between `<<<BEGIN_OPENCLAW_INTERNAL_CONTEXT>>>` and
`<<<END_OPENCLAW_INTERNAL_CONTEXT>>>`. When the classifier used that block,
every OpenClaw request went to the simple tier. This included a request for
code.

If a router does not set its own `reminder_markers`, the chart adds that pair
and the marker of LiteLLM, `<system-reminder>`. The classifier does not read
the blocks between these markers. With these markers, the request for code
went to the medium tier. OpenClaw called the router through Chat Completions,
without reasoning settings.

OpenClaw also sends the images of a tool result, such as a screenshot of its
`computer` tool, to a model that accepts images. It sends them in a separate
user message, which starts with a label, for example
`Image(s) from tool result #1 (computer):`. The classifier read that label as
the newest request. After one screenshot, a request for a design that the router
put in the medium tier went to the simple tier. The chart also adds a pair for
this label (`Image(s) from tool result #` to the next `:`). A router with a
`jev` classifier kept the complex tier with the label as well.

The `reminder_markers` of a router replace the markers of the chart. These
markers must be a list of `{open, close}` pairs, because LiteLLM 1.105.1
accepts only that format. Each pair must have two different strings that are
not blank. The chart refuses all
other values. As a result, the proxy does not refuse to start because of them.

**The agent keeps its conversation and its limits.** If a router does not set
them, the chart changes three router defaults of LiteLLM 1.105.1. For a router
with `classifier_type: jev`, the chart also changes a fourth default:

- `context_compaction: false`. Without this setting, LiteLLM rewrites the
  conversation history near the input limit of the selected model, with more
  model calls. Agents, for example OpenClaw and Hermes, compact their own
  context.
- `max_tokens_from_tier_model: false`. Without this setting, LiteLLM replaces
  the `max_tokens` of the caller with the output limit of the model of the
  tier. With this setting, the value of the caller is forwarded.
- `enable_context_window_escalation: true`. Without this setting, a long
  conversation with a simple newest message can go to a tier whose models
  cannot hold it. Then the provider refuses the conversation. With this
  setting, LiteLLM moves it to the lowest tier that has a model with a
  sufficiently large declared window. For this, set `max_input_tokens` in the
  `model_info` of each model.
- `classifier_fallback: default_model`, with `classifier_type: jev`. The
  decision model cannot always classify a request, for example when it is
  unavailable. Without this setting, the heuristic scorer of LiteLLM then
  classifies the request. In our tests, this scorer put 15 of 40 English
  prompts and 11 of 40 Chinese prompts on the expected tier. With this
  setting, the request goes to the `complexity_router_default_model` of the
  router.

  If the router has no default model, LiteLLM uses the first model of the
  medium tier, or of the simple tier. The request is not moved to another tier
  because of its length or because of the escalation keyword. The response
  header `x-litellm-complexity-router-cause` and the `routing_cause` of the
  request log show `default_model_fallback`, with no tier. A router that sets
  `classifier_fallback` keeps its own value. A router with its own
  `tier_definitions` uses its `fallback_tier`, and gets no default.

**A router is granted only with its models.** A router sends requests to a set
of models. With `operatorIntegration`, the CetiRun operator puts the router on
the key of an Agent only if the Agent can use all of these models. The models of
its tiers, its default model, and a context compaction model must be shared
models that the Agent can use. For an Agent with an AgentPolicy, the
shared-model list of the policy must name these models. It must also name the
router.

A classifier model must be a shared model, but it is not necessary to put it
in the list. This applies to `classifier_llm_config.model`, to a
`jev_classifier_config.deployment_name`, and to the `embedding_model` of
semantic keyword matching. The router calls the classifier model to classify
the request of the Agent, and this call is charged to the key of the Agent. An
Agent without a policy can use every shared model. As a result, it gets every
router whose models are all shared models.

If the operator cannot read a router completely, it does not give the router
to Agents with a policy. These routers include:

- A router with a `complexity_router_config` field that LiteLLM 1.105.1 does
  not have.
- A router with a classifier plugin or a routing plugin.
- A router with a jev classifier without a `deployment_name`.
- Another kind of LiteLLM auto router.

A router that an Agent cannot use stays off the key of the Agent and out of
its model list. If the policy of the Agent lists the router, the page of the
Agent in the Portal shows the reason. The operator reads the routers from the
model list of the Gateway. As a result, a changed router gets to the keys at
the next listing. This occurs in approximately two minutes or less. The
configuration of a router is not part of the Gateway projection of an
AgentPolicy.

**An Agent gets the router's limits.** With `operatorIntegration`, the
operator configures each Agent that gets a router with a context window and
an output limit for it. These are the smallest `max_input_tokens` and
`max_output_tokens` among the models of the router's tiers and its default
model. A model without these values does not count, and neither does a
classifier model. A request that the Agent sizes for the router then fits
whichever model the router selects.

Without these limits, the Agent used its own default, for example 200,000 tokens
for OpenClaw. This default can be more than the model of a tier holds. To use
other values, set `max_input_tokens` and `max_output_tokens` in the `model_info`
of the router. A router that the operator cannot read completely gets no limits.
When these limits change, for example because a tier gets another model, the
Agents that use the router are configured again, which restarts them.

**A failed tier goes to the router's other models.** The model of the tier
that the router selects can fail. For example, the GitHub Copilot proxy of an
Agent can be down. The image's guard callback then lets LiteLLM send the
request to the other models of the router, in this order:

1. The other models of the same tier.
2. The models of the higher tiers, from the lowest.
3. The default model of the router.
4. The models of the lower tiers, from the highest.

With `operatorIntegration`, LiteLLM uses only the models that the key of the
caller can use. Without it, as on a standalone Gateway, LiteLLM does not
examine the key for these fallbacks. They give the key no new model, because
the router can select each of its models for the key's requests.

LiteLLM sends the request to another model only before a response starts.
Errors of all kinds count, for example a 503, a timeout, a 429, or a 400 from
the model. A refusal by the guard does not count, for example the 403 for a
model that the key cannot use. The guard adds these fallbacks to the request
only. It does not change a setting of the Gateway.

These fallbacks do not replace other fallbacks:

- The fallbacks in `router.fallbacks` for a model of the router, or for the
  router's name.
- The fallbacks that a request names, or `disable_fallbacks` in a request.
- The fallbacks in the router settings of a key or its team.
- After an `invalid_encrypted_content` refusal, the request goes to no other
  model. Refer to
  [Encrypted reasoning stays with its resource](#encrypted-reasoning-stays-with-its-resource).

The response names the model that answered, not the router. It has the
`x-litellm-attempted-fallbacks` header, but no `x-litellm-complexity-router-*`
headers. The request log writes a `failure` line for the tier, with the
router's name as `model` and the tier as `routing_tier`. It then writes a line
for the model that answered. This line has `fallback_from` (the router's name),
`fallback_cause` (the error class of the first failure), and
`attempted_fallbacks`. `attempted_fallbacks` counts each time LiteLLM sent the
request on, also to another deployment of the same model (`router.failover`).

A measurement on LiteLLM 1.105.6 used mock models. The simple tier of a router
answered 503. Before the change, the client got the 503 after one upstream
call. With the change, the medium tier answered the request. This occurred on
the Chat Completions API, streamed or not, and on the Responses and Anthropic
Messages APIs, also with a Team router's internal name. With every model of
the router down, each model got one call, and the client got the 503 of the
first model.

On the e2e cluster, the GitHub Copilot proxy of an Agent was scaled to zero.
Its Team's router had `copilot/claude-opus-5.5` as its medium tier. Six requests
to write code went to that tier. The Azure OpenAI model of the router's other
tiers answered each of them after 8 to 12 seconds. A request for the Copilot
model by its own name got 503, without internal names in the message.

**Errors do not show LiteLLM's internal names.** LiteLLM adds its internal
names to the errors that it returns: the model groups of a request, their
fallbacks, and timeouts. For a Team's router, such a model group is the
router's internal name, for example "model group 'tier-down ->
cetirun-router-0123456789abcdef'". The guard sets LiteLLM's
`expose_router_debug_in_errors` to false when it starts. The client then gets
the error of the model only. The Gateway's own log keeps LiteLLM's message
about the fallbacks.

**Models that answer no conversation are not offered to Agents.** Some shared
models have a `model_info.mode` other than `chat`, `completion` or
`responses`. Examples are `evaluation` for a decision model such as
Microsoft-Decision-1, and `embedding`. Such a model is not in the models that
Agents are configured with. It is not used as their default model, and it is
not in the catalog that the Portal reads.

In all other ways, such a model stays a shared model. A router can classify
requests with it, and the key of an Agent without a policy keeps its access
group. If the tier, the default model, or the compaction model of a router is
such a model, the router is not configured correctly. Set `mode` on the
`model_info` of the model. A model without a mode counts as a chat model.

For the keys that the operator issues, the Gateway refuses some requests with
HTTP 403. These are requests that a router sends to a model outside the access
groups of the key. The classifier calls of the router are an exception (see
[The guard callback](#the-guard-callback)). Other keys get to every model of
the tiers of the router. This is also true when they cannot use these models
directly.

A key that has permission for the models but not for the router cannot use
the router. The chart refuses a tier, a default model, or a classifier model
that is not configured in `models`. For a `jev` classifier, this is the model
in `jev_classifier_config.deployment_name`.

**The tiers decide what a router saves.** Each call is charged at the price of
the model that the router selected. With a classifier that asks a model, the
classifier call is also charged. This call reads the system prompt and the
latest user messages. As a result, it costs more when they become longer.

A router saves money when it sends turns to a model with a lower price than
the model that the conversation uses without the router. As a result, put the
tiers in order of price. Keep the top tier near the model that you use without
the router.

The table that follows shows the measured costs of the same 30 turns. These
turns were three conversations with a 6,000-token context, with answers of up
to 800 tokens. The costs use the Azure OpenAI list prices of the models:

| Through | Cost |
| --- | --- |
| A router with tiers gpt-5.4-nano, gpt-5.6-terra, gpt-6-sol, gpt-6-astra and the Decision classifier | 0.312 |
| The same router with the default classifier | 0.082 |
| gpt-6-sol alone | 0.130 |

These turns were sent with the key of a CetiRun Agent. As a result, the GitHub
Copilot route of the Agent answered its gpt-5.6-terra and gpt-6-astra turns,
at the same prices (see [Serving CetiRun Agents](#serving-cetirun-agents)).

In that router, gpt-5.6-terra costs more than gpt-6-sol, which is one tier
above it. The gpt-6-astra model costs almost six times as much as gpt-6-sol.
The Decision classifier sent every reasoning turn to gpt-6-astra. The
classifier calls of this router were 2.7% of its cost.

Then the tiers were put in price order: gpt-5.4-nano, gpt-6-sol, gpt-6-sol
and gpt-6-sol. The answers were up to 6,000 tokens. With these settings, the
same 30 turns cost 0.194 with the Decision classifier and 0.099 with the
default classifier. Through gpt-6-sol alone, they cost 0.215.

The default classifier saved more, because it sent 24 of the 30 turns to
gpt-5.4-nano. These turns included difficult turns. The gpt-6-sol model
compared these answers with its own answers. 3 of those 24 answers were
clearly worse. Of the 14 turns that the Decision classifier sent to
gpt-5.4-nano, 2 answers were clearly worse.

Each model keeps its own prompt cache. A turn can go to a model that the
conversation did not use before. That turn pays the full input price for all
of the conversation. A later turn on a model that the conversation used before
reads the cached history of that model again. This is also true when turns on
other models occurred between them.

A 30,000-token conversation went to gpt-5.4-nano and gpt-6-astra in turn. It
cost 18 to 23% less than the same turns on gpt-6-astra alone. Each time that
the conversation came back to gpt-6-astra, gpt-6-astra read its cache. In both
tests, the largest single charge was the first read of the conversation by
gpt-6-astra. Providers keep a cache only for some minutes after its last use.
As a result, when a model is not used frequently, its cache can be gone.

Test a classifier with prompts that are like the prompts of your users. The
default classifier scores English keywords and the length of the prompt. The
test used 40 prompts, ten for each tier. The default classifier put 15 of
these prompts on the expected tier, and 11 of their Chinese translations. It
never selected the complex tier or the reasoning tier. It sent 37 of the
Chinese prompts to the simple tier.

The Decision classifier put 39 of the 40 prompts on the expected tier in each
language. It put each translation on the same tier as its original.

The Decision classifier sees all of the system prompt of the caller and the
latest user messages. LiteLLM does not make the system prompt shorter.
Microsoft-Decision-1 refuses a request that is larger than its context of
32,768 tokens. In a direct measurement, an English prompt of 32,065 tokens was
classified. A prompt that was 1,000 words longer was refused.

Without the guard, every classification failed with a system prompt of
approximately 40,000 tokens. The router then used the default classifier, and
it did not report an error. The guard now shortens these requests so that they
fit. Refer to [The guard callback](#the-guard-callback). If a classification
fails anyway, the router sends the request to its default model. The response
header `x-litellm-complexity-router-cause` and the `routing_cause` of the
request log then show `default_model_fallback`, with no tier.

**A conversation can change model.** Each request is classified separately. As
a result, one conversation can move between tiers. On the same Azure OpenAI
resource, this continued to work, also with encrypted reasoning. Across
resources, with LiteLLM 1.101.0, an agent, for example OpenClaw, retried the
turn without its encrypted reasoning.

With `router.encryptedContentAffinity` on, LiteLLM 1.105.1 removes the
reasoning that the new resource cannot read before it sends the turn. This
information comes from the source code of LiteLLM, not from a measurement. In
both cases, that reasoning is lost. As a result, keep the models of a router on
one resource if you can.

LiteLLM can keep a conversation on its first tier (`session_affinity`, or
`classification_mode: user_turn`). It can do this only for clients that send a
session ID that it recognizes. OpenClaw 2026.9.6 sends no session ID that
LiteLLM recognizes.

**One router per licensed capability.** Without a LiteLLM license, LiteLLM
1.105.1 lets not more than one router claim each of these items:

- `classifier_type: heuristic_v2`
- `classifier_type: capability`
- `classifier_type: llm_v2`
- Its own `tier_definitions` or classifier prompt. With a classifier that calls
  a model, a classifier prompt is `classifier_llm_config.system_prompt`,
  `classification_prompt` or `classification_examples`. A classifier prompt is
  also `jev_classifier_config.instructions` that are different from the
  instructions of LiteLLM.

With two routers in one class, the proxy refused to start. The chart refuses
the second router. When the chart counts, it includes all
`jev_classifier_config.instructions`. One router in each class is accepted.

The request log line of a routed call contains these items:

- The name of the router as `model`.
- The model that the router selected as `provider_model`.
- `routing_tier`, `routing_cause`, and `routing_score`.
- `routing_escalated`, when an escalation keyword or the length of the prompt
  moved the call up a tier.
- With a model classifier, `classifier_model` and `classifier_cost`.

[Measuring the routers](#measuring-the-routers) has queries for these items.

#### Choosing a classifier

`complexity_router_config.classifier_type` sets the method that selects the
tier:

| `classifier_type` | Decides by | Set |
| --- | --- | --- |
| `heuristic`, the default | A score of English keywords and of the length of the request, without a model call | Nothing |
| `llm` | A chat model that is configured here selects the tier | `classifier_llm_config.model` |
| `jev` | A decision model that is configured here, for example Microsoft-Decision-1, selects the tier | `jev_classifier_config.deployment_name` |

A model classifier makes one call for each request. The request waits for this
call, and the call is charged to the key of the caller. The request log
records the call as a separate line. For `llm`, a small, fast chat model is
sufficient:

```yaml
  - model_name: auto-llm
    litellm_params:
      model: auto_router/complexity_router
      input_cost_per_token: 0.00001
      output_cost_per_token: 0.00004
      complexity_router_config:
        classifier_type: llm
        classifier_llm_config:
          model: gpt-mini
        tiers:
          SIMPLE: gpt-mini
          MEDIUM: gpt-main
          COMPLEX: gpt-main
          REASONING: gpt-main
      complexity_router_default_model: gpt-main
```

A decision model is a separate model entry whose `model_info.mode` is
`evaluation`. LiteLLM calls it through its Decisions API. The operator does
not offer it to Agents. The guard refuses a request that asks it for a
conversation (see [The guard callback](#the-guard-callback)).

Microsoft-Decision-1 is deployed in an Azure AI Foundry resource, not in an
Azure OpenAI resource. LiteLLM gets to it with the `azure_ai` provider at the
endpoint of the resource. As any model, it must have a price. `base_model`
names the price that LiteLLM has for this model:

```yaml
  - model_name: decision-1
    litellm_params:
      model: azure_ai/my-decision-deployment
      api_base: https://example.services.ai.azure.com
      api_key: os.environ/AZURE_AI_FOUNDRY_KEY
    model_info:
      mode: evaluation
      base_model: azure_ai/Microsoft-Decision-1
  - model_name: auto-decision
    litellm_params:
      model: auto_router/complexity_router
      input_cost_per_token: 0.00001
      output_cost_per_token: 0.00004
      complexity_router_config:
        classifier_type: jev
        jev_classifier_config:
          deployment_name: decision-1
        tiers:
          SIMPLE: gpt-mini
          MEDIUM: gpt-main
          COMPLEX: gpt-main
          REASONING: gpt-main
      complexity_router_default_model: gpt-main
```

These results were measured with this pair and a Microsoft-Decision-1
deployment. With a short prompt, each classifier call took approximately half
a second and cost a few millionths of a dollar. The time and cost of the calls
increase with the size of the system prompt of the caller, because the
classifier reads all of it.

Keep the instructions of LiteLLM for the classifier. A router that sets
`jev_classifier_config.instructions` has its own classifier prompt, and
LiteLLM limits these prompts. For more information, see "One router per
licensed capability" above.

A model classifier can fail, or not answer in its `timeout_ms` (3000 by
default). In these cases, a router with an `llm` classifier classifies the
request with `heuristic` instead. A router with a `jev` classifier sends the
request to its default model, as `classifier_fallback: default_model` above
describes. Neither router reports an error. After a timeout, the router skips
the classifier for 30 seconds.

`routing_cause` in the request log and the response header
`x-litellm-complexity-router-cause` show which classifier decided. For a model
classifier, they show `llm_classifier` or `jev_classifier`. For the local
scorer, they show `heuristic_scorer` or `reasoning_override`. After a failed
`jev` classification, they show `default_model_fallback`. Sometimes the model
of the selected tier has no healthy deployment. Then the router sends the
request to another model of its tiers, or to its default model, and they show
`health_failover` or `health_default_fallback`.

#### A router across providers

The tiers of a router can use different providers. In the example that
follows, the simple and medium tiers use GPT models on Azure OpenAI. The
complex and reasoning tiers use a Claude model that is deployed in
Azure AI Foundry. Adjacent tiers that use the same provider keep a
conversation on its provider when the conversation moves one tier up or down.

```yaml
models:
  - model_name: gpt-mini
    litellm_params:
      model: azure/my-gpt-mini-deployment
      api_base: https://example.openai.azure.com/
      api_key: os.environ/AZURE_OPENAI_KEY
      api_version: "2025-04-01-preview"
    model_info:
      base_model: azure/gpt-4o-mini
      max_input_tokens: 128000
  - model_name: gpt-main
    litellm_params:
      model: azure/my-gpt-deployment
      api_base: https://example.openai.azure.com/
      api_key: os.environ/AZURE_OPENAI_KEY
      api_version: "2025-04-01-preview"
    model_info:
      base_model: azure/gpt-4o
      max_input_tokens: 128000
  - model_name: claude
    litellm_params:
      model: azure_ai/my-claude-deployment
      api_base: https://example.services.ai.azure.com/anthropic
      api_key: os.environ/AZURE_AI_FOUNDRY_KEY
      # The Claude model rejects some OpenAI parameters, such as temperature
      # with top_p, or a forced tool_choice; LiteLLM drops those it rejects.
      drop_params: true
      # Prompt caching on the system message and the newest message.
      cache_control_injection_points:
        - location: message
          role: system
        - location: message
          index: -1
      input_cost_per_token: 0.000003
      output_cost_per_token: 0.000015
    model_info:
      max_input_tokens: 200000
  - model_name: auto
    litellm_params:
      model: auto_router/complexity_router
      input_cost_per_token: 0.000003
      output_cost_per_token: 0.000015
      complexity_router_config:
        classification_mode: user_turn
        tiers:
          SIMPLE: gpt-mini
          MEDIUM: gpt-main
          COMPLEX: claude
          REASONING: claude
      complexity_router_default_model: gpt-main
```

Make a router of this type available to clients that use the
Chat Completions API. A client that uses the Responses API or Anthropic's
Messages API sends the provider's own reasoning items back with each turn.
When a conversation moves between providers, these items from the other
provider are dropped, or the request can be refused. On each provider's model,
the first turn costs the full input price for the conversation. Refer to
"The tiers decide what a router saves" above.

A Team's router can also use an Agent's GitHub Copilot models as tiers. On the
e2e cluster, a router classified with Microsoft-Decision-1. Its tiers were the
Gemini, Claude, and Grok models of an Agent's Copilot, and a GPT model on
Azure OpenAI. Each of 27 requests was classified, and each tier answered. One
conversation went through all four models, and its last turn still had the
result of a tool call from an earlier turn. Streamed requests, `/v1/messages`,
and images also worked on each tier.

On the same cluster, the Copilot route of the Gemini model refused every
`reasoning_effort` value. The Copilot route of the Grok model refused `none`.
A router sends such a request to its next tier, after the failed call.

#### Measuring the routers

With the request log that Container Insights collects, the queries that follow
show how the routers route. For this log, refer to
[One log line per call](#one-log-line-per-call). Run a query on the Logs page
of the Log Analytics workspace. Select the time range on that page. When more
than one Gateway writes to the workspace, add
`| where PodNamespace == "<namespace>"` under the line that contains
`ContainerLogV2`.

The first five queries count only the calls that clients sent. A call that
LiteLLM makes itself carries a `call_origin`, for example a health check probe
or a classifier call. A routed call has a `routing_cause`. A routed call
without a `routing_tier` went to the router's default model, for example
because its classifier failed.

For each router, tier, and model that served the call, the next query shows
these values:

- The answered calls and their share of the router's answered calls.
- The failed tries.
- The answered calls that an escalation keyword or the length of the prompt
  moved one tier up.

A request that failed on one deployment and then went to another writes a line
for each try. As a result, `failed` counts tries:

```kusto
let routed = ContainerLogV2
| where ContainerName == "gateway"
| where tostring(LogMessage.log_type) == "cetirun.ai_gateway.request"
| where isempty(tostring(LogMessage.call_origin)) and isnotempty(tostring(LogMessage.routing_cause))
| extend router = tostring(LogMessage.model), tier = tostring(LogMessage.routing_tier),
    model = tostring(LogMessage.provider_model), ok = tostring(LogMessage.status) == "success";
routed
| summarize answered = countif(ok), failed = countif(not(ok)),
    escalated = countif(ok and tostring(LogMessage.routing_escalated) == "true") by router, tier, model
| join kind=inner (routed | summarize router_answered = countif(ok) by router) on router
| project router, tier, model, answered, share_pct = round(100.0 * answered / router_answered, 1),
    failed, escalated
| order by router asc, answered desc
```

A request whose tier failed, and that another model then answered, is not in
`answered`. Its answered line has no routing fields, and its `fallback_from`
names the router. The next query counts these calls by the model that the
request was for and by the model that answered:

```kusto
ContainerLogV2
| where ContainerName == "gateway"
| where tostring(LogMessage.log_type) == "cetirun.ai_gateway.request"
| where isnotempty(tostring(LogMessage.fallback_from))
| summarize calls = count() by requested = tostring(LogMessage.fallback_from), model = tostring(LogMessage.model),
    cause = tostring(LogMessage.fallback_cause), status = tostring(LogMessage.status)
| order by requested asc, calls desc
```

The next query shows what each tier spent, in total and for each answered call.
It also shows the cost to classify the requests that went to the tier
(`classifier_cost`, with a model classifier):

```kusto
ContainerLogV2
| where ContainerName == "gateway"
| where tostring(LogMessage.log_type) == "cetirun.ai_gateway.request"
| where isempty(tostring(LogMessage.call_origin)) and isnotempty(tostring(LogMessage.routing_cause))
| summarize answered = countif(tostring(LogMessage.status) == "success"),
    spend = sum(todouble(LogMessage.spend)), classifier_cost = sum(todouble(LogMessage.classifier_cost))
    by router = tostring(LogMessage.model), tier = tostring(LogMessage.routing_tier)
| extend spend_per_call = spend / answered
| order by router asc, spend desc
```

The next query shows how much of each tier's input the provider read from its
prompt cache. All input tokens include the cached input tokens. Over answered
calls, the query gives the cached input tokens as a share of all input tokens:

```kusto
ContainerLogV2
| where ContainerName == "gateway"
| where tostring(LogMessage.log_type) == "cetirun.ai_gateway.request"
| where isempty(tostring(LogMessage.call_origin)) and isnotempty(tostring(LogMessage.routing_cause))
| where tostring(LogMessage.status) == "success"
| summarize prompt_tokens = sum(tolong(LogMessage.prompt_tokens)),
    cached_tokens = sum(tolong(LogMessage.cached_tokens))
    by router = tostring(LogMessage.model), tier = tostring(LogMessage.routing_tier)
| extend cached_pct = round(100.0 * cached_tokens / prompt_tokens, 1)
| order by router asc, tier asc
```

The next query shows how long the answered calls of each tier took. For
streamed calls, it also shows the time until the first token:

```kusto
ContainerLogV2
| where ContainerName == "gateway"
| where tostring(LogMessage.log_type) == "cetirun.ai_gateway.request"
| where isempty(tostring(LogMessage.call_origin)) and isnotempty(tostring(LogMessage.routing_cause))
| where tostring(LogMessage.status) == "success"
| summarize answered = count(), avg_ms = round(avg(todouble(LogMessage.duration_ms))),
    p95_ms = percentile(todouble(LogMessage.duration_ms), 95),
    first_token_p95_ms = percentile(todouble(LogMessage.time_to_first_token_ms), 95)
    by router = tostring(LogMessage.model), tier = tostring(LogMessage.routing_tier)
| order by router asc, tier asc
```

The next query shows the model classifiers' own calls, for each classifier
model. It shows how many there were, how many failed, what they cost, and how
long they took. A classified request waits for its classifier before its model
is called. The `classifier_cost` of the routed calls repeats the spend of the
classifier calls. As a result, add one or the other to a router's spend, but
not both:

```kusto
ContainerLogV2
| where ContainerName == "gateway"
| where tostring(LogMessage.log_type) == "cetirun.ai_gateway.request"
| where tostring(LogMessage.call_origin) == "autorouter_classifier"
| summarize calls = count(), failed = countif(tostring(LogMessage.status) == "failure"),
    spend = sum(todouble(LogMessage.spend)), avg_ms = round(avg(todouble(LogMessage.duration_ms))),
    p95_ms = percentile(todouble(LogMessage.duration_ms), 95)
    by classifier = tostring(LogMessage.model)
```

**The cost of the same requests on the most expensive tier is not in the
log.** To calculate this cost, the prices of that tier's model are necessary,
and no log line carries these prices. The token counts of the same requests on
that model are also necessary, and nothing records them. Another model counts
the prompt in its own tokens, answers at its own length, and keeps its own
prompt cache. The spend for each answered call compares tiers on different
requests, not on the same requests.

LiteLLM 1.105.1 makes an estimate of this cost itself. The Cost Optimization
page of the LiteLLM console shows the estimate in the Auto-Router tab, and
LiteLLM marks this page as experimental. Only proxy admins see this tab, and
the tab reads `GET /auto_router/benchmarks`. A LiteLLM license is not
necessary. LiteLLM prices a routed request's own tokens at the prices of the
most expensive model in the router's highest tier. Then it subtracts what the
request and its classifier cost.

Except for some requests through the Anthropic Messages API, LiteLLM prices
only requests that did not read from or write to a prompt cache. It counts
every other request as a request that saved nothing. An agent sends its
conversation again with every turn, and providers cache the conversation. As a
result, these other requests can be most of an agent's requests. LiteLLM fills
the tables of the tab only while it writes its spend log (`spendLogs.enabled`,
on by default). The information about this estimate comes from LiteLLM's
source, and the estimate was not tried here.

None of these figures shows if the cheaper tiers answered as well as the most
expensive tier can. To find this, the requests themselves are necessary.

### Settings saved in the console

These settings are in LiteLLM's `router_settings` and `general_settings`. When
you save the Router Settings page of the console, LiteLLM writes the whole block
to the database. Then the database takes precedence over the chart. Refer to
[Editing settings in the console takes them out of chart control](#editing-settings-in-the-console-takes-them-out-of-chart-control).

## Administration

The admin console is enabled. The Gateway serves it on the same port as the
API. The chart creates no Ingress. Use a port-forward to reach the console.
Resource names start with the release name. For a release called `gateway`,
the command is:

```bash
kubectl port-forward svc/gateway-cetirun-ai-gateway 4000:4000
```

Then open <http://localhost:4000/ui>. Sign in as `admin`, with the console
password from the `ui-password` key of the `-auth` Secret:

```bash
kubectl get secret gateway-cetirun-ai-gateway-auth -o jsonpath='{.data.ui-password}' | base64 -d; echo
```

On Windows, the port-forward window prints lines such as `error copying from
local connection to remote stream ... An existing connection was forcibly
closed` when an agent is in use. These lines occur because the client on your
own machine drops idle connections when it opens new ones. The requests at
those moments completed normally. The lines are important only if the agent
shows an error at the same time, or if the port-forward itself exits.

The console password is separate from the master key on purpose. If LiteLLM
has no configured UI password, it accepts the master key instead. In that case,
a console sign-in gives full access to the management API.

`auth.adminUI.enabled: false` sets LiteLLM's `DISABLE_ADMIN_UI`. In v1.101.0,
this makes the console page say that the console is disabled, and it disables
SSO sign-in. But the sign-in endpoint still answers. When the master key was
posted to `/login`, the endpoint returned a console session. This session
grants nothing that the master key does not already grant. As a result, treat
the setting as a way to hide the console, not as a way to remove it.

### Calling the Gateway

Clients in the cluster reach the Gateway at its Service,
`http://<release>-cetirun-ai-gateway.<namespace>.svc:4000`. The chart creates
no Ingress or load balancer. For clients outside the cluster, expose the
Service yourself, with TLS. For a first test, the port-forward above is enough.
A `LoadBalancer` Service type alone is not sufficient. The Service serves plain
HTTP, and the console and the management API use the same port as the model
API.

Give each client its own virtual key. Use the master key to create the key.
Give the key a name, the models that the client can use, and a budget. For
`allowed_routes`, refer to
[Give agent keys model routes only](#give-agent-keys-model-routes-only). For a
release called `gateway`, with the minimal values of [Install](#install):

```bash
MASTER_KEY="$(kubectl get secret gateway-cetirun-ai-gateway-auth \
  -o jsonpath='{.data.master-key}' | base64 -d)"
CLIENT_KEY="$(curl -s http://localhost:4000/key/generate \
  -H "Authorization: Bearer ${MASTER_KEY}" -H 'Content-Type: application/json' \
  -d '{"key_alias": "example-client", "models": ["gpt-4o"], "max_budget": 20,
       "allowed_routes": ["llm_api_routes", "/v1/models", "/models"]}' | jq -r .key)"
```

`CLIENT_KEY` is the client's key. An OpenAI-compatible client takes the
Gateway's address with `/v1` and the key, for example as `OPENAI_BASE_URL` and
`OPENAI_API_KEY`. The client names a model from `models`:

```bash
curl -si http://localhost:4000/v1/chat/completions \
  -H "Authorization: Bearer ${CLIENT_KEY}" -H 'Content-Type: application/json' \
  -d '{"model": "gpt-4o", "messages": [{"role": "user", "content": "What is 2+2?"}]}'
```

To call a router, such as `auto` in [Smart routing](#smart-routing), add its
name to the `models` of the key, and name it as the model.

A response through a router tells how it was routed.
`x-litellm-complexity-router-tier` names the tier. `-cause` names what decided
the tier, and `-score` gives the heuristic score, if there is one. After a
failed `jev` classification, the response has no tier, and `-cause` is
`default_model_fallback`. The request log has the model that answered. Refer to
[Smart routing](#smart-routing).

### Revoking a key takes up to a minute to reach every replica

The replica that handled the deletion refuses a deleted virtual key
immediately. On the other replicas, the key continues to work for up to 60
seconds. Each replica caches key lookups in memory for that time, and
**Redis does not change it**. In a measurement with Redis configured, the
other replica accepted the deleted key for 55 to 60 seconds. Without Redis,
the result was the same.

`general_settings.user_api_key_cache_ttl` sets this window, and its default
is 60. When a test set it to 10, the window decreased to 5 to 10 seconds. On a
standalone Gateway, the chart does not set this value. With
`operatorIntegration`, the chart sets it to 0. The environment variable
`DEFAULT_MANAGEMENT_OBJECT_IN_MEMORY_CACHE_TTL` seems to control this window,
but it does not. LiteLLM passes its own default of 60 to the key cache, so
LiteLLM never reads the variable.

LiteLLM can keep archives of deleted keys. `/key/info` can return HTTP 200 with
`deleted_at` set. But the key is absent from the live `/key/list` results, and
it cannot authorize requests. The Operator treats this archive as an absent
key, not as current evidence of the models or the budget. This does not remove
the accounting history. It also does not change the replica cache window that
is described above.

### Give agent keys model routes only

A virtual key that is created through the management API with default settings
cannot create, change, or list keys. It also cannot add or delete models, or
read spend logs. But it can call `/health`, and each call makes a real request
to the backend. No budget is charged for this request. A leaked key can use
this to generate upstream traffic.

Keys that are created in the console as the "AI APIs" type can use only model
routes, and `/health` refuses them. Create keys through the API in the same
way:

```json
{"models": ["gpt-4o"], "max_budget": 20, "allowed_routes": ["llm_api_routes"]}
```

LiteLLM puts the lookup of one model, `GET /v1/models/<model>`, in its
internal-user routes, not in the model routes. As a result, such a key gets a
403 for that lookup, but `GET /v1/models` works. Clients that query a model
before they use it, for example Hermes, must have access to this lookup. For
these clients, add the two prefixes. The prefixes match only that lookup and
Google's `generateContent` routes, which are model routes already. The keys
that the CetiRun operator issues have the prefixes, and the operator adds them
to the keys that it issued before:

```json
{"allowed_routes": ["llm_api_routes", "/v1/models", "/models"]}
```

### Serving CetiRun Agents

The CetiRun operator gives each Agent a key that is limited to access groups.
It registers the Agent's GitHub Copilot models in the Agent's own group,
`cetirun-copilot-<Agent UID>`. Each of these models uses the proxy of that
Agent. For such a Gateway, set `operatorIntegration: true`. The
`cetirun-platform` chart sets it for the Gateway that it installs. This setting
reserves the `copilot/` prefix (refer to [Reserved names](#reserved-names)),
and it sets `general_settings.enforce_fallback_model_access`.

An Agent sees one name for each model. If a Copilot model is the same model as
a shared model that the Agent is granted, it uses the name of the shared model.
For each deployment of the shared model, the real model is its
`model_info.base_model`. If a deployment has none, the real model is its
`litellm_params.model`, when LiteLLM's price table prices that name. All
deployments of the shared model must agree.

Both names are compared without the provider prefix, in lower case, and with
dots changed to dashes. As a result, `claude-sonnet-4.5` matches
`anthropic/claude-sonnet-4-5`. A Copilot model with a date version, such as
`gpt-4.1-2025-04-14`, is never merged. When several shared models match, the
first name in sort order is used.

Such a route copies the routing fields of the shared model. It takes the lowest
`litellm_params.order` of the shared model's deployments. Of the deployments at
that order, it takes the largest `weight`, `rpm` and `tpm` that any of them
sets. Then `router.routingStrategy` spreads the Agent's requests over the
shared deployments and the Agent's own route. LiteLLM's default cooldown
removes a route that fails from rotation. Refer to
[Failover](#a-failed-request-goes-to-another-deployment).

A request can fail to connect, for example when the Agent's proxy is down.
LiteLLM does not cool down the route for such a request, and it does not retry
the request elsewhere. By default, the operator registers the Copilot routes
without health checks (refer to [Health checks](#health-checks)). As a result,
such a route keeps its share of the model's requests until the proxy is back.
The requests that go to the route fail. In a measurement with a route whose
proxy refused connections, 4 of 20 requests failed.

When the operator's `aiGateway.copilotProxy.healthChecks.enabled` is on, the
health checks remove the route from rotation at their next probe
(`router.healthChecks`). The cost is that the probes make calls on the owner's
Copilot allowance. In both cases, spend is counted at the list price of the
model.

Any other Copilot model uses its own ID as its name, such as `gpt-5.5`, with
`litellm_params.cooldown_time: 0`. Under that name, it is the only deployment
that the Agent's key reaches. As a result, a 429 from Copilot goes to the Agent
as it is. The 429 does not remove the route from rotation for every request.

If a model that the Gateway serves already has that name and is a different
real model, the Copilot model is omitted. It is reported on the Agent
(`status.aiGateway.copilot.skipped`). That model can also be the same real
model, for example a shared model that the Agent is not granted. In this case,
the route takes that name in the Agent's own access group. There, the Agent's
key reaches only its own Copilot route, and never the shared deployments.

For an Agent with an AgentPolicy, the policy's `sharedModels.allowed` also
applies to Copilot models. It applies to each Copilot model that is the same
real model as a shared model, by the comparison above. Such a Copilot model is
registered only when the policy allows one of the shared models that are that
model. Otherwise it is omitted and reported on the Agent
(`status.aiGateway.copilot.notAllowed`). For a Copilot model that is not the
real model of a shared model, only `modelAccess.copilot.allowed` is necessary.

When the policy no longer allows such a model, the operator removes the model's
route before it updates the Agent's key. All of the Agent's Copilot routes are
in the Agent's one access group. While a route cannot be removed, the key does
not hold that group, and `status.aiGateway.copilot.reason` is
`NotAllowedRouteRemovalFailed`. The key also does not hold it while the operator
cannot read the Gateway's models or prices (`PolicyCheckUnavailable`). The key
holds the group again when the route is removed.

The operator removes the `copilot/<model>` aliases that earlier versions put on
the Agent's key. These aliases listed each Copilot model a second time in the
model list of the key. Other aliases on the key are kept. When the shared
models change, the operator registers the routes again with the names and
fields that then apply.

The operator configures an Agent to send images with a request only when every
model that can answer the request accepts images ([Images](#images)). For a
shared model, every deployment must accept images. The Agent's own Copilot route
under the model's name must also accept them. A Copilot route takes the value of
the price table entry that it is billed as.

For a router, every model of its tiers, its default model, and its context
compaction model must accept images. By default, a router does not examine
images when it selects a model (LiteLLM's `modality_routing` is off). As a
result, a request with images can go to any of these models. A model that does
not accept images can refuse the request. Agents do not send images to a router
whose configuration the operator cannot read completely.

LiteLLM validates a key against the requested model only. A fallback that is
configured for the Gateway in `router_settings` or in the console is selected
after that validation. Without the setting, LiteLLM tries the fallback without
validation. For a model name that several Agents share, LiteLLM then selects
from all of them if the caller's groups match none. As a result, a fallback
from a shared model to a Copilot model can reach the proxy of a different
Agent. With the setting, LiteLLM skips a fallback target that the caller's key
cannot request directly.

The groups separate Agents only when keys and teams are limited to the groups.
If a key is granted a model only through access groups, LiteLLM limits the key
to the deployments in those groups. But a key or team that is allowed every
model, or a model name directly, has no group to filter by. As a result,
LiteLLM can spread the requests of that key for a name that Copilot routes
share over the proxy of every Agent.

The image's guard callback prevents this. A key reaches a per-Agent GitHub
Copilot route only if it holds the access group of that route. Keys that are
granted every model, such as the master key, reach only the shared deployments
of a shared model's name. The operator creates teams and keys without such
grants. Do not add these grants in the console.

### The guard callback

The image's entrypoint always registers a second callback, the guard, before
LiteLLM loads its configuration. The `requestLog` value does not change this.
Settings that are saved in the console cannot remove the guard. The guard does
these actions:

- It drops request fields that a caller can use to change the Gateway for its
  own call. These fields are `litellm_request_debug` and the retry fields that
  are described in
  [Upstream errors are not retried](#upstream-errors-are-not-retried), also in
  `router_settings_override`.
- It gives a request to a router fallbacks to the router's other models, and it
  keeps LiteLLM's internal names out of errors. Refer to
  [Smart routing](#smart-routing).
- It excludes the per-Agent GitHub Copilot routes from a request, unless the
  caller's key holds the route's access group, `cetirun-copilot-<Agent UID>`.
  A key granted every model, such as the master key, reaches none of these
  routes. A key that holds `*` or the model name of a route also reaches none
  of them. The same applies to a request whose key LiteLLM does not pass on.
  If such a key asks for a name that only Copilot routes serve, it gets
  LiteLLM's "No deployments available" 429. The same rule applies to the tier
  that an auto router selects.
- It limits a key that the CetiRun operator issued to the deployments in the
  key's access groups. It does this also where LiteLLM does not. LiteLLM
  validates a key against the model that a request names. When that model is
  an auto router, LiteLLM does not validate the model that the router selects.
  The operator grants a router only to Agents that are allowed to use every
  model that the router sends requests to. The guard enforces this on each
  request, also before the operator revokes a grant after a configuration
  change.

  The router can send a request to a model that the key is not allowed to use.
  This request gets a 403, "This key may not use the model <name>.", with no
  retry, fallback, or cooldown. The Gateway logs a warning with the model and
  the key's alias. The request log line of such a call has the status
  `failure`, `error_code` 403, and the router's name as `provider_model`. As a
  result, the queries in [Measuring the routers](#measuring-the-routers) count
  it as a failed try of its tier, under the router's name. If the key is
  allowed to use the model, but none of its deployments is available,
  LiteLLM's own error applies.

  A router with `classifier_type: llm`, or with another classifier that asks
  a model, calls its classifier model. This call is made on behalf of the key,
  and it is not limited to the key's groups. A decision model
  (`mode: evaluation`) answers only those classifier calls. If a key that the
  operator issued sends its own request to the decision model, the request
  gets the same 403. In a measurement on the e2e cluster before the guard did
  this, the provider refused the direct request. LiteLLM cooled down the
  deployment, so every router that classified with it used its default
  classifier instead.

  The guard identifies the operator's keys by their model list. This list
  contains only `cetirun-` access groups, or `no-default-models`. The guard
  leaves the master key, and keys that are granted models by name, `*`, or
  every model, to LiteLLM's own checks.
- It refuses a request for a decision model from any caller, through any API
  except LiteLLM's Decisions API (`/v1/systemone` and `/v1/decisions`). A
  decision model is a model whose every deployment sets `mode: evaluation`. A
  chat, Responses, Anthropic Messages, or embeddings request for such a model
  gets a 400. The message is "The model <name> is a decision model: call it
  through /v1/systemone or /v1/decisions." The guard refuses the request
  before any deployment is called, with no retry, fallback, or cooldown. The
  Gateway logs a warning with the model, the API, and the key's alias.

  A measurement on a Gateway without the operator, before the guard did this,
  gave these results. A chat request for Microsoft-Decision-1 from a key that
  was created in the console got the provider's 404. LiteLLM cooled down the
  deployment. For about four seconds, every router that classified with it
  used its default classifier, for every caller. The Decisions API and a
  router's own classifier calls reach the model as before. A key that the
  operator issued still gets the 403 that is described above.
- It shortens the request that a router with a jev classifier sends to its
  decision model. The limit in bytes starts at the number of tokens that the
  model takes. This number is the smallest `model_info.max_input_tokens` of
  the decision model's deployments, or 32,768 when none sets it. From this
  limit, the guard subtracts the size of the questions that LiteLLM asks, and
  512 bytes for the service's own template.

  LiteLLM sends the caller's whole system prompt and its latest user messages
  to the classifier, and it does not limit their length. Microsoft-Decision-1
  takes 32,768 tokens, and it refused a longer request with a 422. The router
  then classified the request with its fallback, without an error. Each token
  of a decision model represents at least one byte of text. As a result, the
  shortened request fits in any language.

  The guard keeps the newest user message whole if it fits. It also keeps the
  start and the end of the rest. If the newest message does not fit, the guard
  keeps its start and its end. The Gateway logs a warning with the decision
  model and the sizes, but not with the content. The guard does not change a
  client's own calls to the Decisions API.

  A measurement on LiteLLM 1.105.1 with Microsoft-Decision-1 used requests with
  a system prompt of 40,000 English words or 60,000 Chinese characters. It also
  used requests with a 120 KB log as the message. The guard shortened these
  requests to 31.7 KB. Microsoft-Decision-1 counted the shortened requests at
  5,687 to 13,300 tokens, and the decision model classified each request.
  Without the guard, the decision model refused each request, and the
  heuristic scorer classified them.
- It does not let a key that the CetiRun operator issued list what the key
  cannot send requests to. For such a key, `GET /v1/models` (and
  `/model/info`) does not show the key's access groups that LiteLLM lists as
  if they were models. An example is an Agent's Copilot group before its owner
  signs in. These endpoints also do not show models whose every deployment
  sets a `mode` other than `chat`, `completion` or `responses`. An example is
  a decision model that a router classifies with. For these entries,
  `GET /v1/models/<name>` answers 404.

  The operator also omits them from the Agent's configuration. Other callers
  see the list that LiteLLM gives.
- It lists a Team's router under the router's name only, for a key that has
  an alias for the router. The CetiRun operator gives an Agent's key such an
  alias for each Team router that the Agent can use. The alias is the router's
  name, and its target is the internal `cetirun-router-` name. LiteLLM lists a
  key alias next to its target, so the router was listed two times, one time
  under the internal name. `GET /v1/models/<internal name>` still answers.

  A measurement on LiteLLM 1.105.6 with such a key gave these results.
  `GET /v1/models` listed the router one time, by its name. A chat request for
  the router's name was routed. A per-model limit under the internal name
  applied to these requests, and a limit under the router's name did not apply.
- While `router.encryptedContentAffinity` is on, it removes the encrypted
  content of some Responses API reasoning items, but it keeps their summaries.
  It removes this content from items that carry no mark of the deployment that
  produced the content, or that a client shortened. With this setting on, it
  also makes LiteLLM write that mark in a form that clients keep. With this
  setting on, it also stops the fallbacks of a request after a deployment
  refuses the encrypted reasoning of that request. Refer to
  [Encrypted reasoning stays with its resource](#encrypted-reasoning-stays-with-its-resource).
- It removes sampling parameters from a request to a reasoning model of the
  GPT-5 or GPT-6 series. The model can be one that the request names, or one
  that an auto router selects. The parameters are `temperature` other than 1,
  `top_p`, `logprobs` and `top_logprobs`. For such a model, LiteLLM refuses
  these parameters with a 400, unless the reasoning effort resolves to `none`.

  For example, Hermes sends side tasks such as conversation titles with
  `temperature: 0` to the model that the user selected or to the router. Each
  time that model was a reasoning model, these tasks failed on both Azure and
  Copilot routes. The user did not see the failures. LiteLLM's own rule
  decides, and the Gateway logs a warning for each request that it changes.
- It removes `reasoning_effort: none` from a Chat Completions request to an
  Azure deployment of the GPT-5 or GPT-6 series. This applies when LiteLLM's
  price table does not mark the model of the deployment as one that takes this
  value. LiteLLM refuses such a request with a 400 before it calls Azure.
  Hermes sends side tasks with `none`. In a measurement on the e2e cluster, a
  router sent one of these tasks to such a tier. The task failed, and the user
  did not see the failure.

  The model then reasons at its default effort, which is the same result that
  LiteLLM's `drop_params` gives. The guard also removes the sampling
  parameters that the model then refuses, as described above. The Gateway logs
  a warning for each request that it changes. The table decides, so a wrong
  entry lets `none` through. Refer to
  [Prices are not pinned to the image by default](#known-gaps).
- It lets a request fail over from the deployment that its key is kept on.
  Refer to [A key stays on its deployment](#a-key-stays-on-its-deployment).
- It removes fields that Azure OpenAI refuses from the items of a Responses
  API request to an Azure OpenAI deployment. These fields are the `status` of
  a reasoning item, and an item ID that is longer than 64 characters. GitHub
  Copilot models return items with them. Refer to
  [Encrypted reasoning stays with its resource](#encrypted-reasoning-stays-with-its-resource).
- It prevents a GitHub sign-in when LiteLLM prices a request that an auto
  router answered. LiteLLM 1.105.6 compares the cost of such a request with
  the most expensive model of the router's hardest tier. LiteLLM gets the
  provider of that model from `get_llm_provider`. For a model whose
  `base_model` starts with `github_copilot/`, this function asks GitHub for a
  token. The operator registers some GitHub Copilot models in this form.

  The Gateway has no GitHub token, so LiteLLM started GitHub's interactive
  device sign-in and waited for a user. On the e2e cluster, the hardest tier
  of a router was such a Copilot model. After the router answered one request,
  a Gateway replica stopped serving for about three minutes. Then its liveness
  probe restarted the container. The guard makes LiteLLM take such a provider
  from the model name, as LiteLLM does in its other lookups.
- It removes LiteLLM's internal deployment list from error messages. Error
  messages that are returned to callers no longer include LiteLLM's internal
  list of deployments in cooldown. The response keeps its status, 429, and the
  "Try again in N seconds" text. On the Chat Completions and Responses APIs,
  the response also keeps the `retry-after` header. LiteLLM sets no such
  header on its Anthropic Messages API. The Gateway's own log and the request
  log keep the full message.

When `operatorIntegration` is off, the guard's request fields, its decision
model rules, its parameter changes, and its error messages apply in the same
way. A standalone Gateway has no `cetirun-copilot-` groups for the guard to
separate, and no keys that the operator issued.

A measurement on LiteLLM 1.105.1 used two replicas and two Copilot routes. The
routes were added through the management API under one `copilot/` name. Each
of two Agent keys reached only its own route, 6 of 6. A key granted every proxy
model, a key granted the `copilot/` name, and the master key got the 429. The
429 for a model with every deployment in cooldown contained no deployment list
on the Chat Completions, Responses, and Anthropic Messages APIs.

Another measurement put the routes of two Agents under the name of a shared
model, next to its two shared deployments. It also put them under a name of
their own. For each Agent, 12 keys sent 5 requests each. These requests reached
the shared deployments and the Agent's own route, but never the route of the
other Agent.

Shared-only keys, 12 keys granted every proxy model, and the master key reached
only the shared deployments. For the name that only Copilot routes serve, the
master key got the 429. A key granted every proxy model and a key granted the
name directly also got the 429.

A route with the copied `order: 1` was next to a primary and a standby. It
took requests together with the primary, and no requests went to the standby.
When the primary failed, all requests of a shared-only key went to the standby.
A route of its own name that answered 429 forwarded the 429, and it served the
next request immediately after. Without `cooldown_time: 0`, the same route
answered "No deployments available" until its cooldown ended, a few seconds
later.

A measurement on LiteLLM 1.105.1 used one replica and mock models, each in an
access group of its own. It also used routers over these models, configured as
the chart renders them. A key granted a router and the models of its simple
and medium tiers was served on those tiers. It got the 403 for the router's
complex tier. These results occurred on the Chat Completions API, streamed or
not, and on the Responses and Anthropic Messages APIs.

The model of the complex tier received no call. A key granted that model was
served by it immediately after. A router that was registered through the
management API behaved the same. The operator registers a Team's routers in
this way. The GitHub Copilot tier of this router reached only the caller's own
route, 6 of 6.

A router with `classifier_type: llm` called its classifier model for a key that
was not granted the classifier model. Then it served the tiers that the key
holds. A key granted the router by name and the master key reached every tier.
The request log records a refused request with status `failure` and error
code 403. It writes one line, or two lines for a streamed request.

The same results occurred on a test installation with two replicas and Azure
OpenAI models. With a router configured in the chart, a key granted the router
and its simple tier's model was answered on that tier. The same key was refused
with the 403 on the medium and reasoning tiers of the router. These results
occurred on the Chat Completions and Responses APIs. A key granted every tier
was answered on each tier.

On the same installation, a Team's router with an LLM classifier gave these
results for a key not granted the classifier model. The key got the
classifier's decision and answers from the tiers that it holds. It got the 403
for the other tier, also when the request claimed to be a classifier call.

## Credentials

There are four different credentials:

| Credential | Held by | Used for |
| --- | --- | --- |
| Provider API key | Gateway | Calls to OpenAI, Anthropic, Azure OpenAI, and other providers |
| Virtual key | Each agent | Calls to this Gateway |
| Master key | Operators and scripts | LiteLLM's management API |
| Console password | A human administrator | Sign-in to the admin console |

When you do not supply a Secret that already exists, the chart generates the
master key, salt key, and console password. On upgrade, the chart reads each
value back from the live Secret. It does not generate the value again.

To supply your own values, create the Secrets before the installation, and
name them in these values. One Secret can hold several of the keys:

```yaml
auth:
  masterKey:
    existingSecret: gateway-auth     # key master-key
  saltKey:
    existingSecret: gateway-auth     # key salt-key
  adminUI:
    passwordSecret:
      existingSecret: gateway-auth   # key ui-password
redis:
  passwordSecret:
    existingSecret: gateway-valkey   # key redis-password
```

Each value also has a `key` field for another key name. Give the master key
the `sk-` prefix, as the chart does when it generates one. The chart generates
`sk-` and 48 letters and digits.

The chart depends on Helm's `lookup` to read the values back. This function
returns nothing when a chart is rendered with `helm template`. In a test, two
renders produced two different salt keys. Argo CD renders charts in that way.
As a result, if Argo CD renders the chart, every sync generates new values,
also for the salt key.

With Argo CD, or with any tool that renders with `helm template`, supply all
three values through Secrets that already exist. Also use an external
PostgreSQL, because the bundled PostgreSQL generates its password in the same
way.

The chart also generates the password of the bundled Valkey in the same way.
Supply it through `redis.passwordSecret`, or use your own Redis. A password
that is generated again at each render updates the Secret, but it does not
restart Valkey or the Gateway. As a result, if the next restart affects only
one of them, the two have different passwords. Then the replicas no longer
share budgets. When `redis.passwordSecret` is set, the chart generates no
Valkey password, and both Valkey and the Gateway read your password.

### Changing a credential takes a restart

The Gateway reads its credentials from environment variables when it starts.
After you change a provider key, the master key, or the console password in a
Secret, restart the Gateway:

```bash
kubectl rollout restart deployment/gateway-cetirun-ai-gateway
```

In a measurement, a provider key changed in its Secret. A minute later, both
replicas still sent the old key. A `helm upgrade` with unchanged values did not
restart them. After the restart, both replicas sent the new key. Keep the old
provider key valid until the rollout finishes. For this purpose, Azure OpenAI
issues two keys for each resource.

Inline credentials (`providerCredentials.inline`) are the exception. When an
inline value changes, the chart restarts the pods itself.

The Gateway also reads a Redis password from `redis.passwordSecret` at
startup. With the bundled Valkey, the server also reads this password. Until
both restart after a change, the replicas do not share budgets. After you
change the password, restart both:

```bash
kubectl rollout restart deployment/gateway-cetirun-ai-gateway-valkey
kubectl rollout restart deployment/gateway-cetirun-ai-gateway
```

The password of the bundled PostgreSQL takes one more step. The database
applies `POSTGRES_PASSWORD` only when it initializes an empty volume. As a
result, if you change only the Secret, the `litellm` role keeps its old
password, and the next Pods of the Gateway cannot connect.

First, change the role. Then make sure that the new password works. Then
change the Secret, which holds the password twice. Then restart the Gateway.

In the block, set `gateway` to the Gateway's Deployment name
(`<release>-cetirun-ai-gateway`, or `cetirun-ai-gateway` when the platform
chart installs it). Run the block in bash:

```bash
(
  set -euo pipefail
  gateway=gateway-cetirun-ai-gateway
  # The database's StatefulSet and Secret share a name, which the chart
  # shortens for a long release name: read it from the Gateway.
  ref='{.spec.template.spec.containers[?(@.name=="gateway")].env[?(@.name=="DATABASE_URL")].valueFrom.secretKeyRef.name}'
  db="$(kubectl get deployment "$gateway" -o jsonpath="$ref")"
  [[ -n "$db" ]] || { echo "Deployment $gateway names no database Secret; nothing changed" >&2; exit 1; }
  kubectl get statefulset "$db" >/dev/null # only the bundled PostgreSQL has one
  # Read and check the database URL before changing anything.
  url="$(kubectl get secret "$db" -o jsonpath='{.data.database-url}' | base64 -d)"
  [[ "$url" =~ ^postgresql://litellm:[^@/]+@[^@/]+/litellm$ ]] ||
    { echo "unexpected database URL in Secret $db; nothing changed" >&2; exit 1; }
  password="$(openssl rand -hex 24)"
  url="postgresql://litellm:${password}@${url#*@}"
  # Encode the Secret's new values now, so that a failure changes nothing.
  password64="$(printf '%s' "$password" | base64 | tr -d '\n')"
  url64="$(printf '%s' "$url" | base64 | tr -d '\n')"
  [[ -n "$password64" && -n "$url64" ]] ||
    { echo "could not encode the new values; nothing changed" >&2; exit 1; }
  # Change the role over the Pod's local socket, which the database trusts.
  printf "ALTER ROLE litellm WITH PASSWORD '%s';\n" "$password" |
    kubectl exec -i "${db}-0" -c postgresql -- \
      psql -U litellm -d litellm -v ON_ERROR_STOP=1 -q -f -
  # Check the new password over the network, the way the Gateway connects.
  ip="$(kubectl get pod "${db}-0" -o jsonpath='{.status.podIP}')"
  [[ -n "$ip" ]] || { echo "Pod ${db}-0 has no IP" >&2; exit 1; }
  printf '%s\n' "$password" | kubectl exec -i "${db}-0" -c postgresql -- sh -c \
    'read -r PGPASSWORD && export PGPASSWORD && psql -h "$1" -U litellm -d litellm -tAc "select 1"' \
    sh "$ip" >/dev/null
  printf '{"data":{"password":"%s","database-url":"%s"}}' "$password64" "$url64" |
    kubectl patch secret "$db" --type merge --patch-file /dev/stdin
  kubectl rollout restart "deployment/${gateway}"
  kubectl rollout status "deployment/${gateway}" --timeout=10m
)
```

Each command runs only after the previous one succeeds. A failure before
`ALTER ROLE` changes nothing. A failure after it leaves different passwords in
the database and the Secret. Then the running Gateway keeps the connections
that it has, but a Gateway Pod that starts cannot connect. Fix the cause. Then,
before the Gateway restarts, run the block again.

Each run sets a new password, and it makes the database, the Secret, and the
Gateway use the same password again.

The password reaches `psql` and `kubectl patch` through their standard input,
not as an argument. As a result, the password does not show in process
listings.

In a measurement on AKS, requests continued to work during the full procedure.
The restarted Gateway connected with the new password. The database refused
other passwords over the network. A restart of the database Pod kept its data.
A later `helm upgrade` kept the new password, because the chart reads the
Secret back.

## Upgrade contract

### The salt key must never change

`LITELLM_SALT_KEY` encrypts the provider credentials that are stored in the
database. LiteLLM gives no procedure to rotate this key.

When the salt key changes, the proxy starts normally, and `/health/readiness`
reports healthy. But every model that is stored in the database disappears
from `/model/info` and `/v1/models`. The only signal is a warning that the log
repeats. If you restore the original value, everything recovers. As a result,
the danger is that you lose the value, not that you change it.

The chart keeps its generated value across upgrades. But the chart cannot
protect the value if the Secret is deleted. As a result, back up the value:

```bash
kubectl get secret gateway-cetirun-ai-gateway-auth -o jsonpath='{.data.salt-key}' | base64 -d
```

In PowerShell, where `base64` is not available, use these commands:

```powershell
$v = kubectl get secret gateway-cetirun-ai-gateway-auth -o jsonpath='{.data.salt-key}'
[Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($v))
```

If the Secret is deleted anyway, the next `helm upgrade` generates a new salt
key and master key without an error or a warning. The running pods continue
with the old values, so nothing looks wrong. The damage occurs at the next
restart. The cause of that restart is not important. In a measurement after
that restart, the models that were added through the console were not in the
model list, and the Gateway refused them. The Gateway refused the previous
master key with 401.

To recover, use these commands. They write the original value back into the
chart's Secret, and then they restart the Gateway:

```bash
kubectl patch secret gateway-cetirun-ai-gateway-auth --type merge -p \
  "{\"data\":{\"salt-key\":\"$(printf '%s' "$SALT_KEY" | base64)\"}}"
kubectl rollout restart deployment/gateway-cetirun-ai-gateway
```

In these commands, `$SALT_KEY` holds the value that you backed up. To get the
previous master key back, restore `master-key` in the same way. In a test with
both values restored, the models and the previous master key returned. The next
upgrade kept the restored salt key.

Readiness probes do not detect this failure. We recommend that an upgrade check
compares the model count before and after, or makes sure that the Secret is
unchanged.

### Uninstalling keeps the credentials Secret

`helm uninstall` does not remove `<release>-cetirun-ai-gateway-auth`. A later
`helm install` with the same release name and namespace uses it again.

The chart does this on purpose. The database stays after you uninstall the
release. This is true for an external database and for the volume of the
bundled database. As a result, before the chart kept the Secret, an uninstall
and then an install generated a new salt key for the old database. A
measurement gave these results:

| After uninstall and install | Secret deleted | Secret kept |
| --- | --- | --- |
| Salt key, master key, console password | all generated again | unchanged |
| Models added through the console | not in the model list, requests get 400 | served |
| Models in `models` | served | served |
| Virtual keys | still valid | still valid |
| Previous master key | 401 | still valid |

For the first column, the only sign was a decryption warning in the log. The
model rows stay in PostgreSQL, encrypted with a key that no longer exists.

To remove the Gateway completely, delete the Secret yourself after you
uninstall the release. Do this only if you also discard the database. If you
delete the Secret but keep the database, every model that was added through
the console becomes permanently unreadable.

### Upgrading LiteLLM

Build the image again for the new release (refer to [Image](#image)). Then
upgrade the release. Each new Gateway pod applies the migrations of the new
version in its init container before the proxy starts.

An upgrade from `v1.101.0` to `v1.102.0` with data in place was measured on
the upstream image, before the chart moved to its own image. Six migrations
applied. Virtual keys and their spend were kept. Models that were added
through the console were still decrypted and served. A budget that was partly
spent before the upgrade was enforced exactly after the upgrade.

Another measurement used an upgrade from 1.101.0 to 1.105.1, with the
entrypoint's `migrate` on the upstream 1.105.1 image. In six seconds, 30
migrations applied. The key and its spend were kept. A model that was added
through the console was still decrypted and served. The key's budget was
enforced after the upgrade.

From 1.105, LiteLLM builds two indexes on `LiteLLM_SpendLogs` after the
migrations, not in one of them. As a result, the building of these indexes
never blocks writes. The LiteLLM proxies build them only when they migrate
themselves. The chart's proxies do not migrate themselves.

As a result, the `migrate` step builds the indexes after the migrations.
Without these indexes, queries on the spend log are slower. If a build cannot
finish, the step does not fail. The migration of the next pod tries again.

### Rolling back

`helm rollback` to a revision with an older image works. In a test, the older
version started and passed readiness. It served both configured models and
models that were added through the console, and it kept the spend of the keys.

A rollback does not undo migrations. Nothing can undo them. As a result, the
older version runs with a newer schema. The older version detects this, and
it logs this message at every start:

```
LiteLLM:ERROR: check_migration.py - 🚨🚨🚨 prisma schema out of sync with db.
Consider running these sql_commands to sync the two - ['ALTER TABLE ... DROP COLUMN ...', ...]
```

**Do not run those commands.** They drop the columns that the newer version
added, and any data that was written to these columns. The newer migrations
stay recorded as applied. As a result, if you upgrade again, the upgrade does
not create the columns again. Then the newer version fails on these columns.

The error is harmless if you do nothing about it. The chart starts the proxies
with schema updates disabled, so nothing is applied automatically.

A rollback with `migrations.enabled: false`, where the proxies migrate
themselves, was not tested.

### Editing settings in the console takes them out of chart control

A change to any field of a settings block writes that whole block to the
database. This applies to a change through the admin console, and through a
management API that changes one. After that, the database wins on every key in
the block, and the related chart values no longer have an effect. There is no
warning.

This occurred twice. In the first case, a change to a single routing dropdown
in the console also froze `num_retries` and `cooldown_time`. Neither value was
changed. In another case, one API call to enable or disable the unpriced-model
block saved `router_settings`, `litellm_settings` and `general_settings`
together. This included everything that the chart configured.

To recover, delete the row from the `LiteLLM_Config` table. Then restart the
Gateway. This returns those keys to chart control.

For this reason, the chart writes as little as possible into those blocks.

### Values the chart owns

The chart owns these values:

- `model_list`.
- `general_settings.store_model_in_db`, the spend log retention and its
  deletion interval, `general_settings.store_prompts_in_spend_logs`, and the
  health check settings when `router.healthChecks` is on. With
  `operatorIntegration`, also `general_settings.enforce_fallback_model_access`
  and `general_settings.user_api_key_cache_ttl`.
- In `litellm_settings`: `num_retries`, `budget_exceeded_status_code`, the
  unpriced-model block, `global_disable_no_log_param`, the metrics callback and
  its settings, and the Redis cache when Redis is configured.
- In `router_settings`: Redis when it is configured, the routing strategy, and
  the failover, the affinity checks, and the fallbacks of `router`.

The chart does not change the other parts of LiteLLM's configuration. The
image's entrypoint applies the request log and `spendLogs.enabled` outside
these settings blocks. Refer to
[One log line per call](#one-log-line-per-call). Prompt storage and retention
are settings in the configuration file. As a result, settings that are saved
in the database can override them.

### Models added through the console survive upgrades

These models are in the database, not in the configuration file. A chart
upgrade does not remove them. A model that is added through the console can
have the same name as one in `models`. Such a model becomes another backend,
not an override.

### Upgrading with `--reuse-values`

`helm upgrade --reuse-values` keeps the values of the chart version that
installed a release. It does not include the defaults of settings that later
versions added. The chart reads each of these settings with its default, one
key at a time. As a result, a release whose `router` or `spendLogs` comes from
an earlier version gets the defaults of the keys that it does not have. A
release that was installed before these settings existed upgrades with these
values:

- `requestLog`, `spendLogs` and `metrics`: the request log is on, LiteLLM's
  spend log is kept, and metrics are off.
- `spendLogs.storePrompts`: `true`. As a result, prompts and responses are
  stored with the call's metadata, unless you explicitly disable this setting.
- `spendLogs.retention`: `7d`. As a result, LiteLLM starts to delete spend log
  rows that are older than seven days.
- `router`: `simple-shuffle`, failover and the affinity checks on, no
  fallbacks, and health checks every 300 seconds, with at most 8 probes at a
  time. These health checks start to probe every deployment, except the
  deployments that opt out. For example, the GitHub Copilot routes of the
  CetiRun operator opt out by default.
- `budgetExceededStatusCode`: 402.
- `tpmReservation`: `false`. As a result, a tokens-per-minute limit counts the
  tokens of finished requests.
- `podDisruptionBudget`: a PodDisruptionBudget for more than one replica.
- `priceTable`: `remote`. As a result, LiteLLM continues to download the price
  table.

## Availability

### Rollouts let requests in flight finish

When a replica is replaced or evicted, the proxy stops accepting new requests
and waits for accepted requests to finish. The chart sets the Pod's
`terminationGracePeriodSeconds` to 600 seconds and LiteLLM's
`GRACEFUL_SHUTDOWN_TIMEOUT` to 90% of that value: 540 seconds for in-flight
requests, leaving 60 seconds for shutdown cleanup before Kubernetes forcibly
stops the container. A replica exits when its work and cleanup finish; it does
not have to wait for the full budget. Increasing `terminationGracePeriodSeconds`
increases both budgets in the same proportion. A value of zero permits no wait.

LiteLLM 1.108.3 defaults its own shutdown timeout to 30 seconds when the
environment variable is absent. A longer Pod grace period alone therefore does
not protect long requests. The cluster test keeps a 45-second request in flight
while deleting its replica and requires the request to complete successfully.

In earlier cluster measurements, the Kubernetes default of 30 seconds cut off
a 90-second streamed response after 80 chunks; a longer period let it finish.
An idle replica exited four seconds after it was told to stop.

On a single-node test cluster, one of two replicas was deleted while requests
flowed through the Service. This was done both normally and by force. None of
100 requests failed.

On the e2e cluster, a node with one of two replicas was drained while one
request per second went through the Service. One of 183 requests failed with a
503, `upstream connect error`. This request got to the evicted replica as it
stopped. The new replica was ready 56 seconds later.

For more than one replica, the chart creates a PodDisruptionBudget for the
Gateway pods with `maxUnavailable: 1` (`podDisruptionBudget.enabled`, on by
default). As a result, a node drain evicts one replica at a time, and it waits
while another replica is unavailable. The chart does not create it for a single
replica. Because it uses `maxUnavailable`, not `minAvailable`, it does not
block a drain, also when the Deployment is scaled down to one pod.

The bundled PostgreSQL and Valkey have no PodDisruptionBudget. The chart sets
no anti-affinity. As a result, both replicas can still be on one node, and the
two replicas are lost together when that node fails. On a multi-node cluster,
use `affinity` to spread them.

### Pods keep their configuration until they are replaced

The generated configuration is mounted as a single file (`subPath`). As a
result, Kubernetes does not update it inside running pods. Instead, a
configuration change replaces the pods (`checksum/config`).

When the ConfigMap was mounted as a directory, Kubernetes updated the file in
the old pods about a minute after an upgrade. While models are stored in the
database, LiteLLM reads its configuration file again every 30 seconds. It
removes each configured model whose deployment ID is no longer in the file.
But it does not add the new entry until it restarts. As a result, after an
upgrade that changed a model, the old pods did not serve that model until they
were replaced.

Now each pod serves the configuration that it started with. The changed model
stays available during the full rollout.

During a rollout, the Service sends requests to old and new pods. When a model
is added, removed, or renamed, these pods can list different models.

The CetiRun operator lists the models through the Service. As a result, it
waits while the ready pods behind the Gateway Service belong to more than one
pod template. It examines the pods again every 15 seconds. When one template
serves the Gateway, the operator applies the new model list one time.

The operator keeps its previous view for at most ten minutes of a rollout. If
it cannot read the Gateway's Service or pods, it does not wait. While the
rollout runs, Agents are not updated or restarted.

### When Redis is unavailable

The Gateway continues to serve requests. Readiness does not depend on Redis,
so the replicas stay in the Service. In a measurement, Redis was stopped for up
to three and a half minutes, with these results:

- LiteLLM stops its calls to Redis within seconds of the first failures. It
  tries Redis again every 60 seconds. Each replica used Redis again within
  about a minute after Redis returned, without a restart.
- Budgets use the spend that is recorded in PostgreSQL instead. This recorded
  spend was 10 to 15 seconds behind the live spend. Requests inside that
  window are not yet counted. A key with room for five requests was allowed 16
  in a ten-second burst, and 7 when requests arrived five seconds apart.
- No spend was lost. The recorded spend of every key matched the requests that
  it was allowed.

Redis restarts empty. For the same reason, this causes no problem.

Rate limits stop for about a minute. On the e2e cluster, the bundled Valkey Pod
was deleted, and its new Pod was ready within a second. A key with a limit of
3 requests a minute then got 6 of 8 requests through in one minute. After that
minute, the limit applied again: 3 requests, then 429. A key over its budget
got 402 the whole time, because budgets use the spend in PostgreSQL.

### When PostgreSQL is unavailable

The Gateway no longer serves requests. In a measurement, PostgreSQL was stopped
for about three minutes, with these results:

- Within a minute, requests with virtual keys started to fail with 503,
  because the Gateway finds keys in the database. A request to create a key
  returned 500.
- After about a minute, readiness failed on both replicas, and the Service had
  no more endpoints. As a result, the Gateway refused every request, also the
  requests with the master key.
- Nothing restarted. Service started again about 20 seconds after PostgreSQL
  returned, and no action was necessary.
- **Requests answered while the database was going down were never charged.**
  LiteLLM retries a spend write three times, and then it drops the write. On
  two test keys, the spend of 7 and 9 requests never reached the recorded
  total.

With a highly available PostgreSQL, this problem lasts only for the length of
a failover. The spend that is lost is limited to what the Gateway serves during
that time. The 503s keep this amount small.

The bundled PostgreSQL is one replica on a `ReadWriteOnce` disk. On the e2e
cluster, its Pod was deleted, and the new Pod started on another node. It was
ready after 51 seconds, because the disk first had to detach from the old node.
In that time, 11 of 13 requests with a virtual key failed with 503,
`no_db_connection`. Each request with the master key got an answer. The
Service kept its endpoints, and the Agents stayed `Ready`.

## Request log and metrics

LiteLLM keeps a record of each call in its database, and the console's request
view reads this record. LiteLLM also keeps daily totals for each key, user,
Team, and model. Neither of these leaves the Gateway. For collection in other
places, the chart adds a log line for each call, and it can serve LiteLLM's
Prometheus metrics. The measurements used LiteLLM 1.101.0, on AKS with
Container Insights and Azure Monitor managed Prometheus.

### One log line per call

With `requestLog.enabled`, which is the default, each model call writes one
JSON line to the Gateway's stdout, through a callback in the image. The line
records this metadata of the call:

- The virtual key's alias and hash, the user, the Team, and the Agent, from
  the metadata that the CetiRun operator puts on the key.
- The model, the provider model, and the provider's host.
- Tokens, with cached and reasoning tokens when the provider reports them.
- Spend.
- Duration and, for a streamed call, the time to first token.
- The client's address and user agent.
- For a failure, the error.

A call that LiteLLM makes itself carries its origin in `call_origin`. For
example, a background health check probe is `"health_check"`. As a result,
usage figures can omit it. If a call for a request fails, LiteLLM can make
another call to a different model or to another deployment of the same model.
This call carries `fallback_from` (the model that the request was for),
`attempted_fallbacks`, and `fallback_cause`. This is a shortened example of a
line:

```json
{"log_type":"cetirun.ai_gateway.request","schema_version":1,"time":"2026-10-08T13:13:27.780Z","status":"success","route":"/v1/chat/completions","stream":false,"model":"gpt-4o","provider_model":"azure/prod-gpt-4o","prompt_tokens":1000,"completion_tokens":1000,"cached_tokens":800,"spend":0.014,"key_alias":"cetirun-alice-agent-1234","user_id":"alice","team_alias":"research","key_metadata":{"cetirun_agent":"alice-agent","cetirun_team":"research"},"duration_ms":686}
```

The log recorded every call. Across two replicas, 40 calls made 40 lines.
These lines covered the Chat Completions and Responses APIs, streamed and not,
and failures. The spend in the lines added up to the key's spend.

With Container Insights, `ContainerLogV2` parses each line into the dynamic
`LogMessage` column:

```kusto
ContainerLogV2
| where ContainerName == "gateway"
| where tostring(LogMessage.log_type) == "cetirun.ai_gateway.request"
| summarize calls = count(), spend = sum(todouble(LogMessage.spend))
    by agent = tostring(LogMessage.key_metadata.cetirun_agent), model = tostring(LogMessage.model)
```

Lines reached the Log Analytics workspace about 80 seconds after the call. Each
line was billed at 1.4 to 1.9 KB. The larger lines were for the Responses API,
because its response IDs are long.

The CetiRun operator puts `cetirun_agent` in the metadata of the keys that it
issues. For other keys, group by `LogMessage.key_alias` instead.

The cluster must collect container logs. On AKS, the monitoring add-on is
necessary for this. In a test without the add-on, only a Container Insights
data collection rule and its association were present. They collected nothing,
and no `ama-logs` pods ran.

**No content.** The line carries no request or response content, and no
setting adds it. Every string in the line is limited to 256 characters, or to
1,024 for the request ID and the error text. Clients set many of these
strings. Examples are the session ID, the call ID header, the model name, the
end user, the user agent, and the tags. LiteLLM records the session ID as
`trace_id`.

A line is never longer than 15 KiB. Container runtimes split longer lines, and
Container Insights stores the parts as separate records. None of these records
is valid JSON.

Escape sequences can make a line longer, for example for control characters.
In this case, the line first loses the error text, the client's tags, and
extra key metadata. Then the longest strings are cut, and the line carries
`"truncated": true`.

The image's guard callback also drops a client's `litellm_request_debug`
field. When a call had this field, LiteLLM printed the prompt of that call and
the provider's raw response to the log.

**Callers cannot leave it out.** A client can send `"no-log": true` to ask
LiteLLM to skip its logging callbacks for a call. In a measurement without the
chart's setting, such a call wrote no line. The chart always sets
`litellm_settings.global_disable_no_log_param`, which makes LiteLLM ignore the
field.

**Registered by the image, not in LiteLLM's configuration.** The chart sets
`CETIRUN_REQUEST_LOG=true` on the Gateway container. The image's entrypoint
registers the callback before LiteLLM loads its configuration.

If `litellm_settings.callbacks` lists the callback, the callback depends on how
LiteLLM combines its configuration file with the settings that are saved in
its database. When LiteLLM reads its configuration with the database
connected, it replaces a configured list with a saved one. LiteLLM 1.101.0
reads the file before it connects, and it adds saved callbacks to the
configured ones later. As a result, in a test, a saved list did not remove a
configured callback. But this result depends on LiteLLM's startup order.

A configuration that names the callback also prevents the start of an image
without the callback. An image built before the request log exited at startup
with an `ImportError`. The entrypoint's registration depends on neither of
these conditions. An image built before the request log ignores the variable,
and it starts with the chart's configuration.

The proxy runs as one uvicorn process for each pod, which is how LiteLLM starts
by default. The entrypoint refuses more workers, or another server. These load
LiteLLM again without the image's callbacks, also without the guard. The
`requestLog` value does not change this. To scale the Gateway, use
`replicaCount` instead.

### LiteLLM's spend log

LiteLLM writes a row for each call to `LiteLLM_SpendLogs`, and the console's
request log pages read this table. By default, the row holds the call's
metadata, prompt, and response. The default of `spendLogs.storePrompts` is
`true`. The chart always renders `general_settings.store_prompts_in_spend_logs`
with the selected boolean. This default applies to new installs and to upgrades
that have no explicit value for `storePrompts`. It can store sensitive content.

The prompt is the request body of the call: the messages, with images and tool
results, and the tools. If a value in the prompt or in the response is longer
than 2,048 characters, LiteLLM shortens it. This limit is the default of
`MAX_STRING_LENGTH_PROMPT_IN_DB`. LiteLLM keeps the start and the end of the
value. As a result, an image is stored only in part.

To keep the call metadata, but not to store prompts or responses, use these
values:

```yaml
spendLogs:
  enabled: true
  storePrompts: false
  retention: 7d
```

In the platform umbrella, put these values under `cetirun-ai-gateway`. Metrics
and operator integration do not override an explicit `storePrompts: false`.
`storePrompts` accepts a YAML boolean, not a quoted string. If you set it to
`true` with `spendLogs.enabled: false`, the chart render fails.

This setting does not depend on `requestLog.enabled`. It never adds content to
the stdout request log. A change to it updates the configuration checksum. As
a result, Helm replaces the Gateway pods.

If you set `storePrompts` to `false`, the chart writes
`store_prompts_in_spend_logs: false`. This disables content storage in
deployments that the configuration file controls. It does not erase content
that is already stored. A `general_settings` block that is saved through
LiteLLM's console or management API can override this setting in either
direction. It can also override retention in the same way.

Before you rely on an enable or disable setting, examine the effective
configuration. The chart does not read or change `LiteLLM_Config` or the spend
logs that already exist. Refer to
[settings saved in the console](#editing-settings-in-the-console-takes-them-out-of-chart-control).

The table grows by one row for every call. As a result,
`spendLogs.retention`, `7d` by default, renders LiteLLM's
`maximum_spend_logs_retention_period`. It also renders
`maximum_health_check_retention_period` for the stored health check results.
The same row retention applies when content storage is enabled. Spend, budgets,
and the daily usage totals are kept longer than the retention period.

To keep every row and any stored content for an unlimited time, set the
retention to an empty value. A retention period that is saved in the console's
settings wins over the chart's value. This is the same as for any
`general_settings` key.

Each replica deletes the older rows on its own schedule. The first time is one
interval (`maximum_spend_logs_retention_interval`) after the pod starts. Then
the replica deletes rows every interval. LiteLLM makes the interval longer by
up to a minute, which it selects when the pod starts. It also moves the runs of
each replica by up to five more minutes. A replica skips a run while the run of
another replica holds a lock in Redis.

LiteLLM's default interval is a day. With this interval, a Gateway whose pods
are replaced within a day never deletes a row. Every configuration change
replaces the pods. As a result, if a retention is set, the chart sets the
interval to an hour. Then rows stay at most about an hour longer than the
retention. A replaced pod only delays the deletion.

A run with nothing to delete costs each replica two indexed queries for each
table. A run deletes 1,000 rows at a time, at most 500 times for each table,
and for at most five minutes. The next run continues from that point.

A measurement on LiteLLM 1.105.1 used the request log on and prompt storage
disabled. Every call, also the probes, added a row. None of these rows had a
prompt.

With two replicas, the rendered configuration was changed to a one-minute
retention and a 30-second interval. Each replica ran the job on a period of its
own, 49 and 83 seconds. The first run of each replica was 75 and 128 seconds
after it started. Every run took the lock. The runs deleted 88 spend log rows
and 11 health check results, all past the retention. With the chart's hourly
interval, two proxies scheduled their first run 63 and 66 minutes after they
started.

With `spendLogs.enabled: false`, LiteLLM writes no row for a call. You can
disable it only together with the request log. As a result, no configuration
leaves the Gateway without a record of each call. For
`spendLogs.enabled: false`, `requestLog.enabled` and
`spendLogs.storePrompts: false` are necessary. The image's entrypoint disables
the spend log only after it registers the request log callback.

The entrypoint sets LiteLLM's `disable_spend_logs` in the proxy process, not in
`general_settings`. As a result, the setting is not saved with a settings
block, and it does not stay after a rollback. But a `disable_spend_logs` that
is written in `general_settings` still wins. An image built before the request
log continues to write the table.

In a measurement with the spend log off, 16 calls added no row. But the spend
of the key, the Team, and the user each increased by exactly the cost of the
calls. Budgets continued to work, and the daily totals counted all 16 calls.
The console's usage pages read those totals and continue to work. Its request
log pages, which read `LiteLLM_SpendLogs`, stay empty. LiteLLM reads the
setting when the proxy starts.

### Credentials and Container Insights

Container Insights copies the environment of every container into its
`ContainerInventory` table one time each minute. The copy also has the values
that come from Secrets. Container Insights does this unless environment
collection is disabled for the cluster or for the container. The Gateway holds
its master key, salt key, database connection string, and provider keys in its
environment.

Every container in this chart that holds a credential sets
`AZMON_COLLECT_ENV=False`, which disables that collection for that container.
The CetiRun cluster template also disables it for the whole cluster.

### Metrics

`metrics.enabled` registers LiteLLM's Prometheus callback, and it serves the
metrics at `/metrics` on `metrics.port`. If you do not set this port, it is
9090, or 9091 when `service.port`, where the proxy listens, is 9090. As a
result, it is never necessary to select a port by hand when you enable
metrics.

LiteLLM runs a separate process for that port. This process reads the samples
that the proxy puts in a directory that they share. As a result, a scrape never
waits behind model traffic. Each replica serves its own samples. Sum them
across pods.

The metrics port has no authentication. LiteLLM also serves `/metrics` on the
Service port, where it accepts only the master key. It refused both an Agent's
key and a key without route limits with 401. The metrics port is not on the
Service. Anything that can reach a Gateway pod can read every key alias, user,
Team, and spend figure that the metrics carry.

As a result, if other workloads are not allowed to read the metrics port,
restrict it with a NetworkPolicy. A CetiRun Agent's egress reaches only the
Service port.

The metrics carry the key alias and hash, user, Team, model, and route as
labels. Examples of these metrics are `litellm_spend_metric_total`,
`litellm_proxy_total_requests_metric_total`,
`litellm_input_cached_tokens_metric_total`, and
`litellm_remaining_api_key_budget_metric`. `metrics.excludeLabels` omits the
client's address and user agent, and the user's email address. The address and
the user agent start a new series with every Agent restart and client version.

For Azure Monitor managed Prometheus, set `metrics.podMonitor.enabled`. When the
cluster serves the `azmonitoring.coreos.com/v1` API, the chart creates a
PodMonitor of that API. The managed collector reads this API. If the cluster
does not serve it, the chart creates no PodMonitor. As a result, an install
does not fail on a cluster without managed Prometheus.

For the Prometheus Operator, set `metrics.podMonitor.apiVersion` to
`monitoring.coreos.com/v1`. The cetirun-platform chart enables both the
metrics and the PodMonitor. In a measurement, both replicas were scraped. In
the Azure Monitor workspace, this query returned the spend of each key:

```promql
sum by (api_key_alias, team_alias, user, requested_model) (litellm_spend_metric_total)
```

To query the workspace, you must have the Monitoring Data Reader role on it.

Counters lose the increments that a label set makes before its first scrape.
Counters also start again from zero when a pod restarts. As a result, use the
metrics for rates, trends, and alerts. For exact spend, use the request log or
the daily totals.

### Reading the totals

LiteLLM's token counts include cached tokens. `prompt_tokens` counts every
input token. In the daily totals, `cache_read_input_tokens` is the part of
these tokens that was read from the provider's prompt cache. This part is
charged at the lower cache rate. As a result, the token counts priced at the
full input rate are more than the recorded spend, by the cache discount.

In a measurement, 4,000 of 20,000 input tokens were cached at a quarter of the
input rate. The full-rate figure was 0.40, and the recorded spend was 0.37.
When the cached part was priced at its own rate, the result was also 0.37.

`LiteLLM_SpendLogs`, the console's per-request record, uses the provider's
response ID as its key. LiteLLM skips a row whose ID it already has, and it
gives no warning. Real providers return a new ID with every response. A test
backend that repeats one ID keeps one row for all its answers. For example,
five calls left one row, but the key was charged for five calls.

## Migrations

By default, LiteLLM applies migrations at startup. Instead, the chart runs them
in an init container of each Gateway pod, with the image's `migrate` command.
It starts the proxies with `DISABLE_SCHEMA_UPDATE` set.

Pods take turns. In a measurement, two migrations that ran at the same time
caused a deadlock. The deadlock occurs because prisma waits for its own lock
inside an open transaction, and because one of LiteLLM's migrations builds an
index concurrently. This build waits for every open transaction. To resolve
the deadlock, PostgreSQL kills one of them. This can leave a failed migration
that blocks every later deploy.

As a result, `migrate` first takes a PostgreSQL advisory lock of its own. It
polls for this lock, and it does not keep a transaction open while it waits.
In a measurement with two pods that started together, the second pod waited.
Then it found nothing more to apply.

An init container can wait for a database that is not ready yet. The
pre-install hook that this chart used before cannot do this. On first
install, the bundled PostgreSQL is not ready yet. `migrate` waits up to ten
minutes for the database.

To return to startup migrations, set `migrations.enabled: false`. This setting
is applicable only to a single replica.

## Image

The Gateway runs `cetirun-ai-gateway`. This image is built from
`app/cetirun-ai-gateway` and published with the other CetiRun images. It is
open-source LiteLLM, 1.108.3, installed as pinned packages on the Azure Linux
Python base from MCR. It runs as the non-root user 65532.

- `requirements.txt` pins the upstream release's frozen `uv.lock` for the
  Gateway's existing runtime extras. It is installed with `--no-deps`, so
  nothing else is added. `litellm-enterprise`, which carries the BerriAI
  Enterprise License, and the optional Admin MCP group are omitted. The two
  Python 3.12 backports keep their existing CetiRun pins.

  LiteLLM 1.108.3 permits filelock 4.x; the lockfile selects 4.2.4, which includes
  the security fixes released in 4.2.1. The build fails if the enterprise
  package is present or if `pip check` finds an unmet dependency.
- The prisma CLI and engines are installed at build time. As a result, neither
  npm nor network access is necessary for migrations. In a measurement on a
  network with no route out, migrations applied, and the proxy became ready.
- `cetirun-ai-gateway migrate` applies migrations. Any other arguments start the
  proxy. The start never uses a database, because the same image serves the
  per-agent Copilot proxy, which has no database. The Gateway proxy starts in
  the entrypoint's own process, with the guard callback registered. With
  `CETIRUN_REQUEST_LOG=true`, which the chart sets, the request log callback is
  also registered.

To move to a new LiteLLM release, export its frozen `uv.lock` with the uv version
used by its Dockerfile. For 1.108.3 this is uv 0.11.7, with the `proxy`,
`proxy-runtime`, `extra_proxy`, `semantic-router`, `saml` and `bedrock-realtime`
extras and no default groups. Select Linux/Python 3.12 entries, pin the LiteLLM
and proxy-extras workspace packages to their release versions, and keep the
exclusions and backport pins above. Then build the image again. The build's
`pip check` then names any backport that the new set must have. Then the
pull-request dependency review flags any package with a high or critical
advisory.

When `global.registry` is set, the chart uses `<registry>/cetirun-ai-gateway`.
This is the same convention as in the other CetiRun charts. `image.digest` pins
an exact build.

## Testing

```bash
bash scripts/cetirun-ai-gateway-helm-test.sh       # offline render assertions
bash scripts/cetirun-ai-gateway-cluster-test.sh    # install and upgrade on a real cluster
```

A cluster is not necessary for the render assertions. They run in
`make chart-check`.

The cluster test must have kubectl configured for a disposable cluster that
already has the image. It must also have helm, curl, and jq. It creates its own
namespace (`NAMESPACE`, default `gateway-cluster-test`), and it deletes the
namespace after the test. It refuses a namespace that already exists, and it
does not change that namespace.

The test deploys a mock provider. It installs the chart with its defaults,
which include the bundled PostgreSQL and Valkey. Then it tests the chart,
upgrades it, uninstalls it, and installs it again. Then it examines what
survived. It runs in CI on kind for any change to the image or the chart. The
image is built from the same commit.

The test exists because these real bugs did not make the render assertions
fail:

- Two failures in the order of hooks.
- A migration that succeeded but did not create any of LiteLLM's tables.
- A Redis configuration that answered identical requests from different callers
  from the cache.
- An uninstall that deleted the salt key. The database kept everything that
  was encrypted with that key.

The test also examines behaviors that only a running Gateway can show:

- An upstream failure is not retried.
- A replica that shuts down finishes the request that it serves.
- Replicas migrate in turn.
- The bundled services refuse clients without their passwords.
- Every answered call reaches both the spend log and the request log.
- With the spend log disabled, calls are still charged and logged, but they add
  no spend log row.
- A call sent with `"no-log": true` is still logged.
- The metrics port serves a pod's requests, but a key is still necessary on the
  Service port.
- A `callbacks` list that is saved in LiteLLM's database keeps the request log,
  and the spend log setting, in place.

When `PREVIOUS_IMAGE_TAG` is set to an image built before the request log, the
test also makes sure that the chart still starts that image. It also makes sure
that LiteLLM's spend log stays on with that image.

The checks of the test were confirmed in this way. Each original problem was
introduced again, and the test failed at the check that was made to find it.
This procedure also showed weaknesses in the test itself, which were fixed
after that:

- A check that counted every table passed on an empty database, because
  migration creates its own bookkeeping table, also when it applies nothing.
- The mock provider of the test answered every call with the same response ID.
  As a result, LiteLLM's spend log kept one success for any number of
  successes. At one time, this README recorded this result as a gap in
  LiteLLM.

## Known gaps

These gaps are recorded and deferred. None of them makes the Gateway unable to
serve agents.

**Not in the chart yet.** The chart does not have Redis TLS, which is necessary
for Azure Cache for Redis. It also does not have anti-affinity for the Gateway
replicas (set `affinity` by hand).

**Prices are not pinned to the image by default.** LiteLLM downloads its price
table from GitHub each time a Gateway pod starts. If the download fails,
LiteLLM uses the copy that it ships with. As a result, a restart can change
the prices that `base_model` resolves to.

`priceTable: bundled` uses only the copy in the image. This setting is for a
cluster without internet access, or for prices that change only with the image.
With this setting, the chart sets LiteLLM's `LITELLM_LOCAL_MODEL_COST_MAP` on
the proxy and on the migrate step. The migrate step also loads LiteLLM. A
reload of the price table from the console, immediately or on a schedule,
loads the image's copy again.

But new models and price changes arrive only with a new image. A model that is
not in the image's table is unpriced. While `blockUnpricedModels` is on, the
Gateway refuses such a model, unless its entry declares a price. With CetiRun,
the operator prices the Agents' GitHub Copilot models from the table that the
Gateway loaded. As a result, a Copilot model that is newer than the image is
not offered.

In a measurement on LiteLLM 1.105.1, the table's address was set to a mock.
With the variable, neither the proxy's start, the migrate step's import of
LiteLLM, nor a reload requested the table. Without the variable, each of them
requested it.

The table also tells which parameters each model takes. The guard and
LiteLLM's own checks use the table. As a result, the image's copy can be wrong
where the downloaded one is corrected. The copy in LiteLLM 1.105.1 marked
Azure's gpt-6-astra and gpt-6.1-sol as models that take
`reasoning_effort: none`, but Azure refuses this value.

With `priceTable: bundled`, or when the download failed, a request with this
value got Azure's 400, and it did not lose the parameter (measured with
gpt-6-astra). LiteLLM corrected its bundled table in 1.105.6.

**GitOps.** Generated credentials depend on Helm's `lookup`, which returns
nothing under `helm template`. Some tools, such as Argo CD, render in that way.
These tools must supply the master key, salt key, and console password through
Secrets that already exist. They must also supply the password of the bundled
Valkey through `redis.passwordSecret`, or use their own Redis.

They must also use an external PostgreSQL. The password of the bundled
PostgreSQL is generated in the same way, and there is no option to supply it.
If the password changes, the Gateway cannot get access to data that was
initialized with the old one.

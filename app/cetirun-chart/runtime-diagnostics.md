# Issue 486 runtime-call diagnostics

This note records the offline evidence for the runtime-call observations in
#486. It uses synthetic files, loopback servers, and stubbed Gateway behavior
only. It does not contact a model provider or establish provider capacity,
authorization, billing, or browser acceptance.

## OpenClaw effective model selection

The native OpenClaw chart writes both `agents.defaults.model.primary` and
`agents.defaults.utilityModel` from the approved Gateway model. Its HOME
initializer replaces saved primary, fallback, model-list, and Gateway override
state with that rendered configuration. The synthetic regression below verifies
that behavior across two initialization runs.

```text
pytest -q app/cetirun-chart/tests/test_init_home.py \
  -k native_openclaw_replaces_saved_model_and_gateway_overrides
```

This does not explain an observed request for a different upstream model. A
live diagnosis must compare the Agent's final rendered configuration with the
Gateway model entry (`model_name`, `litellm_params.model`,
`model_info.base_model`, and `model_info.mode`) and the requested upstream API.
Those values distinguish a chart/configuration mismatch from a LiteLLM alias
or provider-side route. Do not broaden model grants before that comparison.

## Hermes reasoning parameters

The Gateway guard has synthetic coverage for the LiteLLM rules that remove
sampling parameters from reasoning models and remove only
`reasoning_effort: none` when an Azure deployment refuses that value. It leaves
other effort values to LiteLLM/provider compatibility checks.

```text
python3 -B -m unittest scripts.tests.test_ai_gateway_guard -v
```

An `UnsupportedParamsError` therefore still requires the actual request model,
API mode, effort value, tools, and Gateway model metadata. In particular,
`model_info.base_model` and `model_info.supports_reasoning` affect LiteLLM
recognition differently for Azure and OpenAI-style routes. A later provider
429 is separate capacity/request-sizing evidence; this check does not retry or
qualify it.

## Pi streaming behavior

Pi emits a terminal `finish_reason` followed by `[DONE]` on success. If its
runtime fails after a stream starts, it emits one redacted SSE error and does
not forge a success finish or `[DONE]`. The Portal chat proxy requires a valid
finish and `[DONE]`, and rejects/redacts incomplete or error streams.

```text
node --test examples/pi-agent/test/server.test.mjs
go -C app/cetirun-portal test ./internal/chatproxy -count=1
```

With the locked Pi dependencies installed, `npm --prefix examples/pi-agent test`
passes the Gateway adapter's loopback success, tool, output-limit, failure,
and redirect cases as well as the SSE server checks. The suite reports 34
passed tests and four image-specific skips. Those skips do not qualify the
built image's npm/Undici behavior. These checks do not prove that a browser
rendered a provider failure or that a real provider request completed.

## Required live evidence before a runtime change

For each runtime, capture one bounded synthetic prompt's selected alias, final
Agent configuration, Gateway route metadata, API mode, terminal status, and a
safe correlation/time reference. Do not repeat unchanged 403 or 429 requests.
Capacity, effective model selection, Hermes parameter compatibility, and
browser rendering remain live acceptance work.

# Coding Runtime Examples

These examples adapt three runtimes to the same Custom Chat interface. They do
not add new built-in CetiRun harnesses. **The default mode is entirely offline**:
mock review results are fixed synthetic data, not model-generated output. No
tools, CLIs, or subagents are executed.

## Run and Validate

Requires macOS or Linux and Python 3.11+. Uses only the Python standard library:

```bash
make test check
python3 runtime.py run --runtime copilot
python3 runtime.py run --runtime claude
python3 runtime.py run --runtime codex
python3 runtime.py serve --runtime claude --port 8080
```

Each server runs one runtime at a time. Use different ports for multiple
instances. The default bind address is `127.0.0.1`. This is a development
service with no application authentication; do not expose it publicly.

```bash
curl --fail http://127.0.0.1:8080/healthz
curl --fail http://127.0.0.1:8080/v1/models
curl --fail -N http://127.0.0.1:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"mock/claude","messages":[{"role":"user","content":"Review pricing.py."}],"stream":true}'
```

| Endpoint / Behavior | Implementation |
| --- | --- |
| `GET /healthz` | Reports process readiness only; does not call a model |
| `GET /v1/models` | Returns only `mock/<runtime>` or `native-default/<runtime>`; the latter is a local adapter alias, not a provider model ID |
| `POST /v1/chat/completions` | Text messages with a JSON completion or SSE completion chunks |
| Concurrency | One active request per instance; other requests receive 409, with no queuing or retries |
| Limits | 64 KiB request body, up to 32 messages, 256 KiB native output, and a 120-second native task deadline |
| Native errors | Fixed error codes; stderr and provider error text are not returned to clients |
| Native cancellation | Checks for cancellation on client disconnect and terminates the task process group; cannot undo provider charges or external side effects that have already occurred |
| Sessions | Rebuilds context for each request; does not resume CLI sessions or migrate sessions between runtimes |

SSE **buffers the native task's final result before sending content, a finish
chunk, and `[DONE]`**. This is not a real-time tool or token stream, and no
keepalive is sent while the task is running. Native errors before SSE begins
return an HTTP error without a success marker. A Portal or reverse proxy may
time out during the silent phase, so this simplified implementation is not yet
suitable as a production Portal Chat streaming adapter.

The service implements a text-only subset. It does not support images, tool
messages, external tool calling, or multiple completions. It rejects `tools`,
`tool_choice`, and `n`; other fields, such as temperature or token limits, are
not mapped to native CLI options. Each runtime's configuration still controls
its default model and internal token and tool budgets. This example only bounds
the outer task duration, output size, and concurrency.

## Native Configuration for Each Runtime

| Runtime | Configuration | Invocation |
| --- | --- | --- |
| GitHub Copilot CLI | `workspace/.github/agents/cetirun-reviewer.agent.md` | `--agent cetirun-reviewer -p <fixed-instruction>` selects the custom role as the current session's agent; conversation text is supplied on stdin |
| Claude Code | `workspace/.claude/agents/cetirun-reviewer.md` | `--agent cetirun-reviewer -p` selects the custom agent for the current session |
| Codex CLI | `workspace/.codex/agents/cetirun-reviewer.toml` | `codex exec` instructs the main agent to delegate to this role and wait for its result |

No model is specified; each runtime's default is inherited. This example does
not use an invented `--agent` option for Codex or treat `AGENTS.md` as an agent
registration file. Actual Codex delegation must be verified through native
event or thread records; an HTTP completion does not prove that a subagent ran.
The example does not parse the full thread topology or treat a successful
answer as proof of successful delegation.

## Optional: Local Native CLIs

Install and verify an approved CLI version yourself, then sign in through its
native authentication flow. This example does not automatically install CLIs,
read token files, share credentials, or use `--yolo` or blanket permission
bypasses. During the original local validation, only Copilot CLI help was
checked; Claude, Codex, and real model calls were not validated.
Use the official documentation for CLI options and role formats:
[Copilot CLI](https://docs.github.com/en/copilot/how-tos/copilot-cli/customize-copilot/create-custom-agents-for-cli),
[Claude Code](https://code.claude.com/docs/en/sub-agents), and
[Codex](https://developers.openai.com/codex/subagents).

Inspect the command plan first, without starting a CLI or calling a model:

```bash
python3 runtime.py plan --runtime copilot
python3 runtime.py plan --runtime claude
python3 runtime.py plan --runtime codex
```

Run the following only after explicitly agreeing to let the installed CLI
process the synthetic fixture:

```bash
python3 runtime.py run --runtime copilot --native --ack-data-export
python3 runtime.py run --runtime claude --native --ack-data-export
python3 runtime.py run --runtime codex --native --ack-data-export
```

For local native HTTP mode, use `serve --native --ack-data-export` and set the
client's model to `native-default/<runtime>`. **Do not send private code, secrets,
or sensitive messages to this demonstration service.** CLIs can load native user
configuration and context and may send data to their providers. Tool
restrictions and prompts do not provide filesystem, network, or multi-tenant
isolation. Conversation prompts for all three CLIs are passed through
nonblocking stdin, not command-line arguments. Do not include credentials in
prompts.

The read-only role reviews `workspace/pricing.py` without fixing files. The
fixture deliberately uses 20 where 0.20 is required, allowing you to check
whether a real CLI identifies the same issue. An HTTP completion only indicates
that the outer task finished; it does not prove review correctness, successful
role discovery, or completed internal delegation.

If `OPENAI_BASE_URL` is set or any `CETIRUN_*` environment variable exists, the
example rejects native mode with `platform_native_not_qualified`. Do not remove
platform variables to bypass this restriction: these local CLI adapters are
not a product implementation qualified for the CetiRun Gateway.

## Container and Platform Reference

The `Dockerfile` packages only the **offline mock labeled `copilot`**. It does
not install any real coding CLI. The runtime user is `1024:1024` / `vibe`, the
application is under `/opt`, and the writable HOME is separate from application
files. An administrator must supply an approved base image that satisfies the
CetiRun initialization tool contract and includes Python 3.11+. This example
does not assume an available official image version or digest.

```bash
docker build --build-arg BASE_IMAGE='<approved-cetirun-compatible-base@sha256:digest>' \
  -t coding-runtime-example:local .
docker run --rm --read-only --tmpfs /tmp --cap-drop ALL \
  -p 127.0.0.1:8080:8080 coding-runtime-example:local
```

The Dockerfile checks common initialization tools and the UID/GID, but this
does not replace full HOME initialization or CSI acceptance testing.
`CETIRUN_BIND_HOST` takes precedence over `--host`, preserving the platform's
loopback requirement for mTLS.

`cetirun-agent.yaml.template` is a field reference and **must not be applied
directly**. Fill in an existing Team, Sandbox, user, compatible chart, and an
approved production image digest. The production image must be a separate
implementation with Gateway integration; do not present this mock as a model
runtime. The Custom CRD does not accept arbitrary command or environment
overrides. Build a separate image for each runtime, or let the image's own
entrypoint select its runtime.

During the original validation, Docker builds were not run because the local
Docker daemon was unavailable and no approved base image had been supplied.
Helm rendering, Portal, AKS, Gateway, model, real OAuth/MCP, and PVC acceptance
checks were not run. Since upstream source was unchanged, its full Go,
frontend, and root Makefile checks were not run. This example's `make test check`
covers only this standalone example directory.

## Original Offline Validation Results

On 2026-11-08, `make test check` passed with 22 tests covering HTTP, SSE, input
rejection, concurrency rejection, native output normalization, timeouts,
cancellation, output limits, and the platform native-mode guard.
Native subprocess tests used synthetic Python CLIs, not real providers.
All three runtime labels were also checked using actual server processes for
`/healthz`, `/v1/models`, JSON completions, SSE completions, and CLI plans.
All processes exited normally after SIGTERM.
These results do not establish compatibility with real CLI role discovery,
delegation, providers, or production platforms.

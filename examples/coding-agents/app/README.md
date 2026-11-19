# Native Coding Runtime Applications

This directory packages GitHub Copilot CLI, Claude Code and Codex CLI as private
CetiRun applications. Chat requests invoke the real CLI to edit files and run
tests; there is no mock execution mode. Each image installs one verified runtime.

| Image | Native CLI | Runtime user |
| --- | --- | --- |
| `cetirun-copilot:local` | GitHub Copilot CLI 1.0.107 | `1024:1024` |
| `cetirun-claude:local` | Claude Code 2.1.319 | `1024:1024` |
| `cetirun-codex:local` | Codex CLI 0.167.0 | `1024:1024` |

[artifacts.json](artifacts.json) records the official binary sources and SHA-256
checksums. The [Dockerfile](Dockerfile) pins the CetiRun base image by digest and
preserves its HOME initialization tools. Installation uses verified native
binaries, not remote installer scripts or an implicit npm registry fallback.
Credentials must not appear in build arguments, image layers, version files or
logs. Images support Linux amd64 only.

## Runtime behavior

[server.py](server.py) exposes `/healthz`, `/v1/models`, `/v1/chat/completions` and
a local web page. The workspace persists at `$HOME/workspace`. Runtime profiles
are initialized at their native paths without overwriting existing user files.
Coding tasks are not restricted to a synthetic `pricing.py` example.

Each HTTP request reconstructs task context from the supplied messages. It does
not automatically resume a previous CLI session or transfer sessions between
runtimes. Native session files remain in each runtime's HOME. Adapter-managed
Copilot sessions live in `$HOME/.cetirun/coding-runtime/copilot-sessions`; existing
user `.copilot` sessions are neither migrated nor deleted.

### Gateway and model selection

Model access requires the platform-injected `OPENAI_BASE_URL` and an
`OPENAI_API_KEY` containing the **per-Agent Virtual Key**. `/v1/models` reads the
Gateway catalog authorized for that key. An unavailable model, an authorization
failure or a Gateway error does not produce a fabricated catalog or trigger a
fallback to a personal account or direct provider API.

| Runtime | Gateway integration |
| --- | --- |
| Copilot | Environment-based Gateway configuration using the OpenAI Responses protocol and the exact authorized model ID. |
| Claude Code | Anthropic Messages protocol. A trailing `/v1` is removed from the Gateway base URL to avoid duplicate paths; model and subagent aliases use the same authorized model. |
| Codex | An explicit `cetirun` provider using the Responses protocol. `env_key` references the Virtual Key without requiring a personal OpenAI login. |

HTTP and `coding-tui` share the launch preparation in [native.py](native.py).
Copilot uses a separate temporary `COPILOT_HOME`, `settings.json` and
`COPILOT_PROVIDERS_CONFIG`. Existing provider settings are not used for routing,
and user configuration is not deleted or overwritten. The temporary provider
JSON is an empty registry with no keys. The Gateway Virtual Key is passed only
through the child process environment, not adapter configuration files or argv.

Temporary configuration files use mode `0600` under a `0700` directory and are
removed after normal exit or controlled cancellation. Copilot preferences are
not inherited from the user's existing configuration. Persistent sessions are
stored separately from temporary launch configuration.

### Platform instructions and skills

The platform's canonical read-only view is `$HOME/.agents`. The adapter adds its
`AGENTS.md` to Copilot's managed primary role, Claude's appended system prompt and
Codex's `developer_instructions`. Copying a Codex subagent TOML alone is not the
instruction integration.

Copilot and Claude use the primary role `cetirun-platform-coder`. User-maintained
`AGENTS.md`, `CLAUDE.md` and native role files are preserved. Platform instructions
that exceed safe CLI argument limits fail explicitly rather than being silently
truncated.

Skills are exposed through Copilot's managed skill directories, a temporary
native Claude plugin and Codex's native `$HOME/.agents/skills` discovery. Claude
plugin skills use the `cetirun-platform:` prefix.

### Platform Memory

The adapter registers the `cetirun-memory` stdio MCP server only when the platform
provides `CETIRUN_MEMORY_ASSETS`, an executable helper and `CETIRUN_MEMORY_ROOT`:

```text
cetirun-memory mcp --stdio --owner-private
```

It uses the platform-provided root rather than creating a separate private
Memory store. `CETIRUN_MEMORY_BINARY` can override the helper path.

HTTP requests can set `X-CetiRun-Memory-Mode: off` or `no-capture`. These settings
are combined with inherited `CETIRUN_MEMORY_NO_MEMORY=true` and
`CETIRUN_MEMORY_NO_CAPTURE=true`; the stricter restriction wins.

- `off` prevents the helper from starting and disables the platform Memory skill.
- `no-capture` passes the recording restriction to the helper.
- Each CLI's independent automatic Memory feature is disabled so it cannot bypass
  the platform setting.

This integration does not enable Connections MCP. Terminal sessions use the
environment present at launch. A native CLI's `/memory` command is not a platform
control: change the environment settings above and restart `coding-tui` to apply
new platform restrictions. Disabling capture does not delete native CLI session
history.

## HTTP and completion contract

The API accepts text messages only, not multimodal content, caller-supplied tools
or multiple completions. Requests must use `Content-Length`; `Transfer-Encoding`
is rejected. The complete supplied history is forwarded, and requests exceeding
a limit fail explicitly rather than being truncated.

| Limit | Value |
| --- | --- |
| HTTP task deadline | 600 seconds |
| Concurrent HTTP tasks | 1 |
| Messages per request | 100 |
| Request body | 1 MiB |
| Each user message | 64 KiB, measured as UTF-8 |
| Final text output | 1 MiB |
| Total native event output | 8 MiB |
| Encoded SSE frame | 64 KiB |
| SSE keepalive interval | 15 seconds |

Interactive terminal sessions do not use the HTTP task deadline. HTTP requests
are not automatically queued or replayed. Token and tool budgets still require
platform and runtime configuration.

SSE text is emitted while the CLI runs. Frame limits include JSON escaping,
metadata and the `data:` wrapper; Unicode text and emoji are split only at
character boundaries. A successful finish event and `[DONE]` are sent only after
the native run succeeds and all text has been written successfully.

Copilot receives the complete task on stdin. The adapter does not also pass `-p`,
because that option causes the CLI to ignore stdin. Completion requires both a
root `result` event with an integer `exitCode` of `0` and a successful process
exit. Text output, `assistant.idle`, `session.idle` or a child-agent event alone
is not evidence of success. Aborted idle events, failed results and malformed
exit codes remain errors.

A failed stream sends a fixed error code without a successful finish event or
`[DONE]`. If the connection has already closed, an error frame cannot be delivered;
clients must treat a missing success marker as failure. Native stderr is not
returned over HTTP. Cancellation does not undo tool actions or provider charges.

## Access and isolation

This is a **private, single-tenant workload** with no application-level
authentication of its own. Do not expose it through a public LoadBalancer or
Ingress. Use CetiRun's authorized Chat and terminal paths. The local web page is
only for authorized private access or port-forwarding.

The page does not implement the CetiRun Web UI prefix contract. Keep
`webUI: false`; setting `webUI: true` does not make it compatible.

A persistent HOME is not an isolation boundary. Filesystem and network isolation
must come from the approved CetiRun Sandbox and Policy. Tool permissions support
coding and testing without `--yolo` or
`--dangerously-bypass-approvals-and-sandbox`. Do not share a writable HOME or
checkout between runtimes.

`/healthz` confirms HTTP server health, not model access, coding correctness or
isolation. Platform login also does not replace native CLI or model authorization.

## Build and offline validation

Run these commands from `examples/coding-agents/app/`:

```bash
make test check
make build
```

`make test check` runs the adapter tests and validates Python, TOML and JSON files.
`make build` builds all three local images. Building may require network access to
fetch the pinned base image and official runtime artifacts.

### Cached official artifacts

If direct downloads fail, such as with HTTP 504, use an approved official release
download process to place the public artifacts in a build cache directory:

```bash
docker buildx build --platform linux/amd64 --load \
  --build-context runtime-artifacts='<official-artifact-cache-directory>' \
  --build-arg CODING_RUNTIME=copilot -t cetirun-copilot:local .
```

Repeat with runtime `claude` and `codex`. The cache must use the official filenames
from the URLs in `artifacts.json`. Every artifact is still checked against its
SHA-256 checksum. The cache is mounted only in the installation stage and is not
copied into the final image.

### Native protocol probes

Run the pinned CLIs against synthetic loopback services inside their images:

```bash
for runtime in copilot claude codex; do
  for mode in off no-capture; do
    docker run --rm --platform linux/amd64 --network none --read-only \
      --tmpfs /tmp:rw,exec,nosuid,nodev,size=768m \
      --mount type=bind,source="$PWD",target=/opt/cetirun-coding,readonly \
      -e HOME=/tmp -e PYTHONDONTWRITEBYTECODE=1 --entrypoint python3 \
      "cetirun-$runtime:local" /opt/cetirun-coding/tests/native_probe.py "$runtime" "$mode"
  done
done
```

[tests/native_probe.py](tests/native_probe.py) checks the actual model request for
Gateway routing, user-task forwarding, primary platform instructions, skill
discovery, disabled Memory and propagation of the no-capture MCP environment.
The synthetic Gateway returns an error before inference. The Memory helper is
also a fixture: it does not access a real private store. These probes do not make
billable provider calls.

Copilot has an additional HTTP/JSONL completion probe using the real CLI:

```bash
docker run --rm --platform linux/amd64 --network none --read-only \
  --cap-drop ALL --security-opt no-new-privileges \
  --tmpfs /tmp:rw,exec,nosuid,nodev,size=768m \
  --mount type=bind,source="$PWD",target=/opt/cetirun-coding,readonly \
  -e HOME=/tmp -e PYTHONDONTWRITEBYTECODE=1 --entrypoint python3 \
  cetirun-copilot:local /opt/cetirun-coding/tests/copilot_probe.py -v
```

[tests/copilot_probe.py](tests/copilot_probe.py) serves fixed text from a synthetic
Responses endpoint without running a real model. It verifies long conversation
and Unicode forwarding, successful JSON and SSE completion, and failure without
`[DONE]` or replay.

### What the tests establish

The adapter suite contains 28 tests covering subprocess stdin, file creation,
streaming, authorization, failure, cancellation, preservation of existing
configuration, Memory restrictions, Unicode SSE chunking, transport failures,
request limits, full history and separate HTTP/TUI deadlines. The native protocol
matrix covers three CLIs in two Memory modes. The Copilot completion probe adds
four real-CLI HTTP tests.

These are offline contract checks, not provider inference tests or full
qualification of the adapter and its deployment environment.

## Deployment acceptance

After the platform is configured, run these commands in the Agent's authorized
terminal:

```bash
python3 /opt/cetirun-coding/server.py doctor --model '<authorized-model-id>'
coding-tui --model '<authorized-model-id>'
```

`doctor` checks catalog reachability and model authorization without inference.
Use an exact model ID from the authorized catalog, not a substitute model.

A deployment needs separate acceptance checks for:

- Owner login and authorized Chat/terminal access, including rejection or login
  redirects for unauthenticated requests.
- A real coding task that edits a file, runs tests and completes successfully.
- Error and cancellation behavior without automatic replay.
- Suspension, resumption and preservation of HOME and its PVC binding.
- The actual running image digest, Agent and terminal readiness, and the required
  admission, filesystem and network isolation controls.

Compatibility must be tested for each configured provider path. Claude Code with
a third-party model over Anthropic Messages is a compatibility path, not a claim
of official Claude model support. Codex may require the Gateway to translate
Responses requests to the provider's protocol. Copilot's configured model must
also satisfy any required GitHub authorization. A different model cannot stand in
for the requested one during acceptance.

Confirm that provider terms permit the intended Gateway and automation usage.
Interactive subscription entitlements must not be assumed to permit shared or
application-backend access. Gateway USD budget and spend estimates are management
values, not a replacement for provider billing or credit quotas. Validate the
loaded pricing configuration separately; recorded reference prices do not prove
that budget enforcement is active.

Keep environment-specific deployment files, credentials, billing inputs,
certificate rotation records and live verification logs outside Git. This README
describes the application contract and repeatable checks, not the current state
of a particular cluster.

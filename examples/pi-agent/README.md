# CetiRun Pi Agent

Run [Pi](https://github.com/earendil-works/pi/tree/v0.87.1/packages/coding-agent)
in CetiRun, with its native terminal UI and an OpenAI-compatible HTTP adapter.
The example pins `@earendil-works/pi-coding-agent` to **0.87.1**. It uses Pi's
real tools. When Portal Chat is enabled, the example works with it. The example
does not include a Web UI. The demo image and package are named
`cetirun-pi-agent`.

## HTTP interfaces

Set `spec.agent.port: 8080`. The platform passes that value as `PORT`. The
image starts its HTTP server on `0.0.0.0:PORT`, or on `127.0.0.1:PORT` if the
approved Custom mTLS contract supplies `CETIRUN_BIND_HOST=127.0.0.1`.

| Interface | Implementation |
| --- | --- |
| `GET /healthz` | Returns process readiness and does not call a model. |
| `GET /v1/models` | Lists this Agent's available model identifiers from its selected model-access mode. |
| `POST /v1/chat/completions` | Accepts text messages and optional SSE streaming. Runs Pi's read, bash, edit, write, grep, find, and ls tools. |

[agent-api.mjs](agent-api.mjs) contains all three routes. It accepts one HTTP
chat request at a time, with a two-minute deadline and a budget of eight tool
calls. HTTP history is reconstructed in memory from each request, separate from
the native TUI history. Automatic model retries and compaction are disabled.

During silent model prefill or tool work, streaming responses send bounded SSE
keepalives every 15 seconds. The two-minute task deadline, cancellation, and
output limits still apply. Keepalives do not extend a task or replay a request.

If a run fails after streaming starts, the adapter sends one bounded, redacted
SSE error. It ends the response without a successful finish or `[DONE]`.
The error codes are `tool_budget_exceeded`, `deadline_exceeded`,
`output_limit_exceeded`, and `run_failed`. If the client disconnected, or if
there is no output capacity, the connection can end without that event.
It is possible that earlier tool actions completed. Do not replay the request
to find which actions completed.

Portal Chat records a reported error as failed, not as an unknown transport
interruption. A model response that reaches its output-token limit keeps
`finish_reason: length`. It is not a successfully completed answer.

For the platform rules beyond this example, see the
[Custom Agent contract](../../docs/agent-harness-contract.md).

## Code map

| File | Responsibility |
| --- | --- |
| [server.mjs](server.mjs) | Image entrypoint: start the API and private auth listener. Stop them on shutdown. |
| [agent-api.mjs](agent-api.mjs) | HTTP contract, input validation, streaming, limits, and cancellation. |
| [pi-runtime.mjs](pi-runtime.mjs) | Pi SDK sessions, model discovery, and tool execution. |
| [run-error.mjs](run-error.mjs) | Fixed error codes and messages that the runtime and the HTTP adapter share. The errors contain no provider text. |
| [gateway-runtime.mjs](gateway-runtime.mjs) | Model discovery and requests through the platform AI Gateway, with memory-only credentials and no automatic retries. |
| [connections-runtime.mjs](connections-runtime.mjs) | Lazy native MCP client with a workload proof that rotates, explicit TLS trust, bounded requests, and no automatic replay. |
| [pi-resources.mjs](pi-resources.mjs) | Load the common `$HOME/.agents` view and connect Memory through the Pi SDK. |
| [pi-tui.mjs](pi-tui.mjs) | Start Pi's native interactive mode with the same context and private auth directory. |
| [auth-api.mjs](auth-api.mjs), [copilot-auth.mjs](copilot-auth.mjs) | Loopback-only login/status/cancel API and its operations CLI. |
| [http.mjs](http.mjs) | Shared HTTP I/O and server lifecycle helpers. |
| [Dockerfile](Dockerfile) | Install pinned dependencies and tools, expose `pi-tui`, and run the HTTP entrypoint as a non-root user. |

In image-owned mode, the auth listener is private to the Pod at
`127.0.0.1:9099`. Never expose it through a Service or ingress. From an
approved operations channel inside the Agent container,
`node /opt/cetirun-pi/copilot-auth.mjs --help` describes its CLI. Credentials
stay in `$HOME/.pi/agent/auth.json`. The API does not return them, and they are
not copied from another Agent. Do not disable exec restrictions to reach it.

## Governed Connections and identity

On an explicitly enabled common Sandbox, Pi registers `connections_status`,
`connections_start_link`, `services_list`, and `service_request` with the real
SDK. The HTTP and TUI paths share these tools. The client uses the platform's
`CETIRUN_CONNECTIONS_MCP_URL`, `CETIRUN_CONNECTIONS_MCP_TOKEN_FILE`, and
`CETIRUN_CONNECTIONS_MCP_CA_FILE`. A partial configuration fails, and the
client does not use a personal token instead. The workload proof and trust are
read again for each request. These proofs and provider credentials are not
stored in HOME, tool definitions, prompts, or tool results.

The first tool call does the MCP initialization. Process readiness does not
call the provider or depend on a Ready Agent. Requests have a 30-second bound,
no redirects or automatic retries, and bounded output. If an outcome is
unknown, you must examine the provider state independently before you consider
a repeat. Tool calls count toward the eight-call/two-minute budget that the
HTTP adapter already has. The platform enforces a connection removal or an
Agent suspension on later calls and does not trust a local client flag.

Use explicit owner bindings and the
[Connections setup](../../app/cetirun-connections-mcp/README.md#governed-custom-workloads)
that already exists. Azure Agent Identity, generic Entra workload SDK
projection, and personal GitHub/Google connections stay separate. This example
does not provision cloud identities, federation, OAuth applications, role
assignments, or certificates. For those items, a separately authorized setup
and a real provider validation are necessary.

## Build the image

Run these commands from `examples/pi-agent`:

```bash
npm ci --ignore-scripts --no-audit --no-fund
npm test
npm run check
docker buildx build --platform linux/amd64 --load -t cetirun-pi-agent:local .
```

The root `make build-images` and `make publish-images` targets include this
demo. `scripts/pi-agent-docker-build.sh` builds it separately. To select a
CetiRun base that already exists, use `BASE_IMAGE`. Use `PUSH=true` only to
publish.

The Dockerfile uses the CetiRun base image for common HOME-initialization
tools. It installs the locked SDK and the checksum-pinned `fd`. At build time,
it makes sure that the necessary tools are available. It declares
`USER 1024:1024` and `HOME=/home/vibe`. It keeps the application at
`/opt/cetirun-pi`, outside the HOME mount. Telemetry and automatic Pi
catalog/update checks are disabled, but explicit OAuth and model calls still
work.

The `runtime-source` build stage sets copied JavaScript modules to `0644`. It
sets the application directory and `pi-tui.mjs` to `0755`. These settings
apply even when the build context was created with `umask 077`. Both build
stages examine the entrypoints as UID/GID `1024:1024`. The final stage also
imports the installed Pi runtime. These permission changes do not change HOME
or user credentials.

To adapt this example, keep that image layout, the three HTTP routes, and a
runnable shell/TUI command. Do not add ttyd or Kubernetes credentials to the
Agent image. A different base image must satisfy the initializer contract and
the Dockerfile's dependency checks. Publish under a new tag. Record the source
and image digest, and use that digest below. Publication is a separate
operation.

## MCR publication

After repository onboarding, the release pipeline publishes
`mcr.microsoft.com/aks/ai-runtime/cetirun/cetirun-pi-agent` and the shared
`cetirun-base`. Use an available release digest that your platform
administrator approved.

## Deploy an approved instance

Use an Operator/runtime Chart release that includes the Custom terminal and
its published `cetirun-terminal` image. The Operator provisions the terminal
ServiceAccount, restricted exec access, routing, and storage. Keep the current
gateway configuration. `ttyd.gatewayServiceAccount` must match its identity.

An administrator enables Custom provisioning and approves the exact image.
Custom platform model access always uses the AI Gateway.
The Operator provisions the Agent's Virtual Key and private Gateway access.
The image receives `OPENAI_BASE_URL` and a Secret-backed `OPENAI_API_KEY`.
Merge these settings into the platform values:

```yaml
cetirun-operator:
  customRuntime:
    enabled: true
    approvedImages:
      - mcr.microsoft.com/aks/ai-runtime/cetirun/cetirun-pi-agent@sha256:<published-digest>
    storageClass: default
    storageSize: 100Gi
    accessMode: ReadWriteOnce
    fsGroup: 1024
```

Gateway mode keeps its key in memory, ignores native `auth.json`, and disables
the image's local login listener. Pi discovers models through the Gateway with
its own key and runs its tools locally. The HTTP adapter still owns no history,
and Portal Chat supplies the context of each conversation. Automatic model and
compaction retries are disabled.

The adapter uses a 32K context estimate and a 2K output ceiling. These values
are not a claim about the actual limits of every model. Gateway and provider
limits still apply. Pi's local cost display is not the Gateway's usage ledger.

Shared models appear only when the Gateway grants access. If Copilot is enabled,
use **Sign in with GitHub** on the Portal Overview. The platform deploys a
separate proxy with a chart marked `cetirun.microsoft.com/custom-copilot: v1`.
Its token and proxy key never reach the Agent container or HOME. Ready with an
empty model list means that model access is still not complete. It does not
mean successful inference.

A Gateway configuration that is not set or that failed never selects native
login automatically. If the Gateway environment variables are partial, startup
fails. For a platform-managed Agent, all model access uses the Gateway.

The Operator prepares the namespace, canonical context, and HOME claim template
in one Chart installation. The StatefulSet creates the PVC and Pod. A PVC or UID
that already exists is not necessary. The default context includes
`cetirun-memory`, alongside approved platform and Team shared skills.

On AKS, `default` uses Azure Disk with `WaitForFirstConsumer`. The scheduling
of the Pod starts the binding. For Ready, both a Bound HOME and a ready workload
are necessary. The claim must match the configured storage profile. A change to
that profile does not migrate or resize a HOME that already exists. Storage
handoff and CSI/mesh qualification follow the
[Custom example](../custom-agent/README.md#platform-integration).

Create a new Agent. Replace the owner, digest, and Chart version:

```yaml
apiVersion: cetirun.microsoft.com/v1alpha1
kind: Agent
metadata:
  name: pi-agent
spec:
  team: example-team # An existing, approved Team.
  modelAccess:
    sharedModels: true
    githubCopilot: false
  user:
    alias: example-user
    displayName: Example User
  agent:
    type: custom
    image: mcr.microsoft.com/aks/ai-runtime/cetirun/cetirun-pi-agent@sha256:<published-digest>
    username: vibe
    port: 8080
  helm:
    version: "<chat-and-custom-copilot-enabled-chart-version>"
    values:
      ttyd:
        enabled: true
```

The platform creates the namespace and PVCs. Do not change the type of an
Agent that exists, and do not manually create its storage. To upgrade, edit the
image in Portal or change `spec.agent.image` to another platform-approved
digest. The Operator replaces the active Pod and uses its HOME PVC again, but
does not reseed it. Select an image that is compatible with the user, files,
and native sessions that exist. Deletion of an Agent deletes its namespace and
PVCs and is not a restart procedure.

Only `ttyd.enabled` is an instance override. The terminal image, port, and
gateway identity belong in the Operator's platform values, not in
`spec.helm.values`. Desktop is independent.
`spec.helm.values.desktop.enabled: false` disables Desktop but does not disable
Terminal.

## Open Pi from the Portal

For web chat, enable [Portal Chat](../../site/content/en/docs/guides/custom-chat.md).
Then select **Chat** in the instance. The Portal saves conversations in this
browser and sends their context with each request. Pi runs the model and tools.
This history is separate from Pi's native terminal sessions.

Open the instance and select **Terminal**. Then run:

```bash
pi-tui          # Continue the most recent saved native session
pi-tui --new    # Or start a new session
```

The platform's `cetirun-terminal` service opens a shell in the Agent container.
It tries `zsh`, `bash`, and then `sh`. This image does not install or start
ttyd. The browser uses `/<instance>/term/`. By default, the terminal Service
uses port 7681, which is separate from the Agent's HTTP port.

In Gateway mode, model login stays in Portal Overview. In image-owned mode,
approve Pi's Copilot model-policy updates before you use
`/login github-copilot`. Then select a model with `/model`. Complete the GitHub
device flow with your own approved account. Terminal access does not log you in
to Copilot. The image-owned mode has no Portal model-login action.

Pi works in `$HOME/workspace`. Native sessions stay in the private HOME PVC
under `.pi/agent`, together with image-owned credentials when that mode is
selected. Context and skills come from read-only `.agents`.

Pi saves a new native session after its first assistant response. As a result,
an empty or shell-only session is not a persistence test. A Pod replacement
opens a new shell. To reopen a saved session, run `pi-tui` again.

## Validate

### Source permissions

Select a cached image with Node.js 24 and a POSIX shell. Then run this command
from the repository root:

```bash
CETIRUN_PERMISSIONS_TEST_IMAGE=local/node:24 \
  python3 -B -m unittest scripts.tests.test_example_image_permissions -v
```

Replace `local/node:24` with that image. The suite builds both the Pi and the
VS Code `runtime-source` stages with `umask 077`. It examines non-root reads,
executable scripts, inaccessible modules and nested extension paths, and
unchanged HOME permissions. Builds and containers have no network. Containers
also have a read-only root filesystem and no host mounts or injected
credentials.

If the image variable is not set, the suite is skipped. If the selected image
is not present, the suite fails and does not pull the image. These source-stage
checks do not validate complete Pi or VS Code images, their installed SDK or
distribution, or their production base image.

### Runtime checks

The Node tests cover the HTTP adapter, the CLI entrypoint, and resource
discovery with the real SDK, without OAuth or model calls. Do these steps in a
separately authorized environment:

1. Make sure that Portal → Terminal → `pi-tui` works.
2. Run a synthetic `!printf` command.
3. After a Pod replacement, examine the terminal reconnection and a synthetic
   HOME file.

Copilot login, model calls, streaming/cancellation, and saved model-session
recovery must have separate live checks with an approved account and request
budget. Ready status, a terminal that works, or offline tests that pass do not
establish them.

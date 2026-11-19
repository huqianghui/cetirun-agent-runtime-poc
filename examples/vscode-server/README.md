# VS Code Server with GitHub Copilot

Run Microsoft's official VS Code Server as a CetiRun Custom Agent. The Agent
has a browser editor, an integrated terminal, and native Copilot Chat. Copilot
Chat uses the platform AI Gateway through Bring Your Own Key (BYOK). A GitHub
sign-in and a Copilot plan are not necessary for Chat and local Agent tool
workflows.

The HOME PVC keeps workspace files, installed extensions, and server state.
Browser-profile settings and BYOK secrets use VS Code's browser storage.
This example serves a Web UI, not the Custom Chat API.

Before you use this example, read the
[VS Code Server license](https://code.visualstudio.com/license/server).
Restrictions apply to how you host, share, and redistribute the server. A Pod
for each user does not authorize a public hosted service. A separate review is
necessary for image distribution. Copilot and extensions keep their own terms.

## Build

[vscode-lock.json](vscode-lock.json) pins the official CLI and web-server
artifacts to **1.139.1**. It has SHA-256 checksums for AMD64 and ARM64. The
build uses the CetiRun base image. An explicit license acceptance is necessary
for the build. From the repository root:

```bash
docker buildx build --platform linux/amd64 --load \
  --build-arg ACCEPT_VSCODE_SERVER_LICENSE_TERMS=true \
  -t cetirun-vscode:local examples/vscode-server
```

The image builder makes the license acceptance decision at build time. If you
use the approved prebuilt image, it is not necessary to pass this flag again.
The image contains the runtime acceptance setting.

For a custom `BASE_IMAGE`, supply Node.js 24, Git, Python with tarfile's `data`
extraction filter, and the CetiRun HOME initializer's tools. This example is
not published with the platform images. Deploy an approved image by digest.

The `runtime-source` stage gives mode `0644` to copied JavaScript modules and
mode `0755` to application directories. It also makes the bundled Gateway
extension readable and traversable, and it keeps executable scripts executable.
The build tests these files as UID/GID `1024:1024`. It also tests the installed
extension copy in the final stage. HOME and user credentials are not changed.

## Deploy

Use Operator, CRD, runtime chart, and Portal versions that match and have Custom
Web UI support. An administrator enables the runtime and approves the image:

```yaml
cetirun-operator:
  customRuntime:
    enabled: true
    approvedImages:
      - registry.example.com/cetirun-vscode@sha256:<approved-digest>
    storageClass: <qualified-storage-class>
    storageSize: 20Gi
    accessMode: ReadWriteOnce
    fsGroup: 1024
```

In [agent.yaml](agent.yaml), replace the example owner, image digest, and chart
version. Then apply the file to your approved cluster:

```bash
kubectl --context <cluster-context> apply -f examples/vscode-server/agent.yaml
```

The manifest sets `webUI: true` and `port: 8080`. These settings publish the
editor at `/ui/vscode-example/`. Platform Desktop and Terminal are disabled
because VS Code has its own terminal. The image runs as UID/GID 1024 with
`HOME=/home/vibe`. Let the platform provision its namespace and storage.

Every approved Custom image gets the Gateway binding. The Operator injects
`OPENAI_BASE_URL` and the instance's Secret-backed
`OPENAI_API_KEY`. The manifest enables shared models and disables the optional
Gateway Copilot upstream.

## Use

1. Wait until the Agent is Ready. Then select **Web UI** in the Portal.
2. When VS Code asks, trust your workspace. Then the bundled
   **CetiRun AI Gateway** configuration extension finds the models that the
   instance's Virtual Key allows. It adds a native **Custom Endpoint** group.
   Setup enables the bundled Copilot extension without a sign-in and without
   enrollment in a Copilot plan. Workspace trust and tool approval prompts
   still apply.
3. Open Chat. If you already have a saved model preference, select a model
   under **CetiRun AI Gateway**. New profiles use the first model in the sorted
   catalog as the default. Utility tasks also use a Gateway model. Use the local
   Chat Agent workflow, not a cloud-hosted coding agent.
4. Work in `$HOME/workspace`. Keep the browser site data. BYOK configuration
   and Secret Storage belong to the active VS Code browser profile, not only to
   the PVC.

The bootstrap keeps the user settings that already exist. It connects the
read-only `$HOME/.agents` instructions and Skills to Copilot. Examine
**Chat: Open Customizations** for the selected harness. Project instructions
stay separate.

## Gateway configuration and limits

The extension uses the pinned VS Code command `lm.migrateLanguageModelsProviderGroup`
to store the Virtual Key in native Secret Storage. `chatLanguageModels.json`
contains an `${input:chat.lm.secret...}` reference, not a plaintext key. The
extension does not keep the key in its own state. Protect the browser profile
data. Native Secret Storage is not an isolation boundary from the workspace
owner. An explicit `chat.aiDisabled` or legacy `chat.disableAIFeatures` setting
stops automatic configuration, and it does not enable AI features again.

Discovery uses `GET /v1/models`. Copilot sends model requests directly to the
Gateway's `POST /v1/chat/completions`. The configuration extension does not
proxy chat traffic. Discovery has a timeout and a catalog-size limit, and it
rejects redirects. It removes unresolved Gateway access-group names from the
model list.

The demo is for models that support streaming Chat Completions and tool calls,
with a context window of at least 128K. It configures text input with a
114,688-token input budget and a 16,384-token output limit. These values are
demo defaults, not discovered model capabilities. For models with different
limits or capabilities, change the native configuration. The sampling options
`temperature` and `top_p` are set to `null`. This value tells the pinned
Copilot implementation not to use its defaults and to use the model server's
defaults.

The extension refreshes its untouched model group and defaults when the catalog
or the injected key changes. It does this on startup or with
**CetiRun: Configure AI Gateway Models**. A Secret-backed environment variable
changes only after the container is restarted. The extension keeps other model
groups, comments, and explicit user settings. If you edit the Gateway group
yourself, it becomes user-managed and automatic refresh stops.

To return to managed configuration, remove that group in
**Chat: Manage Language Models**. Then run the CetiRun configuration command.
Before you do these steps, save the dirty model configuration.

Empty catalogs, denied requests, and invalid configuration report a setup
warning. They do not enable a GitHub login fallback. Partial Gateway environment
bindings cause startup to fail. Platform model access uses the Gateway. This
example does not manage browser credentials or configuration that already
exist. For a different mode, use a separate profile.

BYOK covers Chat, local Agent workflows, and utility tasks. It does **not**
supply GitHub's inline code completions, semantic search, or features that
depend on embeddings. For those features, the applicable GitHub account or
service access is still necessary. If the user signs in to a managed Copilot
account, the organization BYOK policy applies. This demo does not configure
experimental Agent Host sessions.

References: [VS Code language models and Custom Endpoint configuration](https://code.visualstudio.com/docs/agent-customization/language-models),
[BYOK without GitHub sign-in](https://code.visualstudio.com/blogs/2026/06/18/byok-vscode),
and the pinned [native configuration command](https://github.com/microsoft/vscode/blob/04c0d99f4fb0d8afe6ce4f0c58e31e183ac3e4b1/src/vs/workbench/contrib/chat/browser/actions/chatLanguageModelActions.ts).
When you update the VS Code lock, examine the internal command and
configuration schema again.

## Offline validation

From the repository root:

```bash
node --test examples/vscode-server/test/*.test.mjs
python3 -B -m unittest discover -s scripts/tests -p test_vscode_example.py -v
```

The tests use loopback servers, synthetic credentials, and a simulated VS Code
API. They cover Gateway discovery, native configuration handoff, secret
references, restart/key refresh, user-file preservation, and the Web IDE proxy
that already exists. They do not start VS Code or validate a real Copilot
request.

Qualify an approved image separately with a signed-out browser profile and a
native Agent tool task. Make sure that the Gateway request metadata matches. Do
this check again after a restart and after a key rotation. A separate
authorization is necessary for deployment and for live model calls.

The shared [source-permission suite](../pi-agent/README.md#source-permissions)
tests both examples with a selected cached Node image. It tests the actual
`runtime-source` stage, not the complete VS Code distribution.

# Shared base image versions

`Dockerfile.base` supplies the tools that both native agent images use. On
2026-10-08, the versions below were compared with the public upstream releases.
Standalone tool archives use version/commit and checksum pins. npm tools use
explicit release versions.

| Component | Selected version | Upstream source |
| --- | --- | --- |
| Azure Linux | 3.0, refreshed core image digest | [Releases](https://github.com/microsoft/azurelinux/releases) |
| Node.js | 24.21.0 LTS | [Release index](https://nodejs.org/dist/index.json) |
| npm | 12.2.0 | [Release notes](https://github.com/npm/cli/releases/tag/v12.2.0) |
| Microsoft Go builder | 1.27.3-1 | [Microsoft Go](https://github.com/microsoft/go) |
| Codex | 0.160.1 | [npm package](https://www.npmjs.com/package/@openai/codex/v/0.160.1) |
| Claude Code | 2.1.292 | [npm package](https://www.npmjs.com/package/@anthropic-ai/claude-code/v/2.1.292) |
| OpenCode | 1.18.35 | [npm package](https://www.npmjs.com/package/opencode-ai/v/1.18.35) |
| GitHub CLI | 2.102.0 | [Release](https://github.com/cli/cli/releases/tag/v2.102.0) |
| kubectl | 1.37.1 | [Release](https://github.com/kubernetes/kubernetes/releases/tag/v1.37.1) |
| Tailscale | 1.104.1 | [Release](https://github.com/tailscale/tailscale/releases/tag/v1.104.1) |
| ripgrep | 15.2.0 | [Release](https://github.com/BurntSushi/ripgrep/releases/tag/15.2.0) |
| uv / uvx | 0.12.23 | [Release](https://github.com/astral-sh/uv/releases/tag/0.12.23) |
| ttyd | 1.7.7, unchanged | [Latest stable release](https://github.com/tsl0922/ttyd/releases/tag/1.7.7) |
| Supervisor | 4.3.0, now explicitly pinned | [PyPI](https://pypi.org/project/supervisor/4.3.0/) |
| pip-vendored msgpack | 1.2.3 | [PyPI](https://pypi.org/project/msgpack/1.2.3/) |
| pip-vendored setuptools | 81.0.0, compatibility ceiling | [Removal of pkg_resources in 82.0.0](https://setuptools.pypa.io/en/latest/history.html#v82-0-0) |
| Oh My Zsh | `60c9a7a839b790cd905d0fd4419435124fd1bdc0` | [Upstream commit](https://github.com/ohmyzsh/ohmyzsh/commit/60c9a7a839b790cd905d0fd4419435124fd1bdc0) |

Later, on 2026-10-21, the Microsoft Go builder was updated to fix
[GO-2026-6809](https://pkg.go.dev/vuln/GO-2026-6809) in `crypto/x509`. All Go
builders use the same digest-pinned 1.27.3 release. This also applies to the
Go-built tools of the runtime image. The other selected dependency versions
are unchanged.

## Compatibility choices

Node stays on the LTS line that it already uses. Node 26.11.1 is the newer
Current release. At the review date, it was not yet an LTS release. The pinned
OpenClaw release accepts Node 24.21.0.

System packages, for example Python, pip, Git, and Azure CLI, follow the
supported Azure Linux 3.0 repositories through `tdnf install` and `tdnf update`.
They are not replaced with unrelated upstream major versions. Azure CLI 2.91.0
is both the current upstream release and the version available from the
distribution. System pip and the private pip of Azure CLI still must have
`pkg_resources`. As a result, their setuptools replacement stays at 81.0.0, the
last compatible release, and not at 84.0.0. After the payloads are updated,
both vendor inventories and SBOM references are examined.

Go 1.27 is now necessary for GitHub CLI and Tailscale. All shared Go builders,
module minimums, and `.go-version` move together. Microsoft Go 1.27 supports
cgo-less OpenSSL automatically. The removed `ms_nocgo_opensslcrypto` experiment
must not be passed to it. Tailscale archive builds explicitly stamp their
release version. The major/minor metadata of kubectl comes from its selected
version.

The [kubectl skew policy](https://kubernetes.io/releases/version-skew-policy/#kubectl)
says that the server must be within one minor version of the client. Before you
deploy this image, validate the target cluster separately. For example, the
legacy `infra/cluster/main.bicep` default is still Kubernetes 1.34. This
version is outside the supported range for this client. This image update does
not change that infrastructure default. It also does not make the image
compatible with a 1.34 cluster.

npm 12 supplies its own fixed undici dependency. The old in-place npm patch is
removed. CLI installs save exact versions so that later agent-layer installs
cannot change them through a caret range. Only the reviewed OpenCode, Claude
Code, and OpenClaw lifecycle scripts are approved, with
`npm install-scripts approve`.

The registry overrides and the `min-release-age=1` setting that already exists
stay in effect. npm measures this setting in days. As a result, tool pins must
refer to releases that are at least 24 hours old. At the time of the check,
Codex 0.161.0 and Claude Code 2.1.293 were inside that window. The selected
releases are the latest stable versions that this policy allowed at the review
time.

When the Pi example uses its older published base, it still fixes undici
6.26.0. It does not change the already-fixed 6.27.0 and 6.28.0 payloads. Its
SDK pin is unchanged.

Oh My Zsh has no stable-release version channel. The selected upstream commit
and archive checksum give an immutable snapshot of it. The uv and uvx tools are
now in `/usr/local/bin`, so that a persistent HOME cannot hide the updated
tools of the image. HOME files that already exist, for example older user-local
copies, are not deleted. This update does not migrate shared HOME contents,
credentials, agent provider settings, or permission defaults. The separate
desktop image and native agent releases are outside this base-tool update.

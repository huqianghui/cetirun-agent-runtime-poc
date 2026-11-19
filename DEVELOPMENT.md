# Developing CetiRun

Start with local tests and render checks. You can run these checks without an
Azure subscription, cluster access, or a live agent account. The download of
tools and dependencies is a separate setup step. Network access can be
necessary for this step.

## Repository layout

| Path | What to work on here |
| --- | --- |
| `app/cetirun-operator/` | Go Kubernetes operator, API types, and generated CRDs |
| `app/cetirun-portal/` | Go portal backend and its Helm chart |
| `app/cetirun-portal/web/` | React and TypeScript portal frontend |
| `app/cetirun-agent-broker/` | Owner Connections, Agent bindings, encrypted OAuth state, and internal credential issuance |
| `app/cetirun-connections-mcp/` | Workload-authenticated MCP service that runs JSON REST requests to fixed services |
| `app/internal/` | Shared Go implementation packages for Registry credentials and terminal policy |
| `app/cetirun-chart/` | Runtime Helm chart, container definitions, and runtime plugins |
| `infra/` | Infrastructure definitions and Istio configuration |
| `scripts/` | Component validation and operational helpers |
| `docs/` | Documentation pointers and project assets |
| `site/` | Hugo/Docsy documentation site, if the checkout includes it |

Put application implementation packages in `internal/`, not in `pkg/`. The code
that the Operator and Portal share is in the `app/internal` Go module. Go
limits imports of this module to modules under `github.com/Azure/CetiRun/app`.
The modules use local `replace` directives for these packages and the
Operator's API types. When you build or test `app/internal`,
`app/cetirun-operator`, and `app/cetirun-portal`, keep them in the same
checkout.

Not every script is a development check. A separately authorized environment
is necessary for deployment, release, and real-cluster test scripts.

### Deployment-specific configuration

The default platform entry point is `make deploy
DEPLOY_CONFIG=/path/to/environment.yaml`. To do a local Helm render without
cluster or registry access, use `make deploy-plan` with the same input. The
entry point manages the separate CetiRun Istio, Broker, and platform releases,
with Portal Chat, Connections MCP, and references to supported runtime
artifacts. It must have explicit inputs for the target, identity, Secret,
model/tier, storage, and owner. Refer to the
[platform deployment guide](app/cetirun-platform/README.md#deploy-the-platform).
The old invocation with only `DOMAIN`/`VALUES_FILE` is no longer enough.

For an existing AKS managed Istio ingress, configure `publicTLS` in the
environment file used by `make deploy`. `make deploy-plan` renders its
certificate-only umbrella values offline, `make deploy` prepares public DNS,
cert-manager HTTP-01 resources, and the full platform, and `make deploy-verify`
performs the read-only public endpoint check. See `infra/cert-manager/README.md`.
These operations do not replace the full platform deployment prerequisites.

`scripts/cetirun-dev-deploy.sh` must have an explicit `REGISTRY` and alias
argument. It no longer inserts a default user team label. To see its modes,
use `--help`. The other helpers for build, deployment, and Helm publication
keep their registry defaults and overrides.

Package defaults use public npm and PyPI. The `NPM_REGISTRY`,
`OPENCLAW_NPM_REGISTRY`, `skills.npmRegistry`, and `PYPI_INDEX_URL` inputs are
still available for other mirrors. Image tags, chart versions, and the
`BASE_IMAGE` override are still available.

## Tools and setup

Install tools only for the component that you change:

- Git, Bash, Make, and curl for repository commands and loopback download tests.
- Go 1.27.3, which is the same version as `.go-version`, all module minimums,
  and the pinned Microsoft Go builder images.
- Node.js 24 and npm for the portal frontend, as in its CI configuration.
- Helm to lint and render charts locally.
- Python 3 for the Operator test runner and the standard-library tests under
  `scripts/tests/`.

When you update tool versions, examine the component manifests and CI. Keep
dependency changes separate from unrelated work.

From your checkout, use a meaningful branch, for example
`git switch -c fix/instance-status`. For the issue and review process, refer to
[CONTRIBUTING.md](CONTRIBUTING.md).

For Go components, fetch dependencies one time when network access is
available:

```bash
go -C app/internal mod download
go -C app/cetirun-operator mod download
go -C app/cetirun-portal mod download
go -C app/cetirun-agent-broker mod download
```

Install the frontend's locked dependencies from the public npm registry:

```bash
make -C app/cetirun-portal frontend-install NPM_REGISTRY=https://registry.npmjs.org
```

Dependency installation is not an offline check. Do not supply production
credentials to resolve a development dependency that is not available.

## Current validation entry points

Run the commands that follow from the repository root. Select the checks for
the components that your change affects.

If the checkout includes the root Makefile, use `make check` for offline
validation, `make test` for unit tests, and `make lint` for static checks.
Before you run these commands, install the necessary tools and dependencies
separately. If `site/` exists, `make docs-check` runs the checks of the
documentation site.
These commands do not authorize cluster or cloud access. For an earlier
checkout without these entry points, use the component commands below. Report
the narrower validation scope.

### Shared internal packages

```bash
go -C app/internal test ./... -count=1
go -C app/internal vet ./...
go -C app/internal test -race ./agenttools ./tlsreload -count=1
```

The root `make go-test` and `make go-lint` targets include this module.
When you change shared behavior, also run the tests of the consumer modules.

### Operator

```bash
make -C app/cetirun-operator test
go -C app/cetirun-operator vet ./...
make -C app/cetirun-operator build
```

The default unit target discovers all packages with `go list ./...`. It tests the
controller package in two complementary groups, because the combined suite took
more than 30 minutes on CI runners. One invocation selects the lifecycle test
prefixes in `GO_CONTROLLER_TEST_GROUP`. The next invocation skips exactly those
prefixes. Every controller test runs one time, and every other package runs in
full. Each package or controller group keeps a 30-minute timeout.

The default `make test-envtest` runs the same seven packages in two
complementary passes. The first pass runs the tests whose names match
`APIServer`, and the second pass runs all other tests. Both passes keep the
`envtest` build tag, disable cached results, and use main's bounded 50-minute
timeout per package. The split only divides the run time, and the first pass
does not contain all API tests. API tests with other names run in the second
pass. A failure in either pass fails the target.

Root Go test gates and CI use the same component targets. Default local runs
keep both complete partitions described above. Operator CI instead distributes
both targets across four independent hash shards with the shared runner. Neither
mode omits tests or changes individual test assertions or runtime/lifecycle
deadlines.

To reproduce one CI shard, set its one-based index and the total shard count:

```bash
make -C app/cetirun-operator test GO_TEST_SHARD_INDEX=1 GO_TEST_SHARD_COUNT=4
KUBEBUILDER_ASSETS=/path/to/local/envtest \
  make -C app/cetirun-operator test-envtest GO_TEST_SHARD_INDEX=1 GO_TEST_SHARD_COUNT=4
```

Run indices 1 through 4 to cover a full sharded suite. The shared runner in
`scripts/ci/go_test.py` runs `go test -json -list .` with the same packages and
build tags to discover tests. Then it assigns each top-level test name to
exactly one shard with a stable hash. New tests are included automatically.
Identical names in different packages stay in the same shard.

Every selected test runs all its subtests. Examples and fuzz seed tests are
included, but benchmarks are not enabled. Discovery failures, invalid indices,
empty shards, and test failures fail the command. No test assertions or
runtime/lifecycle deadlines change.

Each CI worker keeps its 60-minute job limit, and a failure does not cancel
the other shards. The `Operator Go checks` status combines both job matrices,
and it passes only when every shard succeeds. If a worker fails, is skipped, or
is cancelled, the status fails. Generated-API, Portal, build, and vulnerability
checks stay in their worker jobs.

The build writes `app/cetirun-operator/bin/manager`. It does not start the
operator. The component's `make test` and `make vet` targets are also
available. To examine the format of changed Go files, use `gofmt -l`. Use
`gofmt -w` only on files that you intend to change.

For a disconnected Go check with dependencies that are already cached, start
the command with `GOPROXY=off GOTOOLCHAIN=local GOFLAGS=-mod=readonly`. If a
toolchain or dependency is not available, make sure that the check fails and
does not start an implicit download.

### Custom terminal

The Custom terminal uses its own image and
`app/cetirun-operator/Dockerfile.terminal`. Its Go entrypoint shares the
Operator module so that it can use the same Agent API types. But the
entrypoint runs as a separate Deployment with its own ServiceAccount. It does
not start the Operator.

```bash
go -C app/cetirun-operator test ./internal/terminal -count=1
node --test app/cetirun-operator/internal/terminal/reconnect.test.cjs
make -C app/cetirun-operator build-terminal
```

The same build target also builds `bin/loopback-probe`. This static HTTP probe
is packaged with the terminal image for governed Custom HTTP workloads. The
focused common Custom checks are:

```bash
go -C app/cetirun-operator test ./cmd/loopback-probe ./internal/helm ./internal/sandboxpolicy ./internal/controller -run 'TestCommonCustom|TestCustomSandbox|TestCompileCustom|TestProbe' -count=1 -timeout=20m
go -C app/cetirun-agent-broker test ./internal/broker -run '^TestCustom' -count=1
npm --prefix examples/pi-agent test
python3 -B -m unittest discover -s scripts/tests -p test_custom_initialization.py -v
KUBEBUILDER_ASSETS=/path/to/local/envtest \
  go -C app/cetirun-operator test -tags=envtest ./internal/controller -run '^TestCommonCustom(Terminal)?APIServer$' -count=1 -timeout=20m
KUBEBUILDER_ASSETS=/path/to/local/envtest \
  go -C app/cetirun-operator test -tags=envtest ./internal/controller -run '^(TestNativeEntraCommonRuntimeAPIServer|TestNativeConnectionsServedCurrentSourceProof|TestReconciledSandboxIngressMTLSAPIServer)/custom$' -count=1 -timeout=20m
```

These cover both registered backends, all three Custom endpoint modes, the
generated policy schema, current source RBAC, final-Pod admission, retained
restart/suspend/resume/migration, and isolated terminal authority. API-server
tests use synthetic scheduler/kubelet/Gateway observations. They are not a live
qualification of the model, tools, Cilium, CSI, identity provider, or images.
When you change Custom owner access, run the normal Portal backend, frontend,
and chart checks.

Portal mTLS Chat regressions use synthetic output from the policy compiler, not
a handwritten plaintext network rule. The compiler test examines the fixture on
every run. After an intentional contract change, regenerate the fixture with
the pinned Go toolchain:

```bash
CETIRUN_UPDATE_CHAT_FIXTURE=true go -C app/cetirun-operator test ./internal/sandboxpolicy -run '^TestCustomMTLSPortalFixture$' -count=1
go -C app/cetirun-portal test ./internal/kube ./internal/handler -run 'TestCommonChat|TestChatOwnerBindingAndForwarding' -count=1
```

These are offline endpoint/dispatch checks. The Portal transport stays HTTP
to its local mesh. These checks do not qualify live Istio/SDS credentials or
TLS handshakes.

The image build is a separate step. It can download pinned base images and Go
dependencies:

```bash
REGISTRY=registry.example.com PUSH=false bash scripts/terminal-docker-build.sh
```

`make build-images` and `make publish-images` include `cetirun-terminal`.
Neither local tests nor the image build does a cluster exec, a model login,
real gateway authentication, or a Pod restart. For those paths, use tests in a
separately authorized environment.

### Portal

For a frontend change, run the tests and the production build:

```bash
npm --prefix app/cetirun-portal/web test
npm --prefix app/cetirun-portal/web run build
```

The build includes TypeScript checks and creates `web/dist`. Because the Go
entry point embeds that directory, build the frontend before you test or build
the entire portal module:

```bash
go -C app/cetirun-portal test ./... -count=1
go -C app/cetirun-portal vet ./...
make -C app/cetirun-portal build
```

The frontend build also generates `web/dist/THIRD_PARTY_LICENSES.txt` from the
graph of installed production dependencies. The Portal embeds this file and
serves it at `/THIRD_PARTY_LICENSES.txt`. No separate server or authentication
rule is added.

If a necessary dependency/license file does not exist, the build fails unless
`web/licenses/supplements.json` records an exact, checksum-verified supplement.
When you update dependencies, examine the supplements. Do not use old version
approvals again. `npm test` includes offline generator regressions.

Portal CI examines the known supplement versions before it installs
dependencies. If a new entry is necessary for a version, CI makes sure that the
public upstream metadata is correct. CI also makes sure that the attribution
files did not change. Then CI saves a proposed `portal-license-update` patch
for review. CI does not apply the patch or make the build check less strict.
For the local command, the cases for manual review, and the tarball checks,
refer to the
[supplement update procedure](app/cetirun-portal/web/licenses/README.md).

To examine the notice without a frontend build, run this command after
`npm ci`:

```bash
npm --prefix app/cetirun-portal/web run licenses -- --output /tmp/cetirun-portal-notices/THIRD_PARTY_LICENSES.txt
```

The generator reads local package metadata and upstream license files. It does
not use network access or run dependency code. Keep the generated output out of
Git. This JavaScript notice does not cover Go dependencies, image packages,
artwork, commercial software terms, or corresponding-source obligations. The
root `NOTICE` keeps the manual attributions and the unresolved
provenance/distribution items.

For backend-only work without frontend dependencies or build output, this
narrower check covers the internal packages. It does not compile the embedded
UI:

```bash
go -C app/cetirun-portal test ./internal/... -count=1
go -C app/cetirun-portal vet ./internal/...
```

Report that narrower scope. This check does not validate the frontend or the
complete portal binary. Do not use a placeholder embed directory as a frontend
build.

The retained lifecycle APIs also have an isolated API-server test. If you
already installed the pinned Kubernetes 1.37 binaries that are described
below, run:

```bash
KUBEBUILDER_ASSETS=/path/to/local/envtest \
  go -C app/cetirun-portal test -tags=envtest ./internal/kube ./internal/handler -run '^Test(RetainedRestartThroughServedAPI|SandboxDefaultsThroughPortalAndServedAPI)$' -count=1
```

It tests the persisted restart identity, the conditional suspend/resume intent,
duplicate requests, the rejection of a stale UID/version, and CRD validation.
The Sandbox case compares omitted/null runtime defaults and full
resource/storage fields through the Portal and the served Kubernetes API. This
comparison also includes conditional updates. It starts its own API server and
etcd and never uses a cluster that already exists. It does not run Pods,
connect to a model provider, or qualify storage/failure recovery. Operator CI
also runs these tests with its pinned admission-test tools.

Planned migration has a focused Operator API-server regression that uses the
same local assets:

```bash
KUBEBUILDER_ASSETS=/path/to/local/envtest \
  go -C app/cetirun-operator test -tags=envtest ./internal/admission -run '^TestPlannedMigrationServedAPIAndBinding$' -count=1
```

It tests the generated migration schema, durable finalizer authorization, and
both real Pod binding APIs. The binding tests include replacement Nodes that
have the same name. Normal Operator tests use synthetic observations to cover
the order of source/CSI acknowledgements, repeated moves, partial/lost writes,
suspension, and deletion. These checks do not run kubelets or CSI controllers.
They are not AKS validation.

The common-executor RWO HOME contract has an isolated API-server regression:

```bash
KUBEBUILDER_ASSETS=/path/to/local/envtest \
  go -C app/cetirun-operator test -tags=envtest ./internal/controller -run '^TestReconciledSandboxRWOHomeAPIServer$' -count=1
```

It tests served Agent authorization, the common chart render, current Pod
admission, and the immutability of the HOME access mode. Normal controller
tests use synthetic Pod/CSI observations to cover both RWO and RWOP retained
restarts, coordinated shutdown/resume, and repeated healthy-source migration.
These are offline contracts. They are not a qualification of the runtime, file
retention, model spend, or CSI.

These focused offline regression tests are for the explicit
ordinary-container backend:

```bash
go -C app/cetirun-operator test ./api/v1alpha1 ./internal/sandbox ./internal/sandboxplatform ./internal/sandboxworkload -count=1
go -C app/cetirun-operator test ./internal/controller -run '^TestContainerSandbox' -count=1 -timeout=20m
KUBEBUILDER_ASSETS=/path/to/local/envtest \
  go -C app/cetirun-operator test -tags=envtest ./internal/controller -run '^TestContainerSandbox(Common|IngressMTLS)APIServer$' -count=1 -timeout=20m
```

These cover explicit registration/handler pins, common render/admission,
per-container resources without implicit VM overhead, both built-in harnesses,
RWO/RWOP HOME, retained restart, suspension/resume, and healthy-source
migration. Combined container/mTLS tests use the common endpoint path, examine
resource accounting and certificate approval pins, and reject approvals that do
not exist or that changed. The API-server test uses synthetic Gateway,
scheduler, kubelet, and storage observations. The Portal's Sandbox API-server
test above also covers explicit container and gVisor values together with
omitted/null/default Kata. For the runtime selector, use the frontend tests and
the real production build above. None of these checks runs runc or Kata,
qualifies CSI/Cilium, or gives live acceptance.

Common Sandbox policy updates also have a focused served-API regression:

```bash
KUBEBUILDER_ASSETS=/path/to/local/envtest \
  go -C app/cetirun-operator test -tags=envtest ./internal/controller -run '^TestReconciledSandboxPendingRateLimitsAPIServer$' -count=1
```

It adds and removes rate limits before the first Ready observation, with both
RWO and RWOP HOME. This includes a policy status that becomes current during
bootstrap. It tests that the workload, Pod, key, and reservation identities do
not change, and it rejects mixed execution-control edits. Unit tests also cover
retained restarts, both built-in harnesses, full source comparisons, and
final-apply/submission guards. The Gateway, scheduler, and kubelet observations
are synthetic. This local API test does not qualify live traffic, Kata, or
storage behavior.

Kata internal CIDR grants have a focused served-API regression:

```bash
KUBEBUILDER_ASSETS=/path/to/local/envtest \
  go -C app/cetirun-operator test -tags=envtest ./internal/controller -run '^TestReconciledSandboxInternalNetworkAPIServer$' -count=1
```

It tests configuration binding, default TCP ports, owned Cilium policy output,
protected network conflicts, active source reservations, and Pod admission
after policy drift. Normal compiler/controller tests also cover address/port
boundaries, managed-peer isolation, deny precedence, and grant removal for
OpenClaw/Hermes with RWO/RWOP. These are offline checks, not a qualification of
the Cilium dataplane. For the separate authorized live witnesses, use the
[internal-egress example](examples/native-sandbox/README.md#scoped-internal-egress).

Kata Kubernetes access has a focused served-API regression:

```bash
KUBEBUILDER_ASSETS=/path/to/local/envtest \
  go -C app/cetirun-operator test -tags=envtest ./internal/controller -run '^TestReconciledSandboxKubernetes(Access|PostSubmitRecovery|RejectedGrantRecovery|FirstInstallRejection)APIServer$' -count=1
```

It uses real API RBAC and admission for OpenClaw and Hermes. The test includes
administrator-approved namespace and cluster-wide grants, token projection
validation, role-version drift, access withdrawal, and journal/finalizer
cleanup after restart with new starts disabled. Unit tests also cover approvals
that do not exist, foreign approvals, unconfirmed or shared bindings, lost
apply acknowledgements, suspension/resume, and nil-access isolation.

First-install recovery tests inject a failed final authority read before Helm
receives its manifest. A first attempt stays pending until a workload write is
authorized. As a result, retry and deletion can conditionally cancel the
attempt without a downgrade of a submitted record. A served-API regression
tests the cancellation and retry with real conditional writes. Applies that
were authorized earlier, claims that compete, and unconfirmed cancellations
keep their shutdown requirements.

Rejected-grant tests resume stopped Agents that temporarily do not have binding
authority, and then test retry and normal deletion. First-install rejection
tests also examine cancellation before any workload write, unchanged lifetime
identities on retry, and normal deletion.

Helm SDK tests examine the submission immediately before workload CREATEs or
update batches. These checks include first-install adoption, grant rejection,
uncertain responses, and disabled HTTP retries. Controller tests fence delayed
rejection receipts and cover failed/lost outcome writes and independent
uncertain-grant cleanup barriers. All fixtures are synthetic. For kubelet token
rotation, Kubernetes client requests from a running Agent, and Cilium dataplane
enforcement, separately authorized live validation is necessary. Refer to the
[administrator setup](examples/native-sandbox/README.md#kubernetes-api-access).

Registered Kata SCTP rules have a focused served-API regression that uses the
same local assets:

```bash
KUBEBUILDER_ASSETS=/path/to/local/envtest \
  go -C app/cetirun-operator test -tags=envtest ./internal/controller -run '^TestReconciledSandboxSCTPNetworkAPIServer$' -count=1
```

It tests persisted SCTP/default-TCP fields, native Cilium compilation, bindings
that conflict, malformed requests, and active policy reservations. It also
tests Pod admission after control drift and controller holds after
registration drift. Compiler tests cover exact protocol/port isolation, deny
precedence, dual-family rules, and unchanged resource budgets. Controller tests
cover both built-in harnesses and HOME modes.

The API server is local. The observations of running Pods are synthetic. These
checks do not qualify SCTP traffic, Kata, Cilium, NAT, or Services. For the
requested-versus-qualified contract, refer to the
[SCTP example](examples/native-sandbox/README.md#governed-sctp-egress).

The current portal `make lint` target calls `npm run lint`, but the frontend
does not define that script. Until a lint entry point that works is added, use
Go vet, format checks, and the frontend build above.

### Managed Agent ingress mTLS

The focused offline checks for the registered common-executor endpoint are:

```bash
go -C app/cetirun-operator test ./internal/sandboxpolicy ./internal/controller -run 'TestIngressMTLS|TestReconciledSandboxIngressMTLS' -count=1
python3 -B -m unittest discover -s app/cetirun-chart/tests -p test_hermes_ingress.py -v
python3 -B -m unittest discover -s app/cetirun-chart/tests -p test_desktop_ingress.py -v
KUBEBUILDER_ASSETS=/path/to/local/envtest \
  go -C app/cetirun-operator test -tags=envtest ./internal/controller -run '^TestReconciledSandboxIngressMTLS((Preflight|FinalGuard)Recovery)?APIServer$' -count=1
CETIRUN_ENVOY_BINARY="$PWD/.tools/envoy" \
  go -C app/cetirun-operator test -tags=envoy ./internal/ingressmtls -count=1
```

The API-server command uses the same pinned local assets that are described
above, real configuration coordination, RBAC, and Pod admission. The workload
readiness observations stay synthetic.

The transport command must have a separately restored Envoy **1.36.3**
executable. The upstream release asset for Linux x86-64
`https://github.com/envoyproxy/envoy/releases/download/v1.36.3/envoy-1.36.3-linux-x86_64`
has SHA-256
`6dcacec73f0ff18d2f68f605f9f041cd0e1c4d30d3c38977058cd923daa2dea6`.
Verify the asset before you run it. The tests do not download tools. They use
real loopback TLS with generated, short-lived test keys and remove their
listeners, processes, and files.

To also test the real native API/Control UI and ttyd, set
`CETIRUN_OPENCLAW_IMAGE` to an explicitly approved cached OpenClaw 2026.9.6
image. To test the Hermes API/Webapp and ttyd, set `CETIRUN_HERMES_IMAGE` to
an explicitly approved cached Hermes 0.22.7 image. This test uses a fresh HOME,
retained bind overrides, and a supervised restart after the overrides are
added again. The tests use the candidate configuration assets and the HOME
authority helper. No model request is sent.

`CETIRUN_DESKTOP_IMAGE` enables a cached websockify/noVNC HTTP/WebSocket TLS
check with synthetic RFB/CDP backends. This check examines the candidate bind
commands and readiness. It does not qualify Xvfb, Chrome, or graphical
interaction.

These optional tests run Docker with `--pull=never`, no network, a read-only
root, a synthetic in-memory HOME, and only read-only test-tool mounts. They do
not use host credentials, a cluster, a model provider, or a deployment. If the
applicable variable is not set, that native-image test explicitly skips. The
Envoy transport tests still run. Qualification of the exact target image and
qualification of real Istio/Kata/Cilium stay separate. Refer to the
[usage and rotation contract](site/content/en/docs/guides/agent-ingress-mtls.md).

### Managed observed-SNI egress

The [managed egress contract](app/cetirun-operator/README.md#managed-observed-sni-egress)
uses the same controlled Envoy as ingress mTLS and a narrowly scoped capture
init. It has explicit one-second uninspected-timeout semantics, not full domain
enforcement. The focused offline checks are:

```bash
go -C app/cetirun-operator test ./internal/egresscapture ./internal/sandboxpolicy -count=1
go -C app/cetirun-operator test ./internal/controller -run '^TestReconciledSandboxObservedSNI' -count=1
KUBEBUILDER_ASSETS=/path/to/local/envtest \
  go -C app/cetirun-operator test -tags=envtest ./internal/controller -run '^TestReconciledSandboxObservedSNIAPIServer$' -count=1
```

The API test uses local Kubernetes binaries and real Pod admission, not a live
cluster or kubelet. Controller tests cover source drift, the final apply guard,
retained restart, suspend/change/resume, and deletion. They keep the mTLS
source and private Copilot routing behavior. An explicit
`CETIRUN_EGRESS_PROOF_DIR` exports synthetic rendered Pod/config/probe fixtures
from the focused controller test for a separately authorized isolated Docker
check. It does not export live data.

The trusted init image is digest-pinned platform configuration. The CetiRun
base image that already exists supplies its network tools. Runtime capture does
not install packages, inherit an Agent image override, or use copied host
binaries.

### Agent Identity Broker

The Broker owns owner-scoped Connections, per-instance ConnectionBindings,
OAuth/OBO, and encrypted credential storage. CetiRun Connections MCP is a
separate native remote MCP server in `app/cetirun-connections-mcp/`. It
verifies a Pod-bound workload token. It asks the Broker for an independent
check of the instance and authorization. It runs JSON REST requests to fixed
Azure, GitHub, Microsoft Graph, and Google service profiles.

Provider tokens never enter Agent workloads. There is no MCP bridge or direct
Agent token-issuance route. Run the checks of both components with these
commands:

```bash
go -C app/cetirun-agent-broker test ./... -count=1
go -C app/cetirun-agent-broker vet ./...
make -C app/cetirun-agent-broker build
bash scripts/cetirun-agent-broker-helm-test.sh
make -C app/cetirun-connections-mcp test vet chart-lint
python3 -B -m unittest scripts.tests.test_native_mcp_runtime -v
python3 -B -m unittest discover -s app/cetirun-chart/tests -p test_connections_mcp.py -v
```

These are offline checks. They do not call Microsoft Entra, the Auth SDK, Key
Vault, ARM, Resource Graph, GitHub, Google, or a Kubernetes cluster. Native
Hermes and OpenClaw transports include hash-guarded patches. For each request,
these patches read the workload token, which rotates. These transports do not
run a separate bridge process. Held Sandbox and Custom runtimes do not support
this integration and fail closed.

Refer to [CetiRun Connections MCP](app/cetirun-connections-mcp/README.md) and
the [authorization guide](site/content/en/docs/guides/agent-identity.md) for
the TLS reload, Broker service identity, CA distribution, and Agent connection
binding setup.

Custom Agent Identity requests are rejected before the identity is provisioned
or the workload is prepared. The previous `cetirun-agent az rest` wrapper and
its provider-token path are removed. Custom images cannot enable Agent Identity
through the legacy Chart annotation or the typed Broker binding. Focused
offline regressions cover this boundary:

```bash
go -C app/cetirun-operator test ./internal/runtimebindings ./internal/helm ./internal/controller -run 'TestCustomAgentIdentity' -count=1
```

Focused offline regressions cover credential policy application/readback:

```bash
go -C app/internal test ./agenttools -run 'TestCredentialPolicy' -count=1
go -C app/cetirun-agent-broker test ./internal/broker -run 'TestCredentialPolicy' -count=1
go -C app/cetirun-connections-mcp test ./internal/tools -run 'TestCredentialPolicy' -count=1
go -C app/cetirun-operator test ./internal/controller -run 'TestCredentialPolicy' -count=1
```

They cover owned storage, the current-policy/identity match, failed and lost
replies, read-before-retry, status conflicts, removal, and the provider
execution boundary. Unqualified native runtimes still reject stored policy as
authority. The current native integration has more offline checks:

```bash
go -C app/internal test ./agenttools -run 'TestNative' -count=1
go -C app/cetirun-agent-broker test ./internal/broker -run 'TestNative|TestCredentialPolicy' -count=1
go -C app/cetirun-connections-mcp test ./internal/identity ./internal/tools -run 'TestNative|TestCredentialPolicy' -count=1
go -C app/cetirun-operator test ./internal/controller -run 'TestNativeConnections' -count=1
KUBEBUILDER_ASSETS=/path/to/local/envtest \
  go -C app/cetirun-operator test -tags=envtest ./internal/controller -run '^TestNativeConnectionsServedCurrentSourceProof$' -count=1
```

With both harnesses, the API-server test uses the real common chart render,
owned bootstrap, API-issued Pod-bound tokens and TokenReviews, source/UID pins,
and Pod admission. It also uses current credential evidence that reaches
`PolicyApplied`. Broker/MCP served-interface tests use synthetic Kubernetes,
admission, directory, and provider responses.

The API-server case rejects a new token for a recreated ServiceAccount against
the old source pin and clears the applied evidence. The Broker readback fixture
of the API-server test is gated by the real admission handler. This fixture is
not a deployed Broker/MCP/Operator system. Scheduler/kubelet observations
stay synthetic. These checks do not qualify live Kata/Cilium routing, TLS
delivery, Entra federation, provider permissions, or an Azure tenant.

`npm --prefix app/cetirun-portal/web run dev` starts a local frontend server.
Its API proxy expects a backend on localhost and injects development identity
headers. Keep this server local. It is not a production authentication
configuration. If you start the Go backend or operator, it uses Kubernetes
configuration. This step is outside the offline validation loop.

### Skill Marketplace

The operator runs the Skill Marketplace, which publishes approved skills from
one Git repository to the native Sandbox Agents of policy-bound Teams. The
code has two parts:

- `app/cetirun-operator/internal/skillmarketplace/` runs in the operator. It
  lists the policy-bound Teams for the worker, and it creates the read-only
  skills claim of each of their Agents.
- `app/cetirun-operator/cmd/skill-marketplace/` is the worker. It syncs the
  repository, publishes the selection of each Team to Blob NFS storage, and
  writes the catalog. It is a second binary of the operator image. The
  operator chart runs it in a separate Deployment. The worker reads
  repository content, holds the repository credentials, and writes to the
  storage.

`make deploy` enables the Skill Marketplace from the `skillMarketplace` block
of the environment file. Run its offline checks with these commands:

```bash
go -C app/cetirun-operator test ./internal/skillmarketplace/... ./cmd/skill-marketplace/ -count=1
python3 -B -m unittest scripts.tests.test_skill_marketplace_platform \
  scripts.tests.test_full_deployment.SkillMarketplaceTests -v
```

These checks do not contact a Git remote, mount Blob NFS storage, or call a
cluster. Refer to
[Skill Marketplace](site/content/en/docs/operations/skill-marketplace.md) for
the repository, storage, and security requirements.

### Sandbox Entra projection core

The generic `AgentPolicy.spec.entraID` projection is different from Broker
Agent Identity and credential-service policy. You can test its compiler, the
common chart render, and the current Namespace/ServiceAccount binding checks
offline. Native Entra must have an explicit capability/approval and an observed
bootstrap-owned ServiceAccount UID before workload use. A render alone does not
allow execution.

```bash
go -C app/cetirun-operator test ./internal/sandboxpolicy ./internal/sandboxworkload ./internal/helm ./internal/controller -run '^Test(Entra|NativeEntra)' -count=1 -timeout=20m
go -C app/cetirun-operator test -race ./internal/sandboxpolicy ./internal/sandboxworkload ./internal/helm ./internal/controller -run '^Test(Entra|NativeEntra)' -count=1 -timeout=20m
KUBEBUILDER_ASSETS=/path/to/local/envtest \
  go -C app/cetirun-operator test -tags=envtest ./internal/helm ./internal/controller -run '^(TestEntraIdentityBindingAPIServer|TestNativeEntraCommonRuntimeAPIServer)$' -count=1 -timeout=20m
```

The API test starts its own API server/etcd with synthetic identity approvals.
It tests real namespace and ServiceAccount UIDs, metadata drift, confirmed
ServiceAccount deletion/replacement, and the absence of Kubernetes RBAC that
was not requested. The native case tests positive persisted Pod admission, an
accepted create with a lost response, controller restart, and denial after
source/UID drift. It does not run a kubelet/container, request an Entra token,
connect to a cloud/identity API, or qualify federation, Azure permissions, or
cloud-token revocation.

Controller tests cover bootstrap pin interruption, retained restart,
suspension, approval refresh, and projection removal for both harnesses. In
these cases, HOME, Gateway keys, and the recorded identity are kept. Combined
Entra/mTLS cases also test in-place rate-limit updates. Retained bootstrap
tests enable Entra after the original workload was stopped. For the integration
that is still necessary, refer to the
[Entra runtime projection core](app/cetirun-operator/README.md#entra-runtime-projection-core).

### Runtime chart and Python helpers

The [shared base image version notes](app/cetirun-chart/base-image.md) record
the selected tool releases and compatibility ceilings.

```bash
bash scripts/cetirun-helm-lint.sh
bash scripts/cetirun-helm-test.sh
python3 -B -m unittest discover -s scripts/tests -v
```

The Helm scripts lint and render the runtime chart for OpenClaw and Hermes.
The scripts also cover cases with values that are not valid. They use dummy
inputs and do not install resources. They do not validate every chart in the
repository or prove that a live deployment works.

The [Runtime image CVEs workflow](.github/workflows/docker-build-validate.yml)
uses an explicit ten-minute Trivy timeout for each runtime image scan. This
timeout stays within the job time limit that already exists. Scan errors and
timeouts still fail the vulnerability gates. The severity thresholds and the
enabled scanners did not change. These image builds/scans run in CI, not as
part of the offline commands above.

The Hermes image build allows up to 30 minutes for each source transfer. It
uses at most three retries, a 15-second retry delay, and a 30-minute
retry-start window. With this configuration, the build does not discard a
download that makes progress on slow runners, and the retries stay bounded. The
pinned source checksum is verified before extraction. The install stamp must
match the selected release and commit, with external image updates; the package
metadata version is not the release identity. Download, checksum, and identity
failures still cause the image build to fail. The job timeout of the
workflow stays the same.

The [Memory Context workflow](.github/workflows/memory-context-ci.yml) downloads
its pinned Hermes and OpenClaw qualification hosts separately. These downloads
retry transient failures, for example HTTP 429, at most three times with a
15-second retry delay and a two-minute retry-start window. Their five-minute
transfer limit and the 30-minute limit of the job stay the same. Connections
also have a 15-second timeout. Permanent HTTP errors and integrity mismatches
still cause a failure before host installation or qualification. Offline
helper tests send real curl requests to loopback fixtures to cover recovery and
retry exhaustion.

The Memory native suite also runs the installed helper through its CLI. It
terminates its own helper process after a committed HEAD switch, then checks
physical deletion, damaged-note recovery, and later writes. The same check can
run against a separately installed helper:

```bash
python3 -B app/cetirun-memory-context/tests/cli_recovery.py \
  --helper /path/to/cetirun-memory --work-dir /path/to/writable-test-directory
```

This Linux-only check creates and removes disposable synthetic stores inside
the selected existing directory. It does not use the Agent's active Memory
store or call a model. A canary run requires separate cluster authorization and
does not replace native conversation or retained-restart acceptance.

Hermes adapter tests use the same PM-locked Python 3.14 environment as the
image. Prepare its tools and dependencies separately with
`agents/hermes/pm/build.py` under the runtime chart, using a synthetic
`HERMES_HOME` and the checksum-verified source. Select that source and its
`.venv/bin/python` through `MEMORY_HERMES_SOURCE` and `MEMORY_HERMES_PYTHON`
when running `node --test app/cetirun-memory-context/tests/native.test.mjs`.
The host checks validate the install stamp, native hook and middleware calls,
default channel access, configured source restrictions, child inheritance and
bounded model requests to loopback.
Run the same interpreter with
`-B scripts/tests/hermes_mcp_host.py --source /path/to/pinned/hermes` to test
the native MCP SDK over loopback TLS. This includes workload token rotation,
CA verification, and rejection of redirects and SSE fallback. Both checks use
synthetic HOME directories and do not access an existing Agent.

The focused browser storage test uses a prepared Playwright 1.64.0 and Chromium:

```bash
PLAYWRIGHT_MODULE=/path/to/node_modules/playwright \
CHROMIUM_EXECUTABLE=/path/to/chrome \
node scripts/tests/hermes_webapp_storage.cjs
```

This checks storage, storage events and BroadcastChannel isolation between
instance prefixes on loopback pages. It needs no Hermes image, upstream source
checkout or model endpoint. It does not exercise public login or a deployed
cluster.

The Python command tests repository helpers with local fixtures. The Istio mesh
reconciliation tests must have the pinned PyYAML dependency in
`infra/istio/requirements.txt`. The Python command does not run the auth and
native initialization suite.

For that suite, prepare the locked dependencies in
`scripts/ci/requirements.txt` separately. Then run `make python-test`. It runs
the real init scripts on synthetic HOME directories, uses synthetic network
responses, and blocks outbound network connections. OpenClaw initialization
tests must also have `jq`. Keep live Agent or other service integration tests
separately authorized.

Operator render tests keep the original native configuration and resource
baselines. Wiring tests and executable initialization tests separately examine
the rewritten init assets and `CETIRUN_INIT_IMAGE`. Do not replace baseline
fixtures with changed output only to make a regression pass.

Custom runtime tests run through the same Go and Python suites. They cover
typed platform bindings, Chat HTTP, Web UI and headless renders, final manifest
validation, and the common HOME initializer. The VS Code example has a separate
offline suite for its prefix proxy, browser storage, and restart behavior:

```bash
node --test examples/vscode-server/test/*.test.mjs
```

These tests start only loopback fixture servers and use synthetic files and
credentials. They do not start VS Code, run Docker, or connect to
GitHub/Copilot. Image builds, rendered IDE checks, and real Copilot tasks are
separate checks. Refer to [the example](examples/vscode-server/README.md).

`make build-images` and `make publish-images` include the `cetirun-pi-agent`
demo and VS Code Server. For usage and tests, refer to
[the Pi README](examples/pi-agent/README.md). Before any build, the full image
set must have an explicit `ACCEPT_VSCODE_SERVER_LICENSE_TERMS=true`.

### Unified release inventory

[`scripts/release-artifacts.json`](scripts/release-artifacts.json) is the
common inventory for root Make targets and the MCR release pipeline. It
contains 14 images and 9 charts. These include the Broker, VS Code, Memory and
ADX-Mon (`cetirun-observability`) assets. `make artifact-check` examines source
paths and chart coverage. The
driver calls the image builders that already exist, stamps the explicit
version/source SHA, and builds the base image before its consumers. It never
logs in or installs tools.

`REGISTRY` defaults to `mcr.microsoft.com/aks/ai-runtime/cetirun`, including
the repository prefix. `VERSION` defaults to the Runtime Chart version.
Both accept environment variables or Make command-line assignments; an
assignment on the command line takes precedence. For a local test build:

```bash
REGISTRY=registry.example.com/team/test VERSION=0.1.1-dev.1 \
  make build-images ACCEPT_VSCODE_SERVER_LICENSE_TERMS=true
```

Set the license acceptance only after accepting the VS Code Server terms.
MCR is a public pull endpoint, not a push destination. Its default is used
for local image tags and chart packaging. Local builds can shadow official
tags in the local Docker store; use an explicit test registry prefix to keep
them separate. Deployment requires a matching, complete published release;
the source version alone does not establish artifact availability or runtime
compatibility. Until that release has synchronized to MCR, use explicit
registry/version coordinates for a complete compatible test release.
`deploy-plan` renders source offline and does not check published artifacts;
`deploy` checks artifact availability before writing and prints the effective
registry/version and their origins. Direct Runtime Dockerfile builds must
provide an available `BASE_IMAGE`; the build wrappers select its versioned
tag instead of relying on `latest`. Publishing (`make
publish*`, component chart publishers, `PUSH=true`, or component `docker-push`)
requires an explicit writable registry and rejects MCR before a push. For
official MCR publication, use the existing ADO pipeline: it pushes to the
backing ACR, whose artifacts are automatically synchronized to MCR. Do not
run `make publish REGISTRY=mcr.microsoft.com/...` or use the backing ACR as
the deployed chart's public pull address.

Build and publish targets do not edit the source checkout. Image builds always
copy tracked and nonignored working-tree source files to a temporary directory
and preserve their modes. When the version differs, the driver runs the existing
version setter there. Image-only builds need no Helm; chart packaging rebuilds
dependencies and their lock in its separate copy. Ignored dependencies, local
configuration and Git metadata are not copied. The build therefore includes uncommitted source
changes; `GIT_SHA` identifies the base commit, not a clean-tree guarantee.
Only `make set-version VERSION=X.Y.Z` changes the checked-in release version.
`make version-check` still checks source consistency, not a proposed build.

Chart packages contain the selected registry and version defaults, including
the Operator's Runtime Chart URL/version, embedded subcharts and first-party
runtime images. A registry prefix such as `registry.example.com/team/test`
is retained when the Operator resolves OpenClaw, Hermes, Desktop, Terminal
and Memory images. Third-party images and explicit Custom image/digest pins
keep their own coordinates. Existing Agents keep their pinned Runtime Chart
and image; an explicit chart upgrade advances a default image only within the
same registry. Registry migration requires an explicit image-pin migration as
described below. Charts use the fixed `helm` namespace; `CHART_NAMESPACE` and
`--chart-namespace` reject other values. Put all repository prefixes in
`REGISTRY` so packaged URLs and the Operator's runtime image paths agree.

The component `*-helm-release.sh` entry points use the same packager and
publish only their selected chart. `CHART_VERSION` and `APP_VERSION` can
still override `VERSION` separately. `CHART_DIR` selects a source override;
`VALUES_FILE` supplies additional lint/render values, not packaged secrets.
These publishers now use the shared registry preflight and mandatory chart
pull comparison, so they require Python and Helm. Chart existence checks use
Helm's registry authentication; image checks use Docker buildx. They do not
log in. `VERIFY_PULL=false` does not disable their pull comparison.

After registry authentication and publication authorization:

```bash
make publish REGISTRY=registry.example.com VERSION=0.1.1 \
  GIT_SHA="$(git rev-parse HEAD)" ACCEPT_VSCODE_SERVER_LICENSE_TERMS=true
```

`publish` examines every destination image/chart tag before a build or push.
A tag that already exists or an unknown registry error stops it.
`FORCE_PUBLISH=true` explicitly allows the replacement of tags. An ordinary
retry does not allow it. After a partial publication, use a new version unless
replacement is authorized.

Every chart is pulled back and compared with its package. `VERIFY_PULL=false`
is rejected for the unified release. `build-images` builds locally and does
not publish. `publish-images` and `publish-charts` select one artifact class.

The MCR pipeline uses `REGISTRY=mcr.microsoft.com/...` only for local chart
packaging, so the package contains public pull coordinates. Image staging,
image promotion and every Helm push still target the backing ACR; MCR
serves the automatically synchronized artifacts. The public MCR checks are
reads, not pushes. The pipeline uses the same
inventory and builders. It keeps staged pushes,
digest verification, and the promotion of `latest` only after all versioned
images and charts succeed. It passes the
`ACCEPT_VSCODE_SERVER_LICENSE_TERMS=true` value, which is already configured in
the pipeline, to the shared driver. As a result, a new manual approval
parameter is not necessary for the automatic tag releases that already exist.
Local build/publish callers still supply their own explicit acceptance. The
manual-only `forcePublish` control that already exists stays separate.

Offline release/deployment regressions use synthetic APIs and local Helm:

```bash
python3 -B -m unittest scripts.tests.test_release_inventory scripts.tests.test_full_deployment -v
python3 -B -m unittest scripts.tests.test_connections_mcp_release -v
make version-check artifact-check python-check license-check
```

These tests never build an image with Docker or connect to a registry/cluster.
They test the real image build wrappers with a command double and package real
local charts. These tests do not examine production image builds, registry
publication, OAuth, provider calls, or live deployment.

### Migrating an existing Agent registry

Changing the Operator's Runtime Chart URL also changes the registry used to
qualify built-in Agent images. Existing `spec.agent.image` pins are not rewritten.
If a pin belongs to the previous registry, reconciliation rejects it with
`AgentConfigInvalid` before Helm applies a workload; a Portal chart-version
upgrade alone does not migrate that pin. The same applies when a corrected
chart URL starts preserving a previously ignored repository prefix.

Before upgrading an installation that used the previous defaults, explicitly
retain its existing chart URL/version (`cetirun.chart.repoURL` and
`cetirun.chart.version`) and image coordinates. The previous registry default
was `cetirun.azurecr.io`; use the coordinates actually deployed, including any
custom overrides. With a registry-prefixed chart URL, the new Operator uses the
full prefix rather than the old hostname-only interpretation, so retaining that
URL alone does not retain the old image registry.

To move registries, first publish and qualify compatible images and charts at
the target coordinates. In an authorized migration window, update the Operator's
chart URL and each built-in Agent's `spec.agent.image` together with its selected
`spec.helm.version`: use `<target-registry>/openclaw-gateway:<appVersion>` or
`<target-registry>/hermes-gateway:<appVersion>` from the selected chart. Existing
Sandbox Agents must use their normal coordinated configuration-change workflow;
do not bypass admission or lifecycle controls with a raw patch. Keep Custom
images and explicitly qualified digest pins unchanged unless their owners
authorize a separate migration. A registry change is not evidence that images
from both registries are equivalent.

### Release-agent build storage

The Azure Pipelines release job prepares Docker storage before it builds
images. It uses `dockerStorageMount` (default `/mnt`) for Docker layers,
BuildKit cache, and build scratch files instead of the OS disk.

The helper logs filesystem sizes, free space, and inode usage. The helper must
have at least 64 GiB of free space on the selected mount and on both real write
destinations. After it restarts the daemon, it examines the active Docker data
root. Child mounts that already exist and a Docker daemon that is already
configured get the same capacity checks. This free-space threshold is a
guardrail, not a measured maximum release-build size.

`scripts/ci/prepare_docker_storage.py` is an operational helper for **disposable
Linux CI agents only**. It is not a local validation command. It must run as
root. It refuses to interrupt running containers, keeps unrelated daemon
settings, and restores the original configuration if activation fails. It uses
Docker's classic `overlay2` image store because the containerd image store does
not follow Docker's `data-root`.

Old image/cache directories are not deleted or migrated. The release starts
with a fresh local Docker store, keeps staged pushes, and prunes the cache
between images. Later tasks get scratch storage through `TMPDIR`.

If a temporary disk is too small, configure a larger data disk on the hosted
pool. Then set `dockerStorageMount` to its Linux mount path. If you only add a
disk, Docker storage does not move. An increase in the number of agents also
does not move Docker storage. The pipeline helper does not change the hosted
pool, resize disks, or change other agents.

Offline helper and pipeline-contract tests run with `make ci-test`. They mock
Docker and service operations. Only a separately authorized Linux-agent run can
validate the real mount layout, daemon activation, and release disk headroom.

### Documentation

Examine relative links, heading anchors, examples, and the commands that you
changed. If the checkout includes `site/`, run `make -C site check` to build
and validate the documentation. The root `make docs-check` runs this command.
Network access can be necessary for the setup of site dependencies. Run
`git diff --check` for whitespace errors. Examine both new files and tracked
changes.

## Generated files and dependencies

Run `make license-check` to examine the supported first-party source and
Makefiles in all parts of the repository. This check also examines the release
and E2E scripts. The `CetiRun license headers` CI job runs this check on pull
requests and on pushes to `main`. To add a header to a file that does not have
one, refer to [the header helper commands](scripts/README.md#license-headers).
Before you add a header, examine the authorship of the file. The third-party
and generated/scaffold exceptions in `scripts/license_headers.py` must keep
their own notices.

- The Operator API definitions are in `app/cetirun-operator/api/v1alpha1/`.
  `zz_generated.deepcopy.go` and the YAML in `config/crd/bases/` are generated
  from those types. Include the regenerated artifacts with their source changes.
- `controller-gen v0.21.0` is necessary for the operator. When network setup is
  authorized, install it separately with `make -C app/cetirun-operator tools`.
  This command installs it into `app/cetirun-operator/bin`. Add that directory
  to PATH, or set `CONTROLLER_GEN` to its absolute path. Then run
  `GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local make -C app/cetirun-operator generate manifests`.

  These targets examine the version and regenerate the deepcopy methods and the
  CRD YAML. They also regenerate the Operator chart's copies of the CRD YAML in
  `chart/templates/crds/`. They do not download tools. The metadata markers in
  the API types generate the CRD retention annotations. Do not edit the
  generated YAML to add these annotations.

  `make -C app/cetirun-operator check-crds` examines the packaged copies.
  `verify-generated` runs the generation again and compares both copies and
  the deepcopy methods against Git. Review generator upgrades separately. The
  Operator, Portal, and operations-agent RBAC are still source Helm/YAML files.
  They must name the applicable API resources. Do not edit generated output by
  hand.
- `make -C app/cetirun-operator test-envtest` covers Agent API defaulting,
  compatible/strict decoding, and CEL transitions. It also covers Sandbox
  configuration admission and its outage/cleanup boundary. Set
  `KUBEBUILDER_ASSETS` to local Kubernetes 1.37 test binaries that are prepared
  separately. Helm 3 or 4 is also necessary to render the actual admission
  match rules.

  The target starts isolated API server/etcd processes with RBAC. If the
  binaries do not exist, the target fails. It never uses a cluster that already
  exists.

  Loopback TLS tests cover the independent server process and certificate
  replacement. Version-skew fixtures compare the strict rejection of new status
  fields with the compatible behavior for these fields. Helm tests upgrade the
  retained CRD templates while requests continue. Then the tests make sure that
  the CRDs stay after an uninstall. Controller tests make sure that unknown
  status is kept and that resource-version conflicts are handled. A normal
  `go test ./...` command runs unit and schema/serialization tests, but it does
  not run envtest.

  The same target also tests trusted Team assignment and metadata-only UID
  reauthorization through the rendered webhooks. It also tests final-Pod
  validation and the native namespace marker/ownership guard. The guard tests
  include webhook outages and namespace status/finalize updates. These are
  isolated API-server tests. They are not runtime/Cilium/CSI qualification, and
  they do not give permission to remove the execution hold.

  The focused `make -C app/cetirun-operator test-sandbox-integration` target
  runs against an isolated API server. It examines the protected
  namespace/HOME bootstrap, the ownership of published skills, and the
  Gateway-only rejection of unsupported held profiles. Repeated reconciliation
  and a controller restart must not create a workload, publish a route, or mark
  those profiles ready. The native and common-executor controller tests that
  are described below cover the valid Gateway start paths. These tests use
  synthetic platform observations. They do not start containers, install a
  production provider, qualify storage, or authorize deployment to a cluster
  that already exists.

  Normal unit tests also cover stale inputs, metadata-only
  refresh/reauthorization, partial writes, restart, grant revocation,
  suspension, and ordered cleanup. Regression tests count the read/write
  requests for unchanged preparation. They make sure that Agent status changes
  do not invalidate the authorization intent. They also make sure that
  generation changes, reauthorization, and lifecycle holds invalidate it. The
  versioned expectation transition is fail-closed, and it does not change CRDs.
  For the platform prerequisites and the start/observation gates that are still
  necessary, refer to the [operator integration boundary](app/cetirun-operator/README.md#held-controller-integration).

  After a run fails because a tool does not exist, you can provision the
  checksum-pinned Linux amd64/arm64 bundle into a new task-local directory:

  ```bash
  # Networked setup, separate from offline validation; refuses overwrites.
  python3 -B scripts/ci/install_tools.py --bin-dir /tmp/cetirun-envtest-1.37.0 envtest
  KUBEBUILDER_ASSETS=/tmp/cetirun-envtest-1.37.0 \
    make -C app/cetirun-operator test-envtest
  ```

  This command installs only the manifest-pinned `kube-apiserver`, `etcd`, and
  `kubectl` binaries. It does not install a cluster or replace global tools, and
  it is not a runtime qualification.

  Native controller cases in `test-envtest` cover the current single-policy Team
  contract through real Agent/Pod admission and Helm SDK actions. They cover
  concurrent HOME binding during readiness and two Agents that share a policy
  with independent HOME and model keys. They also cover guarded policy mutation
  and ordinary cleanup after a restart with new native starts disabled.
  Platform, Gateway, scheduler, and kubelet observations stay synthetic. These
  cases are not native UI/model/tool acceptance, physical fencing, or CSI
  qualification.

  The focused `make -C app/cetirun-operator test-sandbox-lifecycle` target tests
  durable writer-history and start/shared-change coordination with those same
  isolated API servers. The provider responses and the physical machine/storage
  identities are synthetic. It tests persistence, CAS races, restart recovery,
  and the behavior for unknown outcomes. It does not test actual fencing, volume
  release, or live Agent execution. Unit tests also cover the native
  held-preparation bridge, pre-start acknowledgement, current observations, the
  route/readiness order, and the journal-backed writer integration. These checks
  do not install or implicitly qualify a production provider.

  The coordination package and `cmd/sandbox-config-change` tests cover concrete
  configuration updates. Their API-server cases test real conditional writes and
  observation-only recovery, but they do not test a runtime provider. Both
  `test-envtest` and `test-sandbox-lifecycle` run the production CLI client
  against isolated coordinating admission. These runs also include
  metadata-only updates of minimal manifests. For an unchanged normalized spec,
  the writer keeps the authoritative spec representation and does not
  materialize omitted objects. This keeps generation and acceptance current, and
  it does not weaken policy checks.

  A controller API regression reconciles Agents from two Teams that share a
  policy before any acceptance refresh. It examines compute, routes, readiness,
  and keys. Current acceptance is still necessary for actual spec edits.
  Admission allows only an unchanged generation or one increment for the exact
  reserved mutation. This rule also applies to older delayed submissions. File
  inputs still keep the original generation, UID, and resource version.

  A `Changing` operation that already exists is never resubmitted. Recovery
  must find the same live UID at a different resourceVersion. Exact desired
  writable content proves that the update applied. Exact before content proves
  that the old conditional update was superseded and can safely be released. An
  unchanged resourceVersion or any other content stays uncertain.

  Unit tests also cover status refreshes while `Reserved`, concurrent recovery,
  and stale edit rejection. Local API-server tests examine real status-write
  conflicts, restart recovery, rejection of delayed stale updates, and
  successful fresh updates.

  Build the explicit command with
  `make -C app/cetirun-operator build-sandbox-config-change`. Do not run it
  against a cluster as part of an offline check. It must have separately
  authorized before/after configuration inputs and an explicitly pinned
  authority.

  The same suites cover conditional configuration deletion and observation-only
  recovery with the original before input. For a Portal deletion with recorded
  recovery metadata, the input can be its operation ID without a body. Both
  paths keep the same UID/metadata safety, and they do not prove physical
  cleanup.

  For records that already exist without the optional metadata, `--before` is
  necessary. Upgrade all strict journal readers before you issue new metadata.
  Real API cases test UID/version conflicts, replacement protection, served
  deletion admission, and independent finalizer cleanup. These cases do not use
  a live cluster or a physical writer.

  Native-start and common-executor tests examine Gateway-backed handoff, Pod
  admission, and authenticated ingress with isolated API servers. Run
  `go test -tags=envtest ./internal/controller -run
  '^(TestNativeSandboxStartAPIServer|TestReconciledSandboxIngressMTLSAPIServer)$'
  -count=1` from the Operator module, with the same explicit local envtest
  assets. The common-executor ingress cases cover OpenClaw and Hermes. These
  tests do not contain a scheduler, kubelet, or guest-external runtime observer.
  Their synthetic enrollment is not a runnable activation procedure.
- Helm files in `templates/` are source templates. Rendered Kubernetes
  manifests are validation output. They are not replacements for those
  templates.
- When dependencies change, keep `go.mod` and `go.sum` together. Keep the
  portal's `package.json` and `package-lock.json` consistent. Use `npm ci` for
  installation.
- Dependabot groups shared Go builder updates across the application
  directories. The runtime base exposes its default image in the `go-toolchain`
  stage, and `GO_BUILD_IMAGE` still accepts an explicit override. New Go
  components must be included in the update directories for Docker and Go
  modules. CI makes sure that every Go builder is discoverable and shares the
  pinned digest. A Go release upgrade must also change `.go-version` and every
  module's `go` directive to match. Grouped Docker updates do not make those
  changes.
- Do not commit binaries, `web/dist`, `node_modules`, Python caches or virtual
  environments, coverage output, rendered secrets, or local configuration.
- Keep third-party copyright, license, and source attribution.

## Before review

Examine `git status --short` and your diff. Include tests and documentation for
behavior changes. Examine your contribution against the
[public-data boundary](CONTRIBUTING.md#keep-contributions-safe-to-publish).
List the exact validation that you did. Keep a passed offline check separate
from an integration, deployment, or release check that you did not run. Never
describe a limited test run as proof that every security check passed.

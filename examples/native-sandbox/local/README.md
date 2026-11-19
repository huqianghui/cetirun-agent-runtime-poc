# Local native Sandbox validation

This directory contains a bounded local platform probe. It is not a production
qualification service. The probe combines the network smoke tests that exist
with these items:

- Observations of the actual Kata host and guest resources.
- Read-only filesystem checks.
- Node and DNS traffic controls.
- Ordinary CSI ReadWriteOncePod (RWOP) contention and reopen checks.

First, read the [native first-run guide](../README.md) and the
[development checks](../../../DEVELOPMENT.md). The probe must have an explicit
local Kind name, node-container identity, kubeconfig, context, and expected
cluster UID. It never selects a cluster from ambient configuration. Run it only
on a disposable local environment that you own.

## Tested local stack

The local integration used:

- Kind 0.33.0 / Kubernetes 1.37.0, with the API and HTTPS ingress exposed on
  loopback only.
- Real Kata Containers 4.2.0 QEMU/KVM, not a renamed runc RuntimeClass.
- Cilium 1.20.2 and the upstream CSI host-path test driver, not Azure CNI or
  Azure Disk qualification.
- Istio 1.31.1, a real local Dex 2.46.0 OIDC issuer, and OAuth2 Proxy with a
  task-local CA. Local OIDC is an explicit chart opt-in. The Entra defaults do
  not change.
- The actual CetiRun Operator, admission server, Portal, runtime chart, and
  OpenClaw 2026.9.6 image.
- The actual CetiRun AI Gateway/PostgreSQL/Valkey, backed by local Qwen3-4B
  inference with llama.cpp. A canned model response is not equivalent.

The tested Agent used 4 GiB requested / 8 GiB limited application memory.
This memory size is a tested local choice. It is not a minimum or a
recommendation for production capacity. To diagnose a VMM termination, use the
actual host limits and OOM counters. Do not expect that a larger memory limit
fixes the termination.

### Kind/Kata details that matter

Kata's QEMU shared memory must fit inside the **node container's private**
`/dev/shm`. The usual Docker default of 64 MiB is not sufficient. Examine the
actual mount and its ownership before you set its size. Do not remount the
development machine's shared-memory filesystem.

When the node's systemd hierarchy makes it necessary, use the upstream Kata
`[runtime] sandbox_cgroup_only = true` setting. Then create a fresh sandbox.
Make sure that the VMM and all its threads inherit finite host CPU and memory
limits. Make sure that these limits include the RuntimeClass overhead.
Configuration text alone is not evidence. The probe reports the numeric limits,
ancestry, guest kernel, and guest limits.

Runtime images can use `imagePullPolicy: Always`. In that case, it is not
sufficient to only load an image into Kind. Supply the exact image through a
local registry that the node can resolve and trust, alongside the versioned
runtime chart. Keep TLS verification enabled. A local CA certificate must be
readable by the nonroot processes that use it. Its directories must be
traversable by these processes.

## Run the bounded probe

Install the dependencies separately with the pinned tools that
[DEVELOPMENT.md](../../../DEVELOPMENT.md) describes. In this command, use your
own task-local paths and resource names. Make sure that the resource names are
correct before you run the command:

```bash
python3 -B examples/native-sandbox/local/platform-probe.py \
  --cluster-name cetirun-local-example \
  --node-container cetirun-local-example-control-plane \
  --context kind-cetirun-local-example \
  --cluster-uid-file /absolute/private/cluster-uid \
  --kubeconfig /absolute/private/kubeconfig \
  --kubectl /absolute/tools/kubectl \
  --docker-config /absolute/private/docker \
  --probe-image python@sha256:8cdd496996889eef0a8a7921c152c0a4a9f18a26c7a5e7257d3c90b431891894 \
  --runtime-class kata-qemu \
  --runtime-handler kata-qemu \
  --storage-class local-native-home \
  --output /absolute/private/local-platform-probes.json \
  --allow-local-test-resources
```

`baselineRequiredSatisfied` is true only when all necessary probe checks
passed. Do not replace a false or unknown observation with a declaration of
support. The helper keeps `qualified`, `qualifiesExecution`,
`enforcementObservation`, and `restrictiveFailure` as false. It does not test
production failover or physical writer fencing. Keep its receipt private,
because the receipt contains invocation paths and deployment metadata. Before
you share any excerpt, review and minimize it.

Offline helper tests:

```bash
python3 -B -m unittest discover \
  -s examples/native-sandbox/local -p 'test_*.py'
```

## Finish the actual Agent run

When this probe passes, only the platform step is complete:

1. Populate the administrator-owned native registration from the current
   RuntimeClass and CSI identities, the actual topology, and the observed
   controls. For this non-Azure environment, select `native-single-start` and
   the generic `cilium` adapter. Bind the approved service peers to their
   current backend Pods. Keep their protected peer labels in workload templates.
2. Install the matching components with native execution and enforced Agent
   and Pod admission. Configure one real Gateway model and normal owner
   authentication. Do not copy provider credentials into AgentPolicy or public
   files.
3. Create a Sandbox, an AgentPolicy, a Team, and an Agent through authorized
   APIs. Make sure that the actual application is ready. A created Pod or a
   declared capability alone is not sufficient.
4. Complete the owner login and the explicit browser-device pairing with the
   Portal credential and owner web-terminal flow that exist. Make sure that
   nonowner and anonymous access is denied. Make sure that the authenticated
   native WebSocket connection succeeds.
5. Send a real task through the Agent. Correlate a harmless tool request with
   a fresh nonce at an independently observed task-owned endpoint.
6. Delete that same Agent with UID and resourceVersion preconditions. Make sure
   that the Gateway grant is revoked and the live sessions are closed. Make
   sure that the owned namespace and PV are removed.

Admission replicas on distinct Nodes are still necessary for the deployment
preflight. A one-node local stack does not meet that availability condition.
Report it separately. Do not weaken the check. Do not describe a local
lifecycle success as multi-node admission availability or production
qualification.

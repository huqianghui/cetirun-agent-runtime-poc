# Scoped AKS native Sandbox checks

This guide covers the existing Kubernetes API checks that can be run on an
authorized AKS target. They are narrow smoke and deployment observations. They
do not install a runtime, create an Agent, or qualify a provider for execution.

The local [`platform-probe.py`](../local/platform-probe.py) has a Kind, Docker,
QEMU/KVM, and hostpath CSI contract. Do not run it against AKS or treat its
local receipt as AKS evidence.

Keep the target kubeconfig, context, cluster UID, deployment UIDs, and image
pins in a private run directory. Obtain those pins independently before each
run. The commands never select an ambient context or log in for credentials.

## Runtime and storage prerequisites

Run the read-only prerequisite check with the exact RuntimeClass, CRI handler,
and StorageClass that the target is intended to use:

```bash
python3 -B scripts/sandbox-platform-qualify.py preflight \
  --kubeconfig "$TEST_KUBECONFIG" --context "$TEST_CONTEXT" \
  --cluster-uid "$TEST_CLUSTER_UID" \
  --runtime-class "$RUNTIME_CLASS" --runtime-handler "$RUNTIME_HANDLER" \
  --storage-class "$STORAGE_CLASS"
```

This checks selected Kubernetes API objects only. A matching RuntimeClass does
not establish that a Kata VM is running. A StorageClass does not establish CSI
persistence or writer fencing.

## Existing deployment wiring

Inspect the selected Operator and admission deployments with independently
recorded UIDs and digest-pinned images:

```bash
python3 -B scripts/sandbox-platform-qualify.py deployment-preflight \
  --kubeconfig "$TEST_KUBECONFIG" --context "$TEST_CONTEXT" \
  --cluster-uid "$TEST_CLUSTER_UID" --namespace "$CETIRUN_NAMESPACE" \
  --operator-deployment "$OPERATOR_DEPLOYMENT" --operator-uid "$OPERATOR_UID" \
  --operator-image "$PINNED_OPERATOR_IMAGE" \
  --operator-service-account "$OPERATOR_SERVICE_ACCOUNT" \
  --operator-service-account-uid "$OPERATOR_SERVICE_ACCOUNT_UID" \
  --admission-deployment "$ADMISSION_DEPLOYMENT" --admission-uid "$ADMISSION_UID" \
  --admission-image "$PINNED_ADMISSION_IMAGE" \
  --admission-service-account "$ADMISSION_SERVICE_ACCOUNT" \
  --admission-service-account-uid "$ADMISSION_SERVICE_ACCOUNT_UID"
```

If the target uses AKS system namespace exclusions, an administrator can add
`--allow-aks-system-exclusions`; it accepts only the documented exact AKS
selectors. It does not alter a webhook or its failure policy.

## Synthetic network smoke

Only on a disposable, authorized target, run the TCP smoke with a digest-pinned
Python image:

```bash
python3 -B scripts/sandbox-platform-qualify.py network-smoke \
  --kubeconfig "$TEST_KUBECONFIG" --context "$TEST_CONTEXT" \
  --cluster-uid "$TEST_CLUSTER_UID" --storage-class "$STORAGE_CLASS" \
  --runtime-class "$RUNTIME_CLASS" --runtime-handler "$RUNTIME_HANDLER" \
  --probe-image "$PINNED_PYTHON_IMAGE" --allow-test-resources
```

Use `--network-protocol UDP` for the separate UDP smoke. The runner creates and
UID-pins only its own synthetic resources before ordinary cleanup. It does not
guarantee placement in a requested node pool, the same node, or different nodes;
repeating it does not establish topology coverage.

Every result intentionally keeps `qualifiesExecution: false`. None of these
checks writes capability registration or grants an execution override. Host VMM
identity and aggregate resource limits, CSI persistence/fencing, DNS and
node/network topology, authentication entry points, and Agent lifecycle still
need separately reviewed, target-specific evidence.

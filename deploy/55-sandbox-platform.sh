#!/usr/bin/env bash
# 生成沙箱执行需要的平台登记：HOME 盘的 StorageClass 和 cetirun-system/sandbox-platform ConfigMap。
# 为什么需要：56 开启了 sandboxSources（agent 跑在 Kata 沙箱里），operator 从这个 ConfigMap 读运行时能力和
# 网络拓扑，再给每个 agent 生成 CiliumNetworkPolicy。上游默认不生成它，要按自己集群的实际情况提供。
# 拓扑里的 IP、网段、UID、Cilium 版本都从集群现查，不写死；集群升级或换了网关之后重跑本脚本即可。
# 入口端口 SANDBOX_INGRESS_PORTS 决定入口网关能连 agent 的哪些端口：漏了 6080，Desktop 就会连接超时。
source "$(dirname "$0")/lib.sh"
export PATH="$ROOT/bin:$PATH" KUBECONFIG="$ROOT/state/kubeconfig"

log "1. HOME 盘的 StorageClass ${SANDBOX_HOME_STORAGE_CLASS}（${SANDBOX_HOME_DISK_SKU}）"
kubectl apply -f - <<EOF
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: $SANDBOX_HOME_STORAGE_CLASS
provisioner: disk.csi.azure.com
parameters:
  skuName: $SANDBOX_HOME_DISK_SKU
reclaimPolicy: Delete
volumeBindingMode: WaitForFirstConsumer
allowVolumeExpansion: true
EOF

log "2. 从集群读拓扑"
kubectl get runtimeclass kata-vm-isolation -o name >/dev/null \
  || die "没有 RuntimeClass kata-vm-isolation：Kata 节点池（20-cluster.sh）还没就绪"
GW_SVC_IP=$(kubectl get svc -n cetirun-system cetirun-ai-gateway -o jsonpath='{.spec.clusterIP}' 2>/dev/null || true)
[ -n "$GW_SVC_IP" ] || { warn "AI 网关还没装：先跑 56，它会在部署完后再调用本脚本"; exit 0; }
NET=$(az aks show -g "$RG" -n "$CLUSTER_NAME" --query "{pod:networkProfile.podCidr,svc:networkProfile.serviceCidr,subnet:agentPoolProfiles[0].vnetSubnetId}" -o json)
SUBNET_ID=$(jq -r '.subnet // empty' <<<"$NET")
# 托管 VNet（没有自带子网）时 AKS 固定用 10.224.0.0/12；自带 VNet 时取子网的地址段
NODE_CIDR=10.224.0.0/12
[ -n "$SUBNET_ID" ] && NODE_CIDR=$(az network vnet subnet show --ids "$SUBNET_ID" --query addressPrefix -o tsv)
export SANDBOX_INGRESS_PORTS SANDBOX_HOME_STORAGE_CLASS NODE_CIDR GW_SVC_IP \
  POD_CIDR="$(jq -r .pod <<<"$NET")" SVC_CIDR="$(jq -r .svc <<<"$NET")" \
  DNS_IP="$(kubectl get svc -n kube-system kube-dns -o jsonpath='{.spec.clusterIP}')" \
  API_IP="$(kubectl get endpointslices -n default -l kubernetes.io/service-name=kubernetes -o jsonpath='{.items[0].endpoints[0].addresses[0]}')" \
  INGRESS_DEPLOY="$(kubectl get deploy -n aks-istio-ingress -o name | grep -m1 aks-istio-ingressgateway-external)" \
  CILIUM_VERSION="$(kubectl get ds -n kube-system cilium -o jsonpath='{.spec.template.spec.containers[0].image}' | sed 's/.*://')" \
  RUNTIME_UID="$(kubectl get runtimeclass kata-vm-isolation -o jsonpath='{.metadata.uid}')" \
  SC_UID="$(kubectl get sc "$SANDBOX_HOME_STORAGE_CLASS" -o jsonpath='{.metadata.uid}')"
export INGRESS_UID="$(kubectl get -n aks-istio-ingress "$INGRESS_DEPLOY" -o jsonpath='{.metadata.uid}')"

log "3. 写 cetirun-system/sandbox-platform"
python3 - > "$ROOT/generated/sandbox-platform.json" <<'PY'
import json, os, hashlib
e = os.environ
topology = {
    "managedPeersComplete": True,
    "dnsResolverIPs": [e["DNS_IP"]],
    "dns": [{"name": "coredns", "namespace": "kube-system", "uid": "coredns", "ips": [e["DNS_IP"]],
             "podSelector": {"k8s-app": "kube-dns"}}],
    "gateway": [{"peer": {"name": "cetirun-ai-gateway", "namespace": "cetirun-system", "uid": "ai-gateway",
                          "ips": [e["GW_SVC_IP"]],
                          "podSelector": {"app.kubernetes.io/name": "cetirun-ai-gateway",
                                          "app.kubernetes.io/instance": "cetirun-platform",
                                          "app.kubernetes.io/component": "gateway"}},
                 "ports": [{"port": 4000, "protocol": "TCP"}]}],
    "ingress": [{"peer": {"name": "aks-istio-ingressgateway-external", "namespace": "aks-istio-ingress",
                          "uid": e["INGRESS_UID"], "podSelector": {"istio": "aks-istio-ingressgateway-external"}},
                 "trustedProxyCIDRs": [e["POD_CIDR"]],
                 "ports": [{"port": int(p), "protocol": "TCP"} for p in e["SANDBOX_INGRESS_PORTS"].split()]}],
    "podCIDRs": [e["POD_CIDR"]], "serviceCIDRs": [e["SVC_CIDR"]], "nodeCIDRs": [e["NODE_CIDR"]],
    # API Server 和 Azure 平台地址（WireServer）
    "managementCIDRs": [f'{e["API_IP"]}/32', "168.63.129.16/32"],
    "metadataCIDRs": ["169.254.169.254/32"],
}
# 内容变了 revision 就变，operator 据此重新生成各 agent 的网络策略
topology = {"revision": "topology-" + hashlib.sha256(json.dumps(topology, sort_keys=True).encode()).hexdigest()[:12], **topology}
# Kata（AKS KataVmIsolation）这条运行时在本 PoC 实测得到的能力，见 docs/DEPLOYMENT.md
capabilities = {
    "profile": "native-single-start", "runtimeClassName": "kata-vm-isolation", "revision": "kata-1",
    "hostResourceLimits": True, "perContainerResourceLimits": False, "requirePerContainerResourceLimits": False,
    "readOnlyRootFilesystem": True, "homeAccessModes": ["ReadWriteOncePod"], "ipFamilies": ["IPv4"],
    "protocols": ["TCP", "UDP"], "guestExternalNetwork": True, "denyPrecedence": True, "nodeTraffic": True,
    "dnsIPRestrictions": True, "blockedPorts": False, "blockedFQDNs": False, "authenticatedIngress": True,
    "disabledIngress": False, "mutualTLS": False, "kubernetesAccess": False, "entraID": False,
    "enforcementObservation": False, "restrictiveFailure": False,
}
doc = {
    "version": "v1",
    "registrations": {"kata": {"Available": True, "Capabilities": capabilities}},
    "platforms": {"kata": {"adapter": {"name": "cilium", "revision": e["CILIUM_VERSION"], "qualified": False, "available": True},
                           "topology": topology}},
    "runtimeClasses": {"kata-vm-isolation": {"UID": e["RUNTIME_UID"], "Handler": "kata"}},
    "storageClasses": {e["SANDBOX_HOME_STORAGE_CLASS"]: {"UID": e["SC_UID"], "Provisioner": "disk.csi.azure.com"}},
}
print(json.dumps(doc, indent=2))
PY
before=$(kubectl get configmap sandbox-platform -n cetirun-system -o jsonpath='{.data.platform\.json}' 2>/dev/null | shasum | cut -c1-12 || true)
kubectl create configmap sandbox-platform -n cetirun-system --from-file=platform.json="$ROOT/generated/sandbox-platform.json" \
  --dry-run=client -o yaml | kubectl apply -f -
after=$(kubectl get configmap sandbox-platform -n cetirun-system -o jsonpath='{.data.platform\.json}' | shasum | cut -c1-12)
# operator 启动时读这份登记，内容变了要重启它，各 agent 的网络策略才会按新拓扑重新生成
if [ "$before" != "$after" ] && kubectl get deploy -n cetirun-system cetirun-operator -o name >/dev/null 2>&1; then
  log "4. 登记有变化，重启 operator"
  kubectl rollout restart -n cetirun-system deploy/cetirun-operator
  kubectl rollout status -n cetirun-system deploy/cetirun-operator --timeout=300s
fi
ok "入口端口：${SANDBOX_INGRESS_PORTS}；Cilium ${CILIUM_VERSION}"

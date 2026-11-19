#!/usr/bin/env bash
# 建 AKS：用 CetiRun 自带的 infra/cluster/main.bicep，PoC 规格，所有资源打标签。
# 按 infra/cluster/deploy.sh 的步骤，但不调用它（它的 az group create 会覆盖资源组标签）。
source "$(dirname "$0")/lib.sh"

log "注册 Gateway API 预览功能"
state=$(az feature show --namespace Microsoft.ContainerService --name ManagedGatewayAPIPreview --query properties.state -o tsv)
if [ "$state" != "Registered" ]; then
  az feature register --namespace Microsoft.ContainerService --name ManagedGatewayAPIPreview -o none
  until [ "$(az feature show --namespace Microsoft.ContainerService --name ManagedGatewayAPIPreview --query properties.state -o tsv)" = "Registered" ]; do sleep 30; done
fi
az provider register --namespace Microsoft.ContainerService -o none

tags_json=$(python3 -c 'import json,sys; print(json.dumps(dict(t.split("=",1) for t in sys.argv[1].split())))' "$TAGS")
cat > "$ROOT/generated/cluster.parameters.json" <<EOF
{
  "\$schema": "https://schema.management.azure.com/schemas/2019-04-01/deploymentParameters.json#",
  "contentVersion": "1.0.0.0",
  "parameters": {
    "resourceGroupName": {"value": "$RG"},
    "location": {"value": "$LOCATION"},
    "clusterName": {"value": "$CLUSTER_NAME"},
    "kubernetesVersion": {"value": "1.37"},
    "dnsPrefix": {"value": "$CLUSTER_NAME"},
    "nodeResourceGroup": {"value": "$NODE_RG"},
    "skuTier": {"value": "Standard"},
    "tags": {"value": $tags_json},
    "systemNodePoolVmSize": {"value": "$SYSTEM_VM"},
    "systemNodePoolMinCount": {"value": $SYSTEM_MIN},
    "systemNodePoolMaxCount": {"value": $SYSTEM_MAX},
    "agentNodePoolVmSize": {"value": "$AGENT_VM"},
    "agentNodePoolMinCount": {"value": $AGENT_MIN},
    "agentNodePoolMaxCount": {"value": $AGENT_MAX},
    "agent2NodePoolVmSize": {"value": "Standard_D8s_v5"},
    "agent2NodePoolMinCount": {"value": 0},
    "agent2NodePoolMaxCount": {"value": 1}
  }
}
EOF

if [ "$(az aks show -g "$RG" -n "$CLUSTER_NAME" --query provisioningState -o tsv 2>/dev/null)" = "Succeeded" ] && [ "${REDEPLOY:-false}" != "true" ]; then
  log "AKS 已存在，跳过部署（REDEPLOY=true 可强制重跑 Bicep）"
else
  log "部署 AKS（约 10 到 20 分钟）"
  name="cetirun-cluster-$(date +%Y%m%d%H%M%S)"
  az deployment group create -g "$RG" -n "$name" \
    --template-file "$CETIRUN_SRC/infra/cluster/main.bicep" \
    --parameters "@$ROOT/generated/cluster.parameters.json" \
    --query "properties.{state:provisioningState,outputs:outputs}" -o json
fi

log "给自己分配集群管理员（集群启用了 Azure RBAC）"
CLUSTER_ID=$(az aks show -g "$RG" -n "$CLUSTER_NAME" --query id -o tsv)
ME=$(az ad signed-in-user show --query id -o tsv)
az role assignment create --assignee-object-id "$ME" --assignee-principal-type User \
  --role "Azure Kubernetes Service RBAC Cluster Admin" --scope "$CLUSTER_ID" -o none 2>/dev/null || true

log "kubeconfig"
az aks get-credentials -g "$RG" -n "$CLUSTER_NAME" --overwrite-existing
kubelogin convert-kubeconfig -l azurecli
for i in $(seq 1 20); do
  kubectl auth can-i '*' '*' --all-namespaces >/dev/null 2>&1 && break
  echo "等待集群 RBAC 生效（${i}）"; sleep 20
done

log "关闭 Container Insights 的环境变量采集（同 deploy.sh）"
if [ -z "$(kubectl get configmap container-azm-ms-agentconfig -n kube-system --ignore-not-found -o name)" ]; then
  kubectl create -f "$CETIRUN_SRC/infra/cluster/container-azm-ms-agentconfig.yaml"
fi

log "启用 Gateway API"
az aks update -g "$RG" -n "$CLUSTER_NAME" --enable-gateway-api --yes -o none

log "打标签"
tag_rg "$RG"
tag_rg "$NODE_RG"

kubectl get nodes -o wide
kubectl get pods -n aks-istio-system
kubectl get pods -n aks-istio-ingress

#!/usr/bin/env bash
# Operator 的 Helm 客户端从私有 ACR 拉 agent chart 用的只读凭据。
# 节点拉镜像靠 az aks --attach-acr；Operator 的 Helm 用不上它，要 acrDockerSecret（CetiRun 文档 getting-started/platform.md）。
source "$(dirname "$0")/lib.sh"
# shellcheck disable=SC1091
source "$ROOT/state/registry.env"
export KUBECONFIG="$ROOT/state/kubeconfig"

log "只读 scope map 与 token（仅 cetirun/* 的 content/read 和 metadata/read）"
az acr scope-map show -r "$ACR" -n cetirun-pull -o none 2>/dev/null || \
  az acr scope-map create -r "$ACR" -n cetirun-pull --description "CetiRun operator chart pull" \
    --repository 'cetirun/helm/cetirun' content/read metadata/read -o none
az acr token show -r "$ACR" -n cetirun-operator-pull -o none 2>/dev/null || \
  az acr token create -r "$ACR" -n cetirun-operator-pull --scope-map cetirun-pull --no-passwords -o none

if ! kubectl get secret acr-helm-pull -n cetirun-system >/dev/null 2>&1; then
  pw=$(az acr token credential generate -r "$ACR" -n cetirun-operator-pull --password1 --query "passwords[0].value" -o tsv)
  kubectl create secret docker-registry acr-helm-pull -n cetirun-system \
    --docker-server="$ACR_LOGIN_SERVER" --docker-username=cetirun-operator-pull --docker-password="$pw"
fi
kubectl get secret acr-helm-pull -n cetirun-system

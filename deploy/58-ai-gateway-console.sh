#!/usr/bin/env bash
# 把 AI 网关（LiteLLM）的管理控制台挂到门户域名的 /ai-gateway/ui/ 下。
# 入口仍经过门户前面的 Entra 登录；进控制台后用 LiteLLM 自己的 admin 账号登录（密码见文末命令）。
source "$(dirname "$0")/lib.sh"
export PATH="$ROOT/bin:$PATH" KUBECONFIG="$ROOT/state/kubeconfig"
HOST="$DNS_LABEL.$LOCATION.cloudapp.azure.com"

log "1. 确认控制台已在 /ai-gateway 前缀下提供（由 50-environment.sh 写进 chart 的 auth.adminUI.rootPath）"
kubectl get deploy -n cetirun-system cetirun-ai-gateway -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="SERVER_ROOT_PATH")].value}' \
  | grep -qx /ai-gateway || { echo "AI 网关没有 SERVER_ROOT_PATH=/ai-gateway：先重跑 50-environment.sh 和 56-local-deploy.sh"; exit 1; }

log "2. 入口路由 https://$HOST/ai-gateway/"
kubectl apply -f - <<EOF
apiVersion: networking.istio.io/v1
kind: VirtualService
metadata:
  name: cetirun-ai-gateway-ui
  namespace: cetirun-system
  labels:
    app.kubernetes.io/name: cetirun-ai-gateway-ui
    app.kubernetes.io/managed-by: manual
spec:
  gateways: [aks-istio-ingress/cetirun-gateway]
  hosts: [$HOST]
  http:
  - match: [{uri: {exact: /ai-gateway}}, {uri: {exact: /ai-gateway/}}]
    redirect: {redirectCode: 302, uri: /ai-gateway/ui/}
  - match: [{uri: {prefix: /ai-gateway/}}]
    route: [{destination: {host: cetirun-ai-gateway.cetirun-system.svc.cluster.local, port: {number: 4000}}}]
    timeout: 300s
EOF

log "完成：https://$HOST/ai-gateway/ui/ ，用户名 admin，密码："
echo "  kubectl get secret -n cetirun-system cetirun-ai-gateway-auth -o jsonpath='{.data.ui-password}' | base64 -d"

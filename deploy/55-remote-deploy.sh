#!/usr/bin/env bash
# 在构建机（vm-cetirun-build）上执行 make deploy。本机只发 az / ssh 命令，不在本机安装或运行部署工具。
# 构建机用自己的系统托管身份登录 Azure，只授予这次部署需要的角色。
source "$(dirname "$0")/lib.sh"
# shellcheck disable=SC1091
source "$ROOT/state/registry.env"; source "$ROOT/state/build.env"; source "$ROOT/state/identity.env"
VM=vm-cetirun-build
KEY="$ROOT/state/ssh/id_ed25519"
REMOTE=/home/azureuser/cetirun
SRC_DIR=/home/azureuser/build/cetirun-main   # 46-build-publish.sh 已在这里 set-version 并提交

log "1. 启动构建机、托管身份与角色"
az vm start -g "$RG" -n "$VM" -o none
MSI=$(az vm identity assign -g "$RG" -n "$VM" --query systemAssignedIdentity -o tsv)
CLUSTER_ID=$(az aks show -g "$RG" -n "$CLUSTER_NAME" --query id -o tsv)
NODE_RG_ID=$(az group show -n "$NODE_RG" --query id -o tsv)
ACR_ID=$(az acr show -n "$ACR" --query id -o tsv)
grant() { az role assignment create --assignee-object-id "$MSI" --assignee-principal-type ServicePrincipal \
            --role "$1" --scope "$2" -o none 2>/dev/null || true; }
grant "Azure Kubernetes Service RBAC Cluster Admin" "$CLUSTER_ID"
grant "Azure Kubernetes Service Cluster User Role" "$CLUSTER_ID"
grant "Network Contributor" "$NODE_RG_ID"          # publicTLS 设置/核对公网 IP 的 DNS 标签
grant "Reader" "$(az group show -n "$RG" --query id -o tsv)"
grant "AcrPull" "$ACR_ID"                           # deploy 预检要查 ACR 里的镜像与 chart
echo "DEPLOYER_MSI=$MSI" > "$ROOT/state/deployer.env"
tag_rg "$RG"

VMIP=$(az vm show -d -g "$RG" -n "$VM" --query publicIps -o tsv)
MYIP=$(curl -s https://api.ipify.org)
az network nsg rule update -g "$RG" --nsg-name "${VM}NSG" -n ssh-from-me --source-address-prefixes "$MYIP/32" -o none
SSH=(ssh -i "$KEY" -o StrictHostKeyChecking=accept-new -o ServerAliveInterval=30 "azureuser@$VMIP")
for i in $(seq 1 30); do "${SSH[@]}" true 2>/dev/null && break; echo "等待 SSH（${i}）"; sleep 10; done

log "2. 构建机上的部署工具（az CLI、kubectl 1.37、kubelogin）"
"${SSH[@]}" 'set -e
command -v az >/dev/null || curl -fsSL https://aka.ms/InstallAzureCLIDeb | sudo bash >/dev/null
kubectl version --client 2>/dev/null | grep -q v1.37 && command -v kubelogin >/dev/null || \
  sudo az aks install-cli --client-version 1.37.1 --install-location /usr/local/bin/kubectl \
    --kubelogin-install-location /usr/local/bin/kubelogin >/dev/null
az login --identity -o none
mkdir -p '"$REMOTE"' && chmod 700 '"$REMOTE"'
az aks get-credentials -g '"$RG"' -n '"$CLUSTER_NAME"' --file '"$REMOTE"'/kubeconfig --overwrite-existing -o none
kubelogin convert-kubeconfig -l msi --kubeconfig '"$REMOTE"'/kubeconfig
KUBECONFIG='"$REMOTE"'/kubeconfig kubectl auth whoami -o jsonpath="{.status.userInfo.username}"; echo'

log "3. 环境文件：换成源码构建的版本，路径换成构建机上的路径"
PI_DIGEST=$(az acr manifest show-metadata -r "$ACR" -n "cetirun/cetirun-pi-agent:$BUILD_VERSION" --query digest -o tsv 2>/dev/null \
  || az acr repository show -n "$ACR" --image "cetirun/cetirun-pi-agent:$BUILD_VERSION" --query digest -o tsv)
export ROOT REMOTE SRC_DIR BUILD_VERSION BUILD_GIT_SHA PI_DIGEST ACR_LOGIN_SERVER MSI
python3 - <<'PY'
import os, yaml
e = os.environ
cfg = yaml.safe_load(open(f"{e['ROOT']}/generated/environment.yaml"))
cfg["target"]["kubeconfig"] = f"{e['REMOTE']}/kubeconfig"
cfg["release"].update({"version": e["BUILD_VERSION"], "gitSHA": e["BUILD_GIT_SHA"],
                       "excludeImages": ["cetirun-vscode"]})        # 未接受 VS Code Server 许可，未构建
op = cfg["platform"]["values"]["cetirun-operator"]
op["customRuntime"]["approvedImages"] = [f"{e['ACR_LOGIN_SERVER']}/cetirun/cetirun-pi-agent@{e['PI_DIGEST']}"]
# 准入管理员名单 adminUsers 不改：make deploy 拒绝在已有安装上修改它（需要单独的审批流程）
t = cfg["publicTLS"]
t["stateDirectory"] = f"{e['REMOTE']}/public-tls"
t["certManager"]["chartArchive"] = f"{e['REMOTE']}/cert-manager-v1.21.3.tgz"
yaml.safe_dump(cfg, open(f"{e['ROOT']}/generated/environment-remote.yaml", "w"), sort_keys=False)
PY
scp -i "$KEY" -q "$ROOT/generated/environment-remote.yaml" "azureuser@$VMIP:$REMOTE/environment.yaml"
# publicTLS 的回执要沿用（DNS 标签、证书都已完成），cert-manager 包在构建机上自己拉
scp -i "$KEY" -q -r "$ROOT/state/public-tls" "azureuser@$VMIP:$REMOTE/"
"${SSH[@]}" "[ -f $REMOTE/cert-manager-v1.21.3.tgz ] || helm pull oci://quay.io/jetstack/charts/cert-manager --version v1.21.3 --destination $REMOTE >/dev/null"

log "4. 在构建机上 make deploy（后台运行，日志 $REMOTE/deploy.log）"
"${SSH[@]}" "cd $SRC_DIR; \
  TOKEN=\$(az acr login -n $ACR --expose-token --query accessToken -o tsv); \
  echo \$TOKEN | helm registry login $ACR_LOGIN_SERVER -u 00000000-0000-0000-0000-000000000000 --password-stdin >/dev/null; \
  echo \$TOKEN | sg docker -c \"docker login $ACR_LOGIN_SERVER -u 00000000-0000-0000-0000-000000000000 --password-stdin\" >/dev/null; \
  nohup bash -c 'KUBECONFIG=$REMOTE/kubeconfig sg docker -c \"make deploy DEPLOY_CONFIG=$REMOTE/environment.yaml\"; echo EXIT=\$?' > $REMOTE/deploy.log 2>&1 &"
echo "部署已在构建机上启动：ssh -i $KEY azureuser@$VMIP tail -f $REMOTE/deploy.log"

#!/usr/bin/env bash
# 在本机以本人身份执行 make deploy（方案 B）。
# 为什么不在构建机上：fdpo 租户的条件访问要求用户登录来自受管设备；而 CetiAdmin 配置只允许
# 准入管理员名单里的人写（名单只有本人，make deploy 又禁止在已有安装上改名单）。
# 本机只用到：bin/kubectl（config.env 的 KUBECTL_VERSION，与集群版本差不超过一个小版本）、bin/docker（skopeo 替身，只做镜像存在性检查）。
source "$(dirname "$0")/lib.sh"
mkdir -p "$HOME/.cetirun-poc/public-tls" && chmod 700 "$HOME/.cetirun-poc"   # TLS 状态目录，make deploy 要求在仓库外
export PATH="$ROOT/bin:$PATH"
# kubectl 不进 git：首次运行时下载到 bin/（只放 PoC 目录，不装进系统）
[ -x "$ROOT/bin/kubectl" ] || az aks install-cli --client-version "$KUBECTL_VERSION" \
  --install-location "$ROOT/bin/kubectl" --kubelogin-install-location "$ROOT/bin/kubelogin"
# shellcheck disable=SC1091
source "$ROOT/state/registry.env"; source "$ROOT/state/build.env"
SRC="$CETIRUN_SRC"
export KUBECONFIG="$ROOT/state/kubeconfig"
unset REGISTRY VERSION

log "1. kubeconfig（本人 Azure CLI 登录）"
az aks get-credentials -g "$RG" -n "$CLUSTER_NAME" --file "$KUBECONFIG" --overwrite-existing -o none
kubelogin convert-kubeconfig -l azurecli --kubeconfig "$KUBECONFIG"
kubectl auth whoami -o jsonpath='{.status.userInfo.username}'; echo
TOKEN=$(az acr login -n "$ACR" --expose-token --query accessToken -o tsv)
echo "$TOKEN" | helm registry login "$ACR_LOGIN_SERVER" -u 00000000-0000-0000-0000-000000000000 --password-stdin >/dev/null

log "2. 环境文件"
PI_DIGEST=$("$ROOT/bin/docker" buildx imagetools inspect "$ACR_LOGIN_SERVER/cetirun/cetirun-pi-agent:$BUILD_VERSION" | awk '/^Digest:/{print $2}')
export ROOT BUILD_VERSION BUILD_GIT_SHA PI_DIGEST ACR_LOGIN_SERVER CERT_MANAGER_VERSION OWNER_OBJECT_ID="$(az ad signed-in-user show --query id -o tsv)"
python3 - <<'PY'
import os, yaml
e = os.environ
cfg = yaml.safe_load(open(f"{e['ROOT']}/generated/environment.yaml"))
cfg["target"]["kubeconfig"] = f"{e['ROOT']}/state/kubeconfig"
cfg["release"].update({"version": e["BUILD_VERSION"], "gitSHA": e["BUILD_GIT_SHA"],
                       "excludeImages": ["cetirun-vscode"]})        # 未接受 VS Code Server 许可，未构建
op = cfg["platform"]["values"]["cetirun-operator"]
op["customRuntime"]["approvedImages"] = [f"{e['ACR_LOGIN_SERVER']}/cetirun/cetirun-pi-agent@{e['PI_DIGEST']}"]
op["sandboxAdmission"]["adminUsers"] = [e["OWNER_OBJECT_ID"]]       # 与首次安装一致，不改
op["sandboxAdmission"]["podsEnabled"] = True                        # 开启 Sandbox 执行（policy-teams.md「Enable Sandbox execution」）
op.setdefault("sandboxSources", {}).update({"enabled": True, "nativeExecution": True,
    "bootstrapOwnedPrerequisites": True, "platformConfigMapName": "sandbox-platform"})
cfg["platform"]["values"].setdefault("cetirun-portal", {})["sandboxRuntimeDefaultsFromController"] = True
t = cfg["publicTLS"]
t["stateDirectory"] = os.path.expanduser("~/.cetirun-poc/public-tls")   # make deploy 要求在源码仓库之外
t["certManager"]["chartArchive"] = f"{e['ROOT']}/state/cert-manager-{e['CERT_MANAGER_VERSION']}.tgz"
yaml.safe_dump(cfg, open(f"{e['ROOT']}/generated/environment-local.yaml", "w"), sort_keys=False)
print("approvedImages:", op["customRuntime"]["approvedImages"][0])
PY

log "3. 沙箱平台登记（第一次安装时 AI 网关还没有，会在部署后再跑一次）"
bash "$ROOT/deploy/55-sandbox-platform.sh"

log "4. make deploy"
make -C "$SRC" deploy DEPLOY_CONFIG="$ROOT/generated/environment-local.yaml"
bash "$ROOT/deploy/55-sandbox-platform.sh"
tag_rg "$RG"; tag_rg "$NODE_RG"

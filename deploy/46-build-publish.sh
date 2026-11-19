#!/usr/bin/env bash
# 用源码构建并发布一整套 CetiRun 镜像和 chart（等价于文档里的 make publish）。
# 为什么需要：下载的源码是 2026-11-11 的（版本号仍写 0.1.1），比 MCR 的 0.1.1（10-09）新、比 0.1.2（11-13）旧，
# MCR 上任何一个版本的镜像都和这份源码的 chart 对不上（例如 Operator 不认识 -memory-context-enabled）。
# 构建在资源组里的一台临时 Linux VM 上：OpenClaw 的安装器需要 openat2，ACR Tasks 不支持；本机没有 Docker。
source "$(dirname "$0")/lib.sh"
# shellcheck disable=SC1091
source "$ROOT/state/registry.env"
BUILD_VERSION=0.1.1-src.20261111
VM=vm-cetirun-build
KEY="$ROOT/state/ssh/id_ed25519"

log "1. 源码包与 GIT_SHA"
# macOS tar 默认把扩展属性打成 ._* 文件（AppleDouble），会进 git 并让发布脚本按 UTF-8 读时报错；
# 另外排除 .DS_Store、PPT 及 Office 锁文件，以及部署运行时状态（state/ 里有私钥和 kubeconfig，不能上构建机）
B=$(basename "$CETIRUN_SRC")
COPYFILE_DISABLE=1 tar --no-xattrs --exclude='.DS_Store' --exclude='._*' --exclude='*.pptx' --exclude='~$*' \
  --exclude="$B/.git" --exclude="$B/state" --exclude="$B/generated" --exclude="$B/bin" --exclude="$B/config.env" \
  --exclude='node_modules' -C "$(dirname "$CETIRUN_SRC")" -czf "$ROOT/state/cetirun-src.tgz" "$B"

log "2. 构建 VM（SSH 只放行本机公网 IP）"
mkdir -p "$ROOT/state/ssh"; [ -f "$KEY" ] || ssh-keygen -t ed25519 -N '' -f "$KEY" -q
MYIP=$(curl -s https://api.ipify.org)
if ! az vm show -g "$RG" -n "$VM" -o none 2>/dev/null; then
  # shellcheck disable=SC2086
  az vm create -g "$RG" -n "$VM" -l "$LOCATION" --image Ubuntu2404 --size Standard_D8s_v5 \
    --admin-username azureuser --ssh-key-values "$KEY.pub" --os-disk-size-gb 256 \
    --public-ip-sku Standard --nsg-rule NONE --tags $TAGS -o none
  az network nsg rule create -g "$RG" --nsg-name "${VM}NSG" -n ssh-from-me --priority 100 \
    --source-address-prefixes "$MYIP/32" --destination-port-ranges 22 --access Allow --protocol Tcp -o none
else
  az vm start -g "$RG" -n "$VM" -o none
  # 订阅的安全自动化会删掉这条规则，所以每次都重建（create 对已有规则是覆盖）
  az network nsg rule create -g "$RG" --nsg-name "${VM}NSG" -n ssh-from-me --priority 100 \
    --source-address-prefixes "$MYIP/32" --destination-port-ranges 22 --access Allow --protocol Tcp -o none
fi
tag_rg "$RG"
VMIP=$(az vm show -d -g "$RG" -n "$VM" --query publicIps -o tsv)
SSH=(ssh -i "$KEY" -o StrictHostKeyChecking=accept-new -o ServerAliveInterval=30 "azureuser@$VMIP")
for i in $(seq 1 30); do "${SSH[@]}" true 2>/dev/null && break; echo "等待 SSH（${i}）"; sleep 10; done

log "3. 构建工具"
"${SSH[@]}" 'set -e; command -v docker >/dev/null || { sudo apt-get update -qq; sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq docker.io docker-buildx make jq python3-yaml >/dev/null; sudo usermod -aG docker azureuser; }
command -v helm >/dev/null || curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash >/dev/null'

log "4. 传源码、改版本号"
scp -i "$KEY" -q "$ROOT/state/cetirun-src.tgz" "azureuser@$VMIP:/tmp/src.tgz"
# 源码包没有 .git，而发布脚本用 git ls-files 选文件：在构建副本里 git init，改版本号后提交，
# GIT_SHA 就是这次提交的 commit（只存在于构建机上的副本，不是上游仓库的 commit）
"${SSH[@]}" "set -e; rm -rf ~/build && mkdir ~/build && tar -xzf /tmp/src.tgz -C ~/build && cd ~/build/$(basename "$CETIRUN_SRC")
git init -q && git -c user.name=poc -c user.email=poc@local add -A && git -c user.name=poc -c user.email=poc@local commit -qm 'source 2026-11-11'
make set-version VERSION=$BUILD_VERSION >/dev/null && git -c user.name=poc -c user.email=poc@local commit -qam 'set-version $BUILD_VERSION'
grep -m1 '^version' app/cetirun-chart/Chart.yaml"
GIT_SHA=$("${SSH[@]}" "cd ~/build/$(basename "$CETIRUN_SRC") && git rev-parse HEAD")
echo "BUILD_VERSION=$BUILD_VERSION" > "$ROOT/state/build.env"; echo "BUILD_GIT_SHA=$GIT_SHA" >> "$ROOT/state/build.env"

log "5. 构建并推送（约 30 到 60 分钟）"
TOKEN=$(az acr login -n "$ACR" --expose-token --query accessToken -o tsv)
# 全程以 root 执行：docker / helm 的登录凭据和发布命令用同一个 HOME
"${SSH[@]}" "sudo -H bash -c 'set -e
echo $TOKEN | docker login $ACR_LOGIN_SERVER -u 00000000-0000-0000-0000-000000000000 --password-stdin >/dev/null
echo $TOKEN | helm registry login $ACR_LOGIN_SERVER -u 00000000-0000-0000-0000-000000000000 --password-stdin >/dev/null
cd /home/azureuser/build/$(basename "$CETIRUN_SRC")
PLATFORMS=linux/amd64 python3 -B scripts/cetirun-release.py publish \
  --registry $ACR_LOGIN_SERVER/cetirun --version $BUILD_VERSION --git-sha $GIT_SHA --exclude-image cetirun-vscode'"

log "6. 停机（解除分配，不再计算费用）"
az vm deallocate -g "$RG" -n "$VM" -o none
log "完成：$ACR_LOGIN_SERVER/cetirun 版本 $BUILD_VERSION"

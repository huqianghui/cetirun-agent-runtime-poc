#!/usr/bin/env bash
# 用源码构建并发布一整套 CetiRun 镜像和 chart（等价于文档里的 make publish）。
# 为什么需要：下载的源码是 2026-11-11 的（版本号仍写 0.1.1），比 MCR 的 0.1.1（10-09）新、比 0.1.2（11-13）旧，
# MCR 上任何一个版本的镜像都和这份源码的 chart 对不上（例如 Operator 不认识 -memory-context-enabled）。
# 构建在资源组里的一台临时 Linux VM 上：OpenClaw 的安装器需要 openat2，ACR Tasks 不支持；本机没有 Docker。
source "$(dirname "$0")/lib.sh"
# shellcheck disable=SC1091
source "$ROOT/state/registry.env"
VM=$BUILD_VM   # BUILD_VERSION、BUILD_VM* 都在 config.env
KEY="$ROOT/state/ssh/id_ed25519"

log "1. 源码包与 GIT_SHA"
# macOS tar 默认把扩展属性打成 ._* 文件（AppleDouble），会进 git 并让发布脚本按 UTF-8 读时报错；
# 另外排除 .DS_Store、PPT 及 Office 锁文件，以及部署运行时状态（state/ 里有私钥和 kubeconfig，不能上构建机）
B=$(basename "$CETIRUN_SRC")
COPYFILE_DISABLE=1 tar --no-xattrs --exclude='.DS_Store' --exclude='._*' --exclude='*.pptx' --exclude='~$*' \
  --exclude="$B/.git" --exclude="$B/state" --exclude="$B/generated" --exclude="$B/bin" --exclude="$B/config.env" \
  --exclude='node_modules' -C "$(dirname "$CETIRUN_SRC")" -czf "$ROOT/state/cetirun-src.tgz" "$B"

log "2. 构建 VM（不开任何入站端口，命令都经 az vm run-command 下发）"
# 公司网络出站拦 22 端口，订阅的安全自动化也会删 SSH 入站规则；run-command 走 ARM，
# 用的是本机 az login 的 Entra 身份，构建机不需要公网入站
mkdir -p "$ROOT/state/ssh"; [ -f "$KEY" ] || ssh-keygen -t ed25519 -N '' -f "$KEY" -q
if ! az vm show -g "$RG" -n "$VM" -o none 2>/dev/null; then
  # shellcheck disable=SC2086
  az vm create -g "$RG" -n "$VM" -l "$LOCATION" --image "$BUILD_VM_IMAGE" --size "$BUILD_VM_SIZE" \
    --admin-username azureuser --ssh-key-values "$KEY.pub" --os-disk-size-gb 256 \
    --public-ip-sku Standard --nsg-rule NONE --tags $TAGS -o none
else
  az vm start -g "$RG" -n "$VM" -o none
fi
tag_rg "$RG"
vmrun() {   # 在构建机上以 root 执行一段脚本，打印输出；脚本失败时返回非 0
  local out; out=$(az vm run-command invoke -g "$RG" -n "$VM" --command-id RunShellScript \
    --scripts "$1" --query "value[0].message" -o tsv)
  printf '%s\n' "$out" | sed -n '/^\[stdout\]/,/^\[stderr\]/p' | sed '1d;$d'
  printf '%s\n' "$out" | grep -q '__RC=0__'
}

log "3. 构建工具"
vmrun 'set -e; command -v docker >/dev/null || { apt-get update -qq; DEBIAN_FRONTEND=noninteractive apt-get install -y -qq docker.io docker-buildx make jq python3-yaml git >/dev/null; }
command -v helm >/dev/null || curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash >/dev/null
echo __RC=0__' || die "构建工具安装失败"

log "4. 传源码（封进 ACR 里的一个只含源码包的镜像）、改版本号"
SRC_CTX=$(mktemp -d); cp "$ROOT/state/cetirun-src.tgz" "$SRC_CTX/"
printf 'FROM scratch\nCOPY cetirun-src.tgz /cetirun-src.tgz\n' > "$SRC_CTX/Dockerfile"
az acr build -r "$ACR" -t "cetirun/build-src:$BUILD_VERSION" "$SRC_CTX" -o none --no-logs
rm -rf "$SRC_CTX"
TOKEN=$(az acr login -n "$ACR" --expose-token --query accessToken -o tsv)
B=$(basename "$CETIRUN_SRC")
# 源码包没有 .git，而发布脚本用 git ls-files 选文件：在构建副本里 git init，改版本号后提交，
# GIT_SHA 就是这次提交的 commit（只存在于构建机上的副本，不是上游仓库的 commit）
vmrun "set -e; export HOME=/root
echo $TOKEN | docker login $ACR_LOGIN_SERVER -u 00000000-0000-0000-0000-000000000000 --password-stdin >/dev/null
echo $TOKEN | helm registry login $ACR_LOGIN_SERVER -u 00000000-0000-0000-0000-000000000000 --password-stdin >/dev/null
docker rm -f build-src >/dev/null 2>&1 || true
docker pull -q $ACR_LOGIN_SERVER/cetirun/build-src:$BUILD_VERSION >/dev/null
docker create --name build-src $ACR_LOGIN_SERVER/cetirun/build-src:$BUILD_VERSION /x >/dev/null
docker cp build-src:/cetirun-src.tgz /tmp/src.tgz && docker rm build-src >/dev/null
rm -rf /root/build && mkdir /root/build && tar -xzf /tmp/src.tgz -C /root/build && cd /root/build/$B
git init -q && git -c user.name=poc -c user.email=poc@local add -A && git -c user.name=poc -c user.email=poc@local commit -qm source
make set-version VERSION=$BUILD_VERSION >/dev/null && git -c user.name=poc -c user.email=poc@local commit -qam 'set-version $BUILD_VERSION'
echo GIT_SHA=\$(git rev-parse HEAD); echo __RC=0__" > "$ROOT/state/46-src.out" || { cat "$ROOT/state/46-src.out"; die "传源码失败"; }
GIT_SHA=$(sed -n 's/^GIT_SHA=//p' "$ROOT/state/46-src.out")
[ -n "$GIT_SHA" ] || die "没拿到 GIT_SHA"
echo "BUILD_VERSION=$BUILD_VERSION" > "$ROOT/state/build.env"; echo "BUILD_GIT_SHA=$GIT_SHA" >> "$ROOT/state/build.env"

log "5. 构建并推送（约 30 到 60 分钟，后台执行，每 2 分钟看一次进度）"
# run-command 单次调用有超时，构建放到后台跑，结束时写退出码
vmrun "set -e; export HOME=/root; cd /root/build/$B; rm -f /root/build.rc
nohup bash -c 'PLATFORMS=linux/amd64 python3 -B scripts/cetirun-release.py publish --registry $ACR_LOGIN_SERVER/cetirun --version $BUILD_VERSION --git-sha $GIT_SHA --exclude-image cetirun-vscode > /root/build.log 2>&1; echo \$? > /root/build.rc' >/dev/null 2>&1 &
echo started; echo __RC=0__" || die "构建启动失败"
while :; do
  sleep 120
  st=$(vmrun 'if [ -f /root/build.rc ]; then echo "RC=$(cat /root/build.rc)"; fi; tail -n 2 /root/build.log | cut -c1-160; echo __RC=0__')
  echo "  $(date +%H:%M) $(printf '%s' "$st" | grep -v '^RC=' | tail -n 1)"
  rc=$(printf '%s\n' "$st" | sed -n 's/^RC=//p')
  [ -z "$rc" ] && continue
  [ "$rc" = 0 ] && break
  vmrun 'tail -n 40 /root/build.log; echo __RC=0__'; die "构建失败（退出码 ${rc}），日志在构建机 /root/build.log"
done

log "6. 停机（解除分配，不再计算费用）"
az vm deallocate -g "$RG" -n "$VM" -o none
log "完成：$ACR_LOGIN_SERVER/cetirun 版本 $BUILD_VERSION"

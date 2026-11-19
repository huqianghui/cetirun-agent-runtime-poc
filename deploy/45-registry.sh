#!/usr/bin/env bash
# 自建一份完整、版本一致的 CetiRun 0.1.1 发布仓库（等价于 make publish，但不需要本机 Docker）：
#   1) MCR 上已有的 0.1.1 镜像：skopeo copy --all 原样复制（保留多架构 index 和 digest）
#   2) MCR 缺的镜像：用源码 0.1.1 经 ACR Tasks 在云端构建
#   3) 9 个 chart：从源码打包后 helm push
# MCR 上 0.1.1 缺 cetirun-connections-mcp、cetirun-memory-context；cetirun-observability 在 MCR 上不存在。
source "$(dirname "$0")/lib.sh"
SRC_REG=mcr.microsoft.com/aks/ai-runtime/cetirun
GIT_SHA=0af84e718108aa0700620a798d84b561c706b0b0
ACR=acrcetirun$(echo "$SUBSCRIPTION_ID$RG" | shasum | cut -c1-8)

log "1. ACR $ACR"
if ! az acr show -n "$ACR" -o none 2>/dev/null; then
  # shellcheck disable=SC2086
  az acr create -g "$RG" -n "$ACR" -l "$LOCATION" --sku Standard --tags $TAGS -o none
fi
LOGIN=$(az acr show -n "$ACR" --query loginServer -o tsv)
az aks update -g "$RG" -n "$CLUSTER_NAME" --attach-acr "$ACR" -o none
echo "ACR=$ACR" > "$ROOT/state/registry.env"; echo "ACR_LOGIN_SERVER=$LOGIN" >> "$ROOT/state/registry.env"
# 当前做法只需要 ACR 本身，镜像和 chart 由 46 用源码构建；ACR_ONLY=1 时到此为止
[ "${ACR_ONLY:-0}" = 1 ] && { log "完成：ACR $LOGIN（ACR_ONLY，跳过从 MCR 复制）"; exit 0; }
DEST="$LOGIN/cetirun"
TOKEN=$(az acr login -n "$ACR" --expose-token --query accessToken -o tsv)
CREDS="00000000-0000-0000-0000-000000000000:$TOKEN"

log "2. 镜像"
images=$(python3 -c 'import json,sys; print(" ".join(e["name"] for e in json.load(open(sys.argv[1]))["images"]))' \
  "$CETIRUN_SRC/scripts/release-artifacts.json")
build_context() {  # 与 scripts/*-docker-build.sh 一致
  case "$1" in
    cetirun-connections-mcp) echo "app/cetirun-connections-mcp/Dockerfile ." ;;
    cetirun-memory-context)  echo "app/cetirun-memory-context/Dockerfile ." ;;
    cetirun-observability)   echo "Dockerfile app/cetirun-observability" ;;
    *) return 1 ;;
  esac
}
for name in $images; do
  if skopeo inspect --raw --creds "$CREDS" "docker://$DEST/$name:$VERSION" >/dev/null 2>&1; then
    echo "已存在 $name:$VERSION"; continue
  fi
  if skopeo inspect --raw "docker://$SRC_REG/$name:$VERSION" >/dev/null 2>&1; then
    echo "复制 $name:$VERSION"
    skopeo copy --all --retry-times 3 --dest-creds "$CREDS" \
      "docker://$SRC_REG/$name:$VERSION" "docker://$DEST/$name:$VERSION"
  elif ctx=$(build_context "$name"); then
    read -r dockerfile context <<< "$ctx"
    echo "构建 $name:${VERSION}（ACR Tasks，源码 ${VERSION}）"
    # ACR Tasks 的依赖扫描不认识 `FROM --platform=$BUILDPLATFORM`，在临时副本里去掉它，
    # 并显式给出 TARGETOS/TARGETARCH。构建机就是 linux/amd64，结果与原交叉编译写法一致；原始源码不改。
    # dockerfile 是相对构建上下文的路径；进入上下文目录后用 "." 构建
    work=$(mktemp -d); cp -R "$CETIRUN_SRC/." "$work/"
    python3 -c 'import sys,re; p=sys.argv[1]; s=open(p).read(); open(p,"w").write(re.sub(r"(?m)^FROM --platform=\$BUILDPLATFORM ", "FROM ", s))' \
      "$work/$context/$dockerfile"
    ( cd "$work/$context" && az acr build -r "$ACR" --platform linux/amd64 \
        -t "cetirun/$name:$VERSION" -f "$dockerfile" \
        --build-arg "VERSION=$VERSION" --build-arg "GIT_SHA=$GIT_SHA" \
        --build-arg TARGETOS=linux --build-arg TARGETARCH=amd64 . )
    rm -rf "$work"
  else
    echo "ERROR: $name:$VERSION 既不在 MCR，也没有构建方式" >&2; exit 1
  fi
done

log "3. chart"
echo "$TOKEN" | helm registry login "$LOGIN" -u 00000000-0000-0000-0000-000000000000 --password-stdin
tmp=$(mktemp -d)
cp -R "$CETIRUN_SRC" "$tmp/src"   # 在副本里打包：umbrella chart 用相对路径引用子 chart，也不改动原始源码
python3 -c 'import json,sys; [print(e["name"], e["path"]) for e in json.load(open(sys.argv[1]))["charts"]]' \
  "$CETIRUN_SRC/scripts/release-artifacts.json" | while read -r name path; do
  if helm show chart "oci://$DEST/helm/$name" --version "$VERSION" >/dev/null 2>&1; then echo "已存在 chart $name"; continue; fi
  helm dependency build "$tmp/src/$path" >/dev/null
  helm package "$tmp/src/$path" -d "$tmp" >/dev/null
  helm push "$tmp/$name-$VERSION.tgz" "oci://$DEST/helm"
done
rm -rf "$tmp"

tag_rg "$RG"
log "完成：registry = $DEST"

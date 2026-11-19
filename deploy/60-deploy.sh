#!/usr/bin/env bash
# 正式部署：make deploy（cert-manager + 公网 DNS + Let's Encrypt 证书 + Istio 鉴权 + Broker + 平台）
source "$(dirname "$0")/lib.sh"
export KUBECONFIG="$ROOT/state/kubeconfig"
# registry / version 只以环境文件为准（config.env 里的 REGISTRY 是 MCR 源仓库）
unset REGISTRY VERSION
make -C "$CETIRUN_SRC" deploy DEPLOY_CONFIG="$ROOT/generated/environment.yaml"
tag_rg "$RG"; tag_rg "$NODE_RG"

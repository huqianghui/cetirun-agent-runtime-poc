# 公共函数：读配置、打标签。各脚本 source 本文件。
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck disable=SC1091
source "$ROOT/config.env"
az account set --subscription "$SUBSCRIPTION_ID"
# python.org 的 Python 不带系统根证书，部署脚本的 HTTPS（DoH 查询等）会校验失败；指向 certifi 的证书库
SSL_CERT_FILE="$(python3 -c 'import certifi; print(certifi.where())')"; export SSL_CERT_FILE

log() { printf '\n==> %s\n' "$*"; }
die() { printf '\n!! %s\n' "$*" >&2; exit 1; }
ok() { printf '  ✓ %s\n' "$*"; }
warn() { printf '  ! %s\n' "$*" >&2; }

# 合并标签到一个资源 ID（不覆盖已有标签）
tag_id() {
  # shellcheck disable=SC2086
  az tag update --resource-id "$1" --operation Merge --tags $TAGS -o none
}

# 给资源组本身和其中所有资源合并标签
tag_rg() {
  local rg="$1"
  az group show -n "$rg" -o none 2>/dev/null || return 0
  tag_id "$(az group show -n "$rg" --query id -o tsv)"
  az resource list -g "$rg" --query "[].id" -o tsv | while read -r id; do
    [ -n "$id" ] && tag_id "$id" || true
  done
}

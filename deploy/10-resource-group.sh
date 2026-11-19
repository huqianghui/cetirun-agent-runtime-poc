#!/usr/bin/env bash
# 建资源组并打标签；可重复执行
source "$(dirname "$0")/lib.sh"
log "资源组 $RG"
if ! az group show -n "$RG" -o none 2>/dev/null; then
  # shellcheck disable=SC2086
  az group create -n "$RG" -l "$LOCATION" --tags $TAGS -o none
fi
tag_rg "$RG"
az group show -n "$RG" --query "{name:name,location:location,tags:tags}" -o json

#!/usr/bin/env bash
# 模型：AIServices 账号（开启本地认证，AI 网关用 key 访问）+ 两个 GlobalStandard 部署。
source "$(dirname "$0")/lib.sh"

log "1. AIServices 账号 $AI_ACCOUNT"
az cognitiveservices account show -g "$RG" -n "$AI_ACCOUNT" -o none 2>/dev/null || \
  az cognitiveservices account create -g "$RG" -n "$AI_ACCOUNT" -l "$LOCATION" \
    --kind AIServices --sku S0 --custom-domain "$AI_ACCOUNT" --yes -o none
tag_id "$(az cognitiveservices account show -g "$RG" -n "$AI_ACCOUNT" --query id -o tsv)"

log "2. 模型部署"
for m in gpt-5.6-luna gpt-5.6-terra; do
  az cognitiveservices account deployment show -g "$RG" -n "$AI_ACCOUNT" --deployment-name "$m" -o none 2>/dev/null && continue
  az cognitiveservices account deployment create -g "$RG" -n "$AI_ACCOUNT" --deployment-name "$m" \
    --model-name "$m" --model-version 2026-07-09 --model-format OpenAI \
    --sku-name GlobalStandard --sku-capacity 100 -o none
done
az cognitiveservices account deployment list -g "$RG" -n "$AI_ACCOUNT" \
  --query "[].{name:name,sku:sku.name,capacity:sku.capacity}" -o table

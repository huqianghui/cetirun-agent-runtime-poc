#!/usr/bin/env bash
# 平台前置：共享 skills 存储、命名空间、TLS 证书、AI 网关 Secret、cert-manager 包。
# 依据 CetiRun 文档 operations/storage.md、agent-identity.md 第 3 步、policy-teams.md、public-ingress.md。
source "$(dirname "$0")/lib.sh"
# shellcheck disable=SC1091
source "$ROOT/state/identity.env"
OUT="$ROOT/state/platform.env"; touch "$OUT"
save() { grep -v "^$1=" "$OUT" > "$OUT.tmp" || true; echo "$1=$2" >> "$OUT.tmp"; mv "$OUT.tmp" "$OUT"; }
REV=$(kubectl get deploy -n aks-istio-system -o jsonpath='{.items[0].metadata.labels.istio\.io/rev}')
save ISTIO_REVISION "$REV"

log "1. 共享 skills 的 Blob NFS 存储"
SA=stcetirun$(echo "$SUBSCRIPTION_ID$RG" | shasum | cut -c1-8)
VNET=$(az network vnet list -g "$NODE_RG" --query "[0].name" -o tsv)
SUBNET_ID=$(az network vnet subnet show -g "$NODE_RG" --vnet-name "$VNET" -n aks-subnet --query id -o tsv)
az network vnet subnet update -g "$NODE_RG" --vnet-name "$VNET" -n aks-subnet --service-endpoints Microsoft.Storage -o none
tags_json=$(python3 -c 'import json,sys; print(json.dumps(dict(t.split("=",1) for t in sys.argv[1].split())))' "$TAGS")
az deployment group create -g "$RG" -n "cetirun-storage-$(date +%Y%m%d%H%M%S)" \
  --template-file "$CETIRUN_SRC/infra/storage/main.bicep" \
  --parameters location="$LOCATION" storageAccountName="$SA" containerName=published-skills \
               tags="$tags_json" allowedSubnetIds="[\"$SUBNET_ID\"]" -o none
save SKILLS_STORAGE_ACCOUNT "$SA"

log "2. 命名空间（Istio revision ${REV}）"
for ns in cetirun-credentials cetirun-system; do
  kubectl get ns "$ns" >/dev/null 2>&1 || kubectl create ns "$ns"
  kubectl label ns "$ns" "istio.io/rev=$REV" --overwrite
done

log "3. 证书"
TLS="$ROOT/state/tls"
if [ ! -f "$TLS/ca.crt" ]; then
  umask 077; mkdir -p "$TLS"
  openssl req -x509 -newkey rsa:3072 -nodes -sha256 -days 3650 -subj '/CN=CetiRun PoC identity CA' \
    -addext 'basicConstraints=critical,CA:TRUE' -addext 'keyUsage=critical,keyCertSign,cRLSign' \
    -keyout "$TLS/ca.key" -out "$TLS/ca.crt" 2>/dev/null
  for leaf in broker-server mcp-server mcp-client admission; do
    case "$leaf" in
      broker-server) u=serverAuth; san=DNS:cetirun-agent-broker.cetirun-credentials.svc.cluster.local ;;
      mcp-server)    u=serverAuth; san=DNS:cetirun-connections-mcp.cetirun-system.svc.cluster.local ;;
      mcp-client)    u=clientAuth; san=URI:spiffe://cluster.local/ns/cetirun-system/sa/cetirun-connections-mcp ;;
      admission)     u=serverAuth; san=DNS:cetirun-sandbox-admission.cetirun-system.svc ;;
    esac
    openssl req -new -newkey rsa:3072 -nodes -subj "/CN=$leaf" -keyout "$TLS/$leaf.key" -out "$TLS/$leaf.csr" 2>/dev/null
    printf 'basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature,keyEncipherment\nextendedKeyUsage=%s\nsubjectAltName=%s\n' \
      "$u" "$san" > "$TLS/$leaf.ext"
    # PoC 证书给足一年；到期前要轮换，否则准入 webhook 会拒绝所有受管写入
    openssl x509 -req -sha256 -days 365 -in "$TLS/$leaf.csr" -CA "$TLS/ca.crt" -CAkey "$TLS/ca.key" \
      -CAcreateserial -extfile "$TLS/$leaf.ext" -out "$TLS/$leaf.crt" 2>/dev/null
  done
fi
mk() { kubectl get secret "$2" -n "$1" >/dev/null 2>&1 || kubectl create secret "${@:3}" "$2" -n "$1"; }
mk cetirun-credentials broker-tls generic --from-file=tls.crt="$TLS/broker-server.crt" \
  --from-file=tls.key="$TLS/broker-server.key" --from-file=ca.crt="$TLS/ca.crt"
mk cetirun-system connections-mcp-tls tls --cert="$TLS/mcp-server.crt" --key="$TLS/mcp-server.key"
mk cetirun-system broker-client-tls generic --from-file=tls.crt="$TLS/mcp-client.crt" \
  --from-file=tls.key="$TLS/mcp-client.key" --from-file=ca.crt="$TLS/ca.crt"
mk cetirun-system connections-mcp-ca generic --from-file=ca.crt="$TLS/ca.crt"
mk cetirun-system cetirun-sandbox-admission-tls tls --cert="$TLS/admission.crt" --key="$TLS/admission.key"
save ADMISSION_CA_BUNDLE "$(base64 < "$TLS/ca.crt" | tr -d '\n')"

log "4. AI 网关 Secret"
rnd() { python3 -c 'import secrets; print(secrets.token_urlsafe(32))'; }
AOAI_KEY=$(az cognitiveservices account keys list -g "$RG" -n "$AI_ACCOUNT" --query key1 -o tsv)
mk cetirun-system gateway-provider-keys generic --from-literal=AZURE_OPENAI_KEY="$AOAI_KEY"
mk cetirun-system cetirun-ai-gateway-auth generic --from-literal=master-key="sk-$(rnd)" \
  --from-literal=salt-key="$(rnd)" --from-literal=ui-password="$(rnd)"
mk cetirun-system gateway-valkey-auth generic --from-literal=redis-password="$(rnd)"

log "5. cert-manager chart 包"
CM="$ROOT/state/cert-manager-$CERT_MANAGER_VERSION.tgz"
[ -f "$CM" ] || helm pull oci://quay.io/jetstack/charts/cert-manager --version "$CERT_MANAGER_VERSION" --destination "$ROOT/state"
save CERT_MANAGER_ARCHIVE "$CM"

tag_rg "$RG"; tag_rg "$NODE_RG"
log "完成"; grep -v CA_BUNDLE "$OUT"

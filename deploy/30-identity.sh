#!/usr/bin/env bash
# Entra 与加密基础：登录组、门户登录应用、Blueprint、Authorization 应用、加密身份、Key Vault 密钥。
# 依据 CetiRun 文档 operations/istio.md、entra-agent-identity.md、agent-identity.md。
# 可重复执行：按显示名查找已有对象；ID 写入 state/identity.env。
source "$(dirname "$0")/lib.sh"
OUT="$ROOT/state/identity.env"
touch "$OUT"
save() { grep -v "^$1=" "$OUT" > "$OUT.tmp" || true; echo "$1=$2" >> "$OUT.tmp"; mv "$OUT.tmp" "$OUT"; }
warn() { echo "WARN: $*" | tee -a "$ROOT/state/identity-warnings.log"; }

TENANT_ID=$(az account show --query tenantId -o tsv)
ME=$(az ad signed-in-user show --query id -o tsv)
DOMAIN="$DNS_LABEL.$LOCATION.cloudapp.azure.com"
ISSUER=$(az aks show -g "$RG" -n "$CLUSTER_NAME" --query oidcIssuerProfile.issuerUrl -o tsv)
test -n "$ISSUER"
save TENANT_ID "$TENANT_ID"; save OWNER_OBJECT_ID "$ME"; save DOMAIN "$DOMAIN"; save OIDC_ISSUER "$ISSUER"
GRAPH_APP_ID=00000003-0000-0000-c000-000000000000
GRAPH_SP_ID=$(az ad sp show --id $GRAPH_APP_ID --query id -o tsv)

app_by_name() { az ad app list --display-name "$1" --query "[0].{id:id,appId:appId}" -o tsv; }

add_fic() {  # app object id, name, subject
  local n
  n=$(az rest --method get --url "https://graph.microsoft.com/v1.0/applications/$1/federatedIdentityCredentials" \
      --query "length(value[?name=='$2'])" -o tsv)
  if [ "$n" = "0" ]; then
    az rest --method post --url "https://graph.microsoft.com/v1.0/applications/$1/federatedIdentityCredentials" \
      --body "$(jq -n --arg name "$2" --arg issuer "$ISSUER" --arg subject "$3" \
        '{name:$name,issuer:$issuer,subject:$subject,audiences:["api://AzureADTokenExchange"]}')" -o none
  fi
}

grant_consent() {  # client sp, resource sp, scopes —— 需要管理员权限，失败只告警
  az rest --method post --url https://graph.microsoft.com/v1.0/oauth2PermissionGrants \
    --body "$(jq -n --arg c "$1" --arg r "$2" --arg s "$3" '{clientId:$c,consentType:"AllPrincipals",resourceId:$r,scope:$s}')" \
    -o none 2>/dev/null || warn "管理员同意未授予：$3（需要租户管理员）"
}

log "1. 登录组"
GROUP_ID=$(az ad group list --display-name "$LOGIN_GROUP" --query "[0].id" -o tsv)
if [ -z "$GROUP_ID" ]; then
  GROUP_ID=$(az ad group create --display-name "$LOGIN_GROUP" --mail-nickname "$LOGIN_GROUP" --query id -o tsv)
fi
az ad group member check --group "$GROUP_ID" --member-id "$ME" --query value -o tsv | grep -q true \
  || az ad group member add --group "$GROUP_ID" --member-id "$ME"
save ALLOWED_GROUP_ID "$GROUP_ID"

log "2. 门户登录应用"
read -r PORTAL_OBJ PORTAL_CLIENT_ID <<< "$(app_by_name cetirun-poc-portal-login)"
if [ -z "${PORTAL_OBJ:-}" ]; then
  r=$(az rest --method post --url https://graph.microsoft.com/v1.0/applications --body "$(jq -n --arg d "$DOMAIN" '{
    displayName:"cetirun-poc-portal-login", signInAudience:"AzureADMyOrg",
    web:{redirectUris:["https://\($d)/oauth2/callback"]},
    groupMembershipClaims:"ApplicationGroup",
    optionalClaims:{idToken:[{name:"login_hint",essential:false}]}}')")
  PORTAL_OBJ=$(jq -r .id <<< "$r"); PORTAL_CLIENT_ID=$(jq -r .appId <<< "$r")
fi
PORTAL_SP=$(az ad sp list --filter "appId eq '$PORTAL_CLIENT_ID'" --query "[0].id" -o tsv)
[ -n "$PORTAL_SP" ] || PORTAL_SP=$(az ad sp create --id "$PORTAL_CLIENT_ID" --query id -o tsv)
n=$(az rest --method get --url "https://graph.microsoft.com/v1.0/servicePrincipals/$PORTAL_SP/appRoleAssignedTo" \
    --query "length(value[?principalId=='$GROUP_ID'])" -o tsv)
[ "$n" != "0" ] || az rest --method post --url "https://graph.microsoft.com/v1.0/servicePrincipals/$PORTAL_SP/appRoleAssignedTo" \
  --body "$(jq -n --arg g "$GROUP_ID" --arg sp "$PORTAL_SP" '{principalId:$g,resourceId:$sp,appRoleId:"00000000-0000-0000-0000-000000000000"}')" -o none
perms=""
for scope in openid profile email User.Read; do
  sid=$(az ad sp show --id $GRAPH_APP_ID --query "oauth2PermissionScopes[?value=='$scope'].id | [0]" -o tsv)
  perms="$perms $sid=Scope"
done
# shellcheck disable=SC2086
az ad app permission add --id "$PORTAL_CLIENT_ID" --api $GRAPH_APP_ID --api-permissions $perms 2>/dev/null || true
grant_consent "$PORTAL_SP" "$GRAPH_SP_ID" "openid profile email User.Read offline_access"
add_fic "$PORTAL_OBJ" oauth2-proxy system:serviceaccount:cetirun-istio:oauth2-proxy
save PORTAL_CLIENT_ID "$PORTAL_CLIENT_ID"

log "3. Blueprint"
BP_OBJ=$(az rest --method get --headers OData-Version=4.0 \
  --url "https://graph.microsoft.com/v1.0/applications/microsoft.graph.agentIdentityBlueprint?\$filter=displayName eq 'cetirun-poc-blueprint'" \
  --query "value[0].id" -o tsv 2>/dev/null || true)
if [ -z "$BP_OBJ" ]; then
  r=$(az rest --method post --headers OData-Version=4.0 \
    --url https://graph.microsoft.com/v1.0/applications/microsoft.graph.agentIdentityBlueprint \
    --body "$(jq -n --arg me "$ME" '{"@odata.type":"Microsoft.Graph.AgentIdentityBlueprint",
      displayName:"cetirun-poc-blueprint",
      "sponsors@odata.bind":["https://graph.microsoft.com/v1.0/users/\($me)"],
      "owners@odata.bind":["https://graph.microsoft.com/v1.0/users/\($me)"]}')")
  BP_OBJ=$(jq -r .id <<< "$r")
fi
BP_CLIENT_ID=$(az rest --method get --url "https://graph.microsoft.com/v1.0/applications/$BP_OBJ" --query appId -o tsv)
BP_SP=$(az ad sp list --filter "appId eq '$BP_CLIENT_ID'" --query "[0].id" -o tsv)
if [ -z "$BP_SP" ]; then
  BP_SP=$(az rest --method post --headers OData-Version=4.0 \
    --url https://graph.microsoft.com/v1.0/servicePrincipals/microsoft.graph.agentIdentityBlueprintPrincipal \
    --body "$(jq -n --arg a "$BP_CLIENT_ID" '{appId:$a}')" --query id -o tsv)
fi
add_fic "$BP_OBJ" cetirun-operator system:serviceaccount:cetirun-system:cetirun-operator-controller-manager
add_fic "$BP_OBJ" cetirun-broker-blueprint system:serviceaccount:cetirun-credentials:cetirun-agent-broker
SCOPE_ID=$(az rest --method get --url "https://graph.microsoft.com/v1.0/applications/$BP_OBJ" \
  --query "api.oauth2PermissionScopes[?value=='access_agent'].id | [0]" -o tsv)
if [ -z "$SCOPE_ID" ]; then
  SCOPE_ID=$(python3 -c 'import uuid; print(uuid.uuid4())')
  az rest --method patch --headers OData-Version=4.0 --url "https://graph.microsoft.com/v1.0/applications/$BP_OBJ" \
    --body "$(jq -n --arg id "$SCOPE_ID" --arg app "$BP_CLIENT_ID" '{identifierUris:["api://\($app)"],
      api:{oauth2PermissionScopes:[{id:$id,isEnabled:true,type:"User",value:"access_agent",
        adminConsentDisplayName:"Access agent",adminConsentDescription:"Allow the application to access the agent on behalf of the signed-in user.",
        userConsentDisplayName:"Access agent",userConsentDescription:"Allow the application to access the agent on your behalf."}]}}')" -o none
fi
save BLUEPRINT_CLIENT_ID "$BP_CLIENT_ID"; save BLUEPRINT_OBJECT_ID "$BP_OBJ"; save BLUEPRINT_PRINCIPAL_ID "$BP_SP"

log "4. Authorization 应用"
read -r AUTH_OBJ AUTH_CLIENT_ID <<< "$(app_by_name cetirun-poc-delegated-authorization)"
if [ -z "${AUTH_OBJ:-}" ]; then
  r=$(az ad app create --display-name cetirun-poc-delegated-authorization --sign-in-audience AzureADMyOrg \
      --web-redirect-uris "https://$DOMAIN/api/v1/connections/callback")
  AUTH_OBJ=$(jq -r .id <<< "$r"); AUTH_CLIENT_ID=$(jq -r .appId <<< "$r")
fi
AUTH_SP=$(az ad sp list --filter "appId eq '$AUTH_CLIENT_ID'" --query "[0].id" -o tsv)
[ -n "$AUTH_SP" ] || AUTH_SP=$(az ad sp create --id "$AUTH_CLIENT_ID" --query id -o tsv)
add_fic "$AUTH_OBJ" cetirun-broker-authorization system:serviceaccount:cetirun-credentials:cetirun-agent-broker
az ad app permission add --id "$AUTH_CLIENT_ID" --api "$BP_CLIENT_ID" --api-permissions "$SCOPE_ID=Scope" 2>/dev/null || true
grant_consent "$AUTH_SP" "$BP_SP" access_agent
grant_consent "$AUTH_SP" "$GRAPH_SP_ID" "openid profile offline_access"
save AUTHORIZATION_CLIENT_ID "$AUTH_CLIENT_ID"

log "5. 加密身份与 Key Vault"
# shellcheck disable=SC2086
az identity show -g "$RG" -n cetirun-broker-encryption -o none 2>/dev/null \
  || az identity create -g "$RG" -n cetirun-broker-encryption -l "$LOCATION" --tags $TAGS -o none
ENC_CLIENT_ID=$(az identity show -g "$RG" -n cetirun-broker-encryption --query clientId -o tsv)
ENC_PRINCIPAL=$(az identity show -g "$RG" -n cetirun-broker-encryption --query principalId -o tsv)
az identity federated-credential show -g "$RG" --identity-name cetirun-broker-encryption -n cetirun-broker -o none 2>/dev/null \
  || az identity federated-credential create -g "$RG" --identity-name cetirun-broker-encryption -n cetirun-broker \
       --issuer "$ISSUER" --subject system:serviceaccount:cetirun-credentials:cetirun-agent-broker \
       --audiences api://AzureADTokenExchange -o none
KV=kv-cetirun-$(echo "$SUBSCRIPTION_ID$RG" | shasum | cut -c1-8)
if ! az keyvault show -n "$KV" -o none 2>/dev/null; then
  # shellcheck disable=SC2086
  az keyvault create -g "$RG" -n "$KV" -l "$LOCATION" --enable-rbac-authorization true \
    --enable-purge-protection true --tags $TAGS -o none
fi
KV_ID=$(az keyvault show -n "$KV" --query id -o tsv)
az role assignment create --assignee-object-id "$ME" --assignee-principal-type User \
  --role "Key Vault Crypto Officer" --scope "$KV_ID" -o none 2>/dev/null || true
for i in $(seq 1 20); do
  if az keyvault key show --vault-name "$KV" -n connection-records -o none 2>/dev/null \
     || az keyvault key create --vault-name "$KV" -n connection-records --kty RSA --size 3072 --ops wrapKey unwrapKey -o none 2>/dev/null; then
    break
  fi
  echo "等待 Key Vault 角色生效（${i}）"; sleep 20
done
az keyvault key show --vault-name "$KV" -n connection-records -o none
az role assignment create --assignee-object-id "$ENC_PRINCIPAL" --assignee-principal-type ServicePrincipal \
  --role "Key Vault Crypto Service Encryption User" --scope "$KV_ID/keys/connection-records" -o none 2>/dev/null || true
save ENCRYPTION_CLIENT_ID "$ENC_CLIENT_ID"
save KEY_VAULT_KEY_URL "https://$KV.vault.azure.net/keys/connection-records"
save KEY_VAULT_NAME "$KV"
az keyvault show -n "$KV" --query "{pna:properties.publicNetworkAccess,acl:properties.networkAcls.defaultAction}" -o json

tag_rg "$RG"
log "完成"; cat "$OUT"

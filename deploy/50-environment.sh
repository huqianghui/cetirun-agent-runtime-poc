#!/usr/bin/env bash
# 生成 make deploy 的环境文件 generated/environment.yaml，并做离线 deploy-plan。
# 以 CetiRun 自带的 app/cetirun-platform/ci/deployment-example.yaml 为底，只改环境特有的值。
source "$(dirname "$0")/lib.sh"
# shellcheck disable=SC1091
source "$ROOT/state/identity.env"; source "$ROOT/state/platform.env"; source "$ROOT/state/registry.env"
REGISTRY="$ACR_LOGIN_SERVER/cetirun"   # 自建的完整 0.1.1 仓库（45-registry.sh），不直接用 MCR

KCFG="$ROOT/state/kubeconfig"
az aks get-credentials -g "$RG" -n "$CLUSTER_NAME" --file "$KCFG" --overwrite-existing -o none
kubelogin convert-kubeconfig -l azurecli --kubeconfig "$KCFG"
export KUBECONFIG="$KCFG"

GIT_SHA=0af84e718108aa0700620a798d84b561c706b0b0   # MCR 0.1.1 镜像的 org.opencontainers.image.revision
PI_DIGEST=$(curl -sI -H 'Accept: application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.index.v1+json' \
  "https://mcr.microsoft.com/v2/aks/ai-runtime/cetirun/cetirun-pi-agent/manifests/$VERSION" | awk -F': ' 'tolower($1)=="docker-content-digest"{print $2}' | tr -d '\r')
API_IP=$(kubectl get endpointslices -n default -l kubernetes.io/service-name=kubernetes -o jsonpath='{.items[0].endpoints[0].addresses[0]}')

export ROOT GIT_SHA PI_DIGEST API_IP KCFG RG NODE_RG CLUSTER_NAME SUBSCRIPTION_ID LOCATION REGISTRY VERSION DNS_LABEL CETIRUN_SRC AI_ACCOUNT OWNER_ALIAS
export TENANT_ID OWNER_OBJECT_ID PORTAL_CLIENT_ID BLUEPRINT_CLIENT_ID AUTHORIZATION_CLIENT_ID ENCRYPTION_CLIENT_ID KEY_VAULT_KEY_URL ALLOWED_GROUP_ID
export ISTIO_REVISION ADMISSION_CA_BUNDLE SKILLS_STORAGE_ACCOUNT CERT_MANAGER_ARCHIVE
python3 - <<'PY'
import os, yaml, subprocess
e = os.environ
cfg = yaml.safe_load(open(f"{e['CETIRUN_SRC']}/app/cetirun-platform/ci/deployment-example.yaml"))
kc = lambda *a: subprocess.check_output(["kubectl", *a], text=True).strip()
rev = e["ISTIO_REVISION"]
alias = e["OWNER_ALIAS"]
cfg["target"] = {"kubeconfig": e["KCFG"], "context": kc("config", "current-context"),
                 "cluster": kc("config", "view", "--minify", "-o", "jsonpath={.contexts[0].context.cluster}"),
                 "server": kc("config", "view", "--minify", "-o", "jsonpath={.clusters[0].cluster.server}"),
                 "clusterUID": kc("get", "ns", "kube-system", "-o", "jsonpath={.metadata.uid}")}
cfg["release"] = {"registry": e["REGISTRY"], "version": e["VERSION"], "gitSHA": e["GIT_SHA"]}
cfg.pop("domain", None)                      # azure-public-ip 模式由 Azure 返回 FQDN
cfg["identityRealm"] = "cetirun-poc"
cfg["ownerAlias"] = alias
cfg["identity"] = {"tenantId": e["TENANT_ID"], "portalClientId": e["PORTAL_CLIENT_ID"],
                   "blueprintClientId": e["BLUEPRINT_CLIENT_ID"], "authorizationClientId": e["AUTHORIZATION_CLIENT_ID"],
                   "encryptionClientId": e["ENCRYPTION_CLIENT_ID"], "keyVaultKeyUrl": e["KEY_VAULT_KEY_URL"]}
i = cfg["istio"]
i.update({"revision": rev, "controlPlaneDeployment": f"istiod-{rev}",
          "ingressDeployment": f"aks-istio-ingressgateway-external-{rev}",
          "meshConfigMap": f"istio-shared-configmap-{rev}"})
i["values"]["allowedGroups"] = e["ALLOWED_GROUP_ID"]
op = cfg["platform"]["values"]["cetirun-operator"]
op["cetiadmin"]["owner"] = {"alias": alias, "tenantID": e["TENANT_ID"], "objectID": e["OWNER_OBJECT_ID"]}
op["cetiadmin"]["kubernetes"]["apiEgressCIDRs"] = [f"{e['API_IP']}/32"]
op["cetiadmin"]["model"] = {"default": "gpt-5.6-terra", "initialMonthlyBudgetUSD": "50"}
op["azure"]["clientId"] = e["BLUEPRINT_CLIENT_ID"]
op["acrDockerSecret"] = "acr-helm-pull"      # 47-registry-pull.sh 建的只读拉取凭据
op["sandboxAdmission"]["caBundle"] = e["ADMISSION_CA_BUNDLE"]
op["sandboxAdmission"]["adminUsers"] = [e["OWNER_OBJECT_ID"]]
op["customRuntime"] = {"approvedImages": [f"{e['REGISTRY']}/cetirun-pi-agent@{e['PI_DIGEST']}"],
                       "storageClass": "default", "storageSize": "20Gi", "accessMode": "ReadWriteOnce", "fsGroup": 1024}
ai = cfg["platform"]["values"]["cetirun-ai-gateway"]
ai["postgresql"]["bundled"]["persistence"] = {"storageClass": "default", "size": "20Gi"}
endpoint = f"https://{e['AI_ACCOUNT']}.cognitiveservices.azure.com/"
def model(name, cin, cout):
    return {"model_name": name,
            "litellm_params": {"model": f"azure/{name}", "api_base": endpoint, "api_key": "os.environ/AZURE_OPENAI_KEY",
                               "api_version": "2025-04-01-preview",
                               # LiteLLM 不认识的模型要显式定价，否则网关按"未定价"拒绝
                               "input_cost_per_token": cin, "output_cost_per_token": cout},
            "model_info": {"mode": "chat"}}
ai["models"] = [model("gpt-5.6-luna", 0.00000025, 0.000002), model("gpt-5.6-terra", 0.00000125, 0.00001)]
cfg["modelRouting"] = {"mode": "direct"}
cfg["sharedSkills"] = {"storageClassName": "azureblob-nfs-premium", "resourceGroup": e["RG"],
                       "storageAccount": e["SKILLS_STORAGE_ACCOUNT"], "containerName": "published-skills", "protocol": "nfs"}
cfg["publicTLS"] = {"enabled": True, "dns": {"mode": "azure-public-ip", "label": e["DNS_LABEL"]},
                    "azure": {"subscriptionId": e["SUBSCRIPTION_ID"], "tenantId": e["TENANT_ID"],
                              "resourceGroup": e["RG"], "clusterName": e["CLUSTER_NAME"]},
                    "ingressService": "aks-istio-ingressgateway-external", "gatewayName": "cetirun-acme-http",
                    "certManager": {"mode": "install", "namespace": "cert-manager", "release": "cert-manager",
                                    "chartArchive": e["CERT_MANAGER_ARCHIVE"]},
                    "acme": {"email": ""}, "stateDirectory": f"{e['ROOT']}/state/public-tls", "timeoutSeconds": 900}
yaml.safe_dump(cfg, open(f"{e['ROOT']}/generated/environment.yaml", "w"), sort_keys=False)
print("written")
PY

log "离线 deploy-plan"
make -C "$CETIRUN_SRC" deploy-plan DEPLOY_CONFIG="$ROOT/generated/environment.yaml"

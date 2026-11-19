# CetiRun：常驻型 Agent Runtime

在自己的 Azure 订阅里，用 AKS 搭一套**每人一个常驻 agent** 的 runtime：每个 agent 实例是一个单副本 StatefulSet，跑在 Kata 沙箱里，HOME 盘长期保留，状态和私有记忆都在盘上，不需要做快照。用户在自助门户里创建实例，通过 Agent UI、Terminal、Desktop 三个入口使用；管理员在同一个门户里管团队、策略、沙箱、技能和模型预算。

| 组件 | 目录 | 作用 |
|---|---|---|
| 平台 chart | `app/cetirun-platform/` | 一次装好 operator、门户、AI 网关等全部组件 |
| Operator | `app/cetirun-operator/` | `Agent`、`Team` 两个 CRD 和生命周期管理；内置管理员 agent CetiAdmin；准入 webhook |
| Agent chart | `app/cetirun-chart/` | 每个实例一份：StatefulSet、HOME 盘、Desktop、Terminal、日志 |
| 门户 | `app/cetirun-portal/` | Go 后端 + React 前端，用户自助和管理员后台 |
| AI 网关 | `app/cetirun-ai-gateway/` | LiteLLM：模型路由、按团队记账和预算 |
| 连接与记忆 | `app/cetirun-connections-mcp/`、`app/cetirun-memory-context/` | 代表用户访问 Microsoft Graph / ARM 的委托连接；跨会话记忆 |
| 可观测 | `app/cetirun-observability/` | 实例日志与指标采集 |
| 基础设施 | `infra/` | AKS、Blob NFS 存储、Istio 入口认证 |
| Agent 示例 | `examples/` | 自定义 runtime（pi-agent）、原生沙箱、coding agent |
| 部署脚本 | `deploy/` | 从零部署到可用的编号脚本，见下文 |

## 在线环境

- 门户：<https://cetirun-qiah.eastus2.cloudapp.azure.com/>（Let's Encrypt 证书，cert-manager 自动续期）
- 登录：Entra 账号，必须是 Entra 组 `cetirun-poc-users` 的成员；首次登录要自己同意 openid / profile / email / User.Read / offline_access 这几个低风险权限
- AI 网关控制台：门户管理员后台的 **AI Gateway** 页，或直接打开 `/ai-gateway/ui/`

功能怎么用、每一步看到什么，见 [使用指南](docs/USAGE.md)。

## 部署

### 准备

本机工具：`az`、`kubelogin`、`helm`、`skopeo`、`jq`、`make`、`python3`（带 `pyyaml`、`certifi`）、`openssl`、`ssh`。`kubectl` 不需要预装，部署脚本第一次运行时会下载一个与集群版本匹配的到 `bin/`。

Azure 侧：一个有 Owner 权限的订阅，并且能在租户里创建 Entra 组和应用。

```bash
git clone https://github.com/huqianghui/cetirun-agent-runtime-poc.git && cd cetirun-agent-runtime-poc
cp config.env.example config.env    # 填订阅、模型账号名、门户域名前缀和管理员别名
```

所有脚本都从 `config.env` 读配置，运行中生成的 ID、证书、私钥和 kubeconfig 写进 `state/`，生成的部署配置在 `generated/`（都已被 `.gitignore` 排除）。

### 按编号执行

每个脚本都可以重复跑：中途失败，修好问题后再跑同一个脚本即可。

| 步骤 | 脚本 | 做什么 | 耗时 |
|---|---|---|---|
| 1 | `deploy/10-resource-group.sh` | 两个资源组，打标签 | 1 分钟 |
| 2 | `deploy/15-models.sh` | AIServices 账号，部署 `gpt-5.6-luna`、`gpt-5.6-terra` | 2 分钟 |
| 3 | `deploy/20-cluster.sh` | AKS 1.37：托管 Istio、Azure CNI + Cilium、Azure RBAC，system 池和两个 Kata 池 | 15 分钟 |
| 4 | `deploy/30-identity.sh` | 登录组、门户登录应用、Agent Identity Blueprint、授权应用、加密身份、Key Vault 密钥 | 3 分钟 |
| 5 | `deploy/40-platform-prereqs.sh` | Blob NFS 存储、命名空间、内部 TLS 证书、AI 网关密钥 | 3 分钟 |
| 6 | `ACR_ONLY=1 deploy/45-registry.sh` | 建镜像仓库 ACR 并挂到集群 | 2 分钟 |
| 7 | `deploy/46-build-publish.sh` | 在构建机上用本仓库源码 `make publish`：13 个镜像、9 个 chart 推到 ACR，完成后构建机解除分配 | 30~60 分钟 |
| 8 | `deploy/47-registry-pull.sh` | Operator 拉 agent chart 用的只读 ACR token | 1 分钟 |
| 9 | `deploy/50-environment.sh` | 生成部署配置 `generated/environment.yaml`，离线跑一遍 `deploy-plan` 校验 | 1 分钟 |
| 10 | `deploy/56-local-deploy.sh` | 在本机以本人身份 `make deploy`：平台组件、公网 IP 与域名、Let's Encrypt 证书 | 15 分钟 |
| 11 | `deploy/58-ai-gateway-console.sh` | 把 AI 网关控制台挂到门户域名的 `/ai-gateway/ui/` | 2 分钟 |

完成后打开 `https://<DNS_LABEL>.<LOCATION>.cloudapp.azure.com/`，用 `cetirun-poc-users` 组里的账号登录。执行部署的人（`OWNER_ALIAS`）同时是门户管理员和准入 webhook 的部署管理员；其他用户把他们加进这个组即可。

### 为什么在本机部署

`make deploy` 会写 CetiAdmin 的配置，而这份配置只允许准入管理员名单里的人修改，名单在首次安装时定下，之后的 `make deploy` 不允许改。所以部署要以首次安装那个人的身份执行。`55-remote-deploy.sh`（在构建机上用托管身份部署）因此走不通，留作参考。

本机部署只用到仓库 `bin/` 下的两个工具：`kubectl`（脚本自动下载，部署脚本要求与集群版本差不超过一个小版本）和 `docker`（一个 skopeo 替身，只实现部署时检查镜像是否存在的 `buildx imagetools inspect`），不在系统里装任何东西。

### 版本号

源码版本是 `0.1.1-src.20261111`：`46-build-publish.sh` 在构建机上执行 `make set-version` 后构建，镜像和 chart 用同一个版本号。VS Code Server 镜像没有构建（需要接受它的许可条款），所以没有 VS Code 类型的实例。

## 使用

门户的完整操作步骤和截图见 [使用指南](docs/USAGE.md)，涵盖：

1. **创建自己的 agent**：选团队和 agent 类型（OpenClaw、Hermes、自定义 runtime），几分钟后实例就绪
2. **三个入口**：Agent UI 聊天、Terminal 网页终端、Desktop 远程桌面，都在实例的工作区页里
3. **连接服务**：授权 agent 代表你访问 Microsoft Graph / ARM（需要租户管理员同意，见已知限制）
4. **管理员后台**：Teams、Agent policies（模型、出站规则）、Sandboxes、Skills 市场、全部 Agents
5. **AI 网关**：在管理员后台里直接打开 LiteLLM 控制台，按团队看用量、设预算

## 运维

补标签可以随时重跑：

```bash
bash -c 'source deploy/lib.sh; tag_rg "$RG"; tag_rg "$NODE_RG"'
```

AI 网关控制台的 admin 密码：

```bash
kubectl get secret -n cetirun-system cetirun-ai-gateway-auth -o jsonpath='{.data.ui-password}' | base64 -d
```

## 已知限制

- **建实例时不要勾选 Agent Identity**：部署时强制开启了身份生命周期，但 Operator 删除身份所需的 Graph 权限还没有验证过，验证之前不要让实例申请身份。
- **委托连接要管理员同意**："代表你访问 ARM / Graph"需要租户管理员同意，同意之前用不了。
- **HOME 盘随实例删除**：默认 StorageClass 的回收策略是 Delete，删实例会删数据。
- **内部证书一年有效**：内部 TLS 用 `state/tls/` 下的自签 CA，叶子证书到期前要轮换，否则准入 webhook 会拒绝所有受管写入。
- **订阅的后台安全自动化**：构建机 NSG 的 SSH 入站规则即使打了 `securityControl=Ignore` 也会被删，跑 `46` 前脚本会重新加。
- **本机 GSA**：Global Secure Access 的 Private Access 会截获 `*.cognitiveservices.azure.com`、`*.vault.azure.net`、`*.azurecr.io` 等域名，部署和运维时要关掉 GSA。

## 相关文章

常驻型和会话型两种 runtime 的取舍、管控台和 LiteLLM 的分工，写成了系列文章：

- [00：导读](https://github.com/huqianghui/mindforge/blob/main/Azure/Azure-Kubernetes-Service/AKS-Agent-Runtime%E7%B3%BB%E5%88%9700%EF%BC%9A%E5%AF%BC%E8%AF%BB.md)
- [12：常驻实例与会话调度](https://github.com/huqianghui/mindforge/blob/main/Azure/Azure-Kubernetes-Service/AKS-Agent-Runtime%E7%B3%BB%E5%88%9712%EF%BC%9A%E5%B8%B8%E9%A9%BB%E5%AE%9E%E4%BE%8B%E4%B8%8E%E4%BC%9A%E8%AF%9D%E8%B0%83%E5%BA%A6.md)
- [13：Runtime 管控台与 LiteLLM 的分工](https://github.com/huqianghui/mindforge/blob/main/Azure/Azure-Kubernetes-Service/AKS-Agent-Runtime%E7%B3%BB%E5%88%9713%EF%BC%9ARuntime%E7%AE%A1%E6%8E%A7%E5%8F%B0%E4%B8%8ELiteLLM%E7%9A%84%E5%88%86%E5%B7%A5.md)

会话型 runtime（Turn + OpenSandbox）在另一个仓库 [aks-sandbox-poc](https://github.com/huqianghui/aks-sandbox-poc)。

# CetiRun 部署配置说明

这份文档讲在自己的订阅里部署时要改哪些配置、它们在哪里、运行中会生成什么文件。部署步骤见 [README](../README.md#部署)，部署后怎么用见 [使用指南](USAGE.md)。

所有环境相关的值都在 `config.env` 里，部署脚本只读变量，不写死订阅、区域、资源名、模型和版本。换一个环境只需要改 `config.env`。

## 一、文件在哪里

| 位置 | 进 git | 内容 |
|---|---|---|
| `config.env.example` | 是 | 配置模板，所有配置项和默认值 |
| `config.env` | **否** | 你自己的配置，从模板复制后填写 |
| `deploy/*.sh` | 是 | 编号部署脚本，按顺序执行，都从 `config.env` 读配置 |
| `deploy/lib.sh` | 是 | 公共函数：读 `config.env`、打标签、日志 |
| `bin/docker` | 是 | skopeo 做的 docker 替身，只用于部署时检查镜像是否存在；ACR 令牌缓存 10 分钟 |
| `bin/kubectl`、`bin/kubelogin` | 否 | 第一次运行 `56` 时按 `KUBECTL_VERSION` 下载 |
| `state/` | **否** | 运行中生成的 ID、证书、私钥、kubeconfig，见第四节 |
| `generated/` | 否 | 生成的部署配置（集群参数、`environment.yaml`），可随时重新生成 |
| `~/.cetirun-poc/public-tls/` | 否 | Let's Encrypt 账号和证书状态，`make deploy` 要求在源码仓库之外 |

`state/`、`generated/`、`config.env` 都在 `.gitignore` 里。**不要用 `git add -f` 提交它们**，里面有私钥、证书和各资源 ID。

## 二、必须改的配置

复制模板后，下面这些每个环境都不一样，必须填：

```bash
cp config.env.example config.env
```

| 配置项 | 填什么 | 说明 |
|---|---|---|
| `SUBSCRIPTION_ID` | 你的订阅 ID | 需要 Owner 权限 |
| `AI_ACCOUNT` | 如 `ai-cetirun-<后缀>` | AIServices 账号名，同时是子域名，**全局唯一** |
| `DNS_LABEL` | 如 `cetirun-<后缀>` | 门户域名前缀，门户地址是 `https://<DNS_LABEL>.<LOCATION>.cloudapp.azure.com/`，同一区域内唯一 |
| `OWNER_ALIAS` | 你的 alias | 门户管理员和 CetiAdmin 的 owner。alias 是 Entra 登录名（`preferred_username`）`@` 前面那段，见第五节 |

## 三、可选配置

下面这些有默认值，按需修改。

### 区域与资源名

| 配置项 | 默认值 | 说明 |
|---|---|---|
| `LOCATION` | `eastus2` | 区域。换区域前先确认该区域有下面的 VM 机型配额和模型配额 |
| `RG` | `rg-cetirun-poc` | 资源组，所有资源都放这里 |
| `NODE_RG` | `rg-cetirun-poc-nodes` | AKS 节点资源组 |
| `CLUSTER_NAME` | `aks-cetirun-poc` | AKS 集群名 |
| `TAGS` | `securityControl=Ignore costControl=Ignore project=cetirun-poc` | 所有资源组和资源都打这些标签。订阅有自动关停、自动删规则的策略时，按订阅要求调整 |

### 登录与权限

| 配置项 | 默认值 | 说明 |
|---|---|---|
| `LOGIN_GROUP` | `cetirun-poc-users` | 能登录门户的 Entra 组，`30-identity.sh` 不存在就创建，并把部署人加进去 |
| `PORTAL_ADMIN_ALIASES` | 空 | 其他门户管理员，空格分隔。`OWNER_ALIAS` 自动包含 |
| `MAX_AGENTS_PER_USER` | `2` | 每人可建的 agent 数，`0` 为不限。常驻形态的本意是每人 1 个 |

### 模型

| 配置项 | 默认值 | 说明 |
|---|---|---|
| `MODELS` | `gpt-5.6-luna:2026-07-09:0.00000025:0.000002 gpt-5.6-terra:2026-07-09:0.00000125:0.00001` | 每项 `名字:模型版本:输入单价:输出单价`，空格分隔。名字既是模型名也是部署名；单价单位是美元/token，LiteLLM 按它记账和执行预算 |
| `MODEL_SKU` | `GlobalStandard` | 模型部署的 SKU |
| `MODEL_CAPACITY` | `100` | 每个部署的容量（千 TPM），受订阅配额限制 |
| `AOAI_API_VERSION` | `2025-04-01-preview` | AI 网关调 Azure OpenAI 用的 API 版本 |
| `DEFAULT_MODEL` | `gpt-5.6-terra` | CetiAdmin 默认用的模型，必须在 `MODELS` 里 |
| `CETIADMIN_BUDGET_USD` | `50` | CetiAdmin 的初始月度预算 |

`15-models.sh` 和 `50-environment.sh` 都读 `MODELS`，一处修改，模型部署和 LiteLLM 的定价同时生效。

### 节点池

| 配置项 | 默认值 | 说明 |
|---|---|---|
| `SYSTEM_VM` / `SYSTEM_MIN` / `SYSTEM_MAX` | `Standard_D4s_v5` / 2 / 3 | 系统节点池 |
| `AGENT_VM` / `AGENT_MIN` / `AGENT_MAX` | `Standard_D16s_v5` / 1 / 3 | Kata 池，一台约放 3 个常驻 agent。机型要支持嵌套虚拟化 |
| `AGENT2_VM` / `AGENT2_MIN` / `AGENT2_MAX` | `Standard_D8s_v5` / 0 / 1 | 第二个 Kata 池，平时 0 台，按需扩 |

### 版本

| 配置项 | 默认值 | 说明 |
|---|---|---|
| `K8S_VERSION` | `1.37` | AKS 版本 |
| `KUBECTL_VERSION` | `1.37.1` | 本机 `bin/kubectl` 的版本，部署脚本要求和集群版本差不超过一个小版本 |
| `CERT_MANAGER_VERSION` | `v1.21.3` | 签发门户公网证书用的 cert-manager |
| `BUILD_VERSION` | `0.1.1-src.20261119` | `46` 构建、`56` 部署的版本号。**改了源码要换一个新值再重跑 `46`**，否则 `56` 部署的还是旧镜像 |
| `REGISTRY` / `VERSION` | MCR / `0.1.1` | `45-registry.sh` 的复制来源；`MCR_GIT_SHA` 是这一版的 commit，只在还没跑 `46` 时用 |

### 沙箱执行

`56` 开启了沙箱执行（agent 跑在 Kata 沙箱里），operator 从 `cetirun-system/sandbox-platform` 读运行时能力和网络拓扑，给每个 agent 生成网络策略。这份登记由 `55-sandbox-platform.sh` 生成，`56` 部署前后会自动调用它。

| 配置项 | 默认值 | 说明 |
|---|---|---|
| `SANDBOX_HOME_STORAGE_CLASS` | `sandbox-home` | agent HOME 盘的 StorageClass，`55` 创建；门户的 Sandbox 里选它 |
| `SANDBOX_HOME_DISK_SKU` | `Premium_LRS` | HOME 盘的磁盘类型 |
| `SANDBOX_INGRESS_PORTS` | `18789 8642 9120 7681 6080` | 入口网关能连 agent 的端口：OpenClaw、Hermes API、Hermes Web、Terminal、Desktop。**少一个，对应的入口就会连接超时** |

登记里的 CoreDNS 和 AI 网关地址、Pod/Service/节点网段、API Server 地址、入口网关的 UID、Cilium 版本，都由 `55` 从集群现查，不需要配置。集群升级（Cilium 版本会变）或重建入口网关之后，重跑 `55` 即可；内容有变化时它会自动重启 operator，让各 agent 的网络策略按新登记重新生成。

### 构建机与存储

| 配置项 | 默认值 | 说明 |
|---|---|---|
| `BUILD_VM` / `BUILD_VM_SIZE` / `BUILD_VM_IMAGE` | `vm-cetirun-build` / `Standard_D8s_v5` / `Ubuntu2404` | `46` 用的临时构建机，构建完自动解除分配 |
| `AGENT_STORAGE_SIZE` | `20Gi` | 自定义 runtime agent 的 HOME 盘 |
| `GATEWAY_DB_SIZE` | `20Gi` | AI 网关自带 PostgreSQL 的盘 |

Hermes、OpenClaw 这类 agent 的 HOME 盘大小不在这里，在门户的 **Sandbox** 里配（见使用指南）。

### 运行时开关

这两个不写进 `config.env`，只在执行某个脚本时临时加在命令前面：

| 开关 | 用在 | 作用 |
|---|---|---|
| `ACR_ONLY=1` | `45-registry.sh` | 只建 ACR 并挂到集群，不从 MCR 复制镜像。从源码构建（`46`）时用这个 |
| `REDEPLOY=true` | `20-cluster.sh` | 集群已存在时强制重跑 Bicep（默认跳过） |

脚本里还有一些**平台自身固定的名字**不在配置里：命名空间 `cetirun-system`、`cetirun-istio`、`cetirun-credentials`，Key Vault 密钥 `connection-records`，ACR 拉取令牌 `cetirun-operator-pull`。它们由 chart、operator 和门户代码约定，改了会和平台对不上，不要改。

## 四、运行中生成的文件

脚本跑完会把生成的 ID 写进 `state/`，后面的脚本从这里读。**这些文件不要手工改，也不要提交**。

| 文件 | 由谁生成 | 内容 |
|---|---|---|
| `state/identity.env` | `30-identity.sh` | 租户 ID、部署人 object ID、登录组 ID、门户登录应用、Agent Identity Blueprint、授权应用、加密身份、Key Vault 密钥 |
| `state/platform.env` | `40-platform-prereqs.sh` | Istio revision、skill 存储账户、准入 webhook 的 CA、cert-manager chart 包路径 |
| `state/registry.env` | `45-registry.sh` | 自建 ACR 的名字和登录地址 |
| `state/build.env` | `46-build-publish.sh` | 最后一次构建的版本号和 commit |
| `state/kubeconfig` | `50`、`56` | 本人 Azure CLI 身份的 kubeconfig |
| `state/tls/` | `40` | 内部 TLS 的自签 CA 和叶子证书（一年有效） |
| `state/ssh/` | `46` | 构建机的 SSH 密钥。构建不走 SSH，只在 VM 创建时用 |
| `~/.cetirun-poc/public-tls/` | `56` | 门户公网证书的 ACME 状态 |
| `generated/environment.yaml` | `50` | `make deploy` 的部署配置 |
| `generated/environment-local.yaml` | `56` | 在 `environment.yaml` 基础上补上构建版本和本人身份 |
| `generated/sandbox-platform.json` | `55` | 写进 `sandbox-platform` ConfigMap 的平台登记 |

想从头重来：删掉资源组，再删 `state/`、`generated/` 和 `~/.cetirun-poc/`。

## 五、前置条件与限制

**Azure 与 Entra 权限**

- 订阅 Owner：要建资源组、AKS、ACR、Key Vault，并给托管身份分配角色；
- 能在租户里创建 Entra 组和应用注册：`30-identity.sh` 建登录组、门户登录应用、Agent Identity Blueprint；
- "代表用户访问 ARM / Graph"的委托连接需要**租户管理员同意**，没有同意之前，门户的 Connections 功能用不了，其他功能不受影响。

**配额**

- `AGENT_VM` 机型至少 `AGENT_MAX` 台的 vCPU 配额，加上系统池和构建机；
- `MODELS` 里每个模型 `MODEL_CAPACITY` 的配额。

**本机网络**

- 构建不需要任何入站端口：`46` 的命令全部经 `az vm run-command` 下发，用的是本机 `az login` 的身份，公司网络拦 22 端口也不影响；
- **Global Secure Access 要关掉**：它的 Private Access 会截获 `*.cognitiveservices.azure.com`、`*.vault.azure.net`、`*.azurecr.io` 等域名，部署和运维时请求会超时。

**谁来部署**

`make deploy` 会写 CetiAdmin 的配置，而这份配置只允许准入管理员名单里的人修改，名单在首次安装时定下。所以**之后每次部署都要以首次安装的那个人的身份执行**。别人接手时，要么由原部署人执行，要么从头装一套。

**alias 怎么确定**

门户按 Entra 登录名 `preferred_username` 的 `@` 前面那段算 alias，只允许小写字母、数字和连字符。来宾账号（B2B）按他在自己租户里的原始登录名算，不一定和邮箱相同：例如邮箱是 `alex.li@contoso.com`、来宾 UPN 是 `lialex_contoso.com#EXT#@...` 的人，alias 是 `lialex`。查法：

```bash
az ad user list --filter "mail eq '<邮箱>'" --query "[].userPrincipalName" -o tsv
```

## 六、常见修改

| 想做的事 | 改什么 | 重跑哪些脚本 |
|---|---|---|
| 给别人登录权限 | 把他们加进 `LOGIN_GROUP` 组（门户外操作，`az ad group member add`） | 不用重跑 |
| 加门户管理员 | `PORTAL_ADMIN_ALIASES` | `50`、`56` |
| 改每人 agent 数 | `MAX_AGENTS_PER_USER` | `50`、`56` |
| 换模型或改单价 | `MODELS`（`DEFAULT_MODEL` 要跟着在列表里） | `15`、`50`、`56` |
| 改了平台源码（门户、operator、chart） | `BUILD_VERSION` 换一个新值 | `46`、`50`、`56` |
| 换门户域名 | `DNS_LABEL` | `50`、`56`、`58` |
| 换区域 | `LOCATION`，资源名建议一起换 | 从头部署 |

> [!WARNING]
> 不要在改了源码之后只跑 `56`。`56` 部署的是 ACR 里 `BUILD_VERSION` 那一版的镜像和 chart，源码的改动不会生效，反而可能把线上打回旧版本。

## 七、部署失败怎么处理

**`Release xxx has an unfinished operation`，或 `another operation (install/upgrade/rollback) is in progress`**

上一次 Helm 升级被中途打断（进程被杀、网络断开，或者 operator 在升级 agent 的过程中自己被替换），release 停在 `pending-upgrade`，后面的部署都会拒绝继续。处理：回滚到上一个正常版本，释放锁，再重跑 `56`。

```bash
helm history <release> -n <namespace>          # 找最后一个 deployed 的版本号
helm rollback <release> <版本号> -n <namespace>
bash deploy/56-local-deploy.sh
```

常见的三个 release：平台 `cetirun-platform`（命名空间 `cetirun-system`）、入口 `cetirun-istio`（`cetirun-istio`）、内置管理员 agent `openclaw-cetiadmin`（`cetirun-cetiadmin`）。

> [!WARNING]
> 不要把 `helm`、`make deploy` 的输出接到 `| head` 这类会提前退出的命令上：管道一断，helm 会被中途终止，release 就停在 `pending-upgrade`。

**`helm upgrade cetirun-platform ... failed`，CetiAdmin 一直是 Provisioning**

平台升级的最后一步会等 CetiAdmin 就绪。同一次部署里 operator 也在滚动升级，可能打断它对 CetiAdmin 的升级，于是 CetiAdmin 的 release 卡在 `pending-upgrade`、agent 处于挂起状态。按上面的方法回滚 `openclaw-cetiadmin`，再重跑 `56`。这时 operator 已经是新版本，第二次会顺利完成。

**预检查很慢**

`make deploy` 正式升级前会逐个检查镜像和集群里已有对象的归属，每次查询都要经 kubelogin 换一次令牌，一次部署的预检查要十几到二十几分钟，属于正常现象。

## 八、部署后的手工步骤

这几步不在脚本里，部署完按需做：

- **LiteLLM 控制台密码**：用户名 `admin`，密码 `kubectl get secret -n cetirun-system cetirun-ai-gateway-auth -o jsonpath='{.data.ui-password}' | base64 -d`。只给管理员；
- **第一次登录的权限同意**：每个用户首次登录门户时自己同意 openid、profile、email、User.Read、offline_access；
- **租户管理员同意**：要用 Connections（代表用户访问 ARM / Graph）时，请租户管理员在 Entra 里给门户登录应用授予管理员同意。

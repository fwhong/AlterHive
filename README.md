<h1 align="center">AlterHive 幻巢智能欺骗蜜罐平台</h1>

<div align="center">

<p align="center">
  <a href="https://github.com/Fausto-404/AlterHive/releases">
    <img src="https://img.shields.io/github/v/release/Fausto-404/AlterHive?style=flat-square&label=release&color=blue&cacheSeconds=3600" alt="Release">
  </a>

  <a href="https://github.com/Fausto-404/AlterHive/stargazers">
    <img src="https://img.shields.io/github/stars/Fausto-404/AlterHive?style=flat-square&label=stars&color=brightgreen&cacheSeconds=3600" alt="GitHub Stars">
  </a>

  <a href="https://github.com/Fausto-404/AlterHive/network/members">
    <img src="https://img.shields.io/github/forks/Fausto-404/AlterHive?style=flat-square&label=forks&color=orange&cacheSeconds=3600" alt="GitHub Forks">
  </a>

  <a href="https://github.com/Fausto-404/AlterHive">
    <img src="https://img.shields.io/badge/Go-1.25+-00ADD8?style=flat-square&logo=go&logoColor=white" alt="Go">
  </a>
</p>

</div>

AlterHive（幻巢）是一款面向攻防演练、红蓝对抗和安全研究场景的高交互智能欺骗蜜罐平台。平台以虚拟拓扑、会话记忆、规则引擎和多 Agent 欺骗规划为核心，将攻击者/渗透Agent引导进可控的“子网幻象”环境中，帮助防守方观察攻击意图、拖延攻击节奏、保护真实目标。

> **本分支：`feat/manager-sensor-split`**。默认 Docker Compose 已改为本机 `manager + sensor + ingress` 拆分部署，Web UI 可选。LLM key 与 provider 调用只留在 manager；sensor 本体无 LLM/管理/IPC 凭据，通过 Unix socket 通信，并禁止 IP 出网。本文中的既有产品截图用于功能说明，不代表拆分后登录及配置界面的当前状态。

详细安装、密钥配置、迁移及排障见 [Manager/Sensor 部署指南](deploy/manager-sensor/README.md)。


<img width="2958" height="1502" alt="image" src="https://github.com/user-attachments/assets/7d64669a-b9b2-4943-b585-38ad4e32db2f" />
---

- **<font style="color:#000000;background-color:#FBF5CB;">效果实测请查看：</font>**[**<font style="color:#000000;background-color:#FBF5CB;">https://xz.aliyun.com/news/92504</font>**](https://xz.aliyun.com/news/92504)

## 平台价值
+ **高交互欺骗**：提供 SSH 入口、模拟服务、虚拟文件系统和命令响应，让攻击者获得连续交互体验。
+ **子网幻象**：根据攻击意图动态扩展影子子网、跳板主机、服务资产和可见路径。
+ **证据驱动**：通过 `.env`、日志、bash history、GitLab、Jenkins、数据库、Kubernetes 等线索逐步引导攻击链。
+ **状态可控**：通过 gate、required_state、visible_after、protected target 等机制控制事实暴露节奏。
+ **分层隔离**：规则引擎负责模拟与安全检查；sensor 再由 `network_mode: none`、全线程 seccomp 和最小权限容器限制真实 IP 网络能力。
+ **可视化审计**：Web 控制台展示会话、命令、拓扑、证据命中、攻击者画像和系统状态。
+ **分离运维配置**：拓扑/SSH 配置与 manager 的 LLM 配置分别挂载；默认只读，由运维修改配置并重建对应容器。

## 适用场景
+ 攻防演练期间部署可控入口，观察攻击者后渗透路径和目标偏好。
+ 蓝队希望将攻击者从真实资产引流到虚拟网络和影子目标。
+ 安全研究人员验证自动化渗透 Agent、扫描器和横向移动工具的行为模式。
+ 企业内网需要构造 GitLab、Jenkins、Redis、MySQL、DC、跳板机等组合诱饵场景。
+ 需要沉淀攻击命令、证据命中、拓扑扩展和欺骗策略的复盘材料。

## 当前分支架构

```text
攻击者 ── SSH :2222 ──→ ingress ── ssh.sock ──→ sensor
运维   ── API :8000 ──→ ingress ── api.sock ──→ sensor
                        验证 session token      本地规则、会话、虚拟世界、安全校验
                        转发前移除认证头        │
                                                └── decision.sock ──→ manager ──→ LLM provider
运维   ── 独立管理 token / API :8110 ────────────────────────────────────┘
```

| 角色 | 负责内容 | 凭据与网络边界 |
| :--- | :--- | :--- |
| `manager` | provider 配置、LLM 请求、系统提示模板、模型参数、决策解析 | 独占 provider key；决策仅监听 Unix socket，管理 API 使用另一监听器及独立 token |
| `sensor` | SSH 会话处理、确定性响应、状态上下文采集、决策结果校验与应用 | 无 LLM/管理/IPC token；`network_mode: none`；seccomp 仅允许创建 `AF_UNIX` socket |
| `ingress` | TCP SSH 转发、session API 认证与转发 | 仅持有 session 管理 token，不持有 LLM/manager 凭据；只挂 sensor 入口 socket 卷 |
| `standalone` | 单进程开发兼容模式 | 仍可加载本地 LLM 配置，**不提供 split 隔离保证**；不在默认 Compose 中运行 |

独立 ingress 使 sensor 不需要为 SSH 入站加入任何 IP 网络。Unix 对端通过内核 UID 校验，不使用共享 Bearer token。此实现是**同一 Docker 主机上的 sidecar 方案**，不是跨主机 TCP manager 集群。

### 命令与异步决策流程

1. sensor 的规则/安全/会话路径先提供确定性响应，不等待 LLM 网络调用。
2. 允许上下文导出时，sensor 冻结请求上下文，通过 Unix socket 请求 manager 决策。
3. manager 调用已配置的 provider，返回终端内容、intent、plan 或 WorldPatch；不返回 provider 凭据。
4. sensor 将后台结果排队，在后续串行命令中进行对应的安全、一致性、可见条件检查，再更新虚拟世界/缓存。
5. manager 故障时继续提供规则响应；连接失败退避 30 秒。限流按 `Retry-After` 单独退避，恢复后相同命令仍可重新请求。

## 核心亮点功能展示
### 1、动态拓扑欺骗
+ 支持运行时根据攻击者目标扩展 shadow segment、shadow host 和 pivot edge。
+ 事实一致性策略，已暴露事实锁定机制，前后响应一致。
<img width="2952" height="1524" alt="image" src="https://github.com/user-attachments/assets/0c09d871-641e-475e-8614-97759b1e2c5d" />

### 2、高交互 SSH 蜜罐
+ 支持 SSH 入口、shell 命令、嵌套 SSH、密码提示和跳板上下文。
+ 内置常见命令响应，包括 `nmap`、`fscan`、`curl`、`ssh`、`mysql`、`redis-cli`、`kubectl` 等。
+ 可记录攻击者命令、远端地址、会话状态、证据命中和拓扑变化。
<img width="2896" height="1518" alt="image" src="https://github.com/user-attachments/assets/28a4cc28-8525-4971-b437-8e181dc59981" />

### 3、证据链投放
+ 在虚拟文件系统中放置 `.env`、应用日志、Git 凭据、部署脚本、kubeconfig 等线索。
+ 使用 `visible_after` 和 gate 控制证据出现时机。
+ 根据攻击者行为画像调整线索密度和响应策略。
<img width="2450" height="1334" alt="image" src="https://github.com/user-attachments/assets/c31aef21-ded3-4fa2-8ccf-0868bdfe98e1" />

### 4、Web 管理控制台
+ 运营总览：查看会话数量、活跃攻击者、证据命中和系统状态。
+ 会话管理：查看会话列表、命令历史、攻击者画像和终端回放。
+ 拓扑视图：查看入口网段、主机、服务、边、影子资产和当前会话可见范围。
+ 系统配置：查看运行参数；单独连接 manager 查看 LLM 状态及测试连接。拆分部署默认只读，不再从蜜罐容器调用 Docker 修改部署。
<img width="2962" height="1484" alt="image" src="https://github.com/user-attachments/assets/ec25cd5a-78e5-4074-b029-60c43bbefc10" />



## 项目目录

```text
AlterHive/
├── api/                         # session API、管理认证、模拟接口
├── configs/                     # topology/SSH 示例；standalone 的配置目录
├── deploy/manager-sensor/
│   ├── docker-compose.yml       # 三个后端角色 + 可选 frontend
│   ├── llm.yaml.example         # 无真实 key 的 provider 模板
│   ├── .env                     # 初始化生成：两个独立管理 token，不入库
│   ├── manager.env              # 初始化生成：仅 manager 的 provider key，不入库
│   ├── manager-configs/         # 初始化生成：llm.yaml，不入库
│   └── sensor-configs/          # 初始化生成：topology.yaml、services/ssh.yaml
├── frontend/                    # React/TypeScript；session 与 manager API client 分离
├── internal/
│   ├── deception/               # 规划、决策协议、异步结果及一致性检查
│   ├── decisionsvc/             # manager 决策服务与预算限制
│   ├── ipc/                     # Unix socket、peer UID、SSH 源地址传递
│   ├── sandbox/                 # sensor 环境白名单、Linux seccomp
│   ├── llm/                     # provider 调用、脱敏、配置事务
│   ├── domain/                  # 会话、拓扑、世界、热路径变更队列/快照
│   ├── engine/                  # 规则响应、异步终端增强与缓存
│   ├── responders/              # 各类模拟协议响应
│   └── session/                 # 会话生命周期和串行命令执行
├── scripts/
│   ├── init-split.py             # 只创建缺失配置，不覆盖操作员文件
│   ├── build-local-image.sh      # 本机 Go 编译生产 scratch 镜像
│   └── verify-split.sh           # 合成 key/mock provider 容器端到端回归
├── Dockerfile                   # 使用 Go builder 的源码镜像构建
├── Dockerfile.runtime           # 本机编译后二进制的 scratch 运行镜像
├── docker-compose.yml           # include 安全拆分部署
├── run_manager.go / run_sensor.go / run_ingress.go / run_standalone.go
└── main.go                      # 严格校验 ALTERHIVE_ROLE
```

## 快速启动

以下命令在仓库根目录执行。需要 Docker（运行 Linux 容器）、支持 `include` 的 Docker Compose v2、Python 3；本机镜像构建还需要 Go 1.25+ 和公共 CA bundle。sensor/ingress 的网络过滤支持 Linux amd64/arm64，不支持直接在 macOS/Windows 进程中运行。

### 1. 初始化并启动规则模式

```bash
python3 scripts/init-split.py
scripts/build-local-image.sh alterhive:split
docker compose --env-file deploy/manager-sensor/.env config --quiet
docker compose --env-file deploy/manager-sensor/.env up -d
```

构建脚本不需要拉取 Docker 基础镜像，但 Go 依赖尚未缓存时仍需下载依赖。也可用 `docker build -t alterhive:split .` 替代该脚本；这条路径需要拉取 Go builder 镜像。

初始化会创建两个独立随机管理 token；已有配置不会被覆盖。**新生成的 provider 模板为关闭状态**。Compose 本身显式开启 manager 的 provider 网络门控与 sensor 的上下文导出门控；启用 provider 前应确认允许将会话上下文发给选定服务，而不能将“provider 默认关闭”理解为 manager 网络被隔离。

### 2. 可选 Web 控制台

```bash
docker compose --env-file deploy/manager-sensor/.env --profile ui up -d --build
```

| 入口 | 默认宿主地址 | 用途/认证 |
| :--- | :--- | :--- |
| SSH | `127.0.0.1:2222` | 攻击者入口；示例诱饵密码为 `123456`，由 sensor 的 SSH 配置决定 |
| session API | `http://127.0.0.1:8000` | 除健康检查/登录外的业务接口使用 session 管理 token |
| manager API | `http://127.0.0.1:8110` | 独立 manager 管理 token；不是 sensor 的决策 TCP 入口 |
| Web UI（可选） | `http://127.0.0.1:3000` | 登录 session 平面后，可在状态页另行连接 manager |

Web 用户名为 `admin`，密码为生成的 `.env` 中的 **`ALTERHIVE_SENSOR_ADMIN_TOKEN`**；状态页另行输入 **`ALTERHIVE_ADMIN_TOKEN`**。不再有 `admin/admin` 默认登录，也不将 session token 复用于 manager。

### 3. 启用 LLM（可选）

1. 编辑 `deploy/manager-sensor/manager-configs/llm.yaml`，设置 endpoint、实际模型、`active_provider`，同时启用顶层和所选 provider 的 `enabled`。
2. 将真实 key **只写入** `deploy/manager-sensor/manager.env`，与配置中的 `key_env` 对应。不要写入 sensor 配置、公共 `.env`、镜像或源码。
3. 重新创建 manager 使配置和环境生效：

```bash
docker compose --env-file deploy/manager-sensor/.env up -d --force-recreate manager
```

推理模型可能把生成预算花在思考过程上。对已验证支持该选项的 DeepSeek-compatible 服务，可在 provider 下设置 `thinking: disabled`；详见 [非流式调用与推理模型](deploy/manager-sensor/README.md#非流式调用与推理模型)。

`restart` 不会重新读取 Compose 的 `env_file`。更多操作见 [部署指南](deploy/manager-sensor/README.md)，包括配置样例、端口调整、关闭上下文导出、密钥轮换和旧版本迁移。

### 检查运行状态

```bash
docker compose --env-file deploy/manager-sensor/.env ps
curl --fail http://127.0.0.1:8000/healthz
ssh -p 2222 root@127.0.0.1
```

健康检查只证明入口/sensor 可响应，不代表 provider 已启用或可用。默认 `scratch` 后端镜像没有 `sh`/`curl`，不要用容器内 shell 命令作为健康检查。

## 开发与测试

后端建议继续使用上述 split 容器，前端在本机运行：

```bash
cd frontend
npm ci
npm run dev
```

默认开发地址 `http://127.0.0.1:5173`；Vite 将 `/api` 转发到 8000，`/manager` 转发到 8110，分别使用独立凭据。若只调试 standalone，可参考 [部署指南中的开发说明](deploy/manager-sensor/README.md#standalone-开发兼容模式)，但不能用它验证容器隔离。

```bash
# 在仓库根目录执行
go test -race -count=1 ./...
go vet ./...
(cd frontend && npm ci && npm audit && npm test && npm run build)
scripts/build-local-image.sh alterhive:split
ALTERHIVE_IMAGE=alterhive:split scripts/verify-split.sh
```

端到端测试还需要 `ssh`/`expect`，使用真实生产镜像、独立临时 Compose 项目、mock provider 和合成 key，不调用付费模型；结束时清理自己的容器/网络/卷。验证内容与已实测平台见 [验证说明](deploy/manager-sensor/README.md#验证)。

## 功能模块概览
| 模块 | 主要能力 |
| :--- | :--- |
| 运营总览 | 会话数量、活跃攻击者、证据命中、PPF 触发、近期会话和系统健康状态  |
| 会话管理 | 会话列表、会话详情、命令历史、远端地址、攻击者画像和删除会话 |
| 拓扑视图 | 主机、网段、服务、边、跳板关系、影子资产和会话可见范围 |
| 命令审计 | 记录 SSH 和模拟 API 中的命令、输出、证据 token 和策略结果 |
| 模拟 API | 提供经认证的 nmap、fscan、curl、ssh、nc、ping、goal 等模拟接口，不执行真实扫描 |
| 证据系统 | 证据 token、可见条件、命中状态、证据网格和攻击阶段判断 |
| 规则引擎 | 本地命令响应、协议响应、服务枚举、复杂命令处理和安全拦截 |
| 多 Agent 欺骗 | Intent Router、Topology Planner、World Builder、Evidence、Safety、Consistency |
| LLM 配置 | manager 支持 OpenAI-compatible、Anthropic、Ollama 接口；其他服务需提供兼容 endpoint/model |
| 系统配置 | LLM配置、SSH/API 端口、拓扑 CIDR、会话超时和系统信息（拆分部署配置只读，由运维重建容器） |


## 安全边界

- 默认所有发布端口绑定宿主 loopback。需要让攻击者访问时，只调整 SSH 绑定；UI/session API/manager API 应留在受控运维网络，并按部署环境配置 TLS/VPN/受控代理。
- sensor 的“零出网”是 IP 网络/创建 socket 能力受限，不代表禁止已授权的 Unix decision RPC；该 RPC 仍可将上下文交给 manager。被攻破的 sensor 可能在预算内滥用这一窄接口。
- manager 的决策限流为进程内限制：4 并发、每秒窗口 8 请求、24 小时窗口 10,000 次决策，单次请求 256 KiB；重启会重置预算，provider 账户应另设费用硬上限。
- 不向 sensor 挂载 provider 配置、真实业务密钥、宿主网络、Docker socket 或其他代理 socket。拓扑中的诱饵密码/文件不应复用真实凭据。
- manager/ingress 的管理 token 与 provider key 是不同凭据。ingress 不是“完全无凭据”容器，但不持有 LLM key 或 manager 管理 token。
- 宿主 root、Docker daemon/内核失陷、操作员主动破坏挂载边界不在此隔离保证内。未知角色或无法安装网络过滤器时拒绝启动，不静默降级。
- [迁移与排障](deploy/manager-sensor/README.md#从旧分支部署迁移) 中列出的旧 TCP decision/shared-token 配置不能继续沿用。

## 技术架构

- 后端：Go、Gin、gliderlabs/ssh；本地规则/虚拟状态与 LLM 决策分离。
- 前端：React、TypeScript、Ant Design、Vite；独立 session/manager API client。
- 本机 IPC：Unix socket、peer UID 校验、受信 SSH PROXY header。
- 隔离：Linux seccomp TSYNC、无 sensor IP 网络、非 root、capability 清空、只读根文件系统。
- 运维：Docker Compose、YAML、manager 专用环境文件；日志通过 Logrus/容器日志查看，审计数据通过 session API/控制台查看。

## 免责声明
本项目仅用于合法授权的安全测试、攻防演练、防守验证和安全研究。请勿将本项目用于未授权访问、攻击真实系统或规避安全监测。使用者应自行承担部署、配置和使用过程中的合规与安全责任。

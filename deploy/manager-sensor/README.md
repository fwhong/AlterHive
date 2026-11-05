# 本机 Manager / Sensor 部署指南

[返回项目 README](../../README.md)

适用于 `feat/manager-sensor-split` 当前实现。默认部署是同一 Docker 主机上的 `manager + sensor + ingress`，可选启用 `frontend`。**不是跨主机 TCP manager 集群，也不兼容旧 TCP decision URL / 共享 Bearer token。**

## 架构与信任边界

```text
攻击者 → ingress TCP :2222 → ssh.sock → sensor
运维   → ingress TCP :8000 → api.sock → sensor
                                        │
                                        └→ decision.sock → manager → LLM provider
运维   ────── 独立 manager token / TCP :8110 ────────────────┘
```

| 服务 | UID:GID | 持有的凭据 | 网络与挂载 |
| --- | --- | --- | --- |
| manager | `10001:20000` | provider key、manager 管理 token | 接入 ops 网络；独占 manager 配置；创建 decision socket |
| sensor | `10002:20000` | **无 LLM/管理/IPC token** | `network_mode: none`；只挂 topology/SSH 配置；decision 卷只读；创建入口 sockets |
| ingress | `10003:20000` | 仅 session 管理 token | 接入 edge，不接入 ops；仅挂入口 socket 卷只读；无 LLM/manager 凭据 |
| frontend（可选） | 由前端镜像决定 | 不注入上述 token；浏览器按用途提供 token | 同时接入 edge/ops，分别反代 ingress 和 manager |

manager 负责模型系统提示模板、参数、provider 调用和结果解析；sensor 保留状态上下文构造、本地规则、世界状态、结果安全校验和应用。ingress 负责真实 TCP 入站，使 sensor 不必为 SSH 监听加入 IP 网络。

- Unix socket 为 `0660`、父目录 `0750`，Linux `SO_PEERCRED` 验证对端 UID：manager 只接受 sensor UID，sensor 入口只接受 ingress UID。禁止用 `chmod 777` 解决配置错误。
- ingress 验证 session API token 后，移除 Authorization/Cookie 再向 sensor 转发。SSH 源地址经受信 Unix 对端的 PROXY header 传递。
- sensor 启动时检查环境白名单并拒绝 `llm.yaml`，随后安装不可撤销的 **seccomp TSYNC**，对全部 Go 线程及子进程禁止创建非 `AF_UNIX` socket，并封堵兼容 ABI/io_uring 等绕过路径。失败则退出。
- ingress 先绑定入站 TCP，再安装相同过滤器，因此仍可接收入站连接，但不能新建出站 TCP/UDP。
- 三个后端服务均非 root、`cap_drop: ALL`、`no-new-privileges`、只读根文件系统、限制进程数/内存；这些配置不是对可选 frontend 镜像的统一声明。

## 环境要求

- Docker 能运行 Linux amd64/arm64 容器；macOS/Windows 上不能直接以原生进程运行 sensor/ingress，需使用 Linux 容器。
- 支持顶层 `include` 的 Docker Compose v2，以及 Python 3。
- 使用本机镜像构建脚本时：Go 1.25+、公共 CA bundle；`TARGETARCH` 默认取 Docker daemon 架构。
- 本机前端开发/测试需要 Node.js/npm（前端 Dockerfile 使用 Node 20 builder）；容器端到端测试还需要 `ssh`、`expect`。
- 日常操作命令均在**仓库根目录**执行。不要使用旧的 `cp .env.example .env` 代替强 token 初始化。

## 首次启动

### 初始化配置

```sh
python3 scripts/init-split.py
```

脚本只创建不存在的文件，不覆盖已有文件：

| 文件 | 内容/权限 | 实际用途 |
| --- | --- | --- |
| `deploy/manager-sensor/.env` | 两个独立随机管理 token，`0600` | Compose 插值；不是整份注入 sensor 的 env_file |
| `deploy/manager-sensor/manager.env` | provider key 模板，`0600` | 仅 manager 的 env_file |
| `deploy/manager-sensor/manager-configs/llm.yaml` | 无真实 key 的 provider 配置，`0644` | manager 的只读配置 |
| `deploy/manager-sensor/sensor-configs/topology.yaml` | 公开诱饵拓扑 | sensor 的只读配置 |
| `deploy/manager-sensor/sensor-configs/services/ssh.yaml` | SSH 诱饵配置 | sensor 的只读配置 |

这些运行时文件/目录被 Git/dockerignore 排除。镜像只复制 binary、公共 CA、topology/SSH 示例，不包含 llm.yaml、环境文件、shell、Docker CLI 或 Docker socket。不要把真实 key 写进 `0644` 的 YAML。

### 构建与启动规则模式

```sh
scripts/build-local-image.sh alterhive:split
docker compose --env-file deploy/manager-sensor/.env config --quiet
docker compose --env-file deploy/manager-sensor/.env up -d
```

脚本使用本机 Go 工具链编译 Linux 二进制，再由 `Dockerfile.runtime` 构建生产 scratch 镜像，**无需拉取 Docker 基础镜像**；Go 模块未缓存时仍需下载，不能据此宣称完整离线构建。CA 自动寻找 `/etc/ssl/cert.pem` 或 `/etc/ssl/certs/ca-certificates.crt`，也可用 `SSL_CERT_FILE` 指定公共 CA bundle，不能传私钥。

另一条构建路径是 `docker build -t alterhive:split .`，使用源码 `Dockerfile` 中的 Go builder，需要拉取该基础镜像。若使用其他镜像名，同时在 `.env` 设置 `ALTERHIVE_IMAGE`。

新生成的模板顶层和 provider 的 `enabled` 均为 false，故首次启动是规则模式。**Compose 已显式将 manager 的网络门控及 sensor 的上下文导出门控设为 true**；provider 尚未启用不等于 manager 网络被禁止。

### 可选前端、登录与健康检查

```sh
docker compose --env-file deploy/manager-sensor/.env --profile ui up -d --build
docker compose --env-file deploy/manager-sensor/.env ps
curl --fail http://127.0.0.1:8000/healthz
ssh -p 2222 root@127.0.0.1
```

| 入口 | 默认宿主地址 | 认证 |
| --- | --- | --- |
| SSH | `127.0.0.1:2222` | 示例 `passwordRegex` 接受 `123456`，这是诱饵密码，不是后台密码 |
| session API | `http://127.0.0.1:8000` | session 管理 token；健康检查/登录除外 |
| manager ops | `http://127.0.0.1:8110` | 独立 manager 管理 token |
| UI | `http://127.0.0.1:3000` | 启用 `ui` profile 后可访问 |

- Web 用户名 `admin`，密码为 `.env` 中的 `ALTERHIVE_SENSOR_ADMIN_TOKEN`。不再存在 `admin/admin` 或固定默认 token。
- 状态页另行输入 `.env` 中的 `ALTERHIVE_ADMIN_TOKEN` 连接 manager；两类 token 不能互换。Compose 将前者映射成 ingress 内部的 `ALTERHIVE_ADMIN_TOKEN`，不会传入 sensor。
- manager token 仅存浏览器 `sessionStorage`；manager 401 不清除 session 登录；注销清除两者。session 登录凭据目前存 `localStorage`，不应把两种存储策略混为一谈。
- `/healthz` 200 只证明 ingress/sensor 可响应，不证明 LLM 可用。manager 的 `/healthz` 在 decision Unix router 上，**不是** 8110 管理 router 的路由。
- 后端 scratch 镜像没有 `sh` 或 `curl`，请从宿主检查，不能依赖 `docker exec ... sh`。

## 配置 provider 与密钥

编辑 `deploy/manager-sensor/manager-configs/llm.yaml`。以下使用 OpenAI-compatible 接口举例；必须替换为实际获准的 endpoint/model 后再启用：

```yaml
enabled: true
active_provider: openai
providers:
  - id: openai
    name: OpenAI compatible
    type: openai
    base_url: https://api.openai.com/v1
    key_env: ALTERHIVE_OPENAI_API_KEY
    api_key: ""
    model: YOUR_APPROVED_MODEL_ID
    max_tokens: 1600
    temperature: 0.2
    enabled: true
```

将实际 key 仅写入 `deploy/manager-sensor/manager.env`，不要将下面占位值原样作为有效配置：

```dotenv
ALTERHIVE_OPENAI_API_KEY=REPLACE_WITH_REAL_KEY_LOCALLY
```

`key_env` 可显式指定环境变量；省略时按 provider ID 派生 `ALTERHIVE_<ID>_API_KEY`。环境变量中的 key 优先于 YAML key，不会因连接测试或保存回写 YAML。支持的 provider 类型为 `openai`、`anthropic`、`ollama`；其他厂商需要兼容的接口配置，不代表所有模型已做真实服务验证。

修改后重新创建 manager：

```sh
docker compose --env-file deploy/manager-sensor/.env up -d --force-recreate manager
```

`restart` 不会重新读取 Compose env_file。连接测试和模型列表使用**已保存的配置**，不允许覆盖 endpoint 携带现有 key，也不跟随 HTTP redirect 转发凭据。管理响应只提供 `key_configured` 和脱敏配置；不能从 API 回读 key。绑定已有 key 的 endpoint 不能通过管理 API 静默变更，迁移 provider 应由运维同步审核 endpoint 和凭据。

## 配置变更与日常运维

### 默认只读

- manager 默认 `ALTERHIVE_LLM_CONFIG_READ_ONLY=true`，配置目录只读挂载，UI 禁用写入；sensor 没有运行时配置写路由。修改 host 上的配置后，由运维重建相应容器。
- 若特意启用 manager 管理写入，必须同时将只读开关设为 false、允许 manager 配置目录写入，并保证 UID 10001 有写权限。保存通过 `0600` 临时文件与原子替换完成；持久化失败不修改内存状态。这不是默认推荐部署。
- 不再挂载 docker.sock 或从蜜罐进程自动重建容器。重建 sensor 会丢失内存会话/虚拟世界；操作前先通过 API/控制台保存需要的审计数据。

### 端口、拓扑及 SSH 配置

外部端口在 `deploy/manager-sensor/.env` 设置，容器内监听端口保持原值：

```dotenv
SENSOR_SSH_PUBLISH=127.0.0.1:2222
SENSOR_API_PUBLISH=127.0.0.1:8000
MANAGER_API_PUBLISH=127.0.0.1:8110
FRONTEND_PUBLISH=127.0.0.1:3000
```

只将 SSH 绑定到所需攻击者网络；运维端口不得随之公开。远程管理需 TLS/VPN/受控反向代理。

- 拓扑：编辑 `sensor-configs/topology.yaml`；SSH 诱饵密码、banner、sensor 超时：编辑 `sensor-configs/services/ssh.yaml`。
- 重建 sensor：`docker compose --env-file deploy/manager-sensor/.env up -d --force-recreate sensor`。
- 更新宿主发布端口后重建对应 ingress/manager/frontend；旧的 `SSH_PORT`、`API_PORT`、`FRONTEND_PORT` 不控制这份 split Compose。
- ingress 当前还具有固定的 10 分钟 SSH 连接 deadline；只改 sensor 的 SSH timeout 不能延长这层上限。

### 上下文与网络门控

| 配置 | 设置位置 | 当前 Compose 值 | 作用 |
| --- | --- | --- | --- |
| `ALTERHIVE_ALLOW_LLM_NETWORK` | manager `environment` | `"true"` | 允许 provider 网络请求 |
| `ALTERHIVE_ALLOW_LLM_CONTEXT_EXPORT` | sensor `environment` | `"true"` | 允许所有 LLM 冷路径向 manager 发送上下文 |
| `ALTERHIVE_LLM_CONFIG_READ_ONLY` | manager `environment` | `"true"` | 阻止管理 API 修改 LLM 配置 |

这些值当前直接写在 `deploy/manager-sensor/docker-compose.yml` 的服务环境中，**只在 `.env` 新增同名变量不会改变它们**。需要禁用上下文导出时，修改该 Compose 的 sensor 环境项为 `"false"`，再重新创建 sensor；不能用这一变更替代已有进程的即时取消。禁止将整个宿主环境或 manager.env 传给 sensor；其白名单会拒绝额外变量。

### 密钥/token 轮换与日志

```sh
# 修改 manager.env 或 manager token 后
docker compose --env-file deploy/manager-sensor/.env up -d --force-recreate manager
# 修改 session 管理 token 后
docker compose --env-file deploy/manager-sensor/.env up -d --force-recreate ingress
# 查看日志 / 停止服务
docker compose --env-file deploy/manager-sensor/.env logs --tail=100 manager sensor ingress
docker compose --env-file deploy/manager-sensor/.env --profile ui down
```

轮换 token 后在 UI 重新登录/连接。`.env` 与 `manager.env` 要保持 `0600`；不要把完整 `docker compose config` 或包含环境值的 `docker inspect` 输出发送到公开渠道。配置校验可使用 `config --quiet`，不展开打印凭据。`down` 不删除 host 上的这些配置文件；不要随意用 `down --volumes` 重置部署。

## 协议、异步与失败行为

- 决策端点：`POST /v1/decide`，只监听 `/run/alterhive/decisions/decision.sock`；此 router 不注册 `/api/v1/llm/*` 或登录路由。
- 请求为 `DecisionRequest`，包含 session/call_site/command、冻结的状态上下文及可选 terminal 输入；响应为 `DecisionResponse`，包含 `ok`、终端内容或解析后的 intent/plan/patch。
- Call sites：`complex_render`、`llm_fallback`、`intent_route`、`world_patch` 为 8 秒预算；`topology_plan`、`deception_plan`、`plan_repair` 为 60 秒预算。
- 后台结果在下次串行命令中应用，不从 goroutine 直接改世界；WorldPatch 经过 critic/可见门控，被拒的规划最多异步修复一次并再次校验。管理读取使用独立快照，终端输出缓存保留请求时 CWD。
- 网络/HTTP 故障退避 30 秒，仍提供规则响应。429 按 `Retry-After` 独立退避；HTTP 200 中 `ok:false` 是决策失败，不视为网络断开。旧的规则响应缓存不会永久遮挡 manager 恢复后的同一命令重试。
- 限制为单 manager 进程全局计数，不按 sensor 自报 session_id 分配额度：4 并发、每秒窗口 8 请求、24 小时窗口 10,000 次决策、请求 256 KiB；预算重启会重置，JSON 修复可能额外调用 provider，不能当作持久化费用上限。
- manager 管理监听只有在配置 `ALTERHIVE_ADMIN_ADDR` 且提供至少 32 字符的 token 时启用；默认 Compose 用独立 8110 router，仅发布到宿主 loopback。未知角色或错误安全布尔值拒绝启动。

## 从旧分支部署迁移

1. 在停服前保存所需会话/审计数据；不要把仍加载 LLM key 的旧蜜罐容器继续当作 sensor。
2. 使用初始化脚本产生独立 token 和分离的配置目录；脚本不会覆盖已存在的旧配置，必须人工检查既有文件。
3. 将 provider key 从原蜜罐的环境/YAML/挂载中移走，只配置到 manager.env。若怀疑旧容器已暴露 key，先在 provider 侧吊销并轮换。
4. 去掉旧 `ALTERHIVE_MANAGER_URL`、`ALTERHIVE_MANAGER_ADDR`、`ALTERHIVE_MANAGER_TOKEN`；替换为本机 Unix socket 卷及 peer UID 配置，不新增 TCP fallback。
5. 移除 docker.sock、整仓库/整配置目录挂载、host network 和旧自动重建流程；按当前三角色 Compose 重建。
6. 按两类新 token 登录；LLM 默认只读；修改环境后用重新创建容器，不只 `restart`。

当前实现不提供跨主机传输或多租户 sensor 身份系统。共享 UID/socket 卷不是跨主机认证方案，不应据此宣称具备远程集群隔离能力。

## 排障

| 现象 | 检查/处理 |
| --- | --- |
| 启动提示缺少 token | 先运行初始化；Compose 命令显式使用 `--env-file deploy/manager-sensor/.env` |
| `sensor refuses unexpected environment variable` | 删除不在白名单的 sensor 环境项，尤其是 key/token/proxy；不要绕过检查 |
| `sensor must not mount llm.yaml` | 纠正 sensor 配置挂载，不能把 manager-configs 挂进去 |
| `cannot enforce sensor network isolation` | 使用支持的 Linux amd64/arm64 容器/内核；不要增加“不安全启动”开关 |
| socket 权限或 UID 拒绝 | 核对 10001/10002/10003、GID 20000、卷目录所有者与只读消费者挂载；不使用 777 |
| manager API 401 | 使用 manager token，而不是 session 登录 token；轮换后清除旧浏览器 token |
| 8000 的 provider API 或 8110 的 healthz 返回 404 | 这是平面隔离后的路由安排；provider 管理在 8110，decision healthz 在 Unix socket |
| LLM 配置更新报只读 | 默认行为；修改 manager 的 host 配置并重新创建容器 |
| healthz 正常但没有 LLM 输出 | 检查顶层/provider enabled、实际 model、manager.env、两个门控、provider 状态/限流；首条命令可先返回规则输出 |
| 换 key 后仍用旧配置 | 重新创建 manager，`restart` 不会加载新的 env_file |
| 无法拉取 Docker builder 镜像 | 可用本机 Go 构建脚本；仍需可用 Go 依赖与 CA bundle |

## Standalone 开发兼容模式

二进制未设置角色时默认是 standalone，**但根 Compose 默认是 split**。开发单进程需要显式提供强 token，并只监听本机：

```sh
export ALTERHIVE_ROLE=standalone
export ALTERHIVE_ADMIN_TOKEN="$(openssl rand -hex 32)"
export SSH_ADDR=127.0.0.1:2222
export API_ADDR=127.0.0.1:8000
go run .
```

standalone 使用 `configs/`（或 `CONFIG_DIR`）而不是 split 的两个独立挂载目录；不会提供 sensor 的零凭据/零出网保证。若前端也仅调试这一单进程，在另一终端运行：

```sh
cd frontend
npm ci
VITE_LLM_API_BASE_URL=/ npm run dev
```

此时 LLM 管理经 `/api` proxy 到同一 standalone API，状态页需要单独输入同一进程配置的管理 token。这仅是开发兼容行为；正式 split 必须保留两个不同 token 和默认 `/manager` 路由。

## 验证

```sh
go test -race -count=3 ./...
go vet ./...
(cd frontend && npm ci && npm audit && npm test && npm run build)
scripts/build-local-image.sh alterhive:split
ALTERHIVE_IMAGE=alterhive:split scripts/verify-split.sh
```

端到端脚本使用**实际 Compose 与生产 scratch 镜像**，只替换为随机本地端口、独立临时项目、mock provider 和合成 key；检查真实 SSH、模拟 API、UID socket、认证分离、脱敏/override 拒绝、只读原子性、生产进程 seccomp、network-none、门控及 manager 停机/同命令恢复。需要 Go、Docker、Python 3、ssh/expect；结束时仅清理自己的容器/网络/卷。`VERIFY_WORK=/absolute/path` 可指定证据目录，其中会有合成凭据配置，不含真实 provider key。

2026-11-05 的实现回归记录：

| 范围 | 已完成结果 |
| --- | --- |
| macOS ARM64 Go race | 158 个测试/子测试 × 3 轮，共 474 次通过，无 race |
| Linux ARM64 容器单测 | 全部 10 个有测试的包执行通过，包括 Linux-only seccomp |
| 干净容器端到端 | 两轮独立项目，每轮 39/39 检查通过 |
| 前端 | `npm ci`、5 个 client 安全测试、生产构建通过；当次 `npm audit` 为 0 |
| 构建/静态检查 | `go vet`、本机 build、Linux amd64 cross-build、镜像文件内容核查通过 |

上述是该次实现验证记录，不代表每次文档编辑都重新跑过所有测试；请以重跑结果为准。Linux amd64 仅编译未实机运行；没有使用付费模型/真实 provider key，也未做浏览器逐页视觉回归。实测镜像采用本机 Go + `Dockerfile.runtime` 路径；另一条依赖 Docker Hub builder 的源码构建和完整前端 Nginx 镜像构建链不计入已通过项。前端构建有非阻断的主 chunk 体积提示。

## 安全保证的边界

“禁止出网”针对 sensor 的 IP socket/网络能力，不禁止经已授权 Unix RPC 导出上下文；被攻破的 sensor 仍可在预算内滥用这一窄接口。manager 需要访问 provider，生产费用硬上限应在 provider 账户另设。

不要挂载任意代理 socket、Docker socket、宿主网络或额外秘密；宿主 root、Docker daemon/内核失陷和操作员主动错误挂载不在容器隔离保证内。运维平面仍需受控网络及适当 TLS/VPN 防护。

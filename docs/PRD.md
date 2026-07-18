# Hookploy PRD

> 一个中心化、声明式的 webhook 部署调度器：由中心节点（main）接收 webhook，触发边缘节点（edge）拉取镜像、重启服务。为一人公司 + AI-native 工作流设计。
>
> 状态：v1 草案（2026-07-18）

## 1. 背景与问题

当前部署链路基于 [adnanh/webhook](https://github.com/adnanh/webhook)：GitHub Actions 构建镜像后 curl 一个 webhook，服务器上的 webhook 进程执行 `docker compose pull && up -d`。它在单服务器时代工作良好，但随着服务器数量增长暴露出结构性问题：

1. **不能横向扩展**：每加入一台服务器，都要重新部署一套 webhook 进程 + 域名 + TLS（如 `apps-webhook.reorx.com` 只服务 ali-hk-01，tc-sg-01 需要另起一套）。
2. **不能中心化管理**：每个服务要各自记住"对应哪台服务器的哪个 webhook 域名 + 哪个 secret"；secret 是全局共享的，轮换时要逐个 repo 同步 GitHub Secrets。
3. **无部署状态**：webhook 触发后是黑盒——没有部署历史、没有日志、没有"上一次部署成功了吗"的查询接口，排查问题只能 SSH 上去翻。

## 2. 产品定义

Hookploy 是一个 Go 实现的单一 binary，通过子命令分别以 main / edge 两种角色运行：

- **main**：唯一公网入口。接收 webhook、持有唯一的声明式配置（`hookploy.yaml`）、把部署任务分发给对应服务器、记录部署历史与日志、提供状态查询 API 和 CLI。**main 自身内建 edge 的全部执行能力**——最小部署形态下，单个 main 进程即可完成其所在机器的 webhook 部署（覆盖当前 ali-hk-01 场景）。
- **edge**：无状态执行器。主动外连 main（gRPC 长连接），接收任务、在本机执行固定操作序列、把状态和日志流式回传。**零入站端口、零域名、零证书、零本地配置**——新服务器接入只需要装 binary + 一条启动命令。

### 目标（v1）

- 一个域名、一个 URL 前缀承接所有服务的 webhook；GitHub Actions 侧只需配置 `HOOKPLOY_URL` + 每 repo 一个 `HOOKPLOY_TOKEN`。
- 一个 `hookploy.yaml` 作为全部服务定义的 SSOT：服务名、所在服务器、所在目录、部署操作序列。
- 新服务器接入成本：装 binary、跑一条 `hookploy edge` 命令，完。
- 部署可查询：每次部署有 ID、状态、日志、时间线，CLI 全量支持 `--json`。

### Non-goals（v1 明确不做）

| 不做 | 理由 |
|---|---|
| Web UI | CLI + `--json` 足够，一人公司没有"给别人看"的需求 |
| 回滚 | 回滚 = 再触发一次指向旧 digest 的部署，复用现有机制即可 |
| 多用户 / RBAC | 单用户系统 |
| 监控告警 | 属于独立关注点，不塞进部署工具 |
| 生成 Caddy / 域名路由配置 | 这是 Ansible 的职责（见 §9 边界） |
| Secrets 管理（服务的 .env） | 维持现状：Ansible 下发骨架、人工填真值 |
| main 高可用 | main 是接受的 SPOF；main 挂了只是不能自动部署，服务本身不受影响 |

### 规模假设

约 5 台服务器、10 个服务、每天 20 次部署。所有设计决策以此为准绳——**任何为百倍规模做的预留都是过度设计**。

## 3. 架构

```
GitHub Actions
    │  POST https://hookploy.reorx.com/hooks/<service>
    │  Authorization: Bearer <service-token>
    ▼
┌─ ali-hk-01 ──────────────────────────────┐
│  Caddy ──► hookploy main                 │
│             ├─ hookploy.yaml (SSOT)      │
│             ├─ SQLite (tokens/历史/日志)  │
│             └─ 内建 local executor ──► 本机 docker compose
└──────────────▲───────────▲───────────────┘
               │ gRPC 长连接 │ （edge 主动外连，双向流）
       ┌───────┴───┐   ┌───┴───────┐
       │ tc-sg-01  │   │ hh-hk-01  │
       │ hookploy  │   │ hookploy  │
       │ edge ──► docker compose   │
       └───────────┘   └───────────┘
```

### 连接模型

- edge 启动时主动连接 main 并保持 gRPC 双向流；断线后指数退避重连（1s 起，上限 30s）。
- edge 认证：启动参数携带 server token（`--main` + `--token`），gRPC metadata 鉴权；每台服务器一个独立 token，可单独吊销。
- edge 完全无状态、无本地配置：任务所需的一切（目录、操作序列、payload 变量）由 main 随任务下发。换机重装 = 重跑一条命令。
- TLS：gRPC 走 main 的域名，由 Caddy 反代（h2）终结 TLS，hookploy 自身不管证书。

### 安全模型

核心原则：**edge 只执行固定操作（ops），不执行任意脚本**。

- 操作序列定义在 `hookploy.yaml`（main 侧 SSOT），webhook 调用方只能"按名触发"，payload 无法注入命令（见 §5 插值规则）。
- main → edge 下发的是结构化的 op 列表（`compose.pull`、`compose.up` 等 + 参数），不是 shell 字符串；edge 以 argv 数组方式执行，全程不经过 shell。
- 爆炸半径：service token 泄露 → 只能触发该服务的部署（最坏情况：重启一个服务）；server token 泄露 → 无直接危害（edge 连接是出站的，token 只用于 edge 向 main 自证身份，不能反向下发任务）；main 被攻破 → 等价于所有服务器被攻破，这是中心化架构的固有代价，接受（main 所在机器本就是主力生产机）。
- token 均存储在 main 的 SQLite 中（哈希后存储），由 CLI 管理，明文仅在创建时输出一次。

## 4. 配置：hookploy.yaml

唯一的服务定义 SSOT。存放于 Ansible 所在的 git 仓库，随 Ansible 发布 main 时同步到服务器（见 §9）。**文件中不含任何 secret**——token 在 main 数据库里，服务的 `.env` 维持现有 Ansible 流程。

部署形态只有三种（现有全部服务归入其一）：**形态 1** 标准镜像部署（digest 锁定 + up）；**形态 2** 镜像部署 + 从镜像抽取文件 + 迁移；**形态 3** 镜像（后端）+ 静态 artifact（前端）。

```yaml
# hookploy.yaml — 全部服务定义的 SSOT
listen:
  http: "127.0.0.1:9100"   # webhook + 状态 API，由 Caddy 反代
  grpc: "127.0.0.1:9101"   # edge 接入口，由 Caddy 以 h2 反代

servers:
  ali-hk-01: { local: true }   # main 所在机器，任务走内建 executor
  tc-sg-01: {}
  hh-hk-01: {}

defaults:
  timeout: 10m               # 单次执行超时，可被服务覆盖

services:
  # ─── 形态 1：标准镜像部署 = digest 锁定 + up（linkmind / ideachat / panplayer 同型）───
  linkmind:
    server: ali-hk-01
    dir: /opt/apps/linkmind
    image: ghcr.io/reorx/linkmind
    deploy:
      - image.pin
      - compose.up

  # 形态 1 在 edge 上，完全同型，只是 server 不同
  vocalflow-api:
    server: tc-sg-01
    dir: /opt/apps/vocalflow-rt
    image: ghcr.io/reorx/vocalflow-rt
    deploy:
      - image.pin
      - compose.up

  # 形态 1 + 禁用 webhook（condenser：CD 已拆，仅允许 CLI 手动发版）
  condenser:
    server: hh-hk-01
    dir: /opt/apps/condenser
    image: ghcr.io/reorx/condenser
    webhook: false
    deploy:
      - image.pin
      - compose.up

  # ─── 形态 2：镜像部署 + 抽取文件 + 迁移（breeze）───
  breeze:
    server: ali-hk-01
    dir: /opt/apps/breeze
    image: ghcr.io/reorx/breeze
    deploy:
      - image.pin
      # 从锁定后的镜像抽 /app/static，近原子交换到 ./static
      # （pin 已把本地 :latest 指向锁定镜像，抽到的必然是本次部署的版本）
      - image.extract: { from: /app/static, to: static }
      - compose.run: { service: web, argv: [python, manage.py, migrate, --noinput] }
      - compose.up

  # ─── 形态 3：镜像（后端）+ 静态 artifact（前端）（simul）───
  simul:
    server: ali-hk-01
    dir: /opt/apps/simul
    image: ghcr.io/reorx/simul/server
    deploy:
      # 首次部署 .env 未填时拒绝启动（kagi 会 crash-loop）
      - env.require: { file: .env, keys: [KAGI_EMAIL] }
      - image.pin
      # CI 构建 webdist.tar.gz 上传后，payload 携带 url + sha256
      - artifact.extract:
          url: "${payload.webdist_url}"
          sha256: "${payload.webdist_sha256}"
          to: webdist
      - compose.up
    # 具名任务：不随 webhook 触发，仅 hookploy task simul db-push 手动执行
    # （drizzle db:push 遇破坏性变更会交互确认，绝不能进自动流水线）
    tasks:
      db-push:
        - compose.exec: { service: server, argv: [pnpm, db:push] }
```

服务级字段：`server`（所在服务器）、`dir`（工作目录）、`image`（主镜像声明，供 `image.*` op 复用；未来多镜像场景协议为 `images:` 列表留位，v1 不实现）、`webhook: false`（不接受 `/hooks/` 触发，`hookploy deploy` 手动仍可用）、`deploy`（默认流水线）、`tasks`（具名任务，见下）、`timeout`（覆盖默认超时）。

### Op 词汇表（v1）

步骤语法：字符串 = 无参 op（`- compose.pull`），单键 map = 带参 op（`- compose.up: { force_recreate: true }`）。

| op | 语义 | 参数 |
|---|---|---|
| `image.pin` | digest 锁定部署（模式 op，语义见下） | 无 |
| `image.extract` | 从镜像抽取文件，近原子交换到目录 | `from`（必填）, `to`（必填）, `image`（默认为服务 `image:` 的本地 `:latest`）, `pull: bool` |
| `artifact.extract` | 下载 artifact，校验后解压，近原子交换 | `url`（必填）, `sha256`（必填）, `to`（必填） |
| `compose.pull` | `docker compose pull` | `services: []`（默认全部） |
| `compose.up` | `docker compose up -d` | `force_recreate: bool`, `services: []` |
| `compose.run` | `docker compose run --rm`（起新容器跑一次性命令） | `service`（必填）, `argv: []` |
| `compose.exec` | `docker compose exec -T`（进运行中容器执行） | `service`（必填）, `argv: []` |
| `compose.restart` | `docker compose restart` | `services: []` |
| `env.require` | 断言 env 文件中指定 key 已填值，否则失败 | `file`, `keys: []` |
| `env.write` | 向指定文件写入/更新 KEY=VALUE 行 | `file`, `set: {K: V}` |
| `run` | 在服务目录下执行一条命令（argv 数组，不经 shell） | `argv: []` |

`run` 是逃生舱：用于 op 词汇表未覆盖的场景（如迁移期兼容现有 deploy.sh）。它依然是**配置定义**的（在 SSOT 里、经 git 审计），不破坏"payload 不能注入命令"的安全属性；但新服务应优先用类型化 op。

### 模式 op 语义

**`image.pin`**（零参数，验证内置）。前提：服务声明了 `image:`（缺失则 `validate` 报错）。执行契约：

1. 取 `payload.digest`：存在则校验 `sha256:[0-9a-f]{64}` 格式；缺失（手动触发）则 pull `:latest` 并解析其实际 digest，走同一条路径。
2. `docker pull <image>@<digest>`，3 次重试、间隔 5s（吸收 registry 复制延迟）。
3. `docker tag` 把**本地** `:latest` 指向锁定镜像——compose 文件保持朴素的 `image: <repo>:latest` 不变，本地 tag 即是 pin，后续任何手动 `compose up` 复用的都是最后一次验证过的部署。
4. **注册部署后验证**：流水线中最后一个 `compose.up` 结束后，runtime 自动断言至少一个运行容器的 image ID 等于锁定镜像的 ID，不匹配则整次部署 failed。验证不是显式 op——写了 pin 就必然验证，"忘写 verify 导致白 pin"被协议堵死。

由此 `payload.digest` 是 payload 中唯一有内建语义的**保留字段**，其余 key 全部服务自定义。

**`image.extract`**。`docker create` 临时容器 → `docker cp` 抽出 `from` 路径 → `<to>.new` 近原子交换（rm `to.old` → mv `to`→`to.old` → mv `to.new`→`to` → rm `to.old`）→ 删除临时容器。默认作用于服务 `image:` 锁定后的本地 `:latest`；抽取 compose 之外的镜像时显式传 `image:` 参数并配 `pull: true`。

**`artifact.extract`**。下载 `url`（3 次重试）→ `sha256` 校验（**必填**：artifact 来自公网 URL，无校验等于接受任意代码）→ 按扩展名解压（v1 支持 tar.gz / zip）到 `<to>.new` → 近原子交换（与 `image.extract` 共用同一段交换逻辑）。CI 侧约定：构建产物上传到稳定可下载处（GitHub Release asset 或 OSS），payload 携带 url + sha256。

### 具名任务（tasks）

`deploy` 只是默认流水线；`tasks:` 定义属于该服务、但不随 webhook 触发的具名操作序列（如交互敏感的数据库迁移），仅 `hookploy task <service> <name>` 手动执行。命名刻意避开 "actions"/"jobs"（与 CI 术语混淆）。一次 deploy 或一次 task 的执行在内部统一为 **Execution**，共用状态机、历史与日志。

### 配置生效

- main 启动时加载并校验；`SIGHUP` 或 `hookploy reload` 热重载。
- `hookploy validate [-f hookploy.yaml]`：本地静态校验（schema、服务器引用存在、op 参数合法），供 Ansible 部署前和 AI agent 修改后作为安全网。
- 发布 JSON Schema（`hookploy schema`输出），编辑器与 agent 可用于校验补全。

## 5. 部署语义

### 触发

```
POST /hooks/<service>
Authorization: Bearer <service-token>
Content-Type: application/json

{ "digest": "sha256:..." }        # 任意 JSON，作为 payload
```

- 立即返回 `202 Accepted` + `{ "deploy_id": "dp_xxx", "status_url": "/deploys/dp_xxx" }`，部署异步执行。GitHub Actions 的 curl 一步即绿；需要确认结果时可选地轮询 `status_url`（文档提供现成 snippet）。
- 兼容性：也接受 `X-Deploy-Token` 头，便于各 repo 的 Actions 逐个迁移。

### Payload 插值

- 配置中以 `${payload.<key>}` 引用 payload 字段（点路径支持嵌套）。**必填**：key 缺失则部署直接失败（fail fast）。`${payload.<key>?}` 为**可选**：缺失时得空值，空值语义由 op 自行定义（如 `image.pin` 内部对 digest 的处理：空 = 回退解析 `:latest`）。
- 插值结果只会作为独立的 argv 元素、环境变量值或 op 的结构化参数使用；所有 op 以 argv 数组直接 exec，全程不经过 shell，注入在结构上不可能。
- `payload.digest` 是唯一的保留字段（`image.pin` 隐式消费），其余 key 服务自定义。

### 生命周期与并发

```
queued → dispatching → running → succeeded / failed
                │                      
                ├─ superseded   （被更新的部署顶替）
                └─ unreachable  （edge 离线且重试窗口耗尽）
```

- **同一服务严格串行**；不同服务并行。
- **去重（latest wins）**：某服务已有部署在排队（尚未 running）时，新 webhook 到达 → 旧的标记为 `superseded`，只保留最新一个；已在 running 的部署不中断，跑完后执行队列里最新的那个。连推 N 个 commit 最多执行 2 次部署。
- **edge 离线**：任务进入 dispatching 后若目标 edge 未连接，在 **30 秒**窗口内等待其重连；窗口耗尽标记 `unreachable` 失败。（部署由 CI 触发，CI 可重跑，不做长时间排队。）
- **超时**：默认 10 分钟（`defaults.timeout`，服务可覆盖）；超时由 edge 杀掉进程组并上报 failed。
- **main 重启恢复**：任务状态落 SQLite；main 重启后 running 状态的任务标记为 failed（executor 已丢失），queued 的任务继续调度。

### 日志与历史

- edge 将每个 op 的 stdout/stderr 流式回传，main 按 deploy 落库。
- 部署记录保留最近 **每服务 50 条**，日志随记录一起清理。
- 每条记录含：deploy_id、service、trigger payload、状态、每个 op 的起止时间与退出码、完整日志。

## 6. 状态 API 与 CLI

HTTP API（与 webhook 同端口，Bearer 鉴权使用 admin token）：

```
GET /deploys/<id>              # 单次部署详情（含各 op 状态）
GET /deploys/<id>/logs         # 日志（支持 ?follow=1 流式）
GET /services                  # 服务列表 + 各自最近一次部署状态
GET /services/<name>/deploys   # 某服务部署历史
GET /servers                   # 各 edge 连接状态（online/offline、版本、最近心跳）
```

CLI（AI-native 的主接口，所有查询命令支持 `--json`，输出结构稳定可依赖）：

```
hookploy main -f hookploy.yaml            # 运行 main
hookploy edge --main <url> --token <t>    # 运行 edge（systemd 中常驻）

hookploy status [--json]                  # 总览：servers 在线状态 + services 最近部署
hookploy deploys <service> [--json]       # 部署历史
hookploy logs <deploy-id> [-f]            # 部署日志
hookploy deploy <service> [--payload '{}']# 手动触发部署（等价 webhook，用于人肉/agent 操作）
hookploy task <service> <name>            # 执行具名任务（如 simul 的 db-push）
hookploy validate [-f file]               # 配置静态校验
hookploy schema                           # 输出 hookploy.yaml 的 JSON Schema

hookploy token create <service>           # 创建/轮换/吊销 service token
hookploy token rotate <service>
hookploy token revoke <service>
hookploy server token create <server>     # edge 接入 token
```

远程使用：CLI 通过 `HOOKPLOY_URL` + `HOOKPLOY_ADMIN_TOKEN` 环境变量访问 main 的状态 API，本地开发机和服务器上行为一致。token 管理类命令仅限 main 本机执行（直接操作 SQLite，避免 admin API 拥有发 token 的权力）。

### GitHub Actions 接入示例

```yaml
# repo secrets: HOOKPLOY_TOKEN；org/repo variable: HOOKPLOY_URL
- name: Deploy
  run: |
    curl -fsS -X POST "$HOOKPLOY_URL/hooks/linkmind" \
      -H "Authorization: Bearer $HOOKPLOY_TOKEN" \
      -H "Content-Type: application/json" \
      -d '{"digest": "${{ steps.build.outputs.digest }}"}'
```

## 7. gRPC 协议概要

单一双向流 RPC，edge 为客户端：

```protobuf
service Hookploy {
  rpc Session(stream EdgeMessage) returns (stream MainMessage);
}

// edge → main
message EdgeMessage {
  oneof msg {
    Hello hello;            // server 名、token、binary 版本
    ExecUpdate update;      // execution_id、op 序号、状态、日志块、退出码
  }
}

// main → edge
message MainMessage {
  oneof msg {
    HelloAck ack;
    Execution exec;         // execution_id、kind (deploy|task)、service、dir、
                            // ops[]（已完成插值的结构化 op 列表）、timeout
    CancelExec cancel;      // 预留（v1 不实现取消）
  }
}
```

- 心跳依赖 gRPC/h2 内建 keepalive；main 据此维护 servers 在线状态。
- 版本兼容：Hello 携带 binary 版本，main 检测到 edge 版本落后时在 `hookploy status` 中标注（v1 不做自动升级）。

## 8. 为什么不用现成方案

- **Komodo**（Core/Periphery 架构与本设计同构）：通用的服务器 + 容器管理平台，UI 优先、功能面庞大。我们只需要"中心触发边缘 pull 镜像、重启服务"这一件事，为此运维一整套 Komodo 不成比例。但其协议设计可作实现参考。
- **GitHub self-hosted runner**：runner 常驻太重；更根本的是它迫使部署细节（目标机器、目录、步骤）写进各项目的 Actions 文件——代码仓库与 DevOps 过度耦合，破坏"部署定义集中在一处"的目标。Hookploy 下项目 repo 只知道"通知一个 URL"，其余全在 SSOT。
- **main 直接 SSH 到目标机执行**：无法承载"操作序列由中心配置结构化定义"的模型（SSH 天然是传 shell 字符串），也拿不到结构化的状态回传与日志流；且 main 需要持有全部服务器的 SSH 私钥，爆炸半径更大。

## 9. 与 Ansible 的边界

| 职责 | 归属 |
|---|---|
| 装机：hookploy binary、systemd unit、Caddy 反代配置 | Ansible |
| `hookploy.yaml` 的存放（git，与 ansible 同 repo）与下发（发布 main 时同步） | Ansible |
| 服务的目录结构、docker-compose.yml、.env 骨架 | Ansible（维持现状） |
| 域名 / Caddy 路由 | Ansible（hookploy 不生成任何路由配置） |
| 服务定义（谁在哪台机器、部署操作序列）| **hookploy.yaml** |
| token、部署历史、运行时状态 | hookploy（SQLite） |

替代关系：hookploy 上线后，Ansible 的 `webhook` role（hooks.json、deploy-*.sh 模板、adnanh/webhook 进程）整体退役，`enabled_apps` 与部署动作的联动迁入 `hookploy.yaml`。

## 10. 技术栈与实现约束

- Go 单 binary，无 CGO（SQLite 用 pure-Go 驱动如 modernc.org/sqlite），linux/amd64 + darwin/arm64 交叉编译。
- 存储：单文件 SQLite（`/opt/apps/hookploy/hookploy.db`），WAL 模式。
- 依赖面刻意最小：gRPC + proto、SQLite 驱动、yaml、CLI 框架，无消息队列、无外部数据库。
- 开发方法论：BDD——每个行为（webhook 触发、去重、超时、edge 断连重试、插值失败）先写行为测试再实现。

## 11. 里程碑

- **M1 — 单机可用**：main + 内建 local executor + webhook API + op 引擎 + SQLite 历史 + `status/deploys/logs/validate` CLI。在 ali-hk-01 上替换 adnanh/webhook，现有服务全部迁移。
- **M2 — 主从**：gRPC 协议 + edge 角色 + server token + 离线重试 + 在线状态。tc-sg-01 以 edge 接入，vocalflow-api 的 digest 部署迁移。
- **M3 — 收尾**：`--json` 全覆盖与输出结构冻结、JSON Schema、Ansible role（部署 main/edge）、文档、全部 GitHub Actions 切换、旧 webhook 退役。

## 12. 待确认的开放问题

1. **`run` 逃生舱**是否保留？（本稿保留，理由见 §4；若追求纯粹可去掉，代价是迁移期要把现存脚本逻辑全部翻译成类型化 op。）
2. **日志保留策略**：每服务 50 条是否合适？
3. **状态 API 的 admin token** 与 service token 分离，admin token 只读 + 可触发部署/任务、不可管理 token——权限切分是否够用？

已裁决（2026-07-18 讨论定稿）：digest 锁定为镜像部署默认语义，`image.pin` 零参数、验证内置；静态前端走 CI artifact 而非镜像抽取（`artifact.extract`，sha256 必填）；`compose.run` 与 `compose.exec` 并存；具名任务定名 `tasks`（内部执行统一为 Execution）；去重采用 latest-wins；`webhook: false` 支持手动发版服务；`image:` v1 单值、协议为 `images:` 留位。

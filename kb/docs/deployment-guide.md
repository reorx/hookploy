---
created: 2026-07-19
updated: 2026-09-25
tags:
  - hookploy
  - deployment
  - operations
---

# Hookploy 部署与使用指南

Hookploy 是一个中心化的 webhook 部署调度器：**main** 节点接收 CI（如 GitHub Actions）的 webhook，按 `hookploy.yaml`（单一事实来源，SSOT）里的服务定义，把部署任务分发到目标服务器执行（main 本机走内建 executor，远程服务器走 **edge** 进程的 gRPC 长连接）。一个 Go 单 binary，通过子命令区分角色，无 CGO、无外部依赖，数据存单文件 SQLite。

设计文档见仓库 `docs/PRD.md`；`--json` / HTTP API 的输出契约（M3 起冻结）见 `docs/json-output.md`。本文只讲"怎么用"。

> **关于示例**：文中的服务器名（`prod-01` / `prod-02` / `prod-03`）、服务名（`myapp` / `chatsvc`）、镜像（`ghcr.io/acme/...`）与域名（`hookploy.example.com`）全部是虚构示例，请替换为你自己的环境。反向代理以 Caddy 为例，换成 Nginx / Traefik 等同理。

## 1. 职责边界：hookploy 管什么，不管什么

hookploy 只做一件事：收 webhook → 按服务定义执行部署步骤。装机与周边设施交给你现有的部署手段（Ansible、shell 脚本、手动操作均可，下称"装机工具"）：

| 职责 | 归属 |
|---|---|
| 装机：binary 上传、systemd unit、反向代理配置 | 装机工具 |
| `hookploy.yaml` 的版本管理（建议进 git）与下发 | 装机工具 |
| 服务目录、docker-compose.yml、.env 骨架 | 你现有的部署流程 |
| 域名 / 反代路由 | 反向代理（hookploy 不生成任何路由配置） |
| 服务定义：谁在哪台机、部署步骤序列 | **hookploy.yaml** |
| token、部署历史、运行时状态 | hookploy（SQLite，`hookploy.db`） |

**hookploy.yaml 中不含任何 secret**——token 存在 main 的数据库里，服务的 `.env` 走你现有的流程。改配置的推荐路径永远是：改 git 里的 `hookploy.yaml` → 下发到服务器 → reload（见 §6）。

⚠️ **retag pin 与配置管理工具的相容性（`pull: missing`，2026-07-19 实咬）**：`image.pin` 的"钉死"= 把**本地** `<repo>:latest` tag 指向已验证 digest，compose 保持朴素 `image: <repo>:latest`。这个模型的前提是**没有别的东西重新解析 registry 的 `:latest`**——而 Ansible role 里常见的 `docker_compose_v2` + `pull: always`（或任何例行 `docker compose pull`）恰好会：把本地 tag repoint 到 registry 当前版，紧接着的 recreate 就把服务**静默切到一个从未过流水线的镜像**上。两个失效方向：① registry `:latest` = "CI 最后一次 push"，本地 pin = "最后一次部署成功"——CI push 后部署失败/被 superseded、或刚手动回滚到旧 digest 时，二者不同，always-pull 直接把未验证/已回滚掉的版本推上线；② 绕过整条 deploy 流水线——形态 2 服务的 migrate/extract 不会跑（镜像换了但 DB 没迁移、静态文件没抽），pin 验证和 healthcheck 也不存在。这是 deploy 仓库 2026-07-13 `DEPLOY_IMAGE` 插值陷阱（静默回滚）的镜像版：配置管理重跑时静默**前滚**。**规则：hookploy 管的服务，配置管理侧一律 `pull: missing`**（镜像本地缺失才拉——新机器首次 provisioning 照常，之后永远复用本地 pin）。接管已运行服务时若 registry 尚无 `:latest`（如只有 `:master`），先 `docker tag <旧引用> <repo>:latest` 引导出本地 pin，防拉取空档。

## 2. 构建与产物

```sh
make test                # go test ./...
make dist                # 发布产物：dist/hookploy-<version>-<os>-<arch>.tar.gz + checksums.txt
make build               # 本机平台调试构建：tmp/hookploy
make build-linux-amd64   # 仅裸 binary dist/hookploy-linux-amd64
```

- 版本号默认取 `git describe --tags --always --dirty`，可 `make dist VERSION=v0.x.y` 覆盖；经 `-ldflags -X ...version.Version=` 烧进 binary。main 和 edge 握手时互报版本，`hookploy status` 会给落后的 edge 标注 `(outdated)`。
- 发布 tarball 内含 binary + `hookploy-ctl.sh`（无 systemd 场景与手动运维的兜底控制脚本）；`make dist` 同时保留裸 binary `dist/hookploy-<os>-<arch>`，方便装机工具直接上传。
- 正式 release：push `v*` tag 触发 `.github/workflows/release.yml`，产物与 `make dist` 同形态（tar.gz + checksums.txt），挂到 GitHub Releases；本地 `make dist` 即可复现。
- main 和 edge 是**同一个 binary**，只是运行子命令不同。
- **Web UI 资源全部编译进 binary**（CSS/JS 经 `go:embed`，templ 模板的生成码提交在仓库里），发布产物自包含：部署 main 不需要拷贝任何静态文件目录，部署 edge 也不需要剥离什么——edge 子命令根本不挂载 UI 路由。开发侧改了 `internal/webui/views/*.templ` 后必须跑 `scripts/gentempl.sh` 重新生成并提交；release workflow 会重新生成并比对，生成码过期时发布直接失败。

## 3. 单机部署（只有 main）

main 内建本机执行能力，单进程即可服务它所在机器上的全部服务。

### 3.1 hookploy.yaml 最小示例

```yaml
listen:
  http: "127.0.0.1:9100"   # webhook + 状态 API，由反向代理反代
  grpc: "127.0.0.1:9101"   # edge 接入口（单机形态暂时用不到，但默认会监听）

servers:
  prod-01: { local: true }   # local: true = 任务走 main 内建 executor

services:
  myapp:
    server: prod-01
    dir: /opt/apps/myapp
    image: ghcr.io/acme/myapp
    deploy:
      - image.pin        # 按 payload.digest 锁定镜像并验证
      - compose.up
```

### 3.2 启动

```sh
hookploy main -f /opt/apps/hookploy/hookploy.yaml
```

进程管理二选一，**勿并用**：

- systemd unit（推荐，`Restart=always`，`ExecReload=/bin/kill -HUP $MAINPID` 支持热重载）
- `hookploy-ctl.sh start`（PID 文件方式，只控制自己目录里的实例）

### 3.3 反向代理路由

单机形态只需要反代 HTTP（Caddy 示例）：

```
hookploy.example.com {
    reverse_proxy 127.0.0.1:9100
}
```

HTTP 口上的所有路径（`/hooks/*`、admin API、`/ui/`、`/github/webhook`）同 listener，整口反代即可；其中 `/github/webhook`（§3.6，可选）需要 GitHub 能从公网访问到。

### 3.4 Token 初始化（main 本机执行，直接操作 SQLite）

```sh
cd /opt/apps/hookploy
./hookploy token create <service> -f hookploy.yaml   # service token（hpt_），给 GitHub Actions
./hookploy admin-token create -f hookploy.yaml       # admin token（hpa_），给状态 API / CLI
```

明文只在创建时输出一次，库里只存哈希。轮换/吊销：`token rotate` / `token revoke`。

### 3.5 GitHub Actions 侧接入

```yaml
# repo secret: HOOKPLOY_TOKEN；org/repo variable: HOOKPLOY_URL
- name: Deploy
  run: |
    curl -fsS -X POST "$HOOKPLOY_URL/hooks/myapp" \
      -H "Authorization: Bearer $HOOKPLOY_TOKEN" \
      -H "Content-Type: application/json" \
      -d '{"digest": "${{ steps.build.outputs.digest }}"}'
```

立即返回 202 + `deploy_id`，部署异步执行；需要等结果时轮询 `GET /deploys/<id>`。

### 3.6 GitHub workflow_run webhook（构建状态回传，可选）

与 §3.5 的方向相反：让 GitHub 把 Actions 构建状态**推给** hookploy，Web UI 便能展示各服务的构建情况（dashboard 进行中构建、`/ui/actions` 构建列表、服务详情页近期构建）。不需要 GitHub token、不轮询 GitHub API。

配置两处：

```yaml
github:
  webhook_secret: "<随机字符串>"   # HMAC 校验密钥；未配置时端点关闭（404）
services:
  myapp:
    github_repo: reorx/myapp       # 把该 repo 的构建关联到本服务
```

GitHub 侧（repo Settings → Webhooks → Add webhook）：

- Payload URL：`https://hookploy.example.com/github/webhook`
- Content type：`application/json`
- Secret：与 `github.webhook_secret` 一致
- 事件：选 "Let me select individual events"，只勾 **Workflow runs**

说明：

- 每次投递用 `X-Hub-Signature-256` HMAC 校验，签名不对返回 401 不落库；`ping` 与其他事件类型一律返回 200（GitHub 侧不会标红），但只有 `workflow_run` 会被记录。
- 同一 run 的多次投递（requested → in_progress → completed）按 run id 就地更新，乱序投递不会把状态改回去。
- 每个 repo 保留最近 200 条构建记录，自动清理。
- 多个 repo 可以指向不同服务；未被任何服务 `github_repo` 引用的 repo 事件也会保存，在 `/ui/actions` 可见（service 列为空）。
- 两个 key 都支持热 reload（§6），改完 `-/reload` 即生效。

### 3.7 通知（可选）

把部署失败、以及节点的上下线推到一个外部通道。第一阶段只有 Telegram Bot 一种后端；`provider` 同一时刻只有一个生效。**省略整个 `notify:` 块 = 不发通知**（和 `github.webhook_secret` 一样，未配置即关闭）。

```yaml
notify:
  provider: telegram                    # 省略 = 关闭；另一个保留值是 center（见文末）
  base_url: https://hookploy.example.com  # 可选：消息里附 /ui/deploys/<id> 链接，留空则不带
  # 可选。下面这行就是默认值：部署失败 + 全部节点事件
  events: [deploy.failed, main.started, edge.offline, edge.online]
  edge_offline_after: 5m                # 可选，这就是默认值；edge 断连多久算离线
  telegram:
    bot_token: "123456789:AA..."        # 明文，用时现读，不会出现在任何 API / Web UI / --json 输出里
    chat_id: "-1001234567890"

services:
  noisy-cron:
    notify: { enabled: false }          # 本服务完全静音（只影响 deploy.*，管不着节点事件）
  payments:
    notify:
      events: [deploy.failed, deploy.recovered]   # 替换全局列表，不是追加
```

拿 Telegram 凭据：找 [@BotFather](https://t.me/BotFather) `/newbot` 拿 `bot_token`；把 bot 拉进目标频道/群并给发消息权限，然后向频道发一条消息、访问 `https://api.telegram.org/bot<token>/getUpdates` 从返回里读 `chat.id`（频道 id 通常是 `-100` 开头的负数）。

事件词汇表分两类作用域，写错事件名在 `hookploy validate` 阶段就报错。

**deploy 作用域**——属于某个服务，可被服务级 `notify.events` 覆盖：

| 事件 | 何时触发 |
|---|---|
| `deploy.failed` | 部署以失败告终（**默认开启**）。含 op 失败、超时、rollout 中途 abort |
| `deploy.unreachable` | 目标服务器全程联系不上，main 从未得知结果——刻意与 failed 区分，别把它当成"部署坏了" |
| `deploy.succeeded` | 部署成功 |
| `deploy.recovered` | 本次成功，而同一服务上一次是失败/unreachable。它**替代** `deploy.succeeded`，所以只订阅 succeeded 的服务不会收到恢复通知，反之亦然 |

**节点作用域**——属于整个部署而非某个服务，三个**默认全开**，只认顶层 `notify.events`。在服务级 `notify.events` 里写它们会**加载失败**（服务对"main 重启了没有"没有发言权）：

| 事件 | 何时触发 |
|---|---|
| `main.started` | main 进程启动，两个监听端口都绑定成功之后。消息带版本号 |
| `edge.offline` | 某个非 `local` 的 server 断连超过 `edge_offline_after`。消息带节点名、已离线时长、最后已知版本 |
| `edge.online` | **发过 offline 告警的**节点重新连上。消息带节点名、本次离线总时长、回来时的版本 |

说明：

- **一次部署一条消息**，正文列出每个未成功的实例及其错误，不是每个实例一条。
- **失败但不通知的两种情况**：整个 rollout 被取消（只发生在 main 关机时取消未派发的波次，不是事故）、部署被更新的一次取代（superseded，它根本没跑）。
- **重启不刷屏**：main 重启时收尾上个进程遗留的部署会判为失败，但刻意不发通知。
- **一次离线一条消息**：edge 掉线多久都只发一条 `edge.offline`，回来时配一条 `edge.online`；没跨过阈值的抖动两条都不发。
- **`edge_offline_after` 要大于 60s**：edge 断流后 main 会保留在途执行 60s 等它重连（§4），这是常态而非事故。阈值设得比它还短会把正常的流断也报成告警。
- **覆盖"从来没连上"的情况**：判定靠周期巡检（每 30s 比对 `servers:` 与当前在线的 edge），不是靠断连回调。所以 main 重启后一直没起来的 edge 同样会告警，计时从 main 启动算起——这种情况消息里不会有版本号，因为本进程从没见过它。`local: true` 的 server 不参与巡检。
- **投递是尽力而为**：内存有界队列（满了丢最旧）+ 有限次退避重试，失败记日志放弃，不落库、进程重启会丢。通知从不阻塞部署，也从不影响 webhook 响应速度。
- `notify:` 整块支持热 reload（§6），改完 `-/reload` 即生效，包括轮换 bot_token 和调整 `edge_offline_after`（对正在进行中的离线也立刻生效）。
- `provider: telegram` 却缺 `bot_token`/`chat_id` 会**加载失败**——配了却哑掉比明确关闭更糟。
- `provider: center`（统一通知中心）是为第二阶段保留的值：schema 已接受，但当前会在 `hookploy validate` 报"not implemented yet"。

## 4. 多机部署（main + edge）

### 4.1 模型

- edge 是**无状态执行器**：主动外连 main 并保持长连接，接任务、本机执行 `docker compose` 操作、流式回传日志。断线后指数退避重连（1s 起、上限 30s）。
- edge **零入站端口、零域名、零证书、零本地配置**——任务所需的一切（目录、步骤、参数）由 main 随任务下发。
- edge 的身份由 server token 决定（token 的 subject 就是 server 名），启动命令只要 `--main` + `--token`。

**两条通路（`--transport`，默认 `grpc`）**：

| | `grpc` | `sse` |
|---|---|---|
| 端口 | 专用 gRPC 口（9101） | 与 HTTP API 同口（9100） |
| 反代 | 需要 h2/gRPC 分流；CF 橙云需开 zone 的 gRPC 开关 | 普通 HTTP 反代即可，**CF 无需任何开关** |
| 保活 | 传输层 keepalive 30s/10s | 45s 心跳注释帧（真实流量，代理都认） |
| 流断容忍 | 无：连接断 = 执行判失败 | **有**：执行照常跑完，重连后补报结果 |

两条通路同时可用、共享同一套内部逻辑，一台 edge 只选一条。**域名在 Cloudflare 橙云后面就选 `sse`**——CF 对所有代理流量有约 100s 空闲超时且不认 h2 PING，gRPC 长连接每天会断上千次，落在发版窗口就会把实际成功的部署记成 failed。gRPC 通路保留兼容，后续逐步弃用。

**流断容忍语义（仅 `sse` 通路完整生效）**：

- 连接断开时，main 为该 server 已下发的执行保留 **60s 宽限**：不判失败，等 edge 回来。
- edge 重连时会声明自己还在跑哪些执行；main 认领它们，收到补报的结果后正常结算（成功就是成功）。
- edge 重连但没声明某执行（例如 edge 进程重启过） → 该执行立即判 `failed`，不空等宽限。
- 宽限内没回来 → 判 `unreachable`（不是 `failed`）：main 从未得知结果，不该替它下结论。CI 重跑即可。
- 中间日志不重放，只补报最终结果；执行本身的 `timeout` 仍是兜底。

### 4.2 新服务器接入清单

1. **main 侧改配置**：`hookploy.yaml` 的 `servers:` 加一行（非 local）：

   ```yaml
   servers:
     prod-01: { local: true }
     prod-02: {}          # 新 edge
   ```

   下发后 reload（见 §6）。未在 `servers:` 里声明的机器即使 token 有效也会被拒连。

2. **main 本机签发 server token**：

   ```sh
   ./hookploy server token create prod-02 -f hookploy.yaml   # 输出 hps_ 开头的 token
   ```

3. **edge 机器**：上传 binary + 一条启动命令：

   ```sh
   hookploy edge --main https://hookploy.example.com --token hps_xxx
   # 走 CF 橙云 / 想要流断容忍：
   hookploy edge --main https://hookploy.example.com --token hps_xxx --transport sse
   ```

   - token 也可用环境变量 `HOOKPLOY_SERVER_TOKEN` 传（systemd unit 里配 `EnvironmentFile` 更合适）。
   - `--transport` 也可用 `HOOKPLOY_TRANSPORT` 传；默认 `grpc`。两条通路**同一个 `--main` URL**，不做自动探测也不自动回退（静默切换只会掩盖配置错误）。对着旧版 main 用 `--transport sse` 会拿到 404，edge 日志会明确提示并持续退避重试。
   - `--server <name>` 可选，仅作身份断言（名字来自 token，不填也行）。
   - `https://` URL 走 TLS（由 main 侧反向代理终结），`http://` 为明文（仅限内网/本机测试）。

4. **验证**：`hookploy status` 应显示该 server `online` + 版本 + 连接时长。

换机重装 = 重跑第 3 步，无需任何迁移。吊销一台机器：`servers:` 删掉 + revoke 其 server token。

### 4.3 反向代理侧

**`--transport sse`（推荐）**：edge 只用普通 HTTP，反代不需要任何特殊配置——单条 `reverse_proxy 127.0.0.1:9100` 就够，gRPC 口可以完全不暴露。唯一要求是反代**不缓冲响应**（hookploy 已发 `X-Accel-Buffering: no`，nginx 需要额外确认 `proxy_buffering off`）。CF 橙云下无需开任何开关。

**`--transport grpc`**：反向代理需要以 h2 反代 gRPC 口（Caddy 示例，HTTP 与 gRPC 共用一个域名）：

```
hookploy.example.com {
    @grpc protocol grpc
    reverse_proxy @grpc h2c://127.0.0.1:9101
    reverse_proxy 127.0.0.1:9100
}
```

（hookploy 自身不管证书；gRPC keepalive 双向探活，死连接约 40s 内检出。）

⚠️ **域名走 Cloudflare 代理（橙云）时，gRPC 依赖 zone 的 Network → gRPC 开关**（改用 `--transport sse` 可以整段绕开本条）：开关不开，edge 的 handshake 一律报 `Internal: server closed the stream without sending trailers`，请求根本到不了源站反代——这不是反代配置问题，**edge 排 offline 先查这个开关**。开关打开后，单域名 443 同时承载 HTTP 与 gRPC 即可打通（握手、任务分发、日志流均正常）。开不了开关时的绕行：**明文 h2c 直连源站 IP 上一个已放行的端口**——`edge --main http://<源站IP>:<port>`，该端口的 Caddy 监听须声明 `servers :<port> { protocols h1 h2c }`（全局选项）再用 `@grpc protocol grpc` 分流到 gRPC 口；此时 server token 会明文过网线，仅建议临时或内网使用。

### 4.4 edge 的进程管理

- systemd（推荐）：`Restart=always` 即可，edge 自带重连，进程本身极少退出；token 用 `EnvironmentFile` 注入 `HOOKPLOY_SERVER_TOKEN`。
- 无 systemd 兜底：`hookploy-ctl.sh` 的 edge 命令组（`edge-start / edge-stop / edge-status / edge-restart / edge-logs [-f]`），需要同目录两个 dotfile（0600）：
  - `.edge_main` — main 的 URL
  - `.server_token` — server token

### 4.5 多实例服务与 rollout

一个服务部署到多台机器时，用 `instances` + `rollout` 波次（示例服务 `chatsvc`）：

```yaml
chatsvc:
  image: ghcr.io/acme/chatsvc
  dir: /opt/apps/chatsvc            # 实例默认 dir，可被实例覆盖
  deploy:                           # 所有实例共用同一条流水线
    - image.pin
    - compose.up
    - healthcheck: { url: "http://127.0.0.1:8080/healthz" }
  instances:
    main:  { server: prod-01 }
    api-1: { server: prod-02 }
    api-2: { server: prod-03 }
  rollout:
    - main              # 波 1：单实例
    - [api-1, api-2]    # 波 2：并行，波 1 全部成功后才开始
```

语义要点：

- 一次 webhook = 一次 rollout；digest 在 rollout 层解析一次，**全部实例 pin 同一镜像**。
- 波间门控：任一实例失败 → 后续波取消，rollout failed；已完成的波不回滚，但逐实例 ✓/✗ 在 `GET /deploys/<id>` 里一等可见。
- `rollout` 省略 = 按 `instances` 声明顺序逐实例串行。单机服务的 `server: xxx` 写法就是"单实例单波"的语法糖。
- 目标 edge 离线时等 30s 重连窗口，窗口耗尽标记 `unreachable`（CI 可重跑）。

### 4.6 步骤定向（`on:`）与多节点 migration

同一条流水线在每个实例上各跑一遍，migration 这类步骤只该跑一次。在 step 上加保留键 `on:`，值是 **instance 名**（不是 server 名），单个名或列表：

```yaml
chatsvc:
  image: ghcr.io/acme/chatsvc
  dir: /opt/apps/chatsvc
  deploy:
    - image.pin
    - compose.run: { service: api, argv: [alembic, upgrade, head] }
      on: [main]                    # 只在 main 上执行；其余实例根本没有这一步
    - compose.up
    - healthcheck: { url: "http://127.0.0.1:8080/healthz" }
  instances:
    main:  { server: prod-01 }
    api-1: { server: prod-02 }
    api-2: { server: prod-03 }
  rollout:
    - main
    - [api-1, api-2]
```

要点：

- 无参 op 要带 `on:` 时写成空值 map：`- image.pin:` 换行 `  on: [main]`。
- 名字必须是本服务声明过的实例，且不能把任何实例的流水线过滤成空——两者都在加载期报错（`hookploy validate` 先跑一遍再 reload）。
- `tasks:` 不接受 `on:`：任务用 `hookploy task <svc> <name> --instance <inst>` 选目标。
- 过滤在入队时完成，每个 execution 的 ops 快照只含它真正要跑的步骤，日志与部署详情里看到的就是各节点的真实流水线。

**推荐的多节点 migration 模式**：

1. migration 步骤定向到第一波的中心节点（上例 `main`），排在 `compose.up` **之前**——它失败则流水线停在启动新版本之前，部署 failed 并发通知，后续波次一台不动，全线仍跑旧版本。
2. 迁移必须遵守 expand-contract：波 1 跑完后波 2 的旧容器还在跑，所以每次迁移都要兼容"旧代码 + 新库结构"这一中间态（加列先可空、删列分两次发布）。
3. 应用侧关掉"启动时自动迁移"，改成启动时只检查（如 Django `migrate --check`、Alembic 比对 head）——迁移由流水线负责，应用只负责拒绝在错误的库结构上启动。
4. 超长迁移（大表回填）不进部署流水线，走 `tasks:` 手动触发，避免把 deploy 超时拖满。

### 4.7 步骤超时与重试（`timeout:` / `retries:`）

execution 级 `timeout`（`defaults.timeout` / 服务 `timeout`）是整条流水线的总预算。registry CDN 偶发的"单个 blob 卡在半截"会让一次 `docker pull` 无限等下去，独吞整份预算，而换条新连接通常几秒就好。给容易卡住的步骤加修饰符，写在 op 名旁边（与 `on:` 同级缩进）：

```yaml
deploy:
  - image.pin:
    timeout: 3m     # 每次尝试最多 3 分钟，到点杀掉进程组
    retries: 2      # 失败/超时后最多再来 2 次，每次全新执行，间隔 5s
  - compose.up
  - healthcheck: { url: "http://127.0.0.1:8080/healthz", attempts: 10 }
```

- `timeout` 所有 op 都能用；`retries` 只对 `image.pin`、`compose.pull`、`artifact.extract` 开放（从头再跑必然安全），写在别的 op 上 `validate` 报错。`image.pin` / `artifact.extract` 不写 `retries` 时默认 2。
- `timeout × 尝试次数 + 5s × 重试次数` 必须不超过服务 `timeout`，否则加载期报错。取值参考：127 MB 镜像正常拉取 7–40 秒，`timeout: 3m` + `retries: 2` 最坏 9m10s，落在默认 10m 之内。
- 修饰符写成 op 参数（缩进进了 op 下面）会被指出来：`"timeout" is a step modifier, not an arg`。
- 重试过程在部署日志里可见（`image.pin attempt 2/3`），失败时错误带齐每次原因：`failed after 3 attempts: [1/3] timed out after 3m0s; …`。
- **edge 版本要求**：修饰符由执行步骤的 edge 负责，edge < v0.7.0 会静默忽略它们。main 派发时发现目标 edge 过旧，直接判该 execution failed（`edge "x" runs hookploy v0.6.0, too old for …; upgrade the edge first`），不会悄悄按老语义跑。`healthcheck` 的 `attempts` 不是默认值 5 时同样受此约束。

**v0.7.0 升级注意（`healthcheck.retries` 更名为 `attempts`）**：旧键是硬切的——新 main 加载含 `healthcheck: { retries: … }` 的配置直接报错，老 main 也不认识 `attempts`，所以**配置改名与 main 升级必须一起下发**（改完配置后若老 main 先 reload，会失败但保留旧配置继续跑，无害；随后换新 binary 重启即可）。推荐顺序：

1. **先升级全部 edge**：新 edge 能解码老 main 下发的快照（老快照里的 `healthcheck.retries` 会被当作 `attempts` 读），这一步零风险。
2. 再把配置里的 `retries:` 改成 `attempts:`，与 main 升级一起下发、重启 main。下发前**用新 binary** 对改好的配置跑一次 `validate`（老 binary 不认 `attempts`，会报错）。如果部署脚本是"先换 binary、再校验配置"的顺序，校验失败时磁盘上就剩下新 binary 配旧配置，main 下次重启会起不来，所以这次预检不能省。
3. 最后再给需要的步骤加 `timeout:` / `retries:`。

若沿用"先 main 后 edge"，则在 edge 升级完成前，凡是 `attempts` ≠ 5 或带修饰符的服务，派往老 edge 的部署都会被上述版本门控拒绝。

## 5. 日常运维

CLI 远程使用（本地开发机和服务器行为一致）：

```sh
export HOOKPLOY_URL=https://hookploy.example.com
export HOOKPLOY_ADMIN_TOKEN=hpa_xxx

hookploy status              # servers 在线状态（版本/连接时长）+ 各服务最近部署
hookploy deploys <service>   # 部署历史（每服务保留 50 条）
hookploy logs <deploy-id> -f # 跟踪部署日志直到结束
hookploy deploy <service> [--payload '{}']  # 手动触发（等价 webhook）
hookploy task <service> <name> [--instance <i>]  # 具名任务（不随 webhook 触发）
hookploy version             # binary 版本号（--version / -v 同义）
```

**`--json` 全覆盖，输出结构 M3 起冻结**——CLI `--json` 与 HTTP API 序列化同一批 DTO（`internal/api`），输出逐字节同型；字段只增不改、新增必可选，脚本与 agent 可安全依赖。`logs --json` 是 NDJSON 流（每行一帧，`-f` 结束时多发一个 `done` 终止帧）。完整字段表与冻结规则见仓库 `docs/json-output.md`。

token 管理命令（`token` / `server token` / `admin-token`，均支持 `--json`）仅限 main 本机执行。

### Web UI

main 内置只读 Web 界面：浏览器访问 `https://hookploy.example.com/ui/`（根路径 `/` 自动跳转），用 admin token 登录。登录后种下 HttpOnly 会话 cookie（7 天有效，main 重启失效需重新登录）；该 cookie 只对 GET 类端点生效，所有触发/reload 操作仍必须 Bearer token——UI 本身不提供任何写操作。

页面结构：Dashboard（进行中部署卡片含实时日志尾部、进行中的 GitHub Actions 构建——仅在有构建时出现、服务器清单——在线状态/版本/edge 连接时长、服务清单、近期发布——被去重的 superseded 触发也在列）→ Actions（`/ui/actions`：近期构建列表，可按 service 过滤，需 §3.6 的 workflow_run webhook 接入）→ 服务详情（rollout×实例拓扑、deploy/tasks 流水线定义、近期构建——声明了 `github_repo` 的服务、历史）→ 部署详情（按波次的执行时间线、op 耗时与退出码、日志查看器：实时跟随、按实例过滤、op 行点击定位日志）。顶栏平时保持安静，仅当有服务器离线时显示红色警示徽章。

**安全注意**：`/ui` 与 admin API 同 listener 同鉴权，把 UI 暴露公网等于暴露 admin API。建议仅内网访问，或反代层加护（IP 白名单 / basic auth / VPN）。

**部署要点**：

- UI 资源全部编译在 binary 里（见 §2），部署带 Web UI 的 main 没有任何额外步骤——按 §3.2 启动、按 §3.3 反代整个 HTTP 口即可，`/ui/` 与 API 同端口同域名，不需要单独 serve 静态文件，也没有 WebSocket（实时日志走同源 NDJSON 流 + 片段轮询）。
- 不想暴露 UI 时，`hookploy.yaml` 顶层加 `webui: false`（默认 `true`）：`/ui/` 路由与根路径跳转完全不注册（404），会话 cookie 机制也不启用，admin API 回到纯 Bearer token。该开关在启动时定型，改动需**重启 main** 生效（`-/reload` / SIGHUP 热重载不改变路由挂载）。
- edge 节点与 UI 无关：edge 子命令不挂载任何 UI 路由，无需任何配置。

## 6. 配置变更流程

1. 改 git 里的 `hookploy.yaml`
2. `hookploy validate -f hookploy.yaml` 静态校验（schema、服务器引用、op 参数）——**下发前必跑**（适合放进 CI 或部署脚本）
3. 下发到服务器后热重载，三选一：
   - `curl -X POST https://.../-/reload -H "Authorization: Bearer $HOOKPLOY_ADMIN_TOKEN"`
   - `kill -HUP <main pid>`（systemd: `systemctl reload hookploy`，unit 里配 `ExecReload=/bin/kill -HUP $MAINPID`）
   - 重启进程（会中断 in-flight 部署：running 的标记 failed，queued 的恢复调度）

reload 失败时 main 保留旧配置继续运行；in-flight 的执行永远用入队时的快照，不受 reload 影响。

### 编辑器集成：`hookploy schema`

`hookploy schema` 输出 `hookploy.yaml` 的 JSON Schema（draft-07）。生成一份放配置旁边，文件头加 yaml-language-server modeline，编辑器（VS Code YAML 扩展、Neovim LSP）与 agent 即得补全、悬停文档与实时校验：

```sh
hookploy schema > .hookploy-schema.json
```

```yaml
# yaml-language-server: $schema=./.hookploy-schema.json
listen:
  http: "127.0.0.1:9100"
```

注意分层：schema 是**宽松上界**（字段名、类型、op 参数、互斥结构），不做跨字段语义校验（服务器引用是否存在、rollout 是否恰好覆盖全部实例等）；schema 通过 ≠ 配置合法，**最终以 `hookploy validate` 为准**。

## 7. 排障速查

| 现象 | 排查 |
|---|---|
| server 显示 offline | edge 进程在不在（`systemctl status` / `edge-status`）；edge 日志有无 `handshake rejected`（token 被吊销 / server 未在 yaml 声明）；`--transport grpc` 时反代 gRPC 路由是否 h2、CF zone 的 gRPC 开关是否打开（§4.3）；`--transport sse` 时日志有无 404 提示（main 版本过旧） |
| edge 每天断连上千次 | 域名在 CF 橙云后面且用 `--transport grpc`。改用 `--transport sse`（§4.1）；这是根治手段，不是绕行 |
| 部署 `unreachable` | 目标 edge 离线超 30s 分派窗口，或执行中途 edge 失联超 60s 宽限。main 从未得知结果，edge 恢复后 CI 重跑即可 |
| 部署 `failed`，error 带 "edge disconnected" / "reconnected without execution" | 执行中途断连且 edge 没能保住该执行（`grpc` 通路一律如此；`sse` 通路则说明 edge 进程重启过）。看 edge 日志确认 |
| status 里版本标 `(outdated)` | edge binary 落后于 main，按 §2 重新构建分发（先停进程再覆盖，否则 text file busy） |
| webhook 401/403 | service token 错误或被轮换；`webhook: false` 的服务只接受 CLI 手动触发 |
| 部署被顶掉（`superseded`） | 正常：同服务排队时 latest-wins，连推 N 个 commit 最多执行 2 次部署 |
| 部署 `failed`，error 带 "too old for op … upgrade the edge first" | 该服务用了 step 修饰符（或 `healthcheck.attempts` ≠ 5），目标 edge < v0.7.0 会静默忽略它们，main 拒绝派发。升级该 edge（§4.7） |
| `validate` 报 "healthcheck … "retries" was renamed to "attempts"" | v0.7.0 起 `healthcheck.retries` 更名为 `attempts`（值不变，它本来就是总次数）；改名后下发 |

## 附：op 词汇表速查

`image.pin`（digest 锁定+内置验证）、`image.extract`（从镜像抽文件近原子交换）、`artifact.extract`（下载+sha256 校验+解压交换）、`compose.pull` / `compose.up` / `compose.run` / `compose.exec` / `compose.restart`、`env.require` / `env.write`、`healthcheck`（轮询 HTTP 直到健康）、`run`（argv 逃生舱，不经 shell）。

任一 step 都可加保留键 `on:`（instance 名，单个或列表）把它限定到部分实例，仅 `deploy` 可用，见 §4.6；加 `timeout:`（每次尝试的超时）/ `retries:`（仅 `image.pin` / `compose.pull` / `artifact.extract`）见 §4.7。`healthcheck` 的轮询次数参数是 `attempts`（v0.7.0 前叫 `retries`）。

完整参数见 `docs/PRD.md` §4；机器可读的参数定义在 `hookploy schema` 输出里（op 词汇表演进时 schema 随之更新）。

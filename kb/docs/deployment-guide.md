---
created: 2026-07-19
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

## 4. 多机部署（main + edge）

### 4.1 模型

- edge 是**无状态执行器**：主动外连 main 的 gRPC 口并保持长连接，接任务、本机执行 `docker compose` 操作、流式回传日志。断线后指数退避重连（1s 起、上限 30s）。
- edge **零入站端口、零域名、零证书、零本地配置**——任务所需的一切（目录、步骤、参数）由 main 随任务下发。
- edge 的身份由 server token 决定（token 的 subject 就是 server 名），启动命令只要 `--main` + `--token`。

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
   ```

   - token 也可用环境变量 `HOOKPLOY_SERVER_TOKEN` 传（systemd unit 里配 `EnvironmentFile` 更合适）。
   - `--server <name>` 可选，仅作身份断言（名字来自 token，不填也行）。
   - `https://` URL 走 TLS（由 main 侧反向代理终结），`http://` 为明文（仅限内网/本机测试）。

4. **验证**：`hookploy status` 应显示该 server `online` + 版本 + 连接时长。

换机重装 = 重跑第 3 步，无需任何迁移。吊销一台机器：`servers:` 删掉 + revoke 其 server token。

### 4.3 反向代理侧（多机需要暴露 gRPC）

edge 从公网连 main 时，反向代理需要以 h2 反代 gRPC 口（Caddy 示例，HTTP 与 gRPC 共用一个域名）：

```
hookploy.example.com {
    @grpc protocol grpc
    reverse_proxy @grpc h2c://127.0.0.1:9101
    reverse_proxy 127.0.0.1:9100
}
```

（hookploy 自身不管证书；gRPC keepalive 双向探活，死连接约 40s 内检出。）

⚠️ **域名走 Cloudflare 代理（橙云）时，gRPC 依赖 zone 的 Network → gRPC 开关**：开关不开，edge 的 handshake 一律报 `Internal: server closed the stream without sending trailers`，请求根本到不了源站反代——这不是反代配置问题，**edge 排 offline 先查这个开关**。开关打开后，单域名 443 同时承载 HTTP 与 gRPC 即可打通（握手、任务分发、日志流均正常）。开不了开关时的绕行：**明文 h2c 直连源站 IP 上一个已放行的端口**——`edge --main http://<源站IP>:<port>`，该端口的 Caddy 监听须声明 `servers :<port> { protocols h1 h2c }`（全局选项）再用 `@grpc protocol grpc` 分流到 gRPC 口；此时 server token 会明文过网线，仅建议临时或内网使用。

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

页面结构：Dashboard（进行中部署卡片含实时日志尾部、服务器清单——在线状态/版本/edge 连接时长、服务清单、近期发布——被去重的 superseded 触发也在列）→ 服务详情（rollout×实例拓扑、deploy/tasks 流水线定义、历史）→ 部署详情（按波次的执行时间线、op 耗时与退出码、日志查看器：实时跟随、按实例过滤、op 行点击定位日志）。顶栏平时保持安静，仅当有服务器离线时显示红色警示徽章。

**安全注意**：`/ui` 与 admin API 同 listener 同鉴权，把 UI 暴露公网等于暴露 admin API。建议仅内网访问，或反代层加护（IP 白名单 / basic auth / VPN）。

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
| server 显示 offline | edge 进程在不在（`systemctl status` / `edge-status`）；edge 日志有无 `handshake rejected`（token 被吊销 / server 未在 yaml 声明）；反代 gRPC 路由是否 h2；域名走 Cloudflare 时 zone 的 gRPC 开关是否打开（§4.3） |
| 部署 `unreachable` | 目标 edge 离线超 30s 窗口。edge 恢复后 CI 重跑即可 |
| 部署 `failed`，error 带 "edge disconnected" | 执行中途连接断开；main 侧记为失败，edge 侧会同时取消本地执行。看 edge 日志确认 |
| status 里版本标 `(outdated)` | edge binary 落后于 main，按 §2 重新构建分发（先停进程再覆盖，否则 text file busy） |
| webhook 401/403 | service token 错误或被轮换；`webhook: false` 的服务只接受 CLI 手动触发 |
| 部署被顶掉（`superseded`） | 正常：同服务排队时 latest-wins，连推 N 个 commit 最多执行 2 次部署 |

## 附：op 词汇表速查

`image.pin`（digest 锁定+内置验证）、`image.extract`（从镜像抽文件近原子交换）、`artifact.extract`（下载+sha256 校验+解压交换）、`compose.pull` / `compose.up` / `compose.run` / `compose.exec` / `compose.restart`、`env.require` / `env.write`、`healthcheck`（轮询 HTTP 直到健康）、`run`（argv 逃生舱，不经 shell）。完整参数见 `docs/PRD.md` §4；机器可读的参数定义在 `hookploy schema` 输出里（op 词汇表演进时 schema 随之更新）。

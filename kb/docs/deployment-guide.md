---
created: 2026-07-19
tags:
  - hookploy
  - deployment
  - ansible
  - operations
---

# Hookploy 部署与使用指南（面向 Ansible 维护者）

Hookploy 是一个中心化的 webhook 部署调度器：**main** 节点接收 GitHub Actions 的 webhook，按 `hookploy.yaml`（SSOT）里的服务定义，把部署任务分发到目标服务器执行（本机走内建 executor，远程服务器走 **edge** 进程的 gRPC 长连接）。一个 Go 单 binary，通过子命令区分角色，无 CGO、无外部依赖，数据存单文件 SQLite。

设计文档见仓库 `docs/PRD.md`。本文只讲"怎么用"。

## 1. 职责边界：Ansible 管什么，hookploy 管什么

| 职责 | 归属 |
|---|---|
| 装机：binary 上传、systemd unit、Caddy 反代 | **Ansible** |
| `hookploy.yaml` 的存放（与 ansible 同 git repo）与下发 | **Ansible**（发布 main 时同步） |
| 服务目录、docker-compose.yml、.env 骨架 | **Ansible**（维持现状） |
| 域名 / Caddy 路由 | **Ansible**（hookploy 不生成任何路由配置） |
| 服务定义：谁在哪台机、部署步骤序列 | **hookploy.yaml** |
| token、部署历史、运行时状态 | hookploy（SQLite，`hookploy.db`） |

**hookploy.yaml 中不含任何 secret**——token 存在 main 的数据库里，服务的 `.env` 走现有 Ansible 流程。改配置永远是：改 git 里的 `hookploy.yaml` → 随 playbook 下发 → reload（见 §6）。

## 2. 构建与产物

```sh
go test ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
  -ldflags="-s -w -X github.com/reorx/hookploy/internal/version.Version=<版本号>" \
  -o dist/hookploy-linux-amd64 ./cmd/hookploy
```

- `-X ...version.Version=` 把版本号烧进 binary；main 和 edge 握手时互报版本，`hookploy status` 会给落后的 edge 标注 `(outdated)`。
- 发布产物 = binary + `scripts/hookploy-ctl.sh`（无 systemd 场景与手动运维的兜底控制脚本）。
- main 和 edge 是**同一个 binary**，只是运行子命令不同。

## 3. 单机部署（只有 main，当前 ali-hk-01 形态）

main 内建本机执行能力，单进程即可服务它所在机器上的全部服务。

### 3.1 hookploy.yaml 最小示例

```yaml
listen:
  http: "127.0.0.1:9100"   # webhook + 状态 API，由 Caddy 反代
  grpc: "127.0.0.1:9101"   # edge 接入口（单机形态暂时用不到，但默认会监听）

servers:
  ali-hk-01: { local: true }   # local: true = 任务走 main 内建 executor

services:
  linkmind:
    server: ali-hk-01
    dir: /opt/apps/linkmind
    image: ghcr.io/reorx/linkmind
    deploy:
      - image.pin        # 按 payload.digest 锁定镜像并验证
      - compose.up
```

### 3.2 启动

```sh
hookploy main -f /opt/apps/hookploy/hookploy.yaml
```

systemd unit（M3 的 Ansible role 提供）或 `hookploy-ctl.sh start`（PID 文件方式，只控制自己目录里的实例）二选一，**勿并用**。

### 3.3 Caddy 路由（Ansible 负责）

单机形态只需要反代 HTTP：

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
    curl -fsS -X POST "$HOOKPLOY_URL/hooks/linkmind" \
      -H "Authorization: Bearer $HOOKPLOY_TOKEN" \
      -H "Content-Type: application/json" \
      -d '{"digest": "${{ steps.build.outputs.digest }}"}'
```

立即返回 202 + `deploy_id`，部署异步执行；需要等结果时轮询 `GET /deploys/<id>`。

## 4. 多机部署（main + edge，M2 起）

### 4.1 模型

- edge 是**无状态执行器**：主动外连 main 的 gRPC 口并保持长连接，接任务、本机执行 `docker compose` 操作、流式回传日志。断线后指数退避重连（1s 起、上限 30s）。
- edge **零入站端口、零域名、零证书、零本地配置**——任务所需的一切（目录、步骤、参数）由 main 随任务下发。
- edge 的身份由 server token 决定（token 的 subject 就是 server 名），启动命令只要 `--main` + `--token`。

### 4.2 新服务器接入清单（Ansible role 要做的事）

1. **main 侧改配置**：`hookploy.yaml` 的 `servers:` 加一行（非 local）：

   ```yaml
   servers:
     ali-hk-01: { local: true }
     tc-sg-01: {}          # 新 edge
   ```

   下发后 reload（见 §6）。未在 `servers:` 里声明的机器即使 token 有效也会被拒连。

2. **main 本机签发 server token**：

   ```sh
   ./hookploy server token create tc-sg-01 -f hookploy.yaml   # 输出 hps_ 开头的 token
   ```

3. **edge 机器**：上传 binary + 一条启动命令：

   ```sh
   hookploy edge --main https://hookploy.example.com --token hps_xxx
   ```

   - token 也可用环境变量 `HOOKPLOY_SERVER_TOKEN` 传（systemd unit 里配 `EnvironmentFile` 更合适）。
   - `--server <name>` 可选，仅作身份断言（名字来自 token，不填也行）。
   - `https://` URL 走 TLS（由 main 侧 Caddy 终结），`http://` 为明文（仅限内网/本机测试）。

4. **验证**：`hookploy status` 应显示该 server `online` + 版本 + 连接时长。

换机重装 = 重跑第 3 步，无需任何迁移。吊销一台机器：`servers:` 删掉 + revoke 其 server token。

### 4.3 Caddy 侧（多机需要暴露 gRPC）

edge 从公网连 main 时，Caddy 需要以 h2 反代 gRPC 口：

```
hookploy.example.com {
    @grpc protocol grpc
    reverse_proxy @grpc h2c://127.0.0.1:9101
    reverse_proxy 127.0.0.1:9100
}
```

（hookploy 自身不管证书；gRPC keepalive 双向探活，死连接约 40s 内检出。）

⚠️ **域名走 Cloudflare 橙云代理时，gRPC 默认被 CF 掐死**（2026-07-19 生产接入实测）：zone 的 **Network → gRPC 开关**不开，edge 的 handshake 一律 `Internal: server closed the stream without sending trailers`，请求根本到不了源站 Caddy——这不是 Caddy 配置问题，先查 CF。绕行选项（当前生产在用第二种）：

1. 开 CF zone 的 gRPC 开关（面板 Network 页），保持上面的单域名 443 方案；
2. **明文 h2c 直连源站 IP 上一个已放行的端口**：`edge --main http://<源站IP>:<port>`，该端口的 Caddy 监听须声明 `servers :<port> { protocols h1 h2c }`（全局选项）再用 `@grpc protocol grpc` 分流到 9101。server token 会明文过网线，风险自行评估（reorx 生产：复用 vocalflow 心跳的 3031 明文口，pre-launch 接受）。

### 4.4 edge 的进程管理

- systemd（M3 role 提供 unit）：`Restart=always` 即可，edge 自带重连，进程本身极少退出。
- 无 systemd 兜底：`hookploy-ctl.sh` 的 edge 命令组（`edge-start / edge-stop / edge-status / edge-restart / edge-logs [-f]`），需要同目录两个 dotfile（0600）：
  - `.edge_main` — main 的 URL
  - `.server_token` — server token

### 4.5 多实例服务与 rollout

一个服务部署到多台机器时，用 `instances` + `rollout` 波次：

```yaml
vocalflow:
  image: ghcr.io/reorx/vocalflow-rt
  dir: /opt/apps/vocalflow-rt       # 实例默认 dir，可被实例覆盖
  deploy:                           # 所有实例共用同一条流水线
    - image.pin
    - compose.up
    - healthcheck: { url: "http://127.0.0.1:3030/healthz" }
  instances:
    main:    { server: ali-hk-01 }
    api-sg0: { server: tc-sg-01 }
    api-hk0: { server: hh-hk-01 }
  rollout:
    - main                  # 波 1：单实例
    - [api-sg0, api-hk0]    # 波 2：并行，波 1 全部成功后才开始
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
```

所有查询命令支持 `--json`，输出与 HTTP API 完全一致。token 管理命令仅限 main 本机执行。

## 6. 配置变更流程

1. 改 git 里的 `hookploy.yaml`
2. `hookploy validate -f hookploy.yaml` 静态校验（schema、服务器引用、op 参数）——**playbook 里下发前必跑**
3. 下发到服务器后热重载，三选一：
   - `curl -X POST https://.../-/reload -H "Authorization: Bearer $HOOKPLOY_ADMIN_TOKEN"`
   - `kill -HUP <main pid>`（systemd: `systemctl reload hookploy`，unit 里配 `ExecReload=/bin/kill -HUP $MAINPID`）
   - 重启进程（会中断 in-flight 部署：running 的标记 failed，queued 的恢复调度）

reload 失败时 main 保留旧配置继续运行；in-flight 的执行永远用入队时的快照，不受 reload 影响。

## 7. 排障速查

| 现象 | 排查 |
|---|---|
| server 显示 offline | edge 进程在不在（`systemctl status` / `edge-status`）；edge 日志有无 `handshake rejected`（token 被吊销 / server 未在 yaml 声明）；Caddy gRPC 路由是否 h2 |
| 部署 `unreachable` | 目标 edge 离线超 30s 窗口。edge 恢复后 CI 重跑即可 |
| 部署 `failed`，error 带 "edge disconnected" | 执行中途连接断开；main 侧记为失败，edge 侧会同时取消本地执行。看 edge 日志确认 |
| status 里版本标 `(outdated)` | edge binary 落后于 main，按 §2 重新构建分发（先停进程再覆盖，否则 text file busy） |
| webhook 401/403 | service token 错误或被轮换；`webhook: false` 的服务只接受 CLI 手动触发 |
| 部署被顶掉（`superseded`） | 正常：同服务排队时 latest-wins，连推 N 个 commit 最多执行 2 次部署 |

## 8. 当前实例与迁移状态（2026-07-19 快照）

- 正式 main：ali-hk-01 `/opt/apps/hookploy/`（9100/9101），`https://hookploy.reorx.com`，由 deploy 仓库 `ansible/roles/hookploy` 部署；admin token 在同目录 `.admin_token`（0600）。
- 正式 edge：tc-sg-01 / hh-hk-01（deploy 仓库 `ansible/roles/hookploy-edge`：binary + systemd `hookploy-edge` + `edge.env` 骨架），2026-07-19 上线。gRPC 走明文 3031 直连 ali（CF 橙云吃 gRPC，见 §4.3 ⚠️；zone 开关打开后切回 443）。
- 已接管服务：linkmind（单机）、**vocalflow-rt**（多实例 rollout：波 1 main@ali → 波 2 api-hk0@hh + api-sg0@tc 并行；2026-07-19 真实 push 发布验证通过，digest `5ee994ea` 三实例对齐 + 三路实时转录冒烟绿）。
- 真机测试环境：同机 `/opt/apps/hookploy_test/`（9180/9181），含一个模拟 edge（`edge-01`），规范见仓库 `CLAUDE.md`。
- M3 剩余：其余 GHA 服务切换（breeze / simul / panplayer…，多镜像 app 的拷 static 步骤需 op 支持或 `run` 逃生舱）、旧 adnanh/webhook 退役。

## 附：op 词汇表速查

`image.pin`（digest 锁定+内置验证）、`image.extract`（从镜像抽文件近原子交换）、`artifact.extract`（下载+sha256 校验+解压交换）、`compose.pull` / `compose.up` / `compose.run` / `compose.exec` / `compose.restart`、`env.require` / `env.write`、`healthcheck`（轮询 HTTP 直到健康）、`run`（argv 逃生舱，不经 shell）。完整参数见 `docs/PRD.md` §4。

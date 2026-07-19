# Hookploy

中心化 webhook 部署调度器：main 收 webhook，按 `hookploy.yaml`（SSOT）把部署任务分发到本机（内建 executor）或远程服务器（edge 经 gRPC 长连接）。Go 单 binary（main/edge 子命令），无 CGO，SQLite 存储。设计文档见 `docs/PRD.md`。开发方法论：BDD（先写行为测试再实现）。

## 里程碑状态

- M1（单机可用）✅ 已上线 ali-hk-01 正式实例，试点 linkmind
- M2（主从：gRPC + edge + 在线状态 + rollout）✅ 代码完成并真机验证（2026-07-19）
- M3（收尾：Ansible role、--json 冻结、JSON Schema、全量迁移）⏳ 未开始

## 代码地图

- `internal/model` — 纯域类型（无内部依赖）；`internal/api` — HTTP/CLI 共用 DTO（M3 冻结）
- `internal/config` — yaml 加载/归一化/校验（`server:` 语法糖 → instances+rollout 规范形）
- `internal/ops` — op 词汇表、解析、插值、JSON 线格式（DB 快照与 gRPC 下发共用）
- `internal/engine` — op 执行引擎（Runner/HTTP/Sleep 全部可注入，测试不碰 docker）
- `internal/executor` — Executor 抽象 + Registry（30s acquire 窗口 = 离线重连宽限）
- `internal/scheduler` — 串行/去重/波次/digest 提升/恢复
- `internal/grpcapi` — main 侧 gRPC：edge 会话鉴权、在线追踪；会话本身实现 Executor
- `internal/edge` — edge 角色：重连循环、本机执行、流式回传
- `proto/` → `internal/pb` — 协议定义与生成代码，改动后跑 `scripts/genproto.sh`
- `internal/store` — SQLite；`internal/httpapi` — webhook + 状态 API；`internal/cli` — 命令入口

## 关键约束

- edge 只执行结构化 op（argv 直接 exec，不经 shell）；payload 无法注入命令
- server 名由 server token 的 subject 推导（edge 零配置）；token 明文只在创建时输出一次
- in-flight 执行用入队时的 ops 快照，config reload 不影响
- 版本号经 `-ldflags -X internal/version.Version=` 注入，main/edge 握手互报

## Documentation

- `kb/docs/deployment-guide.md` — 部署与使用指南（单机/多机形态、edge 接入清单、Caddy 路由、token 管理、rollout 语义、运维与排障）。涉及 Ansible role 编写、新服务器接入、部署流程问题时先读这份。

## 正式实例（ali-hk-01，M1.5 起）

- 由 deploy 仓库的 `ansible/roles/hookploy` 部署（binary 上传 + `hookploy.yaml` SSOT 模板 + systemd unit + Caddy 路由），**改配置一律改 role 模板再 `ansible-playbook -i inventory.yml playbook.yml --limit ali-hk-01 --tags hookploy`**，勿手改服务器文件。
- 目录 `/opt/apps/hookploy/`，公网入口 `https://hookploy.reorx.com`（Cloudflare 橙云 → Caddy → 127.0.0.1:9100）。
- 进程管理：`systemctl {status,restart} hookploy`；ctl 脚本仅作无 systemd 场景兜底，勿与 systemd 并用。
- 发布新版本：hookploy repo 里 `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o dist/hookploy-linux-amd64 ./cmd/hookploy`，然后跑上面的 playbook（role 从 `~/Code/hookploy/dist/` 上传）。
- 试点服务：linkmind（GHA 传 digest → `image.pin` + `compose.up`）；其余服务仍走旧 adnanh/webhook，M3 全量迁移。

## 真机测试规范（ali-hk-01）

测试部署与未来的正式部署**同机共存**，靠路径和端口隔离；测试部署是临时的、手动的，不进 Ansible SSOT（正式部署由 M3 的 Ansible role 负责）。

### 端口约定

| 用途 | 正式（默认） | 测试 |
|---|---|---|
| main HTTP（webhook + API） | 9100 | **9180** |
| main gRPC（edge 接入） | 9101 | **9181** |
| echo_server 测试服务 | — | **9190** |

测试永远不占用默认端口 9100/9101。

### 路径约定

- `/opt/apps/hookploy_test/` — 测试二进制 `hookploy`、控制脚本 `hookploy-ctl.sh`、`hookploy.yaml`、`hookploy.db`、`hookploy.pid`、`main.log`、`.echo_token`、`.admin_token`（token 文件 0600）
- M2 起同目录还跑一个 edge 进程（server 名 `edge-01`，模拟远程服务器走 gRPC 全链路）：`edge.pid`、`edge.log`、`.edge_main`（main 的 gRPC URL，测试为 `http://127.0.0.1:9181`）、`.server_token`（`./hookploy server token create edge-01 -f hookploy.yaml` 生成）；ctl 命令为 `edge-start / edge-stop / edge-status / edge-restart / edge-logs [-f]`
- `/opt/apps/echo_server/` — 测试服务，`docker-compose.yml` 跑 `traefik/whoami`（127.0.0.1:9190→80），流水线：`compose.pull` → `compose.up` → `healthcheck`

本地侧的配置源文件在 `deploy-test/`（hookploy.yaml、docker-compose.yml、watch-status.sh 状态采样脚本），改动后 scp 覆盖服务器对应文件。

### 构建与上传

```sh
go test ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w -X github.com/reorx/hookploy/internal/version.Version=<ver>' -o tmp/hookploy-linux-amd64 ./cmd/hookploy
scp tmp/hookploy-linux-amd64 ali-hk-01:/opt/apps/hookploy_test/hookploy
```

上传前先 `hookploy-ctl.sh stop`（以及 `edge-stop`），否则覆盖运行中的二进制会报 text file busy。proto 改动后用 `scripts/genproto.sh` 重新生成 `internal/pb`（需要 protoc + protoc-gen-go + protoc-gen-go-grpc）。

改动控制脚本后同样 scp：`scp scripts/hookploy-ctl.sh ali-hk-01:/opt/apps/hookploy_test/hookploy-ctl.sh`

### 启动 / 重启 / 验证

进程管理一律通过 `hookploy-ctl.sh`（PID 文件方式，只控制自己目录里的实例）。
**禁止用 pkill / pgrep 控制进程**：同机可能存在正式版 hookploy，按进程名匹配会误杀；
`pkill -f "hookploy main"` 还会匹配到 ssh 远程 shell 自身、直接杀掉会话。

```sh
# start / stop / restart / status / logs [-f]
ssh ali-hk-01 '/opt/apps/hookploy_test/hookploy-ctl.sh restart'
ssh ali-hk-01 '/opt/apps/hookploy_test/hookploy-ctl.sh status'

# 触发部署（webhook）
ssh ali-hk-01 'cd /opt/apps/hookploy_test && curl -sS -X POST http://127.0.0.1:9180/hooks/echo_server -H "Authorization: Bearer $(cat .echo_token)" -H "Content-Type: application/json" -d "{}"'

# 查询状态（CLI 走 admin API）
ssh ali-hk-01 'cd /opt/apps/hookploy_test && export HOOKPLOY_URL=http://127.0.0.1:9180 HOOKPLOY_ADMIN_TOKEN=$(cat .admin_token) && ./hookploy status && ./hookploy deploys echo_server'
```

token 丢失时重新生成：`./hookploy token create echo_server -f hookploy.yaml`（service）、`./hookploy admin-token create -f hookploy.yaml`（admin），写入对应 dotfile。

### 清理

```sh
ssh ali-hk-01 '/opt/apps/hookploy_test/hookploy-ctl.sh stop; cd /opt/apps/echo_server && docker compose down; rm -rf /opt/apps/hookploy_test /opt/apps/echo_server'
```

### 注意

- ali-hk-01 是 x86_64 主力生产机，SSH 见 `~/.ssh/config`（root@8.210.184.77:1122）。测试期间不得动生产服务（linkmind / breeze / simul 等）与默认端口。
- `scripts/hookploy-ctl.sh` 是发布产物的一部分：打包发布时随二进制一起分发，测试与正式部署都用它做进程控制；正式运行（M3 Ansible role）另外提供 systemd unit，ctl 脚本作为无 systemd 场景与手动运维的兜底。

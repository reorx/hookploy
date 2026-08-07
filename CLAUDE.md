# Hookploy

中心化 webhook 部署调度器：main 收 webhook，按 `hookploy.yaml`（SSOT）把部署任务分发到本机（内建 executor）或远程服务器（edge 经 gRPC 长连接）。Go 单 binary（main/edge 子命令），无 CGO，SQLite 存储。设计文档见 `docs/PRD.md`。开发方法论：BDD（先写行为测试再实现）。

## 项目状态

M1–M3 全部完成（2026-07-19）；M4 Web UI（`/ui/`，只读）已实现（2026-07-22，计划见 `kb/plans/2026-07-21-web-ui-plan.md`）。GitHub Actions 集成（workflow_run webhook 推送，见 `kb/plans/2026-07-22-github-actions-plan.md`）已实现（2026-07-22）：`POST /github/webhook` 收构建事件，三处 UI 展示；数据不经 `internal/api`，DTO 契约未动。传输层解耦 + SSE 通路 + 流断容忍已实现（2026-07-30，计划见 `kb/plans/2026-07-30-transport-decouple-and-sse.md`）：新增 `internal/edgehub`（传输无关核心）、`internal/edgewire`，grpcapi 瘦身为适配器，httpapi 加 SSE 端点；真机验证（ali-hk-01，`ss -K` 掐流）通过。`--json` / `internal/api` DTO / HTTP API 契约已冻结（`model.EdgeInfo` 加了 `Transport`，仅内部与 Web UI 用，`api.ServerInfo` 未动）。已知遗留（决定不修）：`logs -f` 探针用类型化 FollowFrame 后解码变严，分歧帧被静默丢弃（两端同源、风险低）。

部署失败通知第一阶段已实现（2026-08-05）：新增 `internal/notify`（Telegram Bot），`hookploy.yaml` 加 `notify:` 段（全局 + 每 service 覆盖），事件词汇表定在 `model.EventKind`。顺带把 `store.RecomputeDeployStatus` 的终态写入改成 `finished_at` 上的 CAS 并新增 `settled bool` 返回值——它一次 rollout 会被调用 3+ 次，此前无从分辨哪次才是真正的结算（同时修掉了 `Done` 事件重复 publish）。`internal/api` / `--json` / Web UI 契约未动。二阶段（通知中心）接口已预留，见代码地图。

节点事件已实现（2026-08-07）：词汇表加 `main.started` / `edge.offline` / `edge.online`，并引入 `model.EventScope`（`ScopeDeploy` / `ScopeNode`）把"属于某服务的部署结果"与"属于整个部署的节点动静"分开——它同时是 `notify.Event` 载荷的判别式（`Deploy *DeployEvent` / `Node *NodeEvent`，恰好设一个）。四个默认事件：`deploy.failed` + 三个节点事件。服务级 `notify.events` 只收 deploy.*，写节点事件加载失败，从全局继承来的节点事件静默过滤。新增 `internal/edgewatch`（周期巡检，见代码地图），`notify.Hub` 加三个窄入口方法。`internal/api` / `--json` / Web UI 契约仍未动。

**v0.5.0 已发布并上生产（2026-08-08）**：生产三台对齐 v0.5.0，两台 edge 全部切到 SSE 通路（`kb/notes/2026-08-05-edge-sse-migration-guide.md` 已执行完毕），Telegram 通知启用（main.started 实测送达）。gRPC 通路进入实际弃用观察期。

生产部署、服务迁移等运维事项不在本仓库跟踪（见用户全局 CLAUDE.md 的 DevOps 约定，统一在 deploy 目录管理）。

## 常用命令

- 测试：`go test ./...`；CLI golden 快照（`internal/cli/testdata/golden/`）更新：`go test ./internal/cli -update`
- proto 改动后：`scripts/genproto.sh` 重新生成 `internal/pb`（需要 protoc + protoc-gen-go + protoc-gen-go-grpc）
- templ 改动后（`internal/webui/views/*.templ`）：`scripts/gentempl.sh` 重新生成 `*_templ.go`（需要 templ CLI，版本与 go.mod 一致）；生成文件提交进仓库
- 发布：`make dist`；push `v*` tag 触发 GitHub Releases。版本号经 `-ldflags -X .../internal/version.Version=` 注入，main/edge 握手互报

## 代码地图

- `internal/model` — 纯域类型（无内部依赖）；含 `EventKind` 通知词汇表与 `EventScope`（config 要校验事件名但不能 import notify，所以词汇表归 model，规则归 notify）；`internal/api` — HTTP/CLI 共用 DTO（已冻结）
- `internal/config` — yaml 加载/归一化/校验（`server:` 语法糖 → instances+rollout 规范形）；`config.JSONSchema()` 供 `hookploy schema`
- `internal/ops` — op 词汇表、解析、插值、JSON 线格式（DB 快照与 gRPC 下发共用）
- `internal/engine` — op 执行引擎（Runner/HTTP/Sleep 全部可注入，测试不碰 docker）；`internal/runner` — argv 执行（不经 shell）
- `internal/executor` — Executor 抽象 + Registry（30s acquire 窗口 = 离线重连宽限）
- `internal/scheduler` — 串行/去重/波次/digest 提升/恢复
- `internal/edgehub` — **main 侧传输无关核心**：attach/detach 状态、per-server Executor、update 路由、60s 宽限窗口与重连认领。gRPC 与 SSE 两个适配器共用它；`Edges()` 天然是合并视图
- `internal/grpcapi` — gRPC 适配器：握手鉴权 + pb 编解码 + 驱动 edgehub
- `internal/httpapi/edge.go` — SSE 适配器：`GET /edge/session`（单写者 goroutine，45s 心跳注释帧）+ `POST /edge/executions/{id}/updates|done`，serverAuth 中间件（server token，不吃 cookie）
- `internal/edgewire` — SSE 通路 JSON wire 类型（定位对标 `internal/pb`，不进冻结的 `internal/api`）
- `internal/edge` — edge 角色：`agent.go` 传输无关核心（退避重连、执行去重、结果缓冲+ack 补报）、`transport.go` 接口与中立 Task/Update 类型、`transport_grpc.go`（Resumable=false）、`transport_sse.go` + `sseread.go`（Resumable=true）
- `proto/` → `internal/pb` — 协议定义与生成代码
- `internal/notify` — 通知（一阶段 Telegram，二阶段切通知中心）：`notify.go`（`Event` 两半载荷 + `Hub` 异步投递——有界队列、退避重试、按 `pending.key()` 去重、关机前有界补投；入口 `Notify(deployID)` 与三个节点方法 `MainStarted`/`EdgeOffline`/`EdgeOnline`）、`classify.go`（deploy 终态 → `model.EventKind`，含 recovered 判定与 per-service 策略过滤）、`telegram.go`（`Provider` 实现 + `providerFor` 分发 + 按载荷分流的渲染，渲染是 provider 私有）。**两条载荷的不对称是有意的**：deploy 只入队 id、投递时才查 store 建 Event（reload 中途生效、被 retention 回收就静默跳过），节点事件入队即完整（状态只在内存，事后无处可查）；两者的策略门都在每次投递时重查。scheduler / httpapi 只持有裸签名 `func(deployID string)`，**都不 import 本包**；二阶段加 `center.go` + `providerFor` 一个 case + config 三处追加即可，Hub 与生产者不用动
- `internal/edgewatch` — edge 离线巡检：每 30s 比对 `cfg.Servers`（排除 `local`）与注入的 `hub.Edges` 快照，自维护 per-server `lastSeen`/`version`/`alerted`，跨过 `notify.edge_offline_after` 报一次 `OnOffline`，回来报 `OnOnline`。**选轮询而非 edgehub 断连回调**，是因为 main 重启后从未连上的 edge 根本不会触发断连——那恰是最该告警的情况；代价是告警延迟 ≤1 个 tick，收益是 edgehub 一行不动。回调是裸 `func(Outage)`，本包不 import notify，由 `cmd_main` 接线。首次见到某 server 时以"当下"为基线（不追溯进程启动前）；config 里删掉的 server 立即忘记状态
- `internal/store` — SQLite（含 workflow_runs：GitHub 构建记录，按 run id upsert、每 repo 留 200 条）；`internal/httpapi` — webhook + 状态 API + GitHub workflow_run webhook（`github.go`：HMAC 校验，secret 未配置时端点 404）；`internal/cli` — 命令入口；`internal/apiclient` — CLI 访问 admin API 的 HTTP 客户端；`internal/token` — token 生成/哈希
- `internal/webui` — 内置只读 Web UI（`/ui/`，静态资源 go:embed 进 binary，发布包自包含；顶层配置 `webui: false` 可整体不挂载，重启生效）：templ 服务端渲染 + 会话 cookie（admin token 登录；cookie 仅对 GET admin API 生效）；页面 Dashboard / Actions（`/ui/actions`，按 service 过滤）/ 服务详情 / 部署详情；`views/` templ 源与生成码，`static/` embed 的 CSS/JS（app.js 片段轮询、logs.js NDJSON 日志流）。repo→service 映射在查询时经 service 的 `github_repo` 解析，热 reload 即时生效

## 关键约束

- edge 只执行结构化 op（argv 直接 exec，不经 shell）；payload 无法注入命令
- server 名由 server token 的 subject 推导（edge 零配置）；token 明文只在创建时输出一次
- in-flight 执行用入队时的 ops 快照，config reload 不影响
- **两条 edge 通路并存**（`edge --transport grpc|sse`，默认 grpc，也可用 `HOOKPLOY_TRANSPORT`）：同一个 `--main` URL，无自动探测、无自动回退。SSE 走 HTTP 口，CF 橙云下无需 gRPC 开关，且**独享流断容忍**（连接断 → main 保留执行 60s → edge 重连声明 inflight → 认领 → 补报结果 → 部署照常 succeeded；宽限内没回来判 unreachable 而非 failed）。gRPC wire 无 DoneAck/Hello.inflight，跨会话保留执行无法被 ack，故 `Resumable=false`、维持"断连即失败"现状语义，在 deprecation 路上

## 文档

- `kb/docs/deployment-guide.md` — 通用部署与使用手册（部署形态、edge 接入、反代与 gRPC 路由、token、rollout、配置变更 validate/reload、构建发布、`--json` 契约、op 词汇表、排障）。涉及部署形态或使用方式问题先读这份；文中服务器/域名均为虚构示例
- `docs/json-output.md` — `--json` 与 HTTP API 输出契约。改动 `internal/api` DTO 或消费 JSON 输出前先读这份

## 真机测试（ali-hk-01）

ali-hk-01 是 x86_64 主力生产机（SSH 见 `~/.ssh/config`）。测试部署与正式部署**同机共存**，靠路径和端口隔离，临时手动、不进任何配置管理 SSOT。**不得动生产服务与默认端口 9100/9101**（含正式版 hookploy）。

- 测试端口：main HTTP **9180**、main gRPC **9181**、echo_server **9190**
- `/opt/apps/hookploy_test/` — 测试二进制、`hookploy-ctl.sh`、`hookploy.yaml`、db/pid/log、token dotfiles（0600：`.echo_token`、`.admin_token`、`.server_token`、`.edge_main`）。同目录跑一个 edge 进程（server 名 `edge-01`，走 gRPC 全链路，main URL `http://127.0.0.1:9181`）
- `/opt/apps/echo_server/` — 测试服务（`traefik/whoami`，流水线 `compose.pull` → `compose.up` → `healthcheck`）
- 本地配置源在 `deploy-test/`，改动后 scp 覆盖服务器对应文件；`scripts/hookploy-ctl.sh` 改动同理

构建与上传（上传前先 `stop` + `edge-stop`，否则覆盖运行中二进制报 text file busy）：

```sh
go test ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w -X github.com/reorx/hookploy/internal/version.Version=<ver>' -o tmp/hookploy-linux-amd64 ./cmd/hookploy
scp tmp/hookploy-linux-amd64 ali-hk-01:/opt/apps/hookploy_test/hookploy
```

进程管理一律用 `hookploy-ctl.sh`（PID 文件方式；命令 `start/stop/restart/status/logs [-f]`，edge 为 `edge-start` 等）。**禁止 pkill/pgrep**：按进程名匹配会误杀同机正式版 hookploy，`pkill -f "hookploy main"` 还会匹配到 ssh 远程 shell 自身、直接杀掉会话。

```sh
ssh ali-hk-01 '/opt/apps/hookploy_test/hookploy-ctl.sh restart'
# 触发部署
ssh ali-hk-01 'cd /opt/apps/hookploy_test && curl -sS -X POST http://127.0.0.1:9180/hooks/echo_server -H "Authorization: Bearer $(cat .echo_token)" -H "Content-Type: application/json" -d "{}"'
# 查询状态（CLI 走 admin API）
ssh ali-hk-01 'cd /opt/apps/hookploy_test && export HOOKPLOY_URL=http://127.0.0.1:9180 HOOKPLOY_ADMIN_TOKEN=$(cat .admin_token) && ./hookploy status && ./hookploy deploys echo_server'
```

token 丢失时重建：`./hookploy token create echo_server -f hookploy.yaml`（service）、`./hookploy admin-token create -f hookploy.yaml`（admin）、`./hookploy server token create edge-01 -f hookploy.yaml`（edge），写入对应 dotfile。

清理：`ssh ali-hk-01 '/opt/apps/hookploy_test/hookploy-ctl.sh stop; cd /opt/apps/echo_server && docker compose down; rm -rf /opt/apps/hookploy_test /opt/apps/echo_server'`

注：`scripts/hookploy-ctl.sh` 是发布产物的一部分（随二进制分发，作为无 systemd 场景的兜底进程控制；正式运行建议 systemd unit）。

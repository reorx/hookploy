# Hookploy — Agent Notes

中心化 webhook 部署调度器：main 收 webhook，按 `hookploy.yaml`（SSOT）把部署任务分发到本机（内建 executor）或远程服务器（edge 经 gRPC 长连接）。Go 单 binary（main/edge 子命令），无 CGO，SQLite 存储。设计文档 `docs/PRD.md`；开发方法论 BDD（先写行为测试）；真机测试与正式实例的运维规范见 `CLAUDE.md`。

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

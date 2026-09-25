---
created: 2026-09-25
tags:
  - hookploy
  - ops
  - engine
  - config
  - edgehub
  - reliability
---

# 实现 step 级 timeout / retries 修饰符：拉取卡死不再吃光整条流水线的预算

## 概要

背景是 2026-09-24 vocalflow-rt `dp_1a0d2a67f8ec5336195` 的失败：GHCR CDN 让两台 API 节点的同一个 blob 卡在 30 MiB，`docker pull` 对停滞连接没有无进度超时，hookploy 也只有 execution 级超时，于是一条卡住的连接吃光了 10 分钟预算。本 session 按已拍板的 plan 实现：step 可加 `timeout:`（每次尝试的超时，所有 op）与 `retries:`（仅 `image.pin` / `compose.pull` / `artifact.extract`），重试是全新执行、间隔 5s。image.pin 与 artifact.extract 原有的内置重试循环被收编为这两个 op 的默认 `retries: 2`，全系统只剩 engine 里一层重试。修饰符要进 ops 线格式，而老 edge 会静默忽略它们，所以 main 派发前按 edge 握手版本门控，快照用到修饰符而 edge < v0.7.0 时直接判 failed。`healthcheck.retries` 按决议硬切改名为 `attempts`，DB 里的老快照仍能解码。加载期校验 `timeout × 尝试次数 + 间隔` 不得超过服务 timeout。Web UI 服务页显示显式写出的修饰符。BDD 流程：每层先写行为测试（ops 解析/线格式、config 预算、schema、engine 重试与真实子进程被杀、edgehub 门控、scheduler 端到端），`go test ./...` 全绿。真机（ali-hk-01 测试环境）验证了门控、SIGSTOP 冻住真实 `docker pull` 后的超时→重试→成功（本机与 edge 各一次），以及重试耗尽后的报错与波次停止。发布与生产接线（验收 6）留给用户确认。

## 修改的文件

- `internal/ops/modifiers.go`（新）— 修饰符保留键、`retryable` 白名单与默认 retries、`RetryInterval`、`ModifiersSince`、`Step.Attempts()`、`NeedsModifierSupport`、`timeout`/`retries` 解析
- `internal/ops/types.go` — `Step` 加 `Timeout` / `Retries *int`；`Healthcheck.Retries` → `Attempts`，`UnmarshalJSON` 兼容旧键
- `internal/ops/parse.go` — 步骤 map 接受任意修饰符组合；参数里的修饰符与 `healthcheck.retries` 给出改法提示
- `internal/ops/json.go` — 线格式加可选 `timeout` / `retries`（未设置即省略，无修饰符的步骤字节不变）
- `internal/ops/catalog.go` — healthcheck 文档改为 attempts
- `internal/config/config.go` — `validateStepBudget`：最坏耗时超过服务 timeout 即报错
- `internal/config/schema.go` — step 的修饰符分支：嵌套 oneOf 每 op 一个 `required`，不可重试的 op 带 `not: required retries`；jsonSchema 加 `minimum`
- `internal/engine/attempts.go`（新）— `runAttempts` / `runAttempt`、`permanentError`、`attemptsError`
- `internal/engine/engine.go` / `pin.go` / `extract.go` / `healthcheck.go` — 删除 PullRetries/PullInterval/DownloadRetries 与两个内置循环；不可自愈的失败标 permanent；image.extract 临时容器清理改用脱离 op ctx 的上下文
- `internal/runner/fake.go` — 在已过期 ctx 上启动的命令返回 ctx 错误（与 ExecRunner 一致）
- `internal/version/version.go` — `AtLeast`（核心版本号比较，`dev` 视为满足）
- `internal/edgehub/hub.go` — `checkEdgeVersion`：派发前门控
- `internal/webui/{pages.go,views/*,static/app.css}` — 服务页渲染 `on` / `timeout` / `retries` 标签（templ 已重新生成）
- 测试：`internal/ops/modifiers_test.go`（新）、`internal/engine/attempts_test.go`（新，含真实子进程被杀的集成测试）、`internal/version/version_test.go`（新），以及 config / schema / edgehub / scheduler / webui / engine / ops 既有测试的增补与改名
- `docs/PRD.md` — 新增「步骤超时与重试」一节，image.pin / artifact.extract / healthcheck 语义与示例同步
- `docs/json-output.md` — `GET /services/<name>` 的 step 对象：可选 `timeout`/`retries`，`args` 形状不在冻结范围，healthcheck `args.retries` → `args.attempts`
- `kb/docs/deployment-guide.md` — 新增 §4.7（含 v0.7.0 升级顺序），排障表两行，速查更新
- `testdata/hookploy.yaml`、`deploy-test/hookploy.yaml` — 示例改名 attempts；测试配置加 `pin_local` / `pin_edge`
- `deploy-test/freeze-pull.sh`（新）— 按 PID 文件冻住测试进程派生的 `docker pull`
- `CLAUDE.md` — 项目状态、代码地图、真机测试说明（顺带修正测试 edge 实际走 SSE 的描述）
- `kb/plans/2026-09-24-op-timeout-and-retry-plan.md` — 状态与「实施记录」
- `kb/known-issues.md`、`kb/next-up.md`（新）— 见下

## 注意事项

- **修饰符上线（wire）之后，老 edge 静默降级就是真实风险**：Go 的 `json.Unmarshal` 忽略未知字段，老 edge 会把 `timeout: 3m` 的快照按老语义照跑。门控放在 `edgehub` 派发处，是唯一同时知道快照内容和 edge 版本的地方。判定写在 `ops.NeedsModifierSupport`，一个地方定义"什么东西老 edge 会跑错"；`healthcheck.attempts == 5` 不拦，因为老 edge 回落的默认值正好是 5。
- **只保留一层重试**：op 实现里不要再写重试循环。可重试 op 的失败若确定不会自愈，就用 `permanent()` 包起来。
- **日志与错误向后兼容的做法**：第一次尝试不打头行，只尝试一次的步骤错误原样返回，所以不带修饰符的配置日志和错误逐字节不变。
- **真实进程测试在 macOS 上的坑**：新写出的可执行文件第一次 exec 会被扫描，并行跑全量测试时能卡几百毫秒到数秒，把"应该成功的那次尝试"的超时吃掉。测试先 exec 一次 warmup 再计时。
- **ssh 后台启动的坑**：`ssh host 'cd X && nohup cmd > f 2>&1 &'` 里的 `&` 作用于整个 `&&` 链，子 shell 继续持有 ssh 的 stdout，会话不返回。写成 `cd X; nohup cmd > f 2>&1 < /dev/null &`。
- **被 SIGSTOP 的进程收不到 SIGTERM**（信号挂起），runner 的 5s 宽限后 SIGKILL 才回收它，所以冻结测试里一次超时实际占 25s。真实卡住的 `docker pull` 会被 SIGTERM 直接杀掉。
- **生产机上的故障注入**：不要掐 dockerd↔registry 连接（会波及同机正式服务）。按 PID 文件定位测试进程的直接子进程，发 SIGSTOP。

## 遗留问题

- v0.7.0 尚未发布，生产未升级。deploy 仓库模板 6 处 `healthcheck … retries: 10` 要与 main 升级同一次下发改名；推荐先升 edge 再升 main。已登记 `kb/known-issues.md` 与 `kb/next-up.md`。
- `defaults.timeout` 是否抬到 15m 未定，留到给 vocalflow-rt 接线时决定（已写进 `kb/next-up.md`）。
- healthcheck 单次请求仍无独立超时，step `timeout:` 只能给整段轮询封顶。`kb/known-issues.md` 条目已按现状改写。
- 测试环境（ali-hk-01 9180）的 main 与 edge 现在是 v0.7.0-rc.1，配置含 `pin_local` / `pin_edge`，pin_test 容器已下线。

## 已解决的已知问题

- 「healthcheck 单次请求没有独立超时」条目里附带的 **`retries` 实为总尝试次数、文案「最大重试次数」不准**：参数改名为 `attempts`，catalog 文案改为「轮询总次数（含第一次）」；由 `TestHealthcheckAttempts` 与 schema 测试覆盖。该条目其余部分（单次请求超时）仍在，已改写。

## 相关文档

- [op 级超时 + 重试计划](../plans/2026-09-24-op-timeout-and-retry-plan.md) — 本次按此实现，更新了状态与实施记录
- [op 级 instance 定向计划](../plans/2026-08-10-op-instance-targeting-plan.md) — 参考：修饰符语法与真机验证做法沿用它
- [部署与使用手册](../docs/deployment-guide.md) — 更新：新增 §4.7、排障与速查
- [v0.5.0 发布 session](2026-08-08-v0.5.0-release-sse-and-telegram-notify-to-production.md) — 参考：生产升级 SOP 与 deploy 仓库结构

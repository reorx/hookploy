---
created: 2026-07-22
tags:
  - postmortem
  - scheduler
  - state-machine
  - concurrency
  - flaky-test
  - release
---

# Postmortem：deploy「结束」语义被 status 字段冒充

一个在 v0.3.0 发布流程中被 CI 偶发失败暴露出来的状态机缺陷，及其两轮修复。

| | |
|---|---|
| 发现时间 | 2026-07-22，v0.3.0 发布流程中 |
| 触发方式 | GitHub Actions release workflow 的 `go test ./...` 失败（run 29889733802） |
| 表面症状 | `internal/scheduler` 的 `TestWaveGatingAndCancel` 偶发失败 |
| 实际性质 | 产品缺陷，非测试问题。影响 `logs -f`、Web UI 日志流、`finished_at` 时间戳、重启恢复 |
| 修复提交 | `23d155e`（Bug A）、`1a8c024`（Bug B） |
| 涉及版本 | 缺陷自 wave 机制引入即存在；v0.3.0 修了一半，v0.3.1 修完 |
| 真机验证 | **进行中**，尚未完成（见文末「验证状态」） |

## 1. 一句话根因

`deploy.status` 一直承担两种不同含义——**「当前状况是坏的」**和**「rollout 结束了」**——而代码里有多处拿它当后者用。第一个实例失败时 status 就变 `failed`，此时其余实例可能仍在运行或等待取消，于是所有「以 status 判断结束」的地方都提前触发。

## 2. 时间线（发现过程）

这段过程本身比结论更有参考价值，尤其是「本地绿、CI 红」和「修了一半」两个环节。

1. **打 tag `v0.3.0` 并推送**，触发 release workflow。发布前本地已跑 `go test ./...` 全绿。
2. **CI 失败**（run 29889733802）。`Test` 步骤挂在：
   ```
   --- FAIL: TestWaveGatingAndCancel (0.45s)
       scheduler_test.go:274: per-instance statuses wrong: map[m0:failed sg0:queued]
   ```
   `Create release` 步骤未执行，**未产生任何 release 产物**——这一点后来决定了可以安全重推 tag。
3. **本地无法复现**：`go test ./internal/scheduler -run TestWaveGatingAndCancel -count=30` 全过。
   > 注意：发布前那次「本地全绿」实际上是 `(cached)` 结果，并未真正执行。这是第一个教训。
4. **换条件复现**：`-race` 下**稳定复现**（`-cpu=1` 反而不复现）。race detector 拖慢了 goroutine 调度，放大了本就存在的时间窗口。
5. **定位根因**：读 `AggregateStatus` → `RecomputeDeployStatus` → `runDeploy`，确认是产品缺陷而非测试断言写错（详见 §3）。
6. **第一版修复走错方向**：改 `AggregateStatus` 让它在有非终态 execution 时不返回终态。结果撞翻了 `TestDeployStatusFailureVisibleEarly`——那个测试明确要求「wave 内一个实例失败要立刻在 deploy 上可见」。
   > **两个需求都是对的**。冲突不在需求之间，而在于一个字段承担了两种语义。于是改为「拆开语义」而非「二选一」。
7. **修复 Bug A**（`23d155e`），全量 `-race` 通过，scheduler + store 连跑 15 轮稳定。删除并重推 `v0.3.0` tag（该 tag 无产物，重用干净），release 成功。
8. **事后 review 发现 Bug B**：原计划由 review agent 执行，但该 agent 两次进入 idle 都未送回报告（消息投递故障），遂由主 session 自行审查。重点检查「CLI `logs -f` 的退出条件依赖 status 还是 Done 事件」时，在 `internal/store/logs.go:125` 发现**同一 bug 的对称另一半**（详见 §5）。
9. **修复 Bug B**（`1a8c024`），发布 `v0.3.1`。

## 3. Bug A：deploy 在后续 wave 尚未取消时就宣告结束

### 缺陷代码

`internal/store/deploys.go` 的 `RecomputeDeployStatus`（修复前）：

```go
agg := model.AggregateStatus(statuses)
if agg.Terminal() {                    // ← 判据只看聚合状态
    // 写 finished_at + 发 Done
}
```

而 `model.AggregateStatus` 在状态混合时，只要发现任一 `failed`/`unreachable`/`canceled` 就返回 `failed`——一个**终态**。

### 触发时序

多 wave rollout，wave 1 只有 `m0`，wave 2 只有 `sg0`：

| 时刻 | scheduler 动作 | executions 状态 | AggregateStatus | 后果 |
|---|---|---|---|---|
| T1 | `m0` 执行失败，`transition()` 内部调用 `RecomputeDeployStatus` | `[failed, queued]` | `failed`（终态） | **写 `finished_at`、`publish(Done:true)`** |
| T2 | `runDeploy` 循环进入下一轮，走 `failed` 分支 | `[failed, canceled]` | `failed` | 此时才把 `sg0` 标记 canceled |

T1 与 T2 之间存在一个真实窗口，窗口内 deploy 已对外宣告「结束」，而 `sg0` 仍是 `queued`。

### 实际影响

- **`finished_at` 记录了错误的时刻**——rollout 实际结束于 T2，记录的是 T1。
- **`logs -f` / Web UI follow 流在 T1 收到 Done 并结束**，此后读取 executions 会看到「deploy 已 failed，但实例还挂在 queued」的不一致状态。测试正是踩中这一点。
- 该窗口在真实负载下通常极短，但 wave 数量多、DB 较慢时会放大。

### 修复（`23d155e`）

**保留** `AggregateStatus` 的早失败可见语义（不改动），新增一个独立判据：

```go
// internal/model/model.go:133
func AllTerminal(statuses []Status) bool {
	for _, s := range statuses {
		if !s.Terminal() {
			return false
		}
	}
	return true
}
```

`RecomputeDeployStatus` 的终结条件（`internal/store/deploys.go:245`）：

```go
if agg.Terminal() && model.AllTerminal(statuses) {
```

于是：status 字段仍立刻变 `failed`（早可见），但 `finished_at` 与 Done 事件推迟到所有 execution 落定。

### 连带修复：重启恢复路径

新的终结条件引入了一个**回归风险**：若有 execution 永远停在非终态，deploy 将永远不终结。

`Scheduler.Recover` 只重新调度「整体从未开始」的 deploy（`ListQueuedDeploys` 只查 `status='queued'` 的 deploy）。因此崩溃时被 gate 住的后续 wave 的 `queued` execution **永远不会被执行**：

- 旧逻辑下它们成为孤儿——deploy 显示 failed，却永远挂着 queued 实例（既存的不一致，只是未被察觉）
- 新逻辑下会更糟——deploy **永远不终结**

故 `RecoverInFlight`（`internal/store/deploys.go:302`）新增：把受影响 deploy 内残留的 `queued` execution 一并标记 `canceled`。注意**只针对有 dispatching/running 的那些 deploy**，不能误伤整体从未开始的 queued deploy（后者要被重新调度）。

## 4. 为什么第一版修复方向是错的

值得单独记录，因为这是本次最有价值的判断点。

第一版直接修改 `AggregateStatus`，让它在存在非终态 execution 时返回 `Running`。这立刻撞翻了：

```go
// internal/scheduler/scheduler_test.go
// Behavior: in a parallel wave, one instance's failure is visible on the
// deploy immediately, not only after the whole wave drains.
func TestDeployStatusFailureVisibleEarly(t *testing.T)
```

这个测试编码的是一个**真实产品需求**：用户不该等一个慢实例跑完才看到失败。

当两个都正确的需求发生冲突时，通常说明**某个抽象承担了过多语义**。这里就是 `status` 字段。正确解法是把「坏消息」与「结束」拆成两个判据，而不是牺牲其中一个需求。

## 5. Bug B：日志流在兄弟实例仍在运行时提前关闭

### 为什么 Bug A 的修复没有覆盖它

Bug A 的修复堵住了**推送侧**：`RecomputeDeployStatus` 不再提前 `publish(Done)`。

但还有一条**对称的拉取侧路径**：新建立的 follower 在握手时会自行读取快照判断 deploy 是否已结束——用的仍是旧判据。

### 缺陷代码

`internal/store/logs.go`（修复前，约 125 行）：

```go
cur, err := s.GetDeploy(deployID)
if err == nil && cur != nil && cur.Status.Terminal() {  // ← 又一次拿 status 当「结束」
    send(Event{Done: true, Status: cur.Status})
    return
}
```

### 触发时序

**同一 wave 内并行**的两个实例 `m0`、`sg0`（即 `TestDeployStatusFailureVisibleEarly` 的场景形态）：

1. `m0` 失败 → `deploy.status = failed`（设计如此，早可见）
2. `sg0` **仍在运行，持续产生日志**
3. 用户此刻执行 `hookploy logs -f <id>`（或 Web UI 打开日志页）
4. `FollowDeploy` 重放已有日志 → 快照检查 `cur.Status.Terminal()` → `failed` 是终态 → **立刻发 Done 并关闭流**
5. **`sg0` 后续的全部输出丢失**，用户看不到仍在运行的那个实例

`internal/httpapi/queries.go` 的 `followLogs` 依赖 Done 事件退出（106–111 行），所以 CLI `logs -f` 与 Web UI 日志流**都**受影响。

### 修复（`1a8c024`）

抽出共用查询 `execStatuses`（`internal/store/deploys.go:204`），新增：

```go
// internal/store/deploys.go:224
// DeploySettled reports whether every execution of a deploy has reached a
// terminal status. The deploy's own status goes failed as soon as one
// instance fails, so it cannot be used to tell that a rollout is over.
func (s *Store) DeploySettled(deployID string) (bool, error)
```

`FollowDeploy` 改为（`internal/store/logs.go:127`）：

```go
if err == nil && cur != nil && cur.Status.Terminal() {
    if settled, serr := s.DeploySettled(deployID); serr == nil && settled {
        send(Event{Done: true, Status: cur.Status})
        return
    }
}
```

至此推送侧与拉取侧使用**同一判据**。

## 6. 修复后的不变量

> **`deploy.status` 回答「现在情况如何」，`finished_at` / Done 事件回答「rollout 是否结束」。判断结束必须用 `DeploySettled`（所有 execution 落终态），绝不能用 `status.Terminal()`。**

对应地：
- `AggregateStatus` — 早失败可见，任一实例失败即 `failed`。**不是**结束信号。
- `AllTerminal` / `DeploySettled` — 唯一的结束判据。
- `RecoverInFlight` — 必须保证恢复后不留下无人推进的非终态 execution，否则 deploy 永不终结。

## 7. 测试

均遵循 TDD：先写测试复现失败，再修。

| 测试 | 位置 | 锁定的行为 |
|---|---|---|
| `TestAllTerminal` | `internal/model/model_test.go` | `[failed, queued]` / `[failed, running]` 不算结束；`[failed, canceled]` 算 |
| `TestRecomputeFailureIsLiveButNotFinishedEarly` | `internal/store/store_test.go` | 失败即刻可见，但 `finished_at` 为空且不发 Done；全部落定后才发 |
| `TestRecoverInFlightCancelsGatedWaves` | `internal/store/store_test.go` | 重启后被 gate 的 wave 变 canceled，deploy 能达终态 |
| `TestRecoverInFlightLeavesUnstartedDeploys` | `internal/store/store_test.go` | 从未开始的 deploy 不被误取消，保持 queued 待重新调度 |
| `TestFollowDeployStreamsUntilAllInstancesSettle` | `internal/store/store_test.go` | status 已 failed 时新建流不提前退出，持续收到兄弟实例输出 |
| `TestWaveGatingAndCancel`（改） | `internal/scheduler/scheduler_test.go` | 改用新增的 `waitFinished` helper 等 `finished_at`，而非等 status |

**`waitFinished` 的必要性**：原 `waitStatus` 轮询 status 字段，会在 T1（早可见的 failed）就返回，然后立刻读 executions——正是这个竞态让测试偶发失败。检查 per-instance 状态的测试必须等**真正结束**。

`TestDeployStatusFailureVisibleEarly` 与 `TestAggregateStatus` 的原有用例**全部保留未改**，早可见语义未被牺牲。

### 验证强度

```sh
go test ./... -race                                          # 全绿
go test ./internal/store ./internal/scheduler ./internal/httpapi -race -count=12   # 稳定
```

## 8. 发布影响

| 版本 | 状态 | 说明 |
|---|---|---|
| v0.3.0（第一次） | CI 失败 | tag 已推但无 release 产物 |
| v0.3.0（重推） | 成功 | 删除原 tag 重推到修复 commit。含 Bug A 修复，**仍带 Bug B** |
| v0.3.1 | 成功 | 含 Bug B 修复 |

v0.3.0 已产生对外产物，故 Bug B 只能以 v0.3.1 跟进，不能重推。

## 9. 经验教训

1. **`(cached)` 的测试结果不是测试结果。** 发布前的「本地全绿」是缓存。发布前应 `go clean -testcache` 或 `-count=1`，并且**跑 `-race`**。本 bug 在 `-race` 下稳定复现、常规模式下 30 次不复现。
2. **CI 偶发失败优先当产品缺陷对待。** 本例中「测试写得不好」是错误结论，真实缺陷影响 `finished_at`、日志流、恢复路径。
3. **两个正确需求打架 = 某个抽象承担了过多语义。** 应拆分语义，而非二选一。
4. **修复一个语义混淆缺陷时，必须搜遍所有依赖该语义的地方。** Bug A 修完就以为完整了，Bug B 是同一个错误假设在另一条路径上的复现。推送侧与拉取侧、主动计算与被动快照，都要检查。
5. **委派出去的 review 没回来，就自己做。** 本次 Bug B 是主 session 自行审查发现的——若因 agent 静默失败而跳过审查，缺陷会留在 v0.3.1 里。

## 10. 遗留风险与验证状态

### 真机验证：进行中，尚未完成

已启动 agent 在 ali-hk-01 测试环境（端口 9180/9181/9190）验证：正常部署基线、Bug A（wave 2 须先 canceled 才 finished）、Bug B（失败后新建 `logs -f` 不提前断流）、重启恢复路径、Web UI 展示。**本文档写作时结果尚未返回**，结论待补。

### 未完全排除的风险

- **是否还有第三处以 `status.Terminal()` 判断结束的代码**：已通过 `grep FollowDeploy` / `grep Done` / `grep FinishedAt` 排查了 `internal/api`、`internal/httpapi`、`internal/webui`、`internal/cli`，未发现其他实例。但这是**关键字级别**的排查，不能保证穷尽。
- **新中间态 `status=failed` 且 `finished_at=nil`**：已确认 `views.Elapsed`（`internal/webui/views/views.go:199`）正确处理 `nil`（显示为仍在计时），且 `finished_at` 在 `internal/api` 中本就是 `omitempty`、running deploy 常态缺失。**API 契约形状未破**，语义反而更准确。但该中间态是新出现的，下游若有未覆盖的假设可能暴露。
- **`RecoverInFlight` 的 SELECT/UPDATE 不在同一事务内**：恢复发生在启动阶段、scheduler 尚未开始调度，实际并发风险低，但严格来说不是原子的。

## 相关文档

- [M4 Web UI 开发计划](../plans/2026-07-21-web-ui-plan.md) — 本次发布的主要内容，日志流相关代码来自该计划
- `kb/docs/deployment-guide.md` — 部署与使用手册
- `docs/json-output.md` — `--json` / HTTP API 输出契约（`finished_at` 语义相关）

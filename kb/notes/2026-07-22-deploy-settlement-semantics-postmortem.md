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

一个在 v0.3.0 发布流程中被 CI 偶发失败暴露出来的状态机缺陷，及其三轮修复。

## 摘要

**起因**：给 v0.3.0 打 tag 时，release workflow 的 `go test` 偶发失败。本地怎么跑都是绿的（后来发现发布前那次「全绿」其实是 `(cached)`），加 `-race` 后稳定复现——确认不是测试不稳，而是 wave 机制引入以来就存在的产品缺陷。

**根因**：`deploy.status` 一个字段被同时当成「当前情况坏了」和「rollout 结束了」两种意思用。第一个实例失败时 status 就变 `failed`，其余实例可能还在跑，于是所有拿 status 判断「是否结束」的地方都提前触发：`finished_at` 记错时刻、`logs -f` 与 Web UI 日志流提前断流、崩溃恢复漏掉半途 rollout。

**过程**：修复走了三轮，每一轮都以为修完了，下一轮又在另一条路径上发现同一个错误假设——Bug A（推送侧，v0.3.0 重推）、Bug B（拉取侧，v0.3.1）、Bug C（恢复侧，`6e34112`，由 review 报告发现，其中一个窗口还是 Bug A 修复自己引入的退化）。最终把语义拆开收敛为一条不变量：**status 回答「现在怎么样」，`finished_at` 回答「结束没有」**。

**验证**：全量代码复查确认本文所有论断属实、无残留误用；`go clean -testcache && go test ./... -race -count=1` 全绿；ali-hk-01 真机六场景全过——正常两波次、Bug B 失败窗口（日志流与 Web UI 均不再提前断）、`kill -9` 真实崩溃恢复、伪造 W1/W2 崩溃残留被正确收尾、未开始的 deploy 不被误杀。

**状态**：代码与验证均已闭环。唯一遗留是**发布 v0.3.2**——Bug C 修复尚在 master 未发布，线上 v0.3.1 仍带 W1「永久残留」缺陷。

| | |
|---|---|
| 发现时间 | 2026-07-22，v0.3.0 发布流程中 |
| 触发方式 | GitHub Actions release workflow 的 `go test ./...` 失败（run 29889733802） |
| 表面症状 | `internal/scheduler` 的 `TestWaveGatingAndCancel` 偶发失败 |
| 实际性质 | 产品缺陷，非测试问题。影响 `logs -f`、Web UI 日志流、`finished_at` 时间戳、重启恢复 |
| 修复提交 | `23d155e`（Bug A）、`1a8c024`（Bug B）、`6e34112`（Bug C + 收敛判据） |
| 涉及版本 | 缺陷自 wave 机制引入即存在；v0.3.0 修了 A，v0.3.1 修了 B，C 待发布 |
| 真机验证 | **已完成**（2026-07-22，ali-hk-01，六个场景全过，见文末「验证状态」） |

> **三轮修复**。每一轮都以为已经完整，下一轮发现同一个错误假设在另一条路径上的复现。§9「经验教训」第 4 条是本文最值得读的部分。

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
10. **取回 review agent 的报告**：该 agent 虽无法投递消息，但其报告内容被用户从其运行痕迹中提取出来（`tmp/code-review-report.md`）。报告**独立发现了 Bug B**（其 Important #1），与主 session 自查结论一致——一次有效的交叉验证。
11. **报告还指出了 Bug C**（其 Important #2）：`RecoverInFlight` 的两个 crash 窗口，主 session 完全没发现，且其中一个是 Bug A 修复引入的退化。核实断言全部成立后修复（`6e34112`），并顺带采纳了报告对判据简化与 Web UI 的建议。

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

> 该修法后被 Bug C 的修复进一步简化为直接判 `cur.FinishedAt != nil`——见 §6 末尾。

## 6. Bug C：崩溃留下的 rollout 无人收尾（含一处由 Bug A 修复引入的退化）

由 review 报告的 Important #2 指出，**主 session 三轮自查均未发现**。

### 缺陷

`RecoverInFlight` 靠「是否存在 dispatching/running 的 execution」来识别被中断的 deploy：

```sql
SELECT DISTINCT deploy_id FROM executions WHERE status IN ('dispatching','running')
```

但崩溃可能让一个**已经开始**的 rollout 一条 in-flight 记录都不剩：

| 窗口 | 崩溃时机 | executions | deploy.status | 是否被收录 |
|---|---|---|---|---|
| **W2** | 标记 wave 1 failed 之后、取消被 gate 的 wave 之前（即原 bug 的毫秒级窗口） | `[failed, queued]` | `failed` | ✗ 无 in-flight 行 |
| **W1** | 两个 wave 之间 | `[succeeded, queued]` | `running` | ✗ 无 in-flight 行 |

两者都不会被 `ListQueuedDeploys` 重新调度——它只查 `status='queued'` 的 deploy。于是**无人取消、无人 recompute**。

### 严重度

- **W2 是 Bug A 修复引入的退化**。修复前，首次失败即 stamp `finished_at`（脏，但 deploy 是闭合的）；修复后 `finished_at` 永为 `NULL`、Done 永不发。Web UI 会显示持续增长的 duration。所幸 `failed` 在 `CleanupService` 的终态列表内，retention 最终会删除，故非永恒。
- **W1 先于本次改动存在，且更严重**：`running` **不在** `CleanupService` 的终态列表 `('succeeded','failed','superseded','unreachable','canceled')` 内——**retention 永远不会回收它**，永久残留。
- 两者都要求进程死在毫秒级区间，频率低。但「原 bug 正是因为该窗口在 CI 中真实出现」，说明窗口并非理论产物。

### 修复（`6e34112`）

改为**以 deploy 为键**的 sweep——未结束、已开始、仍持有未落定 execution：

```sql
SELECT DISTINCT d.id FROM deploys d JOIN executions e ON e.deploy_id = d.id
 WHERE d.finished_at IS NULL AND e.status IN ('queued','dispatching','running')
   AND (d.status != 'queued' OR e.status IN ('dispatching','running'))
```

条件的两半各有职责：
- `d.status != 'queued'` — 收录所有已开始的 rollout（覆盖 W1/W2）
- `OR e.status IN ('dispatching','running')` — 兜住一个边缘窗口：execution 已 dispatching 但 `RecomputeDeployStatus` 尚未把 deploy 从 `queued` 推进。若漏掉，该 deploy 会被 `ListQueuedDeploys` 重新调度，而那条 dispatching 的 execution 因 CAS guard 无法再迁移，将永远停在 dispatching

对收录的每个 deploy：in-flight → `failed`，被 gate 的 queued → `canceled`。**整体从未开始的 deploy 不受影响**，仍由 `Scheduler.Recover` 重新调度。

### 连带简化

搁浅 deploy 被消除后，`finished_at` 成为可信赖的精确 settle 信号，于是：

- `FollowDeploy` 的 attach 判据从 `Status.Terminal() && DeploySettled(id)` 简化为 `cur.FinishedAt != nil`（省一次查询），`DeploySettled` 随之删除
- **Web UI 两处同源缺陷一并修正**（review 指出，先于本次改动存在）：
  - `pages.go:285` `Terminal: d.Status.Terminal()` → `d.FinishedAt != nil`。该字段经 `deploy.templ` 的 `data-terminal` 传给 `logs.js:67`，为 `true` 时走一次性 `replay()` 而非 `follow()`——即失败窗口内打开日志页看不到后续输出
  - `pages.go:67` dashboard 活跃卡片判断 `!d.Status.Terminal()` → `d.FinishedAt == nil`——否则首个实例失败后，仍在运行的 rollout 会从活跃列表中消失

## 7. 修复后的不变量

> **`deploy.status` 回答「现在情况如何」，`finished_at` 回答「rollout 是否结束」。任何「是否结束」的判断都必须读 `finished_at`，绝不能用 `status.Terminal()`。**

对应地：
- `AggregateStatus` — 早失败可见，任一实例失败即 `failed`。**不是**结束信号。
- `model.AllTerminal` — 写入 `finished_at` / 发 Done 的门槛，只在 `RecomputeDeployStatus` 内部使用。
- `finished_at` — 对外唯一的结束判据。消费方（follow 流、Web UI、CLI）一律读它。
- `RecoverInFlight` — 支撑上述不变量的前提：恢复后不得留下无人推进的非终态 execution，否则 `finished_at` 永不写，所有读它的地方都会挂住。

三者是一条链：**恢复保证收敛 → `finished_at` 可信 → 消费方可以只读一个字段**。破坏任一环，另两环失效。

## 8. 测试

均遵循 TDD：先写测试复现失败，再修。

| 测试 | 位置 | 锁定的行为 |
|---|---|---|
| `TestAllTerminal` | `internal/model/model_test.go` | `[failed, queued]` / `[failed, running]` 不算结束；`[failed, canceled]` 算 |
| `TestRecomputeFailureIsLiveButNotFinishedEarly` | `internal/store/store_test.go` | 失败即刻可见，但 `finished_at` 为空且不发 Done；全部落定后才发 |
| `TestRecoverInFlightCancelsGatedWaves` | `internal/store/store_test.go` | 重启后被 gate 的 wave 变 canceled，deploy 能达终态 |
| `TestRecoverInFlightLeavesUnstartedDeploys` | `internal/store/store_test.go` | 从未开始的 deploy 不被误取消，保持 queued 待重新调度 |
| `TestFollowDeployStreamsUntilAllInstancesSettle` | `internal/store/store_test.go` | status 已 failed 时新建流不提前退出，持续收到兄弟实例输出 |
| `TestRecoverSweepsInterruptedRolloutsWithNoInFlightExecutions` | `internal/store/store_test.go` | W1/W2 两个 crash 形态被收尾；已 finished 的 deploy 不被触碰 |
| `TestWaveGatingAndCancel`（改） | `internal/scheduler/scheduler_test.go` | 改用新增的 `waitFinished` helper 等 `finished_at`，而非等 status |
| `mkDeployExec`（改） | `internal/webui/pages_test.go` | fixture 对终态 deploy 同时写 `finished_at`，反映真实数据形态 |

**`waitFinished` 的必要性**：原 `waitStatus` 轮询 status 字段，会在 T1（早可见的 failed）就返回，然后立刻读 executions——正是这个竞态让测试偶发失败。检查 per-instance 状态的测试必须等**真正结束**。

**`mkDeployExec` 为何要改 fixture 而非放宽断言**：Web UI 改用 `finished_at` 判断「是否结束」后，原 fixture 造出的 `status=failed` 且 `finished_at=nil` 在新语义下属于「未结束」，测试因此失败。这不是断言过严，而是 fixture 偏离了真实数据形态——正常路径下 `RecomputeDeployStatus` 保证终态必有 `finished_at`。放宽断言会掩盖语义。

`TestDeployStatusFailureVisibleEarly` 与 `TestAggregateStatus` 的原有用例**全部保留未改**，早可见语义三轮修复中始终未被牺牲。

### 验证强度

```sh
go clean -testcache && go test ./... -race -count=1                      # 全绿（清缓存后）
go test ./internal/store ./internal/scheduler ./internal/webui ./internal/httpapi -race -count=10
```

## 9. 发布影响

| 版本 | 状态 | 说明 |
|---|---|---|
| v0.3.0（第一次） | CI 失败 | tag 已推但无 release 产物，故可安全删除重推 |
| v0.3.0（重推） | 成功 | 含 Bug A 修复，**仍带 Bug B、并引入 Bug C 的 W2 退化** |
| v0.3.1 | 成功 | 含 Bug B 修复，**仍带 Bug C** |
| v0.3.2 | **待发布** | 含 Bug C 修复 + 判据收敛 + Web UI 两处同源修正（`6e34112`） |

v0.3.0 已产生对外产物，故后续只能以新版本跟进，不能重推。

## 10. 经验教训

1. **`(cached)` 的测试结果不是测试结果。** 发布前的「本地全绿」是缓存。发布前应 `go clean -testcache` 或 `-count=1`，并且**跑 `-race`**。本 bug 在 `-race` 下稳定复现、常规模式下 30 次不复现。
2. **CI 偶发失败优先当产品缺陷对待。** 本例中「测试写得不好」是错误结论，真实缺陷影响 `finished_at`、日志流、恢复路径、retention 回收。
3. **两个正确需求打架 = 某个抽象承担了过多语义。** 应拆分语义，而非二选一。
4. **语义混淆类缺陷会在多条路径上复现，而 grep 找不全。** 这是本次最贵的教训——**连续三轮**，每轮都以为完整：
   - Bug A（推送侧：主动计算并 publish）
   - Bug B（拉取侧：被动快照判定）——主 session 自查发现
   - Bug C（恢复侧：崩溃后谁来收尾）——**grep 关键字根本发现不了**，因为缺陷不在「谁用了 `status.Terminal()`」，而在「谁**没有**被纳入恢复范围」
   
   排查语义缺陷不能只搜「谁读了这个字段」，还要问「谁负责让这个字段变成真」。
5. **修复引入的退化，往往藏在被加强的那个不变量的反面。** Bug A 让 `finished_at` 更严格（必须全终态才写），W2 退化恰恰是「有些情况永远达不到全终态」。加强一个前置条件时，必须同时检查**保证该条件可达**的机制是否完备。
6. **委派出去的 review，报告拿不到也要设法取回。** 主 session 因 agent 静默失败而自行审查，找到了 Bug B；但 Bug C 只有 review 报告里有——**若彻底放弃那份报告，W1/W2 会留在生产代码里**，其中 W1 会造成 retention 永不回收的永久残留。agent 通信失败 ≠ 其产出无价值。

## 11. 遗留风险与验证状态

### review 已核实的项（原列为「未排除」，现有依据）

| 项 | 结论 | 依据 |
|---|---|---|
| `--json` / API 契约 | **未破坏** | `docs/json-output.md:170` 将 `finished_at` 定义为「未结束时缺省」的可选字段，无「终态 ⇒ `finished_at` 非空」承诺 |
| CLI `logs -f` 退出条件 | **只按 done 帧退出**，不看 status（`cmd_remote.go:171`），不会提前也不会永不退出 | 唯一例外是 Bug B 的 attach 路径，已修 |
| `views.Elapsed` 对 nil 的处理 | 安全回退到 now（`views.go:199-203`） | — |
| `RecoverInFlight` 的 SELECT/UPDATE 非事务 | **实践中无竞态** | 启动时单线程执行（`cmd_main.go:75`），先于 HTTP listener、先于任何 `Enqueue`（`Enqueue` 在 `Recover` 末尾） |
| Done 事件重复发送 | **无害** | settle 后每次 recompute 都会 publish（单实例成功路径实际 3 次），但每个 follower goroutine 转发首个 Done 后即 return + unsubscribe，publish 非阻塞 |
| 非终态搁浅路径 | 除 W1/W2 外**无搁浅** | shutdown 时 `Acquire` 对已取消 ctx 返回 `ctx.Err()`（`executor.go:88-89`）→ failed；`ErrUnreachable` → unreachable；超时 → failed；`MarkDeploySuperseded` 自写终态 + `finished_at` + Done |

### 真机验证（2026-07-22 完成，ali-hk-01）

被测二进制：`v0.3.1-1-g6e34112-dirty`（`6e34112` + 未提交的取消原因文案改进），main + edge 全链路。为复现 Bug B 窗口，`deploy-test/hookploy.yaml` 新增了可复用的 `echo_bugb` 服务：单 wave 三实例（s1 串行慢成功 → s2 慢/f1 秒级失败并行，靠 per-instance `dir` 放不同 `task.sh` 实现），脚本在服务器 `/opt/apps/hookploy_test/bugb/`。

| 场景 | 做法 | 结果 |
|---|---|---|
| S1 正常两波次 | echo_multi（wave 1 本机 → wave 2 edge/gRPC） | ✅ `finished_at`（…05.712）晚于最后 execution settle（…05.710） |
| S2 Bug B 窗口 | echo_bugb；f1 失败后 s2 仍跑 25s，窗口内取证 | ✅ 窗口内 status=failed、finished_at 缺省、s2 running；`logs -f` 在 f1 失败后继续收 s2 全部 25 条 tick，settle 才收 done 帧退出（exit 1）；Web UI dashboard 窗口内保留 active-card、deploy 页 `data-terminal="false"`，落定后 active 清空 |
| S3 真实崩溃 | s1 running 时 `kill -9` main，重启 | ✅ running→failed("main restarted")、queued→canceled、`finished_at` 恢复时即写入；新 attach 的 `logs -f` 重放后立即收 done |
| S4 W1 伪造 | sqlite3 直造 `[succeeded, queued]` + deploy running/未结束 | ✅ 被 sweep：queued→canceled，deploy failed + `finished_at`（v0.3.1 下会永久残留 running） |
| S5 W2 伪造 | 直造 `[failed, queued]` + deploy failed/未结束 | ✅ 被 sweep 收尾，`finished_at` 写入（v0.3.1 下永不写） |
| S6 未开始 deploy | 直造全 queued 的 deploy | ✅ 未被误取消——被重新调度并**真实执行**（execution 有 started_at，error 是 op 自身的 "exit 1" 而非恢复取消） |

### 仍未排除的风险
- **`TestRecomputeFailureIsLiveButNotFinishedEarly` 的 `select/default` 存在理论假阴性窗口**：publish 同步入 sub 缓冲，但转发到 out 由 follower goroutine 异步完成，`default` 可能先赢。review 判断可接受——同分支内的 `finished_at` 断言会确定性捕获现实回归，只有「把 publish 单独移出该分支」的假想回归才会漏。
- **`AggregateStatus` 尾部分支**：全终态且混合、但无 failed/unreachable/canceled（即 succeeded + superseded 混合）时返回 `StatusRunning`——一个非终态。实际不可达（`MarkDeploySuperseded` 整体操作 deploy，不会与 succeeded 混），故三轮修复均未触碰，但逻辑上是个死角。

## 相关文档

- [M4 Web UI 开发计划](../plans/2026-07-21-web-ui-plan.md) — 本次发布的主要内容，日志流相关代码来自该计划
- `kb/docs/deployment-guide.md` — 部署与使用手册
- `docs/json-output.md` — `--json` / HTTP API 输出契约（`finished_at` 语义相关）
- `tmp/code-review-report.md` — reviewer agent 对 `23d155e` 的完整报告，Bug C 的唯一来源。该 agent 无法投递消息，报告由用户从其运行痕迹中提取

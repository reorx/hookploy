---
created: 2026-08-10
tags:
  - hookploy
  - ops
  - config
  - scheduler
  - migration
---

# hookploy：op 级 instance 定向（step 加 `on:` 修饰符）

**状态：已实现（2026-08-10）**。三个阶段全部落地，`go test ./...` 全绿，真机（ali-hk-01 测试环境 9180）双实例验证通过：定向步骤只出现在 main 的快照与日志里，edge 的快照连索引都是重排后的（不是"跳过"）；定向步骤失败时 wave 1 failed、wave 2 canceled（"earlier wave failed"）、容器不动。实施中相对本计划的两处偏差记在文末「实施记录」。

## Context

动机是把 migration 从「应用启动时自动检查并应用」拆成部署流水线里的显式一步（`compose.run` 跑 migrate → `compose.up`），获得独立日志、独立失败语义（migrate 挂 → 流水线停在 `compose.up` 之前 → 旧版本继续跑 → deploy failed + 通知）。单节点服务现有词汇表已够用（testdata 的 breeze 即范式）。

缺口在多节点服务（vocalflow 形态）：`deploy:` 的 ops 每个 instance 都执行一遍，migrate 会跑 N 次——存在波次内并发竞态（同一波两台并行跑 migrate，Django 等工具无锁保护）、且要求所有节点都能连 DB。

**用户已定的决策**（2026-08-10 讨论）：

1. 不做隐式 `once`（"rollout 第一个 instance"这种位置依赖的语义被明确否掉）；要**显式指定** op 只在哪些节点执行。
2. `on:` 里写 **instance 名**，不是 server 名——instance 才是配置的寻址单位（rollout 用的就是它，dir 也是 per-instance），且天然唯一；同 server 跑一个服务两个 instance 时 server 名有歧义。
3. 失败语义靠现有波次门控（scheduler.go:121-127，前波失败 → 后续波次 skipped "earlier wave failed"）：migrate 定向到第一波的中心节点，挂了则后续波次一台不动。**scheduler 执行侧零改动**。

关键事实（已核实）：`BuildDeploy`（scheduler/build.go:35）目前一份 `opsJSON` 快照所有 execution 共用；normalizeService 里 instances 的解析（config.go:324-343）先于 deploy 流水线解析（:388），validatePipeline 拿到的 `svc` 已有完整 instance 表。

## 设计

### 语法

step 现有两种形式（字符串 / 单键 map，parse.go:15）。新增第三种：**双键 map**，第二个键只认保留字 `on`：

```yaml
vocalflow:
  deploy:
    - image.pin
    - compose.run:
        service: api
        argv: [alembic, upgrade, head]
      on: [main]              # 只在 instance main 上执行
    - compose.up
    - healthcheck: { url: "http://127.0.0.1:3030/healthz" }
  instances:
    main:    { server: ali-hk-01 }
    api-sg0: { server: tc-sg-01 }
    api-hk0: { server: hh-hk-01 }
  rollout:
    - main
    - [api-sg0, api-hk0]
```

- `on` 的值：字符串或字符串数组（`on: main` 与 `on: [main]` 等价，归一化为 `[]string`）。
- 无参 op 要配 `on` 时用空值 map 形式：`- image.pin:` + `  on: [main]`（argsNode 为 null scalar，newStep 需按 nil 处理——现在 decodeStrict 只认 mapping）。
- 键序不敏感（`on` 在前也合法）；除一个注册 op 名 + 可选 `on` 外出现任何其他键报错。yaml.v3 按 1.2 解析，`on` 是普通字符串键，无布尔坑。

### 语义与过滤位置

- `ops.Step` 加 `On []string` 字段。**不进 JSON 线格式**：MarshalJSON/UnmarshalJSON（json.go）不动，`{op, args}` 快照格式零变化。
- 过滤发生在 **`BuildDeploy` 入队时**：插值一次，然后按 instance 过滤（`len(On)==0` → 所有 instance；否则仅列出的 instance）、**逐 instance 序列化各自的 `opsJSON`**。每个 execution 的快照只含自己真实要跑的 op。
- 因为快照生成前已过滤，下游全部照旧：engine / edge / pb / edgewire / executor / 冻结的 `internal/api` 一个都不碰；DB 里存的、Web UI 部署详情展示的 per-execution op 列表天然准确（非目标节点上根本没有这个 op，而不是"跳过"）；in-flight 快照语义不变；恢复路径（DB 反序列化 Step）不见 `On`，天然正确。
- 波次首实例的 digest 提升（scheduler.go:134-146）按 execution 走，与 op 内容无关，不受影响。

### 校验规则（config 加载 / `hookploy validate` 时报错，不留到部署时）

在 `validatePipeline`（config.go:415，已持有 `svc`）中：

1. `on` 里的名字必须是该服务已声明的 instance，拼错即报错（带行号，Step.Line 已有）。
2. 过滤后任一 instance 的 deploy 流水线为空 → 报错（配置写错的信号）。
3. `tasks:` 里禁用 `on`（config.go:396-406 的 task 解析循环处检查）：task 触发时本就用 `--instance` 选目标，两套定向叠加只会混乱。

## 分阶段实施（BDD：每阶段先写行为测试）

### Phase 1 — 解析与校验（internal/ops + internal/config）

- 测试先行：ParseStep 双键 map（op+on、on 在前、scalar/数组两种 on 值、无参 op 空值形式、未知第二键报错、三键报错）；config 校验（未知 instance 名、instance 流水线过滤后为空、task 带 on）三条规则各自报错且带行号。
- 实现：`Step.On`；`ParseStep`/`newStep` 扩展（含 null argsNode 处理）；`validatePipeline` 三条规则。
- JSON round-trip 测试确认 `On` 不泄漏进线格式。

### Phase 2 — 入队过滤（internal/scheduler）

- 测试先行：`BuildDeploy` 多 instance 服务 + 带 `on` 的 step → 各 execution 的 `OpsJSON` 内容断言（目标 instance 含该 op、其余不含、无 `on` 的 op 人人都有）；单 instance 服务带 `on`（指到自己）不受影响；task 路径无 `On` 照旧。
- 实现：`BuildDeploy` 内 per-instance 过滤 + 逐个 marshal（filter 辅助函数放 build.go 即可，不必进 ops 包）。

### Phase 3 — Schema 与文档

- `schema.go` stepSchema()：OneOf 的 map 分支加可选 `on` 属性（string 或 string 数组），描述写清"instance 名"；`hookploy schema` golden 若有则 `go test ./internal/cli -update`。
- `testdata/hookploy.yaml`（PRD §4 完整示例）给 vocalflow 加 migrate 步骤示范 `on:`——它被 config 测试引用，改动后跑全量测试确认无 golden 断裂。
- `kb/docs/deployment-guide.md`：op 词汇表段落加 `on:` 修饰符说明 + 多节点 migration 推荐模式（migrate 定向第一波中心节点 + 波次门控失败语义 + expand-contract 纪律 + 应用侧改为 `migrate --check` 只查不应用）。

## 兼容性

- 纯增量：不带 `on` 的配置行为逐字节不变（单快照 → 逐 instance 快照的重构对无 `on` 服务产出相同内容）。
- 契约全部不动：`internal/api` DTO、`--json`、pb/edgewire 线格式、DB schema。唯一外显变化是 `hookploy schema` 输出多一个可选字段（向后兼容增量）。
- 新 config + 旧 binary：旧版 ParseStep 会对双键 map 报 "op step must be a single-key map"——即 reload/validate 直接失败、不会静默错跑。升级顺序：先升 main binary 再改配置。edge 不感知（收到的是过滤后快照）。

## 验证

1. `go test ./...` 全绿（真跑勿信 cached）。
2. 真机（ali-hk-01 测试环境 9180，规程见 CLAUDE.md）：echo_server 改成双 instance（local + edge-01）、加一个 `run: {argv: [echo, migrated]}` 步骤 `on:` 定向到第一波 instance；触发部署后用 `hookploy deploys echo_server` + Web UI 部署详情确认：目标 execution 有该 op 及其日志，另一 execution 没有；再验证定向 op 失败（argv 换 `[false]`）→ 第一波 failed、第二波 skipped（"earlier wave failed"）、echo_server 旧容器仍在跑。截图归档 `./tmp/<date>-op-instance-targeting/`。
3. `hookploy validate` 对三种坏配置（拼错名 / 空流水线 / task 带 on）给出可读报错。

## 实施记录（2026-08-10）

两处相对计划的偏差，都是实现时发现的更小/更正确的做法：

1. **过滤辅助函数进了 `ops` 包**（计划说放 build.go）。config 的「过滤后流水线为空」校验也要同一个谓词，两处各写一份即是两份定义。落地为 `Step.RunsOn(instance)` + `ops.StepsFor(steps, instance)`，scheduler 与 config 共用。
2. **`image.pin` 的「后面必须有 compose.up」改为逐 instance 校验**。`on:` 让某 instance 实际跑的流水线成为写下来那条的子集，一个被定向走的 `compose.up` 无法验证该 instance 的 pin——不逐 instance 检查就会漏掉这类白 pin。无 `on:` 的配置行为不变（过滤后等于原列表）。

顺带产生的行为放宽：`- compose.up:`（空值 map、无 `on:`）此前报 "expected a mapping"，现在等同无参形式——这是 `- image.pin:` + `on:` 写法的必然推论，schema 的 op 参数节点同步接受 null。

真机证据归档在 `tmp/2026-08-10-op-instance-targeting/`（成功/失败两次部署的 `/deploys/<id>` JSON、日志、Web UI 部署详情 HTML、三份坏配置）。Web UI 是服务端渲染，取的是页面 HTML 而非截图——避免把测试环境 admin token 送进浏览器会话。

## 非目标

- 隐式 `once` / "第一波自动"语义（已明确否掉）
- server 名寻址、按 server 标签/分组定向
- `on` 用于 `tasks:`（显式禁用）
- migration 工具层面的封装（plan 输出、`--check` 门控等属应用侧/配置写法，不进词汇表）
- 超长 migration（大表回填）的支持——仍走 `tasks:` 手动触发，不进部署流水线

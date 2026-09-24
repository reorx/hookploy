---
created: 2026-09-24
tags:
  - hookploy
  - ops
  - config
  - scheduler
  - reliability
---

# hookploy：op 级超时 + 重试（拉取卡死不该吃光整条流水线的预算）

**状态：待实施（诉求已定，未做代码调研）**。本文只把要解决的问题、期望的行为和边界写清楚；具体改哪些包、Step 结构怎么扩、executor 怎么杀子进程，留给实施时调研。

## Context：2026-09-24 vocalflow-rt `dp_1a0d2a67f8ec5336195` 失败

事实链（全部核实过，细节见 deploy 仓库 AGENTS.md 历史 2026-09-24 条）：

- 波 1 main @ ali-hk-01 一切正常：拉 127 MB 镜像（16 层）+ recreate + healthcheck + smoke，共 86 秒。
- 波 2 两个 API 节点（hh-hk-01 / tc-sg-01）同时开始 `image.pin`。两台都在 **7 秒内落盘 16 层里的 15 层**（含 56 MB 基础层），唯独 65 MB 的应用层 `67bfc03ddd51` 卡住：两台机器（不同机房、不同运营商）**停在完全相同的字节偏移 31,457,280 = 30 MiB**，之后再无进展。
- 10 分钟整，两台 edge 报 `execution … failed: op 1 (image.pin): context deadline exceeded`——这是 `defaults.timeout: 10m`（**execution 级**总超时）。dockerd 侧只留下 "Cancel with lease"，说明 pull 是被 hookploy 的 ctx 取消的，docker 自己从未放弃。
- 几分钟后从两台节点用同一凭证拉同一个 blob：8–52 MB/s，几秒完成。手动 `hookploy deploy` 同 digest 重试一次即 succeeded。

结论：**GHCR 的 CDN 对单个 blob 的一次瞬态卡死（推测是边缘节点缓存了半个对象、回源填充挂住），被放大成了一次生产发布失败**。放大的机制有两层：

1. `docker pull`（containerd 存储模式）对停滞的连接没有「无进度超时」，会无限等。
2. hookploy 只有 execution 级 timeout（`defaults.timeout` / 服务级 `timeout`），没有 op 级 timeout、也没有重试。一条卡住的连接就把整条流水线的 10 分钟预算吃光，而这类故障换一条新连接大概率立刻好（当天复测就是证据）。

单纯把服务级 `timeout` 拉长到 20 分钟解决不了「卡死」型故障，只会让失败来得更晚。需要的是**更早止损 + 自动换连接重来**。

## 目标（诉求）

### 1. op 级 `timeout`

- 单个 step 可以声明自己的超时；到点就取消这一步（杀掉子进程，回收 docker 客户端的 ctx），而不是等 execution 总超时。
- 语法上沿用 step 修饰符的既有形式（`on:` 那种双键 map），例如：

  ```yaml
  deploy:
    - image.pin:
      timeout: 3m
      retries: 2
    - compose.up
    - healthcheck: { url: "http://127.0.0.1:3030/api/health", retries: 10 }
  ```

- 无参 op 要带修饰符时同 `on:` 的空值 map 写法。
- execution 级 timeout 仍然是总兜底，不取消、语义不变。

### 2. op 级 `retries`

- step 失败（含 op timeout）后自动重跑，最多 `retries` 次；每次重试是**全新的一次执行**（新进程、新连接），不是在旧连接上等。
- 只对**幂等**的 op 有意义。`image.pin` 是幂等的（按 digest 拉取 + 本地 retag + 校验），是本诉求的首要目标；`compose.pull`、`healthcheck`（已有自己的 retries 参数，别做成两套语义）、`artifact.extract` 也天然可重试。`compose.run` / `run` 这类可能带副作用的 op，**默认不允许重试**，或者要求显式声明才允许——这个边界实施时定，但必须有边界。
- 所有重试总耗时仍受 execution timeout 约束：op timeout × (retries+1) 超过 execution timeout 时，`hookploy validate` 应当报错或至少警告，不要留到部署时才发现「重试还没跑完就被总超时掐了」。

### 3. 可观测

- deploy 日志里重试必须可见：每次尝试开头一行 `image.pin attempt 2/3（上次：context deadline exceeded after 3m0s）` 之类；最终失败的错误要带 op 名 + 已尝试次数 + 每次的失败原因。
- Web UI 部署详情里 op 的状态能看出「重试后成功」与「一次成功」的区别（哪怕只是日志里能看出来；不要求新字段，contract 冻结的 `internal/api` / `--json` 尽量不动，需要动时单独提出来）。
- Telegram 通知不需要为重试单独发消息；失败通知里带上「第 N 次尝试后失败」即可。

### 4. 默认值与 deploy 仓库侧的接线

- 建议默认：`image.pin` 不设 op timeout 时仍走 execution timeout（**向后兼容，现有 hookploy.yaml 行为不变**）。
- 发布后在 deploy 仓库 `roles/hookploy/templates/hookploy.yaml.j2` 里给 vocalflow-rt（以及其他镜像 CD 服务）的 `image.pin` 加上 `timeout` + `retries`。参考数据：三台正常拉取 127 MB 的耗时是 7–40 秒；`timeout: 3m` + `retries: 2` 总上限 9 分钟，落在现有 10 分钟 execution timeout 之内。是否顺手把 `defaults.timeout` 抬到 15 分钟，实施时一起定。
- 可选：`defaults:` 里允许按 op 类型给默认修饰符（例如 `defaults.ops: { image.pin: { timeout: 3m, retries: 2 } }`），避免每个服务重复写。**不是必须**，先做 per-step 形式。

### 5. 进阶（可选，不阻塞上面四条）：拉取停滞检测

固定 timeout 的缺点是「正常慢」和「彻底卡死」一视同仁。`docker pull` 的进度输出是可以观察的（本次事故里能看到每一层 `Download complete` / `Pull complete` 的推进）。一个更聪明的止损是：**连续 N 秒没有任何进度行就判停滞、立即进入重试**，不用等满 timeout。是否做、怎么做（解析 CLI 进度 vs 走 Docker API 的 progress stream）留给实施调研；这里只记录它是比固定 timeout 更好的方向。

## 非目标

- 不做 pull-through cache / registry mirror / 从 main 节点分发镜像（用户明确暂不考虑）。
- 不改 docker daemon 配置（`max-download-attempts` 在 containerd 存储模式下的行为未核实，也不归 hookploy 管）。
- 不改 execution 级 timeout 的语义和 SSE 流断容忍逻辑。

## 验收标准

1. 单测/集成测试：一个会卡住不退出的假 op（或假 `docker` 二进制挂在 PATH 上）→ 到 op timeout 被取消 → 自动重试 → 第二次（假 op 改为成功）通过 → execution succeeded，日志里两次尝试都可见。
2. 重试耗尽 → execution failed，错误信息含 op 名 + 尝试次数 + 每次原因；后续波次按既有门控 canceled。
3. `hookploy validate`：不幂等 op 声明 `retries` 报错（或按实施定的边界处理）；op timeout×(retries+1) > execution timeout 报错或警告；未知修饰符键报错（沿用 `on:` 的严格解析）。
4. 向后兼容：不带新修饰符的现有 hookploy.yaml（deploy 仓库当前渲染结果）行为与日志完全不变。
5. 真机验证（ali-hk-01 测试环境，同 `2026-08-10-op-instance-targeting-plan.md` 的做法）：用 `ss -K` 或 tc/iptables 把一次真实 `image.pin` 的下载掐断/冻结，观察到 op timeout → 重试 → 成功。
6. 发布：出带版本的 GitHub release，deploy 仓库按既有 SOP 升级三台（先 main 后 edge），然后给 vocalflow-rt 的 `image.pin` 加修饰符并用同 digest 手动 deploy 一次验证。

## 待用户拍板的问题

1. `retries` 的边界：只对白名单 op（image.pin / compose.pull / artifact.extract）开放，还是任何 op 都能声明、由写配置的人自负其责？（倾向：白名单 + 明确报错，和 `on:` 禁用于 `tasks:` 的风格一致。）
2. `healthcheck` 已有自己的 `retries` 参数（轮询语义）。新的 step 级 `retries`（重跑语义）与它同名会不会混淆？是否改名为 `attempts` 或 `retry:`？
3. 第 5 节的停滞检测是否纳入本期。

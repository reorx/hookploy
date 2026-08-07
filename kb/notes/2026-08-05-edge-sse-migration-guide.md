---
created: 2026-08-05
tags:
  - migration
  - edge
  - sse
  - transport
  - cloudflare
  - deploy-ops
---

# 把生产 edge 从 gRPC 切到 SSE 通路

**读者**：deploy workspace 的运维 agent（`~/Library/Mobile Documents/com~apple~CloudDocs/deploy`）。本文只讲「怎么把线上两台 edge 切过去、怎么验证、出问题怎么退」，hookploy 本身的用法见 `kb/docs/deployment-guide.md`。

> ✅ **已执行完毕（2026-08-08）。** 生产三台随 **v0.5.0**（含 notify 功能）对齐，两台 edge 均已切到 SSE：升 main（ali）→ hh 切 sse → tc 切 sse，各步验证通过；SSE 通路真实部署（condenser `dp_19fdcf584`）succeeded、日志完整回传。断连基线 1372 次/24h（gRPC，2026-08-07 实测），切换后短窗为 0；§3.4 的 24h 观察窗口因用户要求一次到位而压缩，24h 级对照数据看 deploy workspace 后续记录。本文保留作回退（§5）与排障（§6）手册。

## 1. 为什么要做这件事

### 1.1 现象

edge↔main 的 gRPC 长连接经 Cloudflare 橙云，**每天被掐断约 1370 次**（秒级自动重连），自 2026-07-19 走 CF 443 起一直如此。根因是 CF 对**所有**代理流量都有约 100s 的空闲超时，且**不认 HTTP/2 PING 帧**——两侧本来就配了传输层 keepalive（main 30s/10s、edge 30s/10s），对 CF 完全无效，因为 PING 不被 CF 计作「流量」。

### 1.2 为什么这不是「无害的重连」

平时确实无害。但**断连一旦落在发版窗口内**，状态回传被打断，main 立即把该执行判 `failed` —— 而 edge 上的部署实际已经成功。

已发生过：**2026-07-29 深夜 vocalflow-rt `dp_19faeb11`**，记 `failed`，但三个实例实际全部收敛到新 digest 且健康。

这就是 deploy workspace `TODO.md` 第 1 条，以及 `AGENTS.md`「CI/CD → hookploy 部署」里那条过渡期 SOP 的由来：

> 看到 hookploy deploy failed，先核对各实例真实 digest + 健康再决定重跑。

一个需要人工二次确认才敢信的部署系统，可用性是打折的。

### 1.3 为什么是「新增 SSE 通路」而不是别的办法

讨论过、**被否决**的两条路：

- **关灰云 / 绕开 CF**：这是绕行不是根治。换任何协议、任何链路都可能断连，要解决的是「断连导致部署被误判」本身。
- **只加 gRPC 应用层 Ping**：治不了。CF 不认 h2 PING；就算换成应用层消息帧能保活，也解决不了「真的断了怎么办」。

采用的方案是**新增一条 SSE/HTTP 通路，与 gRPC 并存**，两条通路走完全相同的内部逻辑（main 侧抽出传输无关核心 `internal/edgehub`，edge 侧抽出 `agent.go`）。这样做同时拿到两件事：

1. **保活**：SSE 下行每 45s 发一个注释帧（`: hb`），是**真实流量**，任何代理都认，稳在 CF 的 ~100s 空闲阈值之内。
2. **流断容忍**（这才是关键）：SSE 的结果回传走**独立的 POST 请求**，不依赖下行流。所以下行流被掐断时，执行照常在 edge 上跑完，结果照样 POST 得回来。main 侧则为已下发的执行保留 60s 宽限，不立刻判失败；edge 重连时声明自己还在跑哪些执行，main 认领，收到补报结果后正常结算。

**gRPC 通路做不到第 2 点**：它的 wire 里既没有 DoneAck 也没有 Hello.inflight，跨会话保留的执行永远无法被确认或认领，只会积压。所以 gRPC 维持 `Resumable=false`（断连即失败）的现状语义，并走向 deprecation。**容忍收益只在 SSE 通路生效，这正是要迁移的理由。**

### 1.4 迁移后能得到什么

| | 迁移前（gRPC） | 迁移后（SSE） |
|---|---|---|
| 每天断连次数 | ~1370 | 预期接近 0（45s 心跳 < CF ~100s） |
| 断连落在发版窗口 | deploy 记 failed（**实际成功**） | 执行不中断，重连补报，记 succeeded |
| edge 真死 | 立刻 failed | 60s 宽限后 `unreachable`（CI 可重跑） |
| 反代要求 | CF zone 的 gRPC 开关必须开着 | 普通 HTTPS，**无需任何开关** |
| 过渡期 SOP | 必须逐次人工核对 digest | 可退休（两台都切完并观察后） |

---

## 2. 前置条件

### 2.1 release 已就绪（v0.4.0）

hookploy repo 侧已完成：`feat/transport-decouple-sse` 已合入 master，**v0.4.0** 已发布（<https://github.com/reorx/hookploy/releases/tag/v0.4.0>），产物为 binary + `hookploy-ctl.sh` 的 tarball。

在 deploy workspace 更新 `ansible/group_vars/all.yml`：

```yaml
hookploy_version: v0.4.0
hookploy_release_sha256: 46713fe926e16537a0c006085e7c0cdb62eec082beca4cf864d739e9d3a21671  # hookploy-v0.4.0-linux-amd64.tar.gz
```

### 2.2 升级顺序不可颠倒：先 main，后 edge

- **新 main + 旧 edge（gRPC）**：完全兼容，wire 与握手都没动。
- **旧 main + 新 edge（`--transport sse`）**：main 上根本没有 `/edge/session` 这个路由，返回 404。edge 会打醒目日志提示后按退避持续重试，**不会自动回退到 gRPC**（静默切换只会掩盖配置错误）。

所以：`--limit ali-hk-01 --tags hookploy`（main）跑完并确认 active 之后，再动 edge。

### 2.3 升级 main 本身就会改变一个行为（即使 edge 还在 gRPC）

新 main 上线后，**所有** edge（含仍走 gRPC 的）都适用统一的 60s 宽限：

- edge **真死**（进程没了、机器没了）：从「立刻 failed」变成「**60s 后 unreachable**」。语义更准（main 从未得知结果，不该替它下结论），且 `unreachable` 是 CI 可重跑的状态。
- edge **瞬断秒级重连**（gRPC 的常态）：重连时 gRPC 恒定不声明 inflight，main 据此判定该执行已丢失，**立即判 failed**，不空等 60s。时序与现状几乎一致。

也就是说：**升级 main 不会让 gRPC 的误判问题变好，只会让「真死」的状态名更准确。真正的收益要等 edge 切到 SSE。**

### 2.4 反代与 CF：不需要改任何配置

已核对 `ansible/roles/caddy/templates/Caddyfile.j2` 的 hookploy vhost：

```
@grpc protocol grpc
reverse_proxy @grpc h2c://localhost:{{ hookploy_grpc_port }}   # 9101
reverse_proxy localhost:{{ hookploy_http_port }}               # 9100
```

SSE 是普通 HTTPS 请求，不匹配 `@grpc`，**自动落到第二条 → 9100**，正是 main 的 HTTP 口。所以：

- **Caddy 无需改动**。Caddy 的 `reverse_proxy` 对 `Content-Type: text/event-stream` 会自动立即 flush，不缓冲；hookploy 自身也发了 `X-Accel-Buffering: no`。
- **CF 无需改动**。zone 的 Network → gRPC 开关切完之后其实可以关掉（等 gRPC 通路正式下线再说，现在留着不碍事）。
- **`--main` URL 不变**，仍是 `https://hookploy.reorx.com`。`group_vars` 里 `hookploy_grpc_url` 与 `hookploy_main_url` 本来就是同一个值。

> 变量名 `hookploy_grpc_url` 在切 SSE 后会名不副实。可以改用 `hookploy_main_url`（同值）或改名，但这是一次纯机械的重命名，**建议与本次迁移分开做**，别把两件事混在一个变更里。

### 2.5 唯一真正未验证的点：心跳能否穿透 CF

真机验证是在 ali-hk-01 本机 loopback（9180/9181）上做的，**没有经过 CF**。45s 注释帧穿透 CF 是设计推断，不是实测结论。

所以第一台切过去之后，**首要观察项就是断连次数**（见 §4.3）。如果心跳没穿透，表现会是：edge 每 ~100s 断一次重连，日志里能直接看出来。此时先回退（§5），再回 hookploy repo 调心跳间隔。

---

## 3. 执行步骤

线上两台 edge：**tc-sg-01** 与 **hh-hk-01**（systemd `hookploy-edge`，role `hookploy-edge`）。**一台一台来**，第一台观察够了再动第二台。

建议先切 **hh-hk-01**：它上面是 condenser / panplayer，比 tc 的 vocalflow API 节点（api.sg0）低风险。

### 3.0 可选：先做一次零改动的快速探针

edge 的 systemd unit 有 `EnvironmentFile=/opt/apps/hookploy/edge.env`，而 `--transport` 支持 `HOOKPLOY_TRANSPORT` 环境变量。所以想先摸一下能不能通，可以不动 ansible：

```sh
ssh hh-hk-01 'printf "HOOKPLOY_TRANSPORT=sse\n" >> /opt/apps/hookploy/edge.env && systemctl restart hookploy-edge'
```

这是**配置漂移**，只适合当探针。摸完要么按 §3.1 正式化，要么把这行删掉。

> ⚠️ 如果两者都设了，**命令行 flag 优先**（env 只在 flag 为空时兜底）。所以做完 §3.1 之后一定要把 edge.env 里这行删掉，否则以后看代码会以为它在生效，实际不是。

### 3.1 改 ansible（SSOT）

**改 1** — `ansible/group_vars/all.yml` 加一个变量，默认保持现状：

```yaml
# edge 连 main 的通路：grpc（默认，走 9101）或 sse（走 9100 的 HTTP 口）。
# sse 独享流断容忍：CF 掐断下行流时执行不中断、重连后补报结果。
hookploy_edge_transport: grpc
```

**改 2** — `ansible/roles/hookploy-edge/templates/hookploy-edge.service.j2` 的 ExecStart 加上 flag：

```
ExecStart={{ apps_dir }}/hookploy/hookploy edge --main {{ hookploy_grpc_url }} --transport {{ hookploy_edge_transport }}
```

这两步合起来是**零行为变更**（显式写出当前默认值），可以先提交、先下发，确认两台 edge 照常 online。

### 3.2 切第一台（hh-hk-01）

用 `-e` 覆盖，**不动 group_vars**——这样迁移窗口内 repo 状态始终自洽，别的 tag 顺带跑到这台也不会意外翻转：

```sh
ansible-playbook playbook.yml --limit hh-hk-01 --tags hookploy-edge -e hookploy_edge_transport=sse
```

template 变更会 notify handler 重启 `hookploy-edge`。

### 3.3 验证（逐项做完再进下一台）

**① edge 侧握手**

```sh
ssh hh-hk-01 'journalctl -u hookploy-edge -n 20 --no-pager'
# 期望： connected to main <版本> as server "hh-hk-01"
# 若见 404 提示 → main 还没升级（§2.2）
```

**② main 侧确认 transport**

```sh
ssh ali-hk-01 'journalctl -u hookploy -n 50 --no-pager | grep -E "connected|disconnected"'
# 期望： edge "hh-hk-01" connected (version <版本>, transport sse)
```

**③ CLI 状态**

```sh
ssh ali-hk-01 'cd /opt/apps/hookploy && export HOOKPLOY_URL=http://127.0.0.1:9100 HOOKPLOY_ADMIN_TOKEN=$(cat .admin_token) && ./hookploy status'
```

两台 edge 都应 online、版本对齐。注意 `hookploy status` **不显示 transport**——`internal/api` 的 DTO 契约已冻结，没有为此加字段。要看 transport 就看 ② 的日志或 ④ 的 Web UI。

**④ Web UI**

`https://hookploy.reorx.com/ui/`（admin token 登录），服务器一栏应显示 `edge 在线 (sse)`。

**⑤ 走一次真实部署**

用这台 edge 上**最低风险**的服务触发一次：hh 选 condenser 或 panplayer，tc 选 nce-class。

```sh
ssh ali-hk-01 'cd /opt/apps/hookploy && export HOOKPLOY_URL=http://127.0.0.1:9100 HOOKPLOY_ADMIN_TOKEN=$(cat .admin_token) && ./hookploy deploy condenser && ./hookploy deploys condenser'
```

⚠️ 这是**真实重新部署**（容器 recreate）。hookploy rollout 无 drain，在途连接会断、约 30s 内回归。不想动线上就等下一次自然的 CI 发版，只是验证周期会拉长。

确认 `succeeded`，且 `hookploy logs <deploy_id>` 里日志完整回传（说明 POST 通路正常）。

### 3.4 观察窗口

**至少留 24h** 再动第二台。要看的就一件事——断连次数（§4.3）。

### 3.5 切第二台（tc-sg-01）

同 §3.2，`--limit tc-sg-01`，然后重跑 §3.3 全部验证项。

### 3.6 收尾：让 repo 与现实一致

两台都稳定之后，把默认值翻过来并重新下发（对已切的机器是 no-op re-render）：

```yaml
hookploy_edge_transport: sse
```

```sh
ansible-playbook playbook.yml --limit tc-sg-01,hh-hk-01 --tags hookploy-edge
```

顺带把 §3.0 探针留下的 `HOOKPLOY_TRANSPORT` 行从 edge.env 删掉（如果用过）。

### 3.7 收尾：更新文档

- deploy workspace `TODO.md` 第 1 条 → 移入「✅ 已完成」，写清实测断连次数变化
- deploy workspace `AGENTS.md`「CI/CD → hookploy 部署」→ 两处 ⚠️（CF gRPC 开关、每 2 分钟断连）改写为现状；**过渡期 SOP 可以退休**
- 各服务文档如提到「部署 failed 先核对 digest」，一并更新

---

## 4. 判断迁移是否真的成功

### 4.1 成功判据

不是「edge online」——那个 gRPC 也一直是 online。**真正的判据是断连次数掉到接近 0。**

### 4.2 参照基线

迁移前 main 日志里每台 edge 每天约 1370 条 `disconnected`。

### 4.3 观察命令

```sh
ssh ali-hk-01 'journalctl -u hookploy --since "24 hours ago" --no-pager | grep -c "hh-hk-01\" disconnected"'
ssh ali-hk-01 'journalctl -u hookploy --since "24 hours ago" --no-pager | grep -c "tc-sg-01\" disconnected"'
```

只切了一台时，这正好是一组**对照实验**：切了的那台应接近 0，没切的那台仍在 1000+ 量级。这个对比比任何单独数字都有说服力。

### 4.4 终局验证

等一次**真实发版恰好撞上断连**。SSE 下应该看到 main 日志里这三行连着出现，而 deploy 仍是 `succeeded`：

```
edge "xxx" disconnected, holding 1 execution(s) for up to 1m0s
edge "xxx" connected (version ..., transport sse)
edge "xxx" resumed 1 in-flight execution(s)
```

这三行就是 §1.2 那个事故场景的正确结局。（真机已在 ali-hk-01 测试环境用 `ss -K` 掐 TCP 复现验证过，但那是本机 loopback，没经 CF。）

---

## 5. 回退

### 5.1 回退 transport（保留新 binary）

**这是首选回退路径**，秒级、无副作用：

```sh
ansible-playbook playbook.yml --limit hh-hk-01 --tags hookploy-edge -e hookploy_edge_transport=grpc
```

或直接在机器上 `systemctl edit` / 改 unit 后重启。没有 schema 变更、没有持久化状态，来回切随时可做。

### 5.2 回退 binary 版本（谨慎）

⚠️ **必须先把 edge 切回 grpc，再降 binary。**

v0.3.2 及更早的版本**根本不认识 `--transport` flag**。如果 unit 里还带着它就降级，`flag.Parse` 会失败、进程以退出码 2 退出，而 systemd 是 `Restart=always` —— 结果是**无限重启循环**，edge 永久 offline。

正确顺序：

1. `-e hookploy_edge_transport=grpc` 下发（或从 unit 里删掉该 flag）
2. 确认 edge 以 grpc online
3. 再把 `hookploy_version` / `hookploy_release_sha256` 改回去下发

main 侧降级没有这个问题（main 没加 flag），但降级后 SSE 端点消失，仍在 sse 的 edge 会 404 重试 —— 所以顺序仍然是**先降 edge 再降 main**。

### 5.3 部分回退是安全的

两台 edge 可以一台 sse、一台 grpc 长期共存，main 同时服务两条通路，`hookploy status` 与 Web UI 都是合并视图。不需要为了一致性强行同步。

---

## 6. 排障

| 现象 | 排查 |
|---|---|
| edge 日志报 404 / `main returned 404 for /edge/session` | main 还是旧版，或 main 没升级成功。先查 §2.2 |
| edge 日志报 `no traffic from main for 1m40s` 反复重连 | **心跳没穿透 CF**（§2.5）。该阈值 = 2×45s 心跳 + 10s 余量。先按 §5.1 回退，再回 hookploy repo 调心跳 |
| edge 一直握手被拒 | server token 问题，与 transport 无关。查 `/opt/apps/hookploy/edge.env` 的 `HOOKPLOY_SERVER_TOKEN`，以及 `hookploy.yaml` 的 `servers:` 有没有这台 |
| edge 进程无限重启 | 多半是给旧 binary 传了 `--transport`（§5.2）。`journalctl -u hookploy-edge` 会看到 flag 解析错误 |
| 部署记 `unreachable` | edge 在执行中途失联且 60s 宽限内没回来。main 从未得知结果，CI 重跑即可。若频繁出现，说明 edge 侧不稳，查 edge 机器 |
| 部署记 `failed`，error 带 `reconnected without execution` | edge 进程在执行途中重启过（不是流断，是进程没了）。查 edge 的 systemd 日志有没有 OOM / crash |
| Web UI 显示 `edge 在线` 但没有 `(sse)` | 这台还在 gRPC 通路（transport 为空时不显示后缀） |

---

## 7. 相关文档

### hookploy repo（`~/Code/hookploy`）

- [传输层解耦 + SSE 通路 + 流断容忍（开发计划）](../plans/2026-07-30-transport-decouple-and-sse.md) — 本次改动的完整设计与取舍，包括为什么不重写 gRPC 协议层、为什么 gRPC 必须 `Resumable=false`
- [Postmortem：deploy「结束」语义被 status 字段冒充](2026-07-22-deploy-settlement-semantics-postmortem.md) — 相邻的状态机不变量（`status` 回答「现在怎么样」、`finished_at` 回答「结束没有」）。本次的宽限/认领机制必须维持它的收敛不变量：挂起执行必然收敛
- `kb/docs/deployment-guide.md` §4.1（两条通路对比表 + 流断容忍语义）、§4.2（edge 接入）、§4.3（反代）、§7（排障速查）— 通用手册，本文是它在生产拓扑上的具体化
- `CLAUDE.md`「关键约束」— 两条通路的一句话总结
- 代码：`internal/edgehub/hub.go`（宽限窗口 `expireGrace` / 重连认领 `reconcile`）、`internal/httpapi/edge.go`（SSE 端点、45s 心跳、serverAuth）、`internal/edge/transport_sse.go`（心跳静默阈值、结果 POST 重试）、`internal/cli/cmd_edge.go`（`--transport` flag）
- commit `196c199`

### deploy workspace（`~/Library/Mobile Documents/com~apple~CloudDocs/deploy`）

- `TODO.md` 第 1 条 — 本次迁移要关掉的那条 TODO，含事故记录与过渡期 SOP
- `AGENTS.md`「CI/CD → hookploy 部署」— 生产拓扑、binary 发布流程、两处待更新的 ⚠️
- `ansible/group_vars/all.yml` — `hookploy_version` / `hookploy_release_sha256` / `hookploy_grpc_url`；新增 `hookploy_edge_transport`
- `ansible/roles/hookploy-edge/templates/hookploy-edge.service.j2` — 要改的 ExecStart
- `ansible/roles/hookploy-edge/templates/edge.env.j2` — server token 骨架（role 从不覆盖已存在的文件）
- `ansible/roles/caddy/templates/Caddyfile.j2` — hookploy vhost，`@grpc` 分流规则（**本次无需改动**，已核对）

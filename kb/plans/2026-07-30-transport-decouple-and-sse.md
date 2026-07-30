# hookploy:传输层解耦 + SSE 协议通路 + 流断容忍

## Context

deploy workspace TODO.md 第 1 条的根治方案(方向经两轮讨论后由用户确定)。CF 对**所有**代理流量(gRPC 与 SSE 一样)有 ~100s 空闲超时且不认 h2 PING 帧,导致 edge↔main 长连接每天 ~1370 次断连;断连落在发版窗口会把实际成功的部署记 failed(2026-07-29 vocalflow-rt `dp_19faeb11`)。现有传输层 keepalive(30s/10s,两侧均已配置)对此无效。

**用户的架构决策**(见 memory):
1. 不走灰云绕行——不管什么协议都可能断连,要根治「断连导致可用性下降」。
2. 不重写 gRPC 协议层,而是**新增 SSE/HTTP 通路**;两条通路走相同的内部逻辑——借此推动并检验「协议层与内部运转机制解耦」的内部 API 设计。
3. gRPC 与 SSE 同时运行,之后渐进 deprecate gRPC(deprecation 不在本计划内)。
4. 流断容忍必做:edge 重连后补报结果,main 不因流断判 failed。

已确认的容忍语义:只补报 ExecDone 不重放中间 update;Done 要 ack 才清缓冲;不重发 Execution(edge 不持有的执行快速失败);main 侧宽限窗口 60s,超时判 unreachable;**不做跨 main 重启续传**(RecoverInFlight 维持现状);迟到结果由 `store.TransitionExecution` 的 CAS 守卫(deploys.go:179-201)天然拒绝,**无 schema 改动、无 proto 改动**。

关键事实修正:scheduler 的 Execute 错误路径(scheduler.go:221-227)目前无条件判 failed,ErrUnreachable 只在 Acquire 路径生效——宽限超时映射 unreachable 需要补 `errors.Is` 检查。

postmortem 不变量(kb/notes/2026-07-22-deploy-settlement-semantics-postmortem.md):deploy 终结 = 所有 execution 到终态;挂起执行必然收敛(宽限超时→unreachable;ex.Timeout→failed;重连未列出→failed)。

## 架构

```
internal/edgehub/    新增:main 侧传输无关核心(attach 状态、server 级 inflight、
    hub.go           宽限窗口、done-ack、EdgeInfo、per-server Executor)
    hub_test.go      传输无关行为测试(fake Conn,无网络)
internal/edgewire/   新增:SSE 通路 JSON wire 类型(对标 internal/pb 定位,不进冻结的 internal/api)
internal/grpcapi/    瘦身为适配器:鉴权 + pb 编解码 + Attach/路由(wire 完全兼容,不改 proto)
internal/httpapi/
    edge.go          新增:SSE 适配器(GET /edge/session + 2 个 POST + serverAuth 中间件)
internal/edge/
    edge.go          Run 变装配层
    agent.go         新增:agent 核心(running 表、pendingDone 缓冲、退避循环)
    transport.go     新增:Transport/Session 接口 + 中立 Task 类型
    transport_grpc.go 现 gRPC 逻辑迁入(Resumable=false)
    transport_sse.go 新增:SSE 传输(Resumable=true)
    sseread.go       新增:~80 行 SSE 解析器(无第三方依赖)
```

### edgehub 核心接口(main 侧)

```go
type Hub struct { Registry *executor.Registry; Logger *log.Logger; GraceWindow time.Duration /*默认60s*/; mu; servers map[string]*serverState }
type Conn interface { SendExecution(spec engine.Spec) error; Close() }  // Close: 被新 attach 顶替时由 hub 调用
type AttachInfo struct { Server, Version, Transport string; Inflight []string /*gRPC 恒 nil*/ }
func (h *Hub) Attach(info AttachInfo, conn Conn) *Attachment  // last-writer-wins:Close 旧 conn、取消宽限计时、对"hub 有 inflight 但未列出"快速失败、Registry.Register
func (a *Attachment) Detach()  // 仅当仍是 current(identity guard):清 conn、Unregister、启动宽限计时;到期以 wrap(executor.ErrUnreachable) 收尾所有 inflight
// 入站事件按 server 键控直接进 Hub(不经 Attachment)——SSE 的 POST 回传在下行流断开期间仍要能落到 inflight:
func (h *Hub) HandleOpStart/HandleOpEnd/HandleLog(server, execID, ...)
func (h *Hub) HandleDone(server, execID string, ok bool, errMsg, digest string)  // 无条件接受、幂等(done chan 缓冲1+select/default);未知 id 也接受(让 edge 清缓冲);适配器的 ack = 返回后回 200(SSE)/什么都不做(gRPC)
func (h *Hub) Edges() map[string]model.EdgeInfo  // 天然合并双通路,cmd_main 无需合并闭包
```

内部:`serverState{current *Attachment; conn Conn; info model.EdgeInfo; inflight map[string]*inflight /*跨 attach 存活*/; graceGen int /*计时器代际防竞态*/}`;`serverExecutor{hub, server}` 实现 executor.Executor(每 server 稳定值):登记 inflight → 读 conn(nil → 立即 wrap(ErrUnreachable))→ mu 外 send → select done/ctx.Done,defer 删 inflight。从 grpcapi.go 迁入:inflight/execResult 形状(:141-151)、Execute 主体(:162-211)、dispatch 路由(:215-244 拆 4 个 Handle*)、failAll(:247-255)。`model.EdgeInfo` 加 `Transport string`(internal 模型可加;冻结的 api.ServerInfo 不动)。

### edge 侧接口

```go
type Task struct { ExecutionID, Kind, Service, Instance, Dir, Image, Digest string; OpsJSON []byte; Timeout time.Duration }
type Transport interface { Dial(ctx, inflight []string) (Session, error) }  // inflight = resume 广告(gRPC 实现忽略)
type Session interface {
    Recv() (*Task, error)
    ReportUpdate(execID string, u Update)        // 尽力而为
    ReportDone(execID string, d DoneReport) error // nil = 已确认(gRPC: send 成功;SSE: POST 200)
    Resumable() bool; MainVersion() string; Close()
}
```

agent 核心:`running map[string]context.CancelFunc` 去重(防御性);`pendingDone` 缓冲(容量 128,溢出丢最旧+告警);执行 ctx:Resumable→进程 ctx、否则会话 ctx,均叠加 per-exec timeout;Done 先入缓冲再 ReportDone,nil 即删(ack);Resumable 会话死亡→执行继续、重连后逐条补报;非 Resumable 会话死亡→取消 running+清缓冲(原子于会话切换,否则 inflight 广告会带上已注定失败的 id);退避循环迁入(握手成功重置,现语义);Dial 广告 running∪pending。

### SSE 端点与 wire(edgewire)

```
GET  /edge/session?version=<v>&server=<assert>&inflight=<id>&inflight=<id>
     Bearer server token;text/event-stream + Cache-Control: no-cache + X-Accel-Buffering: no
     event: hello {main_version, server, heartbeat_seconds}(首帧)
     event: exec  {execution_id, kind, service, instance, dir, image, digest, ops(json.RawMessage 内嵌), timeout_ms}
     ": hb" 注释帧每 45s(CF ~100s 的安全边际)
POST /edge/executions/{id}/updates  {updates:[{op_start|op_end|log}]}  → 恒 200
POST /edge/executions/{id}/done     {ok, error, digest}               → 恒 200(未知 id 也 200)
```

- `serverAuth` 中间件(HTTP 层首次出现 server token):三段式逐字对照 grpcapi.Session(:69-84)——LookupToken(token.Hash)→KindServer→名字取 subject(query server 仅断言)→Config().Servers[name] 存在。**不要**包 `admin()`(避免接受浏览器 cookie)。
- SSE handler 单写者:`sseConn.SendExecution` 只向缓冲 16 的 channel 入队(满/关→返错→该执行失败);handler goroutine `select{execCh, 45s ticker, r.Context().Done(), kicked}` 统一写+Flush;每次写前 `http.NewResponseController(w).SetWriteDeadline(30s)` 防死客户端卡死。骨架抄 followLogs(queries.go:86-120)。
- edgewire 类型全集见上;独立包因两侧共用(风格对标 github.go 的包内 DTO,定位对标 internal/pb)。
- edge sseTransport:Dial=GET+读 hello;静默 >2×心跳+10s 判死重连;ReportUpdate 进批量 pump(200ms/100 条 flush,失败丢弃);ReportDone=POST 重试到 200(有限退避,超限返 error 留缓冲);http.Client 不设整体 Timeout,用 ResponseHeaderTimeout。
- sseread.go:`event:`/`data:`(多行拼接)/`:` 注释/空行分发,兼容 CRLF,仅 hello/exec 两类。

### 已定的设计决策

- **gRPC 传输 Resumable=false,send 成功即清缓冲,会话死亡取消执行**——gRPC wire 无 DoneAck 也无 Hello.inflight,跨会话保留执行/缓冲永远无法被 ack/adopt,只会积压;维持现状语义是唯一自洽选择。容忍收益只在 SSE 通路完整生效(转型期可接受,edge 会尽快切 SSE)。
- **统一宽限**:两种传输 Detach 后都走 60s 宽限。gRPC 老 edge 瞬断秒级重连、Hello 无 inflight→立即快速失败,时序与现状几乎一致;真死场景从"立即 failed"变"60s 后 unreachable"(语义更准,timeout 兜底,收敛不变量不破)。AttachInfo.Transport 留了 per-transport 策略的门。
- **--transport {grpc|sse} flag + HOOKPLOY_TRANSPORT env,默认 grpc**;不做 scheme 推断(两通路同 URL),不做自动回退(掩盖配置错误);旧 main 对 SSE 返回 404 时 edge 打醒目日志提示后按退避持续重试。
- 心跳只在 SSE 通路(45s comment);gRPC 不加 Ping(它在 deprecation 路上,且容忍机制已兜住发版窗口)。传输层 keepalive 30s/10s 原样保留。

## 分阶段实施(BDD:每阶段先写行为测试)

### Phase 1 — 纯重构(验收:现有测试全绿且不改)
- 新增 edgehub(本阶段 Detach = 立即 failAll + Unregister,现状语义无宽限);grpcapi 瘦身为适配器(grpcConn:SendExecution=pb 编码+send;Close=关 kick channel——Recv 循环改 pump goroutine+select 以便被踢,同 server 双活流问题 Phase 1 即解);edge 拆出 agent.go/transport.go/transport_grpc.go(本阶段无 pendingDone,Done 直接 Report、忽略错误);cmd_main 组装 `hub := edgehub.New(...)`、`apiSrv.Edges = hub.Edges`(cmd_main.go:102)、webui 同步(:106)。
- **grpcapi_test.go / edge_test.go 一行不改全部通过**是本阶段的守门标准;另加 hub_test 基础用例。

### Phase 2 — 容忍语义进核心
- edgehub:GraceWindow、宽限计时(graceGen 代际)、adopt/快速失败、无 attachment 时 HandleDone 仍路由、conn==nil→ErrUnreachable。
- scheduler.go:221-227 加 `errors.Is(err, executor.ErrUnreachable) → model.StatusUnreachable`(必须,否则宽限到期仍 failed)。
- edge agent:pendingDone+ack+补报重试、Resumable 分支、inflight 广告。
- model.EdgeInfo 加 Transport。
- 测试:hub_test 容忍全套(fake Conn)、agent_test(fake Transport)、grpcapi_test 更新 TestRemoteExecuteEdgeDisconnect(短宽限→unreachable)+ 新增 gRPC 重连空 inflight→快速失败、scheduler_test 补 unreachable 映射。

### Phase 3 — SSE 通路
- edgewire、httpapi/edge.go(3 端点+serverAuth+路由表 server.go:38-53 加 3 条+Server.Hub 字段)、transport_sse.go、sseread.go。
- 测试:httpapi/edge_test(鉴权矩阵 401/403、hello 字段、短间隔心跳、exec 送达、POST updates 进 sink、done 200 且执行完成、未知 id done 仍 200、第二 session 踢第一);sseread_test(解析+静默超时+done 重试);**端到端**(httptest+真 store+FakeRunner+真 edge.Run(transport=sse)):a) SSE 全链路部署成功;b) 执行中掐断 SSE 流→执行继续→POST 补报→deploy succeeded【旗舰用例】;c) edge 不重连→宽限后 unreachable。

### Phase 4 — 接线收尾
- cmd_edge.go `--transport` flag+env;cmd_main apiSrv.Hub 注入;webui servers 页(可选)显示 transport;kb/docs/deployment-guide.md 补 SSE 接入说明(同域名同端口、无需 CF gRPC 开关、CF 场景推荐 sse);CLI golden 检查(flag 帮助文本)。

## 并发/生命周期要点(实现时对照)

1. **identity guard 的闸门必须在 hub 层**:serverExecutor 是每 server 稳定值,Registry.Unregister 的 identity 比较救不了(值相同);旧 attachment Detach 必须先验 `st.current == att`。
2. 宽限计时器回调与 Attach 抢 st.mu,回调必须校验 graceGen 再收尾,防"到期瞬间重连"错杀刚 adopt 的执行。
3. Execute 持锁登记 inflight/读 conn,**mu 外 send**;send 与 Detach 竞态→流错误→失败(与现状一致);conn==nil→立即 wrap(ErrUnreachable)。
4. SSE ResponseWriter 非并发安全:单写者 goroutine 是唯一写路径;写前 SetWriteDeadline(30s),否则死客户端永久卡住 handler(kick 解不开阻塞的写)。
5. SSE 的 main 侧死连检测 = 心跳写失败/超时→Detach;edge 侧 = 静默阈值(≈100s)。
6. adopt 快速失败时序:Attach 持锁先对未列出者收尾,再装 conn、再 Register(宽限期间 Registry 已 Unregister,Acquire 阻塞,不会有新 inflight 被误杀)。
7. SSE 的 POST 与 GET 分属不同连接可能乱序——sink 落库按 index 幂等,Done 幂等,不敏感。

## 兼容性

- 新 main + 旧 edge(gRPC):wire/握手不变;唯一差异是 edge 真死时"立即 failed"→"60s 后 unreachable"。
- 旧 main + 新 edge(--transport grpc,默认):完全兼容。
- 旧 main + 新 edge(--transport sse):404,edge 打醒目提示并退避重试,不自动回退。
- 升级顺序:先 main 后 edge;edge 切 sse 是之后的运维动作(deploy workspace)。

## 验证

1. 每阶段 `go test ./...` 全绿(强制真跑勿信 cached——postmortem 教训);Phase 1 额外验收:grpcapi_test/edge_test 零改动通过。
2. **真机验证(ali-hk-01 测试环境,9180/9181)**:构建上传(先 stop+edge-stop);edge-01 以 `--transport sse` 起;触发 echo_server 部署走完整 SSE 链路;`hookploy status` 看 edge online(transport=sse);流断补报场景优先由端到端测试覆盖,真机可选用 `sudo ss -K` 掐 TCP 连接复现(给 echo 流水线临时加 sleep op 拉长窗口);同时验证 gRPC 通路(edge-01 切回默认)无回归。截图归档 `./tmp/<date>-<task-name>/`。
3. CF 实效验证留给生产发版后(运维侧):edge 切 sse,观察断连频率(预期心跳后接近 0)与发版窗口断连的 deploy 结果(补报成功)。过渡期 SOP 继续有效。

## 非目标

- gRPC deprecation/下线;任何 proto 改动(不加 DoneAck/Ping/Hello.inflight)
- transport 自动探测/回退;跨 main 重启续传;中间 update 重放;CancelExec
- internal/api 任何字段变更(api.ServerInfo 冻结不动);容忍缓冲的持久化(均内存态)
- TODO.md 第 2 条(站外监控)、第 3 条(tc-sg-01 重启)——deploy workspace 运维事项

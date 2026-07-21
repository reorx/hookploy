---
created: 2026-07-21
tags:
  - web-ui
  - templ
  - milestone-m4
  - plan
---

# M4：Hookploy Web UI 开发计划

## 1. 背景与目标

hookploy 目前只有 CLI（走 admin HTTP API）和裸 HTTP API 两种观测方式。本里程碑为 main 进程内置一个面向 DevOps 用户的 Web 界面，随单 binary 分发，零外部依赖。

界面要回答四个问题：

1. **现在有哪些部署正在进行？** 状态如何，日志实时滚动
2. **系统管辖哪些服务？** 每个服务部署在哪些服务器、部署流水线（pipeline）长什么样
3. **近期发生过哪些发布与触发？** 含被去重（superseded）的触发记录
4. **某次发布的详细过程是什么？** 按波次（wave）分解的 execution 时间线、每个 op 的耗时/退出码、完整日志

明确不做（v1 范围外）：从 UI 触发部署/任务/reload（保持只读，避免 CSRF 面）、多用户/权限分级、历史图表统计、移动端专门适配。

## 2. 技术决策记录

| 决策点 | 选择 | 理由 |
|--------|------|------|
| 渲染架构 | 服务端渲染，不做前后端分离 | 简洁、面向 DevOps、单 binary |
| 模板引擎 | **templ (a-h/templ)** | 类型安全、组件化、编译期检查；codegen 模式与项目已有的 protoc 流程同构 |
| 认证 | **登录页 + HttpOnly Cookie 会话** | token 不落 JS 可读存储；服务端渲染与 JS fetch 天然共用 |
| CSS | **纯手写（~300 行，embed）** | 信息密集界面 + 终端风日志查看器需要大量自定义；不引入框架 |
| 实时刷新 | **JS 定时轮询（3s）+ 日志 follow 流** | 单用户场景开销可忽略；不给 scheduler/store 加事件广播面 |
| JS | 纯手写 vanilla JS，无框架、无构建步骤 | 逻辑简单：轮询换片段 + NDJSON 流读取 |

## 3. 数据来源盘点

现有 admin API（M3 冻结，add-only）已覆盖大部分需求：

| 界面需求 | 数据来源 | 状态 |
|----------|---------|------|
| 活跃部署 | `GET /services` 的 `last_deploy`（scheduler 串行+去重保证活跃部署必为该服务最新一条） | ✅ 已有 |
| 服务器在线状态 | `GET /servers`（edge 版本、connected_at） | ✅ 已有 |
| 服务清单 | `GET /services` | ✅ 已有 |
| 单服务历史（50 条） | `GET /services/{name}/deploys` | ✅ 已有 |
| 部署详情（executions × ops） | `GET /deploys/{id}` | ✅ 已有 |
| 日志（回放/实时） | `GET /deploys/{id}/logs`（`?follow=1` NDJSON 流，终帧 `{"done":true,status}`） | ✅ 已有 |
| **服务 pipeline 定义** | 无 —— `ServiceSummary` 只有 name/webhook/servers | ❌ 新增 |
| **全局近期发布记录** | 无跨服务列表 | ❌ 新增 |

关于「触发记录」：hookploy 中每次触发（webhook / manual / task）都会创建 deploy 行，携带 kind、payload、created_at；排队中被新触发顶掉的记录以 `superseded` 状态留存。因此 **deploy 历史即触发历史**，UI 无需单独实体，把 superseded 记录一并展示即可。

## 4. 后端新增

### 4.1 新增 API 端点（admin 鉴权，DTO add-only，不违反冻结）

**`GET /deploys?limit=N`** — 跨服务近期部署列表，`created_at` 降序。limit 默认 20、上限 100。返回 `[]api.Deploy`（不含 executions）。需要 `store.ListRecentDeploys(limit int)` 新查询。

**`GET /services/{name}`** — 服务详情，新 DTO：

```go
// api.ServiceDetail —— 新类型，进入冻结范围（add-only）
type ServiceDetail struct {
    Name      string                     `json:"name"`
    Image     string                     `json:"image,omitempty"`
    Webhook   bool                       `json:"webhook"`
    Timeout   string                     `json:"timeout"`            // "10m"，复用 model.Duration 字符串形式
    Instances []InstanceInfo             `json:"instances"`
    Rollout   [][]string                 `json:"rollout"`            // 波次 × 实例名
    Deploy    []json.RawMessage          `json:"deploy"`             // ops.Step 线格式 {op, args...}
    Tasks     map[string][]json.RawMessage `json:"tasks,omitempty"`
}

type InstanceInfo struct {
    Name   string `json:"name"`
    Server string `json:"server"`
    Dir    string `json:"dir"`
}
```

pipeline 步骤直接复用 `ops.Step` 已有的 `MarshalJSON`（`{op, args}` 线格式，与 DB 快照/gRPC 下发同源），不发明第二种表示。

两个端点同步补进 `docs/json-output.md` 与 CLI golden 快照（若给 CLI 也加 `hookploy service <name>` 命令则一并做，可选，不阻塞 UI）。

### 4.2 会话认证（`internal/webui/session.go`）

- `POST /ui/login`：表单提交 admin token → `store.LookupToken(token.Hash(...))` 校验 kind=admin → 生成随机 session id（crypto/rand 32B hex），存进程内 map（id → expiry），种 cookie：`hookploy_session`，HttpOnly、SameSite=Strict、Path=/、7 天有效。main 重启即失效（重新登录，DevOps 场景可接受），不落库。
- `POST /ui/logout`：删 session、清 cookie。
- **httpapi `admin` 中间件扩展**：`Bearer 头 OR 有效 session cookie` 皆可通过，使 JS 同源 fetch 现有 JSON 端点时 cookie 自动生效。cookie 路径仅接受 **GET** 请求（v1 UI 只读；POST 类端点仍必须 Bearer，天然免 CSRF）。
- webui 页面路由用独立 `requireSession` 中间件：未登录 302 → `/ui/login`。

### 4.3 `internal/webui` 包结构

```
internal/webui/
  webui.go        // Handler()，路由表，挂载到现有 HTTP mux
  session.go      // 会话 map + 中间件 + login/logout handler
  pages.go        // 页面 handler：直接调 store/config（进程内，不 HTTP 自调用）
  fragments.go    // 轮询用 HTML 片段 handler
  views/          // *.templ 源文件 + 生成的 *_templ.go（提交进仓库，同 internal/pb 惯例）
    layout.templ
    login.templ
    dashboard.templ
    service.templ
    deploy.templ
    components.templ   // 状态徽章、时间线、pipeline 步骤等共享组件
  static/         // embed.FS
    app.css
    app.js        // 片段轮询器
    logs.js       // NDJSON follow 流读取 + 日志渲染
```

挂载：`httpapi.Server.Handler()` 增加 `mux.Handle("/ui/", webui.Handler(...))`，同一 listener。`GET /` 302 → `/ui/`。webui 依赖注入与 httpapi.Server 相同（Store、Config、Edges）。

### 4.4 构建链

- `go get github.com/a-h/templ`（runtime 库）+ `templ` CLI（开发机安装，同 protoc 定位）
- 新增 `scripts/gentempl.sh`（对照 `scripts/genproto.sh`）：`templ generate ./internal/webui/views`
- 生成的 `*_templ.go` **提交进仓库**，普通 `go build` / CI 不需要 templ CLI
- CLAUDE.md 补一行：「templ 改动后跑 `scripts/gentempl.sh`」

## 5. 页面设计

### 5.1 `/ui/` Dashboard

```
┌──────────────────────────────────────────────────────────┐
│ hookploy        [服务器: local ● | edge-01 ● v0.3.1 2h]   │  ← 顶栏：服务器状态条
├──────────────────────────────────────────────────────────┤
│ ▶ 进行中 (1)                              [3s 自动刷新]    │
│ ┌──────────────────────────────────────────────────────┐ │
│ │ echo_server  #dp_19a…  running  wave 1/2  开始于 12s前 │ │
│ │ ├ prod-a @ edge-01   compose.up ▸ 运行中               │ │
│ │ └ [日志尾部 8 行，follow 流实时滚动]         [详情 →]   │ │
│ └──────────────────────────────────────────────────────┘ │
├──────────────────────────────────────────────────────────┤
│ 服务                                                      │
│  echo_server   webhook ✓   edge-01        ● succeeded 2h │
│  blog          webhook ✓   local          ● failed   1d  │  ← 行点击 → 服务详情
├──────────────────────────────────────────────────────────┤
│ 近期发布 (GET /deploys)                                   │
│  10:32  echo_server  webhook  ● succeeded  1m12s          │
│  10:31  echo_server  webhook  ○ superseded  —             │  ← 触发被去重也可见
│  09:15  blog         task:backup ● failed   8s   [详情 →] │
└──────────────────────────────────────────────────────────┘
```

- 「进行中」区：由服务列表中 `last_deploy` 状态非终态的项组成；空时显示「当前没有进行中的部署」
- 进行中卡片内嵌一个小日志窗口，直接连该 deploy 的 follow 流（logs.js 复用）
- 除日志窗口外整页按片段轮询刷新（见 §6.2）

### 5.2 `/ui/services/{name}` 服务详情

- 头部：服务名、image、webhook 开关、timeout
- **实例表**：instance / server（含在线状态点）/ dir
- **Rollout 波次图**：`wave 1: [prod-a]  →  wave 2: [prod-b, prod-c]` 横向流
- **Deploy pipeline**：有序步骤列表，每步 op 名 + 参数键值（`compose.pull` → `compose.up` → `healthcheck url=…`）
- **Tasks**：每个 task 同样式的步骤列表
- **历史**：该服务近 50 条 deploy 行（时间、kind/task、状态、digest 短形式、耗时），点击进部署详情

### 5.3 `/ui/deploys/{id}` 部署详情

- 头部：服务名、deploy id、kind（webhook/manual/task:name）、状态大徽章、created/finished、总耗时、digest；payload 折叠显示（`<details>` + pretty JSON）
- **执行时间线**：按 wave 分组 → 每个 execution 一块（instance @ server、dir、状态、耗时）→ 内部 op 列表（序号、op 名、耗时、exit code、error 红字）
- **日志查看器**（页面主体，终端风深色区）：
  - 终态 deploy：一次性拉 `?format=json` 回放渲染
  - 运行中：连 `?follow=1` 流，实时追加；收到 done 帧后停流并刷新上方状态区
  - 行前缀：`[instance/op名]`，stderr 行着橙红色，system 流（op 边界等）着灰色
  - 自动滚底 + 「📌 固定底部」开关（用户上滚时自动暂停跟随）
  - 顶部过滤：按 execution 过滤下拉（多实例并行时日志交织，需要能只看一台）

### 5.4 `/ui/login`

单输入框（admin token，type=password）+ 提交。失败显示错误。已登录访问则 302 → `/ui/`。

### 5.5 视觉基调

信息密集、克制的 DevOps 风：浅色页面 + 深色日志区；等宽字体用于 id/digest/日志；状态色仅五种（running=蓝、succeeded=绿、failed/unreachable=红、superseded/canceled=灰、queued/dispatching=黄）；无动画（日志滚动除外）。

## 6. 前端实现要点

### 6.1 日志流（logs.js，~100 行）

```js
const resp = await fetch(`/deploys/${id}/logs?follow=1`); // cookie 鉴权
const reader = resp.body.getReader();
// TextDecoder + 按 \n 切帧 → JSON.parse
// frame.done → 结束；否则按 execution_id/stream 渲染追加
```

断流（网络错）后 2s 重连：follow 会全量回放，重连时清空容器重渲染，无需去重。

### 6.2 片段轮询（app.js，~60 行）

不在 JS 里复刻渲染逻辑。页面上可变区块标 `data-poll="/ui/fragments/dashboard"`，轮询器每 3s fetch 该片段（templ 组件服务端渲染的 HTML），`innerHTML` 替换。渲染逻辑单一来源（templ），JS 只做搬运。页面隐藏（`document.hidden`）时暂停轮询。日志窗口不参与片段替换（由 logs.js 独立管理）。

片段端点：`/ui/fragments/dashboard`（进行中+服务+近期三区）、`/ui/fragments/deploys/{id}/status`（部署详情的状态/时间线区）。

## 7. 实施步骤（BDD：每步先写行为测试）

按依赖排序，每步可独立提交：

1. **store：`ListRecentDeploys(limit)`** — 先写 store 测试（跨服务排序、limit 截断）→ 实现
2. **API：`GET /deploys` + `GET /services/{name}`** — 先写 httpapi 测试（含 404、limit 上限、ServiceDetail 字段完整性、ops 线格式一致性）→ 实现 → 更新 `docs/json-output.md` + JSON Schema/golden（如涉及）
3. **会话层** — 先写测试：login 成功/失败、cookie 过 GET admin 端点、cookie 过 POST 端点必须 403、logout、过期 → 实现 session.go + admin 中间件扩展
4. **webui 骨架 + templ 构建链** — `scripts/gentempl.sh`、layout/login 页、requireSession 跳转测试（httptest 断言 302/200 与页面关键内容）
5. **Dashboard 页 + 片段端点 + app.js 轮询**
6. **服务详情页**（pipeline/rollout/instances 渲染，测试断言 op 名与参数出现在 HTML）
7. **部署详情页 + logs.js**（回放模式 → follow 模式；follow 用 httptest + 假 store 流验证前端可解析的帧序）
8. **收尾**：`GET /` 重定向、CLAUDE.md/README/deployment-guide 增补（UI 入口、反代注意：`/ui` 与 admin API 同 listener，暴露公网需反代加护或仅内网）、`make dist` 确认 embed 正常
9. **真机验证**（ali-hk-01 测试环境，端口 9180）：浏览器走一遍登录 → 触发 echo_server 部署 → 观察实时日志 → 查历史详情；截图归档 `./tmp/<date>-web-ui/`

预计新增代码量：Go ~800 行（含测试）、templ ~400 行、CSS ~300 行、JS ~160 行。

## 8. 风险与开放问题

- **templ 引入 codegen**：与 protoc 同构可接受；生成文件提交进仓库保证 `go build` 干净。风险低。
- **admin API 与 UI 同 listener**：现默认 `127.0.0.1:9100`，公网暴露依赖反代。文档需明确：暴露 `/ui` 即暴露 admin API（同源同 auth，逻辑上一致，但要写清楚）。
- **多实例日志交织**：v1 用行前缀 + execution 过滤解决；若真机体验差，v2 再考虑按 execution 分栏。
- **会话在 main 重启后失效**：接受（重新登录成本低）；若日后不满意，再落库。
- **未来若加 UI 触发按钮**：需引入 CSRF token（或 POST 也允许 cookie + 校验 `Sec-Fetch-Site`）。v1 只读，明确推迟。

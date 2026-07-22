---
created: 2026-07-22
tags:
  - github
  - webhook
  - web-ui
  - plan
status: done (2026-07-22)
---

# GitHub Actions 集成（workflow_run webhook 推送模式）

> 状态：已实现并验证（2026-07-22，与本计划同日完成）。本文档为归档的实施计划，如实反映最终实现。

## 1. 背景与目标

为 hookploy 增加 GitHub 接入，在 Web UI 展示各服务的 Actions 构建情况。经讨论**否决了主动轮询 GitHub API 的方案**，改为被动接收 webhook：hookploy 提供 `POST /github/webhook` 端点，用户在 GitHub repo Settings 手动配置 webhook（事件只勾 Workflow runs），事件到达即存 SQLite。零 GitHub token、零轮询、数据实时；后续如需更详细数据（job 级明细、日志等）再考虑 token/OAuth 接入。

UI 三处变动：

1. Dashboard 显示进行中的 workflow runs（无构建时整个 section 不出现）
2. 新增 `/ui/actions` 页面：近期构建列表，按 service 过滤（`?service=`），3s 片段轮询
3. 服务详情页（声明了 `github_repo` 的服务）显示关联 repo 的近期构建 + 页头 repo 外链

## 2. 设计决策

- 只处理 `workflow_run` 事件（`ping` 返回 200，其他 event 返回 200 但忽略——GitHub 侧永不标红）；payload 的 `action` 字段不分支，统一按 `status/conclusion` upsert。
- 按 `workflow_run.id` upsert；用 `updated_at` 防乱序回退（SQL TEXT 比较即可：GitHub 时间戳整秒、`fmtTime` RFC3339 无小数位，字典序 == 时间序）。
- 存储以 `repository.full_name` 为 key（COLLATE NOCASE）；repo→service 映射在**查询时**经 service 的 `github_repo` 解析——config 热 reload 即时生效，历史数据不丢。未匹配 service 的 repo 事件也保存（actions 页可见，service 列为空）。
- 安全：HMAC 校验 `X-Hub-Signature-256`（constant-time）；config 顶层新增 `github: { webhook_secret }`。secret 未配置时端点 404（关闭，防无鉴权数据注入）。
- 保留策略：每 repo 保留最近 200 条（`retainWorkflowRunsPerRepo`），写入后 best-effort 清理。
- **不动 `internal/api` DTO、不加 admin API 端点、不加 CLI 命令**（契约冻结）；数据仅 webui 层服务端渲染。
- 不新建 `internal/github` 包：handler+HMAC+payload 类型在 `internal/httpapi/github.go`，域模型在 `internal/model`，DAO 在 `internal/store/workflowruns.go`。

## 3. 落点速查

| 层 | 文件 | 内容 |
|---|---|---|
| config | `raw.go` / `config.go` / `schema.go` | 顶层 `github.webhook_secret`、service `github_repo`（owner/repo 格式校验）；schemaSyncCases 加 rawGithub |
| model | `model.go` | `WorkflowRun` |
| store | `store.go` migration #2 + `workflowruns.go` | workflow_runs 表；Upsert（updated_at guard）/ List（repo 过滤）/ ListActive（status != completed）/ Cleanup |
| httpapi | `github.go` + `server.go` 路由 | `POST /github/webhook`：404（无 secret）→ 413/400（body）→ 401（签名）→ ping/其他 200 → upsert+cleanup |
| webui | `views.go`、`actions.templ`、`dashboard/service/layout.templ`、`pages.go`、`webui.go` | WorkflowRunRow/ActionsPage、RunBadge（GitHub 状态折进 st-* badge 词汇）、`/ui/actions` + fragment 路由、repoServices 映射 |

## 4. 测试（BDD，全部先红后绿）

- config：secret/github_repo 加载与缺省；非法格式报错带 service 名
- store：upsert 不增行；旧 updated_at 不回退；repo 过滤大小写不敏感/排序/limit；active 只含非 completed；cleanup 不跨 repo
- httpapi（测试侧独立实现 HMAC 签名 helper）：正确签名落库字段齐全；错签 401 不落库；无 secret 404（经热 reload 验证）；ping/push 200 不落库；同 run 多次投递同行更新；乱序不回退；413/400
- webui：页面/片段鉴权（302/401）；dashboard 只显示进行中；/ui/actions 全列表与过滤、轮询 URL 带 filter；service 页 section 有无

## 5. 验证记录

- `go test ./...` 全绿；`go vet` 无告警
- 本地端到端冒烟（`tmp/smoke/send_run.sh` 构造签名投递 requested/in_progress/completed/坏签名/ping），三处 UI 浏览器截图归档：`tmp/2026-07-22-github-actions/`

## 6. 文档

- `kb/docs/deployment-guide.md` §3.6（GitHub webhook 配置法）、§3.3（反代注记）、§5 Web UI 页面结构
- `CLAUDE.md` 项目状态 + 代码地图；`README.md` Web UI 小节

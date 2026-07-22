# JSON 输出契约

> hookploy 的 `--json` CLI 输出与 HTTP 状态 API 的结构定义，M3 起冻结。
>
> 状态：v1（2026-07-19）

## 冻结策略

`internal/api` 是 CLI `--json` 与 HTTP API 共用的 DTO 包——两者序列化的是同一批 Go 类型，输出**由构造保证一致**，不存在"CLI 和 API 说法不同"的可能。

M3 起该包冻结，规则（与 `internal/api/api.go` 的包注释一致）：

- **字段只增不改**：重命名、删除、改类型都是破坏性变更，禁止。
- **新增字段必须可选**（`omitempty`）——老消费者拿到的文档形状不变。
- **NDJSON 帧格式同受约束**：`LogLine`、`LogDone`、`FollowFrame` 与普通响应体一视同仁。

回归保护：`internal/cli/testdata/golden/` 下的 golden 快照锁死每个命令的键集与静态值（易变值——id、时间戳、token 明文、版本号、错误文案——被掩码，所以锁的是**形状**而非内容）。改动 DTO 会让 `go test ./internal/cli` 直接变红。快照重生成：

```sh
go test ./internal/cli -update
```

本文所有示例均**取自 golden 快照**，掩码占位符（`<id>` / `<time>` / `<token>` / `<version>` / `<error>`）保持原样。

⚠️ golden 在比对前会把键排序，所以示例里的字段顺序是字典序，与实际输出（Go 结构体声明顺序）不同。**键顺序不属于契约**——按名取值，别依赖顺序。

## schema 与 validate 的分层

配置侧另有一份产物：`hookploy schema` 输出 `hookploy.yaml` 的 JSON Schema（draft-07）。两者关注点不同，别混用：

| | 覆盖范围 | 用途 |
|---|---|---|
| `hookploy schema` | **宽松上界**：字段名、类型、op 参数、互斥结构（磁盘形态） | 编辑器补全 / 校验、agent 改配置时的护栏 |
| `hookploy validate` | 全部跨字段语义：服务器引用是否存在、rollout 是否恰好覆盖每个实例一次、`image.pin` 是否有服务级 `image:` 等 | **最终判据**，playbook 下发前必跑 |

schema 通过不等于配置合法；**以 `hookploy validate` 为准**。

---

## CLI 命令

### `hookploy version --json`

类型：`api.VersionInfo`

| 字段 | 类型 | 含义 | 可选 |
|---|---|---|---|
| `version` | string | binary 内烧入的版本号（`-ldflags -X ...version.Version=`） | 否 |

```json
{
  "version": "<version>"
}
```

### `hookploy validate --json`

类型：`api.ValidateResult`

失败时 `ok` 为 false、`error` 带原因，**进程仍退出 1**——`--json` 只改变输出去向（stdout 而非 stderr），不改变退出码语义。

| 字段 | 类型 | 含义 | 可选 |
|---|---|---|---|
| `ok` | bool | 配置是否通过校验 | 否 |
| `servers` | int | 声明的服务器数（失败时为 0） | 否 |
| `services` | int | 声明的服务数（失败时为 0） | 否 |
| `error` | string | 失败原因；文案属于抛出它的包，不在冻结范围内，只有"是否存在"被冻结 | 是 |

成功（exit 0）：

```json
{
  "ok": true,
  "servers": 3,
  "services": 5
}
```

失败（exit 1）：

```json
{
  "error": "<error>",
  "ok": false,
  "servers": 0,
  "services": 0
}
```

### `hookploy status --json`

类型：`api.Status`

`servers` 与 `services` 分别是 `GET /servers`、`GET /services` 的响应体，status 只是把两次查询合并成一个文档。

`api.Status`：

| 字段 | 类型 | 含义 | 可选 |
|---|---|---|---|
| `servers` | []`api.ServerInfo` | 各服务器连接状态 | 否 |
| `services` | []`api.ServiceSummary` | 各服务及其最近一次部署 | 否 |

`api.ServerInfo`：

| 字段 | 类型 | 含义 | 可选 |
|---|---|---|---|
| `name` | string | server 名（与 `hookploy.yaml` 的 `servers:` 键一致） | 否 |
| `local` | bool | 是否为 main 内建 executor（非 edge） | 否 |
| `status` | string | `online` \| `offline` | 否 |
| `version` | string | edge binary 版本，仅 edge 有 | 是 |
| `connected_at` | RFC3339 时间 | edge 会话建立时刻，仅 edge 有 | 是 |

`api.ServiceSummary`：

| 字段 | 类型 | 含义 | 可选 |
|---|---|---|---|
| `name` | string | 服务名，也是 webhook 路径 `/hooks/<name>` | 否 |
| `webhook` | bool | 是否接受 webhook 触发（`webhook: false` 时为 false） | 否 |
| `servers` | []string | 该服务落在哪些服务器上 | 否 |
| `last_deploy` | `api.Deploy` | 最近一次部署，从未部署过时缺省 | 是 |

```json
{
  "servers": [
    {
      "local": true,
      "name": "s1",
      "status": "online",
      "version": "<version>"
    }
  ],
  "services": [
    {
      "last_deploy": {
        "created_at": "<time>",
        "finished_at": "<time>",
        "id": "<id>",
        "kind": "deploy",
        "payload": {},
        "service": "linkmind",
        "status": "succeeded"
      },
      "name": "linkmind",
      "servers": [
        "s1"
      ],
      "webhook": true
    }
  ]
}
```

### `hookploy deploys <service> --json`

类型：[]`api.Deploy`（`GET /services/<name>/deploys` 的响应体原样透传）

`api.Deploy`：

| 字段 | 类型 | 含义 | 可选 |
|---|---|---|---|
| `id` | string | 部署 ID（多实例场景下指向整个 rollout） | 否 |
| `service` | string | 服务名 | 否 |
| `kind` | string | `deploy` \| `task` | 否 |
| `task` | string | 任务名，`kind` 为 task 时有 | 是 |
| `status` | string | `queued` \| `dispatching` \| `running` \| `succeeded` \| `failed` \| `superseded` \| `unreachable` \| `canceled` | 否 |
| `digest` | string | rollout 层解析出的镜像 digest | 是 |
| `error` | string | 失败原因 | 是 |
| `payload` | object | 触发时的 JSON payload | 是 |
| `created_at` | RFC3339 时间 | 入队时刻 | 否 |
| `finished_at` | RFC3339 时间 | 结束时刻，未结束时缺省 | 是 |
| `executions` | []`api.Execution` | 逐实例执行；列表接口不展开，`GET /deploys/<id>` 展开 | 是 |

`api.Execution`：

| 字段 | 类型 | 含义 | 可选 |
|---|---|---|---|
| `id` | string | 执行 ID（日志按它归属） | 否 |
| `instance` | string | 实例名 | 否 |
| `server` | string | 落在哪台服务器 | 否 |
| `dir` | string | 执行目录 | 否 |
| `wave` | int | 所属 rollout 波次，从 0 起 | 否 |
| `status` | string | 同 deploy 状态机 | 否 |
| `error` | string | 失败原因 | 是 |
| `started_at` | RFC3339 时间 | 开始时刻 | 是 |
| `finished_at` | RFC3339 时间 | 结束时刻 | 是 |
| `ops` | []`api.OpRecord` | op 时间线 | 是 |

`api.OpRecord`：

| 字段 | 类型 | 含义 | 可选 |
|---|---|---|---|
| `index` | int | op 在流水线中的序号，从 0 起 | 否 |
| `name` | string | op 名（如 `compose.up`） | 否 |
| `started_at` | RFC3339 时间 | 开始时刻 | 否 |
| `finished_at` | RFC3339 时间 | 结束时刻 | 是 |
| `exit_code` | int | 子进程退出码；不派生子进程的 op 可能缺省 | 是 |
| `error` | string | 失败原因 | 是 |

```json
[
  {
    "created_at": "<time>",
    "finished_at": "<time>",
    "id": "<id>",
    "kind": "deploy",
    "payload": {},
    "service": "linkmind",
    "status": "succeeded"
  }
]
```

### `hookploy deploy <service> --json` / `hookploy task <service> <name> --json`

类型：`api.Accepted`（与 webhook 的 202 响应体同型）

| 字段 | 类型 | 含义 | 可选 |
|---|---|---|---|
| `deploy_id` | string | 新建部署的 ID | 否 |
| `status_url` | string | 详情路径，形如 `/deploys/<id>` | 否 |

```json
{
  "deploy_id": "<id>",
  "status_url": "/deploys/<id>"
}
```

（`task` 的输出完全相同——task 与 deploy 在内部统一为 Execution。）

### `hookploy token` / `server token` / `admin-token` --json

创建与轮换（`token create` / `token rotate` / `server token create` / `admin-token create`）：`api.TokenCreated`

| 字段 | 类型 | 含义 | 可选 |
|---|---|---|---|
| `kind` | string | `service` \| `server` \| `admin` | 否 |
| `subject` | string | token 的主体：服务名 / server 名 / `admin` | 否 |
| `token` | string | **明文密钥**。main 只在此刻输出一次，库里只存哈希，丢失只能 rotate | 否 |

```json
{
  "kind": "service",
  "subject": "linkmind",
  "token": "<token>"
}
```

server token（`kind` 为 `server`，subject 即 edge 的身份来源）：

```json
{
  "kind": "server",
  "subject": "s1",
  "token": "<token>"
}
```

admin token（subject 固定为 `admin`）：

```json
{
  "kind": "admin",
  "subject": "admin",
  "token": "<token>"
}
```

吊销（`token revoke`）：`api.TokenRevoked`

| 字段 | 类型 | 含义 | 可选 |
|---|---|---|---|
| `kind` | string | `service` \| `server` \| `admin` | 否 |
| `subject` | string | token 主体 | 否 |
| `revoked` | bool | 是否真的吊销了；主体本就没有有效 token 时为 false | 否 |

```json
{
  "kind": "service",
  "revoked": true,
  "subject": "linkmind"
}
```

### `hookploy schema`

输出即 `hookploy.yaml` 的 JSON Schema（draft-07），本身就是 JSON，**没有 `--json` 开关**。它描述的是配置文件而非 API 响应，不属于 `internal/api` 的冻结范围（op 词汇表演进时 schema 随之变化，这是预期行为）。用法见 README 的 editor 集成一节。

---

## NDJSON：`hookploy logs --json`

日志是流，不是文档：每行一个独立 JSON 对象（NDJSON，`Content-Type: application/x-ndjson`），逐行解析、不要等整体读完。

### 日志帧：`api.LogLine`

| 字段 | 类型 | 含义 | 可选 |
|---|---|---|---|
| `execution_id` | string | 该日志块属于哪次执行（多实例 rollout 靠它区分实例） | 否 |
| `op_index` | int | 属于流水线的第几个 op，从 0 起 | 否 |
| `stream` | string | `stdout` \| `stderr` | 否 |
| `data` | string | 日志内容块，**不保证按行切分**，可能含多行或半行 | 否 |
| `at` | RFC3339 时间 | 采集时刻 | 否 |

`hookploy logs <deploy-id> --json`（回放，不跟随）：

```json
{
  "at": "<time>",
  "data": "release deployed!\n",
  "execution_id": "<id>",
  "op_index": 0,
  "stream": "stdout"
}
```

### 终止帧：`api.LogDone`

跟随模式（`-f`，HTTP 侧 `?follow=1`）在部署 settle 后多发一帧终止帧，然后关闭连接：

| 字段 | 类型 | 含义 | 可选 |
|---|---|---|---|
| `done` | bool | 恒为 true——这是终止帧的判别字段 | 否 |
| `status` | string | 部署最终状态（`succeeded` / `failed` / …） | 否 |

`hookploy logs <deploy-id> -f --json`（回放 + 终止帧）：

```json
{
  "at": "<time>",
  "data": "release deployed!\n",
  "execution_id": "<id>",
  "op_index": 0,
  "stream": "stdout"
}
{
  "done": true,
  "status": "succeeded"
}
```

（示例为可读性做了缩进；真实流每帧一行。）

### 消费方式：`api.FollowFrame`

两种帧共用一条流，消费者用一个联合类型解析即可——`api.FollowFrame` 内嵌 `LogLine` 并加上 `done` / `status`，**靠 `done` 区分**：`done` 为 true 即终止帧，其余字段无意义；否则按日志帧处理。这正是 `hookploy logs -f` 自身的做法（终止帧的 `status` 非 `succeeded` 时 CLI 退出 1）。

未来新增帧类型时，判别字段只增不改——按 `done` 分派的消费者不会被打断。

---

## HTTP 状态 API

除 `POST /hooks/<service>`（service token）外，全部端点用 admin token 鉴权（`Authorization: Bearer <hpa_...>`）。响应体与上表的 CLI `--json` 输出**逐字节同型**。

| 端点 | 响应类型 | 对应 CLI |
|---|---|---|
| `POST /hooks/<service>` | `api.Accepted`（202） | —（webhook 入口） |
| `POST /services/<name>/deploy` | `api.Accepted`（202） | `hookploy deploy <service> --json` |
| `POST /services/<name>/tasks/<task>` | `api.Accepted`（202） | `hookploy task <service> <name> --json` |
| `GET /deploys/<id>` | `api.Deploy`（含 `executions` 与逐 op 的 `ops`） | — |
| `GET /deploys/<id>/logs` | 默认纯文本回放；`?format=json` 为 `api.LogLine` NDJSON；`?follow=1` 为 NDJSON 流（回放 + 终止帧） | `hookploy logs <id> [-f] [--json]` |
| `GET /deploys?limit=N` | []`api.Deploy`（跨服务近期部署，`created_at` 降序，不含 `executions`；limit 默认 20、上限 100） | — |
| `GET /services` | []`api.ServiceSummary` | `hookploy status --json` 的 `services` |
| `GET /services/<name>` | `api.ServiceDetail` | — |
| `GET /services/<name>/deploys` | []`api.Deploy` | `hookploy deploys <service> --json` |
| `GET /servers` | []`api.ServerInfo` | `hookploy status --json` 的 `servers` |
| `POST /-/reload` | `{"ok": true}`（热重载确认，非 `internal/api` DTO） | —（见部署指南 §6） |

### `GET /services/<name>`：`api.ServiceDetail`

服务的规范化定义（`server:` 语法糖已展开为 instances + rollout）。M4 Web UI 引入，无对应 CLI 命令。

| 字段 | 类型 | 含义 | 可选 |
|---|---|---|---|
| `name` | string | 服务名 | 否 |
| `image` | string | 服务级镜像声明 | 是 |
| `webhook` | bool | 是否接受 webhook 触发 | 否 |
| `timeout` | string | Go duration 字符串（如 `10m0s`） | 否 |
| `instances` | []`api.InstanceInfo` | 部署目标列表 | 否 |
| `rollout` | [][]string | 波次 × 实例名 | 否 |
| `deploy` | []object | 部署流水线，每步为 ops 线格式 `{"op": ..., "args": {...}}`（与 DB 快照/gRPC 下发同源） | 否 |
| `tasks` | map[string][]object | 各 task 流水线，步骤格式同 `deploy` | 是 |

`api.InstanceInfo`：

| 字段 | 类型 | 含义 | 可选 |
|---|---|---|---|
| `name` | string | 实例名 | 否 |
| `server` | string | 落在哪台服务器 | 否 |
| `dir` | string | 执行目录 | 否 |

```json
{
  "name": "linkmind",
  "image": "ghcr.io/reorx/linkmind",
  "webhook": true,
  "timeout": "10m0s",
  "instances": [
    { "name": "linkmind", "server": "s1", "dir": "/opt/apps/linkmind" }
  ],
  "rollout": [["linkmind"]],
  "deploy": [
    { "op": "compose.pull" },
    { "op": "compose.up" },
    { "op": "healthcheck", "args": { "url": "http://127.0.0.1:8080/health" } }
  ]
}
```

### 错误体：`api.Error`

任何非 2xx 响应（401 缺 token、403 token 无效、404 服务/部署不存在、5xx）统一是：

| 字段 | 类型 | 含义 | 可选 |
|---|---|---|---|
| `error` | string | 错误原因；文案不在冻结范围内，请按 HTTP 状态码分支，勿匹配文案 | 否 |

```json
{ "error": "deploy not found" }
```

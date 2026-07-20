# Hookploy

中心化的 webhook 部署调度器。main 进程接收 CI（如 GitHub Actions）的 webhook，按 `hookploy.yaml` 里的服务定义把部署任务分发到目标服务器执行：main 本机走内建 executor，远程服务器走 edge 进程的 gRPC 长连接。

Go 单 binary（main 与 edge 是同一 binary 的不同子命令），无 CGO，数据存单文件 SQLite。适合一个人维护多台服务器、多个服务的场景。

## 设计要点

- 所有服务共用一个入口：一个域名加 `/hooks/<service>` 路径。GitHub Actions 侧只需要 `HOOKPLOY_URL` 和每个 repo 一个 token。
- `hookploy.yaml` 是唯一事实来源：哪个服务在哪台机、哪个目录、执行哪些步骤，集中在一份配置里，不再散落在各项目的 Actions 文件中。配置里不含任何 secret——token 存在 main 的数据库里，服务的 `.env` 走你既有的流程。
- edge 零配置接入：不开入站端口，不需要域名和证书。edge 主动外连 main 并保持长连接，身份由 server token 的 subject 推导；新服务器接入只需装 binary、跑一条命令。
- 只执行结构化 op，不执行任意脚本：main 下发的是 op 列表（`image.pin`、`compose.up` 等）加参数，edge 以 argv 直接 exec，全程不经过 shell，webhook payload 在结构上无法注入命令。
- digest 锁定与内置验证：`image.pin` 按 payload 里的 digest 拉取镜像并把本地 `:latest` 指向它，流水线末尾自动断言运行中的容器确实来自该镜像。
- 多实例 rollout：`instances` 加 `rollout` 波次，波间门控（含 healthcheck），一次 webhook 让全部节点部署同一个 digest。同服务的部署串行排队，积压时 latest-wins 去重。
- 部署可查询：每次部署有 ID、状态、逐 op 时间线和完整日志。查询命令全部支持 `--json`，输出结构已冻结，脚本和 agent 可以直接依赖。

hookploy 只负责「收 webhook、按定义执行部署步骤」。装机（binary 分发、systemd unit）、反向代理路由、服务目录与 compose 文件的准备，仍由你现有的部署手段管理。

## 架构

```
GitHub Actions
    │  POST https://hookploy.example.com/hooks/<service>
    │  Authorization: Bearer <service-token>
    ▼
┌─ main 所在机器 ──────────────────────────┐
│  Caddy ──► hookploy main                 │
│             ├─ hookploy.yaml (SSOT)      │
│             ├─ SQLite (token/历史/日志)  │
│             └─ 内建 local executor ──► 本机 docker compose
└──────────────▲───────────▲───────────────┘
               │ gRPC 长连接 │（edge 主动外连，双向流）
       ┌───────┴───┐   ┌───┴───────┐
       │  server A │   │  server B │
       │  hookploy │   │  hookploy │
       │  edge ──► docker compose  │
       └───────────┘   └───────────┘
```

单机形态下一个 main 进程就够了：main 自身内建 edge 的全部执行能力。

## 安装

从 [GitHub Releases](https://github.com/reorx/hookploy/releases) 下载对应平台的 tarball（`linux/amd64`、`darwin/arm64`），内含 binary 与 `hookploy-ctl.sh` 控制脚本：

```sh
VERSION=v0.1.0
curl -fsSLO "https://github.com/reorx/hookploy/releases/download/${VERSION}/hookploy-${VERSION}-linux-amd64.tar.gz"
curl -fsSLO "https://github.com/reorx/hookploy/releases/download/${VERSION}/checksums.txt"
sha256sum -c --ignore-missing checksums.txt

tar xzf "hookploy-${VERSION}-linux-amd64.tar.gz"
install -m 0755 "hookploy-${VERSION}-linux-amd64/hookploy" /opt/apps/hookploy/hookploy
install -m 0755 "hookploy-${VERSION}-linux-amd64/hookploy-ctl.sh" /opt/apps/hookploy/
```

从源码构建（产出与 release 同形态）：

```sh
make dist          # dist/hookploy-<version>-<os>-<arch>.tar.gz + checksums.txt
make build         # 本机平台，调试用：tmp/hookploy
```

进程管理：有 systemd 就用 systemd unit；`hookploy-ctl.sh`（PID 文件方式，只控制自己目录里的实例）是无 systemd 场景与手动运维的兜底，勿与 systemd 并用。

## 快速开始

### main 侧

最小 `hookploy.yaml`：

```yaml
listen:
  http: "127.0.0.1:9100"   # webhook + 状态 API，由反向代理反代
  grpc: "127.0.0.1:9101"   # edge 接入口

servers:
  server-a: { local: true }   # local: true = 走 main 内建 executor

services:
  myapp:
    server: server-a
    dir: /opt/apps/myapp
    image: ghcr.io/acme/myapp
    deploy:
      - image.pin        # 按 payload.digest 锁定镜像并验证
      - compose.up
```

校验、启动、签发 token：

```sh
hookploy validate -f hookploy.yaml
hookploy main -f hookploy.yaml

# token 管理仅限 main 本机执行（直接操作 SQLite）；明文只在创建时输出一次
hookploy token create myapp -f hookploy.yaml       # hpt_，给 GitHub Actions
hookploy admin-token create -f hookploy.yaml       # hpa_，给状态 API / CLI
```

GitHub Actions 侧：

```yaml
- name: Deploy
  run: |
    curl -fsS -X POST "$HOOKPLOY_URL/hooks/myapp" \
      -H "Authorization: Bearer $HOOKPLOY_TOKEN" \
      -H "Content-Type: application/json" \
      -d '{"digest": "${{ steps.build.outputs.digest }}"}'
```

webhook 立即返回 202 和 `deploy_id`，部署异步执行；需要等结果时轮询 `GET /deploys/<id>`。

### edge 接入（多机形态）

1. main 侧改配置：`hookploy.yaml` 的 `servers:` 加一行非 local 条目（`server-b: {}`），下发后 reload。未声明的机器即使 token 有效也会被拒连。
2. main 本机签发 server token：`hookploy server token create server-b -f hookploy.yaml`
3. edge 机器装 binary，跑一条命令（token 也可用 `HOOKPLOY_SERVER_TOKEN` 环境变量传）：

   ```sh
   hookploy edge --main https://hookploy.example.com --token hps_xxx
   ```

4. 验证：`hookploy status` 应显示该 server `online`，带版本与连接时长。

换机重装就是重跑第 3 步；下线一台机器则从 `servers:` 删掉并 revoke 其 token。反向代理需以 h2 反代 gRPC 口；域名走 Cloudflare 代理时还需打开 zone 的 Network → gRPC 开关——详见[部署指南](kb/docs/deployment-guide.md) §4.3。

### 多实例与 rollout

一个服务部署到多台机器时，用 `instances` 加 `rollout` 波次：

```yaml
chatsvc:
  image: ghcr.io/acme/chatsvc
  dir: /opt/apps/chatsvc
  deploy:                           # 所有实例共用同一条流水线
    - image.pin
    - compose.up
    - healthcheck: { url: "http://127.0.0.1:8080/healthz" }
  instances:
    main:  { server: server-a }
    api-1: { server: server-b }
    api-2: { server: server-c }
  rollout:
    - main              # 波 1：单实例
    - [api-1, api-2]    # 波 2：并行，波 1 全部成功后才开始
```

digest 在 rollout 层解析一次，全部实例部署同一镜像；任一实例失败则后续波取消，已完成的波不回滚。`rollout` 省略时按 `instances` 声明顺序逐实例串行。

## 配置变更

1. 改 git 里的 `hookploy.yaml`
2. `hookploy validate -f hookploy.yaml`——下发前必跑，适合放进部署脚本
3. 下发到服务器后热重载：`POST /-/reload`（带 admin token）或 `kill -HUP <pid>`（systemd 下即 `systemctl reload hookploy`）

reload 失败时 main 保留旧配置继续运行；执行中的部署用的是入队时的配置快照，不受 reload 影响。

`hookploy schema` 会输出 `hookploy.yaml` 的 JSON Schema（draft-07），生成一份放配置旁边并在文件头加 modeline，编辑器即可提供补全与实时校验：

```sh
hookploy schema > .hookploy-schema.json
```

```yaml
# yaml-language-server: $schema=./.hookploy-schema.json
listen:
  http: "127.0.0.1:9100"
```

schema 只覆盖字段名、类型与 op 参数，不做跨字段语义校验（如服务器引用是否存在），最终以 `hookploy validate` 为准。

## CLI 命令

远程查询命令通过环境变量访问 main 的状态 API，本地开发机与服务器行为一致：

```sh
export HOOKPLOY_URL=https://hookploy.example.com
export HOOKPLOY_ADMIN_TOKEN=hpa_xxx
```

| 命令 | 作用 | `--json` |
|---|---|---|
| `hookploy main -f <file>` | 运行 main（webhook + 调度器 + API） | — |
| `hookploy edge --main <url> --token <t>` | 运行 edge 执行器 | — |
| `hookploy status` | 总览：servers 在线状态 + 各服务最近部署 | 支持 |
| `hookploy deploys <service>` | 部署历史（每服务保留 50 条） | 支持 |
| `hookploy logs <deploy-id> [-f]` | 部署日志，`-f` 跟随至结束 | 支持（NDJSON） |
| `hookploy deploy <service> [--payload '{}']` | 手动触发部署（等价 webhook） | 支持 |
| `hookploy task <service> <name> [--instance <i>]` | 执行具名任务（不随 webhook 触发） | 支持 |
| `hookploy validate [-f <file>]` | 配置静态校验 | 支持 |
| `hookploy schema` | 输出 `hookploy.yaml` 的 JSON Schema | 本身即 JSON |
| `hookploy version`（`--version` / `-v`） | 打印版本号 | 支持 |
| `hookploy token create\|rotate\|revoke <service>` | service token 管理 | 支持 |
| `hookploy server token create <server>` | edge 接入 token | 支持 |
| `hookploy admin-token create` | admin token 管理 | 支持 |

token 管理类命令仅限 main 本机执行（直接操作 SQLite，admin API 不具备签发 token 的权力）。

`--json` 输出与 HTTP API 序列化同一批 DTO，结构已冻结、字段只增不改，可安全依赖——契约见 [docs/json-output.md](docs/json-output.md)。

## op 词汇表

`image.pin`（digest 锁定加内置验证）、`image.extract`（从镜像抽文件近原子交换）、`artifact.extract`（下载、sha256 校验、解压交换）、`compose.pull` / `compose.up` / `compose.run` / `compose.exec` / `compose.restart`、`env.require` / `env.write`、`healthcheck`（轮询 HTTP 直到健康）、`run`（argv 逃生舱，不经 shell）。完整参数见 `docs/PRD.md` §4，机器可读的定义在 `hookploy schema` 输出里。

除默认的 `deploy` 流水线外，服务还可定义 `tasks:` 具名任务（如数据库迁移），不随 webhook 触发，仅 `hookploy task` 手动执行；`webhook: false` 的服务只接受手动触发。

## 文档

- [docs/PRD.md](docs/PRD.md) — 设计文档：架构、安全模型、op 词汇表、部署语义、里程碑
- [kb/docs/deployment-guide.md](kb/docs/deployment-guide.md) — 部署与使用指南：单机/多机形态、edge 接入清单、反代与 gRPC 路由、token 管理、rollout 语义、配置变更、排障速查
- [docs/json-output.md](docs/json-output.md) — `--json` 与 HTTP API 的输出契约（冻结策略、字段表、NDJSON 帧）

## 开发

```sh
go test ./...        # BDD：每个行为先写测试再实现
go vet ./...
make build           # tmp/hookploy
```

proto 改动后跑 `scripts/genproto.sh` 重新生成 `internal/pb`（需要 protoc + protoc-gen-go + protoc-gen-go-grpc）。CLI golden 快照（`internal/cli/testdata/golden/`）更新用 `go test ./internal/cli -update`。

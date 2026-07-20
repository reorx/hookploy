# Hookploy

中心化、声明式的 webhook 部署调度器：**main** 接收 GitHub Actions 的 webhook，按 `hookploy.yaml`（SSOT）把部署任务分发到目标服务器执行——本机走内建 executor，远程服务器走 **edge** 进程的 gRPC 长连接。

Go 单 binary，无 CGO，数据存单文件 SQLite。为一人公司 + AI-native 工作流设计。

## 核心特性

- **一个入口承接全部服务**：一个域名 + `/hooks/<service>`；GitHub Actions 侧只需 `HOOKPLOY_URL` + 每 repo 一个 token。
- **一份 SSOT**：`hookploy.yaml` 集中定义"哪个服务、在哪台机、哪个目录、跑哪些步骤"，部署细节不再散落在各项目的 Actions 文件里。
- **edge 零配置接入**：无入站端口、无域名、无证书、无本地配置。edge 主动外连 main，身份由 server token 的 subject 推导；新服务器接入 = 装 binary + 一条命令。
- **只执行结构化 op，不执行任意脚本**：main 下发的是 op 列表（`compose.up`、`image.pin`…）+ 参数，edge 以 argv 数组直接 exec，全程不经 shell——**webhook payload 在结构上无法注入命令**。
- **digest 锁定 + 内置验证**：`image.pin` 按 `payload.digest` 拉取并把本地 `:latest` 指向它；流水线末尾自动断言运行容器确实是这个镜像，"忘写 verify 导致白 pin"被协议堵死。
- **多实例 rollout**：`instances` + `rollout` 波次，波间门控（含 healthcheck），一次 webhook 让全部节点 pin 同一个 digest。
- **部署可查询**：每次部署有 ID、状态、逐 op 时间线与完整日志；查询命令全量支持 `--json`，输出结构 M3 起冻结。

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

main 与 edge 是**同一个 binary**，只是运行的子命令不同。main 自身内建 edge 的全部执行能力——单机形态下一个 main 进程就够了。

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

从源码构建（产出与 release 同形态的 tarball + checksums）：

```sh
make dist          # dist/hookploy-<version>-<os>-<arch>.tar.gz + checksums.txt
make build         # 本机平台，调试用：tmp/hookploy
```

进程管理：有 systemd 就用 systemd unit；`hookploy-ctl.sh`（PID 文件方式，只控制自己目录里的实例）是无 systemd 场景与手动运维的兜底，**勿与 systemd 并用**。

## 快速开始

### 1. main 侧

最小 `hookploy.yaml`：

```yaml
listen:
  http: "127.0.0.1:9100"   # webhook + 状态 API，由 Caddy 反代
  grpc: "127.0.0.1:9101"   # edge 接入口

servers:
  server-a: { local: true }   # local: true = 走 main 内建 executor

services:
  linkmind:
    server: server-a
    dir: /opt/apps/linkmind
    image: ghcr.io/reorx/linkmind
    deploy:
      - image.pin        # 按 payload.digest 锁定镜像并验证
      - compose.up
```

校验、启动、签发 token：

```sh
hookploy validate -f hookploy.yaml
hookploy main -f hookploy.yaml

# token 管理仅限 main 本机执行（直接操作 SQLite）；明文只输出一次
hookploy token create linkmind -f hookploy.yaml    # hpt_，给 GitHub Actions
hookploy admin-token create -f hookploy.yaml       # hpa_，给状态 API / CLI
```

GitHub Actions 侧：

```yaml
- name: Deploy
  run: |
    curl -fsS -X POST "$HOOKPLOY_URL/hooks/linkmind" \
      -H "Authorization: Bearer $HOOKPLOY_TOKEN" \
      -H "Content-Type: application/json" \
      -d '{"digest": "${{ steps.build.outputs.digest }}"}'
```

### 2. edge 接入（多机形态）

1. **main 侧**：`hookploy.yaml` 的 `servers:` 加一行非 local 条目（`server-b: {}`），下发后 reload。未声明的机器即使 token 有效也会被拒连。
2. **main 本机签发 server token**：`hookploy server token create server-b -f hookploy.yaml`
3. **edge 机器**：装 binary，跑一条命令即可（token 也可用 `HOOKPLOY_SERVER_TOKEN` 环境变量传）：

   ```sh
   hookploy edge --main https://hookploy.example.com --token hps_xxx
   ```

4. **验证**：`hookploy status` 应显示该 server `online` + 版本 + 连接时长。

Caddy 需以 h2 反代 gRPC 口，Cloudflare 橙云还需打开 zone 的 Network → gRPC 开关——详见[部署指南](kb/docs/deployment-guide.md) §4.3。

## CLI 命令参考

远程查询命令通过环境变量访问 main 的状态 API，本地开发机与服务器行为一致：

```sh
export HOOKPLOY_URL=https://hookploy.example.com
export HOOKPLOY_ADMIN_TOKEN=hpa_xxx
```

| 命令 | 作用 | `--json` |
|---|---|---|
| `hookploy main -f <file>` | 运行 main（webhook + 调度器 + API） | — |
| `hookploy edge --main <url> --token <t>` | 运行 edge 执行器 | — |
| `hookploy status` | 总览：servers 在线状态 + 各服务最近部署 | ✅ |
| `hookploy deploys <service>` | 部署历史（每服务保留 50 条） | ✅ |
| `hookploy logs <deploy-id> [-f]` | 部署日志，`-f` 跟随至结束 | ✅（NDJSON） |
| `hookploy deploy <service> [--payload '{}']` | 手动触发部署（等价 webhook） | ✅ |
| `hookploy task <service> <name> [--instance <i>]` | 执行具名任务（不随 webhook 触发） | ✅ |
| `hookploy validate [-f <file>]` | 配置静态校验 | ✅ |
| `hookploy schema` | 输出 `hookploy.yaml` 的 JSON Schema | 输出本身即 JSON |
| `hookploy version`（`--version` / `-v`） | 打印版本号 | ✅ |
| `hookploy token create\|rotate\|revoke <service>` | service token 管理 | ✅ |
| `hookploy server token create <server>` | edge 接入 token | ✅ |
| `hookploy admin-token create` | admin token 管理 | ✅ |

token 管理类命令仅限 main 本机执行（直接操作 SQLite，避免 admin API 拥有发 token 的权力）。

`--json` 输出结构自 M3 起冻结、字段只增不改，可安全依赖——契约详见 [docs/json-output.md](docs/json-output.md)。

## 编辑器集成

`hookploy schema` 输出 `hookploy.yaml` 的 JSON Schema（draft-07）。生成一份放在配置旁边：

```sh
hookploy schema > .hookploy-schema.json
```

然后在 `hookploy.yaml` 文件头加一行 modeline，yaml-language-server（VS Code YAML 扩展、Neovim LSP 等）即可提供补全、悬停文档与实时校验：

```yaml
# yaml-language-server: $schema=./.hookploy-schema.json
listen:
  http: "127.0.0.1:9100"
```

schema 是**宽松上界**：它覆盖磁盘形态（字段名、类型、op 参数、互斥结构），但不做跨字段语义校验（服务器引用是否存在、rollout 是否恰好覆盖全部实例等）。**最终以 `hookploy validate` 为准**——playbook 下发前必跑。

## 文档

- [docs/PRD.md](docs/PRD.md) — 设计文档：架构、安全模型、op 词汇表、部署语义、里程碑
- [kb/docs/deployment-guide.md](kb/docs/deployment-guide.md) — 部署与使用指南：单机/多机形态、edge 接入清单、Caddy 路由、token 管理、rollout 语义、运维与排障
- [docs/json-output.md](docs/json-output.md) — `--json` 与 HTTP API 的输出契约（冻结策略、字段表、NDJSON 帧）

## 开发

```sh
go test ./...        # BDD：每个行为先写测试再实现
go vet ./...
make build           # tmp/hookploy
```

proto 改动后跑 `scripts/genproto.sh` 重新生成 `internal/pb`（需要 protoc + protoc-gen-go + protoc-gen-go-grpc）。

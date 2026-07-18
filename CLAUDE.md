# Hookploy

设计文档见 `docs/PRD.md`。开发方法论：BDD（先写行为测试再实现）。

## 正式实例（ali-hk-01，M1.5 起）

- 由 deploy 仓库的 `ansible/roles/hookploy` 部署（binary 上传 + `hookploy.yaml` SSOT 模板 + systemd unit + Caddy 路由），**改配置一律改 role 模板再 `ansible-playbook -i inventory.yml playbook.yml --limit ali-hk-01 --tags hookploy`**，勿手改服务器文件。
- 目录 `/opt/apps/hookploy/`，公网入口 `https://hookploy.reorx.com`（Cloudflare 橙云 → Caddy → 127.0.0.1:9100）。
- 进程管理：`systemctl {status,restart} hookploy`；ctl 脚本仅作无 systemd 场景兜底，勿与 systemd 并用。
- 发布新版本：hookploy repo 里 `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o dist/hookploy-linux-amd64 ./cmd/hookploy`，然后跑上面的 playbook（role 从 `~/Code/hookploy/dist/` 上传）。
- 试点服务：linkmind（GHA 传 digest → `image.pin` + `compose.up`）；其余服务仍走旧 adnanh/webhook，M3 全量迁移。

## 真机测试规范（ali-hk-01）

测试部署与未来的正式部署**同机共存**，靠路径和端口隔离；测试部署是临时的、手动的，不进 Ansible SSOT（正式部署由 M3 的 Ansible role 负责）。

### 端口约定

| 用途 | 正式（默认） | 测试 |
|---|---|---|
| main HTTP（webhook + API） | 9100 | **9180** |
| main gRPC（edge 接入） | 9101 | **9181** |
| echo_server 测试服务 | — | **9190** |

测试永远不占用默认端口 9100/9101。

### 路径约定

- `/opt/apps/hookploy_test/` — 测试二进制 `hookploy`、控制脚本 `hookploy-ctl.sh`、`hookploy.yaml`、`hookploy.db`、`hookploy.pid`、`main.log`、`.echo_token`、`.admin_token`（token 文件 0600）
- `/opt/apps/echo_server/` — 测试服务，`docker-compose.yml` 跑 `traefik/whoami`（127.0.0.1:9190→80），流水线：`compose.pull` → `compose.up` → `healthcheck`

本地侧的配置源文件在 `tmp/deploy-test/`（hookploy.yaml、docker-compose.yml），改动后 scp 覆盖服务器对应文件。

### 构建与上传

```sh
go test ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o tmp/hookploy-linux-amd64 ./cmd/hookploy
scp tmp/hookploy-linux-amd64 ali-hk-01:/opt/apps/hookploy_test/hookploy
```

改动控制脚本后同样 scp：`scp scripts/hookploy-ctl.sh ali-hk-01:/opt/apps/hookploy_test/hookploy-ctl.sh`

### 启动 / 重启 / 验证

进程管理一律通过 `hookploy-ctl.sh`（PID 文件方式，只控制自己目录里的实例）。
**禁止用 pkill / pgrep 控制进程**：同机可能存在正式版 hookploy，按进程名匹配会误杀；
`pkill -f "hookploy main"` 还会匹配到 ssh 远程 shell 自身、直接杀掉会话。

```sh
# start / stop / restart / status / logs [-f]
ssh ali-hk-01 '/opt/apps/hookploy_test/hookploy-ctl.sh restart'
ssh ali-hk-01 '/opt/apps/hookploy_test/hookploy-ctl.sh status'

# 触发部署（webhook）
ssh ali-hk-01 'cd /opt/apps/hookploy_test && curl -sS -X POST http://127.0.0.1:9180/hooks/echo_server -H "Authorization: Bearer $(cat .echo_token)" -H "Content-Type: application/json" -d "{}"'

# 查询状态（CLI 走 admin API）
ssh ali-hk-01 'cd /opt/apps/hookploy_test && export HOOKPLOY_URL=http://127.0.0.1:9180 HOOKPLOY_ADMIN_TOKEN=$(cat .admin_token) && ./hookploy status && ./hookploy deploys echo_server'
```

token 丢失时重新生成：`./hookploy token create echo_server -f hookploy.yaml`（service）、`./hookploy admin-token create -f hookploy.yaml`（admin），写入对应 dotfile。

### 清理

```sh
ssh ali-hk-01 '/opt/apps/hookploy_test/hookploy-ctl.sh stop; cd /opt/apps/echo_server && docker compose down; rm -rf /opt/apps/hookploy_test /opt/apps/echo_server'
```

### 注意

- ali-hk-01 是 x86_64 主力生产机，SSH 见 `~/.ssh/config`（root@8.210.184.77:1122）。测试期间不得动生产服务（linkmind / breeze / simul 等）与默认端口。
- `scripts/hookploy-ctl.sh` 是发布产物的一部分：打包发布时随二进制一起分发，测试与正式部署都用它做进程控制；正式运行（M3 Ansible role）另外提供 systemd unit，ctl 脚本作为无 systemd 场景与手动运维的兜底。

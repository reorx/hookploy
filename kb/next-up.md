# Next up

> 接下来要做的事，按时间排。做完一项就删掉，结果记到条目指定的 kb 文档，这里不留记录。

## 用户确认发布之后（v0.7.0：step timeout/retries）

- **发布**：master push + 打 `v0.7.0` tag 触发 GitHub Actions 出 release。先问用户。
- **生产升级，顺序先 edge 后 main**（理由见 `kb/docs/deployment-guide.md` §4.7）：deploy 仓库 `ansible/group_vars/all.yml` 的 `hookploy_version` → v0.7.0 + 新 sha256；先 `--tags hookploy-edge` 升两台 edge，`hookploy status` 确认两台 v0.7.0 online；再把 `ansible/roles/hookploy/templates/hookploy.yaml.j2` 里 6 处 `healthcheck: { …, retries: 10 }` 改成 `attempts: 10`，与 main 升级同一次下发，确认 main 起来、`validate` 通过。
- **接线 vocalflow-rt**：模板里 vocalflow-rt 的 `image.pin` 加 `timeout: 3m` + `retries: 2`（其他镜像 CD 服务按需），顺带定 `defaults.timeout` 要不要抬到 15m；reload 后用 2026-09-24 事故的同 digest 手动 `hookploy deploy` 一次，期望 succeeded、日志无 attempt 行。
  → 结果记到 `kb/plans/2026-09-24-op-timeout-and-retry-plan.md`「实施记录」，并删掉 `kb/known-issues.md` 里「v0.7.0 尚未发布」一条

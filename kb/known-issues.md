# Known Issues

## healthcheck：单次请求没有独立超时，服务挂住时一次轮询就能吃掉 5 分钟

- 记录：2026-09-24（写 `kb/plans/2026-09-24-op-timeout-and-retry-plan.md` 时发现）
- `healthcheck` 用的是 engine 共享的 `http.Client{Timeout: 5 * time.Minute}`（`internal/cli/cmd_main.go`、`internal/edge/edge.go` 构造 engine 处，与 `artifact.extract` 下载共用）。服务 accept 连接但不响应时，一次轮询就要 5 分钟，`attempts` 次数再多也等不到第二次。connection refused / 非 200 会立即返回，不受影响。
- 现状缓解：v0.7.0 起可在 healthcheck 步骤上加 step 修饰符 `timeout:` 给整段轮询封顶，但单次请求仍无独立超时。
- 处理方向：每次请求用 `interval` 量级（或独立参数）的 ctx 超时。

## v0.7.0（step timeout/retries）尚未发布、未在生产验证

- 记录：2026-09-25（`kb/plans/2026-09-24-op-timeout-and-retry-plan.md` 验收 6）
- 代码与测试环境真机验证已完成；GitHub release、生产三台升级、deploy 仓库模板里 6 处 `healthcheck … retries:` 改名、vocalflow-rt `image.pin` 加修饰符后的同 digest 手动 deploy 验证都还没做。步骤见 `kb/next-up.md`。

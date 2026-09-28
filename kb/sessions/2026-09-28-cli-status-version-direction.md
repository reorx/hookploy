---
created: 2026-09-28
tags:
  - hookploy
  - cli
  - version
  - bugfix
---

# CLI status 按方向标注 edge 版本：落后、领先、无法比较分开标

## 概要

处理 2026-09-25 v0.7.0 生产升级时登记的 known issue：`hookploy status` 只要 edge 版本和 main 不相等就标 `(outdated)`，按"先 edge 后 main"升级时，已经升好的新 edge 也被标成 `(outdated)`，部署手册的排障表却把这个标记解释为"edge 落后于 main"。先写行为测试，`e-ahead v0.8.0 (outdated)` 这一行复现了问题，然后修复。`version` 包新增 `Compare(a, b) (int, ok)`，按数字核心号 vX.Y.Z 排序，后缀忽略，`dev` 和解析不了的版本返回 ok=false；`AtLeast` 改为基于它实现，行为不变（原有表驱动测试全过）。CLI 新增 `versionMark`：落后标 `(outdated)`，领先标 `(ahead of main)`，核心号相同但字符串不同（`-rc.1`、`git describe` 距离后缀）或有一方无法解析（`dev`）标 `(differs from main)`，相等和离线的 edge 不标。Web UI 的服务器清单只显示版本号，没有同类标记，不需要改。`--json`、`internal/api` DTO 都没动。`go test ./...` 全绿。

## 修改的文件

- `internal/version/version.go`：新增 `Compare`；`AtLeast` 改为调用 `Compare`，`dev` 视为满足任何下限的特判保留在 `AtLeast`。
- `internal/version/version_test.go`：新增 `TestCompare`（排序、后缀忽略、`dev`/空串/裸 hash/两段版本号不可比）。
- `internal/cli/cmd_remote.go`：`cmdStatus` 的标记改用新增的 `versionMark(edge, main)`。
- `internal/cli/cmd_remote_test.go`：新增 `serveStatusAPI`（用固定的 `/servers` 行伪造 admin API）、`statusLine`；新增 `TestStatusVersionMarks`（相同/落后/领先/rc/dev/离线六种 edge）和 `TestStatusVersionMarksNeedLocalServer`。
- `kb/docs/deployment-guide.md`：§2 构建一节改写标记说明（三种标记 + main 版本取自 local server）；§4.7 升级第 1 步注明期间会出现 `(ahead of main)`，并说明 v0.7.0 及更早的 CLI 会误标；§7 排障表新增 `(ahead of main)`、`(differs from main)` 两行。
- `kb/known-issues.md`：删除「CLI status：edge 版本比 main 新时也标 `(outdated)`」。

## 注意事项

- **`Compare` 和 `AtLeast` 对 `dev` 的处理故意不同**。`AtLeast` 用在版本门控上，源码树构建要能拿到全部功能，所以 `dev` 满足任何下限。`Compare` 用来给人看先后，`dev` 在序列里没有位置，硬排反而会误导。
- **同核心号不同后缀不排序**。`v0.7.0-rc.1` 按 semver 早于 `v0.7.0`，`git describe` 生成的 `v0.7.0-3-gabc` 却晚于它，两种后缀在字符串结构上很像。要可靠区分就得靠正则猜后缀类型，所以统一标 `(differs from main)`，和 `AtLeast` "后缀按其核心号算" 的约定一致。
- **main 的版本只能从 local server 那一行拿到**。`/servers` 的 DTO 里没有 main 版本字段（DTO 已冻结），配置里没有 `local: true` 的 server 时，CLI 无从比较，也就不打任何标记。这是原有行为，现在有测试锁定并写进了手册。
- CLI 的渲染测试用的是伪造的 admin API（`serveStatusAPI`），不用起整套 main：`api.ServerInfo` 是冻结的 DTO，直接用真实类型编码，形状不会漂移。

## 遗留问题

- 无。本次只改 CLI 显示；修复要等下一次发版、main 所在机器换上新的 CLI 才生效，不需要为它单独发版。

## 已解决的已知问题

- **CLI status：edge 版本比 main 新时也标 `(outdated)`**：新增 `version.Compare` 区分方向，领先标 `(ahead of main)`、无法比较标 `(differs from main)`。`TestStatusVersionMarks` 先失败（`e-ahead` 被标 `(outdated)`）后通过，全量测试通过。Web UI 已检查，没有同类标记。

## 相关文档

- [v0.7.0 发布与生产升级](2026-09-25-v0.7.0-release-and-production-upgrade.md) — 参考：本问题在那次升级中发现并登记
- [部署与使用手册](../docs/deployment-guide.md) — 更新：版本标记说明、§4.7 升级步骤、§7 排障表

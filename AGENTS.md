# Magpie-Keeper — Agent 维护指南

本项目是独立于字幕主项目的 Magpie 运维与百炼免费试用池工具（Go 1.24+ + 可选 Node 额度桥接）。当前实现以仓库现有代码与自动化测试为事实基准；根目录 [SPEC.md](SPEC.md) 作为架构边界与外部协议参考（变更架构或外部协议时同步更新）。**本项目独立演进，不依赖也不修改旧版 Python 目录 `d:\Program Files\Transcribe`。**

## 1. 强制依赖 DAG（禁止破坏）

- `internal/domain`：仅允许导入 Go 标准库，零外部依赖；冻结模型与端口接口。
- `internal/config`、`internal/state`、`internal/magpie`、`internal/catalog`、`internal/admit`、`internal/planner`、`internal/bailian`：只允许导入 `internal/domain`（及自身子包），**严禁横向互调**。
- `internal/coordinator`：唯一的 Single-Writer Actor，串行执行所有对 `magpie.com` 和 `data/state.json` 的写操作。
- `cmd/magpie-keeper`：CLI 装配、信号处理与 `--companion` stdin-EOF 监听。

## 2. 工程与运维红线

- **严禁直接手写修改 Magpie 的 `providers.json`**：所有分组和通道变更必须先备份 `providers.json`，再通过 `<magpie_dir>\magpie.com` 子进程执行。
- **Magpie 约束**：禁止将分组减空（至少保留 1 个成员）；百炼试用子通道必须附加 `header.X-Transcribe-Trial-Model=<model>` 隔离冷却键；必须尊重用户手动移除/停用（`manual_hold`）。修改注册表解析时，必须保持 `internal/magpie/magpie_test.go` 中的 `TestReadRegistryRealMagpieFormats` 通过，确保兼容真实 Magpie JSON 格式。
- **Windows 路径与凭据安全**：本机路径普遍含空格（如 `D:\Program Files\...`），所有子进程调用必须使用 `exec.CommandContext(ctx, bin, args...)` 传参并隐藏窗口（`CREATE_NO_WINDOW`）。定位 `bailian-cli` 时优先查 `bl.cmd`（回退 `bl.ps1`）所在目录的 `node_modules\bailian-cli`。AK/SK 仅通过 stdin 管道传给 `scripts/bailian-quota-bridge.mjs`，绝不写入日志、命令行参数或临时文件。
- **单文件预算**：核心 `.go` 文件建议控制在 **150～350 行**内（复杂解析逻辑按职责拆分），`internal/planner` 保持 100% 纯函数。
- **私有数据与测试分层**：`data/channels.json`、`data/state.json`、`data/backups/` 为本机私有与运行态数据，严禁入库或输出真实密钥 / workspace ID；修复缺陷先补离线单测 / Mock 契约测试；未经用户确认，不自动执行消耗验收日预算或改写本机 Magpie 配置的命令（如 `sync --apply`、`sync-trial`、`watch`），调试后不遗留污染的 `data/state.json`。

## 3. 标准验证命令（Windows PowerShell）

```powershell
go vet ./...
go test ./...   # 本机无 CGo 编译器，默认不加 -race；CI 或已配置 CGo/MinGW 时再跑 go test -race ./...
go build -o magpie-keeper.exe ./cmd/magpie-keeper
.\magpie-keeper.exe status   # 离线只读检查，不触发云端同步、不写入 data/state.json
```

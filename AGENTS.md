# Magpie-Keeper — Agent 开发铁律

本项目是完全独立于字幕主项目的 Magpie 运维与百炼免费试用池工具（Go 1.24+ + 可选 Node 额度桥接）。所有规格以根目录 [SPEC.md](SPEC.md) 为唯一真相来源。**严禁读取、搜索或参考本工作区以外的任何旧版项目代码。**

## 1. 强制依赖 DAG（禁止破坏）

- `internal/domain`：仅允许导入 Go 标准库，零外部依赖；冻结模型与端口接口。
- `internal/config`、`internal/state`、`internal/magpie`、`internal/catalog`、`internal/admit`、`internal/planner`、`internal/bailian`：只允许导入 `internal/domain`（及自身子包），**严禁横向互调**。
- `internal/coordinator`：唯一的 Single-Writer Actor，串行执行所有对 `magpie.com` 和 `data/state.json` 的写操作。
- `cmd/magpie-keeper`：CLI 装配、信号处理与 `--companion` stdin-EOF 监听。

## 2. 工程红线

- **严禁直接手写修改 Magpie 的 `providers.json`**：所有分组和通道变更必须先备份 `providers.json`，再通过 `<magpie_dir>\magpie.com` 子进程执行。
- **Magpie 约束**：禁止将分组减空（至少保留 1 个成员）；百炼试用子通道必须附加 `header.X-Transcribe-Trial-Model=<model>` 隔离冷却键；必须尊重用户手动移除/停用（`manual_hold`）。
- **Windows 路径与凭据安全**：本机路径普遍含空格（包括 npm 全局目录如 `C:\Users\William King\AppData\Roaming\npm\`），所有子进程调用必须使用 `exec.CommandContext(ctx, bin, args...)` 传参并隐藏窗口（`CREATE_NO_WINDOW`）。定位 `bailian-cli` 时优先查 `bl.cmd`（回退 `bl.ps1`）所在目录的 `node_modules\bailian-cli`。AK/SK 仅通过 stdin 管道传给 `scripts/bailian-quota-bridge.mjs`，绝不写入日志、命令行参数或临时文件。
- **单文件预算**：核心 `.go` 文件控制在 150～350 行内，`internal/planner` 保持 100% 纯函数。

## 3. 标准验证命令（Windows PowerShell）

```powershell
go vet ./...
go test -race ./...
go build -o magpie-keeper.exe ./cmd/magpie-keeper
```

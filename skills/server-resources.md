# Skill：服务器资源巡检（CPU / 内存 / 负载 / 进程 / 磁盘）

## 目标
对选中的每台服务器巡检 CPU、内存、负载、进程（含僵尸进程）与指定挂载点磁盘，并产出 🟢正常 / 🟡注意 / 🔴异常 的判定。

## 流程
1. 调用 `tools/list`，确认 `check_host_resources`、`check_disk` 已注册。
2. 对每台服务器调用 `check_host_resources`（传 `host`、`via`）。
3. 对每台服务器调用 `check_disk`（传 `host`、`via`、`mounts`）。
4. 按阈值规则判定，逐项标注级别。

## 判定规则（阈值取自 config/inspection.toml [thresholds]）
- CPU 使用率 ≥ `cpu_warn` → 🟡注意。
- 内存使用率 ≥ `mem_warn` → 🟡注意。
- load1 ≥ 核数 × `load_per_core_warn` → 🟡注意。
- 存在僵尸进程 → 🟡注意，列出 PID 与命令。
- 磁盘使用率 ≥ `disk_warn` → 🟡注意；≥ `disk_crit` → 🔴异常。
- 主机不可达 / 命令执行失败 → 🔴异常（检查失败，看巡检日志）。

## 约束
- 本 Skill 只描述规则，不执行工具；判断在工作流（inspect_runner + judges.py）完成。
- 发现异常只报告，不自动修复。

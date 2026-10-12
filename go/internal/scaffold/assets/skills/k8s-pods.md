# Skill：K8s Pod 健康巡检（命名空间可多选）

## 目标
巡检选中命名空间下所有 Pod 的健康度：Phase、Ready 比例、重启次数。

## 流程
1. 调用 `tools/list`，确认 `check_pods` 已注册。
2. 调用 `check_pods`，一次传入所有选中的 `namespaces`。
3. 对每个 Pod 按规则判定。

## 判定规则
- Pod Phase 不在 `Running` / `Succeeded`，或 Ready 比例不满 → 🔴异常。
- 重启次数 ≥ `pod_restart_warn` → 🟡注意。
- 其余 → 🟢正常。
- kubectl 不存在或集群不可达（工具返回 `available=false`）→ 本节记 🟡注意，报告写明原因，不中断其他巡检。

## 约束
- 本 Skill 只描述规则，不执行工具。
- kubectl 在控制机本地执行，依赖控制机的 kubeconfig。

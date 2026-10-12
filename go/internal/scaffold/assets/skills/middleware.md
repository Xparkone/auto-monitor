# Skill：中间件服务健康巡检（binary / container / k8s）

## 目标
对选中的中间件服务逐一检查健康度，覆盖三种部署方式。

## 流程
1. 调用 `tools/list`，确认 `check_middleware` 已注册。
2. 按服务所在主机分组调用 `check_middleware`（传 `host`、`via`、`services`）。
3. 工具内部固定顺序：先用 socket 做 TCP 端口探测（无需登录目标机），再按部署方式检查：
   - `binary`：`systemctl is-active <process>`，回退 `pgrep -f <process>`。
   - `container`：`docker ps --filter name=<container>` 存在运行中容器。
   - `k8s`：`kubectl -n <ns> get deploy <name>` 的可用副本数 ≥ 期望副本数。
4. 按规则判定。

## 判定规则
- 端口不可达 → 🔴异常（端口未监听）。
- 部署检查失败（docker/kubectl 缺失或命令报错）→ 🟡注意（检查失败，看巡检日志）。
- 进程/容器/副本不存活 → 🔴异常。
- 端口通且部署存活 → 🟢正常。

## 约束
- 本 Skill 只描述规则，不执行工具。
- 发现异常只报告，不自动重启。

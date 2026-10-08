"""中间件部署方式探针注册表。

每种部署方式一个 @register_probe("类型名") 探针函数：
    fn(svc=dict, host=str, via=str) -> {"alive": bool | None, "detail": str, "command": str}
    alive=None 表示检查执行失败（如 docker/kubectl 缺失），由判定层降级为 🟡。

新增部署方式（如 podman、supervisor、compose）：在此或任意被导入的模块中再写一个
@register_probe("podman") 函数即可，config 的 deploy 校验、check_middleware 自动生效。
"""

from __future__ import annotations

from . import collectors as C

PROBES: dict = {}


def register_probe(deploy_type: str):
    def decorator(func):
        PROBES[deploy_type] = func
        return func

    return decorator


@register_probe("binary")
def probe_binary(svc: dict, host: str, via: str) -> dict:
    """二进制部署：systemctl 单元 → 进程匹配（ps | grep '[首字母]其余' 括号技巧）。

    不用 pgrep -f：macOS 部分执行上下文中 pgrep 读不到其他进程 argv（误报不存活），
    且 ssh 远端执行时探测命令自身 argv 含关键字会自匹配（误报存活）。
    """
    process = svc.get("process") or svc.get("name") or ""
    quoted = C.shell_quote(process)
    regex = C.shell_quote(f"[{process[0]}]{process[1:]}" if process else "")
    cmd = (f"if command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet {quoted} 2>/dev/null; "
           f"then echo active; elif ps -eo args 2>/dev/null | grep -E -- {regex} >/dev/null 2>&1; "
           f"then echo active; else echo inactive; fi")
    res = C.run(cmd, via, host)
    alive = res.ok and res.stdout.strip() == "active"
    return {"alive": alive, "detail": f"systemctl/ps {process}: {res.stdout.strip() or res.error}",
            "command": res.command}


@register_probe("container")
def probe_container(svc: dict, host: str, via: str) -> dict:
    """容器部署：docker ps 中存在运行中的同名容器（精确匹配容器名）。

    --filter name= 是子串匹配（mysql 会命中 mysql-jump），必须在结果里按首列容器名精确过滤，
    否则会因近名容器存活而误报存活。
    """
    container = svc.get("container") or svc.get("name") or ""
    cmd = f"docker ps --filter name={C.shell_quote(container)} --format '{{{{.Names}}}} {{{{.Status}}}}'"
    res = C.run(cmd, via, host)
    if not res.ok:
        return {"alive": None, "detail": f"docker 检查失败: {(res.stderr or res.error).strip()}",
                "command": res.command}
    lines = [l for l in res.stdout.splitlines() if l.strip()]
    exact = C.match_container_lines(lines, container)
    if exact:
        return {"alive": True, "detail": f"docker ps: {exact[0]}", "command": res.command}
    detail = "无运行中的同名容器"
    if lines:
        detail += f"（仅近似匹配: {'、'.join(l.split()[0] for l in lines[:3])}）"
    return {"alive": False, "detail": detail, "command": res.command}


@register_probe("k8s")
def probe_k8s(svc: dict, host: str, via: str) -> dict:  # noqa: ARG001 - kubectl 在控制机本地执行
    """K8s 部署：Deployment 可用副本数 ≥ 期望副本数。"""
    k8s = svc.get("k8s") or {}
    namespace = k8s.get("namespace", "default")
    deployment = k8s.get("deployment") or svc.get("name") or ""
    cmd = (f"kubectl -n {C.shell_quote(namespace)} get deploy {C.shell_quote(deployment)} "
           "-o jsonpath='{.status.availableReplicas}/{.spec.replicas}'")
    res = C.run(cmd, "local")
    if not res.ok:
        return {"alive": None, "detail": f"kubectl 检查失败: {(res.stderr or res.error).strip()}",
                "command": res.command}
    parsed = C.parse_deploy_replicas(res.stdout)
    if not parsed:
        return {"alive": None, "detail": f"副本数解析失败: {res.stdout!r}", "command": res.command}
    return {"alive": parsed["alive"], "detail": f"副本 {parsed['available']}/{parsed['desired']}",
            "command": res.command}

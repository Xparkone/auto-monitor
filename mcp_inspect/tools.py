"""内置 MCP 工具。用 @register_tool 注册，新增工具加在这里（或任意被导入的模块）即可。

工具只负责采集并返回结构化 JSON；阈值判断在工作流侧（sections/*.py + judges.py）。
每个结果都携带 commands（在目标机上执行过的命令）和 errors，供巡检日志排查。
"""

from __future__ import annotations

from . import collectors as C
from .probes import PROBES
from .registry import call_tool, register_tool, tool_specs, TOOLS

__all__ = ["tool_specs", "call_tool", "register_tool", "TOOLS"]


@register_tool(
    "check_host_resources",
    "巡检单台服务器的 CPU、内存、负载、进程（含僵尸进程）",
    {
        "type": "object",
        "properties": {
            "host": {"type": "string", "description": "目标主机地址"},
            "via": {"type": "string", "enum": ["local", "ssh"], "description": "采集方式，默认 ssh"},
        },
        "required": ["host"],
    },
)
def check_host_resources(args: dict) -> dict:
    host = args.get("host") or ""
    via = args.get("via", "ssh")
    result = {"tool": "check_host_resources", "host": host, "via": via,
              "ok": False, "commands": [], "errors": []}

    def exec_on(msg):
        res = C.run(msg, via, host)
        result["commands"].append(res.command)
        return res

    os_res = exec_on("uname -s")
    if not os_res.ok:
        result["errors"].append(f"主机不可达或命令失败: {os_res.error} {os_res.stderr.strip()}".strip())
        return result
    os_name = os_res.stdout.strip()
    result["os"] = os_name

    # CPU：Linux 用 /proc/stat 差值，macOS 用 top 第二次采样
    if os_name == "Darwin":
        cpu_res = exec_on("top -l 2 -n 0 -s 1 | grep 'CPU usage' | tail -1")
        percent = C.parse_cpu_darwin(cpu_res.stdout)
    else:
        cpu_res = exec_on("c1=$(grep '^cpu ' /proc/stat); sleep 1; "
                          "c2=$(grep '^cpu ' /proc/stat); printf '%s\\n%s\\n' \"$c1\" \"$c2\"")
        percent = C.parse_cpu_linux(cpu_res.stdout)
    if percent is not None:
        result["cpu_percent"] = percent
    elif not cpu_res.ok:
        result["errors"].append(f"CPU 采集失败: {cpu_res.error}")

    # 内存
    mem_cmd = "sysctl -n hw.memsize vm.pagesize && vm_stat" if os_name == "Darwin" else "free -m"
    mem_res = exec_on(mem_cmd)
    mem = C.parse_mem_darwin(mem_res.stdout) if os_name == "Darwin" else C.parse_mem_linux(mem_res.stdout)
    if mem:
        result.update(mem_total_mb=mem["total_mb"], mem_used_mb=mem["used_mb"], mem_percent=mem["percent"])
    elif not mem_res.ok:
        result["errors"].append(f"内存采集失败: {mem_res.error}")

    # 负载与核数
    up_res = exec_on("uptime")
    load = C.parse_load(up_res.stdout)
    if load:
        result.update(load)
    cores_res = exec_on("nproc 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null")
    try:
        result["cores"] = int(cores_res.stdout.strip().splitlines()[-1])
    except (ValueError, IndexError):
        pass

    # 进程：僵尸进程 + CPU Top5
    ps_res = exec_on("ps -eo pid=,pcpu=,pmem=,state=,comm=")
    if ps_res.ok:
        zombies, top = C.parse_ps(ps_res.stdout)
        result["zombies"] = zombies
        result["top_processes"] = top
    else:
        result["errors"].append(f"进程采集失败: {ps_res.error}")

    result["ok"] = not result["errors"]
    return result


@register_tool(
    "check_disk",
    "巡检单台服务器指定挂载点的磁盘使用率",
    {
        "type": "object",
        "properties": {
            "host": {"type": "string"},
            "via": {"type": "string", "enum": ["local", "ssh"]},
            "mounts": {"type": "array", "items": {"type": "string"}},
        },
        "required": ["host", "mounts"],
    },
)
def check_disk(args: dict) -> dict:
    host = args.get("host") or ""
    via = args.get("via", "ssh")
    mounts = args.get("mounts") or ["/"]
    loop = "; ".join(f'echo "== {m}"; df -k -P {C.shell_quote(m)} 2>&1 | tail -1' for m in mounts)
    res = C.run(loop, via, host)
    result = {"tool": "check_disk", "host": host, "ok": True, "commands": [res.command], "errors": []}
    sections = C.parse_df_sections(res.stdout)
    for sec in sections:
        if sec.get("ok"):
            sec["size_h"] = C.human_gb(sec["size_kb"])
            sec["used_h"] = C.human_gb(sec["used_kb"])
            for key in ("size_kb", "used_kb", "avail_kb"):
                sec.pop(key, None)
        else:
            result["errors"].append(f"{sec['mount']}: {sec.get('error', 'df 失败')}")
    result["disks"] = sections
    result["ok"] = res.ok
    if not res.ok:
        result["errors"].append(f"df 执行失败: {res.error} {res.stderr.strip()}".strip())
    return result


@register_tool(
    "check_pods",
    "巡检指定命名空间（可多选）下所有 Pod 的健康度（需在控制机配置 kubectl）",
    {
        "type": "object",
        "properties": {
            "namespaces": {"type": "array", "items": {"type": "string"}},
        },
        "required": ["namespaces"],
    },
)
def check_pods(args: dict) -> dict:
    namespaces = args.get("namespaces") or []
    result = {"tool": "check_pods", "namespaces": namespaces,
              "available": False, "pods": [], "commands": [], "errors": []}
    for ns in namespaces:
        res = C.run(f"kubectl -n {C.shell_quote(ns)} get pods -o json", "local")
        result["commands"].append(res.command)
        if not res.ok:
            detail = (res.stderr or res.error).strip().splitlines()
            result["errors"].append({"namespace": ns, "error": detail[0] if detail else "kubectl 调用失败"})
            continue
        try:
            pods = C.parse_pods_json(res.stdout)
        except ValueError as exc:
            result["errors"].append({"namespace": ns, "error": f"JSON 解析失败: {exc}"})
            continue
        result["pods"].extend(pods)
        result["available"] = True
    return result


@register_tool(
    "check_middleware",
    "巡检中间件服务健康度，部署方式由 probes.py 注册表驱动（binary / container / k8s，可自行扩展）",
    {
        "type": "object",
        "properties": {
            "host": {"type": "string", "description": "远端执行部署检查的主机"},
            "via": {"type": "string", "enum": ["local", "ssh"]},
            "services": {"type": "array", "items": {"type": "object"}},
        },
        "required": ["host", "services"],
    },
)
def check_middleware(args: dict) -> dict:
    host = args.get("host") or ""
    via = args.get("via", "ssh")
    result = {"tool": "check_middleware", "host": host, "ok": True, "results": [], "commands": [], "errors": []}
    for svc in args.get("services") or []:
        name = svc.get("name") or "?"
        deploy = svc.get("deploy", "binary")
        svc_host = svc.get("host") or host
        entry = {"name": name, "deploy": deploy, "host": svc_host,
                 "port": svc.get("port"), "port_open": None, "alive": None, "detail": ""}

        port_res = C.check_port(svc_host, svc.get("port") or 0)
        entry["port_open"] = port_res.ok
        result["commands"].append(port_res.command)
        if not port_res.ok:
            entry["detail"] = f"端口不可达: {port_res.error}"

        probe_fn = PROBES.get(deploy)
        if probe_fn is None:
            probe = {"alive": None, "detail": f"未注册的部署类型: {deploy}（已注册: {sorted(PROBES)}）",
                     "command": ""}
        else:
            probe = probe_fn(svc, host, via)
        if probe.get("command"):
            result["commands"].append(probe["command"])
        if probe["alive"] is None:
            entry["detail"] = (entry["detail"] + "；" + probe["detail"]).strip("；")
        else:
            entry["alive"] = probe["alive"] and bool(entry["port_open"])
            entry["detail"] = probe["detail"] if entry["port_open"] else entry["detail"] + "；" + probe["detail"]
        result["results"].append(entry)
    return result

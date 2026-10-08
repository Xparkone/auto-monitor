"""采集层：本机/SSH 命令执行、端口探测与输出解析。

设计原则：
- 远端主机走系统 ssh（BatchMode，要求使用者已配置免密登录），不引入 paramiko。
- 所有执行统一返回 ExecResult，带实际命令与耗时，供巡检日志排查。
- 解析函数与执行分离，便于用夹具数据做单元测试。
"""

from __future__ import annotations

import json
import re
import shlex
import socket
import subprocess
import time
from dataclasses import dataclass


@dataclass
class ExecResult:
    ok: bool
    stdout: str = ""
    stderr: str = ""
    duration_ms: int = 0
    error: str = ""
    command: str = ""


def run(shell_cmd: str, via: str = "local", host: str = "", timeout: int = 20) -> ExecResult:
    """在目标机上执行 shell 命令。via=ssh 时经系统 ssh 到 host 执行。"""
    if via == "ssh":
        if not host:
            return ExecResult(ok=False, error="ssh 模式缺少 host", command=shell_cmd)
        argv = ["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5",
                "-o", "StrictHostKeyChecking=accept-new", host, "--", shell_cmd]
        display = f"ssh {host} -- {shell_cmd}"
        use_shell = False
    else:
        argv = shell_cmd
        display = shell_cmd
        use_shell = True

    start = time.monotonic()
    try:
        proc = subprocess.run(argv, capture_output=True, text=True, timeout=timeout, shell=use_shell)
    except subprocess.TimeoutExpired:
        dur = int((time.monotonic() - start) * 1000)
        return ExecResult(ok=False, duration_ms=dur, error=f"执行超时({timeout}s)", command=display)
    except OSError as exc:
        dur = int((time.monotonic() - start) * 1000)
        return ExecResult(ok=False, duration_ms=dur, error=str(exc), command=display)

    dur = int((time.monotonic() - start) * 1000)
    ok = proc.returncode == 0
    return ExecResult(ok=ok, stdout=proc.stdout, stderr=proc.stderr, duration_ms=dur,
                      error="" if ok else f"退出码 {proc.returncode}", command=display)


def check_port(host: str, port: int, timeout: float = 3) -> ExecResult:
    """TCP 端口探测（socket 连接，无需登录目标机）。"""
    display = f"tcp-connect {host}:{port}"
    start = time.monotonic()
    try:
        with socket.create_connection((host, int(port)), timeout=timeout):
            return ExecResult(ok=True, duration_ms=int((time.monotonic() - start) * 1000), command=display)
    except OSError as exc:
        return ExecResult(ok=False, duration_ms=int((time.monotonic() - start) * 1000),
                          error=str(exc), command=display)


# ---------------------------------------------------------------- 输出解析

def parse_load(uptime_out: str):
    """解析 uptime：'load average: 0.01, 0.02, 0.00' 或 'load averages: 1.62 1.70 1.62'。"""
    match = re.search(r"load averages?:\s*([0-9.,\s]+)$", uptime_out.strip())
    if not match:
        return None
    parts = [p for p in re.split(r"[,\s]+", match.group(1).strip()) if p]
    if len(parts) < 3:
        return None
    return {"load1": float(parts[0]), "load5": float(parts[1]), "load15": float(parts[2])}


def parse_cpu_linux(sample_out: str):
    """解析两行 /proc/stat 的 cpu 行，按差值计算使用率。"""
    lines = [l for l in sample_out.splitlines() if l.startswith("cpu ")]
    if len(lines) < 2:
        return None

    def toks(line):
        return [float(x) for x in line.split()[1:]]

    a, b = toks(lines[0]), toks(lines[1])
    idle_a, idle_b = a[3] + a[4], b[3] + b[4]
    total_a, total_b = sum(a), sum(b)
    d_total, d_idle = total_b - total_a, idle_b - idle_a
    if d_total <= 0 or d_idle < 0:
        return None
    return round((1 - d_idle / d_total) * 100, 1)


def parse_cpu_darwin(top_out: str):
    """解析 macOS top 行：'CPU usage: 5.26% user, 10.52% sys, 84.21% idle'。"""
    match = re.search(r"CPU usage:.*?([\d.]+)%\s*idle", top_out, re.S)
    if not match:
        return None
    return round(100 - float(match.group(1)), 1)


def parse_mem_linux(free_out: str):
    """解析 free -m，使用 available 列计算真实使用率。"""
    for line in free_out.splitlines():
        if line.startswith("Mem:"):
            parts = line.split()
            total = float(parts[1])
            available = float(parts[6]) if len(parts) > 6 else float(parts[3])
            used = total - available
            return {"total_mb": total, "used_mb": used, "percent": round(used / total * 100, 1) if total else 0.0}
    return None


def parse_mem_darwin(sysctl_vmstat_out: str):
    """解析 'sysctl -n hw.memsize vm.pagesize && vm_stat' 的组合输出。"""
    values = [l.strip() for l in sysctl_vmstat_out.splitlines() if l.strip()]
    if len(values) < 3:
        return None
    try:
        memsize = int(values[0])
        page_size = int(values[1])
    except ValueError:
        return None

    pages = {}
    for line in values[2:]:
        match = re.match(r"Pages ([a-z ]+):\s+(\d+)\.", line)
        if match:
            pages[match.group(1).strip()] = int(match.group(2))
    freeish = pages.get("free", 0) + pages.get("inactive", 0) + pages.get("speculative", 0)
    used = memsize - freeish * page_size
    if memsize <= 0 or used < 0:
        return None
    return {"total_mb": memsize / 1048576, "used_mb": used / 1048576,
            "percent": round(used / memsize * 100, 1)}


def parse_ps(ps_out: str):
    """解析 'ps -eo pid=,pcpu=,pmem=,state=,comm='：返回 (僵尸进程, CPU 前 5 进程)。"""
    procs, zombies = [], []
    for line in ps_out.splitlines():
        parts = line.split(None, 4)
        if len(parts) < 5:
            continue
        try:
            entry = {"pid": int(parts[0]), "cpu": float(parts[1]),
                     "mem": float(parts[2]), "comm": parts[4].split()[0]}
        except ValueError:
            continue
        procs.append(entry)
        if parts[3].startswith("Z"):
            zombies.append({"pid": entry["pid"], "comm": entry["comm"]})
    top = sorted(procs, key=lambda p: p["cpu"], reverse=True)[:5]
    return zombies, top


def parse_df_sections(df_out: str):
    """解析批量 df 输出：每段以 '== <mount>' 开头，后跟一行 df -k -P 结果。"""
    sections = []
    current = None
    for line in df_out.splitlines():
        if line.startswith("== "):
            current = {"mount": line[3:].strip(), "ok": False}
            sections.append(current)
        elif current is not None and line.strip():
            parts = line.split()
            if len(parts) >= 6 and parts[4].endswith("%"):
                current.update(ok=True, fs=parts[0], size_kb=int(parts[1]), used_kb=int(parts[2]),
                               avail_kb=int(parts[3]), percent=float(parts[4].rstrip("%")),
                               mount_on=parts[5])
            elif not current.get("error"):
                current["error"] = line.strip()
    return sections


def human_gb(kb: float) -> str:
    return f"{kb / 1048576:.1f}G"


def parse_pods_json(text: str):
    """解析 'kubectl get pods -o json' 的输出。"""
    data = json.loads(text)
    pods = []
    for item in data.get("items", []):
        meta = item.get("metadata", {})
        status = item.get("status", {})
        containers = status.get("containerStatuses") or []
        ready = sum(1 for c in containers if c.get("ready"))
        restarts = sum(int(c.get("restartCount", 0)) for c in containers)
        pods.append({
            "name": meta.get("name", ""),
            "namespace": meta.get("namespace", ""),
            "phase": status.get("phase", "Unknown"),
            "ready": f"{ready}/{len(containers)}" if containers else "0/0",
            "ready_ok": bool(containers) and ready == len(containers),
            "restarts": restarts,
        })
    return pods


def parse_deploy_replicas(jsonpath_out: str):
    """解析 kubectl jsonpath '{.status.availableReplicas}/{.spec.replicas}'，如 '2/2'。"""
    match = re.match(r"\s*(\d*)\s*/\s*(\d+)\s*$", jsonpath_out)
    if not match:
        return None
    available = int(match.group(1)) if match.group(1) else 0
    desired = int(match.group(2))
    return {"available": available, "desired": desired, "alive": desired > 0 and available >= desired}


def shell_quote(value: str) -> str:
    return shlex.quote(str(value))


def match_container_lines(lines: list, name: str) -> list:
    """从 'docker ps --format {{.Names}} {{.Status}}' 的输出行中挑出容器名精确等于 name 的行。

    docker 的 --filter name= 是子串匹配（mysql 会命中 mysql-jump），需要二次精确过滤。
    """
    if not name:
        return []
    return [line for line in lines if line.split(None, 1)[0] == name]

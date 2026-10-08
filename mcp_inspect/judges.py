"""阈值判定：把工具结果翻译成 🟢正常 / 🟡注意 / 🔴异常，与 skills/*.md 规则一一对应。"""

from __future__ import annotations

LEVEL_ORDER = {"ok": 0, "warn": 1, "crit": 2}
EMOJI = {"ok": "🟢", "warn": "🟡", "crit": "🔴"}
LABEL = {"ok": "正常", "warn": "注意", "crit": "异常"}
OVERALL_LABEL = {"ok": "优秀", "warn": "注意", "crit": "异常"}


def worst(*levels: str) -> str:
    known = [lv for lv in levels if lv in LEVEL_ORDER]
    return max(known, key=lambda lv: LEVEL_ORDER[lv]) if known else "ok"


def mark(level: str) -> str:
    return f"{EMOJI[level]} {LABEL[level]}"


def judge_percent(value, warn: float, crit: float = None):
    if value is None:
        return None
    if crit is not None and value >= crit:
        return "crit"
    if value >= warn:
        return "warn"
    return "ok"


def judge_resources(res: dict, th) -> dict:
    """check_host_resources 结果 → {level, issues[], 附带各项级别}。"""
    issues = []
    levels = {}
    if not res.get("ok"):
        return {"level": "crit", "issues": ["主机不可达或命令失败（详见巡检日志）"], "levels": levels}

    cpu_lv = judge_percent(res.get("cpu_percent"), th.cpu_warn)
    if cpu_lv:
        levels["cpu"] = cpu_lv
        if cpu_lv != "ok":
            issues.append(f"CPU 使用率 {res['cpu_percent']}% ≥ {th.cpu_warn}%")

    mem_lv = judge_percent(res.get("mem_percent"), th.mem_warn)
    if mem_lv:
        levels["mem"] = mem_lv
        if mem_lv != "ok":
            issues.append(f"内存使用率 {res['mem_percent']}% ≥ {th.mem_warn}%")

    cores = res.get("cores") or 1
    load1 = res.get("load1")
    if load1 is not None:
        load_lv = "warn" if load1 >= cores * th.load_per_core_warn else "ok"
        levels["load"] = load_lv
        if load_lv != "ok":
            issues.append(f"1 分钟负载 {load1} ≥ {cores} 核 × {th.load_per_core_warn}")

    zombies = res.get("zombies") or []
    levels["zombies"] = "warn" if zombies else "ok"
    if zombies:
        issues.append(_zombie_issue(zombies))

    return {"level": worst(*levels.values()), "issues": issues, "levels": levels}


def _zombie_issue(zombies: list) -> str:
    """按命令名归并，通知里不展开每一个 PID。"""
    counts = {}
    for proc in zombies:
        name = proc.get("comm") or "?"
        counts[name] = counts.get(name, 0) + 1
    ranked = sorted(counts.items(), key=lambda item: (-item[1], item[0]))
    shown = [f"{name} ×{n}" if n > 1 else name for name, n in ranked[:3]]
    text = "、".join(shown)
    if len(ranked) > 3:
        text += f" 等 {len(ranked)} 种"
    return f"僵尸进程 {len(zombies)} 个（{text}）"


def judge_disk(section: dict, th) -> dict:
    if not section.get("ok"):
        return {"level": "warn", "issues": [f"磁盘采集失败: {section.get('error', 'df 失败')}"]}
    level = judge_percent(section.get("percent"), th.disk_warn, th.disk_crit)
    if level == "crit":
        issues = [f"{section['mount']} 使用率 {section['percent']}%（异常 ≥ {th.disk_crit}%）"]
    elif level == "warn":
        issues = [f"{section['mount']} 使用率 {section['percent']}%（注意 ≥ {th.disk_warn}%）"]
    else:
        issues = []
    return {"level": level, "issues": issues}


def judge_pod(pod: dict, th) -> dict:
    issues = []
    level = "ok"
    healthy_phase = pod.get("phase") in ("Running", "Succeeded")
    if not healthy_phase or not pod.get("ready_ok"):
        level = "crit"
        issues.append(f"状态 {pod.get('phase')}，Ready {pod.get('ready')}")
    elif pod.get("restarts", 0) >= th.pod_restart_warn:
        level = "warn"
        issues.append(f"重启 {pod['restarts']} 次 ≥ {th.pod_restart_warn}")
    return {"level": level, "issues": issues}


def judge_middleware(entry: dict) -> dict:
    if entry.get("port_open") is False:
        return {"level": "crit", "issues": ["端口未监听"]}
    if entry.get("alive") is False:
        return {"level": "crit", "issues": [f"部署不存活: {entry.get('detail', '')}"]}
    if entry.get("alive") is None and entry.get("detail"):
        # 部署检查执行失败（docker/kubectl 缺失等），只有端口信息
        return {"level": "warn", "issues": [f"部署检查未完成: {entry.get('detail', '')}"]}
    return {"level": "ok", "issues": []}


def summarize(levels, counters: dict) -> dict:
    """汇总整体健康度与计数。levels 为所有条目级别，counters 原地更新。"""
    overall = worst(*levels) if levels else "ok"
    for lv in levels:
        counters[lv] = counters.get(lv, 0) + 1
    return {"level": overall, "emoji": EMOJI[overall], "label": OVERALL_LABEL[overall]}

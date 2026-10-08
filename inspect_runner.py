"""巡检工作流编排：读取 skills 与配置，按节插件展开调用 MCP 工具，产出报告、日志并推送 webhook。

职责划分：skills/*.md 是规则说明书，本文件是“调用者”，
MCP 服务（mcp_inspect/server.py）暴露工具，工具只负责采集，判定与渲染在各节插件
（mcp_inspect/sections/*.py）中完成 —— 本文件不包含任何具体巡检项的知识，
新增巡检项无需修改此文件。

CLI 示例：
  python3.14 inspect_runner.py --config config/inspection.toml \
      --servers srv-1,srv-16 --namespaces default,middleware \
      --services MySQL,Redis --sections server-resources,middleware --no-notify
"""

from __future__ import annotations

import argparse
import datetime as dt
import json
import logging
import ssl
import sys
import time
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
from urllib.request import Request, urlopen

import notifier
from mcp_inspect import judges
from mcp_inspect.config import load_config
from mcp_inspect.sections import InspectContext, all_sections
from mcp_inspect.server import download_url_for
from mcp_inspect.store import save_inspection
from report_renderer import compose

ROOT = Path(__file__).resolve().parent


def client_ssl_context(url: str, *, verify: bool = True, cafile: str = ""):
    """http 地址返回 None。https 时按配置校验证书；自签证书把 verify 设为 false。"""
    if not url.startswith("https://"):
        return None
    if not verify:
        return ssl._create_unverified_context()
    context = ssl.create_default_context()
    if cafile:
        context.load_verify_locations(cafile)
    return context


def mcp_request(url: str, method: str, params: dict | None = None, token: str = "",
                ssl_context=None) -> dict:
    """MCP 客户端最小实现：JSON-RPC over HTTP 或 HTTPS。token 非空时带上 Bearer。"""
    headers = {"Content-Type": "application/json"}
    if token:
        headers["Authorization"] = f"Bearer {token}"
    body = json.dumps({"jsonrpc": "2.0", "id": 1, "method": method, "params": params or {}}).encode()
    request = Request(url, data=body, headers=headers)
    with urlopen(request, timeout=120, context=ssl_context) as response:
        payload = json.load(response)
    if "error" in payload:
        raise RuntimeError(payload["error"]["message"])
    return payload["result"]


def call_tool(log: logging.Logger, url: str, name: str, arguments: dict, token: str = "",
              ssl_context=None) -> dict:
    """调用工具并记录排查日志；失败时返回降级结果而不是中断整场巡检。"""
    start = time.monotonic()
    log.info("tools/call %s args=%s", name, json.dumps(arguments, ensure_ascii=False))
    try:
        result = mcp_request(url, "tools/call", {"name": name, "arguments": arguments}, token,
                             ssl_context=ssl_context)["content"][0]["json"]
    except Exception as exc:  # noqa: BLE001 - 单个工具失败不影响其他巡检项
        dur = int((time.monotonic() - start) * 1000)
        log.error("  <- %s 调用失败 %dms: %s", name, dur, exc)
        return {"tool": name, "ok": False, "commands": [], "errors": [f"工具调用失败: {exc}"]}
    dur = int((time.monotonic() - start) * 1000)
    for command in result.get("commands", []):
        log.info("  $ %s", command)
    log.info("  <- %s %dms errors=%s", name, dur, result.get("errors") or "无")
    return result


def verify_skills(log: logging.Logger, url: str, sections: list, token: str = "",
                  ssl_context=None) -> None:
    """skill-first 校验：各节声明的规则说明书存在且提及所需工具；tools/list 已注册这些工具。"""
    needed = set()
    for sec in sections:
        if not sec.skill_file:
            continue
        skill_path = ROOT / sec.skill_file
        if not skill_path.is_file():
            raise RuntimeError(f"节 {sec.name} 声明的规则说明书不存在: {sec.skill_file}")
        text = skill_path.read_text(encoding="utf-8")
        missing = [t for t in sec.required_tools if t not in text]
        if missing:
            raise RuntimeError(f"{sec.skill_file} 中未声明所需工具: {missing}")
        needed.update(sec.required_tools)
        log.info("skill 校验通过: %s -> %s", sec.skill_file, list(sec.required_tools))
    registered = {tool["name"] for tool in mcp_request(url, "tools/list", token=token,
                                                       ssl_context=ssl_context)["tools"]}
    if not needed <= registered:
        raise RuntimeError(f"skills 要求的工具未在 MCP 服务注册: {sorted(needed - registered)}")
    log.info("tools/list 校验通过: %s", sorted(registered))


def setup_logger(log_dir: Path):
    log_dir.mkdir(parents=True, exist_ok=True)
    stamp = dt.datetime.now().strftime("%Y%m%d-%H%M%S")
    log_file = log_dir / f"inspection-{stamp}.log"
    log = logging.getLogger(f"inspection-{stamp}")
    log.setLevel(logging.INFO)
    handler = logging.FileHandler(log_file, encoding="utf-8")
    handler.setFormatter(logging.Formatter("%(asctime)s %(levelname)s %(message)s"))
    log.addHandler(handler)
    return log, log_file


def _select(items, selector, keys) -> list:
    """keys(item) 返回该条目可用于匹配的标识元组（如名称、IP）。"""
    if not selector:
        return list(items)

    def idents(item):
        value = keys(item)
        return value if isinstance(value, tuple) else (value,)

    selected = [item for item in items if any(i in selector for i in idents(item))]
    known = {i for item in items for i in idents(item)}
    unknown = [s for s in selector if s not in known]
    if unknown:
        raise ValueError(f"选择的目标不在配置中: {unknown}（可选: {sorted(known)}）")
    return selected


def run_inspection(cfg, *, servers_sel=None, namespaces_sel=None, services_sel=None,
                   sections_sel=None, notify_on=True) -> dict:
    cfg.output.report_dir.mkdir(parents=True, exist_ok=True)
    log, log_file = setup_logger(cfg.output.log_dir)
    now = dt.datetime.now()
    log.info("==== 巡检开始 %s ====", now.isoformat(timespec="seconds"))

    servers = _select(cfg.servers, servers_sel, lambda s: (s.name, s.host))
    unknown_ns = [n for n in namespaces_sel or [] if n not in cfg.namespaces]
    if unknown_ns:
        raise ValueError(f"选择的命名空间不在配置中: {unknown_ns}（可选: {cfg.namespaces}）")
    namespaces = [n for n in cfg.namespaces if not namespaces_sel or n in namespaces_sel]
    services = _select(cfg.services, services_sel, lambda s: (s.name,))

    sections = all_sections()
    unknown_sec = [s for s in sections_sel or [] if s not in {x.name for x in sections}]
    if unknown_sec:
        raise ValueError(f"选择的节未注册: {unknown_sec}（已注册: {[x.name for x in sections]}）")
    if sections_sel:
        sections = [s for s in sections if s.name in sections_sel]
    log.info("巡检目标: 服务器=%s 命名空间=%s 服务=%s 节=%s",
             [s.name for s in servers], namespaces, [s.name for s in services],
             [s.name for s in sections])

    ssl_context = client_ssl_context(cfg.server_url, verify=cfg.tls_verify, cafile=cfg.certfile)
    verify_skills(log, cfg.server_url, sections, cfg.token, ssl_context)

    # 各节插件展开 tools/call 清单，统一并发执行
    ctx = InspectContext(cfg=cfg, servers=servers, namespaces=namespaces,
                         services=services, log=log)
    calls = []
    for sec in sections:
        sec_calls = sec.build_calls(ctx)
        log.info("节 %s 展开 %d 个工具调用", sec.name, len(sec_calls))
        calls.extend(sec_calls)
    with ThreadPoolExecutor(max_workers=8) as pool:
        futures = {c.key: pool.submit(call_tool, log, cfg.server_url, c.tool, c.arguments,
                                      cfg.token, ssl_context)
                   for c in calls}
        results = {key: f.result() for key, f in futures.items()}

    # 各节插件自行判定 + 渲染
    outputs = [out for sec in sections if (out := sec.process(ctx, results)) is not None]

    item_levels = [lv for out in outputs for lv in out.levels]
    counters = {"ok": 0, "warn": 0, "crit": 0}
    overall = judges.summarize(item_levels, counters)
    note = ("（无异常项）" if overall["level"] == "ok"
            else f"（{counters.get('crit', 0)} 项异常 / {counters.get('warn', 0)} 项注意）")
    bullets = [out.ok_text if not out.issues else f"⚠️ {'；'.join(out.issues)}" for out in outputs]
    section_stats = []
    for out in outputs:
        stat = {"ok": 0, "warn": 0, "crit": 0}
        for level in out.levels:
            if level in stat:
                stat[level] += 1
        stat["total"] = stat["ok"] + stat["warn"] + stat["crit"]
        section_stats.append({"name": out.name, **stat})

    title = f"{now.month}.{now.day} 巡检报告"
    try:
        log_display = str(log_file.relative_to(cfg.root))
    except ValueError:
        log_display = str(log_file)
    scope_lines = ["服务器：" + ("、".join(f"{s.host}（{s.name}）" for s in servers) or "—")]
    scope_lines += [out.scope_text for out in outputs if out.scope_text]
    markdown = compose(title=title, scope_lines=scope_lines,
                       overall={"emoji": overall["emoji"], "label": overall["label"], "note": note},
                       bullets=bullets, sections_md=[out.markdown for out in outputs],
                       log_file=log_display)

    report_name = f"{now.month}.{now.day}巡检报告.md"
    target = cfg.output.report_dir / report_name
    if target.exists():
        report_name = f"{now.month}.{now.day}巡检报告_{now.strftime('%H%M%S')}.md"
        target = cfg.output.report_dir / report_name
    target.write_text(markdown, encoding="utf-8")
    log.info("报告已生成: %s", target)

    summary = {
        "title": title,
        "report_name": report_name,
        "report_path": str(target),
        "download_url": download_url_for(cfg.output.base_url, report_name, cfg.token),
        "log_path": str(log_file),
        "log_rel": log_display,
        "checked_at": now.strftime("%Y-%m-%d %H:%M:%S"),
        "overall": overall["label"],
        "overall_emoji": overall["emoji"],
        "note": note,
        "counts": counters,
        "bullets": bullets,
        "details": [{"name": out.name, "ok_text": out.ok_text, "issues": list(out.issues)}
                    for out in outputs],
        "sections": [out.name for out in outputs],
        "section_stats": section_stats,
    }

    summary["database"] = {"saved": False}
    if cfg.database.url:
        try:
            rec_id = save_inspection(cfg.database.url, cfg.root, summary, outputs, results)
            summary["database"] = {"saved": True, "id": rec_id}
            log.info("巡检已入库 id=%s", rec_id)
        except Exception as exc:  # noqa: BLE001 - 入库失败不影响已经生成的报告
            log.error("巡检入库失败: %s", exc)
            summary["database"] = {"saved": False, "error": str(exc)}

    summary["webhooks"] = []
    if notify_on:
        outcomes = notifier.dispatch(cfg.webhook.channels, summary)
        for url, ok, err in outcomes:
            log.info("webhook 推送 %s: %s %s", "成功" if ok else "失败", url, err)
            summary["webhooks"].append({"url": url, "ok": ok, "error": err})

    log.info("==== 巡检结束: %s %s 报告=%s ====", overall["label"], counters, report_name)
    return summary


def main():
    parser = argparse.ArgumentParser(description="自动巡检工作流")
    parser.add_argument("--config", default=str(ROOT / "config" / "inspection.toml"))
    parser.add_argument("--servers", help="服务器多选（名称或 IP，逗号分隔），不传为全量")
    parser.add_argument("--namespaces", help="命名空间多选（逗号分隔），不传为全量")
    parser.add_argument("--services", help="中间件服务多选（名称，逗号分隔），不传为全量")
    parser.add_argument("--sections", help="巡检节多选（如 server-resources,pods），不传为全部已注册节")
    parser.add_argument("--no-notify", action="store_true", help="不推送 webhook")
    parser.add_argument("--json", action="store_true", help="仅输出 JSON 摘要（供 /inspect 接口使用）")
    args = parser.parse_args()

    def split(value):
        return [x for x in (value or "").split(",") if x] or None

    try:
        cfg = load_config(args.config)
        summary = run_inspection(
            cfg,
            servers_sel=split(args.servers),
            namespaces_sel=split(args.namespaces),
            services_sel=split(args.services),
            sections_sel=split(args.sections),
            notify_on=not args.no_notify,
        )
    except Exception as exc:  # noqa: BLE001 - CLI 出口统一报错
        print(f"巡检失败: {exc}", file=sys.stderr)
        sys.exit(1)

    if args.json:
        print(json.dumps(summary, ensure_ascii=False))
    else:
        print(f"{summary['overall_emoji']} {summary['title']}：{summary['overall']} {summary['counts']}")
        for bullet in summary["bullets"]:
            print(f"  - {bullet}")
        print(f"报告: {summary['report_path']}")
        print(f"下载: {summary['download_url']}")
        print(f"日志: {summary['log_path']}")
        db = summary.get("database") or {}
        if db.get("saved"):
            print(f"入库: {db['id']}")
        elif db.get("error"):
            print(f"入库失败: {db['error']}")
        for hook in summary["webhooks"]:
            print(f"webhook {'成功' if hook['ok'] else '失败'}: {hook['url']} {hook['error']}")


if __name__ == "__main__":
    main()

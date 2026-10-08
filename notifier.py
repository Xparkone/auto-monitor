"""Webhook 通知：渠道注册表驱动，内置飞书消息卡片、企业微信 markdown 与通用 JSON。

扩展新渠道（如钉钉）：
1. 用 @register_channel("dingtalk") 注册一个 summary -> payload 构造函数；
2. 在 config/inspection.toml 的 [webhook.channels] 配同名 key = "url"。

通知失败只记录不影响巡检结果；所有异常收敛为返回值。
"""

from __future__ import annotations

import json
from urllib.request import Request, urlopen

CHANNELS: dict = {}

_TEMPLATE = {"🟢": "green", "🟡": "orange", "🔴": "red"}
_TAG_COLOR = {"🟢": "green", "🟡": "orange", "🔴": "red"}
# 企业微信 markdown 只支持 info / comment / warning 三种颜色
_WECOM_COLOR = {"🟢": "info", "🟡": "warning", "🔴": "warning"}
_SECTION_LABEL = {
    "server-resources": "服务器资源",
    "disks": "磁盘",
    "pods": "K8s Pod",
    "middleware": "中间件",
}
# 这两节用和总览相同的三格数字，不用柱状图。数量为 0 时用灰色，避免把「没有异常」画成红色。
_COUNT_SECTIONS = ("pods", "middleware")


def register_channel(name: str):
    def decorator(func):
        CHANNELS[name] = func
        return func

    return decorator


def _metric_column(value, caption: str, color: str) -> dict:
    """飞书卡片里的一块灰色数字。数字用颜色，说明用灰色小字。"""
    return {
        "tag": "column",
        "width": "weighted",
        "weight": 1,
        "vertical_align": "center",
        "elements": [{
            "tag": "markdown",
            "text_align": "center",
            "content": f"<font color='{color}'>**{value}**</font>\n<font color='grey'>{caption}</font>",
        }],
    }


def _count_color(value, color: str) -> str:
    return "grey" if int(value or 0) == 0 else color


def _metric_row(stats: dict) -> dict:
    ok, warn, crit = stats.get("ok", 0), stats.get("warn", 0), stats.get("crit", 0)
    return {
        "tag": "column_set",
        "flex_mode": "trisect",
        "horizontal_spacing": "small",
        "columns": [
            _metric_column(ok, "正常", _count_color(ok, "green")),
            _metric_column(warn, "注意", _count_color(warn, "orange")),
            _metric_column(crit, "异常", _count_color(crit, "red")),
        ],
    }


def _section_count_elements(summary: dict) -> list:
    by_name = {item.get("name"): item for item in summary.get("section_stats") or []}
    elements = []
    for name in _COUNT_SECTIONS:
        stats = by_name.get(name)
        if not stats or not stats.get("total"):
            continue
        title = _SECTION_LABEL.get(name, name)
        elements.append({"tag": "markdown", "content": f"**{title}**"})
        elements.append(_metric_row(stats))
    return elements


def _conclusion_md(summary: dict) -> str:
    """每条问题单独一行。有明细时按主机/挂载点拆开，不再用分号接成一句。"""
    details = summary.get("details") or []
    lines = ["**巡检结论**", ""]
    if not details:
        bullets = summary.get("bullets") or []
        if not bullets:
            lines.append("本次没有产出巡检节。")
        for bullet in bullets:
            lines.append(f"- {str(bullet).removeprefix('⚠️ ').strip()}")
        return "\n".join(lines).rstrip()
    for item in details:
        heading = _SECTION_LABEL.get(item.get("name"), item.get("name") or "")
        if heading:
            lines.append(f"**{heading}**")
        issues = [str(text).strip() for text in item.get("issues") or [] if str(text).strip()]
        if not issues:
            lines.append(f"- {item.get('ok_text') or '正常'}")
        else:
            shown = issues[:6]
            for issue in shown:
                lines.append(f"- {issue}")
            rest = len(issues) - len(shown)
            if rest:
                lines.append(f"- 其余 {rest} 条见巡检报告")
        lines.append("")
    return "\n".join(lines).rstrip()


def build_feishu_card(summary: dict) -> dict:
    """飞书消息卡片：标题 + 状态标签、三项计数、分节结论、下载按钮。"""
    emoji = summary.get("overall_emoji", "🟢")
    label = summary.get("overall") or "未知"
    title = summary.get("title") or "巡检报告"
    tag_color = _TAG_COLOR.get(emoji, "blue")
    counts = summary.get("counts") or {}
    checked_at = summary.get("checked_at") or ""
    title_line = f"<font color='blue'>**{title}**</font> <text_tag color='{tag_color}'>{label}</text_tag>"
    headline = title_line if not checked_at else f"{title_line}\n<font color='grey'>{checked_at}</font>"

    elements = [
        {"tag": "div", "text": {"tag": "lark_md", "content": headline}},
        {
            "tag": "column_set",
            "flex_mode": "trisect",
            "background_style": "grey",
            "horizontal_spacing": "small",
            "columns": [
                _metric_column(counts.get("ok", 0), "正常", "green"),
                _metric_column(counts.get("warn", 0), "注意", "orange"),
                _metric_column(counts.get("crit", 0), "异常", "red"),
            ],
        },
        {"tag": "hr"},
        *_section_count_elements(summary),
        {"tag": "markdown", "content": _conclusion_md(summary)},
    ]
    log_rel = summary.get("log_rel") or summary.get("log_path") or ""
    if log_rel:
        elements.append({
            "tag": "note",
            "elements": [{"tag": "plain_text", "content": f"日志 {log_rel}"}],
        })
    download = summary.get("download_url") or ""
    if download:
        elements.append({
            "tag": "action",
            "actions": [{
                "tag": "button",
                "text": {"tag": "plain_text", "content": "下载巡检报告"},
                "url": download,
                "type": "primary",
            }],
        })
    elements.append({"tag": "markdown", "content": title_line})
    return {
        "msg_type": "interactive",
        "card": {
            "config": {"wide_screen_mode": True},
            "header": {
                "template": _TEMPLATE.get(emoji, "blue"),
                "title": {"tag": "plain_text", "content": f"{emoji} {title}"},
            },
            "elements": elements,
        },
    }


@register_channel("feishu")
def _feishu_payload(summary: dict) -> dict:
    return build_feishu_card(summary)


@register_channel("wecom")
def _wecom_payload(summary: dict) -> dict:
    """企业微信群机器人：markdown。没有卡片按钮，下载地址写成链接。"""
    color = _WECOM_COLOR.get(summary.get("overall_emoji", ""), "comment")
    emoji = summary.get("overall_emoji", "")
    title = summary.get("title", "巡检报告")
    label = summary.get("overall", "")
    lines = [
        f"## <font color=\"{color}\">{emoji} {title}：{label}</font>",
        summary.get("note", ""),
        "",
        _conclusion_md(summary),
    ]
    download = summary.get("download_url", "")
    if download:
        lines += ["", f"[下载巡检报告]({download})"]
    return {"msgtype": "markdown", "markdown": {"content": "\n".join(lines)}}


@register_channel("generic")
def _generic_payload(summary: dict) -> dict:
    return summary


def post_json(url: str, payload: dict, timeout: float = 5) -> int:
    body = json.dumps(payload, ensure_ascii=False).encode()
    request = Request(url, data=body, headers={"Content-Type": "application/json"})
    with urlopen(request, timeout=timeout) as response:
        return response.status


def dispatch(channels: dict, summary: dict) -> list:
    """按配置逐渠道推送。channels: {渠道名: url 或 [url...]}，返回 [(url, ok, error)]。"""
    outcomes = []
    for name, urls in (channels or {}).items():
        builder = CHANNELS.get(name)
        if builder is None:
            outcomes.append((name, False, f"未注册的渠道类型: {name}（已注册: {sorted(CHANNELS)}）"))
            continue
        payload = builder(summary)
        if isinstance(urls, str):
            urls = [urls]
        for url in urls:
            try:
                post_json(url, payload)
                outcomes.append((url, True, ""))
            except Exception as exc:  # noqa: BLE001 - 通知失败不影响巡检主流程
                outcomes.append((url, False, str(exc)))
    return outcomes

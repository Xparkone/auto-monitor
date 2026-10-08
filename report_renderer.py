"""Markdown 巡检报告组装器：报告骨架（标题 / 巡检范围 / 总体结论 / 各节 / 日志位置）。

各节内容由 sections/*.py 插件自渲染并拼接，扩展新巡检项无需修改本文件。
"""

from __future__ import annotations


def compose(*, title: str, scope_lines: list, overall: dict, bullets: list,
            sections_md: list, log_file: str) -> str:
    lines = []
    add = lines.append

    add(f"# {title}")
    add("")
    add("巡检范围：")
    add("")
    for line in scope_lines:
        add(f"- {line}")
    add("")
    add("---")
    add("")

    add("## 总体结论")
    add("")
    add(f"- 整体健康度：{overall['emoji']} {overall['label']}{overall['note']}")
    add("")
    for bullet in bullets:
        add(f"- {bullet}")
    add("")
    add("---")
    add("")

    for md in sections_md:
        add(md.rstrip("\n"))
        add("")

    add("---")
    add("")
    add("# 日志文件位置")
    add("")
    add(f"`{log_file}`")
    add("")
    return "\n".join(lines)

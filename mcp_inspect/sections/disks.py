"""磁盘巡检节：各服务器指定挂载点（根目录 & 关键数据盘）的使用率。"""

from __future__ import annotations

from .. import judges
from .base import (InspectContext, Section, SectionOutput, ToolCall,
                   fmt, judge_cell, register_section)


@register_section
class DisksSection(Section):
    name = "disks"
    skill_file = "skills/server-resources.md"  # 与资源节共用同一份规则说明书
    required_tools = ("check_disk",)

    def build_calls(self, ctx: InspectContext) -> list:
        return [ToolCall(key=f"disk:{s.name}", tool="check_disk",
                         arguments={"host": s.host, "via": s.via, "mounts": s.mounts})
                for s in ctx.servers]

    def process(self, ctx: InspectContext, results: dict):
        rows, levels, issues = [], [], []
        for s in ctx.servers:
            res = results.get(f"disk:{s.name}") or {"ok": False}
            sections = res.get("disks") or []
            if not sections:
                levels.append("warn")
                issues.append(f"{s.name} 磁盘巡检执行失败")
                rows.append({"server": s.name, "mount": "—", "ok": False, "level": "warn",
                             "issues": ["磁盘巡检执行失败（详见巡检日志）"]})
                continue
            for sec in sections:
                j = judges.judge_disk(sec, ctx.th)
                levels.append(j["level"])
                issues += [f"{s.name} {i}" for i in j["issues"]]
                rows.append({"server": s.name, "mount": sec["mount"], "ok": sec.get("ok", False),
                             "size_h": sec.get("size_h", "—"), "used_h": sec.get("used_h", "—"),
                             "percent": sec.get("percent"), "level": j["level"], "issues": j["issues"]})

        mounts = sorted({m for s in ctx.servers for m in s.mounts})
        lines = ["## 磁盘使用状态（根目录 & 关键数据盘）", "",
                 f"巡检时间：{ctx.stamp()}", "",
                 "| 服务器 | 挂载点 | 总量 | 已用 | 使用率 | 判定 |",
                 "| --- | --- | --- | --- | --- | --- |"]
        for row in rows:
            if row["ok"]:
                lines.append(f"| {row['server']} | {row['mount']} | {row['size_h']} | {row['used_h']} "
                             f"| {fmt(row.get('percent'), '%')} | {judges.mark(row['level'])} |")
            else:
                lines.append(f"| {row['server']} | {row['mount']} | — | — | — "
                             f"| {judge_cell(row['level'], row['issues'])} |")
        lines.append("")
        return SectionOutput(
            name=self.name, markdown="\n".join(lines), levels=levels, issues=issues,
            scope_text=f"磁盘挂载点：{'、'.join(mounts)}",
            ok_text="磁盘状态良好：各挂载点使用率均低于注意阈值。",
        )

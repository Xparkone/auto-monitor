"""服务器资源巡检节：CPU / 内存 / 负载 / 进程（含僵尸进程）。"""

from __future__ import annotations

from .. import judges
from .base import (InspectContext, Section, SectionOutput, ToolCall,
                   fmt, judge_cell, register_section)


@register_section
class ResourcesSection(Section):
    name = "server-resources"
    skill_file = "skills/server-resources.md"
    required_tools = ("check_host_resources",)

    def build_calls(self, ctx: InspectContext) -> list:
        return [ToolCall(key=f"resources:{s.name}", tool="check_host_resources",
                         arguments={"host": s.host, "via": s.via}) for s in ctx.servers]

    def process(self, ctx: InspectContext, results: dict):
        rows, levels, issues = [], [], []
        for s in ctx.servers:
            res = results.get(f"resources:{s.name}") or {"ok": False, "errors": ["未执行"]}
            j = judges.judge_resources(res, ctx.th)
            levels.append(j["level"])
            issues += [f"{s.name} {i}" for i in j["issues"]]
            rows.append((s, res, j))

        lines = ["## 服务器资源状态（系统资源巡检）", "",
                 f"巡检时间：{ctx.stamp()}", ""]
        if issues:
            lines += [f"> 结论：{'；'.join(issues)}", ""]
        else:
            lines += ["> 结论：所有服务器资源状态正常，无僵尸进程。", ""]
        lines += ["| 服务器 | CPU | 内存 | 负载(1/5/15) | 僵尸进程 | 判定 |",
                  "| --- | --- | --- | --- | --- | --- |"]
        for s, res, j in rows:
            if not res.get("ok"):
                lines.append(f"| {s.name}（{s.host}） | — | — | — | — | {judge_cell(j['level'], j['issues'])} |")
                continue
            load_text = f"{fmt(res.get('load1'))} / {fmt(res.get('load5'))} / {fmt(res.get('load15'))}"
            zombies = res.get("zombies") or []
            zombie_text = "无" if not zombies else f"⚠️ {len(zombies)} 个"
            lines.append(f"| {s.name}（{s.host}） | {fmt(res.get('cpu_percent'), '%')} "
                         f"| {fmt(res.get('mem_percent'), '%')} | {load_text} | {zombie_text} "
                         f"| {judge_cell(j['level'], j['issues'])} |")
        lines.append("")
        return SectionOutput(
            name=self.name, markdown="\n".join(lines), levels=levels, issues=issues,
            scope_text="资源：CPU、内存、负载、进程",
            ok_text="资源状态良好：所有服务器 CPU/内存/负载均正常，无僵尸进程。",
        )

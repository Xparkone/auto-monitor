"""中间件健康巡检节：部署方式由 probes.py 注册表驱动，按执行主机分组。"""

from __future__ import annotations

import dataclasses

from .. import judges
from ..probes import PROBES
from .base import (InspectContext, Section, SectionOutput, ToolCall,
                   judge_cell, register_section)

DEPLOY_LABEL = {"binary": "二进制", "container": "容器", "k8s": "K8s"}


@register_section
class MiddlewareSection(Section):
    name = "middleware"
    skill_file = "skills/middleware.md"
    required_tools = ("check_middleware",)

    def _via_for(self, ctx: InspectContext, host: str) -> str:
        return next((s.via for s in ctx.cfg.servers if s.host == host), "ssh")

    def build_calls(self, ctx: InspectContext) -> list:
        groups = {}
        for svc in ctx.services:
            groups.setdefault(svc.host, []).append(svc)
        return [ToolCall(key=f"middleware:{host}", tool="check_middleware",
                         arguments={"host": host, "via": self._via_for(ctx, host),
                                    "services": [dataclasses.asdict(svc) for svc in svcs]})
                for host, svcs in groups.items()]

    def process(self, ctx: InspectContext, results: dict):
        if not ctx.services:
            return None
        groups, levels, issues = [], [], []
        for key in sorted(k for k in results if k.startswith("middleware:")):
            host = key[len("middleware:"):]
            rows = []
            for entry in (results[key] or {}).get("results", []):
                j = judges.judge_middleware(entry)
                levels.append(j["level"])
                issues += [f"{entry['name']}: {i}" for i in j["issues"]]
                rows.append((entry, j))
            groups.append((host, rows))

        deploy_types = " / ".join(sorted(PROBES))
        lines = [f"## 中间件服务健康状态（{deploy_types}）", "",
                 f"巡检时间：{ctx.stamp()}", ""]
        for i, (host, rows) in enumerate(groups, 1):
            lines += [f"### 3.{i} {host}", "",
                      "| 服务 | 部署方式 | 端口 | 状态明细 | 判定 |",
                      "| --- | --- | --- | --- | --- |"]
            for entry, j in rows:
                port = f"{entry.get('port')} {'✅' if entry.get('port_open') else '❌'}"
                label = DEPLOY_LABEL.get(entry.get("deploy"), entry.get("deploy"))
                lines.append(f"| {entry['name']} | {label} | {port} | {entry.get('detail') or '—'} "
                             f"| {judge_cell(j['level'], j['issues'])} |")
            lines.append("")
        return SectionOutput(
            name=self.name, markdown="\n".join(lines), levels=levels, issues=issues,
            scope_text=f"服务：{'、'.join(s.name for s in ctx.services)}",
            ok_text=f"中间件服务健康：{len(ctx.services)} 个服务全部正常。",
        )

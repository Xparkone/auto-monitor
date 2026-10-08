"""K8s Pod 健康巡检节：命名空间可多选；kubectl 不可用时降级为说明。"""

from __future__ import annotations

from .. import judges
from .base import (InspectContext, Section, SectionOutput, ToolCall,
                   judge_cell, register_section)


@register_section
class PodsSection(Section):
    name = "pods"
    skill_file = "skills/k8s-pods.md"
    required_tools = ("check_pods",)

    def build_calls(self, ctx: InspectContext) -> list:
        if not ctx.namespaces:
            return []
        return [ToolCall(key="pods", tool="check_pods",
                         arguments={"namespaces": list(ctx.namespaces)})]

    def process(self, ctx: InspectContext, results: dict):
        if not ctx.namespaces:
            return None  # 未配置命名空间：报告与结论都跳过本节
        res = results.get("pods") or {"available": False, "errors": [{"error": "未执行"}]}
        header = ["## K8s Pod 健康状态", "",
                  f"巡检时间：{ctx.stamp()}", "",
                  f"命名空间：{'、'.join(ctx.namespaces)}", ""]

        if not res.get("available"):
            reason = "；".join(e.get("error", "") for e in res.get("errors") or []) or "kubectl 不可用"
            header.append(f"> ⚠️ K8s 巡检不可用：{reason}（详见巡检日志）")
            header.append("")
            return SectionOutput(
                name=self.name, markdown="\n".join(header), levels=["warn"],
                issues=[f"K8s 巡检不可用：{reason}"],
                scope_text=f"命名空间：{'、'.join(ctx.namespaces)}",
            )

        rows, levels, issues = [], [], []
        for pod in res.get("pods", []):
            j = judges.judge_pod(pod, ctx.th)
            levels.append(j["level"])
            issues += [f"{pod['namespace']}/{pod['name']}: {i}" for i in j["issues"]]
            rows.append((pod, j))

        header += ["| 命名空间 | Pod | 状态 | Ready | 重启 | 判定 |",
                   "| --- | --- | --- | --- | --- | --- |"]
        for pod, j in rows:
            header.append(f"| {pod['namespace']} | {pod['name']} | {pod['phase']} | {pod['ready']} "
                          f"| {pod['restarts']} | {judge_cell(j['level'], j['issues'])} |")
        header.append("")
        return SectionOutput(
            name=self.name, markdown="\n".join(header), levels=levels, issues=issues,
            scope_text=f"命名空间：{'、'.join(ctx.namespaces)}",
            ok_text=f"K8s 集群稳定：{len(rows)} 个 Pod 状态正常。",
        )

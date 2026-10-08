"""巡检节（Section）插件协议与注册表 —— 新增巡检项的核心扩展点。

新增一个巡检项共三步：
1. skills/ 下写规则说明书（文中提及将使用的工具名）；
2. 用 @register_tool（见 mcp_inspect/tools.py）注册采集工具；
3. 新建 sections/xxx.py，实现 Section 协议并 @register_section —— sections 包会自动发现加载。

工作流（inspect_runner.py）、报告组装（report_renderer.py）、HTTP 接口均无需改动。
"""

from __future__ import annotations

import datetime as dt
from dataclasses import dataclass, field

from .. import judges


@dataclass
class ToolCall:
    """一次 tools/call 的展开结果：key 是结果字典的键。"""
    key: str
    tool: str
    arguments: dict


@dataclass
class SectionOutput:
    """单节判定 + 渲染产物。"""
    name: str
    markdown: str                                    # 本节的 Markdown（含标题）
    levels: list                                     # 各检查项级别（ok/warn/crit），汇总整体健康度
    issues: list = field(default_factory=list)       # ["对象: 问题"]，异常时用于总体结论 bullet
    scope_text: str = ""                             # 巡检范围行，如 "资源：CPU、内存、负载、进程"
    ok_text: str = ""                                # 全部正常时的结论句


@dataclass
class InspectContext:
    """传给插件的上下文：已按 CLI/接口参数过滤好的目标集合。"""
    cfg: object
    servers: list
    namespaces: list
    services: list
    log: object

    @property
    def th(self):
        return self.cfg.thresholds

    def stamp(self) -> str:
        return dt.datetime.now().strftime("%Y-%m-%d %H:%M:%S")


class Section:
    """插件协议：build_calls 展开采集调用，process 判定并产出 Markdown。"""
    name = "?"
    skill_file = ""              # skills/xxx.md；为空则跳过 skill 校验
    required_tools: tuple = ()   # skill 说明书与 MCP tools/list 都需覆盖这些工具

    def build_calls(self, ctx: InspectContext) -> list:
        raise NotImplementedError

    def process(self, ctx: InspectContext, results: dict):
        """返回 SectionOutput；返回 None 表示本节无对象（报告与结论都跳过）。"""
        raise NotImplementedError


SECTIONS: dict = {}
# 报告中的节顺序；新增节自动追加在内置节之后
ORDER = ["server-resources", "disks", "pods", "middleware"]


def register_section(cls):
    SECTIONS[cls.name] = cls()
    return cls


def all_sections() -> list:
    known = [SECTIONS[n] for n in ORDER if n in SECTIONS]
    extra = [s for n, s in SECTIONS.items() if n not in ORDER]
    return known + extra


# -------------------------------------------------- 各节共用的 Markdown 小工具

def fmt(value, suffix=""):
    return "—" if value is None else f"{value}{suffix}"


def judge_cell(level: str, issues: list) -> str:
    if level == "ok" or not issues:
        return judges.mark(level)
    return f"{judges.mark(level)}<br>" + "<br>".join(issues)

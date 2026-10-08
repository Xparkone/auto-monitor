"""巡检节插件包：目录下的模块会被自动发现并注册。

把新的 sections/xxx.py 放进本目录即完成注册，无需修改任何导入。
"""

import importlib
import pkgutil

from .base import (SECTIONS, InspectContext, Section, SectionOutput, ToolCall,
                   all_sections, fmt, judge_cell, register_section)

__all__ = ["SECTIONS", "InspectContext", "Section", "SectionOutput", "ToolCall",
           "all_sections", "fmt", "judge_cell", "register_section"]


def _auto_import():
    for info in pkgutil.iter_modules(__path__):
        if info.name != "base" and not info.name.startswith("_"):
            importlib.import_module(f".{info.name}", __name__)


_auto_import()

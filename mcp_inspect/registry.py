"""工具注册表：新增巡检工具只需一个 @register_tool 装饰器。

tools/list、tools/call 都由注册表驱动，新增工具后服务端无需任何改动。
"""

from __future__ import annotations

TOOLS: dict = {}


def register_tool(name: str, description: str, input_schema: dict):
    """把函数注册为 MCP 工具。被装饰函数接收 arguments dict，返回结构化 JSON。"""

    def decorator(func):
        TOOLS[name] = {
            "name": name,
            "description": description,
            "inputSchema": input_schema,
            "handler": func,
        }
        return func

    return decorator


def tool_specs() -> list:
    return [{k: v for k, v in tool.items() if k != "handler"} for tool in TOOLS.values()]


def call_tool(name: str, arguments: dict) -> dict:
    if name not in TOOLS:
        raise ValueError(f"unknown tool: {name}")
    return TOOLS[name]["handler"](arguments or {})

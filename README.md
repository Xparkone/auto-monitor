# 自动巡检平台

基于「Skill（规则说明书）→ 工作流（调用者）→ MCP 服务（工具宿主）→ Tool（采集执行）」职责划分的自动巡检平台：覆盖**多台服务器、K8s 命名空间、多形态中间件**，产出 Markdown 报告、巡检日志，支持 HTTP 接口下载与 webhook/飞书通知。

> 需要 Python ≥ 3.11（依赖标准库 `tomllib`），本机验证使用 `python3.14`。SQLite 入库使用标准库；PostgreSQL / MySQL 需另装对应驱动。

## 架构

```text
skills/*.md           三类巡检的规则说明书（阈值、判定级别）
    ↓ 工作流读取并校验
inspect_runner.py     CLI 工作流：选目标 → tools/list 校验 → tools/call → 判定 → 渲染 → 通知
    ↓ HTTP/JSON-RPC
mcp_inspect/server.py 暴露 tools/list、tools/call + 报告接口（/inspect、/reports、/healthz）
    ↓
check_host_resources / check_disk / check_pods / check_middleware  真正执行的工具
    ↓ 采集层（collectors.py）
本机 subprocess · 远端系统 ssh · 端口 socket 探测 · kubectl/docker CLI
```

阈值判断在工作流侧（`mcp_inspect/judges.py`），与 `skills/*.md` 的规则一一对应；工具只负责采集。

## 巡检能力

| 维度 | 说明 |
| --- | --- |
| 服务器资源（可多选） | CPU、内存、负载、进程（含僵尸进程）、指定挂载点磁盘；`via=local` 本机直采，`via=ssh` 经系统 ssh 免密采集 |
| K8s Pod（命名空间可多选） | kubectl 采集 Pod 状态/Ready/重启次数；不可用时节区降级为说明，不中断整体巡检 |
| 中间件健康 | 三种部署方式：`binary`（systemd → 进程 ps/grep）、`container`（docker ps）、`k8s`（Deployment 副本数）；任何部署方式都先做 socket 端口探测 |
| 报告 | Markdown 输出到可配置目录（默认 `reports/`），结构对齐 9.18 巡检报告 |
| 日志 | 每次巡检独立日志（默认 `logs/`），记录每个工具调用、实际命令、耗时、错误 |
| 接口 | `/reports` 列表与下载，`/inspect` 触发巡检，`/healthz` 存活探针 |
| 通知 | 飞书自定义机器人消息卡片（健康度颜色 + 要点 + 下载按钮）、企业微信群机器人 markdown、通用 webhook POST JSON |

## 快速开始

终端一，启动 MCP 服务：

```bash
python3.14 -m mcp_inspect.server            # 默认 127.0.0.1:8765，--host/--port/--config 可改
```

终端二，执行巡检：

```bash
python3.14 inspect_runner.py                                   # 全量巡检并推送 webhook
python3.14 inspect_runner.py --servers srv-1,srv-16 \          # 服务器多选（名称或 IP）
    --namespaces default,middleware \                          # 命名空间多选
    --services MySQL,Redis --no-notify                         # 服务多选；关闭推送
```

定时自动化交给系统 cron（示例：每日 02:00）：

```cron
0 2 * * * cd /path/to/mcp-skill-demo && /usr/bin/env python3.14 inspect_runner.py >> logs/cron.log 2>&1
```

## 配置 `config/inspection.toml`

```toml
[server]
url = "http://127.0.0.1:8765"      # 工作流调用的 MCP 服务地址
[output]
report_dir = "reports"
log_dir = "logs"
base_url = "http://127.0.0.1:8765" # 拼报告下载链接（webhook 用）
[thresholds]
cpu_warn = 80; mem_warn = 80; disk_warn = 75; disk_crit = 90
load_per_core_warn = 2; pod_restart_warn = 5

[[servers]]
name = "srv-16"; host = "192.168.122.16"; via = "ssh"
mounts = ["/", "/data"]

[[namespaces]]
name = "default"

[[services]]
name = "MySQL"; host = "192.168.122.16"; deploy = "container"
port = 3306; container = "mysql"
# deploy = "binary" 时用 process = "mysqld"
# deploy = "k8s" 时用 k8s = { namespace = "middleware", deployment = "mysql" }

[webhook.channels]
feishu = "https://open.feishu.cn/open-apis/bot/v2/hook/xxxx"       # 飞书；留空或整行注释掉即跳过
wecom = "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=xxxx"  # 企业微信群机器人
# 旧写法 feishu_url / extra_urls 仍可用，会自动折叠进 channels

[database]
url = "sqlite:///data/inspection.db"
# url = "postgresql://user:pass@127.0.0.1:5432/inspection"
# url = "mysql://user:pass@127.0.0.1:3306/inspection"
```

每次巡检会写入三张表：`inspections`（这一次的结论和计数）、`inspection_sections`（各节判定和 Markdown）、`inspection_samples`（每个工具返回的原始 JSON）。`url` 留空则不入库；入库失败只记日志，报告和通知照常完成。

## HTTP 接口

```bash
# 触发巡检（servers/namespaces/services 均可选数组，notify=false 不推送）
curl -s -X POST http://127.0.0.1:8765/inspect -H 'Content-Type: application/json' \
  -d '{"servers":["srv-16"],"services":["MySQL"],"notify":false}'

# 报告列表与下载
curl -s http://127.0.0.1:8765/reports
curl -s -OJ "http://127.0.0.1:8765/reports/$(python3.14 -c "from urllib.parse import quote; print(quote('9.24巡检报告.md'))")"

# JSON-RPC（tools/list、tools/call）
curl -s http://127.0.0.1:8765 -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'
```

## 测试

```bash
python3.14 -m unittest -v
```

## 实现备注

- 远端二进制探测不用 `pgrep -f`：macOS 某些执行上下文中 pgrep 读不到其他进程 argv（误报不存活），且 ssh 远端执行时探测命令自身 argv 含关键字会自匹配（误报存活）。改用 `ps -eo args | grep -E '[首字母]其余关键字'`（括号技巧同时避免自匹配）。
- 任何单项检查失败只影响该条目的判定（🟡/🔴 + 详见日志），不会中断整场巡检。

---

## 扩展指南（全部走注册表，不改主流程）

平台有三个注册表扩展点，工作流、报告组装、HTTP 接口都由注册表驱动：

### 1. 新增巡检工具 —— `@register_tool`（mcp_inspect/registry.py）

```python
# mcp_inspect/tools.py 或任意会被导入的模块中
@register_tool("check_ntp", "巡检 NTP 时钟漂移", {"type": "object", "properties": {...}})
def check_ntp(args: dict) -> dict:
    ...  # 采集并返回结构化 JSON（含 commands / errors 供日志排查）
```

`tools/list`、`tools/call` 自动包含新工具，服务进程重启即生效。

### 2. 新增中间件部署方式 —— `@register_probe`（mcp_inspect/probes.py）

```python
@register_probe("podman")
def probe_podman(svc, host, via):
    # 返回 {"alive": bool|None, "detail": str, "command": str}；alive=None=检查执行失败
    ...
```

配置里 `deploy = "podman"` 即可通过校验，`check_middleware` 自动按类型分发，报告表格也会自动列出已注册类型。

### 3. 新增巡检项（报告新增加一节） —— `@register_section`（mcp_inspect/sections/）

完整示例（新增「NTP 时钟」节）：

```python
# ① skills/ntp.md —— 规则说明书（文中提及 check_ntp 即通过 skill-first 校验）
# ② 见上面的 @register_tool("check_ntp")
# ③ mcp_inspect/sections/ntp.py —— 文件丢进目录即被自动发现
from .. import judges
from .base import Section, SectionOutput, ToolCall, register_section

@register_section
class NtpSection(Section):
    name = "ntp"
    skill_file = "skills/ntp.md"
    required_tools = ("check_ntp",)

    def build_calls(self, ctx):
        return [ToolCall(key=f"ntp:{s.name}", tool="check_ntp",
                         arguments={"host": s.host, "via": s.via}) for s in ctx.servers]

    def process(self, ctx, results):
        # 用 judges 判定，产出本节的 markdown / levels / issues / scope_text / ok_text；
        # 返回 None 则本节整体跳过（如未配置目标）
        ...
```

工作流会自动：读取 skill → `tools/list` 校验 → 展开并并发执行工具调用 → 各节判定渲染 → 汇入总体结论与报告。新节会排在四个内置节（`base.ORDER`）之后，可用 `--sections ntp` 单独巡检。自定义配置段（如 `[[ntp]]`）可从 `cfg.raw` 读取。

### 4. 新增通知渠道 —— `@register_channel`（notifier.py）

```python
@register_channel("dingtalk")
def _dingtalk(summary: dict) -> dict:
    ...  # 组装钉钉机器人消息体
```

config 中加 `dingtalk = "https://oapi.dingtalk.com/..."`（位于 `[webhook.channels]`）即生效；未注册的渠道名会在日志里报出而不是静默丢弃。

---

## Go 版本（`go/` 目录）

`go/` 下是本平台的 Go 移植版：单二进制 CLI，功能、配置 schema、HTTP 接口、报告格式与 Python 版一致。

```bash
cd go && go build -o auto-monitor ./cmd/auto-monitor     # 需要 Go ≥ 1.24

# 1. 在指定目录初始化（生成 config/skills/reports/logs/data，重复执行不覆盖已有文件，--force 强制覆盖）
./auto-monitor init /path/to/dir

# 2. 启动巡检 MCP 服务（默认 127.0.0.1:8765）
./auto-monitor server -c /path/to/dir/config/inspection.toml

# 3. 执行巡检（选择参数与 Python 版相同）
./auto-monitor run -c /path/to/dir/config/inspection.toml \
    --servers srv-1,srv-16 --namespaces default,middleware \
    --services MySQL,Redis --sections server-resources,pods --no-notify
```

三个子命令对应 Python 版的三个入口：`run` ⇔ `inspect_runner.py`，`server` ⇔ `python -m mcp_inspect.server`，`init` 为 Go 版新增。差异点：

- `/inspect` 接口在进程内直接执行巡检（Python 版是 fork 子进程），对外行为一致（600s 超时、同样的 JSON 摘要）。
- sqlite 驱动为纯 Go 实现（modernc.org/sqlite，无 cgo）；PostgreSQL/MySQL 使用 database/sql 驱动。
- cron 用法相同：`0 2 * * * cd /path/to/dir && auto-monitor run >> logs/cron.log 2>&1`

Go 版测试：`cd go && go test ./...`（采集解析、阈值判定、sqlite 入库均有覆盖）。



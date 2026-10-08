"""巡检 MCP 服务：tools/list、tools/call（JSON-RPC）+ 报告 HTTP 接口。

- `POST /`        现有 JSON-RPC 形态：tools/list、tools/call
- `POST /inspect` 触发一次巡检（调用 inspect_runner.py），返回报告路径与下载链接
- `GET  /reports`            报告文件列表（JSON）
- `GET  /reports/<filename>` 以接口形式下载 Markdown 报告
- `GET  /healthz`            存活探针（不校验 token，不返回业务数据）

配置了 token（环境变量 INSPECTION_TOKEN，或 [server].token）后，其余接口必须带
`Authorization: Bearer <token>`，否则返回 401。报告下载是浏览器直接打开的链接，带不上这个请求头，
因此飞书按钮使用由 token 签出的 `exp` 与 `sig`（只对这一份文件有效，默认 7 天）。访问日志不记录 query。
"""

from __future__ import annotations

import argparse
import hmac
import json
import logging
import posixpath
import ssl
import subprocess
import sys
import time
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

from . import tools as tool_registry
from .config import load_config

ROOT = Path(__file__).resolve().parent.parent

log = logging.getLogger("mcp_inspect.server")


def setup_logging(log_dir: Path) -> Path:
    """服务端日志：写 <log_dir>/server.log（logging 每条都 flush，避免 nohup 下 print 缓冲）。"""
    log_dir.mkdir(parents=True, exist_ok=True)
    log_file = log_dir / "server.log"
    log.setLevel(logging.INFO)
    log.propagate = False
    if not log.handlers:
        handler = logging.FileHandler(log_file, encoding="utf-8")
        handler.setFormatter(logging.Formatter("%(asctime)s %(levelname)s %(message)s"))
        log.addHandler(handler)
    return log_file


def list_tools():
    return {"tools": tool_registry.tool_specs()}


def call_tool(params: dict):
    name = params["name"]
    start = time.monotonic()
    try:
        result = tool_registry.call_tool(name, params.get("arguments", {}))
    except ValueError as exc:
        log.warning("tools/call %s 拒绝: %s", name, exc)
        raise
    dur = int((time.monotonic() - start) * 1000)
    errors = result.get("errors") if isinstance(result, dict) else None
    log.info("tools/call %-22s %5dms errors=%s", name, dur, errors or "无")
    return {"content": [{"type": "json", "json": result}]}


def bearer_token(header_value: str) -> str:
    """从 `Authorization: Bearer <token>` 取出 token；格式不对则返回空串。"""
    scheme, _, value = (header_value or "").partition(" ")
    if scheme.lower() != "bearer" or not value.strip():
        return ""
    return value.strip()


DOWNLOAD_TTL_SECONDS = 7 * 24 * 3600


def download_url_for(base_url: str, filename: str, token: str, *, now: float | None = None) -> str:
    """浏览器能打开的报告地址。配了 token 时附上过期时间和签名，不把 token 明文放进链接。"""
    url = f"{base_url.rstrip('/')}/reports/{urllib.parse.quote(filename)}"
    if not token:
        return url
    exp = int(time.time() if now is None else now) + DOWNLOAD_TTL_SECONDS
    sig = hmac.new(token.encode(), f"{filename}\n{exp}".encode(), "sha256").hexdigest()
    return f"{url}?exp={exp}&sig={sig}"


def download_sig_ok(filename: str, token: str, exp: str, sig: str, *, now: float | None = None) -> bool:
    if not token or not exp or not sig:
        return False
    try:
        exp_i = int(exp)
    except ValueError:
        return False
    if exp_i < int(time.time() if now is None else now):
        return False
    expected = hmac.new(token.encode(), f"{filename}\n{exp_i}".encode(), "sha256").hexdigest()
    return hmac.compare_digest(expected, sig)


def token_ok(presented: str, expected: str) -> bool:
    """expected 为空表示未启用校验。比较用 hmac，避免按字符提前返回。"""
    if not expected:
        return True
    if not presented:
        return False
    return hmac.compare_digest(presented.encode(), expected.encode())


class Handler(BaseHTTPRequestHandler):
    config_path = str(ROOT / "config" / "inspection.toml")
    token = ""

    # ------------------------------------------------------------ 工具方法
    def _authorized(self) -> bool:
        if token_ok(bearer_token(self.headers.get("Authorization", "")), self.token):
            return True
        self._send_json({"error": "unauthorized"}, 401)
        return False

    def _send_json(self, payload, status=200):
        body = json.dumps(payload, ensure_ascii=False).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _report_dir(self) -> Path:
        return load_config(self.config_path).output.report_dir

    def _read_body(self) -> dict:
        size = int(self.headers.get("Content-Length", "0"))
        if not size:
            return {}
        return json.loads(self.rfile.read(size))

    # ------------------------------------------------------------ POST
    def do_POST(self):  # noqa: N802
        if not self._authorized():
            return
        path = urllib.parse.urlparse(self.path).path
        try:
            if path in ("/", ""):
                self._handle_jsonrpc()
            elif path == "/inspect":
                self._handle_inspect()
            else:
                self._send_json({"error": f"unknown path: {path}"}, 404)
        except (KeyError, ValueError, json.JSONDecodeError) as exc:
            self._send_json({"jsonrpc": "2.0", "id": None, "error": {"message": str(exc)}}, 400)

    def _handle_jsonrpc(self):
        request = self._read_body()
        method = request.get("method")
        if method == "tools/list":
            result = list_tools()
        elif method == "tools/call":
            result = call_tool(request.get("params", {}))
        else:
            raise ValueError(f"unknown method: {method}")
        self._send_json({"jsonrpc": "2.0", "id": request.get("id"), "result": result})

    def _handle_inspect(self):
        body = self._read_body() or {}
        log.info("/inspect 触发: %s", json.dumps(body, ensure_ascii=False))
        cmd = [sys.executable, str(ROOT / "inspect_runner.py"),
               "--config", body.get("config", self.config_path), "--json"]
        for key, flag in (("servers", "--servers"), ("namespaces", "--namespaces"),
                          ("services", "--services"), ("sections", "--sections")):
            value = body.get(key)
            if value:
                cmd += [flag, ",".join(value) if isinstance(value, list) else str(value)]
        if not body.get("notify", True):
            cmd.append("--no-notify")
        try:
            proc = subprocess.run(cmd, capture_output=True, text=True, timeout=600, cwd=str(ROOT))
        except subprocess.TimeoutExpired:
            log.error("/inspect 执行超时(600s): %s", body)
            self._send_json({"error": "巡检执行超时(600s)"}, 500)
            return
        if proc.stdout.strip():
            try:
                summary = json.loads(proc.stdout.strip().splitlines()[-1])
                log.info("/inspect 完成: %s %s 报告=%s",
                         summary.get("overall_emoji"), summary.get("counts"), summary.get("report_name"))
                self._send_json(summary, 200 if proc.returncode == 0 else 500)
                return
            except ValueError:
                pass
        log.error("/inspect 执行失败 rc=%s stderr=%s", proc.returncode, proc.stderr[-500:])
        self._send_json({"error": "巡检执行失败", "stderr": proc.stderr[-2000:]}, 500)

    # ------------------------------------------------------------ GET
    def do_GET(self):  # noqa: N802
        parsed = urllib.parse.urlparse(self.path)
        path = parsed.path
        if path == "/healthz":
            self._send_json({"status": "ok"})
            return
        if path.startswith("/reports/") and path != "/reports/":
            self._download_report(path[len("/reports/"):])
            return
        if not self._authorized():
            return
        if path == "/reports":
            self._list_reports()
        else:
            self._send_json({"error": f"unknown path: {path}"}, 404)

    def _list_reports(self):
        report_dir = self._report_dir()
        files = []
        if report_dir.is_dir():
            for item in sorted(report_dir.glob("*.md"), key=lambda p: p.stat().st_mtime, reverse=True):
                files.append({
                    "name": item.name,
                    "size": item.stat().st_size,
                    "download_url": f"/reports/{urllib.parse.quote(item.name)}",
                })
        self._send_json({"reports": files})

    def _download_allowed(self, filename: str) -> bool:
        """请求头里的 Bearer，或链接上的签名，二者任一通过即可。未配置 token 则放行。"""
        if token_ok(bearer_token(self.headers.get("Authorization", "")), self.token):
            return True
        query = urllib.parse.parse_qs(urllib.parse.urlparse(self.path).query)
        exp = (query.get("exp") or [""])[0]
        sig = (query.get("sig") or [""])[0]
        if download_sig_ok(filename, self.token, exp, sig):
            return True
        self._send_json({"error": "unauthorized"}, 401)
        return False

    def _download_report(self, raw_name: str):
        name = Path(posixpath.basename(urllib.parse.unquote(raw_name))).name
        if not self._download_allowed(name):
            return
        if not name.endswith(".md"):
            self._send_json({"error": "仅支持下载 .md 报告"}, 400)
            return
        target = self._report_dir() / name
        if not target.is_file():
            self._send_json({"error": f"报告不存在: {name}"}, 404)
            return
        body = target.read_bytes()
        self.send_response(200)
        self.send_header("Content-Type", "text/markdown; charset=utf-8")
        self.send_header("Content-Disposition",
                         f"attachment; filename*=UTF-8''{urllib.parse.quote(name)}")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_request(self, code="-", size="-"):
        # 只记路径，不记 query，避免 token 出现在访问日志里
        path = urllib.parse.urlparse(self.path).path
        log.info("%s %s %s -> %s", self.client_address[0], self.command, path, code)

    def log_message(self, fmt, *args):
        log.warning("handler: " + fmt, *args)


def main():
    parser = argparse.ArgumentParser(description="巡检 MCP 服务")
    parser.add_argument("--config", default=str(ROOT / "config" / "inspection.toml"))
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=8765)
    args = parser.parse_args()

    Handler.config_path = str(Path(args.config).resolve()) if not Path(args.config).is_absolute() else args.config
    cfg = load_config(Handler.config_path)
    Handler.token = cfg.token
    log_file = setup_logging(cfg.output.log_dir)
    server = ThreadingHTTPServer((args.host, args.port), Handler)
    scheme = "http"
    if cfg.certfile and cfg.keyfile:
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.load_cert_chain(cfg.certfile, cfg.keyfile)
        server.socket = context.wrap_socket(server.socket, server_side=True)
        scheme = "https"
    if cfg.token:
        log.info("token 校验已启用（除 /healthz 外需要 Authorization: Bearer）")
    else:
        log.warning("未配置 token，接口不校验身份。设置 INSPECTION_TOKEN 或 [server].token 后才会拒绝未授权访问")
    log.info("巡检 MCP 服务监听于 %s://%s:%s（config: %s，日志: %s）",
             scheme, args.host, args.port, args.config, log_file)
    server.serve_forever()


if __name__ == "__main__":
    main()

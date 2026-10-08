"""TOML 配置加载与校验（标准库 tomllib，无第三方依赖）。"""

from __future__ import annotations

try:
    import tomllib  # Python >= 3.11
except ImportError:  # Python 3.10 回退：pip install tomli
    import tomli as tomllib

import os
from dataclasses import dataclass, field
from pathlib import Path

from .probes import PROBES


@dataclass
class ServerCfg:
    name: str
    host: str
    via: str = "ssh"  # local | ssh
    mounts: list = field(default_factory=lambda: ["/"])


@dataclass
class ServiceCfg:
    name: str
    deploy: str  # binary | container | k8s
    host: str = ""
    port: int = 0
    process: str = ""
    container: str = ""
    k8s: dict = field(default_factory=dict)


@dataclass
class OutputCfg:
    report_dir: Path
    log_dir: Path
    base_url: str


@dataclass
class Thresholds:
    cpu_warn: float = 80.0
    mem_warn: float = 80.0
    disk_warn: float = 75.0
    disk_crit: float = 90.0
    load_per_core_warn: float = 2.0
    pod_restart_warn: int = 5


@dataclass
class WebhookCfg:
    # 渠道名 -> 单个 URL 或 URL 列表；渠道类型由 notifier.CHANNELS 注册表解释（内置 feishu / wecom / generic，可扩展）
    channels: dict = field(default_factory=dict)


@dataclass
class DatabaseCfg:
    # 空字符串表示不入库。sqlite / postgresql / mysql，见 mcp_inspect/store.py
    url: str = ""


@dataclass
class Config:
    root: Path  # 项目根目录
    path: Path
    server_url: str
    token: str  # 非空时，HTTP 接口要求 Authorization: Bearer <token>
    output: OutputCfg
    thresholds: Thresholds
    servers: list
    namespaces: list
    services: list
    webhook: WebhookCfg
    database: DatabaseCfg
    certfile: str = ""       # 非空且与 keyfile 成对时，MCP 服务以 HTTPS 监听
    keyfile: str = ""
    tls_verify: bool = True  # 工作流访问 https 时是否校验证书；自签证书可设为 false
    raw: dict = field(default_factory=dict)  # 配置原文，扩展插件可读取自定义配置段


def _resolve(root: Path, value: str) -> Path:
    path = Path(value).expanduser()
    return path if path.is_absolute() else (root / path)


def _require(mapping: dict, key: str, where: str):
    if key not in mapping or mapping[key] in (None, ""):
        raise ValueError(f"配置错误：{where} 缺少必填字段 {key}")
    return mapping[key]


def load_config(path: str) -> Config:
    config_path = Path(path).expanduser().resolve()
    if not config_path.is_file():
        raise FileNotFoundError(f"配置文件不存在: {config_path}")
    data = tomllib.loads(config_path.read_text(encoding="utf-8"))
    # 约定：配置文件位于 <root>/config/ 下；否则以配置文件所在目录为根。
    root = config_path.parent.parent if config_path.parent.name == "config" else config_path.parent

    out = data.get("output", {})
    th = data.get("thresholds", {})
    wh = data.get("webhook", {})

    servers = []
    for i, raw in enumerate(data.get("servers", [])):
        where = f"servers[{i}]"
        via = raw.get("via", "ssh")
        if via not in ("local", "ssh"):
            raise ValueError(f"配置错误：{where}.via 只能是 local 或 ssh，当前为 {via}")
        servers.append(ServerCfg(
            name=_require(raw, "name", where),
            host=_require(raw, "host", where),
            via=via,
            mounts=raw.get("mounts", ["/"]),
        ))

    namespaces = [_require(raw, "name", f"namespaces[{i}]") for i, raw in enumerate(data.get("namespaces", []))]

    services = []
    for i, raw in enumerate(data.get("services", [])):
        where = f"services[{i}]"
        deploy = _require(raw, "deploy", where)
        if deploy not in PROBES:
            raise ValueError(f"配置错误：{where}.deploy 未注册（已注册: {sorted(PROBES)}），当前为 {deploy}")
        services.append(ServiceCfg(
            name=_require(raw, "name", where),
            deploy=deploy,
            host=raw.get("host", ""),
            port=int(_require(raw, "port", where)),
            process=raw.get("process", ""),
            container=raw.get("container", ""),
            k8s=raw.get("k8s", {}),
        ))

    channels = dict(wh.get("channels", {}))
    # 兼容旧写法：feishu_url / extra_urls 折叠进 channels
    if wh.get("feishu_url") and "feishu" not in channels:
        channels["feishu"] = wh["feishu_url"]
    if wh.get("extra_urls") and "generic" not in channels:
        channels["generic"] = list(wh["extra_urls"])

    server = data.get("server", {})
    # 环境变量优先，避免把口令写进可提交的配置文件
    token = os.environ.get("INSPECTION_TOKEN", "").strip() or str(server.get("token", "")).strip()
    # 环境变量优先，避免把数据库口令写进可提交的配置文件
    database_url = os.environ.get("INSPECTION_DATABASE_URL", "").strip() or str(
        data.get("database", {}).get("url", "")).strip()
    certfile = str(server.get("certfile", "")).strip()
    keyfile = str(server.get("keyfile", "")).strip()
    if bool(certfile) != bool(keyfile):
        raise ValueError("配置错误：[server] 的 certfile 和 keyfile 必须同时填写")
    cert_path = str(_resolve(root, certfile)) if certfile else ""
    key_path = str(_resolve(root, keyfile)) if keyfile else ""
    for label, file_path in (("certfile", cert_path), ("keyfile", key_path)):
        if file_path and not Path(file_path).is_file():
            raise FileNotFoundError(f"配置错误：[server].{label} 文件不存在: {file_path}")

    return Config(
        root=root,
        path=config_path,
        server_url=server.get("url", "http://127.0.0.1:8765").rstrip("/"),
        token=token,
        output=OutputCfg(
            report_dir=_resolve(root, out.get("report_dir", "reports")),
            log_dir=_resolve(root, out.get("log_dir", "logs")),
            base_url=out.get("base_url", "http://127.0.0.1:8765").rstrip("/"),
        ),
        thresholds=Thresholds(
            cpu_warn=float(th.get("cpu_warn", 80)),
            mem_warn=float(th.get("mem_warn", 80)),
            disk_warn=float(th.get("disk_warn", 75)),
            disk_crit=float(th.get("disk_crit", 90)),
            load_per_core_warn=float(th.get("load_per_core_warn", 2)),
            pod_restart_warn=int(th.get("pod_restart_warn", 5)),
        ),
        servers=servers,
        namespaces=namespaces,
        services=services,
        webhook=WebhookCfg(channels=channels),
        database=DatabaseCfg(url=database_url),
        certfile=cert_path,
        keyfile=key_path,
        tls_verify=bool(server.get("tls_verify", True)),
        raw=data,
    )

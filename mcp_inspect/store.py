"""把一次巡检写入数据库。SQLite、PostgreSQL、MySQL 共用同一套表。

地址写在配置 [database].url，或环境变量 INSPECTION_DATABASE_URL（优先）。留空则不入库。

    sqlite:///data/inspection.db                 相对项目根目录
    sqlite:////var/lib/auto-monitor/inspection.db 绝对路径
    postgresql://user:pass@127.0.0.1:5432/inspection
    mysql://user:pass@127.0.0.1:3306/inspection

SQLite 使用标准库。PostgreSQL 需要 ``pip install 'psycopg[binary]'``，
MySQL 需要 ``pip install pymysql``。
"""

from __future__ import annotations

import json
import uuid
from pathlib import Path
from urllib.parse import parse_qs, unquote, urlparse

_INSPECTIONS = """
CREATE TABLE IF NOT EXISTS inspections (
    id VARCHAR(36) PRIMARY KEY,
    checked_at VARCHAR(32) NOT NULL,
    title VARCHAR(255) NOT NULL,
    overall VARCHAR(16) NOT NULL,
    note VARCHAR(255) NOT NULL,
    ok_count INTEGER NOT NULL,
    warn_count INTEGER NOT NULL,
    crit_count INTEGER NOT NULL,
    report_name VARCHAR(255) NOT NULL,
    report_path __TEXT__ NOT NULL,
    log_path __TEXT__ NOT NULL,
    download_url __TEXT__ NOT NULL,
    sections __TEXT__ NOT NULL,
    bullets __TEXT__ NOT NULL
)
"""

_SECTIONS = """
CREATE TABLE IF NOT EXISTS inspection_sections (
    id VARCHAR(36) PRIMARY KEY,
    inspection_id VARCHAR(36) NOT NULL,
    name VARCHAR(64) NOT NULL,
    levels __TEXT__ NOT NULL,
    issues __TEXT__ NOT NULL,
    scope_text __TEXT__ NOT NULL,
    ok_text __TEXT__ NOT NULL,
    markdown __TEXT__ NOT NULL
)
"""

_SAMPLES = """
CREATE TABLE IF NOT EXISTS inspection_samples (
    id VARCHAR(36) PRIMARY KEY,
    inspection_id VARCHAR(36) NOT NULL,
    item_key VARCHAR(255) NOT NULL,
    tool VARCHAR(64) NOT NULL,
    ok INTEGER,
    payload __TEXT__ NOT NULL
)
"""


def _dump(value) -> str:
    return json.dumps(value, ensure_ascii=False, default=str)


def sqlite_path(url: str, root: Path) -> str:
    """sqlite URL 转成本地文件路径。`:memory:` 原样返回。"""
    path = urlparse(url).path or ""
    if path in ("/:memory:", "/:memory:", ":memory:"):
        return ":memory:"
    # sqlite:////abs/file.db → path 以 // 开头，去掉一个斜杠后是绝对路径
    if path.startswith("//"):
        return path[1:]
    relative = path.lstrip("/")
    if not relative:
        raise ValueError("sqlite 地址缺少文件路径")
    target = Path(relative).expanduser()
    if not target.is_absolute():
        target = root / target
    return str(target)


def connect(url: str, root: Path):
    """返回 (连接, 占位符, 大文本类型)。调用方负责关闭连接。"""
    scheme = (urlparse(url).scheme or "").lower()
    if scheme == "sqlite":
        import sqlite3

        path = sqlite_path(url, root)
        if path != ":memory:":
            Path(path).parent.mkdir(parents=True, exist_ok=True)
        conn = sqlite3.connect(path)
        return conn, "?", "TEXT"
    if scheme in ("postgresql", "postgres"):
        try:
            import psycopg
        except ImportError as exc:
            raise RuntimeError("PostgreSQL 需要先安装驱动：pip install 'psycopg[binary]'") from exc
        return psycopg.connect(url), "%s", "TEXT"
    if scheme in ("mysql", "mariadb"):
        try:
            import pymysql
        except ImportError as exc:
            raise RuntimeError("MySQL 需要先安装驱动：pip install pymysql") from exc
        parsed = urlparse(url)
        charset = (parse_qs(parsed.query).get("charset") or ["utf8mb4"])[0]
        database = parsed.path.lstrip("/")
        if not database:
            raise ValueError("mysql 地址缺少库名")
        conn = pymysql.connect(
            host=parsed.hostname or "127.0.0.1",
            port=parsed.port or 3306,
            user=unquote(parsed.username or ""),
            password=unquote(parsed.password or ""),
            database=database,
            charset=charset,
            autocommit=False,
        )
        return conn, "%s", "LONGTEXT"
    raise ValueError(f"不支持的数据库地址：{scheme or url}（可用 sqlite、postgresql、mysql）")


def _sql(statement: str, placeholder: str, text_type: str) -> str:
    statement = statement.replace("__TEXT__", text_type)
    if placeholder != "?":
        statement = statement.replace("?", placeholder)
    return statement


def _execute(conn, placeholder: str, text_type: str, statement: str, params=()):
    cur = conn.cursor()
    cur.execute(_sql(statement, placeholder, text_type), params)
    return cur


def ensure_schema(conn, placeholder: str, text_type: str) -> None:
    for statement in (_INSPECTIONS, _SECTIONS, _SAMPLES):
        _execute(conn, placeholder, text_type, statement)


def _ok_flag(payload) -> int | None:
    if not isinstance(payload, dict) or "ok" not in payload:
        return None
    return 1 if payload.get("ok") else 0


def save_inspection(url: str, root: Path, summary: dict, outputs: list, results: dict) -> str:
    """写入一次巡检。返回 inspections.id。失败时抛出异常，由调用方决定是否影响巡检结果。"""
    inspection_id = str(uuid.uuid4())
    counts = summary.get("counts") or {}
    conn, placeholder, text_type = connect(url, root)
    try:
        ensure_schema(conn, placeholder, text_type)
        _execute(
            conn, placeholder, text_type,
            """
            INSERT INTO inspections (
                id, checked_at, title, overall, note,
                ok_count, warn_count, crit_count,
                report_name, report_path, log_path, download_url,
                sections, bullets
            ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
            """,
            (
                inspection_id,
                summary.get("checked_at") or "",
                summary.get("title") or "",
                summary.get("overall") or "",
                summary.get("note") or "",
                int(counts.get("ok", 0)),
                int(counts.get("warn", 0)),
                int(counts.get("crit", 0)),
                summary.get("report_name") or "",
                summary.get("report_path") or "",
                summary.get("log_path") or "",
                summary.get("download_url") or "",
                _dump(summary.get("sections") or []),
                _dump(summary.get("bullets") or []),
            ),
        )
        for out in outputs:
            _execute(
                conn, placeholder, text_type,
                """
                INSERT INTO inspection_sections (
                    id, inspection_id, name, levels, issues, scope_text, ok_text, markdown
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
                """,
                (
                    str(uuid.uuid4()),
                    inspection_id,
                    out.name,
                    _dump(out.levels),
                    _dump(out.issues),
                    out.scope_text or "",
                    out.ok_text or "",
                    out.markdown or "",
                ),
            )
        for key, payload in (results or {}).items():
            tool = payload.get("tool") if isinstance(payload, dict) else ""
            _execute(
                conn, placeholder, text_type,
                """
                INSERT INTO inspection_samples (
                    id, inspection_id, item_key, tool, ok, payload
                ) VALUES (?, ?, ?, ?, ?, ?)
                """,
                (
                    str(uuid.uuid4()),
                    inspection_id,
                    key,
                    tool or "",
                    _ok_flag(payload),
                    _dump(payload),
                ),
            )
        conn.commit()
    except Exception:
        conn.rollback()
        raise
    finally:
        conn.close()
    return inspection_id

// Package store 把一次巡检写入数据库。SQLite、PostgreSQL、MySQL 共用同一套表
// （对应 Python 版 mcp_inspect/store.py）。
//
// 地址写在配置 [database].url，或环境变量 INSPECTION_DATABASE_URL（优先）。留空则不入库。
//
//	sqlite:///data/inspection.db                  相对项目根目录
//	sqlite:////var/lib/auto-monitor/inspection.db  绝对路径
//	postgresql://user:pass@127.0.0.1:5432/inspection
//	mysql://user:pass@127.0.0.1:3306/inspection
package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"auto-monitor/internal/sections"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
	_ "modernc.org/sqlite"
)

const inspectionsDDL = `
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
)`

const sectionsDDL = `
CREATE TABLE IF NOT EXISTS inspection_sections (
    id VARCHAR(36) PRIMARY KEY,
    inspection_id VARCHAR(36) NOT NULL,
    name VARCHAR(64) NOT NULL,
    levels __TEXT__ NOT NULL,
    issues __TEXT__ NOT NULL,
    scope_text __TEXT__ NOT NULL,
    ok_text __TEXT__ NOT NULL,
    markdown __TEXT__ NOT NULL
)`

const samplesDDL = `
CREATE TABLE IF NOT EXISTS inspection_samples (
    id VARCHAR(36) PRIMARY KEY,
    inspection_id VARCHAR(36) NOT NULL,
    item_key VARCHAR(255) NOT NULL,
    tool VARCHAR(64) NOT NULL,
    ok INTEGER,
    payload __TEXT__ NOT NULL
)`

// sqlitePath sqlite URL 转成本地文件路径。`:memory:` 原样返回。
func sqlitePath(rawURL, root string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	path := u.Path
	if path == "/:memory:" || path == ":memory:" {
		return ":memory:", nil
	}
	// sqlite:////abs/file.db → path 以 // 开头，去掉一个斜杠后是绝对路径
	if strings.HasPrefix(path, "//") {
		return path[1:], nil
	}
	relative := strings.TrimLeft(path, "/")
	if relative == "" {
		return "", fmt.Errorf("sqlite 地址缺少文件路径")
	}
	target := relative
	if strings.HasPrefix(target, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			target = home + target[1:]
		}
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(root, target)
	}
	return target, nil
}

// dialect 描述一种数据库的打开方式。
type dialect struct {
	driver      string
	dsn         string
	placeholder func(int) string // 返回第 n 个占位符（1-based）
	textType    string
}

func questionMark(int) string { return "?" }

func pgPlaceholder(n int) string { return fmt.Sprintf("$%d", n) }

// resolveDialect 按 URL scheme 选择驱动并给出 DSN。
func resolveDialect(rawURL, root string) (*dialect, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "sqlite":
		path, err := sqlitePath(rawURL, root)
		if err != nil {
			return nil, err
		}
		if path != ":memory:" {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return nil, err
			}
		}
		return &dialect{driver: "sqlite", dsn: path, placeholder: questionMark, textType: "TEXT"}, nil
	case "postgresql", "postgres":
		dsn := rawURL
		// lib/pq 默认 sslmode=require，psycopg 默认 prefer；对齐 Python 行为
		if !strings.Contains(dsn, "sslmode=") {
			if strings.Contains(dsn, "?") {
				dsn += "&sslmode=prefer"
			} else {
				dsn += "?sslmode=prefer"
			}
		}
		return &dialect{driver: "postgres", dsn: dsn, placeholder: pgPlaceholder, textType: "TEXT"}, nil
	case "mysql", "mariadb":
		if u.Path == "" || u.Path == "/" {
			return nil, fmt.Errorf("mysql 地址缺少库名")
		}
		charset := u.Query().Get("charset")
		if charset == "" {
			charset = "utf8mb4"
		}
		password, _ := u.User.Password()
		host := u.Hostname()
		if host == "" {
			host = "127.0.0.1"
		}
		port := u.Port()
		if port == "" {
			port = "3306"
		}
		dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?charset=%s",
			u.User.Username(), password, host, port, strings.TrimPrefix(u.Path, "/"), charset)
		return &dialect{driver: "mysql", dsn: dsn, placeholder: questionMark, textType: "LONGTEXT"}, nil
	}
	return nil, fmt.Errorf("不支持的数据库地址：%s（可用 sqlite、postgresql、mysql）", scheme)
}

func (d *dialect) sql(statement string) string {
	statement = strings.ReplaceAll(statement, "__TEXT__", d.textType)
	if d.placeholder(1) == "?" {
		return statement
	}
	var b strings.Builder
	n := 0
	for _, r := range statement {
		if r == '?' {
			n++
			b.WriteString(d.placeholder(n))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// dump 序列化为 JSON 字符串（对应 Python json.dumps ensure_ascii=False）。
func dump(value any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(value)
	return strings.TrimRight(b.String(), "\n")
}

// uuid4 生成随机 UUID v4（避免额外依赖）。
func uuid4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// SaveInspection 写入一次巡检，返回 inspections.id。
// 失败时返回错误，由调用方决定是否影响巡检结果（Python 版是记日志后继续）。
func SaveInspection(rawURL, root string, summary map[string]any,
	outputs []*sections.Output, results map[string]map[string]any) (string, error) {
	d, err := resolveDialect(rawURL, root)
	if err != nil {
		return "", err
	}
	db, err := sql.Open(d.driver, d.dsn)
	if err != nil {
		return "", err
	}
	defer db.Close()
	ctx := context.Background()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	for _, ddl := range []string{inspectionsDDL, sectionsDDL, samplesDDL} {
		if _, err := tx.ExecContext(ctx, d.sql(ddl)); err != nil {
			return "", fmt.Errorf("建表失败: %w", err)
		}
	}

	inspectionID, err := uuid4()
	if err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, d.sql(`
        INSERT INTO inspections (
            id, checked_at, title, overall, note,
            ok_count, warn_count, crit_count,
            report_name, report_path, log_path, download_url,
            sections, bullets
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		inspectionID,
		strKey(summary, "checked_at"),
		strKey(summary, "title"),
		strKey(summary, "overall"),
		strKey(summary, "note"),
		countOf(summary, "ok"),
		countOf(summary, "warn"),
		countOf(summary, "crit"),
		strKey(summary, "report_name"),
		strKey(summary, "report_path"),
		strKey(summary, "log_path"),
		strKey(summary, "download_url"),
		dump(sliceOrEmpty(summary, "sections")),
		dump(sliceOrEmpty(summary, "bullets")),
	)
	if err != nil {
		return "", fmt.Errorf("写入 inspections 失败: %w", err)
	}

	for _, out := range outputs {
		rowID, err := uuid4()
		if err != nil {
			return "", err
		}
		_, err = tx.ExecContext(ctx, d.sql(`
            INSERT INTO inspection_sections (
                id, inspection_id, name, levels, issues, scope_text, ok_text, markdown
            ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
			rowID, inspectionID, out.Name,
			dump(out.Levels), dump(out.Issues), out.ScopeText, out.OKText, out.Markdown,
		)
		if err != nil {
			return "", fmt.Errorf("写入 inspection_sections 失败: %w", err)
		}
	}

	for key, payload := range results {
		rowID, err := uuid4()
		if err != nil {
			return "", err
		}
		tool := strKey(payload, "tool")
		var okFlag any
		if v, exists := payload["ok"]; exists {
			if b, isBool := v.(bool); isBool {
				if b {
					okFlag = 1
				} else {
					okFlag = 0
				}
			}
		}
		_, err = tx.ExecContext(ctx, d.sql(`
            INSERT INTO inspection_samples (
                id, inspection_id, item_key, tool, ok, payload
            ) VALUES (?, ?, ?, ?, ?, ?)`),
			rowID, inspectionID, key, tool, okFlag, dump(payload),
		)
		if err != nil {
			return "", fmt.Errorf("写入 inspection_samples 失败: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return "", err
	}
	committed = true
	return inspectionID, nil
}

// countOf 读取 summary["counts"] 中的计数；runner 产出 map[string]int，
// 经 JSON 往返后则是 map[string]any（float64），两者都要兼容。
func countOf(summary map[string]any, key string) int {
	switch counts := summary["counts"].(type) {
	case map[string]int:
		return counts[key]
	case map[string]any:
		return intKey(counts, key)
	}
	return 0
}

func strKey(m map[string]any, key string) string {
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

func intKey(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

func sliceOrEmpty(m map[string]any, key string) any {
	if v, ok := m[key]; ok && v != nil {
		return v
	}
	return []any{}
}

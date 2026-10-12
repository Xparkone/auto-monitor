package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"auto-monitor/internal/sections"
)

func TestSQLitePath(t *testing.T) {
	root := "/proj"
	mem, err := sqlitePath("sqlite:///:memory:", root)
	if err != nil || mem != ":memory:" {
		t.Fatalf("memory: %q %v", mem, err)
	}
	rel, err := sqlitePath("sqlite:///data/inspection.db", root)
	if err != nil || rel != "/proj/data/inspection.db" {
		t.Fatalf("relative: %q %v", rel, err)
	}
	abs, err := sqlitePath("sqlite:////var/lib/auto-monitor/inspection.db", root)
	if err != nil || abs != "/var/lib/auto-monitor/inspection.db" {
		t.Fatalf("absolute: %q %v", abs, err)
	}
	if _, err := sqlitePath("sqlite:///", root); err == nil {
		t.Fatal("expect error for empty path")
	}
}

func TestResolveDialectUnsupported(t *testing.T) {
	if _, err := resolveDialect("oracle://x", "."); err == nil {
		t.Fatal("expect error")
	}
}

func TestSaveInspectionSQLite(t *testing.T) {
	dir := t.TempDir()
	dbURL := "sqlite:///data/inspection.db"
	summary := map[string]any{
		"checked_at":   "2026-10-12 09:30:00",
		"title":        "10.12 巡检报告",
		"overall":      "优秀",
		"note":         "（无异常项）",
		"report_name":  "10.12巡检报告.md",
		"report_path":  "reports/10.12巡检报告.md",
		"log_path":     "logs/inspection-20261012-093000.log",
		"download_url": "http://127.0.0.1:8765/reports/10.12%E5%B7%A1%E6%A3%80%E6%8A%A5%E5%91%8A.md",
		"counts":       map[string]int{"ok": 2, "warn": 1, "crit": 0}, // 与 runner 产出类型一致
		"sections":     []any{"server-resources", "disks"},
		"bullets":      []any{"一切正常"},
	}
	outputs := []*sections.Output{{
		Name: "server-resources", Markdown: "## 服务器资源状态",
		Levels: []string{"ok"}, Issues: []string{},
		ScopeText: "资源：CPU、内存、负载、进程", OKText: "资源状态良好",
	}}
	results := map[string]map[string]any{
		"resources:本机": {"tool": "check_host_resources", "ok": true, "cpu_percent": 3.2},
	}

	id, err := SaveInspection(dbURL, dir, summary, outputs, results)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if id == "" {
		t.Fatal("empty id")
	}

	if _, err := os.Stat(filepath.Join(dir, "data", "inspection.db")); err != nil {
		t.Fatalf("db file: %v", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "data", "inspection.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for table, want := range map[string]int{
		"inspections": 1, "inspection_sections": 1, "inspection_samples": 1,
	} {
		var n int
		if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil || n != want {
			t.Fatalf("%s count=%d err=%v, want %d", table, n, err, want)
		}
	}
	var title string
	if err := db.QueryRow("SELECT title FROM inspections WHERE id = ?", id).Scan(&title); err != nil || title != "10.12 巡检报告" {
		t.Fatalf("title=%q err=%v", title, err)
	}
	var okCount, warnCount, critCount int
	if err := db.QueryRow("SELECT ok_count, warn_count, crit_count FROM inspections WHERE id = ?", id).
		Scan(&okCount, &warnCount, &critCount); err != nil {
		t.Fatal(err)
	}
	if okCount != 2 || warnCount != 1 || critCount != 0 {
		t.Fatalf("counts=%d/%d/%d", okCount, warnCount, critCount)
	}
	var okFlag int
	if err := db.QueryRow("SELECT ok FROM inspection_samples WHERE item_key = ?", "resources:本机").Scan(&okFlag); err != nil || okFlag != 1 {
		t.Fatalf("ok flag=%d err=%v", okFlag, err)
	}
}

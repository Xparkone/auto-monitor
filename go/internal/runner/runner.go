// Package runner 巡检工作流编排（对应 Python 版 inspect_runner.py）：
// 读取 skills 与配置，按节插件展开调用 MCP 工具，产出报告、日志并推送 webhook。
//
// 职责划分：skills/*.md 是规则说明书，本包是“调用者”，
// MCP 服务（mcpserver）暴露工具，工具只负责采集，判定与渲染在各节插件
// （sections）中完成 —— 本包不包含任何具体巡检项的知识，新增巡检项无需修改。
package runner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"auto-monitor/internal/config"
	"auto-monitor/internal/judges"
	"auto-monitor/internal/mcpclient"
	"auto-monitor/internal/notifier"
	"auto-monitor/internal/render"
	"auto-monitor/internal/sections"
	"auto-monitor/internal/signurl"
	"auto-monitor/internal/store"
)

// Options 一次巡检的选择参数；空切片 = 全量。
type Options struct {
	ServersSel    []string
	NamespacesSel []string
	ServicesSel   []string
	SectionsSel   []string
	NotifyOn      bool
}

// logger 巡检日志：每行带时间戳与级别，写独立日志文件（并发安全）。
type logger struct {
	mu sync.Mutex
	f  *os.File
}

func newLogger(logDir string) (*logger, string, error) {
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, "", err
	}
	stamp := time.Now().Format("20060102-150405")
	path := filepath.Join(logDir, fmt.Sprintf("inspection-%s.log", stamp))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, "", err
	}
	return &logger{f: f}, path, nil
}

func (l *logger) printf(level, format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	ts := now.Format("2006-01-02 15:04:05") + fmt.Sprintf(",%03d", now.Nanosecond()/1e6)
	fmt.Fprintf(l.f, "%s %s %s\n", ts, level, fmt.Sprintf(format, args...))
}

func (l *logger) Infof(format string, args ...any)  { l.printf("INFO", format, args...) }
func (l *logger) Errorf(format string, args ...any) { l.printf("ERROR", format, args...) }
func (l *logger) Close()                            { _ = l.f.Close() }

// jsonLog 序列化用于日志行（不转义 HTML，贴近 Python ensure_ascii=False）。
func jsonLog(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return strings.TrimRight(b.String(), "\n")
}

func selectItems[T any](items []T, selector []string, keys func(T) []string) ([]T, error) {
	if len(selector) == 0 {
		return items, nil
	}
	wanted := map[string]bool{}
	for _, s := range selector {
		wanted[s] = true
	}
	known := map[string]bool{}
	var selected []T
	for _, item := range items {
		matched := false
		for _, ident := range keys(item) {
			known[ident] = true
			if wanted[ident] {
				matched = true
			}
		}
		if matched {
			selected = append(selected, item)
		}
	}
	var unknown []string
	for _, s := range selector {
		if !known[s] {
			unknown = append(unknown, s)
		}
	}
	if len(unknown) > 0 {
		var knownList []string
		for k := range known {
			knownList = append(knownList, k)
		}
		sort.Strings(knownList)
		return nil, fmt.Errorf("选择的目标不在配置中: [%s]（可选: [%s]）",
			strings.Join(unknown, ", "), strings.Join(knownList, ", "))
	}
	return selected, nil
}

// verifySkills skill-first 校验：各节声明的规则说明书存在且提及所需工具；
// tools/list 已注册这些工具。
func verifySkills(log *logger, client *mcpclient.Client, root string, secs []sections.Section) error {
	needed := map[string]bool{}
	for _, sec := range secs {
		if sec.SkillFile() == "" {
			continue
		}
		skillPath := filepath.Join(root, sec.SkillFile())
		raw, err := os.ReadFile(skillPath)
		if err != nil {
			return fmt.Errorf("节 %s 声明的规则说明书不存在: %s", sec.Name(), sec.SkillFile())
		}
		text := string(raw)
		var missing []string
		for _, tool := range sec.RequiredTools() {
			if !strings.Contains(text, tool) {
				missing = append(missing, tool)
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("%s 中未声明所需工具: [%s]", sec.SkillFile(), strings.Join(missing, ", "))
		}
		for _, tool := range sec.RequiredTools() {
			needed[tool] = true
		}
		log.Infof("skill 校验通过: %s -> %s", sec.SkillFile(), sec.RequiredTools())
	}
	specs, err := client.ListTools()
	if err != nil {
		return err
	}
	registered := map[string]bool{}
	var registeredNames []string
	for _, spec := range specs {
		if name, ok := spec["name"].(string); ok {
			registered[name] = true
			registeredNames = append(registeredNames, name)
		}
	}
	var absent []string
	for tool := range needed {
		if !registered[tool] {
			absent = append(absent, tool)
		}
	}
	if len(absent) > 0 {
		sort.Strings(absent)
		return fmt.Errorf("skills 要求的工具未在 MCP 服务注册: [%s]", strings.Join(absent, ", "))
	}
	sort.Strings(registeredNames)
	log.Infof("tools/list 校验通过: [%s]", strings.Join(registeredNames, ", "))
	return nil
}

// callTool 调用工具并记录排查日志；失败时返回降级结果而不是中断整场巡检。
func callTool(log *logger, client *mcpclient.Client, name string, arguments map[string]any) map[string]any {
	start := time.Now()
	log.Infof("tools/call %s args=%s", name, jsonLog(arguments))
	result, err := client.CallTool(name, arguments)
	if err != nil {
		dur := time.Since(start).Milliseconds()
		log.Errorf("  <- %s 调用失败 %dms: %s", name, dur, err)
		return map[string]any{
			"tool": name, "ok": false,
			"commands": []string{}, "errors": []string{fmt.Sprintf("工具调用失败: %s", err)},
		}
	}
	dur := time.Since(start).Milliseconds()
	for _, command := range strSlice(result["commands"]) {
		log.Infof("  $ %s", command)
	}
	errText := "无"
	if errs := strSlice(result["errors"]); len(errs) > 0 {
		errText = jsonLog(errs)
	}
	log.Infof("  <- %s %dms errors=%s", name, dur, errText)
	return result
}

func strSlice(v any) []string {
	var out []string
	switch items := v.(type) {
	case []string:
		out = items
	case []any:
		for _, item := range items {
			out = append(out, fmt.Sprint(item))
		}
	}
	return out
}

// RunInspection 执行一次完整巡检，返回 summary（结构同 Python 版）。
func RunInspection(cfg *config.Config, opts Options) (map[string]any, error) {
	if err := os.MkdirAll(cfg.Output.ReportDir, 0o755); err != nil {
		return nil, err
	}
	log, logFile, err := newLogger(cfg.Output.LogDir)
	if err != nil {
		return nil, err
	}
	defer log.Close()
	now := time.Now()
	log.Infof("==== 巡检开始 %s ====", now.Format("2006-01-02T15:04:05"))

	servers, err := selectItems(cfg.Servers, opts.ServersSel,
		func(s config.ServerCfg) []string { return []string{s.Name, s.Host} })
	if err != nil {
		return nil, err
	}
	knownNS := map[string]bool{}
	for _, n := range cfg.Namespaces {
		knownNS[n] = true
	}
	var unknownNS []string
	for _, n := range opts.NamespacesSel {
		if !knownNS[n] {
			unknownNS = append(unknownNS, n)
		}
	}
	if len(unknownNS) > 0 {
		return nil, fmt.Errorf("选择的命名空间不在配置中: [%s]（可选: [%s]）",
			strings.Join(unknownNS, ", "), strings.Join(cfg.Namespaces, ", "))
	}
	namespaces := cfg.Namespaces
	if len(opts.NamespacesSel) > 0 {
		namespaces = nil
		for _, n := range cfg.Namespaces {
			if contains(opts.NamespacesSel, n) {
				namespaces = append(namespaces, n)
			}
		}
	}
	services, err := selectItems(cfg.Services, opts.ServicesSel,
		func(s config.ServiceCfg) []string { return []string{s.Name} })
	if err != nil {
		return nil, err
	}

	allSections := sections.All()
	sectionByName := map[string]sections.Section{}
	var sectionNames []string
	for _, sec := range allSections {
		sectionByName[sec.Name()] = sec
		sectionNames = append(sectionNames, sec.Name())
	}
	var unknownSec []string
	for _, name := range opts.SectionsSel {
		if _, ok := sectionByName[name]; !ok {
			unknownSec = append(unknownSec, name)
		}
	}
	if len(unknownSec) > 0 {
		return nil, fmt.Errorf("选择的节未注册: [%s]（已注册: [%s]）",
			strings.Join(unknownSec, ", "), strings.Join(sectionNames, ", "))
	}
	secs := allSections
	if len(opts.SectionsSel) > 0 {
		secs = nil
		for _, sec := range allSections {
			if contains(opts.SectionsSel, sec.Name()) {
				secs = append(secs, sec)
			}
		}
	}
	log.Infof("巡检目标: 服务器=%s 命名空间=%s 服务=%s 节=%s",
		serverNames(servers), namespaces, serviceNames(services), sectionNamesOf(secs))

	client, err := mcpclient.New(cfg.ServerURL, cfg.Token, cfg.TLSVerify, cfg.Certfile)
	if err != nil {
		return nil, err
	}
	if err := verifySkills(log, client, cfg.Root, secs); err != nil {
		return nil, err
	}

	// 各节插件展开 tools/call 清单，统一并发执行（最多 8 并发，同 Python ThreadPoolExecutor）
	ctx := &sections.Context{Cfg: cfg, Servers: servers, Namespaces: namespaces, Services: services}
	var calls []sections.ToolCall
	for _, sec := range secs {
		secCalls := sec.BuildCalls(ctx)
		log.Infof("节 %s 展开 %d 个工具调用", sec.Name(), len(secCalls))
		calls = append(calls, secCalls...)
	}
	results := map[string]map[string]any{}
	{
		var wg sync.WaitGroup
		var mu sync.Mutex
		sem := make(chan struct{}, 8)
		for _, call := range calls {
			wg.Add(1)
			go func(c sections.ToolCall) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				res := callTool(log, client, c.Tool, c.Arguments)
				mu.Lock()
				results[c.Key] = res
				mu.Unlock()
			}(call)
		}
		wg.Wait()
	}

	// 各节插件自行判定 + 渲染
	var outputs []*sections.Output
	for _, sec := range secs {
		if out := sec.Process(ctx, results); out != nil {
			outputs = append(outputs, out)
		}
	}

	var itemLevels []string
	for _, out := range outputs {
		itemLevels = append(itemLevels, out.Levels...)
	}
	counters := map[string]int{"ok": 0, "warn": 0, "crit": 0}
	overall := judges.Summarize(itemLevels, counters)
	note := "（无异常项）"
	if overall.Level != judges.LevelOK {
		note = fmt.Sprintf("（%d 项异常 / %d 项注意）", counters["crit"], counters["warn"])
	}
	var bullets []string
	for _, out := range outputs {
		if len(out.Issues) == 0 {
			bullets = append(bullets, out.OKText)
		} else {
			bullets = append(bullets, "⚠️ "+strings.Join(out.Issues, "；"))
		}
	}
	var sectionStats []map[string]any
	for _, out := range outputs {
		stat := map[string]int{"ok": 0, "warn": 0, "crit": 0}
		for _, level := range out.Levels {
			if _, ok := stat[level]; ok {
				stat[level]++
			}
		}
		sectionStats = append(sectionStats, map[string]any{
			"name": out.Name, "ok": stat["ok"], "warn": stat["warn"], "crit": stat["crit"],
			"total": stat["ok"] + stat["warn"] + stat["crit"],
		})
	}

	title := fmt.Sprintf("%d.%d 巡检报告", int(now.Month()), now.Day())
	logDisplay := relDisplay(cfg.Root, logFile)
	var scopeLines []string
	var serverDescs []string
	for _, s := range servers {
		serverDescs = append(serverDescs, fmt.Sprintf("%s（%s）", s.Host, s.Name))
	}
	scopeLines = append(scopeLines, "服务器："+defaultJoin(serverDescs, "—"))
	for _, out := range outputs {
		if out.ScopeText != "" {
			scopeLines = append(scopeLines, out.ScopeText)
		}
	}
	var sectionsMD []string
	for _, out := range outputs {
		sectionsMD = append(sectionsMD, out.Markdown)
	}
	markdown := render.Compose(title, scopeLines,
		render.Overall{Emoji: overall.Emoji, Label: overall.Label, Note: note},
		bullets, sectionsMD, logDisplay)

	reportName := fmt.Sprintf("%d.%d巡检报告.md", int(now.Month()), now.Day())
	target := filepath.Join(cfg.Output.ReportDir, reportName)
	if _, err := os.Stat(target); err == nil {
		reportName = fmt.Sprintf("%d.%d巡检报告_%s.md", int(now.Month()), now.Day(), now.Format("150405"))
		target = filepath.Join(cfg.Output.ReportDir, reportName)
	}
	if err := os.WriteFile(target, []byte(markdown), 0o644); err != nil {
		return nil, fmt.Errorf("报告写入失败: %w", err)
	}
	log.Infof("报告已生成: %s", target)

	summary := map[string]any{
		"title":         title,
		"report_name":   reportName,
		"report_path":   target,
		"download_url":  signurl.DownloadURLFor(cfg.Output.BaseURL, reportName, cfg.Token),
		"log_path":      logFile,
		"log_rel":       logDisplay,
		"checked_at":    now.Format("2006-01-02 15:04:05"),
		"overall":       overall.Label,
		"overall_emoji": overall.Emoji,
		"note":          note,
		"counts":        map[string]int{"ok": counters["ok"], "warn": counters["warn"], "crit": counters["crit"]},
		"bullets":       bulletsOrEmpty(bullets),
		"sections":      outputNames(outputs),
		"section_stats": sectionStatsOrEmpty(sectionStats),
	}
	{
		details := make([]map[string]any, 0, len(outputs))
		for _, out := range outputs {
			details = append(details, map[string]any{
				"name": out.Name, "ok_text": out.OKText, "issues": issuesOrEmpty(out.Issues),
			})
		}
		summary["details"] = details
	}

	summary["database"] = map[string]any{"saved": false}
	if cfg.DatabaseURL != "" {
		if recID, err := store.SaveInspection(cfg.DatabaseURL, cfg.Root, summary, outputs, results); err != nil {
			log.Errorf("巡检入库失败: %s", err)
			summary["database"] = map[string]any{"saved": false, "error": err.Error()}
		} else {
			summary["database"] = map[string]any{"saved": true, "id": recID}
			log.Infof("巡检已入库 id=%s", recID)
		}
	}

	summary["webhooks"] = []map[string]any{}
	if opts.NotifyOn {
		outcomes := notifier.Dispatch(cfg.Channels, summary)
		var hooks []map[string]any
		for _, outcome := range outcomes {
			status := "成功"
			if !outcome.OK {
				status = "失败"
			}
			log.Infof("webhook 推送 %s: %s %s", status, outcome.URL, outcome.Err)
			hooks = append(hooks, map[string]any{
				"url": outcome.URL, "ok": outcome.OK, "error": outcome.Err,
			})
		}
		summary["webhooks"] = hooksOrEmpty(hooks)
	}

	log.Infof("==== 巡检结束: %s %v 报告=%s ====", overall.Label, counters, reportName)
	return summary, nil
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func serverNames(servers []config.ServerCfg) []string {
	var out []string
	for _, s := range servers {
		out = append(out, s.Name)
	}
	return out
}

func serviceNames(services []config.ServiceCfg) []string {
	var out []string
	for _, s := range services {
		out = append(out, s.Name)
	}
	return out
}

func sectionNamesOf(secs []sections.Section) []string {
	var out []string
	for _, s := range secs {
		out = append(out, s.Name())
	}
	return out
}

func outputNames(outputs []*sections.Output) []string {
	out := []string{}
	for _, o := range outputs {
		out = append(out, o.Name)
	}
	return out
}

func relDisplay(root, path string) string {
	if rel, err := filepath.Rel(root, path); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return path
}

func defaultJoin(items []string, def string) string {
	if len(items) == 0 {
		return def
	}
	return strings.Join(items, "、")
}

func bulletsOrEmpty(b []string) []string {
	if b == nil {
		return []string{}
	}
	return b
}

func issuesOrEmpty(issues []string) []string {
	if issues == nil {
		return []string{}
	}
	return issues
}

func sectionStatsOrEmpty(stats []map[string]any) []map[string]any {
	if stats == nil {
		return []map[string]any{}
	}
	return stats
}

func hooksOrEmpty(hooks []map[string]any) []map[string]any {
	if hooks == nil {
		return []map[string]any{}
	}
	return hooks
}

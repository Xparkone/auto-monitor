package sections

// 服务器资源巡检节：CPU / 内存 / 负载 / 进程（含僵尸进程）。

import (
	"fmt"
	"strings"

	"auto-monitor/internal/judges"
	"auto-monitor/internal/jutil"
)

type resourcesSection struct{}

func init() { register(resourcesSection{}) }

func (resourcesSection) Name() string            { return "server-resources" }
func (resourcesSection) SkillFile() string       { return "skills/server-resources.md" }
func (resourcesSection) RequiredTools() []string { return []string{"check_host_resources"} }

func (s resourcesSection) BuildCalls(ctx *Context) []ToolCall {
	calls := make([]ToolCall, 0, len(ctx.Servers))
	for _, srv := range ctx.Servers {
		calls = append(calls, ToolCall{
			Key:       "resources:" + srv.Name,
			Tool:      "check_host_resources",
			Arguments: map[string]any{"host": srv.Host, "via": srv.Via},
		})
	}
	return calls
}

func (s resourcesSection) Process(ctx *Context, results map[string]map[string]any) *Output {
	type row struct {
		name, host string
		res        map[string]any
		verdict    judges.Verdict
	}
	rows := []row{}
	levels := []string{}
	issues := []string{}
	for _, srv := range ctx.Servers {
		res, ok := results["resources:"+srv.Name]
		if !ok || res == nil {
			res = map[string]any{"ok": false, "errors": []string{"未执行"}}
		}
		v := judges.JudgeResources(res, ctx.Th())
		levels = append(levels, v.Level)
		for _, issue := range v.Issues {
			issues = append(issues, srv.Name+" "+issue)
		}
		rows = append(rows, row{srv.Name, srv.Host, res, v})
	}

	lines := []string{"## 服务器资源状态（系统资源巡检）", "",
		"巡检时间：" + ctx.Stamp(), ""}
	if len(issues) > 0 {
		lines = append(lines, "> 结论："+strings.Join(issues, "；"), "")
	} else {
		lines = append(lines, "> 结论：所有服务器资源状态正常，无僵尸进程。", "")
	}
	lines = append(lines,
		"| 服务器 | CPU | 内存 | 负载(1/5/15) | 僵尸进程 | 判定 |",
		"| --- | --- | --- | --- | --- | --- |")
	for _, r := range rows {
		heading := fmt.Sprintf("%s（%s）", r.name, r.host)
		if okVal, _ := jutil.Bool(r.res, "ok"); !okVal {
			lines = append(lines, fmt.Sprintf("| %s | — | — | — | — | %s |",
				heading, JudgeCell(r.verdict.Level, r.verdict.Issues)))
			continue
		}
		loadText := fmt.Sprintf("%s / %s / %s",
			Fmt(maybeFloat(r.res, "load1"), ""), Fmt(maybeFloat(r.res, "load5"), ""), Fmt(maybeFloat(r.res, "load15"), ""))
		zombieText := "无"
		if zombies := jutil.MapSlice(r.res, "zombies"); len(zombies) > 0 {
			zombieText = fmt.Sprintf("⚠️ %d 个", len(zombies))
		}
		lines = append(lines, fmt.Sprintf("| %s | %s | %s | %s | %s | %s |",
			heading,
			Fmt(maybeFloat(r.res, "cpu_percent"), "%"),
			Fmt(maybeFloat(r.res, "mem_percent"), "%"),
			loadText, zombieText,
			JudgeCell(r.verdict.Level, r.verdict.Issues)))
	}
	lines = append(lines, "")
	return &Output{
		Name:      s.Name(),
		Markdown:  strings.Join(lines, "\n"),
		Levels:    levels,
		Issues:    issues,
		ScopeText: "资源：CPU、内存、负载、进程",
		OKText:    "资源状态良好：所有服务器 CPU/内存/负载均正常，无僵尸进程。",
	}
}

// maybeFloat 把 map 中的数值包成 float64（缺失返回 nil），供 Fmt 渲染。
func maybeFloat(m map[string]any, key string) any {
	if v, ok := jutil.Float(m, key); ok {
		return v
	}
	return nil
}

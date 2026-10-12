package sections

// 磁盘巡检节：各服务器指定挂载点（根目录 & 关键数据盘）的使用率。

import (
	"fmt"
	"sort"
	"strings"

	"auto-monitor/internal/judges"
	"auto-monitor/internal/jutil"
)

type disksSection struct{}

func init() { register(disksSection{}) }

func (disksSection) Name() string            { return "disks" }
func (disksSection) SkillFile() string       { return "skills/server-resources.md" } // 与资源节共用同一份规则说明书
func (disksSection) RequiredTools() []string { return []string{"check_disk"} }

func (s disksSection) BuildCalls(ctx *Context) []ToolCall {
	calls := make([]ToolCall, 0, len(ctx.Servers))
	for _, srv := range ctx.Servers {
		mounts := make([]any, 0, len(srv.Mounts))
		for _, m := range srv.Mounts {
			mounts = append(mounts, m)
		}
		calls = append(calls, ToolCall{
			Key:       "disk:" + srv.Name,
			Tool:      "check_disk",
			Arguments: map[string]any{"host": srv.Host, "via": srv.Via, "mounts": mounts},
		})
	}
	return calls
}

func (s disksSection) Process(ctx *Context, results map[string]map[string]any) *Output {
	type row struct {
		server, mount string
		ok            bool
		sizeH, usedH  string
		percent       any
		level         string
		issues        []string
	}
	rows := []row{}
	levels := []string{}
	issues := []string{}
	for _, srv := range ctx.Servers {
		res, ok := results["disk:"+srv.Name]
		if !ok || res == nil {
			res = map[string]any{"ok": false}
		}
		sections := jutil.MapSlice(res, "disks")
		if len(sections) == 0 {
			levels = append(levels, judges.LevelWarn)
			issues = append(issues, srv.Name+" 磁盘巡检执行失败")
			rows = append(rows, row{server: srv.Name, mount: "—", ok: false,
				level: judges.LevelWarn, issues: []string{"磁盘巡检执行失败（详见巡检日志）"}})
			continue
		}
		for _, sec := range sections {
			v := judges.JudgeDisk(sec, ctx.Th())
			levels = append(levels, v.Level)
			for _, issue := range v.Issues {
				issues = append(issues, srv.Name+" "+issue)
			}
			secOK, _ := jutil.Bool(sec, "ok")
			rows = append(rows, row{
				server:  srv.Name,
				mount:   jutil.Str(sec, "mount"),
				ok:      secOK,
				sizeH:   defaultStr(jutil.Str(sec, "size_h"), "—"),
				usedH:   defaultStr(jutil.Str(sec, "used_h"), "—"),
				percent: maybeFloat(sec, "percent"),
				level:   v.Level,
				issues:  v.Issues,
			})
		}
	}

	mountSet := map[string]bool{}
	var mounts []string
	for _, srv := range ctx.Servers {
		for _, m := range srv.Mounts {
			if !mountSet[m] {
				mountSet[m] = true
				mounts = append(mounts, m)
			}
		}
	}
	sort.Strings(mounts)

	lines := []string{"## 磁盘使用状态（根目录 & 关键数据盘）", "",
		"巡检时间：" + ctx.Stamp(), "",
		"| 服务器 | 挂载点 | 总量 | 已用 | 使用率 | 判定 |",
		"| --- | --- | --- | --- | --- | --- |"}
	for _, r := range rows {
		if r.ok {
			lines = append(lines, fmt.Sprintf("| %s | %s | %s | %s | %s | %s |",
				r.server, r.mount, r.sizeH, r.usedH, Fmt(r.percent, "%"), judges.Mark(r.level)))
		} else {
			lines = append(lines, fmt.Sprintf("| %s | %s | — | — | — | %s |",
				r.server, r.mount, JudgeCell(r.level, r.issues)))
		}
	}
	lines = append(lines, "")
	return &Output{
		Name:      s.Name(),
		Markdown:  strings.Join(lines, "\n"),
		Levels:    levels,
		Issues:    issues,
		ScopeText: "磁盘挂载点：" + strings.Join(mounts, "、"),
		OKText:    "磁盘状态良好：各挂载点使用率均低于注意阈值。",
	}
}

func defaultStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

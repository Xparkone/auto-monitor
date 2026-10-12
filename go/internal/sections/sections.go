// Package sections 巡检节（Section）插件协议与注册表（对应 Python 版 mcp_inspect/sections/）。
//
// 新增一个巡检项共三步：
//  1. skills/ 下写规则说明书（文中提及将使用的工具名）；
//  2. 用 tools 包的 Register 注册采集工具；
//  3. 在本包新增 xxx.go，实现 Section 接口并在 init() 中 register ——
//     工作流、报告组装、HTTP 接口均无需改动。
package sections

import (
	"sort"
	"time"

	"auto-monitor/internal/config"
	"auto-monitor/internal/judges"
	"auto-monitor/internal/jutil"
)

// ToolCall 一次 tools/call 的展开结果：Key 是结果字典的键。
type ToolCall struct {
	Key       string
	Tool      string
	Arguments map[string]any
}

// Output 单节判定 + 渲染产物。
type Output struct {
	Name      string
	Markdown  string   // 本节的 Markdown（含标题）
	Levels    []string // 各检查项级别（ok/warn/crit），汇总整体健康度
	Issues    []string // ["对象: 问题"]，异常时用于总体结论 bullet
	ScopeText string   // 巡检范围行，如 "资源：CPU、内存、负载、进程"
	OKText    string   // 全部正常时的结论句
}

// Context 传给插件的上下文：已按 CLI/接口参数过滤好的目标集合。
type Context struct {
	Cfg        *config.Config
	Servers    []config.ServerCfg
	Namespaces []string
	Services   []config.ServiceCfg
}

func (c *Context) Th() config.Thresholds { return c.Cfg.Thresholds }

func (c *Context) Stamp() string { return time.Now().Format("2006-01-02 15:04:05") }

// Section 插件协议：BuildCalls 展开采集调用，Process 判定并产出 Markdown。
type Section interface {
	Name() string
	SkillFile() string       // skills/xxx.md；为空则跳过 skill 校验
	RequiredTools() []string // skill 说明书与 MCP tools/list 都需覆盖这些工具
	BuildCalls(ctx *Context) []ToolCall
	// Process 返回 Output；返回 nil 表示本节无对象（报告与结论都跳过）。
	Process(ctx *Context, results map[string]map[string]any) *Output
}

var registry = map[string]Section{}

// order 报告中的节顺序；新注册的节自动追加在内置节之后。
var order = []string{"server-resources", "disks", "pods", "middleware"}

func register(s Section) { registry[s.Name()] = s }

// All 返回全部已注册节（内置节按固定顺序，扩展节按注册名排序追加）。
func All() []Section {
	known := make([]Section, 0, len(registry))
	seen := map[string]bool{}
	for _, name := range order {
		if s, ok := registry[name]; ok {
			known = append(known, s)
			seen[name] = true
		}
	}
	var extra []string
	for name := range registry {
		if !seen[name] {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	for _, name := range extra {
		known = append(known, registry[name])
	}
	return known
}

// ------------------------------------------------- 各节共用的 Markdown 小工具

// Fmt 格式化表格单元格：缺失值显示 —（对应 Python sections/base.py 的 fmt）。
func Fmt(value any, suffix string) string {
	if value == nil {
		return "—"
	}
	if s := jutil.PyStr(value); s != "" {
		return s + suffix
	}
	return "—"
}

// JudgeCell 判定列：异常级别时把问题跟在标签后面，<br> 分行。
func JudgeCell(level string, issues []string) string {
	if level == judges.LevelOK || len(issues) == 0 {
		return judges.Mark(level)
	}
	return judges.Mark(level) + "<br>" + joinBR(issues)
}

func joinBR(issues []string) string {
	out := ""
	for i, s := range issues {
		if i > 0 {
			out += "<br>"
		}
		out += s
	}
	return out
}

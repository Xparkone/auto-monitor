package sections

// 中间件健康巡检节：部署方式由 probes 注册表驱动，按执行主机分组。

import (
	"fmt"
	"sort"
	"strings"

	"auto-monitor/internal/config"
	"auto-monitor/internal/judges"
	"auto-monitor/internal/jutil"
	"auto-monitor/internal/probes"
)

var deployLabel = map[string]string{"binary": "二进制", "container": "容器", "k8s": "K8s"}

type middlewareSection struct{}

func init() { register(middlewareSection{}) }

func (middlewareSection) Name() string            { return "middleware" }
func (middlewareSection) SkillFile() string       { return "skills/middleware.md" }
func (middlewareSection) RequiredTools() []string { return []string{"check_middleware"} }

// viaFor 按 host 在 servers 配置里查 via，查不到默认 ssh。
func viaFor(ctx *Context, host string) string {
	for _, srv := range ctx.Cfg.Servers {
		if srv.Host == host {
			return srv.Via
		}
	}
	return "ssh"
}

// serviceMap 等价 Python 的 dataclasses.asdict(ServiceCfg)。
func serviceMap(svc config.ServiceCfg) map[string]any {
	return map[string]any{
		"name": svc.Name, "deploy": svc.Deploy, "host": svc.Host, "port": svc.Port,
		"process": svc.Process, "container": svc.Container, "k8s": svc.K8s,
	}
}

func (s middlewareSection) BuildCalls(ctx *Context) []ToolCall {
	groups := map[string][]config.ServiceCfg{}
	var hostOrder []string
	for _, svc := range ctx.Services {
		if _, seen := groups[svc.Host]; !seen {
			hostOrder = append(hostOrder, svc.Host)
		}
		groups[svc.Host] = append(groups[svc.Host], svc)
	}
	calls := make([]ToolCall, 0, len(groups))
	for _, host := range hostOrder {
		svcs := groups[host]
		svcMaps := make([]any, 0, len(svcs))
		for _, svc := range svcs {
			svcMaps = append(svcMaps, serviceMap(svc))
		}
		calls = append(calls, ToolCall{
			Key:  "middleware:" + host,
			Tool: "check_middleware",
			Arguments: map[string]any{
				"host": host, "via": viaFor(ctx, host), "services": svcMaps,
			},
		})
	}
	return calls
}

func (s middlewareSection) Process(ctx *Context, results map[string]map[string]any) *Output {
	if len(ctx.Services) == 0 {
		return nil
	}
	var keys []string
	for key := range results {
		if strings.HasPrefix(key, "middleware:") {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)

	levels := []string{}
	issues := []string{}
	type group struct {
		host string
		rows []entryRow
	}
	groups := []group{}
	for _, key := range keys {
		host := strings.TrimPrefix(key, "middleware:")
		g := group{host: host}
		for _, entry := range jutil.MapSlice(results[key], "results") {
			v := judges.JudgeMiddleware(entry)
			levels = append(levels, v.Level)
			for _, issue := range v.Issues {
				issues = append(issues, fmt.Sprintf("%s: %s", jutil.Str(entry, "name"), issue))
			}
			g.rows = append(g.rows, entryRow{entry, v})
		}
		groups = append(groups, g)
	}

	deployTypes := strings.Join(probes.Registered(), " / ")
	lines := []string{fmt.Sprintf("## 中间件服务健康状态（%s）", deployTypes), "",
		"巡检时间：" + ctx.Stamp(), ""}
	for i, g := range groups {
		lines = append(lines,
			fmt.Sprintf("### 3.%d %s", i+1, g.host), "",
			"| 服务 | 部署方式 | 端口 | 状态明细 | 判定 |",
			"| --- | --- | --- | --- | --- |")
		for _, r := range g.rows {
			markText := "❌"
			if open, _ := jutil.Bool(r.entry, "port_open"); open {
				markText = "✅"
			}
			portText := fmt.Sprintf("%s %s", jutil.PyStr(portAny(r.entry)), markText)
			label := deployLabel[jutil.Str(r.entry, "deploy")]
			if label == "" {
				label = jutil.Str(r.entry, "deploy")
			}
			detail := defaultStr(jutil.Str(r.entry, "detail"), "—")
			lines = append(lines, fmt.Sprintf("| %s | %s | %s | %s | %s |",
				jutil.Str(r.entry, "name"), label, portText, detail,
				JudgeCell(r.verdict.Level, r.verdict.Issues)))
		}
		lines = append(lines, "")
	}

	names := make([]string, 0, len(ctx.Services))
	for _, svc := range ctx.Services {
		names = append(names, svc.Name)
	}
	return &Output{
		Name:      s.Name(),
		Markdown:  strings.Join(lines, "\n"),
		Levels:    levels,
		Issues:    issues,
		ScopeText: "服务：" + strings.Join(names, "、"),
		OKText:    fmt.Sprintf("中间件服务健康：%d 个服务全部正常。", len(ctx.Services)),
	}
}

type entryRow struct {
	entry   map[string]any
	verdict judges.Verdict
}

// portAny 取 entry 的 port 字段；缺失返回 nil。
// 端口语义上是整数：JSON 往返会把它变成 float64（如 8765.0），渲染时归一为 int。
func portAny(entry map[string]any) any {
	v, ok := entry["port"]
	if !ok || v == nil {
		return nil
	}
	if i, isInt := jutil.Int(entry, "port"); isInt {
		return i
	}
	return v
}

package sections

// K8s Pod 健康巡检节：命名空间可多选；kubectl 不可用时降级为说明。

import (
	"fmt"
	"strings"

	"auto-monitor/internal/judges"
	"auto-monitor/internal/jutil"
)

type podsSection struct{}

func init() { register(podsSection{}) }

func (podsSection) Name() string            { return "pods" }
func (podsSection) SkillFile() string       { return "skills/k8s-pods.md" }
func (podsSection) RequiredTools() []string { return []string{"check_pods"} }

func (s podsSection) BuildCalls(ctx *Context) []ToolCall {
	if len(ctx.Namespaces) == 0 {
		return nil
	}
	namespaces := make([]any, 0, len(ctx.Namespaces))
	for _, ns := range ctx.Namespaces {
		namespaces = append(namespaces, ns)
	}
	return []ToolCall{{Key: "pods", Tool: "check_pods", Arguments: map[string]any{"namespaces": namespaces}}}
}

func (s podsSection) Process(ctx *Context, results map[string]map[string]any) *Output {
	if len(ctx.Namespaces) == 0 {
		return nil // 未配置命名空间：报告与结论都跳过本节
	}
	res, ok := results["pods"]
	if !ok || res == nil {
		res = map[string]any{"available": false,
			"errors": []any{map[string]any{"error": "未执行"}}}
	}
	nsText := strings.Join(ctx.Namespaces, "、")
	header := []string{"## K8s Pod 健康状态", "",
		"巡检时间：" + ctx.Stamp(), "",
		"命名空间：" + nsText, ""}

	if available, _ := jutil.Bool(res, "available"); !available {
		var errTexts []string
		for _, e := range jutil.MapSlice(res, "errors") {
			errTexts = append(errTexts, jutil.Str(e, "error"))
		}
		reason := strings.Join(errTexts, "；")
		if reason == "" {
			reason = "kubectl 不可用"
		}
		header = append(header,
			fmt.Sprintf("> ⚠️ K8s 巡检不可用：%s（详见巡检日志）", reason), "")
		return &Output{
			Name:      s.Name(),
			Markdown:  strings.Join(header, "\n"),
			Levels:    []string{judges.LevelWarn},
			Issues:    []string{"K8s 巡检不可用：" + reason},
			ScopeText: "命名空间：" + nsText,
		}
	}

	levels := []string{}
	issues := []string{}
	type podRow struct {
		pod     map[string]any
		verdict judges.Verdict
	}
	rows := []podRow{}
	for _, pod := range jutil.MapSlice(res, "pods") {
		v := judges.JudgePod(pod, ctx.Th())
		levels = append(levels, v.Level)
		for _, issue := range v.Issues {
			issues = append(issues, fmt.Sprintf("%s/%s: %s",
				jutil.Str(pod, "namespace"), jutil.Str(pod, "name"), issue))
		}
		rows = append(rows, podRow{pod, v})
	}

	header = append(header,
		"| 命名空间 | Pod | 状态 | Ready | 重启 | 判定 |",
		"| --- | --- | --- | --- | --- | --- |")
	for _, r := range rows {
		restarts, _ := jutil.Int(r.pod, "restarts")
		header = append(header, fmt.Sprintf("| %s | %s | %s | %s | %d | %s |",
			jutil.Str(r.pod, "namespace"), jutil.Str(r.pod, "name"), jutil.Str(r.pod, "phase"),
			jutil.Str(r.pod, "ready"), restarts, JudgeCell(r.verdict.Level, r.verdict.Issues)))
	}
	header = append(header, "")
	return &Output{
		Name:      s.Name(),
		Markdown:  strings.Join(header, "\n"),
		Levels:    levels,
		Issues:    issues,
		ScopeText: "命名空间：" + nsText,
		OKText:    fmt.Sprintf("K8s 集群稳定：%d 个 Pod 状态正常。", len(rows)),
	}
}

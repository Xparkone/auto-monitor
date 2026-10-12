// Package notifier Webhook 通知：渠道注册表驱动，内置飞书消息卡片、企业微信 markdown 与通用 JSON
// （对应 Python 版 notifier.py）。
//
// 扩展新渠道（如钉钉）：
//  1. 用 RegisterChannel("dingtalk") 注册一个 summary -> payload 构造函数；
//  2. 在 config/inspection.toml 的 [webhook.channels] 配同名 key = "url"。
//
// 通知失败只记录不影响巡检结果；所有异常收敛为返回值。
package notifier

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"auto-monitor/internal/config"
	"auto-monitor/internal/jutil"
)

type ChannelFn func(summary map[string]any) any

var channels = map[string]ChannelFn{}

// RegisterChannel 注册一个通知渠道的消息构造函数。
func RegisterChannel(name string, fn ChannelFn) { channels[name] = fn }

// Registered 返回已注册渠道名（排序）。
func Registered() []string {
	names := make([]string, 0, len(channels))
	for name := range channels {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

var (
	templateByEmoji = map[string]string{"🟢": "green", "🟡": "orange", "🔴": "red"}
	tagColorByEmoji = map[string]string{"🟢": "green", "🟡": "orange", "🔴": "red"}
	// 企业微信 markdown 只支持 info / comment / warning 三种颜色
	wecomColorByEmoji = map[string]string{"🟢": "info", "🟡": "warning", "🔴": "warning"}
	sectionLabel      = map[string]string{
		"server-resources": "服务器资源",
		"disks":            "磁盘",
		"pods":             "K8s Pod",
		"middleware":       "中间件",
	}
	// 这两节用和总览相同的三格数字，不用柱状图。数量为 0 时用灰色，避免把「没有异常」画成红色。
	countSections = []string{"pods", "middleware"}
)

func metricColumn(value any, caption, color string) map[string]any {
	return map[string]any{
		"tag":            "column",
		"width":          "weighted",
		"weight":         1,
		"vertical_align": "center",
		"elements": []any{map[string]any{
			"tag":        "markdown",
			"text_align": "center",
			"content":    fmt.Sprintf("<font color='%s'>**%v**</font>\n<font color='grey'>%s</font>", color, value, caption),
		}},
	}
}

func countColor(value int, color string) string {
	if value == 0 {
		return "grey"
	}
	return color
}

func metricRow(stats map[string]any) map[string]any {
	okCount, _ := jutil.Int(stats, "ok")
	warnCount, _ := jutil.Int(stats, "warn")
	critCount, _ := jutil.Int(stats, "crit")
	return map[string]any{
		"tag":                "column_set",
		"flex_mode":          "trisect",
		"horizontal_spacing": "small",
		"columns": []any{
			metricColumn(okCount, "正常", countColor(okCount, "green")),
			metricColumn(warnCount, "注意", countColor(warnCount, "orange")),
			metricColumn(critCount, "异常", countColor(critCount, "red")),
		},
	}
}

func sectionCountElements(summary map[string]any) []any {
	byName := map[string]map[string]any{}
	for _, item := range jutil.MapSlice(summary, "section_stats") {
		byName[jutil.Str(item, "name")] = item
	}
	var elements []any
	for _, name := range countSections {
		stats, ok := byName[name]
		if !ok {
			continue
		}
		total, _ := jutil.Int(stats, "total")
		if total == 0 {
			continue
		}
		title := sectionLabel[name]
		if title == "" {
			title = name
		}
		elements = append(elements,
			map[string]any{"tag": "markdown", "content": "**" + title + "**"},
			metricRow(stats))
	}
	return elements
}

// conclusionMD 每条问题单独一行。有明细时按主机/挂载点拆开，不再用分号接成一句。
func conclusionMD(summary map[string]any) string {
	details := jutil.MapSlice(summary, "details")
	lines := []string{"**巡检结论**", ""}
	if len(details) == 0 {
		bullets := jutil.Slice(summary, "bullets")
		if len(bullets) == 0 {
			lines = append(lines, "本次没有产出巡检节。")
		}
		for _, bullet := range bullets {
			text := strings.TrimSpace(strings.TrimPrefix(fmt.Sprint(bullet), "⚠️ "))
			lines = append(lines, "- "+text)
		}
		return strings.TrimRight(strings.Join(lines, "\n"), "\n ")
	}
	for _, item := range details {
		heading := sectionLabel[jutil.Str(item, "name")]
		if heading == "" {
			heading = jutil.Str(item, "name")
		}
		if heading != "" {
			lines = append(lines, "**"+heading+"**")
		}
		var issues []string
		for _, issue := range jutil.Slice(item, "issues") {
			if text := strings.TrimSpace(fmt.Sprint(issue)); text != "" {
				issues = append(issues, text)
			}
		}
		if len(issues) == 0 {
			okText := jutil.Str(item, "ok_text")
			if okText == "" {
				okText = "正常"
			}
			lines = append(lines, "- "+okText)
		} else {
			shown := issues
			if len(shown) > 6 {
				shown = shown[:6]
			}
			for _, issue := range shown {
				lines = append(lines, "- "+issue)
			}
			if rest := len(issues) - len(shown); rest > 0 {
				lines = append(lines, fmt.Sprintf("- 其余 %d 条见巡检报告", rest))
			}
		}
		lines = append(lines, "")
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n ")
}

// BuildFeishuCard 飞书消息卡片：标题 + 状态标签、三项计数、分节结论、下载按钮。
func BuildFeishuCard(summary map[string]any) map[string]any {
	emoji := defaultS(jutil.Str(summary, "overall_emoji"), "🟢")
	label := defaultS(jutil.Str(summary, "overall"), "未知")
	title := defaultS(jutil.Str(summary, "title"), "巡检报告")
	tagColor := tagColorByEmoji[emoji]
	if tagColor == "" {
		tagColor = "blue"
	}
	counts := jutil.Map(summary, "counts")
	checkedAt := jutil.Str(summary, "checked_at")
	titleLine := fmt.Sprintf("<font color='blue'>**%s**</font> <text_tag color='%s'>%s</text_tag>", title, tagColor, label)
	headline := titleLine
	if checkedAt != "" {
		headline = titleLine + "\n" + fmt.Sprintf("<font color='grey'>%s</font>", checkedAt)
	}

	elements := []any{
		map[string]any{"tag": "div", "text": map[string]any{"tag": "lark_md", "content": headline}},
		map[string]any{
			"tag":                "column_set",
			"flex_mode":          "trisect",
			"background_style":   "grey",
			"horizontal_spacing": "small",
			"columns": []any{
				metricColumn(intKey(counts, "ok"), "正常", "green"),
				metricColumn(intKey(counts, "warn"), "注意", "orange"),
				metricColumn(intKey(counts, "crit"), "异常", "red"),
			},
		},
		map[string]any{"tag": "hr"},
	}
	elements = append(elements, sectionCountElements(summary)...)
	elements = append(elements, map[string]any{"tag": "markdown", "content": conclusionMD(summary)})

	logRel := jutil.Str(summary, "log_rel")
	if logRel == "" {
		logRel = jutil.Str(summary, "log_path")
	}
	if logRel != "" {
		elements = append(elements, map[string]any{
			"tag":      "note",
			"elements": []any{map[string]any{"tag": "plain_text", "content": "日志 " + logRel}},
		})
	}
	if download := jutil.Str(summary, "download_url"); download != "" {
		elements = append(elements, map[string]any{
			"tag": "action",
			"actions": []any{map[string]any{
				"tag":  "button",
				"text": map[string]any{"tag": "plain_text", "content": "下载巡检报告"},
				"url":  download,
				"type": "primary",
			}},
		})
	}
	elements = append(elements, map[string]any{"tag": "markdown", "content": titleLine})

	template := templateByEmoji[emoji]
	if template == "" {
		template = "blue"
	}
	return map[string]any{
		"msg_type": "interactive",
		"card": map[string]any{
			"config": map[string]any{"wide_screen_mode": true},
			"header": map[string]any{
				"template": template,
				"title":    map[string]any{"tag": "plain_text", "content": emoji + " " + title},
			},
			"elements": elements,
		},
	}
}

func wecomPayload(summary map[string]any) map[string]any {
	emoji := jutil.Str(summary, "overall_emoji")
	color := wecomColorByEmoji[emoji]
	if color == "" {
		color = "comment"
	}
	title := defaultS(jutil.Str(summary, "title"), "巡检报告")
	label := jutil.Str(summary, "overall")
	lines := []string{
		fmt.Sprintf("## <font color=\"%s\">%s %s：%s</font>", color, emoji, title, label),
		jutil.Str(summary, "note"),
		"",
		conclusionMD(summary),
	}
	if download := jutil.Str(summary, "download_url"); download != "" {
		lines = append(lines, "", fmt.Sprintf("[下载巡检报告](%s)", download))
	}
	return map[string]any{
		"msgtype":  "markdown",
		"markdown": map[string]any{"content": strings.Join(lines, "\n")},
	}
}

func init() {
	RegisterChannel("feishu", func(summary map[string]any) any { return BuildFeishuCard(summary) })
	RegisterChannel("wecom", func(summary map[string]any) any { return wecomPayload(summary) })
	RegisterChannel("generic", func(summary map[string]any) any { return summary })
}

// Outcome 单渠道推送结果。
type Outcome struct {
	URL string
	OK  bool
	Err string
}

// PostJSON POST 一个 JSON body，返回状态码。
func PostJSON(url string, payload any, timeoutSec float64) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: time.Duration(timeoutSec * float64(time.Second))}
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// Dispatch 按配置逐渠道推送（保持配置顺序）。
func Dispatch(channelList []config.WebhookChannel, summary map[string]any) []Outcome {
	outcomes := []Outcome{}
	for _, ch := range channelList {
		builder, ok := channels[ch.Name]
		if !ok {
			outcomes = append(outcomes, Outcome{URL: ch.Name, OK: false,
				Err: fmt.Sprintf("未注册的渠道类型: %s（已注册: %s）", ch.Name, Registered())})
			continue
		}
		payload := builder(summary)
		for _, url := range ch.URLs {
			if err := PostJSON(url, payload, 5); err != nil {
				outcomes = append(outcomes, Outcome{URL: url, OK: false, Err: err.Error()})
			} else {
				outcomes = append(outcomes, Outcome{URL: url, OK: true})
			}
		}
	}
	return outcomes
}

func intKey(m map[string]any, key string) int {
	v, _ := jutil.Int(m, key)
	return v
}

func defaultS(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

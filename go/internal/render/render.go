// Package render Markdown 巡检报告组装器（对应 Python 版 report_renderer.py）。
//
// 各节内容由 sections 插件自渲染并拼接，扩展新巡检项无需修改本文件。
package render

import "strings"

type Overall struct {
	Emoji string
	Label string
	Note  string
}

// Compose 组装报告骨架：标题 / 巡检范围 / 总体结论 / 各节 / 日志位置。
func Compose(title string, scopeLines []string, overall Overall, bullets []string,
	sectionsMD []string, logFile string) string {
	var lines []string
	add := func(s string) { lines = append(lines, s) }

	add("# " + title)
	add("")
	add("巡检范围：")
	add("")
	for _, line := range scopeLines {
		add("- " + line)
	}
	add("")
	add("---")
	add("")

	add("## 总体结论")
	add("")
	add("- 整体健康度：" + overall.Emoji + " " + overall.Label + overall.Note)
	add("")
	for _, bullet := range bullets {
		add("- " + bullet)
	}
	add("")
	add("---")
	add("")

	for _, md := range sectionsMD {
		add(strings.TrimRight(md, "\n"))
		add("")
	}

	add("---")
	add("")
	add("# 日志文件位置")
	add("")
	add("`" + logFile + "`")
	add("")
	return strings.Join(lines, "\n")
}

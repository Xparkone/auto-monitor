// auto-monitor 自动巡检平台命令行：
//
//	auto-monitor init [dir] [--force]            在指定目录生成运行所需文件（配置、skills、目录骨架）
//	auto-monitor server [-c config] [-host H] [-port P]   启动巡检 MCP 服务（工具宿主 + 报告接口）
//	auto-monitor run    [-c config] [选择参数...]         执行一次巡检并产出报告/日志/通知
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"auto-monitor/internal/config"
	"auto-monitor/internal/mcpserver"
	"auto-monitor/internal/probes"
	"auto-monitor/internal/runner"
	"auto-monitor/internal/scaffold"
)

const defaultConfig = "config/inspection.toml"

func usage() {
	fmt.Fprintf(os.Stderr, `自动巡检平台

用法:
  auto-monitor init [dir] [--force]              在指定目录初始化（生成 config/skills/reports/logs/data）
  auto-monitor server [-c 配置文件] [-host H] [-port P]  启动巡检 MCP 服务（默认 127.0.0.1:8765）
  auto-monitor run [-c 配置文件] [--servers a,b] [--namespaces x,y]
                   [--services A,B] [--sections a,b] [--no-notify] [--json]  执行巡检

定时自动化交给系统 cron，示例（每日 02:00）：
  0 2 * * * cd /path/to/dir && auto-monitor run >> logs/cron.log 2>&1
`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "init":
		cmdInit(os.Args[2:])
	case "server":
		cmdServer(os.Args[2:])
	case "run":
		cmdRun(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: %s\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func cmdInit(args []string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	force := fs.Bool("force", false, "覆盖已存在的文件")
	_ = fs.Parse(args)
	dir := "."
	if fs.NArg() > 0 {
		dir = fs.Arg(0)
	}
	if err := scaffold.Init(dir, *force, func(msg string) { fmt.Println(msg) }); err != nil {
		fmt.Fprintf(os.Stderr, "初始化失败: %s\n", err)
		os.Exit(1)
	}
}

func cmdServer(args []string) {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	configPath := fs.String("c", defaultConfig, "配置文件路径")
	host := fs.String("host", "127.0.0.1", "监听地址")
	port := fs.Int("port", 8765, "监听端口")
	_ = fs.Parse(args)
	if err := mcpserver.Run(*configPath, *host, *port); err != nil {
		fmt.Fprintf(os.Stderr, "服务退出: %s\n", err)
		os.Exit(1)
	}
}

func splitSel(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func cmdRun(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	configPath := fs.String("c", defaultConfig, "配置文件路径")
	servers := fs.String("servers", "", "服务器多选（名称或 IP，逗号分隔），不传为全量")
	namespaces := fs.String("namespaces", "", "命名空间多选（逗号分隔），不传为全量")
	services := fs.String("services", "", "中间件服务多选（名称，逗号分隔），不传为全量")
	secs := fs.String("sections", "", "巡检节多选（如 server-resources,pods），不传为全部已注册节")
	noNotify := fs.Bool("no-notify", false, "不推送 webhook")
	asJSON := fs.Bool("json", false, "仅输出 JSON 摘要（供 /inspect 接口使用）")
	_ = fs.Parse(args)

	cfg, err := config.Load(*configPath, probes.Valid)
	if err != nil {
		fmt.Fprintf(os.Stderr, "巡检失败: %s\n", err)
		os.Exit(1)
	}
	summary, err := runner.RunInspection(cfg, runner.Options{
		ServersSel:    splitSel(*servers),
		NamespacesSel: splitSel(*namespaces),
		ServicesSel:   splitSel(*services),
		SectionsSel:   splitSel(*secs),
		NotifyOn:      !*noNotify,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "巡检失败: %s\n", err)
		os.Exit(1)
	}

	if *asJSON {
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(summary)
		fmt.Println(strings.TrimRight(b.String(), "\n"))
		return
	}

	counts := summary["counts"]
	fmt.Printf("%s %s：%s {'ok': %d, 'warn': %d, 'crit': %d}\n",
		strOf(summary, "overall_emoji"), strOf(summary, "title"), strOf(summary, "overall"),
		intFromMap(counts, "ok"), intFromMap(counts, "warn"), intFromMap(counts, "crit"))
	for _, bullet := range anySlice(summary["bullets"]) {
		fmt.Printf("  - %v\n", bullet)
	}
	fmt.Printf("报告: %s\n", strOf(summary, "report_path"))
	fmt.Printf("下载: %s\n", strOf(summary, "download_url"))
	fmt.Printf("日志: %s\n", strOf(summary, "log_path"))
	db := mapOfAny(summary, "database")
	if saved, _ := db["saved"].(bool); saved {
		fmt.Printf("入库: %s\n", strOf(db, "id"))
	} else if e := strOf(db, "error"); e != "" {
		fmt.Printf("入库失败: %s\n", e)
	}
	for _, hook := range anySlice(summary["webhooks"]) {
		if m, ok := hook.(map[string]any); ok {
			status := "成功"
			if okVal, _ := m["ok"].(bool); !okVal {
				status = "失败"
			}
			errText := strOf(m, "error")
			if errText != "" {
				errText = " " + errText
			}
			fmt.Printf("webhook %s: %s%s\n", status, strOf(m, "url"), errText)
		}
	}
}

func strOf(m map[string]any, key string) string {
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

func mapOfAny(m map[string]any, key string) map[string]any {
	if v, ok := m[key].(map[string]any); ok {
		return v
	}
	return map[string]any{}
}

// intFromMap 从 map[string]int / map[string]any 里读取整数字段。
func intFromMap(m any, key string) int {
	var v any
	switch typed := m.(type) {
	case map[string]int:
		return typed[key]
	case map[string]any:
		v = typed[key]
	default:
		return 0
	}
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	}
	return 0
}

// anySlice 兼容 []any、[]string、[]map[string]any 的列表值。
func anySlice(v any) []any {
	switch items := v.(type) {
	case []any:
		return items
	case []string:
		out := make([]any, len(items))
		for i, s := range items {
			out[i] = s
		}
		return out
	case []map[string]any:
		out := make([]any, len(items))
		for i, m := range items {
			out[i] = m
		}
		return out
	}
	return nil
}

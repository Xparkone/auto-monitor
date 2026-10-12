// Package tools 内置 MCP 工具（对应 Python 版 mcp_inspect/tools.py）。
//
// 工具只负责采集并返回结构化 JSON；阈值判断在工作流侧（sections + judges）。
// 每个结果都携带 commands（在目标机上执行过的命令）和 errors，供巡检日志排查。
package tools

import (
	"fmt"
	"strconv"
	"strings"

	"auto-monitor/internal/collectors"
	"auto-monitor/internal/jutil"
	"auto-monitor/internal/probes"
	"auto-monitor/internal/registry"
)

func init() {
	registry.Register("check_host_resources", "巡检单台服务器的 CPU、内存、负载、进程（含僵尸进程）",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"host": map[string]any{"type": "string", "description": "目标主机地址"},
				"via":  map[string]any{"type": "string", "enum": []string{"local", "ssh"}, "description": "采集方式，默认 ssh"},
			},
			"required": []string{"host"},
		}, checkHostResources)

	registry.Register("check_disk", "巡检单台服务器指定挂载点的磁盘使用率",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"host":   map[string]any{"type": "string"},
				"via":    map[string]any{"type": "string", "enum": []string{"local", "ssh"}},
				"mounts": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			},
			"required": []string{"host", "mounts"},
		}, checkDisk)

	registry.Register("check_pods", "巡检指定命名空间（可多选）下所有 Pod 的健康度（需在控制机配置 kubectl）",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"namespaces": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			},
			"required": []string{"namespaces"},
		}, checkPods)

	registry.Register("check_middleware", "巡检中间件服务健康度，部署方式由 probes 注册表驱动（binary / container / k8s，可自行扩展）",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"host":     map[string]any{"type": "string", "description": "远端执行部署检查的主机"},
				"via":      map[string]any{"type": "string", "enum": []string{"local", "ssh"}},
				"services": map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
			},
			"required": []string{"host", "services"},
		}, checkMiddleware)
}

func defaultVia(v string) string {
	if v == "" {
		return "ssh"
	}
	return v
}

func checkHostResources(args map[string]any) map[string]any {
	host := jutil.Str(args, "host")
	via := defaultVia(jutil.Str(args, "via"))
	result := map[string]any{
		"tool": "check_host_resources", "host": host, "via": via,
		"ok": false, "commands": []string{}, "errors": []string{},
	}
	commands := result["commands"].([]string)
	errors := result["errors"].([]string)

	execOn := func(cmd string) collectors.ExecResult {
		res := collectors.Run(cmd, via, host, 20)
		commands = append(commands, res.Command)
		return res
	}

	osRes := execOn("uname -s")
	if !osRes.OK {
		detail := strings.TrimSpace(osRes.Err + " " + strings.TrimSpace(osRes.Stderr))
		errors = append(errors, fmt.Sprintf("主机不可达或命令失败: %s", detail))
		result["commands"], result["errors"] = commands, errors
		return result
	}
	osName := strings.TrimSpace(osRes.Stdout)
	result["os"] = osName

	// CPU：Linux 用 /proc/stat 差值，macOS 用 top 第二次采样
	var cpuRes collectors.ExecResult
	var percent float64
	var ok bool
	if osName == "Darwin" {
		cpuRes = execOn("top -l 2 -n 0 -s 1 | grep 'CPU usage' | tail -1")
		percent, ok = collectors.ParseCPUDarwin(cpuRes.Stdout)
	} else {
		cpuRes = execOn(`c1=$(grep '^cpu ' /proc/stat); sleep 1; ` +
			`c2=$(grep '^cpu ' /proc/stat); printf '%s\n%s\n' "$c1" "$c2"`)
		percent, ok = collectors.ParseCPULinux(cpuRes.Stdout)
	}
	if ok {
		result["cpu_percent"] = percent
	} else if !cpuRes.OK {
		errors = append(errors, fmt.Sprintf("CPU 采集失败: %s", cpuRes.Err))
	}

	// 内存
	memCmd := "free -m"
	if osName == "Darwin" {
		memCmd = "sysctl -n hw.memsize vm.pagesize && vm_stat"
	}
	memRes := execOn(memCmd)
	var mem *collectors.MemInfo
	if osName == "Darwin" {
		mem = collectors.ParseMemDarwin(memRes.Stdout)
	} else {
		mem = collectors.ParseMemLinux(memRes.Stdout)
	}
	if mem != nil {
		result["mem_total_mb"] = mem.TotalMB
		result["mem_used_mb"] = mem.UsedMB
		result["mem_percent"] = mem.Percent
	} else if !memRes.OK {
		errors = append(errors, fmt.Sprintf("内存采集失败: %s", memRes.Err))
	}

	// 负载与核数
	upRes := execOn("uptime")
	if load := collectors.ParseLoad(upRes.Stdout); load != nil {
		result["load1"] = load["load1"]
		result["load5"] = load["load5"]
		result["load15"] = load["load15"]
	}
	coresRes := execOn("nproc 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null")
	if lines := nonEmptyLines(coresRes.Stdout); len(lines) > 0 {
		if n, err := strconv.Atoi(lines[len(lines)-1]); err == nil {
			result["cores"] = n
		}
	}

	// 进程：僵尸进程 + CPU Top5
	psRes := execOn("ps -eo pid=,pcpu=,pmem=,state=,comm=")
	if psRes.OK {
		zombies, top := collectors.ParsePS(psRes.Stdout)
		zombieMaps := make([]map[string]any, 0, len(zombies))
		for _, z := range zombies {
			zombieMaps = append(zombieMaps, map[string]any{"pid": z.PID, "comm": z.Comm})
		}
		topMaps := make([]map[string]any, 0, len(top))
		for _, p := range top {
			topMaps = append(topMaps, map[string]any{"pid": p.PID, "cpu": p.CPU, "mem": p.Mem, "comm": p.Comm})
		}
		result["zombies"] = zombieMaps
		result["top_processes"] = topMaps
	} else {
		errors = append(errors, fmt.Sprintf("进程采集失败: %s", psRes.Err))
	}

	result["ok"] = len(errors) == 0
	result["commands"], result["errors"] = commands, errors
	return result
}

func checkDisk(args map[string]any) map[string]any {
	host := jutil.Str(args, "host")
	via := defaultVia(jutil.Str(args, "via"))
	mounts := jutil.StringSlice(args, "mounts")
	if len(mounts) == 0 {
		mounts = []string{"/"}
	}
	var parts []string
	for _, m := range mounts {
		parts = append(parts, fmt.Sprintf(`echo "== %s"; df -k -P %s 2>&1 | tail -1`, m, collectors.ShellQuote(m)))
	}
	res := collectors.Run(strings.Join(parts, "; "), via, host, 20)
	result := map[string]any{
		"tool": "check_disk", "host": host, "ok": true,
		"commands": []string{res.Command}, "errors": []string{},
	}
	errors := result["errors"].([]string)
	sections := collectors.ParseDFSections(res.Stdout)
	for _, sec := range sections {
		if secOK, _ := sec["ok"].(bool); secOK {
			sizeKB, _ := jutil.Float(sec, "size_kb")
			usedKB, _ := jutil.Float(sec, "used_kb")
			sec["size_h"] = collectors.HumanGB(sizeKB)
			sec["used_h"] = collectors.HumanGB(usedKB)
			delete(sec, "size_kb")
			delete(sec, "used_kb")
			delete(sec, "avail_kb")
		} else {
			detail := jutil.Str(sec, "error")
			if detail == "" {
				detail = "df 失败"
			}
			errors = append(errors, fmt.Sprintf("%s: %s", jutil.Str(sec, "mount"), detail))
		}
	}
	result["disks"] = sections
	result["ok"] = res.OK
	if !res.OK {
		detail := strings.TrimSpace(res.Err + " " + strings.TrimSpace(res.Stderr))
		errors = append(errors, fmt.Sprintf("df 执行失败: %s", detail))
	}
	result["errors"] = errors
	return result
}

func checkPods(args map[string]any) map[string]any {
	namespaces := jutil.StringSlice(args, "namespaces")
	result := map[string]any{
		"tool": "check_pods", "namespaces": namespaces,
		"available": false, "pods": []map[string]any{},
		"commands": []string{}, "errors": []map[string]string{},
	}
	commands := result["commands"].([]string)
	errors := result["errors"].([]map[string]string)
	pods := result["pods"].([]map[string]any)
	for _, ns := range namespaces {
		res := collectors.Run(fmt.Sprintf("kubectl -n %s get pods -o json", collectors.ShellQuote(ns)), "local", "", 30)
		commands = append(commands, res.Command)
		if !res.OK {
			detail := res.Stderr
			if detail == "" {
				detail = res.Err
			}
			first := firstLine(detail)
			if first == "" {
				first = "kubectl 调用失败"
			}
			errors = append(errors, map[string]string{"namespace": ns, "error": first})
			continue
		}
		parsed, err := collectors.ParsePodsJSON(res.Stdout)
		if err != nil {
			errors = append(errors, map[string]string{"namespace": ns, "error": fmt.Sprintf("JSON 解析失败: %s", err)})
			continue
		}
		pods = append(pods, parsed...)
		result["available"] = true
	}
	result["commands"], result["errors"], result["pods"] = commands, errors, pods
	return result
}

func checkMiddleware(args map[string]any) map[string]any {
	host := jutil.Str(args, "host")
	via := defaultVia(jutil.Str(args, "via"))
	result := map[string]any{
		"tool": "check_middleware", "host": host, "ok": true,
		"results": []map[string]any{}, "commands": []string{}, "errors": []string{},
	}
	commands := result["commands"].([]string)
	entries := result["results"].([]map[string]any)
	for _, svc := range jutil.MapSlice(args, "services") {
		name := jutil.Str(svc, "name")
		if name == "" {
			name = "?"
		}
		deploy := jutil.Str(svc, "deploy")
		if deploy == "" {
			deploy = "binary"
		}
		svcHost := jutil.Str(svc, "host")
		if svcHost == "" {
			svcHost = host
		}
		port, _ := jutil.Int(svc, "port")
		entry := map[string]any{
			"name": name, "deploy": deploy, "host": svcHost,
			"port": port, "port_open": nil, "alive": nil, "detail": "",
		}

		portRes := collectors.CheckPort(svcHost, port, 3)
		entry["port_open"] = portRes.OK
		commands = append(commands, portRes.Command)
		if !portRes.OK {
			entry["detail"] = fmt.Sprintf("端口不可达: %s", portRes.Err)
		}

		var probe probes.Result
		if probeFn := probes.Get(deploy); probeFn == nil {
			probe = probes.Result{Alive: nil,
				Detail: fmt.Sprintf("未注册的部署类型: %s（已注册: %s）", deploy, probes.Registered())}
		} else {
			probe = probeFn(svc, host, via)
		}
		if probe.Command != "" {
			commands = append(commands, probe.Command)
		}
		if probe.Alive == nil {
			entry["detail"] = joinDetail(jutil.Str(entry, "detail"), probe.Detail)
		} else {
			portOpen, _ := entry["port_open"].(bool)
			entry["alive"] = *probe.Alive && portOpen
			if portOpen {
				entry["detail"] = probe.Detail
			} else {
				entry["detail"] = joinDetail(jutil.Str(entry, "detail"), probe.Detail)
			}
		}
		entries = append(entries, entry)
	}
	result["results"], result["commands"] = entries, commands
	return result
}

func joinDetail(a, b string) string {
	out := strings.Trim(a+"；"+b, "；")
	return strings.TrimSpace(out)
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func firstLine(s string) string {
	lines := nonEmptyLines(s)
	if len(lines) == 0 {
		return ""
	}
	return lines[0]
}

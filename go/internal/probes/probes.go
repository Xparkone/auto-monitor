// Package probes 中间件部署方式探针注册表（对应 Python 版 mcp_inspect/probes.py）。
//
// 每种部署方式一个 Register("类型名") 探针函数：
//
//	fn(svc=map, host, via) -> Result{Alive *bool, Detail, Command}
//
// Alive=nil 表示检查执行失败（如 docker/kubectl 缺失），由判定层降级为 🟡。
//
// 新增部署方式（如 podman、supervisor、compose）：在本包 init() 中再注册一个
// 函数即可，config 的 deploy 校验、check_middleware 自动生效。
package probes

import (
	"fmt"
	"sort"
	"strings"

	"auto-monitor/internal/collectors"
	"auto-monitor/internal/jutil"
)

type Result struct {
	Alive   *bool // nil = 检查执行失败
	Detail  string
	Command string
}

type ProbeFn func(svc map[string]any, host, via string) Result

var probes = map[string]ProbeFn{}

// Register 注册一种部署类型的探针。
func Register(deployType string, fn ProbeFn) {
	probes[deployType] = fn
}

// Get 返回部署类型对应的探针；未注册返回 nil。
func Get(deployType string) ProbeFn {
	return probes[deployType]
}

// Registered 返回已注册类型（排序后），供配置校验与报告标题使用。
func Registered() []string {
	names := make([]string, 0, len(probes))
	for name := range probes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Valid 判断部署类型是否已注册（供 config 加载校验）。
func Valid(deployType string) bool {
	_, ok := probes[deployType]
	return ok
}

func boolPtr(b bool) *bool { return &b }

func init() {
	Register("binary", probeBinary)
	Register("container", probeContainer)
	Register("k8s", probeK8s)
}

// 二进制部署：systemctl 单元 → 进程匹配（ps | grep '[首字母]其余' 括号技巧）。
// 不用 pgrep -f：macOS 部分执行上下文中 pgrep 读不到其他进程 argv（误报不存活），
// 且 ssh 远端执行时探测命令自身 argv 含关键字会自匹配（误报存活）。
func probeBinary(svc map[string]any, host, via string) Result {
	process := jutil.Str(svc, "process")
	if process == "" {
		process = jutil.Str(svc, "name")
	}
	quoted := collectors.ShellQuote(process)
	regex := "''"
	if r := []rune(process); len(r) > 0 {
		regex = collectors.ShellQuote(fmt.Sprintf("[%s]%s", string(r[0]), string(r[1:])))
	}
	cmd := fmt.Sprintf("if command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet %s 2>/dev/null; "+
		"then echo active; elif ps -eo args 2>/dev/null | grep -E -- %s >/dev/null 2>&1; "+
		"then echo active; else echo inactive; fi", quoted, regex)
	res := collectors.Run(cmd, via, host, 20)
	stdout := strings.TrimSpace(res.Stdout)
	alive := res.OK && stdout == "active"
	if stdout == "" {
		stdout = res.Err
	}
	return Result{Alive: boolPtr(alive), Detail: fmt.Sprintf("systemctl/ps %s: %s", process, stdout), Command: res.Command}
}

// 容器部署：docker ps 中存在运行中的同名容器（精确匹配容器名）。
// --filter name= 是子串匹配（mysql 会命中 mysql-jump），必须在结果里按首列容器名精确过滤。
func probeContainer(svc map[string]any, host, via string) Result {
	container := jutil.Str(svc, "container")
	if container == "" {
		container = jutil.Str(svc, "name")
	}
	cmd := fmt.Sprintf("docker ps --filter name=%s --format '{{.Names}} {{.Status}}'",
		collectors.ShellQuote(container))
	res := collectors.Run(cmd, via, host, 20)
	if !res.OK {
		detail := strings.TrimSpace(res.Stderr)
		if detail == "" {
			detail = res.Err
		}
		return Result{Alive: nil, Detail: fmt.Sprintf("docker 检查失败: %s", detail), Command: res.Command}
	}
	var lines []string
	for _, l := range strings.Split(res.Stdout, "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	exact := collectors.MatchContainerLines(lines, container)
	if len(exact) > 0 {
		return Result{Alive: boolPtr(true), Detail: fmt.Sprintf("docker ps: %s", exact[0]), Command: res.Command}
	}
	detail := "无运行中的同名容器"
	if len(lines) > 0 {
		var near []string
		for _, l := range lines {
			if len(near) >= 3 {
				break
			}
			if fields := strings.Fields(l); len(fields) > 0 {
				near = append(near, fields[0])
			}
		}
		detail += fmt.Sprintf("（仅近似匹配: %s）", strings.Join(near, "、"))
	}
	return Result{Alive: boolPtr(false), Detail: detail, Command: res.Command}
}

// K8s 部署：Deployment 可用副本数 ≥ 期望副本数（kubectl 在控制机本地执行）。
func probeK8s(svc map[string]any, host, via string) Result {
	k8s := jutil.Map(svc, "k8s")
	namespace := jutil.Str(k8s, "namespace")
	if namespace == "" {
		namespace = "default"
	}
	deployment := jutil.Str(k8s, "deployment")
	if deployment == "" {
		deployment = jutil.Str(svc, "name")
	}
	cmd := fmt.Sprintf("kubectl -n %s get deploy %s -o jsonpath='{.status.availableReplicas}/{.spec.replicas}'",
		collectors.ShellQuote(namespace), collectors.ShellQuote(deployment))
	res := collectors.Run(cmd, "local", "", 20)
	if !res.OK {
		detail := strings.TrimSpace(res.Stderr)
		if detail == "" {
			detail = res.Err
		}
		return Result{Alive: nil, Detail: fmt.Sprintf("kubectl 检查失败: %s", detail), Command: res.Command}
	}
	parsed := collectors.ParseDeployReplicas(res.Stdout)
	if parsed == nil {
		return Result{Alive: nil, Detail: fmt.Sprintf("副本数解析失败: %q", res.Stdout), Command: res.Command}
	}
	alive, _ := parsed["alive"].(bool)
	available, _ := parsed["available"].(int)
	desired, _ := parsed["desired"].(int)
	return Result{Alive: boolPtr(alive),
		Detail:  fmt.Sprintf("副本 %d/%d", available, desired),
		Command: res.Command}
}

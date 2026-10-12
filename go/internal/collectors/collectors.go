// Package collectors 采集层：本机/SSH 命令执行、端口探测与输出解析
// （对应 Python 版 mcp_inspect/collectors.py）。
//
// 设计原则：
//   - 远端主机走系统 ssh（BatchMode，要求使用者已配置免密登录），不引入 SSH 库。
//   - 所有执行统一返回 ExecResult，带实际命令与耗时，供巡检日志排查。
//   - 解析函数与执行分离，便于用夹具数据做单元测试。
package collectors

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type ExecResult struct {
	OK         bool
	Stdout     string
	Stderr     string
	DurationMS int
	Err        string
	Command    string
}

// Run 在目标机上执行 shell 命令。via=ssh 时经系统 ssh 到 host 执行。
func Run(shellCmd, via, host string, timeoutSec int) ExecResult {
	var cmd *exec.Cmd
	var display string
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSec)*time.Second)
	defer cancel()

	if via == "ssh" {
		if host == "" {
			return ExecResult{OK: false, Err: "ssh 模式缺少 host", Command: shellCmd}
		}
		cmd = exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5",
			"-o", "StrictHostKeyChecking=accept-new", host, "--", shellCmd)
		display = fmt.Sprintf("ssh %s -- %s", host, shellCmd)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", shellCmd)
		display = shellCmd
	}

	start := time.Now()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	dur := int(time.Since(start).Milliseconds())

	if ctx.Err() == context.DeadlineExceeded {
		return ExecResult{OK: false, DurationMS: dur,
			Err: fmt.Sprintf("执行超时(%ds)", timeoutSec), Command: display}
	}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return ExecResult{OK: false, Stdout: stdout.String(), Stderr: stderr.String(),
				DurationMS: dur, Err: fmt.Sprintf("退出码 %d", exitErr.ExitCode()), Command: display}
		}
		return ExecResult{OK: false, DurationMS: dur, Err: err.Error(), Command: display}
	}
	return ExecResult{OK: true, Stdout: stdout.String(), Stderr: stderr.String(),
		DurationMS: dur, Command: display}
}

// CheckPort TCP 端口探测（socket 连接，无需登录目标机）。
func CheckPort(host string, port int, timeoutSec float64) ExecResult {
	display := fmt.Sprintf("tcp-connect %s:%d", host, port)
	start := time.Now()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)),
		time.Duration(timeoutSec*float64(time.Second)))
	dur := int(time.Since(start).Milliseconds())
	if err != nil {
		return ExecResult{OK: false, DurationMS: dur, Err: err.Error(), Command: display}
	}
	_ = conn.Close()
	return ExecResult{OK: true, DurationMS: dur, Command: display}
}

// ---------------------------------------------------------------- 输出解析

var loadRE = regexp.MustCompile(`load averages?:\s*([0-9.,\s]+)$`)

// ParseLoad 解析 uptime 输出；返回 nil 表示解析失败。
func ParseLoad(out string) map[string]float64 {
	match := loadRE.FindStringSubmatch(strings.TrimSpace(out))
	if match == nil {
		return nil
	}
	fields := strings.FieldsFunc(match[1], func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
	if len(fields) < 3 {
		return nil
	}
	vals := make([]float64, 3)
	for i := 0; i < 3; i++ {
		v, err := strconv.ParseFloat(fields[i], 64)
		if err != nil {
			return nil
		}
		vals[i] = v
	}
	return map[string]float64{"load1": vals[0], "load5": vals[1], "load15": vals[2]}
}

// ParseCPULinux 解析两行 /proc/stat 的 cpu 行，按差值计算使用率。
func ParseCPULinux(sampleOut string) (float64, bool) {
	var lines []string
	for _, l := range strings.Split(sampleOut, "\n") {
		if strings.HasPrefix(l, "cpu ") {
			lines = append(lines, l)
		}
	}
	if len(lines) < 2 {
		return 0, false
	}
	toks := func(line string) []float64 {
		fields := strings.Fields(line)[1:]
		vals := make([]float64, 0, len(fields))
		for _, f := range fields {
			v, err := strconv.ParseFloat(f, 64)
			if err != nil {
				return nil
			}
			vals = append(vals, v)
		}
		return vals
	}
	a, b := toks(lines[0]), toks(lines[1])
	if len(a) < 5 || len(b) < 5 {
		return 0, false
	}
	sum := func(v []float64) float64 {
		s := 0.0
		for _, x := range v {
			s += x
		}
		return s
	}
	idleA, idleB := a[3]+a[4], b[3]+b[4]
	dTotal, dIdle := sum(b)-sum(a), idleB-idleA
	if dTotal <= 0 || dIdle < 0 {
		return 0, false
	}
	return round1((1 - dIdle/dTotal) * 100), true
}

var cpuDarwinRE = regexp.MustCompile(`(?s)CPU usage:.*?([\d.]+)%\s*idle`)

// ParseCPUDarwin 解析 macOS top 行：'CPU usage: 5.26% user, 10.52% sys, 84.21% idle'。
func ParseCPUDarwin(topOut string) (float64, bool) {
	match := cpuDarwinRE.FindStringSubmatch(topOut)
	if match == nil {
		return 0, false
	}
	idle, err := strconv.ParseFloat(match[1], 64)
	if err != nil {
		return 0, false
	}
	return round1(100 - idle), true
}

type MemInfo struct {
	TotalMB float64
	UsedMB  float64
	Percent float64
}

// ParseMemLinux 解析 free -m，使用 available 列计算真实使用率。
func ParseMemLinux(freeOut string) *MemInfo {
	for _, line := range strings.Split(freeOut, "\n") {
		if !strings.HasPrefix(line, "Mem:") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 4 {
			return nil
		}
		total, err1 := strconv.ParseFloat(parts[1], 64)
		var available float64
		var err2 error
		if len(parts) > 6 {
			available, err2 = strconv.ParseFloat(parts[6], 64)
		} else {
			available, err2 = strconv.ParseFloat(parts[3], 64)
		}
		if err1 != nil || err2 != nil {
			return nil
		}
		used := total - available
		percent := 0.0
		if total != 0 {
			percent = round1(used / total * 100)
		}
		return &MemInfo{TotalMB: total, UsedMB: used, Percent: percent}
	}
	return nil
}

var vmStatRE = regexp.MustCompile(`^Pages ([a-z ]+):\s+(\d+)\.$`)

// ParseMemDarwin 解析 'sysctl -n hw.memsize vm.pagesize && vm_stat' 的组合输出。
func ParseMemDarwin(out string) *MemInfo {
	var values []string
	for _, l := range strings.Split(out, "\n") {
		if s := strings.TrimSpace(l); s != "" {
			values = append(values, s)
		}
	}
	if len(values) < 3 {
		return nil
	}
	memsize, err1 := strconv.ParseInt(values[0], 10, 64)
	pageSize, err2 := strconv.ParseInt(values[1], 10, 64)
	if err1 != nil || err2 != nil {
		return nil
	}
	pages := map[string]int64{}
	for _, line := range values[2:] {
		if m := vmStatRE.FindStringSubmatch(line); m != nil {
			n, err := strconv.ParseInt(m[2], 10, 64)
			if err == nil {
				pages[strings.TrimSpace(m[1])] = n
			}
		}
	}
	freeish := pages["free"] + pages["inactive"] + pages["speculative"]
	used := memsize - freeish*pageSize
	if memsize <= 0 || used < 0 {
		return nil
	}
	return &MemInfo{
		TotalMB: float64(memsize) / 1048576,
		UsedMB:  float64(used) / 1048576,
		Percent: round1(float64(used) / float64(memsize) * 100),
	}
}

type Proc struct {
	PID  int
	CPU  float64
	Mem  float64
	Comm string
}

type Zombie struct {
	PID  int
	Comm string
}

// ParsePS 解析 'ps -eo pid=,pcpu=,pmem=,state=,comm='：返回僵尸进程与 CPU 前 5 进程。
func ParsePS(psOut string) (zombies []Zombie, top []Proc) {
	zombies = []Zombie{}
	var procs []Proc
	for _, line := range strings.Split(psOut, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		cpu, err2 := strconv.ParseFloat(fields[1], 64)
		mem, err3 := strconv.ParseFloat(fields[2], 64)
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		entry := Proc{PID: pid, CPU: cpu, Mem: mem, Comm: fields[4]}
		procs = append(procs, entry)
		if strings.HasPrefix(fields[3], "Z") {
			zombies = append(zombies, Zombie{PID: pid, Comm: entry.Comm})
		}
	}
	sort.Slice(procs, func(i, j int) bool { return procs[i].CPU > procs[j].CPU })
	top = procs
	if len(top) > 5 {
		top = top[:5]
	}
	return zombies, top
}

// ParseDFSections 解析批量 df 输出：每段以 '== <mount>' 开头，后跟一行 df -k -P 结果。
// 返回的 map 结构与 Python 版一致（mount/ok/fs/size_kb/used_kb/avail_kb/percent/mount_on/error）。
func ParseDFSections(dfOut string) []map[string]any {
	sections := []map[string]any{}
	var current map[string]any
	for _, line := range strings.Split(dfOut, "\n") {
		if strings.HasPrefix(line, "== ") {
			current = map[string]any{"mount": strings.TrimSpace(line[3:]), "ok": false}
			sections = append(sections, current)
			continue
		}
		if current == nil || strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) >= 6 && strings.HasSuffix(parts[4], "%") {
			sizeKB, _ := strconv.ParseInt(parts[1], 10, 64)
			usedKB, _ := strconv.ParseInt(parts[2], 10, 64)
			availKB, _ := strconv.ParseInt(parts[3], 10, 64)
			percent, _ := strconv.ParseFloat(strings.TrimSuffix(parts[4], "%"), 64)
			current["ok"] = true
			current["fs"] = parts[0]
			current["size_kb"] = sizeKB
			current["used_kb"] = usedKB
			current["avail_kb"] = availKB
			current["percent"] = percent
			current["mount_on"] = parts[5]
		} else if _, has := current["error"]; !has {
			current["error"] = strings.TrimSpace(line)
		}
	}
	return sections
}

func HumanGB(kb float64) string {
	return fmt.Sprintf("%.1fG", kb/1048576)
}

// ParsePodsJSON 解析 'kubectl get pods -o json' 的输出。
func ParsePodsJSON(text string) ([]map[string]any, error) {
	var data struct {
		Items []struct {
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
			Status struct {
				Phase             string `json:"phase"`
				ContainerStatuses []struct {
					Ready        bool `json:"ready"`
					RestartCount int  `json:"restartCount"`
				} `json:"containerStatuses"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(text), &data); err != nil {
		return nil, err
	}
	pods := []map[string]any{}
	for _, item := range data.Items {
		ready := 0
		restarts := 0
		for _, c := range item.Status.ContainerStatuses {
			if c.Ready {
				ready++
			}
			restarts += c.RestartCount
		}
		total := len(item.Status.ContainerStatuses)
		phase := item.Status.Phase
		if phase == "" {
			phase = "Unknown"
		}
		pods = append(pods, map[string]any{
			"name":      item.Metadata.Name,
			"namespace": item.Metadata.Namespace,
			"phase":     phase,
			"ready":     fmt.Sprintf("%d/%d", ready, total),
			"ready_ok":  total > 0 && ready == total,
			"restarts":  restarts,
		})
	}
	return pods, nil
}

var replicasRE = regexp.MustCompile(`^\s*(\d*)\s*/\s*(\d+)\s*$`)

// ParseDeployReplicas 解析 kubectl jsonpath '{.status.availableReplicas}/{.spec.replicas}'，如 '2/2'。
func ParseDeployReplicas(out string) map[string]any {
	m := replicasRE.FindStringSubmatch(out)
	if m == nil {
		return nil
	}
	available := 0
	if m[1] != "" {
		v, err := strconv.Atoi(m[1])
		if err != nil {
			return nil
		}
		available = v
	}
	desired, err := strconv.Atoi(m[2])
	if err != nil {
		return nil
	}
	return map[string]any{
		"available": available,
		"desired":   desired,
		"alive":     desired > 0 && available >= desired,
	}
}

var shellSafeRE = regexp.MustCompile(`^[a-zA-Z0-9_@%+=:,./-]+$`)

// ShellQuote 与 Python shlex.quote 等价。
func ShellQuote(value string) string {
	if value == "" {
		return "''"
	}
	if shellSafeRE.MatchString(value) {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

// MatchContainerLines 从 'docker ps --format {{.Names}} {{.Status}}' 的输出行中
// 挑出容器名精确等于 name 的行。docker 的 --filter name= 是子串匹配，需要二次精确过滤。
func MatchContainerLines(lines []string, name string) []string {
	if name == "" {
		return nil
	}
	var out []string
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == name {
			out = append(out, line)
		}
	}
	return out
}

func round1(v float64) float64 {
	return float64(int(v*10+0.5)) / 10
}

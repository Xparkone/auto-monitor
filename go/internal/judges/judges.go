// Package judges 阈值判定：把工具结果翻译成 🟢正常 / 🟡注意 / 🔴异常，
// 与 skills/*.md 规则一一对应（对应 Python 版 mcp_inspect/judges.py）。
package judges

import (
	"fmt"
	"sort"
	"strings"

	"auto-monitor/internal/config"
	"auto-monitor/internal/jutil"
)

const (
	LevelOK   = "ok"
	LevelWarn = "warn"
	LevelCrit = "crit"
)

var (
	levelOrder   = map[string]int{LevelOK: 0, LevelWarn: 1, LevelCrit: 2}
	emoji        = map[string]string{LevelOK: "🟢", LevelWarn: "🟡", LevelCrit: "🔴"}
	label        = map[string]string{LevelOK: "正常", LevelWarn: "注意", LevelCrit: "异常"}
	overallLabel = map[string]string{LevelOK: "优秀", LevelWarn: "注意", LevelCrit: "异常"}
)

// Worst 返回多个级别里最差的一个；全部未知时视为 ok。
func Worst(levels ...string) string {
	worst, worstRank := "", -1
	for _, lv := range levels {
		if rank, ok := levelOrder[lv]; ok && rank > worstRank {
			worst, worstRank = lv, rank
		}
	}
	if worst == "" {
		return LevelOK
	}
	return worst
}

// Mark 返回“🟢 正常”样式的级别标签。
func Mark(level string) string {
	return fmt.Sprintf("%s %s", emoji[level], label[level])
}

// JudgePercent 判定百分比。value 不存在返回空串（对应 Python 的 None）。
func JudgePercent(value *float64, warn float64, crit *float64) string {
	if value == nil {
		return ""
	}
	if crit != nil && *value >= *crit {
		return LevelCrit
	}
	if *value >= warn {
		return LevelWarn
	}
	return LevelOK
}

type Verdict struct {
	Level  string
	Issues []string
	Levels map[string]string // 各子项级别（部分判定函数使用）
}

func floatPtrOrNil(m map[string]any, key string) *float64 {
	if v, ok := jutil.Float(m, key); ok {
		return &v
	}
	return nil
}

// JudgeResources check_host_resources 结果 → 级别与问题列表。
func JudgeResources(res map[string]any, th config.Thresholds) Verdict {
	verdict := Verdict{Issues: []string{}, Levels: map[string]string{}}
	if ok, _ := jutil.Bool(res, "ok"); !ok {
		return Verdict{Level: LevelCrit,
			Issues: []string{"主机不可达或命令失败（详见巡检日志）"}, Levels: map[string]string{}}
	}

	if cpuLV := JudgePercent(floatPtrOrNil(res, "cpu_percent"), th.CPUWarn, nil); cpuLV != "" {
		verdict.Levels["cpu"] = cpuLV
		if cpuLV != LevelOK {
			v, _ := jutil.Float(res, "cpu_percent")
			verdict.Issues = append(verdict.Issues,
				fmt.Sprintf("CPU 使用率 %s%% ≥ %s%%", jutil.PyFloat(v), jutil.PyFloat(th.CPUWarn)))
		}
	}
	if memLV := JudgePercent(floatPtrOrNil(res, "mem_percent"), th.MemWarn, nil); memLV != "" {
		verdict.Levels["mem"] = memLV
		if memLV != LevelOK {
			v, _ := jutil.Float(res, "mem_percent")
			verdict.Issues = append(verdict.Issues,
				fmt.Sprintf("内存使用率 %s%% ≥ %s%%", jutil.PyFloat(v), jutil.PyFloat(th.MemWarn)))
		}
	}

	cores, ok := jutil.Int(res, "cores")
	if !ok || cores <= 0 {
		cores = 1
	}
	if load1, ok := jutil.Float(res, "load1"); ok {
		loadLV := LevelOK
		if load1 >= float64(cores)*th.LoadPerCoreWarn {
			loadLV = LevelWarn
		}
		verdict.Levels["load"] = loadLV
		if loadLV != LevelOK {
			verdict.Issues = append(verdict.Issues,
				fmt.Sprintf("1 分钟负载 %s ≥ %d 核 × %s", jutil.PyFloat(load1), cores, jutil.PyFloat(th.LoadPerCoreWarn)))
		}
	}

	zombies := jutil.MapSlice(res, "zombies")
	zombieLV := LevelOK
	if len(zombies) > 0 {
		zombieLV = LevelWarn
	}
	verdict.Levels["zombies"] = zombieLV
	if len(zombies) > 0 {
		verdict.Issues = append(verdict.Issues, zombieIssue(zombies))
	}

	verdict.Level = Worst(levelValues(verdict.Levels)...)
	return verdict
}

func zombieIssue(zombies []map[string]any) string {
	counts := map[string]int{}
	for _, proc := range zombies {
		name := jutil.Str(proc, "comm")
		if name == "" {
			name = "?"
		}
		counts[name]++
	}
	type kv struct {
		name  string
		count int
	}
	ranked := make([]kv, 0, len(counts))
	for name, count := range counts {
		ranked = append(ranked, kv{name, count})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].count != ranked[j].count {
			return ranked[i].count > ranked[j].count
		}
		return ranked[i].name < ranked[j].name
	})
	var shown []string
	for i, item := range ranked {
		if i >= 3 {
			break
		}
		if item.count > 1 {
			shown = append(shown, fmt.Sprintf("%s ×%d", item.name, item.count))
		} else {
			shown = append(shown, item.name)
		}
	}
	text := strings.Join(shown, "、")
	if len(ranked) > 3 {
		text += fmt.Sprintf(" 等 %d 种", len(ranked))
	}
	return fmt.Sprintf("僵尸进程 %d 个（%s）", len(zombies), text)
}

// JudgeDisk 单个挂载点磁盘结果 → 级别与问题。
func JudgeDisk(section map[string]any, th config.Thresholds) Verdict {
	if ok, _ := jutil.Bool(section, "ok"); !ok {
		detail := jutil.Str(section, "error")
		if detail == "" {
			detail = "df 失败"
		}
		return Verdict{Level: LevelWarn, Issues: []string{fmt.Sprintf("磁盘采集失败: %s", detail)}}
	}
	warn, crit := th.DiskWarn, th.DiskCrit
	level := JudgePercent(floatPtrOrNil(section, "percent"), warn, &crit)
	mount := jutil.Str(section, "mount")
	percent, _ := jutil.Float(section, "percent")
	switch level {
	case LevelCrit:
		return Verdict{Level: level, Issues: []string{
			fmt.Sprintf("%s 使用率 %s%%（异常 ≥ %s%%）", mount, jutil.PyFloat(percent), jutil.PyFloat(crit))}}
	case LevelWarn:
		return Verdict{Level: level, Issues: []string{
			fmt.Sprintf("%s 使用率 %s%%（注意 ≥ %s%%）", mount, jutil.PyFloat(percent), jutil.PyFloat(warn))}}
	default:
		return Verdict{Level: level, Issues: []string{}}
	}
}

// JudgePod 单个 Pod 结果 → 级别与问题。
func JudgePod(pod map[string]any, th config.Thresholds) Verdict {
	level := LevelOK
	issues := []string{}
	phase := jutil.Str(pod, "phase")
	readyOK, _ := jutil.Bool(pod, "ready_ok")
	if (phase != "Running" && phase != "Succeeded") || !readyOK {
		level = LevelCrit
		issues = append(issues, fmt.Sprintf("状态 %s，Ready %s", phase, jutil.Str(pod, "ready")))
	} else if restarts, _ := jutil.Int(pod, "restarts"); restarts >= th.PodRestartWarn {
		level = LevelWarn
		issues = append(issues, fmt.Sprintf("重启 %d 次 ≥ %d", restarts, th.PodRestartWarn))
	}
	return Verdict{Level: level, Issues: issues}
}

// JudgeMiddleware 单个中间件 service 条目 → 级别与问题。
func JudgeMiddleware(entry map[string]any) Verdict {
	if portOpen, ok := entry["port_open"].(bool); ok && !portOpen {
		return Verdict{Level: LevelCrit, Issues: []string{"端口未监听"}}
	}
	alive, hasAlive := entry["alive"].(bool)
	if hasAlive && !alive {
		return Verdict{Level: LevelCrit,
			Issues: []string{fmt.Sprintf("部署不存活: %s", jutil.Str(entry, "detail"))}}
	}
	if entry["alive"] == nil && jutil.Str(entry, "detail") != "" {
		// 部署检查执行失败（docker/kubectl 缺失等），只有端口信息
		return Verdict{Level: LevelWarn,
			Issues: []string{fmt.Sprintf("部署检查未完成: %s", jutil.Str(entry, "detail"))}}
	}
	return Verdict{Level: LevelOK, Issues: []string{}}
}

type Overall struct {
	Level string
	Emoji string
	Label string
}

// Summarize 汇总整体健康度；counters 原地累加各级别计数。
func Summarize(levels []string, counters map[string]int) Overall {
	overall := Worst(levels...)
	for _, lv := range levels {
		counters[lv]++
	}
	return Overall{Level: overall, Emoji: emoji[overall], Label: overallLabel[overall]}
}

func levelValues(levels map[string]string) []string {
	out := make([]string, 0, len(levels))
	for _, lv := range levels {
		out = append(out, lv)
	}
	return out
}

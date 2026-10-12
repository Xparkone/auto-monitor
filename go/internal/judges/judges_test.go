package judges

import (
	"reflect"
	"testing"

	"auto-monitor/internal/config"
)

var testTh = config.Thresholds{
	CPUWarn: 80, MemWarn: 80, DiskWarn: 75, DiskCrit: 90,
	LoadPerCoreWarn: 2, PodRestartWarn: 5,
}

func TestWorst(t *testing.T) {
	if Worst("ok", "warn", "crit", "unknown") != "crit" {
		t.Fatal("worst should be crit")
	}
	if Worst() != "ok" || Worst("unknown") != "ok" {
		t.Fatal("empty/unknown should be ok")
	}
}

func TestMark(t *testing.T) {
	if Mark("warn") != "🟡 注意" {
		t.Fatalf("got %s", Mark("warn"))
	}
}

func TestJudgeResourcesHealthy(t *testing.T) {
	res := map[string]any{
		"ok": true, "cpu_percent": 10.0, "mem_percent": 20.0,
		"cores": 4, "load1": 1.0, "zombies": []any{},
	}
	v := JudgeResources(res, testTh)
	if v.Level != "ok" || len(v.Issues) != 0 {
		t.Fatalf("got %+v", v)
	}
	if v.Levels["cpu"] != "ok" || v.Levels["mem"] != "ok" || v.Levels["load"] != "ok" || v.Levels["zombies"] != "ok" {
		t.Fatalf("levels %+v", v.Levels)
	}
}

func TestJudgeResourcesWarn(t *testing.T) {
	res := map[string]any{
		"ok": true, "cpu_percent": 85.5, "mem_percent": 20.0,
		"cores": 4, "load1": 9.0,
		"zombies": []any{
			map[string]any{"pid": 1, "comm": "foo"},
			map[string]any{"pid": 2, "comm": "foo"},
			map[string]any{"pid": 3, "comm": "bar"},
		},
	}
	v := JudgeResources(res, testTh)
	if v.Level != "warn" {
		t.Fatalf("level %s", v.Level)
	}
	wantIssues := []string{
		"CPU 使用率 85.5% ≥ 80.0%",
		"1 分钟负载 9.0 ≥ 4 核 × 2.0",
		"僵尸进程 3 个（foo ×2、bar）",
	}
	if !reflect.DeepEqual(v.Issues, wantIssues) {
		t.Fatalf("issues %v, want %v", v.Issues, wantIssues)
	}
}

func TestJudgeResourcesUnreachable(t *testing.T) {
	v := JudgeResources(map[string]any{"ok": false}, testTh)
	if v.Level != "crit" || len(v.Issues) != 1 {
		t.Fatalf("got %+v", v)
	}
}

func TestJudgeDisk(t *testing.T) {
	if v := JudgeDisk(map[string]any{"ok": true, "mount": "/", "percent": 95.0}, testTh); v.Level != "crit" {
		t.Fatalf("got %+v", v)
	}
	if v := JudgeDisk(map[string]any{"ok": true, "mount": "/", "percent": 80.0}, testTh); v.Level != "warn" {
		t.Fatalf("got %+v", v)
	}
	if v := JudgeDisk(map[string]any{"ok": true, "mount": "/", "percent": 10.0}, testTh); v.Level != "ok" || len(v.Issues) != 0 {
		t.Fatalf("got %+v", v)
	}
	if v := JudgeDisk(map[string]any{"ok": false, "mount": "/data"}, testTh); v.Level != "warn" {
		t.Fatalf("got %+v", v)
	}
}

func TestJudgePod(t *testing.T) {
	if v := JudgePod(map[string]any{"phase": "Running", "ready_ok": true, "restarts": 0}, testTh); v.Level != "ok" {
		t.Fatalf("got %+v", v)
	}
	if v := JudgePod(map[string]any{"phase": "Pending", "ready_ok": false, "ready": "0/1"}, testTh); v.Level != "crit" {
		t.Fatalf("got %+v", v)
	}
	if v := JudgePod(map[string]any{"phase": "Running", "ready_ok": true, "restarts": 6}, testTh); v.Level != "warn" {
		t.Fatalf("got %+v", v)
	}
}

func TestJudgeMiddleware(t *testing.T) {
	if v := JudgeMiddleware(map[string]any{"port_open": false, "alive": nil, "detail": "端口不可达: x"}); v.Level != "crit" {
		t.Fatalf("got %+v", v)
	}
	if v := JudgeMiddleware(map[string]any{"port_open": true, "alive": false, "detail": "无运行中的同名容器"}); v.Level != "crit" {
		t.Fatalf("got %+v", v)
	}
	if v := JudgeMiddleware(map[string]any{"port_open": true, "alive": nil, "detail": "docker 检查失败: x"}); v.Level != "warn" {
		t.Fatalf("got %+v", v)
	}
	if v := JudgeMiddleware(map[string]any{"port_open": true, "alive": true, "detail": "docker ps: mysql Up"}); v.Level != "ok" {
		t.Fatalf("got %+v", v)
	}
}

func TestSummarize(t *testing.T) {
	counters := map[string]int{}
	o := Summarize([]string{"ok", "warn", "ok", "crit"}, counters)
	if o.Level != "crit" || o.Emoji != "🔴" || o.Label != "异常" {
		t.Fatalf("got %+v", o)
	}
	want := map[string]int{"ok": 2, "warn": 1, "crit": 1}
	if !reflect.DeepEqual(counters, want) {
		t.Fatalf("counters %v", counters)
	}
}

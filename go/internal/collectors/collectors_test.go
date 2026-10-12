package collectors

import (
	"math"
	"reflect"
	"testing"
)

func TestParseLoadDarwin(t *testing.T) {
	got := ParseLoad(" 9:30  up 2 days,  3:15, 2 users, load averages: 1.62 1.70 1.62\n")
	want := map[string]float64{"load1": 1.62, "load5": 1.70, "load15": 1.62}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestParseLoadLinux(t *testing.T) {
	got := ParseLoad(" 09:30:00 up 10 days, 1 user,  load average: 0.01, 0.02, 0.00\n")
	want := map[string]float64{"load1": 0.01, "load5": 0.02, "load15": 0.00}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if ParseLoad("garbage") != nil {
		t.Fatal("expect nil for unparseable output")
	}
}

func TestParseCPULinux(t *testing.T) {
	// 第二行 idle 增加 50，total 增加 100 → 使用率 50%
	// total 1050→1150（+100），idle 850→900（+50）→ 使用率 50%
	sample := "cpu  100 0 100 800 50 0 0 0 0 0\ncpu  150 0 100 850 50 0 0 0 0 0\n"
	got, ok := ParseCPULinux(sample)
	if !ok || math.Abs(got-50.0) > 0.05 {
		t.Fatalf("got %v ok=%v", got, ok)
	}
	if _, ok := ParseCPULinux("cpu  1 2 3\n"); ok {
		t.Fatal("expect not ok for single sample")
	}
}

func TestParseCPUDarwin(t *testing.T) {
	got, ok := ParseCPUDarwin("CPU usage: 5.26% user, 10.52% sys, 84.21% idle\n")
	if !ok || math.Abs(got-15.8) > 0.05 {
		t.Fatalf("got %v ok=%v", got, ok)
	}
}

func TestParseMemLinux(t *testing.T) {
	out := "               total        used        free      shared  buff/cache   available\n" +
		"Mem:           7828        1234        5678         100         900        6594\n" +
		"Swap:             0           0           0\n"
	got := ParseMemLinux(out)
	if got == nil || got.TotalMB != 7828 || got.UsedMB != 1234 || math.Abs(got.Percent-15.8) > 0.05 {
		t.Fatalf("got %+v", got)
	}
}

func TestParseMemDarwin(t *testing.T) {
	// memsize 8589934592 (8G), pagesize 16384, free+inactive+speculative = 100000 页
	out := "8589934592\n16384\n" +
		"Pages free:                                50000.\n" +
		"Pages active:                             100000.\n" +
		"Pages inactive:                            40000.\n" +
		"Pages speculative:                         10000.\n"
	got := ParseMemDarwin(out)
	if got == nil {
		t.Fatal("nil result")
	}
	// freeish = 100000 页 * 16384 = 1638400000 字节 ≈ 19.07% 空闲 → 使用 ≈ 80.9%
	if math.Abs(got.TotalMB-8192) > 0.5 || math.Abs(got.Percent-80.9) > 0.1 {
		t.Fatalf("got %+v", got)
	}
}

func TestParsePS(t *testing.T) {
	out := "  1   0.0  0.1 S   /sbin/launchd\n" +
		" 42  88.5  1.2 R   /usr/bin/stress --cpu\n" +
		" 77   0.0  0.0 Z   (defunct)\n" +
		" 78   0.0  0.0 Zs  (zombie2)\n"
	zombies, top := ParsePS(out)
	if len(zombies) != 2 || zombies[0].PID != 77 || zombies[1].Comm != "(zombie2)" {
		t.Fatalf("zombies %+v", zombies)
	}
	if len(top) < 1 || top[0].PID != 42 || top[0].CPU != 88.5 {
		t.Fatalf("top %+v", top)
	}
}

func TestParseDFSections(t *testing.T) {
	out := "== /\n/dev/disk1s1  488245288 200000000 280000000    42%    0    0  100%   /\n" +
		"== /data\ndf: /data: No such file or directory\n"
	got := ParseDFSections(out)
	if len(got) != 2 {
		t.Fatalf("sections %v", got)
	}
	if got[0]["ok"] != true || got[0]["percent"] != 42.0 || got[0]["size_kb"] != int64(488245288) {
		t.Fatalf("first section %v", got[0])
	}
	if got[1]["ok"] != false || got[1]["error"] != "df: /data: No such file or directory" {
		t.Fatalf("second section %v", got[1])
	}
}

func TestParsePodsJSON(t *testing.T) {
	payload := `{"items":[{"metadata":{"name":"a","namespace":"default"},
		"status":{"phase":"Running","containerStatuses":[
			{"ready":true,"restartCount":1},{"ready":false,"restartCount":6}]}}]}`
	pods, err := ParsePodsJSON(payload)
	if err != nil || len(pods) != 1 {
		t.Fatalf("pods %v err %v", pods, err)
	}
	p := pods[0]
	if p["ready"] != "1/2" || p["ready_ok"] != false || p["restarts"] != 7 || p["phase"] != "Running" {
		t.Fatalf("pod %v", p)
	}
}

func TestParseDeployReplicas(t *testing.T) {
	got := ParseDeployReplicas("2/2")
	if got == nil || got["available"] != 2 || got["desired"] != 2 || got["alive"] != true {
		t.Fatalf("got %v", got)
	}
	got = ParseDeployReplicas(" /3 ")
	if got == nil || got["available"] != 0 || got["alive"] != false {
		t.Fatalf("got %v", got)
	}
	if ParseDeployReplicas("x") != nil {
		t.Fatal("expect nil")
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"mysql":   "mysql",
		"a b":     "'a b'",
		"a'b":     `'a'"'"'b'`,
		"":        "''",
		"/x/y_1-": "/x/y_1-",
	}
	for in, want := range cases {
		if got := ShellQuote(in); got != want {
			t.Fatalf("ShellQuote(%q)=%q want %q", in, got, want)
		}
	}
}

func TestMatchContainerLines(t *testing.T) {
	lines := []string{"mysql-jump Up 2 days", "mysql Up 3 days", " other Up"}
	got := MatchContainerLines(lines, "mysql")
	if len(got) != 1 || got[0] != "mysql Up 3 days" {
		t.Fatalf("got %v", got)
	}
	if MatchContainerLines(lines, "") != nil {
		t.Fatal("expect nil for empty name")
	}
}

func TestHumanGB(t *testing.T) {
	if HumanGB(1048576) != "1.0G" || HumanGB(488245288) != "465.6G" {
		t.Fatalf("wrong %s %s", HumanGB(1048576), HumanGB(488245288))
	}
}

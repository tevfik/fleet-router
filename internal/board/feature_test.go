package board

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/hostinfo"
	"github.com/tevfik/fleet-router/internal/server"
)

func has(lines []string, sub string) bool {
	for _, l := range lines {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

func count(lines []string, sub string) int {
	n := 0
	for _, l := range lines {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

func boolp(b bool) *bool { return &b }

// ---- small formatters ----

func TestFeature_Spark_ScaleWidthAndClamp(t *testing.T) {
	if got := Spark([]float64{0, 50, 100}, 100, 10); got != "▁▄█" {
		t.Fatalf("%q", got)
	}
	if got := Spark([]float64{1, 2, 3, 4, 5}, 0, 3); utf8.RuneCountInString(got) != 3 || got != "▅▆█" {
		t.Fatalf("last w values, scaled to the largest: %q", got)
	}
	if got := Spark([]float64{0, 0}, 0, 5); got != "▁▁" {
		t.Fatalf("all zero: %q", got)
	}
	if got := Spark([]float64{-5, 500}, 100, 5); got != "▁█" {
		t.Fatalf("clamped: %q", got)
	}
	if got := Spark(nil, 100, 5); got != "" {
		t.Fatalf("empty: %q", got)
	}
	if got := sparkInts([]int64{10, 20}, 5); got != "▄█" {
		t.Fatalf("ints: %q", got)
	}
}

func TestFeature_Bytes(t *testing.T) {
	for n, want := range map[int64]string{
		-1: "?", 0: "0 MiB", 512 << 20: "512 MiB", 1 << 30: "1.0 GiB", 1536 << 20: "1.5 GiB", 1 << 40: "1.0 TiB", 3 << 39: "1.5 TiB",
	} {
		if got := Bytes(n); got != want {
			t.Errorf("Bytes(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestFeature_UptimeAndAge(t *testing.T) {
	for s, want := range map[int64]string{0: "0m", 59: "0m", 720: "12m", 3600: "1h 0m", 5*3600 + 12*60: "5h 12m", 3*86400 + 4*3600: "3d 4h"} {
		if got := Uptime(s); got != want {
			t.Errorf("Uptime(%d) = %q, want %q", s, got, want)
		}
	}
	now := time.Now()
	for d, want := range map[time.Duration]string{
		10 * time.Second: "now", 5*time.Minute + time.Second: "5m ago", 3*time.Hour + time.Minute: "3h ago",
		47 * time.Hour: "47h ago", 72*time.Hour + time.Minute: "3d ago",
	} {
		if got := Age(now.Add(-d)); got != want {
			t.Errorf("Age(-%v) = %q, want %q", d, got, want)
		}
	}
}

func TestFeature_PctAndTemp(t *testing.T) {
	if pct(1, 0) != -1 || pct(-1, 10) != -1 || pct(5, 10) != 50 {
		t.Fatal("pct")
	}
	if pctStr(-1) != "—" || pctStr(49.6) != "50%" || tempStr(-1) != "" || tempStr(61.4) != "61°C" {
		t.Fatal("strings")
	}
	if nonEmpty("", "d") != "d" || nonEmpty("x", "d") != "x" {
		t.Fatal("nonEmpty")
	}
	if trunc("abcdef", 4) != "abc…" || trunc("abc", 4) != "abc" || trunc("ğüşiöç", 3) != "ğü…" {
		t.Fatal("trunc counts runes")
	}
}

func TestFeature_CPUTemp_BoardFallback(t *testing.T) {
	cpu := hostinfo.Sample{Temps: []hostinfo.Temp{{Name: "nvme0", C: 40}, {Name: "cpu", C: 70}}}
	if cpuTemp(cpu) != 70 || cpuTempLabel(cpu) != "70°C" {
		t.Fatalf("cpu: %v %q", cpuTemp(cpu), cpuTempLabel(cpu))
	}
	board := hostinfo.Sample{Temps: []hostinfo.Temp{{Name: "board", C: 55}, {Name: "nvme0", C: 40}}}
	if cpuTemp(board) != 55 || cpuTempLabel(board) != "board 55°C" {
		t.Fatalf("board: %v %q", cpuTemp(board), cpuTempLabel(board))
	}
	none := hostinfo.Sample{Temps: []hostinfo.Temp{{Name: "nvme0", C: 40}}}
	if cpuTemp(none) != -1 || cpuTempLabel(none) != "" {
		t.Fatal("none")
	}
}

// ---- list row cells ----

func TestFeature_NodeCells(t *testing.T) {
	if c, r, g, v, tp := NodeCells(server.HostReport{Err: "unreachable"}); c != "—" || r != "—" || g != "—" || v != "—" || tp != "" {
		t.Fatal("error row")
	}
	if c, _, _, _, _ := NodeCells(server.HostReport{}); c != "—" {
		t.Fatal("not read yet")
	}
	r := server.HostReport{At: time.Now()}
	r.Now = hostinfo.Sample{CPU: 12.4, RAMUsed: 8 << 30, RAMTotal: 32 << 30, Temps: []hostinfo.Temp{{Name: "cpu", C: 60}},
		GPUs: []hostinfo.GPU{
			{Util: 40, MemUsed: 4 << 30, MemTotal: 16 << 30, Temp: 72},
			{Util: 60, MemUsed: 12 << 30, MemTotal: 16 << 30, Temp: 65},
		}}
	cpu, ram, gpu, vram, temp := NodeCells(r)
	if cpu != "12%" || ram != "25%" || gpu != "50%" || vram != "50%" || temp != "72°C" {
		t.Fatalf("%s %s %s %s %s", cpu, ram, gpu, vram, temp)
	}
	// a unified-memory card: util known, no VRAM figures; no GPU at all: dashes
	r.Now.GPUs = []hostinfo.GPU{{Util: 95, MemUsed: -1, MemTotal: -1, Temp: -1}}
	r.Now.Temps = []hostinfo.Temp{{Name: "board", C: 48}}
	if _, _, gpu, vram, temp := NodeCells(r); gpu != "95%" || vram != "—" || temp != "48°C" {
		t.Fatalf("unified: %s %s %s", gpu, vram, temp)
	}
	r.Now.GPUs = nil
	if _, _, gpu, vram, _ := NodeCells(r); gpu != "—" || vram != "—" {
		t.Fatalf("no gpu: %s %s", gpu, vram)
	}
}

func TestFeature_ScoreCell(t *testing.T) {
	if ScoreCell(server.HostReport{Bench: &hostinfo.Bench{Score: 412}, Benching: true}) != "412" {
		t.Fatal("score wins over benching")
	}
	if ScoreCell(server.HostReport{Benching: true}) != "…" || ScoreCell(server.HostReport{}) != "—" {
		t.Fatal("benching / unknown")
	}
}

func TestFeature_NetCell(t *testing.T) {
	if NetCell(server.HostReport{Err: "unreachable"}) != "—" {
		t.Fatal("error should be dash")
	}
	if NetCell(server.HostReport{}) != "—" {
		t.Fatal("zero time should be dash")
	}
	r := server.HostReport{At: time.Now()}
	r.Now = hostinfo.Sample{NetRx: -1, NetTx: -1}
	if NetCell(r) != "—" {
		t.Fatal("unmeasured should be dash")
	}
	r.Now = hostinfo.Sample{NetRx: 0, NetTx: 0}
	if NetCell(r) != "—" {
		t.Fatal("zero rate should be dash")
	}
	r.Now = hostinfo.Sample{NetRx: 12.5, NetTx: 1.2}
	if got := NetCell(r); got != "↓12M ↑1.2M" {
		t.Fatalf("active rate: got %q, want '↓12M ↑1.2M'", got)
	}
	r.Now = hostinfo.Sample{NetRx: 0.05, NetTx: 0.005}
	if got := NetCell(r); got != "↓51K ↑5K" {
		t.Fatalf("kb rate: got %q, want '↓51K ↑5K'", got)
	}
	if got := Rate(12.5); got != "12.5 MB/s" {
		t.Fatalf("Rate: got %q, want '12.5 MB/s'", got)
	}
	if got := Rate(0.05); got != "51 KB/s" {
		t.Fatalf("Rate KB: got %q, want '51 KB/s'", got)
	}
	if got := Rate(0); got != "0 B/s" {
		t.Fatalf("Rate zero: got %q, want '0 B/s'", got)
	}
}

// ---- benchmark lines ----

func sampleBench() *hostinfo.Bench {
	b := &hostinfo.Bench{At: time.Now().Add(-2 * time.Hour), RAMTotal: 64 << 30, RAMBW: 60, CPUGFLOPS: 300,
		GPUs:  []hostinfo.BenchGPU{{Name: "RTX 4090 Laptop", VRAM: 16 << 30, Bandwidth: 576}, {Name: "Mystery"}, {Name: "Tuned", VRAM: 8 << 30, Bandwidth: 999, Source: "config"}},
		Notes: []string{"Mystery: bandwidth not in the table"}}
	b.FastMem, b.FastBW, b.FastWhere = 16<<30, 576, "GPU"
	b.MaxFastB, b.Speed8B, b.Score = 24, 78, 135
	b.Fits = []hostinfo.Fit{{Size: "8B", GB: 4.8, Where: "GPU", TPS: 78}, {Size: "32B", GB: 19.2, Where: "GPU+RAM", TPS: 12}, {Size: "235B", GB: 141}}
	return b
}

func TestFeature_BenchLines_NoBench(t *testing.T) {
	if l := BenchLines(server.HostReport{Benching: true}, false); len(l) != 1 || !strings.Contains(l[0], "benchmarking") {
		t.Fatalf("%q", l)
	}
	if l := BenchLines(server.HostReport{Err: "x"}, true); l != nil {
		t.Fatalf("unreachable machine: %q", l)
	}
	if l := BenchLines(server.HostReport{}, true); !strings.Contains(l[0], "press r") {
		t.Fatalf("self: %q", l)
	}
	if l := BenchLines(server.HostReport{}, false); !strings.Contains(l[0], "its fleet does that itself") || strings.Contains(l[0], "press r") {
		t.Fatalf("other: %q", l)
	}
}

func TestFeature_BenchLines_Score(t *testing.T) {
	r := server.HostReport{Bench: sampleBench()}
	l := BenchLines(r, true)
	if !strings.Contains(l[0], "score 135 = 78 tok/s") || !strings.Contains(l[0], "24B fits in GPU") || !strings.Contains(l[0], "2h ago") || !strings.Contains(l[0], "r to measure again") {
		t.Fatalf("head: %q", l[0])
	}
	if has(l, "calibrated") {
		t.Fatal("no calibration given")
	}
	if !strings.Contains(l[1], "RAM 60 GB/s") || !strings.Contains(l[1], "CPU 300 GFLOPS") ||
		!strings.Contains(l[1], "RTX 4090 Laptop 576 GB/s (spec)") || !strings.Contains(l[1], "Tuned 999 GB/s (config)") || strings.Contains(l[1], "Mystery") {
		t.Fatalf("hardware: %q", l[1])
	}
	if !strings.Contains(l[2], "RUNS  8B GPU ~78 · 32B GPU+RAM ~12 · 235B ✗") {
		t.Fatalf("runs: %q", l[2])
	}
	if !has(l, "note: Mystery") {
		t.Fatalf("notes: %q", l)
	}
	if o := BenchLines(r, false); strings.Contains(o[0], "r to measure") {
		t.Fatalf("another node's fleet measures: %q", o[0])
	}
	r.Benching = true
	if o := BenchLines(r, true); !strings.Contains(o[0], "measuring again…") {
		t.Fatalf("benching: %q", o[0])
	}
}

func TestFeature_BenchLinesCal_CalibratedLine(t *testing.T) {
	l := BenchLinesCal(server.HostReport{Bench: sampleBench()}, false, &server.Calibration{Factor: 0.82, Models: 3, Score: 111})
	if len(l) < 2 || !strings.Contains(l[1], "calibrated ≈ 111") || !strings.Contains(l[1], "×0.82") || !strings.Contains(l[1], "median of 3") {
		t.Fatalf("%q", l)
	}
}

// ---- probe line ----

func TestFeature_ProbeLine_NotProbedSelfVsOther(t *testing.T) {
	e := server.EpStatus{Endpoint: config.Endpoint{Model: "m"}}
	if ProbeLine(e, true) != "not probed (p)" {
		t.Fatal(ProbeLine(e, true))
	}
	if ProbeLine(e, false) != "not probed (its own fleet probes it)" {
		t.Fatal(ProbeLine(e, false))
	}
}

func TestFeature_ProbeLine_Results(t *testing.T) {
	at := time.Now()
	e := server.EpStatus{Endpoint: config.Endpoint{Model: "m", Caps: []string{"chat", "tools"}}, Probe: &server.ProbeResult{
		At: at, Found: []string{"chat", "tools"}, Missing: []string{"vision", "thinking"},
		Why:    map[string]string{"vision": strings.Repeat("image rejected ", 10), "thinking": "no reasoning field"},
		TTFTMS: 240, TPS: 41.6, Dim: 0}}
	got := ProbeLine(e, true)
	for _, w := range []string{"now: ✓ chat tools", "✗ vision (image rejected", "✗ thinking", "first token 240ms", "~42 tok/s"} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in %q", w, got)
		}
	}
	if strings.Contains(got, "no reasoning field") {
		t.Error("why is not shown for thinking")
	}
	if strings.Contains(got, "differs from caps") {
		t.Errorf("caps agree: %q", got)
	}
	if !strings.Contains(got, "…)") {
		t.Errorf("long reason truncated: %q", got)
	}
	// the probe found a capability the endpoint does not list: hint to apply it
	e.Caps = []string{"chat"}
	if got := ProbeLine(e, true); !strings.Contains(got, "differs from caps (apply: a / fleet probe --apply)") {
		t.Fatalf("differs: %q", got)
	}
	emb := server.EpStatus{Endpoint: config.Endpoint{Model: "e", Kind: "embed"}, Probe: &server.ProbeResult{At: at, Found: []string{"embed"}, Dim: 768}}
	if got := ProbeLine(emb, false); !strings.Contains(got, "dim 768") || strings.Contains(got, "differs") {
		t.Fatalf("embed: %q", got)
	}
	bad := server.EpStatus{Probe: &server.ProbeResult{At: at, Err: "connection refused", Found: []string{"chat"}}}
	if got := ProbeLine(bad, true); got != "probe now: connection refused" {
		t.Fatalf("error: %q", got)
	}
}

// ---- marks, suggestions, strain ----

func TestFeature_ModelMark(t *testing.T) {
	if ModelMark(server.EpStatus{}) != " " || ModelMark(server.EpStatus{Info: &server.ModelInfo{}}) != " " {
		t.Fatal("unknown")
	}
	if ModelMark(server.EpStatus{Info: &server.ModelInfo{Loaded: boolp(true)}}) != "◆" ||
		ModelMark(server.EpStatus{Info: &server.ModelInfo{Loaded: boolp(false)}}) != "◇" {
		t.Fatal("loaded / cold")
	}
}

func TestFeature_SuggestLine(t *testing.T) {
	got := SuggestLine(server.Suggestion{Model: "qwen3:32b", From: "laptop", FromTPS: 11.6, FromWhere: "GPU+RAM", To: "spark", ToTPS: 24.4, ToWhere: "GPU"})
	if got != "qwen3:32b: ~12 tok/s on laptop (GPU+RAM) → ~24 on spark (GPU)" {
		t.Fatalf("%q", got)
	}
}

func TestFeature_StrainLines(t *testing.T) {
	if StrainLines(nil) != nil {
		t.Fatal("nil")
	}
	ns := &server.NodeStrain{Strain: hostinfo.Strain{Reasons: []string{"GPU at 93°C"}, Warnings: []string{"disk / is 95% full"}}}
	got := StrainLines(ns)
	if len(got) != 2 || got[0] != "  STRAIN GPU at 93°C — routing prefers other machines" || got[1] != "  WARN  disk / is 95% full" {
		t.Fatalf("%q", got)
	}
	if len(StrainLines(&server.NodeStrain{})) != 0 {
		t.Fatal("calm")
	}
}

func TestFeature_SelfNode(t *testing.T) {
	st := server.Status{Nodes: []server.NodeStatus{{Node: "a"}, {Node: "me", Self: true}, {Node: "b"}}}
	if SelfNode(st) != "me" || SelfNode(server.Status{}) != "" {
		t.Fatal(SelfNode(st))
	}
}

// ---- node detail ----

func sparkReport() server.HostReport {
	r := server.HostReport{Node: "spark", Fleet: "fleet-spark", Version: "v1.4.0", At: time.Now()}
	r.Uptime = 3*86400 + 4*3600
	r.Static = hostinfo.Static{Host: "spark", OS: "Ubuntu 24.04", CPUName: "10× Cortex-X925 + 10× Cortex-A725", Threads: 20, Machine: "ASUSTeK GX10"}
	r.Now = hostinfo.Sample{CPU: 15, Load1: 1.5, RAMUsed: 64 << 30, RAMTotal: 128 << 30, SwapUsed: 2 << 30, SwapTotal: 16 << 30,
		Temps:    []hostinfo.Temp{{Name: "board", C: 52}, {Name: "nvme0", C: 41}},
		GPUs:     []hostinfo.GPU{{Name: "NVIDIA GB10", Util: 90, MemUsed: -1, MemTotal: -1, Temp: 70, Power: 40, PowerLimit: 100}},
		Disks:    []hostinfo.Disk{{Mount: "/", Used: 900 << 30, Total: 1000 << 30}, {Mount: "/data", Used: 1 << 30, Total: 2 << 40}},
		DiskRead: 12.5, DiskWrite: 3,
	}
	r.History = map[string][]float64{"cpu": {10, 20, 15}, "gpu": {80, 90}}
	r.Notes = []string{"nvidia-smi: slow"}
	return r
}

func TestFeature_NodeDetail_MachineUnifiedGPUBoardTemp(t *testing.T) {
	ns := server.NodeStatus{Node: "spark", Via: "⇄ spark", Up: 1, Total: 2, PingMS: 4, AvgMS: 5, MinMS: 3, MaxMS: 9, History: []int64{3, 4, 9},
		Caps: []string{"chat", "tools"}, BestTPS: 41.2, Best: "qwen3:30b"}
	l := NodeDetail(ns, sparkReport(), nil, 120)
	if l[0] != "spark · ⇄ spark · fleet fleet-spark v1.4.0 · up 3d 4h" {
		t.Fatalf("head %q", l[0])
	}
	for _, w := range []string{
		"ASUSTeK GX10 · Ubuntu 24.04 · 10× Cortex-X925 + 10× Cortex-A725 · 20 threads",
		"load 1.50", "board 52°C", "swap 2.0 GiB / 16.0 GiB", "64.0 GiB / 128.0 GiB (50%)",
		"NVIDIA GB10", "VRAM shared with RAM", "70°C", "40 W / 100 W",
		"DISK  /", "(90%)", "/data", "read 12.5 MB/s · write 3.0 MB/s", "nvme0 41°C", "note: nvidia-smi: slow",
		"PING  4ms (avg 5, 3–9)", "MODELS 1/2 up · chat tools · best 41 tok/s (qwen3:30b)",
		"its fleet does that itself",
	} {
		if !has(l, w) {
			t.Errorf("missing %q", w)
		}
	}
	if has(l, "GPU   none found") || has(l, "STRAIN") {
		t.Error("unexpected lines")
	}
	if has(l, "I/O") && has(l, "board 52°C   ") && count(l, "board") > 1 {
		t.Error("board temperature listed twice")
	}
}

func TestFeature_NodeDetail_DiscreteMultiGPUAndNoGPU(t *testing.T) {
	r := sparkReport()
	r.Static.Cores, r.Static.Machine, r.Static.CPUName = 14, "", ""
	r.Now.Temps = []hostinfo.Temp{{Name: "cpu", C: 66}}
	r.Now.GPUs = []hostinfo.GPU{
		{Index: 0, Name: "RTX 4090", Util: 10, MemUsed: 4 << 30, MemTotal: 24 << 30, Temp: -1, Power: -1},
		{Index: 1, Name: "RTX 3090", Util: 20, MemUsed: 1 << 30, MemTotal: 24 << 30, Temp: 50, Power: 120},
	}
	l := NodeDetail(server.NodeStatus{Node: "box", Via: "local", Self: true}, r, nil, 80)
	for _, w := range []string{"Ubuntu 24.04 · CPU · 14 cores / 20 threads", "66°C", "GPU0  RTX 4090", "GPU1  RTX 3090", "VRAM", "4.0 GiB / 24.0 GiB", "120 W", "PING  no sample yet"} {
		if !has(l, w) {
			t.Errorf("missing %q in %q", w, l)
		}
	}
	if has(l, "board") || has(l, "shared with RAM") || has(l, "120 W /") {
		t.Errorf("%q", l)
	}
	r.Now.GPUs = nil
	if l := NodeDetail(server.NodeStatus{Node: "box", Via: "local"}, r, nil, 80); !has(l, "GPU   none found") {
		t.Fatalf("%q", l)
	}
}

func TestFeature_NodeDetail_ErrorReadingAndIdle(t *testing.T) {
	l := NodeDetail(server.NodeStatus{Node: "far", Via: "ssh far", Idle: true}, server.HostReport{Err: "fleet not reachable"}, nil, 80)
	if l[0] != "far · ssh far" || l[1] != "  machine: fleet not reachable" || !has(l, "no sample yet · tunnel idle") {
		t.Fatalf("%q", l)
	}
	if has(l, "AI ") {
		t.Fatal("no AI line for an unreachable machine")
	}
	l = NodeDetail(server.NodeStatus{Node: "far", Via: "ssh far"}, server.HostReport{}, nil, 80)
	if l[1] != "  machine: reading…" {
		t.Fatalf("%q", l)
	}
}

func TestFeature_NodeDetailFull_StrainModelsAndMoves(t *testing.T) {
	ns := server.NodeStatus{Node: "laptop", Via: "local", Self: true, Up: 2, Total: 3,
		Strain: &server.NodeStrain{Strain: hostinfo.Strain{Reasons: []string{"memory pressure: every task stalled 30% of the last 10 s"}, Warnings: []string{"CPU pressure: x"}}}}
	eps := []server.EpStatus{
		{Endpoint: config.Endpoint{Name: "a", Node: "laptop", Model: "qwen3:32b"}, OK: true, Known: true, Info: &server.ModelInfo{Loaded: boolp(true)}},
		{Endpoint: config.Endpoint{Name: "b", Node: "laptop", Model: "llama3:8b"}, OK: false, Known: true, Info: &server.ModelInfo{Loaded: boolp(false)}},
		{Endpoint: config.Endpoint{Name: "c", Node: "laptop", Model: "unknown-model-with-a-really-long-name-that-goes-on"}},
		{Endpoint: config.Endpoint{Name: "d", Node: "spark", Model: "other-node-model"}, OK: true, Known: true},
	}
	r := sparkReport()
	r.Probes = map[string]server.ProbeResult{"llama3:8b": {At: time.Now(), Found: []string{"chat"}}}
	sugg := []server.Suggestion{
		{Model: "qwen3:32b", From: "laptop", FromTPS: 10, FromWhere: "GPU+RAM", To: "spark", ToTPS: 25, ToWhere: "GPU"},
		{Model: "x", From: "spark", To: "laptop"},
	}
	l := NodeDetailFull(ns, r, eps, 120, sugg)
	if !has(l, "  STRAIN memory pressure") || !has(l, "routing prefers other machines") || !has(l, "  WARN  CPU pressure: x") {
		t.Fatalf("strain: %q", l)
	}
	if !has(l, "✓ ◆ qwen3:32b") || !has(l, "✗ ◇ llama3:8b") || !has(l, "?   unknown-model-with-a-really-long-…") {
		t.Fatalf("models: %q", l)
	}
	if has(l, "other-node-model") {
		t.Fatal("another node's model listed")
	}
	if !has(l, "now: ✓ chat") { // the machine's own fleet probed llama3:8b
		t.Fatalf("probe from report: %q", l)
	}
	if !has(l, "not probed (p)") {
		t.Fatalf("self: %q", l)
	}
	if count(l, "MOVE") != 1 || !has(l, "  MOVE  qwen3:32b: ~10 tok/s on laptop (GPU+RAM) → ~25 on spark (GPU)") {
		t.Fatalf("moves only for this node's models: %q", l)
	}
	// STRAIN comes before the AI line
	si, ai := -1, -1
	for i, x := range l {
		if strings.HasPrefix(x, "  STRAIN") && si < 0 {
			si = i
		}
		if strings.HasPrefix(x, "  AI") && ai < 0 {
			ai = i
		}
	}
	if si < 0 || ai < 0 || si > ai {
		t.Fatalf("order strain %d ai %d", si, ai)
	}
}

func TestFeature_NodeDetailFull_CalibrationFromLiveSpeeds(t *testing.T) {
	b := sampleBench()
	b.GPUs = []hostinfo.BenchGPU{{Name: "RTX 4090 Laptop", VRAM: 16 << 30, Bandwidth: 576}}
	b.Notes = nil
	gb := 4.8
	_, est := b.Estimate(gb, 0)
	eps := []server.EpStatus{{Endpoint: config.Endpoint{Name: "a", Node: "laptop", Model: "llama3:8b", TPS: est * 0.5}, OK: true, Known: true,
		TPSSrc: "live", Samples: 5, Info: &server.ModelInfo{Size: int64(gb * (1 << 30))}}}
	r := server.HostReport{At: time.Now(), Bench: b}
	l := NodeDetailFull(server.NodeStatus{Node: "laptop", Via: "local", Self: true}, r, eps, 100, nil)
	if !has(l, "calibrated ≈ 68: its models run ×0.50 the estimate (median of 1 with live speeds)") {
		t.Fatalf("%q", l)
	}
	// too few live samples: no calibration
	eps[0].Samples = 2
	if l := NodeDetailFull(server.NodeStatus{Node: "laptop", Via: "local"}, r, eps, 100, nil); has(l, "calibrated") {
		t.Fatalf("%q", l)
	}
}

func TestRunRowsShowStaleAndLost(t *testing.T) {
	now := time.Now()
	runs := []server.Run{
		{ID: "r1", Model: "m", At: now, Stale: "no healthy answer from g since 2026-10-30 10:00"},
		{ID: "r2", Model: "m", At: now, Ended: now, Lost: "gone"},
		{ID: "r3", Model: "m", At: now},
	}
	rows := RunRows(runs)
	if !has(rows, "stale?") || !has(rows, "no healthy answer") || !has(rows, "fleet run prune --stale") {
		t.Fatalf("%v", rows)
	}
	if !has(rows, "lost") || !has(rows, "running") {
		t.Fatalf("%v", rows)
	}
}

func TestRunRows_IDColumnWidthAndChainSpeed(t *testing.T) {
	now := time.Now()
	runs := []server.Run{
		{
			ID:      "run-20261106-053012-longid",
			Model:   "qwen35-2b-q4km.gguf",
			Engine:  "chain",
			At:      now,
			PredTPS: 0,
			Parts: []server.RunPart{
				{Node: "dino", First: 0, Layers: 24},
				{Node: "victor", First: 24, Layers: 24},
			},
		},
		{
			ID:      "r1",
			Model:   "m",
			Engine:  "ring",
			At:      now,
			PredTPS: 25.5,
			Parts:   []server.RunPart{{Node: "dino", First: 0, Layers: 10}},
		},
	}
	rows := RunRows(runs)
	if len(rows) < 3 {
		t.Fatalf("expected header + 2 rows, got %d rows: %v", len(rows), rows)
	}
	if strings.Contains(rows[1], "plan 0.0") || !strings.Contains(rows[1], "n/a") {
		t.Fatalf("expected speed 'n/a' for chain run with no PredTPS, got line: %s", rows[1])
	}
	idHeader := strings.Fields(rows[0])[0]
	if idHeader != "ID" {
		t.Fatalf("expected ID header, got %s", idHeader)
	}
	idxStateHeader := utf8.RuneCountInString(rows[0][:strings.Index(rows[0], "STATE")])
	idxStateRow1 := utf8.RuneCountInString(rows[1][:strings.Index(rows[1], "running")])
	idxStateRow2 := utf8.RuneCountInString(rows[2][:strings.Index(rows[2], "running")])
	if idxStateHeader != idxStateRow1 || idxStateRow1 != idxStateRow2 {
		t.Fatalf("columns misaligned: header STATE at %d, row1 at %d, row2 at %d\nheader: %s\nrow1:   %s\nrow2:   %s",
			idxStateHeader, idxStateRow1, idxStateRow2, rows[0], rows[1], rows[2])
	}
}

func TestRunLines_ChainLayerRangeAndNoPredTPS(t *testing.T) {
	now := time.Now()
	run := server.Run{
		ID:      "run-chain-1",
		Model:   "qwen35-2b-q4km.gguf",
		Engine:  "chain",
		At:      now,
		PredTPS: 0,
		Master:  "dino",
		Parts: []server.RunPart{
			{Node: "dino", First: 0, Layers: 24},
			{Node: "victor", First: 24, Layers: 24, Output: true},
		},
	}
	lines := RunLines(run, nil)
	text := strings.Join(lines, "\n")
	if strings.Contains(text, "blocks 0--1") {
		t.Fatalf("found invalid 'blocks 0--1':\n%s", text)
	}
	if !strings.Contains(text, "blocks 0-23") {
		t.Fatalf("expected 'blocks 0-23' on dino, got:\n%s", text)
	}
	if !strings.Contains(text, "blocks 24-47") {
		t.Fatalf("expected 'blocks 24-47' on victor, got:\n%s", text)
	}
	if strings.Contains(text, "expected 0.0 t/s") || strings.Contains(text, "plan 0.0") {
		t.Fatalf("unexpected plan 0.0 t/s in run show:\n%s", text)
	}
}

func TestStopLines_OmitsStaysInConfigWhenNotInConfig(t *testing.T) {
	repNotInConfig := server.StopReport{
		Run:      "run-chain-1",
		Endpoint: "distributed-qwen",
		InConfig: false,
		Ended:    true,
	}
	lines := StopLines(repNotInConfig)
	text := strings.Join(lines, "\n")
	if strings.Contains(text, "stays in the configuration") {
		t.Fatalf("must not say endpoint stays in configuration when InConfig is false:\n%s", text)
	}
	if !strings.Contains(text, "run record closed") {
		t.Fatalf("expected run record closed, got:\n%s", text)
	}

	repInConfig := server.StopReport{
		Run:      "run-ring-1",
		Endpoint: "distributed-qwen",
		InConfig: true,
		Ended:    true,
	}
	linesInConfig := StopLines(repInConfig)
	textInConfig := strings.Join(linesInConfig, "\n")
	if !strings.Contains(textInConfig, "stays in the configuration") {
		t.Fatalf("expected 'stays in the configuration' when InConfig is true:\n%s", textInConfig)
	}
}

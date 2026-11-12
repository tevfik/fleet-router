package hostinfo

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"
)

// ---- /proc/cpuinfo ----

func TestFeature_CPUInfo_X86ModelNameAndPhysicalCores(t *testing.T) {
	// two sockets, two cores each, hyper-threaded: 8 processors, 4 physical cores
	var b strings.Builder
	for sock := 0; sock < 2; sock++ {
		for core := 0; core < 2; core++ {
			for ht := 0; ht < 2; ht++ {
				b.WriteString("processor\t: x\nvendor_id\t: GenuineIntel\nmodel name\t: Intel(R) Xeon(R) Gold 6338\n")
				b.WriteString("physical id\t: " + string(rune('0'+sock)) + "\ncore id\t\t: " + string(rune('0'+core)) + "\n\n")
			}
		}
	}
	name, cores := parseCPUInfo(b.String())
	if name != "Intel(R) Xeon(R) Gold 6338" {
		t.Fatalf("name %q", name)
	}
	if cores != 4 {
		t.Fatalf("cores %d, want 4 (core id counted per physical id)", cores)
	}
}

func TestFeature_CPUInfo_FirstModelNameWins(t *testing.T) {
	name, _ := parseCPUInfo("model name : First CPU\nmodel name : Second CPU\n")
	if name != "First CPU" {
		t.Fatalf("%q", name)
	}
}

func dgxSparkCPUInfo() string {
	var b strings.Builder
	for i := 0; i < 20; i++ {
		part := "0xd85"
		if i >= 10 {
			part = "0xd87"
		}
		b.WriteString("processor\t: n\nBogoMIPS\t: 2000.00\nFeatures\t: fp asimd\nCPU implementer\t: 0x41\nCPU architecture: 8\nCPU variant\t: 0x0\nCPU part\t: " + part + "\nCPU revision\t: 1\n\n")
	}
	return b.String()
}

func TestFeature_CPUInfo_ARMPartsCountedPerKind(t *testing.T) {
	name, cores := parseCPUInfo(dgxSparkCPUInfo())
	if name != "10× Cortex-X925 + 10× Cortex-A725" {
		t.Fatalf("name %q", name)
	}
	if cores != 0 { // ARM lists no core id
		t.Fatalf("cores %d", cores)
	}
}

func TestFeature_CPUInfo_ARMUnknownImplementerOrPart(t *testing.T) {
	// a known part number under a different implementer is not ARM's design: shown raw
	name, _ := parseCPUInfo("CPU implementer : 0x61\nCPU part : 0xd85\n\nCPU implementer : 0x61\nCPU part : 0xd85\n")
	if name != "2× ARM part 0xd85" {
		t.Fatalf("other implementer: %q", name)
	}
	name, _ = parseCPUInfo("CPU implementer : 0x41\nCPU part : 0xfff\n")
	if name != "1× ARM part 0xfff" {
		t.Fatalf("unknown part: %q", name)
	}
	// upper-case part numbers are still named
	name, _ = parseCPUInfo("CPU implementer : 0x41\nCPU part : 0xD0C\n")
	if name != "1× Neoverse-N1" {
		t.Fatalf("upper case: %q", name)
	}
}

func TestFeature_CPUInfo_ARMModelOrHardwareNameWins(t *testing.T) {
	name, _ := parseCPUInfo("CPU implementer : 0x41\nCPU part : 0xd08\nHardware : BCM2835\nModel : Raspberry Pi 4 Model B\n")
	if name != "BCM2835" {
		t.Fatalf("%q", name)
	}
	if name, cores := parseCPUInfo(""); name != "" || cores != 0 {
		t.Fatalf("empty: %q %d", name, cores)
	}
}

// ---- machine name ----

func TestFeature_MachineName(t *testing.T) {
	for _, c := range []struct{ vendor, product, dt, want string }{
		{"ASUSTeK COMPUTER INC.", "GX10", "", "ASUSTeK GX10"},
		{"Dell Inc.", "Dell XPS 15 9530", "", "Dell XPS 15 9530"}, // vendor prefix not doubled
		{"LENOVO", "lenovo ThinkPad", "", "lenovo ThinkPad"},      // case-insensitive
		{"HP,", "Z8 G4", "", "HP Z8 G4"},
		{"Micro-Star International Co., Ltd.", "MS-7D25", "", "Micro-Star MS-7D25"},
		{"To Be Filled By O.E.M.", "X570 Taichi", "", "X570 Taichi"}, // junk vendor dropped
		{"Default string", "Default string", "NVIDIA Jetson AGX Orin", "NVIDIA Jetson AGX Orin"},
		{"ASUS", "System Product Name", "", ""},
		{"", "", "Raspberry Pi 5 Model B Rev 1.0", "Raspberry Pi 5 Model B Rev 1.0"}, // device-tree fallback
		{"", "Box", "", "Box"},
	} {
		if got := machineName(c.vendor, c.product, c.dt); got != c.want {
			t.Errorf("machineName(%q, %q, %q) = %q, want %q", c.vendor, c.product, c.dt, got, c.want)
		}
	}
}

func TestFeature_Static_ThisMachineShape(t *testing.T) {
	st := readStatic()
	if st.Host == "" || st.Threads <= 0 || st.OS == "" {
		t.Fatalf("%+v", st)
	}
	if strings.Contains(st.Host, ".") {
		t.Fatalf("host keeps only the first label: %q", st.Host)
	}
}

// ---- strain ----

func TestFeature_Strain_ParsePSI(t *testing.T) {
	some, full := parsePSI("some avg10=1.23 avg60=0.50 avg300=0.10 total=12345\nfull avg10=0.50 avg60=0.20 avg300=0.00 total=99\n")
	if some != 1.23 || full != 0.5 {
		t.Fatalf("some %v full %v", some, full)
	}
	some, full = parsePSI("some avg10=7.00 avg60=0.00 avg300=0.00 total=0\n") // /proc/pressure/cpu on older kernels
	if some != 7 || full != -1 {
		t.Fatalf("cpu: some %v full %v", some, full)
	}
	if some, full := parsePSI(""); some != -1 || full != -1 {
		t.Fatalf("no PSI: %v %v", some, full)
	}
	if some, _ := parsePSI("some avg10=x total=1\njunk\n"); some != -1 {
		t.Fatalf("malformed: %v", some)
	}
}

func TestFeature_Strain_ParseThrottle(t *testing.T) {
	got := parseThrottle("0, Not Active, Active\n1, Active, Not Active\n2, [N/A], [N/A]\nshort, line\n")
	want := []string{"GPU 0: software thermal slowdown", "GPU 1: hardware thermal slowdown"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("%q", got)
	}
	if got := parseThrottle(""); len(got) != 0 {
		t.Fatalf("empty: %q", got)
	}
	if got := parseThrottle("0, Active, Active"); len(got) != 2 {
		t.Fatalf("both: %q", got)
	}
}

func calm() Strain {
	return Strain{MemFull: 0, MemSome: 0, IOFull: 0, CPUSome: 0, GPUTemp: 50, DiskMax: 40, DiskAt: "/"}
}

func TestFeature_Strain_JudgeCalmMachine(t *testing.T) {
	st := calm()
	st.judge()
	if st.Strained() || len(st.Warnings) != 0 {
		t.Fatalf("%+v", st)
	}
	none := Strain{MemFull: -1, MemSome: -1, IOFull: -1, CPUSome: -1, GPUTemp: -1} // no PSI, no GPU
	none.judge()
	if none.Strained() || len(none.Warnings) != 0 {
		t.Fatalf("no readings: %+v", none)
	}
}

func TestFeature_Strain_MemoryFullIsAReason(t *testing.T) {
	st := calm()
	st.MemFull = MemFullLimit - 0.1
	st.MemSome = 60 // only "some" tasks waiting: not a reason
	st.judge()
	if st.Strained() {
		t.Fatalf("below the limit: %+v", st)
	}
	st.MemFull = MemFullLimit
	st.judge()
	if !st.Strained() || !strings.Contains(st.Reasons[0], "memory pressure") || !strings.Contains(st.Reasons[0], "10%") {
		t.Fatalf("at the limit: %+v", st)
	}
}

func TestFeature_Strain_HotGPUUnlessThrottleReason(t *testing.T) {
	st := calm()
	st.GPUTemp = 89.9
	st.judge()
	if st.Strained() {
		t.Fatalf("89.9°C: %+v", st)
	}
	st.GPUTemp = 90
	st.judge()
	if len(st.Reasons) != 1 || st.Reasons[0] != "GPU at 90°C" {
		t.Fatalf("90°C: %q", st.Reasons)
	}
	st.Throttle = []string{"GPU 0: hardware thermal slowdown"}
	st.judge()
	if len(st.Reasons) != 1 || st.Reasons[0] != st.Throttle[0] {
		t.Fatalf("a throttle reason replaces the temperature: %q", st.Reasons)
	}
	cool := calm()
	cool.Throttle = []string{"GPU 1: software thermal slowdown"}
	cool.judge()
	if !cool.Strained() {
		t.Fatal("a throttling card is a reason even when cool")
	}
}

func TestFeature_Strain_IOCPUDiskOnlyWarn(t *testing.T) {
	st := calm()
	st.IOFull, st.CPUSome, st.DiskMax, st.DiskAt = IOFullLimit, CPUSomeLimit, DiskLimit, "/data"
	st.judge()
	if st.Strained() {
		t.Fatalf("warnings must not steer routing: %+v", st.Reasons)
	}
	if len(st.Warnings) != 3 {
		t.Fatalf("%q", st.Warnings)
	}
	joined := strings.Join(st.Warnings, "|")
	for _, w := range []string{"disk I/O pressure", "CPU pressure", "disk /data is 90% full"} {
		if !strings.Contains(joined, w) {
			t.Errorf("missing %q in %q", w, joined)
		}
	}
	st.IOFull, st.CPUSome, st.DiskMax = IOFullLimit-1, CPUSomeLimit-1, DiskLimit-1
	st.judge()
	if len(st.Warnings) != 0 {
		t.Fatalf("below limits: %q", st.Warnings)
	}
}

func TestFeature_Strain_JudgeResetsPreviousVerdict(t *testing.T) {
	st := calm()
	st.MemFull = 50
	st.judge()
	st.judge()
	if len(st.Reasons) != 1 {
		t.Fatalf("judge twice: %q", st.Reasons)
	}
	st.MemFull = 0
	st.judge()
	if st.Strained() {
		t.Fatal("verdict kept after recovery")
	}
}

// A machine with plenty of memory may keep cold pages in swap and be perfectly fast: heavy swap use with no
// PSI stall is not a strain. Strain has no swap input at all; a reading with zero PSI is never strained.
func TestFeature_Strain_HeavySwapWithoutStallIsNotStrain(t *testing.T) {
	st := Strain{MemFull: 0, MemSome: 0, IOFull: 0, CPUSome: 0, GPUTemp: -1}
	st.judge()
	if st.Strained() {
		t.Fatalf("zero PSI must not be strained: %+v", st)
	}
}

func TestFeature_Strain_ReadThisMachineShape(t *testing.T) {
	st := ReadStrain()
	if st.At.IsZero() {
		t.Fatal("no time")
	}
	for name, v := range map[string]float64{"mem_full": st.MemFull, "mem_some": st.MemSome, "io_full": st.IOFull, "cpu_some": st.CPUSome} {
		if v != -1 && (v < 0 || v > 100) {
			t.Errorf("%s %v out of range", name, v)
		}
	}
	if st.DiskMax < 0 || st.DiskMax > 100 {
		t.Errorf("disk %v", st.DiskMax)
	}
	again := st
	again.judge()
	if again.Strained() != st.Strained() || len(again.Warnings) != len(st.Warnings) {
		t.Fatalf("verdict not reproducible: %+v vs %+v", st, again)
	}
}

// ---- benchmark estimates ----

func gpuBench() Bench {
	return Bench{RAMTotal: 64 << 30, RAMBW: 60, FastMem: 16 << 30, FastBW: 576, FastWhere: "GPU"}
}

var ramShare = 0.8

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6*math.Max(1, math.Abs(b)) }

func TestFeature_Estimate_FitsFastMemory(t *testing.T) {
	b := gpuBench()
	where, tps := b.Estimate(4.8, 0)
	if where != "GPU" || !near(tps, gpuEff*576/4.8) {
		t.Fatalf("%s %v", where, tps)
	}
	uni := Bench{RAMTotal: 128 << 30, RAMBW: 100, FastMem: 96 << 30, FastBW: 273, FastWhere: "GPU (unified memory)"}
	where, tps = uni.Estimate(42, 0)
	if where != "GPU" || !near(tps, gpuEff*273/42) {
		t.Fatalf("unified: %s %v", where, tps)
	}
}

func TestFeature_Estimate_GPUPlusRAMSplit(t *testing.T) {
	b := gpuBench()
	gb := 19.2 // 32B at Q4: more than 16 GiB of VRAM - overhead
	where, tps := b.Estimate(gb, 0)
	fast := 16.0 - overhead
	share := fast / gb
	want := 1 / ((gb*share/576)/gpuEff + (gb*(1-share)/60)/cpuEff)
	if where != "GPU+RAM" || !near(tps, want) {
		t.Fatalf("%s %v want %v", where, tps, want)
	}
	g := gpuBench()
	gpuOnly, _ := g.Estimate(4.8, 0)
	if _, small := b.Estimate(4.8, 0); tps >= small || gpuOnly != "GPU" {
		t.Fatal("a split model is slower than one in VRAM")
	}
	// RAMBW unknown: the split uses the fast bandwidth for the RAM part
	b.RAMBW = 0
	if where, tps := b.Estimate(gb, 0); where != "GPU+RAM" || tps <= 0 {
		t.Fatalf("no RAM bandwidth: %s %v", where, tps)
	}
}

func TestFeature_Estimate_RAMOnlyAndDoesNotFit(t *testing.T) {
	cpu := Bench{RAMTotal: 32 << 30, RAMBW: 50, FastMem: int64(float64(32<<30) * ramShare), FastBW: 50, FastWhere: "RAM"}
	where, tps := cpu.Estimate(4.8, 0)
	if where != "RAM" || !near(tps, cpuEff*50/4.8) {
		t.Fatalf("%s %v", where, tps)
	}
	if where, tps := cpu.Estimate(30, 0); where != "" || tps != 0 {
		t.Fatalf("too big for RAM: %s %v", where, tps)
	}
	b := gpuBench()
	if where, tps := b.Estimate(141, 0); where != "" || tps != 0 {
		t.Fatalf("too big for VRAM+RAM: %s %v", where, tps)
	}
	uni := Bench{RAMTotal: 128 << 30, FastMem: 96 << 30, FastBW: 273, FastWhere: "GPU (unified memory)"}
	if where, _ := uni.Estimate(100, 0); where != "" {
		t.Fatalf("unified memory has no RAM to spill into: %s", where)
	}
}

func TestFeature_Estimate_MoEReadsLess(t *testing.T) {
	b := gpuBench()
	_, dense := b.Estimate(12, 0)
	where, moe := b.Estimate(12, 2)
	if where != "GPU" || !near(moe, dense*6) {
		t.Fatalf("readGB < gb: %v vs dense %v", moe, dense)
	}
	_, clamped := b.Estimate(12, 50)
	if !near(clamped, dense) {
		t.Fatalf("readGB > gb must be clamped to gb: %v vs %v", clamped, dense)
	}
}

func TestFeature_Estimate_ZeroBandwidthOrSize(t *testing.T) {
	b := gpuBench()
	b.FastBW = 0
	if where, tps := b.Estimate(4.8, 0); where != "" || tps != 0 {
		t.Fatalf("zero bandwidth: %s %v", where, tps)
	}
	g := gpuBench()
	if where, tps := g.Estimate(0, 0); where != "" || tps != 0 {
		t.Fatalf("zero size: %s %v", where, tps)
	}
}

func TestFeature_Estimate_TinyVRAMNoNegativeShare(t *testing.T) {
	// a card with less VRAM than the runtime overhead has no room for layers: the model runs from RAM
	b := Bench{RAMTotal: 64 << 30, RAMBW: 60, FastMem: 1 << 30, FastBW: 200, FastWhere: "GPU"}
	where, tps := b.Estimate(4.8, 0)
	if where != "RAM" || !near(tps, cpuEff*60/4.8) {
		t.Fatalf("%s %v", where, tps)
	}
}

func TestFeature_Score_UnifiedVsDiscreteVsCPU(t *testing.T) {
	spark := Bench{RAMTotal: 128 << 30, RAMBW: 100, GPUs: []BenchGPU{{Name: "NVIDIA GB10", Bandwidth: 273, Unified: true}}}
	spark.score()
	if spark.FastWhere != "GPU (unified memory)" || spark.FastBW != 273 || spark.FastMem != int64(float64(128<<30)*0.75) {
		t.Fatalf("spark: %+v", spark)
	}
	disc := Bench{RAMTotal: 64 << 30, RAMBW: 60, GPUs: []BenchGPU{{Name: "RTX 4090", VRAM: 24 << 30, Bandwidth: 1008}}}
	disc.score()
	if disc.FastWhere != "GPU" || disc.FastMem != 24<<30 || disc.FastBW != 1008 {
		t.Fatalf("discrete: %+v", disc)
	}
	cpu := Bench{RAMTotal: 32 << 30, RAMBW: 50}
	cpu.score()
	if cpu.FastWhere != "RAM" || cpu.FastBW != 50 || cpu.FastMem != int64(float64(32<<30)*ramShare) {
		t.Fatalf("cpu: %+v", cpu)
	}
	for _, b := range []Bench{spark, disc, cpu} {
		want := int(math.Round(b.Speed8B * math.Sqrt(math.Max(b.MaxFastB, 1)/speedRefB)))
		if b.Score != want || len(b.Fits) != 7 {
			t.Errorf("%s: score %d want %d, fits %d", b.FastWhere, b.Score, want, len(b.Fits))
		}
	}
	if !(spark.MaxFastB > disc.MaxFastB && disc.MaxFastB > 0) || disc.Speed8B <= spark.Speed8B || spark.Speed8B <= cpu.Speed8B {
		t.Fatalf("ordering: spark %+v disc %+v cpu %+v", spark, disc, cpu)
	}
}

func TestFeature_Score_MultiGPUAndUnknownBandwidth(t *testing.T) {
	b := Bench{RAMTotal: 128 << 30, RAMBW: 80, GPUs: []BenchGPU{
		{Name: "RTX 3090", VRAM: 24 << 30, Bandwidth: 936}, {Name: "RTX 4090", VRAM: 24 << 30, Bandwidth: 1008}}}
	b.score()
	if b.FastMem != 48<<30 || b.FastBW != 936 {
		t.Fatalf("two cards: VRAM adds up, the slower card sets the pace: %+v", b)
	}
	u := Bench{RAMTotal: 32 << 30, RAMBW: 40, GPUs: []BenchGPU{{Name: "Mystery", VRAM: 8 << 30}}}
	u.score()
	if u.FastWhere != "GPU" || u.FastBW != 40 {
		t.Fatalf("an unknown card is counted at RAM speed: %+v", u)
	}
}

func TestFeature_SortedFits(t *testing.T) {
	b := Bench{Fits: []Fit{{Size: "70B", GB: 42, Where: "GPU+RAM"}, {Size: "235B", GB: 141}, {Size: "3B", GB: 1.8, Where: "GPU"}}}
	got := b.SortedFits()
	if len(got) != 2 || got[0].Size != "3B" || got[1].Size != "70B" {
		t.Fatalf("%+v", got)
	}
	if len((&Bench{}).SortedFits()) != 0 {
		t.Fatal("empty")
	}
}

// ---- card tables and overrides ----

func TestFeature_GPUSpec_WholeWordMatching(t *testing.T) {
	for name, want := range map[string]float64{
		"NVIDIA L4":                         300,
		"NVIDIA L40":                        864,
		"NVIDIA L40S":                       864,
		"NVIDIA RTX 4090 Laptop GPU":        576,
		"NVIDIA GeForce RTX 4070 Ti SUPER":  672,
		"NVIDIA GeForce RTX 5070 Ti Laptop": 588,
		"Tesla T4":                          320,
		"NVIDIA A10G":                       0, // "a10" is not a whole word of "a10g"
		"NVIDIA H100 PCIe":                  2000,
		"NVIDIA H100 80GB HBM3":             3350,
		"NVIDIA GH200 480GB":                4000,
		"NVIDIA RTX L4000":                  0, // "l4" inside "l4000" is not the L4
		"":                                  0,
	} {
		if got, _ := gpuSpec(name); got != want {
			t.Errorf("gpuSpec(%q) = %v, want %v", name, got, want)
		}
	}
	if _, uni := gpuSpec("NVIDIA L4"); uni {
		t.Error("L4 is discrete")
	}
	if _, uni := gpuSpec("AMD Radeon 8060S"); !uni {
		t.Error("8060S is unified (Strix Halo)")
	}
}

func TestFeature_MatchGPU(t *testing.T) {
	for _, c := range []struct {
		pat, name string
		want      bool
	}{
		{"*", "anything", true},
		{" * ", "anything", true},
		{"L4", "NVIDIA L4", true},
		{"l4", "NVIDIA L40S", false},
		{"l40s", "NVIDIA L40S", true},
		{"rtx 4090", "NVIDIA GeForce RTX 4090 Laptop GPU", true},
		{"4090", "RTX 40900", false},
		{"", "NVIDIA L4", false},
		{"gpu", "GPU gpu", true},
		{"a10", "a10g a10", true}, // second occurrence is a whole word
	} {
		if got := matchGPU(c.pat, c.name); got != c.want {
			t.Errorf("matchGPU(%q, %q) = %v, want %v", c.pat, c.name, got, c.want)
		}
	}
}

func TestFeature_OverrideFor_LongestMatchAndStar(t *testing.T) {
	o := map[string]float64{"*": 100, "rtx": 200, "rtx 4090": 900}
	if bw, ok := overrideFor(o, "NVIDIA GeForce RTX 4090"); !ok || bw != 900 {
		t.Fatalf("longest: %v %v", bw, ok)
	}
	if bw, ok := overrideFor(o, "NVIDIA RTX A6000"); !ok || bw != 200 {
		t.Fatalf("rtx: %v %v", bw, ok)
	}
	if bw, ok := overrideFor(o, "Intel Arc"); !ok || bw != 100 {
		t.Fatalf("star: %v %v", bw, ok)
	}
	if _, ok := overrideFor(map[string]float64{"l4": 300}, "NVIDIA L40S"); ok {
		t.Fatal("no whole-word match")
	}
	if _, ok := overrideFor(map[string]float64{"l40s": 0}, "NVIDIA L40S"); ok {
		t.Fatal("a zero bandwidth is not an override")
	}
	if _, ok := overrideFor(nil, "x"); ok {
		t.Fatal("nil")
	}
}

func TestFeature_Fingerprint_FollowsGPUBandwidthOverride(t *testing.T) {
	base, readable := Fingerprint(nil)
	if len(base) != 16 || readable == "" || !strings.Contains(readable, "RAM ") {
		t.Fatalf("%q %q", base, readable)
	}
	if again, _ := Fingerprint(map[string]float64{}); again != base {
		t.Fatal("not stable / empty override differs from none")
	}
	a, ra := Fingerprint(map[string]float64{"rtx 4090": 1000})
	b, _ := Fingerprint(map[string]float64{"rtx 4090": 1100})
	c, _ := Fingerprint(map[string]float64{"RTX 4090": 1000})
	if a == base || a == b {
		t.Fatalf("override does not change the fingerprint: %s %s %s", base, a, b)
	}
	if c != a {
		t.Fatal("key case should not matter")
	}
	if ra != readable {
		t.Fatal("the readable hardware does not include settings")
	}
	m1, _ := Fingerprint(map[string]float64{"a": 1, "b": 2, "c": 3})
	m2, _ := Fingerprint(map[string]float64{"c": 3, "a": 1, "b": 2})
	if m1 != m2 {
		t.Fatal("map order leaked into the fingerprint")
	}
}

func TestFeature_RunBench_OverrideMarksSourceConfig(t *testing.T) {
	if len(ReadGPUs()) == 0 {
		t.Skip("no GPU on this machine")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // measurements stop at once: only the GPU bookkeeping is checked
	b := RunBench(ctx, map[string]float64{"*": 1234})
	if len(b.GPUs) == 0 {
		t.Fatal("no GPUs in bench")
	}
	for _, g := range b.GPUs {
		if g.Source != "config" || g.Bandwidth != 1234 {
			t.Errorf("%+v", g)
		}
	}
	for _, n := range b.Notes {
		if strings.Contains(n, "not in the table") {
			t.Errorf("an overridden card is not unknown: %q", n)
		}
	}
	plain := RunBench(ctx, nil)
	for _, g := range plain.GPUs {
		if (g.Bandwidth > 0) != (g.Source == "spec") {
			t.Errorf("spec source: %+v", g)
		}
	}
	if plain.Fingerprint == b.Fingerprint {
		t.Error("fingerprint ignores the override")
	}
}

// ---- sampler ----

func TestFeature_Sampler_HistoryCapsAtKeep(t *testing.T) {
	s := &Sampler{hist: map[string][]float64{}}
	for i := 0; i < Keep+15; i++ {
		s.push("cpu", float64(i))
	}
	h := s.hist["cpu"]
	if len(h) != Keep || h[0] != 15 || h[Keep-1] != float64(Keep+14) {
		t.Fatalf("len %d first %v last %v", len(h), h[0], h[len(h)-1])
	}
}

func TestFeature_Sampler_StopsWhenNobodyLooks(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	s := &Sampler{hist: map[string][]float64{"cpu": {1, 2}}, running: true, lastAsk: time.Now().Add(-2 * idleStop)}
	done := make(chan struct{})
	go func() { s.loop(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * Every):
		t.Fatal("loop did not stop")
	}
	if s.running || len(s.hist) != 0 {
		t.Fatalf("running %v hist %v", s.running, s.hist)
	}
}

func TestFeature_Sampler_SnapshotIsACopy(t *testing.T) {
	s := New()
	sn := s.Snapshot()
	for k := range sn.History {
		sn.History[k] = append(sn.History[k][:0], -999)
	}
	again := s.Snapshot()
	for k, v := range again.History {
		for _, x := range v {
			if x == -999 {
				t.Fatalf("%s history shared with the caller", k)
			}
		}
	}
}

func TestFeature_ParseSMI_NotAvailableAndShortLines(t *testing.T) {
	gs := parseSMI("0, NVIDIA L4, [N/A], [Not Supported], 23034, [N/A], [N/A], 72.00\nbroken, line\n")
	if len(gs) != 1 {
		t.Fatalf("%+v", gs)
	}
	g := gs[0]
	if g.Name != "NVIDIA L4" || g.Util != -1 || g.MemUsed != -1 || g.MemTotal != 23034<<20 || g.Temp != -1 || g.Power != -1 || g.PowerLimit != 72 {
		t.Fatalf("%+v", g)
	}
	if len(parseSMI("")) != 0 {
		t.Fatal("empty")
	}
}

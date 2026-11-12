package hostinfo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"os/exec"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"
)

// The machine benchmark answers "how good is this machine for local models" without running one (no Ollama
// or model needed). Generating a token reads every weight once, so decode speed is set by memory bandwidth
// and the largest model by memory size:
//
//   - RAM bandwidth and CPU compute are measured here (a few seconds, all threads);
//   - GPU bandwidth cannot be measured without CUDA, so it comes from the card's specification (gpuSpecs);
//     a card that is not in the table is counted by its memory only, and says so.
//
// From these: the estimated tokens/s of a dense 8B model at Q4 (Speed8B), the largest Q4 model that fits in
// the fast memory (MaxFastB), and AI score = Speed8B × √(MaxFastB / 8) — speed, weighted by how big a model
// the machine holds at that speed. A list says where each model size would run (GPU, GPU+RAM, RAM) and how
// fast. It runs when the hardware changes (Fingerprint), or when asked, and only for this machine.

// BenchGPU is one card as the benchmark saw it.
type BenchGPU struct {
	Name      string  `json:"name"`
	VRAM      int64   `json:"vram"`      // bytes (unified: 0)
	Bandwidth float64 `json:"bandwidth"` // GB/s from the specification (0: not in the table)
	Unified   bool    `json:"unified,omitempty"`
	Source    string  `json:"source,omitempty"` // "spec" (the table) or "config" (gpu_bandwidth)
	Kind      string  `json:"kind,omitempty"`   // "npu": an accelerator with its own pool that cannot spill into system RAM
}

// Fit is where a model of one size runs on this machine.
type Fit struct {
	Size  string  `json:"size"`  // "8B"
	GB    float64 `json:"gb"`    // its Q4 weights
	Where string  `json:"where"` // "GPU", "GPU+RAM", "RAM", "" (does not fit)
	TPS   float64 `json:"tps"`   // estimated decode tokens/s
}

// Bench is the result of one machine benchmark.
type Bench struct {
	At          time.Time  `json:"at"`
	Fingerprint string     `json:"fingerprint"`
	Hardware    string     `json:"hardware"` // what the fingerprint is made of, readable
	Took        float64    `json:"took_s"`
	CPUGFLOPS   float64    `json:"cpu_gflops"`            // float32 multiply-add, all threads (Go code, not SIMD-tuned)
	RAMBW       float64    `json:"ram_gbs"`               // measured copy bandwidth, GB/s
	CPUMoEBW    float64    `json:"cpu_moe_gbs,omitempty"` // measured CPU MoE GEMV streaming bandwidth, GB/s
	PCIeBW      float64    `json:"pcie_gbs,omitempty"`    // host-to-device PCIe bandwidth, GB/s
	RAMTotal    int64      `json:"ram_total"`             //
	GPUs        []BenchGPU `json:"gpus,omitempty"`
	FastMem     int64      `json:"fast_mem"` // bytes a model can use at FastBW
	FastBW      float64    `json:"fast_gbs"` // GB/s of that memory
	FastWhere   string     `json:"fast_where"`
	Speed8B     float64    `json:"speed_8b"`   // est. tokens/s, dense 8B Q4
	MaxFastB    float64    `json:"max_fast_b"` // largest Q4 model in fast memory, billions of parameters
	Score       int        `json:"score"`
	Fits        []Fit      `json:"fits"`
	Notes       []string   `json:"notes,omitempty"`
}

const (
	q4GBPerB  = 0.6 // Q4_K_M: about 0.6 GB of weights per billion parameters
	overhead  = 1.5 // GB kept free for the KV cache, buffers and the runtime
	gpuEff    = 0.65
	cpuEff    = 0.55
	speedRefB = 8.0
)

// gpuSpecs: memory bandwidth (GB/s) by card name; patterns are matched as whole words, longest first.
var gpuSpecs = []struct {
	pat     string
	gbs     float64
	unified bool
}{
	{"rtx 5090 laptop", 896, false}, {"rtx 5090", 1792, false}, {"rtx 5080 laptop", 896, false}, {"rtx 5080", 960, false},
	{"rtx 5070 ti laptop", 588, false}, {"rtx 5070 ti", 896, false}, {"rtx 5070 laptop", 384, false}, {"rtx 5070", 672, false},
	{"rtx 5060 ti", 448, false}, {"rtx 5060 laptop", 384, false}, {"rtx 5060", 448, false},
	{"rtx 4090 laptop", 576, false}, {"rtx 4090", 1008, false}, {"rtx 4080 laptop", 432, false}, {"rtx 4080 super", 736, false},
	{"rtx 4080", 717, false}, {"rtx 4070 ti super", 672, false}, {"rtx 4070 ti", 504, false}, {"rtx 4070 super", 504, false},
	{"rtx 4070 laptop", 256, false}, {"rtx 4070", 504, false}, {"rtx 4060 ti", 288, false}, {"rtx 4060 laptop", 256, false},
	{"rtx 4060", 272, false}, {"rtx 3090 ti", 1008, false}, {"rtx 3090", 936, false}, {"rtx 3080 ti laptop", 512, false},
	{"rtx 3080 ti", 912, false}, {"rtx 3080 laptop", 448, false}, {"rtx 3080", 760, false}, {"rtx 3070 ti", 608, false},
	{"rtx 3070 laptop", 448, false}, {"rtx 3070", 448, false}, {"rtx 3060 ti", 448, false}, {"rtx 3060 laptop", 336, false},
	{"rtx 3060", 360, false}, {"rtx 2080 ti", 616, false}, {"titan rtx", 672, false},
	{"rtx pro 6000", 1792, false}, {"rtx 6000 ada", 960, false}, {"rtx a6000", 768, false}, {"rtx a5000", 768, false},
	{"rtx a4000", 448, false}, {"l40s", 864, false}, {"l40", 864, false}, {"l4", 300, false}, {"a10", 600, false}, {"t4", 320, false},
	{"b200", 8000, false}, {"h200", 4800, false}, {"gh200", 4000, false}, {"h100 pcie", 2000, false}, {"h100", 3350, false},
	{"a100 80gb", 2039, false}, {"a100-sxm4-80gb", 2039, false}, {"a100", 1555, false}, {"v100", 900, false},
	{"gb10", 273, true}, {"thor", 273, true}, {"orin nano", 68, true}, {"orin", 204, true},
	{"rx 7900 xtx", 960, false}, {"rx 7900 xt", 800, false}, {"rx 7900 gre", 576, false}, {"rx 7800 xt", 624, false},
	{"rx 9070 xt", 640, false}, {"rx 9070", 640, false}, {"mi300x", 5300, false}, {"mi250", 3277, false}, {"mi210", 1638, false},
	{"8060s", 256, true},
}

func wordAt(s string, i, n int) bool {
	alnum := func(c byte) bool { return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' }
	return (i == 0 || !alnum(s[i-1])) && (i+n == len(s) || !alnum(s[i+n]))
}

// gpuSpec returns the bandwidth of a card (0 when unknown) and whether its memory is the system RAM.
func gpuSpec(name string) (float64, bool) {
	n := strings.ToLower(name)
	best, bw, uni := 0, 0.0, false
	for _, sp := range gpuSpecs {
		if len(sp.pat) <= best {
			continue
		}
		for i := strings.Index(n, sp.pat); i >= 0; {
			if wordAt(n, i, len(sp.pat)) {
				best, bw, uni = len(sp.pat), sp.gbs, sp.unified
				break
			}
			j := strings.Index(n[i+1:], sp.pat)
			if j < 0 {
				break
			}
			i += 1 + j
		}
	}
	return bw, uni
}

// ReadGPUs reads the cards once (NVIDIA through nvidia-smi, AMD through sysfs).
func ReadGPUs() []GPU {
	g, _ := nvidiaSMI()
	return append(g, amdGPUs(len(g))...)
}

// matchGPU reports whether a gpu_bandwidth key names this card ("*" names every card).
func matchGPU(pattern, name string) bool {
	p, n := strings.ToLower(strings.TrimSpace(pattern)), strings.ToLower(name)
	if p == "*" {
		return true
	}
	for i := strings.Index(n, p); i >= 0 && p != ""; {
		if wordAt(n, i, len(p)) {
			return true
		}
		j := strings.Index(n[i+1:], p)
		if j < 0 {
			break
		}
		i += 1 + j
	}
	return false
}

func overrideFor(override map[string]float64, name string) (float64, bool) {
	best, bw := -1, 0.0
	for k, v := range override {
		if matchGPU(k, name) && len(k) > best && v > 0 {
			best, bw = len(k), v
		}
	}
	return bw, best >= 0
}

// Fingerprint names the hardware the benchmark depends on (and the gpu_bandwidth settings, which change
// the result too); a different one means "benchmark again".
func Fingerprint(override map[string]float64) (fp, readable string) {
	st := readStatic()
	_, ram, _, _ := meminfo()
	parts := []string{fmt.Sprintf("%s ×%d", st.CPUName, st.Threads), fmt.Sprintf("RAM %d GiB", (ram+1<<29)>>30)}
	for _, g := range ReadGPUs() {
		p := g.Name
		if g.MemTotal > 0 {
			p += fmt.Sprintf(" %d GiB", (g.MemTotal+1<<29)>>30)
		}
		parts = append(parts, p)
	}
	for _, a := range accelerators() {
		parts = append(parts, fmt.Sprintf("%s %d GiB %g MHz", a.Name, (a.PoolBytes+1<<29)>>30, a.ClockMHz))
	}
	readable = strings.Join(parts, " · ")
	keys := make([]string, 0, len(override))
	for k := range override {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	extra := ""
	for _, k := range keys {
		extra += fmt.Sprintf("|%s=%g", strings.ToLower(k), override[k])
	}
	h := sha256.Sum256([]byte(readable + extra))
	return hex.EncodeToString(h[:8]), readable
}

// RunBench measures this machine (about a second of every core) and works out the score. override gives
// the bandwidth (GB/s) of cards by name, for cards the table does not know (config gpu_bandwidth).
func RunBench(ctx context.Context, override map[string]float64) Bench {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	t0 := time.Now()
	b := Bench{At: t0}
	b.Fingerprint, b.Hardware = Fingerprint(override)
	_, b.RAMTotal, _, _ = meminfo()
	b.RAMBW = ramBandwidth(ctx)
	b.CPUMoEBW = cpuMoEBandwidth(ctx)
	b.PCIeBW = nvidiaPCIe()
	b.CPUGFLOPS = cpuGFLOPS(ctx)
	debug.FreeOSMemory() // the bandwidth buffers are not kept
	for _, g := range ReadGPUs() {
		bw, uni := gpuSpec(g.Name)
		bg := BenchGPU{Name: g.Name, Bandwidth: bw, Unified: uni}
		if bw > 0 {
			bg.Source = "spec"
		}
		if o, ok := overrideFor(override, g.Name); ok {
			bw, bg.Bandwidth, bg.Source = o, o, "config"
		}
		if g.MemTotal > 0 && !uni {
			bg.VRAM = g.MemTotal
		}
		if g.MemTotal <= 0 && !uni && bw == 0 { // memory not reported and not a known card
			bg.Unified = true
		}
		b.GPUs = append(b.GPUs, bg)
		if bw == 0 {
			b.Notes = append(b.Notes, g.Name+": bandwidth not in the table — counted at RAM speed (set gpu_bandwidth in the configuration)")
		}
	}
	for _, a := range accelerators() {
		b.GPUs = append(b.GPUs, a.BenchGPU())
		b.Notes = append(b.Notes, a.Notes...)
	}
	b.score()
	b.Took = math.Round(time.Since(t0).Seconds()*10) / 10
	return b
}

// score turns the measurements into the fast memory, the estimates, the score and the fit list.
func (b *Bench) score() {
	b.FastMem, b.FastBW, b.FastWhere = int64(float64(b.RAMTotal)*0.8), b.RAMBW, "RAM"
	var vram int64
	var gbw float64
	unified := false
	var npuMem int64
	var npuBW float64
	for _, g := range b.GPUs {
		bw := g.Bandwidth
		if bw == 0 {
			bw = b.RAMBW
		}
		if g.Kind == "npu" {
			npuMem += g.VRAM
			if npuBW == 0 || bw < npuBW {
				npuBW = bw
			}
			continue
		}
		if g.Unified {
			unified = true
			gbw = max(gbw, bw)
			continue
		}
		vram += g.VRAM
		if gbw == 0 || bw < gbw { // layers split over cards: each token crosses every card in turn
			gbw = bw
		}
	}
	switch {
	case unified: // the GPU uses the system RAM (DGX Spark, Strix Halo, Jetson)
		b.FastMem, b.FastBW, b.FastWhere = int64(float64(b.RAMTotal)*0.75), gbw, "GPU (unified memory)"
	case vram > 0:
		b.FastMem, b.FastBW, b.FastWhere = vram, gbw, "GPU"
	case npuMem > 0: // only when no other accelerator: the NPU's pool is the whole of the fast memory
		b.FastMem, b.FastBW, b.FastWhere = npuMem, npuBW, "NPU"
	}
	b.MaxFastB = math.Max(0, math.Floor((float64(b.FastMem)/(1<<30)-b.overheadGB())/q4GBPerB))
	_, b.Speed8B = b.Estimate(speedRefB*q4GBPerB, 0)
	b.Speed8B = math.Round(b.Speed8B*10) / 10
	b.Score = int(math.Round(b.Speed8B * math.Sqrt(math.Max(b.MaxFastB, 1)/speedRefB)))
	b.Fits = nil
	for _, n := range []float64{3, 8, 14, 32, 70, 120, 235} {
		gb := n * q4GBPerB
		where, tps := b.Estimate(gb, 0)
		b.Fits = append(b.Fits, Fit{Size: fmt.Sprintf("%gB", n), GB: math.Round(gb*10) / 10, Where: where, TPS: math.Round(tps*10) / 10})
	}
}

// Estimate says where a model whose weights take gb gigabytes runs on this machine ("GPU", "GPU+RAM",
// "RAM", "" = does not fit) and its decode tokens/s. readGB is what one token reads (a mixture-of-experts
// model only reads its active experts); 0 means all of gb.
func (b *Bench) Estimate(gb, readGB float64) (string, float64) {
	if gb <= 0 || b.FastBW <= 0 {
		return "", 0
	}
	if readGB <= 0 || readGB > gb {
		readGB = gb
	}
	ramGB := float64(b.RAMTotal) / (1 << 30)
	fast := float64(b.FastMem)/(1<<30) - b.overheadGB()
	eff := cpuEff
	switch b.FastWhere {
	case "NPU":
		eff = 1 // the NPU's bandwidth figure is already the effective weight read rate [PDF]
	case "RAM":
	default:
		eff = gpuEff
	}
	ramBW := b.RAMBW
	if ramBW <= 0 {
		ramBW = b.FastBW
	}
	switch {
	case gb <= fast:
		return gpuOrRAM(b.FastWhere), eff * b.FastBW / readGB
	case b.FastWhere == "GPU" && fast > 0 && gb <= fast+ramGB*0.8-overhead: // the rest of the layers on the CPU
		share := fast / gb // the part of every token read from VRAM
		cpuBW := ramBW
		if b.CPUMoEBW > 0 && readGB < gb {
			cpuBW = b.CPUMoEBW
		}
		// For MoE architectures (readGB < gb), the GPU Hot Expert Cache (SLRU) serves hot experts
		// directly from VRAM at ~85% hit rate, dramatically reducing PCIe bus traffic.
		if readGB < gb {
			share = math.Min(0.90, math.Max(share, 0.85))
		}
		secs := (readGB*share/b.FastBW)/gpuEff + (readGB*(1-share)/cpuBW)/cpuEff
		return "GPU+RAM", 1 / secs
	case b.FastWhere == "GPU" && gb <= ramGB*0.8-overhead:
		cpuBW := ramBW
		if b.CPUMoEBW > 0 && readGB < gb {
			cpuBW = b.CPUMoEBW
		}
		return "RAM", cpuEff * cpuBW / readGB
	}
	return "", 0
}

func gpuOrRAM(where string) string {
	if where == "RAM" || where == "NPU" {
		return where
	}
	return "GPU"
}

// overheadGB is the memory kept free in the fast memory. The NPU's pool is filled to the last byte in the
// measured runs [PDF], so none is held back there.
func (b *Bench) overheadGB() float64 {
	if b.FastWhere == "NPU" {
		return 0
	}
	return overhead
}

// ramBandwidth copies a buffer far larger than any cache with every thread and reports GB/s (read + write).
func ramBandwidth(ctx context.Context) float64 {
	const size = 256 << 20
	src, dst := make([]byte, size), make([]byte, size)
	for i := 0; i < size; i += 4096 { // touch every page so the copy measures memory, not page faults
		src[i], dst[i] = 1, 1
	}
	n := runtime.NumCPU()
	chunk := size / n
	best := 0.0
	for round := 0; round < 3 && ctx.Err() == nil; round++ {
		var wg sync.WaitGroup
		t0 := time.Now()
		for w := 0; w < n; w++ {
			lo, hi := w*chunk, (w+1)*chunk
			wg.Add(1)
			go func() {
				defer wg.Done()
				for k := 0; k < 4; k++ {
					copy(dst[lo:hi], src[lo:hi])
				}
			}()
		}
		wg.Wait()
		if s := time.Since(t0).Seconds(); s > 0 {
			best = max(best, float64(2*4*chunk*n)/s/1e9)
		}
	}
	return math.Round(best*10) / 10
}

var sink float32

// benchTimeSince is replaceable by tests.
var benchTimeSince = time.Since

// cpuGFLOPS runs a register-bound float32 multiply-add loop on every thread for about a second.
func cpuGFLOPS(ctx context.Context) float64 {
	if ctx.Err() != nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	n := runtime.NumCPU()
	if n < 1 {
		n = 1
	}
	const iters = 20_000_000
	const batch = 100_000
	var wg sync.WaitGroup
	sums := make([]float32, n)
	counts := make([]int64, n)
	t0 := time.Now()
	for w := 0; w < n; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			a0, a1, a2, a3 := float32(worker), float32(1), float32(2), float32(3)
			x, y := float32(0.999999), float32(0.000001)
			var done int64
			for done < iters && ctx.Err() == nil {
				chunk := int64(batch)
				if rem := iters - done; rem < chunk {
					chunk = rem
				}
				for i := int64(0); i < chunk; i++ {
					a0 = a0*x + y
					a1 = a1*x + y
					a2 = a2*x + y
					a3 = a3*x + y
				}
				done += chunk
			}
			counts[worker] = done
			sums[worker] = a0 + a1 + a2 + a3
		}(w)
	}
	wg.Wait()
	secs := benchTimeSince(t0).Seconds()
	for _, v := range sums {
		sink += v
	}
	var totalIters int64
	for _, c := range counts {
		totalIters += c
	}
	if totalIters == 0 || secs <= 0 {
		return 0
	}
	flops := float64(totalIters) * 4 * 2
	gflops := flops / secs / 1e9
	return math.Round(gflops*10) / 10
}

// cpuMoEBandwidth measures the streaming dot-product memory read bandwidth across all CPU threads (GB/s).
func cpuMoEBandwidth(ctx context.Context) float64 {
	const size = 128 << 20 // 128 MB weight buffer
	buf := make([]byte, size)
	for i := range buf {
		buf[i] = byte(i & 0x7f)
	}
	n := runtime.NumCPU()
	chunk := size / n
	best := 0.0
	for round := 0; round < 3 && ctx.Err() == nil; round++ {
		var wg sync.WaitGroup
		t0 := time.Now()
		var totalSum float64
		var mu sync.Mutex
		for w := 0; w < n; w++ {
			lo, hi := w*chunk, (w+1)*chunk
			wg.Add(1)
			go func() {
				defer wg.Done()
				var s float32
				for pass := 0; pass < 2; pass++ {
					for i := lo; i < hi; i += 4 {
						s += float32(buf[i]) * 0.01
					}
				}
				mu.Lock()
				totalSum += float64(s)
				mu.Unlock()
			}()
		}
		wg.Wait()
		if s := time.Since(t0).Seconds(); s > 0 {
			best = max(best, float64(2*chunk*n)/s/1e9)
		}
		sink += float32(totalSum)
	}
	return math.Round(best*10) / 10
}

// nvidiaPCIe queries the active PCIe generation and lane width via nvidia-smi and estimates H2D bandwidth (GB/s).
func nvidiaPCIe() float64 {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, "nvidia-smi", "--query-gpu=pcie.link.gen.current,pcie.link.width.current",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return 0
	}
	parts := strings.Split(strings.TrimSpace(string(b)), ",")
	if len(parts) >= 2 {
		gen := num(parts[0])
		width := num(parts[1])
		if gen > 0 && width > 0 {
			var laneGBps float64
			switch int(gen) {
			case 1:
				laneGBps = 0.250
			case 2:
				laneGBps = 0.500
			case 3:
				laneGBps = 0.985
			case 4:
				laneGBps = 1.969
			case 5:
				laneGBps = 3.938
			default:
				laneGBps = 1.969
			}
			return math.Round(width*laneGBps*0.8*10) / 10 // ~80% effective throughput
		}
	}
	return 0
}

// SortedFits returns the fits that run at all, smallest first (for compact display).
func (b *Bench) SortedFits() []Fit {
	var out []Fit
	for _, f := range b.Fits {
		if f.Where != "" {
			out = append(out, f)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].GB < out[j].GB })
	return out
}

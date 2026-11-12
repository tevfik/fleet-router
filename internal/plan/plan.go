package plan

import (
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tevfik/fleet-router/internal/hostinfo"

	"github.com/tevfik/fleet-router/internal/archcert"
)

const gib = float64(1 << 30)

// Constants of the cost model. They are estimates to be corrected by measurement (see docs/distributed.md).
const (
	DefaultFixedMS  = 1.5  // the master's own work per token besides its layers: embedding lookup, sampling, graph
	DefaultLinkGBps = 0.11 // master ↔ worker throughput when nothing is known (a 1 GbE link)
	DefaultRTTms    = 2.0  // master ↔ worker round trip when nothing is known
	// DefaultHubRTTms is the round trip assumed for a worker reached through the hub when nothing is declared: an
	// estimate (a profile's layout.hub_rtt_ms or the config's hub_rtt_ms replace it; measurements calibrate it).
	DefaultHubRTTms  = 10.0
	overheadGB       = 1.5 // memory kept free on a machine for buffers and the runtime
	diskGBps         = 1.5 // the master reading the model file (NVMe-class)
	maxSplitNodes    = 8   // above this the subsets are not enumerated
	defaultTopSplits = 10  // the best split candidates shown
	gpuEff, cpuEff   = 0.65, 0.55
)

// Node is a machine as the planner sees it.
type Node struct {
	Name     string
	CapGB    float64 // fast memory free for the model right now (after the runtime's overhead is taken off)
	BW       float64 // GB/s of that memory
	Eff      float64 // how much of BW a decode really gets (0: by the memory kind)
	RTTms    float64 // from where the plan is made (the fleet's own machine: 0) to this node, round trip; the round trip between two machines is bounded by the sum (see Options.RTT)
	LinkGBps float64 // this machine's network throughput (0: DefaultLinkGBps); a link is as fast as its slower end
	// WeightBW is the rate in GB/s at which this machine reads weights while decoding, measured or declared (0:
	// Eff × BW). It is already effective, so Eff is not applied to it. [PDF]: 34.0 at 1000 MHz, 22.6 at 600 MHz.
	WeightBW float64
	Bench    *hostinfo.Bench
	// What the machine already holds of this model (only meaningful with Options.Known).
	HasFile  bool    // the whole file is there
	CachedGB float64 // weights in its worker cache
	// The CPU-expert tier (see experttier.go): host RAM free for MoE expert tensors besides CapGB, and the rate the
	// CPU reads them at while decoding. 0: no tier (the default; WithExpertTier sets them).
	ExpertRAMGB float64
	ExpertGBps  float64
}

// rate is the GB/s at which the machine reads weights for decode.
func (n Node) rate() float64 {
	if n.WeightBW > 0 {
		return n.WeightBW
	}
	return n.eff() * n.BW
}

func (n Node) eff() float64 {
	if n.Eff > 0 {
		return n.Eff
	}
	if n.Bench != nil && n.Bench.FastWhere == "RAM" {
		return cpuEff
	}
	return gpuEff
}

// Options of one planning run.
type Options struct {
	Ctx     int     // context the endpoint will serve
	Np      int     // parallel slots (each holds its own KV cache)
	Master  string  // the machine that has the GGUF ("" : try every machine)
	FixedMS float64 // 0: DefaultFixedMS
	// RingMS is a per-token cost of the ring itself, besides the workers' compute, wire and send, when a split
	// has workers (0: none). Measured on a three-machine ring the plan came out ~8 ms short of the profile.
	RingMS float64
	Top    int // split candidates to keep (0: default)
	// Balance places the blocks so that the slowest stage is as short as it can be (the stages of a pipeline
	// that serves several streams at once work in parallel, so the slowest one sets the rate), instead of
	// filling the fastest machines first (the shortest time for one stream). Candidates are then ranked by
	// PipeTPS.
	Balance bool
	// KVBlocks: how many blocks keep a per-token KV cache (0: all of them; a hybrid model has recurrent blocks
	// that keep a fixed-size state instead). BoundaryBytes: what crosses a machine boundary per token and
	// direction (0: one f32 hidden state; some architectures send more).
	KVBlocks int
	// KVFactor: size of the KV cache data type relative to f16 (q8_0 about 0.53, q4_0 about 0.28; 0: f16, 1).
	KVFactor   float64
	CacheTypeK string
	CacheTypeV string
	// CPUMoE / MoECacheMiB model --cpu-moe + --moe-cache-mib placement on a single machine (VRAM = dense + KV + expert cache + compute buffer).
	CPUMoE        bool
	MoECacheMiB   int
	BoundaryBytes int64
	Known         bool // the file is in the model store: what each machine holds is known and the transfers are priced
	// RTT holds measured round trips in ms between two machines, "a>b" (either order), instead of the sum of
	// the two machines' own figures.
	RTT map[string]float64
}

const (
	// DefaultMoECacheMiB is the default GPU hot expert cache size when --cpu-moe is enabled without --moe-cache-mib.
	DefaultMoECacheMiB = 3300
	// MoEComputeBufferGB is the GPU compute buffer allowance for single-node --cpu-moe placement.
	MoEComputeBufferGB = 0.5
)

// KVCacheFactor is the size of a KV cache element of the given types relative to f16 (K and V averaged).
func KVCacheFactor(k, v string) float64 {
	one := func(t string) float64 {
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "", "f16", "bf16":
			return 1
		case "f32":
			return 2
		case "q8_0":
			return 34.0 / 32 / 2
		case "q5_1":
			return 24.0 / 32 / 2
		case "q5_0":
			return 22.0 / 32 / 2
		case "q4_1":
			return 20.0 / 32 / 2
		case "q4_0", "iq4_nl":
			return 18.0 / 32 / 2
		}
		return 1
	}
	return (one(k) + one(v)) / 2
}

// kvScale scales the f16, every-block KV cache estimate: the share of the blocks that keep a per-token KV
// cache times the size of the cache data type relative to f16.
func (o Options) kvScale(m *Model) float64 {
	s := 1.0
	blocks := o.KVBlocks
	if blocks <= 0 && m != nil {
		blocks = m.KVBlocks
	}
	if m != nil && blocks > 0 && blocks < m.Layers {
		s = float64(blocks) / float64(m.Layers)
	}
	if o.KVFactor > 0 {
		s *= o.KVFactor
	} else if o.CacheTypeK != "" || o.CacheTypeV != "" {
		s *= KVCacheFactor(o.CacheTypeK, o.CacheTypeV)
	}
	return s
}

// MoEVRAMGB returns the GPU VRAM required for single-node MoE host-expert offload (--cpu-moe + --moe-cache-mib):
// dense weights + KV cache + hot expert cache + compute buffer.
func (o Options) MoEVRAMGB(m *Model, ctx, np int) (vramGB, denseGB, kvGB, cacheGB, computeBufGB float64) {
	if m == nil {
		return 0, 0, 0, 0, 0
	}
	denseGB = float64(m.DenseWeights()) / gib
	kvGB = m.KVGB(ctx, np) * o.kvScale(m)
	cacheMiB := o.MoECacheMiB
	if cacheMiB <= 0 {
		cacheMiB = DefaultMoECacheMiB
	}
	cacheGB = float64(cacheMiB) / 1024
	computeBufGB = MoEComputeBufferGB
	vramGB = denseGB + kvGB + cacheGB + computeBufGB
	return vramGB, denseGB, kvGB, cacheGB, computeBufGB
}

func (o Options) boundary(m *Model) int64 {
	if o.BoundaryBytes > 0 {
		return o.BoundaryBytes
	}
	return m.HiddenBytes
}

func (o Options) rtt(a, b Node) float64 {
	for _, k := range []string{a.Name + ">" + b.Name, b.Name + ">" + a.Name} {
		if v, ok := o.RTT[k]; ok {
			return v
		}
	}
	if a.RTTms == 0 && b.RTTms == 0 {
		return DefaultRTTms
	}
	return a.RTTms + b.RTTms
}

func link(a, b Node) float64 {
	la, lb := orDefault(a.LinkGBps, DefaultLinkGBps), orDefault(b.LinkGBps, DefaultLinkGBps)
	return math.Min(la, lb)
}

// Part is what one machine does in a candidate.
type Part struct {
	Node      string  `json:"node"`
	Layers    int     `json:"layers"`
	First     int     `json:"first"` // the first block it holds: blocks follow the order of the parts (the order to start the devices in)
	GB        float64 `json:"gb"`    // weights + KV it holds
	ComputeMS float64 `json:"compute_ms"`
	RTTms     float64 `json:"rtt_ms,omitempty"`
	SendMS    float64 `json:"send_ms,omitempty"`
	Output    bool    `json:"output,omitempty"`   // holds the output projection
	FetchGB   float64 `json:"fetch_gb,omitempty"` // weights it still has to receive
	// CPUExperts: the last this-many blocks of the part keep their MoE expert tensors in the machine's host RAM
	// (deploy --worker-cpu-experts NODE=FIRST-LAST, see CPUExpertBlocks).
	CPUExperts int `json:"cpu_experts,omitempty"`
}

// Candidate is one way to run the model.
type Candidate struct {
	Kind          string   `json:"kind"` // "single" | "split"
	Master        string   `json:"master"`
	Where         string   `json:"where,omitempty"` // single: GPU, GPU+RAM, RAM
	Parts         []Part   `json:"parts"`
	MasterLocalMS float64  `json:"master_local_ms"`
	TokenMS       float64  `json:"token_ms"`
	BottleneckMS  float64  `json:"bottleneck_ms,omitempty"` // the slowest stage (compute + send): the period with several streams in flight
	PipeTPS       float64  `json:"pipe_tps,omitempty"`      // 1000 / BottleneckMS
	RingMS        float64  `json:"ring_ms,omitempty"`       // the per-token ring overhead that was added (Options.RingMS)
	TPS           float64  `json:"tps"`
	LoadS         float64  `json:"load_s"`             // the first load; workers cache the weights afterwards
	FetchGB       float64  `json:"fetch_gb,omitempty"` // everything that has to move before it runs (with Known)
	Notes         []string `json:"notes,omitempty"`
}

// Result is the answer of Plan.
type Result struct {
	Model      string      `json:"model"`
	Arch       string      `json:"arch,omitempty"`
	WeightsGB  float64     `json:"weights_gb"`
	KVGB       float64     `json:"kv_gb"`
	Ctx        int         `json:"ctx"`
	Np         int         `json:"np"`
	SHA256     string      `json:"sha256,omitempty"` // the registered file the plan is for
	Known      bool        `json:"known"`            // the file is in the model store, the transfers are priced
	HasMTP     bool        `json:"has_mtp,omitempty"`
	SpecType   string      `json:"spec_type,omitempty"`
	SpecDesc   string      `json:"spec_desc,omitempty"`
	Candidates []Candidate `json:"candidates"` // best first
	Verdict    string      `json:"verdict"`
	Skipped    []string    `json:"skipped,omitempty"`  // machines left out, and why
	Warnings   []string    `json:"warnings,omitempty"` // e.g. the architecture never passed the ring's correctness gate
}

// KVGB is the KV cache of ctx tokens in np slots over all blocks.
func (m *Model) KVGB(ctx, np int) float64 {
	return float64(m.KVPerTokenLayer) * float64(m.Layers) * float64(ctx) * float64(max(np, 1)) / gib
}

// Plan lists the ways to run m on nodes, best first.
func Plan(m *Model, nodes []Node, o Options) Result {
	if o.Ctx <= 0 {
		o.Ctx = min(m.CtxMax, 8192)
		if o.Ctx <= 0 {
			o.Ctx = 8192
		}
	}
	if o.Np <= 0 {
		o.Np = 1
	}
	if o.FixedMS <= 0 {
		o.FixedMS = DefaultFixedMS
	}
	if o.Top <= 0 {
		o.Top = defaultTopSplits
	}
	specType, _, specDesc := m.Speculation()
	r := Result{
		Model:     m.Name,
		Arch:      m.Arch,
		WeightsGB: float64(m.Weights()) / gib,
		KVGB:      m.KVGB(o.Ctx, o.Np) * o.kvScale(m),
		Ctx:       o.Ctx,
		Np:        o.Np,
		Known:     o.Known,
		HasMTP:    m.HasMTP,
		SpecType:  specType,
		SpecDesc:  specDesc,
	}
	r.Warnings = append(r.Warnings, ShardWarnings(m)...)
	var usable []Node
	for _, n := range nodes {
		if n.BW <= 0 {
			r.Skipped = append(r.Skipped, n.Name+": no memory figures (benchmark it first: fleet nodes bench there)")
			continue
		}
		if n.CapGB <= 0 {
			r.Skipped = append(r.Skipped, n.Name+": no free GPU/system memory right now (other models in use)")
			continue
		}
		usable = append(usable, n)
	}
	sort.Slice(usable, func(i, j int) bool { return usable[i].Name < usable[j].Name })

	var singles, splits []Candidate
	for _, n := range usable {
		if o.Master != "" && n.Name != o.Master {
			continue
		}
		if c, ok := single(m, n, o); ok {
			singles = append(singles, c)
		}
	}
	if len(usable) > 1 && len(usable) <= maxSplitNodes {
		for _, mn := range usable {
			if o.Master != "" && mn.Name != o.Master {
				continue
			}
			splits = append(splits, splitsFor(m, mn, usable, o)...)
		}
	} else if len(usable) > maxSplitNodes {
		r.Skipped = append(r.Skipped, fmt.Sprintf("more than %d machines: splits are not enumerated", maxSplitNodes))
	}
	rate := func(c Candidate) float64 {
		if o.Balance && c.PipeTPS > 0 {
			return c.PipeTPS
		}
		return c.TPS
	}
	byTPS := func(c []Candidate) {
		sort.SliceStable(c, func(i, j int) bool { return rate(c[i]) > rate(c[j]) })
	}
	byTPS(singles)
	byTPS(splits)
	if len(splits) > o.Top {
		splits = splits[:o.Top]
	}
	r.Candidates = append(append(r.Candidates, singles...), splits...)
	byTPS(r.Candidates)
	hasBench := false
	for _, n := range nodes {
		if n.BW > 0 {
			hasBench = true
			break
		}
	}
	r.Verdict = verdict(r, singles, splits, hasBench)
	for _, c := range r.Candidates {
		if c.Kind == "split" { // one machine needs no ring, so no gate
			if w := archcert.Warn(m.Arch, ""); w != "" {
				r.Warnings = append(r.Warnings, w)
			}
			break
		}
	}
	return r
}

// single: the model on one machine, with whatever offload that machine's benchmark estimates.
func single(m *Model, n Node, o Options) (Candidate, bool) {
	if n.Bench == nil {
		return Candidate{}, false
	}
	kvGB := m.KVGB(o.Ctx, o.Np) * o.kvScale(m)
	gb := float64(m.Weights())/gib + kvGB
	var read float64
	for _, b := range m.LayerRead {
		read += float64(b)
	}
	read = (read + float64(m.OutputBytes)) / gib

	moeOffload := (o.CPUMoE || o.MoECacheMiB > 0) && m.ExpertWeights() > 0 && n.Bench.FastWhere == "GPU" && n.Bench.FastMem > 0
	var vramNeedGB, denseGB, cacheGB, computeBufGB float64
	if moeOffload {
		vramNeedGB, denseGB, _, cacheGB, computeBufGB = o.MoEVRAMGB(m, o.Ctx, o.Np)
		gpuFastGB := float64(n.Bench.FastMem)/gib - overheadGB
		if vramNeedGB > gpuFastGB {
			return Candidate{}, false
		}
	}

	where, tps := n.Bench.Estimate(gb, read)
	if moeOffload && (where == "" || tps <= 0) && read > 0 {
		// With --cpu-moe + --moe-cache-mib, VRAM holds dense + KV + cache + compute buffer while cold experts are mapped from RAM/disk.
		ramBW := n.Bench.RAMBW
		if ramBW <= 0 {
			ramBW = n.Bench.FastBW
		}
		if n.Bench.CPUMoEBW > 0 {
			ramBW = n.Bench.CPUMoEBW
		}
		share := 0.85
		if eGB := float64(m.ExpertWeights()) / gib; eGB > 0 {
			hit := math.Min(0.90, math.Max(0.50, cacheGB/eGB+0.60))
			denseShare := math.Min(1.0, denseGB/math.Max(read, 1e-6))
			share = math.Min(0.92, denseShare+(1-denseShare)*hit)
		}
		secs := (read*share/n.Bench.FastBW)/gpuEff + (read*(1-share)/ramBW)/cpuEff
		if secs > 0 {
			where, tps = "GPU+RAM", 1/secs
		}
	}
	if where == "" || tps <= 0 {
		return Candidate{}, false
	}
	c := Candidate{Kind: "single", Master: n.Name, Where: where, TPS: tps, TokenMS: 1000 / tps,
		Parts: []Part{{Node: n.Name, Layers: m.Layers, GB: gb, ComputeMS: 1000 / tps, Output: true}}, LoadS: gb / diskGBps}
	if moeOffload {
		c.Where = "GPU+RAM"
		c.Notes = append(c.Notes, fmt.Sprintf("--cpu-moe VRAM budget %.1f GiB (dense %.1f + KV %.1f + MoE cache %.1f + compute buffer %.1f GiB)", vramNeedGB, denseGB, kvGB, cacheGB, computeBufGB))
	} else if where != "GPU" && where != "NPU" {
		if m.Experts > 0 && where == "GPU+RAM" {
			c.Notes = append(c.Notes, "Hot Expert Cache (SLRU): hot experts cached in GPU VRAM, cold experts hosted in system RAM")
		} else {
			c.Notes = append(c.Notes, "runs from "+where+": part of the weights is read from system memory")
		}
	}
	if o.Known && !n.HasFile {
		fg := float64(m.Weights()) / gib
		c.FetchGB, c.Parts[0].FetchGB = fg, fg
		c.LoadS += fg / orDefault(n.LinkGBps, DefaultLinkGBps)
		c.Notes = append(c.Notes, fmt.Sprintf("%s does not have the file: %.1f GB to fetch first", n.Name, fg))
	}
	return c, true
}

// splitsFor prices the split with mn as the master over every non-empty subset of the other machines.
func splitsFor(m *Model, mn Node, all []Node, o Options) []Candidate {
	var others []Node
	for _, n := range all {
		if n.Name != mn.Name {
			others = append(others, n)
		}
	}
	var out []Candidate
	for mask := 1; mask < 1<<len(others); mask++ {
		var ws []Node
		for i, n := range others {
			if mask>>i&1 == 1 {
				ws = append(ws, n)
			}
		}
		// The output projection on the last worker sends the logits (vocabulary × 4 bytes, ~1 MB for a 250k vocabulary)
		// back every token; on the master only the hidden state comes back. Both placements are candidates.
		for _, outMaster := range []bool{false, true} {
			if outMaster && m.Runtime == RuntimeStagePipe {
				continue // a stage chain runs the output head on its last stage
			}
			if c, ok := allocate(m, mn, ws, o, outMaster); ok {
				out = append(out, c)
			}
		}
	}
	return out
}

// allocate places the blocks: fastest machines first, each up to what its memory holds. Every worker of the
// subset must take part (a smaller subset is its own candidate), so each one pays its round trip for a reason.
func allocate(m *Model, mn Node, ws []Node, o Options, outMaster bool) (Candidate, bool) {
	_, layerGB, layerRead, outGB := blockFigures(m, o)
	sec := func(n Node) float64 { return layerRead / n.rate() } // seconds per block

	sort.SliceStable(ws, func(i, j int) bool { return sec(ws[i]) < sec(ws[j]) })
	last := len(ws) - 1 // the output projection sits on the last machine of the ring
	capOf := func(n Node, idx int) float64 {
		c := n.CapGB
		if idx == -1 { // the master keeps the embedding table
			c -= float64(m.EmbdBytes) / gib
			if outMaster {
				c -= outGB
			}
		}
		if idx == last && !outMaster {
			c -= outGB
		}
		return c
	}
	type slot struct {
		n   Node
		idx int
	}
	order := []slot{{mn, -1}}
	for i, w := range ws {
		order = append(order, slot{w, i})
	}
	sort.SliceStable(order, func(i, j int) bool { return sec(order[i].n) < sec(order[j].n) })
	left := m.Layers
	layers := map[string]int{}
	if len(ws) > 1 && left >= len(order) {
		// Ensure each participant in a multi-worker candidate can hold at least 1 layer
		for _, s := range order {
			if capOf(s.n, s.idx) < layerGB {
				return Candidate{}, false
			}
		}
		// Reserve 1 layer for each worker so multi-machine pipelines (3+ nodes) are not starved
		for _, w := range ws {
			layers[w.Name] = 1
			left--
		}
	}
	if o.Balance {
		// Each next block goes to the machine that would finish its share first (blocks are alike, so this
		// minimises the slowest stage); a machine that is full takes no more.
		busy := make([]float64, len(order)) // seconds already placed on each machine
		for i, s := range order {
			busy[i] = float64(layers[s.n.Name]) * sec(s.n)
			if s.idx == -1 {
				busy[i] += o.FixedMS / 1000 // the master's own work per token counts in its stage
			}
			if (s.idx == last && !outMaster) || (s.idx == -1 && outMaster) {
				busy[i] += outGB / s.n.rate()
			}
		}
		for ; left > 0; left-- {
			best := -1
			for i, s := range order {
				if layers[s.n.Name] >= int(math.Floor(capOf(s.n, s.idx)/layerGB)) {
					continue
				}
				if best < 0 || busy[i]+sec(s.n) < busy[best]+sec(order[best].n) {
					best = i
				}
			}
			if best < 0 {
				break
			}
			layers[order[best].n.Name]++
			busy[best] += sec(order[best].n)
		}
	} else {
		// Distribute remaining layers greedily to fastest machines up to capacity
		for _, s := range order {
			maxExtra := int(math.Floor(capOf(s.n, s.idx) / layerGB))
			if cur, ok := layers[s.n.Name]; ok {
				maxExtra -= cur
			}
			k := max(0, min(maxExtra, left))
			layers[s.n.Name] += k
			left -= k
		}
	}
	// The blocks that did not fit the GPUs: a worker with the CPU-expert tier takes more of them by keeping the
	// experts of some of its blocks in host RAM (a mixture of experts only; never when everything fits).
	cpuExp := map[string]int{}
	if eGB, _ := expertGB(m); left > 0 && eGB > 0 {
		for _, s := range order {
			if s.idx == -1 || left == 0 {
				continue
			}
			if x, c := expertExtra(s.n, capOf(s.n, s.idx), layerGB, eGB, layers[s.n.Name], left); x > 0 {
				layers[s.n.Name] += x
				cpuExp[s.n.Name] = c
				left -= x
			}
		}
	}
	if left > 0 {
		return Candidate{}, false
	}
	for _, w := range ws {
		if layers[w.Name] == 0 {
			return Candidate{}, false
		}
	}
	return price(m, mn, ws, layers, cpuExp, o, outMaster), true
}

// blockFigures: the weights of one block in GB, the same with its share of the KV cache, the GB one block reads per
// token, and the size of the output projection in GB.
func blockFigures(m *Model, o Options) (layerW, layerGB, layerRead, outGB float64) {
	L := float64(m.Layers)
	for i := range m.LayerBytes {
		layerGB += float64(m.LayerBytes[i])
		layerRead += float64(m.LayerRead[i])
	}
	layerW = layerGB / L / gib
	layerGB = layerW + float64(m.KVPerTokenLayer)*float64(o.Ctx)*float64(o.Np)*o.kvScale(m)/gib
	layerRead = layerRead / L / gib
	outGB = float64(m.OutputBytes) / gib
	return
}

// price works out a split whose blocks are placed: mn is the master, ws the workers in ring order (the output
// projection sits on the last one unless outMaster), layers the blocks of every machine, cpuExp the CPU-expert blocks.
func price(m *Model, mn Node, ws []Node, layers, cpuExp map[string]int, o Options, outMaster bool) Candidate {
	layerW, layerGB, layerRead, outGB := blockFigures(m, o)
	sec := func(n Node) float64 { return layerRead / n.rate() }
	last := len(ws) - 1
	c := Candidate{Kind: "split", Master: mn.Name}
	c.MasterLocalMS = float64(layers[mn.Name])*sec(mn)*1000 + o.FixedMS
	masterGB := float64(layers[mn.Name])*layerGB + float64(m.EmbdBytes)/gib
	if outMaster {
		c.MasterLocalMS += outGB / mn.rate() * 1000
		masterGB += outGB
		c.Notes = append(c.Notes, "output projection on the master: only the hidden state comes back from the workers, not the logits")
	}
	c.Parts = append(c.Parts, Part{Node: mn.Name, Layers: layers[mn.Name], GB: masterGB, ComputeMS: c.MasterLocalMS - o.FixedMS, Output: outMaster})
	var workers []WorkerTerm
	var ship float64 // seconds the master spends sending weights
	for i, w := range ws {
		p := Part{Node: w.Name, Layers: layers[w.Name], GB: float64(layers[w.Name]) * layerGB}
		need := float64(layers[w.Name]) * layerW // weights this machine must end up with
		comp := float64(layers[w.Name]) * sec(w) * 1000
		if c := cpuExp[w.Name]; c > 0 {
			eGB, share := expertGB(m)
			p.CPUExperts = c
			comp += float64(c) * eGB * share * (1/w.ExpertGBps - 1/w.rate()) * 1000
		}
		back := float64(o.boundary(m)) // what returns to the master per token
		if i == last && !outMaster {
			p.Output, p.GB = true, p.GB+outGB
			need += outGB
			comp += outGB / w.rate() * 1000
			if m.Vocab > 0 {
				back = float64(m.Vocab) * 4 // the logits (f32)
			}
		}
		p.ComputeMS = comp
		p.RTTms = o.rtt(mn, w)
		lk := link(mn, w)
		p.SendMS = (float64(o.boundary(m)) + back) / (lk * 1e9) * 1000
		workers = append(workers, WorkerTerm{p.ComputeMS, p.RTTms, p.SendMS})
		p.FetchGB = need // nothing known: all of it travels
		if o.Known {
			if w.HasFile {
				p.FetchGB = 0
			} else {
				p.FetchGB = math.Max(0, need-w.CachedGB)
			}
		}
		c.FetchGB += p.FetchGB
		ship += p.FetchGB / lk
		c.Parts = append(c.Parts, p)
	}
	next := 0 // the blocks follow the order of the parts: master first, the workers by speed, the output on the last
	for i := range c.Parts {
		c.Parts[i].First = next
		next += c.Parts[i].Layers
		if c.Parts[i].CPUExperts > 0 {
			c.Notes = append(c.Notes, expertNote(c.Parts[i]))
		}
	}
	c.TokenMS = TokenMS(c.MasterLocalMS, workers)
	c.BottleneckMS = c.MasterLocalMS
	for _, w := range workers {
		c.BottleneckMS = math.Max(c.BottleneckMS, w.ComputeMS+w.SendMS)
	}
	c.PipeTPS = 1000 / c.BottleneckMS
	if len(workers) > 0 && o.RingMS > 0 {
		c.RingMS = o.RingMS
		c.TokenMS += o.RingMS
	}
	c.TPS = 1000 / c.TokenMS
	c.LoadS = math.Max(float64(layers[mn.Name])*layerW/diskGBps, ship) // the master reads its share while it sends the others'
	if o.Known && !mn.HasFile {
		fg := float64(m.Weights()) / gib
		c.FetchGB += fg
		c.LoadS += fg / orDefault(mn.LinkGBps, DefaultLinkGBps)
		c.Notes = append(c.Notes, fmt.Sprintf("master %s does not have the file: %.1f GB to fetch first", mn.Name, fg))
	}
	c.Notes = append(c.Notes, "workers cache the weights: the first load is slow, later ones fast")
	return c
}

func orDefault(v, d float64) float64 {
	if v > 0 {
		return v
	}
	return d
}

// WorkerTerm is one worker's part of a token: its compute, the round trip to it and what is sent.
type WorkerTerm struct{ ComputeMS, RTTms, SendMS float64 }

// TokenMS is the cost model measured on llama-ring: T ≈ master_local + Σ_worker (compute + RTT + send).
func TokenMS(masterLocalMS float64, ws []WorkerTerm) float64 {
	t := masterLocalMS
	for _, w := range ws {
		t += w.ComputeMS + w.RTTms + w.SendMS
	}
	return t
}

func verdict(r Result, singles, splits []Candidate, hasBench bool) string {
	switch {
	case len(r.Candidates) == 0:
		if !hasBench {
			return "no benchmark data yet: no machine has memory bandwidth figures (benchmark with 'fleet nodes bench')"
		}
		return fmt.Sprintf("does not fit: the model needs about %.1f GB (weights %.1f + KV cache %.1f for %d × %d tokens) and no machine, or combination of machines, has that much free memory", r.WeightsGB+r.KVGB, r.WeightsGB, r.KVGB, r.Np, r.Ctx)
	case len(singles) == 0:
		b := splits[0]
		return fmt.Sprintf("fits no single machine; the best split (master %s over %s) gives about %.1f tok/s", b.Master, strings.Join(names(b.Parts), ", "), b.TPS)
	case len(splits) == 0 || splits[0].TPS <= singles[0].TPS:
		b := singles[0]
		s := fmt.Sprintf("run it on %s alone (%s): about %.1f tok/s", b.Master, b.Where, b.TPS)
		if len(splits) > 0 {
			s += fmt.Sprintf("; the best split would give %.1f", splits[0].TPS)
		}
		return s
	default:
		b, s := splits[0], singles[0]
		return fmt.Sprintf("a split is faster than any one machine: master %s over %s gives about %.1f tok/s (best single machine: %s, %.1f)", b.Master, strings.Join(names(b.Parts), ", "), b.TPS, s.Master, s.TPS)
	}
}

func names(ps []Part) []string {
	var n []string
	for _, p := range ps {
		n = append(n, p.Node)
	}
	return n
}

// FromHost makes a Node out of a machine's benchmark and its current reading: the memory a model can use
// now is what the benchmark says the fast memory holds, less what is in use (GPU memory in use on a card;
// system memory in use where the memory is shared with the CPU).
func FromHost(name string, b *hostinfo.Bench, now hostinfo.Sample) Node {
	n := Node{Name: name, Bench: b}
	if b == nil || b.FastBW <= 0 || b.FastMem <= 0 {
		return n
	}
	free := float64(b.FastMem)
	if b.FastWhere == "NPU" { // the pool is the limit: no overhead is held back, nothing spills into system RAM
		n.CapGB, n.BW, n.Eff = free/gib, b.FastBW, 1 // the figure is the effective weight read rate already
		return n
	}
	if b.FastWhere == "GPU" { // a card with its own memory
		for _, g := range now.GPUs {
			if g.MemUsed > 0 {
				free -= float64(g.MemUsed)
			}
		}
		gpuFree := math.Max(0, free/gib-overheadGB)
		// When GPU VRAM is full or constrained, utilize free system RAM (CPU offload)
		// so nodes with large RAM are never excluded from plans.
		if now.RAMTotal > 0 && b.RAMBW > 0 {
			ramAvail := float64(now.RAMTotal - now.RAMUsed)
			if now.RAMUsed == 0 { // fallback if RAMUsed reading not populated
				ramAvail = float64(now.RAMTotal) * 0.75
			}
			ramFree := math.Max(0, ramAvail/gib-overheadGB)
			if gpuFree <= 0 && ramFree > 0 {
				// Pure CPU / system RAM mode
				n.CapGB = ramFree
				n.BW = b.RAMBW
				return n
			} else if gpuFree > 0 && ramFree > 0 {
				// Hybrid mode: expand capacity with system RAM
				n.CapGB = gpuFree + ramFree
				n.BW = (gpuFree*b.FastBW + ramFree*b.RAMBW) / (gpuFree + ramFree)
				return n
			}
		}
		n.CapGB = gpuFree
		n.BW = b.FastBW
		return n
	} else if now.RAMTotal > 0 { // shared or system memory
		free = math.Min(free, float64(now.RAMTotal-now.RAMUsed))
	}
	n.CapGB = math.Max(0, free/gib-overheadGB)
	n.BW = b.FastBW
	return n
}

// Blocks is the part's blocks as a range ("40-43"; "" when it holds none).
func (p Part) Blocks() string {
	switch {
	case p.Layers <= 0:
		return ""
	case p.Layers == 1:
		return fmt.Sprint(p.First)
	}
	return fmt.Sprintf("%d-%d", p.First, p.First+p.Layers-1)
}

// ShardWarnings says what is wrong with the files of a multi-file model: shards that are missing (the weights are then
// an undercount) and look-alike .gguf files next to the shards that are not counted.
func ShardWarnings(m *Model) []string {
	if m == nil || m.Shards == nil {
		return nil
	}
	var w []string
	if n := len(m.Shards.Missing); n > 0 {
		w = append(w, fmt.Sprintf("%d of %d shard files are missing (%v): the weights below are an undercount and the model will not load", n, m.Shards.Total, m.Shards.Missing))
	}
	if n := len(m.Shards.Extra); n > 0 {
		w = append(w, fmt.Sprintf("%d more .gguf file(s) next to the shards (%s, %.1f GB) are not counted in the weights: a model that needs a separate file (e.g. an Engram table) needs that much more memory",
			n, filepath.Base(m.Shards.Extra[0]), float64(m.Shards.ExtraBytes)/gib))
	}
	return w
}

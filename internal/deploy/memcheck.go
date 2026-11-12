package deploy

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/plan"
)

// The memory pre-check: before anything is started, does every machine of the ring have room for the weights and
// the KV cache of the blocks it will hold? A 138 GB model started on a 121 GB machine is not refused by the engine:
// it maps the file and pages it from disk on demand, which thrashes the machine instead of failing.

const (
	gib = float64(1 << 30)
	// memHeadroomGiB is kept free on a machine for buffers, the runtime and the page cache of everything else.
	memHeadroomGiB = 1.5
	// memTightRatio: above this share of the free memory the plan is called tight (a warning, not a refusal).
	memTightRatio = 0.92
)

// NodeMem is what one machine of the ring has to hold, and what it has.
type NodeMem struct {
	Node       string  `json:"node"`
	Blocks     int     `json:"blocks"`
	WeightsGiB float64 `json:"weights_gib"` // the tensors of its blocks (the master also holds the embedding / output)
	KVGiB      float64 `json:"kv_gib"`      // the KV cache of its blocks for ctx x np (an upper bound for a model with recurrent blocks)
	NeedGiB    float64 `json:"need_gib"`

	// Single-node / master --cpu-moe + --moe-cache-mib VRAM budget (VRAM = dense + KV + MoE cache + compute buffer).
	VRAMNeedGiB   float64 `json:"vram_need_gib,omitempty"`
	DenseGiB      float64 `json:"dense_gib,omitempty"`
	MoECacheGiB   float64 `json:"moe_cache_gib,omitempty"`
	ComputeBufGiB float64 `json:"compute_buf_gib,omitempty"`

	// Live reading (Probed): memory available now. -1: not read.
	Probed       bool    `json:"probed,omitempty"`
	RAMFreeGiB   float64 `json:"ram_free_gib,omitempty"`
	VRAMFreeGiB  float64 `json:"vram_free_gib,omitempty"`
	DeclaredGiB  float64 `json:"declared_gib,omitempty"` // mem_gib of the nodes: section
	CPUOnly      bool    `json:"-"`
	recurrentKV  bool
	weightsKnown bool
}

// FreeGiB is the memory a model can use on the machine now, after the headroom (RAM and the first GPU's VRAM; on
// unified-memory machines the GPU reports none and RAM is the pool).
func (n NodeMem) FreeGiB() float64 {
	v := n.RAMFreeGiB
	if !n.CPUOnly {
		v += n.VRAMFreeGiB
	}
	return max(v-memHeadroomGiB, 0)
}

// MemFree is a live memory reading of one machine.
type MemFree struct {
	RAMGiB  float64
	VRAMGiB float64
}

// KVCacheFactor is the size of the KV cache data types (--cache-type-k/-v) relative to f16, for the planner.
func KVCacheFactor(k, v string) float64 { return plan.KVCacheFactor(k, v) }

func kvFactor(k, v string) float64 { return plan.KVCacheFactor(k, v) }

// memNeeds splits the model's weights and KV cache over the machines of the ring the way -dev / -ts will: the
// blocks in device order, in proportion to ts, the embedding and output tensors on the master. devs and ts are the
// -dev and -ts lists; ringNodes[i] is the machine behind RINGi. A device with ts 0 (the CPU of a --worker-cpu-experts
// worker) holds nothing of its own.
func memNeeds(m *plan.Model, devs []string, ts string, master string, ringNodes []string, ctx, np int, cacheK, cacheV string) []NodeMem {
	if m == nil || m.Layers <= 0 || m.Weights() <= 0 {
		return nil
	}
	fields := strings.Split(ts, ",")
	var nodes []string
	var weights []float64
	for i, d := range devs {
		if i >= len(fields) {
			break
		}
		w, _ := strconv.ParseFloat(strings.TrimSpace(fields[i]), 64)
		node := master
		if strings.HasPrefix(d, "RING") {
			k, err := strconv.Atoi(strings.TrimPrefix(d, "RING"))
			if err != nil || k < 0 || k >= len(ringNodes) {
				continue
			}
			node = ringNodes[k]
		} else if d != "CUDA0" && d != "" {
			continue
		}
		nodes = append(nodes, node)
		weights = append(weights, w)
	}
	if len(nodes) == 0 {
		nodes, weights = []string{master}, []float64{1}
	}
	counts := proportional(m.Layers, weights)
	byNode := map[string]*NodeMem{}
	var order []string
	// When m.KVBlocks is known for a hybrid model, KVGiB is already scaled to the attention-KV blocks,
	// so recurrentKV (which treats KVGiB as an unknown upper bound) is only true when KVBlocks is unknown.
	recKV := m.Recurrent && m.KVBlocks <= 0
	get := func(n string) *NodeMem {
		if x, ok := byNode[n]; ok {
			return x
		}
		x := &NodeMem{Node: n, recurrentKV: recKV, weightsKnown: true}
		byNode[n] = x
		order = append(order, n)
		return x
	}
	// llama-server's -c is the TOTAL context, shared by the -np slots (each slot gets c/np): the cache is c tokens, not
	// c x np (np stays in the signature for the callers' sake and because a unified cache is the same size).
	_ = np
	kvScale := kvFactor(cacheK, cacheV)
	if m.KVBlocks > 0 && m.KVBlocks < m.Layers {
		kvScale *= float64(m.KVBlocks) / float64(m.Layers)
	}
	kvLayer := float64(m.KVPerTokenLayer) * float64(max(ctx, 1)) * kvScale
	first := 0
	for i, n := range nodes {
		x := get(n)
		for l := first; l < first+counts[i] && l < len(m.LayerBytes); l++ {
			x.WeightsGiB += float64(m.LayerBytes[l]) / gib
		}
		x.Blocks += counts[i]
		x.KVGiB += kvLayer * float64(counts[i]) / gib
		first += counts[i]
	}
	get(master).WeightsGiB += float64(m.OtherBytes) / gib
	out := make([]NodeMem, 0, len(order))
	for _, n := range order {
		x := byNode[n]
		x.NeedGiB = x.WeightsGiB + x.KVGiB
		out = append(out, *x)
	}
	return out
}

// applyMoECacheBudget annotates the master's NodeMem with its GPU VRAM requirement (dense + KV + MoE cache + compute buffer)
// when --cpu-moe / --n-cpu-moe / --moe-cache-mib is active on an MoE model.
func applyMoECacheBudget(needs []NodeMem, m *plan.Model, master string, cpuMoE bool, ncpuMoE, moeCacheMiB int) []NodeMem {
	if m == nil || m.ExpertWeights() <= 0 || (!cpuMoE && ncpuMoE <= 0 && moeCacheMiB <= 0) {
		return needs
	}
	cacheMiB := moeCacheMiB
	if cacheMiB <= 0 {
		cacheMiB = defaultMoECacheMiB
	}
	cacheGiB := float64(cacheMiB) / 1024
	compGiB := plan.MoEComputeBufferGB
	for i := range needs {
		if needs[i].Node != master {
			continue
		}
		denseGiB := float64(m.DenseWeights()) / gib
		if needs[i].Blocks < m.Layers && m.Layers > 0 {
			expShare := float64(m.ExpertWeights()) / float64(max(m.Weights(), 1))
			denseGiB = needs[i].WeightsGiB * (1 - expShare)
		}
		needs[i].DenseGiB = denseGiB
		needs[i].MoECacheGiB = cacheGiB
		needs[i].ComputeBufGiB = compGiB
		needs[i].VRAMNeedGiB = denseGiB + needs[i].KVGiB + cacheGiB + compGiB
	}
	return needs
}

// memProbeScript prints the memory available on a machine: RAM (MemAvailable, page cache included) and the free
// VRAM of GPU 0 (empty where the GPU shares the RAM and reports none).
const memProbeScript = `awk '/^MemAvailable:/ {print "FLEET_MEM_RAM_KB " $2}' /proc/meminfo 2>/dev/null
v=$(nvidia-smi -i 0 --query-gpu=memory.free --format=csv,noheader,nounits 2>/dev/null | head -n 1 | tr -d ' ')
case "$v" in ''|*[!0-9]*) ;; *) echo "FLEET_MEM_VRAM_MIB $v" ;; esac
`

func parseMemProbe(out string) (MemFree, bool) {
	var f MemFree
	ok := false
	for _, ln := range strings.Split(out, "\n") {
		k, v, found := strings.Cut(strings.TrimSpace(ln), " ")
		n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if !found || err != nil {
			continue
		}
		switch k {
		case "FLEET_MEM_RAM_KB":
			f.RAMGiB, ok = n/(1<<20), true
		case "FLEET_MEM_VRAM_MIB":
			f.VRAMGiB = n / 1024
		}
	}
	return f, ok
}

// probeMemory reads the memory available on node now (here, or over ssh).
var probeMemory = func(ctx context.Context, node string, local bool) (MemFree, error) {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var out []byte
	var err error
	if local {
		out, err = localScript(cctx, memProbeScript)
	} else {
		out, err = sshScript(cctx, node, memProbeScript)
	}
	if err != nil {
		return MemFree{}, err
	}
	f, ok := parseMemProbe(string(out))
	if !ok {
		return MemFree{}, fmt.Errorf("no MemAvailable in the answer")
	}
	return f, nil
}

// MemReport is the outcome of the pre-check.
type MemReport struct {
	Nodes    []NodeMem
	Blockers []string // the model does not fit: the deploy is refused without --force
	Warnings []string
}

// evaluateMemory judges needs against the readings (live: node -> reading, a missing node was not read) and the
// declared mem_gib of the nodes: section. A live reading (RAM + VRAM free) refuses; a declared figure alone only warns,
// because it counts the fast memory (VRAM) and a model may spill into RAM on purpose (--cpu-moe, offload).
func evaluateMemory(needs []NodeMem, live map[string]MemFree, probeErr map[string]error, cfg *config.Config, cpuOnly bool) MemReport {
	rep := MemReport{}
	const slow = "the engine does not fail on a model that does not fit: it maps the file and pages the missing part from disk on every token (mmap), which is very slow (disk speed, not memory speed) and can make the machine unresponsive"
	for _, n := range needs {
		n.CPUOnly = cpuOnly
		if spec, ok := cfg.Node(n.Node); ok {
			n.DeclaredGiB = spec.MemGiB
		}
		if r, ok := live[n.Node]; ok {
			n.Probed, n.RAMFreeGiB, n.VRAMFreeGiB = true, r.RAMGiB, r.VRAMGiB
		}
		rep.Nodes = append(rep.Nodes, n)
		kvNote := ""
		if n.recurrentKV {
			kvNote = " (the KV figure is an upper bound: the model has recurrent blocks that keep no per-token cache)"
		}
		detail := fmt.Sprintf("%s holds %d block(s): about %.1f GiB (weights %.1f + KV cache %.1f)%s", n.Node, n.Blocks, n.NeedGiB, n.WeightsGiB, n.KVGiB, kvNote)
		if n.VRAMNeedGiB > 0 && !n.CPUOnly {
			detail += fmt.Sprintf(" [VRAM budget %.1f GiB = dense %.1f + KV %.1f + MoE cache %.1f + compute buffer %.1f]", n.VRAMNeedGiB, n.DenseGiB, n.KVGiB, n.MoECacheGiB, n.ComputeBufGiB)
		}
		switch {
		case n.Probed:
			free := n.FreeGiB()
			where := fmt.Sprintf("RAM %.1f + VRAM %.1f GiB available now, less %.1f GiB headroom", n.RAMFreeGiB, n.VRAMFreeGiB, memHeadroomGiB)
			// a model with recurrent blocks is refused only on what is certain: the weights
			cert := n.NeedGiB
			if n.recurrentKV {
				cert = n.WeightsGiB
			}
			switch {
			case cert > free:
				rep.Blockers = append(rep.Blockers, fmt.Sprintf("%s but only %.1f GiB is free (%s): it does not fit; %s", detail, free, where, slow))
			case !n.CPUOnly && n.VRAMNeedGiB > 0 && n.VRAMFreeGiB > 0 && n.VRAMNeedGiB > n.VRAMFreeGiB:
				rep.Blockers = append(rep.Blockers, fmt.Sprintf("%s but GPU 0 only has %.1f GiB VRAM free: dense + KV + MoE cache + compute buffer (%.1f GiB) exceeds VRAM", detail, n.VRAMFreeGiB, n.VRAMNeedGiB))
			case n.NeedGiB > free:
				rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s; %.1f GiB is free (%s): the KV cache may not fit", detail, free, where))
			case n.NeedGiB > memTightRatio*free:
				rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s; %.1f GiB is free (%s): tight, anything else running there can push it into paging", detail, free, where))
			}
		case n.DeclaredGiB > 0:
			why := ""
			if e := probeErr[n.Node]; e != nil {
				why = fmt.Sprintf(" (its free memory could not be read: %v)", e)
			}
			if n.NeedGiB > n.DeclaredGiB {
				rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s; mem_gib of %s is %.1f%s: unless the rest spills into system RAM on purpose it does not fit; %s", detail, n.Node, n.DeclaredGiB, why, slow))
			}
		default:
			why := "no mem_gib declared (fleet config node set " + n.Node + " --mem-gib N) and no live reading"
			if e := probeErr[n.Node]; e != nil {
				why = fmt.Sprintf("its free memory could not be read (%v) and no mem_gib is declared", e)
			}
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("memory of %s not checked: %s (%s)", n.Node, why, detail))
		}
	}
	return rep
}

// checkMemory runs the pre-check for a planned deployment: live readings of every machine of the ring (in parallel),
// then evaluateMemory. The returned error is the refusal (nil: go on); the report's warnings are for the user.
func checkMemory(ctx context.Context, cfg *config.Config, res *DeployResult, opts DeployOptions) (MemReport, error) {
	needs := res.Memory
	if len(needs) == 0 {
		return MemReport{}, nil
	}
	type got struct {
		node string
		f    MemFree
		err  error
	}
	ch := make(chan got, len(needs))
	for _, n := range needs {
		go func(node string) {
			f, err := probeMemory(ctx, node, isLocalNode(node))
			ch <- got{node, f, err}
		}(n.Node)
	}
	live, perr := map[string]MemFree{}, map[string]error{}
	for range needs {
		g := <-ch
		if g.err != nil {
			perr[g.node] = g.err
		} else {
			live[g.node] = g.f
		}
	}
	rep := evaluateMemory(needs, live, perr, cfg, opts.CPUOnly)
	res.Memory = rep.Nodes
	if len(rep.Blockers) > 0 && opts.Replace && !opts.Force {
		// The probe sees the previous run's processes (they are stopped only after this check) as used memory: a
		// ring that lost a worker would never come back. It fitted before with the same options.
		for _, b := range rep.Blockers {
			rep.Warnings = append(rep.Warnings, "replacing the previous run, whose memory is freed when it is stopped: "+b)
		}
		rep.Blockers = nil
		return rep, nil
	}
	if len(rep.Blockers) > 0 && !opts.Force {
		return rep, fmt.Errorf("not enough memory: %s\npass --force to deploy anyway (it will be slow or thrash), give a smaller --ctx / --np, a quantized --cache-type-k/-v, or add a worker", strings.Join(rep.Blockers, "\n"))
	}
	return rep, nil
}

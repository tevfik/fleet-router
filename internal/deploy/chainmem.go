package deploy

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/plan"
)

// The memory check of a chain, before any stage is started (the ring path has checkMemory; a chain had none: a 6 GiB
// MoE cache on the stage that also holds the embeddings ran its 16 GB GPU out of memory minutes into the load, and an
// unchecked load is how a unified-memory machine (dino) locks up instead of failing).
//
// For every stage, on the machine it runs on (two stages on one machine add up):
//   VRAM: its dense weights (all weights without --cpu-moe), KV cache, compute buffer, MoE cache, and on stage 0
//         the DFlash draft once per sequence (--n-seq loads one draft each);
//   RAM:  the experts kept in host memory (--cpu-moe; mapped from the file, so page cache that MemAvailable counts).
// A machine whose GPU reports no free VRAM of its own (unified memory: GB10) holds all of it in RAM.
// Free memory is read live (MemAvailable, nvidia-smi); fleet's headroom and the node's reserve_gib stay free.
//
// The MoE cache is what gives: when --cpu-moe/--n-cpu-moe is on, each stage gets the cache its machine's free VRAM
// leaves (at most --moe-cache-mib when given, at most maxAutoMoECacheMiB when not); a stage that cannot fit even
// without a cache is refused (--force deploys anyway).

// ChainStageMem is the memory one stage needs and what its machine has.
type ChainStageMem struct {
	Stage      int     `json:"stage"`
	Node       string  `json:"node"`
	FirstLayer int     `json:"first_layer"`
	Layers     int     `json:"layers"`
	VRAMGiB    float64 `json:"vram_gib"`                // without the MoE cache
	RAMGiB     float64 `json:"ram_gib"`                 // host-resident experts (or everything, without a GPU)
	CacheMiB   int     `json:"moe_cache_mib,omitempty"` // chosen
	DraftGiB   float64 `json:"draft_gib,omitempty"`
}

// chainMemReading is one machine's live memory, after headroom and reserve.
type chainMemReading struct {
	ramGiB, vramGiB float64 // free now
	reserveGiB      float64 // headroom + reserve_gib
	unified         bool    // no VRAM of its own reported: one pool
}

// draftFileGiB reads the size of the draft file on node (0: unknown).
var draftFileGiB = func(ctx context.Context, node, path string) float64 {
	if path == "" {
		return 0
	}
	if isLocalNode(node) {
		if fi, err := os.Stat(path); err == nil {
			return float64(fi.Size()) / gib
		}
		return 0
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := runOn(cctx, node, "stat -c %s "+shellQuote(path)+" 2>/dev/null")
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	return n / gib
}

// CheckChainMemory sizes the MoE cache of every stage and refuses a stage that does not fit (unless o.Force). It sets
// o.StageMoECacheMiB; the notes say what was read and chosen.
func CheckChainMemory(ctx context.Context, c plan.Candidate, m *plan.Model, cfg *config.Config, o *StagePipeOptions) ([]string, []ChainStageMem, error) {
	parts := chainParts(c)
	needs, ok := chainStageNeeds(plan.Candidate{Parts: parts}, m, o.Ctx)
	if !ok || len(needs) == 0 {
		return []string{"memory check: the GGUF lists no per-layer sizes, the stages are not checked"}, nil, nil
	}
	cpuMoE := (o.CPUMoE || o.NCPUMoE > 0) && m.ExpertWeights() > 0
	// read every machine once, in parallel
	type got struct {
		node string
		f    MemFree
		err  error
	}
	nodes := map[string]bool{}
	for _, n := range needs {
		nodes[n.node] = true
	}
	ch := make(chan got, len(nodes))
	for n := range nodes {
		go func(node string) {
			f, err := probeMemory(ctx, node, isLocalNode(node))
			ch <- got{node, f, err}
		}(n)
	}
	read := map[string]chainMemReading{}
	var notes []string
	for range nodes {
		g := <-ch
		if g.err != nil {
			notes = append(notes, fmt.Sprintf("memory check: the free memory of %s could not be read (%v): its stage is not checked", g.node, g.err))
			continue
		}
		r := chainMemReading{ramGiB: g.f.RAMGiB, vramGiB: g.f.VRAMGiB, reserveGiB: memHeadroomGiB, unified: g.f.VRAMGiB <= 0}
		if spec, ok := cfg.Node(g.node); ok && spec.ReserveGiB > 0 {
			r.reserveGiB += spec.ReserveGiB
		}
		read[g.node] = r
	}
	draftGiB := 0.0
	if o.DraftModel != "" {
		// DFlash: one draft per sequence (--n-seq), each with its own KV cache and buffers
		draftGiB = draftFileGiB(ctx, needs[0].node, o.DraftModel) * 1.15 * float64(max(o.Np, 1))
	}

	stages := make([]ChainStageMem, len(needs))
	for i, n := range needs {
		st := ChainStageMem{Stage: i, Node: n.node, FirstLayer: parts[i].First, Layers: n.layers}
		w := n.weightsGiB
		if cpuMoE {
			st.RAMGiB = n.weightsGiB - n.denseGiB
			w = n.denseGiB
		}
		st.VRAMGiB = w + n.kvGiB + chainComputeGiB
		if i == 0 {
			st.DraftGiB = draftGiB
			st.VRAMGiB += draftGiB
		}
		stages[i] = st
	}

	// per machine: what its stages need together, then the cache each stage can have
	var blockers []string
	byNode := map[string][]int{}
	var order []string
	for i, st := range stages {
		if _, seen := byNode[st.Node]; !seen {
			order = append(order, st.Node)
		}
		byNode[st.Node] = append(byNode[st.Node], i)
	}
	cacheCap := maxAutoMoECacheMiB
	if o.MoECacheMiB > 0 {
		cacheCap = o.MoECacheMiB
	}
	o.StageMoECacheMiB = map[int]int{}
	for _, node := range order {
		r, ok := read[node]
		if !ok {
			if cpuMoE {
				for _, i := range byNode[node] { // not read: the cache stays as given
					o.StageMoECacheMiB[stages[i].FirstLayer] = o.MoECacheMiB
					stages[i].CacheMiB = o.MoECacheMiB
				}
			}
			continue
		}
		var vram, ram float64
		for _, i := range byNode[node] {
			vram += stages[i].VRAMGiB
			ram += stages[i].RAMGiB
		}
		desc := fmt.Sprintf("%s (stage %s)", node, stageList(byNode[node]))
		if r.unified {
			// one pool: everything, the experts too, comes out of RAM
			left := r.ramGiB - r.reserveGiB - vram - ram
			if left < 0 {
				blockers = append(blockers, fmt.Sprintf("%s needs about %.1f GiB (weights, KV cache, buffers%s) and %.1f GiB is free less %.1f GiB kept free: %.1f GiB short",
					desc, vram+ram, draftNote(stages, byNode[node]), r.ramGiB, r.reserveGiB, -left))
				continue
			}
			notes = append(notes, fmt.Sprintf("memory check: %s needs about %.1f GiB, %.1f GiB is free (unified memory)", desc, vram+ram, r.ramGiB))
			assignCache(o, stages, byNode[node], cpuMoE, cacheCap, left, &notes, desc)
			continue
		}
		vLeft := r.vramGiB - chainVRAMHeadroomGiB - vram
		rLeft := r.ramGiB - r.reserveGiB - ram
		switch {
		case vLeft < 0:
			hint := "a smaller --ctx, more machines, or the experts on the CPU (--cpu-moe)"
			if cpuMoE {
				hint = "a smaller --ctx or more machines (the experts are already on the CPU)"
			}
			blockers = append(blockers, fmt.Sprintf("%s needs about %.1f GiB of VRAM (dense weights, KV cache, compute buffer%s) and %.1f GiB is free: %.1f GiB short; something else may hold the GPU (nvidia-smi), or use %s",
				desc, vram, draftNote(stages, byNode[node]), r.vramGiB, -vLeft+chainVRAMHeadroomGiB, hint))
			continue
		case rLeft < 0:
			blockers = append(blockers, fmt.Sprintf("%s keeps %.1f GiB of experts in RAM and %.1f GiB is available less %.1f GiB kept free: %.1f GiB short (the experts would be paged from disk on every token)",
				desc, ram, r.ramGiB, r.reserveGiB, -rLeft))
			continue
		}
		notes = append(notes, fmt.Sprintf("memory check: %s needs about %.1f GiB of VRAM (%.1f free) and %.1f GiB of RAM (%.1f available)", desc, vram, r.vramGiB, ram, r.ramGiB))
		assignCache(o, stages, byNode[node], cpuMoE, cacheCap, vLeft, &notes, desc)
	}
	if len(blockers) > 0 && !o.Force {
		return notes, stages, fmt.Errorf("not enough memory for the chain:\n  %s\npass --force to deploy anyway (a GPU that runs out fails the load; a machine whose RAM runs out pages or locks up)", strings.Join(blockers, "\n  "))
	}
	for _, b := range blockers {
		notes = append(notes, "memory check (--force): "+b)
	}
	return notes, stages, nil
}

// assignCache gives the stages of one machine the MoE cache that leftGiB of VRAM allows (split by their expert size).
func assignCache(o *StagePipeOptions, stages []ChainStageMem, idx []int, cpuMoE bool, capMiB int, leftGiB float64, notes *[]string, desc string) {
	if !cpuMoE {
		return
	}
	totalExp := 0.0
	for _, i := range idx {
		totalExp += stages[i].RAMGiB
	}
	for _, i := range idx {
		share := 1.0 / float64(len(idx))
		if totalExp > 0 {
			share = stages[i].RAMGiB / totalExp
		}
		mib := int(leftGiB*share*1024) / autoCacheStepMiB * autoCacheStepMiB
		mib = min(mib, capMiB, int(stages[i].RAMGiB*1024)) // no more than the experts it could cache
		if mib < minAutoMoECacheMiB {
			mib = 0
		}
		stages[i].CacheMiB = mib
		o.StageMoECacheMiB[stages[i].FirstLayer] = mib
		switch {
		case o.MoECacheMiB > 0 && mib < o.MoECacheMiB:
			*notes = append(*notes, fmt.Sprintf("memory check: stage %d on %s gets an MoE cache of %d MiB, not %d: that is what its free VRAM leaves", stages[i].Stage, stages[i].Node, mib, o.MoECacheMiB))
		case mib == 0:
			*notes = append(*notes, fmt.Sprintf("memory check: no VRAM left for an MoE cache on %s: its experts are read from RAM", desc))
		case o.MoECacheMiB == 0:
			*notes = append(*notes, fmt.Sprintf("memory check: stage %d on %s gets an MoE cache of %d MiB (from its free VRAM)", stages[i].Stage, stages[i].Node, mib))
		}
	}
}

// chainParts is the candidate's parts without a leading master that holds no layers (as PlanStagePipe drops it).
func chainParts(c plan.Candidate) []plan.Part {
	p := c.Parts
	for len(p) > 1 && p[0].Layers <= 0 {
		p = p[1:]
	}
	return p
}

func stageList(idx []int) string {
	s := make([]string, len(idx))
	for i, x := range idx {
		s[i] = strconv.Itoa(x)
	}
	sort.Strings(s)
	return strings.Join(s, ", ")
}

func draftNote(stages []ChainStageMem, idx []int) string {
	for _, i := range idx {
		if stages[i].DraftGiB > 0 {
			return fmt.Sprintf(", draft %.1f GiB", stages[i].DraftGiB)
		}
	}
	return ""
}

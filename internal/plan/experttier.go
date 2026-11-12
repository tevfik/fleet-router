package plan

import (
	"fmt"
	"math"

	"github.com/tevfik/fleet-router/internal/hostinfo"
)

// The worker CPU-expert tier: a worker's host RAM holds the MoE expert tensors (blk.N.ffn_*_exps) of some of its
// blocks while the rest of those blocks stays on its GPU (fleet deploy --worker-cpu-experts). The planner uses it only
// for a mixture of experts and only when the blocks do not fit the GPUs otherwise: a layout that fits GPU memory
// never gets it. Its price is the expert bytes a token reads from those blocks at the worker's expert rate.

// WithExpertTier splits a GPU machine's capacity into its GPU memory (CapGB, BW) and the host RAM for expert
// tensors (ExpertRAMGB at ExpertGBps), instead of FromHost's blend of both. expertGBps is a declared or measured rate
// (0: the RAM copy bandwidth of the benchmark at the CPU efficiency, an estimate). Machines without a GPU benchmark
// are returned unchanged.
func WithExpertTier(n Node, b *hostinfo.Bench, now hostinfo.Sample, expertGBps float64) Node {
	if b == nil || b.FastWhere != "GPU" || b.FastBW <= 0 || b.FastMem <= 0 || now.RAMTotal <= 0 || b.RAMBW <= 0 {
		return n
	}
	free := float64(b.FastMem)
	for _, g := range now.GPUs {
		if g.MemUsed > 0 {
			free -= float64(g.MemUsed)
		}
	}
	ramAvail := float64(now.RAMTotal - now.RAMUsed)
	if now.RAMUsed == 0 {
		ramAvail = float64(now.RAMTotal) * 0.75
	}
	n.CapGB = math.Max(0, free/gib-overheadGB)
	n.BW = b.FastBW
	n.ExpertRAMGB = math.Max(0, ramAvail/gib-overheadGB)
	n.ExpertGBps = expertGBps
	if n.ExpertGBps <= 0 {
		n.ExpertGBps = b.RAMBW * cpuEff
	}
	return n
}

// expertGB is the expert weights of one block (GB, the mean over the blocks) and the share of them a token reads.
func expertGB(m *Model) (perBlock, readShare float64) {
	if m.Experts <= 0 || len(m.ExpertBytes) == 0 {
		return 0, 0
	}
	var t float64
	for _, b := range m.ExpertBytes {
		t += float64(b)
	}
	perBlock = t / float64(len(m.ExpertBytes)) / gib
	readShare = 1
	if m.ExpertsUsed > 0 && m.ExpertsUsed < m.Experts {
		readShare = float64(m.ExpertsUsed) / float64(m.Experts)
	}
	return perBlock, readShare
}

// expertExtra finds how many more blocks (up to want) worker w can take when the experts of c of its blocks go to
// its host RAM: (cur+x-c) whole blocks plus c blocks without their experts in capGB, c × E in RAM; c as small as
// possible (GPU first). It returns x and c (0, 0: the tier does not help).
func expertExtra(w Node, capGB, layerGB, e float64, cur, want int) (x, c int) {
	if w.ExpertRAMGB <= 0 || w.ExpertGBps <= 0 || e <= 0 || e >= layerGB {
		return 0, 0
	}
	for x = want; x > 0; x-- {
		total := cur + x
		need := float64(total)*layerGB - capGB
		c = 0
		if need > 0 {
			c = int(math.Ceil(need/e - 1e-9))
		}
		if c <= total && float64(c)*e <= w.ExpertRAMGB+1e-9 && float64(total-c)*layerGB+float64(c)*(layerGB-e) <= capGB+1e-9 {
			return x, c
		}
	}
	return 0, 0
}

// CPUExpertBlocks is the block range whose experts the part keeps in host RAM (the last CPUExperts blocks it holds).
func (p Part) CPUExpertBlocks() (first, last int, ok bool) {
	if p.CPUExperts <= 0 || p.CPUExperts > p.Layers {
		return 0, 0, false
	}
	last = p.First + p.Layers - 1
	return last - p.CPUExperts + 1, last, true
}

func expertNote(p Part) string {
	f, l, _ := p.CPUExpertBlocks()
	return fmt.Sprintf("%s keeps the MoE experts of blocks %d-%d in its host RAM (the model does not fit the GPUs otherwise)", p.Node, f, l)
}

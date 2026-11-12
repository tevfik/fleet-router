package plan

import (
	"math"
	"testing"

	"github.com/tevfik/fleet-router/internal/hostinfo"
)

// pdfModel: 64 equal blocks of ~231 MB, the Qwen3.8-27B of the 3-board report (14.81 GB of weights over 64
// layers: [PDF] total, the equal split per layer is [derived]).
func pdfModel() *Model {
	m := &Model{Name: "pdf-27b", Arch: "qwen", Layers: 64, HiddenBytes: 10240}
	for i := 0; i < 64; i++ {
		m.LayerBytes = append(m.LayerBytes, 231_400_000)
		m.LayerRead = append(m.LayerRead, 231_400_000)
	}
	return m
}

// pdfNodes: two boards at 1000 MHz (34.0 and 32.0 GB/s [PDF]) and one at 600 MHz (22.6 GB/s [PDF]), 8 GiB pools.
func pdfNodes() []Node {
	b := &hostinfo.Bench{FastWhere: "NPU"}
	return []Node{
		{Name: "delta1", CapGB: 8, BW: 34.0, WeightBW: 34.0, Bench: b},
		{Name: "pcie", CapGB: 8, BW: 32.0, WeightBW: 32.0, Bench: b},
		{Name: "delta2", CapGB: 8, BW: 22.6, WeightBW: 22.6, Bench: b},
	}
}

func threeWay(r Result) *Candidate {
	for i := range r.Candidates {
		if len(r.Candidates[i].Parts) == 3 {
			return &r.Candidates[i]
		}
	}
	return nil
}

func TestBalanceSlowNodeGetsFewerLayers(t *testing.T) {
	m := pdfModel()
	r := Plan(m, pdfNodes(), Options{Ctx: 512, Master: "delta1", Balance: true})
	c := threeWay(r)
	if c == nil {
		t.Fatalf("no three-machine candidate: %+v", r)
	}
	layers := map[string]int{}
	sum := 0
	for _, p := range c.Parts {
		layers[p.Node] = p.Layers
		sum += p.Layers
	}
	if sum != 64 {
		t.Fatalf("layers %v", layers)
	}
	if !(layers["delta2"] < layers["pcie"] && layers["delta2"] < layers["delta1"]) {
		t.Fatalf("the 600 MHz board must hold fewest: %v", layers)
	}
	// the layers follow the bandwidth ratio 34 : 32 : 22.6 within a block either way
	total := 34.0 + 32.0 + 22.6
	for name, bw := range map[string]float64{"delta1": 34.0, "pcie": 32.0, "delta2": 22.6} {
		if want := 64 * bw / total; math.Abs(float64(layers[name])-want) > 1.5 {
			t.Errorf("%s: %d layers, proportional share %.1f", name, layers[name], want)
		}
	}
	// stage times are within one block of the slowest machine of each other
	lo, hi := math.Inf(1), 0.0
	for _, p := range c.Parts {
		lo, hi = math.Min(lo, p.ComputeMS), math.Max(hi, p.ComputeMS)
	}
	oneBlock := 231.4e6 / 1e9 / 22.6 * 1000
	if hi-lo > oneBlock*1.01 {
		t.Fatalf("stage times %v..%v ms differ by more than one block (%.1f ms)", lo, hi, oneBlock)
	}
}

func TestBalanceBeatsTheReportedSplit(t *testing.T) {
	// the report's own split put 28 of 64 blocks on the 600 MHz board: its stage was 320 ms of 547 [PDF];
	// the same blocks at the same bandwidth give the bottleneck the balanced plan has to undercut.
	reported := 28 * 231.4e6 / 22.6e9 * 1000
	c := threeWay(Plan(pdfModel(), pdfNodes(), Options{Ctx: 512, Master: "delta1", Balance: true}))
	if c == nil || c.BottleneckMS >= reported*0.8 {
		t.Fatalf("bottleneck %v ms vs the report's split %.0f ms", c.BottleneckMS, reported)
	}
	if want := 1000 / c.BottleneckMS; math.Abs(c.PipeTPS-want) > 1e-9 {
		t.Fatalf("pipe tps %v", c.PipeTPS)
	}
}

func TestBalanceKeepsPoolLimit(t *testing.T) {
	nodes := pdfNodes()
	nodes[0].CapGB = 3 // the fast master holds only ~13 blocks (3 GiB / 0.2155 GiB)
	c := threeWay(Plan(pdfModel(), nodes, Options{Ctx: 512, Master: "delta1", Balance: true}))
	if c == nil {
		t.Fatal("no candidate")
	}
	for _, p := range c.Parts {
		if p.Node == "delta1" && float64(p.Layers)*231.4e6/gib > 3 {
			t.Fatalf("pool exceeded: %d blocks on 3 GiB", p.Layers)
		}
	}
}

func TestWeightBWOverridesMemoryFigures(t *testing.T) {
	n := Node{Name: "x", BW: 100, Eff: 0.5}
	if n.rate() != 50 {
		t.Fatalf("rate %v", n.rate())
	}
	n.WeightBW = 22.6
	if n.rate() != 22.6 {
		t.Fatalf("a declared weight bandwidth is already effective: %v", n.rate())
	}
}

func TestBalanceTradesOneStreamTimeForThroughput(t *testing.T) {
	// without Balance the fastest machines fill first: the smallest sum of stage times for one stream;
	// with it the slowest stage is the shortest.
	d := threeWay(Plan(pdfModel(), pdfNodes(), Options{Ctx: 512, Master: "delta1"}))
	b := threeWay(Plan(pdfModel(), pdfNodes(), Options{Ctx: 512, Master: "delta1", Balance: true}))
	if d == nil || b == nil {
		t.Fatalf("default %v balanced %v", d, b)
	}
	if d.TokenMS > b.TokenMS || b.BottleneckMS >= d.BottleneckMS {
		t.Fatalf("default %.0f ms (slowest stage %.0f), balanced %.0f ms (slowest stage %.0f)", d.TokenMS, d.BottleneckMS, b.TokenMS, b.BottleneckMS)
	}
}

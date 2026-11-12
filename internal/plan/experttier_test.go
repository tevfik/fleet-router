package plan

import (
	"strings"
	"testing"

	"github.com/tevfik/fleet-router/internal/hostinfo"
)

// moeModel: blocks of blockGB, expGB of them MoE experts (256 experts, 8 used per token).
func moeModel(blocks int, blockGB, expGB float64) *Model {
	m := &Model{Name: "moe", Arch: "anyarch", Layers: blocks, Embd: 4096, Experts: 256, ExpertsUsed: 8, HiddenBytes: 4096 * 4, KVPerTokenLayer: 1024}
	for i := 0; i < blocks; i++ {
		b := int64(blockGB * gib)
		e := int64(expGB * gib)
		m.LayerBytes = append(m.LayerBytes, b)
		m.ExpertBytes = append(m.ExpertBytes, e)
		m.LayerRead = append(m.LayerRead, b-e+e*8/256)
	}
	m.OutputBytes, m.EmbdBytes, m.OtherBytes = int64(gib), int64(gib), int64(2*gib)
	return m
}

func tierNodes(withTier bool) []Node {
	big := Node{Name: "big", CapGB: 100, BW: 270, Bench: &hostinfo.Bench{FastWhere: "GPU"}}
	gpu := Node{Name: "laptop", CapGB: 14, BW: 500, RTTms: 20, Bench: &hostinfo.Bench{FastWhere: "GPU"}}
	if withTier {
		gpu.ExpertRAMGB, gpu.ExpertGBps = 60, 30
	}
	return []Node{big, gpu}
}

func TestExpertTierOnlyWhenTheGPUsAreFull(t *testing.T) {
	m := moeModel(48, 2.5, 2.3) // 120 GB of blocks: more than 100 + 14 GB of GPU memory
	if r := Plan(m, tierNodes(false), Options{Master: "big", Ctx: 4096}); len(r.Candidates) != 0 {
		t.Fatalf("without the tier the model fits nowhere: %+v", r.Candidates)
	}
	r := Plan(m, tierNodes(true), Options{Master: "big", Ctx: 4096})
	if len(r.Candidates) == 0 {
		t.Fatal("the worker's host RAM must make it fit")
	}
	c := r.Candidates[0]
	var w Part
	for _, p := range c.Parts {
		if p.Node == "laptop" {
			w = p
		}
	}
	f, l, ok := w.CPUExpertBlocks()
	if !ok || w.CPUExperts <= 0 || l != w.First+w.Layers-1 || f < w.First {
		t.Fatalf("cpu experts on the worker's last blocks: %+v", w)
	}
	if !strings.Contains(strings.Join(c.Notes, "\n"), "host RAM") {
		t.Errorf("the candidate must say it: %v", c.Notes)
	}

	small := moeModel(48, 2.0, 1.8) // 96 GB: fits the GPUs
	r = Plan(small, tierNodes(true), Options{Master: "big", Ctx: 4096})
	for _, c := range r.Candidates {
		for _, p := range c.Parts {
			if p.CPUExperts > 0 {
				t.Fatalf("a layout that fits the GPUs never takes the tier: %+v", c)
			}
		}
	}
}

func TestExpertTierCostsTime(t *testing.T) {
	m := moeModel(48, 2.5, 2.3)
	slow := tierNodes(true)
	slow[1].ExpertGBps = 5
	fast := tierNodes(true)
	a := Plan(m, slow, Options{Master: "big", Ctx: 4096})
	b := Plan(m, fast, Options{Master: "big", Ctx: 4096})
	if len(a.Candidates) == 0 || len(b.Candidates) == 0 || a.Candidates[0].TokenMS <= b.Candidates[0].TokenMS {
		t.Fatalf("a slower expert rate must cost time: %v vs %v", a.Candidates, b.Candidates)
	}
}

func TestWithExpertTier(t *testing.T) {
	b := &hostinfo.Bench{FastWhere: "GPU", FastBW: 500, FastMem: 16 << 30, RAMBW: 60}
	now := hostinfo.Sample{RAMTotal: 64 << 30, RAMUsed: 4 << 30}
	n := WithExpertTier(Node{Name: "w"}, b, now, 0)
	if n.CapGB != 16-overheadGB || n.ExpertRAMGB != 60-overheadGB || n.ExpertGBps != 60*cpuEff {
		t.Fatalf("%+v", n)
	}
	if n := WithExpertTier(Node{Name: "w"}, b, now, 22); n.ExpertGBps != 22 {
		t.Fatalf("a declared rate wins: %+v", n)
	}
	if n := WithExpertTier(Node{Name: "c", CapGB: 3}, &hostinfo.Bench{FastWhere: "RAM"}, now, 0); n.CapGB != 3 || n.ExpertRAMGB != 0 {
		t.Fatalf("no GPU: unchanged: %+v", n)
	}
}

package plan

import (
	"math"
	"strings"
	"testing"
)

func evalNodes() []Node {
	return pdfNodes() // delta1 34, pcie 32, delta2 22.6 GB/s, 8 GiB each
}

func hasProblem(ev Evaluation, sub string) bool {
	for _, p := range ev.Problems {
		if strings.Contains(p, sub) {
			return true
		}
	}
	return false
}

// A split placed by hand with the planner's own block counts is priced exactly as the planner priced it.
func TestEvaluate_MatchesThePlannersPrice(t *testing.T) {
	m := pdfModel()
	nodes := evalNodes()
	for _, outMaster := range []bool{false, true} {
		ws := []Node{nodes[1], nodes[2]}
		c, ok := allocate(m, nodes[0], append([]Node(nil), ws...), Options{Ctx: 4096, Np: 1, FixedMS: DefaultFixedMS}, outMaster)
		if !ok {
			t.Fatalf("allocate failed (outMaster=%v)", outMaster)
		}
		var al []Alloc
		for _, p := range c.Parts[1:] {
			al = append(al, Alloc{p.Node, p.Layers})
		}
		ev := Evaluate(m, nodes, Options{Ctx: 4096, Np: 1}, c.Master, append([]Alloc{{c.Master, c.Parts[0].Layers}}, al...), outMaster)
		if !ev.OK || ev.Candidate == nil {
			t.Fatalf("outMaster=%v: not ok: %v", outMaster, ev.Problems)
		}
		if math.Abs(ev.Candidate.TokenMS-c.TokenMS) > 1e-9 || math.Abs(ev.Candidate.TPS-c.TPS) > 1e-9 {
			t.Errorf("outMaster=%v: token %v tps %v, planner %v %v", outMaster, ev.Candidate.TokenMS, ev.Candidate.TPS, c.TokenMS, c.TPS)
		}
		for i, p := range ev.Candidate.Parts {
			if p.Node != c.Parts[i].Node || p.Layers != c.Parts[i].Layers || p.First != c.Parts[i].First {
				t.Errorf("part %d: %+v, planner %+v", i, p, c.Parts[i])
			}
		}
		if ev.Assigned != m.Layers || ev.Total != 64 {
			t.Errorf("assigned %d total %d", ev.Assigned, ev.Total)
		}
	}
}

func TestEvaluate_Problems(t *testing.T) {
	m := pdfModel()
	nodes := evalNodes()
	o := Options{Ctx: 4096, Np: 1}
	cases := []struct {
		name   string
		master string
		alloc  []Alloc
		want   string
		ok     bool
	}{
		{"too few", "delta1", []Alloc{{"delta1", 20}, {"pcie", 20}}, "24 of 64 block(s) are not placed", false},
		{"too many", "delta1", []Alloc{{"delta1", 30}, {"pcie", 30}, {"delta2", 10}}, "6 block(s) too many", false},
		{"overflow", "delta1", []Alloc{{"delta1", 4}, {"pcie", 60}}, "pcie needs", false},
		{"unknown node", "delta1", []Alloc{{"delta1", 32}, {"ghost", 32}}, "ghost is not a machine", false},
		{"zero worker", "delta1", []Alloc{{"delta1", 30}, {"pcie", 34}, {"delta2", 0}}, "delta2 holds no blocks", false},
		{"no split", "delta1", []Alloc{{"delta1", 64}}, "no split", false},
		{"fits", "delta1", []Alloc{{"delta1", 30}, {"pcie", 34}}, "", true},
	}
	for _, c := range cases {
		ev := Evaluate(m, nodes, o, c.master, c.alloc, false)
		if ev.OK != c.ok {
			t.Errorf("%s: ok=%v problems=%v", c.name, ev.OK, ev.Problems)
		}
		if c.want != "" && !hasProblem(ev, c.want) {
			t.Errorf("%s: want problem %q, got %v", c.name, c.want, ev.Problems)
		}
	}
}

func TestEvaluate_MachineWithoutFigures(t *testing.T) {
	m := pdfModel()
	nodes := append(evalNodes(), Node{Name: "bare"})
	ev := Evaluate(m, nodes, Options{Ctx: 4096, Np: 1}, "delta1", []Alloc{{"delta1", 32}, {"bare", 32}}, false)
	if ev.OK || !hasProblem(ev, "bare cannot take part") {
		t.Fatalf("ok=%v %v", ev.OK, ev.Problems)
	}
	var why string
	for _, mc := range ev.Machines {
		if mc.Node == "bare" {
			why = mc.Why
		}
	}
	if why == "" {
		t.Error("the machine list should say why bare cannot take part")
	}
}

// Blocks follow the order given: master first, then the workers as listed, consecutive.
func TestEvaluate_BlocksAreConsecutive(t *testing.T) {
	m := pdfModel()
	ev := Evaluate(m, evalNodes(), Options{Ctx: 4096, Np: 1}, "delta1", []Alloc{{"delta1", 10}, {"delta2", 24}, {"pcie", 30}}, false)
	if ev.Candidate == nil {
		t.Fatalf("%v", ev.Problems)
	}
	want := []int{0, 10, 34}
	for i, p := range ev.Candidate.Parts {
		if p.First != want[i] {
			t.Errorf("part %d (%s) starts at %d, want %d", i, p.Node, p.First, want[i])
		}
	}
	if !ev.Parts[2].Output || ev.Parts[1].Output {
		t.Errorf("the output projection belongs on the last worker: %+v", ev.Parts)
	}
}

// Moving a block to the slower machine makes the split slower.
func TestEvaluate_SlowerMachineCostsTime(t *testing.T) {
	m := pdfModel()
	fast := Evaluate(m, evalNodes(), Options{Ctx: 4096, Np: 1}, "delta1", []Alloc{{"delta1", 20}, {"pcie", 44}}, false)
	slow := Evaluate(m, evalNodes(), Options{Ctx: 4096, Np: 1}, "delta1", []Alloc{{"delta1", 20}, {"delta2", 44}}, false)
	if fast.Candidate == nil || slow.Candidate == nil {
		t.Fatalf("%v %v", fast.Problems, slow.Problems)
	}
	if slow.Candidate.TPS >= fast.Candidate.TPS {
		t.Errorf("slow %v tps should be below fast %v", slow.Candidate.TPS, fast.Candidate.TPS)
	}
}

func TestEvaluate_Summary(t *testing.T) {
	ev := Evaluate(pdfModel(), evalNodes(), Options{Ctx: 4096, Np: 1}, "delta1", []Alloc{{"delta1", 30}, {"pcie", 34}}, false)
	if got := ev.Summary(); got != "delta1 30 · pcie 34" {
		t.Errorf("summary %q", got)
	}
}

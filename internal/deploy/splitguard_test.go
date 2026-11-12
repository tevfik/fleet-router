package deploy

import (
	"strings"
	"testing"

	"github.com/tevfik/fleet-router/internal/plan"
)

func cand(kind, master string, tps float64, workers ...string) plan.Candidate {
	c := plan.Candidate{Kind: kind, Master: master, TPS: tps, Where: "GPU", Parts: []plan.Part{{Node: master, Layers: 10}}}
	for _, w := range workers {
		c.Parts = append(c.Parts, plan.Part{Node: w, Layers: 10})
	}
	return c
}

func TestCheckSplitRefusesLargeMargin(t *testing.T) {
	r := plan.Result{Candidates: []plan.Candidate{cand("single", "dino", 25.1), cand("split", "dino", 18.4, "victor")}}
	g := CheckSplit(r, "dino", []string{"victor"})
	if !g.Applies || !g.Refuse || g.Ratio < 1.3 {
		t.Fatalf("%+v", g)
	}
	if _, err := g.Check(false); err == nil || !strings.Contains(err.Error(), "--force-split") || !strings.Contains(err.Error(), "estimated") {
		t.Fatalf("%v", err)
	}
	if w, err := g.Check(true); err != nil || w == "" {
		t.Fatalf("forced: %q %v", w, err)
	}
}

func TestCheckSplitSmallMarginOnlyWarns(t *testing.T) {
	r := plan.Result{Candidates: []plan.Candidate{cand("single", "dino", 20), cand("split", "dino", 18, "victor")}}
	g := CheckSplit(r, "", []string{"victor"})
	w, err := g.Check(false)
	if err != nil || !g.Warn || g.Refuse || w == "" {
		t.Fatalf("%+v %q %v", g, w, err)
	}
}

func TestCheckSplitSplitWinsOrNoNumbers(t *testing.T) {
	r := plan.Result{Candidates: []plan.Candidate{cand("split", "dino", 30, "victor"), cand("single", "dino", 20)}}
	if g := CheckSplit(r, "dino", []string{"victor"}); g.Warn {
		t.Fatalf("split is faster, no warning: %+v", g)
	}
	// the model fits no single machine: no verdict
	r = plan.Result{Candidates: []plan.Candidate{cand("split", "dino", 30, "victor")}}
	if g := CheckSplit(r, "dino", []string{"victor"}); g.Applies {
		t.Fatalf("%+v", g)
	}
	if w, err := (SplitGuard{}).Check(false); w != "" || err != nil {
		t.Fatal("empty guard must pass")
	}
}

func TestCheckSplitMatchesWorkerSet(t *testing.T) {
	r := plan.Result{Candidates: []plan.Candidate{cand("single", "dino", 25), cand("split", "dino", 24, "a"), cand("split", "dino", 10, "a", "b")}}
	g := CheckSplit(r, "dino", []string{"a", "b"})
	if g.SplitTPS != 10 || !g.Refuse {
		t.Fatalf("must compare against the a+b split: %+v", g)
	}
}

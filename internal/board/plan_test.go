package board_test

import (
	"strings"
	"testing"

	"github.com/tevfik/fleet-router/internal/board"
	"github.com/tevfik/fleet-router/internal/plan"
)

func TestPlanLines(t *testing.T) {
	r := plan.Result{Model: "big", WeightsGB: 40, KVGB: 2, Ctx: 8192, Np: 1, Verdict: "run it on dino alone", Skipped: []string{"vec: no memory figures"},
		Candidates: []plan.Candidate{
			{Kind: "single", Master: "dino", Where: "GPU", TPS: 27.1, TokenMS: 36.9, LoadS: 30, Parts: []plan.Part{{Node: "dino", Layers: 48}}},
			{Kind: "split", Master: "dino", TPS: 15.6, TokenMS: 64, LoadS: 400, Parts: []plan.Part{{Node: "dino", Layers: 40}, {Node: "p1", First: 40, Layers: 8, RTTms: 10}, {Node: "vec", First: 48, Layers: 0, RTTms: 9}}},
		}}
	out := strings.Join(board.PlanLines(r), "\n")
	for _, want := range []string{"big — weights 40.0 GB", "GPU", "27.1", "split", "p1 40-47 (+10ms link)", "7min", "30s", "run it on dino alone", "left out — vec", "docs/distributed.md"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "vec 0") {
		t.Fatalf("a machine without blocks is not listed:\n%s", out)
	}
	if out := strings.Join(board.PlanLines(plan.Result{Model: "x", Verdict: "does not fit"}), "\n"); !strings.Contains(out, "does not fit") {
		t.Fatal(out)
	}
}

func TestPlanTableMoveColumn(t *testing.T) {
	r := plan.Result{Model: "x", Candidates: []plan.Candidate{
		{Kind: "split", Master: "dino", TPS: 10, TokenMS: 100, FetchGB: 14.25, Parts: []plan.Part{{Node: "dino", Layers: 1}}},
		{Kind: "split", Master: "dino", TPS: 9, TokenMS: 110, Parts: []plan.Part{{Node: "dino", Layers: 1}}},
	}}
	out := strings.Join(board.PlanTable(r), "\n")
	if !strings.Contains(out, "to move") || strings.Count(out, "?") < 2 {
		t.Fatalf("unknown transfers show ?:\n%s", out)
	}
	if !strings.Contains(strings.Join(board.PlanLines(r), "\n"), "register the file") {
		t.Fatal("the hint to register the file")
	}
	r.Known = true
	out = strings.Join(board.PlanTable(r), "\n")
	if !strings.Contains(out, "14.2 GB") && !strings.Contains(out, "14.3 GB") {
		t.Fatalf("%s", out)
	}
	if !strings.Contains(out, "—") || strings.Contains(out, "?") {
		t.Fatalf("nothing to move shows a dash:\n%s", out)
	}
	if strings.Contains(strings.Join(board.PlanLines(r), "\n"), "register the file") {
		t.Fatal("no hint when known")
	}
}

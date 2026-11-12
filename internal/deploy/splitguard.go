package deploy

import (
	"fmt"
	"sort"
	"strings"

	"github.com/tevfik/fleet-router/internal/plan"
)

// SplitRefuseRatio is the margin at which `fleet deploy` refuses a split: the planner's best single machine must be
// at least this many times faster than the requested split (estimated tok/s). Between 1.0x and this ratio the user
// only gets a warning. Both numbers are the planner's estimates, not measurements.
const SplitRefuseRatio = 1.3

// SplitGuard is the planner's opinion of a split the user asked `fleet deploy` for.
type SplitGuard struct {
	Applies    bool    // there were numbers to compare (a single candidate and a matching split)
	SingleTPS  float64 // estimated, best single machine
	SingleNode string
	SplitTPS   float64 // estimated, the requested split
	Ratio      float64 // SingleTPS / SplitTPS
	Warn       bool    // single beats the split
	Refuse     bool    // single beats it by SplitRefuseRatio or more
	Message    string
}

// CheckSplit compares the requested master and workers with the planner's result. It has no side effects: r is the
// output of plan.Plan (or the daemon's /fleet/plan). The requested split is taken from the candidate with the same
// workers (and master, when given); with none, the best split of the plan stands for it. A plan without a single
// candidate (the model fits no one machine) or without any split gives no verdict.
func CheckSplit(r plan.Result, master string, workers []string) SplitGuard {
	var g SplitGuard
	var single, split *plan.Candidate
	want := map[string]bool{}
	for _, w := range workers {
		want[strings.ToLower(w)] = true
	}
	var best *plan.Candidate
	for i := range r.Candidates {
		c := &r.Candidates[i]
		if c.Kind == "single" || len(c.Parts) <= 1 {
			if single == nil || c.TPS > single.TPS {
				single = c
			}
			continue
		}
		if best == nil || c.TPS > best.TPS {
			best = c
		}
		if master != "" && !strings.EqualFold(c.Master, master) {
			continue
		}
		have := map[string]bool{}
		for _, p := range c.Parts {
			if !strings.EqualFold(p.Node, c.Master) {
				have[strings.ToLower(p.Node)] = true
			}
		}
		if len(have) == len(want) {
			same := true
			for w := range want {
				same = same && have[w]
			}
			if same && (split == nil || c.TPS > split.TPS) {
				split = c
			}
		}
	}
	if split == nil {
		split = best
	}
	if single == nil || split == nil || single.TPS <= 0 || split.TPS <= 0 {
		return g
	}
	g.Applies, g.SingleTPS, g.SingleNode, g.SplitTPS = true, single.TPS, single.Master, split.TPS
	g.Ratio = single.TPS / split.TPS
	if g.Ratio <= 1 {
		return g
	}
	g.Warn = true
	g.Refuse = g.Ratio >= SplitRefuseRatio
	nodes := []string{}
	for _, p := range split.Parts {
		nodes = append(nodes, p.Node)
	}
	sort.Strings(nodes)
	g.Message = fmt.Sprintf("the planner estimates %.1f tok/s on %s alone (%s) against %.1f tok/s for the split over %s: %.2fx (estimated, not measured; every extra worker adds a full round trip per token)",
		g.SingleTPS, g.SingleNode, single.Where, g.SplitTPS, strings.Join(nodes, ", "), g.Ratio)
	return g
}

// Check turns the guard into the error `fleet deploy` returns (nil: go on). force is --force-split.
func (g SplitGuard) Check(force bool) (warning string, err error) {
	if !g.Warn {
		return "", nil
	}
	if g.Refuse && !force {
		return "", fmt.Errorf("refusing to split: %s. The refusal threshold is %.1fx; pass --force-split to deploy the split anyway, or drop --workers to run on one machine", g.Message, SplitRefuseRatio)
	}
	if g.Refuse {
		return "split forced although " + g.Message, nil
	}
	return "a single machine looks faster than this split: " + g.Message, nil
}

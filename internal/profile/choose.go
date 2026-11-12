package profile

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/tevfik/fleet-router/internal/plan"
)

// The auto layout. The planner (plan.Plan over the machines' benchmarks and current free memory, the round trips,
// the hub estimate and the CPU-expert tier) lists the candidates that fit now; the profile's runs say what layouts
// really did. Choose picks, in this order:
//
//  1. --explore: the best candidate whose layout has no measurement yet (to measure it); when every candidate is
//     measured, the rules below apply.
//  2. The best measured layout (highest measured t/s among runs with the same speculation setting) that is still
//     feasible: a current candidate has the same master and the same machines (they fit now). It is deployed exactly
//     as measured (its -ts and CPU-expert blocks).
//  3. Otherwise the planner's best candidate, GPU-only first: a candidate that keeps MoE experts in a worker's host
//     RAM is chosen only when no GPU-only candidate exists.
//
// The measurements also calibrate the plan before it is made (CalibrationFor): the planner's fixed and ring costs
// are corrected so that its prediction for a measured layout matches the engine's trace.

// Choice is the layout Choose picked.
type Choice struct {
	Candidate *plan.Candidate // the planner's candidate (rule 1 or 3), nil for a measured layout
	Measured  *Run            // the measured run whose layout is redeployed (rule 2)
	Reason    string
}

// CandidateLayout is the RunLayout a candidate would run with (spec from the profile).
func CandidateLayout(c plan.Candidate, specType string, draftMax int, viaHub []string) RunLayout {
	l := RunLayout{Master: c.Master, SpecType: specType, DraftMax: draftMax, ViaHubNodes: viaHub}
	var ts []string
	for _, p := range c.Parts {
		ts = append(ts, strconv.Itoa(p.Layers))
		if p.Node != c.Master {
			l.Workers = append(l.Workers, p.Node)
		}
		if f, e, ok := p.CPUExpertBlocks(); ok {
			if l.CPUExperts == nil {
				l.CPUExperts = map[string]string{}
			}
			l.CPUExperts[p.Node] = fmt.Sprintf("%d-%d", f, e)
		}
	}
	if len(c.Parts) > 1 {
		l.TS = strings.Join(ts, ",")
		l.FromPlan, l.OutputOnMaster = true, c.Parts[0].Output
	}
	return l
}

func usesCPUExperts(c plan.Candidate) bool {
	for _, p := range c.Parts {
		if p.CPUExperts > 0 {
			return true
		}
	}
	return false
}

func machines(master string, workers []string) string {
	s := []string{strings.ToLower(master)}
	for _, w := range workers {
		s = append(s, strings.ToLower(w))
	}
	sort.Strings(s[1:])
	return strings.Join(s, ",")
}

// sameSpec says whether a run used the speculation the profile asks for now.
func sameSpec(r Run, specType string, draftMax int) bool {
	norm := func(s string) string {
		if s == "" || s == "auto" {
			return "auto"
		}
		return s
	}
	return norm(r.Layout.SpecType) == norm(specType) && (draftMax == 0 || r.Layout.DraftMax == 0 || r.Layout.DraftMax == draftMax)
}

// Choose applies the auto rules to a plan result (candidates best first).
func Choose(r plan.Result, p *Profile, explore bool, viaHub []string) (Choice, error) {
	if len(r.Candidates) == 0 {
		return Choice{}, fmt.Errorf("auto layout: the planner has no candidate (%s)", r.Verdict)
	}
	spec, dmax := p.Spec.Type, p.Spec.DraftMax
	measured := map[string]*Run{}
	for i := range p.Runs {
		run := &p.Runs[i]
		if run.Measured == nil || run.Measured.TPS <= 0 || !sameSpec(*run, spec, dmax) {
			continue
		}
		k := run.Layout.Key()
		if old, ok := measured[k]; !ok || run.Measured.At.After(old.Measured.At) {
			measured[k] = run // the newest measurement of a layout counts
		}
	}
	keyOf := func(c plan.Candidate) string {
		l := CandidateLayout(c, spec, dmax, viaHub)
		// a run records the spec type it really ran with; compare candidates by machines and split only
		l.SpecType, l.DraftMax = "", 0
		return l.Key()
	}
	runKey := func(run *Run) string {
		l := run.Layout
		l.SpecType, l.DraftMax = "", 0
		return l.Key()
	}
	tested := map[string]bool{}
	for _, run := range measured {
		tested[runKey(run)] = true
	}
	if explore {
		for i := range r.Candidates {
			if !tested[keyOf(r.Candidates[i])] {
				c := r.Candidates[i]
				return Choice{Candidate: &c, Reason: fmt.Sprintf("explore: candidate %d of the plan has not been measured yet (planned %.1f t/s)", i+1, c.TPS)}, nil
			}
		}
	}
	feasible := map[string]bool{}
	for _, c := range r.Candidates {
		var ws []string
		for _, pt := range c.Parts {
			if pt.Node != c.Master {
				ws = append(ws, pt.Node)
			}
		}
		feasible[machines(c.Master, ws)] = true
	}
	var best *Run
	for _, run := range measured {
		if !feasible[machines(run.Layout.Master, run.Layout.Workers)] {
			continue
		}
		if best == nil || run.Measured.TPS > best.Measured.TPS {
			best = run
		}
	}
	if best != nil {
		why := fmt.Sprintf("measured: the best measured layout that still fits (%.1f t/s on %s)", best.Measured.TPS, best.Measured.At.Format("2006-01-02"))
		if explore {
			why = "explore: every candidate is measured; " + why
		}
		return Choice{Measured: best, Reason: why}, nil
	}
	for i := range r.Candidates {
		if !usesCPUExperts(r.Candidates[i]) {
			c := r.Candidates[i]
			return Choice{Candidate: &c, Reason: fmt.Sprintf("planned: the planner's best GPU-only candidate (%.1f t/s estimated, nothing measured yet)", c.TPS)}, nil
		}
	}
	c := r.Candidates[0]
	return Choice{Candidate: &c, Reason: fmt.Sprintf("planned: no GPU-only layout fits; the planner's best keeps MoE experts in a worker's host RAM (%.1f t/s estimated)", c.TPS)}, nil
}

// CalibrationFor finds the newest measured run with an engine trace whose layout matches a candidate of r and
// returns the calibration (the FixedMS / RingMS to plan again with). ok is false when there is none.
func CalibrationFor(r plan.Result, p *Profile, fixedMS float64) (plan.Calibration, *Run, bool) {
	for i := len(p.Runs) - 1; i >= 0; i-- {
		run := &p.Runs[i]
		if run.Measured == nil || run.Measured.Trace == nil || run.Measured.Trace.TokenMS <= 0 {
			continue
		}
		for _, c := range r.Candidates {
			l := CandidateLayout(c, "", 0, nil)
			rl := run.Layout
			rl.SpecType, rl.DraftMax, rl.ViaHubNodes = "", 0, nil
			if l.Key() == rl.Key() {
				k, err := plan.Calibrate(c, *run.Measured.Trace, fixedMS)
				if err == nil {
					return k, run, true
				}
			}
		}
	}
	return plan.Calibration{}, nil, false
}

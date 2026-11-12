package plan

import (
	"fmt"
	"math"
)

// Measured is what the engine's trace says about the decode steps of a running candidate (ring-trace / GET
// /ring/trace: the p50 step, the master's own part, the part that went through the workers), in ms per token.
type Measured struct {
	TokenMS float64 `json:"token_ms"`
	LocalMS float64 `json:"local_ms"`
	RingMS  float64 `json:"ring_ms"`
}

// Calibration compares one candidate's prediction with a measurement and says which inputs to change so that the
// next plan matches it.
type Calibration struct {
	PredTokenMS float64 `json:"pred_token_ms"`
	PredLocalMS float64 `json:"pred_local_ms"`
	PredRingMS  float64 `json:"pred_ring_ms"` // everything the plan put on the workers' side: compute, wire, send, RingMS
	ErrorPct    float64 `json:"error_pct"`    // (predicted - measured) / measured; negative: the plan was too optimistic
	LocalRatio  float64 `json:"local_ratio"`  // measured / predicted master-local time
	// Inputs to pass to the next plan: FixedMS absorbs the master's extra local time (never below 0), RingMS what
	// the ring still costs beyond the workers' compute, wire and send.
	FixedMS float64 `json:"fixed_ms"`
	RingMS  float64 `json:"ring_ms"`
}

// Calibrate measures a candidate's prediction against the trace. fixedMS is the Options.FixedMS the plan used
// (0: DefaultFixedMS).
func Calibrate(c Candidate, m Measured, fixedMS float64) (Calibration, error) {
	if m.TokenMS <= 0 {
		return Calibration{}, fmt.Errorf("the measurement has no token time")
	}
	if fixedMS <= 0 {
		fixedMS = DefaultFixedMS
	}
	k := Calibration{PredTokenMS: c.TokenMS, PredLocalMS: c.MasterLocalMS, PredRingMS: c.TokenMS - c.MasterLocalMS}
	k.ErrorPct = (c.TokenMS - m.TokenMS) / m.TokenMS * 100
	if c.MasterLocalMS > 0 && m.LocalMS > 0 {
		k.LocalRatio = m.LocalMS / c.MasterLocalMS
	}
	k.FixedMS = math.Max(0, fixedMS+(m.LocalMS-c.MasterLocalMS))
	k.RingMS = math.Max(0, c.RingMS+(m.RingMS-k.PredRingMS))
	return k, nil
}

// ApplyTarget is the token-time gate: a candidate slower than targetMS per token is marked, and when none meets it
// the result says so (the order of the candidates is not changed).
func ApplyTarget(r *Result, targetMS float64) {
	if targetMS <= 0 || len(r.Candidates) == 0 {
		return
	}
	ok := 0
	for i := range r.Candidates {
		c := &r.Candidates[i]
		if c.TokenMS > targetMS {
			c.Notes = append(c.Notes, fmt.Sprintf("misses the target: %.1f ms per token, target %.1f ms", c.TokenMS, targetMS))
		} else {
			ok++
		}
	}
	if ok == 0 {
		best := r.Candidates[0]
		for _, c := range r.Candidates {
			if c.TokenMS < best.TokenMS {
				best = c
			}
		}
		r.Warnings = append(r.Warnings, fmt.Sprintf("no candidate meets the %.1f ms per token target; the fastest takes %.1f ms (master %s)", targetMS, best.TokenMS, best.Master))
	}
}

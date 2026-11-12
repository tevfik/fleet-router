package server

import (
	"sort"
	"strings"

	"github.com/tevfik/fleet-router/internal/plan"
)

// calibSamples is how many of a run's newest engine samples feed a calibration.
const calibSamples = 10

// Measured is what the run's own RingTrace samples say about a decode step: the median of the newest samples'
// p50 token time and of the master's own time. RingMS is derived (token - local), the trace does not store it
// per sample. n is how many samples had a token time (0: nothing measured yet).
func (r Run) Measured() (m plan.Measured, n int) {
	var tok, loc []float64
	for i := len(r.Samples) - 1; i >= 0 && len(tok) < calibSamples; i-- {
		s := r.Samples[i]
		if s.TokenP50MS > 0 {
			tok = append(tok, s.TokenP50MS)
			if s.LocalMS > 0 {
				loc = append(loc, s.LocalMS)
			}
		}
	}
	if len(tok) == 0 {
		return plan.Measured{}, 0
	}
	med := func(v []float64) float64 {
		if len(v) == 0 {
			return 0
		}
		sort.Float64s(v)
		if len(v)%2 == 1 {
			return v[len(v)/2]
		}
		return (v[len(v)/2-1] + v[len(v)/2]) / 2
	}
	m.TokenMS, m.LocalMS = med(tok), med(loc)
	if m.LocalMS > 0 && m.TokenMS > m.LocalMS {
		m.RingMS = m.TokenMS - m.LocalMS
	}
	return m, len(tok)
}

// LastMeasuredRun picks the newest run that has trace samples, preferring one of the given model (empty: any).
func LastMeasuredRun(runs []Run, model string) (Run, bool) {
	var any, same *Run
	for i := range runs {
		r := &runs[i]
		if _, n := r.Measured(); n == 0 {
			continue
		}
		if any == nil || r.At.After(any.At) {
			any = r
		}
		if model != "" && strings.EqualFold(r.Model, model) && (same == nil || r.At.After(same.At)) {
			same = r
		}
	}
	if same != nil {
		return *same, true
	}
	if any != nil {
		return *any, true
	}
	return Run{}, false
}

// Package eval scores models with a small fixed suite of programmatically checked prompts (see items.go).
//
// The metric follows IFEval (Zhou et al., 2023, arXiv:2311.07911): every prompt carries verifiable checks and
// the headline score is the prompt-level strict accuracy, the share of prompts for which ALL checks hold; the
// share of individual checks that hold ("instruction level") is reported next to it. No model acts as judge:
// small local judges are not reliable, and deterministic checks cost nothing and never drift.
package eval

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Ask sends one prompt to a target (an endpoint name) and returns the answer.
type Ask func(ctx context.Context, target, prompt string) (string, error)

type ItemResult struct {
	ID       string  `json:"id"`
	Category string  `json:"category"`
	Pass     bool    `json:"pass"`
	Passed   int     `json:"checks_passed"`
	Total    int     `json:"checks_total"`
	Seconds  float64 `json:"seconds"`
	Err      string  `json:"err,omitempty"` // no answer at all (error, or a thinking model that never answered): counts as failed
	Answer   string  `json:"answer,omitempty"`
}

type Result struct {
	Target string       `json:"target"`
	Items  []ItemResult `json:"items"`
}

// Score is the prompt-level strict accuracy, 0..1.
func (r Result) Score() float64 {
	if len(r.Items) == 0 {
		return 0
	}
	n := 0
	for _, it := range r.Items {
		if it.Pass {
			n++
		}
	}
	return float64(n) / float64(len(r.Items))
}

// CheckScore is the instruction-level accuracy: single checks passed / all checks, 0..1.
func (r Result) CheckScore() float64 {
	p, t := 0, 0
	for _, it := range r.Items {
		p, t = p+it.Passed, t+it.Total
	}
	if t == 0 {
		return 0
	}
	return float64(p) / float64(t)
}

// ByCategory is the prompt-level accuracy per category.
func (r Result) ByCategory() map[string]float64 {
	n, ok := map[string]int{}, map[string]int{}
	for _, it := range r.Items {
		n[it.Category]++
		if it.Pass {
			ok[it.Category]++
		}
	}
	out := map[string]float64{}
	for c, k := range n {
		out[c] = float64(ok[c]) / float64(k)
	}
	return out
}

func (r Result) AvgSeconds() float64 {
	if len(r.Items) == 0 {
		return 0
	}
	t := 0.0
	for _, it := range r.Items {
		t += it.Seconds
	}
	return t / float64(len(r.Items))
}

// Run evaluates one target on items, one prompt at a time (a model is not hammered with parallel requests).
func Run(ctx context.Context, ask Ask, target string, items []Item, progress func(done, total int)) Result {
	res := Result{Target: target}
	for i, it := range items {
		if ctx.Err() != nil {
			break
		}
		start := time.Now()
		ans, err := ask(ctx, target, it.Prompt)
		if err != nil && ctx.Err() != nil {
			break // cancelled while this item was out: it did not fail, it was not finished (and must not be scored)
		}
		ir := ItemResult{ID: it.ID, Category: it.Category, Total: len(it.Checks), Seconds: time.Since(start).Seconds()}
		if err != nil {
			ir.Err = err.Error()
		} else {
			ir.Answer = ans
			for _, c := range it.Checks {
				if c.OK(ans) {
					ir.Passed++
				}
			}
			ir.Pass = ir.Passed == ir.Total
		}
		res.Items = append(res.Items, ir)
		if progress != nil {
			progress(i+1, len(items))
		}
	}
	return res
}

// RunAll evaluates several targets, up to parallel of them at the same time.
func RunAll(ctx context.Context, ask Ask, targets []string, items []Item, parallel int, progress func(target string, done, total int)) []Result {
	if parallel < 1 {
		parallel = 1
	}
	out := make([]Result, len(targets))
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func(i int, t string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i] = Run(ctx, ask, t, items, func(d, n int) {
				if progress != nil {
					progress(t, d, n)
				}
			})
		}(i, t)
	}
	wg.Wait()
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score() > out[j].Score() })
	return out
}

// ---- what is kept between runs (small: no answers) ----

type Summary struct {
	Score   float64            `json:"score"`  // prompt-level strict accuracy
	Checks  float64            `json:"checks"` // instruction-level accuracy
	ByCat   map[string]float64 `json:"by_category"`
	Seconds float64            `json:"avg_seconds"`
	Items   int                `json:"items"`
	Suite   int                `json:"suite"`
	When    time.Time          `json:"when"`
}

func Summarize(r Result) Summary {
	return Summary{Score: r.Score(), Checks: r.CheckScore(), ByCat: r.ByCategory(), Seconds: r.AvgSeconds(), Items: len(r.Items), Suite: SuiteVersion, When: time.Now()}
}

func Load(path string) map[string]Summary {
	m := map[string]Summary{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

// Save merges the results into the stored scores.
func Save(path string, rs []Result) error {
	m := Load(path)
	for _, r := range rs {
		if len(r.Items) > 0 {
			m[r.Target] = Summarize(r)
		}
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// a temp name of its own: two evals saving at once must not interleave in one ".tmp"
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(b)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp, 0o600)
	}
	if werr == nil {
		werr = os.Rename(tmp, path)
	}
	if werr != nil {
		_ = os.Remove(tmp)
	}
	return werr
}

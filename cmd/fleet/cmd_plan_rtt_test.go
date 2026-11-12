package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/plan"
	"github.com/tevfik/fleet-router/internal/server"
)

func TestMeasureRTTsInjectedProbe(t *testing.T) {
	t.Setenv("FLEET_STATS", filepath.Join(t.TempDir(), "stats.json"))
	calls := 0
	rttProbe = func(ctx context.Context, addr string) (time.Duration, error) {
		calls++
		if !strings.HasSuffix(addr, ":22") {
			t.Errorf("probe must be a TCP connect to a port, got %s", addr)
		}
		return 46 * time.Millisecond, nil
	}
	defer func() { rttProbe = nil }()
	nodes := []server.NodeStatus{{Node: "p1", Self: true}, {Node: "dino"}, {Node: "vec"}}
	m, lines := measureRTTs(context.Background(), nodes, "", map[string]float64{"p1>vec": 3}, 5, false)
	if m["p1>dino"] != 46 || len(m) != 1 || calls != 5 {
		t.Fatalf("%v calls=%d %v", m, calls, lines)
	}
	if got := mergeRTT(m, map[string]float64{"p1>vec": 3, "p1>dino": 9}); got["p1>dino"] != 9 || got["p1>vec"] != 3 {
		t.Fatalf("--rtt must win: %v", got)
	}
	// second call: cached, no new probes
	m, lines = measureRTTs(context.Background(), nodes, "p1", nil, 5, false)
	if calls != 10 && calls != 15 { // vec was not cached (it was given by hand before)
		t.Fatalf("calls=%d", calls)
	}
	_ = lines
	// master elsewhere: nothing measured
	m, lines = measureRTTs(context.Background(), nodes, "dino", nil, 5, false)
	if m != nil || len(lines) != 1 || !strings.Contains(lines[0], "not this machine") {
		t.Fatalf("%v %v", m, lines)
	}
}

func TestGuardSplit(t *testing.T) {
	defer func(f func(string, string, int, int) (plan.Result, error)) { splitPlanFn = f }(splitPlanFn)
	mk := func(s, sp float64) plan.Result {
		return plan.Result{Candidates: []plan.Candidate{
			{Kind: "single", Master: "dino", TPS: s, Where: "GPU", Parts: []plan.Part{{Node: "dino"}}},
			{Kind: "split", Master: "dino", TPS: sp, Parts: []plan.Part{{Node: "dino"}, {Node: "victor"}}}}}
	}
	var out bytes.Buffer
	splitPlanFn = func(string, string, int, int) (plan.Result, error) { return mk(25.1, 18.4), nil }
	if err := guardSplit(&out, "m.gguf", "dino", []string{"victor"}, 4096, 1, false); err == nil || !strings.Contains(err.Error(), "--force-split") {
		t.Fatalf("must refuse: %v", err)
	}
	out.Reset()
	if err := guardSplit(&out, "m.gguf", "dino", []string{"victor"}, 4096, 1, true); err != nil || !strings.Contains(out.String(), "WARNING") {
		t.Fatalf("forced: %v %q", err, out.String())
	}
	out.Reset()
	splitPlanFn = func(string, string, int, int) (plan.Result, error) { return mk(20, 18), nil }
	if err := guardSplit(&out, "m.gguf", "dino", []string{"victor"}, 4096, 1, false); err != nil || !strings.Contains(out.String(), "WARNING") {
		t.Fatalf("small margin warns only: %v %q", err, out.String())
	}
	out.Reset()
	splitPlanFn = func(string, string, int, int) (plan.Result, error) { return plan.Result{}, errors.New("no daemon") }
	if err := guardSplit(&out, "m.gguf", "dino", []string{"victor"}, 4096, 1, false); err != nil || !strings.Contains(out.String(), "skipped") {
		t.Fatalf("unavailable check must not block: %v %q", err, out.String())
	}
}

func TestCalibrateFromLast(t *testing.T) {
	r := plan.Result{Model: "m", Candidates: []plan.Candidate{
		{Kind: "single", Master: "dino", TokenMS: 40, MasterLocalMS: 40, Parts: []plan.Part{{Node: "dino"}}},
		{Kind: "split", Master: "dino", TokenMS: 55, MasterLocalMS: 20, RingMS: 0, Parts: []plan.Part{{Node: "dino"}, {Node: "vec"}}}}}
	var out bytes.Buffer
	if err := calibrateFromLast(&out, nil, r, 1, 0); err != nil || !strings.Contains(out.String(), "no recorded run") {
		t.Fatalf("%v %q", err, out.String())
	}
	run := server.Run{ID: "r1", Model: "m", Master: "dino", Parts: []server.RunPart{{Node: "dino"}, {Node: "vec"}},
		Samples: []server.RunSample{{TokenP50MS: 70, LocalMS: 25}, {TokenP50MS: 70, LocalMS: 25}}}
	out.Reset()
	if err := calibrateFromLast(&out, []server.Run{run}, r, 1, 0); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	// candidate 2 (same line) is compared: fixed 1.5+(25-20)=6.5, ring 0+(45-35)=10
	if !strings.Contains(s, "candidate 2") || !strings.Contains(s, "--fixed-ms 6.5 --ring-ms 10.0") || !strings.Contains(s, "derived") {
		t.Fatalf("%s", s)
	}
}

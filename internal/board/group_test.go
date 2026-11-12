package board

import (
	"strings"
	"testing"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/plan"
	"github.com/tevfik/fleet-router/internal/server"
)

func TestParseGroup(t *testing.T) {
	g, s, err := ParseGroup([]string{"engine=llama-ring", "workers=a, b,,c", "stall=45s", "stats=18191", "addrs=a=10.0.0.2:50152,b=10.0.0.3:50152"})
	if err != nil || g.Engine != "llama-ring" || strings.Join(g.Workers, "|") != "a|b|c" || s != "45s" || g.StatsPort != 18191 || g.Addrs["b"] != "10.0.0.3:50152" {
		t.Fatalf("%+v %q %v", g, s, err)
	}
	for _, off := range [][]string{nil, {"off"}} {
		if g, _, err := ParseGroup(off); g.Engine != "" || err != nil {
			t.Fatalf("off: %q %v", g.Engine, err)
		}
	}
	for _, bad := range [][]string{{"workers=a"}, {"engine"}, {"engine=x", "colour=red"}, {"engine=x", "stats=http"}, {"engine=x", "addrs=a=nohostport"}} {
		if _, _, err := ParseGroup(bad); err == nil {
			t.Fatalf("%v must be refused", bad)
		}
	}
}

func TestGroupLines(t *testing.T) {
	var e server.EpStatus
	if l := GroupLines(e); len(l) != 0 {
		t.Fatalf("a plain endpoint says nothing: %v", l)
	}
	e.Group = &config.Group{Engine: "prima", Workers: []string{"w1", "w2"}}
	if l := GroupLines(e); len(l) != 2 || !strings.Contains(l[1], "not read yet") || !strings.Contains(l[0], "stall limit 1m30s") {
		t.Fatalf("%v", l)
	}
	e.Paused = "stalled: no data for 1m30s"
	e.GroupLive = &server.GroupState{Engine: "prima", Health: "ok", Slots: 2, Busy: 1, Ctx: 4096, TPS: 9.5, KVUsage: 0.5,
		Members: []server.GroupMember{{Node: "w1", Up: true}, {Node: "w2", Err: "no route"}},
		Warn:    []string{"slots is 4 in the config, the engine runs 2"}}
	all := strings.Join(GroupLines(e), "\n")
	for _, want := range []string{"⏸ set aside — stalled", "✓ w1", "✗ w2 — no route", "1/2 slots busy", "ctx 4096", "9.5 t/s", "KV 50%", "⚠ slots is 4"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in\n%s", want, all)
		}
	}
	if !strings.Contains(GroupText(e), "engine=prima workers=w1,w2") {
		t.Fatal(GroupText(e))
	}
}

func TestRunLinesSetMeasuredBesidePlanned(t *testing.T) {
	r := server.Run{ID: "r1", Model: "m", Master: "dino", PredTPS: 10, PredTokenMS: 100,
		Parts: []server.RunPart{{Node: "dino", Layers: 10, ComputeMS: 40}, {Node: "mac", First: 10, Layers: 10, ComputeMS: 20, RTTms: 5, SendMS: 1}, {Node: "pi", First: 20, Layers: 5}},
		Samples: []server.RunSample{{TPS: 9, LocalMS: 4.8, Nodes: []server.RingNode{
			{Node: "mac", BusyMS: 12, WireMS: 0.5, Samples: 50}, {Node: "pi", Samples: 1}}}}}
	all := strings.Join(RunLines(r, nil), "\n")
	for _, want := range []string{"own time 4.8 ms per token (planned 40.0)", "compute 12.0 ms (planned 20.0) · wire 0.5 ms (planned 6.0)", "too few samples (1)"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in\n%s", want, all)
		}
	}
	e := server.EpStatus{Endpoint: config.Endpoint{Group: &config.Group{Engine: "llama-ring"}}, GroupLive: &server.GroupState{
		Trace: &server.RingTrace{Steps: 78, TokenMeanMS: 17.3, TokenP50MS: 16.3, LocalMS: 4.8, RingMS: 12.4,
			Nodes: []server.RingNode{{Addr: "10.0.0.2:1", BusyMS: 12, WireMS: 0.1, SendMS: 0.1, Samples: 78}}}}}
	if g := strings.Join(GroupLines(e), "\n"); !strings.Contains(g, "profile (last 78 tokens)") || !strings.Contains(g, "10.0.0.2:1: compute 12.0 ms · wire 0.1 ms") {
		t.Fatal(g)
	}
}

func TestRunLinesFlagAWorkerTheEngineHasButThePlanDoesNot(t *testing.T) {
	r := server.Run{ID: "r1", Master: "dino", Parts: []server.RunPart{{Node: "dino", Layers: 24}},
		Samples: []server.RunSample{{LocalMS: 4, Nodes: []server.RingNode{{Node: "dino", BusyMS: 12, WireMS: 0.1, Samples: 20}}}}}
	if all := strings.Join(RunLines(r, nil), "\n"); !strings.Contains(all, "dino is a worker in the engine but not in the plan") {
		t.Fatal(all)
	}
}

func TestGroupLinesAndRunLines_ShowSessionReconnects(t *testing.T) {
	// ROADMAP 1.3: session reconnects in GroupLines (fleet status)
	e := server.EpStatus{
		Endpoint: config.Endpoint{
			Group: &config.Group{
				Engine:  "llama-ring",
				Workers: []string{"w1"},
				Addrs:   map[string]string{"w1": "10.0.0.2:50152"},
			},
		},
		GroupLive: &server.GroupState{
			Engine: "llama-ring",
			Trace: &server.RingTrace{
				Reconnects: map[string]server.ReconnectStat{
					"10.0.0.2:50152": {Resumed: 3, Replayed: 12, ReplyResent: 2, LastDowntimeMS: 45},
				},
			},
		},
	}
	lines := strings.Join(GroupLines(e), "\n")
	if !strings.Contains(lines, "session reconnects: w1 (resumed 3×, replayed 12, resent 2, last downtime 45 ms)") {
		t.Fatalf("GroupLines missing reconnects summary:\n%s", lines)
	}

	// ROADMAP 1.3: session reconnects in RunLines (fleet run show)
	r := server.Run{
		ID:     "run-test",
		Master: "dino",
		Samples: []server.RunSample{
			{
				Busy: 1, Slots: 4, KV: 0.25, Health: "ok",
				Reconnects: map[string]server.ReconnectStat{
					"10.0.0.2:50152": {Resumed: 2, Replayed: 5, LastDowntimeMS: 30},
				},
			},
		},
	}
	rlines := strings.Join(RunLines(r, nil), "\n")
	if !strings.Contains(rlines, "session reconnects: 10.0.0.2:50152 (resumed 2×, replayed 5, resent 0, last downtime 30 ms)") {
		t.Fatalf("RunLines missing reconnects summary:\n%s", rlines)
	}
}

func TestPlanExplainShowsTheItemsOfTheTime(t *testing.T) {
	c := plan.Candidate{Master: "dino", TokenMS: 51.3, TPS: 19.5, MasterLocalMS: 20, RingMS: 8,
		Parts: []plan.Part{{Node: "dino", Layers: 33, ComputeMS: 18.5}, {Node: "victor", Layers: 7, ComputeMS: 3.6, RTTms: 12, SendMS: 0.2}}}
	all := strings.Join(PlanExplain(c, 2), "\n")
	for _, want := range []string{"candidate 2 — 51.3 ms per token", "own blocks   18.5 ms + fixed  1.5 ms", "compute    3.6 ms + wire  12.0 ms + send  0.2 ms", "--ring-ms", "total"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in\n%s", want, all)
		}
	}
}

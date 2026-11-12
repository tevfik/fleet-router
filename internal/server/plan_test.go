package server

import (
	"strings"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/hostinfo"
	"github.com/tevfik/fleet-router/internal/plan"
	"github.com/tevfik/fleet-router/internal/rtt"
)

func tinyModel() plan.Model {
	m := plan.Model{Name: "tiny", Arch: "llama", Layers: 4, Embd: 1024, KVPerTokenLayer: 4096, HiddenBytes: 4096,
		OutputBytes: 50 << 20, EmbdBytes: 50 << 20, OtherBytes: 100 << 20}
	for i := 0; i < 4; i++ {
		m.LayerBytes, m.LayerRead = append(m.LayerBytes, 100<<20), append(m.LayerRead, 100<<20)
	}
	return m
}

// planServer has three endpoints on three nodes, all reached locally (so every node answers from this machine).
func planServer(t *testing.T) *Server {
	up := rtNewUp(t, "a")
	s, _ := rtFileServer(t, &config.Config{Listen: config.DefaultListen, Endpoints: []config.Endpoint{
		rtEp("a", "n1", up.URL(), "ma"), rtEp("b", "n2", up.URL(), "mb"), rtEp("c", "n3", up.URL(), "mc")},
		Roles: map[string]config.Role{"r": {Preferred: "a"}}})
	return s
}

func TestPlan_Endpoint(t *testing.T) {
	s := planServer(t)
	s.hwb.res = &hostinfo.Bench{Fingerprint: "x", FastMem: 4 << 30, FastBW: 50, FastWhere: "RAM", RAMTotal: 8 << 30, RAMBW: 50}
	s.hwb.loaded = true
	rec := rtDo(t, s, "POST", "/fleet/plan", PlanRequest{Model: tinyModel(), Ctx: 512, Np: 2})
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var r plan.Result
	rtJSON(t, rec, &r)
	if r.Model != "tiny" || r.Ctx != 512 || r.Np != 2 || len(r.Candidates) == 0 {
		t.Fatalf("%+v", r)
	}
	if r.Candidates[0].TPS <= 0 || r.Verdict == "" {
		t.Fatalf("%+v", r.Candidates[0])
	}
	// a fixed master restricts the candidates
	rec = rtDo(t, s, "POST", "/fleet/plan", PlanRequest{Model: tinyModel(), Master: "n2"})
	rtJSON(t, rec, &r)
	for _, c := range r.Candidates {
		if c.Master != "n2" {
			t.Fatalf("master n2: %+v", c)
		}
	}
}

func TestPlan_EndpointRefusesBadInput(t *testing.T) {
	s := planServer(t)
	if rec := rtDo(t, s, "POST", "/fleet/plan", PlanRequest{}); rec.Code != 400 || !strings.Contains(rec.Body.String(), "incomplete") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	bad := tinyModel()
	bad.LayerRead = bad.LayerRead[:2]
	if rec := rtDo(t, s, "POST", "/fleet/plan", PlanRequest{Model: bad}); rec.Code != 400 {
		t.Fatalf("%d", rec.Code)
	}
	if rec := rtDo(t, s, "POST", "/fleet/plan", "not json"); rec.Code != 400 {
		t.Fatalf("%d", rec.Code)
	}
	if rec := rtDo(t, s, "POST", "/fleet/plan", PlanRequest{Model: tinyModel()}, rtRemote); rec.Code != 403 {
		t.Fatalf("a plan is an admin call: %d", rec.Code)
	}
}

func TestPlan_NodesWithoutBenchmarkAreLeftOut(t *testing.T) {
	s := planServer(t)
	s.hwb.loaded = true // never measured
	rec := rtDo(t, s, "POST", "/fleet/plan", PlanRequest{Model: tinyModel()})
	var r plan.Result
	rtJSON(t, rec, &r)
	if len(r.Candidates) != 0 || len(r.Skipped) == 0 || !strings.HasPrefix(r.Verdict, "no benchmark data yet") {
		t.Fatalf("%+v", r)
	}
}

func TestHostReport_IncludesLocalRTT(t *testing.T) {
	s := planServer(t)
	rttPath := config.RTTPath()
	c := &rtt.Cache{Path: rttPath}
	if err := c.Store(map[string]rtt.Sample{
		"this>n2": {MS: 18.5},
		"this>n3": {MS: 42.0},
	}); err != nil {
		t.Fatal(err)
	}

	h := s.localHost("this")
	if len(h.RTT) != 2 || h.RTT["this>n2"] != 18.5 || h.RTT["this>n3"] != 42.0 {
		t.Fatalf("expected local RTT to be loaded into HostReport, got: %+v", h.RTT)
	}
}

func TestPlan_UsesMasterCentricRTTMatrix(t *testing.T) {
	// Setup server with a remote master node "dino" (URL not local loopback)
	up := rtNewUp(t, "a")
	s, _ := rtFileServer(t, &config.Config{
		Listen: config.DefaultListen,
		Endpoints: []config.Endpoint{
			rtEp("local-ep", "local-node", up.URL(), "m1"),
			rtEp("dino-ep", "dino", "http://10.0.1.100:18090", "m2"),
			rtEp("vec-ep", "victor", "http://10.0.1.101:18090", "m3"),
		},
		Roles: map[string]config.Role{"r": {Preferred: "local-ep"}},
	})
	s.hwb.res = &hostinfo.Bench{Fingerprint: "x", FastMem: 4 << 30, FastBW: 50, FastWhere: "RAM", RAMTotal: 8 << 30, RAMBW: 50}
	s.hwb.loaded = true

	// Simulate dino's HostReport reporting RTT to victor
	s.hosts.m = map[string]HostReport{
		"dino": {
			Node:  "dino",
			RTT:   map[string]float64{"dino>victor": 35.0},
			At:    time.Now(),
			Bench: &hostinfo.Bench{Fingerprint: "x", FastMem: 4 << 30, FastBW: 50, FastWhere: "RAM", RAMTotal: 8 << 30, RAMBW: 50},
		},
		"victor": {
			Node:  "victor",
			At:    time.Now(),
			Bench: &hostinfo.Bench{Fingerprint: "x", FastMem: 4 << 30, FastBW: 50, FastWhere: "RAM", RAMTotal: 8 << 30, RAMBW: 50},
		},
	}

	// Request plan with master dino and no explicit RTT
	rec := rtDo(t, s, "POST", "/fleet/plan", PlanRequest{Model: tinyModel(), Master: "dino"})
	if rec.Code != 200 {
		t.Fatalf("%d: %s", rec.Code, rec.Body)
	}
	var r plan.Result
	rtJSON(t, rec, &r)
	if len(r.Candidates) == 0 {
		t.Fatalf("expected candidates, got none: %+v", r)
	}

	// Verify that candidate parts involving victor use dino's reported RTT (35ms)
	foundMasterRTT := false
	for _, c := range r.Candidates {
		if c.Kind == "split" {
			for _, p := range c.Parts {
				if p.Node == "victor" && p.RTTms == 35.0 {
					foundMasterRTT = true
				}
			}
		}
	}
	if !foundMasterRTT {
		t.Fatalf("expected candidate parts to use dino's reported RTT (35ms for victor): candidates=%+v", r.Candidates)
	}

	// Now verify that caller's explicit RTT overrides master's reported RTT
	rec = rtDo(t, s, "POST", "/fleet/plan", PlanRequest{
		Model:  tinyModel(),
		Master: "dino",
		RTT:    map[string]float64{"dino>victor": 99.0},
	})
	if rec.Code != 200 {
		t.Fatalf("%d: %s", rec.Code, rec.Body)
	}
	rtJSON(t, rec, &r)
	foundExplicitRTT := false
	for _, c := range r.Candidates {
		if c.Kind == "split" {
			for _, p := range c.Parts {
				if p.Node == "victor" && p.RTTms == 99.0 {
					foundExplicitRTT = true
				}
			}
		}
	}
	if !foundExplicitRTT {
		t.Fatalf("expected explicit RTT (99ms) to override master's reported RTT: candidates=%+v", r.Candidates)
	}
}

func TestPlanEvaluate_Endpoint(t *testing.T) {
	s := planServer(t)
	s.hwb.res = &hostinfo.Bench{Fingerprint: "x", FastMem: 4 << 30, FastBW: 50, FastWhere: "RAM", RAMTotal: 8 << 30, RAMBW: 50}
	s.hwb.loaded = true
	req := EvaluateRequest{PlanRequest: PlanRequest{Model: tinyModel(), Ctx: 512, Np: 1}, Alloc: []plan.Alloc{{Node: "n1", Layers: 2}, {Node: "n2", Layers: 2}}}
	rec := rtDo(t, s, "POST", "/fleet/plan/evaluate", req)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var ev plan.Evaluation
	rtJSON(t, rec, &ev)
	if ev.Total != 4 || ev.Assigned != 4 || len(ev.Parts) != 2 || len(ev.Machines) < 3 {
		t.Fatalf("%+v", ev)
	}
	if ev.Candidate == nil || ev.Candidate.Master != "n1" {
		t.Fatalf("%+v", ev)
	}
	// too few blocks is reported, not refused
	req.Alloc = []plan.Alloc{{Node: "n1", Layers: 1}, {Node: "n2", Layers: 1}}
	rtJSON(t, rtDo(t, s, "POST", "/fleet/plan/evaluate", req), &ev)
	if ev.OK || len(ev.Problems) == 0 {
		t.Fatalf("%+v", ev)
	}
	if rec := rtDo(t, s, "POST", "/fleet/plan/evaluate", EvaluateRequest{}); rec.Code != 400 {
		t.Fatalf("%d", rec.Code)
	}
	if rec := rtDo(t, s, "POST", "/fleet/plan/evaluate", req, rtRemote); rec.Code != 403 {
		t.Fatalf("an admin call: %d", rec.Code)
	}
}

// The editor is seeded from a candidate of the plan: checked unchanged, it must give that candidate's own speed.
func TestPlanEvaluate_AgreesWithThePlansCandidates(t *testing.T) {
	s := planServer(t)
	s.hwb.res = &hostinfo.Bench{Fingerprint: "x", FastMem: 4 << 30, FastBW: 50, FastWhere: "RAM", RAMTotal: 8 << 30, RAMBW: 50}
	s.hwb.loaded = true
	pr := PlanRequest{Model: tinyModel(), Ctx: 512, Np: 1}
	var res plan.Result
	rtJSON(t, rtDo(t, s, "POST", "/fleet/plan", pr), &res)
	n := 0
	for _, c := range res.Candidates {
		if c.Kind != "split" {
			continue
		}
		var al []plan.Alloc
		out := false
		for _, p := range c.Parts {
			al = append(al, plan.Alloc{Node: p.Node, Layers: p.Layers})
			out = out || (p.Output && p.Node == c.Master)
		}
		var ev plan.Evaluation
		rtJSON(t, rtDo(t, s, "POST", "/fleet/plan/evaluate", EvaluateRequest{PlanRequest: pr, Alloc: al, OutMaster: out}), &ev)
		if ev.Candidate == nil {
			t.Fatalf("%v: %v", al, ev.Problems)
		}
		if d := ev.Candidate.TPS - c.TPS; d > 1e-6 || d < -1e-6 {
			t.Errorf("%v out-master=%v: editor %.3f tok/s, plan %.3f", al, out, ev.Candidate.TPS, c.TPS)
		}
		n++
	}
	if n == 0 {
		t.Skip("no split candidate on this tiny model")
	}
}

func TestPlanSnapshot_ReusedOnlyForTheSamePlanAndWhileFresh(t *testing.T) {
	var p planSnapshot
	if _, ok := p.get("k", true); ok {
		t.Fatal("nothing was kept yet")
	}
	p.put("k", planSnap{nodes: []plan.Node{{Name: "a"}}})
	if sn, ok := p.get("k", true); !ok || len(sn.nodes) != 1 {
		t.Fatal("the plan just made is reused")
	}
	if _, ok := p.get("k", false); ok {
		t.Fatal("a plan itself always reads the machines")
	}
	if _, ok := p.get("other", true); ok {
		t.Fatal("another file or setting is not the same plan")
	}
	p.at = time.Now().Add(-planSnapMaxAge - time.Second)
	if _, ok := p.get("k", true); ok {
		t.Fatal("an old snapshot is not used")
	}
}

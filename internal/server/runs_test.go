package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/plan"
)

func runPlan() plan.Result {
	return plan.Result{Model: "big-70b", SHA256: "abc123", Ctx: 4096, Np: 2, Candidates: []plan.Candidate{{
		Kind: "split", Master: "n1", TokenMS: 100, TPS: 10,
		Parts: []plan.Part{
			{Node: "w1", Layers: 20, First: 30, Output: true, ComputeMS: 20, RTTms: 5},
			{Node: "n1", Layers: 30, First: 0, ComputeMS: 40},
		}}}}
}

func runServer(t *testing.T) (*Server, *rtUp) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	up := rtNewUp(t, "g")
	e := rtEp("g", "n1", up.URL(), "m")
	e.Probe = "any"
	e.Group = &config.Group{Engine: "llama-ring", Workers: []string{"w1"}}
	e2 := rtEp("far", "w1", "http://10.255.255.1:1/v1", "m2")
	s, _ := rtFileServer(t, &config.Config{Name: "home", Endpoints: []config.Endpoint{e, e2}})
	s.routes = func(string) []nodeRoute { return nil }
	s.hosts.m = map[string]HostReport{"w1": {Node: "w1", At: time.Now().Add(time.Hour)}}
	return s, up
}

func TestRuns_RecordBindSampleAndPersist(t *testing.T) {
	s, _ := runServer(t)
	r, err := s.RecordRun(runPlan(), 1, "g", "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Master != "n1" || r.Parts[0].Node != "n1" || r.Parts[1].Node != "w1" || !r.Parts[1].Output || r.Engine != "llama-ring" || r.PredTPS != 10 {
		t.Fatalf("%+v", r)
	}
	if _, err := s.RecordRun(runPlan(), 2, "", ""); err == nil {
		t.Fatal("there is no candidate 2")
	}
	if _, err := s.RecordRun(runPlan(), 1, "nope", ""); err == nil {
		t.Fatal("an unknown endpoint must be refused")
	}
	// a probe of the bound endpoint adds a sample; a stall and a missing worker become events
	ep := *s.Config().Endpoint("g")
	s.probe(context.Background(), &ep)
	s.penalizeFor("g", time.Minute, "stalled: no data for 1m30s")
	got, _ := s.runs.get(r.ID[:3])
	if len(got.Samples) != 1 || got.Samples[0].At.IsZero() {
		t.Fatalf("samples: %+v", got.Samples)
	}
	if len(got.Events) < 2 || !strings.Contains(got.Events[len(got.Events)-1].What, "stalled") {
		t.Fatalf("events: %+v", got.Events)
	}
	// the file keeps it: a second store reads it back
	var again runStore
	if l := again.recent(5); len(l) != 1 || l[0].ID != r.ID {
		t.Fatalf("persisted: %+v", l)
	}
}

func TestRuns_EndedStopSampling(t *testing.T) {
	s, _ := runServer(t)
	r, _ := s.RecordRun(runPlan(), 1, "g", "")
	rec := rtDo(t, s, "POST", "/fleet/runs/set", map[string]any{"id": r.ID, "end": true})
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	ep := *s.Config().Endpoint("g")
	s.probe(context.Background(), &ep)
	if got, _ := s.runs.get(r.ID); got.Active() || len(got.Samples) != 0 {
		t.Fatalf("an ended run is left alone: %+v", got)
	}
	if rec := rtDo(t, s, "POST", "/fleet/runs/set", map[string]any{"id": "zzz", "end": true}); rec.Code != 404 {
		t.Fatalf("unknown run: %d", rec.Code)
	}
	if rec := rtDo(t, s, "POST", "/fleet/runs/set", map[string]any{"id": r.ID, "endpoint": "nope"}); rec.Code != 400 {
		t.Fatalf("unknown endpoint: %d", rec.Code)
	}
}

func TestRuns_HTTPRecordAndShare(t *testing.T) {
	s, _ := runServer(t)
	rec := rtDo(t, s, "POST", "/fleet/runs", map[string]any{"plan": runPlan(), "pick": 1, "endpoint": "g"})
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var r Run
	rtJSON(t, rec, &r)
	if h := s.localHost("n1"); len(h.Runs) != 1 || h.Runs[0].ID != r.ID {
		t.Fatalf("the machine readings must carry the runs: %+v", h.Runs)
	}
	// another fleet's run arrives through its machine's readings; ours is not listed twice
	other := Run{ID: "rother", Fleet: "far-fleet", At: time.Now().Add(time.Hour), Model: "x", Parts: []RunPart{{Node: "w1"}}}
	s.hosts.m["w1"] = HostReport{Node: "w1", At: time.Now().Add(time.Hour), Runs: []Run{other, r}}
	all := s.AllRuns(context.Background())
	if len(all) != 2 || all[0].ID != "rother" || all[1].ID != r.ID {
		t.Fatalf("%+v", all)
	}
	// …and it cannot be changed from here
	if rec := rtDo(t, s, "POST", "/fleet/runs/set", map[string]any{"id": "rother", "end": true}); rec.Code != 404 {
		t.Fatalf("someone else's run: %d", rec.Code)
	}
	if rec := rtDo(t, s, "GET", "/fleet/runs", nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), "rother") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestDrain_LeaveTheLine(t *testing.T) {
	s, _ := runServer(t)
	r, _ := s.RecordRun(runPlan(), 1, "g", "")
	if !s.healthy("g") {
		t.Fatal("healthy before")
	}
	if rec := rtDo(t, s, "POST", "/fleet/drain", map[string]any{"on": true}); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if s.healthy("g") {
		t.Fatal("this machine's own models come last once it has left")
	}
	if !s.healthy("far") {
		t.Fatal("other machines' models are not affected")
	}
	nodes, left := s.planNodes(httptest.NewRequest("POST", "/fleet/plan", nil), 0)
	for _, n := range nodes {
		if n.Name == "n1" {
			t.Fatal("plans must skip the machine that left")
		}
	}
	if len(left) != 1 || left[0] != "n1" {
		t.Fatalf("left: %v", left)
	}
	if h := s.localHost("n1"); !h.Draining {
		t.Fatal("the readings say it has left")
	}
	if got, _ := s.runs.get(r.ID); !strings.Contains(got.Events[len(got.Events)-1].What, "left the line") {
		t.Fatalf("the run must be told: %+v", got.Events)
	}
	rtDo(t, s, "POST", "/fleet/drain", map[string]any{"on": false})
	if !s.healthy("g") {
		t.Fatal("back in the line")
	}
}

func TestDrain_GroupSeesAWorkerLeave(t *testing.T) {
	s, _ := runServer(t)
	s.hosts.m["w1"] = HostReport{Node: "w1", Draining: true, At: time.Now().Add(time.Hour)}
	ep := *s.Config().Endpoint("g")
	s.probe(context.Background(), &ep)
	st := s.getState("g")
	if st.OK || !strings.Contains(st.Err, "w1") || st.Group.Members[0].Err != "left the line" {
		t.Fatalf("%+v %+v", st, st.Group)
	}
}

func TestFleetPort_Routes(t *testing.T) {
	eSSH := rtEp("s", "dino", "http://x/v1", "m1")
	eSSH.URL, eSSH.SSH, eSSH.Remote, eSSH.FleetPort = "", "dino", "127.0.0.1:8080", 4800
	eURL := rtEp("u", "victor", "http://10.0.0.9:8080/v1", "m2")
	eURL.FleetPort = 4712
	ePlain := rtEp("p", "mac", "http://10.0.0.8:8080/v1", "m3")
	s := &Server{cfg: &config.Config{Endpoints: []config.Endpoint{eSSH, eURL, ePlain}}} // routes only: no tunnel is opened
	if r := s.nodeRoutes("dino"); len(r) != 1 || r[0].remote != "127.0.0.1:4800" {
		t.Fatalf("over SSH, the fleet is where fleet_port says: %+v", r)
	}
	if r := s.nodeRoutes("victor"); len(r) != 1 || r[0].base != "http://10.0.0.9:4712" {
		t.Fatalf("by address: %+v", r)
	}
	if r := s.nodeRoutes("mac"); len(r) != 1 || r[0].base != "http://10.0.0.8:4711" {
		t.Fatalf("the default stays: %+v", r)
	}
}

func TestFleetPort_EndpointSet(t *testing.T) {
	up := rtNewUp(t, "a")
	s, _ := rtFileServer(t, &config.Config{Name: "home", Endpoints: []config.Endpoint{rtEp("p", "mac", up.URL(), "m3")}})
	if rec := rtDo(t, s, "POST", "/fleet/endpoint-set", map[string]any{"name": "p", "fleet_port": 4900}); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if s.Config().Endpoint("p").FleetPort != 4900 {
		t.Fatal("not saved")
	}
	if rec := rtDo(t, s, "POST", "/fleet/endpoint-set", map[string]any{"name": "p", "fleet_port": 70000}); rec.Code != 400 {
		t.Fatalf("a port above 65535: %d", rec.Code)
	}
	rtDo(t, s, "POST", "/fleet/endpoint-set", map[string]any{"name": "p", "fleet_port": 0})
	if s.Config().Endpoint("p").FleetPort != 0 {
		t.Fatal("0 goes back to 4711")
	}
}

func TestRuns_DeleteAndPruneKeepActive(t *testing.T) {
	s, _ := runServer(t)
	live, _ := s.RecordRun(runPlan(), 1, "g", "")
	time.Sleep(3 * time.Millisecond) // ids are millisecond stamps
	done1, _ := s.RecordRun(runPlan(), 1, "", "")
	time.Sleep(3 * time.Millisecond)
	done2, _ := s.RecordRun(runPlan(), 1, "", "")
	for _, r := range []Run{done1, done2} {
		s.runs.update(r.ID, func(x *Run) { x.Ended = time.Now() })
	}
	del := func(body map[string]any) (int, []Run) {
		rec := rtDo(t, s, "POST", "/fleet/runs/delete", body)
		var out struct{ Removed []Run }
		if rec.Code == 200 {
			rtJSON(t, rec, &out)
		}
		return rec.Code, out.Removed
	}
	if code, _ := del(map[string]any{}); code != 400 {
		t.Fatalf("neither id nor prune: %d", code)
	}
	if code, _ := del(map[string]any{"id": live.ID}); code != 409 {
		t.Fatalf("an active run is refused: %d", code)
	}
	if code, _ := del(map[string]any{"id": "zzz"}); code != 404 {
		t.Fatalf("unknown: %d", code)
	}
	// dry-run reports but keeps everything
	if code, got := del(map[string]any{"prune": true, "dry_run": true}); code != 200 || len(got) != 2 {
		t.Fatalf("dry prune: %d %+v", code, got)
	}
	if n := len(s.runs.recent(10)); n != 3 {
		t.Fatalf("dry run deleted: %d left", n)
	}
	if code, got := del(map[string]any{"id": done1.ID}); code != 200 || len(got) != 1 || got[0].ID != done1.ID {
		t.Fatalf("rm: %d %+v", code, got)
	}
	if code, got := del(map[string]any{"prune": true}); code != 200 || len(got) != 1 || got[0].ID != done2.ID {
		t.Fatalf("prune: %d %+v", code, got)
	}
	left := s.runs.recent(10)
	if len(left) != 1 || left[0].ID != live.ID || !left[0].Active() {
		t.Fatalf("the active run must survive: %+v", left)
	}
	var again runStore // persisted
	if l := again.recent(5); len(l) != 1 || l[0].ID != live.ID {
		t.Fatalf("persisted: %+v", l)
	}
}

// A run that was never ended but whose engine is gone is suggested as stale (never changed by listing), and
// `prune --stale` marks it lost without touching anything else.
func TestRuns_StaleSuggestionAndMarkLost(t *testing.T) {
	s, _ := runServer(t)
	alive := true
	old := runAlive
	runAlive = func(context.Context, *http.Client, string, string) bool { return alive }
	t.Cleanup(func() { runAlive = old })

	stale, _ := s.RecordRun(runPlan(), 1, "g", "")
	time.Sleep(3 * time.Millisecond)
	fresh, _ := s.RecordRun(runPlan(), 1, "g", "")
	time.Sleep(3 * time.Millisecond)
	unbound, _ := s.RecordRun(runPlan(), 1, "", "")
	age := func(id string, by time.Duration) {
		s.runs.update(id, func(x *Run) {
			x.At = x.At.Add(-by)
			for i := range x.Samples {
				x.Samples[i].At = x.Samples[i].At.Add(-by)
			}
		})
	}
	age(stale.ID, 3*24*time.Hour) // the Oct 30 case: nothing healthy since
	age(unbound.ID, 7*time.Hour)

	list := func() map[string]Run {
		rec := rtDo(t, s, "GET", "/fleet/runs", nil)
		var out struct{ Runs []Run }
		rtJSON(t, rec, &out)
		m := map[string]Run{}
		for _, r := range out.Runs {
			m[r.ID] = r
		}
		return m
	}
	alive = false
	got := list()
	if !strings.Contains(got[stale.ID].Stale, "no healthy answer") || !strings.Contains(got[unbound.ID].Stale, "never bound") {
		t.Fatalf("stale: %q unbound: %q", got[stale.ID].Stale, got[unbound.ID].Stale)
	}
	if got[fresh.ID].Stale != "" {
		t.Fatalf("a recent run is not stale: %q", got[fresh.ID].Stale)
	}
	// the engine answers now (the daemon just had not sampled it): not stale
	alive = true
	if got = list(); got[stale.ID].Stale != "" {
		t.Fatalf("alive: %q", got[stale.ID].Stale)
	}
	alive = false
	// listing changed nothing in the store
	if r, _ := s.runs.get(stale.ID); !r.Active() || r.Stale != "" || r.Lost != "" {
		t.Fatalf("%+v", r)
	}
	// dry run reports, then the real one marks
	rec := rtDo(t, s, "POST", "/fleet/runs/delete", map[string]any{"stale": true, "dry_run": true})
	var out struct{ Lost []Run }
	rtJSON(t, rec, &out)
	if len(out.Lost) != 2 {
		t.Fatalf("dry: %+v", out.Lost)
	}
	if r, _ := s.runs.get(stale.ID); !r.Active() {
		t.Fatal("dry run changed a run")
	}
	rec = rtDo(t, s, "POST", "/fleet/runs/delete", map[string]any{"stale": true})
	rtJSON(t, rec, &out)
	r, _ := s.runs.get(stale.ID)
	if r.Active() || r.Lost == "" || len(r.Events) == 0 || !strings.HasPrefix(r.Events[len(r.Events)-1].What, "lost:") {
		t.Fatalf("%+v", r)
	}
	if f, _ := s.runs.get(fresh.ID); !f.Active() {
		t.Fatal("the fresh run must stay active")
	}
	if n := len(s.runs.recent(10)); n != 3 {
		t.Fatalf("no record is deleted: %d", n)
	}
	if code := rtDo(t, s, "POST", "/fleet/runs/delete", map[string]any{"stale": true, "id": "x"}).Code; code != 200 && code != 400 {
		t.Fatalf("%d", code)
	}
}

// A run is found by its endpoint's name too: the active one, else the newest (an id or prefix still wins).
func TestRuns_FindByEndpointName(t *testing.T) {
	s, _ := runServer(t)
	s.runs.add(Run{ID: "run-1", Endpoint: "big", At: time.Now().Add(-2 * time.Hour), Ended: time.Now().Add(-time.Hour)})
	s.runs.add(Run{ID: "run-2", Endpoint: "big", At: time.Now().Add(-time.Minute)})
	s.runs.add(Run{ID: "run-3", Endpoint: "big", At: time.Now(), Ended: time.Now()})
	if r, ok := s.runs.get("big"); !ok || r.ID != "run-2" {
		t.Fatalf("active run by endpoint: %v %+v", ok, r)
	}
	s.runs.update("run-2", func(x *Run) { x.Ended = time.Now() })
	if r, ok := s.runs.get("big"); !ok || r.ID != "run-3" {
		t.Fatalf("newest run by endpoint: %v %+v", ok, r)
	}
	if r, _ := s.runs.get("run-1"); r.ID != "run-1" {
		t.Fatal("an id still wins")
	}
	if _, ok := s.runs.get("nope"); ok {
		t.Fatal("unknown")
	}
}

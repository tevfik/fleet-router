package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/deploy"
)

type stopCall struct {
	node  string
	local bool
	port  int
	names string
	dry   bool
}

// fakeStop replaces stopPort: answers per port ("stopped", "none", "found") and records the order of the calls.
func fakeStop(t *testing.T, answers map[int]string, held map[int][]string) *[]stopCall {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir()) // a real stop marks the deploy records of the endpoint: not the user's
	var mu sync.Mutex
	var calls []stopCall
	old := stopPort
	stopPort = func(_ context.Context, node string, local bool, port int, names []string, dry bool) (string, []string, error) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, stopCall{node, local, port, strings.Join(names, "|"), dry})
		a, ok := answers[port]
		if !ok {
			return "", nil, fmt.Errorf("ssh: connect failed")
		}
		if dry && a == "stopped" {
			a = "found"
		}
		return a, held[port], nil
	}
	t.Cleanup(func() { stopPort = old })
	return &calls
}

func ringEndpoint() config.Endpoint {
	return config.Endpoint{Name: "ring", Node: "dino", SSH: "dino", Remote: "127.0.0.1:18090", Model: "m",
		Group: &config.Group{Engine: "llama-ring", Workers: []string{"w1", "w2", "w3"},
			Addrs:   map[string]string{"w1": "10.0.0.2:50052", "w2": "10.0.0.3:50052"},
			Streams: map[string]string{"w2": "127.0.0.1:51000"}}}
}

// The ring stops master first, then its streams, then the workers; a worker without an address is a warning.
func TestStopEndpointOrderAndNames(t *testing.T) {
	s, _ := newNodeDaemon(t, &config.Config{Name: "laptop"})
	calls := fakeStop(t, map[int]string{18090: "stopped", 51000: "stopped", 50052: "none"}, nil)
	rep := s.StopEndpoint(context.Background(), ringEndpoint(), StopOptions{})
	if rep.Failed() || rep.Refused != "" {
		t.Fatalf("%+v", rep)
	}
	var order []string
	for _, c := range *calls {
		order = append(order, fmt.Sprintf("%d", c.port))
	}
	if got := strings.Join(order, ","); got != "18090,51000,50052,50052" {
		t.Fatalf("order %s", got)
	}
	wantMaster := strings.Join(stopMasterNames, "|")
	if (*calls)[0].names != wantMaster || (*calls)[0].node != "dino" || (*calls)[0].local {
		t.Fatalf("master call %+v", (*calls)[0])
	}
	wantWorker := strings.Join(stopWorkerNames, "|")
	if (*calls)[1].names != "fleet" || (*calls)[2].names != wantWorker {
		t.Fatalf("names %+v", *calls)
	}
	if len(rep.Warnings) != 1 || !strings.Contains(rep.Warnings[0], "w3") {
		t.Fatalf("warnings %v", rep.Warnings)
	}
}

// A process fleet did not start on the port is reported, never stopped, and the stop counts as failed.
func TestStopEndpointLeavesForeignProcessesAlone(t *testing.T) {
	s, _ := newNodeDaemon(t, &config.Config{Name: "laptop"})
	fakeStop(t, map[int]string{18090: "none"}, map[int][]string{18090: {"4242 postgres"}})
	rep := s.StopEndpoint(context.Background(), config.Endpoint{Name: "x", Node: "dino", SSH: "dino", Remote: "127.0.0.1:18090"}, StopOptions{})
	if !rep.Failed() || !strings.Contains(rep.Steps[0].Result, "left alone") {
		t.Fatalf("%+v", rep)
	}
}

func TestStopEndpointRefusals(t *testing.T) {
	s, _ := newNodeDaemon(t, &config.Config{Name: "laptop"})
	calls := fakeStop(t, map[int]string{4711: "stopped", 18090: "stopped"}, nil)
	// the daemon's own port
	rep := s.StopEndpoint(context.Background(), config.Endpoint{Name: "d", Node: "dino", URL: "http://10.0.0.1:4711/v1"}, StopOptions{})
	if rep.Refused == "" || len(*calls) != 0 {
		t.Fatalf("%+v %v", rep, *calls)
	}
	// a peer's endpoint
	rep = s.StopEndpoint(context.Background(), config.Endpoint{Name: "p", Peer: "other", URL: "http://10.0.0.1:18090/v1"}, StopOptions{})
	if rep.Refused == "" {
		t.Fatal("a peer's endpoint must be refused")
	}
	// requests running: refused without --force, allowed with it, dry run always reports
	ep := config.Endpoint{Name: "busy", Node: "dino", SSH: "dino", Remote: "127.0.0.1:18090"}
	s.lim("ep:busy").acquire(context.Background(), 0)
	if rep = s.StopEndpoint(context.Background(), ep, StopOptions{}); rep.Refused == "" || rep.InUse != 1 || len(*calls) != 0 {
		t.Fatalf("busy: %+v", rep)
	}
	if rep = s.StopEndpoint(context.Background(), ep, StopOptions{DryRun: true}); rep.Refused != "" || !(*calls)[0].dry || rep.Steps[0].Result != "would stop" && rep.Steps[0].Result != "nothing listening" {
		t.Fatalf("dry: %+v %v", rep, *calls)
	}
	if rep = s.StopEndpoint(context.Background(), ep, StopOptions{Force: true}); rep.Refused != "" || rep.Failed() || len(rep.Warnings) == 0 {
		t.Fatalf("force: %+v", rep)
	}
}

// A run stop ends the record with an event and, on request, removes the endpoint (naming the roles that still use it);
// a failed stop changes nothing.
func TestStopAndBookRunRecordAndEndpoint(t *testing.T) {
	s, _ := runServer(t)
	r, err := s.RecordRun(runPlan(), 1, "g", "")
	if err != nil {
		t.Fatal(err)
	}
	cfg := s.Config()
	cfg.Roles["coder"] = config.Role{Preferred: "g"}
	if err := s.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	ep := *s.Config().Endpoint("g")
	fakeStop(t, map[int]string{}, nil) // every call fails
	rep := s.stopAndBook(context.Background(), r.ID, ep, StopOptions{Remove: true})
	got, _ := s.runs.get(r.ID)
	if !rep.Failed() || rep.Ended || rep.Removed || !got.Active() || s.Config().Endpoint("g") == nil {
		t.Fatalf("a failed stop must change nothing: %+v", rep)
	}
	if !strings.Contains(got.Events[len(got.Events)-1].What, "incomplete") {
		t.Fatalf("events %v", got.Events)
	}
	var port int
	fmt.Sscanf(ep.URL[strings.LastIndex(ep.URL, ":")+1:], "%d", &port)
	fakeStop(t, map[int]string{port: "stopped", 0: "none"}, nil)
	ep.Group = nil
	rep = s.stopAndBook(context.Background(), r.ID, ep, StopOptions{Remove: true})
	if rep.Removed || len(rep.Warnings) == 0 || s.Config().Endpoint("g") == nil {
		t.Fatalf("a role still names the endpoint: it must stay, with a warning: %+v", rep)
	}
	rep = s.stopAndBook(context.Background(), r.ID, ep, StopOptions{Remove: true, MendRoles: true})
	got, _ = s.runs.get(r.ID)
	if rep.Failed() || !rep.Removed || got.Active() || s.Config().Endpoint("g") != nil {
		t.Fatalf("%+v %+v", rep, got)
	}
	if len(rep.Roles) != 1 || !strings.Contains(rep.Roles[0], "coder") || !strings.Contains(rep.Roles[0], "deleted") {
		t.Fatalf("what happened to the role: %v", rep.Roles)
	}
	if _, ok := s.Config().Roles["coder"]; ok {
		t.Fatal("a role with nothing left must go, or the configuration does not validate")
	}
}

func TestParseStopOutput(t *testing.T) {
	st, held := parseStopOutput("FLEET_BUSY 12 nginx\nFLEET_NONE\n")
	if st != "none" || len(held) != 1 {
		t.Fatal(st, held)
	}
	if st, _ = parseStopOutput("FLEET_STOPPED\n"); st != "stopped" {
		t.Fatal(st)
	}
	if st, _ = parseStopOutput("FLEET_NOFUSER\n"); st != "nofuser" {
		t.Fatal(st)
	}
	if !strings.Contains(stopPortScript(18090, stopMasterNames, true), "FLEET_FOUND") || strings.Contains(stopPortScript(18090, stopMasterNames, true), "kill $pids") {
		t.Fatal("a dry run must not kill")
	}
}

// `fleet run end` on a run whose master is on another machine (ep.Node, no ssh field) stops it THERE, never with a
// local fuser; --record-only (keep_processes) closes the record and touches nothing.
func TestRunEndStopsRemoteMasterOnItsNode(t *testing.T) {
	s, _ := runServer(t)
	cfg := s.Config()
	cfg.Endpoints = append(append([]config.Endpoint{}, cfg.Endpoints...), config.Endpoint{Name: "farm", Node: "dino", URL: "http://10.0.0.9:18090/v1", Model: "m",
		Group: &config.Group{Engine: "llama-ring", Workers: []string{"w1"}, Addrs: map[string]string{"w1": "10.0.0.2:50052"}, Streams: map[string]string{"w1": "127.0.0.1:51000"}}})
	if err := s.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	calls := fakeStop(t, map[int]string{18090: "stopped", 51000: "stopped", 50052: "stopped"}, nil)
	r, _ := s.RecordRun(runPlan(), 1, "farm", "")
	r2, _ := s.RecordRun(runPlan(), 1, "farm", "")

	rec := rtDo(t, s, "POST", "/fleet/runs/set", map[string]any{"id": r2.ID, "end": true, "keep_processes": true})
	time.Sleep(100 * time.Millisecond)
	if rec.Code != 200 || len(*calls) != 0 {
		t.Fatalf("--record-only touched processes: %d %+v", rec.Code, *calls)
	}

	s.killRunProcesses(s.Config().Endpoint("farm"))
	if len(*calls) != 3 {
		t.Fatalf("%+v", *calls)
	}
	for _, c := range (*calls)[:2] { // master and its stream: on the master's machine, over ssh
		if c.node != "dino" || c.local {
			t.Fatalf("the master's processes must be stopped on dino, not here: %+v", c)
		}
	}
	if c := (*calls)[2]; c.node != "w1" || c.port != 50052 {
		t.Fatalf("worker: %+v", c)
	}
	_ = r
}

// A stop that took the master down marks the deploy's record stopped: the watchdog must not bring it back.
func TestStopMarksDeployRecordStopped(t *testing.T) {
	s, _ := newNodeDaemon(t, &config.Config{Name: "laptop"})
	fakeStop(t, map[int]string{18090: "stopped", 51000: "stopped", 50052: "none"}, nil)
	deploy.OnState = nil
	rec := deploy.DeployResult{RunID: "run-77", EndpointName: "ring", Status: deploy.StatusRunning, Options: &deploy.DeployOptions{ModelPath: "m.gguf", ExitOnFail: true}}
	dir := filepath.Join(os.Getenv("XDG_CACHE_HOME"), "fleet", "runs")
	_ = os.MkdirAll(dir, 0o755)
	b, _ := json.Marshal(rec)
	_ = os.WriteFile(filepath.Join(dir, "run-77.json"), b, 0o644)
	s.StopEndpoint(context.Background(), ringEndpoint(), StopOptions{})
	if _, err := deploy.LastRunOptions("ring"); err == nil {
		t.Fatal("a deliberately stopped endpoint must have no run to redeploy from")
	}
}

// A deploy opens its run in the daemon's store, updates it, and closes it when it failed.
func TestNoteDeployOpensFindsAndEndsRun(t *testing.T) {
	s, _ := runServer(t)
	s.NoteDeploy(DeployNote{ID: "run-5", Status: "starting", Model: "m.gguf", Master: "dino", Nodes: []string{"dino", "w1"}, Endpoint: "distributed-m"})
	r, ok := s.runs.get("run-5")
	if !ok || !r.Active() || r.Endpoint != "distributed-m" || len(r.Parts) != 2 || r.Parts[0].Node != "dino" {
		t.Fatalf("%+v %v", r, ok)
	}
	if n := len(s.runs.recent(100)); n != 1 {
		t.Fatalf("one deploy is one run: %d", n)
	}
	s.NoteDeploy(DeployNote{ID: "run-5", Status: "running", SHA256: "abc", Endpoint: "distributed-m"})
	r, _ = s.runs.get("run-5")
	if !r.Active() || r.SHA256 != "abc" || len(s.runs.recent(100)) != 1 {
		t.Fatalf("%+v", r)
	}
	s.NoteDeploy(DeployNote{ID: "run-5", Status: "failed", Error: "boom"})
	r, _ = s.runs.get("run-5")
	if r.Active() {
		t.Fatalf("a failed deploy closes the run: %+v", r)
	}
}

// A redeploy that comes up on an endpoint replaces whatever ran there: the earlier records of that endpoint are closed
// (every `--force` deploy used to leave one "running" record behind: 33 piled up for one process), while a run on
// another endpoint, an earlier run that is not up yet, and a failed new deploy leave the others alone.
func TestNoteDeployClosesRunsItReplaces(t *testing.T) {
	s, _ := runServer(t)
	for _, id := range []string{"run-1", "run-2"} {
		s.NoteDeploy(DeployNote{ID: id, Status: "starting", Model: "m.gguf", Master: "victor", Endpoint: "vec-m"})
		s.NoteDeploy(DeployNote{ID: id, Status: "running", Endpoint: "vec-m"})
	}
	s.NoteDeploy(DeployNote{ID: "run-other", Status: "starting", Model: "o.gguf", Master: "dino", Endpoint: "dino-o"})
	s.NoteDeploy(DeployNote{ID: "run-other", Status: "running", Endpoint: "dino-o"})
	// the first run was replaced by the second one, the run on another endpoint was not touched
	if r, _ := s.runs.get("run-1"); r.Active() {
		t.Fatalf("run-1 was replaced by run-2 on the same endpoint and must be closed: %+v", r)
	}
	if r, _ := s.runs.get("run-2"); !r.Active() {
		t.Fatalf("the newest run stays active: %+v", r)
	}
	if r, _ := s.runs.get("run-other"); !r.Active() {
		t.Fatalf("a run on another endpoint stays active: %+v", r)
	}
	// a new deploy that is only starting, or that failed, does not close the run that is serving
	s.NoteDeploy(DeployNote{ID: "run-3", Status: "starting", Model: "m.gguf", Master: "victor", Endpoint: "vec-m"})
	if r, _ := s.runs.get("run-2"); !r.Active() {
		t.Fatalf("a deploy that has not come up must not close the serving run: %+v", r)
	}
	s.NoteDeploy(DeployNote{ID: "run-3", Status: "failed", Error: "boom", Endpoint: "vec-m"})
	if r, _ := s.runs.get("run-2"); !r.Active() {
		t.Fatalf("a failed deploy must not close the serving run: %+v", r)
	}
	if r, _ := s.runs.get("run-2"); len(r.Events) == 0 {
		t.Fatal("events expected")
	}
}

func TestStopChainRun_ReconstructsEndpointFromRecord(t *testing.T) {
	s, _ := runServer(t)
	fakeStop(t, map[int]string{52310: "stopped", 52311: "stopped"}, nil)
	deploy.OnState = nil

	// Simulate a chain deployment recorded in ~/.cache/fleet/runs
	rec := deploy.DeployResult{
		RunID:        "run-chain-99",
		Engine:       "chain",
		EndpointName: "distributed-toy",
		MasterNode:   "local",
		WorkerNodes:  []string{"worker1"},
		WorkerPorts:  map[string]int{"worker1": 52311},
		URL:          "http://127.0.0.1:52310",
		Status:       deploy.StatusRunning,
	}
	dir := filepath.Join(os.Getenv("XDG_CACHE_HOME"), "fleet", "runs")
	_ = os.MkdirAll(dir, 0o755)
	b, _ := json.Marshal(rec)
	_ = os.WriteFile(filepath.Join(dir, "run-chain-99.json"), b, 0o644)

	// Register run in server
	s.NoteDeploy(DeployNote{
		ID:       "run-chain-99",
		Status:   "running",
		Model:    "toy.gguf",
		Engine:   "chain",
		Endpoint: "distributed-toy",
		Master:   "local",
		Nodes:    []string{"local", "worker1"},
	})

	// Stop run via HTTP POST /fleet/runs/stop
	body, _ := json.Marshal(map[string]any{"id": "run-chain-99"})
	req := httptest.NewRequest("POST", "/fleet/runs/stop", bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200 OK from /fleet/runs/stop, got %d: %s", w.Code, w.Body.String())
	}
	var rep StopReport
	if err := json.NewDecoder(w.Body).Decode(&rep); err != nil {
		t.Fatalf("decode StopReport failed: %v", err)
	}
	if rep.Failed() {
		t.Fatalf("StopRun reported failure: %+v", rep)
	}
	r, ok := s.runs.get("run-chain-99")
	if !ok || r.Active() {
		t.Fatalf("expected run-chain-99 to be stopped, got active: %+v", r)
	}
}

// The tunnels of a cross-site chain (--relay) are closed right after the master, before the stages: the -L leg on the
// machine that deployed, the -R leg's listener on the stage that dials.
func TestStopEndpointClosesRelays(t *testing.T) {
	s, _ := newNodeDaemon(t, &config.Config{Name: "laptop"})
	calls := fakeStop(t, map[int]string{18090: "stopped", 53100: "stopped", 5600: "stopped"}, nil)
	ep := config.Endpoint{Name: "x", Node: "a", URL: "http://a:18090/v1", Model: "m",
		Group: &config.Group{Engine: "chain", Workers: []string{"b"}, Addrs: map[string]string{"b": "b:5600"},
			Relays: []config.Relay{{Via: "hub", From: "a", To: "b", Port: 53100}}}}
	rep := s.StopEndpoint(context.Background(), ep, StopOptions{})
	if rep.Failed() {
		t.Fatalf("%+v", rep)
	}
	var got []string
	for _, c := range *calls {
		got = append(got, fmt.Sprintf("%s:%d:%s", c.node, c.port, c.names))
	}
	want := "a:18090:" + strings.Join(stopMasterNames, "|") + ",hub:53100:ssh -R client,hub:53100:ssh,a:53100:ssh|sshd|sshd-session,b:5600:" + strings.Join(stopWorkerNames, "|")
	if strings.Join(got, ",") != want {
		t.Fatalf("calls\n got %v\nwant %s", got, want)
	}
	var relaySteps int
	for _, st := range rep.Steps {
		if st.Role == "relay" {
			relaySteps++
		}
	}
	if relaySteps != 3 {
		t.Fatalf("steps %+v", rep.Steps)
	}
}

// Two stages of a chain on one machine: Addrs can hold one of them (same key), Stages holds both, and both are
// stopped (the second used to be left running with its GPU and RAM).
func TestStopEndpointStopsEveryChainStage(t *testing.T) {
	s, _ := newNodeDaemon(t, &config.Config{Name: "laptop"})
	calls := fakeStop(t, map[int]string{18090: "stopped", 5600: "stopped", 5601: "stopped", 5602: "stopped"}, nil)
	ep := config.Endpoint{Name: "x", Node: "a", URL: "http://a:18090/v1", Model: "m",
		Group: &config.Group{Engine: "chain", Workers: []string{"b", "b", "c"}, Addrs: map[string]string{"b": "b:5600", "c": "c:5602"},
			Stages: []config.StageAddr{{Node: "b", Addr: "b:5600"}, {Node: "b", Addr: "b:5601"}, {Node: "c", Addr: "c:5602"}}}}
	rep := s.StopEndpoint(context.Background(), ep, StopOptions{})
	if rep.Failed() {
		t.Fatalf("%+v", rep)
	}
	var got []string
	for _, c := range *calls {
		got = append(got, fmt.Sprintf("%s:%d", c.node, c.port))
	}
	if g := strings.Join(got, ","); g != "a:18090,b:5600,c:5602,b:5601" {
		t.Fatalf("calls %s", g)
	}
}

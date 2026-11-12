package tui

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/tevfik/fleet-router/internal/client"
	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/hostinfo"
	"github.com/tevfik/fleet-router/internal/server"
)

// fakeDaemon runs a real server.Server handler (fresh temp configuration) behind a recorder. The machine
// benchmark endpoint is answered by the recorder itself so no real benchmark runs in tests.
type fakeDaemon struct {
	mu       sync.Mutex
	paths    []string
	benchErr bool
}

func (f *fakeDaemon) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.paths...)
}

func newNodesDaemon(t *testing.T) (*fakeDaemon, *client.Client) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("FLEET_CONFIG", filepath.Join(dir, "fleet.yaml"))
	t.Setenv("FLEET_STATS", filepath.Join(dir, "stats", "stats.json"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "xdg-config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "xdg-state"))
	t.Setenv("HOME", dir)
	s, err := server.New(filepath.Join(dir, "fleet.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	f := &fakeDaemon{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.paths = append(f.paths, r.Method+" "+r.URL.Path)
		benchErr := f.benchErr
		f.mu.Unlock()
		if r.URL.Path == "/fleet/host/bench" {
			if benchErr {
				http.Error(w, `{"error":{"message":"the machine benchmark is already running"}}`, 409)
				return
			}
			w.Write([]byte(`{"running":true}`))
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	cl := client.New(strings.TrimPrefix(srv.URL, "http://"))
	return f, cl
}

func nodesModel(cl *client.Client) model {
	m := model{cl: cl, sc: scNodes, sel: map[int]bool{}, w: 120}
	m.st.Nodes = []server.NodeStatus{
		{Node: "self", Self: true, Via: "local", Up: 1, Total: 1},
		{Node: "other", Via: "ssh other", Up: 0, Total: 2},
	}
	return m
}

func runCmd(t *testing.T, c tea.Cmd) string {
	t.Helper()
	if c == nil {
		t.Fatal("expected a command")
	}
	msg := c()
	n, ok := msg.(noteMsg)
	if !ok {
		t.Fatalf("command returned %T, want noteMsg", msg)
	}
	return string(n)
}

func TestFeature_NodesKeys_BenchOtherNodeOnlyNotes(t *testing.T) {
	f, cl := newNodesDaemon(t)
	m := nodesModel(cl)
	m.cur = 1
	nm, cmd := m.keyNodes("b")
	if cmd != nil {
		t.Fatal("r on another node must not send a request")
	}
	if note := nm.(model).note; !strings.Contains(note, "other") || !strings.Contains(note, "own fleet") {
		t.Fatalf("note = %q", note)
	}
	if len(f.seen()) != 0 {
		t.Fatalf("requests sent: %v", f.seen())
	}
}

func TestFeature_NodesKeys_BenchSelfCallsHostBench(t *testing.T) {
	f, cl := newNodesDaemon(t)
	m := nodesModel(cl)
	_, cmd := m.keyNodes("b")
	if len(f.seen()) != 0 {
		t.Fatal("the request must happen in the command, not in the key handler")
	}
	note := runCmd(t, cmd)
	if !strings.Contains(note, "benchmarking this machine") {
		t.Fatalf("note = %q", note)
	}
	if got := f.seen(); len(got) != 1 || got[0] != "POST /fleet/host/bench" {
		t.Fatalf("requests = %v", got)
	}
	f.benchErr = true
	_, cmd = m.keyNodes("b")
	if note := runCmd(t, cmd); !strings.Contains(note, "error:") || !strings.Contains(note, "already running") {
		t.Fatalf("error note = %q", note)
	}
}

func TestFeature_NodesKeys_ProbeOtherRefused(t *testing.T) {
	f, cl := newNodesDaemon(t)
	m := nodesModel(cl)
	m.cur = 1
	for _, k := range []string{"p"} {
		nm, cmd := m.keyNodes(k)
		if cmd != nil {
			t.Fatalf("%s on another node must not probe", k)
		}
		if !strings.Contains(nm.(model).note, "probed by its own fleet") {
			t.Fatalf("note = %q", nm.(model).note)
		}
	}
	if len(f.seen()) != 0 {
		t.Fatalf("requests sent: %v", f.seen())
	}
}

func TestFeature_NodesKeys_ProbeSelfReachesDaemon(t *testing.T) {
	f, cl := newNodesDaemon(t)
	m := nodesModel(cl)
	_, cmd := m.keyNodes("p")
	note := runCmd(t, cmd)
	// an empty configuration has no healthy model here: the daemon says so and the note shows it
	if !strings.Contains(note, "error:") || !strings.Contains(note, "no healthy model") {
		t.Fatalf("note = %q", note)
	}
	if got := f.seen(); len(got) != 1 || got[0] != "POST /fleet/probe" {
		t.Fatalf("requests = %v", got)
	}
}

func TestFeature_NodesKeys_ApplyProbeCallsDaemon(t *testing.T) {
	f, cl := newNodesDaemon(t)
	m := nodesModel(cl)
	m.cur = 1 // a works on any node: it only writes caps already probed
	_, cmd := m.keyNodes("a")
	note := runCmd(t, cmd)
	if !strings.Contains(note, "caps on other already match") {
		t.Fatalf("note = %q", note)
	}
	if got := f.seen(); len(got) != 1 || got[0] != "POST /fleet/probe" {
		t.Fatalf("requests = %v", got)
	}
}

func TestFeature_NodesKeys_EnterOpensDashboardFiltered(t *testing.T) {
	m := nodesModel(nil)
	m.cur = 1
	nm, cmd := m.keyNodes("enter")
	got := nm.(model)
	if cmd != nil || got.sc != scDash || got.filter != "other" || got.cur != 0 {
		t.Fatalf("sc=%v filter=%q cur=%d", got.sc, got.filter, got.cur)
	}
	// cursor past the list: nothing happens
	m.cur = 9
	if _, cmd := m.keyNodes("b"); cmd != nil {
		t.Fatal("out-of-range cursor must do nothing")
	}
}

func TestFeature_NodesFetchHosts_UnknownNodeHasError(t *testing.T) {
	_, cl := newNodesDaemon(t)
	m := nodesModel(cl)
	m.st.Nodes = m.st.Nodes[1:] // only "other": no route, so no machine is read
	got := m.fetchHosts(m.st.Nodes)
	r, ok := got["other"]
	if !ok || !strings.Contains(r.Err, "no route") {
		t.Fatalf("report = %+v", r)
	}
}

func gib(n float64) int64 { return int64(n * (1 << 30)) }

func strainedFleet() model {
	slow := &hostinfo.Bench{FastWhere: "RAM", FastBW: 10, RAMBW: 10, FastMem: gib(64), RAMTotal: gib(64), Score: 5}
	fast := &hostinfo.Bench{FastWhere: "GPU", FastBW: 270, RAMBW: 60, FastMem: gib(120), RAMTotal: gib(128), Score: 80}
	loaded, cold := true, false
	m := model{sc: scNodes, sel: map[int]bool{}, w: 140}
	m.st.Nodes = []server.NodeStatus{
		{Node: "slowbox", Via: "ssh slowbox", Up: 2, Total: 2, Strain: &server.NodeStrain{Strain: hostinfo.Strain{
			Reasons: []string{"GPU at 95°C"}, Warnings: []string{"disk / is 95% full"}}}},
		{Node: "spark", Self: true, Via: "local", Up: 1, Total: 1},
	}
	m.st.Endpoints = []server.EpStatus{
		{Endpoint: config.Endpoint{Name: "slow-qwen", Node: "slowbox", Model: "qwen3:8b"}, Known: true, OK: true,
			Info: &server.ModelInfo{Loaded: &loaded, Size: gib(5), Params: "8B"}, Strained: []string{"GPU at 95°C"}},
		{Endpoint: config.Endpoint{Name: "slow-gemma", Node: "slowbox", Model: "gemma3:4b"}, Known: true, OK: true,
			Info: &server.ModelInfo{Loaded: &cold, Size: gib(3), Params: "4B"}},
		{Endpoint: config.Endpoint{Name: "spark-llama", Node: "spark", Model: "llama3:70b"}, Known: true, OK: true,
			Info: &server.ModelInfo{Size: gib(40), Params: "70B"}},
	}
	m.nodeHost = map[string]server.HostReport{
		"slowbox": {Node: "slowbox", Bench: slow},
		"spark":   {Node: "spark", Bench: fast},
	}
	return m
}

func TestFeature_NodesView_StrainedNodeAndMoveLines(t *testing.T) {
	m := strainedFleet()
	v := m.viewNodes()
	if !strings.Contains(v, "⚠ slowbox") {
		t.Fatalf("strained node not marked:\n%s", v)
	}
	if strings.Contains(v, "⚠ spark") {
		t.Fatal("unstrained node marked")
	}
	for _, want := range []string{"STRAIN GPU at 95°C", "WARN  disk / is 95% full", "MOVE  qwen3:8b", "→", "on spark", "◆", "◇"} {
		if !strings.Contains(v, want) {
			t.Errorf("missing %q in:\n%s", want, v)
		}
	}
	// the faster machine is highlighted: no MOVE lines for models that are not its own
	m.cur = 1
	v = m.viewNodes()
	if strings.Contains(v, "MOVE") {
		t.Fatalf("MOVE shown for another node's models:\n%s", v)
	}
	if strings.Contains(v, "STRAIN") {
		t.Fatalf("strain of another node shown:\n%s", v)
	}
}

func TestFeature_NodesView_UnreadHostsAndBudget(t *testing.T) {
	m := strainedFleet()
	m.nodeHost = nil
	v := m.viewNodes()
	if !strings.Contains(v, "…") {
		t.Fatalf("unread machine should show … cells:\n%s", v)
	}
	m.h = 10 // tiny terminal: the detail is cut with a pointer to the CLI
	if v := m.viewNodes(); !strings.Contains(v, "more lines (fleet nodes slowbox)") {
		t.Fatalf("no truncation hint:\n%s", v)
	}
	if v := (model{}).viewNodes(); !strings.Contains(v, "no nodes") {
		t.Fatalf("empty view = %q", v)
	}
}

func TestFeature_Dashboard_LegendMarksAndStrainLine(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FLEET_STATS", filepath.Join(dir, "stats.json")) // qualityLine reads the eval file next to it
	m := strainedFleet()
	m.sc, m.h, m.w, m.cur = scDash, 60, 140, 0
	v := m.viewDash()
	if !strings.Contains(v, "◆ loaded ◇ cold") {
		t.Fatalf("legend missing:\n%s", v)
	}
	rows := strings.Split(v, "\n")
	var qwen, gemma, llama string
	for _, r := range rows {
		switch {
		case strings.Contains(r, "qwen3:8b") && qwen == "":
			qwen = r
		case strings.Contains(r, "gemma3:4b") && gemma == "":
			gemma = r
		case strings.Contains(r, "llama3:70b") && llama == "":
			llama = r
		}
	}
	if !strings.Contains(qwen, "◆") || !strings.Contains(gemma, "◇") || strings.ContainsAny(llama, "◆◇") {
		t.Fatalf("marks wrong:\nqwen=%q\ngemma=%q\nllama=%q", qwen, gemma, llama)
	}
	if !strings.Contains(v, "⚠ slowbox: GPU at 95°C — routing prefers other machines") {
		t.Fatalf("strain line missing:\n%s", v)
	}
	m.cur = 1 // unstrained endpoint: no strain line
	if v := m.viewDash(); strings.Contains(v, "routing prefers other machines") {
		t.Fatalf("strain line on an unstrained endpoint:\n%s", v)
	}
}

func TestFeature_NodesView_LifecycleWarnings(t *testing.T) {
	m := model{
		sc: scNodes,
		w:  120,
		h:  40,
		st: server.Status{
			Nodes: []server.NodeStatus{
				{Node: "dino", Self: true, Up: 1, Total: 1},
				{Node: "worker1", Up: 1, Total: 1},
			},
		},
		linger: "no",
		runs: []server.Run{
			{ID: "r1", Endpoint: "ep-dup", Model: "m1", Master: "dino"},
			{ID: "r2", Endpoint: "ep-dup", Model: "m1", Master: "dino"},
			{ID: "r3", Endpoint: "ep-orphan", Model: "orphan-m", Master: "dino"},
		},
	}
	v := m.viewNodes()
	if !strings.Contains(v, "⚠ dino") {
		t.Fatalf("expected ⚠ dino in view, got:\n%s", v)
	}
	if !strings.Contains(v, "ep-dup") || !strings.Contains(v, "clean up stale records") {
		t.Fatalf("expected duplicate endpoint warning in view, got:\n%s", v)
	}
	if !strings.Contains(v, "orphan-m") || !strings.Contains(v, "has no autostart profile") {
		t.Fatalf("expected unmanaged run warning in view, got:\n%s", v)
	}
	if !strings.Contains(v, "systemd linger is off (Linger=no)") {
		t.Fatalf("expected linger warning in view, got:\n%s", v)
	}
}

package tui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/client"
	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/server"
)

func TestFeature_Nodes_DeclareAndForgetSpec(t *testing.T) {
	_, cl := newNodesDaemon(t) // a real daemon on a temporary configuration
	m := nodesModel(cl)
	if !strings.Contains(buttonLabels(m), "Declare spec…") || !strings.Contains(buttonLabels(m), "Undo config") || !strings.Contains(buttonLabels(m), "Config history") {
		t.Fatal(buttonLabels(m))
	}
	if !strings.Contains(m.viewNodes(), "spec: not declared") {
		t.Fatalf("a machine without a spec says so:\n%s", m.viewNodes())
	}
	m, _ = press(t, m, "e")
	if m.editing != "nodespec" || m.editFor != "self" {
		t.Fatalf("%q %q", m.editing, m.editFor)
	}
	m.editBuf = "addr=10.0.0.5 mem_gib=96 weight_gbps=250 expert_gbps=60"
	nm, cmd := m.keyStr("enter", "")
	m = play(t, nm.(model), cmd)
	cfg, err := cl.Config()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Nodes["self"]; got.Addr != "10.0.0.5" || got.MemGiB != 96 || got.WeightGBps != 250 || got.ExpertGBps != 60 {
		t.Fatalf("%+v", got)
	}
	// the editor starts from what is declared; only the keys typed change
	m.cfg.Nodes = cfg.Nodes
	if !strings.Contains(m.viewNodes(), "spec: addr=10.0.0.5 mem_gib=96 weight_gbps=250 expert_gbps=60") {
		t.Fatalf("declared spec not shown:\n%s", m.viewNodes())
	}
	m, _ = press(t, m, "e")
	if !strings.HasPrefix(m.editBuf, "addr=10.0.0.5 mem_gib=96") {
		t.Fatalf("editor starts blank: %q", m.editBuf)
	}
	m.editBuf = "mem_gib=64"
	nm, cmd = m.keyStr("enter", "")
	m = play(t, nm.(model), cmd)
	if cfg, _ = cl.Config(); cfg.Nodes["self"].MemGiB != 64 || cfg.Nodes["self"].Addr != "10.0.0.5" {
		t.Fatalf("%+v", cfg.Nodes)
	}
	m, _ = press(t, m, "e")
	m.editBuf = "mem_gib=lots"
	nm, cmd = m.keyStr("enter", "")
	if m = play(t, nm.(model), cmd); !strings.Contains(m.note, "mem_gib") {
		t.Fatalf("a bad number is refused: %q", m.note)
	}
	// forget: only a declared machine can be forgotten
	m.cfg.Nodes = cfg.Nodes
	_, note := press(t, m, "f")
	if !strings.Contains(note, "removed") {
		t.Fatalf("%q", note)
	}
	if cfg, _ = cl.Config(); len(cfg.Nodes) != 0 {
		t.Fatalf("%+v", cfg.Nodes)
	}
	m.cur = 1
	if m, _ = press(t, m, "f"); !strings.Contains(m.note, "no declared spec") {
		t.Fatalf("%q", m.note)
	}
}

func TestFeature_ConfigHistoryAndUndoButtons(t *testing.T) {
	_, cl := newNodesDaemon(t)
	if err := cl.SetNodeSpec("a", config.NodeSpec{MemGiB: 1}, false); err != nil {
		t.Fatal(err)
	}
	if err := cl.SetNodeSpec("a", config.NodeSpec{MemGiB: 2}, false); err != nil {
		t.Fatal(err)
	}
	m := nodesModel(cl)
	nm, cmd := m.keyStr("H", "")
	got := play(t, nm.(model), cmd)
	if !strings.Contains(got.note, "earlier configuration") || got.outText == "" {
		t.Fatalf("history: %q %q", got.note, got.outText)
	}
	_, note := press(t, m, "u")
	if !strings.Contains(note, "restored") {
		t.Fatal(note)
	}
	if cfg, _ := cl.Config(); cfg.Nodes["a"].MemGiB != 1 {
		t.Fatalf("undo: %+v", cfg.Nodes)
	}
	// the Roles screen has the history button too
	r := editModel(cl)
	if !strings.Contains(buttonLabels(r), "Config history") {
		t.Fatal(buttonLabels(r))
	}
	if _, cmd := r.keyStr("H", ""); cmd == nil {
		t.Fatal("H on the roles screen")
	}
}

func TestFeature_Runs_PruneAndWatchdogButtons(t *testing.T) {
	var deletes []map[string]any
	ended := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if r.URL.Path == "/fleet/runs/delete" {
			deletes = append(deletes, body)
			json.NewEncoder(w).Encode(map[string]any{"removed": []server.Run{{ID: "r0b", Model: "old", Ended: ended}, {ID: "r0c", Model: "older", Ended: ended}}})
			return
		}
		w.Write([]byte("{}"))
	}))
	defer srv.Close()
	cl := client.New(strings.TrimPrefix(srv.URL, "http://"))
	oldW := watchdogOnce
	watchdogOnce = func() (string, error) { return "checked once", nil }
	defer func() { watchdogOnce = oldW }()

	m := runsModel(t)
	m.cl = cl
	labels := buttonLabels(m)
	for _, want := range []string{"End run", "Bind to endpoint…", "Delete record", "Prune ended…", "Watchdog check"} {
		if !strings.Contains(labels, want) {
			t.Errorf("button %q missing: %s", want, labels)
		}
	}
	// prune: a dry run lists the records, nothing goes until y
	nm, cmd := m.keyStr("P", "")
	m = play(t, nm.(model), cmd)
	if !m.confirm || !strings.Contains(m.outText, "r0b") || !strings.Contains(m.note, "would remove 2") {
		t.Fatalf("confirm %v out %q note %q", m.confirm, m.outText, m.note)
	}
	if len(deletes) != 1 || deletes[0]["dry_run"] != true || deletes[0]["prune"] != true {
		t.Fatalf("the first request must be a dry run: %v", deletes)
	}
	if !strings.Contains(buttonLabels(m), "Yes, delete them") {
		t.Fatal(buttonLabels(m))
	}
	n, _ := press(t, m, "n")
	if n.confirm || len(deletes) != 1 {
		t.Fatal("n cancels without deleting")
	}
	m, note := press(t, m, "y")
	if len(deletes) != 2 || deletes[1]["dry_run"] != false || m.confirm || !strings.Contains(note, "removed 2") {
		t.Fatalf("%v %q", deletes, note)
	}
	_, note = press(t, m, "w")
	if !strings.Contains(note, "checked once") {
		t.Fatal(note)
	}
}

func TestFeature_Hub_KeyStreamPortsAndStreams(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FLEET_CONFIG", filepath.Join(dir, "fleet.yaml"))
	t.Setenv("FLEET_API_KEY", "")
	t.Setenv("HOME", dir) // config.Secret also reads ~/.config/opencode/.env
	rec, cl := newRecorder(t)
	m := model{cl: cl, sc: scHub, w: 140, hubURL: "https://h", cfg: config.Config{HubAPIKey: "sk-abcdefghijklmnop"}}
	if m.listLen() != 15 {
		t.Fatalf("listLen %d", m.listLen())
	}
	v := m.viewHub()
	for _, want := range []string{"Show API Key", "Remove API Key", "Connect with Stream Ports", "Open Stream", "Stop Streams"} {
		if !strings.Contains(v, want) {
			t.Errorf("hub action %q missing", want)
		}
	}
	// the actions are list rows (enter), not hint-line letters
	for _, gone := range []string{"v show key", "x remove key", "s stream ports", "o open stream", "z stop streams"} {
		if strings.Contains(m.help(), gone) {
			t.Errorf("hint line still lists %q", gone)
		}
	}
	row := func(i int) model { r := m; r.cur = i; return r }
	// key show: masked, never in full
	nm, _ := row(7).keyHub("enter")
	if note := nm.(model).note; !strings.Contains(note, "sk-abc...mnop") || strings.Contains(note, "abcdefghijklmnop") {
		t.Fatalf("%q", note)
	}
	// reveal: the key in full (the menu's own explicit action); generate: a new random key, saved through the daemon
	nm, _ = row(9).keyHub("enter")
	if note := nm.(model).note; !strings.Contains(note, "sk-abcdefghijklmnop") {
		t.Fatalf("reveal must show the key in full: %q", note)
	}
	nm, gcmd := row(8).keyHub("enter")
	gm := nm.(model)
	if !strings.HasPrefix(gm.hubAPIKey, "flt_hub_") || gm.cfg.HubAPIKey != gm.hubAPIKey || !strings.Contains(gm.note, gm.hubAPIKey) {
		t.Fatalf("generate: %q %q", gm.hubAPIKey, gm.note)
	}
	if gcmd == nil {
		t.Fatal("generate must save the key through the daemon")
	}
	gcmd()
	if rec.last["/fleet/hub/settings"]["hub_api_key"] != gm.hubAPIKey {
		t.Fatalf("the generated key was not sent to the daemon: %v", rec.last["/fleet/hub/settings"])
	}
	// stream ports: replace, then add
	nm, _ = row(11).keyHub("enter")
	m2 := nm.(model)
	if m2.editing != "hubstreamports" {
		t.Fatal(m2.editing)
	}
	m2.editBuf = "8080, 8081"
	nm, cmd := m2.keyStr("enter", "")
	if cmd == nil {
		t.Fatal(nm.(model).note)
	}
	cmd()
	if got := rec.last["/fleet/tunnel/join"]; got["hub_url"] != "https://h" || len(got["stream_ports"].([]any)) != 2 || got["add_stream_ports"] != nil {
		t.Fatalf("%v", got)
	}
	m2.editing, m2.editBuf = "hubstreamports", "+9000"
	nm, cmd = m2.keyStr("enter", "")
	cmd()
	if got := rec.last["/fleet/tunnel/join"]; got["add_stream_ports"].([]any)[0].(float64) != 9000 || got["stream_ports"] != nil {
		t.Fatalf("%v", got)
	}
	m2.editing, m2.editBuf = "hubstreamports", "70000"
	if nm, _ = m2.keyStr("enter", ""); !strings.Contains(nm.(model).note, "bad port") {
		t.Fatal(nm.(model).note)
	}
	// the enter key of the hub editors is the editor's own (it used to add an ssh host)
	m3 := m
	m3.editing, m3.editBuf = "huburl", "https://other"
	nm, _ = m3.keyStr("enter", "")
	if got := nm.(model); got.hubURL != "https://other" || len(got.cfg.Discover.SSH) != 0 {
		t.Fatalf("hub url %q ssh %v", got.hubURL, got.cfg.Discover.SSH)
	}

	// open a stream (the listener itself is replaced): needs the key, lists it, z stops it
	var opened []string
	oldS := startStream
	startStream = func(hub, node string, port int, listen, key string, insecure bool) (string, error) {
		opened = append(opened, hub+"|"+node+"|"+listen+"|"+key)
		streamMu.Lock()
		streams = append(streams, streamInfo{Listen: listen, Node: node, Port: port, stop: func() {}})
		streamMu.Unlock()
		return listen, nil
	}
	defer func() { startStream = oldS; stopStreams() }()
	nm, _ = row(12).keyHub("enter")
	m4 := nm.(model)
	m4.editBuf = "papa 8080"
	nm, cmd = m4.keyStr("enter", "")
	if got := cmd().(noteMsg); !strings.Contains(string(got), "127.0.0.1:8080") {
		t.Fatal(got)
	}
	if len(opened) != 1 || opened[0] != "https://h|papa|127.0.0.1:8080|sk-abcdefghijklmnop" {
		t.Fatalf("%v", opened)
	}
	if v := m.viewHub(); !strings.Contains(v, "OPEN STREAMS") || !strings.Contains(v, "papa:8080") || !strings.Contains(v, "(1 open)") {
		t.Fatalf("open streams not listed:\n%s", v)
	}
	nm, _ = row(13).keyHub("enter")
	if !strings.Contains(nm.(model).note, "1 stream") || len(activeStreams()) != 0 {
		t.Fatalf("%q", nm.(model).note)
	}
	// no key: refused before anything opens
	m5 := model{cl: cl, sc: scHub, hubURL: "https://h", editing: "hubstream", editBuf: "p1 80"}
	if nm, _ = m5.keyStr("enter", ""); !strings.Contains(nm.(model).note, "API key") || len(opened) != 1 {
		t.Fatalf("%q", nm.(model).note)
	}
	// key rm: gone from the config and from .env
	if err := config.SetSecret("FLEET_API_KEY", "sk-abcdefghijklmnop"); err != nil {
		t.Fatal(err)
	}
	nm, cmd = row(10).keyHub("enter")
	if got := nm.(model); got.cfg.HubAPIKey != "" || got.hubAPIKey != "" {
		t.Fatal("key kept in the model")
	}
	cmd()
	if rec.last["/fleet/hub/settings"]["hub_api_key"] != "" {
		t.Fatalf("%v", rec.last["/fleet/hub/settings"])
	}
	if b, _ := os.ReadFile(filepath.Join(dir, ".env")); strings.Contains(string(b), "FLEET_API_KEY") {
		t.Fatalf(".env still has it: %s", b)
	}
}

func TestHomeHasProfilesAfterPlan(t *testing.T) {
	var titles []string
	for _, it := range home {
		titles = append(titles, it.title)
	}
	j := strings.Join(titles, ",")
	if !strings.Contains(j, "Plan,Profiles") {
		t.Fatal(j)
	}
}

// The hub screen is long: the result of an action must be visible under the action list (not only at the very bottom of
// the screen, where a short terminal cuts it off), and the selected action says what it does.
func TestFeature_Hub_NoteAndDescriptionUnderTheList(t *testing.T) {
	m := model{sc: scHub, cur: 7, note: "hub API key: sk-a…xyz (length 20)"}
	v := m.viewHub()
	if !strings.Contains(v, "[ Remove API Key ]") {
		t.Fatalf("action list missing:\n%s", v)
	}
	if fv := m.View().Content; !strings.Contains(fv, "hub API key: sk-a") {
		t.Fatalf("the note must be in the full view:\n%s", fv)
	}
	if !strings.Contains(v, "Masked hub API key") {
		t.Fatalf("the selected action's description is missing:\n%s", v)
	}
	if strings.Contains(v, "Hub API key for /v1/* requests") {
		t.Fatalf("only the selected action's description is shown:\n%s", v)
	}
}

// The daemon configuration (token, API key) is refreshed on the hub screen, not only on the nodes screen.
func TestHubScreenRefreshesConfig(t *testing.T) {
	src, err := os.ReadFile("tui.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "m.sc == scNodes || m.sc == scHub") {
		t.Fatal("the status poll must reload the configuration on the hub screen")
	}
}

// The Nodes screen shows each machine's fleet version and warns when they differ (victor -32 behind hub -50).
func TestFeature_Nodes_ShowsVersionDrift(t *testing.T) {
	_, cl := newNodesDaemon(t)
	m := nodesModel(cl)
	m.st.Nodes = []server.NodeStatus{{Node: "victor"}, {Node: "hub"}}
	m.nodeHost = map[string]server.HostReport{
		"victor": {Version: "v0.10.1-32-gaaaaaaa", At: time.Now()},
		"hub":    {Version: "v0.10.1-50-gbbbbbbb", At: time.Now()},
	}
	v := m.viewNodes()
	for _, w := range []string{"FLEET", "v0.10.1-32-gaaaaaaa", "version drift", "victor v0.10.1-32-gaaaaaaa", "newest hub v0.10.1-50-gbbbbbbb"} {
		if !strings.Contains(v, w) {
			t.Errorf("missing %q in:\n%s", w, v)
		}
	}
}

// x on a node asks first; y takes it out of the scan list and drops its spec; this machine cannot be removed.
func TestFeature_Nodes_RemoveNode(t *testing.T) {
	_, cl := newNodesDaemon(t)
	m := nodesModel(cl)
	if m, _ = press(t, m, "x"); m.confirm || !strings.Contains(m.note, "this machine") {
		t.Fatalf("self must stay: %q", m.note)
	}
	if got := nodeSSHEntries([]string{"dino", "me@dino", "dino.local", "dgx2"}, "dino"); len(got) != 3 {
		t.Fatalf("%v", got)
	}
}

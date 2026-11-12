package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/tevfik/fleet-router/internal/discover"
	"github.com/tevfik/fleet-router/internal/feature"
	"github.com/tevfik/fleet-router/internal/plan"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/server"
	"github.com/tevfik/fleet-router/internal/store"
)

// ---- fake OpenAI upstream ----

type upstream struct {
	*httptest.Server
	mu    sync.Mutex
	temps []any // the "temperature" field of each chat request (nil when absent)
	chats int
}

func newUpstream(t *testing.T) *upstream {
	u := &upstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/models"):
			fmt.Fprint(w, `{"object":"list","data":[{"id":"tiny"},{"id":"thinker"},{"id":"gemma"}]}`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/chat/completions"):
			var in map[string]any
			json.NewDecoder(r.Body).Decode(&in)
			u.mu.Lock()
			u.chats++
			u.temps = append(u.temps, in["temperature"])
			u.mu.Unlock()
			if st, _ := in["stream"].(bool); st {
				w.Header().Set("Content-Type", "text/event-stream")
				send := func(s string) { fmt.Fprint(w, "data: "+s+"\n\n"); w.(http.Flusher).Flush() }
				send(`{"choices":[{"delta":{"content":"po"}}]}`)
				send(`{"choices":[{"delta":{"content":"ng"},"finish_reason":"stop"}]}`)
				send(`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2}}`)
				send(`[DONE]`)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if in["model"] == "thinker" {
				fmt.Fprint(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"","reasoning_content":"hmm"},"finish_reason":"length"}],"usage":{"prompt_tokens":5,"completion_tokens":9}}`)
				return
			}
			fmt.Fprint(w, `{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *upstream) port(t *testing.T) int {
	p, err := strconv.Atoi(u.URL[strings.LastIndex(u.URL, ":")+1:])
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// ---- in-process daemon ----

// isolate points every fleet path at a temp dir so the user's real configuration and state are never touched.
func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("FLEET_CONFIG", filepath.Join(dir, "unused-config.yaml"))
	t.Setenv("FLEET_STATS", filepath.Join(dir, "state", "stats.json"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "xdg-config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "xdg-state"))
	t.Setenv("HOME", dir)
	return dir
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := l.Addr().String()
	l.Close()
	return a
}

type daemon struct {
	s    *server.Server
	addr string
	cl   *Client
	path string
}

// startDaemon runs a full daemon (server.Run: health, peers, strain; mdns and auto-update off) on a free
// loopback port over a config saved in a temp dir.
func startDaemon(t *testing.T, cfg *config.Config) *daemon {
	t.Helper()
	off := false
	cfg.Listen, cfg.MDNS, cfg.AutoUpdate = freeAddr(t), &off, &off
	if cfg.Roles == nil {
		cfg.Roles = map[string]config.Role{}
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	s, err := server.New(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	d := &daemon{s: s, addr: cfg.Listen, cl: New(cfg.Listen), path: path}
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(6 * time.Second):
			t.Log("daemon did not stop in time")
		}
	})
	for i := 0; i < 100 && !d.cl.Alive(); i++ {
		select {
		case err := <-done:
			t.Fatalf("daemon exited: %v", err)
		case <-time.After(30 * time.Millisecond):
		}
	}
	if !d.cl.Alive() {
		t.Fatal("daemon did not come up")
	}
	return d
}

func (d *daemon) waitHealthy(t *testing.T, names ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, err := d.cl.Status()
		if err == nil {
			ok := 0
			for _, e := range st.Endpoints {
				for _, n := range names {
					if e.Name == n && e.OK {
						ok++
					}
				}
			}
			if ok == len(names) {
				return
			}
		}
		time.Sleep(30 * time.Millisecond)
	}
	st, _ := d.cl.Status()
	t.Fatalf("endpoints %v did not become healthy: %+v", names, st.Endpoints)
}

func poll(t *testing.T, what string, d time.Duration, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func wantErr(t *testing.T, err error, sub string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), sub) {
		t.Fatalf("want an error containing %q, got %v", sub, err)
	}
}

func localConfig(be *upstream) *config.Config {
	return &config.Config{
		Name: "alpha",
		Endpoints: []config.Endpoint{
			{Name: "loc-tiny", Node: "box", URL: be.URL + "/v1", Model: "tiny", Ctx: 4096},
			{Name: "loc-thinker", Node: "box", URL: be.URL + "/v1", Model: "thinker"},
		},
		Roles: map[string]config.Role{"scout": {Preferred: "loc-tiny", Fallback: []string{"loc-thinker"}}},
	}
}

// ---- tests ----

func TestAliveAndUnreachable(t *testing.T) {
	isolate(t)
	cl := New("127.0.0.1:1")
	if cl.Base != "http://127.0.0.1:1" {
		t.Fatalf("Base = %q", cl.Base)
	}
	if cl.Alive() {
		t.Fatal("nothing listens on port 1")
	}
	_, err := cl.Status()
	wantErr(t, err, "cannot reach the fleet daemon")
	// a non-JSON error body is reported with its status code
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(500)
			return
		}
		http.Error(w, "boom", 502)
	}))
	defer h.Close()
	c2 := New(strings.TrimPrefix(h.URL, "http://"))
	if c2.Alive() {
		t.Fatal("healthz 500 must not count as alive")
	}
	_, err = c2.Config()
	wantErr(t, err, "http 502: boom")
	_, _, err = c2.Chat("x", "", "hi", 0, time.Second)
	wantErr(t, err, "http 502")
}

func TestReadEndpoints(t *testing.T) {
	isolate(t)
	be := newUpstream(t)
	d := startDaemon(t, localConfig(be))
	d.waitHealthy(t, "loc-tiny", "loc-thinker")
	cl := d.cl

	st, err := cl.Status()
	if err != nil {
		t.Fatal(err)
	}
	if st.Version != server.Version || st.Listen != d.addr || len(st.Endpoints) != 2 {
		t.Fatalf("status: %+v", st)
	}
	if r := st.Roles["scout"]; r.Active != "loc-tiny" || r.Preferred != "loc-tiny" || len(r.Fallback) != 1 {
		t.Fatalf("role scout: %+v", r)
	}
	foundBox := false
	for _, n := range st.Nodes {
		if n.Node == "box" && n.Total == 2 && n.Up == 2 {
			foundBox = true
		}
	}
	if !foundBox {
		t.Fatalf("nodes: %+v", st.Nodes)
	}

	cfg, err := cl.Config()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "alpha" || cfg.ID == "" || len(cfg.Endpoints) != 2 || (&cfg).Endpoint("loc-tiny").Ctx != 4096 {
		t.Fatalf("config: %+v", cfg)
	}

	oc, err := cl.OpenCode()
	if err != nil {
		t.Fatal(err)
	}
	fl, _ := oc["fleet"].(map[string]any)
	models, _ := fl["models"].(map[string]any)
	settings, _ := fl["settings"].(map[string]any)
	if models["scout"] == nil || models["loc-tiny"] == nil || settings["baseURL"] != "http://"+d.addr+"/v1" {
		t.Fatalf("opencode: %v", oc)
	}

	h, err := cl.Host("")
	if err != nil {
		t.Fatal(err)
	}
	if h.Source != "this machine" || h.Err != "" {
		t.Fatalf("host: %+v", h)
	}
	if h2, err := cl.Host("box"); err != nil || h2.Source != "this machine" {
		t.Fatalf("host box (local URL ⇒ this machine): %+v %v", h2, err)
	}
	if h3, err := cl.Host("ghost"); err != nil || !strings.Contains(h3.Err, "no route to a fleet on ghost") {
		t.Fatalf("unknown node: %+v %v", h3, err)
	}

	ps, err := cl.Peers()
	if err != nil || len(ps) != 0 {
		t.Fatalf("peers: %v %v", ps, err)
	}
	lan, err := cl.LAN()
	if err != nil || len(lan) != 0 {
		t.Fatalf("lan with mdns off: %v %v", lan, err)
	}
}

func TestChatThroughRouter(t *testing.T) {
	isolate(t)
	be := newUpstream(t)
	d := startDaemon(t, localConfig(be))
	d.waitHealthy(t, "loc-tiny", "loc-thinker")
	cl := d.cl

	ans, ep, err := cl.Chat("scout", "be brief", "ping?", 16, 10*time.Second)
	if err != nil || ans != "pong" || ep != "loc-tiny" {
		t.Fatalf("Chat: %q %q %v", ans, ep, err)
	}
	ans, ep, err = cl.ChatCtx(context.Background(), "loc-tiny", "", "ping?", 0)
	if err != nil || ans != "pong" || ep != "loc-tiny" {
		t.Fatalf("ChatCtx: %q %q %v", ans, ep, err)
	}
	zero := 0.0
	if ans, _, err = cl.ChatTemp(context.Background(), "scout", "", "ping?", 0, &zero); err != nil || ans != "pong" {
		t.Fatalf("ChatTemp: %q %v", ans, err)
	}
	be.mu.Lock()
	last := be.temps[len(be.temps)-1]
	be.mu.Unlock()
	if f, ok := last.(float64); !ok || f != 0 {
		t.Fatalf("temperature 0 must reach the upstream, got %v", last)
	}
	// a thinking model that spent everything on reasoning: the error says so
	_, ep, err = cl.Chat("loc-thinker", "", "ping?", 0, 10*time.Second)
	wantErr(t, err, "the model used all its tokens on reasoning")
	if ep != "loc-thinker" {
		t.Fatalf("endpoint of the reasoning-only answer: %q", ep)
	}
	// unknown model
	_, _, err = cl.Chat("no-such-role", "", "x", 0, 10*time.Second)
	wantErr(t, err, "http 404")
	// cancelled context
	cx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err = cl.ChatCtx(cx, "scout", "", "x", 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled ChatCtx: %v", err)
	}

	// TestRole: through the router + each member directly
	rs := cl.TestRole("scout", []string{"loc-tiny", "loc-thinker"})
	if len(rs) != 3 || rs[0].Target != "scout" || rs[0].Endpoint != "loc-tiny" || rs[0].Answer != "pong" || rs[0].Err != nil {
		t.Fatalf("TestRole router: %+v", rs)
	}
	if rs[1].Err != nil || rs[2].Err == nil {
		t.Fatalf("TestRole members: %+v", rs)
	}
	lines := FormatTest(rs)
	if len(lines) != 3 || !strings.Contains(lines[0], "✓ via router  (scout → loc-tiny)") || !strings.Contains(lines[0], `"pong"`) ||
		!strings.HasPrefix(lines[2], "  ✗ loc-thinker") {
		t.Fatalf("FormatTest:\n%s", strings.Join(lines, "\n"))
	}
	long := FormatTest([]TestResult{{Target: "r", Err: errors.New(strings.Repeat("x", 200))}})
	if !strings.HasSuffix(long[0], "…") {
		t.Fatalf("long errors are cut: %q", long[0])
	}

	// the log and the usage report saw the requests
	var es []server.LogEntry
	poll(t, "log entries", 3*time.Second, func() bool {
		es, err = cl.Log(50)
		return err == nil && len(es) >= 5
	})
	sawRole := false
	for _, e := range es {
		if e.Role == "scout" && e.Endpoint == "loc-tiny" && e.Status == 200 {
			sawRole = true
		}
	}
	if !sawRole {
		t.Fatalf("log: %+v", es)
	}
	if es2, _ := cl.Log(1); len(es2) != 1 {
		t.Fatalf("Log(1) returned %d entries", len(es2))
	}
	var rows []server.UsageRow
	poll(t, "usage rows", 3*time.Second, func() bool {
		rows, err = cl.Usage("role", "", "24h")
		for _, r := range rows {
			if r.Key == "scout" && r.Requests >= 3 {
				return true
			}
		}
		return false
	})
	split, err := cl.Usage("role", "model", "7d")
	if err != nil {
		t.Fatal(err)
	}
	gotSub := false
	for _, r := range split {
		if r.Key == "scout" && r.Sub == "box/tiny" {
			gotSub = true
		}
	}
	if !gotSub {
		t.Fatalf("usage split by model: %+v", split)
	}
}

func TestConfigEdits(t *testing.T) {
	isolate(t)
	be := newUpstream(t)
	d := startDaemon(t, localConfig(be))
	cl := d.cl
	cfgNow := func() *config.Config {
		t.Helper()
		c, err := cl.Config()
		if err != nil {
			t.Fatal(err)
		}
		return &c
	}

	if err := cl.SetRole("coder", "loc-tiny", []string{"loc-thinker"}); err != nil {
		t.Fatal(err)
	}
	if r := cfgNow().Roles["coder"]; r.Preferred != "loc-tiny" || len(r.Fallback) != 1 || r.Fallback[0] != "loc-thinker" {
		t.Fatalf("SetRole: %+v", r)
	}
	if err := cl.SetRole("coder", "loc-tiny", nil); err != nil { // nil fallback clears
		t.Fatal(err)
	}
	if r := cfgNow().Roles["coder"]; len(r.Fallback) != 0 {
		t.Fatalf("SetRole nil fallback must clear: %+v", r)
	}
	wantErr(t, cl.SetRole("bad", "no-such-endpoint", nil), `unknown endpoint "no-such-endpoint"`)

	if err := cl.UpdateRole("coder", map[string]any{"priority": 5, "max_parallel": 2, "overflow": true, "strategy": "least-busy", "use": "code"}); err != nil {
		t.Fatal(err)
	}
	if r := cfgNow().Roles["coder"]; r.Priority != 5 || r.MaxParallel != 2 || !r.Overflow || r.Strategy != "least-busy" || r.Use != "code" || r.Preferred != "loc-tiny" {
		t.Fatalf("UpdateRole: %+v", r)
	}
	wantErr(t, cl.UpdateRole("coder", map[string]any{"strategy": "random"}), `unknown strategy "random"`)
	wantErr(t, cl.UpdateRole("", map[string]any{}), "role is required")

	if err := cl.SetMatch("fast", []string{"tag:fast"}); err != nil { // creates the role
		t.Fatal(err)
	}
	if r := cfgNow().Roles["fast"]; len(r.Match) != 1 || r.Match[0] != "tag:fast" {
		t.Fatalf("SetMatch: %+v", r)
	}
	wantErr(t, cl.SetMatch("fast", []string{"colour:red"}), "")
	wantErr(t, cl.SetMatch("fast", nil), "needs a preferred endpoint or a match selector") // clearing the only selector

	if err := cl.SetTags("loc-tiny", []string{"fast", "gpu"}); err != nil {
		t.Fatal(err)
	}
	if e := cfgNow().Endpoint("loc-tiny"); len(e.Tags) != 2 {
		t.Fatalf("SetTags: %+v", e)
	}
	if st, _ := cl.Status(); st.Roles["fast"].Active != "loc-tiny" && st.Roles["fast"].Active != "" {
		t.Fatalf("role fast should match loc-tiny: %+v", st.Roles["fast"])
	}
	if err := cl.SetTags("loc-tiny", nil); err != nil {
		t.Fatal(err)
	}
	if e := cfgNow().Endpoint("loc-tiny"); len(e.Tags) != 0 {
		t.Fatalf("SetTags nil must clear: %+v", e.Tags)
	}
	if err := cl.SetCaps("loc-thinker", []string{"chat", "thinking"}); err != nil {
		t.Fatal(err)
	}
	if e := cfgNow().Endpoint("loc-thinker"); strings.Join(e.Caps, ",") != "chat,thinking" {
		t.Fatalf("SetCaps: %+v", e.Caps)
	}
	if err := cl.SetCaps("loc-thinker", nil); err != nil {
		t.Fatal(err)
	}
	if e := cfgNow().Endpoint("loc-thinker"); len(e.Caps) != 0 {
		t.Fatalf("SetCaps nil (automatic): %+v", e.Caps)
	}
	if err := cl.SetSlots("loc-tiny", 3); err != nil {
		t.Fatal(err)
	}
	if e := cfgNow().Endpoint("loc-tiny"); e.Slots != 3 {
		t.Fatalf("SetSlots: %d", e.Slots)
	}
	if err := cl.SetSlots("loc-tiny", -4); err != nil {
		t.Fatal(err)
	}
	if e := cfgNow().Endpoint("loc-tiny"); e.Slots != 0 {
		t.Fatalf("negative slots clamp to 0: %d", e.Slots)
	}
	wantErr(t, cl.SetSlots("ghost", 1), "no such endpoint: ghost")

	if err := cl.DeleteRole("coder"); err != nil {
		t.Fatal(err)
	}
	if _, ok := cfgNow().Roles["coder"]; ok {
		t.Fatal("DeleteRole left the role")
	}

	extra := config.Endpoint{Name: "extra", Node: "box", URL: be.URL + "/v1", Model: "tiny2"}
	if err := cl.Endpoints([]config.Endpoint{extra}, nil); err != nil {
		t.Fatal(err)
	}
	if cfgNow().Endpoint("extra") == nil {
		t.Fatal("Endpoints add")
	}
	wantErr(t, cl.Endpoints([]config.Endpoint{{Name: "nourl", Model: "m"}}, nil), "url or (ssh + remote) is required")
	wantErr(t, cl.Endpoints([]config.Endpoint{extra}, nil), "duplicate endpoint name: extra")
	if err := cl.Endpoints(nil, []string{"extra"}); err != nil {
		t.Fatal(err)
	}
	if cfgNow().Endpoint("extra") != nil {
		t.Fatal("Endpoints remove")
	}

	// Undo brings back the configuration before the last change (extra present again)
	left, err := cl.Undo()
	if err != nil {
		t.Fatal(err)
	}
	if cfgNow().Endpoint("extra") == nil {
		t.Fatal("Undo did not restore the removed endpoint")
	}
	if left2, err := cl.Undo(); err != nil || left2 != left-1 {
		t.Fatalf("second undo: left %d → %d (%v)", left, left2, err)
	}
	if cfgNow().Endpoint("extra") != nil {
		t.Fatal("the second Undo should go back to before extra was added")
	}

	if err := cl.SetDiscover("8080,9000", []string{}, []string{"box:1919"}); err != nil {
		t.Fatal(err)
	}
	if dc := cfgNow().Discover; dc.Ports != "8080,9000" || len(dc.Ignore) != 1 || len(dc.SSH) != 0 {
		t.Fatalf("SetDiscover: %+v", dc)
	}
}

func TestDiscoverLocalPort(t *testing.T) {
	isolate(t)
	be := newUpstream(t)
	d := startDaemon(t, &config.Config{Name: "alpha"})
	port := be.port(t)
	ignored := newUpstream(t)
	if err := d.cl.SetDiscover("", nil, []string{fmt.Sprintf("alpha:%d", ignored.port(t)), "alpha:4711" /* never pick up a real fleet on the default port */}); err != nil {
		t.Fatal(err)
	}
	r, err := d.cl.Discover(fmt.Sprintf("%d,%d", port, ignored.port(t)), nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	var f *struct{}
	for _, x := range r.Found {
		if x.Port == port && x.SSH == "" && x.Node == "alpha" && strings.Join(x.Models, ",") == "tiny,thinker,gemma" {
			f = &struct{}{}
		}
		if x.Port == ignored.port(t) {
			t.Fatalf("an ignored port was kept: %+v", x)
		}
	}
	if f == nil {
		t.Fatalf("discover did not find the fake server on %d: %+v", port, r)
	}
	if !strings.Contains(strings.Join(r.Notes, "\n"), "on the ignore list") {
		t.Fatalf("notes: %v", r.Notes)
	}
	if len(r.Plan.Add) != 3 {
		t.Fatalf("plan: %+v", r.Plan)
	}
	if c, _ := d.cl.Config(); len(c.Endpoints) != 0 {
		t.Fatal("discover without apply must not register anything")
	}
	r, err = d.cl.Discover(fmt.Sprint(port), nil, false, true)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := d.cl.Config()
	if len(c.Peers) != 0 {
		t.Fatalf("no fleet should have been added as a peer: %+v", c.Peers)
	}
	n := 0
	for _, e := range c.Endpoints {
		if strings.HasPrefix(e.URL, be.URL) || strings.Contains(e.URL, fmt.Sprintf(":%d", port)) {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("apply registered %d endpoints: %+v", n, c.Endpoints)
	}
}

func TestBenchAndProbe(t *testing.T) {
	isolate(t)
	// nothing healthy: an error, not a silent no-op
	empty := startDaemon(t, &config.Config{Name: "empty"})
	wantErr(t, empty.cl.Bench(nil), "no healthy chat endpoint to benchmark")
	wantErr(t, empty.cl.Probe(nil), "no healthy model on this machine to probe")
	if ch, err := empty.cl.ApplyProbe(nil); err != nil || len(ch) != 0 {
		t.Fatalf("ApplyProbe with nothing probed: %v %v", ch, err)
	}

	be := newUpstream(t)
	d := startDaemon(t, localConfig(be))
	d.waitHealthy(t, "loc-tiny")
	wantErr(t, d.cl.Bench([]string{"ghost"}), "no such endpoint: ghost")
	wantErr(t, d.cl.Probe([]string{"ghost"}), "no such node or endpoint: ghost")
	if err := d.cl.Bench([]string{"loc-tiny"}); err != nil {
		t.Fatal(err)
	}
	var st server.Status
	poll(t, "bench to finish", 15*time.Second, func() bool {
		st, _ = d.cl.Status()
		return !st.Bench.Running && st.Bench.Total == 1
	})
	for _, e := range st.Endpoints {
		if e.Name == "loc-tiny" && e.TPS <= 0 && e.BenchErr == "" {
			t.Fatalf("bench left no result: %+v", e)
		}
	}
}

func TestHostBench(t *testing.T) {
	isolate(t)
	d := startDaemon(t, &config.Config{Name: "alpha"})
	if err := d.cl.HostBench(); err != nil {
		t.Fatal(err)
	}
	wantErr(t, d.cl.HostBench(), "already running")
	var h server.HostReport
	poll(t, "machine bench", 20*time.Second, func() bool {
		var err error
		h, err = d.cl.Host("")
		return err == nil && !h.Benching
	})
	if h.Bench == nil || h.Bench.Fingerprint == "" {
		t.Fatalf("no machine benchmark stored: %+v", h.Bench)
	}
}

func TestPeersAndPairing(t *testing.T) {
	isolate(t)
	be := newUpstream(t)
	t.Setenv("FLEET_TEST_KEY_B", "secret-b")
	b := startDaemon(t, &config.Config{Name: "beta", APIKeyEnv: "FLEET_TEST_KEY_B",
		Endpoints: []config.Endpoint{{Name: "b-gemma", Node: "bnode", URL: be.URL + "/v1", Model: "gemma"}}})
	a := startDaemon(t, localConfig(be))
	b.waitHealthy(t, "b-gemma")
	bURL := "http://" + b.addr + "/v1"

	pc, err := b.cl.PairCode()
	if err != nil {
		t.Fatal(err)
	}
	if len(pc.Code) != 6 || time.Until(pc.Until) < 9*time.Minute || !strings.Contains(pc.Note, "listens on "+b.addr+" only") {
		t.Fatalf("pair code: %+v", pc)
	}
	_, err = a.cl.PeerPair(&config.Peer{Name: "beta"}, pc.Code)
	wantErr(t, err, "pairing needs the peer's address")
	wrong := fmt.Sprintf("%06d", (mustAtoi(t, pc.Code)+1)%1000000)
	_, err = a.cl.PeerPair(&config.Peer{Name: "beta", URL: bURL}, wrong)
	wantErr(t, err, "wrong pairing code")
	ps, err := a.cl.PeerPair(&config.Peer{Name: "beta", URL: bURL}, pc.Code)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 1 || ps[0].Name != "beta" || ps[0].URL != bURL {
		t.Fatalf("after pairing: %+v", ps)
	}
	bc, _ := b.cl.Config()
	if len(bc.Paired) != 1 || bc.Paired[0].Name != "alpha" {
		t.Fatalf("B did not record the pairing: %+v", bc.Paired)
	}
	_, err = a.cl.PeerPair(&config.Peer{Name: "beta2", URL: bURL}, pc.Code) // one use
	wantErr(t, err, "no pairing code is open")

	ps, err = a.cl.PeerOp(nil, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 1 || !ps[0].OK || ps[0].Models != 1 || ps[0].Remote != "beta" || ps[0].PeerID != bc.ID {
		t.Fatalf("after sync: %+v", ps)
	}
	if ps2, err := a.cl.Peers(); err != nil || len(ps2) != 1 {
		t.Fatalf("Peers: %+v %v", ps2, err)
	}
	ac, _ := a.cl.Config()
	if m := ac.Lookup("bnode/gemma"); len(m) != 1 || m[0].Peer != "beta" {
		t.Fatalf("imported: %+v", ac.Endpoints)
	}
	// the imported model answers through A → B → upstream
	ans, ep, err := a.cl.Chat("bnode/gemma", "", "hi", 0, 10*time.Second)
	if err != nil || ans != "pong" || ep == "" {
		t.Fatalf("chat via peer: %q %q %v", ans, ep, err)
	}
	// B's machine is read by B and passed on
	h, err := a.cl.Host("bnode")
	if err != nil || h.Err != "" || !strings.Contains(h.Source, "⇄ beta") {
		t.Fatalf("host via peer: %+v %v", h, err)
	}
	// probing is only for this machine's models
	wantErr(t, a.cl.Probe([]string{"bnode"}), "only this machine's models are probed here")

	// add / remove a peer that is not there
	ps, err = a.cl.PeerOp(&config.Peer{Name: "gamma", URL: "http://127.0.0.1:1/v1"}, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 2 {
		t.Fatalf("after add: %+v", ps)
	}
	poll(t, "gamma to be marked down", 5*time.Second, func() bool {
		ps, _ = a.cl.PeerOp(nil, "", true)
		for _, p := range ps {
			if p.Name == "gamma" && !p.OK && p.Err != "" {
				return true
			}
		}
		return false
	})
	_, err = a.cl.PeerOp(&config.Peer{Name: "bad name", URL: "http://x/v1"}, "", false)
	wantErr(t, err, "contains space")
	if ps, err = a.cl.PeerOp(nil, "gamma", false); err != nil || len(ps) != 1 {
		t.Fatalf("remove: %+v %v", ps, err)
	}
	_, err = a.cl.PeerOp(nil, "nobody", false)
	if err == nil {
		t.Log("removing an unknown peer is not an error")
	}
}

func mustAtoi(t *testing.T, s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestMain(m *testing.M) {
	discover.ScanFleetPort = 0 // scans never reach a real fleet on this machine's default port
	feature.SetRunner(true)    // these tests cover the full fleet (with fleet-engine); lite_test.go covers the router-only one
	os.Exit(m.Run())
}

func TestRoute(t *testing.T) {
	isolate(t)
	be := newUpstream(t)
	cfg := localConfig(be)
	cfg.Endpoints[0].TPS, cfg.Endpoints[1].TPS = 20, 80
	cfg.Roles["scout"] = config.Role{Preferred: "loc-tiny", Fallback: []string{"loc-thinker"}, Strategy: "cost"}
	d := startDaemon(t, cfg)
	d.waitHealthy(t, "loc-tiny", "loc-thinker")
	r, err := d.cl.Route("scout", 1000, 200)
	if err != nil {
		t.Fatal(err)
	}
	if r.Role != "scout" || r.Strategy != "cost" || r.In != 1000 || r.Out != 200 || len(r.Order) != 2 || r.Order[0].Endpoint != "loc-thinker" {
		t.Fatalf("%+v", r)
	}
	if r.Order[0].Total <= 0 || r.Order[0].Total >= r.Order[1].Total {
		t.Fatalf("the costs come with the order: %+v", r.Order)
	}
	if _, err := d.cl.Route("", 0, 0); err == nil {
		t.Fatal("an empty model is refused")
	}
}

func TestPlan(t *testing.T) {
	isolate(t)
	be := newUpstream(t)
	d := startDaemon(t, localConfig(be))
	d.waitHealthy(t, "loc-tiny", "loc-thinker")
	m := plan.Model{Name: "tiny", Arch: "llama", Layers: 2, Embd: 64, KVPerTokenLayer: 1024, HiddenBytes: 256,
		LayerBytes: []int64{1 << 20, 1 << 20}, LayerRead: []int64{1 << 20, 1 << 20}, OtherBytes: 1 << 20, OutputBytes: 1 << 19, EmbdBytes: 1 << 19}
	r, err := d.cl.Plan(server.PlanRequest{Model: m, Ctx: 128, Np: 1})
	if err != nil {
		t.Fatal(err)
	}
	if r.Model != "tiny" || r.Ctx != 128 || r.Verdict == "" {
		t.Fatalf("%+v", r)
	}
	if _, err := d.cl.Plan(server.PlanRequest{}); err == nil {
		t.Fatal("a plan without model facts is refused")
	}
}

func TestStore(t *testing.T) {
	dir := isolate(t)
	t.Setenv("LLAMA_CACHE", "")
	d := startDaemon(t, &config.Config{Name: "alpha"})
	p := filepath.Join(dir, "m.gguf")
	var b bytes.Buffer
	u32 := func(v uint32) { binary.Write(&b, binary.LittleEndian, v) }
	u64 := func(v uint64) { binary.Write(&b, binary.LittleEndian, v) }
	str := func(s string) { u64(uint64(len(s))); b.WriteString(s) }
	b.WriteString("GGUF")
	u32(3)
	u64(1)
	u64(3)
	str("general.architecture")
	u32(8)
	str("llama")
	str("llama.block_count")
	u32(4)
	u32(1)
	str("llama.embedding_length")
	u32(4)
	u32(64)
	str("blk.0.w.weight")
	u32(1)
	u64(8)
	u32(0)
	u64(0)
	for b.Len()%32 != 0 {
		b.WriteByte(0)
	}
	b.Write(make([]byte, 4096))
	if err := os.WriteFile(p, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := d.cl.StoreAdd(nil); err == nil {
		t.Fatal("no paths is refused")
	}
	n, err := d.cl.StoreAdd([]string{p})
	if err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	var inv store.Inventory
	poll(t, "the file is registered", 10*time.Second, func() bool {
		inv, err = d.cl.StoreLocal()
		return err == nil && len(inv.Files) == 1 && inv.Hashing == nil
	})
	if inv.Files[0].Name != "m" || !inv.Files[0].Complete {
		t.Fatalf("%+v", inv.Files)
	}
	all, err := d.cl.StoreAll()
	if err != nil || len(all) == 0 || all[0].Store == nil || len(all[0].Store.Files) != 1 {
		t.Fatalf("%+v %v", all, err)
	}
	e, err := d.cl.StoreRemove(inv.Files[0].SHA256[:8])
	if err != nil || e.Name != "m" {
		t.Fatalf("%+v %v", e, err)
	}
	if _, err := d.cl.StoreRemove("ghost"); err == nil {
		t.Fatal("an unknown file")
	}
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/tevfik/fleet-router/internal/discover"
	"github.com/tevfik/fleet-router/internal/feature"
	"io"
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

	"github.com/tevfik/fleet-router/internal/client"
	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/server"
)

// ---- fakes ----

func newUpstream(t *testing.T) *httptest.Server {
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/models"):
			fmt.Fprint(w, `{"object":"list","data":[{"id":"tiny"},{"id":"gemma"}]}`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/chat/completions"):
			var in map[string]any
			json.NewDecoder(r.Body).Decode(&in)
			if st, _ := in["stream"].(bool); st {
				w.Header().Set("Content-Type", "text/event-stream")
				send := func(s string) { fmt.Fprint(w, "data: "+s+"\n\n"); w.(http.Flusher).Flush() }
				send(`{"choices":[{"delta":{"content":"po"}}]}`)
				send(`{"choices":[{"delta":{"content":"ng"},"finish_reason":"stop"}]}`)
				send(`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2}}`)
				send(`[DONE]`)
				return
			}
			// echo the last user message so the test sees what the CLI sent
			msgs, _ := in["messages"].([]any)
			last := ""
			if len(msgs) > 0 {
				m, _ := msgs[len(msgs)-1].(map[string]any)
				last, _ = m["content"].(string)
			}
			sys := ""
			if len(msgs) > 1 {
				m, _ := msgs[0].(map[string]any)
				sys, _ = m["content"].(string)
			}
			b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "echo[" + sys + "]: " + last}}},
				"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 3}})
			w.Header().Set("Content-Type", "application/json")
			w.Write(b)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(be.Close)
	return be
}

func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("FLEET_CONFIG", filepath.Join(dir, "config.yaml"))
	t.Setenv("FLEET_STATS", filepath.Join(dir, "state", "stats.json"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "xdg-config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "xdg-state"))
	t.Setenv("HOME", dir)
	t.Setenv("FLEET_TOKEN", "")
	t.Setenv("FLEET_REPO", "http://127.0.0.1:1/nobody/nothing") // never the real repository
	t.Setenv("FLEET_LISTEN", "127.0.0.1:1")                     // nothing: until a daemon is started
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
	addr string
	cl   *client.Client
	path string
}

// startDaemon runs a full in-process daemon (mdns and auto-update off) on a free loopback port. With
// primary set, its config is FLEET_CONFIG and FLEET_LISTEN points at it, so the CLI commands talk to it.
func startDaemon(t *testing.T, cfg *config.Config, primary bool) *daemon {
	t.Helper()
	off := false
	cfg.Listen, cfg.MDNS, cfg.AutoUpdate = freeAddr(t), &off, &off
	if cfg.Roles == nil {
		cfg.Roles = map[string]config.Role{}
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if primary {
		path = config.Path()
	}
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
	d := &daemon{addr: cfg.Listen, cl: client.New(cfg.Listen), path: path}
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(6 * time.Second):
		}
	})
	for i := 0; i < 100 && !d.cl.Alive(); i++ {
		time.Sleep(30 * time.Millisecond)
	}
	if !d.cl.Alive() {
		t.Fatal("daemon did not come up")
	}
	if primary {
		t.Setenv("FLEET_LISTEN", d.addr)
	}
	return d
}

func (d *daemon) waitHealthy(t *testing.T, names ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, _ := d.cl.Status()
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
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("endpoints %v did not become healthy", names)
}

var outMu sync.Mutex // os.Stdout/os.Stderr are process-wide

// run calls f with stdout and stderr captured.
func run(t *testing.T, f func() error) (stdout, stderr string, err error) {
	t.Helper()
	outMu.Lock()
	defer outMu.Unlock()
	or, ow, _ := os.Pipe()
	er, ew, _ := os.Pipe()
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = ow, ew
	oc, ec := make(chan string), make(chan string)
	go func() { b, _ := io.ReadAll(or); oc <- string(b) }()
	go func() { b, _ := io.ReadAll(er); ec <- string(b) }()
	defer func() {
		os.Stdout, os.Stderr = oldOut, oldErr
	}()
	err = f()
	ow.Close()
	ew.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	return <-oc, <-ec, err
}

func runOK(t *testing.T, f func() error) string {
	t.Helper()
	out, _, err := run(t, f)
	if err != nil {
		t.Fatalf("unexpected error: %v\noutput:\n%s", err, out)
	}
	return out
}

func contains(t *testing.T, out string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if !strings.Contains(out, s) {
			t.Fatalf("output lacks %q:\n%s", s, out)
		}
	}
}

func wantErr(t *testing.T, err error, sub string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), sub) {
		t.Fatalf("want an error containing %q, got %v", sub, err)
	}
}

func baseConfig(be *httptest.Server) *config.Config {
	return &config.Config{
		Name: "alpha",
		Endpoints: []config.Endpoint{
			{Name: "loc-tiny", Node: "box", URL: be.URL + "/v1", Model: "tiny"},
			{Name: "loc-gemma", Node: "box", URL: be.URL + "/v1", Model: "gemma"},
		},
		Roles: map[string]config.Role{"scout": {Preferred: "loc-tiny", Fallback: []string{"loc-gemma"}}},
	}
}

// ---- pure helpers ----

func TestFlagsFirst(t *testing.T) {
	cases := []struct {
		in, value []string
		want      string
	}{
		{[]string{"file.yaml", "--write"}, nil, "--write|file.yaml"},
		{[]string{"scout", "-s", "be short", "hello", "world"}, []string{"s"}, "-s|be short|scout|hello|world"},
		{[]string{"-", "-n", "5"}, []string{"n"}, "-n|5|-"},
		{[]string{"-n"}, []string{"n"}, "-n"}, // a value flag at the end takes nothing
		{nil, nil, ""},
	}
	for _, c := range cases {
		got := strings.Join(flagsFirst(c.in, c.value...), "|")
		if got != c.want {
			t.Errorf("flagsFirst(%q, %q) = %q, want %q", c.in, c.value, got, c.want)
		}
	}
}

func TestTrunc(t *testing.T) {
	if trunc("short", 10) != "short" || trunc("abcdef", 4) != "abc…" || trunc("çğüşöı", 3) != "çğ…" {
		t.Fatal("trunc")
	}
	if trunc44(strings.Repeat("x", 44)) != strings.Repeat("x", 44) || len([]rune(trunc44(strings.Repeat("x", 50)))) != 44 {
		t.Fatal("trunc44")
	}
	if pct(0.5) != " 50%" {
		t.Fatalf("pct: %q", pct(0.5))
	}
}

func TestRefAndRefs(t *testing.T) {
	st := server.Status{Endpoints: []server.EpStatus{{Endpoint: config.Endpoint{Name: "dino-gemma", Node: "dino", Model: "gemma"}}}}
	if ref(st, "dino-gemma") != "dino/gemma" || ref(st, "ghost") != "ghost" {
		t.Fatal("ref")
	}
	if got := refs(st, []string{"dino-gemma", "x"}); strings.Join(got, ",") != "dino/gemma,x" {
		t.Fatalf("refs: %v", got)
	}
	if got := refs(st, nil); len(got) != 0 {
		t.Fatalf("refs(nil): %v", got)
	}
}

func TestRenderBoard(t *testing.T) {
	out := renderBoard(nil)
	if !strings.Contains(out, "TOKENS / SECOND") || !strings.Contains(out, "nothing measured yet") {
		t.Fatalf("empty board:\n%s", out)
	}
	eps := []server.EpStatus{
		{Endpoint: config.Endpoint{Name: "slow", Node: "a", Model: "m1", TPS: 10}},
		{Endpoint: config.Endpoint{Name: "fast", Node: "b", Model: "m2", TPS: 40}, TTFTMS: 120},
		{Endpoint: config.Endpoint{Name: "unmeasured", Node: "c", Model: "m3"}},
		{Endpoint: config.Endpoint{Name: "emb", Node: "c", Model: "bge-m3", Kind: "embed"}},
	}
	out = renderBoard(eps)
	if strings.Contains(out, "nothing measured") || strings.Contains(out, "emb ") {
		t.Fatalf("board:\n%s", out)
	}
	iFast, iSlow, iUn := strings.Index(out, "fast"), strings.Index(out, "slow"), strings.Index(out, "unmeasured")
	if !(iFast < iSlow && iSlow < iUn) {
		t.Fatalf("board order:\n%s", out)
	}
	contains(t, out, "  40.0 t/s", "b · ttft 120ms", "   —")
}

func TestListen(t *testing.T) {
	isolate(t)
	t.Setenv("FLEET_LISTEN", "")
	if listen() != config.DefaultListen {
		t.Fatalf("no config: %s", listen())
	}
	c := config.Default()
	c.Listen = "127.0.0.1:5999"
	if err := c.Save(config.Path()); err != nil {
		t.Fatal(err)
	}
	if listen() != "127.0.0.1:5999" {
		t.Fatalf("from the config: %s", listen())
	}
	t.Setenv("FLEET_LISTEN", "127.0.0.1:6000")
	if listen() != "127.0.0.1:6000" {
		t.Fatalf("FLEET_LISTEN: %s", listen())
	}
}

func TestServe_FleetListen(t *testing.T) {
	isolate(t)
	c := config.Default()
	c.Listen = "127.0.0.1:5999"
	if err := c.Save(config.Path()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLEET_LISTEN", "127.0.0.1:6001")
	srv, err := server.New(config.Path())
	if err != nil {
		t.Fatal(err)
	}
	if l := os.Getenv("FLEET_LISTEN"); l != "" {
		srv.SetListen(l)
	}
	if srv.Config().Listen != "127.0.0.1:6001" {
		t.Fatalf("expected listen 127.0.0.1:6001, got %s", srv.Config().Listen)
	}
	// any later configuration change (an endpoint added through the daemon) must not write the environment's address
	cfg := srv.Config()
	if err := srv.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	saved, err := config.Load(config.Path())
	if err != nil {
		t.Fatal(err)
	}
	if saved.Listen != "127.0.0.1:5999" {
		t.Fatalf("FLEET_LISTEN leaked into the file: listen %s", saved.Listen)
	}
	if srv.Config().Listen != "127.0.0.1:6001" {
		t.Fatalf("the override was lost in memory: %s", srv.Config().Listen)
	}
}

// ---- without a daemon ----

func TestCommandsNeedADaemon(t *testing.T) {
	isolate(t) // FLEET_LISTEN → port 1
	for name, f := range map[string]func() error{
		"status": cmdStatus, "top": cmdTop, "opencode": cmdOpenCode,
		"role":  func() error { return cmdRole([]string{"list"}) },
		"peers": func() error { return cmdPeers(nil) },
		"nodes": func() error { return cmdNodes(nil) },
		"log":   func() error { return cmdLog(nil) },
		"usage": func() error { return cmdUsage(nil) },
		"hosts": func() error { return cmdHosts(nil) },
		"bench": func() error { return cmdBench(nil) },
		"ask":   func() error { return cmdAsk([]string{"scout", "hi"}) },
	} {
		_, _, err := run(t, f)
		if err == nil || !strings.Contains(err.Error(), "the daemon is not running (http://127.0.0.1:1)") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// doctor never fails: it reports
	out := runOK(t, cmdDoctor)
	contains(t, out, "no configuration: "+config.Path(), "configuration is valid", "daemon is not running")
}

func TestConfigHistoryAndUndoOffline(t *testing.T) {
	isolate(t)
	out := runOK(t, func() error { return cmdConfig([]string{"history"}) })
	contains(t, out, "no earlier configurations kept yet")
	c := config.Default()
	c.Name = "one"
	c.Save(config.Path())
	time.Sleep(5 * time.Millisecond)
	c.Name = "two"
	c.Save(config.Path())
	out = runOK(t, func() error { return cmdConfig([]string{"history"}) })
	if strings.Contains(out, "no earlier") || strings.Contains(out, ".yaml") {
		t.Fatalf("history:\n%s", out)
	}
	out = runOK(t, func() error { return cmdConfig([]string{"undo"}) })
	contains(t, out, "✓ restored", "the daemon is not running")
	if back, _ := config.Load(config.Path()); back.Name != "one" {
		t.Fatalf("undo restored %q", back.Name)
	}
	_, _, err := run(t, func() error { return cmdConfig([]string{"what"}) })
	wantErr(t, err, "usage: fleet config history | undo")
}

func TestEvalShowEmpty(t *testing.T) {
	isolate(t)
	out := runOK(t, func() error { return cmdEval([]string{"--show"}) })
	contains(t, out, "no scores yet")
}

// ---- update (fake release server only) ----

func fakeReleases(t *testing.T, tag, body string) *httptest.Server {
	var mu sync.Mutex
	hits := 0
	rs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/fleet/fleet/releases/latest" {
			t.Errorf("unexpected release-server request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		hits++
		mu.Unlock()
		json.NewEncoder(w).Encode(map[string]string{"tag_name": tag, "body": body})
	}))
	t.Cleanup(rs.Close)
	t.Setenv("FLEET_REPO", rs.URL+"/fleet/fleet")
	return rs
}

func TestUpdateCheck(t *testing.T) {
	isolate(t)
	fakeReleases(t, "v1.4.0", "Faster routing\n\n  fixed a bug  \n")
	old := version
	t.Cleanup(func() { version = old })

	version = "v1.4.0"
	out := runOK(t, func() error { return cmdUpdate([]string{"--check"}) })
	contains(t, out, "fleet v1.4.0 is up to date (latest release: v1.4.0)")

	version = "v1.3.2"
	out = runOK(t, func() error { return cmdUpdate([]string{"--check"}) })
	contains(t, out, "fleet v1.3.2 → v1.4.0 available: run `fleet update`", "  Faster routing", "  fixed a bug")

	version = "dev" // not a release: only --force would replace it, so --check says up to date
	out = runOK(t, func() error { return cmdUpdate([]string{"--check"}) })
	contains(t, out, "fleet dev is up to date")

	// --all with no SSH machine: reports it and runs no ssh
	version = "v1.4.0"
	out = runOK(t, func() error { return cmdUpdate([]string{"--check", "--all"}) })
	contains(t, out, "is up to date", "no machines reached over SSH")

	_, _, err := run(t, func() error { return cmdUpdate([]string{"--bogus"}) })
	wantErr(t, err, "flag provided but not defined")
}

func TestUpdateCheckErrors(t *testing.T) {
	isolate(t)
	rs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/empty/") {
			fmt.Fprint(w, `{"tag_name":""}`)
			return
		}
		http.Error(w, "nope", 404)
	}))
	defer rs.Close()
	t.Setenv("FLEET_REPO", rs.URL+"/x/missing")
	_, _, err := run(t, func() error { return cmdUpdate([]string{"--check"}) })
	wantErr(t, err, "404")
	t.Setenv("FLEET_REPO", rs.URL+"/x/empty/")
	_, _, err = run(t, func() error { return cmdUpdate([]string{"--check"}) })
	wantErr(t, err, "no release published yet")
}

func TestUpdateRemotesWithoutSSH(t *testing.T) {
	isolate(t)
	be := newUpstream(t)
	c := baseConfig(be) // URL endpoints and a URL peer only
	c.Peers = []config.Peer{{Name: "far", URL: "http://127.0.0.1:1/v1"}}
	if err := c.Save(config.Path()); err != nil {
		t.Fatal(err)
	}
	out := runOK(t, func() error { updateRemotes(true, false); return nil })
	if strings.TrimSpace(out) != "no machines reached over SSH" {
		t.Fatalf("updateRemotes:\n%s", out)
	}
	os.WriteFile(config.Path(), []byte("endpoints: [ {name: x} ]\n"), 0o600) // invalid: no url
	out = runOK(t, func() error { updateRemotes(false, false); return nil })
	contains(t, out, "could not read the configuration")
}

// ---- against an in-process daemon ----

func TestStatusTopDoctorOpenCode(t *testing.T) {
	isolate(t)
	be := newUpstream(t)
	cfg := baseConfig(be)
	cfg.Endpoints = append(cfg.Endpoints, config.Endpoint{Name: "far-x", Node: "far", URL: "http://127.0.0.2:1/v1", Model: "x"})
	cfg.Roles["lead"] = config.Role{Preferred: "far-x", Fallback: []string{"loc-tiny"}}
	d := startDaemon(t, cfg, true)
	d.waitHealthy(t, "loc-tiny", "loc-gemma")
	for i := 0; i < 100; i++ { // the down endpoint is known after the first health round
		st, _ := d.cl.Status()
		if st.Endpoints[2].Known {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}

	out := runOK(t, cmdStatus)
	contains(t, out, "fleet dev\n", "ROLES", "scout", "→ box/tiny", "(preferred box/tiny, fallback [box/gemma])",
		"lead", "(preferred far/x, fallback [box/tiny])  ← fallback active", "▸ box", "✓ tiny", "▸ far", "✗ x")

	old := version
	version = "v9.0.0"
	out = runOK(t, cmdStatus)
	version = old
	contains(t, out, "fleet v9.0.0 · daemon dev (restart the daemon to run v9.0.0)")

	out = runOK(t, func() error { return cmdRole([]string{"list"}) })
	contains(t, out, "ROLES", "scout")

	out = runOK(t, cmdTop)
	contains(t, out, "TOKENS / SECOND", "loc-tiny", "nothing measured yet")

	out = runOK(t, cmdOpenCode)
	var oc map[string]map[string]any
	if err := json.Unmarshal([]byte(out), &oc); err != nil {
		t.Fatalf("opencode JSON: %v\n%s", err, out)
	}
	if oc["providers"]["fleet"] == nil {
		t.Fatalf("opencode: %s", out)
	}

	out = runOK(t, cmdDoctor)
	contains(t, out, "✓ configuration: "+config.Path(), "✓ configuration is valid", "not exposed beyond this machine",
		"✓ daemon is running (http://"+d.addr+")", "1/3 endpoints down", "role scout → loc-tiny", "✗ role lead: fallback active (loc-tiny)")
	// a node that is struggling is printed as a strain line (the strain readings are the daemon's own)
	st, _ := d.cl.Status()
	for _, n := range st.Nodes {
		if n.Strain != nil {
			for _, r := range n.Strain.Reasons {
				contains(t, out, "✗ "+n.Node+": "+r)
			}
			for _, w := range n.Strain.Warnings {
				contains(t, out, "! "+n.Node+": "+w)
			}
		}
	}
}

func TestDoctorFlagsExposedListenWithoutKey(t *testing.T) {
	isolate(t)
	c := config.Default()
	c.Listen = "0.0.0.0:4799"
	c.APIKeyEnv = "FLEET_TEST_EMPTY_KEY"
	t.Setenv("FLEET_TEST_EMPTY_KEY", "")
	c.Save(config.Path())
	out := runOK(t, cmdDoctor)
	contains(t, out, "✓ listening on 0.0.0.0:4799", "✗ api_key_env names FLEET_TEST_EMPTY_KEY but it is empty")
	c.APIKeyEnv = ""
	c.Save(config.Path())
	out = runOK(t, cmdDoctor)
	contains(t, out, "✗ listening on 0.0.0.0:4799 without api_key_env")
}

func TestRoleCommands(t *testing.T) {
	isolate(t)
	be := newUpstream(t)
	d := startDaemon(t, baseConfig(be), true)
	d.waitHealthy(t, "loc-tiny", "loc-gemma")
	cfg := func() *config.Config {
		c, err := d.cl.Config()
		if err != nil {
			t.Fatal(err)
		}
		return &c
	}
	role := func(args ...string) error {
		_, _, err := run(t, func() error { return cmdRole(args) })
		return err
	}
	if err := role("set", "coder", "loc-gemma", "loc-tiny"); err != nil {
		t.Fatal(err)
	}
	if r := cfg().Roles["coder"]; r.Preferred != "loc-gemma" || strings.Join(r.Fallback, ",") != "loc-tiny" {
		t.Fatalf("role set: %+v", r)
	}
	if err := role("opt", "coder", "priority=3", "max_parallel=2", "overflow=true", "strategy=round-robin", "use=writing"); err != nil {
		t.Fatal(err)
	}
	if r := cfg().Roles["coder"]; r.Priority != 3 || r.MaxParallel != 2 || !r.Overflow || r.Strategy != "round-robin" || r.Use != "writing" {
		t.Fatalf("role opt: %+v", r)
	}
	wantErr(t, role("opt", "coder", "priority=high"), "priority must be a number")
	wantErr(t, role("opt", "coder", "colour=red"), "unknown setting: colour")
	wantErr(t, role("opt", "coder", "strategy=random"), `unknown strategy "random"`)
	if err := role("tag", "loc-tiny", "fast", "cheap"); err != nil {
		t.Fatal(err)
	}
	if e := cfg().Endpoint("loc-tiny"); strings.Join(e.Tags, ",") != "fast,cheap" {
		t.Fatalf("role tag: %v", e.Tags)
	}
	if err := role("group", "loc-tiny", "engine=llama-ring", "workers=w1,w2", "stall=45s"); err != nil {
		t.Fatal(err)
	}
	if e := cfg().Endpoint("loc-tiny"); e.Group == nil || e.Group.Engine != "llama-ring" || len(e.Group.Workers) != 2 || e.Stall != "45s" {
		t.Fatalf("role group: %+v %q", e.Group, e.Stall)
	}
	wantErr(t, role("group", "loc-tiny", "engine=x", "stall=soon"), "stall")
	wantErr(t, role("group", "loc-tiny", "workers=w1"), "engine")
	if err := role("group", "loc-tiny", "off"); err != nil {
		t.Fatal(err)
	}
	if e := cfg().Endpoint("loc-tiny"); e.Group != nil {
		t.Fatalf("off must remove the group: %+v", e.Group)
	}
	if err := role("fleetport", "loc-tiny", "4800"); err != nil {
		t.Fatal(err)
	}
	if e := cfg().Endpoint("loc-tiny"); e.FleetPort != 4800 {
		t.Fatalf("role fleetport: %d", e.FleetPort)
	}
	wantErr(t, role("fleetport", "loc-tiny", "high"), "port must be a number")
	if err := role("fleetport", "loc-tiny", "0"); err != nil || cfg().Endpoint("loc-tiny").FleetPort != 0 {
		t.Fatalf("0 = back to 4711: %v", err)
	}
	if err := role("match", "quick", "tag:fast"); err != nil {
		t.Fatal(err)
	}
	if r := cfg().Roles["quick"]; strings.Join(r.Match, ",") != "tag:fast" {
		t.Fatalf("role match: %+v", r)
	}
	if err := role("slots", "loc-tiny", "4"); err != nil {
		t.Fatal(err)
	}
	if e := cfg().Endpoint("loc-tiny"); e.Slots != 4 {
		t.Fatalf("role slots: %d", e.Slots)
	}
	wantErr(t, role("slots", "loc-tiny", "four"), "slots must be a number")
	wantErr(t, role("slots", "ghost", "1"), "no such endpoint: ghost")

	out := runOK(t, func() error { return cmdRole([]string{"test", "scout"}) })
	contains(t, out, "✓ via router  (scout → loc-tiny)", "✓ loc-tiny", "✓ loc-gemma", `"echo[]: Reply with exactly one word: pong"`)
	_, _, err := run(t, func() error { return cmdRole([]string{"test", "ghost"}) })
	wantErr(t, err, "no such role: ghost")

	out = runOK(t, func() error { return cmdRole([]string{"init"}) })
	if !strings.Contains(out, "use --apply to create them") && !strings.Contains(out, "nothing to suggest") {
		t.Fatalf("role init:\n%s", out)
	}
	before := len(cfg().Roles)
	out = runOK(t, func() error { return cmdRole([]string{"init", "--apply"}) })
	if !strings.Contains(out, "nothing to suggest") && len(cfg().Roles) <= before {
		t.Fatalf("role init --apply created nothing:\n%s", out)
	}

	if err := role("rm", "coder"); err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg().Roles["coder"]; ok {
		t.Fatal("role rm")
	}
	wantErr(t, role("set", "x"), "usage: fleet role set")
	wantErr(t, role(), "usage: fleet role set")
	wantErr(t, role("set", "bad", "ghost"), `unknown endpoint "ghost"`)

	// fleet config history / undo with the daemon running: applied hot
	out = runOK(t, func() error { return cmdConfig([]string{"history"}) })
	if n := len(strings.Split(strings.TrimSpace(out), "\n")); n < 5 {
		t.Fatalf("history has %d entries:\n%s", n, out)
	}
	out = runOK(t, func() error { return cmdConfig([]string{"undo"}) })
	contains(t, out, "✓ previous configuration restored and applied (")
	if _, ok := cfg().Roles["coder"]; !ok {
		t.Fatal("undo did not bring back the removed role")
	}
}

func TestAsk(t *testing.T) {
	isolate(t)
	be := newUpstream(t)
	d := startDaemon(t, baseConfig(be), true)
	d.waitHealthy(t, "loc-tiny")

	out, errOut, err := run(t, func() error { return cmdAsk([]string{"scout", "what", "is", "up"}) })
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "echo[]: what is up" || strings.TrimSpace(errOut) != "[loc-tiny]" {
		t.Fatalf("ask: stdout %q stderr %q", out, errOut)
	}
	// flags after the role, a system prompt and a file
	f := filepath.Join(t.TempDir(), "notes.txt")
	os.WriteFile(f, []byte("FILE"), 0o600)
	out = runOK(t, func() error { return cmdAsk([]string{"scout", "look", "-s", "terse", "-f", f, "-n", "50"}) })
	if strings.TrimSpace(out) != "echo[terse]: look\n\nFILE" {
		t.Fatalf("ask -s -f: %q", out)
	}
	// "-" reads the prompt from stdin
	r, w, _ := os.Pipe()
	oldIn := os.Stdin
	os.Stdin = r
	w.WriteString("from stdin")
	w.Close()
	out, _, err = run(t, func() error { return cmdAsk([]string{"scout", "-"}) })
	os.Stdin = oldIn
	if err != nil || strings.TrimSpace(out) != "echo[]: from stdin" {
		t.Fatalf("ask -: %q %v", out, err)
	}
	_, _, err = run(t, func() error { return cmdAsk(nil) })
	wantErr(t, err, "usage: fleet ask")
	_, _, err = run(t, func() error { return cmdAsk([]string{"scout", "-f", "/nonexistent/file"}) })
	wantErr(t, err, "no such file")
	_, _, err = run(t, func() error { return cmdAsk([]string{"ghost-role", "hi"}) })
	wantErr(t, err, "http 404")
}

func TestLogUsageBench(t *testing.T) {
	isolate(t)
	be := newUpstream(t)
	d := startDaemon(t, baseConfig(be), true)
	d.waitHealthy(t, "loc-tiny", "loc-gemma")

	out := runOK(t, func() error { return cmdUsage(nil) })
	contains(t, out, "no traffic recorded in this period")
	out = runOK(t, func() error { return cmdLog([]string{"-n", "5"}) })
	if strings.TrimSpace(out) != "" {
		t.Fatalf("log before any request:\n%s", out)
	}
	for i := 0; i < 3; i++ {
		if _, _, err := d.cl.Chat("scout", "", "hi", 0, 10*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 100; i++ {
		if rows, _ := d.cl.Usage("role", "", "24h"); len(rows) > 0 && rows[0].Requests == 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	out = runOK(t, func() error { return cmdLog([]string{"-n", "2"}) })
	if lines := strings.Split(strings.TrimSpace(out), "\n"); len(lines) != 2 || !strings.Contains(out, "scout → loc-tiny") {
		t.Fatalf("log -n 2:\n%s", out)
	}
	out = runOK(t, func() error { return cmdUsage([]string{"--by", "role", "--since", "24h"}) })
	contains(t, out, "scout")
	out = runOK(t, func() error { return cmdUsage([]string{"--by=endpoint"}) })
	contains(t, out, "loc-tiny")
	_, _, err := run(t, func() error { return cmdUsage([]string{"--by", "colour"}) })
	wantErr(t, err, "--by must be client, role, model, endpoint or day")

	// G4: fleet usage --hub
	out = runOK(t, func() error { return cmdUsage([]string{"--by", "role", "--hub"}) })
	contains(t, out, "scout")

	out = runOK(t, func() error { return cmdBench([]string{"loc-tiny"}) })
	contains(t, out, "TOKENS / SECOND", "loc-tiny")
	_, _, err = run(t, func() error { return cmdBench([]string{"ghost"}) })
	wantErr(t, err, "no such endpoint: ghost")
}

func TestHosts(t *testing.T) {
	isolate(t)
	d := startDaemon(t, &config.Config{Name: "alpha", Discover: config.Discover{Ports: "8080"}}, true)
	hosts := func(args ...string) string {
		return runOK(t, func() error { return cmdHosts(args) })
	}
	contains(t, hosts(), "ssh    : \n", "ports  : 8080\n")
	contains(t, hosts("add", "h1", "h2", "h1"), "ssh    : h1, h2\n")
	contains(t, hosts("rm", "h1"), "ssh    : h2\n")
	contains(t, hosts("ports", "11434", "8000-8002"), "ports  : 11434,8000-8002\n")
	contains(t, hosts("ignore", "box:1919", "box:1919"), "ignore : box:1919\n")
	if c, _ := d.cl.Config(); strings.Join(c.Discover.SSH, ",") != "h2" || c.Discover.Ports != "11434,8000-8002" {
		t.Fatalf("hosts saved: %+v", c.Discover)
	}
	_, _, err := run(t, func() error { return cmdHosts([]string{"frobnicate", "x"}) })
	wantErr(t, err, "usage: fleet hosts")
}

func TestDiscoverCommand(t *testing.T) {
	isolate(t)
	be := newUpstream(t)
	d := startDaemon(t, &config.Config{Name: "alpha", Discover: config.Discover{Ignore: []string{"alpha:4711"}}} /* a real fleet on the default port is never added */, true)
	port := be.URL[strings.LastIndex(be.URL, ":")+1:]
	out := runOK(t, func() error { return cmdDiscover([]string{"--ports", port, "--ssh", ""}) })
	contains(t, out, "+ ", "tiny", "gemma", "use --apply to register")
	out = runOK(t, func() error { return cmdDiscover([]string{"--ports", port, "--apply"}) })
	contains(t, out, "registered")
	c, _ := d.cl.Config()
	n := 0
	for _, e := range c.Endpoints {
		if strings.Contains(e.URL, ":"+port) {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("discover --apply registered %d: %+v", n, c.Endpoints)
	}
	out = runOK(t, func() error { return cmdDiscover([]string{"--ports", port}) })
	contains(t, out, "✓ already registered:")
}

func TestPeersCommand(t *testing.T) {
	isolate(t)
	be := newUpstream(t)
	other := startDaemon(t, &config.Config{Name: "beta",
		Endpoints: []config.Endpoint{{Name: "b-gemma", Node: "bnode", URL: be.URL + "/v1", Model: "gemma"}}}, false)
	other.waitHealthy(t, "b-gemma")
	third := startDaemon(t, &config.Config{Name: "gamma"}, false)
	d := startDaemon(t, baseConfig(be), true)
	peers := func(args ...string) (string, error) {
		out, _, err := run(t, func() error { return cmdPeers(args) })
		return out, err
	}

	out, err := peers()
	if err != nil {
		t.Fatal(err)
	}
	contains(t, out, "no peers — `fleet discover`")
	if strings.Contains(out, "on this network") {
		t.Fatalf("list must not show an empty LAN section:\n%s", out)
	}
	out, _ = peers("lan")
	contains(t, out, "on this network (mDNS):", "nobody heard yet")

	// pair: the code the other fleet shows
	pairOut := runOK(t, func() error {
		old := os.Getenv("FLEET_LISTEN")
		os.Setenv("FLEET_LISTEN", other.addr)
		defer os.Setenv("FLEET_LISTEN", old)
		return cmdPeers([]string{"pair"})
	})
	contains(t, pairOut, "pairing code: ", "one use", "on the other machine:", "note: this fleet listens on "+other.addr+" only")
	code := strings.Fields(strings.SplitN(pairOut, "pairing code: ", 2)[1])[0]
	if _, err := strconv.Atoi(code); err != nil || len(code) != 6 {
		t.Fatalf("code %q", code)
	}

	_, err = peers("add", "beta", "http://"+other.addr+"/v1", "pair=000000x")
	wantErr(t, err, "wrong pairing code")
	out, err = peers("add", "beta", "http://"+other.addr+"/v1", "pair="+code, "only=node:bnode", "max_hops=1")
	if err != nil {
		t.Fatal(err)
	}
	contains(t, out, "✓ beta", "http://"+other.addr+"/v1", "1 models", "fleet dev")
	c, _ := d.cl.Config()
	if len(c.Peers) != 1 || c.Peers[0].MaxHops != 1 || strings.Join(c.Peers[0].Only, ",") != "node:bnode" {
		t.Fatalf("peer saved: %+v", c.Peers)
	}
	// without a code (loopback needs no key), calls itself something else
	out, err = peers("add", "g", "http://"+third.addr+"/v1")
	if err != nil {
		t.Fatal(err)
	}
	contains(t, out, "✓ g", "0 models", "(calls itself gamma)")
	out, err = peers("sync")
	if err != nil {
		t.Fatal(err)
	}
	contains(t, out, "✓ beta", "✓ g")

	_, err = peers("add", "x", "http://127.0.0.1:1/v1", "max_hops=two")
	wantErr(t, err, "max_hops must be a number")
	_, err = peers("add", "x", "http://127.0.0.1:1/v1", "colour=red")
	wantErr(t, err, "unknown setting: colour")
	_, err = peers("add", "x", "http://127.0.0.1:1/v1", "only=nonsense")
	if err == nil {
		t.Fatal("a bad selector must be refused")
	}

	if _, err = peers("rm", "g"); err != nil {
		t.Fatal(err)
	}
	out, _ = peers("list")
	if strings.Contains(out, " g ") {
		t.Fatalf("rm left the peer:\n%s", out)
	}
	_, err = peers("bogus")
	wantErr(t, err, "usage: fleet peers")
	_, err = peers("add", "onlyname")
	wantErr(t, err, "usage: fleet peers")

	// the peer's node shows up in nodes and can be shown in full; probing it is refused
	out = runOK(t, func() error { return cmdNodes(nil) })
	contains(t, out, "NODE", "box", "this device", "bnode", "⇄ beta", "`fleet nodes <node>` for one machine in full")
	out = runOK(t, func() error { return cmdNodes([]string{"bnode"}) })
	contains(t, out, "bnode")
	out = runOK(t, func() error { return cmdProbe([]string{"--show", "bnode"}) })
	if strings.Contains(out, "gemma") {
		t.Fatalf("probe --show lists only this machine's models:\n%s", out)
	}
}

func TestNodesAndProbe(t *testing.T) {
	isolate(t)
	be := newUpstream(t)
	d := startDaemon(t, baseConfig(be), true)
	d.waitHealthy(t, "loc-tiny", "loc-gemma")

	out := runOK(t, func() error { return cmdNodes(nil) })
	contains(t, out, "NODE", "VIA", "AI", "box", "this device", "2/2")
	out = runOK(t, func() error { return cmdNodes([]string{"BOX"}) }) // case-insensitive
	contains(t, out, "box")
	_, _, err := run(t, func() error { return cmdNodes([]string{"ghost"}) })
	wantErr(t, err, "no such node: ghost")
	out = runOK(t, func() error { return cmdNodes([]string{"suggest"}) })
	contains(t, out, "nothing to suggest")

	out, errOut, err := run(t, func() error { return cmdNodes([]string{"bench"}) })
	if err != nil {
		t.Fatal(err)
	}
	contains(t, errOut, "benchmarking this machine")
	if strings.TrimSpace(out) == "" {
		t.Fatal("nodes bench printed nothing")
	}
	if h, _ := d.cl.Host(""); h.Bench == nil {
		t.Fatal("no machine benchmark after nodes bench")
	}

	out = runOK(t, func() error { return cmdProbe([]string{"--show"}) })
	contains(t, out, "▸ box", "tiny", "gemma", "not probed")
	out = runOK(t, func() error { return cmdProbe([]string{"--show", "--apply"}) })
	contains(t, out, "caps already match the probe")
}

func TestNodesWithoutModels(t *testing.T) {
	isolate(t)
	startDaemon(t, &config.Config{Name: "alpha"}, true)
	out := runOK(t, func() error { return cmdNodes(nil) }) // this machine is a node all the same
	contains(t, out, "this device", server.Hostname())
}

// fakeHotPeer is a peer fleet whose one machine is struggling (it reports a strain with its model).
func fakeHotPeer(t *testing.T) *httptest.Server {
	ps := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fleet/hello":
			json.NewEncoder(w).Encode(server.Hello{Fleet: true, ID: "hot0id", Name: "hotfleet", Version: "v0.9.0"})
		case "/fleet/export":
			st := server.NodeStrain{Source: "this machine"}
			st.At = time.Now()
			st.Reasons = []string{"GPU at 95°C"}
			st.Warnings = []string{"disk / is 97% full"}
			json.NewEncoder(w).Encode(map[string]any{"id": "hot0id", "name": "hotfleet", "models": []server.ExportModel{
				{Ref: "hot/llama", Node: "hot", Model: "llama", Up: true, Path: []string{"hot0id"}, Strain: &st}}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ps.Close)
	return ps
}

func TestDoctorPrintsStrainLines(t *testing.T) {
	isolate(t)
	hot := fakeHotPeer(t)
	d := startDaemon(t, &config.Config{Name: "alpha", Peers: []config.Peer{{Name: "hotpeer", URL: hot.URL + "/v1"}}}, true)
	if ps, err := d.cl.PeerOp(nil, "", true); err != nil || len(ps) != 1 || !ps[0].OK || ps[0].Models != 1 {
		t.Fatalf("sync with the fake peer: %+v %v", ps, err)
	}
	out := runOK(t, cmdDoctor)
	contains(t, out, "  ✗ hot: GPU at 95°C — routing prefers other machines until it recovers", "  ! hot: disk / is 97% full")
	// fleet peers shows the version and the name it gives itself
	out = runOK(t, func() error { return cmdPeers(nil) })
	contains(t, out, "✓ hotpeer", "1 models", "(calls itself hotfleet) · fleet v0.9.0")
	// fleet nodes marks the node with what its peer says
	out = runOK(t, func() error { return cmdNodes(nil) })
	contains(t, out, "hot", "⇄ hotpeer")
}

func TestMain(m *testing.M) {
	discover.ScanFleetPort = 0                       // scans never reach a real fleet on this machine's default port
	feature.SetRunner(true)                          // these tests cover the full fleet (with fleet-engine); lite_test.go covers the router-only one
	config.DefaultHubURL = "https://hub.example.com" // like a build with a default hub; compat_test.go runs the public build
	// deploy tests write run records (~/.cache/fleet/runs): never into the real ones, which plan the ports of real deploys
	cache, err := os.MkdirTemp("", "fleet-cache-")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_CACHE_HOME", cache)
	code := m.Run()
	os.RemoveAll(cache)
	os.Exit(code)
}

func TestEngineGuardKeepsABinaryThatHasTheEngine(t *testing.T) {
	if engineGuard(false) != nil {
		t.Fatal("the plain build updates as before")
	}
	if err := engineGuard(true); err == nil || !strings.Contains(err.Error(), "llama-ring engine") {
		t.Fatalf("%v", err)
	}
}

// fakeDaemonEnv is a virtual clock plus a "daemon" that starts answering at upAt (zero: never) and an embedded
// start that either binds or fails at once.
type fakeDaemonEnv struct {
	now       time.Duration
	upAt      time.Duration
	bindErr   error
	started   int
	startFail error
}

func (f *fakeDaemonEnv) probe() daemonProbe {
	return daemonProbe{
		alive: func() bool { return f.upAt > 0 && f.now >= f.upAt },
		start: func() (<-chan error, error) {
			f.started++
			if f.startFail != nil {
				return nil, f.startFail
			}
			ch := make(chan error, 1)
			if f.bindErr != nil {
				ch <- f.bindErr
			} else if f.upAt == 0 {
				f.upAt = f.now + 200*time.Millisecond // the embedded daemon comes up shortly
			}
			return ch, nil
		},
		sleep: func(d time.Duration) { f.now += d },
		addr:  "127.0.0.1:4711",
	}
}

func TestEnsureDaemon_BusyPortSlowDaemonIsUsed(t *testing.T) {
	f := &fakeDaemonEnv{bindErr: errors.New("listen tcp: address already in use"), upAt: time.Second}
	// alive() is false at t=0, then the slow daemon answers at 1s
	embedded, err := ensureDaemon(f.probe())
	if err != nil || embedded {
		t.Fatalf("embedded=%v err=%v: the slow daemon should be used", embedded, err)
	}
	if f.now < time.Second || f.now > 1500*time.Millisecond {
		t.Fatalf("waited %v", f.now)
	}
}

func TestEnsureDaemon_BusyPortNoAnswerGivesAClearError(t *testing.T) {
	f := &fakeDaemonEnv{bindErr: errors.New("listen tcp 127.0.0.1:4711: bind: address already in use")}
	_, err := ensureDaemon(f.probe())
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"cannot start", "address already in use", "127.0.0.1:4711", "within 3s", "fleet status"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	if f.now < daemonWait || f.now > daemonWait+500*time.Millisecond {
		t.Fatalf("gave up after %v, want about 3s", f.now)
	}
	// an answer just inside the 3 s limit still counts
	g := &fakeDaemonEnv{bindErr: errors.New("busy"), upAt: 2900 * time.Millisecond}
	if embedded, err := ensureDaemon(g.probe()); err != nil || embedded {
		t.Fatalf("embedded=%v err=%v", embedded, err)
	}
}

func TestEnsureDaemon_NoDaemonStartsTheEmbeddedOne(t *testing.T) {
	f := &fakeDaemonEnv{}
	embedded, err := ensureDaemon(f.probe())
	if err != nil || !embedded || f.started != 1 {
		t.Fatalf("embedded=%v err=%v started=%d", embedded, err, f.started)
	}
	// an embedded daemon that never answers: the clear 3 s message
	h := &fakeDaemonEnv{}
	p := h.probe()
	p.alive = func() bool { return false }
	if _, err := ensureDaemon(p); err == nil || !strings.Contains(err.Error(), "did not come up") || !strings.Contains(err.Error(), "fleet daemon") {
		t.Fatalf("%v", err)
	}
	// a daemon already running is used as it is, nothing started
	run := &fakeDaemonEnv{upAt: 1, now: 5}
	if embedded, err := ensureDaemon(run.probe()); err != nil || embedded || run.started != 0 {
		t.Fatalf("embedded=%v err=%v started=%d", embedded, err, run.started)
	}
	// the embedded server cannot even be created
	bad := &fakeDaemonEnv{startFail: errors.New("bad config")}
	if _, err := ensureDaemon(bad.probe()); err == nil || err.Error() != "bad config" {
		t.Fatalf("%v", err)
	}
}

// `fleet usage --hub --by client` must keep --by's value: -hub is boolean, so it is not a value flag
func TestUsageFlagsOrderWithBooleanHub(t *testing.T) {
	for _, args := range [][]string{
		{"--hub", "--by", "client", "--since", "7d"},
		{"--by", "client", "--hub"},
		{"--since", "24h", "--hub", "--by", "model"},
	} {
		got := flagsFirst(args, usageValueFlags...)
		if strings.Join(got, " ") != strings.Join(args, " ") {
			t.Errorf("flagsFirst(%v) = %v", args, got)
		}
		fs := flag.NewFlagSet("usage", flag.ContinueOnError)
		by := fs.String("by", "client", "")
		fs.String("since", "7d", "")
		hub := fs.Bool("hub", false, "")
		if err := fs.Parse(got); err != nil || !*hub || (*by != "client" && *by != "model") {
			t.Errorf("parse(%v): by=%q hub=%v err=%v", args, *by, *hub, err)
		}
	}
}

// started as fleet-engine-stage (the link next to fleet), fleet runs its stage mode
func TestStageInvocation(t *testing.T) {
	if got := stageInvocation("/home/u/.local/bin/fleet-engine-stage", []string{"serve", "-m", "x"}); strings.Join(got, " ") != "chain serve -m x" {
		t.Fatalf("as fleet-engine-stage: %v", got)
	}
	if got := stageInvocation("/home/u/.local/bin/fleet", []string{"status"}); strings.Join(got, " ") != "status" {
		t.Fatalf("as fleet: %v", got)
	}
}

package server

// Feature-verification tests for the router / proxy / admin side of the server.
// Every helper in this file is prefixed with "rt".

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/hostinfo"
)

// ---------------------------------------------------------------- helpers

// rtEnv points every path the server may touch at a temporary directory.
func rtEnv(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	t.Setenv("HOME", d)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(d, "xdg-config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(d, "xdg-state"))
	t.Setenv("FLEET_CONFIG", filepath.Join(d, "config.yaml"))
	t.Setenv("FLEET_STATS", filepath.Join(d, "state", "stats.json"))
	return d
}

// rtUp is a fake model server: OpenAI-style /v1 plus (optionally) Ollama's /api/tags and /api/ps.
type rtUp struct {
	srv    *httptest.Server
	name   string
	mu     sync.Mutex
	status int // non-zero: chat/embeddings answer with this status
	chunks int // SSE content chunks (default 4)
	delay  time.Duration
	gate   chan struct{} // when set, chat requests wait until it is closed
	gOnce  sync.Once
	abort  bool // stream one chunk, then cut the connection
	bodies []map[string]any
	order  []string // first message content of each chat request, in arrival order
	tags   string   // /api/tags reply ("" → 404)
	ps     string   // /api/ps reply ("" → 404)
}

func rtNewUp(t *testing.T, name string) *rtUp {
	u := &rtUp{name: name, chunks: 4}
	u.srv = httptest.NewServer(http.HandlerFunc(u.serve))
	t.Cleanup(func() { u.open(); u.srv.Close() })
	return u
}

func (u *rtUp) URL() string { return u.srv.URL + "/v1" }

func (u *rtUp) open() {
	u.mu.Lock()
	g := u.gate
	u.mu.Unlock()
	if g != nil {
		u.gOnce.Do(func() { close(g) })
	}
}

func (u *rtUp) hold() {
	u.mu.Lock()
	u.gate = make(chan struct{})
	u.gOnce = sync.Once{}
	u.mu.Unlock()
}

func (u *rtUp) hits() int { u.mu.Lock(); defer u.mu.Unlock(); return len(u.bodies) }

func (u *rtUp) lastBody() map[string]any {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.bodies) == 0 {
		return nil
	}
	return u.bodies[len(u.bodies)-1]
}

func (u *rtUp) set(f func(u *rtUp)) { u.mu.Lock(); f(u); u.mu.Unlock() }

// rtSSE is the exact stream a fake upstream sends.
func rtSSE(name string, n int) []string {
	var ev []string
	for i := 0; i < n; i++ {
		ev = append(ev, fmt.Sprintf(`data: {"choices":[{"delta":{"content":"%s-%d "}}]}`+"\n\n", name, i))
	}
	ev = append(ev, `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`+"\n\n")
	ev = append(ev, fmt.Sprintf(`data: {"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":%d}}`+"\n\n", n))
	ev = append(ev, "data: [DONE]\n\n")
	return ev
}

func (u *rtUp) serve(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	tags, ps := u.tags, u.ps
	u.mu.Unlock()
	switch {
	case r.URL.Path == "/api/tags" || r.URL.Path == "/api/ps":
		v := tags
		if r.URL.Path == "/api/ps" {
			v = ps
		}
		if v == "" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, v)
		return
	case r.Method == "GET" && r.URL.Path == "/v1/models":
		fmt.Fprintf(w, `{"object":"list","data":[{"id":"%s"}]}`, u.name)
		return
	}
	var in map[string]any
	json.NewDecoder(r.Body).Decode(&in)
	u.mu.Lock()
	u.bodies = append(u.bodies, in)
	first := ""
	if msgs, _ := in["messages"].([]any); len(msgs) > 0 {
		if m, _ := msgs[0].(map[string]any); m != nil {
			first, _ = m["content"].(string)
		}
	}
	u.order = append(u.order, first)
	status, chunks, delay, gate, abort := u.status, u.chunks, u.delay, u.gate, u.abort
	u.mu.Unlock()
	if status != 0 {
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"error":{"message":"%s says %d"}}`, u.name, status)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/embeddings") {
		n := 1
		if arr, ok := in["input"].([]any); ok {
			n = len(arr)
		}
		var d []string
		for i := 0; i < n; i++ {
			d = append(d, fmt.Sprintf(`{"index":%d,"embedding":[0.%d,0.5,0.25]}`, i, i+1))
		}
		fmt.Fprintf(w, `{"object":"list","data":[%s],"usage":{"prompt_tokens":2}}`, strings.Join(d, ","))
		return
	}
	if gate != nil {
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
	}
	if st, _ := in["stream"].(bool); !st {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"x","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"%s says hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`, u.name)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Upstream", u.name)
	fl := w.(http.Flusher)
	if delay > 0 {
		time.Sleep(delay) // time to first token
	}
	for i, ev := range rtSSE(u.name, chunks) {
		io.WriteString(w, ev)
		fl.Flush()
		if abort && i == 0 {
			panic(http.ErrAbortHandler) // cut the connection mid-stream
		}
		if delay > 0 && i < chunks {
			time.Sleep(delay)
		}
	}
}

// rtServer builds a bare Server around a configuration (no file on disk).
func rtServer(t *testing.T, cfg *config.Config) *Server {
	t.Helper()
	rtEnv(t)
	if cfg.Roles == nil {
		cfg.Roles = map[string]config.Role{}
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("test config invalid: %v", err)
	}
	tr := &http.Transport{}
	t.Cleanup(tr.CloseIdleConnections)
	s := &Server{cfg: cfg, state: map[string]*EpState{}, lims: map[string]*limiter{},
		client: &http.Client{Transport: tr}, tunnels: newTunnels(),
		kick: make(chan struct{}, 1), peerKick: make(chan struct{}, 1)}
	s.syncLimits()
	return s
}

// rtFileServer builds a Server with server.New from a configuration written to a temp file.
func rtFileServer(t *testing.T, cfg *config.Config) (*Server, string) {
	t.Helper()
	d := rtEnv(t)
	path := filepath.Join(d, "config.yaml")
	if cfg.Roles == nil {
		cfg.Roles = map[string]config.Role{}
	}
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	s.syncLimits()
	return s, path
}

type rtMod func(*http.Request)

func rtRemote(r *http.Request) { r.RemoteAddr = "10.0.1.50:1234" }
func rtBearer(k string) rtMod {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+k) }
}
func rtUA(ua string) rtMod          { return func(r *http.Request) { r.Header.Set("User-Agent", ua) } }
func rtCtx(c context.Context) rtMod { return func(r *http.Request) { *r = *r.WithContext(c) } }

// rtDo serves one request through s.Handler() (loopback client unless rtRemote is given).
func rtDo(t *testing.T, s *Server, method, path string, body any, mods ...rtMod) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		rd = strings.NewReader(string(raw))
	}
	req := httptest.NewRequest(method, path, rd)
	req.RemoteAddr = "127.0.0.1:40000"
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, m := range mods {
		m(req)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func rtChat(model string, stream bool) map[string]any {
	return map[string]any{"model": model, "stream": stream, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
}

func rtChatMsg(model, msg string) map[string]any {
	return map[string]any{"model": model, "messages": []any{map[string]any{"role": "user", "content": msg}}}
}

func rtNames(eps []config.Endpoint) string {
	var n []string
	for _, e := range eps {
		n = append(n, e.Name)
	}
	return strings.Join(n, ",")
}

func rtEventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func rtEp(name, node, url, model string) config.Endpoint {
	return config.Endpoint{Name: name, Node: node, URL: url, Model: model}
}

func rtBool(b bool) *bool { return &b }

func rtJSON(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("bad JSON (%d): %v\n%s", rec.Code, err, rec.Body.String())
	}
}

func rtStrain(s *Server, node string, at time.Time) {
	s.strain.set(node, NodeStrain{Strain: hostinfo.Strain{At: at, Reasons: []string{"x"}}}, false)
}

// rt3 is a three-endpoint fleet on three nodes behind one upstream per endpoint.
func rt3(t *testing.T, role config.Role) (*Server, map[string]*rtUp) {
	ups := map[string]*rtUp{"a": rtNewUp(t, "a"), "b": rtNewUp(t, "b"), "c": rtNewUp(t, "c")}
	cfg := &config.Config{
		Endpoints: []config.Endpoint{
			rtEp("a", "n1", ups["a"].URL(), "ma"),
			rtEp("b", "n2", ups["b"].URL(), "mb"),
			rtEp("c", "n3", ups["c"].URL(), "mc"),
		},
		Roles: map[string]config.Role{"r": role},
	}
	return rtServer(t, cfg), ups
}

// ---------------------------------------------------------------- role resolution

func TestRouting_RoleResolution_PreferredThenFallback(t *testing.T) {
	s, ups := rt3(t, config.Role{Preferred: "b", Fallback: []string{"c", "a"}})
	if got := rtNames(s.Resolve("r")); got != "b,c,a" {
		t.Fatalf("Resolve = %s, want b,c,a", got)
	}
	rec := rtDo(t, s, "POST", "/v1/chat/completions", rtChat("r", false))
	if rec.Code != 200 || rec.Header().Get("X-Fleet-Endpoint") != "b" || ups["b"].hits() != 1 {
		t.Fatalf("code %d via %q", rec.Code, rec.Header().Get("X-Fleet-Endpoint"))
	}
	if m := ups["b"].lastBody()["model"]; m != "mb" {
		t.Fatalf("upstream got model %v, want the wire id mb", m)
	}
}

func TestRouting_RoleResolution_UnhealthyPreferredGoesLast(t *testing.T) {
	s, ups := rt3(t, config.Role{Preferred: "a", Fallback: []string{"b"}})
	s.state["a"] = &EpState{OK: false, Err: "down"}
	if got := rtNames(s.Resolve("r")); got != "b,a" {
		t.Fatalf("Resolve = %s, want b,a", got)
	}
	rec := rtDo(t, s, "POST", "/v1/chat/completions", rtChat("r", false))
	if rec.Header().Get("X-Fleet-Endpoint") != "b" || ups["a"].hits() != 0 {
		t.Fatalf("went to %q (a hits %d)", rec.Header().Get("X-Fleet-Endpoint"), ups["a"].hits())
	}
	// every member down: all are still tried, in order
	s.state["b"] = &EpState{OK: false}
	if got := rtNames(s.Resolve("r")); got != "a,b" {
		t.Fatalf("all down: Resolve = %s, want a,b", got)
	}
}

func TestRouting_RoleResolution_MatchSelectors(t *testing.T) {
	up := rtNewUp(t, "x")
	eps := []config.Endpoint{
		{Name: "g1", Node: "dino", URL: up.URL(), Model: "gemma4:27b", Tags: []string{"big"}},
		{Name: "g2", Node: "vec", URL: up.URL(), Model: "gemma4:e4b", Tags: []string{"fast"}},
		{Name: "q", Node: "vec", URL: up.URL(), Model: "qwen3:8b", Tags: []string{"fast"}},
		{Name: "v", Node: "p1", URL: up.URL(), Model: "qwen2.5-vl:7b"},
		{Name: "e", Node: "dino", URL: up.URL(), Model: "nomic-embed-text"},
	}
	cases := map[string]struct {
		role config.Role
		want string
	}{
		"tag":        {config.Role{Match: []string{"tag:fast"}}, "g2,q"},
		"node":       {config.Role{Match: []string{"node:dino"}}, "g1,e"},
		"cap":        {config.Role{Match: []string{"cap:vision"}}, "v"},
		"model-glob": {config.Role{Match: []string{"model:gemma4*"}}, "g1,g2"},
		"pref+match": {config.Role{Preferred: "q", Match: []string{"tag:fast", "node:p1"}}, "q,g2,v"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s := rtServer(t, &config.Config{Endpoints: eps, Roles: map[string]config.Role{"r": c.role}})
			if got := rtNames(s.Resolve("r")); got != c.want {
				t.Fatalf("pool = %s, want %s", got, c.want)
			}
		})
	}
}

func TestRouting_RoleResolution_DirectModelLookup(t *testing.T) {
	up := rtNewUp(t, "x")
	s := rtServer(t, &config.Config{Endpoints: []config.Endpoint{
		rtEp("dino-llama", "dino", up.URL(), "llama3:latest"),
		rtEp("vec-llama", "vec", up.URL(), "llama3"),
		rtEp("vec-qwen", "vec", up.URL(), "qwen3:8b"),
	}})
	for ref, want := range map[string]string{
		"vec-qwen":      "vec-qwen",             // endpoint name
		"vec/qwen3:8b":  "vec-qwen",             // node/model
		"dino/llama3":   "dino-llama",           // node/model, ":latest" optional
		"llama3":        "dino-llama,vec-llama", // bare id: a pool over nodes
		"llama3:latest": "dino-llama,vec-llama",
		"nope":          "",
	} {
		if got := rtNames(s.Resolve(ref)); got != want {
			t.Errorf("Resolve(%q) = %q, want %q", ref, got, want)
		}
	}
	rec := rtDo(t, s, "POST", "/v1/chat/completions", rtChat("vec/qwen3:8b", false))
	if rec.Code != 200 || rec.Header().Get("X-Fleet-Endpoint") != "vec-qwen" {
		t.Fatalf("node/model request: %d via %q", rec.Code, rec.Header().Get("X-Fleet-Endpoint"))
	}
}

func TestRouting_RoleResolution_UnknownModel404(t *testing.T) {
	s, _ := rt3(t, config.Role{Preferred: "a"})
	rec := rtDo(t, s, "POST", "/v1/chat/completions", rtChat("ghost", false))
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), "ghost") {
		t.Fatalf("code %d body %s", rec.Code, rec.Body.String())
	}
	if rec := rtDo(t, s, "POST", "/v1/chat/completions", "{not json"); rec.Code != 400 {
		t.Fatalf("invalid JSON: code %d", rec.Code)
	}
	if e := s.reqs.last(0); len(e) != 2 || e[0].Status != 404 || e[0].Model != "ghost" {
		t.Fatalf("log: %+v", e)
	}
}

// ---------------------------------------------------------------- failover

func rtFailoverCase(t *testing.T, breakA func(s *Server, ups map[string]*rtUp)) (*Server, map[string]*rtUp, *httptest.ResponseRecorder) {
	s, ups := rt3(t, config.Role{Preferred: "a", Fallback: []string{"b"}})
	breakA(s, ups)
	rec := rtDo(t, s, "POST", "/v1/chat/completions", rtChat("r", true))
	return s, ups, rec
}

func rtExpectFailedOver(t *testing.T, s *Server, ups map[string]*rtUp, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != 200 || rec.Header().Get("X-Fleet-Endpoint") != "b" || ups["b"].hits() != 1 {
		t.Fatalf("code %d via %q (b hits %d): %s", rec.Code, rec.Header().Get("X-Fleet-Endpoint"), ups["b"].hits(), rec.Body.String())
	}
	if l := s.reqs.last(1); len(l) != 1 || l[0].Attempts != 2 || l[0].Endpoint != "b" {
		t.Fatalf("log entry: %+v", l)
	}
	if s.healthy("a") {
		t.Fatal("the failed endpoint must be penalized")
	}
}

func TestRouting_Failover_ConnectionError(t *testing.T) {
	s, ups, rec := rtFailoverCase(t, func(s *Server, ups map[string]*rtUp) {
		dead := httptest.NewServer(http.NotFoundHandler())
		dead.Close()
		cfg := s.Config()
		cfg.Endpoints = append([]config.Endpoint{}, cfg.Endpoints...)
		cfg.Endpoints[0].URL = dead.URL + "/v1"
		s.cfg = cfg
	})
	rtExpectFailedOver(t, s, ups, rec)
}

func TestRouting_Failover_5xx(t *testing.T) {
	s, ups, rec := rtFailoverCase(t, func(_ *Server, ups map[string]*rtUp) { ups["a"].set(func(u *rtUp) { u.status = 503 }) })
	rtExpectFailedOver(t, s, ups, rec)
	if ups["a"].hits() != 1 {
		t.Fatalf("a hits %d", ups["a"].hits())
	}
}

func TestRouting_Failover_429(t *testing.T) {
	s, ups, rec := rtFailoverCase(t, func(_ *Server, ups map[string]*rtUp) { ups["a"].set(func(u *rtUp) { u.status = 429 }) })
	rtExpectFailedOver(t, s, ups, rec)
}

func TestRouting_Failover_4xxPassedThrough(t *testing.T) {
	_, ups, rec := rtFailoverCase(t, func(_ *Server, ups map[string]*rtUp) { ups["a"].set(func(u *rtUp) { u.status = 400 }) })
	if rec.Code != 400 || ups["b"].hits() != 0 || !strings.Contains(rec.Body.String(), "a says 400") {
		t.Fatalf("code %d, b hits %d, body %s", rec.Code, ups["b"].hits(), rec.Body.String())
	}
	if rec.Header().Get("X-Fleet-Endpoint") != "a" {
		t.Fatalf("X-Fleet-Endpoint %q", rec.Header().Get("X-Fleet-Endpoint"))
	}
}

func TestRouting_Failover_NotAfterFirstByte(t *testing.T) {
	_, ups, rec := rtFailoverCase(t, func(_ *Server, ups map[string]*rtUp) { ups["a"].set(func(u *rtUp) { u.abort = true }) })
	if rec.Code != 200 || ups["b"].hits() != 0 || rec.Header().Get("X-Fleet-Endpoint") != "a" {
		t.Fatalf("a stream cut after the first byte must not fail over: code %d b hits %d", rec.Code, ups["b"].hits())
	}
	if !strings.Contains(rec.Body.String(), "a-0") || strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("body: %q", rec.Body.String())
	}
}

func TestRouting_Failover_PenalizeOrdersLast(t *testing.T) {
	s, ups := rt3(t, config.Role{Preferred: "a", Fallback: []string{"b"}})
	ups["a"].set(func(u *rtUp) { u.status = 500 })
	rtDo(t, s, "POST", "/v1/chat/completions", rtChat("r", false))
	if got := rtNames(s.Resolve("r")); got != "b,a" {
		t.Fatalf("after a failure Resolve = %s, want b,a", got)
	}
	st := s.getState("a")
	if st == nil || time.Until(st.penalty) < 15*time.Second || time.Until(st.penalty) > 21*time.Second {
		t.Fatalf("penalty: %+v", st)
	}
	// the next request goes straight to b (a is not tried again)
	ups["a"].set(func(u *rtUp) { u.status = 0 })
	rtDo(t, s, "POST", "/v1/chat/completions", rtChat("r", false))
	if ups["a"].hits() != 1 || ups["b"].hits() != 2 {
		t.Fatalf("a %d b %d", ups["a"].hits(), ups["b"].hits())
	}
	// once the penalty expires the order is back
	s.mu.Lock()
	s.state["a"].penalty = time.Now().Add(-time.Second)
	s.mu.Unlock()
	if got := rtNames(s.Resolve("r")); got != "a,b" {
		t.Fatalf("after the penalty Resolve = %s", got)
	}
	// penalize on an endpoint without state creates a healthy-but-penalized entry
	s.penalize("c")
	if s.healthy("c") || !s.getState("c").OK {
		t.Fatal("penalize without state")
	}
}

func TestRouting_Failover_AllCandidatesFailed502(t *testing.T) {
	s, ups := rt3(t, config.Role{Preferred: "a", Fallback: []string{"b"}})
	ups["a"].set(func(u *rtUp) { u.status = 500 })
	ups["b"].set(func(u *rtUp) { u.status = 502 })
	rec := rtDo(t, s, "POST", "/v1/chat/completions", rtChat("r", false))
	if rec.Code != 502 || !strings.Contains(rec.Body.String(), "all candidates failed") || !strings.Contains(rec.Body.String(), "b: http 502") {
		t.Fatalf("code %d body %s", rec.Code, rec.Body.String())
	}
	if l := s.reqs.last(1)[0]; l.Attempts != 2 || l.Status != 502 || l.Err == "" {
		t.Fatalf("log %+v", l)
	}
	if lv, _ := s.live.get("a"); lv.Errors != 1 {
		t.Fatalf("errors counted: %+v", lv)
	}
}

// ---------------------------------------------------------------- strategies

func TestRouting_Strategy_FailoverKeepsOrder(t *testing.T) {
	s, ups := rt3(t, config.Role{Preferred: "a", Fallback: []string{"b", "c"}})
	for i := 0; i < 4; i++ {
		rtDo(t, s, "POST", "/v1/chat/completions", rtChat("r", false))
	}
	if ups["a"].hits() != 4 {
		t.Fatalf("failover sends everything to the first healthy member: a %d", ups["a"].hits())
	}
}

func TestRouting_Strategy_RoundRobinRotates(t *testing.T) {
	s, _ := rt3(t, config.Role{Preferred: "a", Fallback: []string{"b", "c"}, Strategy: "round-robin"})
	var got []string
	for i := 0; i < 6; i++ {
		got = append(got, rtDo(t, s, "POST", "/v1/chat/completions", rtChat("r", false)).Header().Get("X-Fleet-Endpoint"))
	}
	if strings.Join(got, ",") != "a,b,c,a,b,c" {
		t.Fatalf("rotation %v", got)
	}
	// an unhealthy member drops out of the rotation
	s.state["b"] = &EpState{OK: false}
	seen := map[string]int{}
	for i := 0; i < 4; i++ {
		seen[rtDo(t, s, "POST", "/v1/chat/completions", rtChat("r", false)).Header().Get("X-Fleet-Endpoint")]++
	}
	if seen["b"] != 0 || seen["a"] != 2 || seen["c"] != 2 {
		t.Fatalf("with b down: %v", seen)
	}
}

func TestRouting_Strategy_LeastBusy(t *testing.T) {
	s, _ := rt3(t, config.Role{Preferred: "a", Fallback: []string{"b"}, Strategy: "least-busy"})
	s.lim("ep:a").try() // a has one request in flight
	if got := rtDo(t, s, "POST", "/v1/chat/completions", rtChat("r", false)).Header().Get("X-Fleet-Endpoint"); got != "b" {
		t.Fatalf("least-busy picked %q, want b", got)
	}
	s.lim("ep:a").release()
	s.lim("ep:b").try()
	s.lim("ep:b").try()
	if got := rtDo(t, s, "POST", "/v1/chat/completions", rtChat("r", false)).Header().Get("X-Fleet-Endpoint"); got != "a" {
		t.Fatalf("least-busy picked %q, want a", got)
	}
}

func TestRouting_Strategy_LeastBusyRelativeToSlots(t *testing.T) {
	s, _ := rt3(t, config.Role{Preferred: "a", Fallback: []string{"b"}, Strategy: "least-busy"})
	cfg := s.Config()
	cfg.Endpoints = append([]config.Endpoint{}, cfg.Endpoints...)
	cfg.Endpoints[0].Slots = 8 // a: 2/8 busy
	cfg.Endpoints[1].Slots = 2 // b: 1/2 busy
	s.cfg = cfg
	s.syncLimits()
	s.lim("ep:a").try()
	s.lim("ep:a").try()
	s.lim("ep:b").try()
	if got := rtDo(t, s, "POST", "/v1/chat/completions", rtChat("r", false)).Header().Get("X-Fleet-Endpoint"); got != "a" {
		t.Fatalf("load is relative to slots: picked %q, want a", got)
	}
}

func TestRouting_Strategy_FastestUsesBenchThenLive(t *testing.T) {
	s, _ := rt3(t, config.Role{Preferred: "a", Fallback: []string{"c", "b"}, Strategy: "fastest"})
	cfg := s.Config()
	cfg.Endpoints = append([]config.Endpoint{}, cfg.Endpoints...)
	cfg.Endpoints[0].TPS = 10  // a: slow (bench)
	cfg.Endpoints[1].TPS = 100 // b: fast (bench); c: unknown
	s.cfg = cfg
	_, role, rc := s.route("r")
	if got := rtNames(s.applyStrategy(role, rc, s.Resolve("r"))); got != "b,a,c" {
		t.Fatalf("fastest order %s, want b,a,c (unknown speed last)", got)
	}
	if got := rtDo(t, s, "POST", "/v1/chat/completions", rtChat("r", false)).Header().Get("X-Fleet-Endpoint"); got != "b" {
		t.Fatalf("fastest picked %q", got)
	}
	// live figures replace the benchmark: a is now measured at 1000 tok/s
	s.live.record("a", "ma", obsResult{TPS: 1000, DecTok: 1000, DecSec: 1, TTFT: 10 * time.Millisecond, Out: 1001})
	if got := rtDo(t, s, "POST", "/v1/chat/completions", rtChat("r", false)).Header().Get("X-Fleet-Endpoint"); got != "a" {
		t.Fatalf("with live tps fastest picked %q, want a", got)
	}
}

func TestRouting_Strategy_OverflowSpillsWhenPreferredFull(t *testing.T) {
	s, ups := rt3(t, config.Role{Preferred: "a", Fallback: []string{"b"}, Overflow: true})
	cfg := s.Config()
	cfg.Endpoints = append([]config.Endpoint{}, cfg.Endpoints...)
	cfg.Endpoints[0].Slots = 1
	s.cfg = cfg
	s.syncLimits()
	ups["a"].hold()
	done := make(chan string, 1)
	go func() {
		done <- rtDo(t, s, "POST", "/v1/chat/completions", rtChat("r", false)).Header().Get("X-Fleet-Endpoint")
	}()
	rtEventually(t, "first request at a", func() bool { return ups["a"].hits() == 1 })
	if got := rtDo(t, s, "POST", "/v1/chat/completions", rtChat("r", false)).Header().Get("X-Fleet-Endpoint"); got != "b" {
		t.Fatalf("overflow: second request went to %q, want b", got)
	}
	ups["a"].open()
	if got := <-done; got != "a" {
		t.Fatalf("first request %q", got)
	}
}

func TestRouting_Strategy_NoOverflowWaitsForSlot(t *testing.T) {
	s, ups := rt3(t, config.Role{Preferred: "a", Fallback: []string{"b"}})
	cfg := s.Config()
	cfg.Endpoints = append([]config.Endpoint{}, cfg.Endpoints...)
	cfg.Endpoints[0].Slots = 1
	s.cfg = cfg
	s.syncLimits()
	ups["a"].hold()
	var wg sync.WaitGroup
	got := make([]string, 2)
	for i := 0; i < 2; i++ {
		if i == 1 {
			rtEventually(t, "first request at a", func() bool { return ups["a"].hits() == 1 })
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i] = rtDo(t, s, "POST", "/v1/chat/completions", rtChat("r", false)).Header().Get("X-Fleet-Endpoint")
		}(i)
	}
	rtEventually(t, "second request queued on a", func() bool { _, w, _ := s.lim("ep:a").stats(); return w == 1 })
	if ups["b"].hits() != 0 || ups["a"].hits() != 1 {
		t.Fatalf("without overflow the request must wait: a %d b %d", ups["a"].hits(), ups["b"].hits())
	}
	if st := s.Status(); st.Endpoints[0].InUse != 1 || st.Endpoints[0].Waiting != 1 || st.Roles["r"].Active != "a" {
		t.Fatalf("status in_use/waiting: %+v", st.Endpoints[0])
	}
	ups["a"].open()
	wg.Wait()
	if got[0] != "a" || got[1] != "a" {
		t.Fatalf("got %v", got)
	}
}

func TestRouting_Strategy_PooledStrategySpillsWithoutOverflowFlag(t *testing.T) {
	s, ups := rt3(t, config.Role{Preferred: "a", Fallback: []string{"b"}, Strategy: "round-robin"})
	cfg := s.Config()
	cfg.Endpoints = append([]config.Endpoint{}, cfg.Endpoints...)
	cfg.Endpoints[0].Slots, cfg.Endpoints[1].Slots = 1, 1
	s.cfg = cfg
	s.syncLimits()
	s.lim("ep:a").try() // a full
	for i := 0; i < 2; i++ {
		if got := rtDo(t, s, "POST", "/v1/chat/completions", rtChat("r", false)).Header().Get("X-Fleet-Endpoint"); got != "b" {
			t.Fatalf("round %d: %q, want b (a is full)", i, got)
		}
	}
	if ups["a"].hits() != 0 {
		t.Fatal("a was full")
	}
}

func TestRouting_Strategy_RoleMaxParallelQueues(t *testing.T) {
	s, ups := rt3(t, config.Role{Preferred: "a", MaxParallel: 1})
	ups["a"].hold()
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		if i == 1 {
			rtEventually(t, "first at a", func() bool { return ups["a"].hits() == 1 })
		}
		wg.Add(1)
		go func() { defer wg.Done(); rtDo(t, s, "POST", "/v1/chat/completions", rtChat("r", false)) }()
	}
	rtEventually(t, "second queued in the role", func() bool { _, w, _ := s.lim("role:r").stats(); return w == 1 })
	if ups["a"].hits() != 1 {
		t.Fatalf("max_parallel 1 let %d through", ups["a"].hits())
	}
	if rs := s.Status().Roles["r"]; rs.InUse != 1 || rs.Waiting != 1 {
		t.Fatalf("role status %+v", rs)
	}
	ups["a"].open()
	wg.Wait()
	if ups["a"].hits() != 2 {
		t.Fatalf("hits %d", ups["a"].hits())
	}
	if l := s.reqs.last(2); l[1].QueueMS < 0 {
		t.Fatalf("queue ms %+v", l)
	}
}

func TestRouting_Strategy_PriorityAcrossRoles(t *testing.T) {
	up := rtNewUp(t, "a")
	s := rtServer(t, &config.Config{
		Endpoints: []config.Endpoint{{Name: "a", Node: "n", URL: up.URL(), Model: "m", Slots: 1}},
		Roles:     map[string]config.Role{"lo": {Preferred: "a"}, "hi": {Preferred: "a", Priority: 10}},
	})
	l := s.lim("ep:a")
	l.try() // the only slot is taken
	var wg sync.WaitGroup
	for _, role := range []string{"lo", "hi"} {
		wg.Add(1)
		go func() { defer wg.Done(); rtDo(t, s, "POST", "/v1/chat/completions", rtChatMsg(role, role)) }()
		want := 1
		if role == "hi" {
			want = 2
		}
		rtEventually(t, role+" queued", func() bool { _, w, _ := l.stats(); return w == want })
	}
	l.release()
	wg.Wait()
	up.mu.Lock()
	order := strings.Join(up.order, ",")
	up.mu.Unlock()
	if order != "hi,lo" {
		t.Fatalf("served in order %s, want hi,lo (higher priority first)", order)
	}
}

func TestRouting_Strategy_QueueGivesUpWhenClientLeaves(t *testing.T) {
	s, _ := rt3(t, config.Role{Preferred: "a"})
	cfg := s.Config()
	cfg.Endpoints = append([]config.Endpoint{}, cfg.Endpoints...)
	cfg.Endpoints[0].Slots = 1
	s.cfg = cfg
	s.syncLimits()
	s.lim("ep:a").try()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	rec := rtDo(t, s, "POST", "/v1/chat/completions", rtChat("r", false), rtCtx(ctx))
	if rec.Code != 503 {
		t.Fatalf("code %d", rec.Code)
	}
	if _, w, _ := s.lim("ep:a").stats(); w != 0 {
		t.Fatal("the waiter must leave the queue")
	}
}

// ---------------------------------------------------------------- capabilities, embeddings, context

func TestRouting_Caps_ImageNeedsVision422(t *testing.T) {
	s, ups := rt3(t, config.Role{Preferred: "a", Fallback: []string{"b"}})
	body := map[string]any{"model": "r", "messages": []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "what is this"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}},
	}}}}
	rec := rtDo(t, s, "POST", "/v1/chat/completions", body)
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "vision") {
		t.Fatalf("code %d body %s", rec.Code, rec.Body.String())
	}
	if ups["a"].hits()+ups["b"].hits() != 0 {
		t.Fatal("nothing must be sent")
	}
	// give b vision: the request now goes there even though a is preferred
	cfg := s.Config()
	cfg.Endpoints = append([]config.Endpoint{}, cfg.Endpoints...)
	cfg.Endpoints[1].Caps = []string{"chat", "vision"}
	s.cfg = cfg
	if rec := rtDo(t, s, "POST", "/v1/chat/completions", body); rec.Code != 200 || rec.Header().Get("X-Fleet-Endpoint") != "b" {
		t.Fatalf("vision request: %d via %q", rec.Code, rec.Header().Get("X-Fleet-Endpoint"))
	}
}

func TestRouting_Caps_EmbeddingsGoToEmbedEndpoint(t *testing.T) {
	chat, emb := rtNewUp(t, "chat"), rtNewUp(t, "emb")
	s := rtServer(t, &config.Config{
		Endpoints: []config.Endpoint{rtEp("c", "n", chat.URL(), "qwen3:8b"), rtEp("e", "n", emb.URL(), "nomic-embed-text")},
		Roles:     map[string]config.Role{"mixed": {Preferred: "c", Fallback: []string{"e"}}, "chatonly": {Preferred: "c"}},
	})
	rec := rtDo(t, s, "POST", "/v1/embeddings", map[string]any{"model": "mixed", "input": "hello"})
	if rec.Code != 200 || rec.Header().Get("X-Fleet-Endpoint") != "e" || chat.hits() != 0 {
		t.Fatalf("embeddings: %d via %q", rec.Code, rec.Header().Get("X-Fleet-Endpoint"))
	}
	// and chat skips the embedding model
	if rec := rtDo(t, s, "POST", "/v1/chat/completions", rtChat("mixed", false)); rec.Header().Get("X-Fleet-Endpoint") != "c" {
		t.Fatalf("chat via %q", rec.Header().Get("X-Fleet-Endpoint"))
	}
	if rec := rtDo(t, s, "POST", "/v1/embeddings", map[string]any{"model": "chatonly", "input": "x"}); rec.Code != 422 {
		t.Fatalf("no embed endpoint: %d", rec.Code)
	}
}

func TestRouting_Caps_ContextSizeFilter(t *testing.T) {
	s, ups := rt3(t, config.Role{Preferred: "a", Fallback: []string{"b"}})
	cfg := s.Config()
	cfg.Endpoints = append([]config.Endpoint{}, cfg.Endpoints...)
	cfg.Endpoints[0].Ctx = 2048
	cfg.Endpoints[1].Ctx = 131072
	s.cfg = cfg
	big := rtChatMsg("r", strings.Repeat("word ", 4000)) // ≈ 5000 tokens
	if rec := rtDo(t, s, "POST", "/v1/chat/completions", big); rec.Header().Get("X-Fleet-Endpoint") != "b" {
		t.Fatalf("a large prompt went to %q", rec.Header().Get("X-Fleet-Endpoint"))
	}
	if rec := rtDo(t, s, "POST", "/v1/chat/completions", rtChat("r", false)); rec.Header().Get("X-Fleet-Endpoint") != "a" {
		t.Fatalf("a small prompt went to %q", rec.Header().Get("X-Fleet-Endpoint"))
	}
	// max_tokens counts too
	b := rtChat("r", false)
	b["max_tokens"] = 4000.0
	if rec := rtDo(t, s, "POST", "/v1/chat/completions", b); rec.Header().Get("X-Fleet-Endpoint") != "b" {
		t.Fatalf("max_tokens 4000 went to %q", rec.Header().Get("X-Fleet-Endpoint"))
	}
	if ups["a"].hits() != 1 {
		t.Fatal("a hits")
	}
}

// ---------------------------------------------------------------- streaming and live stats

func TestRouting_Stream_PassthroughIntegrityAndLiveStats(t *testing.T) {
	s, ups := rt3(t, config.Role{Preferred: "a"})
	ups["a"].set(func(u *rtUp) { u.chunks, u.delay = 12, 25*time.Millisecond })
	front := httptest.NewServer(s.Handler())
	defer front.Close()
	req, _ := http.NewRequest("POST", front.URL+"/v1/chat/completions", strings.NewReader(`{"model":"r","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("User-Agent", "opencode/1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("X-Fleet-Endpoint") != "a" || resp.Header.Get("X-Upstream") != "a" || !strings.Contains(resp.Header.Get("Content-Type"), "event-stream") {
		t.Fatalf("headers %v", resp.Header)
	}
	// the first chunk arrives before the upstream has finished (no buffering)
	br := bufio.NewReader(resp.Body)
	t0 := time.Now()
	line, err := br.ReadString('\n')
	if err != nil || !strings.Contains(line, "a-0") || time.Since(t0) > 200*time.Millisecond {
		t.Fatalf("first line %q after %v (%v)", line, time.Since(t0), err)
	}
	rest, _ := io.ReadAll(br)
	if got, want := line+string(rest), strings.Join(rtSSE("a", 12), ""); got != want {
		t.Fatalf("stream changed in transit:\n got %q\nwant %q", got, want)
	}
	lv, ok := s.live.get("a")
	if !ok || lv.Samples != 1 || lv.TPS <= 0 || lv.TTFTMS <= 0 || lv.TokensOut != 12 || lv.TokensIn != 11 || lv.Requests != 1 {
		t.Fatalf("live stats: %+v", lv)
	}
	l := s.reqs.last(1)[0]
	if l.TPS <= 0 || l.Out != 12 || l.In != 11 || l.Client != "opencode" || l.Role != "r" || l.Endpoint != "a" || l.Status != 200 {
		t.Fatalf("log entry %+v", l)
	}
	if es := s.Status().Endpoints[0]; es.TPSSrc != "live" || es.TPS <= 0 || es.Samples != 1 {
		t.Fatalf("status figures %+v", es)
	}
}

func TestRouting_Stream_NonStreamUsageRecorded(t *testing.T) {
	s, _ := rt3(t, config.Role{Preferred: "a"})
	rec := rtDo(t, s, "POST", "/v1/chat/completions", rtChat("r", false))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "a says hi") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if l := s.reqs.last(1)[0]; l.In != 5 || l.Out != 3 || l.TPS != 0 {
		t.Fatalf("non-stream usage %+v", l)
	}
	if lv, _ := s.live.get("a"); lv.Samples != 0 || lv.TokensOut != 3 {
		t.Fatalf("a non-stream reply must not produce a speed sample: %+v", lv)
	}
}

func TestRouting_Stream_EndpointBodyDefaults(t *testing.T) {
	s, ups := rt3(t, config.Role{Preferred: "a"})
	cfg := s.Config()
	cfg.Endpoints = append([]config.Endpoint{}, cfg.Endpoints...)
	cfg.Endpoints[0].Body = map[string]any{"think": false, "temperature": 0.1}
	s.cfg = cfg
	b := rtChat("r", false)
	b["temperature"] = 0.9
	rtDo(t, s, "POST", "/v1/chat/completions", b)
	got := ups["a"].lastBody()
	if got["think"] != false || got["temperature"] != 0.9 {
		t.Fatalf("body defaults: %v", got)
	}
}

// ---------------------------------------------------------------- /v1/models

func TestRouting_Models_List(t *testing.T) {
	up := rtNewUp(t, "x")
	s := rtServer(t, &config.Config{
		Endpoints: []config.Endpoint{
			{Name: "q", Node: "vec", URL: up.URL(), Model: "qwen3:8b", Tags: []string{"fast"}},
			{Name: "e", Node: "dino", URL: up.URL(), Model: "bge-m3"},
		},
		Roles: map[string]config.Role{"zeta": {Preferred: "q"}, "alpha": {Preferred: "q"}},
	})
	s.state["e"] = &EpState{OK: false}
	rec := rtDo(t, s, "GET", "/v1/models", nil)
	var raw struct {
		Object string           `json:"object"`
		Data   []map[string]any `json:"data"`
	}
	rtJSON(t, rec, &raw)
	var ids []string
	for _, d := range raw.Data {
		ids = append(ids, d["id"].(string))
	}
	if raw.Object != "list" || strings.Join(ids, ",") != "alpha,zeta,vec/qwen3:8b,dino/bge-m3" {
		t.Fatalf("models %v", ids)
	}
	if raw.Data[0]["owned_by"] != "fleet-role" || raw.Data[2]["owned_by"] != "vec" || raw.Data[2]["up"] != true || raw.Data[3]["up"] != false {
		t.Fatalf("fields %v", raw.Data)
	}
	if caps := fmt.Sprint(raw.Data[3]["caps"]); caps != "[embed]" {
		t.Fatalf("embed caps %s", caps)
	}
	if tags := fmt.Sprint(raw.Data[2]["tags"]); tags != "[fast]" {
		t.Fatalf("tags %s", tags)
	}
}

// ---------------------------------------------------------------- Ollama-compatible API

func rtOllama(t *testing.T) (*Server, *rtUp, *rtUp) {
	chat, emb := rtNewUp(t, "chat"), rtNewUp(t, "emb")
	s := rtServer(t, &config.Config{
		Endpoints: []config.Endpoint{rtEp("m1", "n", chat.URL(), "real"), rtEp("emb", "n", emb.URL(), "nomic-embed-text")},
		Roles:     map[string]config.Role{"scout": {Preferred: "m1"}, "vec": {Preferred: "emb"}},
	})
	return s, chat, emb
}

func TestRouting_Ollama_TagsAndVersion(t *testing.T) {
	s, _, _ := rtOllama(t)
	var v map[string]any
	rtJSON(t, rtDo(t, s, "GET", "/api/version", nil), &v)
	if v["version"] != ollamaVersion {
		t.Fatalf("version %v", v)
	}
	if rec := rtDo(t, s, "GET", "/", nil); !strings.Contains(rec.Body.String(), "Ollama is running") {
		t.Fatalf("root %q", rec.Body.String())
	}
	var tags struct {
		Models []struct {
			Name, Model, Digest string
			Details             struct{ Family, Format string }
		}
	}
	rtJSON(t, rtDo(t, s, "GET", "/api/tags", nil), &tags)
	var names []string
	for _, m := range tags.Models {
		names = append(names, m.Name+"("+m.Details.Format+")")
		if len(m.Digest) != 64 {
			t.Fatalf("digest %q", m.Digest)
		}
	}
	if strings.Join(names, ",") != "scout(role),vec(role),m1(endpoint),emb(endpoint)" {
		t.Fatalf("tags %v", names)
	}
}

func TestRouting_Ollama_ChatStreamNDJSON(t *testing.T) {
	s, _, _ := rtOllama(t)
	rec := rtDo(t, s, "POST", "/api/chat", map[string]any{"model": "scout:latest", "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/x-ndjson" || rec.Header().Get("X-Fleet-Endpoint") != "m1" {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
	if len(lines) != 5 { // 4 content pieces + done
		t.Fatalf("lines %d: %s", len(lines), rec.Body.String())
	}
	var text strings.Builder
	for i, l := range lines {
		var v map[string]any
		if err := json.Unmarshal([]byte(l), &v); err != nil {
			t.Fatalf("line %d not JSON: %q", i, l)
		}
		if v["model"] != "scout:latest" {
			t.Fatalf("model %v", v["model"])
		}
		m, _ := v["message"].(map[string]any)
		text.WriteString(fmt.Sprint(m["content"]))
		if last := i == len(lines)-1; v["done"] != last {
			t.Fatalf("line %d done=%v", i, v["done"])
		} else if last && (v["done_reason"] != "stop" || v["eval_count"] != 4.0 || v["prompt_eval_count"] != 11.0) {
			t.Fatalf("final %v", v)
		}
	}
	if text.String() != "chat-0 chat-1 chat-2 chat-3 " {
		t.Fatalf("text %q", text.String())
	}
	if l := s.reqs.last(1)[0]; l.Via != "ollama" || l.Role != "scout" {
		t.Fatalf("log %+v", l)
	}
}

func TestRouting_Ollama_ChatNonStream(t *testing.T) {
	s, _, _ := rtOllama(t)
	rec := rtDo(t, s, "POST", "/api/chat", map[string]any{"model": "scout", "stream": false, "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	var v map[string]any
	rtJSON(t, rec, &v)
	m, _ := v["message"].(map[string]any)
	if v["done"] != true || m["content"] != "chat-0 chat-1 chat-2 chat-3 " || m["role"] != "assistant" || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("%v", v)
	}
}

func TestRouting_Ollama_GenerateMapsSystemAndPrompt(t *testing.T) {
	s, chat, _ := rtOllama(t)
	rec := rtDo(t, s, "POST", "/api/generate", map[string]any{"model": "m1", "system": "be brief", "prompt": "why", "stream": false})
	var v map[string]any
	rtJSON(t, rec, &v)
	if v["response"] != "chat-0 chat-1 chat-2 chat-3 " || v["done"] != true {
		t.Fatalf("%v", v)
	}
	msgs, _ := chat.lastBody()["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["role"] != "system" || msgs[1].(map[string]any)["content"] != "why" {
		t.Fatalf("messages %v", msgs)
	}
}

func TestRouting_Ollama_OptionsAndFormatMapping(t *testing.T) {
	s, chat, _ := rtOllama(t)
	rtDo(t, s, "POST", "/api/chat", map[string]any{"model": "scout", "stream": false, "format": "json",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"options":  map[string]any{"temperature": 0.2, "num_predict": 64, "stop": []any{"END"}, "seed": 7, "top_p": 0.9, "num_ctx": 8192}})
	b := chat.lastBody()
	rf, _ := b["response_format"].(map[string]any)
	if b["temperature"] != 0.2 || b["max_tokens"] != 64.0 || fmt.Sprint(b["stop"]) != "[END]" || b["seed"] != 7.0 || b["top_p"] != 0.9 ||
		rf["type"] != "json_object" || b["stream"] != true || b["model"] != "real" {
		t.Fatalf("translated body %v", b)
	}
	if _, ok := b["num_ctx"]; ok {
		t.Fatal("num_ctx is not an OpenAI field")
	}
	// num_predict -1 means "no limit"; a schema format becomes json_schema
	rtDo(t, s, "POST", "/api/chat", map[string]any{"model": "scout", "stream": false, "format": map[string]any{"type": "object"},
		"messages": []any{map[string]any{"role": "user", "content": "hi"}}, "options": map[string]any{"num_predict": -1}})
	b = chat.lastBody()
	rf, _ = b["response_format"].(map[string]any)
	if _, ok := b["max_tokens"]; ok || rf["type"] != "json_schema" {
		t.Fatalf("body %v", b)
	}
}

func TestRouting_Ollama_ErrorsAndVision(t *testing.T) {
	s, chat, _ := rtOllama(t)
	rec := rtDo(t, s, "POST", "/api/chat", map[string]any{"model": "ghost", "messages": []any{}})
	var v map[string]any
	rtJSON(t, rec, &v)
	if rec.Code != 404 || !strings.Contains(fmt.Sprint(v["error"]), "ghost") {
		t.Fatalf("%d %v", rec.Code, v)
	}
	rec = rtDo(t, s, "POST", "/api/chat", map[string]any{"model": "scout", "messages": []any{map[string]any{"role": "user", "content": "see", "images": []any{"AAAA"}}}})
	rtJSON(t, rec, &v)
	if rec.Code != 422 || !strings.Contains(fmt.Sprint(v["error"]), "vision") {
		t.Fatalf("images without a vision model: %d %v", rec.Code, v)
	}
	chat.set(func(u *rtUp) { u.status = 500 })
	rec = rtDo(t, s, "POST", "/api/chat", map[string]any{"model": "scout", "stream": false, "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	rtJSON(t, rec, &v)
	if rec.Code != 502 || !strings.Contains(fmt.Sprint(v["error"]), "all candidates failed") {
		t.Fatalf("backend down: %d %v", rec.Code, v)
	}
	if rec := rtDo(t, s, "POST", "/api/chat", "{bad"); rec.Code != 400 {
		t.Fatalf("bad json %d", rec.Code)
	}
}

func TestRouting_Ollama_EmbedAndLegacyEmbeddings(t *testing.T) {
	s, chat, emb := rtOllama(t)
	var v struct {
		Model      string      `json:"model"`
		Embeddings [][]float64 `json:"embeddings"`
		Embedding  []float64   `json:"embedding"`
	}
	rtJSON(t, rtDo(t, s, "POST", "/api/embed", map[string]any{"model": "vec", "input": []any{"a", "b"}}), &v)
	if v.Model != "vec" || len(v.Embeddings) != 2 || len(v.Embeddings[1]) != 3 || v.Embeddings[1][0] != 0.2 {
		t.Fatalf("embed %+v", v)
	}
	if in := emb.lastBody()["input"]; fmt.Sprint(in) != "[a b]" {
		t.Fatalf("input forwarded %v", in)
	}
	v.Embedding = nil
	rtJSON(t, rtDo(t, s, "POST", "/api/embeddings", map[string]any{"model": "emb", "prompt": "hello"}), &v)
	if len(v.Embedding) != 3 || emb.lastBody()["input"] != "hello" {
		t.Fatalf("legacy %+v / %v", v, emb.lastBody())
	}
	// a chat-only role cannot embed
	rec := rtDo(t, s, "POST", "/api/embed", map[string]any{"model": "scout", "input": "x"})
	if rec.Code != 422 || chat.hits() != 0 {
		t.Fatalf("chat role embed: %d", rec.Code)
	}
}

// ---------------------------------------------------------------- auth

func rtAuthServer(t *testing.T) (*Server, *rtUp) {
	up := rtNewUp(t, "a")
	s := rtServer(t, &config.Config{
		APIKeyEnv: "RT_FLEET_KEY",
		Endpoints: []config.Endpoint{rtEp("a", "n", up.URL(), "m")},
		Roles:     map[string]config.Role{"r": {Preferred: "a"}},
	})
	t.Setenv("RT_FLEET_KEY", "s3cret")
	return s, up
}

func TestRouting_Auth_LoopbackExempt(t *testing.T) {
	s, _ := rtAuthServer(t)
	if rec := rtDo(t, s, "POST", "/v1/chat/completions", rtChat("r", false)); rec.Code != 200 {
		t.Fatalf("loopback: %d", rec.Code)
	}
	if rec := rtDo(t, s, "GET", "/v1/models", nil); rec.Code != 200 {
		t.Fatalf("loopback models: %d", rec.Code)
	}
}

func TestRouting_Auth_RemoteNeedsBearer(t *testing.T) {
	s, up := rtAuthServer(t)
	for name, mods := range map[string][]rtMod{
		"none":  {rtRemote},
		"wrong": {rtRemote, rtBearer("nope")},
		"basic": {rtRemote, func(r *http.Request) { r.Header.Set("Authorization", "Basic s3cret") }},
	} {
		rec := rtDo(t, s, "POST", "/v1/chat/completions", rtChat("r", false), mods...)
		if rec.Code != 401 || rec.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Fatalf("%s: %d", name, rec.Code)
		}
		if rec := rtDo(t, s, "GET", "/api/tags", nil, mods...); rec.Code != 401 {
			t.Fatalf("%s /api/tags: %d", name, rec.Code)
		}
	}
	if up.hits() != 0 {
		t.Fatal("unauthenticated request reached the model")
	}
	if rec := rtDo(t, s, "POST", "/v1/chat/completions", rtChat("r", false), rtRemote, rtBearer("s3cret")); rec.Code != 200 {
		t.Fatalf("right key: %d", rec.Code)
	}
	if l := s.reqs.last(1)[0]; !l.Remote {
		t.Fatalf("remote flag %+v", l)
	}
	// open endpoints stay open
	if rec := rtDo(t, s, "GET", "/healthz", nil, rtRemote); rec.Code != 200 {
		t.Fatalf("healthz %d", rec.Code)
	}
	if rec := rtDo(t, s, "GET", "/api/version", nil, rtRemote); rec.Code != 200 {
		t.Fatalf("version %d", rec.Code)
	}
}

func TestRouting_Auth_PairedKeyAccepted(t *testing.T) {
	s, _ := rtAuthServer(t)
	h := sha256.Sum256([]byte("pair-key-123"))
	cfg := s.Config()
	cfg.Paired = []config.Paired{{ID: "f1", Name: "other", KeyHash: hex.EncodeToString(h[:])}}
	s.cfg = cfg
	if rec := rtDo(t, s, "POST", "/v1/chat/completions", rtChat("r", false), rtRemote, rtBearer("pair-key-123")); rec.Code != 200 {
		t.Fatalf("paired key: %d", rec.Code)
	}
	if rec := rtDo(t, s, "POST", "/v1/chat/completions", rtChat("r", false), rtRemote, rtBearer("pair-key-124")); rec.Code != 401 {
		t.Fatalf("other key: %d", rec.Code)
	}
	// the variable is unset: only paired keys work, and "" never matches
	t.Setenv("RT_FLEET_KEY", "")
	if rec := rtDo(t, s, "POST", "/v1/chat/completions", rtChat("r", false), rtRemote, rtBearer("pair-key-123")); rec.Code != 200 {
		t.Fatalf("paired key without api key: %d", rec.Code)
	}
	if rec := rtDo(t, s, "POST", "/v1/chat/completions", rtChat("r", false), rtRemote); rec.Code != 401 {
		t.Fatalf("no key with empty variable: %d", rec.Code)
	}
}

func TestRouting_Auth_NoKeyConfiguredIsOpen(t *testing.T) {
	s, _ := rt3(t, config.Role{Preferred: "a"})
	if rec := rtDo(t, s, "POST", "/v1/chat/completions", rtChat("r", false), rtRemote); rec.Code != 200 {
		t.Fatalf("no api_key_env: %d", rec.Code)
	}
}

func TestRouting_Auth_AdminLoopbackOnlyUnlessToken(t *testing.T) {
	s, _ := rt3(t, config.Role{Preferred: "a"})
	if rec := rtDo(t, s, "GET", "/fleet/status", nil); rec.Code != 200 {
		t.Fatalf("loopback admin %d", rec.Code)
	}
	if rec := rtDo(t, s, "GET", "/fleet/status", nil, rtRemote); rec.Code != 403 {
		t.Fatalf("remote admin without token %d", rec.Code)
	}
	if rec := rtDo(t, s, "POST", "/fleet/role", map[string]any{"role": "x", "preferred": "a"}, rtRemote, rtBearer("anything")); rec.Code != 403 {
		t.Fatalf("remote admin write without admin_token %d", rec.Code)
	}
	cfg := s.Config()
	cfg.AdminToken = "adm1n"
	s.cfg = cfg
	if rec := rtDo(t, s, "GET", "/fleet/status", nil, rtRemote, rtBearer("adm1n")); rec.Code != 200 {
		t.Fatalf("remote admin with token %d", rec.Code)
	}
	if rec := rtDo(t, s, "GET", "/fleet/config", nil, rtRemote, rtBearer("wrong")); rec.Code != 403 {
		t.Fatalf("wrong token %d", rec.Code)
	}
	var c map[string]any
	rtJSON(t, rtDo(t, s, "GET", "/fleet/config", nil), &c)
	if strings.Contains(fmt.Sprint(c), "adm1n") {
		t.Fatal("the admin token must not be exported in /fleet/config")
	}
}

// ---------------------------------------------------------------- admin API

func rtAdmin(t *testing.T) (*Server, string, *rtUp) {
	up := rtNewUp(t, "a")
	s, path := rtFileServer(t, &config.Config{
		Listen: config.DefaultListen,
		Endpoints: []config.Endpoint{
			{Name: "a", Node: "n1", URL: up.URL(), Model: "ma"},
			{Name: "b", Node: "n2", URL: up.URL(), Model: "mb"},
		},
		Roles: map[string]config.Role{"r": {Preferred: "a"}},
	})
	return s, path, up
}

func TestRouting_Admin_RoleSetUpdateDelete(t *testing.T) {
	s, path, _ := rtAdmin(t)
	rec := rtDo(t, s, "POST", "/fleet/role", map[string]any{"role": "coder", "preferred": "a", "fallback": []string{"b"}, "max_parallel": 2, "priority": 5})
	var rs RoleStatus
	rtJSON(t, rec, &rs)
	if rec.Code != 200 || rs.Preferred != "a" || rs.Active != "a" || rs.MaxParallel != 2 {
		t.Fatalf("set: %d %+v", rec.Code, rs)
	}
	if _, _, c := s.lim("role:coder").stats(); c != 2 {
		t.Fatalf("max_parallel not applied hot: cap %d", c)
	}
	// update keeps the fields not given
	rtJSON(t, rtDo(t, s, "POST", "/fleet/role", map[string]any{"role": "coder", "strategy": "least-busy", "overflow": true}), &rs)
	if rs.Preferred != "a" || len(rs.Fallback) != 1 || rs.Priority != 5 || rs.Strategy != "least-busy" || !rs.Overflow {
		t.Fatalf("update: %+v", rs)
	}
	onDisk, err := config.Load(path)
	if err != nil || onDisk.Roles["coder"].Strategy != "least-busy" {
		t.Fatalf("not saved: %v %+v", err, onDisk)
	}
	// invalid input is refused and changes nothing
	for _, bad := range []map[string]any{
		{"role": "coder", "strategy": "random"},
		{"role": "coder", "preferred": "ghost"},
		{"role": "x", "preferred": ""},
		{"preferred": "a"},
	} {
		if rec := rtDo(t, s, "POST", "/fleet/role", bad); rec.Code != 400 {
			t.Fatalf("%v accepted: %d", bad, rec.Code)
		}
	}
	if s.Config().Roles["coder"].Strategy != "least-busy" {
		t.Fatal("a refused change was applied")
	}
	// the new role routes immediately
	if rec := rtDo(t, s, "POST", "/v1/chat/completions", rtChat("coder", false)); rec.Code != 200 {
		t.Fatalf("new role: %d", rec.Code)
	}
	if rec := rtDo(t, s, "POST", "/fleet/role", map[string]any{"role": "coder", "delete": true}); rec.Code != 200 {
		t.Fatalf("delete %d", rec.Code)
	}
	if _, ok := s.Config().Roles["coder"]; ok {
		t.Fatal("role still there")
	}
	if rec := rtDo(t, s, "POST", "/v1/chat/completions", rtChat("coder", false)); rec.Code != 404 {
		t.Fatalf("deleted role: %d", rec.Code)
	}
}

func TestRouting_Admin_EndpointsAddRemove(t *testing.T) {
	s, _, up := rtAdmin(t)
	rec := rtDo(t, s, "POST", "/fleet/endpoints", map[string]any{"add": []map[string]any{{"name": "c", "node": "n3", "url": up.URL(), "model": "mc"}}})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"endpoints":3`) {
		t.Fatalf("add %d %s", rec.Code, rec.Body.String())
	}
	if rec := rtDo(t, s, "POST", "/v1/chat/completions", rtChat("mc", false)); rec.Header().Get("X-Fleet-Endpoint") != "c" {
		t.Fatalf("new endpoint not routed: %q", rec.Header().Get("X-Fleet-Endpoint"))
	}
	if rec := rtDo(t, s, "POST", "/fleet/endpoints", map[string]any{"add": []map[string]any{{"name": "c", "node": "n3", "url": up.URL(), "model": "mc"}}}); rec.Code != 400 {
		t.Fatalf("duplicate accepted: %d", rec.Code)
	}
	if rec := rtDo(t, s, "POST", "/fleet/endpoints", map[string]any{"add": []map[string]any{{"name": "d", "model": "md"}}}); rec.Code != 400 {
		t.Fatalf("endpoint without url accepted: %d", rec.Code)
	}
	if rec := rtDo(t, s, "POST", "/fleet/endpoints", map[string]any{"remove": []string{"c"}}); rec.Code != 200 || s.Config().Endpoint("c") != nil {
		t.Fatalf("remove %d", rec.Code)
	}
	// removing the endpoint a role points at is refused
	if rec := rtDo(t, s, "POST", "/fleet/endpoints", map[string]any{"remove": []string{"a"}}); rec.Code != 400 {
		t.Fatalf("removing a role's preferred endpoint: %d", rec.Code)
	}
}

func TestRouting_Admin_EndpointSetGroupAndStall(t *testing.T) {
	up := rtNewUp(t, "a")
	s, _ := rtFileServer(t, &config.Config{Endpoints: []config.Endpoint{rtEp("a", "n1", up.URL(), "ma")}})
	body := map[string]any{"name": "a", "group": map[string]any{"engine": "llama-ring", "workers": []string{"n2"}}, "stall": "45s"}
	if rec := rtDo(t, s, "POST", "/fleet/endpoint-set", body); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	e := s.Config().Endpoint("a")
	if e.Group == nil || e.Group.Engine != "llama-ring" || e.Stall != "45s" || e.StallAfter() != 45*time.Second {
		t.Fatalf("%+v %q", e.Group, e.Stall)
	}
	if rec := rtDo(t, s, "POST", "/fleet/endpoint-set", map[string]any{"name": "a", "stall": "soon"}); rec.Code != 400 {
		t.Fatalf("a bad stall must be refused: %d", rec.Code)
	}
	if s.Config().Endpoint("a").Stall != "45s" {
		t.Fatal("refused change must not stick")
	}
	rtDo(t, s, "POST", "/fleet/endpoint-set", map[string]any{"name": "a", "group": map[string]any{"engine": ""}})
	if s.Config().Endpoint("a").Group != nil {
		t.Fatal("an empty engine removes the group")
	}
}

func TestRouting_Admin_EndpointSetSlotsCapsTags(t *testing.T) {
	s, _, _ := rtAdmin(t)
	rec := rtDo(t, s, "POST", "/fleet/endpoint-set", map[string]any{"name": "a", "slots": 3, "caps": []string{"chat", "vision"}, "tags": []string{"fast"}})
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	e := s.Config().Endpoint("a")
	if e.Slots != 3 || fmt.Sprint(e.Caps) != "[chat vision]" || fmt.Sprint(e.Tags) != "[fast]" {
		t.Fatalf("%+v", e)
	}
	if _, _, c := s.lim("ep:a").stats(); c != 3 {
		t.Fatalf("slots not applied hot: %d", c)
	}
	// a tag selector picks it up at once
	rtDo(t, s, "POST", "/fleet/role", map[string]any{"role": "quick", "match": []string{"tag:fast"}})
	if got := rtNames(s.Resolve("quick")); got != "a" {
		t.Fatalf("tag role %s", got)
	}
	// negative slots clamp to 0 (unlimited), empty caps go back to automatic
	rtDo(t, s, "POST", "/fleet/endpoint-set", map[string]any{"name": "a", "slots": -4, "caps": []string{}})
	if e := s.Config().Endpoint("a"); e.Slots != 0 || len(e.Caps) != 0 {
		t.Fatalf("%+v", e)
	}
	if rec := rtDo(t, s, "POST", "/fleet/endpoint-set", map[string]any{"name": "ghost", "slots": 1}); rec.Code != 404 {
		t.Fatalf("unknown endpoint %d", rec.Code)
	}
	if rec := rtDo(t, s, "POST", "/fleet/endpoint-set", map[string]any{"slots": 1}); rec.Code != 400 {
		t.Fatalf("no name %d", rec.Code)
	}
}

func TestRouting_Admin_UndoAndHistory(t *testing.T) {
	s, path, _ := rtAdmin(t)
	if rec := rtDo(t, s, "POST", "/fleet/undo", nil); rec.Code != 400 {
		t.Fatalf("nothing to undo: %d", rec.Code)
	}
	rtDo(t, s, "POST", "/fleet/role", map[string]any{"role": "one", "preferred": "a"})
	rtDo(t, s, "POST", "/fleet/role", map[string]any{"role": "two", "preferred": "b"})
	var h struct{ Snapshots []string }
	rtJSON(t, rtDo(t, s, "GET", "/fleet/history", nil), &h)
	if len(h.Snapshots) != 2 {
		t.Fatalf("history %v", h.Snapshots)
	}
	var u struct {
		Restored string
		Left     int
	}
	rtJSON(t, rtDo(t, s, "POST", "/fleet/undo", nil), &u)
	if u.Restored != h.Snapshots[1] || u.Left != 1 {
		t.Fatalf("undo %+v", u)
	}
	c := s.Config()
	if _, ok := c.Roles["two"]; ok {
		t.Fatal("undo did not remove role two (hot)")
	}
	if _, ok := c.Roles["one"]; !ok {
		t.Fatal("undo went too far")
	}
	if d, _ := config.Load(path); d.Roles["two"].Preferred != "" {
		t.Fatal("undo not written to disk")
	}
	rtDo(t, s, "POST", "/fleet/undo", nil)
	if _, ok := s.Config().Roles["one"]; ok {
		t.Fatal("second undo")
	}
}

func TestRouting_Admin_Reload(t *testing.T) {
	s, path, up := rtAdmin(t)
	c, _ := config.Load(path)
	c.Endpoints = append(c.Endpoints, config.Endpoint{Name: "z", Node: "n9", URL: up.URL(), Model: "mz", Slots: 4})
	if err := c.Save(path); err != nil {
		t.Fatal(err)
	}
	if s.Config().Endpoint("z") != nil {
		t.Fatal("not loaded yet")
	}
	if rec := rtDo(t, s, "POST", "/fleet/reload", nil); rec.Code != 200 {
		t.Fatalf("reload %d", rec.Code)
	}
	if s.Config().Endpoint("z") == nil {
		t.Fatal("reload did not pick up the file")
	}
	if _, _, cp := s.lim("ep:z").stats(); cp != 4 {
		t.Fatalf("reload must apply slots too: cap %d", cp)
	}
	// a broken file is refused and the running configuration stays
	if err := rtWrite(path, "endpoints: [ {name: a}"); err != nil {
		t.Fatal(err)
	}
	if rec := rtDo(t, s, "POST", "/fleet/reload", nil); rec.Code != 400 || s.Config().Endpoint("z") == nil {
		t.Fatalf("broken file: %d", rec.Code)
	}
}

func TestRouting_Admin_StatusShape(t *testing.T) {
	s, _, _ := rtAdmin(t)
	s.state["b"] = &EpState{OK: false, Err: "http 500", Checked: time.Now()}
	var st map[string]any
	rec := rtDo(t, s, "GET", "/fleet/status", nil)
	rtJSON(t, rec, &st)
	for _, k := range []string{"version", "listen", "endpoints", "roles", "nodes", "probe", "bench"} {
		if _, ok := st[k]; !ok {
			t.Fatalf("status has no %q: %v", k, st)
		}
	}
	eps := st["endpoints"].([]any)
	if len(eps) != 2 {
		t.Fatalf("endpoints %v", eps)
	}
	b := eps[1].(map[string]any)
	if b["name"] != "b" || b["ok"] != false || b["known"] != true || b["err"] != "http 500" || b["base"] == "" {
		t.Fatalf("endpoint b %v", b)
	}
	if a := eps[0].(map[string]any); a["known"] != false || a["in_use"] != 0.0 {
		t.Fatalf("endpoint a %v", a)
	}
	r := st["roles"].(map[string]any)["r"].(map[string]any)
	if r["active"] != "a" || r["preferred"] != "a" {
		t.Fatalf("role %v", r)
	}
	nodes := st["nodes"].([]any)
	var names []string
	for _, n := range nodes {
		names = append(names, n.(map[string]any)["node"].(string))
	}
	sort.Strings(names)
	if !strings.Contains(strings.Join(names, ","), "n1") || !strings.Contains(strings.Join(names, ","), "n2") {
		t.Fatalf("nodes %v", names)
	}
}

func TestRouting_Admin_Log(t *testing.T) {
	s, _, _ := rtAdmin(t)
	rtDo(t, s, "POST", "/v1/chat/completions", rtChat("r", false))
	rtDo(t, s, "POST", "/v1/chat/completions", rtChat("ghost", false))
	var l struct{ Entries []LogEntry }
	rtJSON(t, rtDo(t, s, "GET", "/fleet/log?n=1", nil), &l)
	if len(l.Entries) != 1 || l.Entries[0].Model != "ghost" || l.Entries[0].Status != 404 {
		t.Fatalf("n=1 %+v", l.Entries)
	}
	rtJSON(t, rtDo(t, s, "GET", "/fleet/log", nil), &l)
	if len(l.Entries) != 2 || l.Entries[0].Endpoint != "a" || l.Entries[0].Attempts != 1 {
		t.Fatalf("all %+v", l.Entries)
	}
}

func TestRouting_Admin_UsageGrouping(t *testing.T) {
	s, _, _ := rtAdmin(t)
	rtDo(t, s, "POST", "/v1/chat/completions", rtChat("r", false), rtUA("opencode/1.0"))
	rtDo(t, s, "POST", "/v1/chat/completions", rtChat("r", false), rtUA("opencode/1.0"))
	rtDo(t, s, "POST", "/v1/chat/completions", rtChat("mb", false), rtUA("curl/8"))
	rtDo(t, s, "POST", "/v1/chat/completions", rtChat("ghost", false), func(r *http.Request) { r.Header.Set("X-Fleet-Client", "MyApp") })
	get := func(q string) []UsageRow {
		var v struct {
			By   string
			Rows []UsageRow
		}
		rtJSON(t, rtDo(t, s, "GET", "/fleet/usage?"+q, nil), &v)
		return v.Rows
	}
	keys := func(rows []UsageRow) string {
		var k []string
		for _, r := range rows {
			x := fmt.Sprintf("%s:%d", r.Key, r.Requests)
			if r.Sub != "" {
				x = fmt.Sprintf("%s/%s:%d", r.Key, r.Sub, r.Requests)
			}
			k = append(k, x)
		}
		return strings.Join(k, ",")
	}
	if got := keys(get("by=client")); got != "opencode:2,curl:1,myapp:1" {
		t.Fatalf("by client %s", got)
	}
	if got := keys(get("by=role")); got != "-:2,r:2" {
		t.Fatalf("by role %s", got)
	}
	if got := keys(get("by=model")); got != "n1/ma:2,-:1,n2/mb:1" {
		t.Fatalf("by model %s", got)
	}
	if got := keys(get("by=endpoint")); got != "a:2,-:1,b:1" {
		t.Fatalf("by endpoint %s", got)
	}
	if got := keys(get("by=day&since=24h")); got != time.Now().Format("2006-01-02")+":4" {
		t.Fatalf("by day %s", got)
	}
	if got := keys(get("by=role&split=model")); got != "-/-:1,-/n2/mb:1,r/n1/ma:2" {
		t.Fatalf("split %s", got)
	}
	rows := get("by=client")
	if rows[0].In != 10 || rows[0].Out != 6 || rows[2].Errors != 1 {
		t.Fatalf("figures %+v", rows)
	}
}

// ---------------------------------------------------------------- machine state: strain and warm models

func TestRouting_Strain_StrainedNodeGoesBehind(t *testing.T) {
	s, ups := rt3(t, config.Role{Preferred: "a", Fallback: []string{"b", "c"}})
	rtStrain(s, "n1", time.Now())
	if got := rtNames(s.Resolve("r")); got != "b,c,a" {
		t.Fatalf("Resolve %s, want b,c,a", got)
	}
	rec := rtDo(t, s, "POST", "/v1/chat/completions", rtChat("r", false))
	if rec.Header().Get("X-Fleet-Endpoint") != "b" || ups["a"].hits() != 0 {
		t.Fatalf("proxy went to %q", rec.Header().Get("X-Fleet-Endpoint"))
	}
	st := s.Status()
	if st.Roles["r"].Active != "b" || fmt.Sprint(st.Endpoints[0].Strained) != "[x]" {
		t.Fatalf("status %+v / %+v", st.Roles["r"], st.Endpoints[0])
	}
	// a strained machine still serves when it is the only healthy one
	s.state["b"], s.state["c"] = &EpState{OK: false}, &EpState{OK: false}
	if got := rtNames(s.Resolve("r")); got != "a,b,c" {
		t.Fatalf("only a healthy: %s", got)
	}
}

func TestRouting_Strain_AvoidStrainOff(t *testing.T) {
	s, _ := rt3(t, config.Role{Preferred: "a", Fallback: []string{"b"}})
	s.cfg.AvoidStrain = rtBool(false)
	rtStrain(s, "n1", time.Now())
	if got := rtNames(s.Resolve("r")); got != "a,b" {
		t.Fatalf("avoid_strain false: %s", got)
	}
	if rec := rtDo(t, s, "POST", "/v1/chat/completions", rtChat("r", false)); rec.Header().Get("X-Fleet-Endpoint") != "a" {
		t.Fatalf("proxy %q", rec.Header().Get("X-Fleet-Endpoint"))
	}
}

func TestRouting_Strain_AllStrainedKeepsOrder(t *testing.T) {
	s, _ := rt3(t, config.Role{Preferred: "b", Fallback: []string{"a", "c"}})
	for _, n := range []string{"n1", "n2", "n3"} {
		rtStrain(s, n, time.Now())
	}
	if got := rtNames(s.Resolve("r")); got != "b,a,c" {
		t.Fatalf("all strained: %s", got)
	}
}

func TestRouting_Strain_UnhealthyStayLast(t *testing.T) {
	s, _ := rt3(t, config.Role{Preferred: "a", Fallback: []string{"b", "c"}})
	s.state["a"] = &EpState{OK: false} // unstrained but down
	rtStrain(s, "n2", time.Now())      // b strained, healthy
	if got := rtNames(s.Resolve("r")); got != "c,b,a" {
		t.Fatalf("got %s, want c,b,a", got)
	}
	got := rtNames(s.condition(config.Role{}, []config.Endpoint{*s.Config().Endpoint("a"), *s.Config().Endpoint("b"), *s.Config().Endpoint("c")}))
	if got != "c,b,a" {
		t.Fatalf("condition direct: %s", got)
	}
}

func TestRouting_Strain_StaleIgnored(t *testing.T) {
	s, _ := rt3(t, config.Role{Preferred: "a", Fallback: []string{"b"}})
	rtStrain(s, "n1", time.Now().Add(-2*time.Minute))
	if got := rtNames(s.Resolve("r")); got != "a,b" {
		t.Fatalf("stale strain moved a: %s", got)
	}
	if _, ok := s.NodeStrainOf("n1"); ok {
		t.Fatal("stale strain returned")
	}
	// a strain reported by a peer counts as well
	s.strain.set("n1", NodeStrain{Strain: hostinfo.Strain{At: time.Now(), Reasons: []string{"gpu hot"}}, Source: "peer"}, true)
	if got := rtNames(s.Resolve("r")); got != "b,a" {
		t.Fatalf("peer strain: %s", got)
	}
	// fresh but without reasons (warnings only) is not strain
	rtStrainWarn := NodeStrain{Strain: hostinfo.Strain{At: time.Now(), Warnings: []string{"disk 95%"}}}
	s.strain.set("n2", rtStrainWarn, false)
	s.strain.set("n1", rtStrainWarn, true)
	if got := rtNames(s.Resolve("r")); got != "a,b" {
		t.Fatalf("warnings only: %s", got)
	}
}

func TestRouting_Strain_DirectModelPoolAvoidsStrain(t *testing.T) {
	up := rtNewUp(t, "x")
	s := rtServer(t, &config.Config{Endpoints: []config.Endpoint{
		rtEp("dino-l", "dino", up.URL(), "llama3"), rtEp("vec-l", "vec", up.URL(), "llama3"),
	}})
	rtStrain(s, "dino", time.Now())
	if got := rtNames(s.Resolve("llama3")); got != "vec-l,dino-l" {
		t.Fatalf("bare id pool %s", got)
	}
}

func TestRouting_Warm_WarmFirstDefaults(t *testing.T) {
	for _, c := range []struct {
		role config.Role
		want bool
	}{
		{config.Role{}, false},
		{config.Role{Strategy: "failover"}, false},
		{config.Role{Strategy: "least-busy"}, true},
		{config.Role{Strategy: "round-robin"}, true},
		{config.Role{Strategy: "fastest"}, true},
		{config.Role{Strategy: "failover", PreferWarm: rtBool(true)}, true},
		{config.Role{Strategy: "least-busy", PreferWarm: rtBool(false)}, false},
	} {
		if got := c.role.WarmFirst(); got != c.want {
			t.Errorf("%+v: WarmFirst %v", c.role, got)
		}
	}
}

func TestRouting_Warm_ColdGoesBehindWhenRoleWantsIt(t *testing.T) {
	cold, warm := false, true
	cases := []struct {
		name string
		role config.Role
		want string
	}{
		{"failover keeps order", config.Role{Preferred: "a", Fallback: []string{"b", "c"}}, "a,b,c"},
		{"failover prefer_warm", config.Role{Preferred: "a", Fallback: []string{"b", "c"}, PreferWarm: rtBool(true)}, "b,c,a"},
		{"least-busy", config.Role{Preferred: "a", Fallback: []string{"b", "c"}, Strategy: "least-busy"}, "b,c,a"},
		{"least-busy prefer_warm false", config.Role{Preferred: "a", Fallback: []string{"b", "c"}, Strategy: "least-busy", PreferWarm: rtBool(false)}, "a,b,c"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, _ := rt3(t, c.role)
			s.warm.set("a", ModelInfo{Loaded: &cold})
			s.warm.set("b", ModelInfo{Loaded: &warm})
			// c: unknown (no entry) counts as warm
			if got := rtNames(s.Resolve("r")); got != c.want {
				t.Fatalf("Resolve %s, want %s", got, c.want)
			}
			if got := rtDo(t, s, "POST", "/v1/chat/completions", rtChat("r", false)).Header().Get("X-Fleet-Endpoint"); got != c.want[:1] {
				t.Fatalf("proxy picked %q, want %s", got, c.want[:1])
			}
		})
	}
}

func TestRouting_Warm_NilLoadedCountsAsWarm(t *testing.T) {
	s, _ := rt3(t, config.Role{Preferred: "a", Fallback: []string{"b"}, Strategy: "least-busy"})
	s.warm.set("a", ModelInfo{Size: 5 << 30}) // known size, Loaded nil (runtime does not say)
	if s.cold("a") {
		t.Fatal("nil Loaded must count as warm")
	}
	if got := rtNames(s.Resolve("r")); got != "a,b" {
		t.Fatalf("%s", got)
	}
}

func TestRouting_Warm_StrainOutweighsCold(t *testing.T) {
	cold := false
	s, _ := rt3(t, config.Role{Preferred: "a", Fallback: []string{"b", "c"}, Strategy: "least-busy"})
	s.warm.set("a", ModelInfo{Loaded: &cold}) // a: cold, machine fine
	rtStrain(s, "n2", time.Now())             // b: warm, machine strained
	s.warm.set("c", ModelInfo{Loaded: &cold}) // c: cold
	rtStrain(s, "n3", time.Now())             // … and strained
	if got := rtNames(s.Resolve("r")); got != "a,b,c" {
		t.Fatalf("got %s, want a (cold) before b (strained) before c (both)", got)
	}
}

func TestRouting_Warm_RoundRobinSkipsColdWhileWarmExist(t *testing.T) {
	cold := false
	s, _ := rt3(t, config.Role{Preferred: "a", Fallback: []string{"b", "c"}, Strategy: "round-robin"})
	s.warm.set("c", ModelInfo{Loaded: &cold})
	seen := map[string]int{}
	for i := 0; i < 6; i++ {
		seen[rtDo(t, s, "POST", "/v1/chat/completions", rtChat("r", false)).Header().Get("X-Fleet-Endpoint")]++
	}
	if seen["c"] != 0 || seen["a"] == 0 || seen["b"] == 0 {
		t.Fatalf("round-robin with c cold: %v", seen)
	}
}

// rtOllamaUp is a fake Ollama: OpenAI-compatible chat plus /api/tags and /api/ps.
func rtOllamaUp(t *testing.T, loaded ...string) *rtUp {
	u := rtNewUp(t, "ollama")
	var ps []string
	for _, m := range loaded {
		ps = append(ps, fmt.Sprintf(`{"name":%q,"model":%q,"size":123}`, m, m))
	}
	u.tags = `{"models":[
		{"name":"llama3:latest","model":"llama3:latest","size":4661224676,"details":{"parameter_size":"8.0B"}},
		{"name":"big:74b-a9b","model":"big:74b-a9b","size":45000000000,"details":{"parameter_size":"74B"}},
		{"name":"Qwen3:8B","model":"Qwen3:8B","size":5200000000,"details":{"parameter_size":"8.2B"}}]}`
	u.ps = `{"models":[` + strings.Join(ps, ",") + `]}`
	return u
}

func TestRouting_WarmPoll_OllamaLoadedSizesParams(t *testing.T) {
	ol := rtOllamaUp(t, "big:74b-a9b")
	s := rtServer(t, &config.Config{Endpoints: []config.Endpoint{
		rtEp("l", "n", ol.URL(), "llama3"),        // ":latest" normalised
		rtEp("big", "n", ol.URL(), "big:74b-a9b"), // mixture of experts
		rtEp("q", "n", ol.URL(), "qwen3:8b"),      // case-insensitive
		rtEp("gone", "n", ol.URL(), "missing:1b"), // not pulled
	}})
	s.pollRuntimes(context.Background(), s.Config())
	l, ok := s.warm.get("l")
	if !ok || l.Loaded == nil || *l.Loaded || l.Size != 4661224676 || l.Params != "8.0B" || l.Active != 0 {
		t.Fatalf("llama3 %+v", l)
	}
	b, _ := s.warm.get("big")
	if b.Loaded == nil || !*b.Loaded || b.Active != 9 || b.Params != "74B" {
		t.Fatalf("big %+v", b)
	}
	if q, _ := s.warm.get("q"); q.Size != 5200000000 {
		t.Fatalf("qwen3 (case) %+v", q)
	}
	if g, ok := s.warm.get("gone"); !ok || g.Loaded == nil || *g.Loaded || g.Size != 0 {
		t.Fatalf("missing %+v", g)
	}
	if !s.cold("l") || s.cold("big") {
		t.Fatal("cold()")
	}
	var info *ModelInfo
	for _, e := range s.Status().Endpoints {
		if e.Name == "big" {
			info = e.Info
		}
	}
	if info == nil || info.Active != 9 || info.Loaded == nil || !*info.Loaded {
		t.Fatalf("EpStatus.Info %+v", info)
	}
	// /api/ps changes are seen on the next round (tags are cached)
	ol.set(func(u *rtUp) { u.ps = `{"models":[{"name":"llama3:latest","model":"llama3:latest"}]}`; u.tags = "" })
	s.pollRuntimes(context.Background(), s.Config())
	if l, _ := s.warm.get("l"); l.Loaded == nil || !*l.Loaded || l.Size != 4661224676 {
		t.Fatalf("after reload %+v", l)
	}
	if s.cold("l") || !s.cold("big") {
		t.Fatal("cold() after second poll")
	}
}

func TestRouting_WarmPoll_ActiveParamsNames(t *testing.T) {
	for name, want := range map[string]float64{
		"qwen3:30b-a3b": 3, "gpt-oss:120b": 0, "x:74b-a9b-q4": 9, "y_a2.5b": 2.5, "llama3": 0, "a3b": 3,
	} {
		if got := activeParams(name); got != want {
			t.Errorf("activeParams(%q) = %v, want %v", name, got, want)
		}
	}
	if normModel("Llama3:LATEST") != "llama3" || normModel("llama3:latest") != "llama3" {
		t.Fatal("normModel")
	}
}

func TestRouting_WarmPoll_NonOllamaStaysUnknown(t *testing.T) {
	up := rtNewUp(t, "llamacpp") // /api/tags → 404
	s := rtServer(t, &config.Config{Endpoints: []config.Endpoint{rtEp("x", "n", up.URL(), "model")}})
	s.pollRuntimes(context.Background(), s.Config())
	if _, ok := s.warm.get("x"); ok {
		t.Fatal("a non-Ollama runtime must leave the model unknown")
	}
	if s.cold("x") {
		t.Fatal("unknown counts as warm")
	}
	if es := s.Status().Endpoints[0]; es.Info != nil {
		t.Fatalf("info %+v", es.Info)
	}
}

func TestRouting_WarmPoll_PsFailsLeavesLoadedUnknown(t *testing.T) {
	ol := rtOllamaUp(t)
	ol.ps = "" // /api/ps 404
	s := rtServer(t, &config.Config{Endpoints: []config.Endpoint{rtEp("l", "n", ol.URL(), "llama3")}})
	s.pollRuntimes(context.Background(), s.Config())
	l, ok := s.warm.get("l")
	if !ok || l.Loaded != nil || l.Size == 0 {
		t.Fatalf("%+v", l)
	}
}

func TestRouting_WarmPoll_PeerEndpointsSkipped(t *testing.T) {
	ol := rtOllamaUp(t)
	s := rtServer(t, &config.Config{
		Peers:     []config.Peer{{Name: "p", URL: ol.URL()}},
		Endpoints: []config.Endpoint{{Name: "p-l", Node: "far", URL: ol.URL(), Model: "llama3", Peer: "p"}},
	})
	s.pollRuntimes(context.Background(), s.Config())
	if _, ok := s.warm.get("p-l"); ok {
		t.Fatal("peer endpoints carry what the peer knows; they are not polled")
	}
}

func TestRouting_WarmPoll_EndToEndPicksLoadedModel(t *testing.T) {
	ol := rtOllamaUp(t, "qwen3:8b")
	s := rtServer(t, &config.Config{
		Endpoints: []config.Endpoint{rtEp("l", "n", ol.URL(), "llama3"), rtEp("q", "n", ol.URL(), "qwen3:8b")},
		Roles:     map[string]config.Role{"pool": {Preferred: "l", Fallback: []string{"q"}, Strategy: "least-busy"}},
	})
	s.pollRuntimes(context.Background(), s.Config())
	rec := rtDo(t, s, "POST", "/v1/chat/completions", rtChat("pool", false))
	if rec.Header().Get("X-Fleet-Endpoint") != "q" || ol.lastBody()["model"] != "qwen3:8b" {
		t.Fatalf("picked %q", rec.Header().Get("X-Fleet-Endpoint"))
	}
	if s.Status().Roles["pool"].Active != "q" {
		t.Fatal("status active")
	}
}

// TestRouting_WarmPoll_RuntimeGoneKeepsStaleCold: a base that answered /api/tags earlier and stops answering
// it (restarted as something else, or the request timed out) keeps the endpoint's last "cold" entry.
func TestRouting_WarmPoll_RuntimeGoneKeepsStaleCold(t *testing.T) {
	ol := rtOllamaUp(t) // nothing loaded
	s := rtServer(t, &config.Config{Endpoints: []config.Endpoint{rtEp("l", "n", ol.URL(), "llama3")}})
	s.pollRuntimes(context.Background(), s.Config())
	if !s.cold("l") {
		t.Fatal("setup: l should be cold")
	}
	ol.set(func(u *rtUp) { u.tags, u.ps = "", "" }) // the runtime stops answering the Ollama API
	s.warm.mu.Lock()
	for _, rb := range s.warm.bases {
		rb.checked = time.Time{} // tags due again
	}
	s.warm.mu.Unlock()
	s.pollRuntimes(context.Background(), s.Config())
	if s.cold("l") {
		t.Fatal("l stays cold after its runtime stopped answering the Ollama API")
	}
}

// ---------------------------------------------------------------- misc

func rtWrite(path, s string) error {
	return os.WriteFile(path, []byte(s), 0o600)
}

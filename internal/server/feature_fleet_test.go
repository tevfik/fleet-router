package server

// Feature tests: fleets talking to fleets (peers, pairing, readings passed on) and the machines
// themselves (host readings, capability probes, benchmarks, advice, metrics, nodes, LAN announcement).
// Every fleet runs in-process behind an httptest server on 127.0.0.1; every path fleet writes to is a
// temp dir (flEnv). Helpers are prefixed fl.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/hostinfo"
	"github.com/tevfik/fleet-router/internal/mdns"
)

// ---- helpers ----

// flEnv keeps everything fleet reads or writes (config dir, stats, probe.json, machine.json, .env
// files) inside temp dirs. Returns the state dir (where probe.json / machine.json go).
func flEnv(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	t.Setenv("HOME", d)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(d, "xdg-config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(d, "xdg-state"))
	t.Setenv("FLEET_CONFIG", filepath.Join(d, "unused", "config.yaml"))
	state := filepath.Join(d, "state")
	t.Setenv("FLEET_STATS", filepath.Join(state, "stats.json"))
	return state
}

// flFleet starts a fleet (no background loops) over cfg saved in a temp dir. wrap, when set, sits in
// front of its handler (to make callers look remote, to switch it off, to record requests).
func flFleet(t *testing.T, cfg *config.Config, wrap func(http.Handler) http.Handler) (*Server, *httptest.Server) {
	t.Helper()
	if cfg.Roles == nil {
		cfg.Roles = map[string]config.Role{}
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	s.ctx = context.Background()
	s.ensureID()
	var h http.Handler = s.Handler()
	if wrap != nil {
		h = wrap(h)
	}
	front := httptest.NewServer(h)
	t.Cleanup(front.Close)
	return s, front
}

// flFromLAN makes every request look like it came from another machine (10.0.1.50).
func flFromLAN(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = "10.0.1.50:1234"
		h.ServeHTTP(w, r)
	})
}

// flRecorder keeps the headers and URLs of the requests a fleet received.
type flRecorder struct {
	mu   sync.Mutex
	reqs []*http.Request
}

func (rc *flRecorder) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc.mu.Lock()
		rc.reqs = append(rc.reqs, r.Clone(context.Background()))
		rc.mu.Unlock()
		h.ServeHTTP(w, r)
	})
}

func (rc *flRecorder) find(path string) []*http.Request {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	var out []*http.Request
	for _, r := range rc.reqs {
		if r.URL.Path == path {
			out = append(out, r)
		}
	}
	return out
}

// flSwitch answers 503 to everything while down is set (a peer that went away).
type flSwitch struct{ down atomic.Bool }

func (sw *flSwitch) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sw.down.Load() {
			http.Error(w, "gone", 503)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// flModel is a fake OpenAI-compatible model server: it streams a short answer and records the model
// value and the headers of every request.
type flModel struct {
	*httptest.Server
	mu     sync.Mutex
	models []string
	auth   []string
}

func flUpstream(t *testing.T) *flModel {
	m := &flModel{}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		json.NewDecoder(r.Body).Decode(&in)
		mod, _ := in["model"].(string)
		m.mu.Lock()
		m.models = append(m.models, mod)
		m.auth = append(m.auth, r.Header.Get("Authorization"))
		m.mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/models") {
			fmt.Fprint(w, `{"data":[]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, s := range []string{`{"choices":[{"delta":{"content":"Hel"}}]}`, `{"choices":[{"delta":{"content":"lo"},"finish_reason":"stop"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2}}`, `[DONE]`} {
			fmt.Fprint(w, "data: "+s+"\n\n")
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(m.Close)
	return m
}

func (m *flModel) seen() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string{}, m.models...)
}

// flImported lists the endpoints imported from a peer.
func flImported(s *Server, peer string) []config.Endpoint {
	var out []config.Endpoint
	for _, e := range s.Config().Endpoints {
		if e.Peer == peer {
			out = append(out, e)
		}
	}
	return out
}

func flRefs(eps []config.Endpoint) []string {
	var out []string
	for _, e := range eps {
		out = append(out, e.Ref())
	}
	return out
}

func flHas(l []string, x string) bool {
	for _, v := range l {
		if v == x {
			return true
		}
	}
	return false
}

// flDo sends a request and returns the status and the body.
func flDo(t *testing.T, method, url, body string, hdr map[string]string) (int, []byte, http.Header) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, resp.Header
}

func flChat(t *testing.T, base, model string, hdr map[string]string) (int, []byte, http.Header) {
	t.Helper()
	return flDo(t, "POST", base+"/v1/chat/completions", `{"model":"`+model+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`, hdr)
}

// flWait polls cond for up to 3 s.
func flWait(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func flWriteJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// flTwoFleets: bravo (B) owns dino/gemma behind a fake model server; laptop (A) has B as a peer, synced.
func flTwoFleets(t *testing.T, bWrap func(http.Handler) http.Handler) (a, b *Server, aFront, bFront *httptest.Server, up *flModel) {
	t.Helper()
	up = flUpstream(t)
	b, bFront = flFleet(t, &config.Config{Name: "bravo", Endpoints: []config.Endpoint{
		{Name: "dino-gemma", Node: "dino", URL: up.URL + "/v1", Model: "gemma", Ctx: 8192, Caps: []string{"chat", "tools"}, Tags: []string{"fast"}}}}, bWrap)
	a, aFront = flFleet(t, &config.Config{Name: "laptop"}, nil)
	if err := a.AddPeer(config.Peer{Name: "bravo", URL: bFront.URL + "/v1"}); err != nil {
		t.Fatal(err)
	}
	a.SyncPeers(context.Background())
	return
}

// ---- peers ----

func TestFleet_Peers_HelloAndExport(t *testing.T) {
	flEnv(t)
	_, b, _, bFront, _ := flTwoFleets(t, nil)
	code, body, _ := flDo(t, "GET", bFront.URL+"/fleet/hello", "", nil)
	var h Hello
	if code != 200 || json.Unmarshal(body, &h) != nil || !h.Fleet || h.ID != b.selfID() || h.ID == "" || h.Name != "bravo" || h.Version != Version {
		t.Fatalf("hello: %d %s", code, body)
	}
	code, body, _ = flDo(t, "GET", bFront.URL+"/fleet/export", "", nil)
	var ex struct {
		ID, Name string
		Models   []ExportModel
	}
	if code != 200 || json.Unmarshal(body, &ex) != nil {
		t.Fatalf("export: %d %s", code, body)
	}
	if ex.ID != b.selfID() || ex.Name != "bravo" || len(ex.Models) != 1 {
		t.Fatalf("export: %+v", ex)
	}
	m := ex.Models[0]
	if m.Ref != "dino/gemma" || m.Node != "dino" || m.Model != "gemma" || !m.Up || m.Ctx != 8192 ||
		len(m.Path) != 1 || m.Path[0] != b.selfID() || !flHas(m.Caps, "tools") || !flHas(m.Tags, "fast") {
		t.Fatalf("exported model: %+v", m)
	}
}

func TestFleet_Peers_SyncImportsByNodeModel(t *testing.T) {
	flEnv(t)
	a, b, aFront, _, _ := flTwoFleets(t, nil)
	got := flImported(a, "bravo")
	if len(got) != 1 {
		t.Fatalf("imported: %+v", got)
	}
	e := got[0]
	if e.Name != "bravo-dino-gemma" || e.Ref() != "dino/gemma" || e.Wire() != "dino/gemma" || e.Ctx != 8192 ||
		len(e.Path) != 1 || e.Path[0] != b.selfID() || !flHas(e.Caps, "tools") || !flHas(e.Tags, "fast") ||
		!strings.HasSuffix(e.URL, "/v1") || !a.healthy(e.Name) {
		t.Fatalf("imported endpoint: %+v", e)
	}
	// listed in /v1/models under its node/model, up
	code, body, _ := flDo(t, "GET", aFront.URL+"/v1/models", "", nil)
	if code != 200 || !strings.Contains(string(body), `"id":"dino/gemma"`) || !strings.Contains(string(body), `"up":true`) {
		t.Fatalf("models: %d %s", code, body)
	}
	// a locally measured speed survives the next sync, and so does the name
	cfg := a.Config()
	for i := range cfg.Endpoints {
		cfg.Endpoints[i].TPS = 77
	}
	if err := a.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	a.SyncPeers(context.Background())
	if got := flImported(a, "bravo"); len(got) != 1 || got[0].TPS != 77 || got[0].Name != e.Name {
		t.Fatalf("resync must keep the name and local speed: %+v", got)
	}
	// a model that is down on the peer is imported down
	b.mu.Lock()
	b.state["dino-gemma"] = &EpState{OK: false, Checked: time.Now(), Err: "x"}
	b.mu.Unlock()
	a.SyncPeers(context.Background())
	if st := a.getState(e.Name); st == nil || st.OK || st.Err != "down on bravo" {
		t.Fatalf("state of a model down on the peer: %+v", st)
	}
	if ps := a.PeerStatuses(); len(ps) != 1 || !ps[0].OK || ps[0].PeerID != b.selfID() || ps[0].Remote != "bravo" || ps[0].Models != 1 || ps[0].Version != Version {
		t.Fatalf("peer status: %+v", ps)
	}
}

func TestFleet_Peers_NameCollisionGetsSuffix(t *testing.T) {
	flEnv(t)
	up := flUpstream(t)
	_, bFront := flFleet(t, &config.Config{Name: "bravo", Endpoints: []config.Endpoint{{Name: "g", Node: "dino", URL: up.URL + "/v1", Model: "gemma"}}}, nil)
	a, _ := flFleet(t, &config.Config{Name: "laptop", Endpoints: []config.Endpoint{{Name: "bravo-dino-gemma", Node: "here", URL: up.URL + "/v1", Model: "other"}}}, nil)
	if err := a.AddPeer(config.Peer{Name: "bravo", URL: bFront.URL + "/v1"}); err != nil {
		t.Fatal(err)
	}
	a.SyncPeers(context.Background())
	if got := flImported(a, "bravo"); len(got) != 1 || got[0].Name != "bravo-dino-gemma-2" {
		t.Fatalf("an imported model must not take a local endpoint's name: %+v", got)
	}
}

func TestFleet_Peers_OnlySkipSelectors(t *testing.T) {
	flEnv(t)
	up := flUpstream(t)
	u := up.URL + "/v1"
	_, bFront := flFleet(t, &config.Config{Name: "bravo", Endpoints: []config.Endpoint{
		{Name: "g", Node: "dino", URL: u, Model: "gemma", Tags: []string{"fast"}},
		{Name: "q", Node: "dino", URL: u, Model: "qwen3", Tags: []string{"fast"}},
		{Name: "v", Node: "vec", URL: u, Model: "llava", Caps: []string{"chat", "vision"}},
		{Name: "e", Node: "vec", URL: u, Model: "nomic-embed", Caps: []string{"embed"}},
	}}, nil)
	cases := []struct {
		only, skip []string
		want       []string
	}{
		{nil, nil, []string{"dino/gemma", "dino/qwen3", "vec/llava", "vec/nomic-embed"}},
		{[]string{"tag:fast"}, []string{"model:qwen*"}, []string{"dino/gemma"}},
		{[]string{"cap:vision", "cap:embed"}, nil, []string{"vec/llava", "vec/nomic-embed"}},
		{nil, []string{"node:dino", "cap:embed"}, []string{"vec/llava"}},
	}
	for i, c := range cases {
		a, _ := flFleet(t, &config.Config{Name: "laptop"}, nil)
		if err := a.AddPeer(config.Peer{Name: "bravo", URL: bFront.URL + "/v1", Only: c.only, Skip: c.skip}); err != nil {
			t.Fatal(err)
		}
		a.SyncPeers(context.Background())
		if got := strings.Join(flRefs(flImported(a, "bravo")), ","); got != strings.Join(c.want, ",") {
			t.Errorf("case %d (only %v skip %v): %s, want %v", i, c.only, c.skip, got, c.want)
		}
	}
}

// Three fleets in a ring: C owns cbox/cm, B peers C, A peers B, C peers A.
func TestFleet_Peers_MaxHopsAndLoopPrevention(t *testing.T) {
	flEnv(t)
	up := flUpstream(t)
	c, cFront := flFleet(t, &config.Config{Name: "c", Endpoints: []config.Endpoint{{Name: "cm", Node: "cbox", URL: up.URL + "/v1", Model: "cm"}}}, nil)
	b, bFront := flFleet(t, &config.Config{Name: "b", Endpoints: []config.Endpoint{{Name: "bm", Node: "bbox", URL: up.URL + "/v1", Model: "bm"}}}, nil)
	a, aFront := flFleet(t, &config.Config{Name: "a", Endpoints: []config.Endpoint{{Name: "am", Node: "abox", URL: up.URL + "/v1", Model: "am"}}}, nil)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(b.AddPeer(config.Peer{Name: "c", URL: cFront.URL + "/v1"}))
	must(a.AddPeer(config.Peer{Name: "b", URL: bFront.URL + "/v1"}))
	must(c.AddPeer(config.Peer{Name: "a", URL: aFront.URL + "/v1", MaxHops: 9})) // no hop limit to hide a loop
	b.SyncPeers(ctx)
	a.SyncPeers(ctx)
	c.SyncPeers(ctx)
	b.SyncPeers(ctx)
	a.SyncPeers(ctx)

	// default max_hops 2: A sees B's own model and C's model through B
	ai := flImported(a, "b")
	if refs := flRefs(ai); len(refs) != 2 || !flHas(refs, "bbox/bm") || !flHas(refs, "cbox/cm") {
		t.Fatalf("A imported %v", refs)
	}
	for _, e := range ai {
		if e.Ref() == "cbox/cm" && (len(e.Path) != 2 || e.Path[0] != c.selfID() || e.Path[1] != b.selfID()) {
			t.Fatalf("path of a two-hop model must be owner first, exporter last: %v", e.Path)
		}
	}
	// C must never import its own model back, whatever the hop limit; it does get A's and B's
	if refs := flRefs(flImported(c, "a")); flHas(refs, "cbox/cm") || !flHas(refs, "abox/am") || !flHas(refs, "bbox/bm") {
		t.Fatalf("C imported %v (its own model came back around?)", refs)
	}
	// and no fleet ever holds an endpoint whose path contains its own id
	for _, s := range []*Server{a, b, c} {
		for _, e := range s.Config().Endpoints {
			if contains(e.Path, s.selfID()) {
				t.Fatalf("%s imported a path through itself: %+v", s.selfName(), e)
			}
		}
	}
	// a two-hop request works end to end: A → B → C → model
	if code, body, _ := flChat(t, aFront.URL, "cbox/cm", nil); code != 200 || !strings.Contains(string(body), "Hel") {
		t.Fatalf("A → B → C: %d %s", code, body)
	}
	if s := up.seen(); s[len(s)-1] != "cm" {
		t.Fatalf("the owner must send its plain model id: %v", s)
	}
	// max_hops 1: only the peer's own models
	must(a.AddPeer(config.Peer{Name: "b", URL: bFront.URL + "/v1", MaxHops: 1}))
	a.SyncPeers(ctx)
	if refs := flRefs(flImported(a, "b")); len(refs) != 1 || refs[0] != "bbox/bm" {
		t.Fatalf("max_hops 1: %v", refs)
	}
}

func TestFleet_Peers_ForwardedPathNotSentBack(t *testing.T) {
	flEnv(t)
	rec := &flRecorder{}
	a, b, aFront, _, _ := flTwoFleets(t, rec.wrap)
	if code, _, _ := flChat(t, aFront.URL, "dino/gemma", map[string]string{headerPath: b.selfID()}); code != 404 {
		t.Fatalf("a request that already passed B must not go back to B: %d", code)
	}
	if code, _, _ := flChat(t, aFront.URL, "dino/gemma", map[string]string{headerPath: "x, y ,z"}); code != 404 {
		t.Fatalf("a request that crossed %d fleets must only be served locally: %d", maxServeHops, code)
	}
	if len(rec.find("/v1/chat/completions")) != 0 {
		t.Fatal("B must not have been asked")
	}
	code, _, _ := flChat(t, aFront.URL, "dino/gemma", map[string]string{headerPath: "other"})
	if code != 200 {
		t.Fatalf("a path with another fleet is fine: %d", code)
	}
	r := rec.find("/v1/chat/completions")
	if len(r) != 1 || r[0].Header.Get(headerPath) != "other,"+a.selfID() {
		t.Fatalf("A must append its id to the path: %v", r)
	}
}

func TestFleet_Peers_DownMarksModelsAndTheyComeBack(t *testing.T) {
	flEnv(t)
	sw := &flSwitch{}
	a, _, _, _, _ := flTwoFleets(t, sw.wrap)
	got := flImported(a, "bravo")
	if len(got) != 1 || !a.healthy(got[0].Name) {
		t.Fatalf("imported up: %+v", got)
	}
	name := got[0].Name
	sw.down.Store(true)
	a.SyncPeers(context.Background())
	st := a.getState(name)
	if a.healthy(name) || st == nil || !strings.HasPrefix(st.Err, "peer bravo: ") {
		t.Fatalf("a peer that went away must mark its models down: %+v", st)
	}
	if ps := a.PeerStatuses(); ps[0].OK || ps[0].Err == "" || ps[0].Models != 1 {
		t.Fatalf("models are kept while the peer is away: %+v", ps[0])
	}
	sw.down.Store(false)
	a.SyncPeers(context.Background())
	if got := flImported(a, "bravo"); len(got) != 1 || got[0].Name != name || !a.healthy(name) {
		t.Fatalf("the models must come back by themselves: %+v", got)
	}
}

func TestFleet_Peers_ModelNoLongerOfferedIsRemoved(t *testing.T) {
	flEnv(t)
	a, b, _, _, up := flTwoFleets(t, nil)
	cfg := b.Config()
	cfg.Endpoints = append(cfg.Endpoints, config.Endpoint{Name: "dino-qwen", Node: "dino", URL: up.URL + "/v1", Model: "qwen"})
	if err := b.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	a.SyncPeers(context.Background())
	if n := len(flImported(a, "bravo")); n != 2 {
		t.Fatalf("two models expected, got %d", n)
	}
	cfg = b.Config()
	cfg.Endpoints = cfg.Endpoints[:1]
	if err := b.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	a.SyncPeers(context.Background())
	if refs := flRefs(flImported(a, "bravo")); len(refs) != 1 || refs[0] != "dino/gemma" {
		t.Fatalf("qwen must be gone: %v", refs)
	}
	if a.getState("bravo-dino-qwen") != nil {
		t.Fatal("the dropped model's health must be forgotten")
	}
}

func TestFleet_Peers_RemovePeerDropsItsModels(t *testing.T) {
	flEnv(t)
	a, _, _, _, up := flTwoFleets(t, nil)
	cfg := a.Config()
	cfg.Endpoints = append(cfg.Endpoints, config.Endpoint{Name: "own", Node: "laptop", URL: up.URL + "/v1", Model: "tiny"})
	if err := a.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if err := a.RemovePeer("nope"); err == nil {
		t.Fatal("removing an unknown peer must fail")
	}
	if err := a.RemovePeer("bravo"); err != nil {
		t.Fatal(err)
	}
	c := a.Config()
	if len(c.Peers) != 0 || len(c.Endpoints) != 1 || c.Endpoints[0].Name != "own" {
		t.Fatalf("after RemovePeer: %+v %+v", c.Peers, c.Endpoints)
	}
}

func TestFleet_Peers_SelfIsNoPeer(t *testing.T) {
	flEnv(t)
	a, aFront := flFleet(t, &config.Config{Name: "laptop"}, nil)
	if err := a.AddPeer(config.Peer{Name: "me", URL: aFront.URL + "/v1"}); err != nil {
		t.Fatal(err)
	}
	a.SyncPeers(context.Background())
	if ps := a.PeerStatuses(); ps[0].OK || !strings.Contains(ps[0].Err, "this fleet itself") {
		t.Fatalf("%+v", ps[0])
	}
}

func TestFleet_Peers_ChatEndToEnd(t *testing.T) {
	flEnv(t)
	rec := &flRecorder{}
	a, b, aFront, _, up := flTwoFleets(t, rec.wrap)
	code, body, hdr := flChat(t, aFront.URL, "dino/gemma", nil)
	if code != 200 || !strings.Contains(string(body), "Hel") || hdr.Get("X-Fleet-Endpoint") != "bravo-dino-gemma" {
		t.Fatalf("through A: %d %q via %q", code, body, hdr.Get("X-Fleet-Endpoint"))
	}
	if s := up.seen(); len(s) != 1 || s[0] != "gemma" {
		t.Fatalf("the model server must get the owner's plain model id: %v", s)
	}
	r := rec.find("/v1/chat/completions")
	if len(r) != 1 || r[0].Header.Get(headerPath) != a.selfID() || r[0].Header.Get("X-Fleet-Client") != "fleet:laptop" {
		t.Fatalf("headers at B: %+v", r)
	}
	var rows []UsageRow
	flWait(t, "B's usage", func() bool { rows = b.Usage("client", "", time.Hour); return len(rows) > 0 })
	if rows[0].Key != "fleet:laptop" {
		t.Fatalf("B must count the request under fleet:laptop: %+v", rows)
	}
	flWait(t, "A's own statistics", func() bool { lv, ok := a.live.get("bravo-dino-gemma"); return ok && lv.Requests == 1 })
}

func TestFleet_Peers_ExportCarriesInfoAndStrain(t *testing.T) {
	flEnv(t)
	up := flUpstream(t)
	b, bFront := flFleet(t, &config.Config{Name: "bravo", Endpoints: []config.Endpoint{{Name: "g", Node: "dino", URL: up.URL + "/v1", Model: "gemma"}}}, nil)
	loaded := false
	b.warm.set("g", ModelInfo{Loaded: &loaded, Size: 5 << 30, Params: "8B"})
	b.strain.set("dino", NodeStrain{Strain: hostinfo.Strain{At: time.Now(), MemFull: 25, Reasons: []string{"memory stalls 25%"}}, Source: "this machine"}, false)
	// on the exporter
	code, body, _ := flDo(t, "GET", bFront.URL+"/fleet/export", "", nil)
	var ex struct{ Models []ExportModel }
	if code != 200 || json.Unmarshal(body, &ex) != nil || len(ex.Models) != 1 || ex.Models[0].Info == nil || ex.Models[0].Strain == nil {
		t.Fatalf("export: %s", body)
	}
	if ex.Models[0].Info.Size != 5<<30 || ex.Models[0].Strain.MemFull != 25 {
		t.Fatalf("export: %+v %+v", ex.Models[0].Info, ex.Models[0].Strain)
	}
	// on the importer
	a, _ := flFleet(t, &config.Config{Name: "laptop"}, nil)
	if err := a.AddPeer(config.Peer{Name: "bravo", URL: bFront.URL + "/v1"}); err != nil {
		t.Fatal(err)
	}
	a.SyncPeers(context.Background())
	var es *EpStatus
	for _, e := range a.Status().Endpoints {
		if e.Peer == "bravo" {
			e := e
			es = &e
		}
	}
	if es == nil || es.Info == nil || es.Info.Size != 5<<30 || es.Info.Params != "8B" || es.Info.Loaded == nil || *es.Info.Loaded {
		t.Fatalf("Info on the importer: %+v", es)
	}
	if len(es.Strained) != 1 || es.Strained[0] != "memory stalls 25%" {
		t.Fatalf("Strained on the importer: %+v", es.Strained)
	}
	a.strain.mu.Lock()
	ns, ok := a.strain.peer["dino"]
	a.strain.mu.Unlock()
	if !ok || ns.Source != "⇄ bravo" {
		t.Fatalf("peer strain map: %+v %v", ns, ok)
	}
	if !a.cold(es.Name) || !a.strainedNode("dino") {
		t.Fatal("routing must see the imported model as cold and its machine as strained")
	}
}

// ---- host readings ----

func flBench(score int) hostinfo.Bench {
	return hostinfo.Bench{At: time.Now(), Fingerprint: "fp-test", Hardware: "test box", Score: score, RAMTotal: 64 << 30,
		RAMBW: 50, FastMem: 51 << 30, FastBW: 50, FastWhere: "RAM"}
}

func TestFleet_Host_LocalReport(t *testing.T) {
	state := flEnv(t)
	flWriteJSON(t, filepath.Join(state, "machine.json"), flBench(42))
	flWriteJSON(t, filepath.Join(state, "probe.json"), map[string]ProbeResult{
		"dino-gemma": {At: time.Now(), Found: []string{"chat", "tools"}}, "ghost": {Found: []string{"chat"}}})
	_, _, _, bFront, _ := flTwoFleets(t, nil)
	code, body, _ := flDo(t, "GET", bFront.URL+"/fleet/host", "", nil)
	var r HostReport
	if code != 200 || json.Unmarshal(body, &r) != nil {
		t.Fatalf("host: %d %s", code, body)
	}
	if r.Node != hostLabel() || r.Source != "this machine" || r.Fleet != "bravo" || r.Version != Version || r.Err != "" || r.At.IsZero() {
		t.Fatalf("report: %+v", r)
	}
	if r.Static.Threads <= 0 || r.Static.OS == "" || r.Now.RAMTotal <= 0 || r.History == nil {
		t.Fatalf("snapshot shape: %+v", r.Snapshot)
	}
	if r.Bench == nil || r.Bench.Score != 42 || r.Benching {
		t.Fatalf("bench: %+v", r.Bench)
	}
	if len(r.Probes) != 1 || !flHas(r.Probes["gemma"].Found, "tools") {
		t.Fatalf("probes by model (own models only): %+v", r.Probes)
	}
}

func TestFleet_Host_ThroughPeer(t *testing.T) {
	flEnv(t)
	rec := &flRecorder{}
	a, _, aFront, _, _ := flTwoFleets(t, rec.wrap)
	r := a.HostOf(context.Background(), "dino", nil)
	if r.Err != "" || r.Source != "⇄ bravo" || r.Fleet != "bravo" || r.Node != "dino" || r.Static.Threads <= 0 {
		t.Fatalf("through the peer: %+v", r)
	}
	q := rec.find("/fleet/host")
	if len(q) != 1 || q[0].URL.Query().Get("node") != "dino" || q[0].Header.Get(headerPath) != a.selfID() {
		t.Fatalf("question at B: %+v", q)
	}
	// over A's HTTP API too
	code, body, _ := flDo(t, "GET", aFront.URL+"/fleet/host?node=dino", "", nil)
	if code != 200 || !strings.Contains(string(body), `"source":"⇄ bravo"`) {
		t.Fatalf("GET /fleet/host?node=dino: %d %s", code, body)
	}
	// a node nobody knows
	if r := a.HostOf(context.Background(), "nowhere", nil); !strings.Contains(r.Err, "no route to a fleet on nowhere") {
		t.Fatalf("unknown node: %+v", r)
	}
}

// flHostPeer is a fake fleet that answers /fleet/host with the given status (counting the questions).
func flHostPeer(t *testing.T, status int, hits *atomic.Int32, lastPath *atomic.Value) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fleet/host" {
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		if lastPath != nil {
			lastPath.Store(r.Header.Get(headerPath))
		}
		if status != 200 {
			http.Error(w, "nope", status)
			return
		}
		fmt.Fprint(w, `{"node":"x","fleet":"fake","source":"this machine","static":{"threads":4}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// flHandPeer: a fleet with one hand-made peer endpoint on node "remote1" behind peer "fake" at url.
func flHandPeer(t *testing.T, url string) *Server {
	a, _ := flFleet(t, &config.Config{Name: "laptop", Peers: []config.Peer{{Name: "fake", URL: url}},
		Endpoints: []config.Endpoint{{Name: "fake-remote1-m", Node: "remote1", Model: "m", URL: url, Peer: "fake", Path: []string{"fakeid"}}}}, nil)
	return a
}

func TestFleet_Host_OldFleetIsTooOld(t *testing.T) {
	flEnv(t)
	for _, st := range []int{404, 405} {
		var hits atomic.Int32
		srv := flHostPeer(t, st, &hits, nil)
		a := flHandPeer(t, srv.URL+"/v1")
		r := a.HostOf(context.Background(), "remote1", nil)
		if hits.Load() != 1 || r.Source != "⇄ fake" || !strings.Contains(r.Err, "too old to report its machine") || !strings.Contains(r.Err, "remote1") {
			t.Fatalf("http %d: %+v", st, r)
		}
	}
	if m := hostErr(errOld, "n"); !strings.Contains(m, "fleet update") {
		t.Fatal(m)
	}
}

func TestFleet_Host_ConnectionRefusedIsNoFleet(t *testing.T) {
	flEnv(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close() // nothing listens there now
	a := flHandPeer(t, "http://"+addr+"/v1")
	r := a.HostOf(context.Background(), "remote1", nil)
	if !strings.Contains(r.Err, "no fleet runs on remote1") {
		t.Fatalf("%+v", r)
	}
	if m := hostErr(io.EOF, "n"); !strings.Contains(m, "no fleet runs on n") {
		t.Fatal(m)
	}
	if m := hostErr(fmt.Errorf("something else"), "n"); m != "something else" {
		t.Fatal(m)
	}
}

func TestFleet_Host_LoopIs508(t *testing.T) {
	flEnv(t)
	a, aFront := flFleet(t, &config.Config{Name: "laptop"}, nil)
	code, _, _ := flDo(t, "GET", aFront.URL+"/fleet/host", "", map[string]string{headerPath: "zz," + a.selfID()})
	if code != 508 {
		t.Fatalf("a question that already passed this fleet: %d, want 508", code)
	}
}

func TestFleet_Host_CacheOneAndAHalfSeconds(t *testing.T) {
	flEnv(t)
	var hits atomic.Int32
	var lastPath atomic.Value
	srv := flHostPeer(t, 200, &hits, &lastPath)
	a := flHandPeer(t, srv.URL+"/v1")
	ctx := context.Background()
	r1 := a.HostOf(ctx, "remote1", nil)
	r2 := a.HostOf(ctx, "remote1", nil)
	if hits.Load() != 1 || r1.Source != "⇄ fake" || r2.Fleet != "fake" || r1.Node != "remote1" {
		t.Fatalf("two quick questions must be one fetch: %d %+v", hits.Load(), r2)
	}
	if p, _ := lastPath.Load().(string); p != a.selfID() {
		t.Fatalf("the question must carry this fleet's id: %q", p)
	}
	a.hosts.mu.Lock()
	r := a.hosts.m["remote1"]
	r.At = time.Now().Add(-hostFresh - time.Millisecond)
	a.hosts.m["remote1"] = r
	a.hosts.mu.Unlock()
	a.HostOf(ctx, "remote1", nil)
	if hits.Load() != 2 {
		t.Fatalf("a stale entry must be fetched again: %d", hits.Load())
	}
}

// A question whose only route is skipped (the peer is already on its path) should say so, and must not
// leave an empty answer in the cache for the next, ordinary question.
func TestFleet_Host_LoopSkippedRouteSaysWhy(t *testing.T) {
	flEnv(t)
	var hits atomic.Int32
	srv := flHostPeer(t, 200, &hits, nil)
	a := flHandPeer(t, srv.URL+"/v1")
	a.setPeerRT("fake", peerRT{id: "fakeid", ok: true})
	r := a.HostOf(context.Background(), "remote1", []string{"fakeid"})
	if hits.Load() != 0 {
		t.Fatal("the peer on the path must not be asked")
	}
	if r.Err == "" {
		t.Fatal("host.go fetchHost returns an empty HostReport (no Err, no Source) when every route is skipped for the loop/hop check")
	}
}

func TestFleet_Host_LoopedAnswerNotCachedForOthers(t *testing.T) {
	flEnv(t)
	var hits atomic.Int32
	srv := flHostPeer(t, 200, &hits, nil)
	a := flHandPeer(t, srv.URL+"/v1")
	a.setPeerRT("fake", peerRT{id: "fakeid", ok: true})
	a.HostOf(context.Background(), "remote1", []string{"fakeid"}) // looped: the peer is skipped
	r := a.HostOf(context.Background(), "remote1", nil)           // an ordinary question right after
	if hits.Load() != 1 || r.Err != "" || r.Source != "⇄ fake" {
		t.Fatalf("%d %+v: host.go HostOf caches by node only — the empty answer to a looped question is served to the next ordinary one for 1.5 s", hits.Load(), r)
	}
}

// ---- pairing ----

var flSixDigits = regexp.MustCompile(`^\d{6}$`)

func TestFleet_Pair_CodeShape(t *testing.T) {
	flEnv(t)
	s, _ := flFleet(t, &config.Config{Name: "dino", Listen: "127.0.0.1:4800"}, nil)
	pc := s.NewPairCode()
	if !flSixDigits.MatchString(pc.Code) {
		t.Fatalf("code %q", pc.Code)
	}
	if d := time.Until(pc.Until); d < pairValid-time.Minute || d > pairValid {
		t.Fatalf("valid for %v", d)
	}
	if !strings.Contains(pc.Note, "listen: 0.0.0.0:4800") {
		t.Fatalf("a loopback fleet must be told to open its port: %q", pc.Note)
	}
	for _, u := range pc.URLs {
		if !strings.HasPrefix(u, "http://") || !strings.HasSuffix(u, ":4800/v1") {
			t.Fatalf("url %q", u)
		}
	}
	// a new code replaces the old one
	pc2 := s.NewPairCode()
	if pc2.Code != pc.Code {
		if _, code, _ := s.acceptPair(pairRequest{Code: pc.Code, ID: "x"}); code != 403 {
			t.Fatalf("the previous code must stop working: %d", code)
		}
	}
}

func TestFleet_Pair_FiveWrongCodesEndIt(t *testing.T) {
	flEnv(t)
	s, _ := flFleet(t, &config.Config{Name: "dino"}, nil)
	pc := s.NewPairCode()
	wrong := "000000"
	if pc.Code == wrong {
		wrong = "111111"
	}
	for i := 1; i <= 4; i++ {
		if _, code, err := s.acceptPair(pairRequest{Code: wrong, ID: "a"}); code != 403 || !strings.Contains(err.Error(), "wrong") {
			t.Fatalf("try %d: %d %v", i, code, err)
		}
	}
	if s.pair.tries != 4 {
		t.Fatalf("tries %d", s.pair.tries)
	}
	// four wrong tries: the right one still works
	if _, code, err := s.acceptPair(pairRequest{Code: " " + pc.Code + " ", ID: "a", Name: "laptop"}); code != 200 {
		t.Fatalf("right code after four wrong: %d %v", code, err)
	}
	pc = s.NewPairCode()
	if s.pair.tries != 0 {
		t.Fatal("a new code starts with no tries")
	}
	for i := 0; i < pairMaxTries; i++ {
		s.acceptPair(pairRequest{Code: wrong, ID: "a"})
	}
	if _, code, err := s.acceptPair(pairRequest{Code: pc.Code, ID: "a"}); code != 403 || !strings.Contains(err.Error(), "no pairing code") {
		t.Fatalf("five wrong codes must end the code: %d %v", code, err)
	}
}

func TestFleet_Pair_ExpiredAndOneUse(t *testing.T) {
	flEnv(t)
	s, _ := flFleet(t, &config.Config{Name: "dino"}, nil)
	if _, code, _ := s.acceptPair(pairRequest{Code: "123456", ID: "a"}); code != 403 {
		t.Fatalf("no code open: %d", code)
	}
	pc := s.NewPairCode()
	s.pair.mu.Lock()
	s.pair.until = time.Now().Add(-time.Second)
	s.pair.mu.Unlock()
	if _, code, err := s.acceptPair(pairRequest{Code: pc.Code, ID: "a"}); code != 403 || !strings.Contains(err.Error(), "no pairing code") {
		t.Fatalf("expired: %d %v", code, err)
	}
	pc = s.NewPairCode()
	a1, code, err := s.acceptPair(pairRequest{Code: pc.Code, ID: "a", Name: "laptop"})
	if code != 200 || err != nil || !strings.HasPrefix(a1.Key, "fleet-") || a1.ID != s.selfID() || a1.Name != "dino" {
		t.Fatalf("first use: %d %v %+v", code, err, a1)
	}
	if _, code, _ := s.acceptPair(pairRequest{Code: pc.Code, ID: "b"}); code != 403 {
		t.Fatalf("second use: %d", code)
	}
	if len(s.Config().Paired) != 1 {
		t.Fatalf("paired: %+v", s.Config().Paired)
	}
}

func TestFleet_Pair_HashOnlyAndRepairReplaces(t *testing.T) {
	flEnv(t)
	s, _ := flFleet(t, &config.Config{Name: "dino"}, nil)
	pc := s.NewPairCode()
	k1, _, err := s.acceptPair(pairRequest{Code: pc.Code, ID: "laptop-id", Name: "laptop"})
	if err != nil {
		t.Fatal(err)
	}
	p := s.Config().Paired
	if len(p) != 1 || p[0].ID != "laptop-id" || p[0].Name != "laptop" || p[0].KeyHash != keyHash(k1.Key) || p[0].At.IsZero() {
		t.Fatalf("paired: %+v", p)
	}
	raw, _ := os.ReadFile(s.path)
	if strings.Contains(string(raw), k1.Key) || !strings.Contains(string(raw), keyHash(k1.Key)) {
		t.Fatal("only the sha256 of the key may be stored")
	}
	if !s.pairedKey(k1.Key) || s.pairedKey("") || s.pairedKey("fleet-other") || s.pairedKey(keyHash(k1.Key)) {
		t.Fatal("pairedKey")
	}
	pc = s.NewPairCode()
	k2, _, err := s.acceptPair(pairRequest{Code: pc.Code, ID: "laptop-id", Name: "laptop2"})
	if err != nil {
		t.Fatal(err)
	}
	if p := s.Config().Paired; len(p) != 1 || p[0].Name != "laptop2" {
		t.Fatalf("pairing again must replace: %+v", p)
	}
	if s.pairedKey(k1.Key) || !s.pairedKey(k2.Key) {
		t.Fatal("the old key must stop working, the new one work")
	}
}

func TestFleet_Pair_V1AuthAcceptsPairedKey(t *testing.T) {
	flEnv(t)
	t.Setenv("FL_TEST_API_KEY", "the-real-key")
	s, _ := flFleet(t, &config.Config{Name: "dino", APIKeyEnv: "FL_TEST_API_KEY"}, nil)
	pc := s.NewPairCode()
	k, _, err := s.acceptPair(pairRequest{Code: pc.Code, ID: "x", Name: "x"})
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	call := func(remote, tok string) int {
		req := httptest.NewRequest("GET", "/v1/models", nil)
		req.RemoteAddr = remote
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}
	lan := "10.0.1.50:1234"
	for _, c := range []struct {
		remote, tok string
		want        int
	}{{lan, "", 401}, {lan, "wrong", 401}, {lan, k.Key, 200}, {lan, "the-real-key", 200}, {"127.0.0.1:5555", "", 200}} {
		if got := call(c.remote, c.tok); got != c.want {
			t.Errorf("%s with %q: %d, want %d", c.remote, c.tok, got, c.want)
		}
	}
}

func TestFleet_Pair_EndToEnd(t *testing.T) {
	flEnv(t)
	t.Setenv("FL_B_KEY", "b-secret")
	up := flUpstream(t)
	// B wants a key and every caller looks like another machine
	b, bFront := flFleet(t, &config.Config{Name: "bravo", APIKeyEnv: "FL_B_KEY",
		Endpoints: []config.Endpoint{{Name: "g", Node: "dino", URL: up.URL + "/v1", Model: "gemma"}}}, flFromLAN)
	a, aFront := flFleet(t, &config.Config{Name: "laptop"}, nil)
	if code, _, _ := flDo(t, "GET", bFront.URL+"/fleet/export", "", nil); code != 401 {
		t.Fatalf("B must want a key: %d", code)
	}
	// a wrong code is refused and adds nothing
	pc := b.NewPairCode()
	wrong := "000000"
	if pc.Code == wrong {
		wrong = "111111"
	}
	code, body, _ := flDo(t, "POST", aFront.URL+"/fleet/peers", `{"add":{"name":"bravo","url":"`+bFront.URL+`/v1"},"pair":"`+wrong+`"}`, nil)
	if code != 400 || !strings.Contains(string(body), "wrong pairing code") || len(a.Config().Peers) != 0 {
		t.Fatalf("wrong code: %d %s", code, body)
	}
	code, body, _ = flDo(t, "POST", aFront.URL+"/fleet/peers", `{"add":{"name":"bravo","url":"`+bFront.URL+`/v1"},"pair":"`+pc.Code+`"}`, nil)
	if code != 200 {
		t.Fatalf("pair: %d %s", code, body)
	}
	ps := a.Config().Peers
	if len(ps) != 1 || !strings.HasPrefix(ps[0].Key, "fleet-") {
		t.Fatalf("A must keep the key it got: %+v", ps)
	}
	if bp := b.Config().Paired; len(bp) != 1 || bp[0].ID != a.selfID() || bp[0].Name != "laptop" || bp[0].KeyHash != keyHash(ps[0].Key) {
		t.Fatalf("B's paired list: %+v", bp)
	}
	a.SyncPeers(context.Background())
	if st := a.PeerStatuses(); !st[0].OK || st[0].Models != 1 {
		t.Fatalf("the paired peer must sync: %+v", st[0])
	}
	imp := flImported(a, "bravo")
	if len(imp) != 1 || imp[0].Key != ps[0].Key {
		t.Fatalf("imported endpoints must carry the key: %+v", imp)
	}
	if code, body, _ := flChat(t, aFront.URL, "dino/gemma", nil); code != 200 {
		t.Fatalf("a request through the paired peer must authenticate: %d %s", code, body)
	}
}

func TestFleet_Pair_NeedsURL(t *testing.T) {
	flEnv(t)
	a, aFront := flFleet(t, &config.Config{Name: "laptop"}, nil)
	code, body, _ := flDo(t, "POST", aFront.URL+"/fleet/peers", `{"add":{"name":"bravo","ssh":"bravo"},"pair":"123456"}`, nil)
	if code != 400 || !strings.Contains(string(body), "pairing needs the peer's address") || len(a.Config().Peers) != 0 {
		t.Fatalf("%d %s", code, body)
	}
}

// ---- capability probe ----

// flProbeServer is a fake OpenAI server: "smart" streams with reasoning, calls tools and sees red;
// "blind" streams plainly, writes tool calls as text and rejects images; /embeddings returns 3 numbers.
func flProbeServer(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var in map[string]any
		json.Unmarshal(raw, &in)
		model, _ := in["model"].(string)
		switch {
		case strings.HasSuffix(r.URL.Path, "/embeddings"):
			fmt.Fprint(w, `{"data":[{"embedding":[0.1,0.2,0.3]}]}`)
		case in["stream"] == true:
			w.Header().Set("Content-Type", "text/event-stream")
			send := func(s string) { fmt.Fprint(w, "data: "+s+"\n\n"); w.(http.Flusher).Flush() }
			if model == "smart" {
				send(`{"choices":[{"delta":{"reasoning_content":"the sky..."}}]}`)
			}
			for i := 0; i < 10; i++ {
				time.Sleep(2 * time.Millisecond)
				send(`{"choices":[{"delta":{"content":"w "}}]}`)
			}
			send(`{"choices":[],"usage":{"completion_tokens":11}}`)
			send(`[DONE]`)
		case in["tools"] != nil:
			if model == "smart" {
				fmt.Fprint(w, `{"choices":[{"message":{"tool_calls":[{"function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]}}]}`)
			} else {
				fmt.Fprint(w, `{"choices":[{"message":{"content":"get_weather(Paris)"}}]}`)
			}
		case bytes.Contains(raw, []byte("image_url")):
			if model == "smart" {
				fmt.Fprint(w, `{"choices":[{"message":{"content":"Red."}}]}`)
			} else {
				w.WriteHeader(400)
				fmt.Fprint(w, `{"error":{"message":"image input is not supported by this model"}}`)
			}
		default:
			http.Error(w, "?", 500)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func flProbeDone(t *testing.T, s *Server) {
	t.Helper()
	flWait(t, "the probe", func() bool { return !s.ProbeStatus().Running })
}

func TestFleet_Probe_FindsCapsAndPersists(t *testing.T) {
	state := flEnv(t)
	srv := flProbeServer(t)
	u := srv.URL + "/v1"
	s, _ := flFleet(t, &config.Config{Name: "laptop", Endpoints: []config.Endpoint{
		{Name: "smart", Node: "box", URL: u, Model: "smart"},
		{Name: "blind", Node: "box", URL: u, Model: "blind", TPS: 42, Caps: []string{"chat", "vision"}},
		{Name: "emb", Node: "box", URL: u, Model: "nomic-embed-text"},
	}}, nil)
	if !s.isSelfNode("box") {
		t.Fatal("a node reached at 127.0.0.1 is this machine")
	}
	if err := s.Probe([]string{"box"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Probe([]string{"box"}); err == nil || !strings.Contains(err.Error(), "already running") {
		// the probe may already be done on a fast machine; only a concurrent run must be refused
		if s.ProbeStatus().Running {
			t.Fatalf("a second concurrent probe must be refused: %v", err)
		}
	}
	flProbeDone(t, s)
	if st := s.ProbeStatus(); st.Done != 3 || st.Total != 3 || len(st.Current) != 0 {
		t.Fatalf("status: %+v", st)
	}
	res := s.ProbeResults()
	sm, bl, em := res["smart"], res["blind"], res["emb"]
	for _, c := range []string{"chat", "thinking", "tools", "vision"} {
		if !flHas(sm.Found, c) {
			t.Errorf("smart: %s not found: %+v", c, sm)
		}
	}
	if sm.Err != "" || sm.TPS <= 0 || len(sm.Missing) != 0 {
		t.Errorf("smart: %+v", sm)
	}
	if !flHas(bl.Found, "chat") || !flHas(bl.Missing, "vision") || !flHas(bl.Missing, "tools") || !flHas(bl.Missing, "thinking") {
		t.Errorf("blind: %+v", bl)
	}
	if !strings.Contains(bl.Why["vision"], "image input is not supported by this model") || !strings.Contains(bl.Why["vision"], "http 400") {
		t.Errorf("the runtime's own message must be kept: %q", bl.Why["vision"])
	}
	if bl.Why["tools"] != "wrote the call as text instead of a tool call" {
		t.Errorf("tools why: %q", bl.Why["tools"])
	}
	if !flHas(em.Found, "embed") || em.Dim != 3 || em.Err != "" {
		t.Errorf("emb: %+v", em)
	}
	// keepProbeSpeeds: only endpoints without a speed get the probe's
	c := s.Config()
	if e := c.Endpoint("smart"); e.TPS != sm.TPS {
		t.Errorf("smart TPS %v, want the probe's %v", e.TPS, sm.TPS)
	}
	if e := c.Endpoint("blind"); e.TPS != 42 {
		t.Errorf("a known speed must not be overwritten: %v", e.TPS)
	}
	// persisted and reloaded by a new daemon
	if _, err := os.Stat(filepath.Join(state, "probe.json")); err != nil {
		t.Fatal(err)
	}
	s2, err := New(s.path)
	if err != nil {
		t.Fatal(err)
	}
	r2 := s2.ProbeResults()
	if len(r2) != 3 || r2["emb"].Dim != 3 || !flHas(r2["smart"].Found, "vision") {
		t.Fatalf("reloaded: %+v", r2)
	}
}

func TestFleet_Probe_RefusesOtherMachines(t *testing.T) {
	flEnv(t)
	s, _ := flFleet(t, &config.Config{Name: "laptop", Endpoints: []config.Endpoint{
		{Name: "far", Node: "dino", URL: "http://10.255.255.1:1/v1", Model: "m"}}}, nil)
	if s.isSelfNode("dino") {
		t.Fatal("a node at another address is not this machine")
	}
	if err := s.Probe([]string{"dino"}); err == nil || !strings.Contains(err.Error(), "only this machine's models") {
		t.Fatalf("%v", err)
	}
	if err := s.Probe([]string{"nothere"}); err == nil || !strings.Contains(err.Error(), "no such node") {
		t.Fatalf("%v", err)
	}
	if err := s.Probe(nil); err == nil || !strings.Contains(err.Error(), "no healthy model on this machine") {
		t.Fatalf("%v", err)
	}
}

func TestFleet_Probe_ApplyWritesCapsButNotPeerModels(t *testing.T) {
	flEnv(t)
	srv := flProbeServer(t)
	u := srv.URL + "/v1"
	s, _ := flFleet(t, &config.Config{Name: "laptop",
		Peers: []config.Peer{{Name: "pp", URL: "http://10.255.255.1:1/v1"}},
		Endpoints: []config.Endpoint{
			{Name: "smart", Node: "box", URL: u, Model: "smart"},
			{Name: "blind", Node: "box", URL: u, Model: "blind", Caps: []string{"chat", "vision"}},
			{Name: "pp-far-m", Node: "far", Model: "m", URL: "http://10.255.255.1:1/v1", Peer: "pp", Path: []string{"zz"}, Caps: []string{"chat"}},
		}}, nil)
	if err := s.Probe(nil); err != nil {
		t.Fatal(err)
	}
	flProbeDone(t, s)
	if _, ok := s.ProbeResults()["pp-far-m"]; ok {
		t.Fatal("a peer model must not be probed here")
	}
	// a probe result for the peer model (e.g. left from before it was imported) must not be applied
	s.probes.mu.Lock()
	s.probes.results["pp-far-m"] = ProbeResult{Found: []string{"chat", "vision", "tools"}}
	s.probes.mu.Unlock()
	ch, err := s.ApplyProbe(nil)
	if err != nil || len(ch) != 2 {
		t.Fatalf("changes: %v %v", ch, err)
	}
	c := s.Config()
	if got := strings.Join(c.Endpoint("smart").Caps, ","); got != "chat,vision,tools,thinking" {
		t.Errorf("smart caps: %s", got)
	}
	if got := strings.Join(c.Endpoint("blind").Caps, ","); got != "chat" {
		t.Errorf("blind caps: %s", got)
	}
	if got := strings.Join(c.Endpoint("pp-far-m").Caps, ","); got != "chat" {
		t.Errorf("peer model caps must be left alone: %s", got)
	}
	if ch, _ := s.ApplyProbe(nil); len(ch) != 0 {
		t.Errorf("applying twice changes nothing: %v", ch)
	}
}

// ---- advice ----

const flGiB = 1 << 30

func flBenchOf(where string, fastBW float64, fastGiB int64) *hostinfo.Bench {
	b := &hostinfo.Bench{RAMTotal: 64 * flGiB, RAMBW: 50, FastMem: fastGiB * flGiB, FastBW: fastBW, FastWhere: where, Score: 40}
	return b
}

func flHosts(m map[string]*hostinfo.Bench) map[string]HostReport {
	out := map[string]HostReport{}
	for n, b := range m {
		out[n] = HostReport{Node: n, Bench: b}
	}
	return out
}

func flEp(node, model string, gib int64) EpStatus {
	return EpStatus{Endpoint: config.Endpoint{Name: node + "-" + model, Node: node, Model: model}, Info: &ModelInfo{Size: gib * flGiB}}
}

func flMeasured(e EpStatus, tps float64, samples int) EpStatus {
	e.TPS, e.TPSSrc, e.Samples = tps, "live", samples
	return e
}

func TestFleet_Advice_ParamsB(t *testing.T) {
	for in, want := range map[string]float64{"7.5B": 7.5, "74B": 74, " 8b ": 8, "1.2T": 1200, "500M": 0.5, "abc": 0, "7.5": 0, "": 0, "xB": 0} {
		if got := paramsB(in); math.Abs(got-want) > 1e-9 {
			t.Errorf("paramsB(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestFleet_Advice_WeightsMoE(t *testing.T) {
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
	e := flEp("n", "dense", 10)
	if gb, rd := weights(e); !near(gb, 10) || !near(rd, 10) {
		t.Fatalf("dense: %v %v", gb, rd)
	}
	e = flEp("n", "moe", 18)
	e.Info.Active, e.Info.Params = 3, "30B"
	if gb, rd := weights(e); !near(gb, 18) || !near(rd, 1.8) {
		t.Fatalf("Info.Active: %v %v", gb, rd)
	}
	e = flEp("n", "qwen-74b-a9b", 37)
	e.Info.Params = "74B"
	if _, rd := weights(e); !near(rd, 37*9.0/74) {
		t.Fatalf("active share from the name: %v", rd)
	}
	e.Info.Params = "" // total unknown: everything is read
	if _, rd := weights(e); !near(rd, 37) {
		t.Fatalf("no total: %v", rd)
	}
	e = flEp("n", "odd", 10)
	e.Info.Active, e.Info.Params = 40, "30B" // nonsense active > total: ignored
	if _, rd := weights(e); !near(rd, 10) {
		t.Fatalf("active > total: %v", rd)
	}
	if gb, rd := weights(EpStatus{}); gb != 0 || rd != 0 {
		t.Fatal("unknown size")
	}
}

func TestFleet_Advice_SuggestBestTarget(t *testing.T) {
	slow := flBenchOf("RAM", 50, 51) // 5 GiB model: 0.55·50/5 = 5.5 tok/s
	hosts := flHosts(map[string]*hostinfo.Bench{"slow": slow,
		"gpu": flBenchOf("GPU", 1000, 24), // 130 tok/s
		"mid": flBenchOf("GPU", 300, 24),  // 39 tok/s
		"nob": nil})
	got := Suggest([]EpStatus{flEp("slow", "m", 5)}, hosts)
	if len(got) != 1 {
		t.Fatalf("%+v", got)
	}
	g := got[0]
	if g.Model != "m" || g.From != "slow" || g.To != "gpu" || g.FromTPS != 5.5 || g.ToTPS != 130 || g.FromWhere != "RAM" || g.ToWhere != "GPU" {
		t.Fatalf("best move: %+v", g)
	}
	// gpu already has the model (":latest" is the same model): mid is next best
	got = Suggest([]EpStatus{flEp("slow", "m", 5), flEp("gpu", "m:latest", 5)}, hosts)
	if len(got) != 1 || got[0].To != "mid" || got[0].From != "slow" {
		t.Fatalf("a node that has the model is skipped: %+v", got)
	}
	// unknown size or a node without a benchmark: nothing
	if got := Suggest([]EpStatus{{Endpoint: config.Endpoint{Node: "slow", Model: "x"}}, flEp("nob", "y", 5)}, hosts); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
}

func TestFleet_Advice_SuggestThresholds(t *testing.T) {
	hosts := flHosts(map[string]*hostinfo.Bench{"slow": flBenchOf("RAM", 50, 51),
		"near": flBenchOf("GPU", 60, 24)}) // 7.8 tok/s for 5 GiB: < 1.5 × 5.5
	if got := Suggest([]EpStatus{flEp("slow", "m", 5)}, hosts); len(got) != 0 {
		t.Fatalf("7.8 vs 5.5 is under 1.5×: %+v", got)
	}
	// measured 4 tok/s (3 samples): 7.8 is ≥ 1.5× and +3.8 → suggested, from the measured figure
	got := Suggest([]EpStatus{flMeasured(flEp("slow", "m", 5), 4, 3)}, hosts)
	if len(got) != 1 || got[0].FromWhere != "measured" || got[0].FromTPS != 4 || got[0].To != "near" {
		t.Fatalf("measured: %+v", got)
	}
	// only 2 samples: the estimate (5.5) is used, and 7.8 is not enough
	if got := Suggest([]EpStatus{flMeasured(flEp("slow", "m", 5), 4, 2)}, hosts); len(got) != 0 {
		t.Fatalf("2 samples must not count as measured: %+v", got)
	}
	// ≥ 1.5× but less than 3 tok/s faster: measured 2, target 6.5 (+4.5) ok; target 4.5 (+2.5) not
	h2 := flHosts(map[string]*hostinfo.Bench{"slow": flBenchOf("RAM", 50, 51), "t": flBenchOf("GPU", 4.5*5/0.65, 24)})
	if got := Suggest([]EpStatus{flMeasured(flEp("slow", "m", 5), 2, 5)}, h2); len(got) != 0 {
		t.Fatalf("+2.5 tok/s is under the 3 tok/s floor: %+v", got)
	}
}

func TestFleet_Advice_SuggestOrder(t *testing.T) {
	hosts := flHosts(map[string]*hostinfo.Bench{"slow": flBenchOf("RAM", 50, 51), "gpu": flBenchOf("GPU", 1000, 24)})
	eps := []EpStatus{
		flMeasured(flEp("slow", "z", 5), 50, 3), // 130/50 = 2.6×
		flEp("slow", "y", 5),                    // 130/5.5 ≈ 23.6×
		flEp("slow", "x", 5),                    // same gain as y: by name
		flEp("slow", "a", 10),                   // 65/2.8 ≈ 23.2×
	}
	got := Suggest(eps, hosts)
	var order []string
	for _, s := range got {
		order = append(order, s.Model)
	}
	if strings.Join(order, ",") != "x,y,a,z" {
		t.Fatalf("order by gain, then name: %v", order)
	}
}

func TestFleet_Advice_Calibrate(t *testing.T) {
	b := flBenchOf("RAM", 50, 51) // 5 GiB → 5.5, 10 GiB → 2.75 estimated
	eps := []EpStatus{
		flMeasured(flEp("n", "a", 5), 11, 3),    // 2
		flMeasured(flEp("n", "b", 10), 2.75, 4), // 1
		flMeasured(flEp("n", "c", 5), 2.75, 3),  // 0.5
		flMeasured(flEp("n", "d", 5), 99, 2),    // too few samples
		flMeasured(flEp("other", "e", 5), 1, 9), // another node
		flEp("n", "f", 5),                       // no measurement
	}
	c, ok := Calibrate("n", eps, b)
	if !ok || c.Factor != 1 || c.Models != 3 || c.Score != 40 {
		t.Fatalf("odd count, median: %+v %v", c, ok)
	}
	eps = append(eps, flMeasured(flEp("n", "g", 5), 16.5, 3)) // 3 → ratios .5 1 2 3
	c, ok = Calibrate("n", eps, b)
	if !ok || c.Factor != 1.5 || c.Models != 4 || c.Score != 60 {
		t.Fatalf("even count, mean of the middle two: %+v %v", c, ok)
	}
	if _, ok := Calibrate("nobody", eps, b); ok {
		t.Fatal("no data must say so")
	}
	if _, ok := Calibrate("n", eps, nil); ok {
		t.Fatal("no benchmark must say so")
	}
}

// ---- machine benchmark ----

func TestFleet_HWBench_LoadsStored(t *testing.T) {
	state := flEnv(t)
	s, _ := flFleet(t, &config.Config{Name: "x"}, nil)
	if b, running := s.HWBench(); b != nil || running {
		t.Fatalf("nothing stored: %+v %v", b, running)
	}
	flWriteJSON(t, filepath.Join(state, "machine.json"), flBench(77))
	s2, _ := flFleet(t, &config.Config{Name: "y"}, nil)
	if b, _ := s2.HWBench(); b == nil || b.Score != 77 || b.Fingerprint != "fp-test" {
		t.Fatalf("stored: %+v", b)
	}
	if b, _ := s2.HWBench(); b != nil {
		b.Score = 1 // a copy: the caller cannot change the stored result
		if c, _ := s2.HWBench(); c.Score != 77 {
			t.Fatal("HWBench must return a copy")
		}
	}
	flWriteJSON(t, filepath.Join(state, "machine.json"), map[string]any{"score": 5}) // no fingerprint: not a result
	s3, _ := flFleet(t, &config.Config{Name: "z"}, nil)
	if b, _ := s3.HWBench(); b != nil {
		t.Fatalf("a result without a fingerprint is ignored: %+v", b)
	}
}

func TestFleet_HWBench_RunOnceAtATimeAndSave(t *testing.T) {
	state := flEnv(t)
	s, _ := flFleet(t, &config.Config{Name: "x"}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // a cancelled context keeps the benchmark short (the measuring loops stop at once)
	s.ctx = ctx
	if err := s.RunHWBench("test"); err != nil {
		t.Fatal(err)
	}
	if _, running := s.HWBench(); running {
		if err := s.RunHWBench("again"); err == nil || !strings.Contains(err.Error(), "already running") {
			t.Fatalf("a second run must be refused while one runs: %v", err)
		}
	}
	flWait(t, "the benchmark", func() bool { _, r := s.HWBench(); return !r })
	b, _ := s.HWBench()
	fp, _ := hostinfo.Fingerprint(nil)
	if b == nil || b.Fingerprint != fp || b.RAMTotal <= 0 || len(b.Fits) == 0 {
		t.Fatalf("result: %+v", b)
	}
	raw, err := os.ReadFile(filepath.Join(state, "machine.json"))
	var saved hostinfo.Bench
	if err != nil || json.Unmarshal(raw, &saved) != nil || saved.Fingerprint != fp {
		t.Fatalf("machine.json: %v %s", err, raw)
	}
}

// hwBenchLoop waits 20 s before it decides, so its decision is not checked here (see the _Slow test);
// a daemon that stops before that never benchmarks.
func TestFleet_HWBench_LoopStopsWithTheDaemon(t *testing.T) {
	state := flEnv(t)
	s, _ := flFleet(t, &config.Config{Name: "x"}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() { s.hwBenchLoop(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("hwBenchLoop must return when the context ends")
	}
	if b, running := s.HWBench(); b != nil || running {
		t.Fatal("no benchmark expected")
	}
	if _, err := os.Stat(filepath.Join(state, "machine.json")); err == nil {
		t.Fatal("nothing must be saved")
	}
}

// The real decision (none stored → run; same fingerprint → no run; other fingerprint → run) takes 20 s
// per case; run with FLEET_SLOW_TESTS=1.
func TestFleet_HWBench_LoopDecision_Slow(t *testing.T) {
	if os.Getenv("FLEET_SLOW_TESTS") == "" {
		t.Skip("takes 3 × 20 s (hwBenchLoop's start delay); set FLEET_SLOW_TESTS=1")
	}
	fp, _ := hostinfo.Fingerprint(nil)
	for _, c := range []struct {
		name   string
		stored string // "" none
		run    bool
	}{{"none", "", true}, {"same", fp, false}, {"changed", "other", true}} {
		t.Run(c.name, func(t *testing.T) {
			state := flEnv(t)
			if c.stored != "" {
				b := flBench(1)
				b.Fingerprint = c.stored
				flWriteJSON(t, filepath.Join(state, "machine.json"), b)
			}
			s, _ := flFleet(t, &config.Config{Name: "x"}, nil)
			cctx, cancel := context.WithCancel(context.Background())
			cancel()
			s.ctx = cctx // short benchmark
			ctx, stop := context.WithTimeout(context.Background(), 25*time.Second)
			defer stop()
			s.hwBenchLoop(ctx)
			flWait(t, "the benchmark", func() bool { _, r := s.HWBench(); return !r })
			b, _ := s.HWBench()
			ran := b != nil && b.Fingerprint == fp && b.Score != 1
			if ran != c.run {
				t.Fatalf("ran=%v, want %v (%+v)", ran, c.run, b)
			}
		})
	}
}

// ---- metrics ----

func TestFleet_Metrics_ValidPrometheusText(t *testing.T) {
	state := flEnv(t)
	flWriteJSON(t, filepath.Join(state, "machine.json"), flBench(33))
	a, _, aFront, _, _ := flTwoFleets(t, nil)
	cfg := a.Config()
	up := flUpstream(t)
	cfg.Endpoints = append(cfg.Endpoints, config.Endpoint{Name: "own", Node: "box", URL: up.URL + "/v1", Model: "tiny"})
	if err := a.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	a.strain.set("box", NodeStrain{Strain: hostinfo.Strain{At: time.Now(), MemFull: 12.5, Reasons: []string{"memory"}}}, false)
	code, body, hdr := flDo(t, "GET", aFront.URL+"/metrics", "", nil)
	if code != 200 || !strings.HasPrefix(hdr.Get("Content-Type"), "text/plain") {
		t.Fatalf("%d %s", code, hdr.Get("Content-Type"))
	}
	type fam struct {
		help, typ, samples int
		closed             bool
	}
	fams := map[string]*fam{}
	get := func(n string) *fam {
		if fams[n] == nil {
			fams[n] = &fam{}
		}
		return fams[n]
	}
	sampleRe := regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{[^}]*\})? (\S+)$`)
	cur := ""
	sc := bufio.NewScanner(bytes.NewReader(body))
	for ln := 1; sc.Scan(); ln++ {
		line := sc.Text()
		switch {
		case line == "":
			t.Errorf("line %d: empty line", ln)
		case strings.HasPrefix(line, "# HELP "):
			f := strings.Fields(line)
			x := get(f[2])
			x.help++
			if x.samples > 0 {
				t.Errorf("line %d: HELP after samples of %s", ln, f[2])
			}
		case strings.HasPrefix(line, "# TYPE "):
			f := strings.Fields(line)
			if len(f) != 4 || (f[3] != "gauge" && f[3] != "counter") {
				t.Errorf("line %d: %q", ln, line)
				continue
			}
			x := get(f[2])
			x.typ++
			if x.samples > 0 {
				t.Errorf("line %d: TYPE after samples of %s", ln, f[2])
			}
		case strings.HasPrefix(line, "#"):
			t.Errorf("line %d: unknown comment %q", ln, line)
		default:
			m := sampleRe.FindStringSubmatch(line)
			if m == nil {
				t.Errorf("line %d: not a sample: %q", ln, line)
				continue
			}
			if _, err := strconv.ParseFloat(m[3], 64); err != nil {
				t.Errorf("line %d: value %q", ln, m[3])
			}
			x := fams[m[1]]
			if x == nil || x.help != 1 || x.typ != 1 {
				t.Errorf("line %d: %s has no HELP/TYPE before it", ln, m[1])
				continue
			}
			if m[1] != cur {
				if x.closed {
					t.Errorf("line %d: samples of %s are not contiguous", ln, m[1])
				}
				if c := fams[cur]; c != nil {
					c.closed = true
				}
				cur = m[1]
			}
			x.samples++
		}
	}
	for n, f := range fams {
		if f.help != 1 || f.typ != 1 {
			t.Errorf("%s: %d HELP, %d TYPE", n, f.help, f.typ)
		}
	}
	for _, n := range []string{"fleet_endpoint_up", "fleet_host_cpu_percent", "fleet_host_ram_total_bytes", "fleet_host_ai_score",
		"fleet_node_ping_ms", "fleet_node_strained", "fleet_node_memory_stall_percent"} {
		if f := fams[n]; f == nil || f.samples == 0 {
			t.Errorf("%s: no samples", n)
		}
	}
	text := string(body)
	for _, want := range []string{`fleet_host_ai_score 33`, `fleet_node_strained{node="box"} 1`, `fleet_node_memory_stall_percent{node="box"} 12.5`,
		`fleet_endpoint_up{endpoint="bravo-dino-gemma",node="dino"} 1`} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q", want)
		}
	}
}

// ---- nodes ----

func flBare(cfg *config.Config) *Server {
	return &Server{cfg: cfg, state: map[string]*EpState{}, lims: map[string]*limiter{}, tunnels: newTunnels()}
}

func TestFleet_Nodes_ThisDeviceFirst(t *testing.T) {
	flEnv(t)
	s := flBare(&config.Config{})
	ns := s.Nodes(s.Status().Endpoints)
	if len(ns) != 1 || !ns[0].Self || ns[0].Via != "this device" || ns[0].Node != hostLabel() || ns[0].Total != 0 {
		t.Fatalf("no models: %+v", ns)
	}
	// the local node comes first even when listed last
	s = flBare(&config.Config{Endpoints: []config.Endpoint{
		{Name: "r", Node: "far", URL: "http://10.0.0.9:8080/v1", Model: "m"},
		{Name: "l", Node: "box", URL: "http://127.0.0.1:11434/v1", Model: "m"}}})
	ns = s.Nodes(s.Status().Endpoints)
	if len(ns) != 2 || ns[0].Node != "box" || !ns[0].Self || ns[0].Via != "this device" || ns[1].Node != "far" || ns[1].Via != "url 10.0.0.9" {
		t.Fatalf("%+v", ns)
	}
}

func TestFleet_Nodes_PingHistoryCapsAt30(t *testing.T) {
	flEnv(t)
	cfg := &config.Config{Endpoints: []config.Endpoint{{Name: "a", Node: "far", URL: "http://10.0.0.9:8080/v1", Model: "m"}}}
	s := flBare(cfg)
	t0 := time.Now()
	for i := 1; i <= 35; i++ {
		st := &EpState{OK: true, Latency: int64(i), Checked: t0.Add(time.Duration(i) * time.Second)}
		s.pings.record(cfg, func(string) *EpState { return st })
	}
	h := s.pings.get("far")
	if len(h) != pingKeep || h[0] != 6 || h[len(h)-1] != 35 {
		t.Fatalf("history: %v", h)
	}
	var far NodeStatus
	for _, n := range s.Nodes(s.Status().Endpoints) {
		if n.Node == "far" {
			far = n
		}
	}
	if far.PingMS != 35 || far.MinMS != 6 || far.MaxMS != 35 || far.AvgMS != (6+35)/2 || len(far.History) != 30 {
		t.Fatalf("node: %+v", far)
	}
}

func TestFleet_Nodes_NodeEndpoints(t *testing.T) {
	cfg := &config.Config{Endpoints: []config.Endpoint{
		{Name: "b2", Node: "dino"}, {Name: "a1", Node: "dino"}, {Name: "c3", Node: "vec"}, {Name: "dino", Node: "other"}}}
	if got := strings.Join(NodeEndpoints(cfg, []string{"DINO"}), ","); got != "a1,b2" {
		t.Fatalf("by node (any case): %s", got)
	}
	if got := strings.Join(NodeEndpoints(cfg, []string{"c3", "vec", "a1"}), ","); got != "a1,c3" {
		t.Fatalf("by name, no duplicates, sorted: %s", got)
	}
	if got := strings.Join(NodeEndpoints(cfg, []string{"dino"}), ","); got != "dino" {
		t.Fatalf("an endpoint name wins over a node name: %s", got)
	}
	if got := NodeEndpoints(cfg, []string{"nothing"}); len(got) != 0 {
		t.Fatalf("%v", got)
	}
}

// ---- LAN ----

func TestFleet_LAN_MDNSInfo(t *testing.T) {
	flEnv(t)
	if (flBare(&config.Config{Name: "x"})).mdnsInfo().Instance != "" {
		t.Fatal("a fleet without an id announces nothing")
	}
	for _, c := range []struct {
		listen, scope string
		port          int
		key           bool
	}{
		{"127.0.0.1:4800", "loopback", 4800, false},
		{"0.0.0.0:4711", "lan", 4711, true},
		{":4712", "lan", 4712, false},
		{"10.0.1.5:4711", "lan", 4711, false},
		{"[::1]:4711", "loopback", 4711, false},
	} {
		cfg := &config.Config{ID: "abcdef123456", Name: "dino", Listen: c.listen}
		if c.key {
			cfg.APIKeyEnv = "SOME_KEY"
		}
		in := flBare(cfg).mdnsInfo()
		if in.Instance != "dino (abcdef)" || in.Host != hostLabel() || in.Port != c.port || in.TXT["scope"] != c.scope ||
			in.TXT["id"] != "abcdef123456" || in.TXT["name"] != "dino" || in.TXT["v"] != Version {
			t.Errorf("%s: %+v", c.listen, in)
		}
		if _, has := in.TXT["key"]; has != c.key || (c.key && in.TXT["key"] != "1") {
			t.Errorf("%s: key %+v", c.listen, in.TXT)
		}
		if _, has := in.TXT["user"]; has {
			t.Errorf("%s: no user name may be announced: %+v", c.listen, in.TXT)
		}
	}
}

func TestFleet_LAN_AddCommand(t *testing.T) {
	cmd, hint := addCommand(LANFleet{Name: "Big Box", Host: "big.local", Addr: "10.0.1.7", Port: 4711, Scope: "lan"})
	if cmd != "fleet peers add big-box http://10.0.1.7:4711/v1" || hint != "" {
		t.Fatalf("%q %q", cmd, hint)
	}
	cmd, hint = addCommand(LANFleet{Name: "dino", Host: "dino.local", Addr: "10.0.1.7", Port: 4800, Scope: "loopback"})
	if cmd != "" || !strings.Contains(hint, "on dino.local set listen: 0.0.0.0:4800") {
		t.Fatalf("%q %q", cmd, hint)
	}
	cmd, hint = addCommand(LANFleet{Name: "dino", Host: "dino.local", Port: 4711, Scope: "lan"}) // heard, but no address
	if cmd != "" || hint == "" {
		t.Fatalf("%q %q", cmd, hint)
	}
	cmd, hint = addCommand(LANFleet{Name: "dino", Host: "dino.local", Addr: "fd00::7", Port: 4711, Scope: "lan", Key: true})
	if cmd != "fleet peers add dino http://[fd00::7]:4711/v1 pair=<code>" || !strings.Contains(hint, "fleet peers pair") {
		t.Fatalf("%q %q", cmd, hint)
	}
}

// LANFleets needs a running mDNS node (port 5353) to hear anything, so only the empty cases are checked.
func TestFleet_LAN_FleetsWithoutDiscovery(t *testing.T) {
	flEnv(t)
	s := flBare(&config.Config{ID: "me"})
	if got := s.LANFleets(); got != nil {
		t.Fatalf("discovery off: %v", got)
	}
	s.lan = mdns.New(s.mdnsInfo) // never run: hears nothing
	if got := s.LANFleets(); len(got) != 0 {
		t.Fatalf("nothing heard: %v", got)
	}
}

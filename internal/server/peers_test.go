package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
)

// startFleet runs a fleet's HTTP side (no background loops) over a config saved in a temp dir.
func startFleet(t *testing.T, cfg *config.Config) (*Server, *httptest.Server) {
	t.Helper()
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
	front := httptest.NewServer(s.Handler())
	t.Cleanup(front.Close)
	return s, front
}

func TestPeerNetwork(t *testing.T) {
	be := fakeBackend()
	defer be.Close()
	// B owns the model; A reaches it only through B; B also lists A as a peer (a loop).
	b, bFront := startFleet(t, &config.Config{Name: "bravo", Roles: map[string]config.Role{},
		Endpoints: []config.Endpoint{{Name: "dino-gemma", Node: "dino", URL: be.URL, Model: "gemma", Ctx: 8192}}})
	a, aFront := startFleet(t, &config.Config{Name: "laptop", Roles: map[string]config.Role{},
		Endpoints: []config.Endpoint{{Name: "own", Node: "laptop", URL: be.URL, Model: "tiny"}}})
	if err := a.AddPeer(config.Peer{Name: "bravo", URL: bFront.URL + "/v1"}); err != nil {
		t.Fatal(err)
	}
	if err := b.AddPeer(config.Peer{Name: "laptop", URL: aFront.URL + "/v1"}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	a.SyncPeers(ctx)
	b.SyncPeers(ctx)
	a.SyncPeers(ctx) // second round: A now exports bravo's model too, B must not take it back

	// A imported exactly B's model, named by node/model
	var got []config.Endpoint
	for _, e := range a.Config().Endpoints {
		if e.Peer != "" {
			got = append(got, e)
		}
	}
	if len(got) != 1 || got[0].Ref() != "dino/gemma" || got[0].Peer != "bravo" || len(got[0].Path) != 1 || got[0].Path[0] != b.selfID() || got[0].Ctx != 8192 {
		t.Fatalf("imported: %+v", got)
	}
	// loop prevention: B must not import its own model back from A, only A's own model
	for _, e := range b.Config().Endpoints {
		if e.Peer == "laptop" && e.Ref() != "laptop/tiny" {
			t.Fatalf("B imported %s from A — a loop: %+v", e.Ref(), e)
		}
	}
	st := a.PeerStatuses()
	if len(st) != 1 || !st[0].OK || st[0].Models != 1 || st[0].PeerID != b.selfID() || st[0].Remote != "bravo" {
		t.Fatalf("peer status: %+v", st)
	}

	// a request for node/model on A is forwarded to B and answered there
	req := func(model string) *http.Response {
		r, err := http.Post(aFront.URL+"/v1/chat/completions", "application/json",
			stringsReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r
	}
	if r := req("dino/gemma"); r.StatusCode != 200 || r.Header.Get("X-Fleet-Endpoint") != got[0].Name {
		t.Fatalf("forwarded request: %d via %q", r.StatusCode, r.Header.Get("X-Fleet-Endpoint"))
	}
	// the owner counted it under the asking fleet; the asker measured it locally
	var owner []UsageRow
	for i := 0; i < 100 && len(owner) == 0; i++ { // the handler records usage just after the response is sent
		time.Sleep(10 * time.Millisecond)
		owner = b.Usage("client", "", time.Hour)
	}
	if len(owner) == 0 || owner[0].Key != "fleet:laptop" {
		t.Fatalf("B's usage: %+v", owner)
	}
	lv, ok := a.live.get(got[0].Name)
	for i := 0; i < 100 && lv.Requests == 0; i++ {
		time.Sleep(10 * time.Millisecond)
		lv, ok = a.live.get(got[0].Name)
	}
	if !ok || lv.Requests != 1 {
		t.Fatalf("A should have its own statistics for the peer model: %+v", lv)
	}
	// ...and the model list names it like any other
	if m := a.Config().Lookup("dino/gemma"); len(m) != 1 || m[0].Peer != "bravo" {
		t.Fatalf("lookup: %+v", m)
	}

	// a request that already passed through B is never sent to B again (no ping-pong)
	rq, _ := http.NewRequest("POST", aFront.URL+"/v1/chat/completions", stringsReader(`{"model":"dino/gemma","messages":[]}`))
	rq.Header.Set(headerPath, b.selfID())
	rr, _ := http.DefaultClient.Do(rq)
	rr.Body.Close()
	if rr.StatusCode != 404 {
		t.Fatalf("a request that came from B must not be forwarded back to B, got %d", rr.StatusCode)
	}
}

func TestPeerDownKeepsModelsAndRecovers(t *testing.T) {
	be := fakeBackend()
	defer be.Close()
	_, bFront := startFleet(t, &config.Config{Name: "bravo", Roles: map[string]config.Role{},
		Endpoints: []config.Endpoint{{Name: "x", Node: "dino", URL: be.URL, Model: "gemma"}}})
	a, _ := startFleet(t, &config.Config{Name: "laptop", Roles: map[string]config.Role{}})
	if err := a.AddPeer(config.Peer{Name: "bravo", URL: bFront.URL + "/v1"}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	a.SyncPeers(ctx)
	name := ""
	for _, e := range a.Config().Endpoints {
		name = e.Name
	}
	if name == "" || !a.healthy(name) {
		t.Fatal("imported model should be up")
	}
	bFront.Close() // the link to the peer drops
	a.SyncPeers(ctx)
	if a.healthy(name) {
		t.Fatal("models behind an unreachable peer must be marked down")
	}
	if ps := a.PeerStatuses(); ps[0].OK || ps[0].Err == "" || ps[0].Models != 1 {
		t.Fatalf("status while down: %+v", ps[0])
	}
}

func TestPeerFiltersAndRemoval(t *testing.T) {
	be := fakeBackend()
	defer be.Close()
	_, bFront := startFleet(t, &config.Config{Name: "bravo", Roles: map[string]config.Role{}, Endpoints: []config.Endpoint{
		{Name: "g", Node: "dino", URL: be.URL, Model: "gemma"},
		{Name: "q", Node: "dino", URL: be.URL, Model: "qwen"},
		{Name: "v", Node: "victor", URL: be.URL, Model: "bonsai"},
	}})
	a, _ := startFleet(t, &config.Config{Name: "laptop", Roles: map[string]config.Role{}})
	if err := a.AddPeer(config.Peer{Name: "bravo", URL: bFront.URL + "/v1", Only: []string{"node:dino"}, Skip: []string{"model:qwen*"}}); err != nil {
		t.Fatal(err)
	}
	a.SyncPeers(context.Background())
	var refs []string
	for _, e := range a.Config().Endpoints {
		refs = append(refs, e.Ref())
	}
	if len(refs) != 1 || refs[0] != "dino/gemma" {
		t.Fatalf("only/skip: %v", refs)
	}
	if err := a.RemovePeer("bravo"); err != nil || len(a.Config().Endpoints) != 0 {
		t.Fatalf("removing a peer must drop its models: %v %+v", err, a.Config().Endpoints)
	}
}

func stringsReader(s string) *strings.Reader { return strings.NewReader(s) }

func TestDropLoopsNeverTrustsAPeerWithoutPath(t *testing.T) {
	cands := []config.Endpoint{
		{Name: "direct"},
		{Name: "peer-ok", Peer: "p", Path: []string{"B"}},
		{Name: "peer-back", Peer: "p", Path: []string{"A"}}, // would go back to a fleet the request already passed
		{Name: "peer-nopath", Peer: "p"},                    // hand-edited: unknown route
	}
	names := func(es []config.Endpoint) (o []string) {
		for _, e := range es {
			o = append(o, e.Name)
		}
		return
	}
	if got := names(dropLoops(cands, nil)); len(got) != 4 {
		t.Fatalf("a fresh request keeps everything: %v", got)
	}
	got := names(dropLoops(cands, []string{"A"}))
	if len(got) != 2 || got[0] != "direct" || got[1] != "peer-ok" {
		t.Fatalf("after passing A: %v", got)
	}
	if got := names(dropLoops(cands, []string{"X", "Y", "Z"})); len(got) != 1 || got[0] != "direct" {
		t.Fatalf("after %d hops only direct models are used: %v", maxServeHops, got)
	}
}

func TestStaleHealthIsForgottenWhenAPeerDropsAModel(t *testing.T) {
	be := fakeBackend()
	defer be.Close()
	b, bFront := startFleet(t, &config.Config{Name: "bravo", Roles: map[string]config.Role{}, Endpoints: []config.Endpoint{
		{Name: "g", Node: "dino", URL: be.URL, Model: "gemma"}, {Name: "q", Node: "dino", URL: be.URL, Model: "qwen"}}})
	a, _ := startFleet(t, &config.Config{Name: "laptop", Roles: map[string]config.Role{}})
	if err := a.AddPeer(config.Peer{Name: "bravo", URL: bFront.URL + "/v1"}); err != nil {
		t.Fatal(err)
	}
	a.SyncPeers(context.Background())
	if len(a.state) != 2 {
		t.Fatalf("two imported models should have a health entry: %d", len(a.state))
	}
	cfg := b.Config()
	cfg.Endpoints = cfg.Endpoints[:1] // bravo stops offering qwen
	if err := b.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	a.SyncPeers(context.Background())
	if len(a.Config().Endpoints) != 1 || len(a.state) != 1 {
		t.Fatalf("the dropped model and its health must be gone: %d endpoints, %d states", len(a.Config().Endpoints), len(a.state))
	}
}

func TestLANAddCommand(t *testing.T) {
	if c, h := addCommand(LANFleet{Name: "dino", Host: "dino.local", Addr: "10.0.1.5", Port: 4800, Scope: "loopback"}); c != "" || !strings.Contains(h, "listen: 0.0.0.0:4800") {
		t.Fatal(c, h)
	}
	if c, _ := addCommand(LANFleet{Name: "Victor Box", Addr: "10.0.1.5", Port: 4711, Scope: "lan"}); c != "fleet peers add victor-box http://10.0.1.5:4711/v1" {
		t.Fatal(c)
	}
	if c, h := addCommand(LANFleet{Name: "dino", Host: "dino.local", Addr: "10.0.1.5", Port: 4711, Scope: "lan", Key: true}); c != "fleet peers add dino http://10.0.1.5:4711/v1 pair=<code>" || !strings.Contains(h, "fleet peers pair") {
		t.Fatal(c, h)
	}
}

// A peer offering the same node/model twice (a registered server and a run of the same file) is imported once, and keeps
// syncing: both used to take the kept name and every sync failed with "duplicate endpoint name".
func TestPeerSameModelTwiceImportsOnce(t *testing.T) {
	be := fakeBackend()
	defer be.Close()
	_, bFront := startFleet(t, &config.Config{Name: "victor", Roles: map[string]config.Role{}, Endpoints: []config.Endpoint{
		{Name: "srv", Node: "victor", URL: be.URL, Model: "flash.gguf"},
		{Name: "run", Node: "victor", URL: be.URL, Model: "flash.gguf"},
	}})
	a, _ := startFleet(t, &config.Config{Name: "laptop", Roles: map[string]config.Role{}})
	if err := a.AddPeer(config.Peer{Name: "victor", URL: bFront.URL + "/v1"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ { // the later syncs take the "already imported" path that collided
		a.SyncPeers(context.Background())
	}
	var names []string
	for _, e := range a.Config().Endpoints {
		if e.Peer == "victor" {
			names = append(names, e.Name)
		}
	}
	if len(names) != 1 {
		t.Fatalf("want one imported endpoint, got %v", names)
	}
}

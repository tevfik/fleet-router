package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
)

func TestCovBenchMeasureAndRun(t *testing.T) {
	flEnv(t)
	// Upstream that returns valid SSE stream with 12 chunks
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, ok := w.(http.Flusher)
		for i := 0; i < 12; i++ {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"word%d \"}}]}\n\n", i)
			if ok {
				fl.Flush()
			}
			time.Sleep(2 * time.Millisecond)
		}
		fmt.Fprintf(w, "data: {\"choices\":[],\"usage\":{\"completion_tokens\":12}}\n\n")
		fmt.Fprintf(w, "data: [DONE]\n\n")
		if ok {
			fl.Flush()
		}
	}))
	defer srv.Close()

	cfg := &config.Config{
		Name:   "node1",
		Listen: "127.0.0.1:4711",
		Endpoints: []config.Endpoint{
			{Name: "ep-bench", Node: "node1", URL: srv.URL + "/v1", Model: "bench-model"},
		},
	}
	s, _ := flFleet(t, cfg, nil)

	// Mark ep-bench healthy
	s.state["ep-bench"] = &EpState{OK: true}

	// 1. Run bench on ep-bench
	if err := s.Bench([]string{"ep-bench"}); err != nil {
		t.Fatalf("s.Bench failed: %v", err)
	}

	// Wait for benchmark to finish
	rtEventually(t, "benchmark to finish", func() bool {
		return !s.BenchStatus().Running
	})

	res, ok := s.benchResult("ep-bench")
	if !ok || res.Err != "" {
		t.Fatalf("bench failed: ok=%v, err=%s", ok, res.Err)
	}
	if s.Config().Endpoint("ep-bench").TPS <= 0 {
		t.Fatalf("expected TPS > 0, got %f", s.Config().Endpoint("ep-bench").TPS)
	}

	// 2. Bench error when no healthy chat endpoint
	s.state["ep-bench"] = &EpState{OK: false}
	if err := s.Bench(nil); err == nil {
		t.Fatal("expected error when no healthy chat endpoint exists")
	}
}

func TestCovOllamaRoutes(t *testing.T) {
	flEnv(t)
	cfg := &config.Config{
		Name:   "node1",
		Listen: "127.0.0.1:4711",
		Roles: map[string]config.Role{
			"scout": {Preferred: "ep1"},
		},
		Endpoints: []config.Endpoint{
			{Name: "ep1", Node: "node1", URL: "http://127.0.0.1:8000/v1", Model: "qwen:32b"},
		},
	}
	s, _ := flFleet(t, cfg, nil)

	// GET /
	rec := rtDo(t, s, "GET", "/", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Ollama is running") {
		t.Fatalf("GET / got %d: %s", rec.Code, rec.Body.String())
	}

	// GET /api/version
	rec = rtDo(t, s, "GET", "/api/version", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), ollamaVersion) {
		t.Fatalf("GET /api/version got %d: %s", rec.Code, rec.Body.String())
	}

	// GET /api/tags
	rec = rtDo(t, s, "GET", "/api/tags", nil)
	if rec.Code != 200 {
		t.Fatalf("GET /api/tags got %d", rec.Code)
	}

	// GET /api/ps
	rec = rtDo(t, s, "GET", "/api/ps", nil)
	if rec.Code != 200 {
		t.Fatalf("GET /api/ps got %d", rec.Code)
	}

	// POST /api/show missing model -> 404
	rec = rtDo(t, s, "POST", "/api/show", map[string]any{"model": "nonexistent-model"})
	if rec.Code != 404 {
		t.Fatalf("POST /api/show nonexistent got %d, want 404", rec.Code)
	}

	// POST /api/show existing model -> 200
	rec = rtDo(t, s, "POST", "/api/show", map[string]any{"model": "qwen:32b"})
	if rec.Code != 200 {
		t.Fatalf("POST /api/show existing got %d: %s", rec.Code, rec.Body.String())
	}

	// Bad JSON handling on ollama endpoints
	for _, path := range []string{"/api/generate", "/api/chat", "/api/embed", "/api/embeddings"} {
		rec = rtDo(t, s, "POST", path, "{bad-json")
		if rec.Code != 400 {
			t.Fatalf("expected 400 on %s with bad json, got %d", path, rec.Code)
		}
	}
}

func TestCovLinkAndPrivateKind(t *testing.T) {
	flEnv(t)
	// privateKind checks
	if k := privateKind("127.0.0.1"); k != "lan" {
		t.Fatalf("127.0.0.1 kind = %q, want lan", k)
	}
	if k := privateKind("10.0.1.10"); k != "lan" {
		t.Fatalf("10.0.1.10 kind = %q, want lan", k)
	}
	if k := privateKind("10.0.0.5"); k != "lan" {
		t.Fatalf("10.0.0.5 kind = %q, want lan", k)
	}
	if k := privateKind("100.64.0.1"); k != "vpn" {
		t.Fatalf("100.64.0.1 kind = %q, want vpn", k)
	}
	if k := privateKind("8.8.8.8"); k != "wan" {
		t.Fatalf("8.8.8.8 kind = %q, want wan", k)
	}

	cfg := &config.Config{
		Name:   "node1",
		Listen: "127.0.0.1:4711",
		Endpoints: []config.Endpoint{
			{Name: "ep1", Node: "node1", URL: "http://127.0.0.1:8000/v1", Model: "m1"},
		},
	}
	s, _ := flFleet(t, cfg, nil)

	// POST /fleet/store/link missing sha256 -> 400
	rec := rtDo(t, s, "POST", "/fleet/store/link", map[string]any{"sha256": ""})
	if rec.Code != 400 {
		t.Fatalf("POST /fleet/store/link empty sha got %d, want 400", rec.Code)
	}
}

func TestCovStrain(t *testing.T) {
	flEnv(t)
	cfg := &config.Config{
		Name:   "node1",
		Listen: "127.0.0.1:4711",
		Endpoints: []config.Endpoint{
			{Name: "ep1", Node: "node1", URL: "http://127.0.0.1:8000/v1", Model: "m1"},
		},
	}
	s, _ := flFleet(t, cfg, nil)

	if s.selfNode() == "" {
		t.Fatal("selfNode should not be empty")
	}
	st := s.localStrain()
	if st.Source == "" {
		t.Fatal("localStrain source should not be empty")
	}

	rec := rtDo(t, s, "GET", "/fleet/strain", nil)
	if rec.Code != 200 {
		t.Fatalf("GET /fleet/strain got %d", rec.Code)
	}

	s.readStrains(context.Background())
}

func TestCovStoreAndEndpointsServingFile(t *testing.T) {
	flEnv(t)
	cfg := &config.Config{
		Name:   "node1",
		Listen: "127.0.0.1:4711",
		Endpoints: []config.Endpoint{
			{Name: "ep1", Node: "node1", URL: "http://127.0.0.1:8000/v1", Model: "m1"},
		},
	}
	s, _ := flFleet(t, cfg, nil)

	// POST /fleet/store/rm with missing ref -> 400
	rec := rtDo(t, s, "POST", "/fleet/store/rm", map[string]any{"ref": ""})
	if rec.Code != 400 {
		t.Fatalf("expected 400 for empty ref, got %d", rec.Code)
	}

	users := s.endpointsServingFile("test-file-ref")
	if len(users) != 0 {
		t.Fatalf("expected 0 users, got %v", users)
	}

	// expandUser
	if exp := expandUser("/absolute/path"); exp != "/absolute/path" {
		t.Fatalf("expandUser(/absolute/path) = %q", exp)
	}
	if exp := expandUser(""); exp != "" {
		t.Fatalf("expandUser(\"\") = %q", exp)
	}
}

func TestCovLearnDims(t *testing.T) {
	flEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"embedding":[0.1, 0.2, 0.3, 0.4]}]}`)
	}))
	defer srv.Close()

	cfg := &config.Config{
		Name:   "node1",
		Listen: "127.0.0.1:4711",
		Endpoints: []config.Endpoint{
			{Name: "ep-embed", Node: "node1", URL: srv.URL + "/v1", Model: "embed-m", Kind: "embed", Dim: 0},
		},
	}
	s, _ := flFleet(t, cfg, nil)
	s.state["ep-embed"] = &EpState{OK: true}

	s.learnDims(context.Background())
	ep := s.Config().Endpoint("ep-embed")
	if ep.Dim != 4 {
		t.Fatalf("expected learned dim = 4, got %d", ep.Dim)
	}
}

func TestCovServerRoutes(t *testing.T) {
	flEnv(t)
	cfg := &config.Config{
		Name:   "node1",
		Listen: "127.0.0.1:4711",
		Endpoints: []config.Endpoint{
			{Name: "ep1", Node: "node1", URL: "http://127.0.0.1:8000/v1", Model: "m1"},
		},
	}
	s, _ := flFleet(t, cfg, nil)

	// GET /healthz
	rec := rtDo(t, s, "GET", "/healthz", nil)
	if rec.Code != 200 || rec.Body.String() != "ok" {
		t.Fatalf("GET /healthz got %d: %q", rec.Code, rec.Body.String())
	}

	// GET /metrics
	rec = rtDo(t, s, "GET", "/metrics", nil)
	if rec.Code != 200 {
		t.Fatalf("GET /metrics got %d", rec.Code)
	}

	// GET /fleet/hello
	rec = rtDo(t, s, "GET", "/fleet/hello", nil)
	if rec.Code != 200 {
		t.Fatalf("GET /fleet/hello got %d", rec.Code)
	}

	// GET /fleet/export
	rec = rtDo(t, s, "GET", "/fleet/export", nil)
	if rec.Code != 200 {
		t.Fatalf("GET /fleet/export got %d", rec.Code)
	}

	// GET /fleet/host
	rec = rtDo(t, s, "GET", "/fleet/host", nil)
	if rec.Code != 200 {
		t.Fatalf("GET /fleet/host got %d", rec.Code)
	}

	// SetListen
	s.SetListen("127.0.0.1:4712")
	if s.Config().Listen != "127.0.0.1:4712" {
		t.Fatalf("SetListen not reflected in config: %s", s.Config().Listen)
	}
}

func TestCovProfilesAPI(t *testing.T) {
	flEnv(t)
	cfg := &config.Config{
		Name:   "node1",
		Listen: "127.0.0.1:4711",
		Endpoints: []config.Endpoint{
			{Name: "ep1", Node: "node1", URL: "http://127.0.0.1:8000/v1", Model: "m1"},
		},
	}
	s, _ := flFleet(t, cfg, nil)

	// GET all profiles
	rec := rtDo(t, s, "GET", "/fleet/profiles", nil)
	if rec.Code != 200 {
		t.Fatalf("GET /fleet/profiles got %d", rec.Code)
	}

	// PUT invalid name
	rec = rtDo(t, s, "PUT", "/fleet/profiles/bad@name", "layout: plain")
	if rec.Code != 400 {
		t.Fatalf("PUT invalid name got %d, want 400", rec.Code)
	}

	// PUT valid profile
	validYAML := "model: /m/o.gguf\nmaster: dino\nlayout: {mode: auto, via_hub: true}\n"
	rec = rtDo(t, s, "PUT", "/fleet/profiles/testprof", validYAML)
	if rec.Code != 200 {
		t.Fatalf("PUT valid profile got %d: %s", rec.Code, rec.Body.String())
	}

	// GET by name JSON
	rec = rtDo(t, s, "GET", "/fleet/profiles/testprof", nil)
	if rec.Code != 200 {
		t.Fatalf("GET profile json got %d", rec.Code)
	}

	// GET by name YAML
	rec = rtDo(t, s, "GET", "/fleet/profiles/testprof?format=yaml", nil)
	if rec.Code != 200 {
		t.Fatalf("GET profile yaml got %d", rec.Code)
	}

	// GET nonexistent
	rec = rtDo(t, s, "GET", "/fleet/profiles/nonexistent", nil)
	if rec.Code != 404 {
		t.Fatalf("GET nonexistent profile got %d, want 404", rec.Code)
	}

	// DELETE existing
	rec = rtDo(t, s, "DELETE", "/fleet/profiles/testprof", nil)
	if rec.Code != 200 {
		t.Fatalf("DELETE existing profile got %d", rec.Code)
	}

	// DELETE nonexistent
	rec = rtDo(t, s, "DELETE", "/fleet/profiles/nonexistent", nil)
	if rec.Code != 404 {
		t.Fatalf("DELETE nonexistent profile got %d, want 404", rec.Code)
	}
}

func TestCovRunStopAPI(t *testing.T) {
	flEnv(t)
	cfg := &config.Config{
		Name:   "node1",
		Listen: "127.0.0.1:4711",
		Endpoints: []config.Endpoint{
			{Name: "ep1", Node: "node1", URL: "http://127.0.0.1:8000/v1", Model: "m1"},
		},
	}
	s, _ := flFleet(t, cfg, nil)

	// POST /fleet/runs/stop: empty ID -> 400
	rec := rtDo(t, s, "POST", "/fleet/runs/stop", map[string]any{"id": ""})
	if rec.Code != 400 {
		t.Fatalf("POST /fleet/runs/stop empty ID got %d, want 400", rec.Code)
	}

	// POST /fleet/runs/stop: nonexistent ID -> 404
	rec = rtDo(t, s, "POST", "/fleet/runs/stop", map[string]any{"id": "run-not-found"})
	if rec.Code != 404 {
		t.Fatalf("POST /fleet/runs/stop nonexistent ID got %d, want 404", rec.Code)
	}

	// POST /fleet/endpoints/stop: empty name -> 400
	rec = rtDo(t, s, "POST", "/fleet/endpoints/stop", map[string]any{"name": ""})
	if rec.Code != 400 {
		t.Fatalf("POST /fleet/endpoints/stop empty name got %d, want 400", rec.Code)
	}

	// POST /fleet/endpoints/stop: nonexistent name -> 404
	rec = rtDo(t, s, "POST", "/fleet/endpoints/stop", map[string]any{"name": "ep-unknown"})
	if rec.Code != 404 {
		t.Fatalf("POST /fleet/endpoints/stop nonexistent name got %d, want 404", rec.Code)
	}

	// POST /fleet/endpoints/stop: existing endpoint ep1 -> 200
	rec = rtDo(t, s, "POST", "/fleet/endpoints/stop", map[string]any{"name": "ep1"})
	if rec.Code != 200 {
		t.Fatalf("POST /fleet/endpoints/stop ep1 got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCovStoreEndpoints(t *testing.T) {
	flEnv(t)
	cfg := &config.Config{
		Name:   "node1",
		Listen: "127.0.0.1:4711",
		Endpoints: []config.Endpoint{
			{Name: "ep1", Node: "node1", URL: "http://127.0.0.1:8000/v1", Model: "m1"},
		},
	}
	s, _ := flFleet(t, cfg, nil)

	// GET /fleet/store
	rec := rtDo(t, s, "GET", "/fleet/store", nil)
	if rec.Code != 200 {
		t.Fatalf("GET /fleet/store got %d", rec.Code)
	}

	// GET /fleet/store/all
	rec = rtDo(t, s, "GET", "/fleet/store/all", nil)
	if rec.Code != 200 {
		t.Fatalf("GET /fleet/store/all got %d", rec.Code)
	}

	// POST /fleet/store/add with empty paths -> 400
	rec = rtDo(t, s, "POST", "/fleet/store/add", map[string]any{"paths": []string{}})
	if rec.Code != 400 {
		t.Fatalf("POST /fleet/store/add empty paths got %d, want 400", rec.Code)
	}

	// POST /fleet/store/prune -> 200
	rec = rtDo(t, s, "POST", "/fleet/store/prune", nil)
	if rec.Code != 200 {
		t.Fatalf("POST /fleet/store/prune got %d", rec.Code)
	}
}

func TestCovLanAndLink(t *testing.T) {
	flEnv(t)
	mdnsOff := false
	cfg := &config.Config{
		Name:   "node1",
		Listen: "127.0.0.1:4711",
		MDNS:   &mdnsOff,
		Endpoints: []config.Endpoint{
			{Name: "ep1", Node: "node1", URL: "http://127.0.0.1:8000/v1", Model: "m1"},
		},
	}
	s, _ := flFleet(t, cfg, nil)

	if Hostname() == "" {
		t.Fatal("Hostname() should not be empty")
	}
	_ = s.LANFleets()
	s.lanLoop(context.Background())

	// Link checks
	if k := privateKind("172.20.0.1"); k != "lan" {
		t.Fatalf("172.20.0.1 kind = %s, want lan", k)
	}
	if k := privateKind("169.254.1.1"); k != "lan" {
		t.Fatalf("169.254.1.1 kind = %s, want lan", k)
	}

	rec := rtDo(t, s, "POST", "/fleet/store/link", map[string]any{"sha256": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"})
	if rec.Code != 404 {
		t.Fatalf("POST /fleet/store/link nonexistent file got %d, want 404", rec.Code)
	}
}

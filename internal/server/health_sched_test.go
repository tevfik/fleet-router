package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
)

func countingModels(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") { // health checks only (/api/ps, /api/tags are the runtime poll)
			n.Add(1)
		}
		w.Write([]byte(`{"data":[{"id":"m"}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func healthServer(t *testing.T, eps ...config.Endpoint) *Server {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.yaml")
	cfg := &config.Config{Name: "n1", Endpoints: eps}
	if err := cfg.Save(p); err != nil {
		t.Fatal(err)
	}
	s, err := New(p)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// runHealth starts the loop and returns a stop that waits for it to exit (it reads package vars the next test changes).
func runHealth(s *Server) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.healthLoop(ctx); close(done) }()
	return func() { cancel(); <-done }
}

func kickN(s *Server, n int) {
	for i := 0; i < n; i++ {
		time.Sleep(60 * time.Millisecond)
		select {
		case s.kick <- struct{}{}:
		default:
		}
	}
	time.Sleep(100 * time.Millisecond)
}

// A healthy endpoint is checked once per healthEvery, not on every round (rounds come every 10 s and on every config
// change): the polling was what filled Ollama's log.
func TestHealth_HealthyEndpointIsLeftAlone(t *testing.T) {
	srv, n := countingModels(t)
	s := healthServer(t, config.Endpoint{Name: "a", Node: "n1", URL: srv.URL + "/v1", Model: "m"})
	stopHealth := runHealth(s)
	defer stopHealth()
	kickN(s, 6)
	if got := n.Load(); got != 1 {
		t.Fatalf("healthy endpoint asked %d times within healthEvery, want 1", got)
	}
	if st := s.getState("a"); st == nil || !st.OK {
		t.Fatalf("state: %+v", st)
	}
}

// Real traffic it answered says it is up: no probe needed.
func TestHealth_RealTrafficReplacesProbes(t *testing.T) {
	srv, n := countingModels(t)
	s := healthServer(t, config.Endpoint{Name: "a", Node: "n1", URL: srv.URL + "/v1", Model: "m"})
	old := healthEvery
	healthEvery = 300 * time.Millisecond
	defer func() { healthEvery = old }()
	stopHealth := runHealth(s)
	defer stopHealth()
	time.Sleep(100 * time.Millisecond) // first check
	stop := make(chan struct{})
	go func() { // a request every 50 ms
		for {
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
				s.live.bump("a", true)
			}
		}
	}()
	kickN(s, 12) // ~800 ms: without traffic that is 2-3 more probes
	close(stop)
	if got := n.Load(); got != 1 {
		t.Fatalf("probes while serving real requests: %d, want only the first", got)
	}
}

// A failing endpoint is retried after 10 s, then 20, 40 … up to 5 min; success resets it.
func TestHealth_FailureBackoff(t *testing.T) {
	now := time.Now()
	sc := &healthSched{}
	var got []time.Duration
	for i := 0; i < 8; i++ {
		healthNext(now, &EpState{OK: false}, sc, time.Minute)
		got = append(got, sc.next.Sub(now))
	}
	want := []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second,
		5 * time.Minute, 5 * time.Minute, 5 * time.Minute}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("backoff %d: %v, want %v (all: %v)", i, got[i], want[i], got)
		}
	}
	healthNext(now, &EpState{OK: true}, sc, time.Minute)
	if sc.fails != 0 || sc.next.Sub(now) != time.Minute {
		t.Fatalf("success must reset: %+v", sc)
	}
}

// A re-pointed endpoint (a deploy, a run started on another port) is checked at once, not when its old schedule says.
func TestHealth_ChangedEndpointIsCheckedAtOnce(t *testing.T) {
	srv, n := countingModels(t)
	srv2, n2 := countingModels(t)
	s := healthServer(t, config.Endpoint{Name: "a", Node: "n1", URL: srv.URL + "/v1", Model: "m"})
	stopHealth := runHealth(s)
	defer stopHealth()
	kickN(s, 2)
	c := s.Config()
	c.Endpoints = append([]config.Endpoint(nil), c.Endpoints...) // the slice is shared with the running configuration
	c.Endpoints[0].URL = srv2.URL + "/v1"
	if err := s.SetConfig(c); err != nil { // kicks the loop
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if n.Load() != 1 || n2.Load() != 1 {
		t.Fatalf("old server %d, new server %d probes; want 1 and 1", n.Load(), n2.Load())
	}
}

// A distributed endpoint nobody listens on is a run that is not started (runs are on demand): idle, not a failure.
func TestHealth_UnstartedGroupIsIdle(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	e := rtEp("g", "n1", url+"/v1", "m")
	e.Probe = "any"
	e.Group = &config.Group{Engine: "llama-ring"}
	s := rtServer(t, &config.Config{Endpoints: []config.Endpoint{e}})
	s.hosts.m = map[string]HostReport{}
	ep := s.Config().Endpoints[0]
	s.probe(context.Background(), &ep)
	if st := s.getState("g"); st == nil || st.OK || !st.OnDemand {
		t.Fatalf("want idle (not ok): %+v", st)
	}
	// a plain endpoint that does not answer stays a failure
	p := rtEp("p", "n1", url+"/v1", "m")
	s2 := rtServer(t, &config.Config{Endpoints: []config.Endpoint{p}})
	pe := s2.Config().Endpoints[0]
	s2.probe(context.Background(), &pe)
	if st := s2.getState("p"); st == nil || st.OK || st.OnDemand {
		t.Fatalf("a plain endpoint is down, not idle: %+v", st)
	}
}

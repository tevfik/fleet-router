package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
)

// stuckUp answers the chat request like a master whose worker link hung: nothing (or one chunk) and then silence.
func stuckUp(t *testing.T, chunkFirst bool) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			w.Write([]byte(`{"data":[{"id":"m"}]}`))
			return
		}
		io.Copy(io.Discard, r.Body) // lets the server notice the client hanging up
		if chunkFirst {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
			w.(http.Flusher).Flush()
		}
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv
}

func stallServer(t *testing.T, cfg *config.Config, stall string) *Server {
	s := rtServer(t, cfg)
	s.cfg.Endpoints[0].Stall = stall // below the 1s a configuration may ask for: only a test needs it
	return s
}

func TestStall_MidStreamCutsAndQuarantines(t *testing.T) {
	srv := stuckUp(t, true)
	cfg := &config.Config{Endpoints: []config.Endpoint{rtEp("a", "n1", srv.URL+"/v1", "m")}}
	s := stallServer(t, cfg, "150ms")
	t0 := time.Now()
	rec := rtDo(t, s, "POST", "/v1/chat/completions", rtChat("m", true))
	if d := time.Since(t0); d > 3*time.Second {
		t.Fatalf("took %v: the watchdog did not cut the stream", d)
	}
	if !strings.Contains(rec.Body.String(), "hi") {
		t.Fatalf("what came before the stall must reach the client: %q", rec.Body.String())
	}
	if s.healthy("a") {
		t.Fatal("a stalled endpoint must be set aside")
	}
	if st := s.getState("a"); st == nil || !strings.Contains(st.Why, "stalled") {
		t.Fatalf("state: %+v", st)
	}
}

func TestStall_BeforeFirstByteFailsOver(t *testing.T) {
	srv := stuckUp(t, false)
	b := rtNewUp(t, "b")
	cfg := &config.Config{
		Endpoints: []config.Endpoint{rtEp("a", "n1", srv.URL+"/v1", "ma"), rtEp("b", "n2", b.URL(), "mb")},
		Roles:     map[string]config.Role{"r": {Preferred: "a", Fallback: []string{"b"}}},
	}
	s := stallServer(t, cfg, "100ms") // 3× = 300ms before the first byte
	rec := rtDo(t, s, "POST", "/v1/chat/completions", rtChat("r", true))
	if rec.Code != 200 || rec.Header().Get("X-Fleet-Endpoint") != "b" {
		t.Fatalf("code %d via %q: %s", rec.Code, rec.Header().Get("X-Fleet-Endpoint"), rec.Body.String())
	}
	if s.healthy("a") {
		t.Fatal("a must be set aside")
	}
}

func TestStall_HealthCheckDoesNotClearQuarantine(t *testing.T) {
	srv := stuckUp(t, true)
	cfg := &config.Config{Endpoints: []config.Endpoint{rtEp("a", "n1", srv.URL+"/v1", "m")}}
	s := stallServer(t, cfg, "100ms")
	rtDo(t, s, "POST", "/v1/chat/completions", rtChat("m", true))
	e := s.Config().Endpoints[0]
	s.probe(context.Background(), &e)
	if st := s.getState("a"); st == nil || !st.OK || s.healthy("a") || st.Why == "" {
		t.Fatalf("the master still answers /models but must stay set aside: %+v", st)
	}
}

func TestStall_Defaults(t *testing.T) {
	e := config.Endpoint{}
	if e.StallAfter() != 0 {
		t.Fatal("a plain endpoint has no stall limit")
	}
	e.Group = &config.Group{Engine: "llama-ring"}
	if e.StallAfter() != config.DefaultGroupStall {
		t.Fatalf("group default: %v", e.StallAfter())
	}
	e.Stall = "off"
	if e.StallAfter() != 0 {
		t.Fatal("off disables")
	}
	e.Stall = "30s"
	if e.StallAfter() != 30*time.Second {
		t.Fatal("explicit value")
	}
	c := &config.Config{Endpoints: []config.Endpoint{{Name: "a", URL: "http://x/v1", Model: "m", Stall: "soon"}}}
	if c.Validate() == nil {
		t.Fatal("a bad duration must be refused")
	}
	c.Endpoints[0].Stall, c.Endpoints[0].Group = "", &config.Group{}
	if c.Validate() == nil {
		t.Fatal("a group needs an engine")
	}
}

// engine is a stock llama-server: /health, /props, /slots, /metrics at the root and /v1 below.
func engine(t *testing.T, health string) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"data":[{"id":"m"}]}`)) })
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}]}`))
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"status":"` + health + `"}`)) })
	mux.HandleFunc("/props", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"total_slots":2,"model_path":"/m/x.gguf","default_generation_settings":{"n_ctx":4096}}`))
	})
	mux.HandleFunc("/slots", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id":0,"is_processing":true},{"id":1,"is_processing":false}]`))
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("# HELP x\nllamacpp:predicted_tokens_seconds 12.5\nllamacpp:kv_cache_usage_ratio 0.25\n"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestGroup_ReadsEngineAndMembers(t *testing.T) {
	srv := engine(t, "ok")
	e := rtEp("g", "n1", srv.URL+"/v1", "m")
	e.Slots = 4
	e.Group = &config.Group{Engine: "llama-ring", Workers: []string{"w1"}}
	s := rtServer(t, &config.Config{Endpoints: []config.Endpoint{e}})
	s.routes = func(string) []nodeRoute { return nil }
	s.hosts.m = map[string]HostReport{"w1": {Node: "w1", At: time.Now().Add(time.Hour)}} // reachable
	ep := s.Config().Endpoints[0]
	s.probe(context.Background(), &ep)
	st := s.getState("g")
	if st == nil || !st.OK || st.Group == nil {
		t.Fatalf("state: %+v", st)
	}
	g := st.Group
	if g.Engine != "llama-ring" || g.Health != "ok" || g.Slots != 2 || g.Busy != 1 || g.Ctx != 4096 ||
		g.TPS != 12.5 || g.KVUsage != 0.25 || g.Model != "/m/x.gguf" {
		t.Fatalf("group: %+v", g)
	}
	if len(g.Members) != 1 || !g.Members[0].Up {
		t.Fatalf("members: %+v", g.Members)
	}
	if len(g.Warn) != 1 || !strings.Contains(g.Warn[0], "slots") {
		t.Fatalf("slots 4 in the config vs 2 in the engine must warn: %v", g.Warn)
	}
}

func TestGroup_WorkerDownTakesEndpointDown(t *testing.T) {
	srv := engine(t, "ok")
	e := rtEp("g", "n1", srv.URL+"/v1", "m")
	e.Group = &config.Group{Engine: "prima", Workers: []string{"w9"}}
	s := rtServer(t, &config.Config{Endpoints: []config.Endpoint{e}})
	s.routes = func(string) []nodeRoute { return nil } // no way to reach w9
	ep := s.Config().Endpoints[0]
	s.probe(context.Background(), &ep)
	st := s.getState("g")
	if st.OK || !strings.Contains(st.Err, "w9") || st.Group.Members[0].Up {
		t.Fatalf("a missing worker means the group is down: %+v", st)
	}
	if s.healthy("g") {
		t.Fatal("not healthy")
	}
}

func TestGroup_EngineNotReadyWarns(t *testing.T) {
	srv := engine(t, "loading model")
	e := rtEp("g", "n1", srv.URL+"/v1", "m")
	e.Group = &config.Group{Engine: "llamacpp-rpc"}
	s := rtServer(t, &config.Config{Endpoints: []config.Endpoint{e}})
	ep := s.Config().Endpoints[0]
	s.probe(context.Background(), &ep)
	if g := s.getState("g").Group; len(g.Warn) != 1 || !strings.Contains(g.Warn[0], "loading") {
		t.Fatalf("%+v", g)
	}
}

// The reply is what a real `fleet ring serve` answered (CPU worker, CUDA master, loopback).
const realTrace = `{"steps_total":82,"window":82,"decode":{"steps":78,"tokens":78,"mean_ms":17.302,"steps_per_s":57.796,"p50_ms":16.311,"p95_ms":16.95,"max_ms":55.836,"ring_ms":12.451,"local_ms":4.851,"endpoints":{"127.0.0.1:50201":{"send_ms":0.124,"get_ms":12.081,"worker_busy_ms":12.061,"net_ms":12.057,"worker_samples":78}}},"batch":{"steps":2,"tokens":19,"mean_ms":84.842}}`

func traceServer(t *testing.T, reply string) int {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ring/trace" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	var port int
	fmt.Sscanf(srv.URL[strings.LastIndex(srv.URL, ":")+1:], "%d", &port)
	return port
}

// traceInfoServer answers /ring/trace and /ring/info (the latter with the workers the engine lost, why, and their features).
func traceInfoServer(t *testing.T, trace, info string) int {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ring/trace":
			w.Write([]byte(trace))
		case "/ring/info":
			w.Write([]byte(info))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	var port int
	fmt.Sscanf(srv.URL[strings.LastIndex(srv.URL, ":")+1:], "%d", &port)
	return port
}

func TestGroup_EngineLostAWorkerTakesEndpointDown(t *testing.T) {
	srv := engine(t, "ok") // llama-server itself still says ok
	e := rtEp("g", "n1", srv.URL+"/v1", "m")
	e.Probe = "any"
	info := `{"engine":"llama-ring","protocol":101,"failed_workers":["127.0.0.1:50201"]}`
	e.Group = &config.Group{Engine: "llama-ring", Workers: []string{"w1"}, StatsPort: traceInfoServer(t, realTrace, info)}
	s := rtServer(t, &config.Config{Endpoints: []config.Endpoint{e}})
	s.routes = func(string) []nodeRoute { return nil }
	s.hosts.m = map[string]HostReport{"w1": {Node: "w1", At: time.Now().Add(time.Hour)}}
	ep := s.Config().Endpoints[0]
	s.probe(context.Background(), &ep)
	st := s.getState("g")
	if st.OK || !strings.Contains(st.Err, "127.0.0.1:50201") {
		t.Fatalf("a lost worker must take the endpoint out of routing: %+v", st)
	}
	if s.healthy("g") {
		t.Fatal("not healthy")
	}
	if tr := st.Group.Trace; tr == nil || len(tr.Failed) != 1 {
		t.Fatalf("%+v", st.Group.Trace)
	}
}

func TestGroup_NoFailedWorkersStaysUp(t *testing.T) {
	srv := engine(t, "ok")
	e := rtEp("g", "n1", srv.URL+"/v1", "m")
	e.Probe = "any"
	e.Group = &config.Group{Engine: "llama-ring", Workers: []string{"w1"}, StatsPort: traceInfoServer(t, realTrace, `{"engine":"llama-ring","failed_workers":[]}`)}
	s := rtServer(t, &config.Config{Endpoints: []config.Endpoint{e}})
	s.routes = func(string) []nodeRoute { return nil }
	s.hosts.m = map[string]HostReport{"w1": {Node: "w1", At: time.Now().Add(time.Hour)}}
	ep := s.Config().Endpoints[0]
	s.probe(context.Background(), &ep)
	if st := s.getState("g"); !st.OK {
		t.Fatalf("no failed workers: the endpoint stays up: %+v", st)
	}
}

func TestGroup_ReadsTheEngineProfile(t *testing.T) {
	srv := engine(t, "ok")
	e := rtEp("g", "n1", srv.URL+"/v1", "m")
	e.Probe = "any"
	e.Group = &config.Group{Engine: "llama-ring", Workers: []string{"w1"}, StatsPort: traceServer(t, realTrace)}
	s := rtServer(t, &config.Config{Endpoints: []config.Endpoint{e}})
	s.routes = func(string) []nodeRoute { return nil }
	s.hosts.m = map[string]HostReport{"w1": {Node: "w1", At: time.Now().Add(time.Hour)}}
	ep := s.Config().Endpoints[0]
	s.probe(context.Background(), &ep)
	tr := s.getState("g").Group.Trace
	if tr == nil || tr.Steps != 78 || tr.TokenP50MS != 16.311 || tr.LocalMS != 4.851 || len(tr.Nodes) != 1 {
		t.Fatalf("%+v", tr)
	}
	n := tr.Nodes[0]
	if n.Node != "w1" || n.BusyMS != 12.061 || !n.Trusted() {
		t.Fatalf("a lone worker is named by the group: %+v", n)
	}
	// the engine's net_ms (12.057) still holds the worker's compute; the wire is get − busy
	if n.WireMS < 0.019 || n.WireMS > 0.021 {
		t.Fatalf("wire %.4f ms, want ≈ 0.02 (get 12.081 − busy 12.061)", n.WireMS)
	}
}

// noMetrics is a stock llama-server started without --metrics: the same, but /metrics says 501.
func noMetrics(t *testing.T) *httptest.Server {
	up := engine(t, "ok")
	u, _ := url.Parse(up.URL)
	rp := httputil.NewSingleHostReverseProxy(u)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			http.Error(w, "not supported", 501)
			return
		}
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestGroup_ProfileFeedsTheRun(t *testing.T) {
	srv := noMetrics(t)
	e := rtEp("g", "n1", srv.URL+"/v1", "m")
	e.Probe = "any"
	e.Group = &config.Group{Engine: "llama-ring", Workers: []string{"w1"}, StatsPort: traceServer(t, realTrace)}
	s := rtServer(t, &config.Config{Endpoints: []config.Endpoint{e}})
	s.routes = func(string) []nodeRoute { return nil }
	s.hosts.m = map[string]HostReport{"w1": {Node: "w1", At: time.Now().Add(time.Hour)}}
	r, _ := s.RecordRun(runPlan(), 1, "g", "")
	ep := s.Config().Endpoints[0]
	s.probe(context.Background(), &ep)
	got, _ := s.runs.get(r.ID)
	if len(got.Samples) != 1 || got.Samples[0].TokenP50MS != 16.311 || len(got.Samples[0].Nodes) != 1 {
		t.Fatalf("%+v", got.Samples)
	}
	if tps := got.Samples[0].TPS; tps < 61 || tps > 62 { // no /metrics here: 1000 / p50
		t.Fatalf("tps %.2f from the profile", tps)
	}
}

func TestGroup_ProfileEdgeCases(t *testing.T) {
	srv := engine(t, "ok")
	mk := func(reply string, workers []string, addrs map[string]string) *RingTrace {
		e := rtEp("g", "n1", srv.URL+"/v1", "m")
		e.Probe = "any"
		e.Group = &config.Group{Engine: "llama-ring", Workers: workers, Addrs: addrs, StatsPort: traceServer(t, reply)}
		s := rtServer(t, &config.Config{Endpoints: []config.Endpoint{e}})
		s.hosts.m = map[string]HostReport{}
		ep := s.Config().Endpoints[0]
		s.probe(context.Background(), &ep)
		return s.getState("g").Group.Trace
	}
	if tr := mk(`{"steps_total":0,"decode":{"steps":0}}`, nil, nil); tr == nil || tr.Steps != 0 {
		t.Fatalf("nothing generated yet is not an error: %+v", tr)
	}
	if tr := mk(`not json`, nil, nil); tr != nil {
		t.Fatalf("%+v", tr)
	}
	two := `{"decode":{"steps":10,"mean_ms":20,"p50_ms":20,"endpoints":{"10.0.0.2:50152":{"get_ms":8,"worker_busy_ms":6,"worker_samples":10},"10.0.0.3:50152":{"get_ms":3,"worker_busy_ms":1,"worker_samples":2}}}}`
	tr := mk(two, []string{"a", "b"}, map[string]string{"b": "10.0.0.3:50152"})
	if len(tr.Nodes) != 2 || tr.Nodes[0].Node != "" || tr.Nodes[1].Node != "b" || tr.Nodes[1].Trusted() || tr.Nodes[0].WireMS != 2 {
		t.Fatalf("addresses name machines only when told; two samples are not enough: %+v", tr.Nodes)
	}
}

// Found with the real engine: kill a worker and the master still answers /health and /models. A group is
// therefore checked with a one-token completion, and a master that cannot produce one is down.
func TestGroup_DefaultProbeIsATokenAndCatchesADeadWorker(t *testing.T) {
	up := engine(t, "ok")
	u, _ := url.Parse(up.URL)
	rp := httputil.NewSingleHostReverseProxy(u)
	broken := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if broken && strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.Error(w, "worker lost", 500)
			return
		}
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	e := rtEp("g", "n1", srv.URL+"/v1", "m")
	e.Group = &config.Group{Engine: "llama-ring"} // no probe setting: the group default
	s := rtServer(t, &config.Config{Endpoints: []config.Endpoint{e}})
	ep := s.Config().Endpoints[0]
	s.probe(context.Background(), &ep)
	if st := s.getState("g"); !st.OK {
		t.Fatalf("a working group is up: %+v", st)
	}
	broken = true
	s.probe(context.Background(), &ep)
	if st := s.getState("g"); st.OK || !strings.Contains(st.Err, "500") {
		t.Fatalf("/models and /health are fine but no token comes: %+v", st)
	}
}

// llama-server sends the headers of a streamed answer before the first token: they must not count as life.
func headersThenUp(t *testing.T, wait time.Duration, nonStream bool) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			w.Write([]byte(`{"data":[{"id":"m"}]}`))
			return
		}
		io.Copy(io.Discard, r.Body)
		if !nonStream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
		}
		select {
		case <-time.After(wait):
		case <-r.Context().Done():
			return
		}
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"late\"}}]}\n\n"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestStall_HeadersAloneAreNotLife_FailsOverBeforeTheFirstToken(t *testing.T) {
	a := headersThenUp(t, time.Hour, false) // headers at once, the token never comes
	b := rtNewUp(t, "b")
	cfg := &config.Config{
		Endpoints: []config.Endpoint{rtEp("a", "n1", a.URL+"/v1", "ma"), rtEp("b", "n2", b.URL(), "mb")},
		Roles:     map[string]config.Role{"r": {Preferred: "a", Fallback: []string{"b"}}},
	}
	s := stallServer(t, cfg, "100ms")
	t0 := time.Now()
	rec := rtDo(t, s, "POST", "/v1/chat/completions", rtChat("r", true))
	d := time.Since(t0)
	if rec.Code != 200 || rec.Header().Get("X-Fleet-Endpoint") != "b" || !strings.Contains(rec.Body.String(), "data:") {
		t.Fatalf("the client must get b's answer, not an empty 200: %d via %q %q", rec.Code, rec.Header().Get("X-Fleet-Endpoint"), rec.Body.String())
	}
	if d < 280*time.Millisecond {
		t.Fatalf("the first token gets three times the stall time (300ms), cut after %v", d)
	}
	if s.healthy("a") {
		t.Fatal("a must be set aside")
	}
}

func TestStall_SlowFirstTokenIsNotAStall(t *testing.T) {
	a := headersThenUp(t, 220*time.Millisecond, false) // a long prompt: more than the stall time (100ms), less than 3×
	cfg := &config.Config{Endpoints: []config.Endpoint{rtEp("a", "n1", a.URL+"/v1", "m")}}
	s := stallServer(t, cfg, "100ms")
	rec := rtDo(t, s, "POST", "/v1/chat/completions", rtChat("m", true))
	if !strings.Contains(rec.Body.String(), "late") || !s.healthy("a") {
		t.Fatalf("a slow prompt is not a stall: %q healthy=%v", rec.Body.String(), s.healthy("a"))
	}
}

func TestStall_AnswerThatIsNotStreamedGetsTenTimes(t *testing.T) {
	a := headersThenUp(t, 450*time.Millisecond, true) // says nothing for 4.5× the stall time, then answers whole
	cfg := &config.Config{Endpoints: []config.Endpoint{rtEp("a", "n1", a.URL+"/v1", "m")}}
	s := stallServer(t, cfg, "100ms")
	rec := rtDo(t, s, "POST", "/v1/chat/completions", rtChat("m", false))
	if !strings.Contains(rec.Body.String(), "late") || !s.healthy("a") {
		t.Fatalf("%q healthy=%v", rec.Body.String(), s.healthy("a"))
	}
}

func TestGroup_ProfileThroughATunnelToTheMastersStatsPort(t *testing.T) {
	srv := engine(t, "ok")
	e := config.Endpoint{Name: "g", Node: "dino", SSH: "dino", Remote: "127.0.0.1:18090", Model: "m", Probe: "any",
		Group: &config.Group{Engine: "llama-ring", Workers: []string{"w1"}, StatsPort: 18191}}
	if got := e.StatsRemote(); got != "127.0.0.1:18191" {
		t.Fatalf("the statistics port beside the models: %q", got)
	}
	if (&config.Endpoint{SSH: "x", Remote: "127.0.0.1:1"}).StatsRemote() != "" || (&config.Endpoint{URL: "http://h/v1", Group: e.Group}).StatsRemote() != "" {
		t.Fatal("no group port, or not over SSH: nothing to tunnel")
	}
	s := rtServer(t, &config.Config{Endpoints: []config.Endpoint{e}})
	s.routes = func(string) []nodeRoute { return nil }
	s.hosts.m = map[string]HostReport{"w1": {Node: "w1", At: time.Now().Add(time.Hour)}}
	var enginePort, statsPort int
	fmt.Sscanf(srv.URL[strings.LastIndex(srv.URL, ":")+1:], "%d", &enginePort)
	statsPort = traceServer(t, realTrace)
	ep := s.Config().Endpoints[0]
	s.tunnels.ports[tkey("dino", "127.0.0.1:18090")] = enginePort
	s.probe(context.Background(), &ep)
	if g := s.getState("g").Group; g == nil || g.Trace != nil {
		t.Fatalf("the tunnel to the statistics is not up yet: nothing, and no failure: %+v", g)
	}
	s.tunnels.ports[tkey("dino", "127.0.0.1:18191")] = statsPort // the fleet's own forward, as syncTunnels opens it
	s.probe(context.Background(), &ep)
	if g := s.getState("g").Group; g.Trace == nil || g.Trace.Steps != 78 {
		t.Fatalf("%+v", g)
	}
}

// A worker that stopped: /health is ok, /models answers, but the ring takes no step while a request is running.
func TestGroup_FrozenRingIsNoticedFromTheStepCounter(t *testing.T) {
	steps := 100
	tsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"steps_total":%d,"decode":{"steps":5,"mean_ms":20,"p50_ms":20}}`, steps)
	}))
	t.Cleanup(tsrv.Close)
	var port int
	fmt.Sscanf(tsrv.URL[strings.LastIndex(tsrv.URL, ":")+1:], "%d", &port)
	srv := engine(t, "ok") // one slot is processing
	e := rtEp("g", "n1", srv.URL+"/v1", "m")
	e.Probe = "any"
	e.Group = &config.Group{Engine: "llama-ring", StatsPort: port}
	s := rtServer(t, &config.Config{Endpoints: []config.Endpoint{e}})
	s.cfg.Endpoints[0].Stall = "120ms"
	ep := s.Config().Endpoints[0]
	probe := func() { s.probe(context.Background(), &ep) }
	probe()
	if !s.healthy("g") {
		t.Fatal("the first reading only starts the clock")
	}
	time.Sleep(160 * time.Millisecond)
	steps = 101 // the ring moved: no alarm
	probe()
	time.Sleep(60 * time.Millisecond)
	probe()
	if !s.healthy("g") {
		t.Fatal("a ring that moves is not frozen")
	}
	time.Sleep(160 * time.Millisecond) // …and now it does not
	probe()
	st := s.getState("g")
	if s.healthy("g") || !strings.Contains(st.Why, "no step") || len(st.Group.Warn) == 0 {
		t.Fatalf("a busy engine whose ring stands still is stalled: healthy=%v %+v", s.healthy("g"), st)
	}
}

func probeWithInfo(t *testing.T, info string) (*RingTrace, *EpState) {
	t.Helper()
	srv := engine(t, "ok")
	e := rtEp("g", "n1", srv.URL+"/v1", "m")
	e.Probe = "any"
	e.Group = &config.Group{Engine: "llama-ring", Workers: []string{"w1"}, StatsPort: traceInfoServer(t, realTrace, info)}
	s := rtServer(t, &config.Config{Endpoints: []config.Endpoint{e}})
	s.routes = func(string) []nodeRoute { return nil }
	s.hosts.m = map[string]HostReport{"w1": {Node: "w1", At: time.Now().Add(time.Hour)}}
	ep := s.Config().Endpoints[0]
	s.probe(context.Background(), &ep)
	st := s.getState("g")
	return st.Group.Trace, st
}

func TestGroup_ChainVictimAndCulprit(t *testing.T) {
	info := `{"engine":"llama-ring","failed_workers":["10.0.0.2:50153"],
	"failure_report":[{"worker":"10.0.0.2:50153","role":"culprit","reason":"no reply in 15 s","cause":""},
	{"worker":"10.0.0.3:50154","role":"victim","reason":"connection dropped","cause":"10.0.0.2:50153"}],
	"worker_features":{"10.0.0.2:50153":31,"10.0.0.3:50154":23}}`
	tr, st := probeWithInfo(t, info)
	if st.OK || !strings.Contains(st.Err, "10.0.0.2:50153 (culprit: no reply in 15 s)") {
		t.Fatalf("culprit with its reason: %+v", st)
	}
	if !strings.Contains(st.Err, "not at fault: 10.0.0.3:50154") || strings.Contains(strings.SplitN(st.Err, "[", 2)[0], "10.0.0.3") {
		t.Fatalf("the victim is a side remark, never the cause: %q", st.Err)
	}
	if tr == nil || len(tr.Report) != 2 || tr.Features["10.0.0.3:50154"] != 23 {
		t.Fatalf("%+v", tr)
	}
}

func TestGroup_ChainTwoLinks(t *testing.T) {
	info := `{"failed_workers":[],"failure_report":[
	{"worker":"a:1","role":"link","reason":"peer link to b:2 broke","cause":"b:2"},
	{"worker":"b:2","role":"link","reason":"peer link to a:1 broke","cause":"a:1"}]}`
	_, st := probeWithInfo(t, info)
	if st.OK || !strings.Contains(st.Err, "a:1 (link") || !strings.Contains(st.Err, "b:2 (link") {
		t.Fatalf("a broken link takes the endpoint down and names both ends: %+v", st)
	}
}

func TestGroup_OnlyVictimsTakeItDownWithoutBlame(t *testing.T) {
	info := `{"failed_workers":[],"failure_report":[{"worker":"c:3","role":"victim","reason":"dropped","cause":"x:9"}]}`
	_, st := probeWithInfo(t, info)
	if st.OK || !strings.Contains(st.Err, "victim only, culprit unknown") || strings.Contains(st.Err, "lost its worker") {
		t.Fatalf("a victim-only report breaks the ring but blames nobody: %+v", st)
	}
}

func TestGroup_OldEngineHasNoReportOrFeatures(t *testing.T) {
	tr, st := probeWithInfo(t, `{"engine":"llama-ring","failed_workers":["w:1"]}`)
	if st.OK || !strings.Contains(st.Err, "w:1") {
		t.Fatalf("%+v", st)
	}
	if tr.Report != nil || tr.Features != nil {
		t.Fatalf("absent fields are unknown: %+v", tr)
	}
	tr, st = probeWithInfo(t, `not json at all`)
	if !st.OK || tr == nil || tr.Features != nil {
		t.Fatalf("an unreadable /ring/info is no error: %+v %+v", tr, st)
	}
}

// A llama-server started with -m /dir/file.gguf lists the full path as the model id: an endpoint registered under the
// file name is up, not "model not listed".
func TestProbeAcceptsTheFileNameOfAListedPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"/usr/share/ollama/models/blobs/sha256-abc"}]}`)
	}))
	t.Cleanup(srv.Close)
	for model, want := range map[string]bool{"sha256-abc": true, "sha256-other": false} {
		e := rtEp("g", "n1", srv.URL+"/v1", model)
		s := rtServer(t, &config.Config{Endpoints: []config.Endpoint{e}})
		ok, _, why := s.call(context.Background(), &e, srv.URL+"/v1/models", nil, time.Second)
		if ok != want {
			t.Errorf("%s: ok=%v (%s)", model, ok, why)
		}
	}
}

func TestGroup_SessionReconnectsTelemetryAndRunEvents(t *testing.T) {
	srv := engine(t, "ok")
	info := `{"engine":"llama-ring","protocol":101,"failed_workers":[],"worker_features":{"10.0.0.2:50152":127},"reconnects":{"10.0.0.2:50152":{"resumed":2,"replayed":5,"reply_resent":1,"last_downtime_ms":142.4}}}`
	e := rtEp("g", "n1", srv.URL+"/v1", "m")
	e.Probe = "any"
	e.Group = &config.Group{
		Engine:    "llama-ring",
		Workers:   []string{"w1"},
		Addrs:     map[string]string{"w1": "10.0.0.2:50152"},
		StatsPort: traceInfoServer(t, realTrace, info),
	}
	s := rtServer(t, &config.Config{Endpoints: []config.Endpoint{e}})
	s.routes = func(string) []nodeRoute { return nil }
	s.hosts.m = map[string]HostReport{"w1": {Node: "w1", At: time.Now().Add(time.Hour)}}

	r, err := s.RecordRun(runPlan(), 1, "g", "llama-ring")
	if err != nil {
		t.Fatal(err)
	}
	ep := s.Config().Endpoints[0]
	s.probe(context.Background(), &ep)

	st := s.getState("g")
	if st == nil || !st.OK || st.Group == nil || st.Group.Trace == nil {
		t.Fatalf("unexpected state: %+v", st)
	}
	tr := st.Group.Trace
	if tr.Features["10.0.0.2:50152"] != 127 || (tr.Features["10.0.0.2:50152"]&FeatSession) == 0 {
		t.Fatalf("expected FeatSession bit in worker_features, got %+v", tr.Features)
	}
	rec, ok := tr.Reconnects["10.0.0.2:50152"]
	if !ok || rec.Resumed != 2 || rec.Replayed != 5 || rec.ReplyResent != 1 || rec.LastDowntimeMS != 142.4 {
		t.Fatalf("unexpected reconnect stat: %+v", tr.Reconnects)
	}
	sum := tr.ReconnectSummary(e.Group)
	if !strings.Contains(sum, "w1 (resumed 2×, replayed 5, resent 1, last downtime 142 ms)") {
		t.Fatalf("unexpected ReconnectSummary: %q", sum)
	}

	got, _ := s.runs.get(r.ID)
	if len(got.Samples) != 1 || got.Samples[0].Reconnects["10.0.0.2:50152"].Resumed != 2 {
		t.Fatalf("expected reconnects in RunSample, got %+v", got.Samples)
	}
	foundEvent := false
	for _, ev := range got.Events {
		if strings.Contains(ev.What, "session resumed: w1 (resumed 2×, replayed 5, resent 1, downtime 142 ms)") {
			foundEvent = true
		}
	}
	if !foundEvent {
		t.Fatalf("expected session resumed event in run events: %+v", got.Events)
	}

	// Probing again with the same resumed count must not duplicate the event
	evCount := len(got.Events)
	s.probe(context.Background(), &ep)
	got2, _ := s.runs.get(r.ID)
	if len(got2.Events) != evCount {
		t.Fatalf("expected no duplicate event when resumed count did not increase: %+v", got2.Events)
	}
}

// A chain stage lists the model name from the GGUF's metadata, not the file name its endpoint is registered under: the one
// model a distributed engine lists is the one it runs. A plain endpoint still has to be listed by name.
func TestProbeAcceptsTheOneModelOfADistributedEngine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"Qwen3.5-122B-A10B-ck2100-v1014-MTP1740"}]}`)
	}))
	t.Cleanup(srv.Close)
	for _, c := range []struct {
		name  string
		group *config.Group
		want  bool
	}{
		{"chain", &config.Group{Engine: "chain"}, true},
		{"ring", &config.Group{Engine: "llama-ring"}, true},
		{"lone chain stage (no group)", nil, true},
		{"plain endpoint", nil, false},
	} {
		e := rtEp("g", "n1", srv.URL+"/v1", "Ornith-1.5-122B-Q4_K_M.gguf")
		e.Group = c.group
		if strings.HasPrefix(c.name, "lone") {
			e.Tags = []string{"distributed", "chain"}
		}
		s := rtServer(t, &config.Config{Endpoints: []config.Endpoint{e}})
		ok, _, why := s.call(context.Background(), &e, srv.URL+"/v1/models", nil, time.Second)
		if ok != c.want {
			t.Errorf("%s: ok=%v (%s)", c.name, ok, why)
		}
	}
	// two listed models and none of them its own: still "model not listed"
	two := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"a"},{"id":"b"}]}`)
	}))
	t.Cleanup(two.Close)
	e := rtEp("g", "n1", two.URL+"/v1", "Ornith.gguf")
	e.Group = &config.Group{Engine: "chain"}
	s := rtServer(t, &config.Config{Endpoints: []config.Endpoint{e}})
	if ok, _, why := s.call(context.Background(), &e, two.URL+"/v1/models", nil, time.Second); ok || why != "model not listed" {
		t.Errorf("ok=%v why=%q", ok, why)
	}
}

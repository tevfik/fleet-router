package server

import (
	"github.com/tevfik/fleet-router/internal/config"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestObserverStream(t *testing.T) {
	o := newObserver(time.Now().Add(-2500*time.Millisecond), true)
	var sb strings.Builder
	for i := 0; i < 20; i++ {
		sb.WriteString(`data: {"choices":[{"delta":{"content":"x"}}]}` + "\n\n")
	}
	sb.WriteString(`data: {"choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":20}}` + "\n\ndata: [DONE]\n\n")
	b := []byte(sb.String())
	o.feed(b[:37]) // a chunk boundary in the middle of a line must not matter
	o.first = time.Time{}
	o.feed(b[37:])
	o.first, o.last = time.Now().Add(-2*time.Second), time.Now()
	r := o.result(5)
	if r.Prompt != 1000 || r.TTFT < 400*time.Millisecond || r.TTFT > time.Second {
		t.Fatalf("prompt/ttft wrong: %+v", r)
	}
	if r.TPS < 8 || r.TPS > 12 { // 19 tokens in ~2s
		t.Fatalf("tps %.1f", r.TPS)
	}
}

// dec makes a decode measurement of tokens over seconds.
func dec(tok int, sec float64, ttft time.Duration, prompt int) obsResult {
	return obsResult{DecTok: tok, DecSec: sec, TPS: float64(tok) / sec, TTFT: ttft, Prompt: prompt, Out: tok + 1}
}

func TestEstimateIsATrueAverage(t *testing.T) {
	var l liveStore
	l.record("a", "m", dec(100, 1, time.Second, 1000))   // 100 tok/s
	l.record("a", "m", dec(1000, 20, time.Second, 2000)) // 50 tok/s, but ten times as many tokens
	x, _ := l.get("a")
	// total tokens / total seconds = 1100/21 ≈ 52.4 — not the 75 a plain mean of the two rates would give
	if x.TPS < 52 || x.TPS > 53 || x.Samples != 2 || x.MaxPrompt != 2000 || x.LastPrompt != 2000 || x.PrefillTPS == 0 {
		t.Fatalf("%+v", x)
	}
	l.record("b", "m", obsResult{Prompt: 10, TTFT: time.Second}) // short prompt: no prefill figure, no decode sample
	if y, _ := l.get("b"); y.PrefillTPS != 0 || y.Samples != 0 || y.TTFTMS != 1000 {
		t.Fatalf("%+v", y)
	}
}

func TestOutliersAreIgnored(t *testing.T) {
	var l liveStore
	for i := 0; i < 8; i++ {
		l.record("a", "m", dec(100, 1, 0, 0)) // steady 100 tok/s
	}
	l.record("a", "m", dec(100, 10, 0, 0)) // one stalled stream: 10 tok/s
	if x, _ := l.get("a"); x.TPS < 99 || x.TPS > 101 {
		t.Fatalf("a single stall moved the estimate to %.1f", x.TPS)
	}
}

func TestFastAdaptationToARealChange(t *testing.T) {
	var l liveStore
	for i := 0; i < 15; i++ {
		l.record("a", "m", dec(100, 1, 0, 0)) // 100 tok/s for a while …
	}
	for i := 0; i < 3; i++ {
		l.record("a", "m", dec(100, 4, 0, 0)) // … then three streams at 25 tok/s (GPU now shared)
	}
	if x, _ := l.get("a"); x.TPS > 30 {
		t.Fatalf("three consecutive slow streams should reset the window, estimate is still %.1f", x.TPS)
	}
}

func TestStatsSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "stats.json")
	var a liveStore
	a.record("e", "model-1", dec(100, 1, 300*time.Millisecond, 500))
	a.bump("e", true)
	a.bump("e", false)
	if err := a.save(path); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("stats file: %v %v", fi, err)
	}
	var b liveStore
	if err := b.load(path); err != nil {
		t.Fatal(err)
	}
	x, ok := b.get("e")
	if !ok || x.TPS < 99 || x.TTFTMS != 300 || x.Requests != 1 || x.Errors != 1 || x.TokensIn != 500 {
		t.Fatalf("after reload: %+v", x)
	}
	// a re-pointed endpoint starts from scratch, but keeps nothing of the old model's speed
	b.record("e", "model-2", dec(10, 1, 0, 0))
	if y, _ := b.get("e"); y.TPS > 11 || y.Samples != 1 {
		t.Fatalf("new model inherited old figures: %+v", y)
	}
	// stale samples are dropped on load, a missing file is fine
	if err := (&liveStore{}).load(filepath.Join(t.TempDir(), "none.json")); err != nil {
		t.Fatal(err)
	}
}

func TestFastestStrategy(t *testing.T) {
	s := &Server{state: map[string]*EpState{}, lims: map[string]*limiter{}}
	eps := []config.Endpoint{{Name: "slow", TPS: 20}, {Name: "nodata"}, {Name: "fast", TPS: 100}, {Name: "bursty", TPS: 10}}
	for _, e := range eps {
		s.state[e.Name] = &EpState{OK: true}
	}
	s.live.record("bursty", "m", dec(200, 1, 600*time.Millisecond, 0)) // live beats the saved benchmark
	rc := config.Role{Strategy: "fastest"}
	got := s.applyStrategy("r", rc, eps)
	want := []string{"bursty", "fast", "slow", "nodata"}
	for i, n := range want {
		if got[i].Name != n {
			t.Fatalf("order %v, want %v", names(got), want)
		}
	}
	// load stretches the estimate: "bursty" (2.1s) with its only slot taken counts 4.2s and loses to idle "fast" (3.5s)
	s.lim("ep:bursty").setCap(1)
	s.lim("ep:bursty").try()
	if got := s.applyStrategy("r", rc, eps); got[0].Name != "fast" {
		t.Fatalf("a loaded endpoint should lose to an idle, similar-speed one: %v", names(got))
	}
}

func names(es []config.Endpoint) (n []string) {
	for _, e := range es {
		n = append(n, e.Name)
	}
	return
}

func TestV1Auth(t *testing.T) {
	t.Setenv("FLEET_TEST_KEY", "s3cret")
	s := &Server{cfg: &config.Config{APIKeyEnv: "FLEET_TEST_KEY"}, state: map[string]*EpState{}, lims: map[string]*limiter{}}
	h := s.v1Auth(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	try := func(remote, auth string) int {
		r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		r.RemoteAddr = remote
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		w := httptest.NewRecorder()
		h(w, r)
		return w.Code
	}
	if c := try("127.0.0.1:5000", ""); c != 200 {
		t.Fatalf("loopback needs no key, got %d", c)
	}
	if c := try("10.0.1.9:5000", ""); c != 401 {
		t.Fatalf("remote without key got %d", c)
	}
	if c := try("10.0.1.9:5000", "Bearer wrong"); c != 401 {
		t.Fatalf("remote with wrong key got %d", c)
	}
	if c := try("10.0.1.9:5000", "s3cret"); c != 401 {
		t.Fatalf("key without the Bearer scheme got %d", c)
	}
	if c := try("[::1]:5000", ""); c != 200 {
		t.Fatalf("IPv6 loopback needs no key, got %d", c)
	}
	if c := try("10.0.1.9:5000", "bearer s3cret"); c != 200 {
		t.Fatalf("scheme is case-insensitive, got %d", c)
	}
	if c := try("10.0.1.9:5000", "Bearer s3cret"); c != 200 {
		t.Fatalf("remote with key got %d", c)
	}
}

// A non-stream reply's wall time holds the queue, a cold model load and the prefill: it must not become a decode
// sample (a 2-token answer after a 5 s load read as 0.4 tok/s). The server's own timings, when sent, are exact.
func TestObserver_NonStreamOnlyFromTimings(t *testing.T) {
	o := newObserver(time.Now().Add(-5*time.Second), false)
	o.feed([]byte(`{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":12,"completion_tokens":2}}`))
	if r := o.result(0); r.DecTok != 0 || r.TPS != 0 || r.Out != 2 || r.Prompt != 12 {
		t.Fatalf("plain non-stream reply gave a speed: %+v", r)
	}
	o = newObserver(time.Now().Add(-60*time.Second), false)
	o.feed([]byte(`{"usage":{"prompt_tokens":147,"completion_tokens":295},"timings":{"prompt_n":147,"prompt_ms":4603.75,"predicted_n":295,"predicted_ms":40207.79}}`))
	r := o.result(0)
	if r.DecTok != 295 || r.TPS < 7.3 || r.TPS > 7.4 || r.PreTok != 147 {
		t.Fatalf("timings not used: %+v", r)
	}
}

// In a stream the server's timings beat the chunk clock (several tokens per chunk with speculation, network jitter).
func TestObserver_StreamTimingsWin(t *testing.T) {
	o := newObserver(time.Now(), true)
	for i := 0; i < 5; i++ {
		o.feed([]byte(`data: {"choices":[{"delta":{"content":"x"}}]}` + "\n"))
	}
	o.feed([]byte(`data: {"choices":[{"delta":{}}],"timings":{"prompt_n":500,"prompt_ms":250,"predicted_n":40,"predicted_ms":400}}` + "\n"))
	r := o.result(0)
	if r.DecTok != 40 || r.DecSec != 0.4 || r.TPS != 100 || r.PreTok != 500 || r.PreSec != 0.25 {
		t.Fatalf("%+v", r)
	}
	var l liveStore
	l.record("e", "m", r)
	if lv, _ := l.get("e"); lv.TPS != 100 || lv.PrefillTPS != 2000 {
		t.Fatalf("live %+v", lv)
	}
}

// Prompt tokens served from the cache are not prefill work: 10 000 tokens of which 9 900 cached is a 100-token prefill.
func TestLive_PrefillExcludesCachedTokens(t *testing.T) {
	o := newObserver(time.Now().Add(-time.Second), true)
	o.feed([]byte(`data: {"choices":[{"delta":{"content":"a"}}],"usage":{"prompt_tokens":10000,"prompt_tokens_details":{"cached_tokens":9900},"completion_tokens":1}}` + "\n"))
	r := o.result(0)
	if r.Prompt != 10000 || r.Cached != 9900 {
		t.Fatalf("%+v", r)
	}
	var l liveStore
	l.record("e", "m", r)
	if lv, _ := l.get("e"); lv.PrefillTPS != 0 || lv.TokensIn != 10000 {
		t.Fatalf("cached prompt counted as prefill: %+v", lv)
	}
	r.Cached = 0
	l.record("f", "m", r)
	if lv, _ := l.get("f"); lv.PrefillTPS <= 0 {
		t.Fatalf("uncached prompt gave no prefill figure: %+v", lv)
	}
}

// Samples saved by the old measurement (version 1) are dropped on load; totals stay.
func TestLive_LoadDropsV1Samples(t *testing.T) {
	p := filepath.Join(t.TempDir(), "live.json")
	now := time.Now().Format(time.RFC3339Nano)
	os.WriteFile(p, []byte(`{"version":1,"endpoints":{"e":{"model":"m","decode":[{"t":"`+now+`","n":2,"s":5}],"ttft":[{"t":"`+now+`","n":100,"s":1}],"tokens_out":2}}}`), 0o600)
	var l liveStore
	if err := l.load(p); err != nil {
		t.Fatal(err)
	}
	if lv, _ := l.get("e"); lv.Samples != 0 || lv.TPS != 0 || lv.TokensOut != 2 || lv.TTFTMS != 100 {
		t.Fatalf("%+v", lv)
	}
}

package server

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

// ---------------------------------------------------------------- computeCost: the terms

func TestCost_IdleWarmModel(t *testing.T) {
	c := computeCost(costIn{TPS: 50, PrefillTPS: 1000, TTFT: 0.2, Cap: 2}, reqSize{In: 2000, Out: 500})
	if c.Wait != 0 || c.Load != 0 {
		t.Fatalf("idle warm model has nothing to wait for: %+v", c)
	}
	if !near(c.Prefill, 0.2+2) || !near(c.Decode, 10) || !near(c.Total, 12.2) {
		t.Fatalf("prompt 0.2+2000/1000, reply 500/50: %+v", c)
	}
}

func TestCost_UnknownSpeedIsInfinite(t *testing.T) {
	c := computeCost(costIn{}, reqSize{})
	if !c.Unknown || !math.IsInf(c.Total, 1) {
		t.Fatalf("no speed → unknown, +Inf: %+v", c)
	}
	b, err := json.Marshal(c)
	if err != nil || !strings.Contains(string(b), `"total":null`) {
		t.Fatalf("+Inf must serialise as null, got %s (%v)", b, err)
	}
	if b, _ := json.Marshal(computeCost(costIn{TPS: 10}, reqSize{})); !strings.Contains(string(b), `"total":`) || strings.Contains(string(b), "null") {
		t.Fatalf("a known cost serialises as a number: %s", b)
	}
}

func TestCost_DefaultOutputTokens(t *testing.T) {
	c := computeCost(costIn{TPS: 30}, reqSize{})
	if !near(c.Decode, float64(defaultOutTokens)/30) {
		t.Fatalf("no max_tokens → %d tokens: %+v", defaultOutTokens, c)
	}
}

func TestCost_PromptSpeedFallsBackToDecodeMultiple(t *testing.T) {
	c := computeCost(costIn{TPS: 25}, reqSize{In: 2000, Out: 1})
	if !near(c.Prefill, 2000/(25*prefillPerDecode)) {
		t.Fatalf("unmeasured prompt speed = %g × decode: %+v", prefillPerDecode, c)
	}
}

func TestCost_ColdModelPaysItsLoad(t *testing.T) {
	big := computeCost(costIn{TPS: 20, Cold: true, SizeGB: 45}, reqSize{Out: 100})
	if !near(big.Load, 45/loadGBps) {
		t.Fatalf("45 GB at %g GB/s: %+v", loadGBps, big)
	}
	unknown := computeCost(costIn{TPS: 20, Cold: true}, reqSize{Out: 100})
	if unknown.Load != unknownLoadSec {
		t.Fatalf("cold, size unknown: %+v", unknown)
	}
	if warm := computeCost(costIn{TPS: 20, SizeGB: 45}, reqSize{Out: 100}); warm.Load != 0 {
		t.Fatalf("a loaded model costs no load: %+v", warm)
	}
}

func TestCost_QueueOnlyWhenSlotsAreTaken(t *testing.T) {
	free := computeCost(costIn{TPS: 100, Cap: 2, Used: 1}, reqSize{Out: 100})
	if free.Wait != 0 {
		t.Fatalf("a free slot means no wait: %+v", free)
	}
	full := computeCost(costIn{TPS: 100, Cap: 1, Used: 1, TTFT: 0}, reqSize{Out: 100})
	if !near(full.Wait, 1) { // one ahead on one slot: one request's own time (100/100 s)
		t.Fatalf("full single slot waits one request: %+v", full)
	}
	deep := computeCost(costIn{TPS: 100, Cap: 1, Used: 1, Waiting: 2}, reqSize{Out: 100})
	if !near(deep.Wait, 3) {
		t.Fatalf("two queued plus the running one: %+v", deep)
	}
	if nolimit := computeCost(costIn{TPS: 100, Used: 50}, reqSize{Out: 100}); nolimit.Wait != 0 {
		t.Fatalf("an endpoint with no slot limit never queues: %+v", nolimit)
	}
}

func TestCost_BusyMachineStretchesDecodeOnly(t *testing.T) {
	idle := computeCost(costIn{TPS: 50, PrefillTPS: 500}, reqSize{In: 1000, Out: 100})
	busy := computeCost(costIn{TPS: 50, PrefillTPS: 500, NodeBusy: 2}, reqSize{In: 1000, Out: 100})
	if !near(busy.Decode, idle.Decode*(1+2*streamShare)) || busy.Prefill != idle.Prefill {
		t.Fatalf("two other streams stretch the reply, not the prompt: idle %+v busy %+v", idle, busy)
	}
}

func TestCost_StrainDoublesDecode(t *testing.T) {
	ok := computeCost(costIn{TPS: 50}, reqSize{Out: 100})
	bad := computeCost(costIn{TPS: 50, Strained: true}, reqSize{Out: 100})
	if !near(bad.Decode, ok.Decode*strainFactor) {
		t.Fatalf("strained: %+v vs %+v", bad, ok)
	}
	if !strings.Contains(strings.Join(bad.Notes, ","), "strained") {
		t.Fatalf("the notes say why: %v", bad.Notes)
	}
}

// ---------------------------------------------------------------- the choices cost makes

func TestCost_LongPromptPrefersFastPrompt(t *testing.T) {
	// a decodes faster but crunches prompts slowly (a unified-memory box); b is the reverse
	a := computeCost(costIn{TPS: 60, PrefillTPS: 200}, reqSize{In: 20000, Out: 200})
	b := computeCost(costIn{TPS: 40, PrefillTPS: 4000}, reqSize{In: 20000, Out: 200})
	if b.Total >= a.Total {
		t.Fatalf("a 20k-token prompt belongs on the fast-prompt machine: a=%.1f b=%.1f", a.Total, b.Total)
	}
	a = computeCost(costIn{TPS: 60, PrefillTPS: 200}, reqSize{In: 50, Out: 600})
	b = computeCost(costIn{TPS: 40, PrefillTPS: 4000}, reqSize{In: 50, Out: 600})
	if a.Total >= b.Total {
		t.Fatalf("a short prompt with a long reply belongs on the fast decoder: a=%.1f b=%.1f", a.Total, b.Total)
	}
}

func TestCost_ColdBigLosesToWarmSmall_ColdSmallDoesNot(t *testing.T) {
	warmSlow := computeCost(costIn{TPS: 15}, reqSize{Out: 100})                         // 6.7 s
	coldBig := computeCost(costIn{TPS: 60, Cold: true, SizeGB: 70}, reqSize{Out: 100})  // 47 s load
	coldSmall := computeCost(costIn{TPS: 60, Cold: true, SizeGB: 3}, reqSize{Out: 100}) // 2 s load
	if coldBig.Total <= warmSlow.Total {
		t.Fatalf("loading 70 GB costs more than a slow warm model: %.1f vs %.1f", coldBig.Total, warmSlow.Total)
	}
	if coldSmall.Total >= warmSlow.Total {
		t.Fatalf("a small cold model on a fast machine still wins: %.1f vs %.1f", coldSmall.Total, warmSlow.Total)
	}
}

// ---------------------------------------------------------------- on a server

func costServer(t *testing.T, tps map[string]float64) *Server {
	s, _ := rt3(t, config.Role{Preferred: "a", Fallback: []string{"b", "c"}, Strategy: "cost"})
	cfg := s.Config()
	cfg.Endpoints = append([]config.Endpoint{}, cfg.Endpoints...)
	for i := range cfg.Endpoints {
		cfg.Endpoints[i].TPS = tps[cfg.Endpoints[i].Name]
	}
	s.cfg = cfg
	return s
}

func order(s *Server, sz reqSize) string {
	_, role, rc := s.route("r")
	return rtNames(s.applyStrategySized(role, rc, s.Resolve("r"), sz))
}

func TestCost_Strategy_OrdersByExpectedTime(t *testing.T) {
	s := costServer(t, map[string]float64{"a": 20, "b": 100, "c": 50})
	if got := order(s, reqSize{}); got != "b,c,a" {
		t.Fatalf("order %s, want b,c,a", got)
	}
	if got := rtDo(t, s, "POST", "/v1/chat/completions", rtChat("r", false)).Header().Get("X-Fleet-Endpoint"); got != "b" {
		t.Fatalf("picked %q", got)
	}
}

func TestCost_Strategy_NoSpeedLast_StableTies(t *testing.T) {
	s := costServer(t, map[string]float64{"b": 40}) // a, c unknown
	if got := order(s, reqSize{}); got != "b,a,c" {
		t.Fatalf("known first, unknown keep their pool order: %s", got)
	}
}

func TestCost_Strategy_ColdModelWaitsItsTurn(t *testing.T) {
	s := costServer(t, map[string]float64{"a": 100, "b": 60, "c": 30})
	cold := false
	s.warm.set("a", ModelInfo{Loaded: &cold, Size: 60 << 30}) // fastest, but 40 s to load
	if got := order(s, reqSize{}); got != "b,c,a" {
		t.Fatalf("cold a goes behind warm b and c: %s", got)
	}
	warm := true
	s.warm.set("a", ModelInfo{Loaded: &warm, Size: 60 << 30})
	if got := order(s, reqSize{}); got != "a,b,c" {
		t.Fatalf("once loaded a is first again: %s", got)
	}
}

func TestCost_Strategy_StrainAndQueue(t *testing.T) {
	s := costServer(t, map[string]float64{"a": 100, "b": 60, "c": 30})
	rtStrain(s, "n1", time.Now()) // a's machine is struggling: 100 tok/s counts as 50
	if got := order(s, reqSize{}); got != "b,a,c" {
		t.Fatalf("strained a: %s", got)
	}
	s.cfg.AvoidStrain = rtBool(false)
	if got := order(s, reqSize{}); got != "a,b,c" {
		t.Fatalf("avoid_strain off ignores it: %s", got)
	}
	s.cfg.AvoidStrain = nil
	s.cfg.Endpoints[2].TPS = 55
	if got := order(s, reqSize{}); strings.Index(got, "b") > strings.Index(got, "c") {
		t.Fatalf("with no queue b (60 tok/s) is ahead of c (55): %s", got)
	}
	s.lim("ep:b").setCap(1)
	s.lim("ep:b").try() // b's only slot is taken: a request would wait a whole reply for it
	if got := order(s, reqSize{}); strings.Index(got, "c") > strings.Index(got, "b") {
		t.Fatalf("a queue in front of b sends the request to c: %s", got)
	}
}

func TestCost_Strategy_BusyMachineSharedAcrossItsEndpoints(t *testing.T) {
	s := costServer(t, map[string]float64{"a": 100, "b": 100, "c": 100})
	cfg := s.Config()
	cfg.Endpoints = append([]config.Endpoint{}, cfg.Endpoints...)
	cfg.Endpoints[1].Node = "n1" // b lives on a's machine
	s.cfg = cfg
	s.lim("ep:a").try() // a stream runs on n1 through a
	s.lim("ep:a").try()
	if got := order(s, reqSize{}); !strings.HasPrefix(got, "c") {
		t.Fatalf("n1 is busy, so its endpoints (a, b) both cost more than c on idle n3: %s", got)
	}
}

func TestCost_Strategy_LiveSpeedBeatsBench(t *testing.T) {
	s := costServer(t, map[string]float64{"a": 100, "b": 50, "c": 20})
	s.live.record("a", "ma", dec(100, 10, 200*time.Millisecond, 0)) // measured 10 tok/s
	if got := order(s, reqSize{}); got != "b,c,a" {
		t.Fatalf("measured 10 tok/s overrides the benchmark's 100: %s", got)
	}
}

func TestCost_PrefersPromptSpeedForBigRequests(t *testing.T) {
	s := costServer(t, map[string]float64{"a": 60, "b": 40, "c": 10})
	s.live.record("a", "ma", dec(120, 2, 200*time.Millisecond, 8000)) // a: 8000-token prompt took ~a while
	s.live.record("b", "mb", dec(80, 2, 100*time.Millisecond, 8000))
	la, _ := s.live.get("a")
	lb, _ := s.live.get("b")
	// whatever the figures are, the strategy must agree with the cost terms of the two
	ca, cb := s.costOf(*s.Config().Endpoint("a"), reqSize{In: 20000, Out: 100}), s.costOf(*s.Config().Endpoint("b"), reqSize{In: 20000, Out: 100})
	want := "a"
	if cb.Total < ca.Total {
		want = "b"
	}
	if got := order(s, reqSize{In: 20000, Out: 100}); !strings.HasPrefix(got, want) {
		t.Fatalf("order %s disagrees with the costs a=%.1f b=%.1f (live %+v %+v)", got, ca.Total, cb.Total, la, lb)
	}
}

func TestCost_DownEndpointStaysLast(t *testing.T) {
	s := costServer(t, map[string]float64{"a": 200, "b": 50, "c": 20})
	s.state["a"] = &EpState{OK: false}
	if got := order(s, reqSize{}); got != "b,c,a" {
		t.Fatalf("a is down: %s", got)
	}
}

func TestCost_SizeOf(t *testing.T) {
	raw := []byte(strings.Repeat("x", 400))
	if sz := sizeOf(raw, map[string]any{}); sz.In != 100 || sz.Out != 0 {
		t.Fatalf("%+v", sz)
	}
	if sz := sizeOf(raw, map[string]any{"max_tokens": 64.0}); sz.Out != 64 {
		t.Fatalf("%+v", sz)
	}
	if sz := sizeOf(raw, map[string]any{"max_completion_tokens": 32.0}); sz.Out != 32 {
		t.Fatalf("%+v", sz)
	}
}

func TestCost_ConfigAcceptsStrategy(t *testing.T) {
	cfg := &config.Config{Endpoints: []config.Endpoint{{Name: "a", Node: "n", URL: "http://x", Model: "m"}}, Roles: map[string]config.Role{"r": {Preferred: "a", Strategy: "cost"}}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("strategy cost: %v", err)
	}
	if !cfg.Roles["r"].WarmFirst() {
		t.Fatal("pooled strategies prefer warm models")
	}
}

// ---------------------------------------------------------------- fleet route (explain)

func TestCost_Explain_Endpoint(t *testing.T) {
	s := costServer(t, map[string]float64{"a": 20, "b": 100, "c": 50})
	rec := rtDo(t, s, "GET", "/fleet/route?model=r&in=4000&out=250", nil)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var r RouteExplain
	rtJSON(t, rec, &r)
	if r.Role != "r" || r.Strategy != "cost" || r.In != 4000 || r.Out != 250 || len(r.Order) != 3 {
		t.Fatalf("%+v", r)
	}
	if r.Order[0].Endpoint != "b" || r.Order[2].Endpoint != "a" || !(r.Order[0].Total < r.Order[1].Total) {
		t.Fatalf("explain shows the order a request would take: %+v", r.Order)
	}
	if rtDo(t, s, "GET", "/fleet/route", nil).Code != 400 {
		t.Fatal("model= is required")
	}
	if rtDo(t, s, "GET", "/fleet/route?model=r", nil, rtRemote).Code != 403 {
		t.Fatal("explain is an admin call")
	}
}

func TestCost_Explain_UnknownSpeedAndDownSerialise(t *testing.T) {
	s := costServer(t, map[string]float64{"b": 50})
	s.state["c"] = &EpState{OK: false}
	rec := rtDo(t, s, "GET", "/fleet/route?model=r", nil)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var r RouteExplain
	rtJSON(t, rec, &r)
	if !r.Order[1].Unknown || !r.Order[2].Down {
		t.Fatalf("a has no speed, c is down: %+v", r.Order)
	}
}

func TestCost_Explain_DoesNotMoveRoundRobinOrTakeSlots(t *testing.T) {
	s, _ := rt3(t, config.Role{Preferred: "a", Fallback: []string{"b", "c"}, Strategy: "round-robin"})
	before := rtNames(s.Resolve("r"))
	for i := 0; i < 3; i++ {
		rtDo(t, s, "GET", "/fleet/route?model=r", nil)
	}
	if used, _, _ := s.lim("ep:a").stats(); used != 0 {
		t.Fatalf("explaining took a slot: %d", used)
	}
	_, role, rc := s.route("r")
	if got := rtNames(s.applyStrategy(role, rc, s.Resolve("r"))); got != before {
		t.Fatalf("explain advanced the rotation: first real request got %s, want %s", got, before)
	}
}

func TestCost_Explain_Model(t *testing.T) {
	s := costServer(t, map[string]float64{"a": 20})
	rec := rtDo(t, s, "GET", "/fleet/route?model=ma", nil)
	var r RouteExplain
	rtJSON(t, rec, &r)
	if rec.Code != 200 || r.Role != "" || r.Strategy != "failover" || len(r.Order) != 1 || r.Order[0].Endpoint != "a" {
		t.Fatalf("%d %+v", rec.Code, r)
	}
}

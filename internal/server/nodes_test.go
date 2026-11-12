package server

import (
	"strings"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
)

func TestNodePings(t *testing.T) {
	cfg := &config.Config{Endpoints: []config.Endpoint{
		{Name: "a", Node: "dino", SSH: "dino"}, {Name: "b", Node: "dino", SSH: "dino"},
		{Name: "c", Node: "dino", SSH: "dino", Probe: "chat"}, // generates a token: not a ping
		{Name: "d", Node: "victor", URL: "http://127.0.0.1:1920/v1"},
	}}
	now := time.Now()
	st := map[string]*EpState{"a": {OK: true, Latency: 30, Checked: now}, "b": {OK: true, Latency: 12, Checked: now},
		"c": {OK: true, Latency: 2, Checked: now}, "d": {OK: false, Latency: 5, Checked: now}}
	var p pingStore
	p.record(cfg, func(n string) *EpState { return st[n] })
	if h := p.get("dino"); len(h) != 1 || h[0] != 12 {
		t.Fatalf("dino ping should be its fastest health check: %v", h)
	}
	if h := p.get("victor"); len(h) != 0 {
		t.Fatalf("a failed check is no ping: %v", h)
	}
	p.record(cfg, func(n string) *EpState { return st[n] }) // nothing new checked
	if h := p.get("dino"); len(h) != 1 {
		t.Fatalf("the same check must not be sampled twice: %v", h)
	}
}

func TestProbeMerge(t *testing.T) {
	r := &ProbeResult{Found: []string{"chat", "tools"}, Missing: []string{"vision", "thinking"}}
	if got := r.merged([]string{"chat", "vision", "audio"}); len(got) != 3 || got[0] != "chat" || got[1] != "audio" || got[2] != "tools" {
		t.Fatalf("merged: %v", got)
	}
	e := &config.Endpoint{Model: "llava", Caps: []string{"chat", "vision"}}
	if !r.Differs(e) {
		t.Fatal("a probe that found no vision differs from caps that claim it")
	}
	if (&ProbeResult{Err: "tunnel down"}).Differs(e) {
		t.Fatal("a failed probe says nothing")
	}
	if via(config.Endpoint{URL: "http://127.0.0.1:11434/v1"}) != "local" || via(config.Endpoint{SSH: "dino"}) != "ssh dino" || via(config.Endpoint{Peer: "b"}) != "⇄ b" {
		t.Fatal("via")
	}
}

func TestErrMessage(t *testing.T) {
	if m := errMessage([]byte(`{"error":{"message":"Multimodal data provided"}}`)); m != "Multimodal data provided" {
		t.Fatal(m)
	}
	if m := errMessage([]byte(`{"error":"model not found"}`)); m != "model not found" {
		t.Fatal(m)
	}
}

func TestThisDeviceIsANode(t *testing.T) {
	t.Setenv("FLEET_STATS", t.TempDir()+"/stats.json")
	s := &Server{cfg: &config.Config{Endpoints: []config.Endpoint{{Name: "a", Node: "dino", Model: "m", SSH: "dino"}}},
		state: map[string]*EpState{}, lims: map[string]*limiter{}, tunnels: newTunnels()}
	ns := s.Nodes(s.Status().Endpoints)
	if len(ns) != 2 || !ns[0].Self || ns[0].Via != "this device" || ns[0].Node != hostLabel() || ns[1].Node != "dino" {
		t.Fatalf("this machine must be listed first even without models: %+v", ns)
	}
	if err := s.Probe([]string{"dino"}); err == nil || !strings.Contains(err.Error(), "only this machine's models") {
		t.Fatalf("another machine's models must not be probed from here: %v", err)
	}
}

package main

import (
	"strings"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/client"
	"github.com/tevfik/fleet-router/internal/tunnel"
)

func TestHubStatusLinesShowRefusalAndNextRetry(t *testing.T) {
	now := time.Now()
	hs := client.HubStatus{HubURL: "https://hub.example", AutoJoin: true, Status: tunnel.Status{
		LastError: "hub answered 401: this hub requires an API key", LastErrorAt: now.Add(-20 * time.Second),
		Rejected: true, Attempts: 4, NextRetry: now.Add(45 * time.Second)}}
	all := strings.Join(hubStatusLines(hs, now), "\n")
	for _, w := range []string{"NOT connected", "401", "REFUSED", "4 failed", "next attempt in 45s"} {
		if !strings.Contains(all, w) {
			t.Errorf("missing %q in:\n%s", w, all)
		}
	}
	if got := hubStatusLines(client.HubStatus{}, now); got != nil {
		t.Errorf("no hub, nothing to say: %v", got)
	}
	ok := strings.Join(hubStatusLines(client.HubStatus{HubURL: "h", Connected: true, Insecure: true, ActiveStreams: map[string]int{"w": 2}}, now), "\n")
	if !strings.Contains(ok, "connected") || !strings.Contains(ok, "not verified") || !strings.Contains(ok, "w:2") {
		t.Errorf("%s", ok)
	}
}

func TestHubStreamWarning(t *testing.T) {
	if hubStreamWarning(nil) != "" || hubStreamWarning(map[string]int{}) != "" {
		t.Fatal("no streams, no warning")
	}
	w := hubStreamWarning(map[string]int{"papa": 2, "dino": 1})
	if !strings.Contains(w, "3 open stream") || !strings.Contains(w, "dino:1 papa:2") || !strings.Contains(w, "restarts it") {
		t.Fatal(w)
	}
}

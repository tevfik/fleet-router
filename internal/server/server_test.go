package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
)

func TestLimiterPriorityOrder(t *testing.T) {
	l := &limiter{cap: 1}
	if !l.try() {
		t.Fatal("first try must succeed")
	}
	got := make(chan string, 2)
	go func() { l.acquire(context.Background(), 0); got <- "low"; l.release() }()
	time.Sleep(30 * time.Millisecond)
	go func() { l.acquire(context.Background(), 10); got <- "high"; l.release() }()
	time.Sleep(30 * time.Millisecond)
	l.release()
	if first := <-got; first != "high" {
		t.Fatalf("first served = %s, want high", first)
	}
}

func TestRequestNeedsAndFilter(t *testing.T) {
	body := map[string]any{"messages": []any{map[string]any{"content": []any{map[string]any{"type": "image_url"}}}}}
	need := requestNeeds("/v1/chat/completions", body)
	if len(need) != 2 || need[1] != "vision" {
		t.Fatalf("need = %v", need)
	}
	eps := []config.Endpoint{{Name: "plain", Model: "qwen3:8b"}, {Name: "v", Model: "x", Caps: []string{"chat", "vision"}}}
	if got := filterNeeds(eps, need); len(got) != 1 || got[0].Name != "v" {
		t.Fatalf("filter = %v", got)
	}
	if got := filterNeeds(eps, requestNeeds("/v1/embeddings", nil)); len(got) != 0 {
		t.Fatalf("no embed endpoint expected, got %v", got)
	}
}

func TestCtxFilterKeepsAllWhenNothingFits(t *testing.T) {
	eps := []config.Endpoint{{Name: "s", Ctx: 2000}, {Name: "l", Ctx: 100000}}
	if got := filterCtx(eps, 5000); len(got) != 1 || got[0].Name != "l" {
		t.Fatalf("got %v", got)
	}
	if got := filterCtx(eps, 500000); len(got) != 2 {
		t.Fatalf("when nothing fits the list must stay, got %v", got)
	}
}

func TestEmbedDimensionGuard(t *testing.T) {
	c := &config.Config{
		Endpoints: []config.Endpoint{
			{Name: "e1", URL: "http://x/v1", Model: "a-embed", Kind: "embed", Dim: 768},
			{Name: "e2", URL: "http://x/v1", Model: "b-embed", Kind: "embed", Dim: 384},
		},
		Roles: map[string]config.Role{"embed": {Preferred: "e1", Fallback: []string{"e2"}}},
	}
	if err := c.Validate(); err == nil {
		t.Fatal("mixing 768 and 384 dimensions must be rejected")
	}
}

func TestLocalPortsListsOnlyOpenTunnels(t *testing.T) {
	tn := newTunnels()
	tn.set("a|x", 41000, "")
	tn.set("b|y", 0, "tunnel dropped: exit status 255")
	got := tn.LocalPorts()
	if len(got) != 1 || got[41000] != "a|x" {
		t.Fatalf("LocalPorts = %v, want only the open tunnel", got)
	}
}

// a malformed url in the configuration must fail that candidate, not crash the request
func TestMalformedEndpointURLDoesNotPanic(t *testing.T) {
	be := fakeBackend()
	defer be.Close()
	s := &Server{
		cfg: &config.Config{
			Endpoints: []config.Endpoint{{Name: "bad", Node: "n", URL: "http://[::1", Model: "m"}, {Name: "good", Node: "n", URL: be.URL, Model: "m"}},
			Roles:     map[string]config.Role{"r": {Preferred: "bad", Fallback: []string{"good"}}},
		},
		state: map[string]*EpState{}, lims: map[string]*limiter{}, client: http.DefaultClient,
	}
	front := httptest.NewServer(s.Handler())
	defer front.Close()
	resp, err := http.Post(front.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"r","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("X-Fleet-Endpoint") != "good" {
		t.Fatalf("the request should fall back to the working endpoint: %d via %q", resp.StatusCode, resp.Header.Get("X-Fleet-Endpoint"))
	}
}

func TestContainsAndRemoveNeed(t *testing.T) {
	need := []string{"chat", "vision", "tools"}
	if !containsNeed(need, "vision") {
		t.Fatal("expected vision to be found")
	}
	if containsNeed(need, "audio") {
		t.Fatal("expected audio not to be found")
	}
	rem := removeNeed(need, "vision")
	if containsNeed(rem, "vision") || len(rem) != 2 {
		t.Fatalf("expected vision removed, got %v", rem)
	}
}

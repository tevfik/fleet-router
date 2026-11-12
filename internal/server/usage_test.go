package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
)

func usageServer(t *testing.T) (*Server, *httptest.Server, func()) {
	be := fakeBackend()
	s := &Server{
		cfg: &config.Config{
			Endpoints: []config.Endpoint{
				{Name: "a", Node: "dino", URL: be.URL, Model: "gemma", Tags: []string{"fast"}},
				{Name: "b", Node: "victor", URL: be.URL, Model: "bonsai", Tags: []string{"fast"}},
			},
			Roles: map[string]config.Role{"scout": {Match: []string{"tag:fast"}, Strategy: "least-busy"}},
		},
		state: map[string]*EpState{}, lims: map[string]*limiter{}, client: http.DefaultClient,
	}
	front := httptest.NewServer(s.Handler())
	return s, front, func() { front.Close(); be.Close() }
}

func ask(t *testing.T, url, model, client string) *http.Response {
	req, _ := http.NewRequest("POST", url+"/v1/chat/completions", strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", client)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	return r
}

func TestNodeModelRoutingAndUsage(t *testing.T) {
	s, front, done := usageServer(t)
	defer done()
	if r := ask(t, front.URL, "dino/gemma", "opencode/1.2.3"); r.Header.Get("X-Fleet-Endpoint") != "a" {
		t.Fatalf("node/model went to %q", r.Header.Get("X-Fleet-Endpoint"))
	}
	if r := ask(t, front.URL, "victor/bonsai", "claude-cli/2.0 (external)"); r.Header.Get("X-Fleet-Endpoint") != "b" {
		t.Fatalf("node/model went to %q", r.Header.Get("X-Fleet-Endpoint"))
	}
	ask(t, front.URL, "scout", "opencode/1.2.3") // a role built from tags only
	ask(t, front.URL, "scout", "opencode/1.2.3")
	ask(t, front.URL, "nope", "curl/8")

	// a request is counted once its handler returns, which can be just after the client has read the answer
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		n := 0
		for _, r := range s.Usage("client", "", time.Hour) {
			n += r.Requests
		}
		if n == 5 {
			break
		}
	}
	byClient := s.Usage("client", "", time.Hour)
	if len(byClient) != 3 || byClient[0].Key != "opencode" || byClient[0].Requests != 3 || byClient[0].In != 21 || byClient[0].Out != 6 {
		t.Fatalf("by client: %+v", byClient)
	}
	for _, r := range byClient {
		if r.Key == "curl" && r.Errors != 1 {
			t.Fatalf("the 404 must count as an error: %+v", r)
		}
	}
	byRole := s.Usage("role", "", time.Hour)
	if byRole[0].Key != "-" && byRole[0].Key != "scout" {
		t.Fatalf("by role: %+v", byRole)
	}
	byModel := s.Usage("model", "", time.Hour)
	seen := map[string]int{}
	for _, r := range byModel {
		seen[r.Key] = r.Requests
	}
	if seen["dino/gemma"] == 0 || seen["victor/bonsai"] == 0 || seen["-"] != 1 {
		t.Fatalf("by model: %v", seen)
	}

	// the report is served over the admin API and survives a restart
	resp, err := http.Get(front.URL + "/fleet/usage?by=day&since=24h")
	if err != nil {
		t.Fatal(err)
	}
	var out struct{ Rows []UsageRow }
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if len(out.Rows) != 1 || out.Rows[0].Requests != 5 {
		t.Fatalf("/fleet/usage: %+v", out.Rows)
	}
	split := s.Usage("role", "model", time.Hour)
	if len(split) < 3 || split[0].Sub == "" {
		t.Fatalf("role → model split: %+v", split)
	}
	path := filepath.Join(t.TempDir(), "stats.json")
	if err := s.live.save(path); err != nil {
		t.Fatal(err)
	}
	var again liveStore
	if err := again.load(path); err != nil {
		t.Fatal(err)
	}
	if got := again.usageReport("client", "", time.Hour); len(got) != 3 || got[0].Requests != 3 {
		t.Fatalf("after reload: %+v", got)
	}
}

func TestModelsListHasNodeModelIDs(t *testing.T) {
	_, front, done := usageServer(t)
	defer done()
	resp, err := http.Get(front.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Data []struct {
			ID   string
			Tags []string
		}
	}
	json.NewDecoder(resp.Body).Decode(&out)
	ids := map[string]bool{}
	for _, d := range out.Data {
		ids[d.ID] = true
	}
	if !ids["scout"] || !ids["dino/gemma"] || !ids["victor/bonsai"] {
		t.Fatalf("ids = %v", ids)
	}
}

func TestSavingDropsExpiredUsageFromMemory(t *testing.T) {
	var l liveStore
	old := time.Now().Add(-usageKeep - 48*time.Hour)
	l.addUsage(LogEntry{Time: old, Client: "c", Status: 200}, "n/m")
	l.addUsage(LogEntry{Time: time.Now(), Client: "c", Status: 200}, "n/m")
	if len(l.usage) != 2 {
		t.Fatalf("setup: %d buckets", len(l.usage))
	}
	if recs := l.usageRecs(); len(recs) != 1 || len(l.usage) != 1 {
		t.Fatalf("expired bucket must leave memory when saving: %d saved, %d kept", len(recs), len(l.usage))
	}
}

func TestModelsListUpFilter(t *testing.T) {
	s, front, done := usageServer(t)
	defer done()
	s.penalizeFor("b", time.Minute, "test") // victor/bonsai goes down
	get := func(q string) map[string]*bool {
		resp, err := http.Get(front.URL + "/v1/models" + q)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out struct {
			Data []struct {
				ID string
				Up *bool
			}
		}
		json.NewDecoder(resp.Body).Decode(&out)
		m := map[string]*bool{}
		for _, d := range out.Data {
			m[d.ID] = d.Up
		}
		return m
	}
	all := get("")
	if up := all["victor/bonsai"]; up == nil || *up {
		t.Fatalf("down model must be listed and marked up=false: %v", all)
	}
	only := get("?up=1")
	if _, ok := only["victor/bonsai"]; ok {
		t.Fatalf("?up=1 must drop the down model: %v", only)
	}
	if _, ok := only["dino/gemma"]; !ok {
		t.Fatalf("?up=1 must keep the live model: %v", only)
	}
}

func TestHubTrafficAndUsageWithTunnelClient(t *testing.T) {
	s, front, done := usageServer(t)
	defer done()

	now := time.Now()
	// Simulate local direct traffic (client: curl)
	s.live.addUsage(LogEntry{Time: now, Model: "a", Endpoint: "a", Client: "curl", Status: 200, In: 10, Out: 20}, "dino/gemma")

	// Simulate forwarded tunnel traffic from hub (client: hub-tunnel)
	s.live.addUsage(LogEntry{Time: now, Model: "a", Endpoint: "a", Client: "hub-tunnel", Status: 200, In: 50, Out: 100}, "dino/gemma")

	// All local traffic includes hub-tunnel
	allLocal := s.Usage("client", "", time.Hour)
	foundTunnel := false
	for _, r := range allLocal {
		if r.Key == "hub-tunnel" {
			foundTunnel = true
		}
	}
	if !foundTunnel {
		t.Fatalf("expected hub-tunnel in all local usage: %+v", allLocal)
	}

	// Filtered local traffic excludes hub-tunnel
	filtered := s.UsageFiltered("client", "", time.Hour, "hub-tunnel")
	for _, r := range filtered {
		if r.Key == "hub-tunnel" {
			t.Fatalf("hub-tunnel should be excluded from filtered usage: %+v", filtered)
		}
	}
	if len(filtered) != 1 || filtered[0].Key != "curl" {
		t.Fatalf("expected only curl in filtered usage: %+v", filtered)
	}

	// GET /fleet/usage?exclude_client=hub-tunnel via HTTP
	resp, err := http.Get(front.URL + "/fleet/usage?by=client&since=1h&exclude_client=hub-tunnel")
	if err != nil {
		t.Fatal(err)
	}
	var out struct{ Rows []UsageRow }
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	for _, r := range out.Rows {
		if r.Key == "hub-tunnel" {
			t.Fatalf("GET /fleet/usage included hub-tunnel despite exclude_client: %+v", out.Rows)
		}
	}
}

// /fleet/usage stays admin-only: no key unlocks it for a remote caller. The node-readable copy is /fleet/tunnel/usage,
// guarded by the credential a node joins the hub with (streamAuth); an API key restricted to another role must not read it
// (it names every client of the hub and what each one used).
func TestHubUsageAuthWithHubKey(t *testing.T) {
	isolate(t)
	t.Setenv("TEST_USAGE_HUBKEY", "hub-secret-key-12345")
	s := &Server{
		cfg: &config.Config{
			AdminToken: "admin-secret-tok",
			APIKeyEnv:  "TEST_USAGE_HUBKEY",
		},
		state: map[string]*EpState{}, lims: map[string]*limiter{}, client: http.DefaultClient,
	}
	front := httptest.NewServer(s.Handler())
	defer front.Close()
	clientKey, err := GetKeyStore().Create("some-app", "", "coder")
	if err != nil {
		t.Fatal(err)
	}
	get := func(path, token string) int {
		req, _ := http.NewRequest("GET", front.URL+path, nil)
		req.Header.Set("X-Forwarded-For", "10.0.1.100") // not a direct loopback caller
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, c := range []struct {
		name, path, token string
		want              int
	}{
		{"admin route without a token", "/fleet/usage?by=client", "", http.StatusForbidden},
		{"admin route: the hub key does not unlock it", "/fleet/usage?by=client", "hub-secret-key-12345", http.StatusForbidden},
		{"hub route with the hub key", "/fleet/tunnel/usage?by=client", "hub-secret-key-12345", http.StatusOK},
		{"hub route without a token", "/fleet/tunnel/usage?by=client", "", http.StatusUnauthorized},
		{"hub route: an API key of another role", "/fleet/tunnel/usage?by=client", clientKey.Key, http.StatusUnauthorized},
	} {
		if got := get(c.path, c.token); got != c.want {
			t.Errorf("%s: status %d, want %d", c.name, got, c.want)
		}
	}
}

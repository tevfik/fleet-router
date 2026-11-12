package server

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/tevfik/fleet-router/internal/config"
)

func TestCovAdminAuthAndLoopback(t *testing.T) {
	flEnv(t)
	cfg := &config.Config{
		Name:       "node1",
		Listen:     "127.0.0.1:4711",
		AdminToken: "admin-s3cret",
		HubToken:   "hub-token-raw",
		HubAPIKey:  "hub-apikey-raw",
		Endpoints: []config.Endpoint{
			{Name: "ep1", Node: "node1", URL: "http://127.0.0.1:8000/v1", Model: "m1"},
		},
	}
	s, _ := flFleet(t, cfg, nil)

	// 1. Remote call without admin token -> 403
	rec := rtDo(t, s, "GET", "/fleet/status", nil, rtRemote)
	if rec.Code != 403 {
		t.Fatalf("remote call without token got status %d, want 403", rec.Code)
	}

	// 2. Remote call with wrong token -> 403
	rec = rtDo(t, s, "GET", "/fleet/status", nil, rtRemote, rtBearer("wrong-token"))
	if rec.Code != 403 {
		t.Fatalf("remote call with wrong token got status %d, want 403", rec.Code)
	}

	// 3. Remote call with correct admin token -> 200
	rec = rtDo(t, s, "GET", "/fleet/status", nil, rtRemote, rtBearer("admin-s3cret"))
	if rec.Code != 200 {
		t.Fatalf("remote call with correct token got status %d, want 200", rec.Code)
	}

	// 4. Forwarded headers on loopback must be treated as non-direct loopback (403 without token)
	for _, hdr := range []string{"X-Forwarded-For", "Forwarded", "X-Real-Ip"} {
		rec = rtDo(t, s, "GET", "/fleet/status", nil, func(r *http.Request) {
			r.Header.Set(hdr, "10.0.0.1")
		})
		if rec.Code != 403 {
			t.Fatalf("header %s did not trigger 403, got %d", hdr, rec.Code)
		}
	}

	// Route and Plan handlers
	rec = rtDo(t, s, "GET", "/fleet/route?model=ep1", nil)
	if rec.Code != 200 {
		t.Fatalf("GET /fleet/route got %d", rec.Code)
	}

	rec = rtDo(t, s, "POST", "/fleet/plan", map[string]any{"model": "ep1"})
	if rec.Code != 200 && rec.Code != 400 && rec.Code != 404 {
		t.Fatalf("POST /fleet/plan got %d", rec.Code)
	}

	rec = rtDo(t, s, "POST", "/fleet/node-spec", map[string]any{
		"name": "n1",
		"spec": config.NodeSpec{Addr: "10.0.0.1", MemGiB: 64},
	})
	if rec.Code != 200 {
		t.Fatalf("POST /fleet/node-spec got %d", rec.Code)
	}

	rec = rtDo(t, s, "POST", "/fleet/node-spec", map[string]any{
		"name":   "n1",
		"remove": true,
	})
	if rec.Code != 200 {
		t.Fatalf("POST /fleet/node-spec remove got %d", rec.Code)
	}

	// Bad JSON for node-spec
	rec = rtDo(t, s, "POST", "/fleet/node-spec", "{bad")
	if rec.Code != 400 {
		t.Fatalf("POST /fleet/node-spec bad json got %d, want 400", rec.Code)
	}

	// POST /fleet/discover: bad SSH target -> 400
	rec = rtDo(t, s, "POST", "/fleet/discover", map[string]any{
		"ssh": []string{"bad;injection"},
	})
	if rec.Code != 400 {
		t.Fatalf("POST /fleet/discover bad ssh got %d, want 400", rec.Code)
	}

	// POST /fleet/discover: scan single port
	rec = rtDo(t, s, "POST", "/fleet/discover", map[string]any{
		"ports": "59999",
		"ssh":   []string{},
		"apply": false,
	})
	if rec.Code != 200 {
		t.Fatalf("POST /fleet/discover got %d: %s", rec.Code, rec.Body.String())
	}

	// POST /fleet/peers pairing with missing peer URL -> 400
	rec = rtDo(t, s, "POST", "/fleet/peers", map[string]any{
		"add":  &config.Peer{Name: "p-no-url"},
		"pair": "1234",
	})
	if rec.Code != 400 {
		t.Fatalf("POST /fleet/peers pairing without url got %d, want 400", rec.Code)
	}

	// Direct loopback call -> 200 without token
	rec = rtDo(t, s, "GET", "/fleet/status", nil)
	if rec.Code != 200 {
		t.Fatalf("direct loopback got %d, want 200", rec.Code)
	}

	// 6. Config secret redaction: direct loopback sees secrets, remote sees "***"
	recLoop := rtDo(t, s, "GET", "/fleet/config", nil)
	if recLoop.Code != 200 {
		t.Fatalf("GET /fleet/config loopback got %d", recLoop.Code)
	}
	var loopCfg config.Config
	json.NewDecoder(recLoop.Body).Decode(&loopCfg)
	if loopCfg.HubToken != "hub-token-raw" || loopCfg.HubAPIKey != "hub-apikey-raw" {
		t.Fatalf("loopback should see unredacted secrets, got token=%q key=%q", loopCfg.HubToken, loopCfg.HubAPIKey)
	}

	recRem := rtDo(t, s, "GET", "/fleet/config", nil, rtRemote, rtBearer("admin-s3cret"))
	if recRem.Code != 200 {
		t.Fatalf("GET /fleet/config remote got %d", recRem.Code)
	}
	var remCfg config.Config
	json.NewDecoder(recRem.Body).Decode(&remCfg)
	if remCfg.HubToken != "***" || remCfg.HubAPIKey != "***" {
		t.Fatalf("remote should see redacted secrets, got token=%q key=%q", remCfg.HubToken, remCfg.HubAPIKey)
	}

	// 7. redactSecret direct checks
	if redactSecret("") != "" {
		t.Fatal("redactSecret(\"\") should be empty")
	}
	if redactSecret("anything") != "***" {
		t.Fatal("redactSecret should mask")
	}
}

func TestCovOpenCodeProvider(t *testing.T) {
	flEnv(t)
	cfg := &config.Config{
		Name:   "node1",
		Listen: "127.0.0.1:4711",
		Roles: map[string]config.Role{
			"coder": {Preferred: "ep1"},
		},
		Endpoints: []config.Endpoint{
			{Name: "ep1", Node: "node1", URL: "http://127.0.0.1:8000/v1", Model: "qwen:32b", Ctx: 32768},
			{Name: "ep2", Node: "node1", URL: "http://127.0.0.1:8000/v1", Model: "bge-m3", Kind: "embed"},
		},
	}
	s, _ := flFleet(t, cfg, nil)
	rec := rtDo(t, s, "GET", "/fleet/opencode", nil)
	if rec.Code != 200 {
		t.Fatalf("GET /fleet/opencode got %d", rec.Code)
	}
	var prov map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&prov); err != nil {
		t.Fatal(err)
	}
	fleetMap, ok := prov["fleet"].(map[string]any)
	if !ok || fleetMap["name"] != "Fleet (router)" {
		t.Fatalf("unexpected opencode provider: %+v", prov)
	}
}

func TestCovRoleManagement(t *testing.T) {
	flEnv(t)
	cfg := &config.Config{
		Name:   "node1",
		Listen: "127.0.0.1:4711",
		Roles: map[string]config.Role{
			"editor": {Preferred: "ep1"},
		},
		Endpoints: []config.Endpoint{
			{Name: "ep1", Node: "node1", URL: "http://127.0.0.1:8000/v1", Model: "qwen:32b"},
		},
	}
	s, _ := flFleet(t, cfg, nil)

	// Bad JSON
	rec := rtDo(t, s, "POST", "/fleet/role", "{bad-json")
	if rec.Code != 400 {
		t.Fatalf("expected 400 for bad json, got %d", rec.Code)
	}

	// Empty role
	rec = rtDo(t, s, "POST", "/fleet/role", map[string]any{"role": ""})
	if rec.Code != 400 {
		t.Fatalf("expected 400 for empty role, got %d", rec.Code)
	}

	// Add/update role with all options
	prio := 15
	maxPar := 4
	overflow := true
	strat := "round-robin"
	use := "reasoning"
	rec = rtDo(t, s, "POST", "/fleet/role", map[string]any{
		"role":         "coder",
		"preferred":    "ep1",
		"fallback":     []string{"ep1"},
		"match":        []string{"model:qwen*"},
		"priority":     &prio,
		"max_parallel": &maxPar,
		"overflow":     &overflow,
		"strategy":     &strat,
		"use":          &use,
	})
	if rec.Code != 200 {
		t.Fatalf("POST /fleet/role got %d: %s", rec.Code, rec.Body.String())
	}
	r := s.Config().Roles["coder"]
	if r.Preferred != "ep1" || r.Priority != 15 || r.MaxParallel != 4 || !r.Overflow || r.Strategy != "round-robin" {
		t.Fatalf("role not updated properly: %+v", r)
	}

	// Delete role
	rec = rtDo(t, s, "POST", "/fleet/role", map[string]any{
		"role":   "editor",
		"delete": true,
	})
	if rec.Code != 200 {
		t.Fatalf("DELETE role got %d", rec.Code)
	}
	if _, ok := s.Config().Roles["editor"]; ok {
		t.Fatal("role editor should have been deleted")
	}
}

func TestCovEndpointSetAndEndpoints(t *testing.T) {
	flEnv(t)
	cfg := &config.Config{
		Name:   "node1",
		Listen: "127.0.0.1:4711",
		Roles: map[string]config.Role{
			"coder": {Preferred: "ep1", Fallback: []string{"ep2"}},
		},
		Endpoints: []config.Endpoint{
			{Name: "ep1", Node: "node1", URL: "http://127.0.0.1:8000/v1", Model: "qwen:32b"},
			{Name: "ep2", Node: "node1", URL: "http://127.0.0.1:8000/v1", Model: "llama:70b"},
		},
	}
	s, _ := flFleet(t, cfg, nil)

	// 1. POST /fleet/endpoint-set: bad JSON & missing name
	rec := rtDo(t, s, "POST", "/fleet/endpoint-set", "{bad")
	if rec.Code != 400 {
		t.Fatalf("expected 400 on bad json, got %d", rec.Code)
	}
	rec = rtDo(t, s, "POST", "/fleet/endpoint-set", map[string]any{"name": ""})
	if rec.Code != 400 {
		t.Fatalf("expected 400 on empty name, got %d", rec.Code)
	}

	// Non-existent endpoint -> 404
	rec = rtDo(t, s, "POST", "/fleet/endpoint-set", map[string]any{"name": "ep-missing"})
	if rec.Code != 404 {
		t.Fatalf("expected 404 on nonexistent endpoint, got %d", rec.Code)
	}

	// Modify ep1 properties
	slots := 8
	fport := 5001
	stall := "45s"
	rec = rtDo(t, s, "POST", "/fleet/endpoint-set", map[string]any{
		"name":       "ep1",
		"slots":      &slots,
		"fleet_port": &fport,
		"stall":      &stall,
		"caps":       []string{"chat", "vision"},
		"tags":       []string{"fast", "gpu"},
		"group":      &config.Group{Engine: "llama-ring", Workers: []string{"node2"}},
	})
	if rec.Code != 200 {
		t.Fatalf("POST /fleet/endpoint-set got %d: %s", rec.Code, rec.Body.String())
	}
	ep := s.Config().Endpoint("ep1")
	if ep.Slots != 8 || ep.FleetPort != 5001 || ep.Stall != "45s" || len(ep.Caps) != 2 || ep.Group == nil {
		t.Fatalf("ep1 not updated properly: %+v", ep)
	}

	// Clear group by setting empty engine
	rec = rtDo(t, s, "POST", "/fleet/endpoint-set", map[string]any{
		"name":  "ep1",
		"group": &config.Group{Engine: ""},
	})
	if rec.Code != 200 {
		t.Fatalf("clear group got %d", rec.Code)
	}
	if s.Config().Endpoint("ep1").Group != nil {
		t.Fatal("ep1 group should be nil after clearing")
	}

	// 2. POST /fleet/endpoints:
	// Remove ep2 without mend_roles -> should fail because role 'coder' uses it
	rec = rtDo(t, s, "POST", "/fleet/endpoints", map[string]any{
		"remove": []string{"ep2"},
	})
	if rec.Code != 400 {
		t.Fatalf("expected 400 when removing role-dependent endpoint without mend_roles, got %d", rec.Code)
	}

	// Remove ep2 with mend_roles=true -> succeeds and mends role
	rec = rtDo(t, s, "POST", "/fleet/endpoints", map[string]any{
		"remove":     []string{"ep2"},
		"mend_roles": true,
	})
	if rec.Code != 200 {
		t.Fatalf("remove with mend_roles got %d: %s", rec.Code, rec.Body.String())
	}

	// Add new endpoint
	rec = rtDo(t, s, "POST", "/fleet/endpoints", map[string]any{
		"add": []config.Endpoint{
			{Name: "ep3", Node: "node1", URL: "http://127.0.0.1:8000/v1", Model: "deepseek:67b"},
		},
	})
	if rec.Code != 200 {
		t.Fatalf("add endpoint got %d: %s", rec.Code, rec.Body.String())
	}
	if s.Config().Endpoint("ep3") == nil {
		t.Fatal("ep3 not added")
	}
}

func TestCovSettingsEconomyAndBench(t *testing.T) {
	flEnv(t)
	cfg := &config.Config{
		Name:   "node1",
		Listen: "127.0.0.1:4711",
		Endpoints: []config.Endpoint{
			{Name: "ep-embed", Node: "node1", URL: "http://127.0.0.1:8000/v1", Model: "embed", Kind: "embed"},
		},
	}
	s, _ := flFleet(t, cfg, nil)

	// POST /fleet/settings
	rec := rtDo(t, s, "POST", "/fleet/settings", map[string]any{
		"ports":  "8000-8005",
		"ssh":    []string{"user@host"},
		"ignore": []string{"host:8000"},
	})
	if rec.Code != 200 {
		t.Fatalf("POST /fleet/settings got %d", rec.Code)
	}
	if s.Config().Discover.Ports != "8000-8005" {
		t.Fatalf("settings not applied: %+v", s.Config().Discover)
	}

	// POST /fleet/economy/settings
	rec = rtDo(t, s, "POST", "/fleet/economy/settings", map[string]any{
		"enabled": true,
	})
	if rec.Code != 200 {
		t.Fatalf("POST /fleet/economy/settings got %d", rec.Code)
	}

	// POST /fleet/bench: nonexistent endpoint -> 409
	rec = rtDo(t, s, "POST", "/fleet/bench", map[string]any{
		"names": []string{"nonexistent"},
	})
	if rec.Code != 409 {
		t.Fatalf("expected 409 on benching nonexistent endpoint, got %d", rec.Code)
	}

	// POST /fleet/bench: embed endpoint -> 409
	rec = rtDo(t, s, "POST", "/fleet/bench", map[string]any{
		"names": []string{"ep-embed"},
	})
	if rec.Code != 409 {
		t.Fatalf("expected 409 on benching embed endpoint, got %d", rec.Code)
	}

	// POST /fleet/host/bench -> when already running, returns 409
	s.hwb.mu.Lock()
	s.hwb.running = true
	s.hwb.mu.Unlock()
	rec = rtDo(t, s, "POST", "/fleet/host/bench", nil)
	if rec.Code != 409 {
		t.Fatalf("POST /fleet/host/bench got %d, want 409", rec.Code)
	}
	s.hwb.mu.Lock()
	s.hwb.running = false
	s.hwb.mu.Unlock()

	// POST /fleet/probe with apply=true
	rec = rtDo(t, s, "POST", "/fleet/probe", map[string]any{
		"names": []string{"ep-embed"},
		"apply": true,
	})
	if rec.Code != 200 && rec.Code != 400 {
		t.Fatalf("POST /fleet/probe got %d", rec.Code)
	}
}

func TestCovTunnelJoinLeaveAndHubSettings(t *testing.T) {
	flEnv(t)
	cfg := &config.Config{
		Name:   "node1",
		Listen: "127.0.0.1:4711",
		Endpoints: []config.Endpoint{
			{Name: "ep1", Node: "node1", URL: "http://127.0.0.1:8000/v1", Model: "m1"},
		},
	}
	s, _ := flFleet(t, cfg, nil)

	// 1. POST /fleet/tunnel/join: missing hub_url -> 400
	rec := rtDo(t, s, "POST", "/fleet/tunnel/join", map[string]any{})
	if rec.Code != 400 {
		t.Fatalf("expected 400 on missing hub_url, got %d", rec.Code)
	}

	// 2. Stream port out of range -> 400
	rec = rtDo(t, s, "POST", "/fleet/tunnel/join", map[string]any{
		"hub_url":      "https://hub.example.com",
		"stream_ports": []int{70000},
	})
	if rec.Code != 400 {
		t.Fatalf("expected 400 on invalid port, got %d", rec.Code)
	}

	// 3. Stream port equals listen port (4711) -> 400
	rec = rtDo(t, s, "POST", "/fleet/tunnel/join", map[string]any{
		"hub_url":      "https://hub.example.com",
		"stream_ports": []int{4711},
	})
	if rec.Code != 400 {
		t.Fatalf("expected 400 when stream port == listen port, got %d", rec.Code)
	}

	// 4. POST /fleet/tunnel/leave & GET status
	rec = rtDo(t, s, "POST", "/fleet/tunnel/leave", nil)
	if rec.Code != 200 {
		t.Fatalf("POST /fleet/tunnel/leave got %d", rec.Code)
	}
	rec = rtDo(t, s, "GET", "/fleet/tunnel/status", nil)
	if rec.Code != 200 {
		t.Fatalf("GET /fleet/tunnel/status got %d", rec.Code)
	}

	// 5. POST /fleet/hub/settings: update hub_api_key only
	apikey := "live_key_123"
	rec = rtDo(t, s, "POST", "/fleet/hub/settings", map[string]any{
		"hub_api_key": &apikey,
	})
	if rec.Code != 200 {
		t.Fatalf("update hub_api_key got %d", rec.Code)
	}
	if s.Config().HubAPIKey != "live_key_123" {
		t.Fatalf("hub_api_key not saved, got %q", s.Config().HubAPIKey)
	}

	// Update hub_url and hub_token
	rec = rtDo(t, s, "POST", "/fleet/hub/settings", map[string]any{
		"hub_url":   "https://hub2.example.com",
		"hub_token": "token_abc",
	})
	if rec.Code != 200 {
		t.Fatalf("update hub_url got %d", rec.Code)
	}
	if s.Config().HubURL != "https://hub2.example.com" || s.Config().HubToken != "token_abc" {
		t.Fatalf("hub settings not saved properly: %+v", s.Config())
	}
}

func TestCovHistoryUndoApikeysAndPeers(t *testing.T) {
	flEnv(t)
	cfg := &config.Config{
		Name:   "node1",
		Listen: "127.0.0.1:4711",
		Endpoints: []config.Endpoint{
			{Name: "ep1", Node: "node1", URL: "http://127.0.0.1:8000/v1", Model: "m1"},
		},
	}
	s, _ := flFleet(t, cfg, nil)

	// History
	rec := rtDo(t, s, "GET", "/fleet/history", nil)
	if rec.Code != 200 {
		t.Fatalf("GET /fleet/history got %d", rec.Code)
	}

	// Undo
	rec = rtDo(t, s, "POST", "/fleet/undo", nil)
	if rec.Code != 200 && rec.Code != 400 {
		t.Fatalf("POST /fleet/undo got unexpected %d", rec.Code)
	}

	// Apikeys API
	ksPath := filepath.Join(t.TempDir(), "apikeys.json")
	SetKeyStore(newKeyStore(ksPath))

	// GET keys
	rec = rtDo(t, s, "GET", "/fleet/apikeys", nil)
	if rec.Code != 200 {
		t.Fatalf("GET /fleet/apikeys got %d", rec.Code)
	}

	// POST create key
	rec = rtDo(t, s, "POST", "/fleet/apikeys", map[string]any{
		"name":   "dev-key",
		"qos":    "ultra-low-latency",
		"role":   "coder",
		"models": []string{"qwen3:8b"},
	})
	if rec.Code != 200 {
		t.Fatalf("POST /fleet/apikeys got %d", rec.Code)
	}
	var created APIKey
	json.NewDecoder(rec.Body).Decode(&created)
	if created.Key == "" || created.Name != "dev-key" {
		t.Fatalf("key creation failed: %+v", created)
	}

	// DELETE key
	rec = rtDo(t, s, "DELETE", "/fleet/apikeys/"+created.Key, nil)
	if rec.Code != 200 {
		t.Fatalf("DELETE key got %d", rec.Code)
	}

	// Peers API
	rec = rtDo(t, s, "GET", "/fleet/peers", nil)
	if rec.Code != 200 {
		t.Fatalf("GET /fleet/peers got %d", rec.Code)
	}
	rec = rtDo(t, s, "POST", "/fleet/peers", map[string]any{
		"sync": true,
	})
	if rec.Code != 200 {
		t.Fatalf("POST /fleet/peers sync got %d", rec.Code)
	}
	rec = rtDo(t, s, "POST", "/fleet/peers", map[string]any{
		"add": &config.Peer{Name: "p1", URL: "http://10.0.0.2:4711/v1"},
	})
	if rec.Code != 200 {
		t.Fatalf("POST /fleet/peers add got %d", rec.Code)
	}
	rec = rtDo(t, s, "POST", "/fleet/peers", map[string]any{
		"remove": "p1",
	})
	if rec.Code != 200 {
		t.Fatalf("POST /fleet/peers remove got %d", rec.Code)
	}

	// GET log, usage, pair/code
	rec = rtDo(t, s, "GET", "/fleet/log?n=10", nil)
	if rec.Code != 200 {
		t.Fatalf("GET /fleet/log got %d", rec.Code)
	}
	rec = rtDo(t, s, "GET", "/fleet/usage", nil)
	if rec.Code != 200 {
		t.Fatalf("GET /fleet/usage got %d", rec.Code)
	}
	rec = rtDo(t, s, "POST", "/fleet/pair/code", nil)
	if rec.Code != 200 {
		t.Fatalf("POST /fleet/pair/code got %d", rec.Code)
	}
}

func TestCovHubUsageAuth(t *testing.T) {
	flEnv(t)
	cfg := &config.Config{
		Name:       "node1",
		Listen:     "127.0.0.1:4711",
		APIKeyEnv:  "TEST_HUB_KEY",
		AdminToken: "admin-token",
		Endpoints: []config.Endpoint{
			{Name: "ep1", Node: "node1", URL: "http://127.0.0.1:8000/v1", Model: "m1"},
		},
	}
	t.Setenv("TEST_HUB_KEY", "correct-hub-secret")
	s, _ := flFleet(t, cfg, nil)

	// No auth -> 401
	rec := rtDo(t, s, "GET", "/fleet/tunnel/usage", nil, rtRemote)
	if rec.Code != 401 {
		t.Fatalf("hub usage without key got %d, want 401", rec.Code)
	}

	// Wrong key -> 401
	rec = rtDo(t, s, "GET", "/fleet/tunnel/usage", nil, rtRemote, rtBearer("wrong-key"))
	if rec.Code != 401 {
		t.Fatalf("hub usage with wrong key got %d, want 401", rec.Code)
	}

	// Correct key via APIKeyEnv -> 200
	rec = rtDo(t, s, "GET", "/fleet/tunnel/usage", nil, rtRemote, rtBearer("correct-hub-secret"))
	if rec.Code != 200 {
		t.Fatalf("hub usage with correct key got %d, want 200", rec.Code)
	}

	// Paired key -> 200
	tok := "paired-token-xyz"
	c := s.Config()
	c.Paired = append(c.Paired, config.Paired{Name: "peer1", KeyHash: keyHash(tok)})
	_ = s.SetConfig(c)

	rec = rtDo(t, s, "GET", "/fleet/tunnel/usage", nil, rtRemote, rtBearer(tok))
	if rec.Code != 200 {
		t.Fatalf("hub usage with paired key got %d, want 200", rec.Code)
	}

	// Key with "stream" role in KeyStore -> 200
	ksPath := filepath.Join(t.TempDir(), "apikeys.json")
	ks := newKeyStore(ksPath)
	SetKeyStore(ks)
	ak, _ := ks.Create("streamer", "default", "stream")
	rec = rtDo(t, s, "GET", "/fleet/tunnel/usage", nil, rtRemote, rtBearer(ak.Key))
	if rec.Code != 200 {
		t.Fatalf("hub usage with stream role key got %d, want 200", rec.Code)
	}
}

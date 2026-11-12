package server

import (
	"bufio"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tevfik/fleet-router/internal/config"
)

func TestCovProxySSE(t *testing.T) {
	flEnv(t)
	up := rtNewUp(t, "up-sse")
	cfg := &config.Config{
		Name:   "node1",
		Listen: "127.0.0.1:4711",
		Endpoints: []config.Endpoint{
			{Name: "ep-sse", Node: "node1", URL: up.URL(), Model: "test-model"},
		},
	}
	s, _ := flFleet(t, cfg, nil)

	// Stream chat request
	rec := rtDo(t, s, "POST", "/v1/chat/completions", rtChat("test-model", true))
	if rec.Code != 200 {
		t.Fatalf("expected 200 for SSE stream, got %d: %s", rec.Code, rec.Body.String())
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("expected text/event-stream, got %q", ct)
	}
	if rec.Header().Get("X-Fleet-Endpoint") != "ep-sse" {
		t.Fatalf("expected X-Fleet-Endpoint: ep-sse, got %q", rec.Header().Get("X-Fleet-Endpoint"))
	}

	sc := bufio.NewScanner(rec.Body)
	chunks := 0
	foundDone := false
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") {
			chunks++
			if strings.Contains(line, "[DONE]") {
				foundDone = true
			}
		}
	}
	if chunks < 2 || !foundDone {
		t.Fatalf("SSE stream incomplete: chunks=%d, foundDone=%v", chunks, foundDone)
	}
}

func TestCovProxyFailover(t *testing.T) {
	flEnv(t)
	up1 := rtNewUp(t, "up1")
	up2 := rtNewUp(t, "up2")
	cfg := &config.Config{
		Name:   "node1",
		Listen: "127.0.0.1:4711",
		Roles: map[string]config.Role{
			"coder": {Preferred: "ep1", Fallback: []string{"ep2"}},
		},
		Endpoints: []config.Endpoint{
			{Name: "ep1", Node: "node1", URL: up1.URL(), Model: "model-a"},
			{Name: "ep2", Node: "node1", URL: up2.URL(), Model: "model-b"},
		},
	}
	s, _ := flFleet(t, cfg, nil)

	// 1. up1 returns 500 -> failover to up2 returns 200
	up1.set(func(u *rtUp) { u.status = 500 })
	rec := rtDo(t, s, "POST", "/v1/chat/completions", rtChat("coder", false))
	if rec.Code != 200 {
		t.Fatalf("expected 200 on failover, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Fleet-Endpoint") != "ep2" {
		t.Fatalf("failover should have hit ep2, got %q", rec.Header().Get("X-Fleet-Endpoint"))
	}

	// 2. up1 returns 429 -> failover to up2
	up1.set(func(u *rtUp) { u.status = 429 })
	rec = rtDo(t, s, "POST", "/v1/chat/completions", rtChat("coder", false))
	if rec.Code != 200 {
		t.Fatalf("expected 200 on 429 failover, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Fleet-Endpoint") != "ep2" {
		t.Fatalf("429 failover should have hit ep2, got %q", rec.Header().Get("X-Fleet-Endpoint"))
	}

	// 3. Both up1 and up2 fail -> 502 Bad Gateway
	up2.set(func(u *rtUp) { u.status = 503 })
	rec = rtDo(t, s, "POST", "/v1/chat/completions", rtChat("coder", false))
	if rec.Code != 502 {
		t.Fatalf("expected 502 when all candidates fail, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCovProxyAPIKeyAuthAndRejection(t *testing.T) {
	flEnv(t)
	ksPath := filepath.Join(t.TempDir(), "apikeys.json")
	ks := newKeyStore(ksPath)
	SetKeyStore(ks)

	// Key restricted to model "qwen*" / "coder" and role "coder"
	ak, err := ks.Create("tester", "ultra-low-latency", "coder", "qwen*", "coder")
	if err != nil {
		t.Fatal(err)
	}

	// Key allowing model "coder" but with role "reviewer"
	akRole, err := ks.Create("tester-role", "default", "reviewer", "coder")
	if err != nil {
		t.Fatal(err)
	}

	up := rtNewUp(t, "up-key")
	cfg := &config.Config{
		Name:   "node1",
		Listen: "127.0.0.1:4711",
		Roles: map[string]config.Role{
			"coder":  {Preferred: "ep-qwen"},
			"writer": {Preferred: "ep-llama"},
		},
		Endpoints: []config.Endpoint{
			{Name: "ep-qwen", Node: "node1", URL: up.URL(), Model: "qwen-coder"},
			{Name: "ep-llama", Node: "node1", URL: up.URL(), Model: "llama-70b"},
		},
	}
	s, _ := flFleet(t, cfg, nil)

	// 1. Invalid flt_live_ token -> 401
	rec := rtDo(t, s, "POST", "/v1/chat/completions", rtChat("qwen-coder", false), rtBearer("flt_live_nonexistent12345"))
	if rec.Code != 401 {
		t.Fatalf("expected 401 for invalid flt_live_ key, got %d: %s", rec.Code, rec.Body.String())
	}

	// 2. Key does not allow model "llama-70b" -> 403
	rec = rtDo(t, s, "POST", "/v1/chat/completions", rtChat("llama-70b", false), rtBearer(ak.Key))
	if rec.Code != 403 {
		t.Fatalf("expected 403 for unauthorized model, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "not authorized to access model") {
		t.Fatalf("unexpected error message: %s", rec.Body.String())
	}

	// 3. Key does not allow role "coder" -> 403
	rec = rtDo(t, s, "POST", "/v1/chat/completions", rtChat("coder", false), rtBearer(akRole.Key))
	if rec.Code != 403 {
		t.Fatalf("expected 403 for unauthorized role, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "not authorized for role") {
		t.Fatalf("unexpected error message: %s", rec.Body.String())
	}

	// 4. Valid access to "qwen-coder" with role "coder" -> 200
	rec = rtDo(t, s, "POST", "/v1/chat/completions", rtChat("coder", false), rtBearer(ak.Key))
	if rec.Code != 200 {
		t.Fatalf("expected 200 for authorized request, got %d: %s", rec.Code, rec.Body.String())
	}

	// 5. Verify usage was recorded for key
	updatedKey, ok := ks.Lookup(ak.Key)
	if !ok || updatedKey.TotalRequests < 1 {
		t.Fatalf("usage not recorded: %+v", updatedKey)
	}
}

func TestCovV1AuthRemoteRejection(t *testing.T) {
	flEnv(t)
	cfg := &config.Config{
		Name:      "node1",
		Listen:    "127.0.0.1:4711",
		APIKeyEnv: "FLEET_TEST_V1_KEY",
		Endpoints: []config.Endpoint{
			{Name: "ep1", Node: "node1", URL: "http://127.0.0.1:8000/v1", Model: "m1"},
		},
	}
	t.Setenv("FLEET_TEST_V1_KEY", "expected-secret-key")
	s, _ := flFleet(t, cfg, nil)

	// 1. Remote caller with no auth -> 401
	rec := rtDo(t, s, "GET", "/v1/models", nil, rtRemote)
	if rec.Code != 401 {
		t.Fatalf("remote call without key got %d, want 401", rec.Code)
	}

	// 2. Remote caller with wrong auth -> 401
	rec = rtDo(t, s, "GET", "/v1/models", nil, rtRemote, rtBearer("wrong-key"))
	if rec.Code != 401 {
		t.Fatalf("remote call with wrong key got %d, want 401", rec.Code)
	}

	// 3. Remote caller with correct APIKeyEnv bearer -> 200
	rec = rtDo(t, s, "GET", "/v1/models", nil, rtRemote, rtBearer("expected-secret-key"))
	if rec.Code != 200 {
		t.Fatalf("remote call with correct key got %d, want 200", rec.Code)
	}

	// 4. Remote caller with keystore key -> 200
	ksPath := filepath.Join(t.TempDir(), "apikeys.json")
	ks := newKeyStore(ksPath)
	SetKeyStore(ks)
	validKey, _ := ks.Create("remote-user", "default", "")

	rec = rtDo(t, s, "GET", "/v1/models", nil, rtRemote, rtBearer(validKey.Key))
	if rec.Code != 200 {
		t.Fatalf("remote call with keystore key got %d, want 200", rec.Code)
	}

	// 5. Direct loopback caller without key -> 200
	rec = rtDo(t, s, "GET", "/v1/models", nil)
	if rec.Code != 200 {
		t.Fatalf("direct loopback got %d, want 200", rec.Code)
	}
}

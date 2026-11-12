package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
)

func TestAPIKeyStore(t *testing.T) {
	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "apikeys.json")

	ks := newKeyStore(storePath)

	// Create
	ak, err := ks.Create("test-client", "ultra-low-latency", "coder")
	if err != nil {
		t.Fatalf("failed to create key: %v", err)
	}
	if ak.Name != "test-client" || ak.QoS != "ultra-low-latency" || ak.Role != "coder" {
		t.Errorf("unexpected key attributes: %+v", ak)
	}

	// Validate
	val, ok := ks.Validate(ak.Key)
	if !ok || val.Key != ak.Key {
		t.Fatalf("failed to validate key %s", ak.Key)
	}
	if val.TotalRequests != 1 {
		t.Errorf("expected 1 request, got %d", val.TotalRequests)
	}

	// Record Usage
	ks.RecordUsage(ak.Key, 150, 45)
	val, _ = ks.Lookup(ak.Key) // Lookup/Validate return copies: read the stored state again
	if val.TotalTokens != 150 || val.TotalLatencyMS != 45 {
		t.Errorf("expected 150 tokens and 45ms, got tokens=%d lat=%d", val.TotalTokens, val.TotalLatencyMS)
	}

	// Reload from disk
	ksReloaded := newKeyStore(storePath)
	list := ksReloaded.List()
	if len(list) != 1 || list[0].Key != ak.Key {
		t.Fatalf("reloaded store missing key: %+v", list)
	}

	// Delete
	if err := ksReloaded.Delete(ak.Key); err != nil {
		t.Fatalf("failed to delete key: %v", err)
	}
	if _, ok := ksReloaded.Validate(ak.Key); ok {
		t.Errorf("key still valid after delete")
	}
}

func TestExtractBearer(t *testing.T) {
	req := httptest.NewRequest("GET", "/v1/models", nil)
	if ExtractBearer(req) != "" {
		t.Errorf("expected empty token")
	}

	req.Header.Set("Authorization", "Bearer flt_live_1234567890abcdef")
	if tok := ExtractBearer(req); tok != "flt_live_1234567890abcdef" {
		t.Errorf("unexpected token: %s", tok)
	}
}

func TestAPIKey_ModelAndRoleAuthorization(t *testing.T) {
	// Unrestricted key
	allKey := &APIKey{
		Key:  "flt_all",
		Name: "unrestricted",
	}
	if !allKey.AllowsModel("qwen3.5-122b") || !allKey.AllowsModel("deepseek-v4.1") {
		t.Errorf("unrestricted key should allow any model")
	}
	if !allKey.AllowsRole("coder") || !allKey.AllowsRole("chat") {
		t.Errorf("unrestricted key should allow any role")
	}

	// Restricted models with glob
	qwenOnly := &APIKey{
		Key:           "flt_qwen",
		Name:          "qwen-client",
		AllowedModels: []string{"qwen*", "mistral-7b"},
		Role:          "coder",
	}
	if !qwenOnly.AllowsModel("qwen3.5-122b") {
		t.Errorf("expected qwen3.5-122b to be allowed by pattern qwen*")
	}
	if !qwenOnly.AllowsModel("mistral-7b") {
		t.Errorf("expected mistral-7b to be allowed")
	}
	if qwenOnly.AllowsModel("deepseek-v4.1") {
		t.Errorf("deepseek-v4.1 should NOT be allowed for qwenOnly key")
	}
	if !qwenOnly.AllowsRole("coder") {
		t.Errorf("expected coder role to be allowed")
	}
	if qwenOnly.AllowsRole("finance") {
		t.Errorf("expected finance role to be denied")
	}

	// Admin key bypasses all role checks
	adminKey := &APIKey{
		Key:  "flt_admin",
		Name: "admin-root",
		Role: "admin",
	}
	if !adminKey.AllowsRole("anything") {
		t.Errorf("admin role should allow any role request")
	}
}

func TestProxyV1_RecordsAPIKeyUsage(t *testing.T) {
	ks := newKeyStore(filepath.Join(t.TempDir(), "apikeys.json"))
	prev := GetKeyStore()
	SetKeyStore(ks)
	t.Cleanup(func() { SetKeyStore(prev) })

	ak, err := ks.Create("usage-client", "ultra-low-latency", "coder")
	if err != nil {
		t.Fatal(err)
	}

	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hello"}}],"usage":{"prompt_tokens":12,"completion_tokens":8}}`))
	}))
	defer be.Close()

	s := &Server{
		cfg: &config.Config{
			Endpoints: []config.Endpoint{{Name: "ep1", Node: "n", URL: be.URL, Model: "qwen3:8b"}},
			Roles:     map[string]config.Role{"coder": {Preferred: "ep1"}},
		},
		state:  map[string]*EpState{},
		lims:   map[string]*limiter{},
		client: http.DefaultClient,
	}
	front := httptest.NewServer(s.Handler())
	defer front.Close()

	req, err := http.NewRequest("POST", front.URL+"/v1/chat/completions", strings.NewReader(`{"model":"coder","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+ak.Key)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var got *APIKey
	for end := time.Now().Add(time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		var ok bool
		got, ok = ks.Lookup(ak.Key)
		if !ok {
			t.Fatal("key missing from store")
		}
		if got.TotalTokens > 0 {
			break
		}
	}
	if got.TotalRequests != 1 {
		t.Errorf("TotalRequests = %d, want 1", got.TotalRequests)
	}
	if got.TotalTokens != 20 {
		t.Errorf("TotalTokens = %d, want 20 (12 prompt + 8 completion)", got.TotalTokens)
	}
}

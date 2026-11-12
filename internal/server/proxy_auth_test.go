package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/tevfik/fleet-router/internal/config"
)

func TestProxyV1_APIKeyAuthorization(t *testing.T) {
	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "apikeys.json")
	ks := newKeyStore(storePath)
	SetKeyStore(ks)

	// Create a key restricted only to "qwen*"
	restrictedKey, err := ks.Create("restricted-client", "default", "coder", "qwen*")
	if err != nil {
		t.Fatalf("failed to create key: %v", err)
	}

	cfgPath := filepath.Join(tmpDir, "fleet.json")
	cfg := config.Config{
		Endpoints: []config.Endpoint{
			{Name: "ep-qwen", Model: "qwen-test", URL: "http://127.0.0.1:9999"},
			{Name: "ep-deepseek", Model: "deepseek-test", URL: "http://127.0.0.1:9998"},
		},
	}
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	srv, err := New(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Invalid API Key -> 401
	{
		body := []byte(`{"model":"qwen-test","messages":[{"role":"user","content":"hi"}]}`)
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer flt_live_invalid_key_123")
		rec := httptest.NewRecorder()
		srv.proxyV1(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("expected 401 for invalid key, got %d", rec.Code)
		}
	}

	// 2. Disallowed Model -> 403
	{
		body := []byte(`{"model":"deepseek-test","messages":[{"role":"user","content":"hi"}]}`)
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+restrictedKey.Key)
		rec := httptest.NewRecorder()
		srv.proxyV1(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("expected 403 for disallowed model, got %d: %s", rec.Code, rec.Body.String())
		}
	}

	// 3. Allowed Model -> Passes auth check (proceeds to routing / backend failover)
	{
		body := []byte(`{"model":"qwen-test","messages":[{"role":"user","content":"hi"}]}`)
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+restrictedKey.Key)
		rec := httptest.NewRecorder()
		srv.proxyV1(rec, req)
		// Should NOT be 401 or 403
		if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
			t.Errorf("request was rejected at auth layer: code=%d body=%s", rec.Code, rec.Body.String())
		}
	}
}

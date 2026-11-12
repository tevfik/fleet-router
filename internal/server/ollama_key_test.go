package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tevfik/fleet-router/internal/config"
)

// A key limited to "qwen*" must not reach another model through the Ollama API either.
func TestOllamaRoutesApplyTheCallersKeyRestrictions(t *testing.T) {
	dir := t.TempDir()
	ks := newKeyStore(filepath.Join(dir, "apikeys.json"))
	SetKeyStore(ks)
	key, err := ks.Create("restricted", "default", "coder", "qwen*")
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "fleet.json")
	cfg := config.Config{Endpoints: []config.Endpoint{
		{Name: "ep-qwen", Model: "qwen-test", URL: "http://127.0.0.1:9999"},
		{Name: "ep-deepseek", Model: "deepseek-test", URL: "http://127.0.0.1:9998"},
	}}
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	srv, err := New(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/chat", "/api/generate"} {
		body := `{"model":"deepseek-test","messages":[{"role":"user","content":"hi"}],"prompt":"hi","stream":false}`
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key.Key)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: a key limited to qwen* got %d for deepseek-test: %s", path, rec.Code, rec.Body.String())
		}
	}
}

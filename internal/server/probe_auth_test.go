package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tevfik/fleet-router/internal/config"
)

func TestProbeAuthScenarios(t *testing.T) {
	const validKey = "sk-valid-token-12345"

	// Mock server requiring Bearer auth
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer "+validKey {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error": {"message": "invalid api key"}}`))
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		// Send a minimal valid SSE stream for probeStream
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"The sky is blue because of Rayleigh scattering.\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer up.Close()

	p := filepath.Join(t.TempDir(), "config.json")
	cfg := &config.Config{
		Name: "test-node",
		Endpoints: []config.Endpoint{
			{Name: "no-key", Node: "test-node", URL: up.URL + "/v1", Model: "qwen"},
			{Name: "wrong-key", Node: "test-node", URL: up.URL + "/v1", Model: "qwen", KeyEnv: "MY_TEST_KEY"},
			{Name: "valid-key-env", Node: "test-node", URL: up.URL + "/v1", Model: "qwen", KeyEnv: "MY_VALID_KEY"},
			{Name: "valid-key-inline", Node: "test-node", URL: up.URL + "/v1", Model: "qwen", Key: validKey},
		},
	}
	if err := cfg.Save(p); err != nil {
		t.Fatal(err)
	}
	s, err := New(p)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Probe without any key: should guide user to set --key or --key-env
	epNoKey := cfg.Endpoints[0]
	resNoKey := s.probeOne(context.Background(), &epNoKey)
	if resNoKey.Err == "" {
		t.Fatalf("expected error on unauthenticated probe, got success")
	}
	if !strings.Contains(resNoKey.Err, "endpoint requires an API key; set --key or --key-env on the endpoint") {
		t.Fatalf("expected guidance error message, got: %s", resNoKey.Err)
	}

	// 2. Probe with wrong key: should point to KeyEnv name
	t.Setenv("MY_TEST_KEY", "wrong-secret")
	epWrong := cfg.Endpoints[1]
	resWrong := s.probeOne(context.Background(), &epWrong)
	if resWrong.Err == "" {
		t.Fatalf("expected error on wrong key probe, got success")
	}
	if !strings.Contains(resWrong.Err, "authentication failed: check the key provided via key_env MY_TEST_KEY") {
		t.Fatalf("expected auth failed message naming key_env, got: %s", resWrong.Err)
	}

	// 3. Probe with valid key via key_env: should succeed
	t.Setenv("MY_VALID_KEY", validKey)
	epValidEnv := cfg.Endpoints[2]
	resValidEnv := s.probeOne(context.Background(), &epValidEnv)
	if resValidEnv.Err != "" {
		t.Fatalf("expected success with valid key_env, got error: %s", resValidEnv.Err)
	}

	// 4. Probe with valid key inline: should succeed
	epValidInline := cfg.Endpoints[3]
	resValidInline := s.probeOne(context.Background(), &epValidInline)
	if resValidInline.Err != "" {
		t.Fatalf("expected success with valid inline key, got error: %s", resValidInline.Err)
	}
}

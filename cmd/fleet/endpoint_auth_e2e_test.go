package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tevfik/fleet-router/internal/config"
)

func TestRealScenario_EndpointAuthenticationFlow(t *testing.T) {
	isolate(t)
	const secretKey = "sk-ollama-super-secret-key-999"

	// 1. Simulating an Ollama / vLLM server that strictly enforces Bearer authentication
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer "+secretKey {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]string{
					"message": "unauthorized: invalid or missing api key",
				},
			})
			return
		}

		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{
					{"id": "qwen2.5-coder:7b"},
				},
			})
		case "/v1/chat/completions":
			var reqBody struct {
				Stream bool `json:"stream"`
			}
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &reqBody)
			if reqBody.Stream {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Hello from authenticated Ollama!\"}}]}\n\n"))
				_, _ = w.Write([]byte("data: [DONE]\n\n"))
			} else {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"choices": []map[string]any{
						{"message": map[string]string{"content": "Hello from authenticated Ollama!"}},
					},
				})
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer mockServer.Close()

	startDaemon(t, &config.Config{Name: "test-box"}, true)

	// Step 1: User adds the endpoint WITHOUT an API key
	runOK(t, func() error {
		return cmdEndpoint([]string{"add", "ollama-unauthed", mockServer.URL + "/v1", "--model", "qwen2.5-coder:7b"})
	})

	// Step 2: User probes the unauthenticated endpoint -> should fail with clear guidance
	probeOut := runOK(t, func() error {
		return cmdProbe([]string{"ollama-unauthed"})
	})
	if !strings.Contains(probeOut, "endpoint requires an API key; set --key or --key-env on the endpoint") {
		t.Fatalf("expected guidance error about missing API key, got:\n%s", probeOut)
	}

	// Step 3: User registers endpoint with --key-env and provides key in environment
	t.Setenv("MY_OLLAMA_KEY", secretKey)
	runOK(t, func() error {
		return cmdEndpoint([]string{"add", "ollama-authed", mockServer.URL + "/v1", "--model", "qwen2.5-coder:7b", "--key-env", "MY_OLLAMA_KEY"})
	})

	// Step 4: Probing with key-env configured must succeed!
	probeAuthed := runOK(t, func() error {
		return cmdProbe([]string{"ollama-authed"})
	})
	if !strings.Contains(probeAuthed, "✓ chat") {
		t.Fatalf("expected successful probe with '✓ chat', got:\n%s", probeAuthed)
	}

	// Step 5: User sets a role and performs an ask request through fleet router
	runOK(t, func() error {
		return cmdRole([]string{"set", "coder", "ollama-authed"})
	})

	askOut := runOK(t, func() error {
		return cmdAsk([]string{"coder", "test prompt"})
	})
	if !strings.Contains(askOut, "Hello from authenticated Ollama!") {
		t.Fatalf("expected response from authenticated Ollama through router, got:\n%s", askOut)
	}
}

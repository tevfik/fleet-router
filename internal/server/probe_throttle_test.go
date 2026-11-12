package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
)

// A ring group gets at most one chat probe per chatProbeEvery (cache_prompt off); the rounds between use /models.
func TestGroupChatProbeIsThrottled(t *testing.T) {
	var mu sync.Mutex
	chats, models := 0, 0
	var body string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			chats++
			b, _ := io.ReadAll(r.Body)
			body = string(b)
		case strings.HasSuffix(r.URL.Path, "/models"):
			models++
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[],"choices":[{"message":{"content":"x"}}]}`))
	}))
	defer up.Close()
	p := filepath.Join(t.TempDir(), "c.json")
	cfg := &config.Config{Name: "m", Endpoints: []config.Endpoint{{Name: "g", Node: "m", URL: up.URL + "/v1", Model: "x",
		Group: &config.Group{Engine: "llama-ring", Workers: []string{"w"}}}}}
	if err := cfg.Save(p); err != nil {
		t.Fatal(err)
	}
	s, err := New(p)
	if err != nil {
		t.Fatal(err)
	}
	oldEvery, oldFail := healthEvery, healthFailFirst
	// due again on every kick below (a healthy endpoint is otherwise left alone a minute; this one's worker "w" does not
	// answer, so it counts as failing and would back off 10 s)
	healthEvery, healthFailFirst = time.Millisecond, time.Millisecond
	defer func() { healthEvery, healthFailFirst = oldEvery, oldFail }() // runs after stopHealth: the loop has exited
	stopHealth := runHealth(s)
	defer stopHealth()
	for i := 0; i < 4; i++ {
		time.Sleep(150 * time.Millisecond)
		s.kick <- struct{}{}
	}
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if chats != 1 {
		t.Errorf("chat probes: %d, want 1 within chatProbeEvery", chats)
	}
	if models < 3 {
		t.Errorf("/models probes between chat probes: %d, want >= 3", models)
	}
	if !strings.Contains(body, `"cache_prompt":false`) {
		t.Errorf("chat probe must not leave prompt cache state: %s", body)
	}
}

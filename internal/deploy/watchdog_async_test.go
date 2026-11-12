package deploy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
)

// An endpoint that has been healthy for StableAfter gets its restart count back: the limit is for one bad spell,
// not for the watchdog's whole life.
func TestWatchdogRestartCountResetsAfterAStablePeriod(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	cfg := config.Default()
	cfg.Endpoints = []config.Endpoint{{Name: "ring-a", URL: srv.URL + "/v1", Group: &config.Group{Engine: "llama-ring", StatsPort: 50155}}}
	now := time.Now()
	w := NewWatchdog(cfg)
	w.logFile = filepath.Join(os.TempDir(), "fleet-watchdog-test.log")
	w.Failed = func(context.Context, config.Endpoint) ([]string, error) { return nil, nil }
	w.Redeploy = func(context.Context, config.Endpoint) error { return nil }
	w.Now = func() time.Time { return now }
	w.Supervised = func(config.Endpoint) bool { return false }
	w.StableAfter = time.Hour
	w.stateOf("ring-a").restarts = 3

	w.CheckOnce(context.Background())
	now = now.Add(30 * time.Minute)
	w.CheckOnce(context.Background())
	if w.stateOf("ring-a").restarts != 3 {
		t.Fatal("30 healthy minutes are not enough")
	}
	now = now.Add(31 * time.Minute)
	w.CheckOnce(context.Background())
	if r := w.stateOf("ring-a").restarts; r != 0 {
		t.Fatalf("restarts = %d after a stable hour", r)
	}
}

// A long redeploy of one endpoint does not hold up the check of the others.
func TestWatchdogRedeployDoesNotBlockOtherEndpoints(t *testing.T) {
	cfg := config.Default()
	cfg.Endpoints = []config.Endpoint{
		{Name: "ring-a", URL: "http://127.0.0.1:1/v1", Group: &config.Group{Engine: "llama-ring", StatsPort: 50155}},
		{Name: "ring-b", URL: "http://127.0.0.1:1/v1", Group: &config.Group{Engine: "llama-ring", StatsPort: 50156}},
	}
	now := time.Now()
	w := NewWatchdog(cfg)
	w.logFile = filepath.Join(os.TempDir(), "fleet-watchdog-test.log")
	w.Failed = func(context.Context, config.Endpoint) ([]string, error) { return []string{"w"}, nil }
	w.Now = func() time.Time { return now }
	w.Supervised = func(config.Endpoint) bool { return false }
	release := make(chan struct{})
	var mu sync.Mutex
	started := map[string]bool{}
	w.Redeploy = func(_ context.Context, ep config.Endpoint) error {
		mu.Lock()
		started[ep.Name] = true
		mu.Unlock()
		if ep.Name == "ring-a" {
			<-release // a long redeploy
		}
		return nil
	}
	done := make(chan struct{})
	go func() { w.checkCluster(context.Background(), &http.Client{Timeout: time.Second}); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the pass was held up by ring-a's long redeploy")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		ok := started["ring-a"] && started["ring-b"]
		mu.Unlock()
		if ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	if !started["ring-b"] {
		t.Fatal("ring-b was not redeployed while ring-a's redeploy was running")
	}
	mu.Unlock()
	close(release)
	w.wg.Wait()
}

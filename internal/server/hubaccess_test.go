package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
)

const (
	hubSecretKey   = "hubkey-SECRET-0123456789"
	hubSecretToken = "jointoken-SECRET-0123456789"
)

// isolate keeps the test away from the real ~/.config/fleet and the real environment's keys.
func isolate(t *testing.T) {
	t.Helper()
	d := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", d)
	t.Setenv("HOME", d)
	t.Setenv("FLEET_API_KEY", "")
}

func newKeyedHub(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	t.Setenv("TEST_HUB_KEY", hubSecretKey)
	p := filepath.Join(t.TempDir(), "hub.json")
	if err := (&config.Config{Name: "hub", APIKeyEnv: "TEST_HUB_KEY", HubRequireKey: true}).Save(p); err != nil {
		t.Fatal(err)
	}
	s, err := New(p)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

// With hub_require_key the control tunnel wants the API key: a node with its key joins, one without is refused,
// and the refusal is visible in the node's tunnel status (rejected, last error, next retry) instead of silent retries.
func TestHubRequireKeyAndVisibleRefusal(t *testing.T) {
	isolate(t)
	hub, hubSrv := newKeyedHub(t)

	good, _ := newNodeDaemon(t, &config.Config{Name: "good", HubAPIKey: hubSecretKey})
	if err := good.joinHub(good.bumpJoinGen(), hubSrv.URL, hubSecretToken, nil, false); err != nil {
		t.Fatalf("node with the key must join: %v", err)
	}
	waitNode(t, hub, "good")

	bad, _ := newNodeDaemon(t, &config.Config{Name: "bad", HubURL: hubSrv.URL, HubAutoJoin: true})
	old := autoJoinFirstWait
	autoJoinFirstWait = 10 * time.Millisecond
	t.Cleanup(func() { autoJoinFirstWait = old })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go bad.autoJoinLoop(ctx)
	var st map[string]any
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st = bad.hubStatus()
		if b, _ := json.Marshal(st["status"]); strings.Contains(string(b), `"rejected":true`) && strings.Contains(string(b), "next_retry") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	b, _ := json.Marshal(st)
	if !strings.Contains(string(b), `"rejected":true`) || !strings.Contains(string(b), "requires an API key") {
		t.Fatalf("the refusal must show in the status: %s", b)
	}
	for _, n := range hub.hub.ConnectedNodes() {
		if n.NodeName == "bad" {
			t.Fatal("a node without the key must not be registered")
		}
	}
}

// The open hub (default) still takes nodes without a key, and a join token is not accepted in place of the API key.
func TestJoinTokenIsNotTheAPIKey(t *testing.T) {
	isolate(t)
	_, hubSrv := newKeyedHub(t)
	n, _ := newNodeDaemon(t, &config.Config{Name: "n"})
	// token equal to the API key's value must not matter: the key is read from hub_api_key / .env, never from the token
	if err := n.joinHub(n.bumpJoinGen(), hubSrv.URL, hubSecretKey, nil, false); err == nil {
		t.Fatal("the join token must not pass as the API key")
	}
}

// Secrets stay out of what the daemon answers: /fleet/config to a caller that is not this machine, the hub settings
// answer, the tunnel status and error messages.
func TestHubSecretsNeverInResponses(t *testing.T) {
	isolate(t)
	s, _ := newNodeDaemon(t, &config.Config{Name: "n", HubToken: hubSecretToken, HubAPIKey: hubSecretKey, HubURL: "https://hub.example"})
	s2 := s
	s2.cfg.AdminToken = "admintok"
	for _, mods := range [][]rtMod{{rtRemote, rtBearer("admintok")}, {func(r *http.Request) { r.Header.Set("X-Forwarded-For", "203.0.113.9") }}} {
		rec := rtDo(t, s2, "GET", "/fleet/config", nil, mods...)
		body := rec.Body.String()
		if strings.Contains(body, hubSecretKey) || strings.Contains(body, hubSecretToken) || strings.Contains(body, "admintok") {
			t.Fatalf("a secret left in /fleet/config for a non-local caller: %s", body)
		}
	}
	// the menu on this machine still gets them
	if body := rtDo(t, s2, "GET", "/fleet/config", nil).Body.String(); !strings.Contains(body, hubSecretKey) {
		t.Fatal("the local menu needs the hub key to show whether it is set")
	}
	rec := rtDo(t, s2, "POST", "/fleet/hub/settings", map[string]any{"hub_url": "https://h", "hub_token": hubSecretToken})
	if rec.Code != 200 || strings.Contains(rec.Body.String(), hubSecretToken) {
		t.Fatalf("hub settings answer must not echo the token: %d %s", rec.Code, rec.Body.String())
	}
	rec = rtDo(t, s2, "GET", "/fleet/tunnel/status", nil)
	if strings.Contains(rec.Body.String(), hubSecretToken) || strings.Contains(rec.Body.String(), hubSecretKey) {
		t.Fatalf("tunnel status leaks a secret: %s", rec.Body.String())
	}
}

// Neither the daemon's log nor the errors of a refused or successful join contain the API key or the join token.
func TestJoinLogsAndErrorsCarryNoSecrets(t *testing.T) {
	isolate(t)
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	_, hubSrv := newKeyedHub(t)

	n, _ := newNodeDaemon(t, &config.Config{Name: "n", HubAPIKey: "wrong-" + hubSecretKey, HubURL: hubSrv.URL, HubToken: hubSecretToken, HubAutoJoin: true})
	err := n.joinHub(n.bumpJoinGen(), hubSrv.URL, hubSecretToken, nil, false)
	if err == nil {
		t.Fatal("a wrong key must be refused")
	}
	old := autoJoinFirstWait
	autoJoinFirstWait = 5 * time.Millisecond
	t.Cleanup(func() { autoJoinFirstWait = old })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	n.autoJoinLoop(ctx)
	out := buf.String() + err.Error() + fmt.Sprint(n.hubStatus())
	for _, secret := range []string{hubSecretKey, hubSecretToken} {
		if strings.Contains(out, secret) {
			t.Fatalf("a secret leaked into logs/errors/status:\n%s", out)
		}
	}
	if !strings.Contains(buf.String(), "refused this node") {
		t.Fatalf("the refusal should be logged:\n%s", buf.String())
	}
}

// hubUsageTestServer creates a keyed hub with a usage endpoint for testing.
func hubUsageTestServer(t *testing.T, key string) (*Server, *httptest.Server) {
	t.Helper()
	t.Setenv("TEST_HUB_KEY", key)
	p := filepath.Join(t.TempDir(), "hub.json")
	if err := (&config.Config{Name: "hub", APIKeyEnv: "TEST_HUB_KEY", HubRequireKey: true}).Save(p); err != nil {
		t.Fatal(err)
	}
	s, err := New(p)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

// TestHubUsageEndpoint tests the /fleet/tunnel/usage route which returns usage statistics
// authorized by the hub API key instead of admin rights.
func TestHubUsageEndpoint(t *testing.T) {
	isolate(t)
	_, hubSrv := hubUsageTestServer(t, hubSecretKey)

	tests := []struct {
		name       string
		auth       string
		wantStatus int
	}{
		{
			name:       "correct hub key",
			auth:       "Bearer " + hubSecretKey,
			wantStatus: 200,
		},
		{
			name:       "wrong hub key",
			auth:       "Bearer wrong-key",
			wantStatus: 401,
		},
		{
			name:       "no auth header",
			auth:       "",
			wantStatus: 401,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest("GET", hubSrv.URL+"/fleet/tunnel/usage?by=client&since=1h", nil)
			if tt.auth != "" {
				req.Header.Set("Authorization", tt.auth)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tt.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			// When successful, verify we get a valid JSON response with rows
			if tt.wantStatus == 200 {
				var out struct {
					By      string     `json:"by"`
					SinceS  int        `json:"since_s"`
					Rows    []UsageRow `json:"rows"`
					Page    int        `json:"page"`
					PerPage int        `json:"per_page"`
				}
				json.NewDecoder(resp.Body).Decode(&out)
				if out.By != "client" {
					t.Errorf("by = %q, want \"client\"", out.By)
				}
			}
		})
	}
}

// TestHubUsageEndpointNoHubKey tests that /fleet/tunnel/usage returns 404 when no hub key is configured.
func TestHubUsageEndpointNoHubKey(t *testing.T) {
	isolate(t)
	p := filepath.Join(t.TempDir(), "hub.json")
	if err := (&config.Config{Name: "hub"}).Save(p); err != nil {
		t.Fatal(err)
	}
	s, err := New(p)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/fleet/tunnel/usage", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("status = %d, want 401 (no key configured: never open)", resp.StatusCode)
	}
}

// the hub also accepts, for its streams and tunnel, a paired key and an API key with the stream role: the same
// credentials must read the usage statistics
func TestHubUsageEndpointAcceptsStreamCredentials(t *testing.T) {
	isolate(t)
	s, ts := hubUsageTestServer(t, hubSecretKey)
	ak, err := GetKeyStore().Create("node-stream", "", "stream")
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		token string
		want  int
	}{"stream-role API key": {ak.Key, 200}, "unknown token": {"flt_not_a_key", 401}}
	_ = s
	for name, c := range cases {
		req, _ := http.NewRequest("GET", ts.URL+"/fleet/tunnel/usage?by=client", nil)
		req.Header.Set("Authorization", "Bearer "+c.token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s: status %d, want %d", name, resp.StatusCode, c.want)
		}
	}
}

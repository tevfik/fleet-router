package tunnel

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestForwardToLocal_HubTunnelClientHeader verifies that X-Fleet-Client is always set to "hub-tunnel"
// even when the hub sends a malicious value in the request message.
func TestForwardToLocal_HubTunnelClientHeader(t *testing.T) {
	// 1. Local backend server that echoes back the X-Fleet-Client header value
	var receivedHeader string
	var mu sync.Mutex
	localBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		receivedHeader = r.Header.Get("X-Fleet-Client")
		mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Node-Echo", "edge-1")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer localBackend.Close()

	// 2. Hub server
	var joinedMu sync.Mutex
	var joinedNode string
	hub := NewHub(func(reg NodeRegistration) {
		joinedMu.Lock()
		joinedNode = reg.NodeName
		joinedMu.Unlock()
	}, func(nodeName string) {})

	mux := http.NewServeMux()
	mux.Handle("/fleet/tunnel/connect", hub.Handler())
	hubServer := httptest.NewServer(mux)
	defer hubServer.Close()

	// 3. Client connects
	reg := NodeRegistration{
		NodeName: "test-node",
		GPU:      "RTX 5070 Ti",
		JoinedAt: time.Now(),
	}
	cli, err := StartClient(hubServer.URL, localBackend.URL, reg)
	if err != nil {
		t.Fatalf("failed to start tunnel client: %v", err)
	}
	defer cli.Close()

	// Wait for registration
	time.Sleep(100 * time.Millisecond)

	joinedMu.Lock()
	jn := joinedNode
	joinedMu.Unlock()
	if jn != "test-node" {
		t.Fatalf("expected onJoin called with test-node, got: %s", jn)
	}

	// 4. Test that X-Fleet-Client is overridden to "hub-tunnel" even when hub sends "evil"
	// Create an HTTP request with a malicious X-Fleet-Client header
	req := httptest.NewRequest("POST", "/v1/test", strings.NewReader("hello-model"))
	req.Header.Set("X-Fleet-Client", "evil")
	rec := httptest.NewRecorder()
	err = hub.ForwardRequest("test-node", req, rec)
	if err != nil {
		t.Fatalf("failed to forward request: %v", err)
	}

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}

	// Verify the local backend received the correct header value (should be "hub-tunnel", not "evil")
	mu.Lock()
	received := receivedHeader
	mu.Unlock()

	if received != hubTunnelClientValue {
		t.Errorf("expected X-Fleet-Client header to be %q, got %q", hubTunnelClientValue, received)
	}
}

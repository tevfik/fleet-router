package tunnel

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTunnel_EndToEnd(t *testing.T) {
	// 1. Local backend server that represents edge fleet
	localBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Node-Echo", "edge-1")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(fmt.Sprintf("echo: %s", string(body))))
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
		NodeName: "dino-node",
		GPU:      "RTX 5070 Ti",
		Models:   []string{"qwen3.5-122b"},
		JoinedAt: time.Now(),
	}
	cli, err := StartClient(hubServer.URL, localBackend.URL, reg)
	if err != nil {
		t.Fatalf("failed to start tunnel client: %v", err)
	}
	defer cli.Close()

	// Wait for registration
	time.Sleep(100 * time.Millisecond)

	nodes := hub.ConnectedNodes()
	if len(nodes) != 1 || nodes[0].NodeName != "dino-node" {
		t.Fatalf("expected dino-node in connected nodes, got: %+v", nodes)
	}
	joinedMu.Lock()
	jn := joinedNode
	joinedMu.Unlock()
	if jn != "dino-node" {
		t.Fatalf("expected onJoin called with dino-node, got: %s", jn)
	}

	// 4. Test ForwardRequest through tunnel
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("hello-model"))
	rec := httptest.NewRecorder()
	err = hub.ForwardRequest("dino-node", req, rec)
	if err != nil {
		t.Fatalf("failed to forward request: %v", err)
	}

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if rec.Header().Get("X-Node-Echo") != "edge-1" {
		t.Errorf("unexpected echo header: %s", rec.Header().Get("X-Node-Echo"))
	}
	if rec.Body.String() != "echo: hello-model" {
		t.Errorf("unexpected body: %s", rec.Body.String())
	}
}

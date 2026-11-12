package server

import (
	"context"
	"encoding/json"
	"github.com/tevfik/fleet-router/internal/config"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/tunnel"
)

// A request for a model of a joined node reaches that node's daemon on the same /v1 path, with the node's own model name.
func TestHubForwardsV1PathToNode(t *testing.T) {
	hub, hubSrv := newHubDaemon(t)
	var mu sync.Mutex
	var gotPath, gotModel string
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		mu.Lock()
		gotPath = r.URL.Path
		gotModel, _ = m["model"].(string)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer node.Close()
	cli, err := tunnel.StartClient(hubSrv.URL, node.URL, tunnel.NodeRegistration{NodeName: "worker", Models: []string{"m1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	waitNode(t, hub, "worker")
	resp, err := http.Post(hubSrv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"worker/m1","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	mu.Lock()
	defer mu.Unlock()
	if resp.StatusCode != 200 || gotPath != "/v1/chat/completions" || gotModel != "m1" {
		t.Fatalf("status %d body %s: node got path %q model %q, want /v1/chat/completions m1", resp.StatusCode, body, gotPath, gotModel)
	}
}

// TestHubTunnelUsageNodes records usage with the correct "node/model" key from tunnel responses.
func TestHubTunnelUsageNodes(t *testing.T) {
	hub, hubSrv := newHubDaemon(t)

	// Create a node that returns usage in its response
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":11,"completion_tokens":7}}`))
	}))
	defer node.Close()

	// Join node to hub
	cli, err := tunnel.StartClient(hubSrv.URL, node.URL, tunnel.NodeRegistration{NodeName: "worker", Models: []string{"m1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	waitNode(t, hub, "worker")

	// Make a request through the hub
	resp, err := http.Post(hubSrv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"worker/m1","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}

	// Wait for usage to be recorded
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		rows := hub.Usage("model", "", time.Hour)
		for _, r := range rows {
			if r.Key == "worker/m1" && r.Requests == 1 && r.In == 11 && r.Out == 7 {
				return // success
			}
		}
	}
	t.Fatalf("usage not recorded correctly: got %+v", hub.Usage("model", "", time.Hour))
}

// TestHubTunnelUsageStreaming records usage for streaming (SSE) responses.
func TestHubTunnelUsageStreaming(t *testing.T) {
	hub, hubSrv := newHubDaemon(t)

	// Create a node that returns streaming SSE with usage
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		// Simulate SSE stream with usage in last chunk
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"))
		flusher.Flush()
		time.Sleep(10 * time.Millisecond)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{}}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":3}}\n\n"))
		flusher.Flush()
	}))
	defer node.Close()

	// Join node to hub
	cli, err := tunnel.StartClient(hubSrv.URL, node.URL, tunnel.NodeRegistration{NodeName: "worker", Models: []string{"m1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	waitNode(t, hub, "worker")

	// Make a streaming request through the hub
	resp, err := http.Post(hubSrv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"worker/m1","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}

	// Wait for usage to be recorded
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		rows := hub.Usage("model", "", time.Hour)
		for _, r := range rows {
			if r.Key == "worker/m1" && r.Requests == 1 && r.In == 5 && r.Out == 3 {
				return // success
			}
		}
	}
	t.Fatalf("streaming usage not recorded correctly: got %+v", hub.Usage("model", "", time.Hour))
}

// On the hub, an endpoint of a joined node is healthy while the node is connected and announces the model (it used
// to be probed at tunnel://… and was always down); a node that leaves makes it down.
func TestHubTunnelEndpointHealth(t *testing.T) {
	hub, hubSrv := newHubDaemon(t)
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("{}")) }))
	defer node.Close()
	cli, err := tunnel.StartClient(hubSrv.URL, node.URL, tunnel.NodeRegistration{NodeName: "worker", Models: []string{"m1"}})
	if err != nil {
		t.Fatal(err)
	}
	waitNode(t, hub, "worker")
	var ep *config.Endpoint
	for i := range hub.Config().Endpoints {
		if e := hub.Config().Endpoints[i]; e.Node == "worker" && e.Model == "m1" {
			ep = &e
		}
	}
	if ep == nil {
		t.Fatalf("no endpoint for worker/m1: %+v", hub.Config().Endpoints)
	}
	hub.probe(context.Background(), ep)
	if !hub.healthy(ep.Name) {
		t.Fatalf("connected node: %+v", hub.getState(ep.Name))
	}
	if ok, why := hub.tunnelServes("worker", "other"); ok || !strings.Contains(why, "no longer announces") {
		t.Fatalf("another model: %v %s", ok, why)
	}
	cli.Close()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		hub.probe(context.Background(), ep)
		if !hub.healthy(ep.Name) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("node gone, still healthy: %+v", hub.getState(ep.Name))
}

// A node whose model server answers 502 is not passed to the client as a success: the hub's proxy reports the
// failure (and would try another endpoint of the model).
func TestHubTunnelUpstreamErrorNotSuccess(t *testing.T) {
	hub, hubSrv := newHubDaemon(t)
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"error":"backend down"}`))
	}))
	defer node.Close()
	cli, err := tunnel.StartClient(hubSrv.URL, node.URL, tunnel.NodeRegistration{NodeName: "worker", Models: []string{"m1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	waitNode(t, hub, "worker")
	resp, err := http.Post(hubSrv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"worker/m1","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode < 500 || !strings.Contains(string(body), "backend down") {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
}

package tunnel

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

func TestCovStatusErrorAndTransientAndExplain(t *testing.T) {
	// StatusError.Error() and IsAuth()
	se401 := &StatusError{Status: http.StatusUnauthorized, Msg: "invalid key"}
	if !se401.IsAuth() {
		t.Error("401 should be IsAuth")
	}
	if !strings.Contains(se401.Error(), "401") || !strings.Contains(se401.Error(), "invalid key") {
		t.Errorf("unexpected StatusError.Error(): %s", se401.Error())
	}

	se403 := &StatusError{Status: http.StatusForbidden, Msg: "forbidden"}
	if !se403.IsAuth() {
		t.Error("403 should be IsAuth")
	}

	se500 := &StatusError{Status: http.StatusInternalServerError, Msg: "internal"}
	if se500.IsAuth() {
		t.Error("500 should not be IsAuth")
	}

	// transient(err)
	if !transient(&StatusError{Status: http.StatusBadGateway, Msg: "gateway error"}) {
		t.Error("502 without 'node' in msg should be transient")
	}
	if transient(&StatusError{Status: http.StatusBadGateway, Msg: "node refused"}) {
		t.Error("502 with 'node' in msg should NOT be transient")
	}
	if !transient(&StatusError{Status: http.StatusServiceUnavailable}) {
		t.Error("503 should be transient")
	}
	if transient(&StatusError{Status: http.StatusUnauthorized}) {
		t.Error("401 should NOT be transient")
	}
	if !transient(&websocket.DialError{Err: errors.New("connection reset")}) {
		t.Error("websocket.DialError should be transient")
	}
	if transient(errors.New("some other error")) {
		t.Error("generic error should not be transient")
	}

	// explain(err)
	sc := &StreamConnector{HubURL: "https://hub.example.com", Node: "dino", Port: 8080}
	tests := []struct {
		err  error
		hint string
	}{
		{errors.New("raw dial failed"), "cannot reach the hub"},
		{&StatusError{Status: http.StatusUnauthorized}, "API key"},
		{&StatusError{Status: http.StatusNotFound}, "not joined"},
		{&StatusError{Status: http.StatusForbidden}, "did not declare port"},
		{&StatusError{Status: http.StatusTooManyRequests}, "too many streams"},
		{&StatusError{Status: http.StatusBadGateway}, "is the worker on port 8080 running"},
		{&StatusError{Status: http.StatusTeapot}, "stream to dino:8080"},
	}

	for _, tc := range tests {
		got := sc.explain(tc.err).Error()
		if !strings.Contains(got, tc.hint) {
			t.Errorf("explain(%v) got %q, want containing %q", tc.err, got, tc.hint)
		}
	}
}

func TestCovStreamHubClaimAndFailStream(t *testing.T) {
	hub := NewHub(nil, nil)

	// failStream with empty ID
	if hub.failStream(nil, "", "reason") {
		t.Error("failStream with empty ID should return false")
	}

	// failStream with non-existent stream ID
	if hub.failStream(nil, "stream-ghost", "reason") {
		t.Error("failStream with ghost ID should return false")
	}

	// Add a dummy pending stream to hub
	streamID := "test-stream-1"
	ch := make(chan acceptResult, 1)
	nt := &nodeTunnel{}
	hub.pendingStreams[streamID] = &pendingStream{
		nt: nt,
		ch: ch,
	}

	// failStream with wrong nt
	otherNT := &nodeTunnel{}
	if hub.failStream(otherNT, streamID, "wrong node") {
		t.Error("failStream with mismatched nt should return false")
	}

	// failStream with correct nt
	if !hub.failStream(nt, streamID, "node refused connection") {
		t.Error("failStream with correct nt should return true")
	}

	// Verify result was delivered to channel
	select {
	case res := <-ch:
		if res.err != "node refused connection" {
			t.Fatalf("unexpected acceptResult: %+v", res)
		}
	default:
		t.Fatal("channel should have received acceptResult")
	}

	// claimStream
	streamID2 := "test-stream-2"
	hub.pendingStreams[streamID2] = &pendingStream{nt: nt}
	ps, ok := hub.claimStream(streamID2)
	if !ok || ps.nt != nt {
		t.Fatalf("claimStream failed: ps=%+v, ok=%v", ps, ok)
	}
	// Claiming again should return false
	if _, ok := hub.claimStream(streamID2); ok {
		t.Error("second claimStream should return false")
	}
}

func TestCovTunnelAuthRejection(t *testing.T) {
	// Set short reconnect timers for the test and restore on exit
	origFirst := reconnectFirst.get()
	origMax := reconnectMax.get()
	reconnectFirst.set(50 * time.Millisecond)
	reconnectMax.set(100 * time.Millisecond)
	defer func() {
		reconnectFirst.set(origFirst)
		reconnectMax.set(origMax)
	}()

	// Hub that rejects connect based on auth check
	hub := NewHub(nil, nil)
	hub.SetConnectAuth(func(r *http.Request) bool {
		return r.Header.Get("Authorization") == "Bearer valid-token"
	})

	mux := http.NewServeMux()
	mux.Handle("/fleet/tunnel/connect", hub.Handler())
	srv := httptest.NewServer(mux)
	defer srv.Close()

	reg := NodeRegistration{
		NodeName: "auth-test-node",
		JoinedAt: time.Now(),
	}

	// Client connecting with invalid API key
	opts := Options{
		APIKey: "wrong-token",
	}
	cli, err := StartClientOpts(srv.URL, "http://127.0.0.1:1", reg, opts)
	if err == nil {
		cli.Close()
		t.Fatal("StartClientOpts should fail immediately on auth rejection (401)")
	}

	// Verify it returned a StatusError with 401
	var se *StatusError
	if !errors.As(err, &se) || !se.IsAuth() {
		t.Fatalf("expected 401 auth error, got: %v", err)
	}
}

func TestCovTunnelConnectionDropAndReconnect(t *testing.T) {
	// Shorten reconnect delay and restore on exit
	origFirst := reconnectFirst.get()
	origMax := reconnectMax.get()
	origStable := stableAfter.get()
	reconnectFirst.set(50 * time.Millisecond)
	reconnectMax.set(150 * time.Millisecond)
	stableAfter.set(100 * time.Millisecond)
	defer func() {
		reconnectFirst.set(origFirst)
		reconnectMax.set(origMax)
		stableAfter.set(origStable)
	}()

	var dropCount atomic.Int32
	var joinCount atomic.Int32

	hub := NewHub(func(reg NodeRegistration) {
		joinCount.Add(1)
	}, func(nodeName string) {
		dropCount.Add(1)
	})

	// Use an atomic switch to control whether handler accepts or drops
	var dropConnections atomic.Bool

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if dropConnections.Load() {
			http.Error(w, "server closing down", http.StatusServiceUnavailable)
			return
		}
		hub.Handler().ServeHTTP(w, r)
	})

	mux := http.NewServeMux()
	mux.Handle("/fleet/tunnel/connect", handler)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	reg := NodeRegistration{
		NodeName:  "reconnect-node",
		AuthToken: "recon-tok",
		JoinedAt:  time.Now(),
	}

	cli, err := StartClient(srv.URL, "http://127.0.0.1:1", reg)
	if err != nil {
		t.Fatalf("initial StartClient failed: %v", err)
	}
	defer cli.Close()

	// Wait for connection to establish
	waitFor(t, "connected initially", func() bool {
		return cli.IsConnected() && len(hub.ConnectedNodes()) == 1
	})

	// Force connection drop by closing activeWS directly from hub side
	hub.mu.Lock()
	nt := hub.tunnels["reconnect-node"]
	hub.mu.Unlock()
	if nt != nil {
		_ = nt.ws.Close()
	}

	// Verify client detects drop
	waitFor(t, "client detects drop", func() bool {
		st := cli.Status()
		return !st.Connected && st.LastError != ""
	})

	// Wait for automatic reconnection
	waitFor(t, "client automatically reconnects", func() bool {
		return cli.IsConnected() && len(hub.ConnectedNodes()) == 1
	})

	// Test Status() inspection
	st := cli.Status()
	if !st.Connected {
		t.Fatal("expected status to show Connected == true")
	}
	if st.ConnectedSince.IsZero() {
		t.Error("ConnectedSince should be set")
	}
}

func TestCovStreamDialHandlerValidationAndRefusals(t *testing.T) {
	hub := NewHub(nil, nil)
	hub.SetStreamAuth(func(r *http.Request) bool {
		return r.Header.Get("Authorization") == "Bearer valid-stream-key"
	})

	mux := http.NewServeMux()
	mux.Handle("/fleet/tunnel/stream/dial", hub.StreamDialHandler())
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// 1. Missing / wrong auth
	req, _ := http.NewRequest("GET", srv.URL+"/fleet/tunnel/stream/dial?node=box&port=8080", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized, got %d", resp.StatusCode)
	}

	// 2. Missing node query param
	req, _ = http.NewRequest("GET", srv.URL+"/fleet/tunnel/stream/dial?port=8080", nil)
	req.Header.Set("Authorization", "Bearer valid-stream-key")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for missing node, got %d", resp.StatusCode)
	}

	// 3. Invalid port
	req, _ = http.NewRequest("GET", srv.URL+"/fleet/tunnel/stream/dial?node=box&port=not-a-number", nil)
	req.Header.Set("Authorization", "Bearer valid-stream-key")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for invalid port, got %d", resp.StatusCode)
	}

	// 4. Node not joined (404)
	req, _ = http.NewRequest("GET", srv.URL+"/fleet/tunnel/stream/dial?node=ghost-box&port=8080", nil)
	req.Header.Set("Authorization", "Bearer valid-stream-key")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 Not Found for missing node, got %d", resp.StatusCode)
	}
}

func TestCovStreamConnectorServeAndHandle(t *testing.T) {
	origWait := dialRetryWait
	origRetries := dialRetries
	dialRetryWait = 5 * time.Millisecond
	dialRetries = 1
	defer func() {
		dialRetryWait = origWait
		dialRetries = origRetries
	}()

	// Start local listener for StreamConnector
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	sc := &StreamConnector{
		HubURL: "http://127.0.0.1:1", // intentionally invalid hub URL so Dial fails quickly
		Node:   "test-node",
		Port:   9999,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveDone := make(chan error, 1)
	go func() {
		serveDone <- sc.Serve(ctx, ln)
	}()

	// Connect a dummy client to trigger handle()
	clientConn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_ = clientConn.Close()

	// Wait a moment for handle to finish Dial failure
	time.Sleep(50 * time.Millisecond)

	// Cancel context and wait for Serve to exit cleanly
	cancel()
	select {
	case err := <-serveDone:
		if err != nil {
			t.Errorf("Serve returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not terminate after context cancel")
	}
}

func TestCovStreamHelpersAndOpenTimeout(t *testing.T) {
	// wsBase
	if got := wsBase("https://hub.example.com/"); got != "wss://hub.example.com" {
		t.Errorf("wsBase(https) got %q, want wss://hub.example.com", got)
	}
	if got := wsBase("http://hub.example.com:8080"); got != "ws://hub.example.com:8080" {
		t.Errorf("wsBase(http) got %q, want ws://hub.example.com:8080", got)
	}
	if got := wsBase("wss://hub.example.com"); got != "wss://hub.example.com" {
		t.Errorf("wsBase(wss) got %q, want wss://hub.example.com", got)
	}
	if got := wsBase("ws://hub.example.com"); got != "ws://hub.example.com" {
		t.Errorf("wsBase(ws) got %q, want ws://hub.example.com", got)
	}
	if got := wsBase("hub.example.com"); got != "wss://hub.example.com" {
		t.Errorf("wsBase(bare) got %q, want wss://hub.example.com", got)
	}

	// newStreamID
	id := newStreamID()
	if !strings.HasPrefix(id, "st-") || len(id) != 3+32 {
		t.Errorf("newStreamID() got %q, want st-<32hex>", id)
	}

	// openTimeout
	hub := NewHub(nil, nil)
	if d := hub.openTimeout(); d != DefaultStreamOpenTimeout {
		t.Errorf("openTimeout() default got %v, want %v", d, DefaultStreamOpenTimeout)
	}
	hub.StreamOpenTimeout = 5 * time.Second
	if d := hub.openTimeout(); d != 5*time.Second {
		t.Errorf("openTimeout() custom got %v, want 5s", d)
	}

	// streamSet.add when already closed
	var ss streamSet
	ss.closeAll()
	r, w := net.Pipe()
	defer r.Close()
	if ss.add(w) {
		t.Error("streamSet.add on closed set should return false")
	}
}

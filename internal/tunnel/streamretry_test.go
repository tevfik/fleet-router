package tunnel

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fastDialRetry(t *testing.T) {
	t.Helper()
	r, w := dialRetries, dialRetryWait
	dialRetries, dialRetryWait = 3, 10*time.Millisecond
	t.Cleanup(func() { dialRetries, dialRetryWait = r, w })
}

// A hub that is restarting (503 from the proxy, then up) does not fail a new stream: Dial waits and succeeds.
func TestStreamDialRetriesWhileHubRestarts(t *testing.T) {
	fastDialRetry(t)
	hub, srv := newStreamHub(t)
	port := startTCP(t, func(c net.Conn) { io.Copy(c, c) }).port
	joinNode(t, hub, srv.URL, "w", "", port)
	var down atomic.Int32
	down.Store(2)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Add(-1) >= 0 {
			http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
			return
		}
		httputilProxy(srv.URL, w, r)
	}))
	defer front.Close()
	sc := &StreamConnector{HubURL: front.URL, Node: "w", Port: port, Key: testKey}
	ws, err := sc.Dial()
	if err != nil {
		t.Fatalf("Dial must outlast a short hub outage: %v", err)
	}
	ws.Close()
}

// A refusal that will not change (wrong key) is not retried and says what to do.
func TestStreamDialExplainsRefusalWithoutRetry(t *testing.T) {
	fastDialRetry(t)
	hub, srv := newStreamHub(t)
	var hits atomic.Int32
	counting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		httputilProxy(srv.URL, w, r)
	}))
	defer counting.Close()
	_ = hub
	sc := &StreamConnector{HubURL: counting.URL, Node: "w", Port: 1234, Key: "sekrit-k3y"}
	_, err := sc.Dial()
	if err == nil || !strings.Contains(err.Error(), "does not accept the API key") || !strings.Contains(err.Error(), "401") {
		t.Fatalf("want a 401 explained, got %v", err)
	}
	// one dial plus one diagnostic probe, never the retry series
	if n := hits.Load(); n > 2 {
		t.Fatalf("a 401 must not be retried, hub saw %d requests", n)
	}
	if strings.Contains(err.Error(), "sekrit-k3y") {
		t.Fatal("the key must not appear in the error")
	}
}

func TestStreamDialNotJoinedHint(t *testing.T) {
	fastDialRetry(t)
	_, srv := newStreamHub(t)
	sc := &StreamConnector{HubURL: srv.URL, Node: "ghost", Port: 50160, Key: testKey}
	_, err := sc.Dial()
	if err == nil || !strings.Contains(err.Error(), "not joined") {
		t.Fatalf("got %v", err)
	}
}

func TestHubActiveStreamsCount(t *testing.T) {
	hub, srv := newStreamHub(t)
	port := startTCP(t, func(c net.Conn) { io.Copy(io.Discard, c) }).port
	joinNode(t, hub, srv.URL, "w", "", port)
	if len(hub.ActiveStreams()) != 0 {
		t.Fatal("none yet")
	}
	sc := &StreamConnector{HubURL: srv.URL, Node: "w", Port: port, Key: testKey}
	ws, err := sc.Dial()
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	waitFor(t, "stream counted", func() bool { return hub.ActiveStreams()["w"] == 1 })
}

// httputilProxy forwards a request (WebSocket upgrades included) to the hub at target.
func httputilProxy(target string, w http.ResponseWriter, r *http.Request) {
	u, _ := url.Parse(target)
	httputil.NewSingleHostReverseProxy(u).ServeHTTP(w, r)
}

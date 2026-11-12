package tunnel

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// A node that reconnects replaces its old tunnel; the old tunnel's cleanup must not report the node as dropped
// (the server removes the node's endpoints on a drop, which would erase what the new tunnel just registered).
func TestReconnectDoesNotReportDrop(t *testing.T) {
	var drops atomic.Int32
	hub := NewHub(nil, func(string) { drops.Add(1) })
	mux := http.NewServeMux()
	mux.Handle("/fleet/tunnel/connect", hub.Handler())
	srv := httptest.NewServer(mux)
	defer srv.Close()
	reg := NodeRegistration{NodeName: "w1", AuthToken: "tok", JoinedAt: time.Now()}

	c1, err := StartClient(srv.URL, "http://127.0.0.1:1", reg)
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	waitJoined(t, hub, "w1")
	c2, err := StartClient(srv.URL, "http://127.0.0.1:1", reg) // same node, same token: replaces c1's tunnel
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	time.Sleep(300 * time.Millisecond) // the old handler's cleanup runs now
	if n := drops.Load(); n != 0 {
		t.Fatalf("a replaced tunnel reported %d drop(s) while the node is connected", n)
	}
	c2.Close()
	waitFor(t, "the real drop", func() bool { return drops.Load() == 1 })
}

// Without a join token nothing proves a second tunnel is the same node: it must not take over a live tunnel's name.
func TestTokenlessNodeNameNotTakenOverWhileLive(t *testing.T) {
	hub := NewHub(nil, nil)
	mux := http.NewServeMux()
	mux.Handle("/fleet/tunnel/connect", hub.Handler())
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c1, err := StartClient(srv.URL, "http://127.0.0.1:1", NodeRegistration{NodeName: "w1", JoinedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	waitJoined(t, hub, "w1")
	if c2, err := StartClient(srv.URL, "http://127.0.0.1:1", NodeRegistration{NodeName: "w1", JoinedAt: time.Now()}); err == nil {
		c2.Close()
		t.Fatal("a tokenless second tunnel took over a live node name")
	}
}

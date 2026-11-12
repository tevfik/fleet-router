package tunnel

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type blockingWriter struct {
	hdr     http.Header
	release chan struct{}
	once    sync.Once
	started chan struct{}
}

func (b *blockingWriter) Header() http.Header { return b.hdr }
func (b *blockingWriter) WriteHeader(int)     {}
func (b *blockingWriter) Write(p []byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	<-b.release
	return len(p), nil
}

func tunnelHubWithBackend(t *testing.T, backend http.Handler) (*Hub, *httptest.Server, *Client, *httptest.Server) {
	t.Helper()
	be := httptest.NewServer(backend)
	t.Cleanup(be.Close)
	hub := NewHub(nil, nil)
	mux := http.NewServeMux()
	mux.Handle("/fleet/tunnel/connect", hub.Handler())
	hs := httptest.NewServer(mux)
	t.Cleanup(hs.Close)
	cli, err := StartClient(hs.URL, be.URL, NodeRegistration{NodeName: "n1", AuthToken: "t", JoinedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cli.Close)
	waitJoined(t, hub, "n1")
	return hub, hs, cli, be
}

// When the node's tunnel drops, a request waiting for its answer ends at once (it used to wait 10 minutes).
func TestForwardRequestEndsWhenTheTunnelDrops(t *testing.T) {
	hold := make(chan struct{})
	hub, _, cli, _ := tunnelHubWithBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-hold }))
	t.Cleanup(func() { close(hold) })
	done := make(chan error, 1)
	go func() {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("x"))
		done <- hub.ForwardRequest("n1", req, httptest.NewRecorder())
	}()
	time.Sleep(200 * time.Millisecond)
	cli.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a dropped tunnel must be an error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the request kept waiting after the tunnel dropped")
	}
}

// A client that reads too slowly gives up its own request; the other requests of the node go on.
func TestSlowConsumerDoesNotStallOtherRequests(t *testing.T) {
	old := slowConsumerWait
	slowConsumerWait = 100 * time.Millisecond
	t.Cleanup(func() { slowConsumerWait = old })
	chunk := strings.Repeat("a", 512)
	hub, _, _, _ := tunnelHubWithBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/fast") {
			_, _ = w.Write([]byte("ok"))
			return
		}
		fl := w.(http.Flusher)
		for i := 0; i < 400; i++ {
			_, _ = w.Write([]byte(chunk))
			fl.Flush()
			time.Sleep(2 * time.Millisecond)
		}
	}))
	bw := &blockingWriter{hdr: http.Header{}, release: make(chan struct{}), started: make(chan struct{})}
	slowDone := make(chan error, 1)
	go func() {
		slowDone <- hub.ForwardRequest("n1", httptest.NewRequest("POST", "/v1/slow", strings.NewReader("x")), bw)
	}()
	<-bw.started
	time.Sleep(1500 * time.Millisecond) // the slow request's channel fills up and is given up

	fast := httptest.NewRecorder()
	fdone := make(chan error, 1)
	go func() {
		fdone <- hub.ForwardRequest("n1", httptest.NewRequest("POST", "/v1/fast", strings.NewReader("x")), fast)
	}()
	select {
	case err := <-fdone:
		if err != nil || fast.Body.String() != "ok" {
			t.Fatalf("fast request: %v %q", err, fast.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a slow reader stalled the other request of the node")
	}
	close(bw.release)
	select {
	case err := <-slowDone:
		if !errors.Is(err, ErrResponseStarted) {
			t.Fatalf("the abandoned request must report a started response, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the abandoned request never returned")
	}
}

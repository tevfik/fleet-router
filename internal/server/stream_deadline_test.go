package server

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A client that connects and never reads must not hold the handler for ever.
func TestStreamGivesUpOnAClientThatDoesNotRead(t *testing.T) {
	old := streamWriteTimeout
	streamWriteTimeout = 300 * time.Millisecond
	t.Cleanup(func() { streamWriteTimeout = old })
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		stream(w, strings.NewReader(strings.Repeat("x", 64<<20)), nil) // far more than the socket buffers hold
	}))
	defer srv.Close()
	c, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")) // and never read the answer
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the handler is still writing to a client that does not read")
	}
}

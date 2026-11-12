package tunnel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// What does the hub key cost? It is sent once, in the handshake of a connection (the Authorization header of the
// WebSocket upgrade); the messages that follow carry none. These benchmarks put a number on both: the cost of a
// request through an established tunnel (same with and without a required key) and the cost of connecting.
func benchTunnel(b *testing.B, keyed bool) (*Hub, string, func()) {
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) }))
	hub := NewHub(nil, nil)
	if keyed {
		hub.SetConnectAuth(func(r *http.Request) bool { return bearerToken(r) == "k-0123456789abcdef0123456789abcdef" })
	}
	mux := http.NewServeMux()
	mux.Handle("/fleet/tunnel/connect", hub.Handler())
	hs := httptest.NewServer(mux)
	return hub, hs.URL + "|" + be.URL, func() { hs.Close(); be.Close() }
}

func benchRequests(b *testing.B, keyed bool) {
	hub, urls, closeAll := benchTunnel(b, keyed)
	defer closeAll()
	hubURL, beURL, _ := strings.Cut(urls, "|")
	opts := Options{}
	if keyed {
		opts.APIKey = "k-0123456789abcdef0123456789abcdef"
	}
	cli, err := StartClientOpts(hubURL, beURL, NodeRegistration{NodeName: "n", AuthToken: "t", JoinedAt: time.Now()}, opts)
	if err != nil {
		b.Fatal(err)
	}
	defer cli.Close()
	deadline := time.Now().Add(3 * time.Second)
	for len(hub.ConnectedNodes()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec := httptest.NewRecorder()
		if err := hub.ForwardRequest("n", httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m"}`)), rec); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTunnelRequestOpenHub(b *testing.B)  { benchRequests(b, false) }
func BenchmarkTunnelRequestKeyedHub(b *testing.B) { benchRequests(b, true) }

func benchHandshake(b *testing.B, keyed bool) {
	_, urls, closeAll := benchTunnel(b, keyed)
	defer closeAll()
	hubURL, beURL, _ := strings.Cut(urls, "|")
	opts := Options{}
	if keyed {
		opts.APIKey = "k-0123456789abcdef0123456789abcdef"
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cli, err := StartClientOpts(hubURL, beURL, NodeRegistration{NodeName: "n", AuthToken: "t", JoinedAt: time.Now()}, opts)
		if err != nil {
			b.Fatal(err)
		}
		cli.Close()
	}
}

func BenchmarkTunnelConnectOpenHub(b *testing.B)  { benchHandshake(b, false) }
func BenchmarkTunnelConnectKeyedHub(b *testing.B) { benchHandshake(b, true) }

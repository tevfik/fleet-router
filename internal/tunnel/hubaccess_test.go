package tunnel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

func fastTiming(t *testing.T) {
	t.Helper()
	ds := []*dur{pingEvery, readTimeout, reconnectFirst, reconnectMax, stableAfter}
	old := make([]time.Duration, len(ds))
	for i, d := range ds {
		old[i] = d.get()
	}
	for i, v := range []time.Duration{20 * time.Millisecond, 150 * time.Millisecond, 20 * time.Millisecond, 80 * time.Millisecond, time.Hour} {
		ds[i].set(v)
	}
	t.Cleanup(func() {
		for i, d := range ds {
			d.set(old[i])
		}
	})
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The node verifies the hub's certificate by default: a self-signed hub is refused unless Insecure is set.
func TestClientVerifiesHubCertificate(t *testing.T) {
	hub := NewHub(nil, nil)
	mux := http.NewServeMux()
	mux.Handle("/fleet/tunnel/connect", hub.Handler())
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()
	reg := NodeRegistration{NodeName: "n1", JoinedAt: time.Now()}

	if cli, err := StartClientOpts(srv.URL, "http://127.0.0.1:1", reg, Options{}); err == nil {
		cli.Close()
		t.Fatal("a self-signed hub certificate must be refused by default")
	} else if !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("error should name the certificate: %v", err)
	}
	cli, err := StartClientOpts(srv.URL, "http://127.0.0.1:1", reg, Options{Insecure: true})
	if err != nil {
		t.Fatalf("Insecure must connect: %v", err)
	}
	defer cli.Close()
	waitJoined(t, hub, "n1")
}

// A hub with a connect check refuses a tunnel without the key (401, a clear error, Status.Rejected) and
// accepts it with the key; the join token in the registration is not what is checked.
func TestHubConnectAuth(t *testing.T) {
	fastTiming(t)
	hub := NewHub(nil, nil)
	hub.SetConnectAuth(func(r *http.Request) bool { return bearerToken(r) == "hubkey" })
	mux := http.NewServeMux()
	mux.Handle("/fleet/tunnel/connect", hub.Handler())
	srv := httptest.NewServer(mux)
	defer srv.Close()
	reg := NodeRegistration{NodeName: "n1", AuthToken: "jointok", JoinedAt: time.Now()}

	_, err := StartClientOpts(srv.URL, "http://127.0.0.1:1", reg, Options{})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("want a 401 error, got %v", err)
	}
	if _, err := StartClientOpts(srv.URL, "http://127.0.0.1:1", reg, Options{APIKey: "jointok"}); err == nil {
		t.Fatal("the join token must not pass as the API key")
	}
	cli, err := StartClientOpts(srv.URL, "http://127.0.0.1:1", reg, Options{APIKey: "hubkey"})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	waitJoined(t, hub, "n1")
	if len(hub.ConnectedNodes()) != 1 {
		t.Fatal("node not registered")
	}
}

// A second tunnel under a live node's name with another join token is refused (no takeover); the same token
// (the node reconnecting) replaces the old tunnel.
func TestNodeNameTakeoverRefused(t *testing.T) {
	hub, srv := newStreamHub(t)
	joinNode(t, hub, srv.URL, "w1", "tok-a")

	_, err := StartClient(srv.URL, "http://127.0.0.1:1", NodeRegistration{NodeName: "w1", AuthToken: "tok-b", JoinedAt: time.Now()})
	var rej *RejectedError
	if err == nil || !strings.Contains(err.Error(), "another join token") {
		t.Fatalf("takeover must be refused with a reason, got %v", err)
	}
	_ = rej
	nodes := hub.ConnectedNodes()
	if len(nodes) != 1 || nodes[0].AuthToken != "tok-a" {
		t.Fatalf("the original tunnel must stay: %+v", nodes)
	}
	cli2, err := StartClient(srv.URL, "http://127.0.0.1:1", NodeRegistration{NodeName: "w1", AuthToken: "tok-a", JoinedAt: time.Now()})
	if err != nil {
		t.Fatalf("same token = same node, must replace: %v", err)
	}
	cli2.Close()
}

// A hub that stops answering (half-open connection) is noticed by the read deadline, the node reconnects and
// Status says what happened. A silent peer is simulated by a server that accepts the registration and then ignores everything.
func TestClientDetectsDeadHubAndReports(t *testing.T) {
	fastTiming(t)
	silent := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {
		var m Message
		if websocket.JSON.Receive(ws, &m) != nil {
			return
		}
		_ = websocket.JSON.Send(ws, Message{Type: MsgPong})
		for { // read and never answer pings
			if websocket.JSON.Receive(ws, &m) != nil {
				return
			}
		}
	}))
	defer silent.Close()
	cli, err := StartClient(silent.URL, "http://127.0.0.1:1", NodeRegistration{NodeName: "n", JoinedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	waitFor(t, "the dead connection to be noticed", func() bool { return !cli.IsConnected() })
	st := cli.Status()
	if !strings.Contains(st.LastError, "no sign of life") || st.NextRetry.IsZero() {
		t.Fatalf("status should say why and when: %+v", st)
	}
	waitFor(t, "a reconnect", func() bool { return cli.IsConnected() })
}

// While the hub keeps refusing, the client keeps its status visible: last error, rejected, attempts, next retry.
func TestClientReportsRefusalWhileRetrying(t *testing.T) {
	fastTiming(t)
	hub := NewHub(nil, nil)
	allow := make(chan struct{})
	hub.SetConnectAuth(func(r *http.Request) bool {
		select {
		case <-allow:
			return true
		default:
			return r.URL.Query().Get("x") == "never"
		}
	})
	mux := http.NewServeMux()
	mux.Handle("/fleet/tunnel/connect", hub.Handler())
	srv := httptest.NewServer(mux)
	defer srv.Close()
	reg := NodeRegistration{NodeName: "n", JoinedAt: time.Now()}
	// start connected through a first hub-less run: allow once, connect, then close the allowance off
	close(allow)
	cli, err := StartClient(srv.URL, "http://127.0.0.1:1", reg)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	waitJoined(t, hub, "n")
	// now every new connection is refused: drop the tunnel from the hub side
	hub.SetConnectAuth(func(*http.Request) bool { return false })
	hub.mu.Lock()
	for _, nt := range hub.tunnels {
		_ = nt.ws.Close()
	}
	hub.mu.Unlock()
	waitFor(t, "a refused retry to show", func() bool {
		st := cli.Status()
		return !st.Connected && st.Rejected && st.Attempts >= 2 && strings.Contains(st.LastError, "401") && !st.NextRetry.IsZero()
	})
}

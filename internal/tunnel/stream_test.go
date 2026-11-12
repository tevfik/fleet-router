package tunnel

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

const testKey = "master-key"

// ── fixtures ────────────────────────────────────────────────────────────────────────────────────────

func newStreamHub(t *testing.T) (*Hub, *httptest.Server) {
	t.Helper()
	hub := NewHub(nil, nil)
	hub.SetStreamAuth(func(r *http.Request) bool { return bearerToken(r) == testKey })
	mux := http.NewServeMux()
	mux.Handle("/fleet/tunnel/connect", hub.Handler())
	mux.Handle("GET "+StreamDialPath, hub.StreamDialHandler())
	mux.Handle("GET "+StreamAcceptPath, hub.StreamAcceptHandler())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return hub, srv
}

func waitJoined(t *testing.T, hub *Hub, node string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, n := range hub.ConnectedNodes() {
			if n.NodeName == node {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("node %s never joined", node)
}

func joinNode(t *testing.T, hub *Hub, hubURL, name, token string, ports ...int) *Client {
	t.Helper()
	cli, err := StartClient(hubURL, "http://127.0.0.1:1", NodeRegistration{NodeName: name, AuthToken: token, StreamPorts: ports, JoinedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cli.Close)
	waitJoined(t, hub, name)
	return cli
}

// tcpServer runs handle for every connection on a loopback port; closed receives one value per
// connection when handle returns.
type tcpServer struct {
	port   int
	closed chan struct{}
}

func startTCP(t *testing.T, handle func(net.Conn)) *tcpServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	s := &tcpServer{port: ln.Addr().(*net.TCPAddr).Port, closed: make(chan struct{}, 1024)}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			setNoDelay(c)
			go func() {
				handle(c)
				c.Close()
				s.closed <- struct{}{}
			}()
		}
	}()
	return s
}

func echo(c net.Conn) { _, _ = io.Copy(c, c) }

// reqReply answers every 300 B request with a 6 KB reply made of the request repeated.
func reqReply(c net.Conn) {
	req := make([]byte, 300)
	reply := make([]byte, 6144)
	for {
		if _, err := io.ReadFull(c, req); err != nil {
			return
		}
		for i := range reply {
			reply[i] = req[i%len(req)]
		}
		if _, err := c.Write(reply); err != nil {
			return
		}
	}
}

// connectLocal runs a StreamConnector on a loopback listener and returns its address.
func connectLocal(t *testing.T, hubURL, node string, port int) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	sc := &StreamConnector{HubURL: hubURL, Node: node, Port: port, Key: testKey, Logf: t.Logf}
	done := make(chan struct{})
	go func() { _ = sc.Serve(ctx, ln); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return ln.Addr().String()
}

func dialTCP(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	setNoDelay(c)
	t.Cleanup(func() { c.Close() })
	return c
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

// roundTrip writes payload and reads the same amount back, concurrently (the payload may be larger
// than the socket buffers).
func roundTrip(c net.Conn, payload []byte) ([]byte, error) {
	werr := make(chan error, 1)
	go func() { _, err := c.Write(payload); werr <- err }()
	got := make([]byte, len(payload))
	_ = c.SetReadDeadline(time.Now().Add(20 * time.Second))
	_, rerr := io.ReadFull(c, got)
	_ = c.SetReadDeadline(time.Time{})
	if err := <-werr; err != nil {
		return nil, err
	}
	return got, rerr
}

func expectEOF(t *testing.T, c net.Conn, within time.Duration) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(within))
	_, err := c.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("expected the connection to be closed, read data")
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatalf("connection still open after %s", within)
	}
}

func waitClosed(t *testing.T, s *tcpServer, within time.Duration) {
	t.Helper()
	select {
	case <-s.closed:
	case <-time.After(within):
		t.Fatalf("the node-side connection was not closed within %s", within)
	}
}

func httpStatus(t *testing.T, rawURL, token string) int {
	t.Helper()
	req, _ := http.NewRequest("GET", rawURL, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// ── tests ───────────────────────────────────────────────────────────────────────────────────────────

func TestStream_RoundTripExact(t *testing.T) {
	hub, srv := newStreamHub(t)
	echoSrv := startTCP(t, echo)
	rr := startTCP(t, reqReply)
	joinNode(t, hub, srv.URL, "worker", "join-tok", echoSrv.port, rr.port)

	// 5 MB through the echo port
	c := dialTCP(t, connectLocal(t, srv.URL, "worker", echoSrv.port))
	big := randBytes(5 << 20)
	got, err := roundTrip(c, big)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, big) {
		t.Fatal("5 MB payload came back different")
	}
	// small messages of odd sizes on the same stream
	for i := 1; i < 200; i += 7 {
		p := randBytes(i)
		if got, err := roundTrip(c, p); err != nil || !bytes.Equal(got, p) {
			t.Fatalf("%d B round trip: %v", i, err)
		}
	}

	// many 300 B → 6 KB request/replies
	c2 := dialTCP(t, connectLocal(t, srv.URL, "worker", rr.port))
	reply := make([]byte, 6144)
	for i := 0; i < 500; i++ {
		req := randBytes(300)
		if _, err := c2.Write(req); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(c2, reply); err != nil {
			t.Fatal(err)
		}
		for j := range reply {
			if reply[j] != req[j%300] {
				t.Fatalf("round %d: reply byte %d differs", i, j)
			}
		}
	}
}

func TestStream_Concurrent(t *testing.T) {
	hub, srv := newStreamHub(t)
	echoSrv := startTCP(t, echo)
	joinNode(t, hub, srv.URL, "worker", "", echoSrv.port)
	addr := connectLocal(t, srv.URL, "worker", echoSrv.port)

	const n = 12
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, err := net.Dial("tcp", addr)
			if err != nil {
				errs <- err
				return
			}
			defer c.Close()
			for k := 0; k < 20; k++ {
				p := randBytes(1000 + i*4096 + k*13)
				got, err := roundTrip(c, p)
				if err != nil {
					errs <- err
					return
				}
				if !bytes.Equal(got, p) {
					errs <- fmt.Errorf("stream %d round %d: data differs", i, k)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestStream_ClosePropagation(t *testing.T) {
	hub, srv := newStreamHub(t)
	echoSrv := startTCP(t, echo)
	// closer replies "bye" and closes its side
	closer := startTCP(t, func(c net.Conn) {
		b := make([]byte, 3)
		if _, err := io.ReadFull(c, b); err == nil {
			_, _ = c.Write([]byte("bye"))
		}
	})
	joinNode(t, hub, srv.URL, "worker", "", echoSrv.port, closer.port)

	t.Run("master closes", func(t *testing.T) {
		c := dialTCP(t, connectLocal(t, srv.URL, "worker", echoSrv.port))
		if got, err := roundTrip(c, []byte("hello")); err != nil || string(got) != "hello" {
			t.Fatalf("round trip: %q %v", got, err)
		}
		c.Close()
		waitClosed(t, echoSrv, 3*time.Second)
	})
	t.Run("node closes", func(t *testing.T) {
		c := dialTCP(t, connectLocal(t, srv.URL, "worker", closer.port))
		if _, err := c.Write([]byte("hi!")); err != nil {
			t.Fatal(err)
		}
		b := make([]byte, 3)
		if _, err := io.ReadFull(c, b); err != nil || string(b) != "bye" {
			t.Fatalf("read: %q %v", b, err)
		}
		expectEOF(t, c, 3*time.Second)
	})
}

func TestStream_UndeclaredPortRefused(t *testing.T) {
	hub, srv := newStreamHub(t)
	echoSrv := startTCP(t, echo)
	other := startTCP(t, echo)
	joinNode(t, hub, srv.URL, "worker", "", echoSrv.port)

	// the hub refuses a port the node did not declare, before asking the node
	u := srv.URL + StreamDialPath + "?node=worker&port=" + strconv.Itoa(other.port)
	if code := httpStatus(t, u, testKey); code != http.StatusForbidden {
		t.Fatalf("undeclared port: status %d, want 403", code)
	}
	sc := &StreamConnector{HubURL: srv.URL, Node: "worker", Port: other.port, Key: testKey}
	if ws, err := sc.Dial(); err == nil {
		ws.Close()
		t.Fatal("dial to an undeclared port succeeded")
	}
	// unknown node
	if code := httpStatus(t, srv.URL+StreamDialPath+"?node=nobody&port=1", testKey); code != http.StatusNotFound {
		t.Fatalf("unknown node: status %d, want 404", code)
	}
}

// freePort returns a loopback port nothing listens on.
func freePort(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return p
}

// A declared port with nothing listening: the node's refusal reaches the master as 502, at once.
func TestStream_NodeRefusesWhenLocalPortIsDown(t *testing.T) {
	hub, srv := newStreamHub(t)
	port := freePort(t)
	joinNode(t, hub, srv.URL, "worker", "", port)
	u := srv.URL + StreamDialPath + "?node=worker&port=" + strconv.Itoa(port)
	start := time.Now()
	if code := httpStatus(t, u, testKey); code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", code)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("the refusal took %s: the hub waited for its timeout instead of the node's answer", d)
	}
}

// The node checks the port itself too: a hub (here a fake one) asking for an undeclared port is refused.
func TestStream_NodeRefusesUndeclaredPort(t *testing.T) {
	got := make(chan Message, 1)
	fake := httptest.NewServer(websocket.Server{
		Handshake: func(*websocket.Config, *http.Request) error { return nil },
		Handler: func(ws *websocket.Conn) {
			var reg Message
			if websocket.JSON.Receive(ws, &reg) != nil {
				return
			}
			_ = websocket.JSON.Send(ws, Message{Type: MsgPong})
			_ = websocket.JSON.Send(ws, Message{Type: MsgStreamOpen, ID: "st-x", Path: "127.0.0.1:22"})
			for {
				var m Message
				if websocket.JSON.Receive(ws, &m) != nil {
					return
				}
				if m.Type == MsgClose {
					got <- m
					return
				}
			}
		},
	})
	defer fake.Close()
	cli, err := StartClient(fake.URL, "http://127.0.0.1:1", NodeRegistration{NodeName: "n", StreamPorts: []int{50160}})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	select {
	case m := <-got:
		if m.ID != "st-x" || !strings.Contains(m.Error, "not declared") {
			t.Fatalf("unexpected refusal %+v", m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the node did not refuse")
	}
}

func TestStream_Auth(t *testing.T) {
	hub, srv := newStreamHub(t)
	echoSrv := startTCP(t, echo)
	joinNode(t, hub, srv.URL, "worker", "", echoSrv.port)
	u := srv.URL + StreamDialPath + "?node=worker&port=" + strconv.Itoa(echoSrv.port)
	if code := httpStatus(t, u, ""); code != http.StatusUnauthorized {
		t.Fatalf("no key: status %d, want 401", code)
	}
	if code := httpStatus(t, u, "wrong"); code != http.StatusUnauthorized {
		t.Fatalf("wrong key: status %d, want 401", code)
	}
	sc := &StreamConnector{HubURL: srv.URL, Node: "worker", Port: echoSrv.port, Key: "wrong"}
	if ws, err := sc.Dial(); err == nil {
		ws.Close()
		t.Fatal("dial with a wrong key succeeded")
	}
	// a hub without a stream auth check refuses everything
	bare := NewHub(nil, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", u, nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	bare.StreamDialHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("hub without auth: status %d, want 401", rec.Code)
	}
	// accept with an unknown id
	if code := httpStatus(t, srv.URL+StreamAcceptPath+"?id=st-nope", ""); code != http.StatusNotFound {
		t.Fatalf("unknown stream id: status %d, want 404", code)
	}
}

// fakeNode plays a node by hand over the protocol, to check what the hub demands of it.
type fakeNode struct {
	control *websocket.Conn
	opens   chan Message
}

func startFakeNode(t *testing.T, hub *Hub, hubURL string, reg NodeRegistration) *fakeNode {
	t.Helper()
	ws, err := dialWSOpts(formatWSURL(hubURL), "", true, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	if err := websocket.JSON.Send(ws, Message{Type: MsgRegister, NodeInfo: &reg}); err != nil {
		t.Fatal(err)
	}
	fn := &fakeNode{control: ws, opens: make(chan Message, 16)}
	go func() {
		for {
			var m Message
			if websocket.JSON.Receive(ws, &m) != nil {
				return
			}
			if m.Type == MsgStreamOpen {
				fn.opens <- m
			}
		}
	}()
	waitJoined(t, hub, reg.NodeName)
	return fn
}

func TestStream_AcceptNeedsJoinToken_And_NodeDropClosesStreams(t *testing.T) {
	hub, srv := newStreamHub(t)
	fn := startFakeNode(t, hub, srv.URL, NodeRegistration{NodeName: "fake", AuthToken: "join-secret", StreamPorts: []int{4242}})

	type dialRes struct {
		ws  *websocket.Conn
		err error
	}
	dialed := make(chan dialRes, 1)
	go func() {
		ws, err := (&StreamConnector{HubURL: srv.URL, Node: "fake", Port: 4242, Key: testKey}).Dial()
		dialed <- dialRes{ws, err}
	}()
	var open Message
	select {
	case open = <-fn.opens:
	case <-time.After(3 * time.Second):
		t.Fatal("the hub did not send stream_open")
	}
	if open.Path != "127.0.0.1:4242" || !strings.HasPrefix(open.ID, "st-") {
		t.Fatalf("stream_open %+v", open)
	}
	acceptURL := srv.URL + StreamAcceptPath + "?id=" + url.QueryEscape(open.ID)
	if code := httpStatus(t, acceptURL, ""); code != http.StatusUnauthorized {
		t.Fatalf("accept without the join token: status %d, want 401", code)
	}
	if code := httpStatus(t, acceptURL, "wrong"); code != http.StatusUnauthorized {
		t.Fatalf("accept with a wrong join token: status %d, want 401", code)
	}
	nodeWS, err := dialWSAuth(wsBase(srv.URL)+StreamAcceptPath+"?id="+url.QueryEscape(open.ID), "join-secret", false)
	if err != nil {
		t.Fatal(err)
	}
	defer nodeWS.Close()
	res := <-dialed
	if res.err != nil {
		t.Fatal(res.err)
	}
	master := res.ws
	defer master.Close()
	// the stream works both ways
	if _, err := master.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 4)
	if _, err := io.ReadFull(nodeWS, b); err != nil || string(b) != "ping" {
		t.Fatalf("node read %q %v", b, err)
	}
	if _, err := nodeWS.Write([]byte("pong")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(master, b); err != nil || string(b) != "pong" {
		t.Fatalf("master read %q %v", b, err)
	}
	// the id is single use
	if code := httpStatus(t, acceptURL, "join-secret"); code != http.StatusNotFound {
		t.Fatalf("reused stream id: status %d, want 404", code)
	}
	// the node's control tunnel drops: the hub closes its streams
	fn.control.Close()
	_ = master.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := master.Read(b); err == nil {
		t.Fatal("the stream survived its node's control tunnel")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("the stream is still open after the node's control tunnel dropped")
	}
}

func TestStream_NodeDisconnectClosesStreams(t *testing.T) {
	hub, srv := newStreamHub(t)
	echoSrv := startTCP(t, echo)
	cli := joinNode(t, hub, srv.URL, "worker", "", echoSrv.port)
	c := dialTCP(t, connectLocal(t, srv.URL, "worker", echoSrv.port))
	if got, err := roundTrip(c, []byte("x")); err != nil || string(got) != "x" {
		t.Fatalf("round trip %q %v", got, err)
	}
	cli.Close()
	expectEOF(t, c, 3*time.Second)
	waitClosed(t, echoSrv, 3*time.Second)
}

func TestStream_PerNodeLimit(t *testing.T) {
	hub, srv := newStreamHub(t)
	hub.MaxStreamsPerNode = 2
	echoSrv := startTCP(t, echo)
	joinNode(t, hub, srv.URL, "worker", "", echoSrv.port)
	sc := &StreamConnector{HubURL: srv.URL, Node: "worker", Port: echoSrv.port, Key: testKey}
	var open []*websocket.Conn
	for i := 0; i < 2; i++ {
		ws, err := sc.Dial()
		if err != nil {
			t.Fatal(err)
		}
		open = append(open, ws)
	}
	u := srv.URL + StreamDialPath + "?node=worker&port=" + strconv.Itoa(echoSrv.port)
	if code := httpStatus(t, u, testKey); code != http.StatusTooManyRequests {
		t.Fatalf("third stream: status %d, want 429", code)
	}
	for _, ws := range open {
		ws.Close()
	}
	// the slots come back once the streams are gone
	deadline := time.Now().Add(3 * time.Second)
	for {
		ws, err := sc.Dial()
		if err == nil {
			ws.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no slot after closing the streams: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Streams leave the HTTP tunnel alone, and a message type a node does not know is ignored.
func TestStream_HTTPTunnelUnchanged(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_, _ = w.Write(append([]byte("echo: "), body...))
	}))
	defer backend.Close()
	hub, srv := newStreamHub(t)
	echoSrv := startTCP(t, echo)
	cli, err := StartClient(srv.URL, backend.URL, NodeRegistration{NodeName: "worker", StreamPorts: []int{echoSrv.port}})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	waitJoined(t, hub, "worker")

	hub.mu.RLock()
	nt := hub.tunnels["worker"]
	hub.mu.RUnlock()
	nt.mu.Lock()
	_ = websocket.JSON.Send(nt.ws, Message{Type: "something_new", ID: "z"})
	nt.mu.Unlock()

	c := dialTCP(t, connectLocal(t, srv.URL, "worker", echoSrv.port))
	if got, err := roundTrip(c, []byte("abc")); err != nil || string(got) != "abc" {
		t.Fatalf("stream %q %v", got, err)
	}
	rec := httptest.NewRecorder()
	if err := hub.ForwardRequest("worker", httptest.NewRequest("POST", "/v1/x", strings.NewReader("hi")), rec); err != nil {
		t.Fatal(err)
	}
	if rec.Body.String() != "echo: hi" {
		t.Fatalf("HTTP through the tunnel: %q", rec.Body.String())
	}
}

// TestStream_Latency measures the median round trip of a 300 B request / 6 KB reply directly over
// loopback TCP and through the hub (master listener → hub → node → server). go test -run Latency -v
func TestStream_Latency(t *testing.T) {
	hub, srv := newStreamHub(t)
	rr := startTCP(t, reqReply)
	joinNode(t, hub, srv.URL, "worker", "", rr.port)
	rounds := 2000
	if testing.Short() {
		rounds = 200
	}
	measure := func(addr string) (median, p90 time.Duration) {
		c := dialTCP(t, addr)
		req := randBytes(300)
		reply := make([]byte, 6144)
		times := make([]time.Duration, 0, rounds)
		for i := 0; i < rounds+50; i++ {
			start := time.Now()
			if _, err := c.Write(req); err != nil {
				t.Fatal(err)
			}
			if _, err := io.ReadFull(c, reply); err != nil {
				t.Fatal(err)
			}
			if i >= 50 { // warm-up
				times = append(times, time.Since(start))
			}
		}
		sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
		return times[len(times)/2], times[len(times)*9/10]
	}
	dMed, dP90 := measure(net.JoinHostPort("127.0.0.1", strconv.Itoa(rr.port)))
	hMed, hP90 := measure(connectLocal(t, srv.URL, "worker", rr.port))
	t.Logf("300 B request / 6 KB reply, %d rounds over loopback:", rounds)
	t.Logf("  direct TCP:   median %v  p90 %v", dMed, dP90)
	t.Logf("  through hub:  median %v  p90 %v  (added %v at the median)", hMed, hP90, hMed-dMed)
	if hMed > 50*time.Millisecond {
		t.Errorf("median round trip through the hub is %v: something is buffering", hMed)
	}
}

// Even when declared, the daemon's own port is never streamed: a raw stream arrives from 127.0.0.1 and would pass the
// daemon's loopback admin check.
func TestStream_NodeRefusesDaemonPort(t *testing.T) {
	got := make(chan Message, 1)
	fake := httptest.NewServer(websocket.Server{
		Handshake: func(*websocket.Config, *http.Request) error { return nil },
		Handler: func(ws *websocket.Conn) {
			var reg Message
			if websocket.JSON.Receive(ws, &reg) != nil {
				return
			}
			_ = websocket.JSON.Send(ws, Message{Type: MsgPong})
			_ = websocket.JSON.Send(ws, Message{Type: MsgStreamOpen, ID: "st-d", Path: "127.0.0.1:4711"})
			for {
				var m Message
				if websocket.JSON.Receive(ws, &m) != nil {
					return
				}
				if m.Type == MsgClose {
					got <- m
					return
				}
			}
		},
	})
	defer fake.Close()
	cli, err := StartClient(fake.URL, "http://127.0.0.1:4711", NodeRegistration{NodeName: "n", StreamPorts: []int{4711}})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	select {
	case m := <-got:
		if m.ID != "st-d" || !strings.Contains(m.Error, "fleet daemon") {
			t.Fatalf("unexpected refusal %+v", m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the node did not refuse")
	}
}

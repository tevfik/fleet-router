package tunnel

// Raw TCP streams through the hub.
//
// A master that cannot reach a node directly opens one WebSocket per TCP connection to the hub
// (StreamDialPath). The hub asks the node over its control tunnel to open the stream (MsgStreamOpen);
// the node dials the declared local port, connects back to the hub (StreamAcceptPath) and the hub
// splices the two WebSockets. Only binary frames carry data, one frame per chunk read, written and
// flushed at once: there is no framing or JSON on the data path.

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

const (
	// The stream endpoints live under /fleet/tunnel/ so that a reverse proxy rule that already passes
	// the control tunnel through (e.g. a Traefik /fleet/tunnel/* route in front of the hub) covers them.
	StreamDialPath   = "/fleet/tunnel/stream/dial"
	StreamAcceptPath = "/fleet/tunnel/stream/accept"

	// DefaultMaxStreamsPerNode bounds the concurrent (pending + open) streams to one node.
	DefaultMaxStreamsPerNode = 16
	// DefaultStreamOpenTimeout is how long the hub waits for the node to connect back.
	DefaultStreamOpenTimeout = 10 * time.Second

	streamBufSize = 64 << 10
)

// wsOrigin is the Origin header of a WebSocket dial: the hub itself (ws→http, wss→https).
func wsOrigin(wsURL string) string {
	switch {
	case strings.HasPrefix(wsURL, "wss://"):
		wsURL = "https://" + strings.TrimPrefix(wsURL, "wss://")
	case strings.HasPrefix(wsURL, "ws://"):
		wsURL = "http://" + strings.TrimPrefix(wsURL, "ws://")
	}
	if u, err := url.Parse(wsURL); err == nil && u.Host != "" {
		return u.Scheme + "://" + u.Host
	}
	return "http://localhost"
}

// wsBase turns a hub URL (https://host, http://host:port, ws(s)://…, bare host) into ws(s)://host[:port].
func wsBase(raw string) string {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	switch {
	case strings.HasPrefix(raw, "https://"):
		raw = "wss://" + strings.TrimPrefix(raw, "https://")
	case strings.HasPrefix(raw, "http://"):
		raw = "ws://" + strings.TrimPrefix(raw, "http://")
	case strings.HasPrefix(raw, "ws://"), strings.HasPrefix(raw, "wss://"):
	default:
		raw = "wss://" + raw
	}
	return raw
}

// dialWSAuth opens a binary WebSocket with an optional bearer token. insecure skips TLS verification.
func dialWSAuth(wsURL, token string, insecure bool) (*websocket.Conn, error) {
	return dialWSOpts(wsURL, token, insecure, true)
}

// newStreamID returns an unguessable stream id: it is the only thing the node presents besides its
// join token when it connects back, so it must not be predictable.
func newStreamID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return "st-" + hex.EncodeToString(b)
}

func setNoDelay(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
}

// closeWS closes a WebSocket without hanging: x/net/websocket's Close takes the write lock to send the
// close frame, so a Write blocked on a peer that stopped reading would block it forever. The write
// deadline releases such a Write first.
func closeWS(ws *websocket.Conn) {
	_ = ws.SetWriteDeadline(time.Now().Add(time.Second))
	_ = ws.Close()
}

func closeEnd(c io.Closer) {
	if ws, ok := c.(*websocket.Conn); ok {
		closeWS(ws)
		return
	}
	_ = c.Close()
}

// copyChunks forwards every chunk as soon as it is read: no coalescing, no extra buffering. On a
// WebSocket destination each Write is one binary frame, flushed by the library before it returns.
func copyChunks(dst io.Writer, src io.Reader) {
	buf := make([]byte, streamBufSize)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// splice copies a↔b until either side ends, then closes both. It returns when both copies are done.
func splice(a, b io.ReadWriteCloser) {
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			closeEnd(a)
			closeEnd(b)
		})
	}
	done := make(chan struct{})
	go func() {
		copyChunks(a, b)
		closeBoth()
		close(done)
	}()
	copyChunks(b, a)
	closeBoth()
	<-done
}

// streamSet tracks the open ends of streams so that they can all be closed at once.
type streamSet struct {
	mu     sync.Mutex
	closed bool
	ends   map[io.Closer]struct{}
}

// add registers an end; it returns false (and closes it) when the set was already closed.
func (s *streamSet) add(c io.Closer) bool {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		closeEnd(c)
		return false
	}
	if s.ends == nil {
		s.ends = make(map[io.Closer]struct{})
	}
	s.ends[c] = struct{}{}
	s.mu.Unlock()
	return true
}

func (s *streamSet) remove(c io.Closer) {
	s.mu.Lock()
	delete(s.ends, c)
	s.mu.Unlock()
}

// closeAll closes every registered end and refuses later ones.
func (s *streamSet) closeAll() {
	s.mu.Lock()
	s.closed = true
	ends := s.ends
	s.ends = nil
	s.mu.Unlock()
	for c := range ends {
		closeEnd(c)
	}
}

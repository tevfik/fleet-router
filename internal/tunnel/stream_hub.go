package tunnel

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/websocket"
)

// pendingStream is a stream the hub asked a node to open and is waiting for.
type pendingStream struct {
	nt *nodeTunnel
	ch chan acceptResult // buffered (1): exactly one result is ever sent
}

// acceptResult is what the waiting dial gets: the node's WebSocket, or why there is none. The accept
// handler keeps ws open until done is closed.
type acceptResult struct {
	ws   *websocket.Conn
	done chan struct{}
	err  string
}

// SetStreamAuth sets the check for masters opening streams (StreamDialHandler). Without one every
// stream dial is refused.
func (h *Hub) SetStreamAuth(fn func(r *http.Request) bool) {
	h.smu.Lock()
	h.streamAuth = fn
	h.smu.Unlock()
}

func (h *Hub) maxStreams() int32 {
	if h.MaxStreamsPerNode > 0 {
		return int32(h.MaxStreamsPerNode)
	}
	return DefaultMaxStreamsPerNode
}

func (h *Hub) openTimeout() time.Duration {
	if h.StreamOpenTimeout > 0 {
		return h.StreamOpenTimeout
	}
	return DefaultStreamOpenTimeout
}

// claimStream removes a pending stream; the caller that removes it owns its result channel.
func (h *Hub) claimStream(id string) (*pendingStream, bool) {
	h.smu.Lock()
	defer h.smu.Unlock()
	ps, ok := h.pendingStreams[id]
	if ok {
		delete(h.pendingStreams, id)
	}
	return ps, ok
}

// failStream delivers a node's refusal of a pending stream; false if id is not a stream of nt.
func (h *Hub) failStream(nt *nodeTunnel, id, reason string) bool {
	if id == "" {
		return false
	}
	h.smu.Lock()
	ps, ok := h.pendingStreams[id]
	if ok && ps.nt == nt {
		delete(h.pendingStreams, id)
	}
	h.smu.Unlock()
	if !ok || ps.nt != nt {
		return false
	}
	if reason == "" {
		reason = "node refused the stream"
	}
	ps.ch <- acceptResult{err: reason}
	return true
}

func streamHTTPErr(w http.ResponseWriter, code int, msg string) {
	http.Error(w, msg, code)
}

// StreamDialHandler serves GET StreamDialPath?node=<node>&port=<port>: a master's WebSocket that becomes
// a raw TCP stream to 127.0.0.1:<port> on the node. Every refusal happens before the WebSocket upgrade,
// with a plain HTTP status: 401 auth, 400 bad query, 404 node not joined, 403 port not declared by the
// node, 429 too many streams to the node, 502 node refused or failed, 504 node did not connect back.
func (h *Hub) StreamDialHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.smu.Lock()
		auth := h.streamAuth
		h.smu.Unlock()
		if auth == nil || !auth(r) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			streamHTTPErr(w, http.StatusUnauthorized, "missing or wrong API key (Authorization: Bearer …)")
			return
		}
		q := r.URL.Query()
		node := strings.TrimSpace(q.Get("node"))
		port, err := strconv.Atoi(q.Get("port"))
		if node == "" || err != nil || port <= 0 || port > 65535 {
			streamHTTPErr(w, http.StatusBadRequest, "node and port (1-65535) are required")
			return
		}
		h.mu.RLock()
		nt, ok := h.tunnels[node]
		h.mu.RUnlock()
		if !ok || nt.dead.Load() {
			streamHTTPErr(w, http.StatusNotFound, fmt.Sprintf("node %q is not joined", node))
			return
		}
		if !nt.reg.AllowsStreamPort(port) {
			streamHTTPErr(w, http.StatusForbidden, fmt.Sprintf("node %q did not declare port %d as streamable (fleet join --stream-port %d)", node, port, port))
			return
		}
		if nt.streamCount.Add(1) > h.maxStreams() {
			nt.streamCount.Add(-1)
			streamHTTPErr(w, http.StatusTooManyRequests, fmt.Sprintf("node %q already has %d streams", node, h.maxStreams()))
			return
		}
		defer nt.streamCount.Add(-1)

		id := newStreamID()
		ps := &pendingStream{nt: nt, ch: make(chan acceptResult, 1)}
		h.smu.Lock()
		h.pendingStreams[id] = ps
		h.smu.Unlock()

		nt.mu.Lock()
		err = websocket.JSON.Send(nt.ws, Message{Type: MsgStreamOpen, ID: id, Path: fmt.Sprintf("127.0.0.1:%d", port)})
		nt.mu.Unlock()
		if err != nil {
			h.claimStream(id)
			streamHTTPErr(w, http.StatusBadGateway, "control tunnel to the node failed: "+err.Error())
			return
		}

		timer := time.NewTimer(h.openTimeout())
		defer timer.Stop()
		var res acceptResult
		select {
		case res = <-ps.ch:
		case <-timer.C:
			if _, mine := h.claimStream(id); mine {
				streamHTTPErr(w, http.StatusGatewayTimeout, fmt.Sprintf("node %q did not open the stream in %s", node, h.openTimeout()))
				return
			}
			res = <-ps.ch // the accept claimed it just now: it delivers at once
		case <-r.Context().Done():
			if _, mine := h.claimStream(id); mine {
				return
			}
			res = <-ps.ch
		case <-nt.gone:
			if _, mine := h.claimStream(id); mine {
				streamHTTPErr(w, http.StatusBadGateway, fmt.Sprintf("node %q left", node))
				return
			}
			res = <-ps.ch
		}
		if res.ws == nil {
			streamHTTPErr(w, http.StatusBadGateway, fmt.Sprintf("node %q: %s", node, res.err))
			return
		}
		nodeWS := res.ws
		defer func() {
			closeWS(nodeWS)
			close(res.done)
		}()
		if r.Context().Err() != nil {
			return
		}

		websocket.Server{
			Handshake: func(*websocket.Config, *http.Request) error { return nil },
			Handler: func(masterWS *websocket.Conn) {
				masterWS.PayloadType = websocket.BinaryFrame
				if !nt.streams.add(masterWS) {
					return
				}
				defer nt.streams.remove(masterWS)
				if !nt.streams.add(nodeWS) {
					closeWS(masterWS)
					return
				}
				defer nt.streams.remove(nodeWS)
				splice(masterWS, nodeWS)
			},
		}.ServeHTTP(w, r)
	})
}

// StreamAcceptHandler serves GET StreamAcceptPath?id=<id>: the node's WebSocket for a stream the hub
// asked it to open. It must carry the node's join token (Authorization: Bearer <token>; none if the
// node joined without one) and an id the hub sent to that node and still waits for.
func (h *Hub) StreamAcceptHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		tok := bearerToken(r)
		h.smu.Lock()
		ps, ok := h.pendingStreams[id]
		authOK := ok && subtle.ConstantTimeCompare([]byte(tok), []byte(ps.nt.reg.AuthToken)) == 1
		if authOK {
			delete(h.pendingStreams, id)
		}
		h.smu.Unlock()
		if !ok {
			streamHTTPErr(w, http.StatusNotFound, "no such pending stream")
			return
		}
		if !authOK {
			w.Header().Set("WWW-Authenticate", "Bearer")
			streamHTTPErr(w, http.StatusUnauthorized, "wrong join token for this stream")
			return
		}
		delivered := false
		websocket.Server{
			Handshake: func(*websocket.Config, *http.Request) error { return nil },
			Handler: func(nodeWS *websocket.Conn) {
				nodeWS.PayloadType = websocket.BinaryFrame
				done := make(chan struct{})
				delivered = true
				ps.ch <- acceptResult{ws: nodeWS, done: done}
				<-done // the dial side splices and closes; returning closes the connection
			},
		}.ServeHTTP(w, r)
		if !delivered {
			ps.ch <- acceptResult{err: "the node's stream handshake failed"}
		}
	})
}

// bearerToken returns the token of an "Authorization: Bearer <token>" header ("" when absent).
func bearerToken(r *http.Request) string {
	v := r.Header.Get("Authorization")
	if len(v) > 7 && strings.EqualFold(v[:7], "bearer ") {
		return strings.TrimSpace(v[7:])
	}
	return ""
}

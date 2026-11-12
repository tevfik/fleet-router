package tunnel

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/websocket"
)

// Hub manages active inbound WebSocket tunnels from edge nodes.
type Hub struct {
	mu      sync.RWMutex
	tunnels map[string]*nodeTunnel
	reqSeq  uint64
	onJoin  func(reg NodeRegistration)
	onDrop  func(nodeName string)

	// raw TCP streams (stream_hub.go)
	smu            sync.Mutex
	pendingStreams map[string]*pendingStream
	streamAuth     func(r *http.Request) bool
	connectAuth    func(r *http.Request) bool
	// MaxStreamsPerNode bounds concurrent streams to one node (default DefaultMaxStreamsPerNode).
	MaxStreamsPerNode int
	// StreamOpenTimeout bounds the wait for the node to connect back (default DefaultStreamOpenTimeout).
	StreamOpenTimeout time.Duration
}

type nodeTunnel struct {
	reg     NodeRegistration
	ws      *websocket.Conn
	mu      sync.Mutex
	pending map[string]chan Message
	dead    atomic.Bool

	gone        chan struct{} // closed when the control tunnel is gone
	streams     streamSet     // open stream ends, closed with the control tunnel
	streamCount atomic.Int32  // pending + open streams
}

// writeTimeout bounds one write to a node's control tunnel: a node that stops reading must not hold nt.mu (and with it
// every sender, ping answers included) forever.
const writeTimeout = 30 * time.Second

// sendLocked writes msg with a write deadline; the caller holds nt.mu.
func (nt *nodeTunnel) sendLocked(msg Message) error {
	_ = nt.ws.SetWriteDeadline(time.Now().Add(writeTimeout))
	err := websocket.JSON.Send(nt.ws, msg)
	_ = nt.ws.SetWriteDeadline(time.Time{})
	return err
}

// ErrResponseStarted: the node's answer had already been (partly) written to the client when the tunnel failed, so
// the request cannot be tried on another endpoint.
var ErrResponseStarted = errors.New("tunnel failed after the response had started")

// slowConsumerWait is how long the read loop waits for a request's reader before it gives that request up.
var slowConsumerWait = 5 * time.Second

// NewHub constructs a tunnel hub.
func NewHub(onJoin func(reg NodeRegistration), onDrop func(nodeName string)) *Hub {
	return &Hub{
		tunnels:        make(map[string]*nodeTunnel),
		pendingStreams: make(map[string]*pendingStream),
		onJoin:         onJoin,
		onDrop:         onDrop,
	}
}

// SetConnectAuth sets the check for nodes opening the control tunnel (Handler). Without one any caller may
// join (the default, for nodes that predate keys); with one a refused request gets 401 before any upgrade.
func (h *Hub) SetConnectAuth(fn func(r *http.Request) bool) {
	h.smu.Lock()
	h.connectAuth = fn
	h.smu.Unlock()
}

// Handler returns the HTTP Handler for GET /fleet/tunnel/connect.
func (h *Hub) Handler() http.Handler {
	s := websocket.Server{
		Handler: h.handleConn,
		Handshake: func(cfg *websocket.Config, req *http.Request) error {
			return nil
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.smu.Lock()
		auth := h.connectAuth
		h.smu.Unlock()
		if auth != nil && !auth(r) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "this hub requires an API key to join (fleet key set <key>; or hub_api_key in the config)", http.StatusUnauthorized)
			return
		}
		s.ServeHTTP(w, r)
	})
}

func (h *Hub) handleConn(ws *websocket.Conn) {
	defer ws.Close()

	// Wait for registration message
	_ = ws.SetReadDeadline(time.Now().Add(readTimeout.get()))
	var regMsg Message
	if err := websocket.JSON.Receive(ws, &regMsg); err != nil || regMsg.Type != MsgRegister || regMsg.NodeInfo == nil {
		_ = websocket.JSON.Send(ws, Message{Type: MsgClose, Error: "expected registration message"})
		return
	}

	nodeName := strings.TrimSpace(regMsg.NodeInfo.NodeName)
	if nodeName == "" {
		nodeName = "edge-node"
	}

	nt := &nodeTunnel{
		reg:     *regMsg.NodeInfo,
		ws:      ws,
		pending: make(map[string]chan Message),
		gone:    make(chan struct{}),
	}

	h.mu.Lock()
	if old, exists := h.tunnels[nodeName]; exists && !old.dead.Load() &&
		(old.reg.AuthToken == "" || subtle.ConstantTimeCompare([]byte(old.reg.AuthToken), []byte(nt.reg.AuthToken)) != 1) {
		// a live tunnel of this name with another join token: not a reconnect of the same node, do not let it be taken over.
		// Two empty tokens are not "the same token": a node that joined without one proves nothing, so its name cannot
		// be taken while its tunnel lives (a real reconnect succeeds once the old tunnel is noticed dead, <= readTimeout).
		h.mu.Unlock()
		_ = websocket.JSON.Send(ws, Message{Type: MsgClose, Error: fmt.Sprintf("node name %q is in use by a tunnel with another join token", nodeName)})
		return
	}
	if old, exists := h.tunnels[nodeName]; exists {
		old.dead.Store(true)
		_ = old.ws.Close()
	}
	h.tunnels[nodeName] = nt
	h.mu.Unlock()

	if h.onJoin != nil {
		h.onJoin(nt.reg)
	}

	_ = websocket.JSON.Send(ws, Message{Type: MsgPong, Body: []byte("registered")})

	// Read loop
	for {
		// nodes ping every 15 s: silence for readTimeout means the peer is gone (half-open TCP)
		_ = ws.SetReadDeadline(time.Now().Add(readTimeout.get()))
		var msg Message
		if err := websocket.JSON.Receive(ws, &msg); err != nil {
			break
		}
		if msg.Type == MsgPing {
			nt.mu.Lock()
			_ = nt.sendLocked(Message{Type: MsgPong})
			nt.mu.Unlock()
			continue
		}
		if msg.Type == MsgClose && h.failStream(nt, msg.ID, msg.Error) {
			continue // the node refused a stream
		}
		if msg.ID != "" {
			nt.mu.Lock()
			ch, ok := nt.pending[msg.ID]
			nt.mu.Unlock()
			if ok {
				select {
				case ch <- msg:
				case <-time.After(slowConsumerWait):
					// The client of this request reads too slowly: give THAT request up (its reader sees the closed
					// channel) instead of stalling every other request, and the pings, of this node.
					nt.mu.Lock()
					if nt.pending[msg.ID] == ch {
						delete(nt.pending, msg.ID)
						close(ch)
						_ = nt.sendLocked(Message{Type: MsgClose, ID: msg.ID})
					}
					nt.mu.Unlock()
				}
			}
		}
	}

	// Cleanup on disconnect: the node's streams go with its control tunnel
	nt.dead.Store(true)
	close(nt.gone)
	nt.streams.closeAll()
	h.mu.Lock()
	current := h.tunnels[nodeName] == nt
	if current {
		delete(h.tunnels, nodeName)
	}
	h.mu.Unlock()

	// Only when this was still the node's tunnel: a node that reconnected already replaced it, and dropping now
	// would remove the endpoints the new tunnel has just registered.
	if current && h.onDrop != nil {
		h.onDrop(nodeName)
	}
}

// ForwardRequest forwards an incoming HTTP request across the tunnel to the edge node and streams response.
func (h *Hub) ForwardRequest(nodeName string, r *http.Request, w http.ResponseWriter) error {
	h.mu.RLock()
	nt, ok := h.tunnels[nodeName]
	h.mu.RUnlock()
	if !ok || nt.dead.Load() {
		return fmt.Errorf("tunnel for node %q is offline", nodeName)
	}

	reqID := fmt.Sprintf("req-%d", atomic.AddUint64(&h.reqSeq, 1))
	body, _ := io.ReadAll(r.Body)

	hdr := make(map[string]string)
	for k, v := range r.Header {
		if len(v) > 0 {
			hdr[k] = v[0]
		}
	}

	respChan := make(chan Message, 64)
	nt.mu.Lock()
	nt.pending[reqID] = respChan
	nt.mu.Unlock()
	defer func() {
		nt.mu.Lock()
		delete(nt.pending, reqID)
		nt.mu.Unlock()
	}()

	// Send request over tunnel
	reqMsg := Message{
		Type:    MsgRequest,
		ID:      reqID,
		Method:  r.Method,
		Path:    r.URL.Path,
		Headers: hdr,
		Body:    body,
	}
	nt.mu.Lock()
	err := nt.sendLocked(reqMsg)
	nt.mu.Unlock()
	if err != nil {
		return fmt.Errorf("failed to send request over tunnel: %w", err)
	}

	flusher, _ := w.(http.Flusher)
	headerWritten := false
	// an answer that starts with 5xx or 429 is not passed on: its body is read (bounded) and the request returns an
	// error, so the caller tries another endpoint, as it does for a direct upstream
	failStatus := 0
	var failBody []byte

	// idle limit, not a total one: a slow model streaming a long answer (a 122B split at 7 tok/s) was cut at 10 minutes
	// while its tokens were still coming; a node that sends nothing for this long is given up
	idle := time.NewTimer(tunnelIdleLimit)
	defer idle.Stop()
	timeout := idle.C
	for {
		select {
		case <-r.Context().Done():
			// Client disconnected, inform edge
			nt.mu.Lock()
			_ = nt.sendLocked(Message{Type: MsgClose, ID: reqID})
			nt.mu.Unlock()
			return r.Context().Err()
		case <-nt.gone:
			// the node's tunnel dropped: nothing more will arrive (it used to wait out the 10 minutes)
			return tunnelLost(nodeName, headerWritten)
		case <-timeout:
			return tunnelLostErr("tunnel request timed out", headerWritten)
		case msg, ok := <-respChan:
			if !ok { // the read loop gave this request up (slow reader)
				return tunnelLostErr("tunnel request dropped: the client reads too slowly", headerWritten)
			}
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(tunnelIdleLimit)
			if msg.Type == MsgClose {
				// a close with an error is a failed forward on the node (its daemon is down, the path refused, the request
				// failed): it used to end the request as a success, an empty 200 with no failover
				if msg.Error != "" {
					return tunnelLostErr("node "+nodeName+": "+msg.Error, headerWritten)
				}
				if failStatus > 0 {
					return &UpstreamStatusError{Node: nodeName, Status: failStatus, Body: strings.TrimSpace(string(failBody))}
				}
				return nil
			}
			if failStatus > 0 {
				if len(failBody) < 4096 {
					failBody = append(failBody, msg.Body[:min(len(msg.Body), 4096-len(failBody))]...)
				}
				continue
			}
			if !headerWritten && (msg.StatusCode >= 500 || msg.StatusCode == http.StatusTooManyRequests) {
				failStatus = msg.StatusCode
				failBody = append(failBody, msg.Body[:min(len(msg.Body), 4096)]...)
				continue
			}
			if !headerWritten {
				for k, v := range msg.Headers {
					w.Header().Set(k, v)
				}
				if msg.StatusCode > 0 {
					w.WriteHeader(msg.StatusCode)
				} else {
					w.WriteHeader(http.StatusOK)
				}
				headerWritten = true
			}
			if len(msg.Body) > 0 {
				_, _ = w.Write(msg.Body)
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
	}
}

// UpstreamStatusError: the node's model server answered with a 5xx or 429 before anything was written to the client.
type UpstreamStatusError struct {
	Node   string
	Status int
	Body   string
}

func (e *UpstreamStatusError) Error() string {
	b := e.Body
	if len(b) > 300 {
		b = b[:300] + "…"
	}
	return fmt.Sprintf("node %s answered %d: %s", e.Node, e.Status, b)
}

// tunnelIdleLimit: how long a forwarded request may go without a message from the node.
var tunnelIdleLimit = 10 * time.Minute

// ConnectedNodes returns metadata of all currently connected tunnel nodes.
func (h *Hub) ConnectedNodes() []NodeRegistration {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var list []NodeRegistration
	for _, nt := range h.tunnels {
		if !nt.dead.Load() {
			list = append(list, nt.reg)
		}
	}
	return list
}

// ActiveStreams returns the open raw TCP streams (pending ones included) per node name; nodes without any are left out.
func (h *Hub) ActiveStreams() map[string]int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := map[string]int{}
	for name, nt := range h.tunnels {
		if n := int(nt.streamCount.Load()); n > 0 && !nt.dead.Load() {
			out[name] = n
		}
	}
	return out
}

func tunnelLost(node string, started bool) error {
	return tunnelLostErr(fmt.Sprintf("tunnel for node %q dropped", node), started)
}

func tunnelLostErr(msg string, started bool) error {
	if started {
		return fmt.Errorf("%s: %w", msg, ErrResponseStarted)
	}
	return errors.New(msg)
}

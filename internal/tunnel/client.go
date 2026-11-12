package tunnel

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/websocket"
)

// Options tune how a node connects to the hub.
type Options struct {
	// APIKey is sent as "Authorization: Bearer" on the control tunnel (a hub with hub_require_key refuses
	// a tunnel without a valid key). It is NOT the join token, which travels in the registration.
	APIKey string
	// Insecure skips verification of the hub's TLS certificate (self-signed hubs). Off by default.
	Insecure bool
}

// Timing of the tunnel (package vars: tests shorten them).
var (
	pingEvery      = newDur(15 * time.Second) // node -> hub keepalive
	readTimeout    = newDur(45 * time.Second) // no message (pong included) for this long: the connection is dead
	reconnectFirst = newDur(3 * time.Second)  // first wait before a reconnect
	reconnectMax   = newDur(time.Minute)      // reconnect waits double up to this
	stableAfter    = newDur(30 * time.Second) // a connection this old resets the wait
)

// dur is a duration tests can change while background goroutines of earlier tests still read it.
type dur struct{ v atomic.Int64 }

func newDur(d time.Duration) *dur {
	x := &dur{}
	x.v.Store(int64(d))
	return x
}
func (d *dur) get() time.Duration  { return time.Duration(d.v.Load()) }
func (d *dur) set(v time.Duration) { d.v.Store(int64(v)) }

// Status is what the daemon shows about the hub tunnel (fleet status, the menu's Hub screen).
type Status struct {
	Connected      bool      `json:"connected"`
	ConnectedSince time.Time `json:"connected_since,omitempty"`
	// LastError is why the last attempt (or the last connection) failed; empty after a successful connect.
	LastError   string    `json:"last_error,omitempty"`
	LastErrorAt time.Time `json:"last_error_at,omitempty"`
	// Rejected: the hub answered 401/403, retrying with the same token or key will not help until it is changed.
	Rejected  bool      `json:"rejected,omitempty"`
	Attempts  int       `json:"attempts,omitempty"` // failed attempts since the last connection
	NextRetry time.Time `json:"next_retry,omitempty"`
}

// Client maintains an outbound persistent tunnel to a Fleet Hub.
type Client struct {
	hubURL    string
	localAddr string
	reg       NodeRegistration
	opts      Options
	ctx       context.Context
	cancel    context.CancelFunc
	running   atomic.Bool
	connected atomic.Bool
	mu        sync.Mutex
	activeWS  *websocket.Conn
	st        Status
	streams   streamSet // open raw TCP streams (stream_node.go), closed by Close
}

// StartClient is StartClientOpts with default options (certificate verified, no API key).
func StartClient(hubURL, localAddr string, reg NodeRegistration) (*Client, error) {
	return StartClientOpts(hubURL, localAddr, reg, Options{})
}

// StartClientOpts initializes and runs the background tunnel connection with auto-reconnect.
func StartClientOpts(hubURL, localAddr string, reg NodeRegistration, opts Options) (*Client, error) {
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{
		hubURL:    hubURL,
		localAddr: localAddr,
		reg:       reg,
		opts:      opts,
		ctx:       ctx,
		cancel:    cancel,
	}
	c.running.Store(true)

	// Test first connect synchronously to surface immediate URL errors
	ws, err := c.connect()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed initial connect to %s: %w", formatWSURL(hubURL), err)
	}
	c.markConnected(ws)
	go c.runLoop(ws)
	return c, nil
}

// connect dials the hub and registers.
func (c *Client) connect() (*websocket.Conn, error) {
	ws, err := dialWSOpts(formatWSURL(c.hubURL), c.opts.APIKey, c.opts.Insecure, false)
	if err != nil {
		return nil, err
	}
	reg := c.reg
	if err := websocket.JSON.Send(ws, Message{Type: MsgRegister, NodeInfo: &reg}); err != nil {
		_ = ws.Close()
		return nil, fmt.Errorf("registration: %w", err)
	}
	// The hub answers a registration with a pong, or with a close carrying the reason it refused.
	_ = ws.SetReadDeadline(time.Now().Add(readTimeout.get()))
	var ack Message
	if err := websocket.JSON.Receive(ws, &ack); err != nil {
		_ = ws.Close()
		return nil, fmt.Errorf("no answer to the registration: %w", err)
	}
	if ack.Type == MsgClose {
		_ = ws.Close()
		return nil, &RejectedError{Reason: ack.Error}
	}
	return ws, nil
}

// RejectedError: the hub refused this node (bad credentials, name taken by another token).
type RejectedError struct{ Reason string }

func (e *RejectedError) Error() string { return "hub refused the node: " + e.Reason }

func (c *Client) markConnected(ws *websocket.Conn) {
	c.mu.Lock()
	c.activeWS = ws
	c.st = Status{Connected: true, ConnectedSince: time.Now()}
	c.mu.Unlock()
	c.connected.Store(true)
}

func (c *Client) markFailed(err error, next time.Time) {
	c.mu.Lock()
	c.st.Connected = false
	c.st.LastError = err.Error()
	c.st.LastErrorAt = time.Now()
	c.st.Attempts++
	c.st.NextRetry = next
	var rej *RejectedError
	var se *StatusError
	c.st.Rejected = errors.As(err, &rej) || (errors.As(err, &se) && se.IsAuth())
	c.mu.Unlock()
}

// Status returns the connection state, the last failure and the next retry.
func (c *Client) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.st
	st.Connected = c.connected.Load()
	return st
}

// IsConnected returns whether the tunnel is actively connected.
func (c *Client) IsConnected() bool {
	return c.connected.Load()
}

// Close disconnects and stops the tunnel client.
func (c *Client) Close() {
	c.running.Store(false)
	c.connected.Store(false)
	c.cancel()
	c.mu.Lock()
	if c.activeWS != nil {
		_ = c.activeWS.Close()
	}
	c.mu.Unlock()
	c.streams.closeAll()
}

func (c *Client) runLoop(initialWS *websocket.Conn) {
	ws := initialWS
	wait := reconnectFirst.get()
	for c.running.Load() {
		if ws == nil {
			var err error
			ws, err = c.connect()
			if err != nil {
				if !c.running.Load() {
					return
				}
				c.markFailed(err, time.Now().Add(wait))
				select {
				case <-c.ctx.Done():
					return
				case <-time.After(wait):
				}
				if wait *= 2; wait > reconnectMax.get() {
					wait = reconnectMax.get()
				}
				ws = nil
				continue
			}
			c.markConnected(ws)
		}

		since := time.Now()
		err := c.handleConnection(ws)
		c.mu.Lock()
		c.activeWS = nil
		c.mu.Unlock()
		_ = ws.Close()
		ws = nil
		if !c.running.Load() {
			c.connected.Store(false)
			return
		}
		if time.Since(since) >= stableAfter.get() {
			wait = reconnectFirst.get()
		}
		if err == nil {
			err = errors.New("connection closed by the hub")
		}
		c.markFailed(fmt.Errorf("tunnel dropped: %w", err), time.Now().Add(wait))
		c.mu.Lock()
		c.st.Attempts = 0 // a drop of an established connection is not a failed attempt
		c.mu.Unlock()
		// Only now: a reader that sees "not connected" must also find why and when (Status, IsConnected).
		c.connected.Store(false)
		select {
		case <-c.ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// handleConnection serves one connection until it fails; the error says why. A missing message for
// readTimeout (the hub answers every ping with a pong) counts as a dead connection.
func (c *Client) handleConnection(ws *websocket.Conn) error {
	stop := make(chan struct{})
	defer close(stop)
	// requests forwarded on this connection end with it: their answer can only go back over this websocket
	connCtx, connCancel := context.WithCancel(c.ctx)
	defer connCancel()
	go func() {
		ticker := time.NewTicker(pingEvery.get())
		defer ticker.Stop()
		for {
			select {
			case <-c.ctx.Done():
				return
			case <-stop:
				return
			case <-ticker.C:
				_ = ws.SetWriteDeadline(time.Now().Add(readTimeout.get()))
				if err := websocket.JSON.Send(ws, Message{Type: MsgPing}); err != nil {
					_ = ws.Close()
					return
				}
			}
		}
	}()

	for {
		_ = ws.SetReadDeadline(time.Now().Add(readTimeout.get()))
		var msg Message
		if err := websocket.JSON.Receive(ws, &msg); err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return fmt.Errorf("no sign of life from the hub for %s", readTimeout.get())
			}
			return err
		}
		switch msg.Type {
		case MsgRequest:
			go c.forwardToLocal(connCtx, ws, msg)
		case MsgStreamOpen:
			go c.openStream(ws, msg)
		}
	}
}

// okForwardTarget: what the hub may ask this node's daemon for. The hub is a different machine (possibly a compromised
// one) and its requests arrive from 127.0.0.1, so they would pass the daemon's loopback admin check: only the /v1 API,
// no userinfo trick ("@evil.host/..."), no query, no path climbing, only GET and POST.
func okForwardTarget(method, path string) error {
	if method != http.MethodGet && method != http.MethodPost {
		return fmt.Errorf("method %q is not forwarded", method)
	}
	if !strings.HasPrefix(path, "/v1/") || strings.ContainsAny(path, "@?#\\ \t\r\n") || strings.Contains(path, "..") {
		return fmt.Errorf("path %q is not forwarded (only /v1/...)", path)
	}
	return nil
}

const hubTunnelClientHeader = "X-Fleet-Client"
const hubTunnelClientValue = "hub-tunnel"

func (c *Client) forwardToLocal(ctx context.Context, ws *websocket.Conn, reqMsg Message) {
	if err := okForwardTarget(reqMsg.Method, reqMsg.Path); err != nil {
		_ = websocket.JSON.Send(ws, Message{Type: MsgClose, ID: reqMsg.ID, Error: err.Error()})
		return
	}
	localURL := fmt.Sprintf("%s%s", strings.TrimRight(c.localAddr, "/"), reqMsg.Path)
	httpReq, err := http.NewRequestWithContext(ctx, reqMsg.Method, localURL, bytes.NewReader(reqMsg.Body))
	if err != nil {
		_ = websocket.JSON.Send(ws, Message{Type: MsgClose, ID: reqMsg.ID, Error: err.Error()})
		return
	}
	for k, v := range reqMsg.Headers {
		httpReq.Header.Set(k, v)
	}
	httpReq.Header.Set(hubTunnelClientHeader, hubTunnelClientValue)

	client := &http.Client{Timeout: 0}
	resp, err := client.Do(httpReq)
	if err != nil {
		_ = websocket.JSON.Send(ws, Message{Type: MsgClose, ID: reqMsg.ID, Error: err.Error()})
		return
	}
	defer resp.Body.Close()

	respHdr := make(map[string]string)
	for k, v := range resp.Header {
		if len(v) > 0 {
			respHdr[k] = v[0]
		}
	}

	// Send initial response headers
	buf := make([]byte, 16384)
	n, readErr := resp.Body.Read(buf)
	firstMsg := Message{
		Type:       MsgResponse,
		ID:         reqMsg.ID,
		StatusCode: resp.StatusCode,
		Headers:    respHdr,
		Body:       buf[:n],
	}
	_ = websocket.JSON.Send(ws, firstMsg)

	// Stream rest of chunks
	for readErr == nil {
		n, readErr = resp.Body.Read(buf)
		if n > 0 {
			_ = websocket.JSON.Send(ws, Message{
				Type: MsgChunk,
				ID:   reqMsg.ID,
				Body: buf[:n],
			})
		}
	}

	// Signal end of stream
	_ = websocket.JSON.Send(ws, Message{Type: MsgClose, ID: reqMsg.ID})
}

func formatWSURL(raw string) string {
	return wsBase(raw) + "/fleet/tunnel/connect"
}

// StatusError: the hub refused the WebSocket handshake with an HTTP status and (the start of) a message.
type StatusError struct {
	Status int
	Msg    string
}

func (e *StatusError) Error() string {
	if e.Msg != "" {
		return fmt.Sprintf("hub answered %d: %s", e.Status, e.Msg)
	}
	return fmt.Sprintf("hub answered %d", e.Status)
}

// IsAuth: the key or token was refused (retrying the same one will not help).
func (e *StatusError) IsAuth() bool {
	return e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden
}

// dialWSOpts dials a WebSocket; the hub's certificate is verified unless insecure. A refused handshake
// is probed with a plain request so that the error says why (the library drops the status code).
func dialWSOpts(wsURL, token string, insecure, binary bool) (*websocket.Conn, error) {
	cfg, err := websocket.NewConfig(wsURL, wsOrigin(wsURL))
	if err != nil {
		return nil, err
	}
	if token != "" {
		cfg.Header = http.Header{"Authorization": {"Bearer " + token}}
	}
	if strings.HasPrefix(wsURL, "wss://") && insecure {
		cfg.TlsConfig = &tls.Config{InsecureSkipVerify: true}
	}
	cfg.Dialer = &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	ws, err := websocket.DialConfig(cfg)
	if err != nil {
		var de *websocket.DialError
		if errors.As(err, &de) && errors.Is(de.Err, websocket.ErrBadStatus) {
			return nil, probeRefusal(wsURL, token, insecure, err)
		}
		return nil, err
	}
	if binary {
		ws.PayloadType = websocket.BinaryFrame
	}
	return ws, nil
}

// probeRefusal asks the hub the same thing over plain HTTP to learn the status and message.
func probeRefusal(wsURL, token string, insecure bool, orig error) error {
	u := "http" + strings.TrimPrefix(wsURL, "ws")
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return orig
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	hc := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure}}}
	resp, err := hc.Do(req)
	if err != nil {
		return orig
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
	msg := strings.TrimSpace(string(b))
	return &StatusError{Status: resp.StatusCode, Msg: msg}
}

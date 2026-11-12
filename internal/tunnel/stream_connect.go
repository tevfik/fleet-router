package tunnel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

// StreamConnector is the master side of a raw TCP stream: every connection accepted on a local
// listener becomes one WebSocket to the hub, spliced to 127.0.0.1:Port on Node.
type StreamConnector struct {
	HubURL   string // https://hub.example.com, http://host:port, ws(s)://…
	Node     string // the joined node's name
	Port     int    // a port the node declared with fleet join --stream-port
	Key      string // API key for the hub (Authorization: Bearer)
	Insecure bool   // skip TLS verification of the hub
	// Logf, if set, reports each stream's start and end.
	Logf func(format string, args ...any)
}

func (sc *StreamConnector) logf(format string, args ...any) {
	if sc.Logf != nil {
		sc.Logf(format, args...)
	}
}

// DialURL is the hub URL of one stream.
func (sc *StreamConnector) DialURL() string {
	q := url.Values{"node": {sc.Node}, "port": {strconv.Itoa(sc.Port)}}
	return wsBase(sc.HubURL) + StreamDialPath + "?" + q.Encode()
}

// Dial retry (package vars: tests shorten them): a hub restarting (a container update behind Traefik) answers
// 502/503/504 or refuses connections for a few seconds; a new stream waits for it instead of failing at once.
var (
	dialRetries   = 4
	dialRetryWait = time.Second // doubles: 1 s, 2 s, 4 s
)

// transient: worth another try (the hub is not there for the moment), as opposed to a refusal that will not change.
func transient(err error) bool {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Status == http.StatusBadGateway && !strings.Contains(se.Msg, "node") || se.Status == http.StatusServiceUnavailable
	}
	var de *websocket.DialError
	return errors.As(err, &de) // connection refused/reset/timeout, TLS handshake cut: not an HTTP answer
}

// explain turns the hub's refusal into what to do about it.
func (sc *StreamConnector) explain(err error) error {
	var se *StatusError
	if !errors.As(err, &se) {
		return fmt.Errorf("cannot reach the hub %s: %w", sc.HubURL, err)
	}
	var hint string
	switch se.Status {
	case http.StatusUnauthorized:
		hint = "the hub does not accept the API key (fleet key show; --key)"
	case http.StatusNotFound:
		hint = fmt.Sprintf("node %s is not joined to the hub (is its fleet daemon up? fleet join)", sc.Node)
	case http.StatusForbidden:
		hint = fmt.Sprintf("node %s did not declare port %d (fleet join --stream-port %d on that node)", sc.Node, sc.Port, sc.Port)
	case http.StatusTooManyRequests:
		hint = fmt.Sprintf("too many streams to %s at once", sc.Node)
	case http.StatusBadGateway, http.StatusGatewayTimeout, http.StatusServiceUnavailable:
		hint = fmt.Sprintf("the hub is restarting, or %s could not open its side (is the worker on port %d running?)", sc.Node, sc.Port)
	}
	if hint == "" {
		return fmt.Errorf("stream to %s:%d: %w", sc.Node, sc.Port, err)
	}
	return fmt.Errorf("stream to %s:%d: %w: %s", sc.Node, sc.Port, err, hint)
}

// Dial opens one stream; reads and writes on the result are the raw bytes of the node's TCP connection.
func (sc *StreamConnector) Dial() (*websocket.Conn, error) {
	wait := dialRetryWait
	for attempt := 0; ; attempt++ {
		ws, err := dialWSAuth(sc.DialURL(), sc.Key, sc.Insecure)
		if err == nil {
			return ws, nil
		}
		if attempt >= dialRetries || !transient(err) {
			return nil, sc.explain(err)
		}
		sc.logf("stream to %s:%d: %v; hub not ready, retrying in %s", sc.Node, sc.Port, err, wait)
		time.Sleep(wait)
		wait *= 2
	}
}

// Serve accepts connections on ln until ctx ends or ln fails, and splices each through its own stream.
func (sc *StreamConnector) Serve(ctx context.Context, ln net.Listener) error {
	var open streamSet
	var wg sync.WaitGroup
	stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stop()
	defer func() {
		open.closeAll()
		wg.Wait()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		setNoDelay(conn)
		wg.Add(1)
		go func() {
			defer wg.Done()
			sc.handle(conn, &open)
		}()
	}
}

func (sc *StreamConnector) handle(conn net.Conn, open *streamSet) {
	peer := conn.RemoteAddr().String()
	ws, err := sc.Dial()
	if err != nil {
		sc.logf("stream from %s: %v", peer, err)
		_ = conn.Close()
		return
	}
	if !open.add(conn) {
		closeWS(ws)
		return
	}
	defer open.remove(conn)
	if !open.add(ws) {
		_ = conn.Close()
		return
	}
	defer open.remove(ws)
	sc.logf("stream from %s → %s:%d open", peer, sc.Node, sc.Port)
	splice(conn, ws)
	sc.logf("stream from %s closed", peer)
}

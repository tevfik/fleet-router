package tunnel

import (
	"fmt"
	"log"
	"net"
	"net/url"
	"strconv"
	"time"

	"golang.org/x/net/websocket"
)

// openStream handles MsgStreamOpen on a joined node: it dials the declared local port, connects back
// to the hub with the stream id and splices the two. Anything else is refused with a MsgClose.
func (c *Client) openStream(control *websocket.Conn, msg Message) {
	refuse := func(reason string) {
		_ = websocket.JSON.Send(control, Message{Type: MsgClose, ID: msg.ID, Error: reason})
	}
	host, portStr, err := net.SplitHostPort(msg.Path)
	port, perr := strconv.Atoi(portStr)
	if err != nil || perr != nil || host != "127.0.0.1" || msg.ID == "" {
		refuse(fmt.Sprintf("bad stream target %q", msg.Path))
		return
	}
	if !c.reg.AllowsStreamPort(port) {
		refuse(fmt.Sprintf("port %d is not declared as streamable on this node", port))
		return
	}
	// never the daemon itself: a raw stream arrives from 127.0.0.1 and would pass its loopback admin check
	// (configuration writes, keys, joins) — the hub reaches the daemon only through the filtered /v1 forwarding
	if port == c.localPort() {
		log.Printf("hub stream to 127.0.0.1:%d refused: that is this fleet daemon", port)
		refuse(fmt.Sprintf("port %d is the fleet daemon: not streamable", port))
		return
	}
	local, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 5*time.Second)
	if err != nil {
		refuse(err.Error())
		return
	}
	setNoDelay(local)
	acceptURL := wsBase(c.hubURL) + StreamAcceptPath + "?id=" + url.QueryEscape(msg.ID)
	// same certificate policy as the control tunnel
	ws, err := dialWSAuth(acceptURL, c.reg.AuthToken, c.opts.Insecure)
	if err != nil {
		_ = local.Close()
		refuse("connect back to the hub: " + err.Error())
		return
	}
	if !c.streams.add(local) {
		closeWS(ws)
		return
	}
	defer c.streams.remove(local)
	if !c.streams.add(ws) {
		_ = local.Close()
		return
	}
	defer c.streams.remove(ws)
	t0 := time.Now()
	log.Printf("hub stream %s → 127.0.0.1:%d opened", msg.ID, port)
	splice(local, ws)
	log.Printf("hub stream %s → 127.0.0.1:%d closed after %s", msg.ID, port, time.Since(t0).Round(time.Second))
}

// localPort: the port of the daemon this client forwards to (0 if unknown).
func (c *Client) localPort() int {
	u, err := url.Parse(c.localAddr)
	if err != nil {
		return 0
	}
	p, _ := strconv.Atoi(u.Port())
	return p
}

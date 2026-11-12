package server

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/mdns"
)

// Fleets on the same network find each other over mDNS / DNS-SD (service _fleet._tcp): each daemon
// announces its id, name, version and how it can be reached, and lists the fleets it hears. Nothing heard
// this way is added on its own — anyone on the network can announce anything — so a fleet seen here is
// a suggestion, with the `fleet peers add` command that would make it a peer.

// LANFleet is a fleet heard on the local network.
type LANFleet struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Version string    `json:"version,omitempty"`
	Host    string    `json:"host"`           // e.g. "victor.local"
	Addr    string    `json:"addr"`           // the address it was heard from
	Port    int       `json:"port"`           //
	Scope   string    `json:"scope"`          // "lan": its port is open on the network; "loopback": it listens on 127.0.0.1 only
	Key     bool      `json:"key,omitempty"`  // it wants an API key (api_key_env) from other machines
	Peer    string    `json:"peer,omitempty"` // the configured peer that is this fleet, if any
	Add     string    `json:"add,omitempty"`  // the command that adds it as a peer (open port only)
	Hint    string    `json:"hint,omitempty"` // why it cannot be added yet
	Seen    time.Time `json:"seen"`
}

// mdnsInfo is this fleet's announcement, built from the current configuration.
func (s *Server) mdnsInfo() mdns.Info {
	c := s.Config()
	if c.ID == "" {
		return mdns.Info{}
	}
	host, port := hostLabel(), 4711
	scope := "loopback"
	if h, p, err := net.SplitHostPort(c.Listen); err == nil {
		port, _ = strconv.Atoi(p)
		if ip := net.ParseIP(h); h == "" || (ip != nil && !ip.IsLoopback()) {
			scope = "lan"
		}
	}
	txt := map[string]string{"id": c.ID, "name": c.Name, "v": Version, "scope": scope}
	if c.APIKeyEnv != "" {
		txt["key"] = "1"
	}
	return mdns.Info{Instance: fmt.Sprintf("%s (%s)", c.Name, shortID(c.ID)), Host: host, Port: port, TXT: txt}
}

func shortID(id string) string {
	if len(id) > 6 {
		return id[:6]
	}
	return id
}

// Hostname is this machine's short name, as nodes and announcements use it.
func Hostname() string { return hostLabel() }

func hostLabel() string {
	h, _ := os.Hostname()
	return strings.ToLower(strings.Split(h, ".")[0])
}

// lanLoop runs the mDNS responder/browser while the daemon runs (unless mdns: false).
func (s *Server) lanLoop(ctx context.Context) {
	if !s.Config().MDNSOn() {
		return
	}
	n := mdns.New(s.mdnsInfo)
	s.mu.Lock()
	s.lan = n
	s.mu.Unlock()
	if err := n.Run(ctx); err != nil {
		log.Printf("mdns: local network discovery is off: %v", err)
		s.mu.Lock()
		s.lan = nil
		s.mu.Unlock()
	}
}

// LANFleets lists the other fleets heard on the local network.
func (s *Server) LANFleets() []LANFleet {
	s.mu.RLock()
	n := s.lan
	s.mu.RUnlock()
	if n == nil {
		return nil
	}
	self := s.selfID()
	byID := map[string]string{} // peer id → configured peer name
	for _, p := range s.PeerStatuses() {
		if p.PeerID != "" {
			byID[p.PeerID] = p.Name
		}
	}
	var out []LANFleet
	for _, e := range n.Entries() {
		id := e.TXT["id"]
		if id == "" || id == self {
			continue
		}
		f := LANFleet{ID: id, Name: e.TXT["name"], Version: e.TXT["v"], Host: e.Host, Port: e.Port,
			Scope: e.TXT["scope"], Key: e.TXT["key"] == "1", Peer: byID[id], Seen: e.Seen}
		if f.Name == "" {
			f.Name = e.Instance
		}
		if len(e.Addrs) > 0 {
			f.Addr = e.Addrs[0]
		}
		if f.Peer == "" {
			f.Add, f.Hint = addCommand(f)
		}
		out = append(out, f)
	}
	return out
}

// addCommand is the `fleet peers add` line for a fleet heard on the network — its URL, since it is on the
// same network. A fleet that listens on loopback only cannot be reached that way: the hint says what to change.
func addCommand(f LANFleet) (cmd, hint string) {
	if f.Scope != "lan" || f.Addr == "" {
		return "", fmt.Sprintf("listens on 127.0.0.1 only — on %s set listen: 0.0.0.0:%d (and api_key_env) to reach it from here", f.Host, f.Port)
	}
	cmd = fmt.Sprintf("fleet peers add %s http://%s/v1", config.Slug(f.Name), net.JoinHostPort(f.Addr, strconv.Itoa(f.Port)))
	if f.Key {
		cmd += " pair=<code>"
		return cmd, "it wants a key: run `fleet peers pair` on " + f.Host + " and put the code it shows in pair="
	}
	return cmd, ""
}

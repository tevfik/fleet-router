package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/hostinfo"
	"github.com/tevfik/fleet-router/internal/rtt"
	"github.com/tevfik/fleet-router/internal/store"
)

// What a machine is doing (CPU, RAM, GPUs, temperatures, disks) is read by the fleet running on it and
// asked for over GET /fleet/host. For a node of this configuration the request goes to the fleet on that
// machine: this one, the peer the node's models come from (which passes it on, like a model request), the
// machine's fleet port through the SSH connection its models already use (127.0.0.1:4711 is forwarded
// alongside them), or its address when it listens on the network.

// HostReport is one machine's readings and where they came from.
type HostReport struct {
	hostinfo.Snapshot
	Node    string    `json:"node"`
	Fleet   string    `json:"fleet,omitempty"` // the name of the fleet that read them
	Version string    `json:"version,omitempty"`
	Engine  string    `json:"engine,omitempty"` // the ring engine built into that fleet ("" from a fleet too old to say)
	Source  string    `json:"source"`           // "this machine", "ssh papa", "⇄ bravo", "lan 10.0.1.5"
	Err     string    `json:"err,omitempty"`
	At      time.Time `json:"at"`
	// Bench is the machine's own benchmark (AI score), Benching whether one is running there.
	Bench    *hostinfo.Bench `json:"bench,omitempty"`
	Benching bool            `json:"benching,omitempty"`
	// Probes are the capability probes its fleet ran on its own models, by model name.
	Probes map[string]ProbeResult `json:"probes,omitempty"`
	// Store is what the machine has of model files and in its worker cache (nil from a fleet too old to say).
	Store *store.Inventory `json:"store,omitempty"`
	// Draining: the machine has left the line (fleet leave): plans skip it, groups see it going.
	Draining bool `json:"draining,omitempty"`
	// Runs are the distributed runs its fleet planned, newest first (runs.go): every fleet can see them.
	Runs []Run `json:"runs,omitempty"`
	// RTT carries this node's measured round trips to other nodes, keyed "from>to" (e.g. "dino>victor": 1.2).
	RTT map[string]float64 `json:"rtt,omitempty"`
}

type hostCache struct {
	mu sync.Mutex
	m  map[string]HostReport
}

const hostFresh = 1500 * time.Millisecond

func (s *Server) localHost(node string) HostReport {
	c := s.Config()
	if node == "" {
		node = hostLabel()
	}
	r := HostReport{Snapshot: s.host.Snapshot(), Node: node, Fleet: c.Name, Version: Version, Engine: Engine, Source: "this machine", At: time.Now()}
	r.Bench, r.Benching = s.HWBench()
	inv := s.LocalStore()
	r.Store = &inv
	r.Draining = c.Drain
	r.Runs = s.runs.recent(maxSharedRuns)
	res := s.ProbeResults()
	for _, e := range c.Endpoints {
		if p, ok := res[e.Name]; ok && e.Peer == "" && s.isSelfNode(e.Node) {
			if r.Probes == nil {
				r.Probes = map[string]ProbeResult{}
			}
			r.Probes[e.Model] = p
		}
	}
	if cPath := config.RTTPath(); cPath != "" {
		cache := (&rtt.Cache{Path: cPath}).Load()
		if len(cache) > 0 {
			m := make(map[string]float64, len(cache))
			for k, smp := range cache {
				if smp.MS > 0 {
					m[k] = smp.MS
				}
			}
			if len(m) > 0 {
				r.RTT = m
			}
		}
	}
	return r
}

// isSelfNode: the node is this machine (by name, or because its models are reached locally).
func (s *Server) isSelfNode(node string) bool {
	c := s.Config()
	if strings.EqualFold(node, hostLabel()) || strings.EqualFold(node, c.Name) {
		return true
	}
	for _, e := range c.Endpoints {
		if e.Node == node && e.Peer == "" && via(e) == "local" {
			return true
		}
	}
	return false
}

// HostOf returns the readings of a node (path: the fleets the question already passed, against loops).
func (s *Server) HostOf(ctx context.Context, node string, path []string) HostReport {
	if node == "" || s.isSelfNode(node) {
		return s.localHost(node)
	}
	s.hosts.mu.Lock()
	if r, ok := s.hosts.m[node]; ok && time.Since(r.At) < hostFresh {
		s.hosts.mu.Unlock()
		return r
	}
	s.hosts.mu.Unlock()
	r := s.fetchHost(ctx, node, path)
	r.Node, r.At = node, time.Now()
	if len(path) > 0 { // the answer depends on the path (routes back along it are skipped): not one to reuse
		return r
	}
	s.hosts.mu.Lock()
	if s.hosts.m == nil {
		s.hosts.m = map[string]HostReport{}
	}
	s.hosts.m[node] = r
	s.hosts.mu.Unlock()
	return r
}

// nodeRoute is one way to reach the fleet on a node.
type nodeRoute struct {
	source, base string // base: http://host:port (no /v1)
	peer         *config.Peer
	ssh          string
	remote       string // ssh: the fleet's address on that machine
	key          string
}

// nodeRoutes lists the ways to reach the fleet on a node: through a peer, over SSH, at the address of its
// models, or heard on the local network.
func (s *Server) nodeRoutes(node string) []nodeRoute {
	if s.routes != nil { // a test says how to reach its machines
		return s.routes(node)
	}
	cfg := s.Config()
	var routes []nodeRoute
	seen := map[string]bool{}
	add := func(r nodeRoute) {
		k := r.source + r.base
		if !seen[k] {
			seen[k] = true
			routes = append(routes, r)
		}
	}
	for _, e := range cfg.Endpoints {
		if e.Node != node {
			continue
		}
		switch {
		case e.Peer != "":
			for i := range cfg.Peers {
				if p := cfg.Peers[i]; p.Name == e.Peer {
					add(nodeRoute{source: "⇄ " + p.Name, peer: &p})
				}
			}
		case e.SSH != "":
			add(nodeRoute{source: "ssh " + e.SSH, ssh: e.SSH, remote: e.FleetRemote()})
		default:
			if u, err := url.Parse(e.URL); err == nil && u.Hostname() != "" {
				add(nodeRoute{source: "url " + u.Hostname(), base: "http://" + net.JoinHostPort(u.Hostname(), e.FleetHostPort()), key: e.APIKey()})
			}
		}
	}
	for _, f := range s.LANFleets() { // a fleet heard on the network under this name
		if f.Scope == "lan" && f.Addr != "" && (strings.EqualFold(f.Name, node) || strings.EqualFold(strings.TrimSuffix(f.Host, ".local"), node)) {
			add(nodeRoute{source: "lan " + f.Addr, base: "http://" + net.JoinHostPort(f.Addr, fmt.Sprint(f.Port))})
		}
	}
	return routes
}

// openRoute makes a route usable: the address to call (empty: the tunnel is down), the key to send and, for a
// peer, the query that names the node; rel releases the tunnel. skip is true when the route would lead back the
// way the question came (path: the fleets it already passed).
func (s *Server) openRoute(ctx context.Context, r nodeRoute, node string, path []string) (base, key, q string, rel func(), skip bool) {
	base, key, rel = r.base, r.key, func() {}
	switch {
	case r.peer != nil:
		tr := r.peer.Transport()
		if contains(path, s.peerID(r.peer.Name)) || len(path) >= maxServeHops {
			return "", "", "", rel, true
		}
		var b string
		b, rel = s.baseURLUse(ctx, &tr)
		base, key, q = strings.TrimSuffix(b, "/v1"), tr.APIKey(), "?node="+url.QueryEscape(node)
	case r.ssh != "":
		var p int
		remote := r.remote
		if remote == "" {
			remote = config.DefaultListen
		}
		p, rel = s.tunnels.Use(ctx, r.ssh, remote)
		base = ""
		if p > 0 {
			base = fmt.Sprintf("http://127.0.0.1:%d", p)
		}
	}
	return base, key, q, rel, false
}

func (s *Server) fetchHost(ctx context.Context, node string, path []string) HostReport {
	routes := s.nodeRoutes(node)
	if len(routes) == 0 {
		return HostReport{Source: "—", Err: "no route to a fleet on " + node}
	}
	last := HostReport{Source: "—", Err: "every route to a fleet on " + node + " leads back the way the question came"}
	for _, r := range routes {
		base, key, q, rel, skip := s.openRoute(ctx, r, node, path)
		if skip {
			continue
		}
		if base == "" {
			rel()
			last = HostReport{Source: r.source, Err: "tunnel down"}
			continue
		}
		rep, err := s.getHost(ctx, base+"/fleet/host"+q, key, path)
		rel()
		rep.Source = r.source + rep.viaSuffix()
		if err == nil {
			return rep
		}
		last = HostReport{Source: r.source, Err: hostErr(err, node)}
	}
	return last
}

// viaSuffix: when the answer was itself passed on by the peer, say from where.
func (r HostReport) viaSuffix() string {
	if r.Source == "" || r.Source == "this machine" {
		return ""
	}
	return " → " + r.Source
}

var errOld = errors.New("old fleet")

func hostErr(err error, node string) string {
	switch {
	case errors.Is(err, errOld):
		return "the fleet on " + node + " is too old to report its machine — run fleet update there"
	case errors.Is(err, syscall.ECONNREFUSED), strings.Contains(err.Error(), "connection refused"), strings.Contains(err.Error(), "connection reset"), errors.Is(err, io.EOF):
		return "no fleet runs on " + node + " (at its fleet port, 4711 unless fleet_port says otherwise) — install it there to see the machine"
	}
	return short(err.Error())
}

func (s *Server) getHost(ctx context.Context, u, key string, path []string) (HostReport, error) {
	cx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(cx, "GET", u, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set(headerPath, strings.Join(append(append([]string{}, path...), s.selfID()), ","))
	resp, err := s.client.Do(req)
	if err != nil {
		return HostReport{}, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode == 404 || resp.StatusCode == 405 {
		return HostReport{}, errOld
	}
	if resp.StatusCode != 200 {
		return HostReport{}, fmt.Errorf("http %d %s", resp.StatusCode, short(strings.TrimSpace(string(b))))
	}
	var r HostReport
	if err := json.Unmarshal(b, &r); err != nil {
		return HostReport{}, err
	}
	if r.Err != "" {
		return r, errors.New(r.Err)
	}
	return r, nil
}

// peerID is the fleet id a configured peer reported (empty until it synced).
func (s *Server) peerID(name string) string {
	return s.peerStates()[name].id
}

func (s *Server) hostHandler(w http.ResponseWriter, r *http.Request) {
	path := parsePath(r.Header.Get(headerPath))
	if contains(path, s.selfID()) {
		httpErr(w, 508, "loop")
		return
	}
	writeJSON(w, s.HostOf(r.Context(), r.URL.Query().Get("node"), path))
}

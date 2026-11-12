package server

import (
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
)

// Nodes are the machines the endpoints run on. A node's ping is the fastest answer any of its models
// gave to the last health check (GET /models through the same tunnel or URL the requests take), so it is
// the round trip a request pays before the model starts working. Chat-probed endpoints do not count: their
// check includes generating a token. The last pingKeep samples are kept for the dashboard.

const pingKeep = 30

// NodeStatus summarises one machine.
type NodeStatus struct {
	Node    string      `json:"node"`
	Self    bool        `json:"self,omitempty"` // this machine (listed first, even without models)
	Via     string      `json:"via"`            // "local", "ssh <host>", "url <host>", "⇄ <peer>"
	Up      int         `json:"up"`
	Total   int         `json:"total"`
	Idle    bool        `json:"idle,omitempty"` // every tunnel to it is closed for being unused
	PingMS  int64       `json:"ping_ms"`        // the last sample (0: none yet)
	AvgMS   int64       `json:"avg_ms,omitempty"`
	MinMS   int64       `json:"min_ms,omitempty"`
	MaxMS   int64       `json:"max_ms,omitempty"`
	History []int64     `json:"history,omitempty"` // oldest first
	Caps    []string    `json:"caps,omitempty"`    // what its healthy models can do (probed where probed)
	BestTPS float64     `json:"best_tps,omitempty"`
	Best    string      `json:"best,omitempty"`   // the model with BestTPS
	Strain  *NodeStrain `json:"strain,omitempty"` // its machine's state, when read within the last minute
	// Probed is when its models' capabilities were last checked (fleet probe); zero: never.
	Probed time.Time `json:"probed,omitempty"`
}

type pingStore struct {
	mu   sync.Mutex
	hist map[string][]int64   // node → samples
	last map[string]time.Time // node → newest health check already sampled
}

// record takes one sample per node from the health checks that happened since the last call.
func (p *pingStore) record(cfg *config.Config, state func(string) *EpState) {
	best := map[string]int64{}
	newest := map[string]time.Time{}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.hist == nil {
		p.hist, p.last = map[string][]int64{}, map[string]time.Time{}
	}
	for _, e := range cfg.Endpoints {
		if e.Probe == "chat" {
			continue
		}
		st := state(e.Name)
		if st == nil || !st.OK || st.Latency <= 0 || !st.Checked.After(p.last[e.Node]) {
			continue
		}
		if b, ok := best[e.Node]; !ok || st.Latency < b {
			best[e.Node] = st.Latency
		}
		if st.Checked.After(newest[e.Node]) {
			newest[e.Node] = st.Checked
		}
	}
	for n, ms := range best {
		h := append(p.hist[n], ms)
		if len(h) > pingKeep {
			h = h[len(h)-pingKeep:]
		}
		p.hist[n], p.last[n] = h, newest[n]
	}
}

func (p *pingStore) get(node string) []int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int64{}, p.hist[node]...)
}

// via says how a node is reached.
func via(e config.Endpoint) string {
	switch {
	case e.Peer != "":
		return "⇄ " + e.Peer
	case e.SSH != "":
		return "ssh " + e.SSH
	}
	if u, err := url.Parse(e.URL); err == nil {
		h := u.Hostname()
		if h == "127.0.0.1" || h == "localhost" || h == "::1" {
			return "local"
		}
		return "url " + h
	}
	return "url"
}

// Nodes summarises every machine, in the order the endpoints list them.
func (s *Server) Nodes(eps []EpStatus) []NodeStatus {
	var order []string
	by := map[string]*NodeStatus{}
	caps := map[string]map[string]bool{}
	idle := map[string]bool{}
	for _, e := range eps {
		n := by[e.Node]
		if n == nil {
			n = &NodeStatus{Node: e.Node, Via: via(e.Endpoint)}
			by[e.Node], caps[e.Node], idle[e.Node] = n, map[string]bool{}, true
			order = append(order, e.Node)
		} else if v := via(e.Endpoint); v != n.Via && !strings.Contains(n.Via, v) {
			n.Via += ", " + v
		}
		n.Total++
		idle[e.Node] = idle[e.Node] && e.Idle
		if !e.OK {
			continue
		}
		n.Up++
		cs := effCaps(&e.Endpoint)
		if e.Probe != nil && e.Probe.Err == "" {
			cs = e.Probe.merged(cs)
		}
		for _, c := range cs {
			caps[e.Node][c] = true
		}
		if e.TPS > n.BestTPS {
			n.BestTPS, n.Best = e.TPS, e.Model
		}
		if e.Probe != nil && e.Probe.At.After(n.Probed) {
			n.Probed = e.Probe.At
		}
	}
	self := ""
	for _, name := range order {
		if s.isSelfNode(name) {
			self = name
			break
		}
	}
	if self == "" { // this machine runs no model: it is a node all the same
		self = hostLabel()
		by[self], caps[self], idle[self] = &NodeStatus{Node: self}, map[string]bool{}, false
		order = append(order, self)
	}
	by[self].Self, by[self].Via = true, "this device"
	sort.SliceStable(order, func(i, j int) bool { return order[i] == self && order[j] != self })
	out := make([]NodeStatus, 0, len(order))
	for _, name := range order {
		n := by[name]
		n.Idle = idle[name]
		if st, ok := s.NodeStrainOf(name); ok {
			n.Strain = &st
		}
		for _, c := range []string{"chat", "embed", "vision", "audio", "image_gen", "tools", "thinking"} {
			if caps[name][c] {
				n.Caps = append(n.Caps, c)
			}
		}
		if h := s.pings.get(name); len(h) > 0 {
			n.History, n.PingMS, n.MinMS, n.MaxMS = h, h[len(h)-1], h[0], h[0]
			var sum int64
			for _, v := range h {
				sum += v
				n.MinMS, n.MaxMS = min(n.MinMS, v), max(n.MaxMS, v)
			}
			n.AvgMS = sum / int64(len(h))
		}
		out = append(out, *n)
	}
	return out
}

// NodeEndpoints lists the endpoints of the given nodes (names that are endpoints are taken as they are).
func NodeEndpoints(cfg *config.Config, names []string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(n string) {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	for _, want := range names {
		if cfg.Endpoint(want) != nil {
			add(want)
			continue
		}
		for _, e := range cfg.Endpoints {
			if strings.EqualFold(e.Node, want) {
				add(e.Name)
			}
		}
	}
	sort.Strings(out)
	return out
}

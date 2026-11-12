package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
)

// Peers: fleets that can reach each other form a model network. Every model a peer can reach shows up
// here as an endpoint named by its node/model; requests are forwarded to the peer (which routes them on),
// and speed and usage are measured locally, so network latency counts like any other slowness.
//
// Discovery needs no registry: a fleet answers GET /fleet/hello on its (default) port 4711, and the model
// scan recognises that answer. Loops are prevented twice: exported models carry the ids of the fleets they
// passed through (a fleet never imports a path containing its own id), and forwarded requests carry
// X-Fleet-Path (a fleet never forwards to a peer already on the path).

const (
	headerPath    = "X-Fleet-Path"
	maxServeHops  = 3 // a request that already crossed this many fleets is only served locally
	peerSyncEvery = 15 * time.Second
)

// Hello is what a fleet says about itself, unauthenticated: just enough to be recognised.
type Hello struct {
	Fleet   bool   `json:"fleet"`
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// ExportModel is one model a fleet offers to its peers.
type ExportModel struct {
	Ref    string   `json:"ref"` // node/model
	Node   string   `json:"node"`
	Model  string   `json:"model"`
	Caps   []string `json:"caps,omitempty"`
	Ctx    int      `json:"ctx,omitempty"`
	Dim    int      `json:"dim,omitempty"`
	Tags   []string `json:"tags,omitempty"`
	Up     bool     `json:"up"`
	TPS    float64  `json:"tps,omitempty"`
	TTFTMS int64    `json:"ttft_ms,omitempty"`
	Path   []string `json:"path"` // ids of the fleets between the owner and the exporter, ending with the exporter
	// what the exporter knows about the model and its machine (v0.7+)
	Info   *ModelInfo  `json:"info,omitempty"`
	Strain *NodeStrain `json:"strain,omitempty"`
}

// PeerStatus is the state of one configured peer.
type PeerStatus struct {
	config.Peer
	PeerID  string    `json:"peer_id,omitempty"`
	Remote  string    `json:"remote_name,omitempty"` // the name the peer gives itself
	Version string    `json:"version,omitempty"`     // the fleet version the peer runs
	OK      bool      `json:"ok"`
	Err     string    `json:"err,omitempty"`
	Models  int       `json:"models"` // endpoints imported from it
	Checked time.Time `json:"checked"`
	RTTMS   int64     `json:"rtt_ms,omitempty"`
}

// ensureID gives this fleet a stable identity (kept in the configuration file).
func (s *Server) ensureID() {
	c := s.Config()
	if c.ID != "" {
		return
	}
	b := make([]byte, 6)
	rand.Read(b)
	c.ID = hex.EncodeToString(b)
	if c.Name == "" {
		h, _ := os.Hostname()
		c.Name = strings.Split(h, ".")[0]
	}
	if err := s.SetConfig(c); err != nil {
		log.Printf("could not save the fleet id: %v", err)
	}
}

func (s *Server) selfID() string   { return s.Config().ID }
func (s *Server) selfName() string { return s.Config().Name }

func (s *Server) hello(w http.ResponseWriter, _ *http.Request) {
	c := s.Config()
	writeJSON(w, Hello{Fleet: true, ID: c.ID, Name: c.Name, Version: Version})
}

// Version is set by main.
var Version = "dev"

// Engine describes the ring engine linked into this binary (set by main; "" when unknown).
var Engine = ""

// effCaps lists what an endpoint can do (explicit caps, else inferred).
func effCaps(e *config.Endpoint) []string {
	var caps []string
	for _, c := range []string{"chat", "embed", "vision", "audio", "image_gen", "tools", "thinking"} {
		if e.Has(c) {
			caps = append(caps, c)
		}
	}
	return caps
}

// exportModels lists every model this fleet can reach — its own and those of its peers — for a peer to import.
func (s *Server) exportModels() []ExportModel {
	cfg := s.Config()
	var out []ExportModel
	for i := range cfg.Endpoints {
		e := &cfg.Endpoints[i]
		m := ExportModel{Ref: e.Ref(), Node: e.Node, Model: e.Model, Caps: effCaps(e), Ctx: e.Ctx, Dim: e.Dim, Tags: e.Tags,
			Up: s.healthy(e.Name), TPS: e.TPS, Path: append(append([]string{}, e.Path...), cfg.ID)}
		if lv, ok := s.live.get(e.Name); ok {
			if lv.Samples > 0 {
				m.TPS = float64(int(lv.TPS*10+0.5)) / 10
			}
			m.TTFTMS = int64(lv.TTFTMS)
		}
		if mi, ok := s.warm.get(e.Name); ok {
			m.Info = &mi
		}
		if ns, ok := s.NodeStrainOf(e.Node); ok {
			m.Strain = &ns
		}
		out = append(out, m)
	}
	return out
}

func (s *Server) export(w http.ResponseWriter, _ *http.Request) {
	c := s.Config()
	writeJSON(w, map[string]any{"id": c.ID, "name": c.Name, "models": s.exportModels()})
}

// ---- importing ----

type peerRT struct {
	id, name, err string
	version       string
	ok            bool
	rtt           time.Duration
	checked       time.Time
}

func (s *Server) peerStates() map[string]peerRT {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]peerRT, len(s.peerRT))
	for k, v := range s.peerRT {
		out[k] = v
	}
	return out
}

func (s *Server) setPeerRT(name string, v peerRT) {
	s.mu.Lock()
	if s.peerRT == nil {
		s.peerRT = map[string]peerRT{}
	}
	s.peerRT[name] = v
	s.mu.Unlock()
}

// PeerStatuses reports every configured peer.
func (s *Server) PeerStatuses() []PeerStatus {
	cfg := s.Config()
	rt := s.peerStates()
	var out []PeerStatus
	for _, p := range cfg.Peers {
		st := PeerStatus{Peer: p}
		v := rt[p.Name]
		st.PeerID, st.Remote, st.OK, st.Err, st.Checked, st.RTTMS = v.id, v.name, v.ok, v.err, v.checked, v.rtt.Milliseconds()
		st.Version = v.version
		for _, e := range cfg.Endpoints {
			if e.Peer == p.Name {
				st.Models++
			}
		}
		out = append(out, st)
	}
	return out
}

func (s *Server) peerLoop(ctx context.Context) {
	t := time.NewTicker(peerSyncEvery)
	defer t.Stop()
	for {
		s.SyncPeers(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.peerKick:
		}
	}
}

// SyncPeers refreshes the imported models of every peer once.
func (s *Server) SyncPeers(ctx context.Context) {
	for _, p := range s.Config().Peers {
		s.syncPeer(ctx, p)
	}
}

func (s *Server) peerGet(ctx context.Context, p config.Peer, base, path string, v any) (time.Duration, error) {
	cx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(cx, "GET", base+path, nil)
	tr := p.Transport()
	if k := tr.APIKey(); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}
	t0 := time.Now()
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	rtt := time.Since(t0)
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return rtt, fmt.Errorf("reading the answer: %w", err)
	}
	if resp.StatusCode != 200 {
		return rtt, fmt.Errorf("http %d (is that a fleet? does it need api_key_env → key_env?)", resp.StatusCode)
	}
	return rtt, json.Unmarshal(b, v)
}

func (s *Server) syncPeer(ctx context.Context, p config.Peer) {
	rt := peerRT{checked: time.Now()}
	defer func() { s.setPeerRT(p.Name, rt) }()
	tr := p.Transport()
	b, relTunnel := s.baseURLUse(ctx, &tr) // a peer is synced regularly: its tunnel counts as used
	defer relTunnel()
	base := strings.TrimSuffix(b, "/v1")
	if base == "" {
		rt.err = "tunnel down"
		if m := s.tunnels.Err(tr.SSH, tr.Remote); m != "" {
			rt.err = m
		}
		s.markPeerDown(p.Name, rt.err)
		return
	}
	var h Hello
	if _, err := s.peerGet(ctx, p, base, "/fleet/hello", &h); err != nil || !h.Fleet {
		rt.err = "not a fleet"
		if err != nil {
			rt.err = short(err.Error())
		}
		s.markPeerDown(p.Name, rt.err)
		return
	}
	rt.id, rt.name, rt.version = h.ID, h.Name, h.Version
	self := s.Config()
	if h.ID == self.ID {
		rt.err = "this peer is this fleet itself"
		s.markPeerDown(p.Name, rt.err)
		return
	}
	var ex struct {
		Models []ExportModel `json:"models"`
	}
	rtt, err := s.peerGet(ctx, p, base, "/fleet/export", &ex)
	if err != nil {
		rt.err = short(err.Error())
		s.markPeerDown(p.Name, rt.err)
		return
	}
	rt.ok, rt.rtt = true, rtt
	s.reconcilePeer(p, h.ID, ex.Models, rtt)
}

// markPeerDown flags the endpoints imported from an unreachable peer as down (they are kept, so they
// come back by themselves when the peer does).
func (s *Server) markPeerDown(name, why string) {
	cfg := s.Config()
	s.mu.Lock()
	for _, e := range cfg.Endpoints {
		if e.Peer == name {
			st := s.state[e.Name]
			if st == nil {
				st = &EpState{}
			}
			cp := *st
			cp.OK, cp.Err, cp.Checked = false, "peer "+name+": "+why, time.Now()
			s.state[e.Name] = &cp
		}
	}
	s.mu.Unlock()
}

func peerEligible(p config.Peer, m ExportModel, ownID string) bool {
	if len(m.Path) == 0 || len(m.Path) > p.Hops() {
		return false
	}
	for _, id := range m.Path {
		if id == ownID { // our own model, coming back around
			return false
		}
	}
	probe := config.Endpoint{Node: m.Node, Model: m.Model, Tags: m.Tags, Caps: m.Caps}
	if len(p.Only) > 0 {
		hit := false
		for _, sel := range p.Only {
			hit = hit || probe.Matches(sel)
		}
		if !hit {
			return false
		}
	}
	for _, sel := range p.Skip {
		if probe.Matches(sel) {
			return false
		}
	}
	return true
}

func (s *Server) reconcilePeer(p config.Peer, peerID string, models []ExportModel, rtt time.Duration) {
	cfg := s.Config()
	tr := p.Transport()
	existing := map[string]config.Endpoint{} // ref → current imported endpoint
	taken := map[string]bool{}
	var rest []config.Endpoint
	var oldSet []config.Endpoint
	for _, e := range cfg.Endpoints {
		if e.Peer == p.Name {
			existing[e.Ref()] = e
			oldSet = append(oldSet, e)
		} else {
			rest = append(rest, e)
			taken[e.Name] = true
		}
	}
	var fresh []config.Endpoint
	seen := map[string]bool{}
	for _, m := range models {
		if !peerEligible(p, m, cfg.ID) {
			continue
		}
		ep := config.Endpoint{Node: m.Node, Model: m.Model, Peer: p.Name, Path: m.Path,
			URL: tr.URL, SSH: tr.SSH, Remote: tr.Remote, KeyEnv: tr.KeyEnv, Key: tr.Key,
			Caps: m.Caps, Ctx: m.Ctx, Dim: m.Dim, Tags: m.Tags, TPS: m.TPS}
		// a peer may offer the same node/model twice (e.g. a registered server and a run of the same file): requests to it
		// are addressed by node/model, so one endpoint covers both — two took the same kept name and the whole import
		// was refused ("duplicate endpoint name") every sync
		if seen[ep.Ref()] {
			continue
		}
		seen[ep.Ref()] = true
		if old, ok := existing[ep.Ref()]; ok {
			ep.Name, ep.Slots = old.Name, old.Slots // keep the identity (statistics) and what the user set
			if old.TPS > 0 {
				ep.TPS = old.TPS // a locally measured value (benchmark) beats the peer's report
			}
			if old.Ctx > 0 {
				ep.Ctx = old.Ctx
			}
		} else {
			base := config.Slug(p.Name + "-" + m.Node + "-" + m.Model)
			ep.Name = base
			for n := 2; taken[ep.Name]; n++ {
				ep.Name = fmt.Sprintf("%s-%d", base, n)
			}
		}
		taken[ep.Name] = true
		fresh = append(fresh, ep)
	}
	keep := map[string]bool{}
	for _, e := range fresh {
		keep[e.Name] = true
	}
	s.mu.Lock()
	for _, e := range oldSet { // forget the health of models the peer no longer offers
		if !keep[e.Name] {
			delete(s.state, e.Name)
		}
	}
	s.mu.Unlock()
	sort.SliceStable(fresh, func(i, j int) bool { return fresh[i].Ref() < fresh[j].Ref() })
	sort.SliceStable(oldSet, func(i, j int) bool { return oldSet[i].Ref() < oldSet[j].Ref() })
	if !reflect.DeepEqual(oldSet, fresh) && !(len(oldSet) == 0 && len(fresh) == 0) {
		cfg.Endpoints = append(rest, fresh...)
		if err := s.SetConfig(cfg); err != nil {
			log.Printf("peer %s: could not apply its models: %v", p.Name, err)
		}
	}
	// health comes from the peer's own report (cheaper than probing every model through it)
	byRef := map[string]ExportModel{}
	for _, m := range models {
		byRef[m.Ref] = m
	}
	s.mu.Lock()
	for _, e := range fresh {
		m := byRef[e.Ref()]
		if m.Info != nil {
			s.warm.set(e.Name, *m.Info)
		}
		if m.Strain != nil {
			ns := *m.Strain
			ns.Source = "⇄ " + p.Name
			s.strain.set(e.Node, ns, true)
		}
		st := &EpState{Checked: time.Now(), OK: m.Up, Latency: rtt.Milliseconds()}
		if !m.Up {
			st.Err = "down on " + p.Name
		}
		if old := s.state[e.Name]; old != nil {
			st.penalty = old.penalty
		}
		s.state[e.Name] = st
	}
	s.mu.Unlock()
}

// AddPeer registers a peer (replacing one with the same name) and syncs it right away.
func (s *Server) AddPeer(p config.Peer) error {
	cfg := s.Config()
	var ps []config.Peer
	for _, x := range cfg.Peers {
		if x.Name != p.Name {
			ps = append(ps, x)
		}
	}
	cfg.Peers = append(ps, p)
	if err := s.SetConfig(cfg); err != nil {
		return err
	}
	select {
	case s.peerKick <- struct{}{}:
	default:
	}
	return nil
}

// RemovePeer forgets a peer and the models imported from it.
func (s *Server) RemovePeer(name string) error {
	cfg := s.Config()
	var ps []config.Peer
	found := false
	for _, x := range cfg.Peers {
		if x.Name == name {
			found = true
		} else {
			ps = append(ps, x)
		}
	}
	if !found {
		return fmt.Errorf("no such peer: %s", name)
	}
	var eps []config.Endpoint
	for _, e := range cfg.Endpoints {
		if e.Peer != name {
			eps = append(eps, e)
		}
	}
	cfg.Peers, cfg.Endpoints = ps, eps
	return s.SetConfig(cfg)
}

// parsePath reads the X-Fleet-Path header (comma-separated fleet ids).
func parsePath(h string) []string {
	var out []string
	for _, p := range strings.Split(h, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// dropLoops removes peer endpoints that would send a request back to a fleet it already passed
// (or any peer, once it has crossed maxServeHops fleets).
func dropLoops(cands []config.Endpoint, path []string) []config.Endpoint {
	if len(path) == 0 {
		return cands
	}
	var out []config.Endpoint
	for _, e := range cands {
		if e.Peer != "" {
			// an imported endpoint always has a path; a hand-edited one without it cannot be checked, so it is never used
			if len(e.Path) == 0 || len(path) >= maxServeHops || contains(path, e.Path[len(e.Path)-1]) {
				continue
			}
		}
		out = append(out, e)
	}
	return out
}

func contains(l []string, x string) bool {
	for _, v := range l {
		if v == x {
			return true
		}
	}
	return false
}

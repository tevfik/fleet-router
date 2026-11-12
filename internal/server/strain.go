package server

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/hostinfo"
)

// Strain: whether a machine is struggling right now (memory stalls, a GPU slowing down for heat — see
// hostinfo.Strain; swap use alone is not a sign). Every 15 s this fleet reads its own machine and asks the
// fleets of the machines it reaches over SSH (through the forward that rides on the models' connection,
// only while that connection is open: strain never wakes an idle tunnel). Models behind a peer carry what
// the peer knows. Routing (route / applyStrategy → condition) puts a strained machine's models behind the
// healthy alternatives of their role, and loaded (warm) models ahead of cold ones where the role wants it.

const (
	strainEvery = 15 * time.Second
	strainFresh = time.Minute
)

// NodeStrain is one machine's strain and where it was read.
type NodeStrain struct {
	hostinfo.Strain
	Source string `json:"source"`
}

type strainStore struct {
	mu    sync.Mutex
	m     map[string]NodeStrain // node → read by this fleet (itself or the machine's fleet)
	peer  map[string]NodeStrain // node → reported by a peer
	local NodeStrain            // this machine, cached for the /fleet/strain answer
}

func (st *strainStore) set(node string, v NodeStrain, fromPeer bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.m == nil {
		st.m, st.peer = map[string]NodeStrain{}, map[string]NodeStrain{}
	}
	if fromPeer {
		st.peer[node] = v
	} else {
		st.m[node] = v
	}
}

// NodeStrainOf returns a node's strain if it was read within the last minute.
func (s *Server) NodeStrainOf(node string) (NodeStrain, bool) {
	st := &s.strain
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, m := range []map[string]NodeStrain{st.m, st.peer} {
		if v, ok := m[node]; ok && time.Since(v.At) < strainFresh {
			return v, true
		}
	}
	return NodeStrain{}, false
}

func (s *Server) strainedNode(node string) bool {
	v, ok := s.NodeStrainOf(node)
	return ok && v.Strained()
}

// selfNode is the name this machine's models carry (its hostname when it has none).
func (s *Server) selfNode() string {
	for _, e := range s.Config().Endpoints {
		if e.Peer == "" && s.isSelfNode(e.Node) {
			return e.Node
		}
	}
	return hostLabel()
}

// localStrain reads this machine (cached 5 s: /fleet/strain may be asked by several fleets).
func (s *Server) localStrain() NodeStrain {
	st := &s.strain
	st.mu.Lock()
	cur := st.local
	st.mu.Unlock()
	if time.Since(cur.At) < 5*time.Second {
		return cur
	}
	cur = NodeStrain{Strain: hostinfo.ReadStrain(), Source: "this machine"}
	st.mu.Lock()
	st.local = cur
	st.mu.Unlock()
	return cur
}

func (s *Server) strainHandler(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.localStrain())
}

func (s *Server) strainLoop(ctx context.Context) {
	t := time.NewTicker(strainEvery)
	defer t.Stop()
	for {
		s.readStrains(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// readStrains takes one round: this machine, and every SSH machine whose connection is open.
func (s *Server) readStrains(ctx context.Context) {
	s.strain.set(s.selfNode(), s.localStrain(), false)
	cfg := s.Config()
	seen := map[string]bool{}
	var wg sync.WaitGroup
	for _, e := range cfg.Endpoints {
		if e.Peer != "" || e.SSH == "" || seen[e.Node] || s.isSelfNode(e.Node) || s.tunnels.IsIdle(e.SSH) {
			continue
		}
		seen[e.Node] = true
		p := s.tunnels.Port(e.SSH, e.FleetRemote())
		if p == 0 {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			var v NodeStrain
			if err := s.getJSON(ctx, fmt.Sprintf("http://127.0.0.1:%d/fleet/strain", p), "", &v); err == nil && !v.At.IsZero() {
				v.Source = "ssh " + e.SSH
				s.strain.set(e.Node, v, false)
			}
		}()
	}
	wg.Wait()
}

// condition orders the healthy candidates by the machine's state: models on strained machines behind the
// others (when avoid_strain is on and an unstrained one exists), and — if the role wants it — loaded models
// ahead of cold ones. Unhealthy candidates stay at the end, in their order.
func (s *Server) condition(rc config.Role, cands []config.Endpoint) []config.Endpoint {
	if len(cands) < 2 {
		return cands
	}
	avoid, warm := s.Config().AvoidStrainOn(), rc.WarmFirst()
	if !avoid && !warm {
		return cands
	}
	var good, bad []config.Endpoint
	for _, e := range cands {
		if s.healthy(e.Name) {
			good = append(good, e)
		} else {
			bad = append(bad, e)
		}
	}
	if len(good) < 2 {
		return cands
	}
	key := func(e config.Endpoint) int {
		k := 0
		if avoid && s.strainedNode(e.Node) {
			k += 2
		}
		if warm && s.cold(e.Name) {
			k++
		}
		return k
	}
	sort.SliceStable(good, func(i, j int) bool { return key(good[i]) < key(good[j]) })
	return append(good, bad...)
}

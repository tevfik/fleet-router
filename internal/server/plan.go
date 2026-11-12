package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tevfik/fleet-router/internal/plan"
	"github.com/tevfik/fleet-router/internal/store"
)

// PlanRequest asks where a model would run best. The model's facts are read where the file is (the CLI
// reads the GGUF header) and sent here, where the machines' benchmarks and readings are.
type PlanRequest struct {
	Model    plan.Model         `json:"model"`
	Ctx      int                `json:"ctx,omitempty"`
	Np       int                `json:"np,omitempty"`
	Master   string             `json:"master,omitempty"`
	RTT      map[string]float64 `json:"rtt,omitempty"`       // "a>b": measured round trip in ms
	LinkGBps float64            `json:"link_gbps,omitempty"` // every machine's network throughput, when known
	FixedMS  float64            `json:"fixed_ms,omitempty"`
	RingMS   float64            `json:"ring_ms,omitempty"`
	Top      int                `json:"top,omitempty"`
	Balance  bool               `json:"balance,omitempty"` // shortest slowest stage instead of fastest machines first (see plan.Options.Balance)
	// KVBlocks: blocks that keep a per-token KV cache (hybrid models: fewer than all). BoundaryBytes: what
	// crosses a machine boundary per token and direction, when the runtime sends more than one hidden state.
	KVBlocks int `json:"kv_blocks,omitempty"`
	// KVFactor: KV cache data type size relative to f16 (--cache-type-k/-v); 0 means f16.
	KVFactor      float64 `json:"kv_factor,omitempty"`
	CacheTypeK    string  `json:"cache_type_k,omitempty"`
	CacheTypeV    string  `json:"cache_type_v,omitempty"`
	CPUMoE        bool    `json:"cpu_moe,omitempty"`
	MoECacheMiB   int     `json:"moe_cache_mib,omitempty"`
	BoundaryBytes int64   `json:"boundary_bytes,omitempty"`
	// Nodes limits the plan to these machines (empty: every machine); a profile's master and workers.
	Nodes []string `json:"nodes,omitempty"`
	// ExpertTier lets the plan keep MoE expert tensors of a worker's blocks in its host RAM (plan.WithExpertTier) when
	// the blocks do not fit the GPUs otherwise. A node's expert_gbps (nodes: section) is used as its rate when declared.
	ExpertTier bool `json:"expert_tier,omitempty"`
}

// PlanNodes are the machines a plan can use: each node's benchmark and current memory use, and its ping as
// the round trip from this fleet.
func (s *Server) planNodes(r *http.Request, linkGBps float64) (out []plan.Node, left []string) {
	return s.planNodesFor(r, PlanRequest{LinkGBps: linkGBps})
}

func (s *Server) planNodesFor(r *http.Request, req PlanRequest) (out []plan.Node, left []string) {
	linkGBps := req.LinkGBps
	want := map[string]bool{}
	for _, n := range req.Nodes {
		want[strings.ToLower(n)] = true
	}
	cfg := s.Config()
	for _, ns := range s.Status().Nodes {
		if len(want) > 0 && !want[strings.ToLower(ns.Node)] {
			continue
		}
		h := s.HostOf(r.Context(), ns.Node, nil)
		if h.Draining || (ns.Self && s.Draining()) {
			left = append(left, ns.Node) // it has left the line
			continue
		}
		n := plan.FromHost(ns.Node, h.Bench, h.Now)
		if req.ExpertTier {
			spec, _ := cfg.Node(ns.Node)
			n = plan.WithExpertTier(n, h.Bench, h.Now, spec.ExpertGBps)
		}
		if spec, ok := cfg.Node(ns.Node); ok && spec.WeightGBps > 0 {
			n.WeightBW = spec.WeightGBps
		}
		if !ns.Self {
			n.RTTms = float64(ns.PingMS)
		}
		n.LinkGBps = linkGBps
		out = append(out, n)
	}
	return out, left
}

// planInputs gathers what a plan (or the check of a hand-made split) needs: the machines with what each one holds of
// the file, and the options with the measured round trips.
// The layer editor asks again after every key; reading every machine over ssh each time takes seconds. With reuse it
// takes the figures of the plan the user is looking at (when it was made a few minutes ago for the same file and
// settings), which also keeps its numbers on the same basis as that plan's table, not on a later ping.
func (s *Server) planInputs(r *http.Request, req PlanRequest, reuse bool) (nodes []plan.Node, left []string, known *store.Manifest, o plan.Options) {
	key := fmt.Sprintf("%s|%d|%s|%v|%t", req.Model.Name, req.Model.FileBytes, req.Master, req.Nodes, req.ExpertTier)
	var rtts map[string]float64
	if snap, ok := s.planSnap.get(key, reuse); ok {
		nodes, left, known, rtts = snap.nodes, snap.left, snap.known, snap.rtts
	} else {
		nodes, left = s.planNodesFor(r, req)
		known = s.Files().Find(req.Model.FileBytes, req.Model.Name) // a registered file: what each machine holds is known
		if known != nil {
			nodes = s.holdings(r.Context(), known, nodes)
		}
		rtts = req.RTT
		if req.Master != "" {
			mHost := s.HostOf(r.Context(), req.Master, nil)
			if len(mHost.RTT) > 0 {
				merged := make(map[string]float64, len(mHost.RTT)+len(rtts))
				for k, v := range mHost.RTT {
					merged[k] = v
				}
				for k, v := range rtts {
					merged[k] = v
				}
				rtts = merged
			}
		}
		if !reuse {
			s.planSnap.put(key, planSnap{nodes: slices.Clone(nodes), left: left, known: known, rtts: rtts})
		}
	}
	nodes = slices.Clone(nodes) // the planner sorts and edits what it is given
	o = plan.Options{Ctx: req.Ctx, Np: req.Np, Master: req.Master, FixedMS: req.FixedMS, RingMS: req.RingMS, Top: req.Top, RTT: rtts, Balance: req.Balance,
		KVBlocks: req.KVBlocks, KVFactor: req.KVFactor, CacheTypeK: req.CacheTypeK, CacheTypeV: req.CacheTypeV, CPUMoE: req.CPUMoE, MoECacheMiB: req.MoECacheMiB, BoundaryBytes: req.BoundaryBytes, Known: known != nil}
	return
}

// planSnapshot keeps the inputs of the last plan for planSnapMaxAge.
type planSnapshot struct {
	mu   sync.Mutex
	key  string
	at   time.Time
	snap planSnap
}

type planSnap struct {
	nodes []plan.Node
	left  []string
	known *store.Manifest
	rtts  map[string]float64
}

const planSnapMaxAge = 10 * time.Minute

func (p *planSnapshot) get(key string, reuse bool) (planSnap, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !reuse || p.key != key || time.Since(p.at) > planSnapMaxAge {
		return planSnap{}, false
	}
	return p.snap, true
}

func (p *planSnapshot) put(key string, sn planSnap) {
	p.mu.Lock()
	p.key, p.at, p.snap = key, time.Now(), sn
	p.mu.Unlock()
}

func validModel(m *plan.Model) bool {
	return m.Layers > 0 && len(m.LayerBytes) == m.Layers && len(m.LayerRead) == m.Layers
}

func (s *Server) planHandler(w http.ResponseWriter, r *http.Request) {
	var req PlanRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&req); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	if !validModel(&req.Model) {
		httpErr(w, 400, "the model facts are incomplete (layers, layer_bytes, layer_read)")
		return
	}
	nodes, left, known, o := s.planInputs(r, req, false)
	res := plan.Plan(&req.Model, nodes, o)
	if known != nil {
		res.SHA256 = known.SHA256
	}
	for _, n := range left {
		res.Skipped = append(res.Skipped, n+": has left the line (fleet leave --back there)")
	}
	writeJSON(w, res)
}

// EvaluateRequest is a split placed by hand: the planning inputs plus how many blocks each machine holds.
type EvaluateRequest struct {
	PlanRequest
	Alloc     []plan.Alloc `json:"alloc"`
	OutMaster bool         `json:"out_master,omitempty"`
}

// planEvaluateHandler checks a hand-made split against the machines' memory and prices it like a candidate.
func (s *Server) planEvaluateHandler(w http.ResponseWriter, r *http.Request) {
	var req EvaluateRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&req); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	if !validModel(&req.Model) {
		httpErr(w, 400, "the model facts are incomplete (layers, layer_bytes, layer_read)")
		return
	}
	master := req.Master // the planning options keep the request's own master (a pinned one), as the plan table was made
	if master == "" && len(req.Alloc) > 0 {
		master = req.Alloc[0].Node
	}
	nodes, _, _, o := s.planInputs(r, req.PlanRequest, true)
	writeJSON(w, plan.Evaluate(&req.Model, nodes, o, master, req.Alloc, req.OutMaster))
}

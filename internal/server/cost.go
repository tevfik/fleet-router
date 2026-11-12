package server

import (
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"strconv"

	"github.com/tevfik/fleet-router/internal/config"
)

// Cost-based routing (strategy "cost"): every candidate gets the time this request is expected to take
// there, in seconds, and the cheapest one goes first. One unit for every signal, so nothing needs weights:
//
//	total = wait + load + prefill + decode
//	wait    the queue in front of the endpoint's slots (requests ahead × what one takes)
//	load    a model the runtime has not loaded yet: its size ÷ how fast weights come off the disk
//	prefill the prompt: tokens ÷ prompt speed (a long prompt makes a slow-prompt machine expensive)
//	decode  the reply: tokens ÷ speed, stretched by streams already running on the same machine and by strain
//
// Speeds are the live figures from real traffic, the benchmark's number until there are any. A model with no
// speed at all costs +Inf: it only gets a request when everything else is down, and it gets measured then.

const (
	defaultOutTokens = 300  // a typical reply when the request does not say
	loadGBps         = 1.5  // weights read into memory per second when a cold model loads (NVMe-class disk)
	unknownLoadSec   = 10.0 // a cold model whose size is unknown
	prefillPerDecode = 8.0  // prompt speed ÷ decode speed when no prompt speed was measured
	streamShare      = 0.5  // each stream running on the same machine slows another by about this much (batching shares the work)
	strainFactor     = 2.0  // decode on a machine that is struggling right now (memory stalls, thermal throttle)
	nominalSlots     = 8    // slots of an endpoint without a limit
)

// reqSize is what the request asks of a machine, in tokens.
type reqSize struct{ In, Out int }

// costIn is everything the cost of one candidate rests on.
type costIn struct {
	TPS        float64 // decode, tokens/s (0: unknown)
	PrefillTPS float64 // prompt, tokens/s (0: not measured)
	TTFT       float64 // seconds to a first token with nothing to load (network + start)
	Used       int     // requests running on the endpoint
	Waiting    int     // requests queued for it
	Cap        int     // its slots (0: no limit)
	NodeBusy   int     // requests running on the whole machine (all its endpoints)
	Cold       bool    // the runtime must load the model first
	Parked     bool    // the model is warm in host RAM; needs fast PCIe H2D restore rather than disk read
	PCIeGBps   float64 // measured PCIe bandwidth for waking a parked model (default: 6.3 GB/s)
	SizeGB     float64 // weights, 0: unknown
	Strained   bool    // the machine is struggling right now
}

// Cost is a candidate's expected time, split into its terms.
type Cost struct {
	Endpoint string   `json:"endpoint"`
	Node     string   `json:"node"`
	Model    string   `json:"model"`
	Wait     float64  `json:"wait"`
	Load     float64  `json:"load"`
	Prefill  float64  `json:"prefill"`
	Decode   float64  `json:"decode"`
	Total    float64  `json:"total"` // seconds; +Inf when there is no speed figure (null in JSON, see Unknown)
	Down     bool     `json:"down,omitempty"`
	Unknown  bool     `json:"unknown,omitempty"`
	Notes    []string `json:"notes,omitempty"` // what moved the number: cold, strained, queue, estimated speed
}

func computeCost(in costIn, sz reqSize) Cost {
	var c Cost
	if sz.Out <= 0 {
		sz.Out = defaultOutTokens
	}
	if in.TPS <= 0 {
		c.Unknown, c.Total = true, math.Inf(1)
		c.Notes = append(c.Notes, "no speed figure yet")
		return c
	}
	cp := in.Cap
	if cp == 0 {
		cp = nominalSlots
	}
	// wait: the queue ahead (and the slots all taken) in units of one request's own time
	own := in.TTFT + float64(sz.Out)/in.TPS
	if ahead := in.Used + in.Waiting + 1 - cp; ahead > 0 && in.Cap > 0 {
		c.Wait = float64(ahead) / float64(cp) * own
		c.Notes = append(c.Notes, "queue")
	}
	if in.Cold {
		c.Load = unknownLoadSec
		if in.SizeGB > 0 {
			c.Load = in.SizeGB / loadGBps
		}
		c.Notes = append(c.Notes, "cold")
	} else if in.Parked {
		// Parked in Host RAM: restores via PCIe H2D fast-path, bypassing disk read
		bw := in.PCIeGBps
		if bw <= 0 {
			bw = 6.3 // default PCIe H2D bandwidth
		}
		c.Load = 0.5 // base wake latency
		if in.SizeGB > 0 {
			c.Load += in.SizeGB / bw
		}
		c.Notes = append(c.Notes, "parked (RAM->VRAM)")
	}
	pf := in.PrefillTPS
	if pf <= 0 {
		pf = in.TPS * prefillPerDecode
	}
	c.Prefill = in.TTFT + float64(sz.In)/pf
	// others already running on this machine share its memory bandwidth with us
	others := in.NodeBusy
	if others > 0 {
		c.Notes = append(c.Notes, "busy machine")
	}
	f := 1 + streamShare*float64(others)
	if in.Strained {
		f *= strainFactor
		c.Notes = append(c.Notes, "strained")
	}
	c.Decode = float64(sz.Out) / in.TPS * f
	c.Total = c.Wait + c.Load + c.Prefill + c.Decode
	return c
}

// costOf gathers what is known about an endpoint and works its cost out.
func (s *Server) costOf(e config.Endpoint, sz reqSize) Cost {
	in := costIn{TPS: e.TPS, TTFT: 0.2}
	if lv, ok := s.live.get(e.Name); ok {
		if lv.Samples > 0 {
			in.TPS = lv.TPS
		}
		in.PrefillTPS = lv.PrefillTPS
	}
	in.Used, in.Waiting, in.Cap = s.lim("ep:" + e.Name).stats()
	for _, o := range s.Config().Endpoints { // streams running on this machine, whichever endpoint they use
		if o.Node == e.Node {
			u, _, _ := s.lim("ep:" + o.Name).stats()
			in.NodeBusy += u
		}
	}
	if in.Cap > 0 && in.Used >= in.Cap {
		in.NodeBusy-- // all slots taken: we wait for one to free up and take its place
	}
	if m, ok := s.warm.get(e.Name); ok {
		in.Cold = m.Loaded != nil && !*m.Loaded
		in.SizeGB = float64(m.Size) / (1 << 30)
	}
	in.Strained = s.Config().AvoidStrainOn() && s.strainedNode(e.Node)
	c := computeCost(in, sz)
	c.Endpoint, c.Node, c.Model = e.Name, e.Node, e.Model
	if in.TPS > 0 && e.TPS > 0 {
		if lv, ok := s.live.get(e.Name); !ok || lv.Samples == 0 {
			c.Notes = append(c.Notes, "benchmark speed")
		}
	}
	return c
}

// byCost orders endpoints cheapest first (no speed figure last, ties keep their order).
func (s *Server) byCost(cands []config.Endpoint, sz reqSize) ([]config.Endpoint, []Cost) {
	cs := make([]Cost, len(cands))
	idx := make([]int, len(cands))
	for i, e := range cands {
		cs[i], idx[i] = s.costOf(e, sz), i
	}
	sort.SliceStable(idx, func(a, b int) bool { return cs[idx[a]].Total < cs[idx[b]].Total })
	out := make([]config.Endpoint, len(cands))
	oc := make([]Cost, len(cands))
	for k, i := range idx {
		out[k], oc[k] = cands[i], cs[i]
	}
	return out, oc
}

// MarshalJSON writes +Inf (no speed figure) as null: JSON has no infinity.
func (c Cost) MarshalJSON() ([]byte, error) {
	type plain Cost
	if math.IsInf(c.Total, 0) {
		return json.Marshal(struct {
			plain
			Total *float64 `json:"total"`
		}{plain: plain(c)})
	}
	return json.Marshal(plain(c))
}

// RouteExplain is the answer of GET /fleet/route: where a request would go and why.
type RouteExplain struct {
	Model    string `json:"model"`
	Role     string `json:"role,omitempty"`
	Strategy string `json:"strategy"`
	In       int    `json:"in_tokens"`
	Out      int    `json:"out_tokens"`
	Order    []Cost `json:"order"` // in the order the request would try them
}

// ExplainRoute works out the order a request for model (a role or a model name) would take, with each
// candidate's cost terms. Nothing is sent and no slot is taken.
func (s *Server) ExplainRoute(model string, sz reqSize) RouteExplain {
	cands, role, rc := s.route(model)
	if sz.Out <= 0 {
		sz.Out = defaultOutTokens
	}
	if rc.Strategy != "round-robin" { // rotating it would move the real rotation
		cands = s.applyStrategySized(role, rc, cands, sz)
	}
	r := RouteExplain{Model: model, Role: role, Strategy: strategyOf(rc), In: sz.In, Out: sz.Out}
	for _, e := range cands {
		c := s.costOf(e, sz)
		if !s.healthy(e.Name) {
			c.Down = true
			c.Notes = append(c.Notes, "down")
		}
		r.Order = append(r.Order, c)
	}
	return r
}

func strategyOf(rc config.Role) string {
	if rc.Strategy == "" {
		return "failover"
	}
	return rc.Strategy
}

func (s *Server) routeHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	model := q.Get("model")
	if model == "" {
		httpErr(w, 400, "model= (a role or a model name) is required")
		return
	}
	in, _ := strconv.Atoi(q.Get("in"))
	out, _ := strconv.Atoi(q.Get("out"))
	writeJSON(w, s.ExplainRoute(model, reqSize{In: in, Out: out}))
}

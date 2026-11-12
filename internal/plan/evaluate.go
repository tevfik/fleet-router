package plan

import (
	"fmt"
	"math"
	"strings"
)

// Alloc is one machine's share of a split that was placed by hand: how many blocks it holds.
type Alloc struct {
	Node   string `json:"node"`
	Layers int    `json:"layers"`
}

// PartCheck is what one machine of a hand-made split needs, against what it has.
type PartCheck struct {
	Node   string  `json:"node"`
	Layers int     `json:"layers"`
	Master bool    `json:"master,omitempty"`
	Output bool    `json:"output,omitempty"` // holds the output projection
	NeedGB float64 `json:"need_gb"`          // its blocks with their KV cache, plus the embedding table (master) / output projection
	CapGB  float64 `json:"cap_gb"`           // fast memory free for the model right now
	Fits   bool    `json:"fits"`
}

// Machine is a machine the planner knows, with what it has free and why it cannot take part (empty: it can).
type Machine struct {
	Node  string  `json:"node"`
	CapGB float64 `json:"cap_gb"`
	Why   string  `json:"why,omitempty"`
}

// Evaluation is the answer of Evaluate: the hand-made split checked against the machines' memory and priced with the
// same cost model as the planner's own candidates.
type Evaluation struct {
	Total    int         `json:"total"`    // blocks the model has
	Assigned int         `json:"assigned"` // blocks the split places
	LayerGB  float64     `json:"layer_gb"` // one block with its KV cache
	Parts    []PartCheck `json:"parts"`    // master first, then the workers in ring order
	Machines []Machine   `json:"machines"` // every machine the planner knows (to add one)
	// Candidate is the split priced; nil when it cannot be (a machine without memory figures, no worker). Its blocks may
	// still be too many or too few: OK says whether it can be deployed as it stands.
	Candidate *Candidate `json:"candidate,omitempty"`
	OK        bool       `json:"ok"`
	Problems  []string   `json:"problems,omitempty"`
}

// Evaluate checks and prices a split whose blocks were placed by hand. The master comes first (it holds the first
// blocks and the embedding table), the workers follow in the order given (the output projection sits on the last one
// unless outMaster). A worker without blocks is a problem, not silently dropped: it would pay its round trip for nothing.
func Evaluate(m *Model, nodes []Node, o Options, master string, alloc []Alloc, outMaster bool) Evaluation {
	if o.Ctx <= 0 {
		o.Ctx = min(m.CtxMax, 8192)
		if o.Ctx <= 0 {
			o.Ctx = 8192
		}
	}
	if o.Np <= 0 {
		o.Np = 1
	}
	if o.FixedMS <= 0 {
		o.FixedMS = DefaultFixedMS
	}
	ev := Evaluation{Total: m.Layers}
	_, layerGB, _, outGB := blockFigures(m, o)
	ev.LayerGB = layerGB
	embdGB := float64(m.EmbdBytes) / gib

	byName := map[string]Node{}
	for _, n := range nodes {
		byName[n.Name] = n
		mc := Machine{Node: n.Name, CapGB: n.CapGB}
		switch {
		case n.BW <= 0:
			mc.Why = "no memory figures (benchmark it first: fleet nodes bench there)"
		case n.CapGB <= 0:
			mc.Why = "no free GPU/system memory right now"
		}
		ev.Machines = append(ev.Machines, mc)
	}

	// the order: master first, then the workers as given; a machine named twice counts once (its blocks add up)
	order := []string{}
	layers := map[string]int{}
	add := func(name string, n int) {
		if _, seen := layers[name]; !seen {
			order = append(order, name)
		}
		layers[name] += max(0, n)
	}
	if master == "" && len(alloc) > 0 {
		master = alloc[0].Node
	}
	add(master, 0)
	for _, a := range alloc {
		add(a.Node, a.Layers)
	}
	for _, name := range order {
		ev.Assigned += layers[name]
	}

	problems := func(f string, a ...any) { ev.Problems = append(ev.Problems, fmt.Sprintf(f, a...)) }
	priceable := true
	var ws []Node
	var mn Node
	for i, name := range order {
		n, known := byName[name]
		if !known {
			problems("%s is not a machine of this fleet", name)
			priceable = false
			continue
		}
		if n.BW <= 0 || n.CapGB <= 0 {
			why := "no memory figures (benchmark it first: fleet nodes bench there)"
			if n.BW > 0 {
				why = "no free GPU/system memory right now"
			}
			problems("%s cannot take part: %s", name, why)
			priceable = false
		}
		if i == 0 {
			mn = n
		} else if layers[name] > 0 {
			ws = append(ws, n)
		}
	}
	if outMaster && m.Runtime == RuntimeStagePipe {
		outMaster = false // a stage chain runs the output head on its last stage
	}
	// where the output projection sits: the last worker that holds blocks, else (or with outMaster) the master
	outNode := master
	if !outMaster && len(ws) > 0 {
		outNode = ws[len(ws)-1].Name
	}
	for i, name := range order {
		n := byName[name]
		need := float64(layers[name]) * layerGB
		pc := PartCheck{Node: name, Layers: layers[name], Master: i == 0, CapGB: n.CapGB}
		if i == 0 {
			need += embdGB
		}
		if name == outNode {
			need += outGB
			pc.Output = true
		}
		pc.NeedGB = need
		pc.Fits = need <= n.CapGB+1e-9
		if _, known := byName[name]; known && !pc.Fits {
			problems("%s needs %.1f GB for %d block(s) and has %.1f GB free: %.1f GB too much", name, need, layers[name], n.CapGB, need-n.CapGB)
		}
		if i > 0 && layers[name] == 0 {
			problems("%s holds no blocks: it would only add a round trip — remove it or give it some", name)
		}
		ev.Parts = append(ev.Parts, pc)
	}
	switch {
	case ev.Assigned < ev.Total:
		problems("%d of %d block(s) are not placed yet", ev.Total-ev.Assigned, ev.Total)
	case ev.Assigned > ev.Total:
		problems("%d block(s) too many: the model has %d", ev.Assigned-ev.Total, ev.Total)
	}
	if len(ws) == 0 && ev.Assigned > 0 {
		problems("every block is on %s: that is no split — add a machine and give it blocks", master)
	}
	if len(ws) == 0 {
		priceable = false
	}
	if priceable && ev.Assigned > 0 {
		c := price(m, mn, ws, layers, nil, o, outMaster)
		if math.IsInf(c.TPS, 0) || math.IsNaN(c.TPS) {
			problems("the speed cannot be estimated for this split")
		} else {
			c.Notes = append(c.Notes, "placed by hand")
			ev.Candidate = &c
		}
	}
	ev.OK = len(ev.Problems) == 0 && ev.Candidate != nil
	return ev
}

// Summary is one line for the candidate table and the notes: where the blocks go, as the user placed them.
func (ev Evaluation) Summary() string {
	var p []string
	for _, pc := range ev.Parts {
		if pc.Layers > 0 {
			p = append(p, fmt.Sprintf("%s %d", pc.Node, pc.Layers))
		}
	}
	return strings.Join(p, " · ")
}

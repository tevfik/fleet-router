package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/tevfik/fleet-router/internal/plan"
	"github.com/tevfik/fleet-router/internal/server"
)

// layerEd is the layer editor of the plan screen (S): the user picks the machines and says how many blocks each one
// holds; every change is checked against the machines' memory and priced by the daemon (plan.Evaluate).
type layerEd struct {
	alloc     []plan.Alloc // the master first, then the workers in ring order
	outMaster bool
	chain     bool // a chain of stages runs the output projection on its last stage, never on the master
	cur       int  // the highlighted row
	adding    bool // the machine list is open
	addCur    int
	seq       int
	ev        *plan.Evaluation
	err       string
}

type layerEvalMsg struct {
	seq int
	ev  plan.Evaluation
	err error
}

// openLayers starts the editor from the chosen candidate (or, with none that splits, from this machine alone).
func (m model) openLayers() (tea.Model, tea.Cmd) {
	if m.planModel == nil || m.planRes == nil || len(m.planRes.Candidates) == 0 {
		m.note = helpS.Render("plan a file first, then S splits the layers by hand")
		return m, nil
	}
	cand := m.planRes.Candidates[min(m.planPick, len(m.planRes.Candidates)-1)]
	ed := &layerEd{}
	for _, p := range cand.Parts {
		ed.alloc = append(ed.alloc, plan.Alloc{Node: p.Node, Layers: p.Layers})
		ed.outMaster = ed.outMaster || (p.Output && p.Node == cand.Master)
	}
	if len(ed.alloc) == 0 || ed.alloc[0].Node != cand.Master {
		ed.alloc = append([]plan.Alloc{{Node: cand.Master, Layers: 0}}, ed.alloc...)
	}
	if eng, _, _, _ := m.effectiveEngine(cand); eng == "chain" {
		ed.chain, ed.outMaster = true, false
	}
	m.lay = ed
	return m, m.evalLayers()
}

// evalLayers asks the daemon to check and price the current split.
func (m *model) evalLayers() tea.Cmd {
	ed := m.lay
	if ed == nil || m.planModel == nil {
		return nil
	}
	ed.seq++
	seq, cl := ed.seq, m.cl
	req := server.EvaluateRequest{
		PlanRequest: server.PlanRequest{Model: *m.planModel, Ctx: m.currentPlanCtx(), Np: planNps[m.planNp], Master: m.planMaster}, // the same inputs as the table's plan
		Alloc:       append([]plan.Alloc(nil), ed.alloc...),
		OutMaster:   ed.outMaster,
	}
	return func() tea.Msg {
		ev, err := cl.PlanEvaluate(req)
		return layerEvalMsg{seq: seq, ev: ev, err: err}
	}
}

func (ed *layerEd) placed() int {
	n := 0
	for _, a := range ed.alloc {
		n += a.Layers
	}
	return n
}

func (m model) keyLayers(s string) (tea.Model, tea.Cmd) {
	ed := *m.lay // a copy: the model is a value
	ed.alloc = append([]plan.Alloc(nil), ed.alloc...)
	m.lay = &ed
	total := 0
	if m.planModel != nil {
		total = m.planModel.Layers
	}
	if ed.adding {
		free := ed.freeMachines()
		switch s {
		case "up", "k":
			ed.addCur = max(0, ed.addCur-1)
		case "down", "j":
			ed.addCur = min(len(free)-1, ed.addCur+1)
		case "enter":
			if ed.addCur < 0 || ed.addCur >= len(free) {
				ed.adding = false
				return m, nil
			}
			give := 0
			if total > ed.placed() {
				give = total - ed.placed()
			}
			ed.alloc = append(ed.alloc, plan.Alloc{Node: free[ed.addCur].Node, Layers: give})
			ed.cur, ed.adding = len(ed.alloc)-1, false
			return m, m.evalLayers()
		case "esc", "a":
			ed.adding = false
		}
		return m, nil
	}
	step := func(d int) (tea.Model, tea.Cmd) {
		a := &ed.alloc[ed.cur]
		a.Layers = max(0, min(total, a.Layers+d))
		return m, m.evalLayers()
	}
	switch s {
	case "up", "k":
		ed.cur = max(0, ed.cur-1)
	case "down", "j":
		ed.cur = min(len(ed.alloc)-1, ed.cur+1)
	case "left", "h", "-":
		return step(-1)
	case "right", "l", "+", "=":
		return step(1)
	case "shift+left", "H", "[":
		return step(-5)
	case "shift+right", "L", "]":
		return step(5)
	case "0": // the rest of the blocks to the highlighted machine
		a := &ed.alloc[ed.cur]
		a.Layers = max(0, total-(ed.placed()-a.Layers))
		return m, m.evalLayers()
	case "a":
		if len(ed.freeMachines()) == 0 {
			m.note = helpS.Render("every machine is in the split already")
			return m, nil
		}
		ed.adding, ed.addCur = true, 0
	case "x", "delete":
		if ed.cur == 0 {
			m.note = helpS.Render("the master stays (it holds the first blocks and the embedding table); change it by planning with another master")
			return m, nil
		}
		ed.alloc = append(ed.alloc[:ed.cur], ed.alloc[ed.cur+1:]...)
		ed.cur = min(ed.cur, len(ed.alloc)-1)
		return m, m.evalLayers()
	case "K", "u": // earlier in the ring
		if ed.cur > 1 {
			ed.alloc[ed.cur-1], ed.alloc[ed.cur] = ed.alloc[ed.cur], ed.alloc[ed.cur-1]
			ed.cur--
			return m, m.evalLayers()
		}
	case "J", "n": // later in the ring
		if ed.cur >= 1 && ed.cur < len(ed.alloc)-1 {
			ed.alloc[ed.cur+1], ed.alloc[ed.cur] = ed.alloc[ed.cur], ed.alloc[ed.cur+1]
			ed.cur++
			return m, m.evalLayers()
		}
	case "o":
		if ed.chain {
			m.note = helpS.Render("a chain runs the output projection on its last stage: it cannot stay on the master")
			return m, nil
		}
		ed.outMaster = !ed.outMaster
		return m, m.evalLayers()
	case "enter":
		return m.useLayers()
	case "esc":
		m.lay = nil
	}
	return m, nil
}

// freeMachines are the machines the planner knows that the split does not use yet.
func (ed *layerEd) freeMachines() []plan.Machine {
	if ed.ev == nil {
		return nil
	}
	in := map[string]bool{}
	for _, a := range ed.alloc {
		in[a.Node] = true
	}
	var out []plan.Machine
	for _, mc := range ed.ev.Machines {
		if !in[mc.Node] {
			out = append(out, mc)
		}
	}
	return out
}

// useLayers puts the split into the candidate table as one more candidate (replacing an earlier hand-made one) and
// chooses it, so f, w, i and d act on it.
func (m model) useLayers() (tea.Model, tea.Cmd) {
	ed := m.lay
	if ed.ev == nil {
		m.note = helpS.Render("still checking the split …")
		return m, nil
	}
	if !ed.ev.OK {
		why := "the split cannot be used yet"
		if len(ed.ev.Problems) > 0 {
			why = ed.ev.Problems[0]
		}
		m.note = badS.Render("✗ " + why)
		return m, nil
	}
	res := *m.planRes
	res.Candidates = nil
	for _, c := range m.planRes.Candidates {
		if !handMade(c) {
			res.Candidates = append(res.Candidates, c)
		}
	}
	res.Candidates = append(res.Candidates, *ed.ev.Candidate)
	m.planRes, m.planPick = &res, len(res.Candidates)-1
	m.lay = nil
	m.note = okS.Render(fmt.Sprintf("✓ candidate %d is your split (%s) — i dry-runs it, d deploys it", m.planPick+1, ed.ev.Summary()))
	return m, nil
}

func handMade(c plan.Candidate) bool {
	for _, n := range c.Notes {
		if n == "placed by hand" {
			return true
		}
	}
	return false
}

// viewLayers draws the editor: one row per machine with its blocks, what they need against what is free, and below
// them whether the split works and how fast it would be.
func (m model) viewLayers(width int) string {
	ed := m.lay
	var b strings.Builder
	total := 0
	if m.planModel != nil {
		total = m.planModel.Layers
	}
	placed := ed.placed()
	head := fmt.Sprintf("LAYER EDITOR — %d blocks, %d placed", total, placed)
	hs := okS
	if placed != total {
		hs = badS
	}
	verdict := "✓"
	switch {
	case placed < total:
		verdict = fmt.Sprintf("%d still to place", total-placed)
	case placed > total:
		verdict = fmt.Sprintf("%d too many", placed-total)
	}
	b.WriteString("  " + headS.Render("▸ ") + headS.Render(head) + "  " + hs.Render(verdict) + "\n")
	barW := 20
	for i, a := range ed.alloc {
		role := "worker"
		if i == 0 {
			role = "master"
		}
		fill := 0
		if total > 0 {
			fill = (a.Layers*barW + total/2) / total
		}
		bar := strings.Repeat("■", min(fill, barW)) + strings.Repeat("·", barW-min(fill, barW))
		extra := ""
		if ed.ev != nil && i < len(ed.ev.Parts) {
			pc := ed.ev.Parts[i]
			mark := okS.Render("✓")
			if !pc.Fits {
				mark = badS.Render("✗")
			}
			extra = fmt.Sprintf("  needs %.1f / %.1f GB %s", pc.NeedGB, pc.CapGB, mark)
			var holds []string
			if pc.Master {
				holds = append(holds, "embeddings")
			}
			if pc.Output {
				holds = append(holds, "output")
			}
			if len(holds) > 0 {
				extra += helpS.Render("  " + strings.Join(holds, "+"))
			}
		}
		line := fmt.Sprintf("%-10s %-6s %s %3d%s", trunc(a.Node, 10), role, bar, a.Layers, extra)
		if i == ed.cur && !ed.adding {
			b.WriteString("  " + curMark(true) + trunc(line, width-6) + "\n")
		} else {
			b.WriteString("    " + trunc(line, width-6) + "\n")
		}
	}
	if ed.adding {
		b.WriteString(helpS.Render("  add which machine?") + "\n")
		for i, mc := range ed.freeMachines() {
			l := fmt.Sprintf("%-10s %5.1f GB free", mc.Node, mc.CapGB)
			if mc.Why != "" {
				l += "  — " + mc.Why
			}
			mark := "    "
			if i == ed.addCur {
				mark = "  " + curMark(true)
			}
			b.WriteString(mark + trunc(l, width-6) + "\n")
		}
	}
	switch {
	case ed.err != "":
		b.WriteString("  " + badS.Render(trunc("error: "+ed.err, width-4)) + "\n")
	case ed.ev == nil:
		b.WriteString(helpS.Render("  checking …") + "\n")
	default:
		for _, p := range ed.ev.Problems {
			b.WriteString("  " + badS.Render("✗ ") + trunc(p, width-6) + "\n")
		}
		if c := ed.ev.Candidate; c != nil {
			best := ""
			if m.planRes != nil && len(m.planRes.Candidates) > 0 {
				best = fmt.Sprintf("  (the plan's first candidate: %.1f)", m.planRes.Candidates[0].TPS)
			}
			s := okS
			if !ed.ev.OK {
				s = helpS
			}
			b.WriteString("  " + s.Render(fmt.Sprintf("≈ %.1f tok/s · %.0f ms per token%s", c.TPS, c.TokenMS, best)) + "\n")
		}
	}
	b.WriteString("\n")
	return b.String()
}

package tui

import (
	tea "charm.land/bubbletea/v2"
	"fmt"
	"github.com/tevfik/fleet-router/internal/board"
	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/server"
	"strings"
)

func (m model) viewDash() string {
	var b strings.Builder
	eps := m.dashEps()
	maxTPS := board.Max(board.Rows(eps))
	b.WriteString(m.searchLine())
	b.WriteString(helpS.Render("  ● live  ○ benchmark  ◆ loaded ◇ cold   caps: C chat · V vision · A audio · I image-gen · E embed · K rerank · T tools · R thinking") + "\n")
	b.WriteString(helpS.Render(fmt.Sprintf("    %-28s %-8s %-13s %s", "MODEL", "NODE", "TOK/S", "CAPS")) + "\n")
	budget := m.h - 17
	top, end := window(len(eps), m.cur, budget)
	if top > 0 {
		b.WriteString(helpS.Render(fmt.Sprintf("      ↑ %d more", top)) + "\n")
	}
	for i := top; i < end; i++ {
		e := eps[i]
		tps := helpS.Render(fmt.Sprintf("%-13s", "—"))
		if e.TPS > 0 {
			src := " " // ● = measured from real traffic, ○ = from a benchmark
			if e.TPSSrc == "live" {
				src = okS.Render("●")
			} else if e.TPSSrc == "bench" {
				src = helpS.Render("○")
			}
			tps = cSecondS.Render(board.Bar(e.TPS, maxTPS, 6)) + fmt.Sprintf(" %5.1f", e.TPS) + src
		}
		fmt.Fprintf(&b, "%s%s %-28s %-8s %s %s\n", curMark(i == m.cur), mark(e), trunc(board.ModelMark(e)+peerMark(e)+e.Model, 28), trunc(e.Node, 8), tps, capLetters(e.Endpoint)+tagLabel(e.Endpoint))
	}
	if end < len(eps) {
		b.WriteString(helpS.Render(fmt.Sprintf("      ↓ %d more", len(eps)-end)) + "\n")
	}
	if len(eps) == 0 {
		msg := "no endpoints — add them from the Discover screen"
		if m.filter != "" {
			msg = "nothing matches the search — esc clears it"
		}
		b.WriteString(descS.Render(msg) + "\n")
		return b.String()
	}
	if m.cur < len(eps) { // detail panel for the highlighted endpoint
		e := eps[m.cur]
		src := "auto-detected"
		if len(e.Caps) > 0 {
			src = "set explicitly"
		}
		b.WriteString("\n" + groupS.Render(e.Ref()) + helpS.Render(" · endpoint "+e.Name) + "\n")
		detail := fmt.Sprintf("slots %s (%d in use, %d waiting) · ctx %s", slotStr(e.Slots), e.InUse, e.Waiting, ctxStr(e.Ctx))
		if e.LatencyMS > 0 {
			detail += fmt.Sprintf(" · ping %dms", e.LatencyMS)
		}
		b.WriteString("  " + detail + "\n")
		b.WriteString("  " + speedLine(e) + "\n")
		if e.Idle {
			b.WriteString("  " + helpS.Render("💤 SSH tunnel idle — closed to save the connection, it opens on the next request") + "\n")
		}
		if q := qualityLine(e.Name); q != "" {
			b.WriteString("  " + q + "\n")
		}
		if len(e.Strained) > 0 {
			b.WriteString("  " + badS.Render("⚠ "+e.Node+": "+strings.Join(e.Strained, " · ")+" — routing prefers other machines") + "\n")
		}
		b.WriteString("  " + helpS.Render("probe "+board.ProbeLine(e, e.Node == board.SelfNode(m.st))) + "\n")
		b.WriteString("  caps: " + accS.Render(capNames(e.Endpoint)) + helpS.Render(" ("+src+") — press c to change") + "\n")
		for _, l := range board.GroupLines(e) {
			b.WriteString("  " + helpS.Render(l) + "\n")
		}
		if e.Err != "" {
			b.WriteString("  " + badS.Render(e.Err) + "\n")
		}
	}
	return b.String()
}

// speedLine: decode speed, time to first token, prefill speed and prompt sizes of one endpoint.
func speedLine(e server.EpStatus) string {
	if e.TPS == 0 && e.TTFTMS == 0 && e.MaxPrompt == 0 {
		return helpS.Render("no speed data yet — it is collected from real requests (or press b to benchmark)")
	}
	parts := []string{}
	if e.TPS > 0 {
		src := "benchmark"
		if e.TPSSrc == "live" {
			src = fmt.Sprintf("live, %d streams", e.Samples)
		}
		parts = append(parts, fmt.Sprintf("%.1f tok/s (%s)", e.TPS, src))
	}
	if e.TTFTMS > 0 {
		parts = append(parts, fmt.Sprintf("first token %s", msStr(e.TTFTMS)))
	}
	if e.PrefillTPS > 0 {
		parts = append(parts, fmt.Sprintf("prefill %.0f tok/s", e.PrefillTPS))
	}
	if e.MaxPrompt > 0 {
		c := fmt.Sprintf("prompt last %s · largest %s", tokStr(e.LastPrompt), tokStr(e.MaxPrompt))
		if e.Ctx > 0 {
			c += fmt.Sprintf(" of %s ctx (%d%%)", tokStr(e.Ctx), 100*e.MaxPrompt/e.Ctx)
		}
		parts = append(parts, c)
	}
	return strings.Join(parts, " · ")
}

func msStr(ms int64) string {
	if ms >= 1000 {
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	}
	return fmt.Sprintf("%dms", ms)
}

func tokStr(n int) string {
	if n >= 10000 {
		return fmt.Sprintf("%dk", n/1000)
	}
	return fmt.Sprint(n)
}

func ctxStr(n int) string {
	if n <= 0 {
		return "?"
	}
	return fmt.Sprintf("%dk", n/1024)
}

// speedRows ranks the endpoints that pass the machine filter by tokens/s.
func (m model) speedRows() []board.Row { return board.Rows(m.boardEps()) }

// viewBoard is the leaderboard: view tabs, machine filter, top cards, then the chosen view.
func (m model) viewBoard() string {
	var b strings.Builder
	b.WriteString(m.viewBoardHeader())
	if m.showCards() {
		b.WriteString("\n" + m.viewCards())
	}
	b.WriteString("\n" + m.viewBoardBody())
	return b.String()
}

func (m model) viewSpeed() string {
	rows := m.speedRows()
	var b strings.Builder
	b.WriteString("  " + groupS.Render("TOKENS / SECOND") + helpS.Render("   b: benchmark highlighted · B: all healthy") + "\n\n")
	maxv := board.Max(rows)
	budget := m.h - m.boardChrome()
	top, end := window(len(rows), m.cur, budget)
	if m.h == 0 {
		top, end = 0, len(rows)
	}
	if top > 0 {
		b.WriteString(helpS.Render(fmt.Sprintf("      ↑ %d more", top)) + "\n")
	}
	for i := top; i < end; i++ {
		r := rows[i]
		rank, medal := "  ", "  "
		if r.Rank > 0 {
			rank = fmt.Sprintf("%2d", r.Rank)
			if r.Medal != "" {
				medal = r.Medal
			}
		}
		bar, val := helpS.Render(board.Bar(0, maxv, 20)), helpS.Render("     —")
		if r.TPS > 0 {
			style := cSecondS
			if r.Rank == 1 {
				style = accS
			}
			bar, val = style.Render(board.Bar(r.TPS, maxv, 20)), fmt.Sprintf("%6.1f t/s", r.TPS)
		}
		extra := helpS.Render(trunc(r.Node, 8))
		if r.TTFTMS > 0 {
			extra = helpS.Render(fmt.Sprintf("%s ttft %s", trunc(r.Node, 8), msStr(r.TTFTMS)))
		}
		if r.Err != "" {
			extra = badS.Render("bench: " + trunc(r.Err, 24))
		}
		fmt.Fprintf(&b, "%s%s %s %-24s %s %s %s\n", curMark(i == m.cur), rank, medal, trunc(r.Node+"/"+r.Model, 24), bar, val, extra)
	}
	if end < len(rows) {
		b.WriteString(helpS.Render(fmt.Sprintf("      ↓ %d more", len(rows)-end)) + "\n")
	}
	if len(rows) == 0 {
		b.WriteString(descS.Render("  no chat endpoints yet — discover some first") + "\n")
	} else if maxv == 0 {
		b.WriteString("\n" + descS.Render("  nothing measured yet — press B to benchmark every healthy endpoint") + "\n")
	}
	return b.String()
}

// window returns the slice bounds [top,end) that keep cur inside a budget-row viewport.
func window(total, cur, budget int) (int, int) {
	if budget < 3 {
		budget = 3
	}
	top := 0
	if total > budget {
		top = max(0, min(cur-budget/2, total-budget))
	}
	return top, min(total, top+budget)
}

// allCaps lists the capabilities in display order with a one-letter code and a description.
var allCaps = []struct{ name, letter, desc string }{
	{"chat", "C", "answers text prompts"},
	{"vision", "V", "accepts images"},
	{"audio", "A", "accepts audio"},
	{"image_gen", "I", "generates images"},
	{"embed", "E", "produces embeddings (vectors)"},
	{"rerank", "K", "ranks documents for a query"},
	{"tools", "T", "function / tool calling"},
	{"thinking", "R", "reasoning (thinking) mode"},
}

func capLetters(e config.Endpoint) string {
	var b strings.Builder
	for _, c := range allCaps {
		if e.Has(c.name) {
			b.WriteString(c.letter)
		} else {
			b.WriteString("·")
		}
	}
	return b.String()
}

func capNames(e config.Endpoint) string {
	var l []string
	for _, c := range allCaps {
		if e.Has(c.name) {
			l = append(l, c.name)
		}
	}
	if len(l) == 0 {
		return "none"
	}
	return strings.Join(l, ", ")
}

func (m model) epByName(name string) (server.EpStatus, bool) {
	for _, e := range m.st.Endpoints {
		if e.Name == name {
			return e, true
		}
	}
	return server.EpStatus{}, false
}

func (m model) keyCaps(s string) (tea.Model, tea.Cmd) {
	e, ok := m.epByName(m.capName)
	if !ok {
		m.sc, m.cur = scDash, 0
		return m, nil
	}
	if s != "enter" && s != " " && s != "space" {
		return m, nil
	}
	cl, name := m.cl, m.capName
	switch {
	case m.cur < len(allCaps): // toggle one capability (starts from the effective set)
		c := allCaps[m.cur].name
		var next []string
		for _, k := range allCaps {
			on := e.Has(k.name)
			if k.name == c {
				on = !on
			}
			if on {
				next = append(next, k.name)
			}
		}
		return m, func() tea.Msg {
			if err := cl.SetCaps(name, next); err != nil {
				return noteMsg(badS.Render("error: " + err.Error()))
			}
			return noteMsg(okS.Render("✓ capabilities saved"))
		}
	case m.cur == len(allCaps): // back to automatic detection
		return m, func() tea.Msg {
			if err := cl.SetCaps(name, nil); err != nil {
				return noteMsg(badS.Render("error: " + err.Error()))
			}
			return noteMsg(okS.Render("✓ back to automatic detection"))
		}
	}
	m.sc, m.cur = scDash, 0
	for i, x := range m.st.Endpoints {
		if x.Name == name {
			m.cur = i
		}
	}
	return m, nil
}

func (m model) viewCaps() string {
	e, ok := m.epByName(m.capName)
	if !ok {
		return ""
	}
	var b strings.Builder
	src := "automatic (from the server / the model name)"
	if len(e.Caps) > 0 {
		src = "set explicitly"
	}
	fmt.Fprintf(&b, "%s  %s\n%s\n\n", accS.Render(e.Ref()), helpS.Render("endpoint "+e.Name), helpS.Render("capabilities: "+src))
	for i, c := range allCaps {
		box := "[ ]"
		if e.Has(c.name) {
			box = okS.Render("[x]")
		}
		fmt.Fprintf(&b, "%s%s %-10s %s\n", curMark(i == m.cur), box, c.name, descS.Render(c.desc))
	}
	b.WriteString("\n" + curMark(m.cur == len(allCaps)) + helpS.Render("↺ reset to automatic detection") + "\n")
	b.WriteString(curMark(m.cur == len(allCaps)+1) + helpS.Render("← back") + "\n")
	b.WriteString("\n" + helpS.Render("Capabilities decide routing: images only reach models with vision, embeddings only embed models, …"))
	return b.String()
}

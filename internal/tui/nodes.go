package tui

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"

	"github.com/tevfik/fleet-router/internal/board"
	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/server"
	"github.com/tevfik/fleet-router/internal/setup"
)

// The Nodes screen lists the machines (ping, load, memory, GPU, hottest sensor) and shows the highlighted
// one in full below: what its fleet reads from it, and what each of its models can do (probe).

// fetchHosts asks the daemon for every node's readings at once (it reaches each machine's own fleet).
func (m model) fetchHosts(nodes []server.NodeStatus) map[string]server.HostReport {
	out := make(map[string]server.HostReport, len(nodes))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, n := range nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := m.cl.Host(n.Node)
			if err != nil {
				r.Err = err.Error()
			}
			mu.Lock()
			out[n.Node] = r
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

func (m model) viewNodes() string {
	var b strings.Builder
	nodes := m.st.Nodes
	if len(nodes) == 0 {
		return descS.Render("no nodes — add models from the Discover screen") + "\n"
	}
	b.WriteString(helpS.Render(fmt.Sprintf("    %-12s %-18s %-6s %-7s %5s %5s %5s %5s %6s %5s %11s", "NODE", "VIA", "UP", "PING", "CPU", "RAM", "GPU", "VRAM", "TEMP", "AI", "NET")+"  FLEET") + "\n")
	for i, n := range nodes {
		r, seen := m.nodeHost[n.Node]
		cpu, ram, gpu, vram, temp := board.NodeCells(r)
		netRate := board.NetCell(r)
		if !seen {
			cpu, ram, gpu, vram, netRate = "…", "…", "…", "…", "…"
		}
		ping := "—"
		if n.PingMS > 0 {
			ping = fmt.Sprintf("%dms", n.PingMS)
		}
		up := fmt.Sprintf("%d/%d", n.Up, n.Total)
		upS := okS
		if n.Up < n.Total {
			upS = badS
			if n.Up > 0 {
				upS = accS
			}
		}
		via := fmt.Sprintf("%-18s", trunc(n.Via, 18))
		if n.Self {
			via = accS.Render(via)
		}
		score := board.ScoreCell(r)
		if !seen {
			score = "…"
		}
		name := board.PadRight(trunc(n.Node, 12), 12)
		lcWarns := m.nodeLifecycleWarnings(n.Node)
		if (n.Strain != nil && n.Strain.Strained()) || len(lcWarns) > 0 {
			name = badS.Render(board.PadRight(trunc("⚠ "+n.Node, 12), 12))
		}
		ver := "…"
		if seen {
			ver = board.VersionCell(r)
		}
		fmt.Fprintf(&b, "%s %s %s %s %-7s %5s %5s %5s %5s %6s %5s %11s  %s\n", curMark(i == m.cur), name, via,
			upS.Render(fmt.Sprintf("%-6s", up)), ping, cpu, ram, gpu, vram, temp, accS.Render(fmt.Sprintf("%5s", score)), netRate, ver)
	}
	width := m.w
	if width <= 0 {
		width = 100
	}
	var vs []board.NodeVersion
	for _, n := range nodes {
		if r, ok := m.nodeHost[n.Node]; ok && r.Err == "" {
			vs = append(vs, board.NodeVersion{Node: n.Node, Fleet: r.Version, Engine: r.Engine})
		}
	}
	for _, l := range board.VersionDrift(vs) {
		b.WriteString(badS.Render(trunc("  ⚠ "+l, width-2)) + "\n")
	}
	if m.cur >= len(nodes) {
		return b.String()
	}
	n := nodes[m.cur]
	lines := board.NodeDetailFull(n, m.nodeHost[n.Node], m.st.Endpoints, width, server.Suggest(m.st.Endpoints, m.nodeHost))
	for _, w := range m.nodeLifecycleWarnings(n.Node) {
		lines = append(lines, "  ⚠ "+w)
	}
	spec, declared := m.cfg.Nodes[n.Node]
	lines = append(lines, specLine(spec, declared))
	budget := len(lines)
	if m.h > 0 {
		budget = max(4, m.h-len(nodes)-12)
	}
	b.WriteString("\n")
	for i, l := range lines {
		if i >= budget {
			b.WriteString(helpS.Render(fmt.Sprintf("  … %d more lines (fleet nodes %s)", len(lines)-i, n.Node)) + "\n")
			break
		}
		switch {
		case i == 0:
			l = groupS.Render(l)
		case strings.HasPrefix(l, "  ⚠"):
			l = badS.Render(trunc(l, width-2))
		case strings.HasPrefix(l, "  AI    score"), strings.HasPrefix(l, "  MOVE"):
			l = accS.Render(trunc(l, width-2))
		case strings.HasPrefix(l, "  STRAIN"):
			l = badS.Render(trunc(l, width-2))
		case strings.HasPrefix(l, "  WARN"):
			l = cSecondS.Render(trunc(l, width-2))
		case strings.HasPrefix(l, "  machine:") || strings.HasPrefix(l, "  note:"):
			l = badS.Render(l)
		case strings.HasPrefix(l, "    ✗"):
			l = badS.Render(trunc(l, width-2))
		default:
			l = trunc(l, width-2)
		}
		b.WriteString(l + "\n")
	}
	return b.String()
}

func (m model) nodeLifecycleWarnings(node string) []string {
	var warnings []string

	// 1. checkDuplicateEndpointRuns for endpoints involving this node
	byEp := make(map[string][]string)
	for _, r := range m.runs {
		if r.Ended.IsZero() && r.Lost == "" && r.Endpoint != "" {
			involves := (r.Master == node)
			for _, p := range r.Parts {
				if p.Node == node {
					involves = true
					break
				}
			}
			if involves {
				byEp[r.Endpoint] = append(byEp[r.Endpoint], r.ID)
			}
		}
	}
	var eps []string
	for ep, ids := range byEp {
		if len(ids) > 1 {
			eps = append(eps, ep)
		}
	}
	sort.Strings(eps)
	for _, ep := range eps {
		ids := byEp[ep]
		sort.Strings(ids)
		warnings = append(warnings, fmt.Sprintf("endpoint %q has %d active runs (%s) — clean up stale records with 'fleet run end <id> --record-only'", ep, len(ids), strings.Join(ids, ", ")))
	}

	// 2. checkUnmanagedRuns for active runs on this node
	for _, r := range m.runs {
		if !r.Ended.IsZero() || r.Lost != "" {
			continue
		}
		involves := (r.Master == node)
		for _, p := range r.Parts {
			if p.Node == node {
				involves = true
				break
			}
		}
		if !involves {
			continue
		}
		profName := ""
		for _, p := range m.profList {
			if p == nil {
				continue
			}
			for _, pr := range p.Runs {
				if pr.RunID == r.ID {
					profName = p.Name
					break
				}
			}
			if profName == "" && r.Endpoint == p.Name {
				profName = p.Name
			}
		}
		hasAuto := false
		if profName != "" {
			_, err := os.Stat(setup.ProfileUnitPath(profName))
			hasAuto = (err == nil)
		}
		if !hasAuto {
			warnings = append(warnings, fmt.Sprintf("active run %s (%s, model %s) has no autostart profile (no fleet-profile-*.service): it will not come back after a reboot", r.ID, r.Endpoint, r.Model))
		}
	}

	// 3. checkLinger on self node
	isSelf := false
	for _, n := range m.st.Nodes {
		if n.Node == node && n.Self {
			isSelf = true
			break
		}
	}
	if isSelf && m.linger != "" && m.linger != "yes" { // m.linger is read in fetch(): View must not run loginctl
		warnings = append(warnings, "systemd linger is off (Linger=no): user services will not start at boot without active login — run `loginctl enable-linger`")
	}

	return warnings
}

func (m model) keyNodes(s string) (tea.Model, tea.Cmd) {
	if nm, cmd, ok := m.confirmPending(s); ok {
		return nm, cmd
	}
	if s == "+" || s == "n" {
		m.editing, m.editBuf = "addnode", ""
		return m, nil
	}
	if s == "j" || s == "J" {
		m.hubPrev = scNodes
		m.sc, m.cur, m.note = scHub, 0, ""
		m.hubShowModels = true
		return m, tea.Batch(m.fetch(), m.fetchRemoteHubModels())
	}
	if s == "u" {
		return m, m.undoConfig()
	}
	if s == "H" {
		return m, m.historyCmd()
	}
	nodes := m.st.Nodes
	if m.cur >= len(nodes) {
		return m, nil
	}
	n := nodes[m.cur]
	elsewhere := func(what string) (tea.Model, tea.Cmd) {
		m.note = helpS.Render(n.Node + " " + what + " — every fleet measures only its own machine (fleet nodes bench / fleet probe there)")
		return m, nil
	}
	switch s {
	case "e": // fleet config node set (edit the spec)
		spec := m.cfg.Nodes[n.Node]
		m.editing, m.editFor, m.editBuf = "nodespec", n.Node, specText(spec)
		if m.editBuf != "" {
			m.editBuf += " "
		}
		return m, nil
	case "x": // remove the node: out of the scan list (and its spec); asks first
		if n.Self {
			m.note = helpS.Render(n.Node + " is this machine — it cannot be removed (d leaves the line)")
			return m, nil
		}
		_, hasSpec := m.cfg.Nodes[n.Node]
		if !hasSpec && len(nodeSSHEntries(m.cfg.Discover.SSH, n.Node)) == 0 {
			m.note = helpS.Render(n.Node + " is not in the scan list or nodes: — it is found another way (peer / network); remove it where it comes from")
			return m, nil
		}
		m.pend, m.confirm = &pendingAct{kind: "rm-node", name: n.Node}, true
		m.note = badS.Render("remove node " + n.Node + " (scan list entry and declared spec; nothing on that machine is touched)? y remove, n cancel")
		return m, nil
	case "f": // fleet config node rm
		if _, ok := m.cfg.Nodes[n.Node]; !ok {
			m.note = helpS.Render(n.Node + " has no declared spec")
			return m, nil
		}
		cl, name := m.cl, n.Node
		return m, func() tea.Msg {
			if err := cl.SetNodeSpec(name, config.NodeSpec{}, true); err != nil {
				return noteMsg(badS.Render("error: " + err.Error()))
			}
			return noteMsg(okS.Render("✓ spec of " + name + " removed from nodes:"))
		}
	case "enter": // its models on the Dashboard
		m.sc, m.filter, m.cur = scDash, n.Node, 0
		return m, nil
	case "d": // leave the line (drain) / rejoin: only this machine can say so
		if !n.Self {
			return elsewhere("leaves the line by its own fleet (fleet leave there)")
		}
		on := !m.st.Draining
		return m, func() tea.Msg {
			if err := m.cl.Drain(on); err != nil {
				return noteMsg(badS.Render("error: " + err.Error()))
			}
			if on {
				return noteMsg(okS.Render("✓ this machine left the line: plans skip it, its models come last, groups are told"))
			}
			return noteMsg(okS.Render("✓ back in the line"))
		}
	case "b": // measure this machine again (benchmark)
		if !n.Self {
			return elsewhere("is benchmarked by its own fleet")
		}
		return m, func() tea.Msg {
			if err := m.cl.HostBench(); err != nil {
				return noteMsg(badS.Render("error: " + err.Error()))
			}
			return noteMsg(okS.Render("⏱  benchmarking this machine — a few seconds (RAM bandwidth, CPU, GPU memory)"))
		}
	case "p":
		if !n.Self {
			return elsewhere("has its models probed by its own fleet")
		}
		return m, m.probe(nil)
	case "a":
		node := nodes[m.cur].Node
		return m, func() tea.Msg {
			ch, err := m.cl.ApplyProbe([]string{node})
			switch {
			case err != nil:
				return noteMsg(badS.Render("error: " + err.Error()))
			case len(ch) == 0:
				return noteMsg(okS.Render("✓ caps on " + node + " already match the probe (or nothing probed yet: p)"))
			}
			return noteMsg(okS.Render(fmt.Sprintf("✓ caps updated: %s", strings.Join(ch, " · "))))
		}
	}
	return m, nil
}

// probe starts the capability probe of nodes or endpoints (nil: every healthy one).
func (m model) probe(names []string) tea.Cmd {
	return func() tea.Msg {
		if err := m.cl.Probe(names); err != nil {
			return noteMsg(badS.Render("error: " + err.Error()))
		}
		what := "this machine's models (each one is loaded)"
		if len(names) > 0 {
			what = strings.Join(names, ", ")
		}
		return noteMsg(okS.Render("🔬 probing " + what + " — chat, tools, thinking, vision, embed; results appear here"))
	}
}

// nodeSSHEntries are the scan-list entries that stand for a node: "dino", "user@dino" or "dino.local".
func nodeSSHEntries(ssh []string, node string) []string {
	var out []string
	for _, e := range ssh {
		h := e
		if i := strings.LastIndex(h, "@"); i >= 0 {
			h = h[i+1:]
		}
		if h == node || strings.HasPrefix(h, node+".") {
			out = append(out, e)
		}
	}
	return out
}

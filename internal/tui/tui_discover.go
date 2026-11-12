package tui

import (
	tea "charm.land/bubbletea/v2"
	"fmt"
	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/discover"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func contains(l []string, x string) bool {
	for _, v := range l {
		if v == x {
			return true
		}
	}
	return false
}

// loadHosts reads the configuration and merges ~/.ssh/config Host names with the configured targets.
func (m *model) loadHosts() {
	if c, err := m.cl.Config(); err == nil {
		m.cfg = c
	}
	m.rebuildHosts()
}

// rebuildHosts recomputes the target list from m.cfg without asking the daemon: right after an edit the
// (asynchronous) save may not have finished, and re-reading the configuration would bring back the old one.
func (m *model) rebuildHosts() {
	set := map[string]bool{}
	for _, h := range m.cfg.Discover.SSH {
		set[h] = true
	}
	for _, h := range sshConfigHosts() {
		set[h] = true
	}
	m.hosts = m.hosts[:0]
	for h := range set {
		m.hosts = append(m.hosts, h)
	}
	sort.Strings(m.hosts)
}

func sshConfigHosts() []string {
	h, _ := os.UserHomeDir()
	b, err := os.ReadFile(filepath.Join(h, ".ssh", "config"))
	if err != nil {
		return nil
	}
	self, _ := os.Hostname()
	var out []string
	for _, ln := range strings.Split(string(b), "\n") {
		f := strings.Fields(ln)
		if len(f) < 2 || !strings.EqualFold(f[0], "host") {
			continue
		}
		for _, n := range f[1:] {
			if strings.ContainsAny(n, "*?!") || n == self {
				continue
			}
			out = append(out, n)
		}
	}
	return out
}

func (m model) saveDiscover(ports string, ssh []string, ok string) tea.Cmd {
	cl, ign := m.cl, m.cfg.Discover.Ignore
	return func() tea.Msg {
		if err := cl.SetDiscover(ports, ssh, ign); err != nil {
			return noteMsg(badS.Render("error: " + err.Error()))
		}
		return noteMsg(okS.Render("✓ " + ok))
	}
}

// vrow is one visible line of the grouped discovery results.
type vrow struct {
	header bool
	act    string // "register" | "back" for the action rows at the bottom
	node   string
	idx    int // index into disc.Plan.Add (items only)
}

// visRows flattens the discovery results: one collapsible header per machine, then its (filtered) models.
func (m model) visRows() []vrow {
	if m.disc == nil {
		return nil
	}
	return m.itemRows()
}

// itemRows is visRows without the action rows.
func (m model) itemRows() []vrow {
	var order []string
	byNode := map[string][]int{}
	f := strings.ToLower(m.filter)
	for i, e := range m.disc.Plan.Add {
		if f != "" && !strings.Contains(strings.ToLower(e.Name+" "+e.Model), f) {
			continue
		}
		if _, ok := byNode[e.Node]; !ok {
			order = append(order, e.Node)
		}
		byNode[e.Node] = append(byNode[e.Node], i)
	}
	var rows []vrow
	for _, n := range order {
		rows = append(rows, vrow{header: true, node: n})
		if !m.collapsed[n] {
			for _, i := range byNode[n] {
				rows = append(rows, vrow{node: n, idx: i})
			}
		}
	}
	return rows
}

// groupIdx returns the (filtered) item indexes of a machine, collapsed or not.
func (m model) groupIdx(node string) []int {
	var out []int
	f := strings.ToLower(m.filter)
	for i, e := range m.disc.Plan.Add {
		if e.Node == node && (f == "" || strings.Contains(strings.ToLower(e.Name+" "+e.Model), f)) {
			out = append(out, i)
		}
	}
	return out
}

// mrow is one line of the targets menu.
type mrow struct {
	kind  string // host | addhost | port | addport | embed | scan
	i     int    // host index
	ports []int  // port rows
	spec  string
	label string
}

func (m model) enabledPorts() map[int]bool {
	set := map[int]bool{}
	for _, p := range discover.ParsePorts(m.cfg.Discover.Ports) {
		set[p] = true
	}
	return set
}

// menuRows builds the whole targets menu: hosts, well-known ports (checkboxes), custom ports, actions.
func (m model) menuRows() []mrow {
	var rows []mrow
	for i := range m.hosts {
		rows = append(rows, mrow{kind: "host", i: i})
	}
	rows = append(rows, mrow{kind: "addhost"})
	covered := map[int]bool{}
	for _, pr := range discover.PortPresets {
		ps := discover.ParsePorts(pr.Spec)
		for _, p := range ps {
			covered[p] = true
		}
		rows = append(rows, mrow{kind: "port", ports: ps, spec: pr.Spec, label: pr.Name})
	}
	for _, p := range discover.ParsePorts(m.cfg.Discover.Ports) { // ports you configured that are not presets
		if !covered[p] {
			rows = append(rows, mrow{kind: "port", ports: []int{p}, spec: fmt.Sprint(p), label: "custom"})
		}
	}
	return append(rows, mrow{kind: "addport"}, mrow{kind: "embed"})
}

func (m model) menuLen() int { return len(m.menuRows()) }

func (m model) togglePorts(ps []int) (model, tea.Cmd) {
	set := m.enabledPorts()
	all := true
	for _, p := range ps {
		all = all && set[p]
	}
	for _, p := range ps {
		if all {
			delete(set, p)
		} else {
			set[p] = true
		}
	}
	var l []int
	for p := range set {
		l = append(l, p)
	}
	m.cfg.Discover.Ports = discover.FormatPorts(l)
	return m, m.saveDiscover(m.cfg.Discover.Ports, m.cfg.Discover.SSH, "ports updated")
}

func (m model) scan() (model, tea.Cmd) {
	if m.busy {
		return m, nil
	}
	m.busy, m.note = true, "scanning… (this machine + checked SSH hosts)"
	emb, cl := m.inclEmb, m.cl
	return m, func() tea.Msg {
		cfg, err := cl.Config()
		if err != nil {
			return discMsg{err: err}
		}
		r, err := cl.Discover(cfg.Discover.Ports, cfg.Discover.SSH, emb, false)
		return discMsg{r, err}
	}
}

func (m model) toggleHost(h string) (model, tea.Cmd) {
	var ssh []string
	if contains(m.cfg.Discover.SSH, h) {
		for _, v := range m.cfg.Discover.SSH {
			if v != h {
				ssh = append(ssh, v)
			}
		}
	} else {
		ssh = append(append([]string{}, m.cfg.Discover.SSH...), h)
	}
	m.cfg.Discover.SSH = ssh
	return m, m.saveDiscover(m.cfg.Discover.Ports, ssh, "targets updated")
}

func (m model) keyDiscover(s string) (tea.Model, tea.Cmd) {
	if m.focus == 0 {
		return m.keyTargets(s)
	}
	return m.keyResults(s)
}

func (m model) keyTargets(s string) (tea.Model, tea.Cmd) {
	rows := m.menuRows()
	switch s {
	case "enter", " ", "space":
		if m.cur >= len(rows) {
			return m, nil
		}
		r := rows[m.cur]
		switch r.kind {
		case "host":
			return m.toggleHost(m.hosts[r.i])
		case "addhost":
			m.editing, m.editBuf = "addhost", ""
		case "port":
			return m.togglePorts(r.ports)
		case "addport":
			m.editing, m.editBuf = "addport", ""
		case "embed":
			m.inclEmb = !m.inclEmb
		}
	case "d", "r":
		return m.scan()
	case "x": // remove a configured target (~/.ssh/config entries stay as candidates)
		if m.cur < len(rows) && rows[m.cur].kind == "host" {
			h := m.hosts[rows[m.cur].i]
			var ssh []string
			for _, v := range m.cfg.Discover.SSH {
				if v != h {
					ssh = append(ssh, v)
				}
			}
			m.cfg.Discover.SSH = ssh
			return m, m.saveDiscover(m.cfg.Discover.Ports, ssh, h+" removed from scanning")
		}
	case "L": // jump to the last scan's results
		if m.disc != nil {
			m.focus, m.cur, m.otherCur = 1, 0, m.cur
		}
	}
	return m, nil
}

func (m model) keyResults(s string) (tea.Model, tea.Cmd) {
	rows := m.visRows()
	switch s {
	case "e":
		m.inclEmb = !m.inclEmb
	case "/":
		m.editing, m.editBuf = "filter", m.filter
	case "a", "n": // select all / none (of what the filter shows)
		for _, r := range rows {
			if !r.header && r.act == "" {
				m.sel[r.idx] = s == "a"
			}
		}
	case "left", "h":
		if m.cur < len(rows) {
			r := rows[m.cur]
			if r.header {
				if m.collapsed == nil {
					m.collapsed = map[string]bool{}
				}
				m.collapsed[r.node] = true
			} else if r.act == "" { // jump to the group header
				for i := m.cur; i >= 0; i-- {
					if rows[i].header {
						m.cur = i
						break
					}
				}
			}
		}
	case "right", "l":
		if m.cur < len(rows) && rows[m.cur].header {
			delete(m.collapsed, rows[m.cur].node)
		}
	case "i": // permanently ignore the machine:port of the highlighted result (e.g. a tunnel to another machine)
		if m.cur < len(rows) && rows[m.cur].act == "" && !rows[m.cur].header {
			e := m.disc.Plan.Add[rows[m.cur].idx]
			key := e.Node + ":" + portOf(e)
			m.cfg.Discover.Ignore = append(append([]string{}, m.cfg.Discover.Ignore...), key)
			var rest []config.Endpoint
			for _, x := range m.disc.Plan.Add {
				if x.Node+":"+portOf(x) != key {
					rest = append(rest, x)
				}
			}
			m.disc.Plan.Add, m.cur = rest, 0
			m.sel = map[int]bool{}
			for i := range rest {
				m.sel[i] = true
			}
			return m, m.saveDiscover(m.cfg.Discover.Ports, m.cfg.Discover.SSH, key+" will be ignored")
		}
	case "w":
		return m.acceptSelected()
	case "enter", " ", "space":
		if m.cur >= len(rows) {
			return m, nil
		}
		r := rows[m.cur]
		switch {
		case r.act == "register":
			return m.acceptSelected()
		case r.act == "back":
			m.focus, m.cur = 0, m.otherCur
		case r.header: // toggle the whole machine
			idx := m.groupIdx(r.node)
			all := true
			for _, i := range idx {
				all = all && m.sel[i]
			}
			for _, i := range idx {
				m.sel[i] = !all
			}
		default:
			m.sel[r.idx] = !m.sel[r.idx]
		}
	}
	return m, nil
}

func (m model) acceptSelected() (tea.Model, tea.Cmd) {
	var pick []config.Endpoint
	for i, e := range m.disc.Plan.Add {
		if m.sel[i] {
			pick = append(pick, e)
		}
	}
	if len(pick) == 0 {
		m.note = helpS.Render("nothing selected — tick some models first")
		return m, nil
	}
	cl := m.cl
	return m, func() tea.Msg {
		var names []string
		for _, e := range pick {
			names = append(names, e.Name)
		}
		return regMsg{names: names, err: cl.Endpoints(pick, nil)}
	}
}

func (m model) viewDiscover() string {
	if m.focus == 1 && m.disc != nil {
		return m.viewResults()
	}
	var b strings.Builder
	b.WriteString(groupS.Render("Where should I look?") + helpS.Render("   ↑/↓ move · enter or space toggles") + "\n\n")
	rows := m.menuRows()
	top, end := window(len(rows)+1, m.cur, m.h-11) // +1 for the static line below
	line := func(k int, text string) {
		if k >= top && k < end {
			b.WriteString(curMark(k == m.cur) + text + "\n")
		}
	}
	b.WriteString("    " + okS.Render("[x]") + " " + normalS.Render(discoverSelf()) + helpS.Render("  this machine — always scanned") + "\n")
	set := m.enabledPorts()
	for k, r := range rows {
		switch r.kind {
		case "host":
			box := "[ ]"
			if contains(m.cfg.Discover.SSH, m.hosts[r.i]) {
				box = okS.Render("[x]")
			}
			if k == 0 && k >= top {
				b.WriteString(groupS.Render("  SSH hosts") + "\n")
			}
			line(k, box+" "+m.hosts[r.i])
		case "addhost":
			line(k, accS.Render("＋ Add an SSH host…"))
		case "port":
			on := true
			for _, p := range r.ports {
				on = on && set[p]
			}
			box := "[ ]"
			if on {
				box = okS.Render("[x]")
			}
			line(k, fmt.Sprintf("%s %-12s %s", box, r.spec, helpS.Render(r.label)))
		case "addport":
			line(k, accS.Render("＋ Add a port or range…"))
		case "embed":
			emb := "off"
			if m.inclEmb {
				emb = "on"
			}
			line(k, accS.Render("⚙  Include embedding / reranker models: ")+emb)
		}
	}
	if m.editing == "addhost" || m.editing == "addport" {
		label := map[string]string{"addhost": "new SSH host (comma-separated for several)", "addport": "port or range, e.g. 9000 or 9000-9010"}[m.editing]
		fmt.Fprintf(&b, "\n%s %s▏\n", accS.Render(label+" →"), m.editBuf)
	}
	return b.String()
}

func (m model) viewResults() string {
	var b strings.Builder
	sel := 0
	for _, v := range m.sel {
		if v {
			sel++
		}
	}
	fmt.Fprintf(&b, "%s  %s\n", groupS.Render("Scan results"),
		helpS.Render(fmt.Sprintf("%d found · %d already registered · %d skipped · %d of %d selected", len(m.disc.Found), len(m.disc.Plan.Known), len(m.disc.Plan.Skip), sel, len(m.disc.Plan.Add))))
	if m.filter != "" || m.editing == "filter" {
		txt := m.filter
		if m.editing == "filter" {
			txt = m.editBuf + "▏"
		}
		b.WriteString("  filter: " + accS.Render(txt) + "\n")
	}
	for _, n := range m.disc.Notes {
		b.WriteString("  " + helpS.Render(n) + "\n")
	}
	rows := m.visRows()
	budget := m.h - (9 + len(m.disc.Notes))
	if budget < 6 {
		budget = 6
	}
	top := 0
	if len(rows) > budget {
		top = max(0, min(m.cur-budget/2, len(rows)-budget))
	}
	end := min(len(rows), top+budget)
	if top > 0 {
		b.WriteString(helpS.Render(fmt.Sprintf("      ↑ %d more", top)) + "\n")
	}
	for k := top; k < end; k++ {
		r, cur := rows[k], curMark(k == m.cur)
		switch {
		case r.act == "register":
			b.WriteString("\n" + cur + okS.Render(fmt.Sprintf("✔ Register %d selected model(s)", sel)) + "\n")
		case r.act == "back":
			b.WriteString(cur + helpS.Render("← Back to targets") + "\n")
		case r.header:
			idx := m.groupIdx(r.node)
			n := 0
			for _, i := range idx {
				if m.sel[i] {
					n++
				}
			}
			box, arrow := "[ ]", "▾"
			if n == len(idx) && n > 0 {
				box = okS.Render("[x]")
			} else if n > 0 {
				box = accS.Render("[~]")
			}
			if m.collapsed[r.node] {
				arrow = "▸"
			}
			fmt.Fprintf(&b, "%s%s %s %s %s\n", cur, arrow, box, groupS.Render(r.node), helpS.Render(fmt.Sprintf("(%d models, %d selected)", len(idx), n)))
		default:
			e := m.disc.Plan.Add[r.idx]
			box := "[ ]"
			if m.sel[r.idx] {
				box = okS.Render("[x]")
			}
			fmt.Fprintf(&b, "%s    %s %-34s %s %s\n", cur, box, trunc(e.Model, 34), accS.Render(capLetters(e)), descS.Render(capsLabel(e)))
		}
	}
	if end < len(rows) {
		b.WriteString(helpS.Render(fmt.Sprintf("      ↓ %d more", len(rows)-end)) + "\n")
	}
	if len(m.itemRows()) == 0 {
		b.WriteString(descS.Render("  nothing new to register (everything is already known, or the filter matches nothing)") + "\n")
	}
	return b.String()
}

func portOf(e config.Endpoint) string {
	h := e.Remote
	if h == "" {
		parts := strings.SplitN(e.URL, "://", 2)
		h = strings.TrimSuffix(parts[len(parts)-1], "/v1")
	}
	return h[strings.LastIndex(h, ":")+1:]
}

func discoverSelf() string { h, _ := os.Hostname(); return strings.Split(h, ".")[0] }

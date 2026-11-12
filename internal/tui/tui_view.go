package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/tevfik/fleet-router/internal/board"
	"github.com/tevfik/fleet-router/internal/server"
	"github.com/tevfik/fleet-router/internal/setup"
	"github.com/tevfik/fleet-router/internal/update"
)

type integ struct {
	icon, title, desc string
	run               func(m *model) (string, error)
}

func integrations() []integ {
	return []integ{
		{"🤖", "Install for Claude Code", "MCP registration + skill + /fleet-review, /fleet-implement (role table generated from the live configuration)",
			func(m *model) (string, error) {
				exe, err := setup.Exe()
				if err != nil {
					return "", err
				}
				out, err := setup.Claude(exe, m.listen)
				if err != nil {
					return out, err
				}
				return guide(out, setup.ClaudeGuidance)
			}},
		{"🧩", "Install for OpenCode", "providers.fleet (backed up) + fleet-scout/architect/coder/verifier agents + commands",
			func(m *model) (string, error) {
				p, err := m.cl.OpenCode()
				if err != nil {
					return "", err
				}
				path, err := setup.OpenCode(p)
				if err != nil {
					return "", err
				}
				return guide("written: "+path, setup.OpenCodeGuidance)
			}},
		{"🧠", "Install the llama-ring engine", "downloads the engine build that suits this machine (CPU or CUDA) from the package registry and replaces this binary with it (fleet engine install)",
			func(m *model) (string, error) {
				exe, err := setup.Exe()
				if err != nil {
					return "", err
				}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
				defer cancel()
				msg, err := update.EnsureEngine(ctx, exe, "", false, nil)
				if err != nil {
					return "", err
				}
				return msg + " — restart the daemon to use it", nil
			}},
		{"📝", "Refresh guidance", "regenerates the Claude skill and the OpenCode agents from the configuration (after changing roles)",
			func(m *model) (string, error) {
				o, err := guide("", setup.ClaudeGuidance)
				if err != nil {
					return o, err
				}
				return guide(o, setup.OpenCodeGuidance)
			}},
		{"⚙ ", "Install systemd service", "fleet serve starts at login and keeps the SSH tunnels alive",
			func(m *model) (string, error) {
				exe, err := setup.Exe()
				if err != nil {
					return "", err
				}
				p, err := setup.ServiceInstall(exe)
				return "servis: " + p, err
			}},
		{"📦", "Install binary to ~/.local/bin", "copies the running fleet executable to a permanent location",
			func(m *model) (string, error) {
				p, err := setup.InstallBinary("")
				return "kuruldu: " + p, err
			}},
	}
}

// visualLines is how many terminal rows s takes at the model's width (long lines wrap).
func (m model) visualLines(s string) int {
	w := m.w
	if w <= 0 {
		w = 80
	}
	n := 0
	for _, l := range strings.Split(s, "\n") {
		n += wrappedRows(l, w)
	}
	return n
}

// wrappedRows is how many terminal rows one line takes in a w-column terminal. It wraps the way a terminal does:
// escape sequences take no room, a double-width character (CJK, emoji) that does not fit at the end of a row
// moves to the next one, and a combining mark adds nothing — so it is not just ceil(width/w).
func wrappedRows(line string, w int) int {
	if w <= 0 || line == "" {
		return 1
	}
	return strings.Count(ansi.Hardwrap(line, w, true), "\n") + 1
}

// fitBody puts the result note in a sticky spot right under the screen title (a note at the bottom of a long
// screen fell outside the terminal and the action looked dead) and, when the screen plus its footer is taller
// than the terminal, shows only a window of the body around the selected row (the last ▸ line).
func (m model) fitBody(body, footer string) string {
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	head := 0
	if m.sc != scAsk && len(lines) >= 2 && lines[1] == "" {
		head = 2 // the title and the blank line after it
	}
	var note []string
	if m.note != "" {
		text := m.note
		if m.w > 20 { // a long note (an error with its advice) wraps at words: the terminal would cut it at the width
			text = ansi.Wrap(text, m.w-2, "")
		}
		note = append(strings.Split(text, "\n"), "")
	}
	if m.h <= 0 {
		out := append(append(append([]string{}, lines[:head]...), note...), lines[head:]...)
		return strings.Join(out, "\n") + "\n"
	}
	rest := lines[head:]
	headTxt := strings.Join(append(append([]string{}, lines[:head]...), note...), "\n")
	used := m.visualLines(headTxt) + m.visualLines(footer)
	budget := m.h - used
	if headTxt == "" {
		budget = m.h - m.visualLines(footer) + 1
	}
	total := 0
	for _, l := range rest {
		total += m.visualLines(l)
	}
	if total > budget && budget >= 3 && len(rest) > 3 {
		cur := 0
		for i, l := range rest {
			if strings.Contains(l, "▸") {
				cur = i
			}
		}
		var w []string
		for keep := budget - 2; keep >= 1; keep-- { // room for the "more" markers; shrink while wrapped lines overflow
			top, end := window(len(rest), cur, max(keep, 3))
			end = min(end, top+keep)
			if cur >= end {
				top, end = cur-keep+1, cur+1
			}
			w = w[:0]
			if top > 0 {
				w = append(w, helpS.Render(fmt.Sprintf("  ↑ %d more lines", top)))
			}
			w = append(w, rest[top:end]...)
			if end < len(rest) {
				w = append(w, helpS.Render(fmt.Sprintf("  ↓ %d more lines", len(rest)-end)))
			}
			if m.visualLines(strings.Join(w, "\n")) <= budget {
				break
			}
		}
		rest = w
	}
	out := append(append(append([]string{}, lines[:head]...), note...), rest...)
	return strings.Join(out, "\n") + "\n"
}

// versionLine: this binary's version, and a warning when the daemon runs another one (a running daemon keeps the
// old binary until restarted). A daemon too old to report a version is a mismatch too.
func (m model) versionLine() string {
	line := helpS.Render("fleet " + server.Version)
	if m.embedded || (m.st.Listen == "" && m.st.Version == "") { // the daemon built into this TUI always matches; no status yet
		return line
	}
	switch d := m.st.Version; {
	case d == "":
		line += badS.Render("  · the daemon is older (it reports no version) — restart it (systemctl --user restart fleet) to use " + server.Version)
	case d != server.Version:
		line += badS.Render("  · daemon runs " + d + ", this is " + server.Version + " — restart the daemon (systemctl --user restart fleet) so both match")
	}
	return line
}

func (m model) help() string {
	base := "  ↑/↓ select • ↓ at the end or tab → buttons • esc back"
	if m.editing == "newrole" || m.editing == "match" || m.editing == "tags" || m.editing == "group" || m.editing == "fleetport" || m.editing == "runbind" || m.editing == "planpath" || m.editing == "planprofile" || m.editing == "storeadd" || m.editing == "storefill" || strings.HasPrefix(m.editing, "prof") || strings.HasPrefix(m.editing, "hub") || m.editing == "nodespec" {
		return "  type • enter save • esc cancel"
	}
	switch m.sc {
	case scHome:
		return "  ↑/↓ select • enter open • q quit"
	case scDash:
		if m.confirm {
			return "  y yes • n cancel"
		}
		return base + " • / search • c capabilities • t tags • + - slots • g group • f fleet port • b benchmark • p probe • s stop its processes • x remove"
	case scRoles:
		if m.pick {
			return "  ↑/↓ select • enter choose • / search • esc cancel"
		}
		return base + " • enter edit role • n new role • x delete role • T test • u undo • t starter roles • p preferred • f add fallback • c clear fallbacks • H config history"
	case scCaps:
		return "  ↑/↓ move • enter/space toggle • esc back"
	case scRoleEdit:
		if m.pick {
			return "  ↑/↓ select • enter add • / search • esc cancel"
		}
		return base + " • [ ] reorder • a add model • x remove • + - parallel • < > priority • o overflow • s strategy • z request size • m selectors • u undo"
	case scDiscover:
		if m.editing != "" {
			return "  type • enter save • esc cancel"
		}
		if m.focus == 0 || m.disc == nil {
			return "  ↑/↓ move • enter/space toggle • x remove host • esc back"
		}
		return "  ↑/↓ move • enter/space select • ←/→ fold • / filter • a all • n none • esc back"
	case scBoard:
		return "  1-4 switch view • / search • n next machine • b benchmark highlighted • B benchmark all healthy • esc back"
	case scNodes:
		return base + " • + connect new machine • j join hub • d leave the line / rejoin (this machine) • enter its models • b benchmark this device • p probe this device's models • a apply probe to caps • e declare its spec (addr, memory, speeds) • x remove the node • f forget its spec • u undo config • H config history"
	case scRuns:
		if m.confirm {
			if m.pend != nil && m.pend.kind == "rm-stale-run" {
				return "  y close the record and delete it • n cancel"
			}
			if m.pend != nil {
				return "  y stop • n cancel"
			}
			return "  y delete • n cancel"
		}
		return base + " • s stop the run's processes (master, streams, workers) and close it • e close the record (and stop in the background) • x delete an ended (or dead) run's record • P prune all ended records • w watchdog check now • b bind it to the endpoint that serves it (its engine is then followed)"
	case scProfiles:
		if m.confirm {
			return "  y delete • n cancel"
		}
		return base + " • enter deploy • A deploy with auto layout • a autostart on/off • s stop • n dry run • b bench • m measure • e edit • y YAML • c save copy • o export • i import • x delete"
	case scEconomy:
		return base + " • enter/space toggle setting • j join/leave Fleet Hub"
	case scHub:
		return "  ↑/↓ select • enter run • c connect/disconnect • k API key • r refresh models • esc back"
	case scStore:
		if m.pick {
			return "  ↑/↓ select • enter choose • esc cancel"
		}
		if m.confirm {
			return "  y fetch • n cancel"
		}
		return base + " • n add a file or directory (on this machine; a split model: every shard) • f fetch the file (all shards) to a machine • c fill a machine's worker cache • a abort transfer • x forget the highlighted file"
	case scPlan:
		if m.lay != nil {
			return "  ↑/↓ machine • ←/→ one block fewer/more (H/L five) • 0 the rest of the blocks • a add a machine • x remove • K/J move in the ring • o output projection on the master • enter use this split • esc close"
		}
		if m.pick {
			return "  ↑/↓ choose the candidate • enter take it • esc cancel"
		}
		return base + " • 1-4 jump to section • Tab next section • ←/→ change the focused value • enter plan the highlighted file • c choose candidate • S split the layers by hand • e explain • r plan again • f fetch • w record run • t test in chat • d deploy • i dry run • b via hub • a save as profile • L profiles • p path"
	case scUsage:
		return "  / search • g change grouping • p change period • totals are kept across restarts (30 days) • esc back"
	case scActivity:
		return "  ↑/↓ browse • red = failed • amber ↪N = failover (N endpoints tried) • ↗ remote caller • esc back"
	case scInteg:
		return base + " • enter run (y to confirm)"
	case scAsk:
		return "  type prompt • enter send • tab switch target • enter on empty or ctrl+a full agenttui • esc back"
	}
	return base
}

func (m model) summary() string {
	ok, bad := 0, 0
	for _, e := range m.st.Endpoints {
		if e.OK {
			ok++
		} else if e.Known {
			bad++
		}
	}
	return helpS.Render(fmt.Sprintf("  %d endpoint · ", len(m.st.Endpoints))) + okS.Render(fmt.Sprintf("%d ✓", ok)) + " " + badS.Render(fmt.Sprintf("%d ✗", bad)) +
		helpS.Render(fmt.Sprintf(" · %d roles", len(m.st.Roles))) + "\n"
}

// ref shows an endpoint by what it serves ("node/model") instead of its configuration name.
func (m model) ref(name string) string {
	for _, e := range m.st.Endpoints {
		if e.Name == name {
			return e.Ref()
		}
	}
	return name
}

func (m model) refs(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = m.ref(n)
	}
	return out
}

// peerMark flags models reached through another fleet.
func peerMark(e server.EpStatus) string {
	if e.Peer != "" {
		return "⇄ "
	}
	return ""
}

func mark(e server.EpStatus) string {
	switch {
	case !e.Known:
		return helpS.Render("?")
	case e.OK:
		return okS.Render("✓")
	}
	return badS.Render("✗")
}

func curMark(on bool) string {
	if on {
		return activeS.Render("▸ ")
	}
	return "  "
}

// trunc shortens s to n terminal columns (wide characters count 2, escape sequences 0), ending in "…".
func trunc(s string, n int) string {
	if n <= 0 || ansi.StringWidth(s) <= n {
		return s
	}
	return ansi.Truncate(s, n, "…")
}

func (m model) viewInteg() string {
	var b strings.Builder
	for i, it := range integrations() {
		st, ds := normalS.PaddingLeft(0), descS.PaddingLeft(4)
		if i == m.cur {
			st, ds = activeS.PaddingLeft(0), actDesc.PaddingLeft(4)
		}
		fmt.Fprintf(&b, "%s%s\n    %s\n", curMark(i == m.cur), st.Render(it.icon+" "+it.title), ds.Render(it.desc))
		if m.confirm && i == m.cur {
			b.WriteString("    " + accS.Render("Run it? (y/n)") + "\n")
		}
		b.WriteString("\n")
	}
	b.WriteString(groupS.Render("Embeddings") + "\n  base URL for RAG and embedding tools (OpenAI-compatible): " + m.cl.Base + "/v1\n  (point an embedding endpoint at it with `fleet role set embed <endpoint>`; model name = embed)\n")
	return b.String()
}

// ---- button bar -----------------------------------------------------------------------------

type button struct{ label, key string }

// buttons returns the on-screen buttons of the current screen. Each one triggers the same action as
// its letter shortcut, so keyboard shortcuts and buttons can never drift apart.
func (m model) buttons() []button {
	back := button{"← Back", "esc"}
	switch m.sc {
	case scDash:
		return []button{{"Search", "/"}, {"Capabilities", "c"}, {"Tags", "t"}, {"Group…", "g"}, {"Fleet port…", "f"}, {"Slots −", "-"}, {"Slots +", "+"}, {"Benchmark", "b"}, {"Probe", "p"}, {"Stop processes", "s"}, {"Remove", "x"}, back}
	case scNodes:
		leave := "Leave the line"
		if m.st.Draining {
			leave = "Rejoin the line"
		}
		return []button{{"Connect machine…", "+"}, {"Join Hub…", "j"}, {"Models", "enter"}, {leave, "d"}, {"Benchmark this device", "b"}, {"Probe its models", "p"}, {"Apply probe", "a"},
			{"Declare spec…", "e"}, {"Remove node", "x"}, {"Forget spec", "f"}, {"Undo config", "u"}, {"Config history", "H"}, back}
	case scProfiles:
		return m.profileButtons()
	case scRuns:
		if m.confirm {
			if m.pend != nil && m.pend.kind == "rm-stale-run" {
				return []button{{"Yes, delete it", "y"}, {"No", "n"}}
			}
			if m.pend != nil {
				return []button{{"Yes, stop them", "y"}, {"No", "n"}}
			}
			return []button{{"Yes, delete them", "y"}, {"No", "n"}}
		}
		return []button{{"Stop processes", "s"}, {"End run (record)", "e"}, {"Bind to endpoint…", "b"}, {"Delete record", "x"}, {"Prune ended…", "P"}, {"Watchdog check", "w"}, back}
	case scStore:
		if m.pick {
			return []button{{"Choose", "enter"}, {"Cancel", "esc"}}
		}
		if m.confirm {
			return []button{{"Yes, fetch", "y"}, {"No", "n"}}
		}
		return []button{{"Add file…", "n"}, {"Fetch to…", "f"}, {"Fill cache…", "c"}, {"Abort transfer", "a"}, {"Forget file", "x"}, back}
	case scPlan:
		if m.pick {
			return []button{{"Choose", "enter"}, {"Cancel", "esc"}}
		}
		if m.lay != nil {
			if m.lay.adding {
				return []button{{"Add this machine", "enter"}, {"Cancel", "esc"}}
			}
			return []button{{"Use this split", "enter"}, {"Add machine…", "a"}, {"Remove machine", "x"}, {"Block −", "-"}, {"Block +", "+"}, {"Give it the rest", "0"}, {"Output on master", "o"}, {"Close editor", "esc"}}
		}
		return []button{{"Plan this file", "enter"}, {"Choose candidate…", "c"}, {"Explain", "e"}, {"Type a path…", "p"}, {"Split by hand…", "S"}, {"Again", "r"}, {"Fetch what the plan needs", "f"}, {"Record as run", "w"},
			{"Save as profile…", "a"}, {"Profiles", "L"}, {"Test in chat", "t"}, {"Via hub: " + map[bool]string{true: "on", false: "off"}[m.planViaHub], "b"}, {"Dry run", "i"}, {"Deploy", "d"}, back}
	case scActivity:
		return []button{back}
	case scUsage:
		return []button{{"Search", "/"}, {"Group by", "g"}, {"Period", "p"}, back}
	case scBoard:
		return []button{{"Search", "/"}, {"Speed", "1"}, {"Usage today", "2"}, {"By role", "3"}, {"Daily", "4"}, {"Machine", "n"},
			{"Benchmark highlighted", "b"}, {"Benchmark all healthy", "B"}, back}
	case scRoles:
		if m.pick {
			return []button{{"Search", "/"}, {"Cancel", "esc"}}
		}
		if m.confirm {
			return []button{{"Yes, delete", "y"}, {"No", "n"}}
		}
		return []button{{"Edit role", "enter"}, {"New role", "n"}, {"Delete role", "x"}, {"Test", "T"}, {"Undo", "u"}, {"Starter roles", "t"}, {"Set preferred", "p"}, {"Add fallback", "f"}, {"Clear fallbacks", "c"}, {"Config history", "H"}, back}
	case scRoleEdit:
		if m.pick {
			return []button{{"Search", "/"}, {"Cancel", "esc"}}
		}
		return []button{{"Move up", "["}, {"Move down", "]"}, {"Add model", "a"}, {"Remove", "x"}, {"Parallel −", "-"}, {"Parallel +", "+"},
			{"Priority −", "<"}, {"Priority +", ">"}, {"Overflow", "o"}, {"Strategy", "s"}, {"Request size", "z"}, {"Selectors", "m"}, {"Undo", "u"}, back}
	case scInteg:
		if m.confirm {
			return []button{{"Yes, run it", "y"}, {"No", "n"}}
		}
		return []button{{"Run selected", "enter"}, back}
	case scAsk:
		if m.pick {
			return []button{{"Select", "enter"}, {"Cancel", "esc"}}
		}
		return []button{{"Send", "enter"}, {"Target model/role", "t"}, {"Clear chat", "ctrl+l"}, back}
	case scDiscover:
		if m.focus == 0 {
			bs := []button{{"🔍 Scan now", "d"}, {"Remove host", "x"}}
			if m.disc != nil {
				bs = append(bs, button{"Last results", "L"})
			}
			return append(bs, back)
		}
		n := 0
		for _, v := range m.sel {
			if v {
				n++
			}
		}
		return []button{{fmt.Sprintf("✔ Register %d selected", n), "w"}, {"Select all", "a"}, {"Select none", "n"}, {"Filter…", "/"},
			{"Ignore host:port", "i"}, {"Embedding models", "e"}, {"← Back to targets", "esc"}}
	}
	return nil
}

// barKey implements the focus zones: ↓ at the end of a list (or tab) moves to the bar, ←/→ pick a
// button, enter/space press it, ↑/↓ keep browsing the list, tab goes back to it.
func (m model) barKey(s string) (model, tea.Cmd, bool) {
	bs := m.buttons()
	if len(bs) == 0 || m.editing != "" {
		return m, nil, false
	}
	if !m.inBar {
		folds := m.sc == scDiscover && m.focus == 1 // ←/→ fold groups on the scan results
		switch {
		case s == "tab", (s == "down" || s == "j") && m.cur >= m.listLen()-1:
			m.inBar, m.barCur = true, min(m.barCur, len(bs)-1)
		case (s == "up" || s == "k") && m.cur <= 0: // the bar sits below the list: wrap around from the top
			m.inBar, m.barCur = true, min(m.barCur, len(bs)-1)
		case !folds && (s == "right" || s == "l"):
			m.inBar, m.barCur = true, 0
		case !folds && (s == "left" || s == "h"):
			m.inBar, m.barCur = true, len(bs)-1
		default:
			return m, nil, false
		}
		return m, nil, true
	}
	m.barCur = min(m.barCur, len(bs)-1)
	switch s {
	case "left", "h":
		m.barCur = max(0, m.barCur-1)
	case "right", "l":
		m.barCur = min(len(bs)-1, m.barCur+1)
	case "tab":
		m.inBar = false
	case "up", "k", "down", "j":
		return m, nil, false // keep browsing the list while the bar stays focused
	case "enter", " ", "space":
		before := m
		press := m
		press.inBar = false // the button's key is handled as a plain key; with the bar still focused an "enter" button would press itself forever
		nm, cmd := press.keyStr(bs[m.barCur].key, bs[m.barCur].key)
		mm := press.navAfter(bs[m.barCur].key, nm).(model)
		if mm.sc != before.sc || mm.focus != before.focus || mm.pick != before.pick || mm.confirm != before.confirm {
			mm.inBar, mm.barCur = false, 0 // moved to another screen/mode: back to its list
		} else {
			mm.inBar = true // stay on the bar so +/− can be pressed repeatedly
		}
		return mm, cmd, true
	default:
		return m, nil, false // letter shortcuts still work from the bar
	}
	return m, nil, true
}

func (m model) viewBar() string {
	bs := m.buttons()
	if len(bs) == 0 {
		return ""
	}
	width := m.w
	if width <= 0 {
		width = 80
	}
	focus := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFFFFF")).Background(cPrimary)
	var lines []string
	cur, curW := "", 0
	for i, b := range bs {
		label := " " + b.label + " "
		w := ansi.StringWidth(label) + 2
		if curW+w > width-2 && cur != "" {
			lines = append(lines, cur)
			cur, curW = "", 0
		}
		if m.inBar && i == min(m.barCur, len(bs)-1) {
			cur += focus.Render(label) + " "
		} else {
			cur += helpS.Render("["+b.label+"]") + " "
			w = ansi.StringWidth(b.label) + 3
		}
		curW += w
	}
	lines = append(lines, cur)
	return "  " + strings.Join(lines, "\n  ")
}

// viewActivity lists the router's recent requests, newest first; the highlighted one is shown in full below.
func (m model) viewActivity() string {
	var b strings.Builder
	n := len(m.reqlog)
	if n == 0 {
		return helpS.Render("  no requests yet — they appear here as clients use the router (refreshes every 3 s)") + "\n"
	}
	at := func(i int) server.LogEntry { return m.reqlog[n-1-i] } // i-th newest
	top, end := window(n, m.cur, m.h-14)
	width := max(40, m.w-4)
	for i := top; i < end; i++ {
		e := at(i)
		line := trunc(board.LogLine(e), width)
		switch {
		case e.Status >= 400:
			line = badS.Render(line)
		case e.Attempts > 1:
			line = accS.Render(line)
		}
		b.WriteString(curMark(i == m.cur) + line + "\n")
	}
	if end < n {
		b.WriteString(helpS.Render(fmt.Sprintf("      ↓ %d older", n-end)) + "\n")
	}
	if m.cur < n {
		e := at(m.cur)
		b.WriteString("\n" + groupS.Render(e.Path) + helpS.Render(fmt.Sprintf(" · asked for %q", e.Model)))
		if e.Role != "" {
			b.WriteString(helpS.Render(" (role)"))
		}
		b.WriteString("\n")
		d := fmt.Sprintf("  attempts %d · queue %dms · total %s", e.Attempts, e.QueueMS, msStr(e.DurMS))
		if e.Out > 0 {
			d += fmt.Sprintf(" · %d tokens in, %d out", e.In, e.Out)
		}
		b.WriteString(d + "\n")
		if e.Err != "" {
			b.WriteString("  " + badS.Render(trunc(strings.Join(strings.Fields(e.Err), " "), width)) + "\n")
		}
	}
	return b.String()
}

var (
	usageBy    = []string{"client", "role", "model", "endpoint", "day"}
	usageSince = []string{"24h", "7d", "30d"}
)

// viewUsage shows who used what over the chosen period; g changes the grouping, p the period.
// usageRows are the usage rows the search text leaves.
func (m model) usageRows() []server.UsageRow {
	if m.filter == "" {
		return m.usage
	}
	var out []server.UsageRow
	for _, r := range m.usage {
		if wordsMatch(r.Key+" "+r.Sub, m.filter) {
			out = append(out, r)
		}
	}
	return out
}

func (m model) viewUsage() string {
	var b strings.Builder
	b.WriteString(helpS.Render(fmt.Sprintf("  by %s · last %s", usageBy[m.useBy], usageSince[m.useSince])) + "\n\n")
	b.WriteString(m.searchLine())
	rows := m.usageRows()
	if len(rows) == 0 {
		msg := "  no traffic recorded in this period (refreshes every 3 s)"
		if m.filter != "" {
			msg = "  nothing matches the search — esc clears it"
		}
		return b.String() + helpS.Render(msg) + "\n"
	}
	width := m.w - 4
	if width <= 0 {
		width = 100
	}
	lines := board.RenderUsageWidth(rows, usageBy[m.useBy], m.w)
	b.WriteString("    " + groupS.Render(trunc(lines[0], width)) + "\n")
	body := lines[1:]
	top, end := window(len(body), m.cur, m.h-14)
	for i := top; i < end; i++ {
		l := trunc(body[i], width)
		if rows[i].Errors > 0 {
			l = accS.Render(l)
		}
		b.WriteString(curMark(i == m.cur) + l + "\n")
	}
	if end < len(body) {
		b.WriteString(helpS.Render(fmt.Sprintf("      ↓ %d more", len(body)-end)) + "\n")
	}
	return b.String()
}

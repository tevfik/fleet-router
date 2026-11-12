package tui

import (
	tea "charm.land/bubbletea/v2"
	"fmt"
	"github.com/tevfik/fleet-router/internal/board"
	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/discover"
	"strconv"
	"strings"
)

func (m model) back() model {
	m.sc, m.cur, m.note, m.pick, m.confirm, m.editing, m.filter = scHome, 0, "", false, false, "", ""
	m.pend = nil
	return m
}

func (m model) key(k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	nm, cmd := m.keyStr(k.String(), k.Text)
	return m.navAfter(k.String(), nm), cmd
}

// navEntry is a screen left by going forward, and where its cursor was.
type navEntry struct {
	sc  screen
	cur int
}

// navAfter keeps the way back. A key that moved to another screen (not esc/q) remembers the one it left; esc/q that
// fell to the home menu go to the screen before instead (Nodes → its models → esc is Nodes, not Home).
func (m model) navAfter(s string, nm tea.Model) tea.Model {
	n, ok := nm.(model)
	if !ok {
		return nm
	}
	if n.sc == m.sc {
		return n
	}
	if m.sc == scHome {
		n.homeCur = m.cur // the entry the screen was opened from: esc comes back to it
	}
	if s != "esc" && s != "q" {
		if len(n.nav) < 16 {
			n.nav = append(append([]navEntry{}, n.nav...), navEntry{m.sc, m.cur})
		}
		return n
	}
	for len(n.nav) > 0 {
		top := n.nav[len(n.nav)-1]
		n.nav = n.nav[:len(n.nav)-1]
		if top.sc == n.sc { // the screen's own way back already went there
			break
		}
		if n.sc == scHome && top.sc != m.sc {
			n.sc, n.cur, n.filter = top.sc, top.cur, ""
			break
		}
	}
	if n.sc == scHome && m.sc != scHome {
		n.cur = n.homeCur // back on the start menu: on the entry that was left (a second esc goes to the top)
	}
	return n
}

// keyStr handles one key (by name, plus its typed text). Buttons call it with their shortcut key.
func (m model) keyStr(s, text string) (tea.Model, tea.Cmd) {
	if s == "delete" && m.editing == "" { // x is the delete/remove key on every screen; the Delete key is its twin
		s, text = "x", "x"
	}
	if m.editing != "" { // text input
		switch s {
		case "enter":
			if m.editing == "filter" {
				m.editing = ""
				return m, nil
			}
			if m.editing != "addport" && m.editing != "addhost" { // every other editor has its own finish (the hub ones were missing here: enter added an ssh host)
				return m.finishEdit()
			}
			ports, ssh := m.cfg.Discover.Ports, m.cfg.Discover.SSH
			if m.editing == "addport" {
				ps := append(discover.ParsePorts(ports), discover.ParsePorts(m.editBuf)...)
				ports = discover.FormatPorts(ps)
			} else { // addhost: several comma-separated hosts allowed; added ones start enabled
				for _, h := range strings.Split(m.editBuf, ",") {
					if h = strings.TrimSpace(h); h != "" && !contains(ssh, h) {
						ssh = append(append([]string{}, ssh...), h)
					}
				}
			}
			m.editing = ""
			m.cfg.Discover = config.Discover{Ports: ports, SSH: ssh, Ignore: m.cfg.Discover.Ignore}
			m.rebuildHosts()
			return m, m.saveDiscover(ports, ssh, "discovery settings saved")
		case "esc":
			if m.editing == "filter" {
				m.filter = ""
			}
			m.editing = ""
		case "backspace":
			if r := []rune(m.editBuf); len(r) > 0 {
				m.editBuf = string(r[:len(r)-1])
			}
		default:
			if text != "" {
				m.editBuf += text
			}
		}
		if m.editing == "filter" {
			m.filter, m.cur = m.editBuf, 0 // live filtering
		}
		return m, nil
	}
	if m.sc == scPlan && m.lay != nil && m.editing == "" && s != "esc" && s != "ctrl+c" {
		m.inBar = false // the editor takes the keys while it is open
		return m.keyLayers(s)
	}
	if m.sc == scPlan && m.profOpen && s != "esc" && s != "ctrl+c" {
		// the profiles panel takes the keys while it is open: ↑/↓ used to move the file list behind it (or jump to
		// the button bar), so x deleted a profile other than the one the user had moved to
		m.inBar = false
		return m.keyProfiles(s)
	}
	if m.sc == scPlan && m.editing == "" && !m.pick && !m.confirm && !m.profOpen && len(s) == 1 && s[0] >= '1' && s[0] <= '4' {
		// 1 model files · 2 runtime parameters · 3 candidates / devices · 4 the deploy line
		if sec := int(s[0] - '1'); sec <= m.planLastSection() && (sec != 3 || m.planLastSection() == 3) {
			m.inBar, m.planSection = false, sec
			if sec == 1 {
				if m.planParamIdx() == 0 {
					m.planParamCur = 0
				}
			}
			return m, nil
		}
		return m, nil
	}
	if m.sc == scPlan && !m.inBar && m.editing == "" && !m.pick {
		// ↓/tab walk the sections in order: files → sliders → candidates → deploy → the button bar.
		down := s == "down" || s == "j"
		last := m.planLastSection()
		switch {
		case m.planSection == 0:
			if s == "tab" || (down && len(m.planFiles) > 0 && m.cur >= len(m.planFiles)-1) {
				return m.keyPlan(s)
			}
		case m.planSection == last && (s == "tab" || (down && (last >= 2 || m.planParamCur == m.planParamLast()))):
			// leaves the last section: the bar takes it (below)
		default:
			switch s {
			case "up", "k", "down", "j", "left", "h", "right", "l", "enter", "tab", "shift+tab", "backtab":
				return m.keyPlan(s)
			}
		}
	}
	if m.sc == scAsk && !m.inBar {
		return m.keyAsk(s, text)
	}
	if nm, cmd, handled := m.barKey(s); handled {
		return nm, cmd
	}
	switch s {
	case "ctrl+c":
		return m, tea.Quit
	case "q":
		if m.sc == scHome {
			return m, tea.Quit
		}
		if m.sc == scRoleEdit && !m.pick {
			m.sc, m.cur = scRoles, m.roleCur
			return m, nil
		}
		if m.sc == scDiscover && m.focus == 1 {
			m.focus, m.cur = 0, m.otherCur
			return m, nil
		}
		m = m.back()
		return m, nil
	case "esc":
		switch {
		case m.lay != nil && m.lay.adding:
			ed := *m.lay
			ed.adding = false
			m.lay = &ed
		case m.lay != nil:
			m.lay = nil // the layer editor closes first
		case m.profOpen:
			m.profOpen, m.pend = false, nil // the profiles panel closes first (a delete it asked about is dropped)
		case m.filter != "" && !m.drilledFromNodes() && (m.pick || m.sc == scDash || m.sc == scUsage || m.sc == scBoard):
			m.filter, m.cur = "", 0 // first esc clears the search, the next one leaves
		case m.pick:
			m.pick = false
			if m.sc == scStore {
				m.cur = m.storeCur
			}
			if m.sc == scPlan {
				m.cur = m.planCur
			}
		case m.confirm:
			// the same as n: the question and the action waiting for its y both go (a later y must do nothing)
			m.confirm, m.pend, m.note = false, nil, ""
			if m.sc == scRuns || m.sc == scDash {
				m.outText = "" // the dry run that was asked about
			}
		case m.sc == scCaps:
			m.sc, m.cur = scDash, 0
		case m.sc == scRoleEdit:
			m.sc, m.cur = scRoles, m.roleCur
		case m.sc == scDiscover && m.focus == 1:
			m.focus, m.cur = 0, m.otherCur
		case m.sc == scPlan && m.planSection > 0:
			m.planSection = 0
		case m.sc == scHub:
			prev := m.hubPrev
			if prev == 0 {
				prev = scHome
			}
			m.sc, m.cur = prev, 0
		case m.sc != scHome:
			m = m.back()
		default:
			m.cur = 0 // on the start menu esc goes to its top (q quits)
		}
		return m, nil
	case "up", "k":
		if m.letterIsShortcut(s) {
			break // the screen's own k/j (advertised in its list) wins over vim-style navigation; the arrows still move
		}
		if m.cur > 0 {
			m.cur--
		}
		return m, nil
	case "down", "j":
		if m.letterIsShortcut(s) {
			break
		}
		if m.cur < m.listLen()-1 {
			m.cur++
		}
		return m, nil
	}
	switch m.sc {
	case scHome:
		if s == "enter" {
			switch homeItems()[m.cur].title {
			case "Dashboard":
				m.sc = scDash
			case "Chat / Ask":
				return m.launchAgentTUI("")
			case "Nodes":
				m.sc, m.cur, m.note, m.nodeHost = scNodes, 0, "", nil
				return m, m.fetch()
			case "Fleet Hub":
				m.hubPrev = scHome
				m.sc, m.cur, m.note = scHub, 0, ""
				m.hubShowModels = true
				return m, tea.Batch(m.fetch(), m.fetchRemoteHubModels())
			case "Plan":
				return m.enterPlan(), nil
			case "Profiles":
				m = m.enterProfiles()
				return m, nil
			case "Model files":
				m = m.enterStore()
				return m, m.fetch()
			case "Runs":
				m = m.enterRuns()
				return m, m.fetch()
			case "Roles":
				m.sc = scRoles
			case "Discover":
				m.sc = scDiscover
				m.cur = 0
				m.loadHosts()
				m.focus, m.otherCur = 0, 0
			case "Leaderboard":
				m.sc, m.cur, m.note, m.bTab, m.bData, m.bWeek = scBoard, 0, "", 0, nil, nil
				return m, m.fetch() // load the usage behind the top cards right away
			case "Activity":
				m.sc, m.cur, m.note = scActivity, 0, ""
				return m, m.fetch() // load the log right away instead of on the next tick
			case "Usage":
				m.sc, m.cur, m.note, m.usage = scUsage, 0, "", nil
				return m, m.fetch()
			case "Economy & Wallet":
				m.sc, m.cur, m.note = scEconomy, 0, ""
				return m, m.fetch()
			case "Integrations":
				m.sc = scInteg
			case "Quit":
				return m, tea.Quit
			}
			m.cur, m.note = 0, ""
		}
	case scUsage:
		switch s {
		case "/":
			m.editing, m.editBuf = "filter", m.filter
			return m, nil
		case "g":
			m.useBy, m.cur = (m.useBy+1)%len(usageBy), 0
			return m, m.fetch()
		case "p":
			m.useSince = (m.useSince + 1) % len(usageSince)
			return m, m.fetch()
		}
	case scBoard:
		rows := m.speedRows()
		switch s {
		case "/":
			m.editing, m.editBuf = "filter", m.filter
			return m, nil
		case "1", "2", "3", "4":
			m.bTab, m.cur, m.bData = int(s[0]-'1'), 0, nil
			return m, m.fetch()
		case "n":
			nodes := m.boardNodes()
			for i, n := range nodes {
				if n == m.bNode {
					m.bNode = nodes[(i+1)%len(nodes)]
					break
				}
			}
			m.cur = 0
			return m, nil
		case "b":
			if m.cur < len(rows) {
				return m, m.bench([]string{rows[m.cur].Name})
			}
		case "B":
			return m, m.bench(nil)
		}
	case scDash:
		if nm, cmd, ok := m.confirmPending(s); ok {
			return nm, cmd
		}
		if s == "/" {
			m.editing, m.editBuf = "filter", m.filter
			return m, nil
		}
		if s == "t" && m.cur < len(m.dashEps()) {
			e := m.dashEps()[m.cur]
			m.editing, m.editFor, m.editBuf = "tags", e.Name, strings.Join(e.Tags, " ")
			return m, nil
		}
		if s == "f" && m.cur < len(m.dashEps()) {
			e := m.dashEps()[m.cur]
			m.editing, m.editFor, m.editBuf = "fleetport", e.Name, board.FleetPortText(e)
			return m, nil
		}
		if s == "g" && m.cur < len(m.dashEps()) {
			e := m.dashEps()[m.cur]
			m.editing, m.editFor, m.editBuf = "group", e.Name, board.GroupText(e)
			return m, nil
		}
		if s == "c" && m.cur < len(m.dashEps()) {
			m.capName, m.sc, m.cur = m.dashEps()[m.cur].Name, scCaps, 0
			return m, nil
		}
		if s == "p" && m.cur < len(m.dashEps()) {
			return m, m.probe([]string{m.dashEps()[m.cur].Name})
		}
		if s == "b" && m.cur < len(m.dashEps()) && !m.dashEps()[m.cur].IsEmbed() {
			return m, m.bench([]string{m.dashEps()[m.cur].Name})
		}
		if (s == "+" || s == "=" || s == "-") && m.cur < len(m.dashEps()) {
			e := m.dashEps()[m.cur]
			n := e.Slots
			if s == "-" {
				n = max(0, n-1)
			} else {
				n++
			}
			return m, func() tea.Msg {
				if err := m.cl.SetSlots(e.Name, n); err != nil {
					return noteMsg(badS.Render("error: " + err.Error()))
				}
				return noteMsg(okS.Render(fmt.Sprintf("✓ %s slot = %s", e.Name, slotStr(n))))
			}
		}
		if s == "x" && m.cur < len(m.dashEps()) {
			return m.askRemoveEndpoint(m.dashEps()[m.cur].Name), nil
		}
		if s == "s" && m.cur < len(m.dashEps()) {
			return m, m.previewStop("stop-ep", m.dashEps()[m.cur].Name)
		}
	case scRoles:
		return m.keyRoles(s)
	case scRoleEdit:
		return m.keyRoleEdit(s)
	case scCaps:
		return m.keyCaps(s)
	case scDiscover:
		return m.keyDiscover(s)
	case scNodes:
		return m.keyNodes(s)
	case scPlan:
		return m.keyPlan(s)
	case scStore:
		return m.keyStore(s)
	case scRuns:
		return m.keyRuns(s)
	case scProfiles:
		return m.keyProfilesScreen(s)
	case scEconomy:
		return m.keyEconomy(s)
	case scHub:
		return m.keyHub(s)
	case scInteg:
		its := integrations()
		switch {
		case s == "enter" && !m.confirm:
			m.confirm, m.note = true, ""
		case m.confirm && (s == "y" || s == "enter"):
			it := its[m.cur]
			m.note = "working…"
			return m, func() tea.Msg { out, err := it.run(&m); return actMsg{out, err} }
		case m.confirm && s == "n":
			m.confirm = false
		}
	case scAsk:
		switch s {
		case "esc":
			m.sc, m.cur = scHome, 0
			return m, nil
		case "tab", "right", "l":
			targets := m.askTargets()
			for i, t := range targets {
				if t == m.askTarget {
					m.askTarget = targets[(i+1)%len(targets)]
					break
				}
			}
			return m, nil
		case "left", "h":
			targets := m.askTargets()
			for i, t := range targets {
				if t == m.askTarget {
					m.askTarget = targets[(i-1+len(targets))%len(targets)]
					break
				}
			}
			return m, nil
		case "c":
			if m.askInput == "" {
				m.askLog = nil
				return m, nil
			}
			m.askInput += "c"
			return m, nil
		case "t":
			if m.askInput == "" {
				targets := m.askTargets()
				for i, t := range targets {
					if t == m.askTarget {
						m.askTarget = targets[(i+1)%len(targets)]
						break
					}
				}
				return m, nil
			}
			m.askInput += "t"
			return m, nil
		case "enter":
			if strings.TrimSpace(m.askInput) != "" && !m.askBusy {
				p := m.askInput
				m.askInput = ""
				m.askBusy = true
				return m, m.runAsk(m.askTarget, p)
			}
			return m, nil
		case "backspace":
			if len(m.askInput) > 0 {
				r := []rune(m.askInput)
				m.askInput = string(r[:len(r)-1])
			}
			return m, nil
		default:
			if len(text) > 0 && text != "\n" && text != "\r" {
				m.askInput += text
			}
			return m, nil
		}
	}
	return m, nil
}

func (m model) keyRoles(s string) (tea.Model, tea.Cmd) {
	roles := m.roleNames()
	if m.pick {
		if s == "/" {
			m.editing, m.editBuf = "filter", m.filter
			return m, nil
		}
		eps := m.chatEps()
		if s == "enter" && m.cur < len(eps) && m.roleCur < len(roles) {
			role, ep := roles[m.roleCur], eps[m.cur].Name
			r := m.st.Roles[role]
			pref, fb := r.Preferred, append([]string{}, r.Fallback...)
			if m.pickMode == "preferred" {
				pref = ep
			} else {
				fb = append(fb, ep)
			}
			m.pick, m.cur, m.filter = false, m.roleCur, ""
			cl, mode := m.cl, m.pickMode
			return m, func() tea.Msg {
				if err := cl.SetRole(role, pref, fb); err != nil {
					return noteMsg(badS.Render("error: " + err.Error()))
				}
				return noteMsg(okS.Render(fmt.Sprintf("✓ %s → %s (%s) — applied hot", role, ep, mode)))
			}
		}
		return m, nil
	}
	if s == "t" {
		return m, m.suggestRoles()
	}
	if s == "n" && !m.confirm {
		m.editing, m.editFor, m.editBuf = "newrole", "", ""
		return m, nil
	}
	if s == "u" && !m.confirm {
		return m, m.undoConfig()
	}
	if s == "H" && !m.confirm {
		return m, m.historyCmd()
	}
	if s == "T" && !m.confirm && m.cur < len(roles) {
		return m, m.testRole(roles[m.cur])
	}
	if len(roles) == 0 || m.cur >= len(roles) {
		return m, nil
	}
	if m.confirm { // delete confirmation
		if s == "y" || s == "x" || s == "enter" {
			role, cl := roles[m.cur], m.cl
			m.confirm = false
			return m, func() tea.Msg {
				if err := cl.DeleteRole(role); err != nil {
					return noteMsg(badS.Render("error: " + err.Error()))
				}
				return noteMsg(okS.Render("✓ role deleted: " + role))
			}
		}
		m.confirm = false
		return m, nil
	}
	if s == "x" {
		m.confirm, m.note = true, badS.Render("delete role "+roles[m.cur]+"? y = yes, any other key = no")
		return m, nil
	}
	switch s {
	case "t":
		return m, m.suggestRoles()
	case "enter":
		m.sc, m.roleCur, m.cur, m.route = scRoleEdit, m.cur, 0, nil
		return m, m.fetch() // the route table right away, not on the next tick
	case "p":
		m.pick, m.pickMode, m.roleCur, m.cur = true, "preferred", m.cur, 0
	case "f":
		m.pick, m.pickMode, m.roleCur, m.cur = true, "fallback", m.cur, 0
	case "c":
		role := roles[m.cur]
		r := m.st.Roles[role]
		return m, func() tea.Msg {
			if err := m.cl.SetRole(role, r.Preferred, nil); err != nil {
				return noteMsg(badS.Render("error: " + err.Error()))
			}
			return noteMsg(okS.Render("✓ fallbacks cleared: " + role))
		}
	}
	return m, nil
}

// finishEdit applies the "newrole" / "match" / "tags" text input.
func (m model) finishEdit() (tea.Model, tea.Cmd) {
	kind, target, fields := m.editing, m.editFor, strings.Fields(m.editBuf)
	buf := strings.TrimSpace(m.editBuf)
	m.editing, m.editBuf = "", ""
	cl := m.cl
	if kind == "storefill" {
		node, sha, _ := strings.Cut(target, "|")
		out, parts := false, []string{}
		for _, f := range fields {
			if strings.EqualFold(f, "output") {
				out = true
			} else {
				parts = append(parts, f)
			}
		}
		blocks := strings.Join(parts, ",")
		if blocks == "" && !out {
			return m, nil
		}
		m.note = helpS.Render("asking " + node + " to fill its worker cache …")
		shas := strings.Split(sha, ",") // a split model: the same blocks of every shard, in turn
		return m, func() tea.Msg {
			if len(shas) > 1 {
				if err := cl.FillAll(node, shas, blocks, out); err != nil {
					return noteMsg(badS.Render("error: " + err.Error()))
				}
				return noteMsg(okS.Render("✓ " + node + " has filled its worker cache from every shard"))
			}
			if err := cl.StoreFill(node, "", sha, blocks, out); err != nil {
				return noteMsg(badS.Render("error: " + err.Error()))
			}
			return noteMsg(okS.Render("✓ " + node + " is filling its worker cache — progress is listed below"))
		}
	}
	if nm, cmd, ok := m.finishProfileEdit(kind, target, buf); ok {
		return nm, cmd
	}
	if nm, cmd, ok := m.finishExtraEdit(kind, target, buf); ok {
		return nm, cmd
	}
	if kind == "runbind" {
		return m.finishRunBind(target, buf)
	}
	if kind == "storeadd" {
		if buf == "" {
			return m, nil
		}
		return m, func() tea.Msg {
			n, err := cl.StoreAdd([]string{expandHome(buf)})
			if err != nil {
				return noteMsg(badS.Render("error: " + err.Error()))
			}
			return noteMsg(okS.Render(fmt.Sprintf("🗂 hashing %d file(s) on this machine — reading each one once; progress shows here", n)))
		}
	}
	if kind == "planprofile" {
		if buf == "" {
			return m, nil
		}
		return m.saveCandidateProfile(buf)
	}
	if kind == "planpath" {
		if buf == "" {
			return m, nil
		}
		m.planPath, m.planModel, m.planRes, m.planErr, m.planBusy = buf, nil, nil, "", true
		cmd := m.runPlan(buf)
		return m, cmd
	}
	if kind == "addnode" {
		if buf == "" {
			return m, nil
		}
		host := buf
		m.note = helpS.Render("connecting to " + host + " …")
		return m, func() tea.Msg {
			cfg, err := cl.Config()
			if err != nil {
				return noteMsg(badS.Render("error: " + err.Error()))
			}
			ssh := cfg.Discover.SSH
			if !contains(ssh, host) {
				ssh = append(ssh, host)
			}
			if err := cl.SetDiscover(cfg.Discover.Ports, ssh, cfg.Discover.Ignore); err != nil {
				return noteMsg(badS.Render("error: " + err.Error()))
			}
			return noteMsg(okS.Render("✓ machine " + host + " added to cluster discovery!"))
		}
	}
	if kind == "huburl" {
		if buf != "" {
			m.hubURL = buf
			m.cfg.HubURL = buf
			m.note = okS.Render("✓ Hub URL set to " + m.hubURL)
			if cl != nil {
				return m, func() tea.Msg {
					_ = cl.SetHubSettings(m.hubURL, m.hubToken)
					return noteMsg(okS.Render("✓ Hub URL saved to configuration"))
				}
			}
		}
		return m, nil
	}
	if kind == "hubtoken" {
		m.hubToken = buf
		m.cfg.HubToken = buf
		msg := "✓ Auth token configured"
		if buf == "" {
			msg = "✓ Auth token cleared"
		}
		m.note = okS.Render(msg)
		if cl != nil {
			return m, func() tea.Msg {
				_ = cl.SetHubSettings(m.hubURL, m.hubToken)
				return noteMsg(okS.Render(msg + " (saved)"))
			}
		}
		return m, nil
	}
	if kind == "hubapikey" {
		m.hubAPIKey = buf
		m.cfg.HubAPIKey = buf
		msg := "✓ Hub API key configured"
		if buf == "" {
			msg = "✓ Hub API key cleared"
		}
		m.note = okS.Render(msg)
		if cl != nil {
			return m, func() tea.Msg {
				if err := cl.SetHubAPIKey(buf); err != nil {
					return noteMsg(badS.Render("error saving API key: " + err.Error()))
				}
				return noteMsg(okS.Render(msg + " (saved)"))
			}
		}
		return m, nil
	}
	if kind == "joinhub" {
		if buf == "" {
			buf = config.DefaultHubURL
		}
		if buf == "" {
			m.note = badS.Render(config.ErrNoHub.Error())
			return m, nil
		}
		hubURL := buf
		m.note = helpS.Render("connecting reverse tunnel to " + hubURL + " …")
		return m, func() tea.Msg {
			if err := cl.JoinHub(hubURL, ""); err != nil {
				return noteMsg(badS.Render("error: " + err.Error()))
			}
			return noteMsg(okS.Render("✓ Connected to Fleet Hub: " + hubURL))
		}
	}
	fail := func(err error) tea.Msg { return noteMsg(badS.Render("error: " + err.Error())) }
	switch kind {
	case "match":
		return m, func() tea.Msg {
			if err := cl.SetMatch(target, fields); err != nil {
				return fail(err)
			}
			if len(fields) == 0 {
				return noteMsg(okS.Render("✓ " + target + ": selectors cleared"))
			}
			return noteMsg(okS.Render("✓ " + target + " now also takes every model matching " + strings.Join(fields, " ")))
		}
	case "fleetport":
		n := 0
		if buf != "" {
			var err error
			if n, err = strconv.Atoi(buf); err != nil || n < 0 || n > 65535 {
				m.note = badS.Render("the port must be a number from 0 (4711) to 65535")
				return m, nil
			}
		}
		return m, func() tea.Msg {
			if err := cl.SetFleetPort(target, n); err != nil {
				return fail(err)
			}
			return noteMsg(okS.Render(fmt.Sprintf("✓ the fleet on %s's machine: port %d (0 = 4711)", target, n)))
		}
	case "group":
		g, stall, err := board.ParseGroup(fields)
		if err != nil {
			m.note = badS.Render(err.Error())
			return m, nil
		}
		return m, func() tea.Msg {
			if err := cl.SetGroup(target, g, stall); err != nil {
				return fail(err)
			}
			if g.Engine == "" {
				return noteMsg(okS.Render("✓ " + target + ": no longer a group"))
			}
			return noteMsg(okS.Render("✓ " + target + ": group " + g.Engine + " on " + strings.Join(g.Workers, ", ")))
		}
	case "tags":
		return m, func() tea.Msg {
			if err := cl.SetTags(target, fields); err != nil {
				return fail(err)
			}
			return noteMsg(okS.Render("✓ tags of " + target + ": " + strings.Join(fields, " ")))
		}
	}
	// newrole: "<name> [selector …]" — with selectors the pool is built from them, without a model must be chosen later
	if len(fields) == 0 {
		return m, nil
	}
	name, sel := fields[0], fields[1:]
	if _, taken := m.st.Roles[name]; taken {
		m.note = badS.Render("a role called " + name + " already exists")
		return m, nil
	}
	f := map[string]any{}
	if len(sel) > 0 {
		f["match"] = sel
	} else {
		eps := m.chatEps()
		if len(eps) == 0 {
			m.note = badS.Render("no chat model registered yet")
			return m, nil
		}
		f["preferred"] = eps[0].Name // a role needs a model or selectors; replace it with p / enter
	}
	return m, func() tea.Msg {
		if err := cl.UpdateRole(name, f); err != nil {
			return fail(err)
		}
		if len(sel) > 0 {
			return noteMsg(okS.Render("✓ role " + name + " created from " + strings.Join(sel, " ") + " — enter to tune it"))
		}
		return noteMsg(okS.Render("✓ role " + name + " created with a placeholder model — p sets the preferred one, f adds fallbacks"))
	}
}

// editPrompt is the text-input line of the role / tag editors.
func (m model) editPrompt() string {
	label := map[string]string{
		"newrole":        "new role: name, optionally followed by selectors (tag:fast node:dino cap:vision model:gemma4*)",
		"match":          "selectors of " + m.editFor + " (tag:x node:x cap:x model:glob, space-separated; empty = none)",
		"runbind":        "endpoint that serves run " + m.editFor + " (its group is then followed; empty = unbind)",
		"fleetport":      "port of the fleet daemon on the machine of " + m.editFor + " (empty or 0 = 4711)",
		"group":          "group of " + m.editFor + ": engine=llama-ring workers=a,b stall=90s (off or empty engine = not a group)",
		"tags":           "tags of " + m.editFor + " (space-separated; empty = none)",
		"planpath":       "path of a GGUF file (on this machine; ~ is your home)",
		"planprofile":    "save the chosen candidate as a profile named (letters, digits, . _ -)",
		"storefill":      "blocks to cache on " + strings.SplitN(m.editFor, "|", 2)[0] + " (like 0-3,7; add the word output when it holds the last blocks)",
		"storeadd":       "a GGUF file or a directory of them, on the daemon's machine (~ is your home)",
		"addnode":        "connect a new machine (hostname, IP or user@host for SSH)",
		"joinhub":        "connect to a Fleet Hub (URL, e.g. https://hub.example.com)",
		"huburl":         "target Fleet Hub address (e.g. https://hub.example.com)",
		"hubtoken":       "tunnel join token for Fleet Hub (empty = none; not the API key)",
		"hubapikey":      "Fleet Hub API key for /v1/* requests (empty = clear)",
		"profmeasure":    "measured tokens/s of the last run of " + m.editFor + " (a number)",
		"profedit":       "change " + m.editFor + ": key=value (layout.ts=34,14  layout.mode=auto  spec.type=none  workers=a,b  ctx=8192; empty value removes)",
		"profcopy":       "save " + m.editFor + " as a new profile named",
		"profexport":     "write " + m.editFor + " as YAML to this file",
		"profimport":     "import a profile from this YAML file (add the word force after the path to replace one of the same name)",
		"nodespec":       "declared spec of " + m.editFor + ": addr=IP mem_gib=N weight_gbps=N expert_gbps=N (an empty value clears it)",
		"hubstreamports": "stream ports this node allows the hub to open: 8080,8081 replaces the list, +8080 adds to it (empty = the config's hub_stream_ports)",
		"hubstream":      "open a stream: <node> <remote-port> [listen-address] (a local listener that reaches 127.0.0.1:<port> on <node> through the hub)",
	}[m.editing]
	if label == "" {
		return ""
	}
	return fmt.Sprintf("%s\n%s %s▏\n", accS.Render(label+" →"), accS.Render("›"), m.editBuf)
}

// pastedText makes pasted text fit one input line: line breaks and tabs become single spaces, and the ends are
// trimmed (a path copied from a terminal often comes with a newline, or with quotes around it).
func pastedText(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '\'' || s[0] == '"') && s[len(s)-1] == s[0] {
		s = s[1 : len(s)-1]
	}
	return s
}

// letterIsShortcut: screens whose list advertises a j/k key of their own (Economy j: tunnel, Hub k: API key, Nodes j: Hub).
// The global vim-style up/down used to take these keys first, so the advertised shortcut did nothing.
func (m model) letterIsShortcut(s string) bool {
	switch m.sc {
	case scEconomy, scNodes:
		return s == "j"
	case scHub:
		return s == "k"
	}
	return false
}

// drilledFromNodes: the Dashboard was opened from a node on the Nodes screen (its filter is the node, not a search).
func (m model) drilledFromNodes() bool {
	return m.sc == scDash && len(m.nav) > 0 && m.nav[len(m.nav)-1].sc == scNodes
}

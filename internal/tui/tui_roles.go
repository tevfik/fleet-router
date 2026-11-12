package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/tevfik/fleet-router/internal/client"
	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/eval"
	"github.com/tevfik/fleet-router/internal/roles"
	"github.com/tevfik/fleet-router/internal/server"
)

func (m model) viewRoles() string {
	var b strings.Builder
	roles := m.roleNames()
	if m.pick {
		fmt.Fprintf(&b, "choose %s for %s\n%s\n", accS.Render(roles[m.roleCur]), m.pickMode, m.searchLine())
		for i, e := range m.chatEps() {
			fmt.Fprintf(&b, "%s%s %-40s %s\n", curMark(i == m.cur), mark(e), e.Ref(), helpS.Render(capLetters(e.Endpoint)))
		}
		return b.String()
	}
	for i, r := range roles {
		x := m.st.Roles[r]
		act := okS.Render(m.ref(x.Active))
		if x.Active != x.Preferred {
			act = badS.Render(m.ref(x.Active) + " (fallback active)")
		}
		opt := fmt.Sprintf("priority %d · parallel %s", x.Priority, slotStr(x.MaxParallel))
		if x.Overflow {
			opt += " · overflow"
		}
		if x.Strategy != "" && x.Strategy != "failover" {
			opt += " · " + x.Strategy
		}
		if x.InUse > 0 || x.Waiting > 0 {
			opt += fmt.Sprintf(" · %d in use, %d waiting", x.InUse, x.Waiting)
		}
		fmt.Fprintf(&b, "%s%-10s preferred %-28s fallback %v\n             now → %s   %s\n", curMark(i == m.cur), r, m.ref(x.Preferred), m.refs(x.Fallback), act, helpS.Render(opt))
	}
	if len(roles) == 0 {
		b.WriteString(descS.Render("no roles yet — press t to create starter roles (scout, verifier, architect, coder, vision, embed) from your models,\n or use `fleet role set <role> <endpoint>`") + "\n")
	}
	return b.String()
}

// qualityLine shows the stored `fleet eval` score of an endpoint ("" when it was never evaluated).
func qualityLine(name string) string {
	sum, ok := eval.Load(config.EvalPath())[name]
	if !ok {
		return ""
	}
	age := ""
	if d := time.Since(sum.When); d > 48*time.Hour {
		age = fmt.Sprintf(" · %d days ago", int(d.Hours()/24))
	}
	return fmt.Sprintf("quality %s", accS.Render(fmt.Sprintf("%.0f%%", sum.Score*100))) +
		helpS.Render(fmt.Sprintf(" (format %.0f · reasoning %.0f · language %.0f · knowledge %.0f)%s — fleet eval",
			sum.ByCat[eval.CatFormat]*100, sum.ByCat[eval.CatReasoning]*100, sum.ByCat[eval.CatLanguage]*100, sum.ByCat[eval.CatKnowledge]*100, age))
}

func tagLabel(e config.Endpoint) string {
	if len(e.Tags) == 0 {
		return ""
	}
	return " " + helpS.Render("#"+strings.Join(e.Tags, " #"))
}

func capsLabel(e config.Endpoint) string {
	if len(e.Caps) > 0 {
		return strings.Join(e.Caps, " ")
	}
	var c []string
	for _, k := range []string{"chat", "embed", "vision", "audio"} {
		if e.Has(k) {
			c = append(c, k)
		}
	}
	return strings.Join(c, " ")
}

func guide(prefix string, fn func(*config.Config) ([]string, error)) (string, error) {
	cfg, err := config.Load(config.Path())
	if err != nil {
		return prefix, err
	}
	res, err := fn(cfg)
	return strings.TrimSpace(prefix + " · " + strings.Join(res, " · ")), err
}

// bench starts a benchmark (names empty → every healthy chat endpoint); progress shows in the footer.
func (m model) bench(names []string) tea.Cmd {
	cl := m.cl
	return func() tea.Msg {
		if err := cl.Bench(names); err != nil {
			return noteMsg(badS.Render("error: " + err.Error()))
		}
		return noteMsg(okS.Render("✓ benchmark started"))
	}
}

func slotStr(n int) string {
	if n == 0 {
		return "unlimited"
	}
	return fmt.Sprint(n)
}

// roleOrder is the role's priority order — [preferred, fallback1, fallback2…].
func (m model) roleOrder() []string {
	roles := m.roleNames()
	if m.roleCur >= len(roles) {
		return nil
	}
	r := m.st.Roles[roles[m.roleCur]]
	var o []string
	if r.Preferred != "" {
		o = append(o, r.Preferred)
	}
	return append(o, r.Fallback...)
}

func (m model) saveOrder(role string, order []string) tea.Cmd {
	pref, fb := "", []string{}
	if len(order) > 0 {
		pref, fb = order[0], order[1:]
	}
	cl := m.cl
	return func() tea.Msg {
		if err := cl.UpdateRole(role, map[string]any{"preferred": pref, "fallback": fb}); err != nil {
			return noteMsg(badS.Render("error: " + err.Error()))
		}
		return noteMsg(okS.Render("✓ order updated — applied hot"))
	}
}

// testRole asks a tiny question through the router and of every member, and shows who answered.
func (m model) testRole(role string) tea.Cmd {
	cl := m.cl
	cfg := config.Config{}
	for _, e := range m.st.Endpoints {
		cfg.Endpoints = append(cfg.Endpoints, e.Endpoint)
	}
	var members []string
	for _, e := range cfg.Pool(m.st.Roles[role].Role) {
		members = append(members, e.Name)
	}
	return func() tea.Msg {
		return noteMsg("test of " + accS.Render(role) + ":\n" + strings.Join(client.FormatTest(cl.TestRole(role, members)), "\n"))
	}
}

// undoConfig restores the configuration as it was before the last change (roles, tags, endpoints, …).
func (m model) undoConfig() tea.Cmd {
	cl := m.cl
	return func() tea.Msg {
		left, err := cl.Undo()
		if err != nil {
			return noteMsg(badS.Render("undo: " + err.Error()))
		}
		return noteMsg(okS.Render(fmt.Sprintf("✓ previous configuration restored (%d older kept)", left)))
	}
}

func (m model) keyRoleEdit(s string) (tea.Model, tea.Cmd) {
	roles := m.roleNames()
	if m.roleCur >= len(roles) {
		return m, nil
	}
	role := roles[m.roleCur]
	r := m.st.Roles[role]
	upd := func(f map[string]any, ok string) tea.Cmd {
		cl := m.cl
		return func() tea.Msg {
			if err := cl.UpdateRole(role, f); err != nil {
				return noteMsg(badS.Render("error: " + err.Error()))
			}
			return noteMsg(okS.Render("✓ " + ok))
		}
	}
	if m.pick { // choosing an endpoint to add as a fallback
		if s == "/" {
			m.editing, m.editBuf = "filter", m.filter
			return m, nil
		}
		if s == "enter" && m.cur < len(m.chatEps()) {
			name := m.chatEps()[m.cur].Name
			order := m.roleOrder()
			for _, o := range order {
				if o == name {
					m.pick, m.filter, m.note = false, "", badS.Render("already in the list: "+name)
					return m, nil
				}
			}
			m.pick, m.cur, m.filter = false, len(order), ""
			return m, m.saveOrder(role, append(order, name))
		}
		return m, nil
	}
	order := m.roleOrder()
	switch s {
	case "[", "{": // move up (higher priority)
		if m.cur > 0 && m.cur < len(order) {
			order[m.cur-1], order[m.cur] = order[m.cur], order[m.cur-1]
			m.cur--
			return m, m.saveOrder(role, order)
		}
	case "]", "}":
		if m.cur < len(order)-1 {
			order[m.cur+1], order[m.cur] = order[m.cur], order[m.cur+1]
			m.cur++
			return m, m.saveOrder(role, order)
		}
	case "u":
		return m, m.undoConfig()
	case "z": // the size of request the route table prices
		m.routeSize, m.route = (m.routeSize+1)%len(requestSizes), nil
		return m, m.fetch()
	case "m":
		m.editing, m.editFor, m.editBuf = "match", role, strings.Join(r.Match, " ")
	case "a":
		m.pick, m.pickMode, m.cur = true, "add", 0
	case "x":
		if len(order) > 1 && m.cur < len(order) {
			order = append(order[:m.cur], order[m.cur+1:]...)
			m.cur = min(m.cur, len(order)-1)
			return m, m.saveOrder(role, order)
		}
		m.note = badS.Render("cannot remove the last endpoint")
	case "+", "=":
		return m, upd(map[string]any{"max_parallel": r.MaxParallel + 1}, fmt.Sprintf("role parallel = %d", r.MaxParallel+1))
	case "-":
		n := max(0, r.MaxParallel-1)
		return m, upd(map[string]any{"max_parallel": n}, "role parallel = "+slotStr(n))
	case ">", ".":
		return m, upd(map[string]any{"priority": r.Priority + 1}, fmt.Sprintf("priority = %d", r.Priority+1))
	case "<", ",":
		return m, upd(map[string]any{"priority": r.Priority - 1}, fmt.Sprintf("priority = %d", r.Priority-1))
	case "o":
		return m, upd(map[string]any{"overflow": !r.Overflow}, fmt.Sprintf("overflow = %v", !r.Overflow))
	case "s": // cycle the balancing strategy
		next := map[string]string{"": "least-busy", "failover": "least-busy", "least-busy": "fastest", "fastest": "cost", "cost": "round-robin", "round-robin": "failover"}[r.Strategy]
		return m, upd(map[string]any{"strategy": next}, "strategy = "+next)
	}
	return m, nil
}

func strategyName(s string) string {
	if s == "" {
		return "failover"
	}
	return s
}

// explain describes, in plain words, what the current role settings do to a request.
func explain(role string, r server.RoleStatus, order []string) string {
	if len(order) == 0 {
		return "This role has no model yet — press a to add one."
	}
	var l []string
	switch r.Strategy {
	case "least-busy":
		l = append(l, fmt.Sprintf("1. A request for %q goes to whichever of these %d models is least busy right now.", role, len(order)))
	case "fastest":
		l = append(l, fmt.Sprintf("1. A request for %q goes to whichever of these %d models is expected to answer soonest (measured speed, adjusted for current load).", role, len(order)))
	case "cost":
		l = append(l, fmt.Sprintf("1. A request for %q goes to whichever of these %d models is expected to finish it soonest: queue + loading a cold model + the prompt + the reply, on each machine's current load and strain (fleet route %s shows the figures).", role, len(order), role))
	case "round-robin":
		l = append(l, fmt.Sprintf("1. Requests for %q are handed out to these %d models in turn.", role, len(order)))
	default:
		l = append(l, fmt.Sprintf("1. A request for %q goes to %s (the first healthy model in the list).", role, order[0]))
	}
	switch {
	case r.Strategy == "least-busy" || r.Strategy == "round-robin" || r.Strategy == "fastest" || r.Strategy == "cost":
		l = append(l, "2. A model with no free slot is skipped while another has room; if all are full the request queues.")
	case r.Overflow && len(order) > 1:
		l = append(l, fmt.Sprintf("2. If %s has no free slot the request spills over to the next model instead of waiting.", order[0]))
	default:
		l = append(l, fmt.Sprintf("2. If %s has no free slot the request waits in its queue (higher priority goes first).", order[0]))
	}
	if r.MaxParallel > 0 {
		l = append(l, fmt.Sprintf("3. At most %d requests of this role run at the same time; the rest wait.", r.MaxParallel))
	} else {
		l = append(l, "3. The number of simultaneous requests is limited only by each model's slots.")
	}
	l = append(l, "4. A model that is down (or errors before answering) is skipped and the next one is used.")
	return strings.Join(l, "\n   ")
}

func (m model) viewRoleEdit() string {
	roles := m.roleNames()
	if m.roleCur >= len(roles) {
		return ""
	}
	role := roles[m.roleCur]
	r := m.st.Roles[role]
	var b strings.Builder
	ov := "off (waits when the preferred endpoint is full)"
	if r.Overflow {
		ov = okS.Render("on") + " (spills to the next fallback when the preferred endpoint is full)"
	}
	fmt.Fprintf(&b, "%s  %s\n\n", accS.Render(role), descS.Render(r.Use))
	fmt.Fprintf(&b, "  priority  %s   %s\n", accS.Render(fmt.Sprint(r.Priority)), helpS.Render("the higher one goes first in the queue for the same endpoint   (< >)"))
	fmt.Fprintf(&b, "  parallel  %s   %s\n", accS.Render(slotStr(r.MaxParallel)), helpS.Render("concurrent requests this role may have in flight   (+ -)"))
	fmt.Fprintf(&b, "  overflow  %s   %s\n", ov, helpS.Render("(o)"))
	fmt.Fprintf(&b, "  strategy  %s   %s\n\n", accS.Render(strategyName(r.Strategy)), helpS.Render("how a request picks a model from the list below   (s)"))
	if m.pick {
		b.WriteString(groupS.Render("Endpoint to add as a fallback:") + "\n" + m.searchLine())
		for i, e := range m.chatEps() {
			fmt.Fprintf(&b, "%s%s %-40s %s\n", curMark(i == m.cur), mark(e), e.Ref(), helpS.Render(capLetters(e.Endpoint)))
		}
		return b.String()
	}
	sel := "none"
	if len(r.Match) > 0 {
		sel = accS.Render(strings.Join(r.Match, " "))
	}
	fmt.Fprintf(&b, "  selectors %s   %s\n", sel, helpS.Render("models that join this role by property, e.g. tag:fast node:dino   (m)"))
	if len(r.Match) > 0 {
		var names []string
		for _, e := range m.st.Endpoints {
			for _, x := range r.Match {
				if e.Matches(x) {
					names = append(names, m.ref(e.Name))
					break
				}
			}
		}
		fmt.Fprintf(&b, "            %s\n", helpS.Render(fmt.Sprintf("%d matching now: %s", len(names), strings.Join(names, ", "))))
	}
	b.WriteString("\n")
	b.WriteString(helpS.Render("   "+explain(role, r, m.refs(m.roleOrder()))) + "\n\n")
	b.WriteString(groupS.Render("Models of this role (order matters for failover; every member counts for least-busy / round-robin)") + "\n")
	eps := map[string]struct {
		e server.EpStatus
	}{}
	for _, e := range m.st.Endpoints {
		eps[e.Name] = struct{ e server.EpStatus }{e}
	}
	for i, n := range m.roleOrder() {
		e := eps[n].e
		tag := ""
		if n == r.Active {
			tag = okS.Render("  ← current")
		}
		fmt.Fprintf(&b, "%s%d. %s %-40s slots %s · %d in use · %d waiting%s\n", curMark(i == m.cur), i+1, mark(e), m.ref(n), slotStr(e.Slots), e.InUse, e.Waiting, tag)
	}
	b.WriteString(m.routeSection(role))
	return b.String()
}

// suggestRoles creates the starter roles that do not exist yet (see package roles).
func (m model) suggestRoles() tea.Cmd {
	cl := m.cl
	return func() tea.Msg {
		cfg, err := cl.Config()
		if err != nil {
			return noteMsg(badS.Render("error: " + err.Error()))
		}
		sg := roles.Suggest(&cfg)
		if len(sg) == 0 {
			return noteMsg(helpS.Render("nothing to suggest — the starter roles already exist"))
		}
		var names []string
		for _, x := range sg {
			if err := cl.SetRole(x.Role, x.Value.Preferred, x.Value.Fallback); err != nil {
				return noteMsg(badS.Render("error: " + err.Error()))
			}
			names = append(names, x.Role)
		}
		return noteMsg(okS.Render("✓ created roles: " + strings.Join(names, ", ") + " — review them with enter"))
	}
}

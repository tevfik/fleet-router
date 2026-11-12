// Package tui is the fleet menu: a main menu of screens; Esc goes back.
package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/tevfik/fleet-router/internal/board"
	"github.com/tevfik/fleet-router/internal/client"
	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/feature"
	"github.com/tevfik/fleet-router/internal/plan"
	"github.com/tevfik/fleet-router/internal/profile"
	"github.com/tevfik/fleet-router/internal/server"
	"github.com/tevfik/fleet-router/internal/setup"
)

// Colour palette.
var (
	cPrimary = lipgloss.Color("#7C3AED")
	cSecond  = lipgloss.Color("#06B6D4")
	cAccent  = lipgloss.Color("#F59E0B")
	cOK      = lipgloss.Color("#10B981")
	cErr     = lipgloss.Color("#EF4444")
	cMuted   = lipgloss.Color("#6B7280")
	cFg      = lipgloss.Color("#E2E8F0")
	cDim     = lipgloss.Color("#94A3B8")

	logoS    = lipgloss.NewStyle().Bold(true).Foreground(cPrimary)
	titleS   = lipgloss.NewStyle().Bold(true).Foreground(cFg).Background(cPrimary).Padding(0, 2)
	subS     = lipgloss.NewStyle().Foreground(cDim).Italic(true)
	activeS  = lipgloss.NewStyle().Foreground(cPrimary).Bold(true)
	normalS  = lipgloss.NewStyle().Foreground(cFg)
	descS    = lipgloss.NewStyle().Foreground(cDim).Italic(true)
	actDesc  = lipgloss.NewStyle().Foreground(cSecond).Italic(true)
	helpS    = lipgloss.NewStyle().Foreground(cMuted)
	okS      = lipgloss.NewStyle().Foreground(cOK)
	badS     = lipgloss.NewStyle().Foreground(cErr)
	accS     = lipgloss.NewStyle().Foreground(cAccent).Bold(true)
	groupS   = lipgloss.NewStyle().Foreground(cSecond).Bold(true)
	cSecondS = lipgloss.NewStyle().Foreground(cSecond)
)

const logo = `
  ▄▄▄▄ █    ▄▄▄▄ ▄▄▄▄ ▄▄▄▄▄
  █▄▄  █    █▄▄  █▄▄    █
  █    █▄▄▄ █▄▄▄ █▄▄▄   █`

type screen int

const (
	scHome screen = iota
	scDash
	scRoles
	scDiscover
	scBoard
	scInteg
	scRoleEdit
	scCaps
	scActivity
	scUsage
	scNodes
	scPlan
	scStore
	scRuns
	scAsk
	scEconomy
	scHub
	scProfiles
)

type menuItem struct{ icon, title, desc, group string }

// The home menu is grouped by what one comes to do: look at the fleet, run a model, set up machines and routing,
// and the network features few need at first. A group's name is printed once above its first item.
var home = []menuItem{
	{"📊", "Dashboard", "Endpoint health, latency, tokens/s, slot usage", "Use"},
	{"💬", "Chat / Ask", "Test models & roles live — interactive question and answer", ""},
	{"📜", "Activity", "Live request log — which model answered, failovers, speed", ""},
	{"🏆", "Leaderboard", "Tokens/second ranking of every model — benchmark them live", ""},
	{"📈", "Usage", "Who used which model — requests, tokens, errors per client / role / model / day", ""},
	{"🧩", "Plan", "Where a model would run best: one machine, or split over several — from its GGUF file", "Run a model"},
	{"📑", "Profiles", "Saved deploy configurations: deploy, dry run, benchmark, edit, export / import, delete", ""},
	{"📡", "Runs", "Models running over several machines: the path of each block, plan against measured, who left — every fleet sees it", ""},
	{"📂", "Model files", "Which GGUF files each machine has, and what its worker cache holds — hashed once, shared between fleets", ""},
	{"💻", "Nodes", "The machines: ping, CPU, RAM, GPU, temperatures, disks — and what their models can do", "Machines & routing"},
	{"🔍", "Discover", "Find models on this machine + SSH hosts, register them under the machine name", ""},
	{"🎯", "Roles", "Role → model assignment, order, priority, parallelism (applied hot)", ""},
	{"🔌", "Integrations", "Claude Code · OpenCode · embeddings · systemd service", ""},
	{"🌐", "Fleet Hub", "Manage the outbound reverse tunnel to a Fleet Hub", "Advanced"},
	{"🪙", "Economy & Wallet", "Decentralized compute tokens, wallet balance, and idle GPU sharing settings", ""},
	{"👋", "Quit", "Close the menu", ""},
}

// runnerItems are the home menu's screens of the runner side: only where fleet-engine is (feature.Runner).
var runnerItems = map[string]bool{"Plan": true, "Profiles": true, "Runs": true, "Model files": true}

// homeItems is the home menu of this machine: without fleet-engine the runner's screens are left out. A group whose
// first item went keeps its heading on its next item.
func homeItems() []menuItem {
	if feature.Runner() {
		return home
	}
	out := make([]menuItem, 0, len(home))
	pending := ""
	for _, it := range home {
		if runnerItems[it.title] {
			if it.group != "" {
				pending = it.group
			}
			continue
		}
		if it.group != "" {
			pending = ""
		} else if pending != "" {
			it.group, pending = pending, ""
		}
		out = append(out, it)
	}
	return out
}

// homeStart is the one line a first-time user needs: how a model gets running, and where to stop it.
const homeStart = "New here?  Plan → pick a GGUF file → Deploy   ·   stop it from Runs   ·   try it in Chat / Ask"

// homeStartRouter is homeStart without fleet-engine: the models already run somewhere, fleet finds and routes to them.
const homeStartRouter = "New here?  Discover → register your model servers   ·   Roles → pick who answers   ·   try it in Chat / Ask"

func homeHint() string {
	if feature.Runner() {
		return homeStart
	}
	return homeStartRouter
}

type tickMsg time.Time
type statusMsg struct {
	st        server.Status
	log       []server.LogEntry            // only fetched while the Activity screen is open
	use       []server.UsageRow            // only fetched while the Usage screen is open
	week, tab []server.UsageRow            // only fetched while the Leaderboard is open
	hosts     map[string]server.HostReport // only fetched while the Nodes screen is open
	route     *server.RouteExplain         // only fetched while a role's settings are open
	store     []server.NodeStore           // only fetched while the Model files screen is open
	runs      []server.Run                 // only fetched while the Runs screen is open
	runsGen   int                          // m.runsGen when the fetch started
	runsErr   error                        // the runs could not be fetched (runs is nil then)
	cfg       *config.Config               // only fetched while the Nodes screen is open (the declared specs)
	hub       *hubInfo                     // only fetched while the Hub or Economy screen is open
	linger    string
	err       error
}
type discMsg struct {
	r   client.DiscoverResult
	err error
}
type noteMsg string
type regMsg struct { // result of registering models from the scan results
	names []string
	err   error
}
type actMsg struct {
	out string
	err error
}

type model struct {
	cl       *client.Client
	listen   string
	embedded bool
	sc       screen
	cur      int
	st       server.Status
	err      error
	w, h     int
	note     string

	// roller
	pick     bool
	pickMode string
	roleCur  int

	// discover
	disc      *client.DiscoverResult
	busy      bool
	sel       map[int]bool
	inclEmb   bool
	cfg       config.Config
	editing   string // "", "ports", "ssh"
	editBuf   string
	editFor   string          // what a "newrole" / "match" / "tags" input is about: the role or endpoint name
	hosts     []string        // candidate SSH targets (~/.ssh/config + configuration)
	focus     int             // discover: 0 = targets, 1 = results
	collapsed map[string]bool // discover results: collapsed machine groups
	capName   string          // endpoint whose capabilities are being edited
	inBar     bool            // keyboard focus is on the button bar
	reqlog    []server.LogEntry
	usage     []server.UsageRow
	useBy     int                          // index into usageBy
	useSince  int                          // index into usageSince
	bTab      int                          // leaderboard view (boardTabs)
	bNode     string                       // leaderboard machine filter ("" = all)
	bWeek     []server.UsageRow            // usage by model, last 7 days (top cards)
	bData     []server.UsageRow            // usage rows of the open leaderboard view
	barCur    int                          // highlighted button
	filter    string                       // discover results: name filter
	otherCur  int                          // cursor of the other list when focus switches
	nodeHost  map[string]server.HostReport // Nodes screen: each machine's last readings
	route     *server.RouteExplain         // role settings: where a request would go now
	routeSize int                          // index into requestSizes
	// plan
	planFiles       []string
	planPath        string
	planModel       *plan.Model
	ctxMaxOf        map[string]int // path → the model's max context, read from its header once (the map is shared by copies of the model)
	planRes         *plan.Result
	planErr         string
	planBusy        bool
	planPick        int                // the candidate f and w act on (0 = the best); chosen with s
	planCur         int                // the highlighted file while a candidate is being chosen
	planExpl        bool               // the items of the chosen candidate's time per token are shown (e)
	lay             *layerEd           // the layer editor of the plan screen is open (S)
	profOpen        bool               // the profiles panel of the plan screen is open (L)
	profList        []*profile.Profile // what it lists
	profCur         int
	planCtx         int
	planNp          int
	planStrat       int // 0: latency, 1: throughput, 2: memory, 3: auto
	planMaster      string
	planInit        bool
	planSection     int                // 0: files, 1: params, 2: candidates, 3: deploy
	planAdv         bool               // the advanced runtime settings (target … engine) are open
	planParamCur    int                // 0: context, 1: slots, 2: qos, 3: master, 4: target, 5: moe, 6: spec, 7: vision, 8: paging, 9: engine
	planCPUOnly     bool               // force execution on CPU (-ngl 0)
	planMMProjCPU   bool               // offload multimodal projector to CPU (0 VRAM)
	planNoMMap      bool               // false: mmap enabled (default), true: load RAM only
	planDisableSpec bool               // false: speculative ring hopping enabled (default), true: sequential ring only
	planMoECache    int                // 0: Auto (Max VRAM), 1: 3300 MiB, 2: 2048 MiB, 3: 1024 MiB, 4: Disabled
	planSpecMode    int                // 0: Auto (Smart Topology), 1: DFlash Diffusion, 2: MTP Native, 3: N-gram Lookahead, 4: Disabled
	planEngineMode  int                // 0: Auto (RTT Smart), 1: ring (Protocol 1), 2: chain (Protocol 2 / FSP1)
	planViaHub      bool               // reach the workers through the hub's raw TCP streams (fleet deploy --via-hub)
	outText         string             // multi-line result of the last action (dry run, bench, prune …), shown under the screen
	deployStatus    string             // real-time stage progress during deployment
	deployChan      chan string        // channel receiving progress steps
	deployCancel    context.CancelFunc // cancels the running deploy (x)
	planSeq         int                // number of the last runPlan: older planMsg replies are dropped
	runs            []server.Run       // Runs screen: what every fleet reports
	runsGen         int                // bumped when a run row is removed here: a fetch started before that is not taken
	runsErr         error              // the last GET of the runs failed (the list shown is the last good one)
	linger          string             // systemd linger setting on local node
	storeNodes      []server.NodeStore // Model files screen: what every machine reported
	pickNodes       []string           // Model files: the machines to choose from
	storeCur        int                // Model files: the highlighted file while a machine is chosen
	// ask / chat
	askTarget string
	askInput  string
	askLog    []chatItem
	askBusy   bool
	// entegrasyon
	confirm bool
	pend    *pendingAct // a stop / removal waiting for its y (confirm is set meanwhile)
	// hub
	hubPrev         screen
	homeCur         int        // the start-menu entry the last opened screen came from: esc comes back to it
	nav             []navEntry // the screens left by going forward: esc/q return to the last one (not always the home menu)
	hubURL          string
	fetching        bool      // a tick-started fetch is out
	fetchSince      time.Time // when it started
	hub             hubInfo   // what the Hub and Economy screens show; filled by fetch(), never read from a View
	hubToken        string    // tunnel join token
	hubAPIKey       string    // hub API key (fleet key set), separate from hubToken
	hubShowModels   bool
	hubRemoteModels []hubModelInfo
	hubRemoteErr    string
	hubRemoteBusy   bool
}

type hubModelInfo struct {
	ID      string   `json:"id"`
	OwnedBy string   `json:"owned_by"`
	Caps    []string `json:"caps"`
	Up      bool     `json:"up"`
}

type hubModelsMsg struct {
	models []hubModelInfo
	err    error
}

func Run(cl *client.Client, listen string, embedded bool) error {
	_, err := tea.NewProgram(model{cl: cl, listen: listen, embedded: embedded, sel: map[int]bool{}}).Run()
	return err
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.fetch(), tick(), func() tea.Msg { return nil })
}

func tick() tea.Cmd { return tea.Tick(3*time.Second, func(t time.Time) tea.Msg { return tickMsg(t) }) }

func (m model) fetch() tea.Cmd {
	wantLog, wantUse, wantBoard := m.sc == scActivity, m.sc == scUsage, m.sc == scBoard
	by, since := usageBy[m.useBy], usageSince[m.useSince]
	bTab := m.bTab
	wantHosts, wantStore, wantRuns := m.sc == scNodes || m.sc == scHub, m.sc == scStore, m.sc == scRuns || m.sc == scProfiles || m.sc == scNodes
	if !feature.Runner() { // no runner side here: the daemon has no runs to show (and no route for them)
		wantStore, wantRuns = false, false
	}
	wantCfg := m.sc == scEconomy
	wantHub := m.sc == scHub || m.sc == scEconomy
	runsGen := m.runsGen
	routeRole, rsz := "", requestSizes[m.routeSize%len(requestSizes)]
	if roles := m.roleNames(); m.sc == scRoleEdit && m.roleCur < len(roles) {
		routeRole = roles[m.roleCur]
	}
	return func() tea.Msg {
		st, err := m.cl.Status()
		var hosts map[string]server.HostReport
		if err == nil && wantHosts {
			hosts = m.fetchHosts(st.Nodes)
		}
		var stored []server.NodeStore
		if err == nil && wantStore {
			stored = fetchStore(m)
		}
		var runs []server.Run
		var runsErr error
		if err == nil && wantRuns {
			// a failed GET leaves runs nil: the list on screen stays (an empty one would say there are no runs)
			if runs, runsErr = m.cl.Runs(); runsErr == nil && runs == nil {
				runs = []server.Run{}
			}
		}
		var linger string
		if wantHosts || m.sc == scProfiles {
			if v, err := setup.LoginctlLinger(); err == nil {
				linger = v
			}
		}
		var ncfg *config.Config
		if err == nil && (wantHosts || wantCfg) {
			if c, e := m.cl.Config(); e == nil {
				ncfg = &c
			}
		}
		var route *server.RouteExplain
		if err == nil && routeRole != "" {
			if r, e := m.cl.Route(routeRole, rsz.in, rsz.out); e == nil {
				route = &r
			}
		}
		var lg []server.LogEntry
		var use []server.UsageRow
		if err == nil && wantLog {
			lg, _ = m.cl.Log(200)
		}
		fetchUsage := func(b, s, sn string) []server.UsageRow {
			if m.cfg.HubURL != "" {
				loc, err := m.cl.UsageFiltered(b, s, sn, "hub-tunnel")
				if err != nil {
					loc, _ = m.cl.Usage(b, s, sn)
					return loc
				}
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				hubRows, err := m.cl.HubUsage(ctx, m.cfg.HubURL, m.cfg.HubAPIKey, b, s, sn)
				cancel()
				if err == nil && len(hubRows) > 0 {
					return board.CombineUsage(loc, hubRows)
				}
				return loc
			}
			r, _ := m.cl.Usage(b, s, sn)
			return r
		}
		if err == nil && wantUse {
			use = fetchUsage(by, "", since)
		}
		var week, tab []server.UsageRow
		if err == nil && wantBoard {
			week = fetchUsage("model", "", "7d")
			switch bTab {
			case 1:
				tab = fetchUsage("model", "", "24h")
			case 2:
				tab = fetchUsage("role", "model", "7d")
			case 3:
				tab = fetchUsage("day", "model", "7d")
			}
		}
		var hub *hubInfo
		if err == nil && wantHub {
			hi := m.readHubInfo()
			hub = &hi
		}
		return statusMsg{st: st, log: lg, use: use, week: week, tab: tab, hosts: hosts, route: route, store: stored, runs: runs, runsGen: runsGen, runsErr: runsErr, cfg: ncfg, hub: hub, linger: linger, err: err}
	}
}

func (m model) roleNames() []string {
	var r []string
	for k := range m.st.Roles {
		r = append(r, k)
	}
	sort.Strings(r)
	return r
}

// epMatches: every word of the search text occurs in the model's name, node, model id, tags or capabilities.
func epMatches(e server.EpStatus, filter string) bool {
	hay := strings.ToLower(strings.Join([]string{e.Name, e.Node, e.Model, strings.Join(e.Tags, " "), capNames(e.Endpoint)}, " "))
	for _, w := range strings.Fields(strings.ToLower(filter)) {
		if !strings.Contains(hay, w) {
			return false
		}
	}
	return true
}

// chatEps lists the chat models; while choosing one for a role the search text narrows it down.
func (m model) chatEps() []server.EpStatus {
	var out []server.EpStatus
	for _, e := range m.st.Endpoints {
		if !e.IsEmbed() && (!m.pick || epMatches(e, m.filter)) {
			out = append(out, e)
		}
	}
	return out
}

// dashEps lists the dashboard's models, narrowed by the search text.
func (m model) dashEps() []server.EpStatus {
	if m.filter == "" {
		return m.st.Endpoints
	}
	var out []server.EpStatus
	for _, e := range m.st.Endpoints {
		if epMatches(e, m.filter) {
			out = append(out, e)
		}
	}
	return out
}

// searchLine shows the search text above a list that can be searched with "/".
func (m model) searchLine() string {
	switch {
	case m.editing == "filter":
		return "  search: " + accS.Render(m.editBuf+"▏") + "\n"
	case m.filter != "" && m.drilledFromNodes():
		return "  machine: " + accS.Render(m.filter) + helpS.Render("   (/ to search, esc back to Nodes)") + "\n"
	case m.filter != "":
		return "  search: " + accS.Render(m.filter) + helpS.Render("   (/ to change, esc to clear)") + "\n"
	}
	return ""
}

func (m model) listLen() int {
	switch m.sc {
	case scHome:
		return len(homeItems())
	case scDash:
		return len(m.dashEps())
	case scRoles:
		if m.pick {
			return len(m.chatEps())
		}
		return len(m.roleNames())
	case scDiscover:
		if m.focus == 0 {
			return m.menuLen()
		}
		return len(m.visRows())
	case scBoard:
		switch m.bTab {
		case 1:
			return len(m.useRows(m.bData, false))
		case 2:
			return len(m.roleLines())
		case 3:
			return 0
		}
		return len(m.speedRows())
	case scActivity:
		return len(m.reqlog)
	case scUsage:
		return len(m.usageRows())
	case scInteg:
		return len(integrations())
	case scNodes:
		return len(m.st.Nodes)
	case scPlan:
		if m.pick && m.planRes != nil {
			return len(m.planRes.Candidates)
		}
		return len(m.planFiles)
	case scStore:
		if m.pick {
			return len(m.pickNodes)
		}
		return len(storeModels(m.storeNodes))
	case scRuns:
		return len(m.runs)
	case scProfiles:
		return len(m.profList)
	case scAsk:
		return 1
	case scEconomy:
		return 5
	case scHub:
		return 15
	case scRoleEdit:
		if m.pick {
			return len(m.chatEps())
		}
		return len(m.roleOrder())
	case scCaps:
		return len(allCaps) + 2
	}
	return 0
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch v := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = v.Width, v.Height
	case tickMsg:
		// A tick does not start a second fetch while the last one is still out (a slow hub made them pile up and
		// their answers arrive out of order); after 15 s it goes on regardless (a lost answer must not stop it).
		if m.fetching && time.Since(m.fetchSince) < 15*time.Second {
			return m, tick()
		}
		m.fetching, m.fetchSince = true, time.Now()
		return m, tea.Batch(m.fetch(), tick())
	case statusMsg:
		m.fetching = false
		// A failed fetch must not blank the lists (the zero Status): keep the last good one and show the error.
		if v.err != nil {
			m.err = v.err
		} else {
			m.st, m.err = v.st, nil
		}
		if v.hub != nil {
			m.hub = *v.hub
		}
		if v.log != nil {
			m.reqlog = v.log // oldest first
		}
		if m.sc == scUsage {
			m.usage = v.use
		}
		if m.sc == scBoard {
			m.bWeek, m.bData = v.week, v.tab
		}
		if m.sc == scNodes && v.hosts != nil {
			m.nodeHost = v.hosts
		}
		if v.cfg != nil && (m.sc == scNodes || m.sc == scHub || m.sc == scEconomy) && m.editing == "" {
			// the daemon's copy is the truth (the token, API key, nodes, economy may have changed from the shell or
			// another TUI); the local discover edits are saved to the daemon at once, so nothing is lost
			disc := m.cfg.Discover
			m.cfg = *v.cfg
			if m.sc != scNodes {
				m.cfg.Discover = disc
			}
		}
		if m.sc == scProfiles && m.editing == "" && !m.confirm {
			keep := ""
			if m.cur < len(m.profList) {
				keep = m.profList[m.cur].Name
			}
			m = m.reloadProfiles(keep) // a profile saved from the shell or another TUI shows up
		}
		if m.sc == scRoleEdit {
			m.route = v.route
		}
		if m.sc == scStore && v.store != nil {
			m.storeNodes = v.store
		}
		if v.linger != "" {
			m.linger = v.linger
		}
		if (m.sc == scRuns || m.sc == scProfiles || m.sc == scNodes) && v.runsErr != nil && v.runsGen == m.runsGen {
			m.runsErr = v.runsErr
		}
		// a list asked for before a row was deleted here would bring the row back: only a later fetch is taken
		if (m.sc == scRuns || m.sc == scProfiles || m.sc == scNodes) && v.runs != nil && v.runsGen == m.runsGen {
			m.runs, m.runsErr = v.runs, nil
			if m.sc == scRuns {
				m.cur = min(m.cur, max(len(m.runs)-1, 0))
			}
		}
	case hubModelsMsg:
		m.hubRemoteBusy = false
		m.hubRemoteModels = v.models
		if v.err != nil {
			m.hubRemoteErr = v.err.Error()
		} else {
			m.hubRemoteErr = ""
		}
	case layerEvalMsg:
		if m.lay != nil && v.seq == m.lay.seq {
			ed := *m.lay
			if v.err != nil {
				ed.err, ed.ev = v.err.Error(), nil
			} else {
				ed.err, ed.ev = "", &v.ev
			}
			m.lay = &ed
		}
	case planMsg:
		m.lay = nil             // a new plan: the split being edited belonged to the old one
		if v.seq != m.planSeq { // a newer plan request is on its way
			break
		}
		m.planBusy = false
		if v.err != nil {
			m.planErr, m.planRes = v.err.Error(), nil
			break
		}
		m.planErr, m.planPath, m.planModel, m.planRes, m.planPick, m.planCtx = "", v.path, v.model, &v.res, 0, v.ctxIdx
	case askMsg:
		m.askBusy = false
		item := chatItem{
			user:   v.prompt,
			model:  v.target,
			reply:  v.reply,
			durMS:  v.durMS,
			tokens: v.tokens,
		}
		if v.err != nil {
			item.err = v.err.Error()
		} else if v.durMS > 0 {
			item.tps = float64(v.tokens) / (float64(v.durMS) / 1000.0)
		}
		if v.ep != "" {
			item.model = v.ep
		}
		m.askLog = append(m.askLog, item)
	case askExecDoneMsg:
		return m, m.fetch()
	case deployProgressMsg:
		if m.deployChan == nil { // a step that arrives after the deploy ended must not bring the banner back
			return m, nil
		}
		m.deployStatus = string(v)
		return m, waitForDeployStep(m.deployChan)
	case deployDoneMsg:
		m.deployStatus = ""
		m.deployChan, m.deployCancel = nil, nil
		if v.err != nil {
			m.note = badS.Render("deploy failed: " + v.err.Error())
		} else if v.res != nil {
			m.note = okS.Render(fmt.Sprintf("✓ Deployed %s (%s) — URL: %s (PID %d) · press 't' to test in Chat", v.res.EndpointName, v.res.Strategy, v.res.URL, v.res.MasterPID)) + badS.Render(v.warn)
		}
		return m, m.fetch()
	case noteMsg:
		m.note = string(v)
		return m, m.fetch()
	case pullAskMsg:
		if v.sc != m.sc {
			return m, nil
		}
		act := v.act
		m.pend, m.confirm = &act, true
		m.note = badS.Render(v.line + " — y fetch anyway, n cancel")
		return m, nil
	case stopPreviewMsg:
		if v.sc != m.sc { // answered after the user left the screen: "y" there must not confirm this
			return m, nil
		}
		return m.onStopPreview(v), nil
	case stopDoneMsg:
		m = m.onStopDone(v)
		return m, m.fetch()
	case pruneMsg:
		if v.sc != m.sc {
			return m, nil
		}
		m.pend = nil
		switch {
		case v.err != nil:
			m.note = badS.Render("error: " + v.err.Error())
		case len(v.gone) == 0:
			m.note = helpS.Render("nothing to prune (only ended runs are removed; active ones are kept)")
			for _, r := range m.runs {
				if r.Active() && r.Stale != "" {
					m.note = helpS.Render("nothing to prune: only ended runs are removed — " + r.ID + " looks dead: x on it closes and deletes its record")
					break
				}
			}
		default:
			var b strings.Builder
			for _, r := range v.gone {
				fmt.Fprintf(&b, "%s  %s  ended %s\n", r.ID, r.Model, r.Ended.Format("2006-01-02 15:04"))
			}
			m.outText = strings.TrimRight(b.String(), "\n")
			m.confirm = true
			m.note = helpS.Render(runsRemovedText(len(v.gone), true) + " — y delete them, n cancel")
		}
		return m, nil
	case outMsg:
		m.note, m.outText = v.note, v.text
		if m.sc == scProfiles {
			keep := ""
			if m.cur < len(m.profList) {
				keep = m.profList[m.cur].Name
			}
			m = m.reloadProfiles(keep)
		}
		return m, m.fetch()
	case regMsg:
		if v.err != nil {
			m.note = badS.Render("error: " + v.err.Error())
			break
		}
		done := map[string]bool{}
		for _, n := range v.names {
			done[n] = true
		}
		if m.disc != nil { // stay on the results; drop what was just registered
			var rest []config.Endpoint
			for _, e := range m.disc.Plan.Add {
				if !done[e.Name] {
					rest = append(rest, e)
				}
			}
			m.disc.Plan.Add = rest
			m.sel = map[int]bool{}
			m.cur = 0
		}
		m.note = okS.Render(fmt.Sprintf("✓ registered %d model(s) — %d left in this scan", len(v.names), len(m.disc.Plan.Add)))
		return m, m.fetch()
	case actMsg:
		m.confirm = false
		if v.err != nil {
			m.note = badS.Render("error: " + v.err.Error())
		} else {
			m.note = okS.Render("✓ " + v.out)
		}
	case discMsg:
		m.busy = false
		if v.err != nil {
			m.note = badS.Render("discovery error: " + v.err.Error())
			break
		}
		m.disc, m.sel, m.note = &v.r, map[int]bool{}, ""
		m.focus, m.cur, m.otherCur = 1, 0, m.cur // always show the results page
		m.inBar, m.barCur = false, 0             // a scan started from the button bar must not leave the focus there: enter/space would press buttons instead of selecting
		for i := range v.r.Plan.Add {
			m.sel[i] = true
		}
	case tea.PasteMsg: // text pasted into an input line (the terminal's bracketed paste)
		if m.editing != "" {
			m.editBuf += pastedText(v.Content)
		}
	case tea.KeyPressMsg:
		nm, cmd := m.key(v)
		if mm, ok := nm.(model); ok && (mm.sc != m.sc || mm.focus != m.focus || mm.pick != m.pick || mm.confirm != m.confirm) {
			mm.inBar, mm.barCur = false, 0
			if mm.sc != m.sc {
				mm.outText = ""
			}
			return mm, cmd
		}
		return nm, cmd
	}
	return m, nil
}

func (m model) View() tea.View {
	var b strings.Builder
	switch m.sc {
	case scHome:
		overhead := 14
		small := m.h > 0 && m.h < 26
		if small {
			overhead = 7 // a one-line banner: the whole menu (Quit included) fits 24 rows
		}
		// In expanded mode, each item takes 3 lines (title + description + spacing).
		items := homeItems()
		// Non-compact requires at least len(items)*3 + overhead lines (59 lines).
		// If the terminal cannot fit the expanded layout without clipping, use compact 1-line mode
		// so every menu item (including Quit at the bottom) remains visible.
		// headers (and the hint above) take rows too: they are shown when the window has room for them
		hint := !small
		showGroups := m.h == 0 || m.h >= 26
		extra := 0
		if showGroups {
			extra += 7 // four headers and the blank lines between the groups
		}
		if hint {
			extra += 2
		}
		compact := m.h == 0 || m.h < (len(items)*3+overhead+extra)
		if m.h == 0 || m.h >= 26 {
			b.WriteString(logoS.Render(logo) + "\n")
		} else {
			b.WriteString(logoS.Render("  fleet") + "\n")
		}
		base := ""
		if m.cl != nil {
			base = m.cl.Base
		}
		if small {
			b.WriteString(helpS.Render("  "+base+"/v1  ") + m.versionLine() + "\n\n")
		} else {
			b.WriteString(subS.Render("  Model fleet: one endpoint · role-based routing · hot configuration") + "\n")
			b.WriteString(helpS.Render("  "+base+"/v1") + "\n")
			b.WriteString(helpS.Render("  ") + m.versionLine() + "\n\n")
		}

		if m.h > 0 && compact && m.h < len(items)+overhead+extra {
			showGroups, hint, extra = false, false, 0 // a short terminal keeps the plain list: every item first
		}
		if hint {
			b.WriteString(helpS.Render("  "+homeHint()) + "\n\n")
		}
		itemsTop, itemsEnd := 0, len(items)
		if compact && m.h > 0 && m.h < len(items)+overhead+extra {
			budget := m.h - overhead - extra
			if budget < 3 {
				budget = 3
			}
			itemsTop, itemsEnd = window(len(items), m.cur, budget)
			if itemsTop > 0 {
				b.WriteString(helpS.Render(fmt.Sprintf("  ↑ %d more items\n", itemsTop)))
			}
		}
		for i := itemsTop; i < itemsEnd; i++ {
			it := items[i]
			if showGroups && it.group != "" {
				if i > itemsTop {
					b.WriteString("\n")
				}
				b.WriteString("  " + groupS.Render(strings.ToUpper(it.group)) + "\n")
			}
			cur, st, ds := "  ", normalS.PaddingLeft(2), descS.PaddingLeft(4)
			if i == m.cur {
				cur, st, ds = activeS.Render("▸ "), activeS.PaddingLeft(0), actDesc.PaddingLeft(4)
			}
			if compact {
				w := 78
				if m.w > 0 {
					w = m.w - 2
				}
				b.WriteString(cur + st.Render(it.icon+" "+it.title) + " " + descS.Render(trunc("— "+it.desc, max(10, w-ansi.StringWidth(it.title)-8))) + "\n")
				continue
			}
			b.WriteString(cur + st.Render(it.icon+" "+it.title) + "\n    " + ds.Render(it.desc) + "\n\n")
		}
		if compact && itemsEnd < len(items) {
			b.WriteString(helpS.Render(fmt.Sprintf("  ↓ %d more items\n", len(items)-itemsEnd)))
		}
		b.WriteString(m.summary())
	case scDash:
		b.WriteString(titleS.Render("Dashboard") + "\n\n" + m.viewDash())
	case scRoles:
		b.WriteString(titleS.Render("Roles") + "\n\n" + m.viewRoles())
	case scDiscover:
		b.WriteString(titleS.Render("Discover") + "\n\n" + m.viewDiscover())
	case scBoard:
		b.WriteString(titleS.Render("Leaderboard") + "\n\n" + m.viewBoard())
	case scActivity:
		b.WriteString(titleS.Render("Activity") + "\n\n" + m.viewActivity())
	case scUsage:
		b.WriteString(titleS.Render("Usage") + "\n\n" + m.viewUsage())
	case scNodes:
		b.WriteString(titleS.Render("Nodes") + "\n\n" + m.viewNodes())
	case scPlan:
		b.WriteString(titleS.Render("Plan a model") + "\n\n" + m.viewPlan())
	case scStore:
		b.WriteString(titleS.Render("Model files") + "\n\n" + m.viewStore())
	case scRuns:
		b.WriteString(titleS.Render("Runs") + "\n\n" + m.viewRuns())
	case scEconomy:
		b.WriteString(titleS.Render("Economy & Wallet") + "\n\n" + m.viewEconomy())
	case scHub:
		b.WriteString(titleS.Render("Fleet Hub & Cloud Peering") + "\n\n" + m.viewHub())
	case scProfiles:
		b.WriteString(titleS.Render("Profiles") + "\n\n" + m.viewProfilesScreen())
	case scInteg:
		b.WriteString(titleS.Render("Integrations") + "\n\n" + m.viewInteg())
	case scRoleEdit:
		b.WriteString(titleS.Render("Role settings") + "\n\n" + m.viewRoleEdit())
	case scCaps:
		b.WriteString(titleS.Render("Capabilities") + "\n\n" + m.viewCaps())
	case scAsk:
		b.WriteString(m.viewAsk())
	}
	body := b.String()
	var f strings.Builder
	f.WriteString(m.viewOut())
	if p := m.editPrompt(); p != "" {
		f.WriteString("\n" + p)
	}
	if m.err != nil {
		f.WriteString("\n" + badS.Render("daemon: "+m.err.Error()))
	}
	if ps := m.st.Probe; ps.Running {
		f.WriteString("\n" + accS.Render(fmt.Sprintf("🔬 probe %d/%d — %s …", ps.Done, ps.Total, strings.Join(ps.Current, ", "))) + "\n")
	}
	if bs := m.st.Bench; bs.Running {
		f.WriteString("\n" + accS.Render(fmt.Sprintf("⏱  benchmark %d/%d — measuring %s …", bs.Done, bs.Total, bs.Current)) + "\n")
	}
	if bar := m.viewBar(); bar != "" {
		f.WriteString("\n" + bar)
	}
	f.WriteString("\n" + helpS.Render(m.help()))
	if m.sc != scHome { // the home screen shows it in the banner
		f.WriteString("\n" + helpS.Render("  ") + m.versionLine())
	}
	if m.sc != scHome {
		body = m.fitBody(body, f.String())
	} else if m.note != "" {
		body += "\n" + m.note + "\n"
	}
	v := tea.NewView(body + f.String())
	v.AltScreen = true
	return v
}

package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/server"

	"github.com/tevfik/fleet-router/internal/client"
	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/discover"
	"github.com/tevfik/fleet-router/internal/hostinfo"
)

func testModel() model {
	add := []config.Endpoint{
		{Name: "dino-gemma4-12b", Node: "dino", Model: "gemma4:12b"},
		{Name: "dino-qwen", Node: "dino", Model: "qwen3:8b"},
		{Name: "victor-gemma", Node: "victor", Model: "gemma3:4b"},
	}
	m := model{sel: map[int]bool{}, focus: 1}
	m.disc = &client.DiscoverResult{Plan: discover.Plan{Add: add}}
	return m
}

func TestVisRowsGroupsAndFilter(t *testing.T) {
	m := testModel()
	if got := len(m.itemRows()); got != 5 { // 2 headers + 3 items
		t.Fatalf("rows = %d, want 5", got)
	}
	if got := len(m.visRows()); got != 5 {
		t.Fatalf("visible rows = %d, want 5", got)
	}
	m.filter = "gemma"
	rows := m.itemRows()
	if len(rows) != 4 { // dino header + gemma4, victor header + gemma3
		t.Fatalf("filtered rows = %d, want 4", len(rows))
	}
	m.collapsed = map[string]bool{"dino": true}
	if got := len(m.itemRows()); got != 3 {
		t.Fatalf("collapsed rows = %d, want 3", got)
	}
}

func TestCollapseExpandAndGroupToggle(t *testing.T) {
	m := testModel()
	// cursor on the first header (dino): ← collapses, → expands
	nm, _ := m.keyDiscover("left")
	m = nm.(model)
	if !m.collapsed["dino"] {
		t.Fatal("left on a header should collapse the group")
	}
	nm, _ = m.keyDiscover("right")
	m = nm.(model)
	if m.collapsed["dino"] {
		t.Fatal("right on a header should expand the group")
	}
	// space on a header selects the whole machine, again deselects it
	nm, _ = m.keyDiscover(" ")
	m = nm.(model)
	if !m.sel[0] || !m.sel[1] || m.sel[2] {
		t.Fatalf("group toggle wrong: %v", m.sel)
	}
	nm, _ = m.keyDiscover(" ")
	m = nm.(model)
	if m.sel[0] || m.sel[1] {
		t.Fatalf("second toggle should clear the group: %v", m.sel)
	}
	// 'a' selects everything visible, 'n' clears it
	nm, _ = m.keyDiscover("a")
	m = nm.(model)
	if !(m.sel[0] && m.sel[1] && m.sel[2]) {
		t.Fatalf("select all failed: %v", m.sel)
	}
	nm, _ = m.keyDiscover("n")
	m = nm.(model)
	if m.sel[0] || m.sel[1] || m.sel[2] {
		t.Fatalf("select none failed: %v", m.sel)
	}
}

func TestPortPresetToggleAndFormat(t *testing.T) {
	m := model{cfg: config.Config{Discover: config.Discover{Ports: "8080,11434"}}, hosts: []string{"dino"}}
	rows := m.menuRows()
	find := func(spec string) mrow {
		for _, r := range rows {
			if r.kind == "port" && r.spec == spec {
				return r
			}
		}
		t.Fatalf("no row for %s", spec)
		return mrow{}
	}
	nm, _ := m.togglePorts(find("1234").ports)
	if got := nm.cfg.Discover.Ports; got != "1234,8080,11434" {
		t.Fatalf("after enabling 1234: %q", got)
	}
	nm, _ = nm.togglePorts(find("8080").ports)
	if got := nm.cfg.Discover.Ports; got != "1234,11434" {
		t.Fatalf("after disabling 8080: %q", got)
	}
	nm, _ = nm.togglePorts(find("30000-30020").ports)
	if got := nm.cfg.Discover.Ports; got != "1234,11434,30000-30020" {
		t.Fatalf("range preset: %q", got)
	}
}

func TestButtonBarNavigation(t *testing.T) {
	m := model{sc: scDash, sel: map[int]bool{}}
	for i := 0; i < 3; i++ {
		m.st.Endpoints = append(m.st.Endpoints, server.EpStatus{Endpoint: config.Endpoint{Name: "e"}})
	}
	m.cur = 2 // last endpoint
	nm, _ := m.keyStr("down", "")
	m = nm.(model)
	if !m.inBar {
		t.Fatal("↓ on the last row should move focus to the button bar")
	}
	nm, _ = m.keyStr("right", "")
	m = nm.(model)
	if m.barCur != 1 {
		t.Fatalf("→ should highlight the next button, barCur=%d", m.barCur)
	}
	nm, _ = m.keyStr("up", "")
	m = nm.(model)
	if !m.inBar || m.cur != 1 {
		t.Fatalf("↑ should move through the models and keep the bar focused (inBar=%v cur=%d)", m.inBar, m.cur)
	}
	nm, _ = m.keyStr("tab", "")
	if m = nm.(model); m.inBar {
		t.Fatal("tab should leave the bar")
	}
	// last button is "← Back": pressing enter leaves the dashboard
	m.inBar, m.barCur = true, len(m.buttons())-1
	nm, _ = m.keyStr("enter", "")
	m = nm.(model)
	if m.sc != scHome || m.inBar {
		t.Fatalf("Back button should return to the menu, sc=%v inBar=%v", m.sc, m.inBar)
	}
}

func TestRegisteringStaysOnResults(t *testing.T) {
	m := testModel()
	m.sc, m.focus = scDiscover, 1
	nm, _ := m.Update(regMsg{names: []string{"dino-gemma4-12b"}})
	m = nm.(model)
	if m.sc != scDiscover || m.focus != 1 || m.disc == nil {
		t.Fatalf("must stay on the results page (sc=%v focus=%d)", m.sc, m.focus)
	}
	if got := len(m.disc.Plan.Add); got != 2 {
		t.Fatalf("registered model should be removed from the list, %d left", got)
	}
}

func TestBarReachableFromAnywhere(t *testing.T) {
	m := model{sc: scDash, sel: map[int]bool{}}
	for i := 0; i < 20; i++ {
		m.st.Endpoints = append(m.st.Endpoints, server.EpStatus{Endpoint: config.Endpoint{Name: "e"}})
	}
	m.cur = 7 // middle of a long list
	nm, _ := m.keyStr("right", "")
	if m = nm.(model); !m.inBar || m.barCur != 0 || m.cur != 7 {
		t.Fatalf("→ in the middle should enter the bar: inBar=%v cur=%d", m.inBar, m.cur)
	}
	m.inBar, m.cur = false, 0
	nm, _ = m.keyStr("up", "")
	if m = nm.(model); !m.inBar {
		t.Fatal("↑ on the first row should wrap to the bar")
	}
}

func TestActivityScreen(t *testing.T) {
	now := time.Now()
	m := model{sc: scActivity, w: 100, h: 30, sel: map[int]bool{}, reqlog: []server.LogEntry{
		{Time: now, Model: "scout", Endpoint: "bonsai", Status: 200, Attempts: 1, DurMS: 1200, TTFTMS: 200, TPS: 110, In: 20, Out: 100},
		{Time: now.Add(time.Second), Model: "architect", Endpoint: "gemma", Status: 200, Attempts: 2, DurMS: 4000},
		{Time: now.Add(2 * time.Second), Model: "nope", Status: 404, Attempts: 0, Err: `{"error":"model not found"}`},
	}}
	v := m.viewActivity()
	i404, iScout := strings.Index(v, "nope"), strings.Index(v, "scout")
	if i404 < 0 || iScout < 0 || i404 > iScout {
		t.Fatalf("newest request should come first:\n%s", v)
	}
	if !strings.Contains(v, "↪2") || !strings.Contains(v, "model not found") {
		t.Fatalf("failover marker or error missing:\n%s", v)
	}
	if m.listLen() != 3 {
		t.Fatalf("listLen %d", m.listLen())
	}
	// home → Activity
	h := model{sc: scHome, cur: homeIdx("Activity"), sel: map[int]bool{}}
	nm, _ := h.keyStr("enter", "")
	if nm.(model).sc != scActivity {
		t.Fatal("Activity should be the 6th home entry")
	}
}

func TestUsageScreen(t *testing.T) {
	m := model{sc: scUsage, w: 110, h: 30, sel: map[int]bool{}, usage: []server.UsageRow{
		{Key: "opencode", Requests: 40, Errors: 1, In: 12000, Out: 3400, AvgMS: 2100, TPS: 88.5},
		{Key: "claude-cli", Requests: 10, In: 900, Out: 300, AvgMS: 800},
	}}
	v := m.viewUsage()
	for _, want := range []string{"by client", "last 24h", "opencode", "12k", "88.5", "claude-cli"} {
		if !strings.Contains(v, want) {
			t.Fatalf("missing %q:\n%s", want, v)
		}
	}
	if m.listLen() != 2 {
		t.Fatalf("listLen %d", m.listLen())
	}
	nm, _ := m.keyStr("g", "")
	if nm.(model).useBy != 1 {
		t.Fatal("g should change the grouping")
	}
	nm, _ = nm.(model).keyStr("p", "")
	if nm.(model).useSince != 1 {
		t.Fatal("p should change the period")
	}
	h := model{sc: scHome, cur: homeIdx("Usage"), sel: map[int]bool{}}
	nm, _ = h.keyStr("enter", "")
	if nm.(model).sc != scUsage {
		t.Fatal("Usage should be the 7th home entry")
	}
}

func TestLeaderboardViews(t *testing.T) {
	day := func(back int) string { return time.Now().AddDate(0, 0, -back).Format("2006-01-02") }
	m := model{sc: scBoard, w: 120, h: 40, sel: map[int]bool{}}
	m.st.Endpoints = []server.EpStatus{
		{Endpoint: config.Endpoint{Name: "a", Node: "dino", Model: "gemma", Ctx: 131072, TPS: 40}, TPSSrc: "live", TTFTMS: 900},
		{Endpoint: config.Endpoint{Name: "b", Node: "victor", Model: "bonsai", Ctx: 32768, TPS: 118}, TPSSrc: "live", TTFTMS: 150},
	}
	m.bWeek = []server.UsageRow{{Key: "dino/gemma", Requests: 5, In: 4000, Out: 1000}, {Key: "victor/bonsai", Requests: 20, In: 90000, Out: 10000}}
	v := m.viewBoard()
	for _, want := range []string{"Speed", "Daily", "all machines", "FASTEST", "victor/bonsai", "118 tok/s", "MOST USED", "100k tokens", "FIRST TOKEN", "128k tokens"} {
		if !strings.Contains(v, want) {
			t.Fatalf("missing %q:\n%s", want, v)
		}
	}
	// machine filter narrows the ranking and the cards
	m.bNode = "dino"
	if rows := m.speedRows(); len(rows) != 1 || rows[0].Name != "a" {
		t.Fatalf("filtered rows %+v", rows)
	}
	if strings.Contains(m.viewBoard(), "bonsai") {
		t.Fatal("the dino filter must hide victor's model")
	}
	m.bNode = ""

	m.bTab = 1
	m.bData = []server.UsageRow{{Key: "victor/bonsai", Requests: 20, In: 900, Out: 100}, {Key: "dino/gemma", Requests: 5, In: 90, Out: 10}}
	v = m.viewBoard()
	if i, j := strings.Index(v, "victor/bonsai"), strings.Index(v, "dino/gemma"); i < 0 || j < 0 || i > j || !strings.Contains(v, "90.9%") {
		t.Fatalf("usage today ranking:\n%s", v)
	}

	m.bTab = 2
	m.bData = []server.UsageRow{{Key: "scout", Sub: "dino/gemma", Requests: 1, In: 10, Out: 10}, {Key: "scout", Sub: "victor/bonsai", Requests: 3, In: 70, Out: 10}, {Key: "coder", Sub: "dino/gemma", Requests: 2, In: 5, Out: 5}}
	v = m.viewBoard()
	if !strings.Contains(v, "SCOUT") || !strings.Contains(v, "CODER") || !strings.Contains(v, "80.0%") {
		t.Fatalf("by role:\n%s", v)
	}
	if m.listLen() != len(m.roleLines()) || m.listLen() < 5 {
		t.Fatalf("listLen %d", m.listLen())
	}

	m.bTab = 3
	m.bData = []server.UsageRow{{Key: day(0), Sub: "dino/gemma", Requests: 1, In: 50, Out: 50}, {Key: day(0), Sub: "victor/bonsai", Requests: 1, In: 100, Out: 0}, {Key: day(2), Sub: "dino/gemma", Requests: 1, In: 30, Out: 0}}
	v = m.viewBoard()
	if !strings.Contains(v, day(0)[5:]) || !strings.Contains(v, "■") || !strings.Contains(v, "200") {
		t.Fatalf("daily:\n%s", v)
	}

	// keys: 1-4 switch the view, n cycles machines
	nm, _ := model{sc: scBoard, sel: map[int]bool{}}.keyStr("3", "")
	if nm.(model).bTab != 2 {
		t.Fatal("3 should open the by-role view")
	}
	m.bTab = 0
	nm, _ = m.keyStr("n", "")
	if nm.(model).bNode != "dino" {
		t.Fatalf("n should move to the next machine, got %q", nm.(model).bNode)
	}
}

func TestNodesScreen(t *testing.T) {
	m := model{sc: scNodes, w: 120, h: 50, sel: map[int]bool{}}
	m.st.Endpoints = []server.EpStatus{
		{Endpoint: config.Endpoint{Name: "a", Node: "dino", Model: "gemma", SSH: "dino"}, Known: true, OK: true,
			Probe: &server.ProbeResult{At: time.Now(), Found: []string{"chat", "tools"}, Missing: []string{"vision"}, Why: map[string]string{"vision": "http 400"}, TPS: 41}},
		{Endpoint: config.Endpoint{Name: "b", Node: "victor", Model: "bonsai", SSH: "victor"}, Known: true},
	}
	m.st.Nodes = []server.NodeStatus{{Node: "dino", Via: "ssh dino", Up: 1, Total: 1, PingMS: 12, AvgMS: 10, MinMS: 8, MaxMS: 14, History: []int64{8, 12, 14}},
		{Node: "victor", Via: "ssh victor", Up: 0, Total: 1}}
	m.nodeHost = map[string]server.HostReport{
		"dino": {Node: "dino", Fleet: "dino", Source: "ssh dino", At: time.Now(), Snapshot: hostinfo.Snapshot{
			Static: hostinfo.Static{Host: "dino", OS: "Ubuntu", CPUName: "Grace", Threads: 20},
			Now: hostinfo.Sample{CPU: 37, RAMUsed: 60 << 30, RAMTotal: 128 << 30, Temps: []hostinfo.Temp{{Name: "cpu", C: 55}},
				GPUs:  []hostinfo.GPU{{Name: "NVIDIA GB10", Util: 93, MemUsed: -1, MemTotal: -1, Temp: 71, Power: 40, PowerLimit: -1}},
				Disks: []hostinfo.Disk{{Mount: "/", Used: 1 << 40, Total: 4 << 40}}, DiskRead: 1, DiskWrite: 2},
			History: map[string][]float64{"cpu": {10, 37}, "gpu": {90, 93}}}},
		"victor": {Node: "victor", Err: "no fleet runs on victor (port 4711) — install it there to see the machine", At: time.Now()},
	}
	v := m.viewNodes()
	for _, want := range []string{"dino", "37%", "47%", "93%", "71°C", "NVIDIA GB10", "VRAM shared with RAM", "12ms (avg 10, 8–14)", "✓ chat tools", "✗ vision (http 400)", "~41 tok/s"} {
		if !strings.Contains(v, want) {
			t.Fatalf("missing %q:\n%s", want, v)
		}
	}
	m.cur = 1
	if v := m.viewNodes(); !strings.Contains(v, "no fleet runs on victor") {
		t.Fatalf("the reason a machine cannot be read must show:\n%s", v)
	}
	nm, _ := m.keyStr("enter", "")
	if mm := nm.(model); mm.sc != scDash || mm.filter != "victor" {
		t.Fatal("enter should open the node's models on the Dashboard")
	}
	h := model{sc: scHome, cur: homeIdx("Nodes"), sel: map[int]bool{}}
	if nm, _ := h.keyStr("enter", ""); nm.(model).sc != scNodes {
		t.Fatal("Nodes should be the 2nd home entry")
	}
}

func TestHomeMenuQuitVisibility(t *testing.T) {
	// Terminal heights where Quit was previously cut off (e.g. 38, 40, 45, 50)
	for _, height := range []int{24, 30, 38, 40, 45, 50, 60} {
		m := model{sc: scHome, w: 100, h: height, sel: map[int]bool{}}
		v := m.View()
		rendered := v.Content
		if !strings.Contains(rendered, "Quit") {
			t.Errorf("at height %d, 'Quit' should be visible in home menu, but was missing:\n%s", height, rendered)
		}
	}

	// For very small height (e.g. 18 lines), when navigated to bottom, Quit must be in view
	mSmall := model{sc: scHome, w: 100, h: 18, cur: len(home) - 1, sel: map[int]bool{}}
	if !strings.Contains(mSmall.View().Content, "Quit") {
		t.Errorf("at height 18 with cursor on Quit, 'Quit' must be visible")
	}
}

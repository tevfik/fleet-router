package tui

import (
	tea "charm.land/bubbletea/v2"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/tevfik/fleet-router/internal/plan"
)

func plain(s string) string { return ansi.Strip(s) }

// The grouped home menu: every entry is in exactly one group, the groups are in the order of the work, and each
// header is printed once, above its first entry, at any height that has room for it.
func TestHome_GroupsCoverEveryEntryInOrder(t *testing.T) {
	var groups []string
	cur := ""
	for i, it := range home {
		if it.group != "" {
			cur = it.group
			groups = append(groups, cur)
		}
		if cur == "" {
			t.Fatalf("entry %d (%s) is before the first group", i, it.title)
		}
	}
	if strings.Join(groups, "|") != "Use|Run a model|Machines & routing|Advanced" {
		t.Fatalf("groups: %v", groups)
	}
	if home[len(home)-1].title != "Quit" {
		t.Fatal("Quit must stay last")
	}
}

func TestHome_RenderAtManyHeights(t *testing.T) {
	for _, h := range []int{0, 24, 26, 30, 38, 45, 62, 70} {
		for cur := 0; cur < len(home); cur++ {
			m := model{sc: scHome, cur: cur, h: h, w: 120, sel: map[int]bool{}}
			out := plain(m.View().Content)
			// the highlighted entry is always on screen, and so is Quit when the whole list fits
			if !strings.Contains(out, "▸ "+strings.TrimSpace(home[cur].icon)) && !strings.Contains(out, home[cur].title) {
				t.Errorf("h=%d cur=%d (%s) not on screen", h, cur, home[cur].title)
			}
			if rows := strings.Count(out, "\n") + 1; h > 0 && rows > h {
				t.Errorf("h=%d cur=%d: %d rows do not fit", h, cur, rows)
			}
		}
	}
}

func TestHome_HeadersOnlyWhenTheyFit(t *testing.T) {
	tall := plain(model{sc: scHome, h: 50, w: 120, sel: map[int]bool{}}.View().Content)
	for _, g := range []string{"USE", "RUN A MODEL", "MACHINES & ROUTING", "ADVANCED"} {
		if strings.Count(tall, g) != 1 {
			t.Errorf("%q printed %d times at 50 rows", g, strings.Count(tall, g))
		}
	}
	if !strings.Contains(tall, "New here?") || !strings.Contains(tall, "Quit") {
		t.Errorf("the start line and Quit are expected at 50 rows:\n%s", tall)
	}
	small := plain(model{sc: scHome, h: 24, w: 100, sel: map[int]bool{}}.View().Content)
	if strings.Contains(small, "RUN A MODEL") || strings.Contains(small, "New here?") || !strings.Contains(small, "Quit") {
		t.Errorf("24 rows keep the plain list with Quit:\n%s", small)
	}
	// the order on screen is the order of the groups
	i1, i2, i3, i4 := strings.Index(tall, "USE"), strings.Index(tall, "RUN A MODEL"), strings.Index(tall, "MACHINES"), strings.Index(tall, "ADVANCED")
	if !(i1 < i2 && i2 < i3 && i3 < i4) {
		t.Errorf("groups out of order: %d %d %d %d", i1, i2, i3, i4)
	}
}

// The cursor walks the entries, not the headers: down from the last entry of a group lands on the first of the next.
func TestHome_CursorSkipsHeaders(t *testing.T) {
	m := model{sc: scHome, h: 50, w: 120, sel: map[int]bool{}}
	seen := []string{home[0].title}
	for i := 0; i < len(home)-1; i++ {
		nm, _ := m.keyStr("down", "")
		m = nm.(model)
		seen = append(seen, home[m.cur].title)
	}
	if len(seen) != len(home) || seen[len(seen)-1] != "Quit" {
		t.Fatalf("walk: %v", seen)
	}
	// enter on each entry reaches its own screen (the titles are what the dispatch uses)
	_, cl := newNodesDaemon(t)
	for _, title := range []string{"Dashboard", "Plan", "Profiles", "Runs", "Model files", "Nodes", "Discover", "Roles", "Integrations", "Fleet Hub", "Economy & Wallet", "Activity", "Usage", "Leaderboard"} {
		m := model{cl: cl, sc: scHome, cur: homeIdx(title), h: 50, w: 120, sel: map[int]bool{}}
		nm, _ := m.keyStr("enter", "")
		if nm.(model).sc == scHome {
			t.Errorf("enter on %s stays on the menu", title)
		}
	}
}

// ---- the folded advanced settings of the plan screen ----

func planWithCandidates(t *testing.T) model {
	t.Helper()
	_, cl := newNodesDaemon(t)
	m := model{cl: cl, sel: map[int]bool{}, w: 140, h: 60}.enterPlan()
	r := plan.Result{Model: "big", Ctx: 8192, Np: 1, Candidates: []plan.Candidate{{Kind: "single", Master: "dino", Parts: []plan.Part{{Node: "dino", Layers: 32}}}}}
	m.planRes, m.planFiles, m.planPath = &r, []string{"/models/a.gguf"}, "/models/a.gguf"
	return m
}

func TestPlanAdvanced_FoldedByDefaultAndWalkable(t *testing.T) {
	m := planWithCandidates(t)
	v := plain(m.viewPlan())
	for _, shown := range []string{"context", "slots", "QoS mode", "master", "advanced"} {
		if !strings.Contains(v, shown) {
			t.Errorf("%q missing from the folded view", shown)
		}
	}
	for _, hidden := range []string{"moe cache  ", "spec engine  ", "vision  ", "paging  ", "engine     ["} {
		if strings.Contains(v, hidden) {
			t.Errorf("%q should be folded away:\n%s", hidden, v)
		}
	}
	// ↓ from the files walks context → slots → qos → master → advanced → candidates
	m.planSection = 1
	var walk []int
	for i := 0; i < 5; i++ {
		walk = append(walk, m.planParamCur)
		nm, _ := m.keyStr("down", "")
		m = nm.(model)
	}
	if m.planSection != 2 || len(walk) != 5 || walk[3] != 3 || walk[4] != planAdvRow {
		t.Fatalf("walk %v, ended in section %d", walk, m.planSection)
	}
	// ↑ from the candidates lands on the advanced line, then master
	nm, _ := m.keyStr("up", "")
	m = nm.(model)
	if m.planSection != 1 || m.planParamCur != planAdvRow {
		t.Fatalf("up from the candidates: section %d param %d", m.planSection, m.planParamCur)
	}
}

func TestPlanAdvanced_OpenAndFold(t *testing.T) {
	m := planWithCandidates(t)
	m.planSection, m.planParamCur = 1, planAdvRow
	nm, _ := m.keyStr("enter", "")
	m = nm.(model)
	if !m.planAdv {
		t.Fatal("enter on the advanced line opens it")
	}
	v := plain(m.viewPlan())
	for _, shown := range []string{"target ", "moe cache ", "spec engine ", "vision ", "paging ", "engine "} {
		if !strings.Contains(v, shown) {
			t.Errorf("%q missing from the open view", shown)
		}
	}
	// open: ↓ goes through all six, then to the candidates
	var walk []int
	for i := 0; i < 7; i++ {
		nm, _ := m.keyStr("down", "")
		m = nm.(model)
		walk = append(walk, m.planParamCur)
	}
	if m.planSection != 2 || len(walk) != 7 {
		t.Fatalf("open walk %v ended in section %d", walk, m.planSection)
	}
	if walk[0] != 4 || walk[5] != 9 {
		t.Fatalf("open walk %v: target first, engine last", walk)
	}
	// ←/→ on the line folds it again
	m.planSection, m.planParamCur = 1, planAdvRow
	nm, _ = m.keyStr("left", "")
	if nm.(model).planAdv {
		t.Fatal("← on the advanced line folds it")
	}
}

// What was changed inside the fold shows on the folded line, and a warning about the engine is never hidden.
func TestPlanAdvanced_ChangesAndWarningsStayVisible(t *testing.T) {
	m := planWithCandidates(t)
	m.planCPUOnly, m.planSpecMode, m.planEngineMode = true, 2, 2
	v := plain(m.viewPlan())
	for _, want := range []string{"changed:", "CPU only", "spec ", "engine chain"} {
		if !strings.Contains(v, want) {
			t.Errorf("folded line lacks %q:\n%s", want, v)
		}
	}
	m.planAdv = true
	if v := plain(m.viewPlan()); strings.Contains(v, "changed:") {
		t.Error("an open line does not repeat the settings")
	}
}

// The digit jump to section 2 still works with the fold, and the settings inside the fold still change and re-plan.
func TestPlanAdvanced_SettingsInsideStillWork(t *testing.T) {
	m := planWithCandidates(t)
	m.planAdv, m.planSection, m.planParamCur = true, 1, 6 // spec engine
	before := m.planSpecMode
	nm, _ := m.keyStr("right", "")
	if nm.(model).planSpecMode == before {
		t.Fatal("→ must change the spec engine inside the open fold")
	}
	m = nm.(model)
	nm, _ = m.keyStr("2", "")
	if got := nm.(model); got.planSection != 1 {
		t.Fatalf("2 jumps to the settings: %d", got.planSection)
	}
}

func TestPlanAdvanced_OpenLineHintDoesNotContradict(t *testing.T) {
	m := planWithCandidates(t)
	m.planAdv = true
	if v := plain(m.viewPlan()); strings.Contains(v, "folds them again  (enter or") {
		t.Errorf("an open line must not also say it opens:\n%s", v)
	}
}

// Every menu icon is one wide emoji (2 cells). Narrow-by-default glyphs (🖥 🛰 🗂) are drawn 1 or 2 cells wide depending on
// the terminal, which pushes the title of that line out of the column.
func TestHome_IconsAreTwoCellsWide(t *testing.T) {
	for _, it := range home {
		if w := ansi.StringWidth(it.icon); w != 2 {
			t.Errorf("%s: icon %q is %d cells wide", it.title, it.icon, w)
		}
		if strings.ContainsRune(it.icon, '️') || strings.HasSuffix(it.icon, " ") {
			t.Errorf("%s: icon %q must be a bare wide emoji", it.title, it.icon)
		}
	}
}

// Before a plan exists ③ (the machines) is still a place to go: 3, ↓ and tab reach it, a hint says what to do, and ↓ from there
// takes the button bar. It used to refuse (3 did nothing, ↓ stopped on the last setting).
func TestPlan_SectionThreeIsReachableBeforeAnyPlan(t *testing.T) {
	_, cl := newNodesDaemon(t)
	m := model{cl: cl, sel: map[int]bool{}, w: 140, h: 60}.enterPlan()
	m.planFiles = []string{"/models/a.gguf"}

	// the digit
	nm, _ := m.keyStr("2", "")
	m = nm.(model)
	nm, _ = m.keyStr("3", "")
	m = nm.(model)
	if m.planSection != 2 {
		t.Fatalf("3 must focus ③, section %d", m.planSection)
	}
	if v := plain(m.viewPlan()); !strings.Contains(v, "nothing to choose yet") {
		t.Fatalf("③ says what to do first:\n%s", v)
	}
	nm, _ = m.keyStr("4", "")
	if nm.(model).planSection != 2 {
		t.Fatal("4 (the deploy line) needs candidates")
	}
	// enter there explains instead of selecting a candidate that is not there
	nm, _ = m.keyStr("enter", "")
	if n := plain(nm.(model).note); !strings.Contains(n, "no candidates yet") {
		t.Fatalf("note %q", n)
	}

	// ↓ from the last setting (the folded advanced line, then the open ones) reaches ③, one more takes the bar
	for _, open := range []bool{false, true} {
		m := model{cl: cl, sel: map[int]bool{}, w: 140, h: 60}.enterPlan()
		m.planAdv, m.planSection = open, 1
		m.planParamCur = m.planParamLast()
		nm, _ := m.keyStr("down", "")
		m = nm.(model)
		if m.planSection != 2 {
			t.Fatalf("open=%v: ↓ from the last setting goes to ③, section %d", open, m.planSection)
		}
		nm, _ = m.key(tea.KeyPressMsg{Code: tea.KeyDown})
		if !nm.(model).inBar {
			t.Fatalf("open=%v: ↓ from ③ takes the button bar", open)
		}
	}

	// tab walks ① → ② → ③ → the bar
	m = model{cl: cl, sel: map[int]bool{}, w: 140, h: 60}.enterPlan()
	var got []int
	for i := 0; i < 2; i++ {
		nm, _ := m.key(tea.KeyPressMsg{Code: tea.KeyTab})
		m = nm.(model)
		got = append(got, m.planSection)
	}
	if got[0] != 1 || got[1] != 2 {
		t.Fatalf("tab walk %v", got)
	}
	nm, _ = m.key(tea.KeyPressMsg{Code: tea.KeyTab})
	if !nm.(model).inBar {
		t.Fatal("tab from ③ takes the bar")
	}
}

// A long result note (a failed deploy with its advice) wraps to the width instead of being cut at the right edge.
func TestNote_LongErrorWrapsAndKeepsItsEnd(t *testing.T) {
	_, cl := newNodesDaemon(t)
	err := "deploy failed: deploy-chain: the model Ornith-1.5-122B-Q4_K_M.gguf is not on victor (looked at models/Ornith-1.5-122B-Q4_K_M.gguf and in its model directories); copy it with `fleet store pull` or register it there with `fleet store add`"
	for _, w := range []int{80, 120} {
		m := model{cl: cl, sc: scPlan, sel: map[int]bool{}, w: w, h: 60, note: badS.Render(err)}.enterPlan()
		m.note = badS.Render(err)
		out := plain(m.fitBody("Plan a model\n\nbody\n", ""))
		if !strings.Contains(strings.Join(strings.Fields(out), " "), "register it there with `fleet store add`") {
			t.Errorf("w=%d: the end of the error is gone:\n%s", w, out)
		}
		for _, l := range strings.Split(out, "\n") {
			if ansi.StringWidth(l) > w {
				t.Errorf("w=%d: a line is %d wide: %q", w, ansi.StringWidth(l), l)
			}
		}
	}
}

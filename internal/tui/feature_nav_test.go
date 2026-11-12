package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/tevfik/fleet-router/internal/plan"
)

// Every action must be reachable with the arrow keys alone; the letters are shortcuts. These tests press
// real key messages through Update (the same path the terminal takes) and check that each shortcut of a
// screen has a button, and that the buttons do what the letters do.

var arrowKeys = map[string]tea.KeyPressMsg{
	"up": {Code: tea.KeyUp}, "down": {Code: tea.KeyDown}, "left": {Code: tea.KeyLeft}, "right": {Code: tea.KeyRight},
	"enter": {Code: tea.KeyEnter}, "tab": {Code: tea.KeyTab}, "esc": {Code: tea.KeyEscape},
}

// arrows presses named keys through Update, running the commands they return and feeding the results back.
func arrows(t *testing.T, m model, keys ...string) model {
	t.Helper()
	for _, k := range keys {
		msg, ok := arrowKeys[k]
		if !ok {
			t.Fatalf("no key %q", k)
		}
		nm, cmd := m.Update(msg)
		m = nm.(model)
		m = settle(t, m, cmd)
	}
	return m
}

// settle runs a command and delivers the messages that are about this screen (plan results); the rest
// (notes, refreshes) are delivered too, since the daemon is the fake one.
func settle(t *testing.T, m model, cmd tea.Cmd) model {
	t.Helper()
	if cmd == nil {
		return m
	}
	msg := cmd()
	switch v := msg.(type) {
	case tea.BatchMsg:
		for _, c := range v {
			m = settle(t, m, c)
		}
	case nil:
	default:
		nm, next := m.Update(msg)
		m = nm.(model)
		if _, again := msg.(statusMsg); !again && next != nil {
			if _, isTick := msg.(tickMsg); !isTick {
				return m
			}
		}
	}
	return m
}

func buttonKeys(m model) string {
	var k []string
	for _, b := range m.buttons() {
		k = append(k, b.key)
	}
	return strings.Join(k, " ")
}

// What each screen's key handler understands, by letter. Every one of them needs a button.
func TestNav_EveryShortcutHasAButton(t *testing.T) {
	roleM := roleModel()
	disc := testModel()
	disc.sc, disc.focus = scDiscover, 0
	for _, c := range []struct {
		name string
		m    model
		keys []string
	}{
		{"dashboard", model{sc: scDash}, []string{"/", "c", "t", "g", "f", "-", "+", "b", "p", "x"}},
		{"nodes", model{sc: scNodes}, []string{"+", "enter", "d", "b", "p", "a"}},
		{"roles", model{sc: scRoles}, []string{"enter", "n", "x", "T", "u", "t", "p", "f", "c"}},
		{"role settings", roleM, []string{"[", "]", "a", "x", "-", "+", "<", ">", "o", "s", "z", "m", "u"}},
		{"discover targets", disc, []string{"d", "x", "L"}},
		{"discover results", func() model { d := testModel(); d.sc = scDiscover; return d }(), []string{"w", "a", "n", "/", "i", "e"}},
		{"leaderboard", model{sc: scBoard}, []string{"/", "1", "2", "3", "4", "n", "b", "B"}},
		{"usage", model{sc: scUsage}, []string{"/", "g", "p"}},
		{"plan", model{sc: scPlan}, []string{"enter", "c", "e", "p", "r", "f", "w", "a", "L", "t", "b", "i", "d"}},
		{"runs", model{sc: scRuns}, []string{"e", "b"}},
		{"model files", model{sc: scStore}, []string{"n", "f", "c", "a", "x"}},
		{"integrations", model{sc: scInteg}, []string{"enter"}},
	} {
		if c.name == "discover targets" {
			c.m.disc = testModel().disc // the "last results" button needs a scan to show
		}
		have := " " + buttonKeys(c.m) + " "
		for _, k := range c.keys {
			if !strings.Contains(have, " "+k+" ") {
				t.Errorf("%s: the shortcut %q has no button (buttons: %s)", c.name, k, buttonKeys(c.m))
			}
		}
		if !strings.Contains(have, " esc ") && c.name != "discover results" {
			t.Errorf("%s: no way back by button", c.name)
		}
	}
}

func TestNav_HomeToPlanWithArrowsOnly(t *testing.T) {
	m, _ := planModelAt(t)
	m.cur = 0
	var downs []string
	for i, it := range home {
		if it.title == "Plan" {
			for j := 0; j < i; j++ {
				downs = append(downs, "down")
			}
		}
	}
	m = arrows(t, m, append(downs, "enter")...)
	if m.sc != scPlan || len(m.planFiles) != 1 {
		t.Fatalf("screen %v files %v", m.sc, m.planFiles)
	}
	// enter plans the highlighted file; the answer arrives
	m = arrows(t, m, "enter")
	if m.planRes == nil || m.planBusy {
		t.Fatalf("busy %v res %v err %q", m.planBusy, m.planRes, m.planErr)
	}
	// ↓ past the files focuses the sliders; → changes the context and the plan is made again
	before := m.planCtx
	m = arrows(t, m, "down", "right")
	if m.planSection != 1 || m.planCtx != (before+1)%len(planCtxs) {
		t.Fatalf("section %d ctx %d", m.planSection, m.planCtx)
	}
	if m.planRes == nil || m.planRes.Ctx != planCtxs[m.planCtx] {
		t.Fatalf("the plan follows the context: %+v", m.planRes)
	}
	// ↓ to Slots, → changes it
	m = arrows(t, m, "down", "right")
	if m.planNp != 1 {
		t.Fatalf("slots %d", m.planNp)
	}
	// esc returns to the files; ↑ from the top of the list → the bar, on "Plan this file"
	m = arrows(t, m, "esc", "up")
	if !m.inBar || m.buttons()[m.barCur].label != "Plan this file" {
		t.Fatalf("on %q", m.buttons()[m.barCur].label)
	}
	// "Type a path…" opens the input; esc cancels it and leaves the screen intact
	m = arrows(t, m, "right", "right", "right", "enter")
	if m.editing != "planpath" {
		t.Fatalf("editing %q", m.editing)
	}
	m = arrows(t, m, "esc")
	if m.editing != "" || m.sc != scPlan {
		t.Fatalf("editing %q screen %v", m.editing, m.sc)
	}
	// the last button is Back
	m = arrows(t, m, "left") // wraps? no: left from the first button stays; go to the end
	for i := 0; i < len(m.buttons()); i++ {
		m = arrows(t, m, "right")
	}
	if m.buttons()[m.barCur].label != "← Back" {
		t.Fatalf("on %q", m.buttons()[m.barCur].label)
	}
	m = arrows(t, m, "enter")
	if m.sc != scHome {
		t.Fatalf("Back leaves the screen: %v", m.sc)
	}
}

func TestNav_PlanListScrollsAndBarIsReachableWithoutFiles(t *testing.T) {
	m := model{sc: scPlan, sel: map[int]bool{}, w: 100, h: 30}
	for i := 0; i < 20; i++ {
		m.planFiles = append(m.planFiles, "/models/m"+string(rune('a'+i))+".gguf")
	}
	m = arrows(t, m, "down", "down", "down", "down", "down", "down", "down", "down", "down")
	if m.cur != 9 {
		t.Fatalf("cursor %d", m.cur)
	}
	if v := m.viewPlan(); !strings.Contains(v, "mj.gguf") || strings.Contains(v, "ma.gguf") {
		t.Fatalf("the list scrolls with the cursor:\n%s", v)
	}
	// with no files at all, ↓ goes straight to the buttons
	e := model{sc: scPlan, sel: map[int]bool{}}
	e = arrows(t, e, "down")
	if !e.inBar {
		t.Fatal("nothing to move through: the bar")
	}
}

func TestNav_RoleSettingsRequestSizeByButton(t *testing.T) {
	_, cl := newNodesDaemon(t)
	m := roleModel()
	m.cl = cl
	m.roleCur = 0
	m = arrows(t, m, "tab")
	idx := -1
	for i, b := range m.buttons() {
		if b.key == "z" {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatal("no Request size button")
	}
	for i := 0; i < idx; i++ {
		m = arrows(t, m, "right")
	}
	if m.buttons()[m.barCur].label != "Request size" {
		t.Fatalf("on %q", m.buttons()[m.barCur].label)
	}
	m = arrows(t, m, "enter")
	if m.routeSize != 1 {
		t.Fatalf("size %d", m.routeSize)
	}
	m.st = roleModel().st // the fake daemon has no such role: put the fixture's status back
	if v := m.viewRoleEdit(); !strings.Contains(v, requestSizes[1].name) {
		t.Fatalf("the heading names the size:\n%s", v)
	}
}

func TestNav_ResultViewKeepsTheVerdictWhenTheTableIsCut(t *testing.T) {
	m := model{sc: scPlan, sel: map[int]bool{}, w: 100, h: 24, planPath: "/m/x.gguf"}
	r := planResultN(40)
	m.planRes = &r
	v := m.viewPlan()
	if !strings.Contains(v, "run it on") {
		t.Fatalf("the verdict comes before the table:\n%s", v)
	}
	if !strings.Contains(v, "more lines") {
		t.Fatalf("a long table is cut and says so:\n%s", v)
	}
}

func planResultN(n int) plan.Result {
	r := plan.Result{Model: "big", Ctx: 8192, Np: 1, Verdict: "run it on dino alone (GPU): about 20.0 tok/s"}
	for i := 0; i < n; i++ {
		r.Candidates = append(r.Candidates, plan.Candidate{Kind: "split", Master: "dino", TPS: float64(20 - i/4), TokenMS: 50, Parts: []plan.Part{{Node: "dino", Layers: 10}}})
	}
	return r
}

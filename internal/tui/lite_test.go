package tui

import (
	"strings"
	"testing"

	"github.com/tevfik/fleet-router/internal/feature"
)

// Without fleet-engine the menu has no runner screens, the groups keep their headings, and the hint is the router's.
func TestLite_HomeMenuHasNoRunnerScreens(t *testing.T) {
	defer feature.SetRunner(false)()
	items := homeItems()
	if len(items) != len(home)-len(runnerItems) {
		t.Fatalf("lite menu has %d items, want %d", len(items), len(home)-len(runnerItems))
	}
	groups := 0
	for _, it := range items {
		if runnerItems[it.title] {
			t.Errorf("runner screen %q in the lite menu", it.title)
		}
		if it.group != "" {
			groups++
		}
	}
	if items[0].title != "Dashboard" || items[0].group == "" || items[len(items)-1].title != "Quit" {
		t.Errorf("menu ends wrong: first %+v last %+v", items[0], items[len(items)-1])
	}
	if groups != 3 { // Use · Machines & routing · Advanced ("Run a model" was only runner screens)
		t.Errorf("%d group headings, want 3", groups)
	}
	out := plain(model{sc: scHome, h: 60, w: 120, sel: map[int]bool{}}.View().Content)
	for _, s := range []string{"Profiles", "Model files", "RUN A MODEL", "Deploy"} {
		if strings.Contains(out, s) {
			t.Errorf("lite home screen shows %q", s)
		}
	}
	if !strings.Contains(out, "Discover → register your model servers") {
		t.Error("lite home screen lacks the router hint")
	}
	// enter on every item goes where its title says (dispatch is by title, so the shorter list must line up)
	for i := range items {
		m := model{sc: scHome, cur: i, h: 50, w: 120, sel: map[int]bool{}}
		if got := m.listLen(); got != len(items) {
			t.Fatalf("listLen %d, want %d", got, len(items))
		}
	}
}

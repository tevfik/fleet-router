package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

// nk presses a key the way the program does (through key, so the way back is kept).
func nk(m model, s string) model {
	var k tea.KeyPressMsg
	switch s {
	case "esc":
		k = tea.KeyPressMsg{Code: tea.KeyEscape}
	case "enter":
		k = tea.KeyPressMsg{Code: tea.KeyEnter}
	default:
		k = tea.KeyPressMsg{Code: rune(s[0]), Text: s}
	}
	nm, _ := m.key(k)
	return nm.(model)
}

// Nodes → a node's models → esc is Nodes again, not the home menu; the cursor is where it was.
func TestNav_EscFromNodeModelsReturnsToNodes(t *testing.T) {
	_, cl := newNodesDaemon(t)
	m := nodesModel(cl)
	m.cur = 0
	m = nk(m, "enter")
	if m.sc != scDash {
		t.Fatalf("enter should open the models: %v", m.sc)
	}
	m = nk(m, "esc")
	if m.sc != scNodes || m.filter != "" {
		t.Fatalf("esc must return to Nodes: sc=%v filter=%q", m.sc, m.filter)
	}
	m = nk(m, "esc")
	if m.sc != scHome || len(m.nav) != 0 {
		t.Fatalf("a second esc reaches the menu with nothing left on the way: sc=%v nav=%v", m.sc, m.nav)
	}
}

// Every screen reached from the home menu returns to it on esc, whatever it did on the way.
func TestNav_EveryHomeEntryReturnsHome(t *testing.T) {
	_, cl := newNodesDaemon(t)
	for i, h := range home {
		if h.title == "Chat / Ask" {
			continue // opens the agent program
		}
		m := model{cl: cl, sel: map[int]bool{}, w: 120, h: 40, cur: i}
		m = nk(m, "enter")
		if m.sc == scHome {
			continue
		}
		m = nk(m, "esc")
		if m.sc != scHome {
			t.Errorf("%s: esc ends on screen %v, not the menu", h.title, m.sc)
		}
	}
}

// homeIdx is the position of a home menu entry by its title (the order is a layout choice, not a contract).
func homeIdx(title string) int {
	for i, it := range home {
		if it.title == title {
			return i
		}
	}
	panic("no home entry " + title)
}

// Back on the start menu, esc once more puts the cursor on the first entry (the way out of a long menu); q still quits.
func TestNav_EscOnHomeGoesToTheTop(t *testing.T) {
	_, cl := newNodesDaemon(t)
	m := model{cl: cl, sc: scHome, cur: homeIdx("Nodes"), h: 50, w: 120, sel: map[int]bool{}}
	m = nk(m, "enter")
	if m.sc != scNodes {
		t.Fatalf("screen %v", m.sc)
	}
	m = nk(m, "esc")
	if m.sc != scHome || m.cur != homeIdx("Nodes") {
		t.Fatalf("esc returns to the menu on the entry it came from: sc %v cur %d", m.sc, m.cur)
	}
	m = nk(m, "esc")
	if m.sc != scHome || m.cur != 0 {
		t.Fatalf("a second esc goes to the top: sc %v cur %d", m.sc, m.cur)
	}
	m = nk(m, "esc")
	if m.sc != scHome || m.cur != 0 {
		t.Fatalf("esc at the top stays there: sc %v cur %d", m.sc, m.cur)
	}
}

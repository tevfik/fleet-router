package tui

import (
	"testing"
)

func TestAskTypingNoPickerTrigger(t *testing.T) {
	m := model{}.enterAsk("scout")
	if m.sc != scAsk {
		t.Fatalf("expected scAsk, got %v", m.sc)
	}

	// Type "This is a test" character by character
	phrase := "This is a test"
	for _, ch := range phrase {
		s := string(ch)
		nm, _ := m.keyAsk(s, s)
		m = nm.(model)
		if m.pick {
			t.Fatalf("character %q caused picker to open unexpectedly! askInput was: %q", s, m.askInput)
		}
	}

	if m.askInput != phrase {
		t.Fatalf("expected askInput=%q, got %q", phrase, m.askInput)
	}

	// Test Tab opens picker
	nm, _ := m.keyAsk("tab", "")
	m = nm.(model)
	if !m.pick {
		t.Fatalf("expected Tab to open picker, but m.pick is false")
	}

	// Test Esc closes picker
	nm, _ = m.keyAsk("esc", "")
	m = nm.(model)
	if m.pick {
		t.Fatalf("expected Esc to close picker, but m.pick is true")
	}

	// Test launchAgentTUI returns command
	_, cmd := m.launchAgentTUI("scout")
	if cmd == nil {
		t.Fatalf("expected tea.Cmd from launchAgentTUI, got nil")
	}
}

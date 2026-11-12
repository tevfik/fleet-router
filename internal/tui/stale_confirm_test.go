package tui

import "testing"

// A dry-run reply that arrives after the user went to another screen must not arm a confirmation there: the "y"
// the banner asks for would run the other screen's destructive action (delete a role, remove a profile).
func TestLateReplyDoesNotArmConfirmOnAnotherScreen(t *testing.T) {
	m := testModel()
	m.sc = scRoles
	nm, _ := m.Update(stopPreviewMsg{sc: scRuns, act: pendingAct{kind: "stop-run", name: "r1"}})
	if nm.(model).confirm {
		t.Fatal("stopPreviewMsg from another screen armed confirm")
	}
	nm, _ = m.Update(pruneMsg{sc: scRuns, gone: nil})
	if nm.(model).confirm {
		t.Fatal("pruneMsg from another screen armed confirm")
	}
	nm, _ = m.Update(pullAskMsg{sc: scStore, act: pendingAct{kind: "pull"}, line: "x"})
	if nm.(model).confirm {
		t.Fatal("pullAskMsg from another screen armed confirm")
	}
}

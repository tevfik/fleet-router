package chatpick

import "testing"

func TestHistoryIsBoundedAndCountsTrimmedTurns(t *testing.T) {
	var h History
	for i := 0; i < maxStoredTurns+50; i++ {
		h.Add("q", "a")
	}
	if len(h.Msgs) != maxStoredTurns*2 {
		t.Fatalf("%d messages kept", len(h.Msgs))
	}
	if h.Turns() != maxStoredTurns+50 {
		t.Fatalf("turns %d: the user's turn count must include the trimmed ones", h.Turns())
	}
	if _, dropped := h.Window(); dropped < 50 {
		t.Fatalf("dropped %d: trimmed turns are not sent either", dropped)
	}
	h.Clear()
	if h.Turns() != 0 {
		t.Fatal("clear resets")
	}
}

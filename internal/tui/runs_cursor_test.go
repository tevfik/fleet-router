package tui

import (
	"strings"
	"testing"

	"github.com/tevfik/fleet-router/internal/server"
)

// A "stale?" line under a run must not shift the cursor: the marked row is the run s/e/d act on.
func TestRunsCursorSkipsStaleLines(t *testing.T) {
	m := model{sc: scRuns, w: 140, runs: []server.Run{
		{ID: "run-a", Stale: "master gone"},
		{ID: "run-b"},
	}}
	for cur, want := range []string{"run-a", "run-b"} {
		m.cur = cur
		var marked []string
		for _, l := range strings.Split(m.viewRuns(), "\n") {
			if strings.Contains(l, "▸") {
				marked = append(marked, l)
			}
		}
		if len(marked) != 1 || !strings.Contains(marked[0], want) {
			t.Errorf("cur %d: marked rows %q, want one row with %s", cur, marked, want)
		}
	}
}

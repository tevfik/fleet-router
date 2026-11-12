package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/tevfik/fleet-router/internal/board"
	"github.com/tevfik/fleet-router/internal/server"
)

// The Runs screen: the distributed runs every fleet knows about (the same list as `fleet run`), and the one
// highlighted drawn as a path — each machine in the order of the line, what it holds, what comes before and after
// it, what the plan expected and what was measured. `s` stops its processes and closes it, `e` closes the record, `b` binds it to the endpoint that serves it.

func (m model) enterRuns() model {
	m.sc, m.cur, m.note, m.runs, m.runsErr, m.confirm, m.pend = scRuns, 0, "", nil, nil, false, nil
	return m
}

func (m model) viewRuns() string {
	var b strings.Builder
	if m.runsErr != nil { // the list below (if any) is the last one that came through
		b.WriteString(badS.Render(trunc("  the runs could not be fetched: "+m.runsErr.Error(), max(m.w, 110)-2)) + "\n")
	}
	if m.runs == nil {
		if m.runsErr != nil {
			return b.String()
		}
		if m.err != nil {
			return badS.Render("  the daemon does not answer — the runs cannot be listed") + "\n"
		}
		return helpS.Render("  asking the fleets …") + "\n"
	}
	width := m.w
	if width <= 0 {
		width = 110
	}
	rows := board.RunRows(m.runs)
	b.WriteString(helpS.Render(trunc(rows[0], width-2)) + "\n")
	if len(m.runs) == 0 {
		return b.String()
	}
	// RunRows adds "stale?" lines under a run and a summary line after them: the cursor marks run rows only.
	run := 0
	for _, l := range rows[1:] {
		isRun := run < len(m.runs) && !strings.HasPrefix(l, "          ")
		b.WriteString(curMark(isRun && run == m.cur) + trunc(l, width-4) + "\n")
		if isRun {
			run++
		}
	}
	if m.cur < len(m.runs) {
		st := m.st
		b.WriteString("\n")
		for i, l := range board.RunLines(m.runs[m.cur], &st) {
			switch {
			case i == 0:
				l = groupS.Render(l)
			case strings.Contains(l, "✗"), strings.Contains(l, "left the line"), strings.Contains(l, "stalled"), strings.Contains(l, "down:"):
				l = badS.Render(trunc(l, width-2))
			case strings.Contains(l, "slowest"):
				l = accS.Render(trunc(l, width-2))
			default:
				l = helpS.Render(trunc(l, width-2))
			}
			b.WriteString("  " + l + "\n")
		}
	}
	return b.String()
}

func (m model) keyRuns(s string) (tea.Model, tea.Cmd) {
	cl := m.cl
	if nm, cmd, ok := m.confirmPending(s); ok {
		return nm, cmd
	}
	if m.confirm { // pruning: the records that would go were listed first
		switch s {
		case "y", "enter":
			m.confirm = false
			return m, func() tea.Msg {
				gone, err := cl.DeleteRuns("", false)
				if err != nil {
					return noteMsg(badS.Render("error: " + err.Error()))
				}
				return noteMsg(okS.Render("✓ " + runsRemovedText(len(gone), false)))
			}
		case "n":
			m.confirm = false
			m.note, m.outText = "", ""
		}
		return m, nil
	}
	switch s {
	case "P": // fleet run prune (first --dry-run, then the confirmation)
		sc := m.sc
		return m, func() tea.Msg {
			gone, err := cl.DeleteRuns("", true)
			if err != nil {
				return pruneMsg{sc: sc, err: err}
			}
			return pruneMsg{sc: sc, gone: gone}
		}
	case "w": // fleet watchdog --once
		m.note = helpS.Render("watchdog: checking the ring endpoints …")
		return m, func() tea.Msg {
			out, err := watchdogOnce()
			if err != nil {
				return noteMsg(badS.Render("error: " + err.Error()))
			}
			return noteMsg(okS.Render("✓ " + out))
		}
	}
	if m.cur >= len(m.runs) {
		return m, nil
	}
	r := m.runs[m.cur]
	switch s {
	case "s":
		if !r.Active() {
			m.note = helpS.Render("that run has already ended")
			return m, nil
		}
		return m, m.previewStop("stop-run", r.ID)
	case "e":
		if !r.Active() {
			m.note = helpS.Render("that run has already ended")
			return m, nil
		}
		return m, func() tea.Msg {
			if _, err := cl.SetRun(r.ID, nil, true); err != nil {
				return noteMsg(badS.Render("error: " + err.Error()))
			}
			return noteMsg(okS.Render("✓ run " + r.ID + " ended"))
		}
	case "x", "delete":
		if r.Active() && r.Stale != "" {
			// its processes are gone (the daemon says why): close the record without stopping anything, then delete
			// it — what `fleet run end --record-only` + `fleet run rm` do; asks first
			m.pend, m.confirm, m.outText = &pendingAct{kind: "rm-stale-run", name: r.ID}, true, ""
			m.note = badS.Render("run " + r.ID + " looks dead (" + r.Stale + ") — y close its record (nothing is stopped) and delete it, n cancel")
			return m, nil
		}
		if r.Active() {
			m.note = helpS.Render("that run is still active: s stops its processes, e ends it; then x deletes its record")
			return m, nil
		}
		return m.removeRunRow(r.ID, func() error { _, err := cl.DeleteRuns(r.ID, false); return err })
	case "b":
		m.editing, m.editFor, m.editBuf = "runbind", r.ID, r.Endpoint
	}
	return m, nil
}

// removeRunRow takes a run's row off the list at once and runs del in the background; the list fetched next is the
// truth (on an error the row comes back with it). A fetch already out when the row went is not taken.
func (m model) removeRunRow(id string, del func() error) (model, tea.Cmd) {
	for i := range m.runs {
		if m.runs[i].ID == id {
			m.runs = append(append([]server.Run{}, m.runs[:i]...), m.runs[i+1:]...)
			break
		}
	}
	m.runsGen++
	m.cur = min(m.cur, max(len(m.runs)-1, 0))
	return m, func() tea.Msg {
		if err := del(); err != nil {
			return noteMsg(badS.Render("error: " + err.Error()))
		}
		return noteMsg(okS.Render("✓ run record " + id + " deleted (nothing was stopped)"))
	}
}

// finishRunBind sends the endpoint typed for a run.
func (m model) finishRunBind(id, endpoint string) (tea.Model, tea.Cmd) {
	cl := m.cl
	return m, func() tea.Msg {
		if _, err := cl.SetRun(id, &endpoint, false); err != nil {
			return noteMsg(badS.Render("error: " + err.Error()))
		}
		if endpoint == "" {
			return noteMsg(okS.Render(fmt.Sprintf("✓ run %s is no longer bound to an endpoint", id)))
		}
		return noteMsg(okS.Render(fmt.Sprintf("✓ run %s is followed through endpoint %s", id, endpoint)))
	}
}

// pruneMsg is the dry run of `fleet run prune`: the ended records that would go; the user confirms next.
type pruneMsg struct {
	sc   screen
	gone []server.Run
	err  error
}

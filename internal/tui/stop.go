package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/tevfik/fleet-router/internal/board"
	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/server"
)

// Stopping and removing what a model runs on, from the Runs screen (a run) and the model list (an endpoint). Both
// first show a dry run (what would be stopped, where, in which order) and ask; nothing is touched before the y.

// pendingAct is the action waiting for the user's y (m.confirm is set meanwhile).
type pendingAct struct {
	kind  string   // "stop-run" | "stop-ep" | "rm-ep" | "rm-stale-run" | "rm-profile" | "pull"
	name  string   // run id, endpoint name or model name
	force bool     // requests are running; the user was told
	node  string   // pull: the machine that receives
	shas  []string // pull: the files, in turn
}

// pullAskMsg: a big fetch over a line that is not the LAN waits for the user's y.
type pullAskMsg struct {
	sc   screen // the screen that asked: a reply that arrives after the user moved on must not arm a confirmation there
	act  pendingAct
	line string
}

// stopPreviewMsg carries the dry run of a stop.
type stopPreviewMsg struct {
	sc  screen
	act pendingAct
	rep server.StopReport
	err error
}

// stopDoneMsg carries the result of the real stop.
type stopDoneMsg struct {
	rep server.StopReport
	err error
}

func (m model) previewStop(kind, name string) tea.Cmd {
	cl, sc := m.cl, m.sc
	return func() tea.Msg {
		var rep server.StopReport
		var err error
		if kind == "stop-run" {
			rep, err = cl.StopRun(name, server.StopOptions{DryRun: true})
		} else {
			rep, err = cl.StopEndpoint(name, server.StopOptions{DryRun: true})
		}
		return stopPreviewMsg{sc: sc, act: pendingAct{kind: kind, name: name, force: rep.InUse > 0}, rep: rep, err: err}
	}
}

// onStopPreview shows the dry run and waits for the confirmation.
func (m model) onStopPreview(v stopPreviewMsg) model {
	switch {
	case v.err != nil:
		m.note = badS.Render("error: " + v.err.Error())
	case v.rep.Refused != "":
		m.note, m.outText = badS.Render("✗ "+v.rep.Refused), ""
	default:
		m.outText = strings.Join(board.StopLines(v.rep), "\n")
		act := v.act
		m.pend, m.confirm = &act, true
		q := "y stop them (master first, then workers), n cancel"
		if v.rep.InUse > 0 {
			q = fmt.Sprintf("%d request(s) are running and would be cut — y stop anyway, n cancel", v.rep.InUse)
		}
		m.note = badS.Render(q)
	}
	return m
}

// askRemoveEndpoint asks before an endpoint is taken out of the configuration.
func (m model) askRemoveEndpoint(name string) model {
	m.pend, m.confirm = &pendingAct{kind: "rm-ep", name: name}, true
	m.outText = ""
	m.note = badS.Render("remove " + name + " from the configuration? its process keeps running; roles that name it are mended (the preferred one becomes the first fallback; Undo on the Roles screen restores) — y remove, n cancel")
	return m
}

// confirmPending handles y/n while a stop or removal waits; handled says the key was for it.
func (m model) confirmPending(s string) (_ model, cmd tea.Cmd, handled bool) {
	if !m.confirm || m.pend == nil {
		return m, nil, false
	}
	act := *m.pend
	switch s {
	case "y", "enter":
		m.confirm, m.pend, m.outText = false, nil, ""
		cl := m.cl
		m.note = helpS.Render("working …")
		switch act.kind {
		case "pull":
			return m, func() tea.Msg {
				if err := cl.PullAll(act.node, act.shas); err != nil {
					return noteMsg(badS.Render("error: " + err.Error() + " (the same fetch again resumes)"))
				}
				return noteMsg(okS.Render("✓ " + act.node + " has " + act.name))
			}, true
		case "rm-stale-run":
			id := act.name
			nm, cmd := m.removeRunRow(id, func() error {
				if _, err := cl.EndRunRecordOnly(id); err != nil {
					return err
				}
				_, err := cl.DeleteRuns(id, false)
				return err
			})
			return nm, cmd, true
		case "rm-node":
			node := act.name
			return m, func() tea.Msg {
				cfg, err := cl.Config()
				if err != nil {
					return noteMsg(badS.Render("error: " + err.Error()))
				}
				gone := map[string]bool{}
				for _, e := range nodeSSHEntries(cfg.Discover.SSH, node) {
					gone[e] = true
				}
				var keep []string
				for _, e := range cfg.Discover.SSH {
					if !gone[e] {
						keep = append(keep, e)
					}
				}
				if len(gone) > 0 {
					if err := cl.SetDiscover(cfg.Discover.Ports, keep, cfg.Discover.Ignore); err != nil {
						return noteMsg(badS.Render("error: " + err.Error()))
					}
				}
				if _, ok := cfg.Nodes[node]; ok {
					if err := cl.SetNodeSpec(node, config.NodeSpec{}, true); err != nil {
						return noteMsg(badS.Render("error: " + err.Error()))
					}
				}
				return noteMsg(okS.Render("✓ node " + node + " removed (it leaves the list on the next refresh)"))
			}, true
		case "rm-ep":
			return m, func() tea.Msg {
				notes, err := cl.EndpointsNotes(nil, []string{act.name}, true)
				if err != nil {
					return noteMsg(badS.Render("error: " + err.Error()))
				}
				return noteMsg(okS.Render("✓ removed: " + act.name + strings.Join(append([]string{""}, notes...), " · ")))
			}, true
		default:
			return m, func() tea.Msg {
				var rep server.StopReport
				var err error
				opt := server.StopOptions{Force: act.force}
				if act.kind == "stop-run" {
					rep, err = cl.StopRun(act.name, opt)
				} else {
					rep, err = cl.StopEndpoint(act.name, opt)
				}
				return stopDoneMsg{rep: rep, err: err}
			}, true
		}
	case "n", "esc":
		m.confirm, m.pend, m.note, m.outText = false, nil, "", ""
		return m, nil, true
	}
	return m, nil, true
}

// onStopDone shows what the stop did.
func (m model) onStopDone(v stopDoneMsg) model {
	switch {
	case v.err != nil:
		m.note = badS.Render("error: " + v.err.Error())
	case v.rep.Failed():
		m.note, m.outText = badS.Render("✗ not everything stopped (see below)"), strings.Join(board.StopLines(v.rep), "\n")
	default:
		m.note, m.outText = okS.Render("✓ stopped "+v.rep.Endpoint), strings.Join(board.StopLines(v.rep), "\n")
	}
	return m
}

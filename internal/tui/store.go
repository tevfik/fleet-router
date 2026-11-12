package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/tevfik/fleet-router/internal/board"
	"github.com/tevfik/fleet-router/internal/server"
)

// The Model files screen: which GGUF files each machine has (the same table as `fleet store list`), the worker
// caches, and the hashing that is running. `p` adds a file or a directory on the daemon's machine, `x` forgets
// the highlighted one (the file itself is never touched).

func (m model) enterStore() model {
	m.sc, m.cur, m.note, m.storeNodes, m.pick = scStore, 0, "", nil, false
	return m
}

func (m model) viewStore() string {
	var b strings.Builder
	if m.pick {
		what := "Machine to fetch the file to"
		if m.pickMode == "storefill" {
			what = "Machine whose worker cache to fill"
		}
		b.WriteString(groupS.Render(what+" (↑/↓, enter)") + "\n")
		for i, n := range m.pickNodes {
			fmt.Fprintf(&b, "%s%s\n", curMark(i == m.cur), n)
		}
		return b.String()
	}
	if m.storeNodes == nil {
		if m.err != nil {
			return badS.Render("  the daemon does not answer — the machines cannot be asked") + "\n"
		}
		return helpS.Render("  asking the machines …") + "\n"
	}
	rows := storeModels(m.storeNodes)
	lines := board.StoreLines(m.storeNodes)
	width := m.w
	if width <= 0 {
		width = 110
	}
	b.WriteString(helpS.Render(trunc(lines[0], width-2)) + "\n")
	nr := max(len(rows), 1)
	for i := 0; i < nr && 1+i < len(lines); i++ {
		l := strings.TrimPrefix(lines[1+i], "  ")
		if len(rows) == 0 {
			b.WriteString(descS.Render("  "+l) + "\n")
			continue
		}
		b.WriteString(curMark(i == m.cur) + trunc(l, width-4) + "\n")
	}
	for _, l := range lines[min(1+nr, len(lines)):] {
		switch {
		case strings.Contains(l, "failed"), strings.Contains(l, "too old"), strings.Contains(l, "no route"):
			l = badS.Render(trunc(l, width-2))
		case strings.Contains(l, "hashing"), strings.Contains(l, " from ") && strings.Contains(l, "%"):
			l = accS.Render(trunc(l, width-2))
		default:
			l = helpS.Render(trunc(l, width-2))
		}
		b.WriteString(l + "\n")
	}
	if len(rows) > 0 && m.cur < len(rows) {
		r := rows[m.cur]
		var has, lacks []string
		for _, n := range m.storeNodes {
			if hasAll(r, n.Node) {
				has = append(has, n.Node)
			} else {
				lacks = append(lacks, n.Node)
			}
		}
		id := "sha256 " + r.Shards[0].SHA256
		if r.Total > 1 {
			id = fmt.Sprintf("%d files (shards); a fetch brings all of them", r.Total)
		}
		b.WriteString("\n" + groupS.Render(r.Name) + helpS.Render("  "+id) + "\n")
		fmt.Fprintf(&b, "  on %s", accS.Render(orNone(has)))
		if len(lacks) > 0 {
			b.WriteString(helpS.Render("  ·  not on " + strings.Join(lacks, ", ")))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// storeModels is the list the screen shows: a split model is one row.
func storeModels(nodes []server.NodeStore) []board.ModelRow {
	return board.StoreModels(board.StoreRows(nodes))
}

// hasAll: the machine holds every file of the model.
func hasAll(r board.ModelRow, node string) bool {
	return r.Count[node] == len(r.Shards) && len(r.Shards) >= max(r.Total, 1)
}

// storeHolders: every machine whose store lists a file of the row, complete or not (a record whose file is gone or changed
// still sits in that machine's store and is what has to be forgotten there).
func storeHolders(nodes []server.NodeStore, r board.ModelRow) []string {
	want := map[string]bool{}
	for _, sh := range r.Shards {
		want[sh.SHA256] = true
	}
	var out []string
	for _, n := range nodes {
		if n.Store == nil {
			continue
		}
		for _, f := range n.Store.Files {
			if want[f.SHA256] {
				out = append(out, n.Node)
				break
			}
		}
	}
	return out
}

func shaList(r board.ModelRow) []string {
	var out []string
	for _, s := range r.Shards {
		out = append(out, s.SHA256)
	}
	return out
}

func orNone(l []string) string {
	if len(l) == 0 {
		return "no machine"
	}
	return strings.Join(l, ", ")
}

func (m model) keyStore(s string) (tea.Model, tea.Cmd) {
	if nm, cmd, ok := m.confirmPending(s); ok {
		return nm, cmd
	}
	rows := storeModels(m.storeNodes)
	if m.pick { // choosing the machine
		if s == "esc" || s == "q" {
			m.pick, m.cur = false, m.storeCur
			return m, nil
		}
		if s == "enter" && m.cur < len(m.pickNodes) && m.storeCur < len(rows) {
			row := rows[m.storeCur]
			target, cl, mode := m.pickNodes[m.cur], m.cl, m.pickMode
			m.pick, m.cur = false, m.storeCur
			if row.Total > 1 && len(row.Shards) < row.Total {
				m.note = badS.Render(fmt.Sprintf("%s is split in %d files but only %d are registered: register the rest (fleet store add) first", row.Name, row.Total, len(row.Shards)))
				return m, nil
			}
			if mode == "storefill" {
				m.editing, m.editFor, m.editBuf = "storefill", target+"|"+strings.Join(shaList(row), ","), ""
				return m, nil
			}
			var todo []string
			var total int64
			for _, sh := range row.Shards {
				if !sh.Has[target] {
					todo = append(todo, sh.SHA256)
					total += sh.Size
				}
			}
			if len(todo) == 0 { // the target received the last shard while the picker was open
				m.note = helpS.Render(target + " already has " + row.Name)
				return m, nil
			}
			m.note = helpS.Render("asking " + target + " to fetch " + row.Name + " …")
			sc := m.sc
			return m, func() tea.Msg {
				// a big file over a line that is not the LAN is asked about first
				if li, err := cl.StoreLink(target, "", todo[0]); err == nil {
					if line := board.LineNote(li, total); line != "" {
						return pullAskMsg{sc: sc, act: pendingAct{kind: "pull", name: row.Name, node: target, shas: todo}, line: line}
					}
				}
				if err := cl.PullAll(target, todo); err != nil {
					return noteMsg(badS.Render("error: " + err.Error() + " (the same fetch again resumes)"))
				}
				return noteMsg(okS.Render("✓ " + target + " has " + row.Name))
			}
		}
		return m, nil
	}
	switch s {
	case "f", "c":
		if m.cur >= len(rows) {
			return m, nil
		}
		var nodes []string
		for _, n := range m.storeNodes {
			if n.Store == nil {
				continue
			}
			if s == "c" || !hasAll(rows[m.cur], n.Node) {
				nodes = append(nodes, n.Node)
			}
		}
		if len(nodes) == 0 {
			m.note = helpS.Render("every machine has this file already")
			return m, nil
		}
		m.pick, m.pickMode, m.pickNodes, m.storeCur, m.cur = true, map[string]string{"f": "storepull", "c": "storefill"}[s], nodes, m.cur, 0
	case "n":
		m.editing, m.editBuf = "storeadd", ""
	case "a":
		cl := m.cl
		var nodes []string
		for _, n := range m.storeNodes {
			if n.Store != nil {
				for _, t := range n.Store.Transfers {
					if t.Running {
						nodes = append(nodes, n.Node)
						break
					}
				}
			}
		}
		if len(nodes) == 0 {
			nodes = []string{""}
		}
		return m, func() tea.Msg {
			var errs []string
			var aborted []string
			for _, n := range nodes {
				if err := cl.StoreAbort(n); err != nil {
					errs = append(errs, fmt.Sprintf("%s: %v", n, err))
				} else {
					target := n
					if target == "" {
						target = "this machine"
					}
					aborted = append(aborted, target)
				}
			}
			if len(errs) > 0 {
				return noteMsg(badS.Render("error: " + strings.Join(errs, "; ")))
			}
			return noteMsg(okS.Render(fmt.Sprintf("✓ transfer aborted on %s — the file on disk is preserved", strings.Join(aborted, ", "))))
		}
	case "x":
		if m.cur >= len(rows) {
			return m, nil
		}
		r, cl := rows[m.cur], m.cl
		holders := storeHolders(m.storeNodes, r)
		return m, func() tea.Msg {
			var done, errs []string
			for _, node := range holders {
				n := 0
				for _, sh := range r.Shards {
					if _, err := cl.StoreRemoveOn(node, sh.SHA256); err != nil {
						if !strings.Contains(err.Error(), "no file in the store matches") {
							errs = append(errs, node+": "+err.Error())
						}
						continue // that shard lives on another machine
					}
					n++
				}
				if n > 0 {
					done = append(done, node)
				}
			}
			if len(errs) > 0 {
				return noteMsg(badS.Render("error: " + strings.Join(errs, "; ")))
			}
			if len(done) == 0 {
				return noteMsg(badS.Render("nothing to forget: no reachable machine holds " + r.Name))
			}
			return noteMsg(okS.Render("✓ forgot " + r.Name + " on " + strings.Join(done, ", ") + " — the files themselves are untouched"))
		}
	}
	return m, nil
}

func fetchStore(m model) []server.NodeStore {
	n, _ := m.cl.StoreAll()
	return n
}

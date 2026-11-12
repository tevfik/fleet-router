package tui

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/profile"
	"github.com/tevfik/fleet-router/internal/server"
)

func rows(s string, w int) int {
	n := 0
	for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		n += max(1, (len([]rune(stripANSI(l)))+w-1)/w)
	}
	return n
}

func stripANSI(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		switch {
		case r == 0x1b:
			in = true
		case in && r == 'm':
			in = false
		case !in:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Every screen shows the result note and the text prompt inside the terminal, also when the screen is long.
func TestFit_NoteAndPromptStayVisibleOnEveryScreen(t *testing.T) {
	t.Setenv("FLEET_CONFIG", filepath.Join(t.TempDir(), "fleet.yaml"))
	_, cl := newRecorder(t)
	screens := []screen{scDash, scRoles, scDiscover, scBoard, scActivity, scUsage, scNodes, scPlan, scStore, scRuns, scEconomy, scHub, scProfiles, scInteg, scRoleEdit, scCaps}
	for _, h := range []int{24, 40} {
		for _, sc := range screens {
			var eps []server.EpStatus
			for i := 0; i < 60; i++ {
				eps = append(eps, server.EpStatus{Name: fmt.Sprintf("model-%d", i)})
			}
			m := model{cl: cl, sc: sc, w: 100, h: h, cur: 50, st: server.Status{Endpoints: eps}, hubShowModels: true,
				note: "NOTE-MARKER"}
			out := m.View().Content
			if rows(out, 100) > h {
				t.Errorf("screen %d h=%d: %d rows exceed the terminal", sc, h, rows(out, 100))
			}
			if !strings.Contains(out, "NOTE-MARKER") {
				t.Errorf("screen %d h=%d: note not visible", sc, h)
			}
			m.note, m.editing, m.editBuf = "", "tags", "PROMPT-BUF"
			if out = m.View().Content; !strings.Contains(out, "PROMPT-BUF") {
				t.Errorf("screen %d h=%d: prompt not visible", sc, h)
			}
		}
	}
}

// The daemon's configuration (hub token/API key, economy, nodes) is applied on the hub, economy and nodes screens, and
// the profile list follows the store on the profiles screen.
func TestStale_ConfigAndProfilesRefresh(t *testing.T) {
	t.Setenv("FLEET_CONFIG", filepath.Join(t.TempDir(), "fleet.yaml"))
	t.Setenv("HOME", t.TempDir())
	for _, sc := range []screen{scHub, scEconomy, scNodes} {
		m := model{sc: sc, cfg: config.Config{HubAPIKey: "old"}}
		nc := config.Config{HubAPIKey: "new", HubToken: "tok"}
		nm, _ := m.Update(statusMsg{cfg: &nc})
		if got := nm.(model).cfg; got.HubAPIKey != "new" || got.HubToken != "tok" {
			t.Errorf("screen %d: stale cfg %+v", sc, got)
		}
	}
	m := model{sc: scProfiles}
	m = m.enterProfiles()
	before := len(m.profList)
	p := &profile.Profile{Name: "fresh", Model: "/m/f.gguf"}
	if err := tuiProfiles().Save(p); err != nil {
		t.Fatal(err)
	}
	nm, _ := m.Update(statusMsg{})
	if got := len(nm.(model).profList); got != before+1 {
		t.Errorf("profile list stale: %d -> %d", before, got)
	}
}

// Keys that only duplicated a list row (economy t/s/q) or were undocumented aliases (runs x, nodes P) are gone.
func TestKeys_NoUndocumentedAliases(t *testing.T) {
	m := model{sc: scEconomy}
	for _, k := range []string{"t", "s", "q"} {
		if _, cmd := m.keyEconomy(k); cmd != nil {
			t.Errorf("economy %q still acts", k)
		}
	}
	for sc, keys := range map[screen][]string{scDash: {"g", "f"}, scRuns: {"x"}, scPlan: {"c", "e", "f", "w", "t"}} {
		h := model{sc: sc}.help()
		for _, k := range keys {
			if !strings.Contains(h, " "+k+" ") {
				t.Errorf("screen %d help does not mention %q: %s", sc, k, h)
			}
		}
	}
}

func TestVersionLine_DaemonMismatch(t *testing.T) {
	st := func(v string) server.Status { return server.Status{Listen: "127.0.0.1:4711", Version: v} }
	if l := (model{st: st(server.Version)}).versionLine(); strings.Contains(l, "restart") {
		t.Errorf("same version warned: %s", l)
	}
	if l := (model{st: st("old-1")}).versionLine(); !strings.Contains(l, "old-1") || !strings.Contains(l, "restart") {
		t.Errorf("mismatch not flagged: %s", l)
	}
	if l := (model{st: st("")}).versionLine(); !strings.Contains(l, "older") {
		t.Errorf("a daemon reporting no version is older: %s", l)
	}
	if l := (model{st: st("old-1"), embedded: true}).versionLine(); strings.Contains(l, "restart") {
		t.Errorf("embedded daemon warned: %s", l)
	}
	if l := (model{}).versionLine(); strings.Contains(l, "restart") {
		t.Errorf("no status yet must not warn: %s", l)
	}
}

// A daemon that does not answer is said so on the screens that wait for its data (not "asking …" forever).
func TestEmpty_DaemonDownIsSaid(t *testing.T) {
	m := model{sc: scRuns, err: fmt.Errorf("refused")}
	if v := m.viewRuns(); !strings.Contains(v, "does not answer") {
		t.Errorf("runs: %s", v)
	}
	m.sc = scStore
	if v := m.viewStore(); !strings.Contains(v, "does not answer") {
		t.Errorf("store: %s", v)
	}
}

// termRows simulates a w-column terminal: escape sequences take no room, a character that does not fit at the
// end of a row starts the next one (a double-width character leaves a gap), combining marks take no room.
func termRows(s string, w int) int {
	n := 0
	for _, l := range strings.Split(s, "\n") {
		rowsN, col := 1, 0
		for _, r := range ansi.Strip(l) {
			cw := ansi.StringWidth(string(r))
			if cw == 0 {
				continue
			}
			if col+cw > w {
				rowsN++
				col = 0
			}
			col += cw
		}
		n += rowsN
	}
	return n
}

func TestFit_WrappedRowsFollowTheTerminal(t *testing.T) {
	red := "\x1b[31m"
	reset := "\x1b[0m"
	for _, c := range []struct {
		name, line string
		w, want    int
	}{
		{"ascii exact", strings.Repeat("a", 10), 10, 1},
		{"ascii over", strings.Repeat("a", 11), 10, 2},
		{"empty", "", 10, 1},
		{"CJK fits", strings.Repeat("字", 5), 10, 1},
		{"CJK leaves a gap at the row end", strings.Repeat("a", 9) + "字", 10, 2},
		{"CJK 3 rows", strings.Repeat("字", 11), 10, 3},
		{"CJK gap on every row (width/w says 2)", strings.Repeat("字", 5), 5, 3},
		{"ANSI takes no room", red + strings.Repeat("a", 10) + reset, 10, 1},
		{"ANSI inside CJK", red + strings.Repeat("字", 5) + reset + red + "b" + reset, 10, 2},
		{"emoji width 2", strings.Repeat("🚀", 5), 10, 1},
		{"emoji gap", "a" + strings.Repeat("🚀", 5), 10, 2},
		{"combining marks", strings.Repeat("é", 10), 10, 1},
	} {
		if got := wrappedRows(c.line, c.w); got != c.want {
			t.Errorf("%s: %d rows, want %d", c.name, got, c.want)
		}
		if got := termRows(c.line, c.w); got != c.want {
			t.Errorf("%s: simulator says %d rows, want %d (test is wrong)", c.name, got, c.want)
		}
	}
}

// A long list of CJK / emoji / ANSI rows in a 24-row terminal: the window keeps the selected row, the whole
// view fits the terminal as a real terminal wraps it.
func TestFit_WindowKeepsTheSelectedRowWithWideAndStyledLines(t *testing.T) {
	_, cl := newRecorder(t)
	for _, w := range []int{40, 60, 100} {
		for _, sel := range []int{0, 7, 25, 49} {
			var b strings.Builder
			b.WriteString("Title\n\n")
			for i := 0; i < 50; i++ {
				mark := "  "
				if i == sel {
					mark = accS.Render("▸ ")
				}
				switch i % 3 {
				case 0:
					fmt.Fprintf(&b, "%s%s\n", mark, strings.Repeat("日本語のモデル", 7)+fmt.Sprint(i))
				case 1:
					fmt.Fprintf(&b, "%s%s %d\n", mark, strings.Repeat("🚀model🦙", 9), i)
				default:
					fmt.Fprintf(&b, "%s%s\n", mark, helpS.Render(fmt.Sprintf("row %d %s", i, strings.Repeat("é́x", 20))))
				}
			}
			m := model{cl: cl, sc: scDash, w: w, h: 24, note: "NOTE-OK"}
			footer := "\n" + helpS.Render("  ↑/↓ select • esc back") + "\n  fleet v"
			out := m.fitBody(b.String(), footer) + footer
			marker := fmt.Sprintf("%d", sel)
			found := false
			for _, l := range strings.Split(out, "\n") {
				if strings.Contains(l, "▸") && strings.Contains(ansi.Strip(l), marker) {
					found = true
				}
			}
			if !found {
				t.Errorf("w=%d sel=%d: the selected row left the window\n%s", w, sel, ansi.Strip(out))
			}
			if got := termRows(out, w); got > 24 {
				t.Errorf("w=%d sel=%d: %d terminal rows exceed 24", w, sel, got)
			}
			if !strings.Contains(out, "NOTE-OK") {
				t.Errorf("w=%d sel=%d: note lost", w, sel)
			}
		}
	}
}

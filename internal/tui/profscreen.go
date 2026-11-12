package tui

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/tevfik/fleet-router/internal/profile"
	"github.com/tevfik/fleet-router/internal/server"
	"github.com/tevfik/fleet-router/internal/setup"
)

// The Profiles screen (Home → Profiles): the saved deploy profiles as a list, the highlighted one in full below,
// and one button per `fleet profile` / `fleet deploy --profile` action. Heavy work (a deploy, a benchmark, an edit
// with the CLI's validation) runs the same fleet binary the CLI would, so the two can never differ.

// outMsg is the multi-line result of an action: text shows under the screen, note on the note line.
type outMsg struct{ note, text string }

// runFleet runs this binary with args and returns its combined output; tests replace it.
var runFleet = func(args ...string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	out, err := exec.Command(exe, args...).CombinedOutput()
	return string(out), err
}

// fleetOut runs `fleet args…` in the background; the output shows under the screen.
func fleetOut(what string, args ...string) tea.Cmd {
	return func() tea.Msg {
		out, err := runFleet(args...)
		out = strings.TrimSpace(out)
		if err != nil {
			last := out
			if i := strings.LastIndex(out, "\n"); i >= 0 {
				last = out[i+1:]
			}
			return outMsg{note: badS.Render("error: " + what + ": " + err.Error() + ": " + last), text: out}
		}
		return outMsg{note: okS.Render("✓ " + what), text: out}
	}
}

func (m model) enterProfiles() model {
	m.sc, m.cur, m.note, m.outText, m.confirm, m.pend = scProfiles, 0, "", "", false, nil
	list, bad, err := tuiProfiles().List()
	if err != nil {
		m.note = badS.Render("error: " + err.Error())
	}
	m.profList = list
	for n, e := range bad {
		m.note = badS.Render(fmt.Sprintf("profile %s is broken: %v", n, e))
	}
	return m
}

// reloadProfiles keeps the list in step with the store after an action, the highlighted profile where it was.
func (m model) reloadProfiles(keep string) model {
	list, _, err := tuiProfiles().List()
	if err != nil {
		return m
	}
	m.profList = list
	m.cur = min(m.cur, max(len(list)-1, 0))
	for i, p := range list {
		if p.Name == keep {
			m.cur = i
		}
	}
	return m
}

func (m model) profileButtons() []button {
	back := button{"← Back", "esc"}
	if m.confirm {
		return []button{{"Yes, delete", "y"}, {"No", "n"}}
	}
	return []button{{"Deploy", "enter"}, {"Down (stop)", "s"}, {"Autostart", "a"}, {"Dry run", "n"}, {"Bench", "b"}, {"Measure…", "m"}, {"Edit…", "e"}, {"YAML", "y"},
		{"Save copy…", "c"}, {"Export…", "o"}, {"Import…", "i"}, {"Delete", "x"}, back}
}

func (m model) profileActiveRun(p *profile.Profile) *server.Run {
	for _, r := range p.Runs {
		for i := range m.runs {
			if m.runs[i].ID == r.RunID && m.runs[i].Active() {
				return &m.runs[i]
			}
		}
	}
	return nil
}

func (m model) keyProfilesScreen(s string) (tea.Model, tea.Cmd) {
	if m.confirm {
		switch s {
		case "y", "enter":
			// the profile x asked about, not the highlighted one: ↑/↓ still move the cursor while the question waits
			act := m.pend
			m.confirm, m.pend = false, nil
			if act != nil && act.kind == "rm-profile" {
				name := act.name
				if err := removeProfile(name); err != nil {
					m.note = badS.Render("error: " + err.Error())
					m = m.reloadProfiles("")
					return m, nil
				}
				keep := ""
				if m.cur < len(m.profList) && m.profList[m.cur].Name != name {
					keep = m.profList[m.cur].Name
				}
				m = m.reloadProfiles(keep)
				m.note = okS.Render("✓ profile " + name + " deleted")
			}
		case "n":
			m.confirm, m.pend, m.note = false, nil, ""
		}
		return m, nil
	}
	if s == "i" { // import needs no profile
		m.editing, m.editBuf, m.editFor = "profimport", "", ""
		return m, nil
	}
	if m.cur >= len(m.profList) {
		if len(m.profList) == 0 && s != "esc" {
			m.note = helpS.Render("no profiles yet — press a on the Plan screen to save the chosen candidate as one, i imports a file")
		}
		return m, nil
	}
	p := m.profList[m.cur]
	name := p.Name
	switch s {
	case "enter":
		m.note = helpS.Render("deploying profile " + name + " (fleet deploy --profile " + name + ") …")
		return m, fleetOut("profile "+name+" deployed", "deploy", "--profile", name)
	case "s":
		m.note = helpS.Render("stopping profile " + name + " (fleet profile down " + name + ") …")
		return m, fleetOut("profile "+name+" stopped", "profile", "down", name)
	case "a":
		target := "on"
		if _, err := os.Stat(setup.ProfileUnitPath(name)); err == nil {
			target = "off"
		}
		m.note = helpS.Render(fmt.Sprintf("profile %s autostart %s …", name, target))
		return m, fleetOut("profile "+name+" autostart "+target, "profile", "autostart", name, target)
	case "A":
		m.note = helpS.Render("deploying profile " + name + " with the planner's layout (--layout auto) …")
		return m, fleetOut("profile "+name+" deployed (auto layout)", "deploy", "--profile", name, "--layout", "auto")
	case "n":
		return m, fleetOut("dry run of profile "+name+": nothing was started", "deploy", "--profile", name, "--dry-run")
	case "b":
		m.note = helpS.Render("benchmarking the last deployed run of " + name + " (fleet profile bench) — a minute or more …")
		return m, fleetOut("profile "+name+" benchmarked", "profile", "bench", name)
	case "m":
		m.editing, m.editFor, m.editBuf = "profmeasure", name, ""
	case "e":
		m.editing, m.editFor, m.editBuf = "profedit", name, ""
	case "y":
		b, err := p.Marshal()
		if err != nil {
			m.note = badS.Render("error: " + err.Error())
			return m, nil
		}
		m.outText = strings.TrimRight(string(b), "\n")
	case "c":
		m.editing, m.editFor, m.editBuf = "profcopy", name, name+"-copy"
	case "o":
		m.editing, m.editFor, m.editBuf = "profexport", name, name+".yaml"
	case "x", "delete":
		m.confirm, m.pend = true, &pendingAct{kind: "rm-profile", name: name}
		q := "delete profile " + name + "? y / n"
		if ar := m.profileActiveRun(p); ar != nil {
			q = "profile " + name + " is running (run " + ar.ID + "): deleting it does not stop it (s does) — delete anyway? y / n"
		}
		m.note = badS.Render(q)
	}
	return m, nil
}

// finishProfileEdit completes the text input of the Profiles screen; ok is false for other editors.
func (m model) finishProfileEdit(kind, target, buf string) (model, tea.Cmd, bool) {
	store := tuiProfiles()
	fail := func(err error) (model, tea.Cmd, bool) {
		m.note = badS.Render("error: " + err.Error())
		return m, nil, true
	}
	switch kind {
	case "profmeasure":
		tps, err := strconv.ParseFloat(buf, 64)
		if err != nil || tps <= 0 {
			return fail(fmt.Errorf("measured tokens/s: a number above 0"))
		}
		return m, fleetOut(fmt.Sprintf("profile %s: %.1f t/s recorded", target, tps), "profile", "measure", target, "--tps", buf), true
	case "profedit":
		if !strings.Contains(buf, "=") {
			return fail(fmt.Errorf("key=value, e.g. layout.ts=34,14 or spec.type=none (empty value removes the key)"))
		}
		return m, fleetOut("profile "+target+" edited: "+buf, "profile", "edit", target, "--set", buf), true
	case "profcopy":
		if buf == "" {
			return m, nil, true
		}
		src, err := store.Load(target)
		if err == nil {
			err = profile.ValidName(buf)
		}
		if err == nil {
			if _, e := store.Load(buf); e == nil {
				err = fmt.Errorf("profile %q exists: delete it first or pick another name", buf)
			}
		}
		if err != nil {
			return fail(err)
		}
		cp := *src
		cp.Name, cp.Runs = buf, nil // a copy has not run yet
		if err := store.Save(&cp); err != nil {
			return fail(err)
		}
		m = m.reloadProfiles(buf)
		m.note = okS.Render("✓ profile " + target + " saved as " + buf)
		return m, nil, true
	case "profexport":
		if buf == "" {
			return m, nil, true
		}
		p, err := store.Load(target)
		var b []byte
		if err == nil {
			b, err = p.Marshal()
		}
		if err == nil {
			err = os.WriteFile(expandHome(buf), b, 0o600)
		}
		if err != nil {
			return fail(err)
		}
		m.note = okS.Render("✓ profile " + target + " written to " + buf)
		return m, nil, true
	case "profimport":
		f := strings.Fields(buf)
		if len(f) == 0 {
			return m, nil, true
		}
		force := len(f) > 1 && f[len(f)-1] == "force"
		if force {
			f = f[:len(f)-1]
		}
		p, err := store.Import(expandHome(strings.Join(f, " ")), "", force)
		if err != nil {
			if strings.Contains(err.Error(), "exists") {
				err = fmt.Errorf("%v — type the path followed by the word force to replace it", err)
			}
			return fail(err)
		}
		m = m.reloadProfiles(p.Name)
		m.note = okS.Render("✓ profile " + p.Name + " imported")
		return m, nil, true
	}
	return m, nil, false
}

func (m model) viewProfilesScreen() string {
	var b strings.Builder
	width := m.w
	if width <= 0 {
		width = 110
	}
	if len(m.profList) == 0 {
		b.WriteString(descS.Render("  no profiles yet — press a on the Plan screen to save the chosen candidate as one; i imports a file; fleet profile save … does it from the shell") + "\n")
		return b.String()
	}
	b.WriteString(helpS.Render(fmt.Sprintf("    %-24s %-9s %-7s %-6s %-12s %-16s %-5s %s", "PROFILE", "STATUS", "ENGINE", "LAYOUT", "MASTER", "WORKERS", "RUNS", "MEASURED")) + "\n")
	for i, p := range m.profList {
		statusStr := "stopped"
		if ar := m.profileActiveRun(p); ar != nil {
			statusStr = "running"
		}
		eng := p.Engine
		if eng == "" {
			eng = "ring"
		}
		meas := "not measured"
		if r := p.LastMeasured(); r != nil {
			meas = fmt.Sprintf("%.1f t/s (%s)", r.Measured.TPS, r.Measured.At.Format("2006-01-02"))
		}
		master := p.Master
		if master == "" {
			master = "(this machine)"
		}
		ws := strings.Join(p.Workers, ",")
		if ws == "" {
			ws = "-"
		}
		b.WriteString(curMark(i == m.cur) + trunc(fmt.Sprintf("%-24s %-9s %-7s %-6s %-12s %-16s %-5d %s", p.Name, statusStr, eng, p.EffectiveMode(), trunc(master, 12), trunc(ws, 16), len(p.Runs), meas), width-4) + "\n")
	}
	if m.cur < len(m.profList) {
		p := m.profList[m.cur]
		b.WriteString("\n" + groupS.Render(p.Name) + "\n")
		line := func(l string) { b.WriteString(helpS.Render(trunc("  "+l, width-2)) + "\n") }
		if ar := m.profileActiveRun(p); ar != nil {
			line("status  RUNNING (endpoint: " + ar.Endpoint + ", run: " + ar.ID + ") — d stops")
		} else {
			line("status  STOPPED — enter deploys")
		}
		line("model   " + p.Model)
		eng := p.Engine
		if eng == "" {
			eng = "ring"
		}
		autoStr := "off"
		if _, err := os.Stat(setup.ProfileUnitPath(p.Name)); err == nil {
			autoStr = "on"
		}
		line(fmt.Sprintf("engine  %s  autostart %s (a toggles)", eng, autoStr))
		if m.linger != "" && m.linger != "yes" { // read in fetch(): View must not run loginctl
			line("linger  warning: Linger=" + m.linger + ": autostart units start only after you log in — run `loginctl enable-linger` once")
		}
		if p.Notes != "" {
			line("notes   " + p.Notes)
		}
		forceNote := ""
		if p.Force {
			forceNote = "  force true"
		}
		line(fmt.Sprintf("layout  %s  ts %s  ctx %d  np %d  spec %s  via hub %v%s", p.EffectiveMode(), orDash(p.Layout.TS), p.Ctx, p.Np, orDash(p.Spec.Type), p.Layout.ViaHub, forceNote))
		for _, r := range p.Runs {
			if r.Measured == nil {
				continue
			}
			l := r.Layout
			line(fmt.Sprintf("  %s  %.1f t/s  master %s + %s  -ts %s  %s  (planned %.1f)", r.Measured.At.Format("2006-01-02"), r.Measured.TPS, l.Master, strings.Join(l.Workers, ","), l.TS, l.SpecType, r.PlannedTPS))
		}
	}
	return b.String()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// viewOut shows the multi-line result of the last action under a screen (the tail when it is long).
func (m model) viewOut() string {
	if m.outText == "" {
		return ""
	}
	width := m.w
	if width <= 0 {
		width = 110
	}
	lines := strings.Split(m.outText, "\n")
	limit := 14
	if m.h > 0 {
		limit = clampInt(m.h/3, 4, 14)
	}
	var b strings.Builder
	if len(lines) > limit {
		b.WriteString(helpS.Render(fmt.Sprintf("  … %d earlier lines", len(lines)-limit)) + "\n")
		lines = lines[len(lines)-limit:]
	}
	for _, l := range lines {
		b.WriteString(descS.Render(trunc("  "+l, width-2)) + "\n")
	}
	return "\n" + b.String()
}

func clampInt(v, lo, hi int) int { return min(max(v, lo), hi) }

// removeProfile deletes a saved profile and, when it had one, its autostart unit (left behind, it would try to deploy
// a profile that no longer exists at every boot).
func removeProfile(name string) error {
	if err := tuiProfiles().Remove(name); err != nil {
		return err
	}
	if setup.ProfileAutostartEnabled(name) {
		if _, err := setup.ProfileAutostartOff(name); err != nil {
			return fmt.Errorf("profile %s deleted, but its autostart unit could not be removed: %w (fleet profile autostart %s off)", name, err, name)
		}
	}
	return nil
}

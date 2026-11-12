package tui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/profile"
)

// The plan screen's profiles: a (save the chosen candidate as a profile), L (the profiles panel: ↑/↓ choose,
// enter deploys it with `fleet deploy --profile`, x deletes it, esc / L closes; the panel shows each profile's
// measurements).

// tuiProfiles is the store the TUI uses (the same directory as `fleet profile`).
var tuiProfiles = profile.Default

// applyProfile runs `fleet deploy --profile NAME` (this binary) and returns its output; tests replace it.
var applyProfile = func(name string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	out, err := exec.Command(exe, "deploy", "--profile", name).CombinedOutput()
	return string(out), err
}

// candidateProfile is the chosen candidate of the plan screen as a profile: its machines and split (fixed), the
// screen's context, slots and speculation.
func (m model) candidateProfile(name string) (*profile.Profile, error) {
	if m.planRes == nil || len(m.planRes.Candidates) == 0 {
		return nil, fmt.Errorf("plan a file first")
	}
	pick := m.planPick
	if pick >= len(m.planRes.Candidates) {
		pick = 0
	}
	c := m.planRes.Candidates[pick]
	spec := []string{"auto", "draft-dflash", "draft-mtp", "ngram-mod", "none"}[min(m.planSpecMode, 4)]
	l := profile.CandidateLayout(c, spec, 0, nil)
	p := &profile.Profile{Name: name, Model: m.planPath, Master: c.Master, Workers: l.Workers, Ctx: m.currentPlanCtx(), Np: planNps[m.planNp],
		CPUOnly: m.planCPUOnly, Spec: profile.Spec{Type: spec}, Layout: profile.Layout{Mode: profile.LayoutFixed, TS: l.TS, WorkerCPUExperts: l.CPUExperts, ViaHub: m.planViaHub},
		Notes: fmt.Sprintf("saved from the plan screen: candidate %d (%s, %.1f t/s estimated)", pick+1, c.Kind, c.TPS)}
	if m.planMaster != "" {
		p.Master = m.planMaster
	}
	return p, p.Validate()
}

func (m model) saveCandidateProfile(name string) (tea.Model, tea.Cmd) {
	p, err := m.candidateProfile(name)
	if err == nil {
		err = tuiProfiles().Save(p)
	}
	if err != nil {
		m.note = badS.Render("error: " + err.Error())
		return m, nil
	}
	m.note = okS.Render("✓ profile " + name + " saved — L lists the profiles, fleet deploy --profile " + name + " deploys it")
	return m, nil
}

func (m model) defaultProfileName() string {
	base := strings.TrimSuffix(filepath.Base(m.planPath), filepath.Ext(m.planPath))
	n := config.Slug(base)
	if len(n) > 48 {
		n = n[:48]
	}
	if n == "" {
		n = "profile"
	}
	return fmt.Sprintf("%s-c%d", n, m.planPick+1)
}

func (m model) openProfiles() model {
	list, _, err := tuiProfiles().List()
	if err != nil {
		m.note = badS.Render("error: " + err.Error())
		return m
	}
	m.profList, m.profOpen = list, true
	if m.profCur >= len(list) {
		m.profCur = 0
	}
	return m
}

// keyProfiles handles the keys while the profiles panel is open.
func (m model) keyProfiles(s string) (tea.Model, tea.Cmd) {
	if m.pend != nil && m.pend.kind == "rm-profile" { // x asked: y deletes the profile it named, any other key cancels
		name := m.pend.name
		m.pend = nil
		if s != "y" && s != "enter" {
			m.note = ""
			return m, nil
		}
		if err := removeProfile(name); err != nil {
			m.note = badS.Render("error: " + err.Error())
			return m, nil
		}
		m = m.openProfiles()
		m.note = okS.Render("✓ profile " + name + " deleted")
		return m, nil
	}
	switch s {
	case "esc", "L":
		m.profOpen = false
	case "up", "k":
		if m.profCur > 0 {
			m.profCur--
		}
	case "down", "j":
		if m.profCur < len(m.profList)-1 {
			m.profCur++
		}
	case "x", "X", "delete":
		if m.profCur < len(m.profList) { // asks first, like the Profiles screen (it used to delete at once)
			name := m.profList[m.profCur].Name
			m.pend = &pendingAct{kind: "rm-profile", name: name}
			m.note = badS.Render("delete profile " + name + "? y delete, n cancel")
		}
	case "s":
		if m.profCur < len(m.profList) {
			name := m.profList[m.profCur].Name
			m.note = helpS.Render("stopping profile " + name + " (fleet profile down " + name + ") …")
			return m, fleetOut("profile "+name+" stopped", "profile", "down", name)
		}
	case "enter":
		if m.profCur < len(m.profList) {
			name := m.profList[m.profCur].Name
			m.note = helpS.Render("deploying profile " + name + " (fleet deploy --profile " + name + ") …")
			return m, func() tea.Msg {
				out, err := applyProfile(name)
				lines := strings.Split(strings.TrimSpace(out), "\n")
				last := lines[len(lines)-1]
				if err != nil {
					return noteMsg(badS.Render("error: " + err.Error() + ": " + last))
				}
				return noteMsg(okS.Render("✓ profile " + name + " deployed: " + last))
			}
		}
	}
	return m, nil
}

// viewProfiles is the profiles panel: one line per profile, the highlighted one with its runs and measurements.
func (m model) viewProfiles(width int) string {
	var b strings.Builder
	b.WriteString(headS.Render("Profiles") + helpS.Render("  ↑/↓ choose • enter deploy • d stop • x delete • esc close") + "\n")
	if len(m.profList) == 0 {
		b.WriteString(helpS.Render("  no profiles yet — a saves the chosen candidate as one, or fleet profile save …") + "\n")
		return b.String()
	}
	for i, p := range m.profList {
		lead := "  "
		if i == m.profCur {
			lead = accS.Render("▸ ")
		}
		m := "not measured"
		if r := p.LastMeasured(); r != nil {
			m = fmt.Sprintf("%.1f t/s (%s)", r.Measured.TPS, r.Measured.At.Format("2006-01-02"))
		}
		b.WriteString(trunc(fmt.Sprintf("%s%-24s %-5s %-12s %d run(s) %s", lead, p.Name, p.EffectiveMode(), p.Master, len(p.Runs), m), width-2) + "\n")
	}
	if m.profCur < len(m.profList) {
		p := m.profList[m.profCur]
		b.WriteString(descS.Render(trunc("  model "+p.Model, width-2)) + "\n")
		for _, r := range p.Runs {
			if r.Measured == nil {
				continue
			}
			l := r.Layout
			b.WriteString(trunc(fmt.Sprintf("    %s  %.1f t/s  master %s + %s  -ts %s  %s  (planned %.1f)", r.Measured.At.Format("2006-01-02"), r.Measured.TPS,
				l.Master, strings.Join(l.Workers, ","), l.TS, l.SpecType, r.PlannedTPS), width-2) + "\n")
		}
	}
	return b.String()
}

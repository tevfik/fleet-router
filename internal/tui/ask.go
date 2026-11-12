package tui

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

type askExecDoneMsg struct {
	err error
}

func (m model) launchAgentTUI(target string) (tea.Model, tea.Cmd) {
	exe, err := os.Executable()
	if err != nil {
		exe = "fleet"
	}
	var args []string
	args = append(args, "ask", "-i")
	if target != "" {
		args = append(args, target)
	} else if m.askTarget != "" {
		args = append(args, m.askTarget)
	} else {
		for _, ep := range m.st.Endpoints {
			if strings.HasPrefix(ep.Name, "distributed-") {
				args = append(args, ep.Name)
				break
			}
		}
	}
	cmd := exec.Command(exe, args...)
	return m, tea.ExecProcess(cmd, func(err error) tea.Msg {
		return askExecDoneMsg{err: err}
	})
}

type chatItem struct {
	user   string
	model  string
	reply  string
	durMS  int64
	tokens int
	tps    float64
	err    string
}

type askMsg struct {
	target string
	prompt string
	reply  string
	ep     string
	err    error
	durMS  int64
	tokens int
}

func (m model) runAsk(target, prompt string) tea.Cmd {
	cl := m.cl
	start := time.Now()
	return func() tea.Msg {
		ans, ep, err := cl.Chat(target, "", prompt, 0, 5*time.Minute)
		dur := time.Since(start).Milliseconds()
		toks := len(strings.Fields(ans)) * 4 / 3 // token estimate
		if toks < 1 && len(ans) > 0 {
			toks = 1
		}
		return askMsg{
			target: target,
			prompt: prompt,
			reply:  ans,
			ep:     ep,
			err:    err,
			durMS:  dur,
			tokens: toks,
		}
	}
}

func (m model) isTargetOnline(target string) bool {
	for _, ep := range m.st.Endpoints {
		if ep.Name == target {
			return ep.OK
		}
	}
	for r, rs := range m.st.Roles {
		if r == target {
			return rs.Active != ""
		}
	}
	return true
}

func (m model) askTargets() []string {
	var deployed []string
	var roles []string
	var others []string
	var offline []string

	for _, ep := range m.st.Endpoints {
		if !ep.OK {
			offline = append(offline, ep.Name)
			continue
		}
		if strings.HasPrefix(ep.Name, "distributed-") || (ep.Group != nil && ep.Group.Engine == "llama-ring") {
			deployed = append(deployed, ep.Name)
		} else {
			others = append(others, ep.Name)
		}
	}
	for r := range m.st.Roles {
		roles = append(roles, r)
	}

	sort.Strings(deployed)
	sort.Strings(roles)
	sort.Strings(others)
	sort.Strings(offline)

	var all []string
	all = append(all, deployed...)
	all = append(all, roles...)
	all = append(all, others...)
	all = append(all, offline...)

	if len(all) == 0 {
		all = []string{"scout", "coder", "architect"}
	}
	return all
}

func (m model) enterAsk(defaultTarget string) model {
	m.sc = scAsk
	m.editing = ""
	m.pick = false
	targets := m.askTargets()

	if defaultTarget != "" {
		m.askTarget = defaultTarget
	} else if m.askTarget == "" || !m.isTargetOnline(m.askTarget) {
		// Prefer an online deployed model, then any online target
		for _, t := range targets {
			if strings.HasPrefix(t, "distributed-") && m.isTargetOnline(t) {
				m.askTarget = t
				break
			}
		}
		if m.askTarget == "" || !m.isTargetOnline(m.askTarget) {
			for _, t := range targets {
				if m.isTargetOnline(t) {
					m.askTarget = t
					break
				}
			}
		}
		if m.askTarget == "" && len(targets) > 0 {
			m.askTarget = targets[0]
		}
	}
	m.note = ""
	return m
}

func (m model) keyAsk(s, text string) (tea.Model, tea.Cmd) {
	targets := m.askTargets()

	// If in target picker modal:
	if m.pick {
		switch s {
		case "esc":
			m.pick = false
			return m, nil
		case "up", "k":
			if m.cur > 0 {
				m.cur--
			}
			return m, nil
		case "down", "j":
			if m.cur < len(targets)-1 {
				m.cur++
			}
			return m, nil
		case "enter":
			if m.cur >= 0 && m.cur < len(targets) {
				m.askTarget = targets[m.cur]
			}
			m.pick = false
			return m, nil
		}
		return m, nil
	}

	// Normal Chat input mode:
	switch s {
	case "esc":
		m.sc, m.cur = scHome, 0
		return m, nil
	case "tab", "ctrl+t":
		// Open target picker (Tab or Ctrl+T)!
		m.pick = true
		m.cur = 0
		for i, t := range targets {
			if t == m.askTarget {
				m.cur = i
				break
			}
		}
		return m, nil
	case "ctrl+a":
		return m.launchAgentTUI(m.askTarget)
	case "enter":
		if strings.TrimSpace(m.askInput) != "" && !m.askBusy {
			p := m.askInput
			m.askInput = ""
			m.askBusy = true
			return m, m.runAsk(m.askTarget, p)
		} else if strings.TrimSpace(m.askInput) == "" && !m.askBusy {
			return m.launchAgentTUI(m.askTarget)
		}
		return m, nil
	case "backspace":
		if len(m.askInput) > 0 {
			r := []rune(m.askInput)
			m.askInput = string(r[:len(r)-1])
		}
		return m, nil
	case "ctrl+l":
		m.askLog = nil
		return m, nil
	default:
		if len(text) > 0 && text != "\n" && text != "\r" {
			m.askInput += text
		}
		return m, nil
	}
}

func (m model) viewAsk() string {
	var b strings.Builder
	b.WriteString(headS.Render("💬 Chat & Model Test (Fleet Ask)") + "\n")
	b.WriteString(descS.Render("Test deployed models and roles live through the Fleet router with token metrics.") + "\n\n")

	targets := m.askTargets()

	// If Target Picker is open, render vertical modal list:
	if m.pick {
		b.WriteString(groupS.Render("── SELECT TARGET MODEL OR ROLE ──") + helpS.Render("  (↑/↓ move · enter select · esc cancel)") + "\n\n")
		start, end := window(len(targets), m.cur, 12)
		for i := start; i < end; i++ {
			t := targets[i]
			tag := ""
			icon := "  "
			if !m.isTargetOnline(t) {
				tag = badS.Render("  [OFFLINE / 503]")
			} else if strings.HasPrefix(t, "distributed-") {
				tag = okS.Render("  [🚀 Deployed Distributed Model]")
			} else if strings.Contains(t, "/") {
				tag = descS.Render("  (endpoint)")
			} else {
				tag = descS.Render("  (role)")
			}
			if i == m.cur {
				icon = accS.Render("▸ ")
				b.WriteString(icon + headS.Render(fmt.Sprintf("%-35s", t)) + tag + "\n")
			} else {
				b.WriteString(icon + descS.Render(fmt.Sprintf("%-35s", t)) + tag + "\n")
			}
		}
		b.WriteString("\n" + helpS.Render("Press Enter to select target model, or Esc to cancel.") + "\n")
		return b.String()
	}

	// Single clean target header:
	targetBadge := okS.Render("[ " + m.askTarget + " ]")
	if !m.isTargetOnline(m.askTarget) {
		targetBadge = badS.Render("[ ✗ OFFLINE: " + m.askTarget + " ]")
	} else if strings.HasPrefix(m.askTarget, "distributed-") {
		targetBadge = okS.Render("[ 🚀 " + m.askTarget + " ]")
	}
	b.WriteString(descS.Render("Target Model: ") + targetBadge + helpS.Render("   (Tab: change target · Enter on empty or Ctrl+A: Full AgentTUI · Ctrl+L: clear)") + "\n\n")

	// Chat conversation area
	b.WriteString(groupS.Render("── CONVERSATION HISTORY ──") + "\n")
	if len(m.askLog) == 0 {
		b.WriteString(helpS.Render("  No messages yet. Type your message below and press Enter to chat with ") + accS.Render(m.askTarget) + ".\n")
	} else {
		for _, item := range m.askLog {
			b.WriteString(accS.Render("👤 You: ") + item.user + "\n")
			if item.err != "" {
				b.WriteString(badS.Render("⚠️ Error: ") + item.err + "\n\n")
			} else {
				metrics := fmt.Sprintf("(%d tokens · %dms", item.tokens, item.durMS)
				if item.tps > 0 {
					metrics += fmt.Sprintf(" · %.1f tok/s", item.tps)
				}
				metrics += ")"
				b.WriteString(okS.Render("🤖 "+item.model+" ") + helpS.Render(metrics) + "\n")
				lines := strings.Split(strings.TrimSpace(item.reply), "\n")
				for _, l := range lines {
					b.WriteString("   " + l + "\n")
				}
				b.WriteString("\n")
			}
		}
	}

	if m.askBusy {
		b.WriteString(accS.Render("⏳ Waiting for " + m.askTarget + " to answer …\n\n"))
	} else {
		b.WriteString("\n")
	}

	// Prompt input area
	b.WriteString(groupS.Render("── PROMPT INPUT ──") + "\n")
	inputDisplay := m.askInput
	if inputDisplay == "" {
		inputDisplay = helpS.Render("Type message & press Enter (or Enter on empty / Ctrl+A for Full AgentTUI)…")
	} else {
		inputDisplay = accS.Render(inputDisplay) + "█"
	}
	b.WriteString("  > " + inputDisplay + "\n\n")

	return b.String()
}

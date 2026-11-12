package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/tevfik/fleet-router/internal/client"
	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/crypto"
	"github.com/tevfik/fleet-router/internal/economy"
)

// viewEconomy renders the Token Economy & Wallet screen.
func (m model) viewEconomy() string {
	var b strings.Builder

	if !m.hub.loaded {
		return helpS.Render("loading the wallet and the tunnel state …")
	}
	if m.hub.walletErr != "" {
		return badS.Render("Error loading wallet identity: " + m.hub.walletErr)
	}

	econ := m.cfg.Economy
	tunnelConnected := m.hub.tunConn
	tunnelURL := m.hubURLNow()
	if tunnelURL == "" {
		tunnelURL = "no hub set"
	}
	if m.hub.tunURL != "" {
		tunnelURL = m.hub.tunURL
	}

	// 1. Wallet Identity Card
	b.WriteString(groupS.Render("CRYPTOGRAPHIC WALLET & IDENTITY") + "\n")
	b.WriteString(fmt.Sprintf("  Address:      %s\n", accS.Render(m.hub.addr)))
	b.WriteString(fmt.Sprintf("  Balance:      %s credits\n", okS.Render(fmt.Sprintf("%d", m.hub.balance))))
	b.WriteString(fmt.Sprintf("  Total Earned: %d credits (hosting idle compute)  ·  Total Spent: %d credits\n\n", m.hub.earned, m.hub.spent))

	// 2. Settings Items
	b.WriteString(groupS.Render("MODULE CONTROLS & SETTINGS (↑/↓ navigate, enter/space toggle)") + "\n")

	items := []struct {
		key     string
		title   string
		enabled bool
		desc    string
	}{
		{
			key:     "",
			title:   "Token Economy Master Switch",
			enabled: econ.IsEnabled(),
			desc:    "Enables decentralized cryptographic accounting and usage receipts",
		},
		{
			key:     "",
			title:   "Fair Queuing & QoS Priority Boost",
			enabled: econ.IsFairQueuing(),
			desc:    "High-credit tasks dynamically receive router queue priority slicing",
		},
		{
			key:     "",
			title:   "Share Idle GPU Compute",
			enabled: econ.IsShareIdle(),
			desc:    "Allows remote peer fleets to lease idle VRAM and earns tokens",
		},
		{
			key:     "j",
			title:   fmt.Sprintf("Fleet Hub Tunnel (%s)", tunnelURL),
			enabled: tunnelConnected,
			desc:    "Maintains outbound WebSocket reverse tunnel to central Fleet Hub",
		},
	}

	for i, it := range items {
		prefix := "  "
		if i == m.cur {
			prefix = "▸ "
		}
		var stateMark string
		if it.key == "d" {
			stateMark = "[+]"
		} else if it.enabled {
			stateMark = okS.Render("[✓]")
		} else {
			stateMark = helpS.Render("[ ]")
		}

		line := fmt.Sprintf("%s%s %-36s  %s", prefix, stateMark, it.title, helpS.Render(it.desc))
		if i == m.cur {
			line = activeS.Render(line)
		}
		b.WriteString(line + "\n")
	}

	b.WriteString("\n" + descS.Render("Tips: Using local GPU is always free. Credits are exchanged for remote compute leasing.") + "\n")
	return b.String()
}

// keyEconomy handles user input on the Economy & Wallet screen.
func (m model) keyEconomy(s string) (tea.Model, tea.Cmd) {
	cfg := m.cfg
	if cfg.Economy == nil {
		t, f := true, false
		cfg.Economy = &config.EconomyConfig{
			Enabled:     &t,
			FairQueuing: &t,
			ShareIdle:   &f,
		}
	}

	switch s {
	case "enter", " ":
		switch m.cur {
		case 0: // Toggle master economy
			val := !cfg.Economy.IsEnabled()
			cfg.Economy.Enabled = &val
			return m.saveEconomyConfig(cfg, fmt.Sprintf("Token economy %s", onOff(val)))
		case 1: // Toggle fair queuing
			val := !cfg.Economy.IsFairQueuing()
			cfg.Economy.FairQueuing = &val
			return m.saveEconomyConfig(cfg, fmt.Sprintf("Fair Queuing %s", onOff(val)))
		case 2: // Toggle idle GPU sharing
			val := !cfg.Economy.IsShareIdle()
			cfg.Economy.ShareIdle = &val
			return m.saveEconomyConfig(cfg, fmt.Sprintf("Idle compute sharing %s", onOff(val)))
		case 3: // Toggle tunnel join/leave
			return m.toggleHubTunnel()
		}

	case "j":
		return m.toggleHubTunnel()
	}

	return m, nil
}

func (m model) saveEconomyConfig(cfg config.Config, msg string) (tea.Model, tea.Cmd) {
	cl := m.cl
	return m, func() tea.Msg {
		if err := cl.SetEconomySettings(*cfg.Economy); err != nil {
			return noteMsg(badS.Render("error: " + err.Error()))
		}
		_ = cl.Reload()
		return noteMsg(okS.Render("✓ " + msg))
	}
}

func (m model) toggleHubTunnel() (tea.Model, tea.Cmd) {
	cl := m.cl
	connected := m.hub.tunConn // from the last fetch (a blocking call here would freeze Update)
	hub := m.hubURLNow()
	return m, func() tea.Msg {
		if connected {
			if err := cl.LeaveHub(); err != nil {
				return noteMsg(badS.Render("error disconnecting from hub: " + err.Error()))
			}
			return noteMsg(okS.Render("✓ Disconnected from Fleet Hub"))
		}
		if hub == "" {
			return noteMsg(badS.Render(config.ErrNoHub.Error()))
		}
		if err := cl.JoinHub(hub, ""); err != nil {
			return noteMsg(badS.Render("error connecting to hub: " + err.Error()))
		}
		return noteMsg(okS.Render("✓ Connected to Fleet Hub (" + hub + ")"))
	}
}

func onOff(v bool) string {
	if v {
		return "ENABLED"
	}
	return "DISABLED"
}

// hubInfo is what the Hub and Economy screens show. fetch() reads it on the command goroutine: the daemon calls
// (5 s timeout each), the wallet identity and the ledger file used to be read inside View on every render.
type hubInfo struct {
	loaded    bool
	hs        client.HubStatus
	hsOK      bool
	tunConn   bool
	tunURL    string
	addr      string
	balance   int64
	earned    int64
	spent     int64
	walletErr string
}

func (m model) readHubInfo() hubInfo {
	hi := hubInfo{loaded: true}
	if m.cl != nil {
		if st, err := m.cl.HubStatus(); err == nil {
			hi.hs, hi.hsOK = st, true
		}
		if c, u, err := m.cl.TunnelStatus(); err == nil {
			hi.tunConn, hi.tunURL = c, u
		}
	}
	id, err := crypto.LoadOrGenerateIdentity("")
	if err != nil {
		hi.walletErr = err.Error()
		return hi
	}
	hi.addr = id.Address()
	ledger, _ := economy.LoadLedger("") // never nil
	acc := ledger.GetOrCreateAccount(id.Address())
	hi.balance, hi.earned, hi.spent = acc.Balance, acc.TotalEarned, acc.TotalSpent
	return hi
}

package tui

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/tevfik/fleet-router/internal/client"
	"github.com/tevfik/fleet-router/internal/config"
)

// localNodeName returns the current node's name, falling back to status self-node or hostname.
func (m model) localNodeName() string {
	if m.cfg.Name != "" {
		return m.cfg.Name
	}
	for _, n := range m.st.Nodes {
		if n.Self && n.Node != "" {
			return n.Node
		}
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "this-node"
}

// viewHub renders the dedicated Fleet Hub & Cloud Peering management screen.
func (m model) viewHub() string {
	var b strings.Builder

	hubURL := m.hubURL
	if hubURL == "" {
		hubURL = m.cfg.HubURL
	}
	if hubURL == "" {
		hubURL = config.DefaultHubURL
	}

	connected := false
	var hs client.HubStatus
	if m.hub.hsOK { // read by fetch(): a daemon that hangs must not freeze the screen
		hs, connected = m.hub.hs, m.hub.hs.Connected
		if hs.HubURL != "" {
			hubURL = hs.HubURL
		}
	}

	localNode := m.localNodeName()

	// 1. Connection Status Card
	b.WriteString(groupS.Render("FLEET HUB & REVERSE TUNNEL STATUS") + "\n")
	if connected {
		b.WriteString(fmt.Sprintf("  Status:        %s\n", okS.Render("● CONNECTED (Active WebSocket Tunnel)")))
	} else {
		b.WriteString(fmt.Sprintf("  Status:        %s\n", badS.Render("○ DISCONNECTED (Offline)")))
		if hs.Status.LastError != "" {
			b.WriteString(fmt.Sprintf("  Last error:    %s\n", badS.Render(trunc(hs.Status.LastError, 90))))
		}
		if hs.Status.Rejected {
			b.WriteString(fmt.Sprintf("  %s\n", badS.Render("The hub REFUSED this node: the token or API key must change (retrying will not help)")))
		}
		if !hs.Status.NextRetry.IsZero() {
			d := time.Until(hs.Status.NextRetry).Round(time.Second)
			if d < 0 {
				d = 0
			}
			b.WriteString(fmt.Sprintf("  Next attempt:  in %s (%d failed in a row)\n", d, hs.Status.Attempts))
		}
	}
	if hs.Insecure {
		b.WriteString(fmt.Sprintf("  %s\n", badS.Render("! the hub's TLS certificate is not verified (hub_insecure)")))
	}
	b.WriteString(fmt.Sprintf("  Hub Endpoint:  %s\n", accS.Render(hubURL)))
	b.WriteString(fmt.Sprintf("  Local Node:    %s (Daemon port 4711)\n", normalS.Render(localNode)))
	b.WriteString(fmt.Sprintf("  Protocol:      %s\n", helpS.Render("WSS / Port 443 (Outbound Reverse Tunnel)")))
	b.WriteString(fmt.Sprintf("  Traefik Proxy: %s\n\n", helpS.Render("Direct WebSocket Pass-Through (Bypasses forward auth)")))

	// 2. Action Menu
	b.WriteString(groupS.Render("ACTIONS (↑/↓ select, enter execute)") + "\n")

	connectAction := "[ Connect to Fleet Hub ] (c)"
	if connected {
		connectAction = "[ Disconnect from Fleet Hub ] (c)"
	}

	tokenDesc := "none"
	token := m.hubToken
	if token == "" {
		token = m.cfg.HubToken // same fallback as keyHub: a token set only in the configuration is used
	}
	if token == "" {
		token = m.cfg.HubToken
	}
	if token != "" {
		tokenDesc = "configured"
	}

	keyDesc := "none"
	if m.hubAPIKeyVal() != "" {
		keyDesc = "configured"
	}

	actions := []struct {
		title string
		desc  string
	}{
		{connectAction, "Toggle outbound WebSocket tunnel connection to Hub"},
		{fmt.Sprintf("[ Set Hub URL ] (%s)", hubURL), "Change target Fleet Hub server address"},
		{fmt.Sprintf("[ Set Auth Token ] (%s)", tokenDesc), "Tunnel join token (used by Connect; not the API key)"},
		{fmt.Sprintf("[ Set API Key ] (%s) (k)", keyDesc), "Hub API key for /v1/* requests (same as 'fleet key set')"},
		{"[ Toggle Models View ]", "Show/hide exported models & remote Hub catalog"},
		{"[ Refresh Models from Hub ] (r)", "Query the hub's /v1/models directly"},
		{"[ Ping & Test Connection ]", "Verify WebSocket handshake & measure tunnel round-trip"},
		{"[ Show API Key ]", "Masked hub API key (same as 'fleet key show')"},
		{"[ Generate API Key ] (g)", "Create a new random hub API key and save it (same as 'fleet key generate'); give it to the nodes that join this hub"},
		{"[ Reveal API Key ] (v)", "Show the hub API key in full on this screen (same as 'fleet key show --reveal')"},
		{"[ Remove API Key ]", "Forget the hub API key (same as 'fleet key rm')"},
		{"[ Connect with Stream Ports… ]", "Join the Hub allowing raw TCP streams to local ports (fleet join --stream-port / --add-stream-port)"},
		{"[ Open Stream… ]", "Listen locally and reach a joined node's port through the Hub (fleet stream connect)"},
		{fmt.Sprintf("[ Stop Streams ] (%d open)", len(activeStreams())), "Close the streams opened here"},
		{"[ ← Back ]", "Return to previous screen"},
	}

	for i, act := range actions {
		prefix := "  "
		if i == m.cur {
			prefix = "▸ "
		}
		line := prefix + act.title
		if i == m.cur {
			line = activeS.Render(line)
		}
		b.WriteString(line + "\n")
	}
	// the description of the selected action, and the result of the last one, right under the list: a long screen
	// would push a note printed at the very bottom out of the terminal and the action would look dead
	if m.cur >= 0 && m.cur < len(actions) {
		b.WriteString("\n  " + helpS.Render(actions[m.cur].desc) + "\n")
	}

	// 3. Models Section
	if m.hubShowModels {
		// 3A. Local Models Exported to Hub
		b.WriteString("\n" + groupS.Render("MODELS EXPORTED TO HUB (reachable via <hub>/v1):") + "\n")

		type epItem struct {
			model string
			node  string
			caps  string
		}
		var myModels []epItem

		// Pull from live daemon status endpoints first
		if len(m.st.Endpoints) > 0 {
			for _, ep := range m.st.Endpoints {
				mdl := ep.Model
				if mdl == "" {
					mdl = ep.Endpoint.Model
				}
				if mdl == "" {
					mdl = ep.Endpoint.Name
				}
				node := ep.Node
				if node == "" {
					node = ep.Endpoint.Node
				}
				capsStr := strings.Join(ep.Caps, ", ")
				if capsStr == "" {
					capsStr = strings.Join(ep.Endpoint.Caps, ", ")
				}
				myModels = append(myModels, epItem{model: mdl, node: node, caps: capsStr})
			}
		} else {
			// Fallback to static config endpoints
			for _, ep := range m.cfg.Endpoints {
				myModels = append(myModels, epItem{model: ep.Model, node: ep.Node, caps: strings.Join(ep.Caps, ", ")})
			}
		}

		if len(myModels) == 0 {
			b.WriteString(helpS.Render("  No local models registered on this node.\n"))
		} else {
			for _, item := range myModels {
				nodeTag := item.node
				if nodeTag == "" {
					nodeTag = localNode
				}
				capsText := ""
				if item.caps != "" {
					capsText = fmt.Sprintf(" (%s)", item.caps)
				}
				b.WriteString(fmt.Sprintf("  • %s %s%s\n", normalS.Render("["+nodeTag+"]"), accS.Render(item.model), helpS.Render(capsText)))
			}
		}

		// 3B. Remote Models Available on Fleet Hub
		b.WriteString("\n" + groupS.Render("REMOTE MODELS ON FLEET HUB ("+hubURL+"/v1/models):") + "\n")
		if m.hubRemoteBusy {
			b.WriteString(helpS.Render("  ⏳ Querying models from Fleet Hub...\n"))
		} else if m.hubRemoteErr != "" {
			b.WriteString(badS.Render(fmt.Sprintf("  ✕ %s\n", m.hubRemoteErr)))
		} else if len(m.hubRemoteModels) == 0 {
			b.WriteString(helpS.Render("  ○ No remote models received. Press 'r' or select [ Refresh Models from Hub ] to fetch.\n"))
		} else {
			for _, rm := range m.hubRemoteModels {
				statusDot := okS.Render("●")
				if !rm.Up {
					statusDot = badS.Render("○")
				}
				capsText := ""
				if len(rm.Caps) > 0 {
					capsText = fmt.Sprintf(" (%s)", strings.Join(rm.Caps, ", "))
				}
				owner := rm.OwnedBy
				if owner == "" {
					owner = "hub"
				}
				b.WriteString(fmt.Sprintf("  %s %-38s  %s%s\n", statusDot, normalS.Render(rm.ID), helpS.Render("owned: "+owner), helpS.Render(capsText)))
			}
			b.WriteString(helpS.Render(fmt.Sprintf("  (Total: %d models active on Fleet Hub)\n", len(m.hubRemoteModels))))
		}
	}

	b.WriteString(streamsView())
	b.WriteString("\n" + descS.Render("Tips: Joining a Hub allows remote clients & Cursor to reach your local models via "+hubURL+"/v1 securely.") + "\n")
	return b.String()
}

// keyHub handles user interaction on the Fleet Hub management screen.
func (m model) keyHub(s string) (tea.Model, tea.Cmd) {
	hubURL := m.hubURL
	if hubURL == "" {
		hubURL = m.cfg.HubURL
	}
	if hubURL == "" {
		hubURL = config.DefaultHubURL
	}
	hubToken := m.hubToken
	if hubToken == "" {
		hubToken = m.cfg.HubToken
	}

	connected := m.hub.tunConn // from the last fetch (never a blocking call inside Update)

	switch s {
	case "enter", " ":
		switch m.cur {
		case 0: // Connect / Disconnect
			return m.toggleHubConnection(connected, hubURL)
		case 1: // Set Hub URL
			m.editing, m.editBuf = "huburl", hubURL
			return m, nil
		case 2: // Set Auth Token
			m.editing, m.editBuf = "hubtoken", m.hubToken
			return m, nil
		case 3: // Set API Key
			m.editing, m.editBuf = "hubapikey", ""
			return m, nil
		case 4: // Toggle Models View
			m.hubShowModels = !m.hubShowModels
			if m.hubShowModels && len(m.hubRemoteModels) == 0 {
				m.hubRemoteBusy = true
				return m, m.fetchRemoteHubModels()
			}
			return m, nil
		case 5: // Refresh Models from Hub
			m.hubShowModels = true
			m.hubRemoteBusy = true
			return m, m.fetchRemoteHubModels()
		case 6: // Ping & Test
			return m.testHubPing(hubURL)
		case 7:
			return m.showHubKey()
		case 8:
			return m.generateHubKey()
		case 9:
			return m.revealHubKey()
		case 10:
			return m.removeHubKey()
		case 11:
			m.editing, m.editBuf = "hubstreamports", ""
			return m, nil
		case 12:
			m.editing, m.editBuf = "hubstream", ""
			return m, nil
		case 13:
			return m.stopHubStreams()
		case 14: // Back
			prev := m.hubPrev
			if prev == 0 {
				prev = scHome
			}
			m.sc, m.cur = prev, 0
			return m, nil
		}

	case "c": // Direct shortcut: connect/disconnect
		return m.toggleHubConnection(connected, hubURL)

	case "k": // Direct shortcut: edit API key
		m.editing, m.editBuf = "hubapikey", ""
		return m, nil

	case "g": // Direct shortcut: generate an API key
		return m.generateHubKey()

	case "v": // Direct shortcut: reveal the API key
		return m.revealHubKey()

	case "r": // Direct shortcut: refresh models
		m.hubShowModels = true
		m.hubRemoteBusy = true
		return m, m.fetchRemoteHubModels()

	}

	return m, nil
}

func (m model) toggleHubConnection(connected bool, hubURL string) (tea.Model, tea.Cmd) {
	cl := m.cl
	token := m.hubToken
	return m, func() tea.Msg {
		if connected {
			if err := cl.LeaveHub(); err != nil {
				return noteMsg(badS.Render("error disconnecting: " + err.Error()))
			}
			return noteMsg(okS.Render("✓ Disconnected from Fleet Hub"))
		}
		if err := cl.JoinHub(hubURL, token); err != nil {
			return noteMsg(badS.Render("error connecting: " + err.Error()))
		}
		return noteMsg(okS.Render("✓ Connected to Fleet Hub (" + hubURL + ")"))
	}
}

// testHubPing performs a real HTTP probe to the Hub endpoint to measure RTT latency in milliseconds.
func (m model) testHubPing(hubURL string) (tea.Model, tea.Cmd) {
	cl := m.cl
	return m, func() tea.Msg {
		probeURL := strings.TrimRight(hubURL, "/")
		client := &http.Client{
			Timeout: 4 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse // don't follow redirects for fast RTT measurement
			},
		}

		t0 := time.Now()
		resp, probeErr := client.Get(probeURL)
		rtt := time.Since(t0)
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}

		ms := float64(rtt.Microseconds()) / 1000.0

		tunnelActive := false
		if cl != nil {
			if c, _, err := cl.TunnelStatus(); err == nil {
				tunnelActive = c
			}
		}

		if probeErr != nil {
			return noteMsg(badS.Render(fmt.Sprintf("✕ Hub unreachable (%s): %v", probeURL, probeErr)))
		}

		if tunnelActive {
			return noteMsg(okS.Render(fmt.Sprintf("✓ Hub is reachable · WebSocket Tunnel is ACTIVE · Latency: %.1f ms", ms)))
		}
		return noteMsg(helpS.Render(fmt.Sprintf("○ Hub is reachable (Latency: %.1f ms) · WebSocket Tunnel is OFFLINE", ms)))
	}
}

// fetchRemoteHubModels queries the Hub's /v1/models endpoint directly over HTTPS.
func (m model) fetchRemoteHubModels() tea.Cmd {
	hubURL := m.hubURL
	if hubURL == "" {
		hubURL = m.cfg.HubURL
	}
	if hubURL == "" {
		hubURL = config.DefaultHubURL
	}
	token := m.hubAPIKeyVal()

	return func() tea.Msg {
		modelsURL := strings.TrimRight(hubURL, "/") + "/v1/models"
		req, err := http.NewRequest("GET", modelsURL, nil)
		if err != nil {
			return hubModelsMsg{err: err}
		}

		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}

		client := &http.Client{Timeout: 6 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return hubModelsMsg{err: fmt.Errorf("failed to query %s: %w", modelsURL, err)}
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return hubModelsMsg{err: fmt.Errorf("Hub requires authentication (HTTP %d). Set the hub API key via [ Set API Key ]", resp.StatusCode)}
		}
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			return hubModelsMsg{err: fmt.Errorf("Hub returned HTTP %d: %s", resp.StatusCode, string(body))}
		}

		var payload struct {
			Data []hubModelInfo `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			return hubModelsMsg{err: fmt.Errorf("failed to decode models JSON: %w", err)}
		}

		return hubModelsMsg{models: payload.Data}
	}
}

// hubAPIKeyVal is the hub API key (model-API credential), distinct from the tunnel join token.
func (m model) hubAPIKeyVal() string {
	if m.hubAPIKey != "" {
		return m.hubAPIKey
	}
	return m.cfg.HubAPIKey
}

// showHubKey is `fleet key show`: the key masked, never in full.
func (m model) showHubKey() (tea.Model, tea.Cmd) {
	k := m.hubKeyNow()
	if k == "" {
		m.note = helpS.Render("no hub API key configured (k sets one)")
		return m, nil
	}
	m.note = helpS.Render(fmt.Sprintf("hub API key: %s (length %d)", maskKey(k), len(k)))
	return m, nil
}

// generateHubKey is `fleet key generate`: a new random key replaces the current one and is shown once in full.
func (m model) generateHubKey() (tea.Model, tea.Cmd) {
	k := config.GenerateHubKey()
	m.hubAPIKey, m.cfg.HubAPIKey = k, k
	m.note = okS.Render("✓ new hub API key (saving …): ") + accS.Render(k) + helpS.Render("  — give it to the nodes that join this hub; hub_require_key: true makes it mandatory")
	cl := m.cl
	if cl == nil {
		return m, nil
	}
	return m, func() tea.Msg {
		if err := cl.SetHubAPIKey(k); err != nil {
			return noteMsg(badS.Render("error saving the generated key: " + err.Error()))
		}
		return noteMsg(okS.Render("✓ new hub API key saved: ") + accS.Render(k) + helpS.Render("  — give it to the nodes that join this hub"))
	}
}

// revealHubKey is `fleet key show --reveal`: the key in full, on this screen only, until the next action.
func (m model) revealHubKey() (tea.Model, tea.Cmd) {
	k := m.hubKeyNow()
	if k == "" {
		m.note = helpS.Render("no hub API key configured (g generates one, k sets one)")
		return m, nil
	}
	m.note = helpS.Render("hub API key: ") + accS.Render(k)
	return m, nil
}

// removeHubKey is `fleet key rm`: the key leaves the configuration and the .env file.
func (m model) removeHubKey() (tea.Model, tea.Cmd) {
	m.hubAPIKey, m.cfg.HubAPIKey = "", ""
	cl := m.cl
	return m, func() tea.Msg {
		_ = config.RemoveSecret("FLEET_API_KEY")
		if cl != nil {
			if err := cl.SetHubAPIKey(""); err != nil {
				return noteMsg(badS.Render("error removing API key: " + err.Error()))
			}
		}
		return noteMsg(okS.Render("✓ hub API key removed"))
	}
}

func (m model) stopHubStreams() (tea.Model, tea.Cmd) {
	if n := stopStreams(); n > 0 {
		m.note = okS.Render(fmt.Sprintf("✓ %d stream(s) closed", n))
	} else {
		m.note = helpS.Render("no stream open")
	}
	return m, nil
}

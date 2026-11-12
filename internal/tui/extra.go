package tui

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/deploy"
	"github.com/tevfik/fleet-router/internal/tunnel"
)

// The TUI side of the smaller CLI commands: fleet config node set|rm, fleet config history, fleet key show|rm,
// fleet join --stream-port / --add-stream-port, fleet stream connect, fleet run prune, fleet watchdog.

// ---- nodes: declared specs, config history ------------------------------------------------------

// specText is a NodeSpec as the key=value text the editor starts from.
func specText(s config.NodeSpec) string {
	var p []string
	if s.Addr != "" {
		p = append(p, "addr="+s.Addr)
	}
	for _, kv := range []struct {
		k string
		v float64
	}{{"mem_gib", s.MemGiB}, {"weight_gbps", s.WeightGBps}, {"expert_gbps", s.ExpertGBps}} {
		if kv.v != 0 {
			p = append(p, kv.k+"="+strconv.FormatFloat(kv.v, 'g', -1, 64))
		}
	}
	return strings.Join(p, " ")
}

// applySpec sets the given key=value pairs on s, like `fleet config node set`; the keys not given stay.
func applySpec(s config.NodeSpec, fields []string) (config.NodeSpec, error) {
	for _, kv := range fields {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return s, fmt.Errorf("%q: key=value (addr, mem_gib, weight_gbps, expert_gbps)", kv)
		}
		num := func() (float64, error) {
			if v == "" {
				return 0, nil
			}
			f, err := strconv.ParseFloat(v, 64)
			if err == nil && f < 0 {
				err = fmt.Errorf("cannot be negative")
			}
			return f, err
		}
		var err error
		switch k {
		case "addr":
			s.Addr = v
		case "notes":
			s.Notes = v
		case "mem_gib":
			s.MemGiB, err = num()
		case "weight_gbps":
			s.WeightGBps, err = num()
		case "expert_gbps":
			s.ExpertGBps, err = num()
		default:
			return s, fmt.Errorf("unknown key %q (addr, mem_gib, weight_gbps, expert_gbps)", k)
		}
		if err != nil {
			return s, fmt.Errorf("%s: %v", k, err)
		}
	}
	return s, nil
}

// specLine is what the Nodes screen shows of a machine's declared spec.
func specLine(s config.NodeSpec, ok bool) string {
	if !ok {
		return "  spec: not declared (s declares addr, memory and speeds for plans and deploys)"
	}
	return "  spec: " + specText(s)
}

func (m model) historyCmd() tea.Cmd {
	cl := m.cl
	return func() tea.Msg {
		snaps, err := cl.History()
		if err != nil {
			return outMsg{note: badS.Render("history: " + err.Error())}
		}
		if len(snaps) == 0 {
			return outMsg{note: helpS.Render("no earlier configurations kept yet")}
		}
		var b strings.Builder
		for i := len(snaps) - 1; i >= 0; i-- {
			b.WriteString(strings.TrimSuffix(snaps[i], ".yaml") + "\n")
		}
		return outMsg{note: okS.Render(fmt.Sprintf("%d earlier configuration(s), newest first — u restores the newest", len(snaps))), text: strings.TrimRight(b.String(), "\n")}
	}
}

// ---- runs: prune, watchdog -------------------------------------------------------------------------

func runsRemovedText(n int, dry bool) string {
	verb := "removed"
	if dry {
		verb = "would remove"
	}
	return fmt.Sprintf("%s %d ended run record(s); nothing was stopped", verb, n)
}

// watchdogOnce is one pass of fleet watchdog (restarts a ring master that went away and was started with exit-on-fail);
// tests replace it.
var watchdogOnce = func() (string, error) {
	cfg, err := config.Load(config.Path())
	if err != nil {
		return "", err
	}
	w := deploy.NewWatchdog(cfg)
	w.SkipChains = cfg.ChainSupervision() // the daemon supervises the chains
	w.CheckOnce(context.Background())
	return "checked every ring endpoint once (what it did is in the watchdog log: ~/.cache/fleet/runs/watchdog.log; fleet watchdog keeps checking)", nil
}

// ---- hub: key, stream ports, streams ---------------------------------------------------------------

func maskKey(k string) string {
	if len(k) > 12 {
		return k[:6] + "..." + k[len(k)-4:]
	}
	return k
}

// hubKeyNow is the hub API key the way fleet key show finds it.
func (m model) hubKeyNow() string {
	k := m.hubAPIKeyVal()
	if k == "" {
		k = config.Secret("FLEET_API_KEY")
	}
	return k
}

// parseStreamPorts reads "8080,8081" (replace) or "+8080" (add) as fleet join --stream-port / --add-stream-port.
func parseStreamPorts(s string) (ports []int, add bool, err error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "+") {
		add, s = true, strings.TrimSpace(s[1:])
	}
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		n, e := strconv.Atoi(f)
		if e != nil || n <= 0 || n > 65535 {
			return nil, false, fmt.Errorf("bad port %q", f)
		}
		ports = append(ports, n)
	}
	if add && len(ports) == 0 {
		return nil, false, fmt.Errorf("+PORT: which port to add?")
	}
	return ports, add, nil
}

// A stream is a local listener whose connections go through the hub to a port of a joined node.
type streamInfo struct {
	Listen string
	Node   string
	Port   int
	stop   func()
}

var (
	streamMu sync.Mutex
	streams  []streamInfo
)

func activeStreams() []streamInfo {
	streamMu.Lock()
	defer streamMu.Unlock()
	return append([]streamInfo{}, streams...)
}

// dropStream forgets the stream listening on listen (it is already closed).
func dropStream(listen string) {
	streamMu.Lock()
	defer streamMu.Unlock()
	kept := streams[:0:0]
	for _, s := range streams {
		if s.Listen != listen {
			kept = append(kept, s)
		}
	}
	streams = kept
}

// stopStreams closes every stream the menu opened.
func stopStreams() int {
	streamMu.Lock()
	defer streamMu.Unlock()
	n := len(streams)
	for _, s := range streams {
		s.stop()
	}
	streams = nil
	return n
}

// startStream opens the listener of `fleet stream connect` inside the menu; tests replace it.
var startStream = func(hub, node string, port int, listen, key string, insecure bool) (string, error) {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return "", fmt.Errorf("listen on %s: %w (choose another address)", listen, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	sc := &tunnel.StreamConnector{HubURL: hub, Node: node, Port: port, Key: key, Insecure: insecure} // no Logf: the menu owns the terminal
	bound := ln.Addr().String()
	go func() {
		_ = sc.Serve(ctx, ln)
		ln.Close()
		dropStream(bound) // Serve ended on its own (an error): the list must not show a dead listener
	}()
	stop := func() { cancel(); ln.Close() }
	streamMu.Lock()
	streams = append(streams, streamInfo{Listen: bound, Node: node, Port: port, stop: stop})
	streamMu.Unlock()
	return bound, nil
}

func (m model) hubURLNow() string {
	u := m.hubURL
	if u == "" {
		u = m.cfg.HubURL
	}
	if u == "" {
		u = config.DefaultHubURL
	}
	return u
}

func streamsView() string {
	list := activeStreams()
	if len(list) == 0 {
		return ""
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Listen < list[j].Listen })
	var b strings.Builder
	b.WriteString("\n" + groupS.Render("OPEN STREAMS (through the hub; z stops them)") + "\n")
	for _, s := range list {
		b.WriteString(fmt.Sprintf("  %s → %s:%d\n", accS.Render(s.Listen), s.Node, s.Port))
	}
	return b.String()
}

// ---- the text inputs of these actions --------------------------------------------------------------

func (m model) finishExtraEdit(kind, target, buf string) (model, tea.Cmd, bool) {
	cl := m.cl
	switch kind {
	case "nodespec":
		fields := strings.Fields(buf)
		if len(fields) == 0 {
			return m, nil, true
		}
		return m, func() tea.Msg {
			cfg, err := cl.Config()
			if err != nil {
				return noteMsg(badS.Render("error: " + err.Error()))
			}
			spec, err := applySpec(cfg.Nodes[target], fields)
			if err != nil {
				return noteMsg(badS.Render("error: " + err.Error()))
			}
			if err := cl.SetNodeSpec(target, spec, false); err != nil {
				return noteMsg(badS.Render("error: " + err.Error()))
			}
			return noteMsg(okS.Render("✓ nodes: " + target + " — " + specText(spec)))
		}, true
	case "hubstreamports":
		ports, add, err := parseStreamPorts(buf)
		if err != nil {
			m.note = badS.Render("error: " + err.Error())
			return m, nil, true
		}
		hub := m.hubURLNow()
		m.note = helpS.Render("connecting to " + hub + " …")
		return m, func() tea.Msg {
			if add {
				err = cl.JoinHubAdd(hub, "", ports)
			} else {
				err = cl.JoinHub(hub, m.hubToken, ports...)
			}
			if err != nil {
				return noteMsg(badS.Render("error: " + err.Error()))
			}
			if add {
				return noteMsg(okS.Render(fmt.Sprintf("✓ connected to %s; streams also allowed to local port(s) %v", hub, ports)))
			}
			if len(ports) == 0 {
				return noteMsg(okS.Render("✓ connected to " + hub + "; streams allowed to the config's hub_stream_ports"))
			}
			return noteMsg(okS.Render(fmt.Sprintf("✓ connected to %s; streams allowed to local port(s) %v", hub, ports)))
		}, true
	case "hubstream":
		f := strings.Fields(buf)
		if len(f) < 2 || len(f) > 3 {
			m.note = badS.Render("error: <node> <remote-port> [listen-address]")
			return m, nil, true
		}
		port, err := strconv.Atoi(f[1])
		if err != nil || port <= 0 || port > 65535 {
			m.note = badS.Render("error: bad remote port " + f[1])
			return m, nil, true
		}
		key := m.hubKeyNow()
		if key == "" {
			m.note = badS.Render("error: no hub API key — set one with k (Set API Key) first")
			return m, nil, true
		}
		listen := net.JoinHostPort("127.0.0.1", f[1])
		if len(f) == 3 {
			listen = f[2]
		}
		hub, node, insecure := m.hubURLNow(), f[0], m.cfg.HubInsecure
		return m, func() tea.Msg {
			addr, err := startStream(hub, node, port, listen, key, insecure) // hub_insecure: same as `fleet stream connect`
			if err != nil {
				return noteMsg(badS.Render("error: " + err.Error()))
			}
			return noteMsg(okS.Render(fmt.Sprintf("✓ %s → %s:%d through %s (z stops it)", addr, node, port, hub)))
		}, true
	}
	return m, nil, false
}

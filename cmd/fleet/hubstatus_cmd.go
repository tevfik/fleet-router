package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tevfik/fleet-router/internal/client"
)

// hubStatusLines describes the hub tunnel for fleet status / doctor: connected or not, and when not, the last error,
// whether the hub refused this node (a stale token or key retries forever without help) and when the next try is.
func hubStatusLines(hs client.HubStatus, now time.Time) []string {
	if hs.HubURL == "" && !hs.AutoJoin && !hs.Connected && len(hs.Nodes) == 0 && hs.Status.LastError == "" {
		return nil // no hub in use
	}
	var out []string
	switch {
	case hs.Connected:
		l := "hub " + hs.HubURL + ": connected"
		if !hs.Status.ConnectedSince.IsZero() {
			l += " for " + now.Sub(hs.Status.ConnectedSince).Round(time.Second).String()
		}
		out = append(out, l)
	default:
		l := "hub " + hs.HubURL + ": NOT connected"
		if hs.AutoJoin {
			l += " (retrying)"
		}
		out = append(out, l)
		if hs.Status.LastError != "" {
			e := "  last error: " + hs.Status.LastError
			if !hs.Status.LastErrorAt.IsZero() {
				e += " (" + now.Sub(hs.Status.LastErrorAt).Round(time.Second).String() + " ago)"
			}
			out = append(out, e)
		}
		if hs.Status.Rejected {
			out = append(out, "  the hub REFUSED this node: retrying with the same join token / API key will not help — fleet key set <key> and/or fleet join --token <token>")
		}
		if hs.Status.Attempts > 0 {
			out = append(out, fmt.Sprintf("  %d failed attempt(s) in a row", hs.Status.Attempts))
		}
		if !hs.Status.NextRetry.IsZero() {
			d := hs.Status.NextRetry.Sub(now).Round(time.Second)
			if d < 0 {
				d = 0
			}
			out = append(out, "  next attempt in "+d.String())
		}
	}
	if hs.Insecure {
		out = append(out, "  ! TLS certificate of the hub is not verified (hub_insecure)")
	}
	if len(hs.Nodes) > 0 {
		var names []string
		for _, n := range hs.Nodes {
			s := n.NodeName
			if n.FleetVersion != "" {
				s += " " + n.FleetVersion
			}
			names = append(names, s)
		}
		sort.Strings(names)
		out = append(out, "  nodes joined here: "+strings.Join(names, ", "))
	}
	if len(hs.ActiveStreams) > 0 {
		var parts []string
		for n, c := range hs.ActiveStreams {
			parts = append(parts, fmt.Sprintf("%s:%d", n, c))
		}
		sort.Strings(parts)
		out = append(out, "  open streams: "+strings.Join(parts, " "))
	}
	return out
}

func printHubStatus(cl *client.Client) {
	hs, err := cl.HubStatus()
	if err != nil {
		return
	}
	for _, l := range hubStatusLines(hs, time.Now()) {
		fmt.Println(l)
	}
	if len(hubStatusLines(hs, time.Now())) > 0 {
		fmt.Println()
	}
}

// localHubStreams asks this machine's daemon (when it runs) which raw TCP streams are open through it as a hub.
func localHubStreams() map[string]int {
	cl := client.New(listen())
	if !cl.Alive() {
		return nil
	}
	hs, err := cl.HubStatus()
	if err != nil {
		return nil
	}
	return hs.ActiveStreams
}

// hubStreamWarning is what a restart of this hub (fleet update restarts the service, or the hub container is
// replaced) does to the streams open through it: every one of them is cut. "" when none are open.
func hubStreamWarning(streams map[string]int) string {
	total := 0
	var parts []string
	for n, c := range streams {
		total += c
		parts = append(parts, fmt.Sprintf("%s:%d", n, c))
	}
	if total == 0 {
		return ""
	}
	sort.Strings(parts)
	return fmt.Sprintf("this hub carries %d open stream(s) (%s): updating restarts it and cuts them (ring workers behind them lose their link; the engine resumes a session, `fleet stream connect` re-dials new connections within ~7 s, the control tunnels of the nodes come back after their next reconnect, up to a minute)", total, strings.Join(parts, " "))
}

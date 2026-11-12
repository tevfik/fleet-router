package board

import (
	"fmt"
	"strings"

	"github.com/tevfik/fleet-router/internal/server"
)

// StopLines renders what a stop did (or, for a dry run, would do), the same text on the command line, in the menu
// and in the chat.
func StopLines(r server.StopReport) []string {
	var out []string
	head := "stop " + r.Endpoint
	if r.Run != "" {
		head = "stop run " + r.Run + " (endpoint " + r.Endpoint + ")"
	}
	if r.DryRun {
		head += " — dry run, nothing is touched"
	}
	out = append(out, head)
	if r.Refused != "" {
		return append(out, "✗ "+r.Refused)
	}
	for _, s := range r.Steps {
		mark := "✓"
		if !s.OK {
			mark = "✗"
		}
		port := ""
		if s.Port > 0 {
			port = fmt.Sprintf(" :%d", s.Port)
		}
		out = append(out, fmt.Sprintf("  %s %-6s %s%s — %s", mark, s.Role, s.Node, port, s.Result))
	}
	for _, w := range r.Warnings {
		out = append(out, "  ! "+w)
	}
	switch {
	case r.DryRun:
	case r.Failed():
		out = append(out, "✗ not everything stopped: the run record and the endpoint were left as they are")
	default:
		if r.Ended {
			out = append(out, "✓ run record closed")
		}
		if r.Removed {
			out = append(out, "✓ endpoint "+r.Endpoint+" removed from the configuration")
		} else if r.InConfig && r.Endpoint != "" {
			out = append(out, "the endpoint "+r.Endpoint+" stays in the configuration (it shows as down; fleet endpoint rm "+r.Endpoint+" removes it)")
		}
	}
	for _, n := range r.Roles {
		out = append(out, "  role: "+strings.TrimSpace(n))
	}
	return out
}

// BigTransfer: from this size a pull over a line that is not the LAN is asked about first.
const BigTransfer = 1 << 30

// LineNote is the warning for a pull of total bytes over li ("" when the line is the LAN or the file is small).
func LineNote(li server.LinkInfo, total int64) string {
	if total < BigTransfer || li.Kind == "lan" || li.Kind == "" {
		return ""
	}
	what := map[string]string{"wan": "the internet", "vpn": "a VPN", "peer": "another fleet, route unknown", "unknown": "the kind of line is not known"}[li.Kind]
	line := fmt.Sprintf("this is %s over the line to %s (%s: %s)", Bytes(total), li.From, li.Route, what)
	if li.LimitMbps > 0 {
		return line + fmt.Sprintf("; the daemon limits pulls to %d Mbit/s (pull_mbps)", li.LimitMbps)
	}
	return line + "; pull_mbps in the configuration limits the speed (now: unlimited)"
}

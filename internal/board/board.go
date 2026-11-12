// Package board renders the tokens/second leaderboard (shared by the TUI and the CLI).
package board

import (
	"fmt"
	"github.com/charmbracelet/x/ansi"
	"sort"
	"strings"

	"github.com/tevfik/fleet-router/internal/server"
)

// Trophy is the small ASCII art shown above the leaderboard.
const Trophy = `   ___________
  '._==_==_=_.'
  .-\:      /-.
 | (|:.     |) |
  '-|:.     |-'
    \::.    /
     '::. .'
       ) (
     _.' '._`

type Row struct {
	Rank   int
	Medal  string
	Name   string
	Node   string
	Model  string
	TPS    float64
	TTFTMS int64
	Err    string
}

var medals = []string{"🥇", "🥈", "🥉"}

// Rows ranks the chat endpoints by tokens/s (measured ones first, descending; unmeasured after).
func Rows(eps []server.EpStatus) []Row {
	var rows []Row
	for _, e := range eps {
		if e.IsEmbed() {
			continue
		}
		rows = append(rows, Row{Name: e.Name, Node: e.Node, Model: e.Model, TPS: e.TPS, TTFTMS: e.TTFTMS, Err: e.BenchErr})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if (rows[i].TPS > 0) != (rows[j].TPS > 0) {
			return rows[i].TPS > 0
		}
		return rows[i].TPS > rows[j].TPS
	})
	rank := 0
	for i := range rows {
		if rows[i].TPS > 0 {
			rank++
			rows[i].Rank = rank
			if rank <= len(medals) {
				rows[i].Medal = medals[rank-1]
			}
		}
	}
	return rows
}

// Max is the highest tokens/s among the rows (for bar scaling).
func Max(rows []Row) float64 {
	m := 0.0
	for _, r := range rows {
		if r.TPS > m {
			m = r.TPS
		}
	}
	return m
}

var partial = []string{"", "▏", "▎", "▍", "▌", "▋", "▊", "▉"}

// Bar draws v/max as a w-cell bar with eighth-block precision, padded with light dots.
func Bar(v, max float64, w int) string {
	if max <= 0 || v <= 0 {
		return strings.Repeat("·", w)
	}
	eighths := int(v / max * float64(w) * 8)
	full, rem := eighths/8, eighths%8
	if full > w {
		full, rem = w, 0
	}
	bar := strings.Repeat("█", full) + partial[rem]
	return bar + strings.Repeat("·", w-full-btoi(rem > 0))
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// RenderLog formats request-log entries as plain lines (newest first when newestFirst is set).
func RenderLog(es []server.LogEntry, newestFirst bool) []string {
	var out []string
	for i := range es {
		e := es[i]
		if newestFirst {
			e = es[len(es)-1-i]
		}
		out = append(out, LogLine(e))
	}
	return out
}

// LogLine is one request: time, role → endpoint, status and the figures that matter when debugging routing.
func LogLine(e server.LogEntry) string {
	who := e.Model
	if e.Endpoint != "" {
		who += " → " + e.Endpoint
	}
	flags := ""
	if e.Attempts > 1 {
		flags += fmt.Sprintf(" ↪%d", e.Attempts) // failover: this many endpoints were tried
	}
	if e.Via != "" {
		flags += " (" + e.Via + ")"
	}
	if e.Remote {
		flags += " ↗"
	}
	dur := fmt.Sprintf("%.1fs", float64(e.DurMS)/1000)
	speed := ""
	if e.TPS > 0 {
		speed = fmt.Sprintf(" ttft %dms %5.1f t/s", e.TTFTMS, e.TPS)
	}
	tok := ""
	if e.In > 0 || e.Out > 0 {
		tok = fmt.Sprintf(" in %d out %d", e.In, e.Out)
	}
	q := ""
	if e.QueueMS >= 500 {
		q = fmt.Sprintf(" queued %.1fs", float64(e.QueueMS)/1000)
	}
	err := ""
	if e.Err != "" {
		err = " " + strings.Join(strings.Fields(e.Err), " ")
	}
	return fmt.Sprintf("%s %s %3d %6s%s%s%s%s%s", e.Time.Format("15:04:05"), PadRight(who, 34), e.Status, dur, speed, tok, q, flags, err)
}

// RenderUsage draws the usage report as a table with a request-share bar per row (standard width).
func RenderUsage(rows []server.UsageRow, by string) []string {
	return RenderUsageWidth(rows, by, 0)
}

// RenderUsageWidth draws the usage report adapted to the terminal width so bar and tps are not clipped.
func RenderUsageWidth(rows []server.UsageRow, by string, termWidth int) []string {
	if len(rows) == 0 {
		return []string{"no traffic recorded in this period"}
	}
	maxReq, w := 0, 0
	hasSource := false
	for _, r := range rows {
		maxReq = max(maxReq, r.Requests)
		w = max(w, len([]rune(r.Key)))
		if r.Source != "" {
			hasSource = true
		}
	}

	// Standard / Wide mode (termWidth == 0 or >= 95):
	if termWidth <= 0 || termWidth >= 95 {
		w = min(max(w, len(by)), 34)
		header := fmt.Sprintf("%s  %7s %5s  %9s %9s  %7s %6s  %s", PadRight(strings.ToUpper(by), w), "REQ", "ERR", "TOK IN", "TOK OUT", "AVG", "TOK/S", "")
		if hasSource {
			header = fmt.Sprintf("%s  %-8s  %7s %5s  %9s %9s  %7s %6s  %s", PadRight(strings.ToUpper(by), w), "SOURCE", "REQ", "ERR", "TOK IN", "TOK OUT", "AVG", "TOK/S", "")
		}
		out := []string{header}
		for _, r := range rows {
			k := r.Key
			if rs := []rune(k); len(rs) > w {
				k = string(rs[:w-1]) + "…"
			}
			tps := "-"
			if r.TPS > 0 {
				tps = fmt.Sprintf("%.1f", r.TPS)
			}
			extra := ""
			if r.Failover > 0 {
				extra = fmt.Sprintf(" ↪%d", r.Failover)
			}
			if hasSource {
				src := r.Source
				if src == "" {
					src = "-"
				}
				out = append(out, fmt.Sprintf("%s  %-8s  %7d %5d  %9s %9s  %6.1fs %6s  %s%s", PadRight(k, w), src, r.Requests, r.Errors,
					Compact(r.In), Compact(r.Out), float64(r.AvgMS)/1000, tps, Bar(float64(r.Requests), float64(maxReq), 16), extra))
			} else {
				out = append(out, fmt.Sprintf("%s  %7d %5d  %9s %9s  %6.1fs %6s  %s%s", PadRight(k, w), r.Requests, r.Errors,
					Compact(r.In), Compact(r.Out), float64(r.AvgMS)/1000, tps, Bar(float64(r.Requests), float64(maxReq), 16), extra))
			}
		}
		return out
	}

	// Compact mode for smaller terminals (< 95 columns, e.g. 70-90):
	// Combines TOK IN/OUT into a single TOKENS column and uses a shorter bar so all metrics and widgets fit!
	w = min(max(w, len(by)), 18)
	barLen := 10
	if termWidth < 75 {
		w = min(max(w, len(by)), 13)
		barLen = 7
	}
	header := fmt.Sprintf("%s  %5s %4s  %8s  %5s %5s  %s", PadRight(strings.ToUpper(by), w), "REQ", "ERR", "TOKENS", "AVG", "TOK/S", "")
	if hasSource {
		header = fmt.Sprintf("%s  %-6s  %5s %4s  %8s  %5s %5s  %s", PadRight(strings.ToUpper(by), w), "SOURCE", "REQ", "ERR", "TOKENS", "AVG", "TOK/S", "")
	}
	out := []string{header}
	for _, r := range rows {
		k := r.Key
		if rs := []rune(k); len(rs) > w {
			k = string(rs[:w-1]) + "…"
		}
		tps := "-"
		if r.TPS > 0 {
			tps = fmt.Sprintf("%.1f", r.TPS)
		}
		extra := ""
		if r.Failover > 0 {
			extra = fmt.Sprintf(" ↪%d", r.Failover)
		}
		totTokens := r.In + r.Out
		if hasSource {
			src := r.Source
			if src == "" {
				src = "-"
			}
			out = append(out, fmt.Sprintf("%s  %-6s  %5d %4d  %8s  %4.1fs %5s  %s%s", PadRight(k, w), src, r.Requests, r.Errors,
				Compact(totTokens), float64(r.AvgMS)/1000, tps, Bar(float64(r.Requests), float64(maxReq), barLen), extra))
		} else {
			out = append(out, fmt.Sprintf("%s  %5d %4d  %8s  %4.1fs %5s  %s%s", PadRight(k, w), r.Requests, r.Errors,
				Compact(totTokens), float64(r.AvgMS)/1000, tps, Bar(float64(r.Requests), float64(maxReq), barLen), extra))
		}
	}
	return out
}

// CombineUsage combines local (excluding hub-tunnel) and hub usage rows into a two-section report
// with a source tag on each row, ending with a combined total row.
func CombineUsage(localRows, hubRows []server.UsageRow) []server.UsageRow {
	var out []server.UsageRow
	var totReq, totErr, totIn, totOut int
	var totMS, totQueue, totGen int64

	for _, r := range localRows {
		r.Source = "local"
		out = append(out, r)
		totReq += r.Requests
		totErr += r.Errors
		totIn += r.In
		totOut += r.Out
		totMS += r.AvgMS * int64(r.Requests)
		totQueue += r.QueueMS * int64(r.Requests)
		if r.TPS > 0 && r.Out > 0 {
			totGen += int64(float64(r.Out) / r.TPS * 1000)
		}
	}

	for _, r := range hubRows {
		r.Source = "hub"
		out = append(out, r)
		totReq += r.Requests
		totErr += r.Errors
		totIn += r.In
		totOut += r.Out
		totMS += r.AvgMS * int64(r.Requests)
		totQueue += r.QueueMS * int64(r.Requests)
		if r.TPS > 0 && r.Out > 0 {
			totGen += int64(float64(r.Out) / r.TPS * 1000)
		}
	}

	if len(out) > 0 {
		total := server.UsageRow{
			Key:      "TOTAL",
			Source:   "combined",
			Requests: totReq,
			Errors:   totErr,
			In:       totIn,
			Out:      totOut,
		}
		if totReq > 0 {
			total.AvgMS = totMS / int64(totReq)
			total.QueueMS = totQueue / int64(totReq)
		}
		if totGen > 0 {
			total.TPS = float64(int(float64(totOut)/(float64(totGen)/1000)*10+0.5)) / 10
		}
		out = append(out, total)
	}

	return out
}

// Compact shortens a count: 1234 → 1.2k, 2500000 → 2.5M.
func Compact(n int) string {
	switch {
	case n >= 999_500: // would print as "1000k"
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 10_000:
		return fmt.Sprintf("%.0fk", float64(n)/1e3)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

// PadRight pads s with spaces to w terminal cells. fmt's %-Ns pads by rune count, which misaligns a table when a
// name holds wide characters (CJK, emoji) or escape sequences.
func PadRight(s string, w int) string {
	if n := ansi.StringWidth(s); n < w {
		return s + strings.Repeat(" ", w-n)
	}
	return s
}

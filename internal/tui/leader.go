package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/tevfik/fleet-router/internal/board"
	"github.com/tevfik/fleet-router/internal/server"
)

// The leaderboard: speed ranking, what was used (today / per role / per day) and top-model cards,
// filterable by machine. Modelled on public model leaderboards, drawn with block characters.

var boardTabs = []string{"Speed", "Usage today", "By role", "Daily"}

// palette for per-model colours in the stacked daily chart (the last one is "everything else").
var modelPalette = []lipgloss.Style{
	lipgloss.NewStyle().Foreground(lipgloss.Color("#7C3AED")),
	lipgloss.NewStyle().Foreground(lipgloss.Color("#06B6D4")),
	lipgloss.NewStyle().Foreground(lipgloss.Color("#F59E0B")),
	lipgloss.NewStyle().Foreground(lipgloss.Color("#10B981")),
	lipgloss.NewStyle().Foreground(lipgloss.Color("#EC4899")),
	lipgloss.NewStyle().Foreground(lipgloss.Color("#3B82F6")),
	lipgloss.NewStyle().Foreground(lipgloss.Color("#6B7280")),
}

// nodeOf extracts the machine from a "node/model" reference.
func nodeOf(ref string) string {
	if i := strings.Index(ref, "/"); i > 0 {
		return ref[:i]
	}
	return ""
}

// boardNodes lists the machine filter chips: "" (all) followed by every node that has endpoints.
func (m model) boardNodes() []string {
	seen := map[string]bool{}
	out := []string{""}
	var nodes []string
	for _, e := range m.st.Endpoints {
		if e.Node != "" && !seen[e.Node] {
			seen[e.Node] = true
			nodes = append(nodes, e.Node)
		}
	}
	sort.Strings(nodes)
	return append(out, nodes...)
}

func (m model) nodeOK(node string) bool { return m.bNode == "" || node == m.bNode }

// boardEps are the chat endpoints that pass the machine filter.
func (m model) boardEps() []server.EpStatus {
	var out []server.EpStatus
	for _, e := range m.st.Endpoints {
		if !e.IsEmbed() && m.nodeOK(e.Node) && (m.filter == "" || epMatches(e, m.filter)) {
			out = append(out, e)
		}
	}
	return out
}

// useRows filters usage rows by machine; Key (or Sub when split is set) holds "node/model".
func (m model) useRows(rows []server.UsageRow, subIsModel bool) []server.UsageRow {
	var out []server.UsageRow
	for _, r := range rows {
		ref := r.Key
		if subIsModel {
			ref = r.Sub
		}
		if (m.bNode == "" || nodeOf(ref) == m.bNode) && wordsMatch(r.Key+" "+r.Sub, m.filter) {
			out = append(out, r)
		}
	}
	return out
}

// weight is the quantity usage is ranked by: tokens when any were counted, else requests.
func weight(r server.UsageRow, tokens bool) int {
	if tokens {
		return r.In + r.Out
	}
	return r.Requests
}

func hasTokens(rows []server.UsageRow) bool {
	for _, r := range rows {
		if r.In+r.Out > 0 {
			return true
		}
	}
	return false
}

func unitName(tokens bool) string {
	if tokens {
		return "tokens"
	}
	return "requests"
}

// ---- cards -------------------------------------------------------------------------------------

type card struct{ title, name, value, note string }

func (m model) boardCards() []card {
	eps := m.boardEps()
	var fast, ttft, ctx *server.EpStatus
	for i := range eps {
		e := &eps[i]
		if e.TPS > 0 && (fast == nil || e.TPS > fast.TPS) {
			fast = e
		}
		if e.TTFTMS > 0 && (ttft == nil || e.TTFTMS < ttft.TTFTMS) {
			ttft = e
		}
		if e.Ctx > 0 && (ctx == nil || e.Ctx > ctx.Ctx) {
			ctx = e
		}
	}
	cs := make([]card, 4)
	for i, t := range []string{"FASTEST", "MOST USED · 7 DAYS", "FIRST TOKEN", "LARGEST CONTEXT"} {
		cs[i] = card{title: t, name: "—", value: " ", note: " "}
	}
	if fast != nil {
		cs[0] = card{"FASTEST", fast.Ref(), fmt.Sprintf("%.0f tok/s", fast.TPS), srcNote(fast.TPSSrc)}
	}
	week := m.useRows(m.bWeek, false)
	tok := hasTokens(week)
	total := 0
	for _, r := range week {
		total += weight(r, tok)
	}
	for _, r := range week { // rows arrive most-requests first; pick by the ranking quantity
		if r.Key != "-" && weight(r, tok) > 0 {
			best := r
			for _, o := range week {
				if o.Key != "-" && weight(o, tok) > weight(best, tok) {
					best = o
				}
			}
			cs[1] = card{cs[1].title, best.Key, board.Compact(weight(best, tok)) + " " + unitName(tok),
				fmt.Sprintf("%.0f%% of all", 100*float64(weight(best, tok))/float64(max(1, total)))}
			break
		}
	}
	if ttft != nil {
		cs[2] = card{cs[2].title, ttft.Ref(), msStr(ttft.TTFTMS), "median, real requests"}
	}
	if ctx != nil {
		cs[3] = card{cs[3].title, ctx.Ref(), fmt.Sprintf("%dk tokens", ctx.Ctx/1024), "configured window"}
	}
	return cs
}

func srcNote(src string) string {
	switch src {
	case "live":
		return "live average"
	case "bench":
		return "from a benchmark"
	}
	return " "
}

func (m model) viewCards() string {
	cs := m.boardCards()
	// Large / Wide terminal mode: 4 rounded boxes side by side
	if m.w >= 96 && (m.h == 0 || m.h >= 28) {
		w := (max(80, m.w) - 4) / len(cs)
		var boxes []string
		for i, c := range cs {
			st := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cMuted).Padding(0, 1).Width(w - 2)
			inner := w - 6
			body := helpS.Render(trunc(fmt.Sprintf("#%d %s", i+1, c.title), inner)) + "\n" +
				normalS.Bold(true).Render(trunc(c.name, inner)) + "\n" +
				accS.Render(trunc(c.value, inner)) + "\n" +
				helpS.Render(trunc(c.note, inner))
			boxes = append(boxes, st.Render(body))
		}
		return "  " + strings.ReplaceAll(lipgloss.JoinHorizontal(lipgloss.Top, boxes...), "\n", "\n  ") + "\n"
	}

	// Compact mode for smaller terminals (80x24 etc.): stylish 1-2 line highlight bar
	var items []string
	icons := []string{"⚡", "🔥", "⏱", "📚"}
	shortTitles := []string{"Fastest", "Most used", "TTFT", "Max ctx"}
	for i, c := range cs {
		if c.name == "—" {
			continue
		}
		val := c.value
		if strings.TrimSpace(val) == "" {
			val = trunc(c.name, 16)
		} else {
			val = fmt.Sprintf("%s (%s)", trunc(c.name, 14), strings.TrimSpace(c.value))
		}
		items = append(items, fmt.Sprintf("%s %s: %s", icons[i], helpS.Render(shortTitles[i]), accS.Render(val)))
	}
	if len(items) == 0 {
		return ""
	}
	if len(items) > 2 && m.w > 0 && m.w < 90 {
		half := (len(items) + 1) / 2
		return "  " + strings.Join(items[:half], "  ·  ") + "\n  " + strings.Join(items[half:], "  ·  ") + "\n"
	}
	return "  " + strings.Join(items, "   ·   ") + "\n"
}

// ---- tabs and filters --------------------------------------------------------------------------

func (m model) viewBoardHeader() string {
	var tabs []string
	for i, t := range boardTabs {
		label := fmt.Sprintf(" %d %s ", i+1, t)
		if i == m.bTab {
			tabs = append(tabs, lipgloss.NewStyle().Bold(true).Foreground(cFg).Background(cPrimary).Render(label))
		} else {
			tabs = append(tabs, helpS.Render(label))
		}
	}
	var chips []string
	for _, n := range m.boardNodes() {
		label := n
		if n == "" {
			label = "all machines"
		}
		if n == m.bNode {
			chips = append(chips, lipgloss.NewStyle().Bold(true).Foreground(cAccent).Render("● "+label))
		} else {
			chips = append(chips, helpS.Render("○ "+label))
		}
	}
	return "  " + strings.Join(tabs, " ") + "\n  " + strings.Join(chips, "  ") + helpS.Render("   n: next machine") + "\n"
}

func (m model) viewBoardBody() string {
	switch m.bTab {
	case 1:
		return m.viewToday()
	case 2:
		return m.viewByRole()
	case 3:
		return m.viewDaily()
	}
	return m.viewSpeed()
}

// shareLines ranks rows by weight: "01 name  bar  share%  amount".
func shareLines(names []string, vals []int, nameW, barW int, unit string, cur, top, end int, marks bool) []string {
	total, maxv := 0, 0
	for _, v := range vals {
		total += v
		maxv = max(maxv, v)
	}
	var out []string
	for i := top; i < end; i++ {
		share := 0.0
		if total > 0 {
			share = 100 * float64(vals[i]) / float64(total)
		}
		medal := "  "
		if i < len(medals) && vals[i] > 0 {
			medal = medals[i]
		}
		style := cSecondS
		if i == 0 {
			style = accS
		}
		line := fmt.Sprintf("%2d %s %-*s %s %5.1f%%  %s", i+1, medal, nameW, trunc(names[i], nameW),
			style.Render(board.Bar(float64(vals[i]), float64(maxv), barW)), share, helpS.Render(board.Compact(vals[i])+" "+unit))
		if marks {
			line = curMark(i == cur) + line
		}
		out = append(out, line)
	}
	return out
}

var medals = []string{"🥇", "🥈", "🥉"}

func (m model) viewToday() string {
	rows := m.useRows(m.bData, false)
	if len(rows) == 0 {
		return helpS.Render("  no traffic in the last 24 hours (refreshes every 3 s)") + "\n"
	}
	tok := hasTokens(rows)
	sort.SliceStable(rows, func(i, j int) bool { return weight(rows[i], tok) > weight(rows[j], tok) })
	names, vals := make([]string, len(rows)), make([]int, len(rows))
	for i, r := range rows {
		names[i], vals[i] = r.Key, weight(r, tok)
	}
	top, end := window(len(rows), m.cur, m.h-m.boardChrome())
	var b strings.Builder
	b.WriteString("  " + groupS.Render("TOP MODELS · LAST 24 HOURS") + helpS.Render("   ranked by "+unitName(tok)) + "\n\n")
	for _, l := range shareLines(names, vals, 26, 22, unitName(tok), m.cur, top, end, true) {
		b.WriteString(l + "\n")
	}
	if end < len(rows) {
		b.WriteString(helpS.Render(fmt.Sprintf("      ↓ %d more", len(rows)-end)) + "\n")
	}
	return b.String()
}

// boardChrome is the number of terminal rows the header, cards, buttons and help take.
func (m model) boardChrome() int {
	n := 14
	if m.showCards() {
		if m.w >= 96 && (m.h == 0 || m.h >= 28) {
			n += 6
		} else {
			n += 3
		}
	}
	return n
}

func (m model) showCards() bool { return m.h == 0 || m.h >= 18 }

// roleLine is one rendered line of the per-role view.
type roleLine struct {
	text   string
	header bool
}

func (m model) roleLines() []roleLine {
	rows := m.useRows(m.bData, true)
	if len(rows) == 0 {
		return nil
	}
	tok := hasTokens(rows)
	var out []roleLine
	for i := 0; i < len(rows); {
		j := i
		total, maxv := 0, 0
		for j < len(rows) && rows[j].Key == rows[i].Key {
			total += weight(rows[j], tok)
			maxv = max(maxv, weight(rows[j], tok))
			j++
		}
		out = append(out, roleLine{groupS.Render(strings.ToUpper(rows[i].Key)) + helpS.Render(fmt.Sprintf("  %s %s", board.Compact(total), unitName(tok))), true})
		grp := rows[i:j]
		sort.SliceStable(grp, func(a, b int) bool { return weight(grp[a], tok) > weight(grp[b], tok) })
		for k, r := range grp {
			if k >= 5 {
				out = append(out, roleLine{helpS.Render(fmt.Sprintf("      … %d more", len(grp)-5)), false})
				break
			}
			share := 100 * float64(weight(r, tok)) / float64(max(1, total))
			out = append(out, roleLine{fmt.Sprintf("  %02d %-26s %s %5.1f%%", k+1, trunc(r.Sub, 26),
				cSecondS.Render(board.Bar(float64(weight(r, tok)), float64(maxv), 22)), share), false})
		}
		i = j
	}
	return out
}

func (m model) viewByRole() string {
	lines := m.roleLines()
	if len(lines) == 0 {
		return helpS.Render("  no role traffic in the last 7 days (refreshes every 3 s)") + "\n"
	}
	var b strings.Builder
	b.WriteString("  " + groupS.Render("MODELS BY ROLE · LAST 7 DAYS") + helpS.Render("   share of each role's usage") + "\n\n")
	top, end := window(len(lines), m.cur, m.h-m.boardChrome())
	for i := top; i < end; i++ {
		b.WriteString(curMark(i == m.cur) + lines[i].text + "\n")
	}
	if end < len(lines) {
		b.WriteString(helpS.Render(fmt.Sprintf("      ↓ %d more", len(lines)-end)) + "\n")
	}
	return b.String()
}

// viewDaily draws one stacked bar per day (last 7), each model in its own colour.
func (m model) viewDaily() string {
	rows := m.useRows(m.bData, true)
	if len(rows) == 0 {
		return helpS.Render("  no traffic in the last 7 days (refreshes every 3 s)") + "\n"
	}
	tok := hasTokens(rows)
	perDay := map[string]map[string]int{}
	byModel := map[string]int{}
	for _, r := range rows {
		if perDay[r.Key] == nil {
			perDay[r.Key] = map[string]int{}
		}
		perDay[r.Key][r.Sub] += weight(r, tok)
		byModel[r.Sub] += weight(r, tok)
	}
	var models []string
	for k := range byModel {
		models = append(models, k)
	}
	sort.Slice(models, func(i, j int) bool {
		if byModel[models[i]] != byModel[models[j]] {
			return byModel[models[i]] > byModel[models[j]]
		}
		return models[i] < models[j]
	})
	colour := map[string]int{}
	for i, k := range models {
		colour[k] = min(i, len(modelPalette)-1)
	}
	maxDay := 0
	for _, mm := range perDay {
		t := 0
		for _, v := range mm {
			t += v
		}
		maxDay = max(maxDay, t)
	}
	width := max(20, min(60, m.w-34))
	var b strings.Builder
	b.WriteString("  " + groupS.Render("DAILY USAGE BY MODEL") + helpS.Render("   "+unitName(tok)+" per day, last 7 days") + "\n\n")
	for d := 6; d >= 0; d-- {
		day := time.Now().AddDate(0, 0, -d).Format("2006-01-02")
		mm := perDay[day]
		total := 0
		for _, v := range mm {
			total += v
		}
		bar := helpS.Render(strings.Repeat("·", width))
		if total > 0 {
			cells := max(1, int(float64(total)/float64(maxDay)*float64(width)+0.5))
			bar = stackBar(models, mm, colour, total, cells) + helpS.Render(strings.Repeat("·", width-cells))
		}
		fmt.Fprintf(&b, "  %s %s %s\n", helpS.Render(day[5:]), bar, helpS.Render(board.Compact(total)))
	}
	b.WriteString("\n")
	var legend []string
	for i, k := range models {
		if i >= 6 {
			legend = append(legend, modelPalette[6].Render("■ other"))
			break
		}
		legend = append(legend, modelPalette[colour[k]].Render("■ ")+helpS.Render(trunc(k, 22)))
	}
	b.WriteString("  " + strings.Join(legend, "  ") + "\n")
	return b.String()
}

// stackBar splits `cells` block cells between models in proportion to their value (largest remainder).
func stackBar(models []string, vals map[string]int, colour map[string]int, total, cells int) string {
	type seg struct {
		idx, n int
		frac   float64
	}
	var segs []seg
	used := 0
	for _, k := range models {
		if vals[k] <= 0 {
			continue
		}
		exact := float64(vals[k]) / float64(total) * float64(cells)
		n := int(exact)
		segs = append(segs, seg{colour[k], n, exact - float64(n)})
		used += n
	}
	order := make([]int, len(segs))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return segs[order[a]].frac > segs[order[b]].frac })
	for i := 0; used < cells && i < len(order); i++ {
		segs[order[i]].n++
		used++
	}
	var b strings.Builder
	for _, s := range segs {
		b.WriteString(modelPalette[s.idx].Render(strings.Repeat("█", s.n)))
	}
	return b.String()
}

// wordsMatch: every word of the search text occurs in hay (case-insensitive); an empty search matches.
func wordsMatch(hay, filter string) bool {
	hay = strings.ToLower(hay)
	for _, w := range strings.Fields(strings.ToLower(filter)) {
		if !strings.Contains(hay, w) {
			return false
		}
	}
	return true
}

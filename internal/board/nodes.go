package board

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/hostinfo"
	"github.com/tevfik/fleet-router/internal/plan"
	"github.com/tevfik/fleet-router/internal/server"
)

// Spark draws the last w values as a sparkline scaled to top (top <= 0: to the largest value).
func Spark(vals []float64, top float64, w int) string {
	if len(vals) > w {
		vals = vals[len(vals)-w:]
	}
	if top <= 0 {
		for _, v := range vals {
			top = max(top, v)
		}
	}
	if top <= 0 {
		top = 1
	}
	ticks := []rune("▁▂▃▄▅▆▇█")
	var b strings.Builder
	for _, v := range vals {
		i := int(v / top * float64(len(ticks)-1))
		b.WriteRune(ticks[min(max(i, 0), len(ticks)-1)])
	}
	return b.String()
}

func sparkInts(vals []int64, w int) string {
	f := make([]float64, len(vals))
	for i, v := range vals {
		f[i] = float64(v)
	}
	return Spark(f, 0, w)
}

// Bytes formats a size (GiB from 1 GiB up, else MiB).
func Bytes(n int64) string {
	switch {
	case n < 0:
		return "?"
	case n >= 1<<40:
		return fmt.Sprintf("%.1f TiB", float64(n)/(1<<40))
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	}
	return fmt.Sprintf("%d MiB", n>>20)
}

func pct(used, total int64) float64 {
	if total <= 0 || used < 0 {
		return -1
	}
	return 100 * float64(used) / float64(total)
}

func pctStr(v float64) string {
	if v < 0 {
		return "—"
	}
	return fmt.Sprintf("%.0f%%", v)
}

func tempStr(c float64) string {
	if c < 0 {
		return ""
	}
	return fmt.Sprintf("%.0f°C", c)
}

// Uptime formats seconds as "3d 4h" / "5h 12m" / "12m".
func Uptime(s int64) string {
	d := time.Duration(s) * time.Second
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
	case d >= time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}

// Age formats how long ago t was ("now", "5m ago", "3h ago", "2d ago").
func Age(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

// cpuTemp is the CPU package temperature, else the board's (machines without a CPU sensor); -1: unknown.
func cpuTemp(s hostinfo.Sample) float64 {
	board := -1.0
	for _, t := range s.Temps {
		switch t.Name {
		case "cpu":
			return t.C
		case "board":
			board = t.C
		}
	}
	return board
}

func cpuTempLabel(s hostinfo.Sample) string {
	for _, t := range s.Temps {
		if t.Name == "cpu" {
			return tempStr(t.C)
		}
		if t.Name == "board" {
			return "board " + tempStr(t.C)
		}
	}
	return ""
}

// SuggestLine is one placement suggestion.
func SuggestLine(x server.Suggestion) string {
	return fmt.Sprintf("%s: ~%.0f tok/s on %s (%s) → ~%.0f on %s (%s)", x.Model, x.FromTPS, x.From, x.FromWhere, x.ToTPS, x.To, x.ToWhere)
}

// ModelMark says whether the model is loaded right now ("◆" loaded, "◇" cold, " " the runtime does not say).
func ModelMark(e server.EpStatus) string {
	if e.Info == nil || e.Info.Loaded == nil {
		return " "
	}
	if *e.Info.Loaded {
		return "◆"
	}
	return "◇"
}

// StrainLines say why a node is struggling (routing prefers others) and what is worth knowing.
func StrainLines(ns *server.NodeStrain) []string {
	if ns == nil {
		return nil
	}
	var out []string
	for _, r := range ns.Reasons {
		out = append(out, "  STRAIN "+r+" — routing prefers other machines")
	}
	for _, w := range ns.Warnings {
		out = append(out, "  WARN  "+w)
	}
	return out
}

// gpuSummary: mean load, hottest card, total VRAM use (-1 where unknown).
func gpuSummary(gs []hostinfo.GPU) (util, temp, vram float64) {
	util, temp, vram = -1, -1, -1
	var n, used, total float64
	for _, g := range gs {
		if g.Util >= 0 {
			util, n = max(util, 0)+g.Util, n+1
		}
		temp = max(temp, g.Temp)
		if g.MemUsed >= 0 && g.MemTotal > 0 {
			used, total = used+float64(g.MemUsed), total+float64(g.MemTotal)
		}
	}
	if n > 0 {
		util /= n
	}
	if total > 0 {
		vram = 100 * used / total
	}
	return
}

// ScoreCell is the AI score of a node's machine for its list row ("…" while it is measured, "—" unknown).
func ScoreCell(r server.HostReport) string {
	switch {
	case r.Bench != nil:
		return fmt.Sprint(r.Bench.Score)
	case r.Benching:
		return "…"
	}
	return "—"
}

// BenchLines describe the machine benchmark: the score and what it is made of, then where each model size runs.
func BenchLines(r server.HostReport, self bool) []string {
	return BenchLinesCal(r, self, nil)
}

// BenchLinesCal is BenchLines with the calibration from the machine's real speeds, when known.
func BenchLinesCal(r server.HostReport, self bool, cal *server.Calibration) []string {
	b := r.Bench
	if b == nil {
		switch {
		case r.Benching:
			return []string{"  AI    benchmarking this machine…"}
		case r.Err != "":
			return nil
		case self:
			return []string{"  AI    not measured yet — press r (fleet nodes bench)"}
		}
		return []string{"  AI    not measured — its fleet does that itself (when its hardware changes, or fleet nodes bench there)"}
	}
	again := ""
	if r.Benching {
		again = " · measuring again…"
	} else if self {
		again = " · r to measure again"
	}
	out := []string{fmt.Sprintf("  AI    score %d = %.0f tok/s on an 8B Q4 model × √(%gB fits in %s / 8B) · measured %s%s",
		b.Score, b.Speed8B, b.MaxFastB, b.FastWhere, Age(b.At), again)}
	if cal != nil {
		out = append(out, fmt.Sprintf("        calibrated ≈ %d: its models run ×%.2f the estimate (median of %d with live speeds)", cal.Score, cal.Factor, cal.Models))
	}
	gpu := ""
	for _, g := range b.GPUs {
		if g.Bandwidth > 0 {
			src := g.Source
			if src == "" {
				src = "spec"
			}
			gpu += fmt.Sprintf(" · %s %.0f GB/s (%s)", g.Name, g.Bandwidth, src)
		}
	}
	out = append(out, fmt.Sprintf("        RAM %.0f GB/s (measured) · CPU %.0f GFLOPS (Go, scalar)%s", b.RAMBW, b.CPUGFLOPS, gpu))
	var fits []string
	for _, f := range b.Fits {
		if f.Where == "" {
			fits = append(fits, f.Size+" ✗")
			continue
		}
		fits = append(fits, fmt.Sprintf("%s %s ~%.0f", f.Size, f.Where, f.TPS))
	}
	out = append(out, "  RUNS  "+strings.Join(fits, " · ")+"  (dense Q4, tok/s)")
	for _, n := range b.Notes {
		out = append(out, "  note: "+n)
	}
	return out
}

// NodeCells are the short figures of a node's list row: CPU %, RAM %, GPU %, VRAM %, hottest temperature.
func NodeCells(r server.HostReport) (cpu, ram, gpu, vram, temp string) {
	if r.Err != "" || r.At.IsZero() {
		return "—", "—", "—", "—", ""
	}
	n := r.Now
	cpu, ram = pctStr(n.CPU), pctStr(pct(n.RAMUsed, n.RAMTotal))
	gu, gt, gv := gpuSummary(n.GPUs)
	gpu, vram = "—", "—"
	if len(n.GPUs) > 0 {
		gpu, vram = pctStr(gu), pctStr(gv)
	}
	t := max(cpuTemp(n), gt)
	return cpu, ram, gpu, vram, tempStr(t)
}

// Rate formats a throughput in MB/s (e.g. "1.2 MB/s", "450 KB/s", "0 B/s").
func Rate(mb float64) string {
	switch {
	case mb < 0:
		return "—"
	case mb >= 1024:
		return fmt.Sprintf("%.1f GB/s", mb/1024)
	case mb >= 1.0:
		return fmt.Sprintf("%.1f MB/s", mb)
	case mb >= 0.001:
		return fmt.Sprintf("%.0f KB/s", mb*1024)
	default:
		return fmt.Sprintf("%.0f B/s", mb*(1<<20))
	}
}

// RateShort formats a rate concisely for table cells (e.g. "1.2M", "45K", "0").
func RateShort(mb float64) string {
	switch {
	case mb < 0:
		return "—"
	case mb >= 1024:
		return fmt.Sprintf("%.1fG", mb/1024)
	case mb >= 10.0:
		return fmt.Sprintf("%.0fM", mb)
	case mb >= 1.0:
		return fmt.Sprintf("%.1fM", mb)
	case mb*1024 >= 0.5:
		return fmt.Sprintf("%.0fK", mb*1024)
	case mb > 0:
		return "<1K"
	default:
		return "0"
	}
}

// NetCell formats instantaneous network throughput (Rx and Tx) for a node row.
func NetCell(r server.HostReport) string {
	if r.Err != "" || r.At.IsZero() || (r.Now.NetRx < 0 && r.Now.NetTx < 0) {
		return "—"
	}
	rx, tx := r.Now.NetRx, r.Now.NetTx
	if rx <= 0.0001 && tx <= 0.0001 {
		return "—"
	}
	return fmt.Sprintf("↓%s ↑%s", RateShort(rx), RateShort(tx))
}

// NodeDetail renders one node: its machine (when its fleet could be asked), how it is reached, and its models.
func NodeDetail(ns server.NodeStatus, r server.HostReport, eps []server.EpStatus, width int) []string {
	return NodeDetailFull(ns, r, eps, width, nil)
}

// NodeDetailFull is NodeDetail with the placement suggestions for the node's models.
func NodeDetailFull(ns server.NodeStatus, r server.HostReport, eps []server.EpStatus, width int, sugg []server.Suggestion) []string {
	var out []string
	add := func(f string, a ...any) { out = append(out, fmt.Sprintf(f, a...)) }
	bar := max(10, min(24, width/5))
	spark := max(10, min(30, width/4))
	head := ns.Node + " · " + ns.Via
	if r.Fleet != "" {
		head += " · fleet " + r.Fleet
		if r.Version != "" {
			head += " " + r.Version
		}
	}
	if r.Uptime > 0 {
		head += " · up " + Uptime(r.Uptime)
	}
	add("%s", head)
	switch {
	case r.Err != "":
		add("  machine: %s", r.Err)
	case r.At.IsZero():
		add("  machine: reading…")
	default:
		st, n, h := r.Static, r.Now, r.History
		cores := fmt.Sprintf("%d threads", st.Threads)
		if st.Cores > 0 {
			cores = fmt.Sprintf("%d cores / %d threads", st.Cores, st.Threads)
		}
		machine := ""
		if st.Machine != "" {
			machine = st.Machine + " · "
		}
		add("  %s%s · %s · %s", machine, st.OS, nonEmpty(st.CPUName, "CPU"), cores)
		load := ""
		if n.Load1 >= 0 {
			load = fmt.Sprintf("   load %.2f", n.Load1)
		}
		add("  CPU   %-*s %5s%s   %s", spark, Spark(h["cpu"], 100, spark), pctStr(n.CPU), load, cpuTempLabel(n))
		swap := ""
		if n.SwapTotal > 0 {
			swap = fmt.Sprintf("   swap %s / %s", Bytes(n.SwapUsed), Bytes(n.SwapTotal))
		}
		add("  RAM   %s %s / %s (%s)%s", Bar(max(0, pct(n.RAMUsed, n.RAMTotal)), 100, bar), Bytes(n.RAMUsed), Bytes(n.RAMTotal), pctStr(pct(n.RAMUsed, n.RAMTotal)), swap)
		if len(n.GPUs) == 0 {
			add("  GPU   none found (nvidia-smi / amdgpu)")
		}
		for i, g := range n.GPUs {
			label := "  GPU   "
			if len(n.GPUs) > 1 {
				label = fmt.Sprintf("  GPU%-2d ", g.Index)
			}
			add("%s%s", label, g.Name)
			line := fmt.Sprintf("        %5s", pctStr(g.Util))
			if i == 0 {
				line = fmt.Sprintf("        %-*s %5s", spark, Spark(h["gpu"], 100, spark), pctStr(g.Util))
			}
			if g.MemTotal > 0 && g.MemUsed >= 0 {
				line += fmt.Sprintf("   VRAM %s %s / %s", Bar(pct(g.MemUsed, g.MemTotal), 100, bar/2), Bytes(g.MemUsed), Bytes(g.MemTotal))
			} else {
				line += "   VRAM shared with RAM"
			}
			if t := tempStr(g.Temp); t != "" {
				line += "   " + t
			}
			if g.Power >= 0 {
				line += fmt.Sprintf("   %.0f W", g.Power)
				if g.PowerLimit > 0 {
					line += fmt.Sprintf(" / %.0f W", g.PowerLimit)
				}
			}
			add("%s", line)
		}
		for i, d := range n.Disks {
			label := "  DISK  "
			if i > 0 {
				label = "        "
			}
			add("%s%-12s %s %s / %s (%s)", label, d.Mount, Bar(pct(d.Used, d.Total), 100, bar), Bytes(d.Used), Bytes(d.Total), pctStr(pct(d.Used, d.Total)))
		}
		io := ""
		if n.DiskRead >= 0 {
			io = fmt.Sprintf("read %.1f MB/s · write %.1f MB/s", n.DiskRead, n.DiskWrite)
		}
		var dt []string
		for _, t := range n.Temps {
			if t.Name != "cpu" && t.Name != "board" {
				dt = append(dt, fmt.Sprintf("%s %.0f°C", t.Name, t.C))
			}
		}
		if io != "" || len(dt) > 0 {
			add("  I/O   %s   %s", io, strings.Join(dt, " · "))
		}
		if n.NetRx >= 0 || n.NetTx >= 0 {
			netStr := fmt.Sprintf("rx %s · tx %s", Rate(n.NetRx), Rate(n.NetTx))
			if sp := Spark(h["net_rx"], 0, spark); strings.TrimSpace(sp) != "" {
				add("  NET   %-*s %s", spark, sp, netStr)
			} else {
				add("  NET   %s", netStr)
			}
		}
		for _, note := range r.Notes {
			add("  note: %s", note)
		}
	}
	var cal *server.Calibration
	if c, ok := server.Calibrate(ns.Node, eps, r.Bench); ok {
		cal = &c
	}
	out = append(out, StrainLines(ns.Strain)...)
	out = append(out, BenchLinesCal(r, ns.Self, cal)...)
	ping := "no sample yet"
	if ns.PingMS > 0 {
		ping = fmt.Sprintf("%dms (avg %d, %d–%d) %s", ns.PingMS, ns.AvgMS, ns.MinMS, ns.MaxMS, sparkInts(ns.History, spark))
	}
	if ns.Idle {
		ping += " · tunnel idle"
	}
	add("  PING  %s", ping)
	models := fmt.Sprintf("  MODELS %d/%d up", ns.Up, ns.Total)
	if len(ns.Caps) > 0 {
		models += " · " + strings.Join(ns.Caps, " ")
	}
	if ns.BestTPS > 0 {
		models += fmt.Sprintf(" · best %.0f tok/s (%s)", ns.BestTPS, ns.Best)
	}
	add("%s", models)
	for _, e := range eps {
		if e.Node != ns.Node {
			continue
		}
		m := "✓"
		if !e.Known {
			m = "?"
		} else if !e.OK {
			m = "✗"
		}
		if e.Probe == nil { // the machine's own fleet may have probed it
			if p, ok := r.Probes[e.Model]; ok {
				e.Probe = &p
			}
		}
		add("    %s %s %-34s %s", m, ModelMark(e), trunc(e.Model, 34), ProbeLine(e, ns.Self))
	}
	for _, x := range sugg {
		if x.From == ns.Node {
			add("  MOVE  %s", SuggestLine(x))
		}
	}
	return out
}

// ProbeLine summarises the last capability probe of an endpoint.
func ProbeLine(e server.EpStatus, self bool) string {
	p := e.Probe
	if p == nil {
		if self {
			return "not probed (p)"
		}
		return "not probed (its own fleet probes it)"
	}
	if p.Err != "" {
		return "probe " + Age(p.At) + ": " + p.Err
	}
	parts := []string{}
	if len(p.Found) > 0 {
		parts = append(parts, "✓ "+strings.Join(p.Found, " "))
	}
	for _, c := range p.Missing {
		s := "✗ " + c
		if w := p.Why[c]; w != "" && c != "thinking" {
			s += " (" + trunc(w, 40) + ")"
		}
		parts = append(parts, s)
	}
	if p.TTFTMS > 0 {
		parts = append(parts, fmt.Sprintf("first token %dms", p.TTFTMS))
	}
	if p.TPS > 0 {
		parts = append(parts, fmt.Sprintf("~%.0f tok/s", p.TPS))
	}
	if p.Dim > 0 {
		parts = append(parts, fmt.Sprintf("dim %d", p.Dim))
	}
	s := Age(p.At) + ": " + strings.Join(parts, " · ")
	if p.Differs(&e.Endpoint) {
		s += " · differs from caps (apply: a / fleet probe --apply)"
	}
	return s
}

// SelfNode is the name of this machine's node in a status ("" if unknown).
func SelfNode(st server.Status) string {
	for _, n := range st.Nodes {
		if n.Self {
			return n.Node
		}
	}
	return ""
}

func nonEmpty(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func trunc(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// RouteLines is the table of `fleet route`: the candidates in the order a request would try them, with the
// terms of the cost (seconds) that decide it under the "cost" strategy.
func RouteLines(r server.RouteExplain) []string {
	title := r.Model
	if r.Role != "" {
		title = "role " + r.Role
	}
	l := []string{fmt.Sprintf("%s — strategy %s, a request of %d tokens in / %d out", title, r.Strategy, r.In, r.Out)}
	if len(r.Order) == 0 {
		return append(l, "  no candidates")
	}
	l = append(l, fmt.Sprintf("  %-3s %-18s %-10s %6s %6s %8s %7s %7s  %s", "", "endpoint", "node", "wait", "load", "prompt", "reply", "total", "why"))
	sec := func(v float64) string {
		if v < 0.05 {
			return "—"
		}
		return fmt.Sprintf("%.1fs", v)
	}
	for i, c := range r.Order {
		mark := fmt.Sprintf("%d.", i+1)
		total := sec(c.Total)
		if c.Unknown {
			total = "?"
		}
		l = append(l, fmt.Sprintf("  %-3s %-18s %-10s %6s %6s %8s %7s %7s  %s", mark, c.Endpoint, c.Node, sec(c.Wait), sec(c.Load), sec(c.Prefill), sec(c.Decode), total, strings.Join(c.Notes, ", ")))
	}
	if r.Strategy != "cost" {
		l = append(l, "", "  the order comes from strategy "+r.Strategy+"; the figures show what the cost strategy would weigh (set strategy=cost to route by them)")
	}
	return l
}

// PlanTable is the candidates of `fleet plan`, best first: a header and one line each.
func PlanTable(r plan.Result) []string {
	var l []string
	if len(r.Candidates) > 0 {
		l = append(l, fmt.Sprintf("  %-3s %-8s %-10s %7s %8s %8s %9s  %s", "", "how", "master", "tok/s", "ms/token", "load", "to move", "where the blocks go"))
	}
	for i, c := range r.Candidates {
		var ps []string
		for _, p := range c.Parts {
			if p.Layers == 0 {
				continue
			}
			t := fmt.Sprintf("%s %s", p.Node, p.Blocks())
			if c.Kind == "split" && p.RTTms > 0 {
				t += fmt.Sprintf(" (+%.0fms link)", p.RTTms)
			}
			ps = append(ps, t)
		}
		how := c.Kind
		if c.Kind == "single" {
			how = c.Where
		}
		move := "?"
		if r.Known {
			move = "—"
			if c.FetchGB >= 0.05 {
				move = fmt.Sprintf("%.1f GB", c.FetchGB)
			}
		}
		l = append(l, fmt.Sprintf("  %-3s %-8s %-10s %7.1f %8.1f %8s %9s  %s", fmt.Sprintf("%d.", i+1), how, c.Master, c.TPS, c.TokenMS, loadStr(c.LoadS), move, strings.Join(ps, " · ")))
	}
	return l
}

// PlanHead is the title of a plan and its verdict.
func PlanHead(r plan.Result) (title, verdict string) {
	spec := ""
	if r.SpecDesc != "" {
		spec = " · speculation: " + r.SpecDesc
	}
	return fmt.Sprintf("%s — weights %.1f GB + KV cache %.1f GB (%d slot(s) × %d tokens)%s", r.Model, r.WeightsGB, r.KVGB, r.Np, r.Ctx, spec), "  " + r.Verdict
}

// PlanNotes are the machines left out and the caveat about the estimates.
func PlanNotes(r plan.Result) []string {
	var l []string
	for _, s := range r.Skipped {
		l = append(l, "  left out — "+s)
	}
	for _, s := range r.Warnings {
		l = append(l, "  warning — "+s)
	}
	if !r.Known {
		l = append(l, "  to move: unknown — register the file on its machine (fleet store add) and the transfers are priced from what each machine already holds")
	}
	return append(l, "  estimates: a benchmark's memory speeds and the cost model of docs/distributed.md; the prompt phase and parallel requests are not priced")
}

// PlanLines is the plan as the terminal prints it: title, the table, the verdict, the notes.
func PlanLines(r plan.Result) []string {
	title, verdict := PlanHead(r)
	l := append([]string{title, ""}, PlanTable(r)...)
	l = append(l, "", verdict)
	return append(l, PlanNotes(r)...)
}

func loadStr(s float64) string {
	switch {
	case s < 90:
		return fmt.Sprintf("%.0fs", s)
	default:
		return fmt.Sprintf("%.0fmin", s/60)
	}
}

// StoreRow is one model file in the machines' inventories, and who has it.
type StoreRow struct {
	SHA256  string
	Name    string
	Size    int64
	Kind    string // "mmproj" for a vision projector, "" for a model
	Shard   int    // 1-based, when the header says the file is one shard of a split model
	ShardOf int
	Has     map[string]bool // node → the whole file is there
}

// StoreRows merges the machines' inventories by file content (the SHA-256), by name.
func StoreRows(nodes []server.NodeStore) []StoreRow {
	by := map[string]*StoreRow{}
	for _, n := range nodes {
		if n.Store == nil {
			continue
		}
		for _, f := range n.Store.Files {
			r := by[f.SHA256]
			if r == nil {
				r = &StoreRow{SHA256: f.SHA256, Name: f.Name, Size: f.Size, Kind: f.Kind, Shard: f.Shard, ShardOf: f.ShardOf, Has: map[string]bool{}}
				by[f.SHA256] = r
			}
			r.Has[n.Node] = r.Has[n.Node] || f.Complete
		}
	}
	var out []StoreRow
	for _, r := range by {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].SHA256 < out[j].SHA256
	})
	return out
}

// StoreLines is the table of `fleet store list`: every file with a mark per machine, then the worker caches,
// the hashing that is running and the machines that could not be asked.
func StoreLines(nodes []server.NodeStore) []string {
	rows := StoreRows(nodes)
	var names []string
	for _, n := range nodes {
		names = append(names, n.Node)
	}
	head := fmt.Sprintf("  %-34s %-9s %9s", "FILE", "SHA256", "SIZE")
	for _, n := range names {
		head += fmt.Sprintf(" %-8s", trimTo(n, 8))
	}
	l := []string{head}
	if len(rows) == 0 {
		l = append(l, "  no model files registered — fleet store add <file|directory> (on the machine that has it)")
	}
	for _, m := range StoreModels(rows) {
		name, sha := m.Name, m.Shards[0].SHA256[:min(8, len(m.Shards[0].SHA256))]
		switch {
		case m.Total > 1:
			name = fmt.Sprintf("%s ×%d", m.Name, m.Total)
			sha = "(shards)"
		case m.Kind == "mmproj":
			name = "[mmproj] " + m.Name
		}
		line := fmt.Sprintf("  %-34s %-9s %9s", trimTo(name, 34), sha, Bytes(m.Size))
		for _, n := range nodes {
			cell := "—"
			have := m.Count[n.Node]
			switch {
			case n.Store == nil:
				cell = "?"
			case have == len(m.Shards) && have > 0 && len(m.Shards) >= max(m.Total, 1):
				cell = "✓"
			case have > 0:
				cell = fmt.Sprintf("%d/%d", have, max(m.Total, len(m.Shards)))
			}
			line += fmt.Sprintf(" %-8s", cell)
		}
		if m.Total > 1 && len(m.Shards) < m.Total {
			line += fmt.Sprintf("  ! only %d of %d shards are registered anywhere", len(m.Shards), m.Total)
		}
		l = append(l, line)
	}
	var extra []string
	for _, n := range nodes {
		if n.Store != nil && n.Store.Cache != nil {
			extra = append(extra, fmt.Sprintf("%s %s (%d entries)", n.Node, Bytes(n.Store.Cache.Bytes), n.Store.Cache.Entries))
		}
	}
	if len(extra) > 0 {
		l = append(l, "", "  worker caches: "+strings.Join(extra, " · "))
	}
	l = append(l, TransferLines(nodes)...)
	for _, n := range nodes {
		if n.Store != nil && n.Store.Hashing != nil && n.Store.Hashing.Total > 0 {
			h := n.Store.Hashing
			l = append(l, fmt.Sprintf("  hashing on %s: %s %d%%", n.Node, baseName(h.Path), h.Done*100/h.Total))
		}
		if n.Store != nil && n.Store.Error != "" {
			l = append(l, fmt.Sprintf("  %s: last add failed — %s", n.Node, n.Store.Error))
		}
		if n.Err != "" {
			l = append(l, fmt.Sprintf("  %s: %s", n.Node, n.Err))
		}
	}
	return l
}

func baseName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

func trimTo(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// ResolveFile finds the file a reference (a SHA-256 prefix or a name) means in the machines' inventories.
func ResolveFile(nodes []server.NodeStore, ref string) (string, error) {
	ref = strings.ToLower(strings.TrimSpace(ref))
	if ref == "" {
		return "", fmt.Errorf("say which file: a sha256 prefix or a name (fleet store list)")
	}
	var hit []StoreRow
	for _, r := range StoreRows(nodes) {
		if strings.HasPrefix(r.SHA256, ref) || strings.ToLower(r.Name) == ref {
			hit = append(hit, r)
		}
	}
	switch len(hit) {
	case 0:
		return "", fmt.Errorf("no machine has a file matching %q (fleet store list)", ref)
	case 1:
		return hit[0].SHA256, nil
	}
	return "", fmt.Errorf("%q matches %d files: give more of the sha256", ref, len(hit))
}

// TransferLines lists the transfers of every machine: what is running with its progress, then the latest failures.
func TransferLines(nodes []server.NodeStore) []string {
	var l []string
	for _, n := range nodes {
		if n.Store == nil {
			continue
		}
		for _, t := range n.Store.Transfers {
			switch {
			case t.Running:
				l = append(l, fmt.Sprintf("  %s: %s %s from %s — %s of %s (%d%%)", n.Node, t.Kind, t.What, t.Source, Bytes(t.Done), Bytes(t.Total), t.Done*100/max(t.Total, 1)))
			case t.Err != "":
				l = append(l, fmt.Sprintf("  %s: %s %s from %s failed — %s", n.Node, t.Kind, t.What, t.Source, t.Err))
			}
		}
	}
	return l
}

// ParseGroup reads the words of `fleet role group <endpoint> …` and of the Dashboard's group editor:
// engine=llama-ring workers=a,b stall=90s stats=18191 addrs=a=host:port,b=host:port — or "off" (no words also
// means off: an empty engine) to remove the group.
func ParseGroup(words []string) (g config.Group, stall string, err error) {
	if len(words) == 0 || (len(words) == 1 && words[0] == "off") {
		return config.Group{}, "", nil
	}
	for _, w := range words {
		k, v, ok := strings.Cut(w, "=")
		if !ok {
			return config.Group{}, "", fmt.Errorf("%q: expected engine=… workers=a,b stall=90s stats=PORT addrs=a=host:port (or off)", w)
		}
		switch k {
		case "engine":
			g.Engine = v
		case "workers":
			for _, x := range strings.Split(v, ",") {
				if x = strings.TrimSpace(x); x != "" {
					g.Workers = append(g.Workers, x)
				}
			}
		case "stall":
			stall = v
		case "stats":
			n, e := strconv.Atoi(v)
			if e != nil || n < 0 || n > 65535 {
				return config.Group{}, "", fmt.Errorf("stats=%q is not a port", v)
			}
			g.StatsPort = n
		case "addrs":
			g.Addrs = map[string]string{}
			for _, x := range strings.Split(v, ",") {
				if x = strings.TrimSpace(x); x == "" {
					continue
				}
				node, addr, ok := strings.Cut(x, "=")
				if !ok || node == "" || !strings.Contains(addr, ":") {
					return config.Group{}, "", fmt.Errorf("addrs: %q is not machine=host:port", x)
				}
				g.Addrs[node] = addr
			}
		default:
			return config.Group{}, "", fmt.Errorf("unknown %q (engine, workers, stall, stats, addrs)", k)
		}
	}
	if g.Engine == "" {
		return config.Group{}, "", fmt.Errorf("engine=… is required (llama-ring, prima, llamacpp-rpc)")
	}
	return g, stall, nil
}

// GroupText is ParseGroup's input for an endpoint as it is now (what the editor starts from).
func GroupText(e server.EpStatus) string {
	if e.Group == nil {
		return "engine= workers= stall="
	}
	s := "engine=" + e.Group.Engine + " workers=" + strings.Join(e.Group.Workers, ",")
	if e.Stall != "" {
		s += " stall=" + e.Stall
	}
	if e.Group.StatsPort > 0 {
		s += " stats=" + strconv.Itoa(e.Group.StatsPort)
	}
	if len(e.Group.Addrs) > 0 {
		var a []string
		for n, ad := range e.Group.Addrs {
			a = append(a, n+"="+ad)
		}
		sort.Strings(a)
		s += " addrs=" + strings.Join(a, ",")
	}
	return s
}

// GroupLines says how an attached group looks: members, what the engine reports, why it is set aside.
func GroupLines(e server.EpStatus) []string {
	var out []string
	if e.Paused != "" {
		out = append(out, "⏸ set aside — "+e.Paused)
	}
	if e.Group == nil {
		return out
	}
	head := "group " + e.Group.Engine
	if d := e.StallAfter(); d > 0 {
		head += " · stall limit " + d.String()
	} else {
		head += " · no stall limit"
	}
	out = append(out, head)
	g := e.GroupLive
	if g == nil {
		return append(out, "  not read yet")
	}
	for _, m := range g.Members {
		if m.Up {
			out = append(out, "  ✓ "+m.Node)
		} else {
			out = append(out, "  ✗ "+m.Node+" — "+orDash(m.Err))
		}
	}
	var eng []string
	if g.Health != "" {
		eng = append(eng, "health "+g.Health)
	}
	if g.Slots > 0 {
		eng = append(eng, fmt.Sprintf("%d/%d slots busy", g.Busy, g.Slots))
	}
	if g.Ctx > 0 {
		eng = append(eng, fmt.Sprintf("ctx %d per slot", g.Ctx))
	}
	if g.TPS > 0 {
		eng = append(eng, fmt.Sprintf("%.1f t/s", g.TPS))
	}
	if g.KVUsage > 0 {
		eng = append(eng, fmt.Sprintf("KV %.0f%%", g.KVUsage*100))
	}
	if len(eng) > 0 {
		out = append(out, "  engine: "+strings.Join(eng, " · "))
	}
	if t := g.Trace; t != nil && t.Steps > 0 {
		out = append(out, fmt.Sprintf("  profile (last %d tokens): %.1f ms/token (p50 %.1f) · master's own %.1f ms · through workers %.1f ms", t.Steps, t.TokenMeanMS, t.TokenP50MS, t.LocalMS, t.RingMS))
		for _, n := range t.Nodes {
			name := n.Node
			if name == "" {
				name = n.Addr
			}
			if n.Trusted() {
				out = append(out, fmt.Sprintf("    %s: compute %.1f ms · wire %.1f ms · send %.1f ms", name, n.BusyMS, n.WireMS, n.SendMS))
			} else {
				out = append(out, fmt.Sprintf("    %s: %d samples — too few to split compute from wire", name, n.Samples))
			}
		}
	}
	if t := g.Trace; t != nil {
		if rec := t.ReconnectSummary(e.Group); rec != "" {
			out = append(out, "  "+rec)
		}
	}
	for _, w := range g.Warn {
		out = append(out, "  ⚠ "+w)
	}
	return out
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// runState is the STATE column: "stale?" for an active run that looks dead (fleet run prune --stale marks it lost).
func runState(r server.Run) string {
	switch {
	case r.Active() && r.Stale != "":
		return "stale?"
	case r.Active():
		return "running"
	case r.Lost != "":
		return "lost"
	}
	return "ended"
}

func padRight(s string, w int) string {
	n := utf8.RuneCountInString(s)
	if n >= w {
		return s
	}
	return s + strings.Repeat(" ", w-n)
}

// RunRows is the list of `fleet run`: one line per run, newest first.
func RunRows(runs []server.Run) []string {
	if len(runs) == 0 {
		return []string{"no runs yet — plan a model and record it: fleet plan <file> --record (or --fetch)"}
	}
	idWidth := 9
	for _, r := range runs {
		if w := utf8.RuneCountInString(trunc(r.ID, 24)); w > idWidth {
			idWidth = w
		}
	}
	if idWidth > 24 {
		idWidth = 24
	}
	out := []string{fmt.Sprintf("%s %-8s %-7s %-24s %-20s %-16s %s", padRight("ID", idWidth), "STATE", "ENGINE", "MODEL", "LINE", "SPEED", "FLEET")}
	stale := 0
	for _, r := range runs {
		state := runState(r)
		eng := r.Engine
		if eng == "" || eng == "llama-ring" {
			eng = "ring"
		}
		line := strings.Join(r.Nodes(), "→")
		speed := fmt.Sprintf("plan %.1f t/s", r.PredTPS)
		if m := r.MeasuredTPS(); m > 0 {
			speed = fmt.Sprintf("%.1f t/s (plan %.1f)", m, r.PredTPS)
		} else if r.PredTPS <= 0 {
			speed = "n/a"
		}
		idStr := padRight(trunc(r.ID, 24), idWidth)
		out = append(out, fmt.Sprintf("%s %-8s %-7s %-24s %-20s %-16s %s", idStr, state, trunc(eng, 7), trunc(r.Model, 24), trunc(line, 20), speed, r.Fleet))
		if r.Active() && r.Stale != "" {
			stale++
			out = append(out, fmt.Sprintf("          stale?  %s", r.Stale))
		}
	}
	if stale > 0 {
		out = append(out, fmt.Sprintf("%d run(s) look dead: fleet run prune --stale [--dry-run] marks them lost (it stops nothing); fleet run stop <id> stops its processes and closes it", stale))
	}
	return out
}

// RunLines draws one run as a path: every machine in the order of the line with what it holds, what it costs
// per token, and what comes before and after it; below it the engine's own figures and what happened.
func RunLines(r server.Run, st *server.Status) []string {
	state := "running"
	if !r.Active() {
		state = "ended " + r.Ended.Format("15:04")
		if r.Lost != "" {
			state = "lost " + r.Ended.Format("15:04") + " (" + r.Lost + ")"
		}
	} else if r.Stale != "" {
		state = "stale? (" + r.Stale + ")"
	}
	head := fmt.Sprintf("%s · %s · %s", r.Model, orDash(r.Engine), state)
	if r.Ctx > 0 {
		head += fmt.Sprintf(" · ctx %d×%d", r.Ctx, max(r.Np, 1))
	}
	out := []string{head, fmt.Sprintf("planned by %s at %s · endpoint %s", orDash(r.Fleet), r.At.Format("2006-01-02 15:04"), orDash(r.Endpoint)), ""}
	up := map[string]server.GroupMember{}
	if st != nil {
		for _, e := range st.Endpoints {
			if e.Name == r.Endpoint && e.GroupLive != nil {
				for _, m := range e.GroupLive.Members {
					up[m.Node] = m
				}
			}
		}
	}
	var worst float64
	for _, p := range r.Parts {
		worst = max(worst, p.ComputeMS+p.RTTms+p.SendMS)
	}
	for i, p := range r.Parts {
		mark := " "
		if m, ok := up[p.Node]; ok {
			mark = "✓"
			if !m.Up {
				mark = "✗"
			}
		}
		role := ""
		switch {
		case p.Node == r.Master:
			role = " (master)"
		case p.Output:
			role = " (output)"
		}
		blocks := fmt.Sprintf("blocks %d-%d", p.First, p.First+p.Layers-1)
		if p.Layers == 1 {
			blocks = fmt.Sprintf("block %d", p.First)
		} else if p.Layers <= 0 {
			blocks = "all blocks"
		}
		if p.GB > 0 {
			blocks += fmt.Sprintf(" (%.1f GB)", p.GB)
		}
		cost := p.ComputeMS + p.RTTms + p.SendMS
		out = append(out, fmt.Sprintf(" %s %s%s  %s", mark, p.Node, role, blocks))
		if p.RTTms > 0 || p.SendMS > 0 {
			out = append(out, fmt.Sprintf("     stage: %.1f ms (compute %.1f ms + net %.1f ms) · %s %s", cost, p.ComputeMS, p.RTTms+p.SendMS, Bar(cost, worst, 18), bottleneck(cost, worst)))
		} else {
			out = append(out, fmt.Sprintf("     stage: %.1f ms (compute %.1f ms) · %s %s", cost, p.ComputeMS, Bar(cost, worst, 18), bottleneck(cost, worst)))
		}
		if l := measuredPart(r, p); l != "" {
			out = append(out, "     "+l)
		}
		if m, ok := up[p.Node]; ok && !m.Up {
			out = append(out, "     ✗ "+orDash(m.Err))
		}
		if i < len(r.Parts)-1 {
			next := r.Parts[i+1]
			out = append(out, fmt.Sprintf("     │ to %s: WAN RTT %.1f ms · wire %.1f ms (hidden activation)", next.Node, next.RTTms, next.SendMS), "     ▼")
		} else if len(r.Parts) > 1 {
			out = append(out, "     └──► return loop back to "+r.Master)
		}
	}
	if nodes, _ := latestNodes(r); len(nodes) > 0 { // workers the engine has that the plan did not give blocks
		planned := map[string]bool{}
		for _, p := range r.Parts {
			if p.Node != r.Master {
				planned[p.Node] = true
			}
		}
		for _, n := range nodes {
			if !planned[n.Node] {
				name := orDash(n.Node)
				if n.Node == "" {
					name = n.Addr
				}
				out = append(out, fmt.Sprintf("   ? %s is a worker in the engine but not in the plan: compute %.1f ms · wire %.1f ms", name, n.BusyMS, n.WireMS))
			}
		}
	}
	out = append(out, "")
	var totCompute, totWire float64
	for _, p := range r.Parts {
		totCompute += p.ComputeMS
		totWire += p.RTTms + p.SendMS
	}
	if totCompute+totWire > 0 {
		out = append(out, fmt.Sprintf("pipeline breakdown: compute %.1f ms (%.0f%%) · network hops %.1f ms (%.0f%%)", totCompute, totCompute/(totCompute+totWire)*100, totWire, totWire/(totCompute+totWire)*100))
	}
	if r.PredTPS > 0 {
		exp := fmt.Sprintf("expected %.1f t/s (%.0f ms/token)", r.PredTPS, r.PredTokenMS)
		if m := r.MeasuredTPS(); m > 0 {
			exp += fmt.Sprintf(" · measured %.1f t/s (%+.0f%%)", m, (m/max(r.PredTPS, 0.001)-1)*100)
		} else {
			exp += " · nothing measured yet"
		}
		out = append(out, exp)
	} else if m := r.MeasuredTPS(); m > 0 {
		out = append(out, fmt.Sprintf("measured %.1f t/s", m))
	}
	if n := len(r.Samples); n > 0 {
		l := r.Samples[n-1]
		out = append(out, fmt.Sprintf("engine now: %d/%d slots busy · KV %.0f%% · health %s", l.Busy, l.Slots, l.KV*100, orDash(l.Health)))
		if rec := server.FormatReconnects(l.Reconnects, nil); rec != "" {
			out = append(out, "  "+rec)
		}
	}
	for _, e := range r.Events {
		out = append(out, "  "+e.At.Format("15:04:05")+"  "+e.What)
	}
	return out
}

func bottleneck(cost, worst float64) string {
	if worst > 0 && cost >= worst && cost > 0 {
		return "← slowest (bottleneck)"
	}
	return ""
}

// latestNodes is what the engine's profile said last about the workers (nil: it never did).
func latestNodes(r server.Run) ([]server.RingNode, float64) {
	for i := len(r.Samples) - 1; i >= 0; i-- {
		if len(r.Samples[i].Nodes) > 0 || r.Samples[i].LocalMS > 0 {
			return r.Samples[i].Nodes, r.Samples[i].LocalMS
		}
	}
	return nil, 0
}

// measuredPart sets what the engine measured beside what the plan expected for one machine.
func measuredPart(r server.Run, p server.RunPart) string {
	nodes, local := latestNodes(r)
	if p.Node == r.Master {
		if local <= 0 {
			return ""
		}
		return fmt.Sprintf("measured: own time %.1f ms per token (planned %.1f)", local, p.ComputeMS)
	}
	for _, n := range nodes {
		if n.Node != p.Node {
			continue
		}
		if !n.Trusted() {
			return fmt.Sprintf("measured: too few samples (%d) to say", n.Samples)
		}
		return fmt.Sprintf("measured: compute %.1f ms (planned %.1f) · wire %.1f ms (planned %.1f)", n.BusyMS, p.ComputeMS, n.WireMS, p.RTTms+p.SendMS)
	}
	return ""
}

// FleetPortText is what the Dashboard's fleet-port editor starts from.
func FleetPortText(e server.EpStatus) string {
	if e.FleetPort > 0 {
		return strconv.Itoa(e.FleetPort)
	}
	return ""
}

// PlanExplain lists the items a candidate's time per token is made of, in the cost model of docs/distributed.md:
// T = master's own time (its blocks + a fixed part) + for every worker (compute + wire + send) + the ring overhead.
// The measured figures of `fleet run show` go beside these; --fixed-ms, --rtt and --ring-ms correct them.
func PlanExplain(c plan.Candidate, n int) []string {
	out := []string{fmt.Sprintf("candidate %d — %.1f ms per token (%.1f tok/s)", n, c.TokenMS, c.TPS)}
	var masterBlocks float64
	for _, p := range c.Parts {
		if p.Node == c.Master {
			masterBlocks = p.ComputeMS
		}
	}
	out = append(out, fmt.Sprintf("  %-12s own blocks %6.1f ms + fixed %4.1f ms (embedding, sampling, graph)   = %6.1f ms  [master]", c.Master, masterBlocks, c.MasterLocalMS-masterBlocks, c.MasterLocalMS))
	sum := c.MasterLocalMS
	for _, p := range c.Parts {
		if p.Node == c.Master || p.Layers == 0 {
			continue
		}
		t := p.ComputeMS + p.RTTms + p.SendMS
		sum += t
		out = append(out, fmt.Sprintf("  %-12s compute %6.1f ms + wire %5.1f ms + send %4.1f ms                       = %6.1f ms", p.Node, p.ComputeMS, p.RTTms, p.SendMS, t))
	}
	if c.RingMS > 0 {
		sum += c.RingMS
		out = append(out, fmt.Sprintf("  %-12s per-token overhead of the ring (--ring-ms)                         = %6.1f ms", "ring", c.RingMS))
	}
	out = append(out, fmt.Sprintf("  %-12s %58s %6.1f ms", "total", "", sum))
	return out
}

// ModelRow is one model of the machines' inventories: a single file, or all the shards (name-00001-of-0000N.gguf) of
// a split one taken as one model.
type ModelRow struct {
	Name   string // for a split model, the name of its first file without the shard part
	Kind   string
	Shards []StoreRow     // in shard order (one entry for a single file)
	Total  int            // how many shards the name says there are (1 for a single file)
	Size   int64          // all the registered shards
	Count  map[string]int // node → how many of Shards it has complete
}

// shardOf says which shard (1-based) of how many a file name is, and the name the whole model goes by.
func shardOf(name string) (base string, idx, total int) {
	names := plan.ShardNames(name)
	if len(names) < 2 {
		return name, 0, 1
	}
	for i, n := range names {
		if n == name {
			idx = i + 1
		}
	}
	base = names[0]
	if k := strings.LastIndex(base, "-of-"); k > 0 {
		base = base[:k]
		for len(base) > 0 && (base[len(base)-1] >= '0' && base[len(base)-1] <= '9') {
			base = base[:len(base)-1]
		}
		base = strings.TrimRight(base, "-_.")
	}
	return base, idx, len(names)
}

// rowShard: the header's split.no / split.count when the entry carries them, else the shard part of the file name.
func rowShard(r StoreRow) (base string, idx, total int) {
	if r.ShardOf > 1 {
		return r.Name, r.Shard, r.ShardOf
	}
	return shardOf(r.Name)
}

// StoreModels groups the file rows into models: the shards of a split model become one row.
func StoreModels(rows []StoreRow) []ModelRow {
	by := map[string]*ModelRow{}
	var order []string
	for _, r := range rows {
		base, _, total := rowShard(r)
		key := base
		if r.ShardOf > 1 {
			key = fmt.Sprintf("%s|%d", base, total)
		}
		if total <= 1 {
			key = r.SHA256
		}
		m := by[key]
		if m == nil {
			m = &ModelRow{Name: base, Kind: r.Kind, Total: total, Count: map[string]int{}}
			by[key] = m
			order = append(order, key)
		}
		m.Shards = append(m.Shards, r)
		m.Size += r.Size
		for n, ok := range r.Has {
			if ok {
				m.Count[n]++
			}
		}
	}
	var out []ModelRow
	for _, k := range order {
		m := by[k]
		sort.SliceStable(m.Shards, func(i, j int) bool {
			if m.Shards[i].Shard != m.Shards[j].Shard {
				return m.Shards[i].Shard < m.Shards[j].Shard
			}
			return m.Shards[i].Name < m.Shards[j].Name
		})
		out = append(out, *m)
	}
	return out
}

// ResolveModel finds the files a reference (a sha256 prefix, a file name, or the name of a split model) means: every
// shard of a split model, so that a fetch brings the whole model. It fails when a shard is not registered anywhere.
func ResolveModel(nodes []server.NodeStore, ref string) ([]StoreRow, error) {
	rows := StoreRows(nodes)
	low := strings.ToLower(strings.TrimSpace(ref))
	if low == "" {
		return nil, fmt.Errorf("say which file: a sha256 prefix or a name (fleet store list)")
	}
	models := StoreModels(rows)
	var hit []ModelRow // the models with a file that matches
	for _, m := range models {
		for _, r := range m.Shards {
			n := strings.ToLower(r.Name)
			if strings.HasPrefix(r.SHA256, low) || n == low || n+".gguf" == low || strings.TrimSuffix(low, ".gguf") == n {
				hit = append(hit, m)
				break
			}
		}
		if m.Total > 1 && (strings.ToLower(m.Name) == low || strings.ToLower(m.Name+".gguf") == low) {
			hit = append(hit, m)
		}
	}
	if len(hit) == 0 && len(low) >= 3 { // a unique start of a name (the list shortens long ones)
		for _, m := range models {
			if strings.HasPrefix(strings.ToLower(m.Name), low) {
				hit = append(hit, m)
			}
		}
	}
	switch len(hit) {
	case 0:
		return nil, fmt.Errorf("no machine has a file matching %q (fleet store list)", ref)
	case 1:
	default:
		// a sha prefix may match two shards of one model: that is still one model
		if hit[0].Name == hit[len(hit)-1].Name && hit[0].Total > 1 {
			hit = hit[:1]
		} else {
			return nil, fmt.Errorf("%q matches %d files: give more of the sha256", ref, len(hit))
		}
	}
	m := hit[0]
	if m.Total > 1 && len(m.Shards) < m.Total {
		return m.Shards, fmt.Errorf("%s is split in %d files but only %d are registered anywhere: register the rest (fleet store add) before fetching", m.Name, m.Total, len(m.Shards))
	}
	return m.Shards, nil
}

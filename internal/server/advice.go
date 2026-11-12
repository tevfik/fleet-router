package server

import (
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/tevfik/fleet-router/internal/hostinfo"
)

// Advice from what is known about the machines (their benchmarks) and the models (their sizes, measured
// speeds): where a model would run much faster, and how far the benchmark's estimates are from what the
// models really do on each machine (calibration).

// Suggestion: the model runs on From; To would run it much faster.
type Suggestion struct {
	Model     string  `json:"model"`
	From      string  `json:"from"`
	FromTPS   float64 `json:"from_tps"`
	FromWhere string  `json:"from_where"` // "GPU", "GPU+RAM", "RAM" (estimated), "measured"
	To        string  `json:"to"`
	ToTPS     float64 `json:"to_tps"`
	ToWhere   string  `json:"to_where"`
}

// paramsB reads "7.5B" / "74B" / "1.2T" as billions of parameters (0 if unknown).
func paramsB(s string) float64 {
	s = strings.TrimSpace(strings.ToUpper(s))
	mul := 1.0
	switch {
	case strings.HasSuffix(s, "T"):
		mul, s = 1000, strings.TrimSuffix(s, "T")
	case strings.HasSuffix(s, "B"):
		s = strings.TrimSuffix(s, "B")
	case strings.HasSuffix(s, "M"):
		mul, s = 0.001, strings.TrimSuffix(s, "M")
	default:
		return 0
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v * mul
}

// weights returns a model's size and what one token reads, in GB (0, 0 when its size is unknown).
func weights(e EpStatus) (gb, readGB float64) {
	if e.Info == nil || e.Info.Size <= 0 {
		return 0, 0
	}
	gb = float64(e.Info.Size) / (1 << 30)
	readGB = gb
	active := e.Info.Active
	if active == 0 {
		active = activeParams(e.Model)
	}
	if total := paramsB(e.Info.Params); active > 0 && total > active {
		readGB = gb * active / total
	}
	return gb, readGB
}

func measuredTPS(e EpStatus) float64 {
	if e.TPSSrc == "live" && e.Samples >= 3 {
		return e.TPS
	}
	return 0
}

// Suggest lists models that another known machine would run at least 1.5× (and 3 tok/s) faster, best first.
// Machines that already have the model are not suggested.
func Suggest(eps []EpStatus, hosts map[string]HostReport) []Suggestion {
	has := map[string]map[string]bool{} // node → models it has
	for _, e := range eps {
		if has[e.Node] == nil {
			has[e.Node] = map[string]bool{}
		}
		has[e.Node][normModel(e.Model)] = true
	}
	var nodes []string
	for n, h := range hosts {
		if h.Bench != nil {
			nodes = append(nodes, n)
		}
	}
	sort.Strings(nodes)
	best := map[string]Suggestion{} // model@node → best move
	for _, e := range eps {
		gb, read := weights(e)
		h, ok := hosts[e.Node]
		if gb == 0 || !ok || h.Bench == nil {
			continue
		}
		cur, where := measuredTPS(e), "measured"
		if cur == 0 {
			where, cur = h.Bench.Estimate(gb, read)
			if where == "" {
				where = "does not fit"
			}
		}
		for _, n := range nodes {
			if n == e.Node || has[n][normModel(e.Model)] {
				continue
			}
			to, tps := hosts[n].Bench.Estimate(gb, read)
			if to == "" || tps < 1.5*cur || tps-cur < 3 {
				continue
			}
			k := e.Model + "@" + e.Node
			if b, ok := best[k]; !ok || tps > b.ToTPS {
				best[k] = Suggestion{Model: e.Model, From: e.Node, FromTPS: round1(cur), FromWhere: where, To: n, ToTPS: round1(tps), ToWhere: to}
			}
		}
	}
	out := make([]Suggestion, 0, len(best))
	for _, s := range best {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		gi, gj := out[i].ToTPS/math.Max(out[i].FromTPS, 0.1), out[j].ToTPS/math.Max(out[j].FromTPS, 0.1)
		if gi != gj {
			return gi > gj
		}
		return out[i].Model < out[j].Model
	})
	return out
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

// Calibration compares the benchmark's estimates with what the machine's models really do.
type Calibration struct {
	Factor float64 `json:"factor"` // median measured / estimated tokens/s
	Models int     `json:"models"` // how many models the factor rests on
	Score  int     `json:"score"`  // the AI score times the factor
}

// Calibrate works out the factor for a node from its models with live speed figures and a known size.
func Calibrate(node string, eps []EpStatus, b *hostinfo.Bench) (Calibration, bool) {
	if b == nil {
		return Calibration{}, false
	}
	var ratios []float64
	for _, e := range eps {
		if e.Node != node {
			continue
		}
		m := measuredTPS(e)
		gb, read := weights(e)
		if m == 0 || gb == 0 {
			continue
		}
		if _, est := b.Estimate(gb, read); est > 0 {
			ratios = append(ratios, m/est)
		}
	}
	if len(ratios) == 0 {
		return Calibration{}, false
	}
	sort.Float64s(ratios)
	f := ratios[len(ratios)/2]
	if len(ratios)%2 == 0 {
		f = (ratios[len(ratios)/2-1] + ratios[len(ratios)/2]) / 2
	}
	f = math.Round(f*100) / 100
	return Calibration{Factor: f, Models: len(ratios), Score: int(math.Round(float64(b.Score) * f))}, true
}

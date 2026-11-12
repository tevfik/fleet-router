package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/tevfik/fleet-router/internal/client"
	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/deploy"
	"github.com/tevfik/fleet-router/internal/plan"
	"github.com/tevfik/fleet-router/internal/rtt"
	"github.com/tevfik/fleet-router/internal/server"
)

// Test seams: nothing here may need a network in tests.
var (
	rttProbe rtt.Probe // nil: a TCP handshake to the worker's SSH port
	// splitPlanFn is the cheap, side-effect-free plan `fleet deploy` checks a split against.
	splitPlanFn = defaultSplitPlan
)

// sshAddr is where the round trip to a node is measured: its SSH port, which every fleet worker already has open.
func sshAddr(node string) string {
	h := node
	if i := strings.LastIndex(h, "@"); i >= 0 {
		h = h[i+1:]
	}
	return deploy.ResolveNodeIP(h) + ":22"
}

// measureRTTs measures the round trip from this machine (the Self node of st) to every other node and returns the
// pairs for plan.Options.RTT. The planner (a star through the master) only uses master<->worker round trips, so
// the figures describe plans whose master is this machine; for another master they are left out. explicit pairs
// (--rtt) are never measured and stay authoritative. Lines describe what was done.
func measureRTTs(ctx context.Context, nodes []server.NodeStatus, master string, explicit map[string]float64, samples int, refresh bool) (map[string]float64, []string) {
	self := ""
	for _, n := range nodes {
		if n.Self {
			self = n.Node
		}
	}
	if self == "" {
		return nil, []string{"rtt: this machine is not among the nodes, nothing measured"}
	}
	if master != "" && !strings.EqualFold(master, self) {
		return nil, []string{fmt.Sprintf("rtt: master %s is not this machine (%s): a round trip measured from here is not master<->worker, using the pings (give --rtt to override)", master, self)}
	}
	skip := map[string]bool{}
	for k := range explicit {
		skip[k] = true
	}
	var targets []rtt.Target
	for _, n := range nodes {
		if !n.Self {
			targets = append(targets, rtt.Target{Node: n.Node, Addr: sshAddr(n.Node)})
		}
	}
	c := &rtt.Collector{From: self, Probe: rttProbe, Cache: &rtt.Cache{Path: config.RTTPath()}, Samples: samples, Refresh: refresh, Skip: skip}
	m, detail := c.Collect(ctx, targets)
	keys := make([]string, 0, len(detail))
	for k := range detail {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var lines []string
	for _, k := range keys {
		d := detail[k]
		switch {
		case d.Skipped != "":
			lines = append(lines, fmt.Sprintf("rtt %s: not measured (%s)", k, d.Skipped))
		case d.Cached:
			lines = append(lines, fmt.Sprintf("rtt %s: %.1f ms (measured, median of %d, cached %s ago)", k, d.MS, d.N, time.Since(d.At).Round(time.Second)))
		default:
			lines = append(lines, fmt.Sprintf("rtt %s: %.1f ms (measured now, median of %d, range %.1f-%.1f)", k, d.MS, d.N, d.MinMS, d.MaxMS))
		}
	}
	return m, lines
}

// mergeRTT puts the measured pairs under the explicit ones (--rtt stays authoritative).
func mergeRTT(measured, explicit map[string]float64) map[string]float64 {
	if len(measured) == 0 {
		return explicit
	}
	out := map[string]float64{}
	for k, v := range measured {
		out[k] = v
	}
	for k, v := range explicit {
		out[k] = v
	}
	return out
}

// printCalibration prints a calibration as a suggestion: it never changes anything.
func printCalibration(w io.Writer, r plan.Result, pick int, mm plan.Measured, fixed float64, source string) error {
	n := min(max(pick, 1), len(r.Candidates))
	k, err := plan.Calibrate(r.Candidates[n-1], mm, fixed)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "\ncalibration of candidate %d (%s): predicted %.1f ms/token (local %.1f + ring %.1f, derived), measured %.1f (local %.1f + ring %.1f): the plan was off by %+.0f%%\n",
		n, source, k.PredTokenMS, k.PredLocalMS, k.PredRingMS, mm.TokenMS, mm.LocalMS, mm.RingMS, k.ErrorPct)
	fmt.Fprintf(w, "  the master's own time was %.2fx the prediction; plan again with: --fixed-ms %.1f --ring-ms %.1f\n", k.LocalRatio, k.FixedMS, k.RingMS)
	return nil
}

// calibrateFromLast suggests --fixed-ms/--ring-ms from the newest recorded run that has a RingTrace summary.
// The candidate compared is the plan's one with the same master and machines as the run (else pick).
func calibrateFromLast(w io.Writer, runs []server.Run, r plan.Result, pick int, fixed float64) error {
	run, ok := server.LastMeasuredRun(runs, r.Model)
	if !ok {
		fmt.Fprintln(w, "\ncalibrate-from-last: no recorded run has a ring trace yet (run a ring endpoint recorded with --record/--fetch, let it serve, then ask again)")
		return nil
	}
	mm, n := run.Measured()
	idx := pick
	for i, c := range r.Candidates {
		if sameLine(c, run) {
			idx = i + 1
			break
		}
	}
	src := fmt.Sprintf("run %s, %s, median of %d trace samples", run.ID, run.Model, n)
	if run.Model != r.Model {
		fmt.Fprintf(w, "\ncalibrate-from-last: newest traced run is for another model (%s); the suggestion below is about the ring, not this model\n", run.Model)
	}
	if mm.LocalMS == 0 {
		fmt.Fprintln(w, "\ncalibrate-from-last: the trace has no master-local time, only the token time can be compared")
	}
	return printCalibration(w, r, idx, mm, fixed, src)
}

func sameLine(c plan.Candidate, r server.Run) bool {
	if !strings.EqualFold(c.Master, r.Master) || len(c.Parts) != len(r.Parts) {
		return false
	}
	have := map[string]bool{}
	for _, p := range c.Parts {
		have[strings.ToLower(p.Node)] = true
	}
	for _, p := range r.Parts {
		if !have[strings.ToLower(p.Node)] {
			return false
		}
	}
	return true
}

func defaultSplitPlan(modelPath, master string, ctx, np int) (plan.Result, error) {
	m, err := plan.LoadModel(modelPath)
	if err != nil {
		return plan.Result{}, err
	}
	cl := client.New(listen())
	if !cl.Alive() {
		return plan.Result{}, fmt.Errorf("the daemon is not running")
	}
	return cl.Plan(server.PlanRequest{Model: *m, Ctx: ctx, Np: max(np, 1), Master: master, KVBlocks: m.KVBlocks})
}

// guardSplit runs the plan check for `fleet deploy --workers …`: nothing is started or changed. A check that cannot
// run (no daemon, the GGUF is on another machine) is reported on w and lets the deploy go on: the guard only acts
// on numbers it has.
func guardSplit(w io.Writer, modelPath, master string, workers []string, ctx, np int, force bool) error {
	r, err := splitPlanFn(modelPath, master, ctx, np)
	if err != nil {
		fmt.Fprintf(w, "note: split check skipped (%v)\n", err)
		return nil
	}
	warn, err := deploy.CheckSplit(r, master, workers).Check(force)
	if err != nil {
		return err
	}
	if warn != "" {
		fmt.Fprintf(w, "WARNING: %s\n", warn)
	}
	return nil
}

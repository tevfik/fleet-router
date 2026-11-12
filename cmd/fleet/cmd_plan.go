package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/tevfik/fleet-router/internal/board"
	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/deploy"
	"github.com/tevfik/fleet-router/internal/plan"
	"github.com/tevfik/fleet-router/internal/server"
)

// cmdPlan reads a GGUF file and shows where the model would run best, on one machine or split over several.
func cmdPlan(args []string) error {
	fs := flag.NewFlagSet("plan", flag.ExitOnError)
	ctx := fs.Int("ctx", 0, "context tokens the endpoint will serve (default: the model's, at most 8192)")
	np := fs.Int("np", 1, "parallel slots (each holds its own KV cache)")
	master := fs.String("master", "", "the machine that has the GGUF (default: try every machine)")
	rtt := fs.String("rtt", "", "measured round trips between machines in ms: a>b=3.5,a>c=12 (default: the pings, summed)")
	link := fs.Float64("link-gbps", 0, "network throughput in GB/s between machines (default 0.11, a 1 GbE link)")
	fixed := fs.Float64("fixed-ms", 0, "the master's own time per token besides its blocks (default 1.5)")
	ringMS := fs.Float64("ring-ms", 0, "per-token ring overhead besides the workers' compute, wire and send, in ms (measure it with fleet run show; default 0)")
	target := fs.Float64("target-ms", 0, "token-time gate: mark the candidates slower than this many ms per token (e.g. 33)")
	measured := fs.String("measured", "", "calibrate the chosen candidate (--pick) against a trace: token=114,local=61,ring=55 (ms per token, from fleet run show or /ring/trace); prints the --fixed-ms and --ring-ms that reproduce it")
	noMeasure := fs.Bool("no-measure", false, "do not measure the master<->worker round trips (default: a median of TCP-connect probes from this machine, cached 15 min; --rtt always wins)")
	refreshRTT := fs.Bool("refresh-rtt", false, "measure the round trips again even when a cached value is fresh")
	calLast := fs.Bool("calibrate-from-last", false, "suggest --fixed-ms/--ring-ms from the newest recorded run that has a ring trace (a suggestion only; nothing is applied)")
	explain := fs.Bool("explain", false, "show the items the chosen candidate's time per token is made of")
	balance := fs.Bool("balance", false, "place the blocks to shorten the slowest stage (several streams in flight) instead of filling the fastest machines first; rank by pipeline throughput")
	top := fs.Int("top", 0, "split candidates to show")
	kvBlocks := fs.Int("kv-blocks", 0, "blocks that keep a per-token KV cache (hybrid models; default: from GGUF, else all)")
	cacheK := fs.String("cache-type-k", "", "KV cache data type for K (f16, q8_0, q4_0)")
	cacheV := fs.String("cache-type-v", "", "KV cache data type for V (f16, q8_0, q4_0)")
	cpuMoE := fs.Bool("cpu-moe", false, "model --cpu-moe host-expert offload on single machine")
	moeCacheMiB := fs.Int("moe-cache-mib", 0, "GPU cache size in MiB for host-resident MoE experts")
	boundary := fs.Int64("boundary-bytes", 0, "bytes crossing a machine boundary per token and direction (default: one f32 hidden state)")
	asJSON := fs.Bool("json", false, "print the result as JSON")
	fetch := fs.Bool("fetch", false, "make a candidate runnable: fetch the file to the master and fill the workers' caches (needs a registered file)")
	pick := fs.Int("pick", 1, "with --fetch / --record: the number of the candidate (default: the best)")
	record := fs.Bool("record", false, "record the candidate as a run (fleet run) without fetching anything; --fetch records it too")
	endpoint := fs.String("endpoint", "", "with --record / --fetch: the endpoint that serves it (its engine and group are then watched into the run)")
	emitCmd := fs.Bool("emit-cmd", false, "emit executable master and worker commands for the chosen candidate (--pick)")
	fs.Parse(flagsFirst(args, "ctx", "np", "master", "rtt", "link-gbps", "fixed-ms", "top", "ring-ms", "target-ms", "measured", "kv-blocks", "cache-type-k", "cache-type-v", "moe-cache-mib", "boundary-bytes", "pick", "endpoint", "emit-cmd"))
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: fleet plan <model.gguf | fleet-model.json | model-dir> [--balance] [--ctx N] [--np N] [--master node] [--rtt a>b=ms,…] [--link-gbps X] [--emit-cmd]")
	}
	m, err := plan.LoadModel(fs.Arg(0)) // a GGUF, or a fleet-model.json (a file or the directory that holds it)
	if err != nil {
		return err
	}
	if *emitCmd && m.Runtime == plan.RuntimeStagePipe {
		return fmt.Errorf("--emit-cmd prints ring commands; a stage-pipe model is started with: fleet deploy --engine stage-pipe --dry-run")
	}
	kvB := *kvBlocks
	if kvB <= 0 {
		kvB = m.KVBlocks
	}
	req := server.PlanRequest{
		Balance: *balance, Model: *m, Ctx: *ctx, Np: *np, Master: *master, LinkGBps: *link, FixedMS: *fixed, RingMS: *ringMS, Top: *top,
		KVBlocks: kvB, KVFactor: plan.KVCacheFactor(*cacheK, *cacheV), CacheTypeK: *cacheK, CacheTypeV: *cacheV,
		CPUMoE: *cpuMoE, MoECacheMiB: *moeCacheMiB, BoundaryBytes: *boundary,
	}
	if *rtt != "" {
		req.RTT = map[string]float64{}
		for _, kv := range strings.Split(*rtt, ",") {
			k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
			f, err := strconv.ParseFloat(v, 64)
			if !ok || err != nil || !strings.Contains(k, ">") {
				return fmt.Errorf("--rtt: %q is not a>b=ms", kv)
			}
			req.RTT[k] = f
		}
	}
	cl, err := mustClient()
	if err != nil {
		return err
	}
	if !*noMeasure {
		if st, err := cl.Status(); err == nil {
			self := ""
			for _, n := range st.Nodes {
				if n.Self {
					self = n.Node
					break
				}
			}
			if *master != "" && !strings.EqualFold(*master, self) {
				if hr, err := cl.Host(*master); err == nil && len(hr.RTT) > 0 {
					req.RTT = mergeRTT(hr.RTT, req.RTT)
					fmt.Fprintf(os.Stderr, "rtt: using %d RTT measurement(s) reported by master %s\n", len(hr.RTT), *master)
				} else {
					fmt.Fprintf(os.Stderr, "rtt: master %s is not this machine (%s): a round trip measured from here is not master<->worker, using the pings (give --rtt to override)\n", *master, self)
				}
			} else {
				m, lines := measureRTTs(context.Background(), st.Nodes, *master, req.RTT, 5, *refreshRTT)
				req.RTT = mergeRTT(m, req.RTT)
				for _, l := range lines {
					fmt.Fprintln(os.Stderr, l)
				}
			}
		}
	}
	r, err := cl.Plan(req)
	if err != nil {
		return err
	}
	plan.ApplyTarget(&r, *target)
	if *asJSON {
		b, _ := json.MarshalIndent(r, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	fmt.Println(strings.Join(board.PlanLines(r), "\n"))
	if *measured != "" && len(r.Candidates) > 0 {
		var mm plan.Measured
		for _, kv := range strings.Split(*measured, ",") {
			k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
			f, err := strconv.ParseFloat(v, 64)
			if !ok || err != nil {
				return fmt.Errorf("--measured: %q is not name=ms", kv)
			}
			switch k {
			case "token":
				mm.TokenMS = f
			case "local":
				mm.LocalMS = f
			case "ring":
				mm.RingMS = f
			default:
				return fmt.Errorf("--measured: unknown %q (token, local, ring)", k)
			}
		}
		if err := printCalibration(os.Stdout, r, *pick, mm, *fixed, "--measured"); err != nil {
			return err
		}
	}
	if *calLast && len(r.Candidates) > 0 {
		runs, err := cl.Runs()
		if err != nil {
			return err
		}
		if err := calibrateFromLast(os.Stdout, runs, r, *pick, *fixed); err != nil {
			return err
		}
	}
	if *explain && len(r.Candidates) > 0 {
		n := min(max(*pick, 1), len(r.Candidates))
		fmt.Println("\n" + strings.Join(board.PlanExplain(r.Candidates[n-1], n), "\n"))
	}
	if *emitCmd && len(r.Candidates) > 0 {
		n := min(max(*pick, 1), len(r.Candidates))
		cfg, _ := config.Load(config.Path())
		cmds, err := deploy.CandidateCommands(cfg, fs.Arg(0), r.Candidates[n-1], r.Ctx, r.Np, r.SpecType)
		if err != nil {
			return fmt.Errorf("--cmd: %w", err)
		}
		fmt.Printf("\n# Candidate %d: %s (%.1f tok/s)\n", n, r.Candidates[n-1].Kind, r.Candidates[n-1].TPS)
		if len(cmds.WorkerCmds) > 0 {
			fmt.Println("# Workers:")
			for _, node := range sortedKeys(cmds.WorkerCmds) {
				fmt.Printf("# [%s]: %s\n", node, cmds.WorkerCmds[node])
			}
			fmt.Println()
		}
		fmt.Printf("# Master [%s]:\n", cmds.MasterNode)
		if cmds.MasterEngine != "" {
			fmt.Printf("# engine: %s\n", cmds.MasterEngine)
		}
		if cmds.MasterEnv != "" {
			fmt.Printf("export %s\n", cmds.MasterEnv)
		}
		fmt.Println(cmds.MasterCmd)
	}
	if *fetch {
		fmt.Println()
		if err := cl.FetchPlan(r, *pick, func(s string) { fmt.Fprintln(os.Stderr, s) }); err != nil {
			return err
		}
	}
	if *fetch || *record {
		run, err := cl.RecordRun(r, *pick, *endpoint, "")
		if err != nil {
			return err
		}
		fmt.Printf("recorded as run %s — fleet run show %s\n", run.ID, run.ID)
	}
	return nil
}

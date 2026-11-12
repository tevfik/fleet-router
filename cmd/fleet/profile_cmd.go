package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tevfik/fleet-router/internal/board"
	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/deploy"
	"github.com/tevfik/fleet-router/internal/plan"
	"github.com/tevfik/fleet-router/internal/profile"
	"github.com/tevfik/fleet-router/internal/server"
	"github.com/tevfik/fleet-router/internal/setup"
)

const profileUsage = `usage: fleet profile <command>
  save <name> [<model.gguf>] [deploy flags…]   save a deploy configuration (the same flags as fleet deploy)
  list                                       the saved profiles
  show <name>                                the profile as YAML (with its runs and measurements)
  rm <name>                                  delete it
  edit <name> --set key=value [--set …]      change keys (layout.ts=34,14  spec.type=none  workers=a,b  env.master.K=V; empty value removes)
  export <name> [-o file]                    the YAML to stdout or a file
  import <file> [--name N] [--force]         add a profile from a file
  bench <name> [--reps 3] [--tokens 128] [--url U] [--via M]   measure the last deployed run (an unreachable LAN address goes through ssh to the master) (3 fixed prompts, greedy) and record it
  measure <name> --tps X [--notes T]         record a measurement by hand on the last run
  up <name> [extra deploy flags]             deploy the profile with extra flags (same as fleet deploy --profile <name> [extra flags])
  down <name>                                stop the profile's running runs (same as fleet run stop <id> for each)
  status [name]                              show running/stopped status for all profiles or named one
  autostart <name> on|off                    install/remove systemd user unit to start profile at boot
Deploy one with: fleet deploy --profile <name> [overriding flags] [--explore]`

func cmdProfile(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Println(profileUsage)
		return nil
	}
	store := profileStore()
	sub, rest := args[0], args[1:]
	need := func() (string, error) {
		if len(rest) < 1 || strings.HasPrefix(rest[0], "-") {
			return "", fmt.Errorf("fleet profile %s <name>\n%s", sub, profileUsage)
		}
		return rest[0], nil
	}
	switch sub {
	case "save":
		name, err := need()
		if err != nil {
			return err
		}
		if err := profile.ValidName(name); err != nil {
			return err
		}
		d := newDeployFlags("profile save")
		d.fs.SetOutput(io.Discard)
		if err := d.fs.Parse(flagsFirst(rest[1:], deployValueFlags...)); err != nil {
			return fmt.Errorf("%v\n%s", err, deployUsage)
		}
		if d.fs.NArg() != 1 {
			return fmt.Errorf("fleet profile save <name> <model.gguf> [deploy flags…]")
		}
		p, err := d.profileFromFlags(name, d.fs.Arg(0))
		if err != nil {
			return err
		}
		if old, err := store.Load(name); err == nil {
			p.Runs = old.Runs
		}
		if err := store.Save(p); err != nil {
			return err
		}
		fmt.Printf("✓ profile %s saved (%s, layout %s)\n", name, store.Dir, p.EffectiveMode())
		return nil
	case "list", "ls":
		list, bad, err := store.List()
		if err != nil {
			return err
		}
		if len(list) == 0 && len(bad) == 0 {
			fmt.Println("no profiles yet — fleet profile save <name> <model.gguf> [deploy flags…] (or fleet deploy … --save-profile <name>)")
		}
		for _, p := range list {
			ws := strings.Join(p.Workers, ",")
			if ws == "" {
				ws = "-"
			}
			master := p.Master
			if master == "" {
				master = "(this machine)"
			}
			m := "not measured"
			if r := p.LastMeasured(); r != nil {
				m = fmt.Sprintf("%.1f t/s on %s", r.Measured.TPS, r.Measured.At.Format("2006-01-02"))
			}
			fmt.Printf("%-24s %-6s master %-14s workers %-20s %d run(s), %s  %s\n", p.Name, p.EffectiveMode(), master, ws, len(p.Runs), m, p.Model)
		}
		for n, e := range bad {
			fmt.Printf("%-24s BROKEN: %v\n", n, e)
		}
		return nil
	case "show", "export":
		name, err := need()
		if err != nil {
			return err
		}
		p, err := store.Load(name)
		if err != nil {
			return err
		}
		b, err := p.Marshal()
		if err != nil {
			return err
		}
		out := ""
		if sub == "export" {
			fs := flag.NewFlagSet("profile export", flag.ContinueOnError)
			o := fs.String("o", "", "file to write")
			if err := fs.Parse(rest[1:]); err != nil {
				return err
			}
			out = *o
		}
		if out == "" {
			fmt.Print(string(b))
			return nil
		}
		if err := os.WriteFile(out, b, 0o600); err != nil {
			return err
		}
		fmt.Printf("✓ profile %s written to %s\n", name, out)
		return nil
	case "rm", "remove", "delete":
		name, err := need()
		if err != nil {
			return err
		}
		if err := store.Remove(name); err != nil {
			return err
		}
		fmt.Printf("✓ profile %s removed\n", name)
		if setup.ProfileAutostartEnabled(name) { // its unit would try to deploy a profile that is gone at every boot
			if _, err := setup.ProfileAutostartOff(name); err != nil {
				fmt.Printf("  ! its autostart unit could not be removed: %v (fleet profile autostart %s off)\n", err, name)
			} else {
				fmt.Printf("  its autostart unit %s was removed\n", setup.ProfileUnitName(name))
			}
		}
		return nil
	case "edit", "set":
		name, err := need()
		if err != nil {
			return err
		}
		var sets []string
		fs := flag.NewFlagSet("profile edit", flag.ContinueOnError)
		fs.Func("set", "key=value (repeatable)", func(v string) error { sets = append(sets, v); return nil })
		if err := fs.Parse(rest[1:]); err != nil {
			return err
		}
		sets = append(sets, fs.Args()...) // fleet profile edit x key=value also works
		if len(sets) == 0 {
			return fmt.Errorf("fleet profile edit <name> --set key=value [--set …]")
		}
		p, err := store.Load(name)
		if err != nil {
			return err
		}
		for _, kv := range sets {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				return fmt.Errorf("--set %q: key=value", kv)
			}
			if err := p.Set(k, v); err != nil {
				return err
			}
		}
		if err := checkProfileExtras(p); err != nil {
			return err
		}
		if err := store.Save(p); err != nil {
			return err
		}
		fmt.Printf("✓ profile %s: %s\n", name, strings.Join(sets, ", "))
		return nil
	case "import":
		fs := flag.NewFlagSet("profile import", flag.ContinueOnError)
		name := fs.String("name", "", "save it under this name (default: the name in the file)")
		force := fs.Bool("force", false, "replace an existing profile of that name")
		if err := fs.Parse(flagsFirst(rest, "name")); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return fmt.Errorf("fleet profile import <file> [--name N] [--force]")
		}
		p, err := store.Import(fs.Arg(0), *name, *force)
		if err != nil {
			return err
		}
		fmt.Printf("✓ profile %s imported\n", p.Name)
		return nil
	case "bench":
		return cmdProfileBench(store, rest)
	case "measure":
		name, err := need()
		if err != nil {
			return err
		}
		fs := flag.NewFlagSet("profile measure", flag.ContinueOnError)
		tps := fs.Float64("tps", 0, "measured tokens/s")
		notes := fs.String("notes", "", "what was measured how")
		if err := fs.Parse(rest[1:]); err != nil {
			return err
		}
		if *tps <= 0 {
			return fmt.Errorf("--tps X (> 0)")
		}
		return recordMeasurement(store, name, profile.Measurement{At: time.Now().UTC(), TPS: *tps, Source: "manual", Notes: *notes})
	case "up":
		return profileUp(store, rest)
	case "down":
		name, err := need()
		if err != nil {
			return err
		}
		return profileDown(name)
	case "status":
		return profileStatus(store, rest)
	case "autostart":
		return profileAutostart(store, rest)
	}
	return fmt.Errorf("unknown profile command %q\n%s", sub, profileUsage)
}

// checkProfileExtras validates the env / extra args of a profile the same way a deploy will.
func checkProfileExtras(p *profile.Profile) error {
	d := newDeployFlags("check")
	d.fs.SetOutput(io.Discard)
	if err := d.fs.Parse(profileArgs(p, false)); err != nil {
		return err
	}
	_, err := d.options(p.Model)
	return err
}

func recordMeasurement(store profile.Store, name string, m profile.Measurement) error {
	p, err := store.Load(name)
	if err != nil {
		return err
	}
	if len(p.Runs) == 0 {
		return fmt.Errorf("profile %s has no run yet: deploy it first (fleet deploy --profile %s)", name, name)
	}
	r := &p.Runs[len(p.Runs)-1]
	r.Measured = &m
	if err := store.Save(p); err != nil {
		return err
	}
	fmt.Printf("✓ profile %s, run %s: %.1f t/s recorded\n", name, r.RunID, m.TPS)
	return nil
}

// benchPrompts is the fixed prompt set of `fleet profile bench` (prose, code, structured output), the same kind of
// mix as the engine's ring benchmark script.
var benchPrompts = []string{
	"Write a short story about a lighthouse keeper who finds a message in a bottle.",
	"Write a Python function that returns the n-th Fibonacci number iteratively, with a docstring and two tests.",
	"Return a JSON array of 10 objects with the fields id, name and email for made-up users. Only JSON.",
}

// reachEndpoint is the test seam of the path to a deployed endpoint (direct, else an ssh tunnel).
var reachEndpoint = deploy.ReachEndpoint

var benchHTTP = &http.Client{Timeout: 10 * time.Minute}

// cmdProfileBench measures the profile's last deployed run: each prompt reps times, greedy, a fixed number of
// tokens; the t/s of a prompt is the median of its reps, the run's figure the median over the prompts. The engine's
// trace (GET /ring/trace on the stats port), when there is one, is recorded too: it calibrates the next auto plan.
func cmdProfileBench(store profile.Store, args []string) error {
	fs := flag.NewFlagSet("profile bench", flag.ContinueOnError)
	reps := fs.Int("reps", 3, "times each prompt is run")
	tokens := fs.Int("tokens", 128, "tokens generated per prompt")
	u := fs.String("url", "", "endpoint base (…/v1; default: the last run's)")
	via := fs.String("via", "", "machine whose ssh to tunnel through when the endpoint's address does not answer from here (default: the last run's master)")
	if err := fs.Parse(flagsFirst(args, "reps", "tokens", "url", "via")); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("fleet profile bench <name> [--reps 3] [--tokens 128] [--url U] [--via M]")
	}
	name := fs.Arg(0)
	p, err := store.Load(name)
	if err != nil {
		return err
	}
	if len(p.Runs) == 0 && *u == "" {
		return fmt.Errorf("profile %s has no run yet: deploy it first", name)
	}
	base, stats, master := *u, 0, *via
	if base == "" {
		r := p.Runs[len(p.Runs)-1]
		base, stats = r.URL, r.StatsPort
		if master == "" {
			master = r.Layout.Master
		}
	}
	// The registered URL is the master's LAN address: from outside the LAN go through the master's ssh instead of
	// failing with "connection refused" (which looks like a dead ring).
	rc, err := reachEndpoint(context.Background(), master, base, stats)
	if err != nil {
		return err
	}
	defer rc.Close()
	if rc.Note != "" {
		fmt.Fprintln(os.Stderr, "note:", rc.Note)
	}
	m, err := benchEndpoint(context.Background(), rc.URL, rc.StatsPort, *reps, *tokens, os.Stderr)
	if err != nil {
		return err
	}
	return recordMeasurement(store, name, m)
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

func benchEndpoint(ctx context.Context, base string, statsPort, reps, tokens int, log io.Writer) (profile.Measurement, error) {
	m := profile.Measurement{At: time.Now().UTC(), Source: "bench"}
	base = strings.TrimSuffix(base, "/")
	for i, prompt := range benchPrompts {
		var got []float64
		for r := 0; r < max(reps, 1); r++ {
			tps, err := benchOnce(ctx, base, prompt, tokens)
			if err != nil {
				return m, fmt.Errorf("prompt %d: %w", i+1, err)
			}
			got = append(got, tps)
		}
		med := median(got)
		m.PerPrompt = append(m.PerPrompt, float64(int(med*10+0.5))/10)
		fmt.Fprintf(log, "prompt %d: %.1f t/s (median of %d)\n", i+1, med, len(got))
	}
	m.TPS = float64(int(median(m.PerPrompt)*10+0.5)) / 10
	if statsPort > 0 {
		if pu, err := url.Parse(base); err == nil {
			if tr, err := fetchTrace(ctx, "http://"+net.JoinHostPort(pu.Hostname(), strconv.Itoa(statsPort))); err == nil {
				m.Trace = tr
			}
		}
	}
	return m, nil
}

func benchOnce(ctx context.Context, base, prompt string, tokens int) (float64, error) {
	body, _ := json.Marshal(map[string]any{
		"model": "default", "messages": []map[string]string{{"role": "user", "content": prompt}},
		"max_tokens": tokens, "temperature": 0, "top_k": 1, "seed": 1, "stream": false,
	})
	req, err := http.NewRequestWithContext(ctx, "POST", base+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := benchHTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return 0, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var out struct {
		Usage struct {
			Completion int `json:"completion_tokens"`
		} `json:"usage"`
		Timings struct {
			PredictedPerSecond float64 `json:"predicted_per_second"`
		} `json:"timings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, err
	}
	if out.Timings.PredictedPerSecond > 0 {
		return out.Timings.PredictedPerSecond, nil // the engine's decode rate (prompt processing excluded)
	}
	if out.Usage.Completion <= 0 {
		return 0, fmt.Errorf("the answer has no token count")
	}
	return float64(out.Usage.Completion) / time.Since(start).Seconds(), nil
}

// fetchTrace reads the engine's decode profile into the planner's terms.
func fetchTrace(ctx context.Context, stats string) (*plan.Measured, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", stats+"/ring/trace?last=256", nil)
	if err != nil {
		return nil, err
	}
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var t struct {
		P50   float64 `json:"token_p50_ms"`
		Local float64 `json:"local_ms"`
		Ring  float64 `json:"ring_ms"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&t); err != nil {
		return nil, err
	}
	if t.P50 <= 0 {
		return nil, fmt.Errorf("no decode steps in the trace")
	}
	return &plan.Measured{TokenMS: t.P50, LocalMS: t.Local, RingMS: t.Ring}, nil
}

// cmdConfigNode manages the nodes: section of the configuration (fleet config node …).
func cmdConfigNode(args []string) error {
	usage := "usage: fleet config node list | set <name> [addr=IP] [mem_gib=N] [weight_gbps=N] [expert_gbps=N] [notes=TEXT] | rm <name>"
	if len(args) == 0 {
		args = []string{"list"}
	}
	path := config.Path()
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	switch args[0] {
	case "list", "ls":
		if len(cfg.Nodes) == 0 {
			fmt.Println("no nodes declared (fleet config node set <name> addr=IP mem_gib=N weight_gbps=N)")
		}
		names := make([]string, 0, len(cfg.Nodes))
		for n := range cfg.Nodes {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			s := cfg.Nodes[n]
			fmt.Printf("%-16s addr %-15s mem_gib %-6g weight_gbps %-6g expert_gbps %-6g %s\n", n, s.Addr, s.MemGiB, s.WeightGBps, s.ExpertGBps, s.Notes)
		}
		return nil
	case "set":
		if len(args) < 2 {
			return fmt.Errorf("%s", usage)
		}
		name := args[1]
		if cfg.Nodes == nil {
			cfg.Nodes = map[string]config.NodeSpec{}
		}
		s := cfg.Nodes[name]
		for _, kv := range args[2:] {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				return fmt.Errorf("%q: key=value\n%s", kv, usage)
			}
			num := func() (float64, error) {
				if v == "" {
					return 0, nil
				}
				return strconv.ParseFloat(v, 64)
			}
			var f float64
			switch k {
			case "addr":
				s.Addr = v
			case "notes":
				s.Notes = v
			case "mem_gib", "weight_gbps", "expert_gbps":
				if f, err = num(); err != nil {
					return fmt.Errorf("%s: %v", k, err)
				}
				switch k {
				case "mem_gib":
					s.MemGiB = f
				case "weight_gbps":
					s.WeightGBps = f
				default:
					s.ExpertGBps = f
				}
			default:
				return fmt.Errorf("unknown key %q\n%s", k, usage)
			}
		}
		cfg.Nodes[name] = s
	case "rm", "remove":
		if len(args) < 2 {
			return fmt.Errorf("%s", usage)
		}
		if _, ok := cfg.Nodes[args[1]]; !ok {
			return fmt.Errorf("no node %q in the nodes: section", args[1])
		}
		delete(cfg.Nodes, args[1])
	default:
		return fmt.Errorf("%s", usage)
	}
	if err := cfg.Save(path); err != nil {
		return err
	}
	if cl := clientIfAlive(); cl != nil {
		_ = cl.Reload()
	}
	fmt.Println("✓ nodes: section saved")
	return nil
}

// RunningRunsForProfile returns the run IDs of runs that belong to a profile and are currently running.
// A run belongs to a profile if it's recorded in the profile's Runs list and its state is not ended.
// This is a pure function that can be tested without a daemon.
func RunningRunsForProfile(p *profile.Profile, runs []server.Run) []string {
	profileRunIDs := make(map[string]bool)
	for _, r := range p.Runs {
		if r.RunID != "" {
			profileRunIDs[r.RunID] = true
		}
	}

	var running []string
	for _, r := range runs {
		if profileRunIDs[r.ID] && r.Ended.IsZero() {
			running = append(running, r.ID)
		}
	}
	return running
}

// profileUp deploys a profile with optional extra deploy flags.
func profileUp(store profile.Store, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: fleet profile up <name> [extra deploy flags]")
	}
	name := args[0]
	args = args[1:]

	if _, err := store.Load(name); err != nil { // fail early and clearly on an unknown profile
		return err
	}

	// Build deploy command with --profile flag
	deployArgs := []string{"--profile", name}
	deployArgs = append(deployArgs, args...)

	return cmdDeploy(deployArgs)
}

// profileDown stops all running runs for a profile.
func profileDown(name string) error {
	store := profileStore()
	p, err := store.Load(name)
	if err != nil {
		return err
	}

	cl, err := mustClient()
	if err != nil {
		return err
	}

	runs, err := cl.Runs()
	if err != nil {
		return err
	}

	running := RunningRunsForProfile(p, runs)
	if len(running) == 0 {
		fmt.Printf("%s: not running\n", name)
		return nil
	}

	fmt.Printf("Stopping %d run(s) for profile %s:\n", len(running), name)
	for _, runID := range running {
		rep, err := cl.StopRun(runID, server.StopOptions{})
		if err != nil {
			return err
		}
		fmt.Println(strings.Join(board.StopLines(rep), "\n"))
		if rep.Failed() {
			return fmt.Errorf("stop of run %s failed", runID)
		}
	}
	fmt.Printf("✓ profile %s stopped\n", name)
	return nil
}

// profileStatus shows the status of profiles.
func profileStatus(store profile.Store, args []string) error {
	cl, err := mustClient()
	if err != nil {
		return err
	}

	runs, err := cl.Runs()
	if err != nil {
		return err
	}

	var profiles []*profile.Profile
	var bad map[string]error
	var all bool
	nameArg := ""

	if len(args) == 0 {
		all = true
		profiles, bad, err = store.List()
		if err != nil {
			return err
		}
	} else {
		nameArg = args[0]
		p, err := store.Load(nameArg)
		if err != nil {
			return err
		}
		profiles = append(profiles, p)
	}

	// Print header
	if all && len(profiles) > 0 {
		fmt.Println("STATUS")
	}

	for _, p := range profiles {
		running := RunningRunsForProfile(p, runs)
		if len(running) > 0 {
			// Find the latest run
			var latestRun *server.Run
			for i := range runs {
				if runs[i].ID == running[0] {
					latestRun = &runs[i]
					break
				}
			}

			if latestRun != nil {
				if latestRun.Endpoint != "" {
					fmt.Printf("%s: running run=%s endpoint=%s\n", p.Name, latestRun.ID, latestRun.Endpoint)
				} else {
					fmt.Printf("%s: running run=%s endpoint=(none)\n", p.Name, latestRun.ID)
				}
			} else {
				fmt.Printf("%s: running run=%s (endpoint info unavailable)\n", p.Name, running[0])
			}
		} else {
			fmt.Printf("%s: stopped\n", p.Name)
		}
	}

	for n, e := range bad {
		fmt.Printf("%s: BROKEN: %v\n", n, e)
	}

	return nil
}

// profileAutostart configures systemd user unit to start/stop profile at boot.
func profileAutostart(store profile.Store, args []string) error {
	if len(args) != 2 || (args[1] != "on" && args[1] != "off") {
		return fmt.Errorf("usage: fleet profile autostart <name> on|off")
	}
	name, mode := args[0], args[1]
	if _, err := store.Load(name); err != nil {
		return err
	}

	if mode == "on" {
		exe, err := setup.Exe()
		if err != nil {
			return err
		}
		lines, err := setup.ProfileAutostartOn(exe, name)
		for _, l := range lines {
			if strings.HasPrefix(l, "!") {
				fmt.Println(" ", l)
			} else {
				fmt.Println("✓", l)
			}
		}
		return err
	}

	lines, err := setup.ProfileAutostartOff(name)
	for _, l := range lines {
		fmt.Println("✓", l)
	}
	return err
}

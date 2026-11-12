package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/tevfik/fleet-router/internal/board"
	"github.com/tevfik/fleet-router/internal/feature"
	"github.com/tevfik/fleet-router/internal/server"
)

// cmdRoute shows where a request for a role or model would go and what each candidate would cost.
func cmdRoute(args []string) error {
	fs := flag.NewFlagSet("route", flag.ExitOnError)
	in := fs.Int("in", 0, "prompt tokens of the request (default: a short prompt)")
	out := fs.Int("out", 0, "reply tokens of the request (default 300)")
	fs.Parse(flagsFirst(args, "in", "out"))
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: fleet route <role|model> [--in N] [--out N]")
	}
	cl, err := mustClient()
	if err != nil {
		return err
	}
	r, err := cl.Route(fs.Arg(0), *in, *out)
	if err != nil {
		return err
	}
	fmt.Println(strings.Join(board.RouteLines(r), "\n"))
	return nil
}

// cmdNodes lists the machines (ping, load, memory, GPUs, temperatures), or shows one in full.
func cmdNodes(args []string) error {
	cl, err := mustClient()
	if err != nil {
		return err
	}
	st, err := cl.Status()
	if err != nil {
		return err
	}
	has := func(l []string, x string) bool {
		for _, v := range l {
			if v == x {
				return true
			}
		}
		return false
	}
	if len(args) > 0 && (args[0] == "add" || args[0] == "connect") {
		if len(args) < 2 {
			return fmt.Errorf("usage: fleet nodes add <host|ip|user@host>…")
		}
		cfg, err := cl.Config()
		if err != nil {
			return err
		}
		ssh := cfg.Discover.SSH
		for _, h := range args[1:] {
			h = strings.TrimSpace(h)
			if h != "" && !has(ssh, h) {
				ssh = append(ssh, h)
			}
		}
		if err := cl.SetDiscover(cfg.Discover.Ports, ssh, cfg.Discover.Ignore); err != nil {
			return err
		}
		fmt.Printf("✓ added %d machine(s) to cluster discovery: %s\n", len(args)-1, strings.Join(args[1:], ", "))
		return nil
	}
	if len(args) > 0 && (args[0] == "rm" || args[0] == "remove") {
		if len(args) < 2 {
			return fmt.Errorf("usage: fleet nodes rm <host|ip|user@host>…")
		}
		cfg, err := cl.Config()
		if err != nil {
			return err
		}
		var keep []string
		for _, h := range cfg.Discover.SSH {
			if !has(args[1:], h) {
				keep = append(keep, h)
			}
		}
		if err := cl.SetDiscover(cfg.Discover.Ports, keep, cfg.Discover.Ignore); err != nil {
			return err
		}
		fmt.Printf("✓ removed %d machine(s) from cluster: %s\n", len(args)-1, strings.Join(args[1:], ", "))
		return nil
	}
	if len(st.Nodes) == 0 {
		fmt.Println("no nodes — add models with `fleet discover` or machines with `fleet nodes add <host>`")
		return nil
	}
	if len(args) > 0 && args[0] == "suggest" {
		hosts := map[string]server.HostReport{}
		for _, n := range st.Nodes {
			if r, err := cl.Host(n.Node); err == nil && r.Err == "" {
				hosts[n.Node] = r
			}
		}
		sg := server.Suggest(st.Endpoints, hosts)
		if len(sg) == 0 {
			fmt.Println("nothing to suggest: every model runs about as fast where it is as anywhere else known (needs the machines' benchmarks and the models' sizes)")
			return nil
		}
		for _, x := range sg {
			fmt.Println("  " + board.SuggestLine(x))
		}
		return nil
	}
	if len(args) > 0 && args[0] == "rtt" { // master<->worker round trips from this machine, cached for fleet plan
		if !feature.Runner() {
			return fmt.Errorf("nodes rtt: %s", feature.RunnerHint)
		}
		fs := flag.NewFlagSet("nodes rtt", flag.ExitOnError)
		samples := fs.Int("samples", 5, "probes per node (the median is kept)")
		refresh := fs.Bool("refresh", false, "ignore the cache")
		fs.Parse(flagsFirst(args[1:], "samples"))
		_, lines := measureRTTs(context.Background(), st.Nodes, "", nil, *samples, *refresh)
		for _, l := range lines {
			fmt.Println(l)
		}
		return nil
	}
	if len(args) > 0 && args[0] == "bench" { // this machine only: every fleet measures its own
		if err := cl.HostBench(); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "benchmarking this machine …")
		for {
			time.Sleep(500 * time.Millisecond)
			r, err := cl.Host("")
			if err != nil {
				return err
			}
			if !r.Benching {
				for _, l := range board.BenchLines(r, true) {
					fmt.Println(l)
				}
				return nil
			}
		}
	}
	if len(args) > 0 {
		for _, n := range st.Nodes {
			if strings.EqualFold(n.Node, args[0]) {
				r, err := cl.Host(n.Node)
				if err != nil {
					r.Err = err.Error()
				}
				for _, l := range board.NodeDetail(n, r, st.Endpoints, 120) {
					fmt.Println(l)
				}
				return nil
			}
		}
		return fmt.Errorf("no such node: %s", args[0])
	}
	reps := make([]server.HostReport, len(st.Nodes))
	var wg sync.WaitGroup
	for i, n := range st.Nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := cl.Host(n.Node)
			if err != nil {
				r.Err = err.Error()
			}
			reps[i] = r
		}()
	}
	wg.Wait()
	fmt.Printf("  %-12s %-18s %-6s %-7s %5s %5s %5s %5s %5s %6s %11s  %s\n", "NODE", "VIA", "UP", "PING", "CPU", "RAM", "GPU", "VRAM", "TEMP", "AI", "NET", "FLEET")
	var notes []string
	for i, n := range st.Nodes {
		cpu, ram, gpu, vram, temp := board.NodeCells(reps[i])
		ping := "—"
		if n.PingMS > 0 {
			ping = fmt.Sprintf("%dms", n.PingMS)
		}
		fmt.Printf("  %-12s %-18s %-6s %-7s %5s %5s %5s %5s %5s %6s %11s  %s\n", n.Node, trunc(n.Via, 18), fmt.Sprintf("%d/%d", n.Up, n.Total), ping, cpu, ram, gpu, vram, temp, board.ScoreCell(reps[i]), board.NetCell(reps[i]), board.VersionCell(reps[i]))
		if e := reps[i].Err; e != "" {
			notes = append(notes, fmt.Sprintf("  %s: %s", n.Node, e))
		}
	}
	for _, n := range notes {
		fmt.Println(n)
	}
	for _, l := range board.DriftOfReports(st.Nodes, reps) {
		fmt.Println("  ⚠ " + l)
	}
	fmt.Println("\n`fleet nodes <node>` for one machine in full · `fleet nodes bench` measures this machine again (AI score) · `fleet probe` checks what this machine's models can do")
	return nil
}

// cmdProbe runs the capability micro benchmark and shows what it found (--apply writes it into caps).
func cmdProbe(args []string) error {
	fs := flag.NewFlagSet("probe", flag.ExitOnError)
	apply := fs.Bool("apply", false, "write the found capabilities into the endpoints' caps")
	show := fs.Bool("show", false, "only show the stored results")
	fs.Parse(args)
	names := fs.Args()
	cl, err := mustClient()
	if err != nil {
		return err
	}
	if !*show {
		if err := cl.Probe(names); err != nil {
			return err
		}
		last := ""
		for {
			time.Sleep(700 * time.Millisecond)
			st, err := cl.Status()
			if err != nil {
				return err
			}
			p := st.Probe
			if !p.Running {
				break
			}
			if line := fmt.Sprintf("probing %d/%d — %s", p.Done, p.Total, strings.Join(p.Current, ", ")); line != last {
				fmt.Fprintln(os.Stderr, line+" …")
				last = line
			}
		}
	}
	st, err := cl.Status()
	if err != nil {
		return err
	}
	want := map[string]bool{}
	if len(names) > 0 {
		cfg, err := cl.Config()
		if err != nil {
			return err
		}
		for _, n := range server.NodeEndpoints(&cfg, names) {
			want[n] = true
		}
	}
	node, differ, self := "", 0, board.SelfNode(st)
	for _, e := range st.Endpoints {
		if (len(want) > 0 && !want[e.Name]) || e.Node != self || e.Peer != "" {
			continue
		}
		if e.Node != node {
			node = e.Node
			fmt.Printf("\n▸ %s\n", node)
		}
		fmt.Printf("  %-36s %s\n", trunc(e.Model, 36), board.ProbeLine(e, true))
		if e.Probe.Differs(&e.Endpoint) && e.Peer == "" {
			differ++
		}
	}
	if *apply {
		ch, err := cl.ApplyProbe(names)
		if err != nil {
			return err
		}
		fmt.Println()
		for _, c := range ch {
			fmt.Println("  ✓ caps", c)
		}
		if len(ch) == 0 {
			fmt.Println("  caps already match the probe")
		}
	} else if differ > 0 {
		fmt.Printf("\n%d endpoint(s) differ from their caps — `fleet probe --apply --show` writes the probed ones\n", differ)
	}
	return nil
}

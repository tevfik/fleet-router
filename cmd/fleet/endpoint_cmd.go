package main

import (
	"flag"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tevfik/fleet-router/internal/board"
	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/deploy"
	"github.com/tevfik/fleet-router/internal/ggufmeta"
	"github.com/tevfik/fleet-router/internal/server"
)

// cmdEndpoint manages the endpoints of the configuration: what is registered, adding one by hand, stopping the
// processes behind one and removing it. Fleet keeps no list of models: an endpoint is a record of a running server.
func cmdEndpoint(args []string) error {
	cl, err := mustClient()
	if err != nil {
		return err
	}
	sub := "list"
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "list", "ls":
		st, err := cl.Status()
		if err != nil {
			return err
		}
		fmt.Print(endpointTable(st.Endpoints))
		return nil
	case "check":
		st, err := cl.Status()
		if err != nil {
			return err
		}
		nodes, _ := cl.StoreAll()
		runs, _ := cl.Runs()
		fmt.Println(strings.Join(board.ConsistencyLines(st, nodes, runs), "\n"))
		return nil
	case "add":
		fs := flag.NewFlagSet("endpoint add", flag.ContinueOnError)
		model := fs.String("model", "", "the model name the server answers to (default: the endpoint name)")
		node := fs.String("node", "", "the machine it runs on")
		ctx := fs.Int("ctx", 0, "context tokens the server runs with")
		slots := fs.Int("slots", 0, "concurrent requests it handles (0: unlimited)")
		tags := fs.String("tags", "", "comma-separated tags")
		if err := fs.Parse(flagsFirst(args, "model", "node", "ctx", "slots", "tags")); err != nil {
			return err
		}
		if fs.NArg() != 2 {
			return fmt.Errorf("usage: fleet endpoint add <name> <url> [--model M] [--node N] [--ctx N] [--slots N] [--tags a,b]   (url like http://host:port/v1)")
		}
		ep := config.Endpoint{Name: fs.Arg(0), URL: fs.Arg(1), Node: *node, Model: *model, Ctx: *ctx, Slots: *slots}
		if ep.Model == "" {
			ep.Model = ep.Name
		}
		if ep.Node == "" {
			ep.Node = "local"
		}
		for _, t := range strings.Split(*tags, ",") {
			if t = strings.TrimSpace(t); t != "" {
				ep.Tags = append(ep.Tags, t)
			}
		}
		if err := cl.Endpoints([]config.Endpoint{ep}, nil); err != nil {
			return err
		}
		fmt.Printf("✓ endpoint %s registered (fleet probe %s finds what it can do)\n", ep.Name, ep.Name)
		return nil
	case "stop", "rm", "remove", "del", "delete":
		fs := flag.NewFlagSet("endpoint "+sub, flag.ContinueOnError)
		dry := fs.Bool("dry-run", false, "only say what would be stopped")
		force := fs.Bool("force", false, "stop although requests are running through the endpoint")
		mend := fs.Bool("mend-roles", false, "also take it out of the roles that name it (else a removal is refused while a role does)")
		stop := fs.Bool("stop", false, "rm: stop the processes behind it first (default: remove the record only)")
		remove := fs.Bool("remove", false, "stop: also remove the endpoint afterwards")
		if err := fs.Parse(flagsFirst(args)); err != nil {
			return err
		}
		if fs.NArg() < 1 || (sub == "stop" && fs.NArg() != 1) {
			return fmt.Errorf("usage: fleet endpoint stop <name> [--dry-run] [--force] [--remove [--mend-roles]] | fleet endpoint rm <name…> [--stop [--force]] [--mend-roles]")
		}
		isRm := sub != "stop"
		if isRm && !*stop {
			notes, err := cl.EndpointsNotes(nil, fs.Args(), *mend)
			if err != nil {
				return err
			}
			fmt.Printf("✓ removed from the configuration: %s (no process was stopped; fleet endpoint stop <name> does that; fleet config undo restores)\n", strings.Join(fs.Args(), ", "))
			for _, n := range notes {
				fmt.Println("  " + n)
			}
			return nil
		}
		failed := false
		for _, name := range fs.Args() {
			rep, err := cl.StopEndpoint(name, server.StopOptions{DryRun: *dry, Force: *force, Remove: isRm || *remove, MendRoles: *mend})
			if err != nil {
				return err
			}
			fmt.Println(strings.Join(board.StopLines(rep), "\n"))
			failed = failed || rep.Failed()
		}
		if failed {
			return fmt.Errorf("the stop was refused or incomplete")
		}
		return nil
	case "prune":
		fs := flag.NewFlagSet("endpoint prune", flag.ContinueOnError)
		downOnly := fs.Bool("down", true, "prune endpoints that are currently down")
		olderThanStr := fs.String("older-than", "", "only prune endpoints older than duration (e.g. 7d, 24h)")
		dry := fs.Bool("dry-run", true, "only list endpoints that would be removed (default true)")
		mend := fs.Bool("mend-roles", false, "also remove pruned endpoints from roles that name them")
		if err := fs.Parse(flagsFirst(args, "down", "older-than", "dry-run", "mend-roles")); err != nil {
			return err
		}
		st, err := cl.Status()
		if err != nil {
			return err
		}
		runs, _ := cl.Runs()
		lastRunTime := map[string]time.Time{}
		for _, r := range runs {
			if r.Endpoint == "" {
				continue
			}
			t := r.At
			if !r.Ended.IsZero() {
				t = r.Ended
			}
			if t.After(lastRunTime[r.Endpoint]) {
				lastRunTime[r.Endpoint] = t
			}
		}

		var olderThan time.Duration
		if *olderThanStr != "" {
			var err error
			olderThan, err = parseDurationDays(*olderThanStr)
			if err != nil {
				return fmt.Errorf("invalid --older-than: %w", err)
			}
		}

		var toPrune []server.EpStatus
		for _, e := range st.Endpoints {
			if e.Peer != "" {
				continue // peer endpoints are owned by other nodes
			}
			if *downOnly {
				if !e.Known || e.OK {
					continue // only down endpoints
				}
			}
			if olderThan > 0 {
				var lastSeen time.Time
				if rt, ok := lastRunTime[e.Name]; ok {
					lastSeen = rt
				}
				if e.Probe != nil && e.Probe.At.After(lastSeen) {
					lastSeen = e.Probe.At
				}
				if e.Checked.After(lastSeen) {
					lastSeen = e.Checked
				}
				if !lastSeen.IsZero() && time.Since(lastSeen) < olderThan {
					continue
				}
			}
			toPrune = append(toPrune, e)
		}

		if len(toPrune) == 0 {
			fmt.Println("nothing to prune")
			return nil
		}

		names := make([]string, len(toPrune))
		for i, e := range toPrune {
			names[i] = e.Name
		}
		sort.Strings(names)

		if *dry {
			fmt.Printf("would remove %d endpoint(s) from the configuration (dry-run; pass --dry-run=false to remove; no process stopped):\n", len(toPrune))
			for _, e := range toPrune {
				errStr := e.Err
				if errStr == "" {
					errStr = "down"
				}
				fmt.Printf("  • %s (%s, %s): %s\n", e.Name, e.Node, filepath.Base(e.Model), errStr)
			}
			return nil
		}

		notes, err := cl.EndpointsNotes(nil, names, *mend)
		if err != nil {
			return err
		}
		fmt.Printf("✓ removed from the configuration: %s (no process was stopped; fleet config undo restores)\n", strings.Join(names, ", "))
		for _, n := range notes {
			fmt.Println("  " + n)
		}
		return nil
	}
	return fmt.Errorf("usage: fleet endpoint [list | check | add <name> <url> … | stop <name> | rm <name…> | prune [--down] [--older-than 7d] [--dry-run]]   (slots/tags/fleetport: fleet role slots|tag|fleetport)")
}

// parseDurationDays parses a duration string, supporting "d" and "days" in addition to Go's standard duration units.
func parseDurationDays(s string) (time.Duration, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, nil
	}
	if strings.HasSuffix(s, "days") {
		dStr := strings.TrimSpace(strings.TrimSuffix(s, "days"))
		n, err := strconv.Atoi(dStr)
		if err != nil {
			return 0, fmt.Errorf("invalid days %q: %w", s, err)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	if strings.HasSuffix(s, "d") {
		dStr := strings.TrimSpace(strings.TrimSuffix(s, "d"))
		n, err := strconv.Atoi(dStr)
		if err != nil {
			return 0, fmt.Errorf("invalid days %q: %w", s, err)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

// endpointTable lists the endpoints: the machine, the model, how it is reached and what is known about it.
func endpointTable(eps []server.EpStatus) string {
	rows := append([]server.EpStatus{}, eps...)
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	var b strings.Builder
	fmt.Fprintf(&b, "%-34s %-10s %-28s %-6s %-5s %-6s %s\n", "ENDPOINT", "MACHINE", "MODEL", "STATE", "CTX", "SLOTS", "TAGS / CAPS")
	for _, e := range rows {
		state := "?"
		switch {
		case e.Known && e.OK:
			state = "up"
		case e.Known:
			state = "down"
		}
		extra := strings.Join(e.Tags, ",")
		if len(e.Caps) > 0 {
			extra += " [" + strings.Join(e.Caps, ",") + "]"
		} else {
			extra += " [caps guessed from the name — fleet probe " + e.Name + "]"
		}
		if e.Group != nil {
			extra += " ring+" + fmt.Sprint(len(e.Group.Workers))
		}
		fmt.Fprintf(&b, "%-34s %-10s %-28s %-6s %-5s %-6s %s\n", trunc(e.Name, 34), trunc(e.Node, 10), trunc(filepath.Base(e.Model), 28), state, kcount(e.Ctx), slotText(e.Slots), strings.TrimSpace(extra))
	}
	return b.String()
}

func kcount(n int) string {
	if n <= 0 {
		return "-"
	}
	if n < 1024 {
		return fmt.Sprint(n)
	}
	return fmt.Sprintf("%dk", n/1024)
}

func slotText(n int) string {
	if n <= 0 {
		return "any"
	}
	return fmt.Sprint(n)
}

// registerRunOn records the server `fleet run-on` started as an endpoint, through the daemon (it applies it at once)
// or, when no daemon runs, in the configuration file. The model name is the file's; the capabilities come from the GGUF header when the file is
// readable here, else they stay automatic: `fleet probe <endpoint>` measures them once the server answers.
func registerRunOn(node, modelPath string, res *deploy.DeployResult, ctxLen int) (string, error) {
	if ctxLen <= 0 {
		ctxLen = 4096
	}
	ep := config.Endpoint{Name: res.EndpointName, Node: node, URL: res.URL, Model: filepath.Base(modelPath), ModelSHA256: res.ModelSHA256, Ctx: ctxLen, Tags: []string{"single-node"}}
	// the capabilities the file's own header implies (a file on another machine cannot be read from here: probe it)
	if g, err := ggufmeta.Read(modelPath); err == nil {
		ep.Caps = g.Caps()
	}
	if cl := clientIfAlive(); cl != nil {
		return ep.Name, cl.Endpoints([]config.Endpoint{ep}, []string{ep.Name})
	}
	cfg, err := config.Load(config.Path())
	if err != nil {
		return ep.Name, err
	}
	replaced := false
	for i := range cfg.Endpoints {
		if cfg.Endpoints[i].Name == ep.Name {
			cfg.Endpoints[i], replaced = ep, true
		}
	}
	if !replaced {
		cfg.Endpoints = append(cfg.Endpoints, ep)
	}
	return ep.Name, cfg.Save(config.Path())
}

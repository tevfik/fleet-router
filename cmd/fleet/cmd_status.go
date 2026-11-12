package main

import (
	"flag"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/tevfik/fleet-router/internal/board"
	"github.com/tevfik/fleet-router/internal/client"
	"github.com/tevfik/fleet-router/internal/discover"
	"github.com/tevfik/fleet-router/internal/roles"
	"github.com/tevfik/fleet-router/internal/server"
)

func cmdStatus() error {
	cl, err := mustClient()
	if err != nil {
		return err
	}
	st, err := cl.Status()
	if err != nil {
		return err
	}
	if st.Version != "" && st.Version != version {
		fmt.Printf("fleet %s · daemon %s (restart the daemon to run %s)\n\n", version, st.Version, version)
	} else {
		fmt.Printf("fleet %s\n\n", version)
	}
	printVersionDrift(cl, st)
	printHubStatus(cl)
	roles := make([]string, 0)
	for r := range st.Roles {
		roles = append(roles, r)
	}
	sort.Strings(roles)
	fmt.Println("ROLES")
	for _, r := range roles {
		x := st.Roles[r]
		flag := ""
		if x.Active != x.Preferred {
			flag = "  ← fallback active"
		}
		fmt.Printf("  %-10s → %-34s (preferred %s, fallback %v)%s\n", r, ref(st, x.Active), ref(st, x.Preferred), refs(st, x.Fallback), flag)
	}
	node := ""
	for _, e := range st.Endpoints {
		if e.Node != node {
			node = e.Node
			fmt.Printf("\n▸ %s\n", node)
		}
		m := "✓"
		if !e.Known {
			m = "?"
		} else if !e.OK {
			m = "✗"
		}
		extra := fmt.Sprintf("%dms", e.LatencyMS)
		if e.TPS > 0 {
			extra = fmt.Sprintf("%s · %4.1f t/s", extra, e.TPS)
		}
		if e.Err != "" {
			extra = e.Err
		}
		fmt.Printf("  %s %-36s %s\n", m, e.Model, extra) // the node is the heading above
		for _, l := range board.GroupLines(e) {
			fmt.Println("      " + l)
		}
	}
	return nil
}

// nodeReports asks every node's fleet for its readings (and so its version), in parallel.
func nodeReports(cl *client.Client, nodes []server.NodeStatus) []server.HostReport {
	reps := make([]server.HostReport, len(nodes))
	var wg sync.WaitGroup
	for i, n := range nodes {
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
	return reps
}

// printVersionDrift warns when the machines of the fleet do not run the same fleet (or engine) version, and
// shows the hub side: the fleet version each joined node reported when it connected.
func printVersionDrift(cl *client.Client, st server.Status) {
	reps := nodeReports(cl, st.Nodes)
	vs := []board.NodeVersion{}
	for i, n := range st.Nodes {
		if reps[i].Err == "" {
			vs = append(vs, board.NodeVersion{Node: n.Node, Fleet: reps[i].Version, Engine: reps[i].Engine})
		}
	}
	if hs, err := cl.HubStatus(); err == nil {
		seen := map[string]bool{}
		for _, v := range vs {
			seen[v.Node] = true
		}
		for _, n := range hs.Nodes {
			if !seen[n.NodeName] && n.FleetVersion != "" {
				vs = append(vs, board.NodeVersion{Node: n.NodeName + " (hub)", Fleet: n.FleetVersion, Engine: n.EngineVersion})
			}
		}
	}
	for _, l := range board.VersionDrift(vs) {
		fmt.Println("⚠ " + l)
	}
	if v := board.ParseVersion(version); v.IsDev() {
		fmt.Println("⚠ this fleet binary is a dev build (no version): `make build` sets one, or install a release")
	}
}

// ref names an endpoint by what it serves ("node/model"); unknown names are returned as they are.
func ref(st server.Status, name string) string {
	for _, e := range st.Endpoints {
		if e.Name == name {
			return e.Ref()
		}
	}
	return name
}

func refs(st server.Status, names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = ref(st, n)
	}
	return out
}

func cmdDiscover(args []string) error {
	fs := flag.NewFlagSet("discover", flag.ExitOnError)
	ports := fs.String("ports", "", "ports (default: from the configuration)")
	ssh := fs.String("ssh", "", "ssh aliases, comma-separated (default: from the configuration)")
	embed := fs.Bool("embed", false, "also register embedding / reranker models")
	apply := fs.Bool("apply", false, "register the new models")
	fs.Parse(args)
	cl, err := mustClient()
	if err != nil {
		return err
	}
	var hosts []string
	if *ssh != "" {
		hosts = strings.Split(*ssh, ",")
	}
	r, err := cl.Discover(*ports, hosts, *embed, *apply)
	if err != nil {
		return err
	}
	for _, n := range r.Notes {
		fmt.Println(n)
	}
	for _, k := range r.Plan.Known {
		fmt.Println("  ✓ already registered:", k)
	}
	for _, s := range r.Plan.Skip {
		fmt.Println("  - skipped:", s)
	}
	for _, p := range r.Plan.Peers {
		how := "ssh " + p.SSH
		if p.SSH == "" {
			how = p.URL
		}
		fmt.Printf("  ⇄ fleet found: %-14s %s  → peer (its models appear as node/model)\n", p.Name, how)
	}
	for _, e := range r.Plan.Add {
		caps := strings.Join(e.Caps, ",")
		if caps == "" {
			caps = "auto"
		}
		fmt.Printf("  + %-40s %-30s caps: %s\n", e.Name, e.Model, caps)
	}
	switch {
	case *apply:
		fmt.Printf("\n✓ %d endpoint(s) and %d peer(s) registered\n", len(r.Plan.Add), len(r.Plan.Peers))
	case len(r.Plan.Add) > 0 || len(r.Plan.Peers) > 0:
		fmt.Println("\nuse --apply to register")
	}
	return nil
}

func cmdRole(args []string) error {
	cl, err := mustClient()
	if err != nil {
		return err
	}
	switch {
	case len(args) >= 3 && args[0] == "set":
		return cl.SetRole(args[1], args[2], args[3:])
	case len(args) == 2 && args[0] == "rm":
		return cl.DeleteRole(args[1])
	case len(args) == 1 && args[0] == "list":
		return cmdStatus()
	case len(args) >= 3 && args[0] == "opt": // fleet role opt <rol> priority=5 max_parallel=2 overflow=true
		f := map[string]any{}
		for _, kv := range args[2:] {
			k, v, _ := strings.Cut(kv, "=")
			switch k {
			case "priority", "max_parallel":
				n, e := strconv.Atoi(v)
				if e != nil {
					return fmt.Errorf("%s must be a number", k)
				}
				f[k] = n
			case "overflow":
				f[k] = v == "true" || v == "1"
			case "use", "strategy":
				f[k] = v
			default:
				return fmt.Errorf("unknown setting: %s (priority, max_parallel, overflow, strategy, use)", k)
			}
		}
		return cl.UpdateRole(args[1], f)
	case len(args) == 2 && args[0] == "test": // fleet role test <role>
		cfg, err := cl.Config()
		if err != nil {
			return err
		}
		r, ok := cfg.Roles[args[1]]
		if !ok {
			return fmt.Errorf("no such role: %s", args[1])
		}
		var members []string
		for _, e := range cfg.Pool(r) {
			members = append(members, e.Name)
		}
		for _, line := range client.FormatTest(cl.TestRole(args[1], members)) {
			fmt.Println(line)
		}
		return nil
	case len(args) >= 1 && args[0] == "init": // fleet role init [--apply]
		cfg, err := cl.Config()
		if err != nil {
			return err
		}
		sg := roles.Suggest(&cfg)
		if len(sg) == 0 {
			fmt.Println("nothing to suggest — the starter roles already exist")
			return nil
		}
		apply := len(args) > 1 && args[1] == "--apply"
		for _, x := range sg {
			fmt.Printf("  %-10s → %s %v   (%s)\n", x.Role, x.Value.Preferred, x.Value.Fallback, x.Reason)
			if apply {
				if err := cl.SetRole(x.Role, x.Value.Preferred, x.Value.Fallback); err != nil {
					return err
				}
			}
		}
		if !apply {
			fmt.Println("\nuse --apply to create them")
		}
		return nil
	case len(args) >= 2 && args[0] == "match": // fleet role match <role> tag:fast node:dino … (none = clear)
		return cl.SetMatch(args[1], args[2:])
	case len(args) >= 2 && args[0] == "tag": // fleet role tag <endpoint> fast gpu … (none = clear)
		return cl.SetTags(args[1], args[2:])
	case len(args) >= 2 && args[0] == "group": // fleet role group <endpoint> engine=llama-ring workers=a,b stall=90s | off
		g, stall, err := board.ParseGroup(args[2:])
		if err != nil {
			return err
		}
		if err := cl.SetGroup(args[1], g, stall); err != nil {
			return err
		}
		if g.Engine == "" {
			fmt.Println("✓ " + args[1] + ": no longer a group")
		} else {
			fmt.Printf("✓ %s: group %s on %v\n", args[1], g.Engine, g.Workers)
		}
		return nil
	case len(args) == 3 && args[0] == "fleetport": // fleet role fleetport <endpoint> <port> (0 = 4711)
		n, e := strconv.Atoi(args[2])
		if e != nil || n < 0 || n > 65535 {
			return fmt.Errorf("the port must be a number from 0 (4711) to 65535")
		}
		if err := cl.SetFleetPort(args[1], n); err != nil {
			return err
		}
		fmt.Printf("✓ %s: the fleet on its machine is at port %s\n", args[1], map[bool]string{true: "4711", false: args[2]}[n == 0])
		return nil
	case len(args) == 3 && args[0] == "slots": // fleet role slots <endpoint> <n>
		n, e := strconv.Atoi(args[2])
		if e != nil {
			return fmt.Errorf("slots must be a number")
		}
		return cl.SetSlots(args[1], n)
	}
	return fmt.Errorf("usage: fleet role set <role> <endpoint> [fallback…] | role rm <role> | role list | role opt <role> priority=N max_parallel=N overflow=true strategy=failover|least-busy|round-robin|fastest|cost | role init [--apply] | role slots <endpoint> <N> | role match <role> [tag:x|node:x|cap:x|model:glob …] | role tag <endpoint> [tags…]")
}

func cmdHosts(args []string) error {
	cl, err := mustClient()
	if err != nil {
		return err
	}
	cfg, err := cl.Config()
	if err != nil {
		return err
	}
	d := cfg.Discover
	has := func(l []string, x string) bool {
		for _, v := range l {
			if v == x {
				return true
			}
		}
		return false
	}
	if len(args) >= 2 {
		switch args[0] {
		case "add":
			for _, h := range args[1:] {
				if !has(d.SSH, h) {
					d.SSH = append(d.SSH, h)
				}
			}
		case "rm":
			var keep []string
			for _, h := range d.SSH {
				if !has(args[1:], h) {
					keep = append(keep, h)
				}
			}
			d.SSH = keep
		case "ports":
			d.Ports = strings.Join(args[1:], ",")
		case "ignore":
			for _, h := range args[1:] {
				if !has(d.Ignore, h) {
					d.Ignore = append(d.Ignore, h)
				}
			}
		default:
			return fmt.Errorf("usage: fleet hosts [add|rm <host…>] [ports <list>] [ignore <machine:port…>]")
		}
		if err := cl.SetDiscover(d.Ports, d.SSH, d.Ignore); err != nil {
			return err
		}
	}
	fmt.Printf("local  : %s (always)\nssh    : %s\nports  : %s\nignore : %s\n", discover.Self(), strings.Join(d.SSH, ", "), d.Ports, strings.Join(d.Ignore, ", "))
	return nil
}

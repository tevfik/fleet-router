package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tevfik/fleet-router/internal/client"
	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/eval"
	"github.com/tevfik/fleet-router/internal/feature"
)

// cmdEval scores models with the fixed suite in internal/eval and stores the result for the menu.
func cmdEval(args []string) error {
	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	role := fs.String("role", "", "evaluate the models of this role")
	all := fs.Bool("all", false, "evaluate every chat model (slow)")
	show := fs.Bool("show", false, "only print the stored scores")
	par := fs.Int("parallel", 3, "models evaluated at the same time")
	if err := fs.Parse(flagsFirst(args, "role", "parallel")); err != nil {
		return err
	}
	if *show {
		printEval(nil)
		return nil
	}
	cl, err := mustClient()
	if err != nil {
		return err
	}
	cfg, err := cl.Config()
	if err != nil {
		return err
	}
	var targets []string
	switch {
	case *role != "":
		r, ok := cfg.Roles[*role]
		if !ok {
			return fmt.Errorf("no such role: %s", *role)
		}
		for _, e := range cfg.Pool(r) {
			targets = append(targets, e.Name)
		}
	case *all:
		for _, e := range cfg.Endpoints {
			if !e.IsEmbed() {
				targets = append(targets, e.Name)
			}
		}
	default:
		targets = fs.Args()
	}
	if len(targets) == 0 {
		return fmt.Errorf("usage: fleet eval --role <role> | --all | <endpoint…>   (fleet eval --show prints the stored scores)")
	}
	items := eval.Items()
	fmt.Fprintf(os.Stderr, "evaluating %d model(s) on %d prompts (temperature 0) …\n", len(targets), len(items))
	zero := 0.0
	ask := func(ctx context.Context, target, prompt string) (string, error) {
		cx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		ans, _, err := cl.ChatTemp(cx, target, "", prompt, 600, &zero)
		return ans, err
	}
	var mu sync.Mutex
	done := map[string]int{}
	fi, _ := os.Stderr.Stat()
	tty := fi != nil && fi.Mode()&os.ModeCharDevice != 0 // a progress line only makes sense on a terminal
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	results := eval.RunAll(ctx, ask, targets, items, *par, func(t string, d, n int) {
		mu.Lock()
		done[t] = d
		total := 0
		for _, v := range done {
			total += v
		}
		if tty {
			fmt.Fprintf(os.Stderr, "\r  %d/%d prompts answered", total, n*len(targets))
		}
		mu.Unlock()
	})
	fmt.Fprintln(os.Stderr)
	if err := eval.Save(config.EvalPath(), results); err != nil {
		fmt.Fprintln(os.Stderr, "could not store the scores:", err)
	}
	printEval(results)
	return nil
}

func pct(f float64) string { return fmt.Sprintf("%3.0f%%", f*100) }

// printEval prints the given results, or the stored scores when rs is nil.
func printEval(rs []eval.Result) {
	type row struct {
		name string
		s    eval.Summary
	}
	var rows []row
	if rs != nil {
		for _, r := range rs {
			rows = append(rows, row{r.Target, eval.Summarize(r)})
		}
	} else {
		for n, s := range eval.Load(config.EvalPath()) {
			rows = append(rows, row{n, s})
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].s.Score > rows[j].s.Score })
	}
	if len(rows) == 0 {
		fmt.Println("no scores yet — run `fleet eval --role <role>`")
		return
	}
	fmt.Printf("%-44s %6s %6s   %-6s %-9s %-8s %-9s %6s\n", "MODEL", "SCORE", "CHECKS", "format", "reasoning", "language", "knowledge", "s/ans")
	for _, r := range rows {
		fmt.Printf("%-44s %6s %6s   %-6s %-9s %-8s %-9s %6.1f\n", trunc44(r.name), pct(r.s.Score), pct(r.s.Checks),
			pct(r.s.ByCat[eval.CatFormat]), pct(r.s.ByCat[eval.CatReasoning]), pct(r.s.ByCat[eval.CatLanguage]), pct(r.s.ByCat[eval.CatKnowledge]), r.s.Seconds)
	}
	for _, r := range rs {
		for _, it := range r.Items {
			if it.Err != "" {
				fmt.Printf("  ! %s / %s: %.100s\n", r.Target, it.ID, it.Err)
				break // one example per model is enough
			}
		}
	}
	fmt.Println("SCORE = prompts where every check passed · CHECKS = single checks passed · errors count as failed")
}

func trunc44(s string) string {
	if r := []rune(s); len(r) > 44 {
		return string(r[:43]) + "…"
	}
	return s
}

// cmdConfig: `fleet config history` lists the saved configurations, `fleet config undo` goes one step back.
func cmdConfig(args []string) error {
	if len(args) >= 1 && (args[0] == "node" || args[0] == "nodes") {
		if !feature.Runner() {
			return fmt.Errorf("config node: %s", feature.RunnerHint)
		}
		return cmdConfigNode(args[1:])
	}
	if len(args) >= 1 && args[0] == "key" {
		return cmdKey(args[1:])
	}
	if len(args) == 1 && args[0] == "history" {
		snaps := config.Snapshots(config.Path())
		if len(snaps) == 0 {
			fmt.Println("no earlier configurations kept yet")
		}
		for i := len(snaps) - 1; i >= 0; i-- {
			fmt.Println(" ", strings.TrimSuffix(snaps[i], ".yaml"))
		}
		return nil
	}
	if len(args) == 1 && args[0] == "undo" {
		if cl := client.New(listen()); cl.Alive() { // the daemon applies it hot
			left, err := cl.Undo()
			if err != nil {
				return err
			}
			fmt.Printf("✓ previous configuration restored and applied (%d older kept)\n", left)
			return nil
		}
		name, err := config.Undo(config.Path())
		if err != nil {
			return err
		}
		fmt.Println("✓ restored", strings.TrimSuffix(name, ".yaml"), "(the daemon is not running; it reads it on start)")
		return nil
	}
	return fmt.Errorf("usage: fleet config history | undo | node list|set|rm | key set|show|rm")
}

func cmdKey(args []string) error {
	usage := "usage: fleet key set <key> | generate | show [--reveal] | rm"
	if len(args) == 0 {
		return fmt.Errorf("%s", usage)
	}
	switch args[0] {
	case "set", "add":
		if len(args) != 2 {
			return fmt.Errorf("usage: fleet key set <key>")
		}
		key := strings.TrimSpace(args[1])
		if key == "" {
			return fmt.Errorf("key cannot be empty")
		}
		if err := config.SaveHubKey(key); err != nil {
			return err
		}
		fmt.Println("✓ hub API key saved to", filepath.Join(config.Dir(), ".env"), "(mode 600) and the config")
		if os.Getenv("FLEET_API_KEY") != "" && os.Getenv("FLEET_API_KEY") != key {
			fmt.Println("  ! $FLEET_API_KEY is set in this environment to another value and wins over the saved one")
		}
		return nil
	case "generate", "gen", "new":
		k := config.GenerateHubKey()
		if err := config.SaveHubKey(k); err != nil {
			return err
		}
		fmt.Println("✓ new hub API key generated and saved (the old one, if any, no longer works for nodes until they get this one)")
		fmt.Println("  key:", k)
		fmt.Println("  give it to the machines that join this hub: fleet key set <key>   (or the menu: Fleet Hub → Set API Key)")
		fmt.Println("  to require it from joining nodes: hub_require_key: true in the hub's config, then restart its fleet")
		return nil
	case "show", "get", "list":
		cfg, _ := config.Load(config.Path())
		k, src := config.ResolveHubAPIKey(cfg)
		if k == "" {
			fmt.Println("no hub API key configured (fleet key set <key> | fleet key generate)")
			return nil
		}
		if len(args) > 1 && (args[1] == "--reveal" || args[1] == "-r") {
			fmt.Printf("hub API key: %s\n  used from: %s\n", k, src)
			return nil
		}
		fmt.Printf("hub API key: %s\n  used from: %s   (--reveal prints it in full)\n", config.MaskSecret(k), src)
		fmt.Println("  order: $FLEET_API_KEY > FLEET_API_KEY in " + filepath.Join(config.Dir(), ".env") + " > hub_api_key of the config > api_key_env")
		if st, err := os.Stat(filepath.Join(config.Dir(), ".env")); err == nil && st.Mode().Perm()&0o077 != 0 {
			fmt.Printf("  ! %s is mode %o: chmod 600 it\n", filepath.Join(config.Dir(), ".env"), st.Mode().Perm())
		}
		if cfg != nil && cfg.HubAPIKey != "" && cfg.HubAPIKey != k {
			fmt.Println("  note: hub_api_key in the config differs from the one in use; fleet key set makes them the same")
		}
		return nil
	case "rm", "remove", "del", "delete":
		if err := config.RemoveSecret("FLEET_API_KEY"); err != nil {
			return fmt.Errorf("remove the key from .env: %w", err)
		}
		if cfg, err := config.Load(config.Path()); err == nil && cfg != nil && cfg.HubAPIKey != "" {
			cfg.HubAPIKey = ""
			if err := cfg.Save(config.Path()); err != nil {
				return fmt.Errorf("clear hub_api_key in the config: %w", err)
			}
		}
		fmt.Println("✓ hub API key removed from .env and the config")
		if os.Getenv("FLEET_API_KEY") != "" {
			fmt.Println("  ! $FLEET_API_KEY is still set in this environment and keeps being used")
		}
		return nil
	default:
		return fmt.Errorf("%s", usage)
	}
}

// cmdAPIKey manages client authorization tokens and QoS profiling.
func cmdAPIKey(args []string) error {
	sub := "list"
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	cl, err := mustClient()
	if err != nil {
		return err
	}
	switch sub {
	case "list", "ls":
		keys, err := cl.ListAPIKeys()
		if err != nil {
			return err
		}
		if len(keys) == 0 {
			fmt.Println("No API keys found. Create one with: fleet apikey create <name> [--qos <tier>]")
			return nil
		}
		fmt.Printf("%-24s %-12s %-16s %-10s %-16s %-8s %-10s\n", "KEY", "NAME", "QOS", "ROLE", "MODELS", "REQS", "LAST USED")
		for _, k := range keys {
			last := k.LastUsedAt.Format("Jan 02 15:04")
			if k.LastUsedAt.IsZero() {
				last = "never"
			}
			modelsDesc := "*"
			if len(k.AllowedModels) > 0 {
				modelsDesc = strings.Join(k.AllowedModels, ",")
				if len(modelsDesc) > 16 {
					modelsDesc = modelsDesc[:13] + "..."
				}
			}
			roleDesc := k.Role
			if roleDesc == "" {
				roleDesc = "-"
			}
			fmt.Printf("%-24s %-12s %-16s %-10s %-16s %-8d %-10s\n", k.Key, k.Name, k.QoS, roleDesc, modelsDesc, k.TotalRequests, last)
		}
		return nil
	case "create", "new":
		fs := flag.NewFlagSet("apikey create", flag.ExitOnError)
		qos := fs.String("qos", "ultra-low-latency", "QoS tier: ultra-low-latency | max-throughput | memory-saver | default")
		role := fs.String("role", "", "role binding (e.g. coder)")
		modelsFlag := fs.String("models", "", "comma-separated allowed models or glob patterns (e.g. 'qwen*,deepseek*')")
		fs.Parse(flagsFirst(args, "qos", "role", "models"))
		if fs.NArg() < 1 {
			return fmt.Errorf("usage: fleet apikey create <name> [--qos <tier>] [--role <role>] [--models <m1,m2...>]")
		}
		name := fs.Arg(0)
		var models []string
		if *modelsFlag != "" {
			for _, m := range strings.Split(*modelsFlag, ",") {
				if m = strings.TrimSpace(m); m != "" {
					models = append(models, m)
				}
			}
		}
		ak, err := cl.CreateAPIKey(name, *qos, *role, models...)
		if err != nil {
			return err
		}
		allowedDesc := "*"
		if len(ak.AllowedModels) > 0 {
			allowedDesc = strings.Join(ak.AllowedModels, ", ")
		}
		roleDesc := ak.Role
		if roleDesc == "" {
			roleDesc = "(any)"
		}
		fmt.Printf("✓ Created API key for %s:\n  Key:     %s\n  QoS:     %s\n  Role:    %s\n  Models:  %s\n", ak.Name, ak.Key, ak.QoS, roleDesc, allowedDesc)
		return nil
	case "rm", "delete":
		if len(args) < 1 {
			return fmt.Errorf("usage: fleet apikey rm <key>")
		}
		if err := cl.DeleteAPIKey(args[0]); err != nil {
			return err
		}
		fmt.Printf("✓ Deleted API key %s\n", args[0])
		return nil
	default:
		return fmt.Errorf("unknown apikey subcommand %q (list, create, rm)", sub)
	}
}

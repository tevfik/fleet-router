package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/ringengine"
	"github.com/tevfik/fleet-router/internal/setup"
	"github.com/tevfik/fleet-router/internal/update"
)

// engineGuard: a release carries the plain build only. Replacing a binary that has the llama-ring engine linked in
// with it would take the runtime away from the machine without a word, so such a binary is left alone.
func engineGuard(hasEngine bool) error {
	if hasEngine {
		return fmt.Errorf("this binary has the llama-ring engine linked in and releases carry the plain build only — update the engine with `fleet engine install` (or build one: make build-engine, then fleet setup install)")
	}
	return nil
}

// updateOnce installs the newest release if it is newer than this binary; returns its tag, or "" when up to date.
func updateOnce(ctx context.Context, force bool) (string, error) {
	rel, err := update.Latest(ctx)
	if err != nil {
		return "", err
	}
	if !update.Newer(rel.Tag, version, force) || (!force && update.Installed() == rel.Tag) {
		return "", nil // up to date, or this very tag was already installed by us (see MarkInstalled)
	}
	if err := engineGuard(ringengine.Available()); err != nil {
		return "", err
	}
	exe, err := setup.Exe()
	if err != nil {
		return "", err
	}
	if err := update.Apply(ctx, rel.Tag, exe); err != nil {
		return "", err
	}
	update.MarkInstalled(rel.Tag)
	update.MarkPending(rel.Tag)
	return rel.Tag, nil
}

func cmdUpdate(args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	check := fs.Bool("check", false, "only report whether a newer release exists")
	force := fs.Bool("force", false, "also replace a dev/unreleased build")
	rollback := fs.Bool("rollback", false, "go back to the version that was installed before the last update")
	all := fs.Bool("all", false, "then run the same on every machine reached over SSH")
	source := fs.Bool("source", false, "print where this build looks for releases")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *source {
		src := update.Source()
		if src == "" {
			return update.ErrNoSource
		}
		fmt.Println(src)
		return nil
	}
	if *all {
		defer updateRemotes(*check, *force)
	}
	if *rollback {
		exe, err := setup.Exe()
		if err != nil {
			return err
		}
		if err := update.Rollback(exe, version); err != nil {
			return err
		}
		fmt.Printf("✓ previous version restored (it will not be installed again automatically: %s is skipped)\n", version)
		restartService()
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	rel, err := update.Latest(ctx)
	if err != nil {
		return err
	}
	if !update.Newer(rel.Tag, version, *force) {
		fmt.Printf("fleet %s is up to date (latest release: %s)\n", version, rel.Tag)
		return nil
	}
	if *check {
		fmt.Printf("fleet %s → %s available: run `fleet update`\n", version, rel.Tag)
		if n := rel.Notes(); n != "" {
			fmt.Println(n)
		}
		return nil
	}
	if kids := serviceChildren(); len(kids) > 0 && !*force {
		return fmt.Errorf("the update restarts the fleet service, which would kill %d process(es) a deploy started inside it (a running model?):\n  %s\nrestart later, or pass --force to update anyway (they die; deploys made by this version survive restarts)", len(kids), strings.Join(kids, "\n  "))
	}
	if msg := hubStreamWarning(localHubStreams()); msg != "" && !*force {
		return fmt.Errorf("%s\nrestart later, or pass --force to update anyway", msg)
	}
	tag, err := updateOnce(ctx, *force)
	if err != nil {
		return err
	}
	fmt.Printf("✓ fleet %s → %s   (go back with `fleet update --rollback`)\n", version, tag)
	if n := rel.Notes(); n != "" {
		fmt.Println("What is new:\n" + n)
	}
	restartService()
	return nil
}

// sshAll runs a command on every SSH host of the configuration (in parallel) and returns the hosts and what each said.
func sshAll(remote string, limit time.Duration) ([]string, []string, error) {
	cfg, err := config.Load(config.Path())
	if err != nil {
		return nil, nil, err
	}
	hosts := cfg.SSHHosts()
	outs := make([]string, len(hosts))
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), limit)
			defer cancel()
			b, err := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=8", "--", h, remote).CombinedOutput()
			out := strings.TrimSpace(string(b))
			if err != nil && out == "" {
				out = err.Error()
			}
			outs[i] = out
		}()
	}
	wg.Wait()
	return hosts, outs, nil
}

// cmdEngine shows the engine of this machine and installs the build that suits it from the package registry.
func cmdEngine(args []string) error {
	sub := "status"
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "source": // where `fleet engine install` looks (no network): scripts/build-engine.sh checks a fresh build with it
		src := update.EngineSource()
		if src == "" {
			return update.ErrNoEngineSource
		}
		fmt.Println(src)
		return nil
	case "status":
		fmt.Println("this binary:", ringengine.Version())
		if src := update.EngineSource(); src != "" {
			fmt.Println("  engine source:", src)
		} else {
			fmt.Println("  engine source: none (set FLEET_ENGINE_REPO)")
		}
		if info := ringengine.Info(); info != "" {
			fmt.Println("  identity:", info)
		}
		if s := update.EngineInstalled(); s != "" {
			fmt.Println("  installed by fleet engine install:", s)
		}
		m := update.DetectMachine()
		fmt.Printf("this machine: %s/%s · GPU %s", m.OS, m.Arch, func() string {
			if m.GPU == "" {
				return "none"
			}
			return fmt.Sprintf("%s (sm_%s, driver CUDA %d)", m.GPU, m.Cap, m.CUDA)
		}())
		fmt.Println()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		files, err := update.EngineFiles(ctx)
		if err != nil {
			return err
		}
		f, why, cerr := update.ChooseVariant(m, files)
		fmt.Printf("package fleet-engine %s:\n", update.PinnedEngineVersion())
		for _, x := range files {
			mark := " "
			if cerr == nil && x.Name == f.Name {
				mark = "→"
			}
			fmt.Printf("  %s %-46s %6.0f MB\n", mark, x.Name, float64(x.Size)/1e6)
		}
		if cerr != nil {
			fmt.Println("none of them suits this machine:", cerr)
		} else {
			fmt.Printf("suits this machine: %s — %s\n", f.Name, why)
		}
		return nil
	case "install":
		fs := flag.NewFlagSet("engine install", flag.ContinueOnError)
		variant := fs.String("variant", "", "the build to install (a file name or part of one; default: the one that suits the machine)")
		force := fs.Bool("force", false, "install even if this build is already there")
		all := fs.Bool("all", false, "then run the same on every machine reached over SSH")
		pin := fs.String("version", "", "the engine package version to install (default: the one this binary is built against; a newer package needs this once, e.g. 0.4.3)")
		if err := fs.Parse(flagsFirst(args, "variant", "version")); err != nil {
			return err
		}
		if *pin != "" {
			if err := update.PinEngineVersion(*pin); err != nil {
				return err
			}
		}
		if *all {
			defer engineRemotes(*variant, *force, *pin)
		}
		exe, err := setup.Exe()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		msg, err := update.EnsureEngine(ctx, exe, *variant, *force, func(s string) { fmt.Fprintln(os.Stderr, s) })
		if err != nil {
			return err
		}
		fmt.Println("✓ " + msg)
		if strings.HasPrefix(msg, "installed") {
			restartService()
		}
		return nil
	}
	return fmt.Errorf("usage: fleet engine [status | source | install [--variant V] [--version X.Y.Z] [--force] [--all]]")
}

// engineRemoteCommand is the shell line `fleet engine install --all` runs on each machine. Version and source go as
// environment variables, not flags: a machine still on an older binary understands (or ignores) them, and one on a build
// without a built-in engine source learns this machine's (otherwise `fleet engine` does not exist there).
func engineRemoteCommand(flags, pin, source string) string {
	env := ""
	if pin != "" {
		env = "FLEET_ENGINE_VERSION=" + pin + " "
	}
	if source != "" && safeShellURL.MatchString(source) {
		env += "FLEET_ENGINE_REPO=" + source + " "
	}
	return `PATH="$HOME/.local/bin:/usr/local/bin:$PATH"; command -v fleet >/dev/null || { echo "no fleet installed (install.sh)"; exit 3; }; ` + env + `fleet engine install` + flags
}

// safeShellURL: a URL that can go into a remote shell line unquoted.
var safeShellURL = regexp.MustCompile(`^https?://[A-Za-z0-9.:/_-]+$`)

func engineRemotes(variant string, force bool, pin string) {
	flags := ""
	if variant != "" {
		flags += " --variant " + variant
	}
	if force {
		flags += " --force"
	}
	hosts, outs, err := sshAll(engineRemoteCommand(flags, pin, update.EngineSource()), 30*time.Minute)
	if err != nil {
		fmt.Println("  could not read the configuration:", err)
		return
	}
	if len(hosts) == 0 {
		fmt.Println("\nno machines reached over SSH")
		return
	}
	for i, h := range hosts {
		fmt.Printf("\n▸ %s\n", h)
		for _, l := range strings.Split(outs[i], "\n") {
			fmt.Println("  " + l)
		}
	}
}

// updateRemotes runs `fleet update` on every SSH host of the configuration (each machine's fleet updates
// itself; this only asks it to do so now) and prints what each said.
func updateRemotes(check, force bool) {
	cfg, err := config.Load(config.Path())
	if err != nil {
		fmt.Println("  could not read the configuration:", err)
		return
	}
	hosts := cfg.SSHHosts()
	if len(hosts) == 0 {
		fmt.Println("\nno machines reached over SSH")
		return
	}
	flags := ""
	if check {
		flags += " --check"
	}
	if force {
		flags += " --force"
	}
	remote := `PATH="$HOME/.local/bin:/usr/local/bin:$PATH"; command -v fleet >/dev/null || { echo "no fleet installed (install.sh)"; exit 3; }; fleet update` + flags
	outs := make([]string, len(hosts))
	errs := make([]error, len(hosts))
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			b, err := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=8", "--", h, remote).CombinedOutput()
			out := strings.TrimSpace(string(b))
			if err != nil && out == "" {
				out = err.Error()
			}
			outs[i], errs[i] = out, err
		}()
	}
	wg.Wait()
	results := make([]updateResult, len(hosts))
	for i, h := range hosts {
		fmt.Printf("\n▸ %s\n", h)
		for _, l := range strings.Split(outs[i], "\n") {
			if strings.HasPrefix(l, "What is new") {
				break // the notes were shown above
			}
			fmt.Println("  " + l)
		}
		results[i] = classifyUpdate(h, outs[i], errs[i], check)
	}
	fmt.Println()
	for _, l := range updateSummary(results, check) {
		fmt.Println(l)
	}
}

// restartService restarts the systemd service (user or system) if there is one: a running daemon keeps
// the old binary until it is restarted.
func restartService() {
	for _, a := range [][]string{{"--user"}, {}} {
		if exec.Command("systemctl", append(a, "is-active", "--quiet", "fleet.service")...).Run() == nil {
			if out, err := exec.Command("systemctl", append(a, "restart", "fleet.service")...).CombinedOutput(); err != nil {
				fmt.Printf("  restart the service yourself: %s\n", strings.TrimSpace(string(out)))
			} else {
				fmt.Println("  service restarted")
			}
			return
		}
	}
}

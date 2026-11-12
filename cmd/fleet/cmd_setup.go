package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/tevfik/fleet-router/internal/board"
	"github.com/tevfik/fleet-router/internal/client"
	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/discover"
	"github.com/tevfik/fleet-router/internal/importer"
	"github.com/tevfik/fleet-router/internal/profile"
	"github.com/tevfik/fleet-router/internal/setup"
)

func cmdOpenCode() error {
	cl, err := mustClient()
	if err != nil {
		return err
	}
	p, err := cl.OpenCode()
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(map[string]any{"providers": p}, "", "  ")
	fmt.Println(string(b))
	return nil
}

func cmdImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	self := fs.String("self", discover.Self(), "name of the machine fleet runs on")
	write := fs.Bool("write", false, "write to the configuration (otherwise only a summary)")
	fs.Parse(flagsFirst(args, "self"))
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: fleet import <fleet.yaml> [--self machine] [--write]")
	}
	cfg, notes, err := importer.Import(fs.Arg(0), *self)
	if err != nil {
		return err
	}
	for _, n := range notes {
		fmt.Println("  !", n)
	}
	fmt.Printf("%d endpoint, %d rol (self=%s)\n", len(cfg.Endpoints), len(cfg.Roles), *self)
	for _, r := range cfg.RoleNames() {
		fmt.Printf("  role %-10s → %s %v\n", r, cfg.Roles[r].Preferred, cfg.Roles[r].Fallback)
	}
	if !*write {
		fmt.Println("use --write to write →", config.Path())
		return nil
	}
	if _, err := os.Stat(config.Path()); err == nil {
		return fmt.Errorf("%s already exists — move or back it up first", config.Path())
	}
	return cfg.Save(config.Path())
}

func cmdSetup(args []string) error {
	system := false
	if n := len(args); n == 2 && args[1] == "--system" {
		system, args = true, args[:1]
	}
	if len(args) != 1 {
		return fmt.Errorf("usage: fleet setup claude|opencode|skills|service|install|all [--system]")
	}
	exe, err := setup.Exe()
	if err != nil {
		return err
	}
	switch args[0] {
	case "claude":
		out, err := setup.Claude(exe, os.Getenv("FLEET_LISTEN"))
		fmt.Println(out)
		if err != nil {
			return err
		}
		return printGuide(setup.ClaudeGuidance)
	case "skills", "guidance":
		err1 := printGuide(setup.ClaudeGuidance)
		err2 := printGuide(setup.OpenCodeGuidance)
		return errors.Join(err1, err2)
	case "opencode":
		cl, err := mustClient()
		if err != nil {
			return err
		}
		p, err := cl.OpenCode()
		if err != nil {
			return err
		}
		path, err := setup.OpenCode(p)
		if err != nil {
			return err
		}
		fmt.Println("✓ written:", path, "(backup made)")
		return printGuide(setup.OpenCodeGuidance)
	case "all":
		steps := []string{"install", "service", "claude", "opencode"}
		if system { // claude/opencode register per user — not as root
			steps = steps[:2]
		}
		for _, a := range steps {
			fmt.Println("▸ setup", a)
			sub := []string{a}
			if system && (a == "install" || a == "service") {
				sub = append(sub, "--system")
			}
			if err := cmdSetup(sub); err != nil {
				fmt.Println("  ! ", err)
			}
		}
		return nil
	case "service":
		if system { // the unit must point at the system-wide binary
			exe = "/usr/local/bin/fleet"
			p, err := setup.SystemServiceInstall(exe)
			if err == nil {
				fmt.Println("✓ system service:", p)
			}
			return err
		}
		p, err := setup.ServiceInstall(exe)
		if err == nil {
			fmt.Println("✓ servis:", p)
		}
		return err
	case "install":
		dir := ""
		if system {
			dir = "/usr/local/bin"
		}
		p, err := setup.InstallBinary(dir)
		if err == nil {
			fmt.Println("✓ kuruldu:", p)
		}
		return err
	}
	return fmt.Errorf("unknown: %s", args[0])
}

func cmdDoctor() error {
	ok := func(c bool, msg, bad string) {
		if c {
			fmt.Println("  ✓", msg)
		} else {
			fmt.Println("  ✗", bad)
		}
	}
	_, err := os.Stat(config.Path())
	ok(err == nil, "configuration: "+config.Path(), "no configuration: "+config.Path()+" (fleet import / discover)")
	cfg, cerr := config.Load(config.Path())
	ok(cerr == nil, "configuration is valid", fmt.Sprint("configuration is invalid: ", cerr))
	if cerr == nil {
		host, _, _ := net.SplitHostPort(cfg.Listen)
		ip := net.ParseIP(host)
		exposed := host == "" || (ip != nil && !ip.IsLoopback())
		ok(!exposed || cfg.APIKeyEnv != "", "listening on "+cfg.Listen+" (not exposed beyond this machine, or protected by api_key_env)",
			"listening on "+cfg.Listen+" without api_key_env — anyone who can reach the port can use your models")
		if cfg.APIKeyEnv != "" {
			ok(config.Secret(cfg.APIKeyEnv) != "", "api key "+cfg.APIKeyEnv+" is set", "api_key_env names "+cfg.APIKeyEnv+" but it is empty (remote clients will always get 401)")
		}
	}
	for _, bin := range []string{"ssh", "opencode", "claude", "curl"} {
		_, e := exec.LookPath(bin)
		ok(e == nil, bin+" is in PATH", bin+" is not in PATH")
	}
	for _, iss := range setup.CheckSkillSymlinks() {
		ok(false, "", iss.Error())
	}
	if kids := serviceChildren(); len(kids) > 0 {
		ok(false, "", fmt.Sprintf("%d deployed process(es) live inside fleet.service and die when it restarts (fleet update, systemctl restart):\n      %s\n      redeploy with this version: it starts them in their own systemd scope", len(kids), strings.Join(kids, "\n      ")))
	}
	if okLinger, lmsg := checkLinger(setup.LoginctlLinger, os.Getenv("USER")); lmsg != "" {
		ok(okLinger, lmsg, lmsg)
	}
	for _, l := range doctorSecretLines(cfg) {
		fmt.Println("  " + l)
	}
	cl := client.New(listen())
	alive := cl.Alive()
	ok(alive, "daemon is running ("+cl.Base+")", "daemon is not running — `fleet serve` / `fleet setup service`")
	if !alive {
		for _, l := range doctorVersionLines(version, nil) {
			fmt.Println("  " + l)
		}
	}
	if alive && cfg != nil {
		st, _ := cl.Status()
		bad, onDemand := 0, 0
		for _, e := range st.Endpoints {
			switch {
			case e.Known && !e.OK && e.OnDemand:
				onDemand++ // a distributed run that is not started: runs are started when wanted
			case e.Known && !e.OK:
				bad++
			}
		}
		ok(bad == 0, fmt.Sprintf("%d endpoints healthy", len(st.Endpoints)-onDemand), fmt.Sprintf("%d/%d endpoints down (fleet status)", bad, len(st.Endpoints)-onDemand))
		if onDemand > 0 {
			fmt.Printf("  · %d distributed endpoint(s) not started (on demand: fleet deploy --profile …)\n", onDemand)
		}
		for r, x := range st.Roles {
			ok(x.Active == x.Preferred, "role "+r+" → "+x.Active, "role "+r+": fallback active ("+x.Active+")")
		}
		for _, w := range cfg.PoolWarnings() {
			fmt.Println("  !", w)
		}
		var vs []board.NodeVersion
		for i, r := range nodeReports(cl, st.Nodes) {
			if r.Err == "" {
				vs = append(vs, board.NodeVersion{Node: st.Nodes[i].Node, Fleet: r.Version, Engine: r.Engine})
			}
		}
		if len(vs) == 0 {
			vs = append(vs, board.NodeVersion{Node: "this machine", Fleet: version})
		}
		for _, l := range doctorVersionLines(version, vs) {
			fmt.Println("  " + l)
		}
		if hs, err := cl.HubStatus(); err == nil {
			for _, l := range hubStatusLines(hs, time.Now()) {
				fmt.Println("  " + l)
			}
		}
		if runs, err := cl.Runs(); err == nil {
			var profiles []*profile.Profile
			profiles, _, _ = profileStore().List()
			for _, w := range checkDuplicateEndpointRuns(runs) {
				ok(false, "", w)
			}
			// runs are started when wanted (fleet deploy / a profile), so one that would not come back after a reboot is
			// how they are meant to work: a note, not a failure
			for _, w := range checkUnmanagedRuns(runs, profiles, setup.ProfileAutostartEnabled) {
				fmt.Println("  · " + w)
			}
		}
		for _, n := range st.Nodes { // machines struggling right now, and what is worth knowing about them
			if n.Strain == nil {
				continue
			}
			for _, r := range n.Strain.Reasons {
				fmt.Printf("  ✗ %s: %s — routing prefers other machines until it recovers\n", n.Node, r)
			}
			for _, w := range n.Strain.Warnings {
				fmt.Printf("  ! %s: %s\n", n.Node, w)
			}
		}
	}
	return nil
}

func printGuide(fn func(*config.Config) ([]string, error)) error {
	cfg, err := config.Load(config.Path())
	if err != nil {
		return err
	}
	res, err := fn(cfg)
	for _, r := range res {
		fmt.Println("  ✓", r)
	}
	return err
}

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tevfik/fleet-router/internal/board"
	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/server"
)

// cmdLog prints the router's recent requests; -f keeps following.
func cmdLog(args []string) error {
	fs := flag.NewFlagSet("log", flag.ContinueOnError)
	n := fs.Int("n", 30, "how many recent requests")
	follow := fs.Bool("f", false, "keep printing new requests")
	if err := fs.Parse(flagsFirst(args, "n")); err != nil {
		return err
	}
	cl, err := mustClient()
	if err != nil {
		return err
	}
	es, err := cl.Log(*n)
	if err != nil {
		return err
	}
	for _, l := range board.RenderLog(es, false) {
		fmt.Println(l)
	}
	if !*follow {
		return nil
	}
	var last time.Time
	if len(es) > 0 {
		last = es[len(es)-1].Time
	}
	for {
		time.Sleep(time.Second)
		es, err := cl.Log(100)
		if err != nil {
			return err
		}
		for _, e := range es {
			if e.Time.After(last) {
				fmt.Println(board.LogLine(e))
				last = e.Time
			}
		}
	}
}

// cmdPeers manages the other fleets this one can reach: their models appear here as node/model.
func cmdPeers(args []string) error {
	cl, err := mustClient()
	if err != nil {
		return err
	}
	show := func(ps []server.PeerStatus) {
		if len(ps) == 0 {
			fmt.Println("no peers — `fleet discover` finds fleets on your SSH hosts, or `fleet peers add <name> <ssh-host|url>`")
			return
		}
		for _, p := range ps {
			state := "✓"
			note := fmt.Sprintf("%d models · %dms", p.Models, p.RTTMS)
			if !p.OK {
				state, note = "✗", p.Err
				if note == "" {
					note = "not synced yet"
				}
			}
			how := p.URL
			if p.SSH != "" {
				how = "ssh " + p.SSH
			}
			name := ""
			if p.Remote != "" && p.Remote != p.Name {
				name = " (calls itself " + p.Remote + ")"
			}
			if p.Version != "" {
				name += " · fleet " + p.Version
			}
			fmt.Printf("  %s %-14s %-24s %s%s\n", state, p.Name, how, note, name)
		}
	}
	showLAN := func(always bool) {
		fs, err := cl.LAN()
		if err != nil || (len(fs) == 0 && !always) {
			return
		}
		fmt.Println("\non this network (mDNS):")
		if len(fs) == 0 {
			fmt.Println("  nobody heard yet — other fleets announce themselves within a minute (unless they set mdns: false)")
		}
		for _, f := range fs {
			where := f.Host
			if f.Addr != "" {
				where += " " + f.Addr
			}
			note := f.Scope
			if f.Version != "" {
				note += " · fleet " + f.Version
			}
			if f.Peer != "" {
				fmt.Printf("  ✓ %-14s %-28s already peer %s\n", f.Name, where, f.Peer)
				continue
			}
			next := f.Add
			if next == "" {
				next = f.Hint
			} else if f.Hint != "" {
				next += "\n        " + f.Hint
			}
			fmt.Printf("  ? %-14s %-28s %s\n      → %s\n", f.Name, where, note, next)
		}
	}
	switch {
	case len(args) == 0 || args[0] == "list":
		ps, err := cl.Peers()
		if err != nil {
			return err
		}
		show(ps)
		showLAN(false)
		return nil
	case args[0] == "lan":
		showLAN(true)
		return nil
	case args[0] == "pair":
		pc, err := cl.PairCode()
		if err != nil {
			return err
		}
		fmt.Printf("pairing code: %s   (valid until %s, one use)\n\non the other machine:\n", pc.Code, pc.Until.Format("15:04"))
		for _, u := range pc.URLs {
			fmt.Printf("  fleet peers add %s %s pair=%s\n", config.Slug(server.Hostname()), u, pc.Code)
		}
		if pc.Note != "" {
			fmt.Println("\nnote:", pc.Note)
		}
		return nil
	case args[0] == "sync":
		ps, err := cl.PeerOp(nil, "", true)
		if err != nil {
			return err
		}
		show(ps)
		return nil
	case len(args) == 2 && (args[0] == "rm" || args[0] == "remove"):
		_, err := cl.PeerOp(nil, args[1], false)
		return err
	case len(args) >= 3 && args[0] == "add": // fleet peers add <name> <ssh-host | http://…/v1> [only=sel,…] [skip=sel,…] [max_hops=N] [key_env=NAME] [remote=host:port]
		p := config.Peer{Name: args[1]}
		pair := ""
		if strings.HasPrefix(args[2], "http") {
			p.URL = args[2]
		} else {
			p.SSH = args[2]
		}
		for _, kv := range args[3:] {
			k, v, _ := strings.Cut(kv, "=")
			switch k {
			case "only":
				p.Only = strings.Split(v, ",")
			case "skip":
				p.Skip = strings.Split(v, ",")
			case "max_hops":
				n, e := strconv.Atoi(v)
				if e != nil {
					return fmt.Errorf("max_hops must be a number")
				}
				p.MaxHops = n
			case "key_env":
				p.KeyEnv = v
			case "remote":
				p.Remote = v
			case "pair":
				pair = v
			default:
				return fmt.Errorf("unknown setting: %s (only, skip, max_hops, key_env, remote, pair)", k)
			}
		}
		var ps []server.PeerStatus
		var err error
		if pair != "" {
			ps, err = cl.PeerPair(&p, pair)
		} else {
			ps, err = cl.PeerOp(&p, "", false)
		}
		if err != nil {
			return err
		}
		time.Sleep(2 * time.Second) // let the first sync finish
		if ps, err = cl.Peers(); err == nil {
			show(ps)
		}
		return nil
	}
	return fmt.Errorf("usage: fleet peers [list] | lan | pair | add <name> <ssh-host|http://host:port/v1> [only=tag:x,node:y] [skip=model:z*] [max_hops=N] [key_env=NAME] [remote=host:port] [pair=CODE] | rm <name> | sync")
}

// usageValueFlags are the flags of `fleet usage` that take a value: -hub is a boolean and must NOT be listed (flagsFirst would
// swallow the argument after it, so `usage --hub --by client` failed with a wrong --by).
var usageValueFlags = []string{"by", "since"}

func cmdUsage(args []string) error {
	fs := flag.NewFlagSet("usage", flag.ContinueOnError)
	by := fs.String("by", "client", "group by: client | role | model | endpoint | day")
	since := fs.String("since", "7d", "period: 24h, 7d, 30d")
	withHub := fs.Bool("hub", false, "include hub traffic (source column, local without hub-tunnel + hub)")
	if err := fs.Parse(flagsFirst(args, usageValueFlags...)); err != nil {
		return err
	}
	switch *by {
	case "client", "role", "model", "endpoint", "day":
	default:
		return fmt.Errorf("--by must be client, role, model, endpoint or day")
	}
	cl, err := mustClient()
	if err != nil {
		return err
	}
	if *withHub {
		localRows, err := cl.UsageFiltered(*by, "", *since, "hub-tunnel")
		if err != nil {
			return err
		}
		var hubRows []server.UsageRow
		cfg, _ := config.Load(config.Path())
		if cfg != nil && cfg.HubURL != "" {
			hubKey, _ := config.ResolveHubAPIKey(cfg)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			hr, err := cl.HubUsage(ctx, cfg.HubURL, hubKey, *by, "", *since)
			cancel()
			switch {
			case err != nil:
				fmt.Fprintf(os.Stderr, "hub usage unavailable (%v): showing this machine only\n", err)
			case len(hr) == 0:
				fmt.Fprintln(os.Stderr, "hub: no traffic recorded in this period")
			default:
				hubRows = hr
			}
		} else {
			fmt.Fprintln(os.Stderr, "no hub configured: showing this machine only")
		}
		if len(hubRows) > 0 {
			rows := board.CombineUsage(localRows, hubRows)
			for _, l := range board.RenderUsage(rows, *by) {
				fmt.Println(l)
			}
			return nil
		}
		for _, l := range board.RenderUsage(localRows, *by) {
			fmt.Println(l)
		}
		return nil
	}
	rows, err := cl.Usage(*by, "", *since)
	if err != nil {
		return err
	}
	for _, l := range board.RenderUsage(rows, *by) {
		fmt.Println(l)
	}
	return nil
}

func cmdTop() error {
	cl, err := mustClient()
	if err != nil {
		return err
	}
	st, err := cl.Status()
	if err != nil {
		return err
	}
	fmt.Print(renderBoard(st.Endpoints))
	return nil
}

// renderBoard is the plain-text leaderboard (also used by the MCP tool).
func renderBoard(eps []server.EpStatus) string {
	rows := board.Rows(eps)
	var sb strings.Builder
	sb.WriteString(board.Trophy + "\n\n TOKENS / SECOND\n\n")
	maxv := board.Max(rows)
	for _, r := range rows {
		rank, medal := "  ", "  "
		if r.Rank > 0 {
			rank = fmt.Sprintf("%2d", r.Rank)
			if r.Medal != "" {
				medal = r.Medal
			}
		}
		val := "   —"
		if r.TPS > 0 {
			val = fmt.Sprintf("%6.1f t/s", r.TPS)
		}
		extra := r.Node
		if r.TTFTMS > 0 {
			extra += fmt.Sprintf(" · ttft %dms", r.TTFTMS)
		}
		fmt.Fprintf(&sb, " %s %s %-28s %s %s  %s\n", rank, medal, r.Name, board.Bar(r.TPS, maxv, 28), val, extra)
	}
	if len(rows) == 0 || maxv == 0 {
		sb.WriteString("\n nothing measured yet — run `fleet bench`\n")
	}
	return sb.String()
}

func cmdBench(args []string) error {
	cl, err := mustClient()
	if err != nil {
		return err
	}
	if err := cl.Bench(args); err != nil {
		return err
	}
	last := ""
	for {
		time.Sleep(700 * time.Millisecond)
		st, err := cl.Status()
		if err != nil {
			return err
		}
		if b := st.Bench; b.Running {
			if line := fmt.Sprintf("benchmarking %d/%d — %s", b.Done, b.Total, b.Current); line != last {
				fmt.Fprintln(os.Stderr, line+" …")
				last = line
			}
			continue
		}
		fmt.Print(renderBoard(st.Endpoints))
		return nil
	}
}

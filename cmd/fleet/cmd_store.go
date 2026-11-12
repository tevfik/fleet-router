package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tevfik/fleet-router/internal/board"
	"github.com/tevfik/fleet-router/internal/client"
	"github.com/tevfik/fleet-router/internal/plan"
	"github.com/tevfik/fleet-router/internal/server"
	"github.com/tevfik/fleet-router/internal/store"
)

// cmdStore manages this machine's model files and shows what every machine has.
func cmdStore(args []string) error {
	cl, err := mustClient()
	if err != nil {
		return err
	}
	sub := "list"
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "add":
		fs := flag.NewFlagSet("store add", flag.ContinueOnError)
		oll := fs.Bool("ollama", false, "also register the models of this machine's Ollama directory (~/.ollama/models; their blobs are GGUFs): a model's projector is registered beside it")
		if err := fs.Parse(flagsFirst(args)); err != nil {
			return err
		}
		args = fs.Args()
		if len(args) == 0 && !*oll {
			return fmt.Errorf("usage: fleet store add [--ollama] <file|directory>…   (a model split in …-00001-of-0000N.gguf: add the directory, every shard is registered)")
		}
		var abs []string
		for _, a := range args {
			p, err := filepath.Abs(a)
			if err != nil {
				return err
			}
			abs = append(abs, p)
		}
		if *oll {
			abs = append(abs, "~/.ollama/models")
		}
		n, err := cl.StoreAdd(abs)
		if err != nil {
			return err
		}
		fmt.Printf("hashing %d file(s) on the daemon's machine …\n", n)
		last := ""
		for {
			time.Sleep(700 * time.Millisecond)
			inv, err := cl.StoreLocal()
			if err != nil {
				return err
			}
			h := inv.Hashing
			if h == nil {
				if inv.Error != "" {
					return fmt.Errorf("%s", inv.Error)
				}
				break
			}
			if line := fmt.Sprintf("%s %d%%", filepath.Base(h.Path), h.Done*100/max(h.Total, 1)); line != last {
				fmt.Fprintln(os.Stderr, line)
				last = line
			}
		}
		if inv, err := cl.StoreLocal(); err == nil {
			for _, n := range inv.Notes {
				fmt.Println("! " + n)
			}
		}
		return storeList(cl)
	case "pull", "fill":
		return storeTransfer(cl, sub, args)
	case "rm", "remove":
		fs := flag.NewFlagSet("store rm", flag.ContinueOnError)
		del := fs.Bool("delete-file", false, "also delete the file from the disk (only a file fleet fetched itself; never one you keep elsewhere)")
		force := fs.Bool("force", false, "with --delete-file: although an endpoint of this machine serves a model of that name")
		if err := fs.Parse(flagsFirst(args)); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: fleet store rm <sha256 prefix | name> [--delete-file [--force]]   (a split model: every shard)")
		}
		refs := []string{fs.Arg(0)}
		if nodes, err := cl.StoreAll(); err == nil {
			for _, n := range nodes {
				if n.Source != "this machine" || n.Store == nil {
					continue
				}
				// the files of this machine that make up the model (all shards of a split one)
				local := []server.NodeStore{n}
				if rows, err := board.ResolveModel(local, fs.Arg(0)); err == nil || len(rows) > 0 {
					refs = refs[:0]
					for _, r := range rows {
						refs = append(refs, r.SHA256)
					}
				}
			}
		}
		for _, ref := range refs {
			var e store.Entry
			var err error
			if *del {
				e, err = cl.StoreDelete(ref, *force)
			} else {
				e, err = cl.StoreRemove(ref)
			}
			if err != nil {
				return err
			}
			if *del {
				fmt.Printf("deleted and forgot %s (%s)\n", e.Name, e.SHA256[:min(8, len(e.SHA256))])
			} else {
				fmt.Printf("forgot %s (%s); the file itself is untouched (--delete-file removes a fetched one)\n", e.Name, e.SHA256[:min(8, len(e.SHA256))])
			}
		}
		return nil
	case "info":
		if len(args) != 1 {
			return fmt.Errorf("usage: fleet store info <sha256 prefix | name>")
		}
		nodes, err := cl.StoreAll()
		if err != nil {
			return err
		}
		lines, err := board.StoreInfo(nodes, args[0])
		if err != nil && len(lines) == 0 {
			return err
		}
		fmt.Println(strings.Join(lines, "\n"))
		return nil
	case "prune":
		gone, err := cl.StorePrune()
		if err != nil {
			return err
		}
		if len(gone) == 0 {
			fmt.Println("nothing to forget: every registered file is still there")
		}
		for _, e := range gone {
			fmt.Printf("forgot %s (%s): the file is gone or changed\n", e.Name, e.SHA256[:min(8, len(e.SHA256))])
		}
		return nil
	case "status":
		if nodes, err := cl.StoreAll(); err == nil {
			for _, l := range board.TransferLines(nodes) {
				fmt.Println(l)
			}
		}
		inv, err := cl.StoreLocal()
		if err != nil {
			return err
		}
		switch {
		case inv.Hashing != nil:
			fmt.Printf("hashing %s: %d%%\n", filepath.Base(inv.Hashing.Path), inv.Hashing.Done*100/max(inv.Hashing.Total, 1))
		case inv.Error != "":
			fmt.Println("last add failed:", inv.Error)
		default:
			fmt.Println("idle")
		}
		fmt.Printf("%d file(s) registered", len(inv.Files))
		if inv.Cache != nil {
			fmt.Printf("; worker cache %s: %d entries, %s", inv.Cache.Dir, inv.Cache.Entries, board.Bytes(inv.Cache.Bytes))
		} else {
			fmt.Print("; no worker cache (set LLAMA_CACHE for the daemon, or ring_cache in the configuration)")
		}
		fmt.Println()
		return nil
	case "list", "ls":
		return storeList(cl)
	case "abort", "cancel":
		fs := flag.NewFlagSet("store abort", flag.ContinueOnError)
		node := fs.String("node", "", "the machine running the transfer (default: the daemon's own)")
		discard := fs.Bool("discard", false, "also delete the unfinished copies (.part) and forget the failed-transfer warnings")
		if err := fs.Parse(args); err != nil {
			return err
		}
		target := "this machine"
		if *node != "" {
			target = *node
		}
		if *discard {
			if err := cl.StoreDiscard(*node); err != nil {
				return err
			}
			fmt.Printf("✓ %s: transfer stopped, unfinished copies deleted, warnings cleared (finished files untouched)\n", target)
			return nil
		}
		if err := cl.StoreAbort(*node); err != nil {
			return err
		}
		fmt.Printf("✓ transfer on %s aborted; the file on disk is preserved\n", target)
		return nil
	}
	return fmt.Errorf("usage: fleet store [list | add [--ollama] <file|directory>… | pull <sha|name> [--yes] | fill <sha|name> --blocks … | rm <sha|name> [--delete-file] | info <sha|name> | prune | abort [--node N] [--discard] | status]")
}

// storeTransfer: `fleet store pull <ref> [--node N] [--from M]` fetches a file onto a machine; `fleet store fill
// <ref> --blocks 0-3,7 [--output] [--node N] [--from M]` fills a machine's worker cache with those blocks.
func storeTransfer(cl *client.Client, sub string, args []string) error {
	fs := flag.NewFlagSet("store "+sub, flag.ContinueOnError)
	node := fs.String("node", "", "the machine that receives it (default: the daemon's own)")
	from := fs.String("from", "", "the machine to take it from (default: any that has it)")
	blocks := fs.String("blocks", "", "fill: the blocks the machine will hold, like 0-3,7")
	output := fs.Bool("output", false, "fill: also the output projection (the machine holds the last blocks)")
	wait := fs.Bool("wait", true, "wait for the transfer and show its progress")
	yes := fs.Bool("yes", false, "pull: do not ask before a big file goes over a line that is not the LAN")
	if err := fs.Parse(flagsFirst(args, "node", "from", "blocks")); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: fleet store %s <sha256 prefix | name> [--node N] [--from M]%s", sub, map[string]string{"fill": " --blocks 0-3,7 [--output]", "pull": " [--yes]"}[sub])
	}
	nodes, err := cl.StoreAll()
	if err != nil {
		return err
	}
	rows, err := board.ResolveModel(nodes, fs.Arg(0)) // every shard of a split model
	if err != nil {
		return err
	}
	target := *node
	if target == "" {
		for _, n := range nodes {
			if n.Source == "this machine" {
				target = n.Node
			}
		}
	}
	if sub == "fill" && *blocks == "" && !*output {
		return fmt.Errorf("fill needs --blocks (like 0-3,7) and/or --output")
	}
	var todo []board.StoreRow
	var total int64
	for _, r := range rows {
		if sub == "pull" && r.Has[target] {
			continue
		}
		todo = append(todo, r)
		total += r.Size
	}
	if len(todo) == 0 {
		fmt.Printf("%s already has every file of %s\n", orDefault(target, "this machine"), fs.Arg(0))
		return nil
	}
	if sub == "pull" {
		if msg := checkLine(cl, *node, *from, todo[0].SHA256, total, *yes); msg != "" {
			return fmt.Errorf("%s", msg)
		}
	}
	if len(todo) > 1 {
		fmt.Printf("%d files (%s) in turn: %s\n", len(todo), board.Bytes(total), strings.Join(shardNames(todo), ", "))
	}
	for _, r := range todo {
		before := -1
		if *wait || len(todo) > 1 {
			before = lastTransferID(nodes, target)
		}
		if sub == "pull" {
			err = cl.StorePull(*node, *from, r.SHA256)
		} else {
			err = cl.StoreFill(*node, *from, r.SHA256, *blocks, *output)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", r.Name, err)
		}
		fmt.Printf("%s of %s started on %s\n", sub, r.Name, orDefault(target, "this machine"))
		if !*wait && len(todo) == 1 {
			return nil
		}
		if err := waitTransfer(cl, target, before); err != nil {
			return fmt.Errorf("%s: %w (run the same command again: a partial file resumes)", r.Name, err)
		}
		if nodes, err = cl.StoreAll(); err != nil {
			return err
		}
	}
	return nil
}

func shardNames(rows []board.StoreRow) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.Name)
	}
	return out
}

// checkLine looks at the line a pull would take; for a big transfer over anything but the LAN it returns the reason
// to stop (a yes, from --yes or the terminal, lets it go on), "" when the pull may start.
func checkLine(cl *client.Client, node, from, sha string, total int64, yes bool) string {
	if total < board.BigTransfer {
		return ""
	}
	li, err := cl.StoreLink(node, from, sha)
	if err != nil {
		return "" // an unknown line is the pull's own problem to report
	}
	line := board.LineNote(li, total)
	if line == "" {
		return ""
	}
	if yes {
		fmt.Fprintln(os.Stderr, "! "+line)
		return ""
	}
	if st, err := os.Stdin.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 {
		fmt.Fprintf(os.Stderr, "%s.\nContinue? [y/N] ", line)
		var ans string
		fmt.Scanln(&ans)
		if strings.HasPrefix(strings.ToLower(ans), "y") {
			return ""
		}
		return "cancelled"
	}
	return line + ". Not started: pass --yes to go on, or fetch from a machine on the LAN (--from)"
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func lastTransferID(nodes []server.NodeStore, node string) int {
	id := 0
	for _, n := range nodes {
		if n.Node == node && n.Store != nil {
			for _, t := range n.Store.Transfers {
				id = max(id, t.ID)
			}
		}
	}
	return id
}

// waitTransfer follows the machine's transfer that is newer than after until it ends.
func waitTransfer(cl *client.Client, node string, after int) error {
	last := ""
	start := time.Now()
	for {
		time.Sleep(900 * time.Millisecond)
		nodes, err := cl.StoreAll()
		if err != nil {
			return err
		}
		running, fresh := false, false
		for _, n := range nodes {
			if n.Node != node || n.Store == nil {
				continue
			}
			for _, t := range n.Store.Transfers {
				if t.ID <= after {
					continue
				}
				fresh = true
				if t.Running {
					running = true
					if line := fmt.Sprintf("%s %s  %s / %s", t.Kind, t.What, board.Bytes(t.Done), board.Bytes(t.Total)); line != last {
						fmt.Fprintln(os.Stderr, line)
						last = line
					}
				} else if t.Err != "" {
					return fmt.Errorf("%s", t.Err)
				}
			}
		}
		if !running && (fresh || time.Since(start) > 5*time.Second) {
			fmt.Println("done")
			return nil
		}
	}
}

func storeList(cl *client.Client) error {
	nodes, err := cl.StoreAll()
	if err != nil {
		return err
	}
	fmt.Println(strings.Join(board.StoreLines(nodes), "\n"))
	return nil
}

// cmdShards writes fleet-shards.json: which bytes of a GGUF belong to which block, with hashes.
func cmdShards(args []string) error {
	fs := flag.NewFlagSet("shards", flag.ExitOnError)
	out := fs.String("out", "", "write the manifest here (default: stdout)")
	noHash := fs.Bool("no-hash", false, "skip the SHA-256 of every block (no full read of the file)")
	need := fs.String("need", "", "first-last: print the byte ranges a worker holding those blocks has to have, instead of the manifest")
	_ = fs.Parse(flagsFirst(args, "out", "need"))
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: fleet shards <model.gguf> [--out fleet-shards.json] [--no-hash] [--need 0-5]")
	}
	s, err := plan.BuildShards(fs.Arg(0), !*noHash && *need == "")
	if err != nil {
		return err
	}
	if *need != "" {
		a, b, ok := strings.Cut(*need, "-")
		first, e1 := strconv.Atoi(a)
		last, e2 := strconv.Atoi(b)
		if !ok || e1 != nil || e2 != nil {
			return fmt.Errorf("--need: %q is not first-last", *need)
		}
		rs, n := s.Need(first, last, false)
		fmt.Printf("blocks %d-%d: %d range(s), %.2f GB of %.2f GB\n", first, last, len(rs), float64(n)/1e9, float64(s.FileBytes)/1e9)
		for _, r := range rs {
			fmt.Printf("  %d +%d\n", r.Offset, r.Length)
		}
		return nil
	}
	b, _ := json.MarshalIndent(s, "", " ")
	if *out == "" {
		fmt.Println(string(b))
		return nil
	}
	return os.WriteFile(*out, b, 0o644)
}

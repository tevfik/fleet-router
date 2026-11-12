package main

import (
	"flag"
	"fmt"
	"strings"

	"github.com/tevfik/fleet-router/internal/board"
	"github.com/tevfik/fleet-router/internal/server"
)

// cmdRun lists the distributed runs every fleet knows about, shows one as a path, binds one to the endpoint that
// serves it, or ends it.
func cmdRun(args []string) error {
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
		runs, err := cl.Runs()
		if err != nil {
			return err
		}
		fmt.Println(strings.Join(board.RunRows(runs), "\n"))
		return nil
	case "show":
		runs, err := cl.Runs()
		if err != nil {
			return err
		}
		ref := "last"
		if len(args) > 0 {
			ref = args[0]
		}
		for _, r := range runs {
			if ref == "last" || strings.HasPrefix(r.ID, ref) {
				st, _ := cl.Status()
				fmt.Println(strings.Join(board.RunLines(r, &st), "\n"))
				return nil
			}
		}
		return fmt.Errorf("no such run: %s", ref)
	case "bind":
		if len(args) != 2 {
			return fmt.Errorf("usage: fleet run bind <id> <endpoint>")
		}
		_, err := cl.SetRun(args[0], &args[1], false)
		return err
	case "stop":
		fs := flag.NewFlagSet("run stop", flag.ContinueOnError)
		dry := fs.Bool("dry-run", false, "only say what would be stopped")
		force := fs.Bool("force", false, "stop although requests are running through the endpoint")
		remove := fs.Bool("remove-endpoint", false, "also remove the endpoint from the configuration")
		mend := fs.Bool("mend-roles", false, "with --remove-endpoint: also take it out of the roles that name it")
		if err := fs.Parse(flagsFirst(args)); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: fleet run stop <id> [--dry-run] [--force] [--remove-endpoint [--mend-roles]]")
		}
		rep, err := cl.StopRun(fs.Arg(0), server.StopOptions{DryRun: *dry, Force: *force, Remove: *remove, MendRoles: *mend})
		if err != nil {
			return err
		}
		fmt.Println(strings.Join(board.StopLines(rep), "\n"))
		if rep.Failed() {
			return fmt.Errorf("the stop was refused or incomplete")
		}
		return nil
	case "end":
		fs := flag.NewFlagSet("run end", flag.ContinueOnError)
		recOnly := fs.Bool("record-only", false, "close the record only; the processes keep running (fleet run stop stops them)")
		if err := fs.Parse(flagsFirst(args)); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: fleet run end <id> [--record-only]   (fleet run stop <id> stops the processes and says what it did)")
		}
		var err error
		if *recOnly {
			_, err = cl.EndRunRecordOnly(fs.Arg(0))
		} else {
			_, err = cl.SetRun(fs.Arg(0), nil, true)
		}
		return err
	case "prune", "rm", "remove":
		fs := flag.NewFlagSet("run "+sub, flag.ContinueOnError)
		dry := fs.Bool("dry-run", false, "only list the records that would be deleted")
		stale := fs.Bool("stale", false, "prune only: mark the active runs that look dead (engine silent, nothing running) lost and ended; stops nothing, deletes nothing")
		if err := fs.Parse(flagsFirst(args)); err != nil {
			return err
		}
		id := ""
		if *stale {
			if sub != "prune" || fs.NArg() != 0 {
				return fmt.Errorf("usage: fleet run prune --stale [--dry-run]")
			}
			lost, err := cl.EndStaleRuns(*dry)
			if err != nil {
				return err
			}
			fmt.Println(formatRunsLost(lost, *dry))
			return nil
		}
		if sub == "prune" {
			if fs.NArg() != 0 {
				return fmt.Errorf("usage: fleet run prune [--dry-run]")
			}
		} else {
			if fs.NArg() != 1 {
				return fmt.Errorf("usage: fleet run rm <id> [--dry-run]")
			}
			if id = fs.Arg(0); id == "" {
				return fmt.Errorf("usage: fleet run rm <id> [--dry-run]")
			}
		}
		gone, err := cl.DeleteRuns(id, *dry)
		if err != nil {
			return err
		}
		fmt.Println(formatRunsRemoved(gone, *dry))
		return nil
	}
	return fmt.Errorf("usage: fleet run [list | show <id> | bind <id> <endpoint> | stop <id> | end <id> [--record-only] | rm <id> | prune [--stale]] [--dry-run]")
}

// formatRunsLost says which stale runs were (or, with dry, would be) marked lost.
func formatRunsLost(lost []server.Run, dry bool) string {
	if len(lost) == 0 {
		return "no stale runs"
	}
	verb := "marked lost"
	if dry {
		verb = "would be marked lost"
	}
	var b strings.Builder
	for _, r := range lost {
		fmt.Fprintf(&b, "%s %s (%s): %s\n", verb, r.ID, r.Model, r.Stale)
	}
	b.WriteString("the records stay: fleet run prune removes ended ones; no process was touched")
	return b.String()
}

// formatRunsRemoved says what `fleet run rm|prune` deleted (or would, with --dry-run).
func formatRunsRemoved(gone []server.Run, dry bool) string {
	if len(gone) == 0 {
		return "nothing to remove (only ended runs are removed; active ones are kept)"
	}
	verb := "removed"
	if dry {
		verb = "would remove"
	}
	var b strings.Builder
	for _, r := range gone {
		fmt.Fprintf(&b, "  %s  %s  ended %s\n", r.ID, r.Model, r.Ended.Format("2006-01-02 15:04"))
	}
	fmt.Fprintf(&b, "%s %d ended run record(s); nothing was stopped", verb, len(gone))
	return b.String()
}

package client

import (
	"fmt"
	"time"

	"github.com/tevfik/fleet-router/internal/plan"
	"github.com/tevfik/fleet-router/internal/server"
)

// fetcher is what FetchPlan needs of the daemon (the Client, or a fake in tests).
type fetcher interface {
	StoreAll() ([]server.NodeStore, error)
	StorePull(node, from, sha string) error
	StoreFill(node, from, sha, blocks string, output bool) error
}

// FetchPlan makes a plan's candidate runnable: the master gets the file when it lacks it, then every worker
// fills its worker cache with the blocks the plan gives it. progress (may be nil) is told what is going on.
// It waits for each step, and stops at the first failure.
func (c *Client) FetchPlan(res plan.Result, pick int, progress func(string)) error {
	return fetchPlan(c, res, pick, progress, 1500*time.Millisecond, 6*time.Hour)
}

func fetchPlan(f fetcher, res plan.Result, pick int, progress func(string), every, limit time.Duration) error {
	say := func(s string) {
		if progress != nil {
			progress(s)
		}
	}
	if res.SHA256 == "" || !res.Known {
		return fmt.Errorf("the plan is not for a registered file: fleet store add <file> on the machine that has it, then plan again")
	}
	if pick < 1 || pick > len(res.Candidates) {
		return fmt.Errorf("the plan has %d candidate(s); there is no number %d", len(res.Candidates), pick)
	}
	cand := res.Candidates[pick-1]
	holds := func(node string) (bool, error) {
		all, err := f.StoreAll()
		if err != nil {
			return false, err
		}
		for _, n := range all {
			if n.Node != node || n.Store == nil {
				continue
			}
			for _, e := range n.Store.Files {
				if e.SHA256 == res.SHA256 && e.Complete {
					return true, nil
				}
			}
		}
		return false, nil
	}
	if ok, err := holds(cand.Master); err != nil {
		return err
	} else if !ok {
		say("fetching the file to " + cand.Master)
		after := lastTransfer(f, cand.Master)
		if err := f.StorePull(cand.Master, "", res.SHA256); err != nil {
			return fmt.Errorf("%s: %w", cand.Master, err)
		}
		if err := waitIdle(f, cand.Master, after, every, limit, say); err != nil {
			return err
		}
		if ok, _ := holds(cand.Master); !ok {
			return fmt.Errorf("%s still lacks the file after the transfer", cand.Master)
		}
	}
	type step struct {
		node  string
		after int
	}
	var started []step
	for _, p := range cand.Parts {
		if p.Node == cand.Master || p.Layers == 0 {
			continue
		}
		say(fmt.Sprintf("filling the worker cache of %s with blocks %s", p.Node, p.Blocks()))
		after := lastTransfer(f, p.Node)
		if err := f.StoreFill(p.Node, cand.Master, res.SHA256, p.Blocks(), p.Output); err != nil {
			return fmt.Errorf("%s: %w", p.Node, err)
		}
		started = append(started, step{p.Node, after})
	}
	for _, st := range started { // the machines work side by side; wait for each
		if err := waitIdle(f, st.node, st.after, every, limit, say); err != nil {
			return err
		}
	}
	say("done")
	return nil
}

// lastTransfer is the number of the newest transfer a machine has listed (0: none).
func lastTransfer(f fetcher, node string) int {
	all, _ := f.StoreAll()
	id := 0
	for _, n := range all {
		if n.Node == node && n.Store != nil {
			for _, t := range n.Store.Transfers {
				id = max(id, t.ID)
			}
		}
	}
	return id
}

// waitIdle waits until the transfer a machine was asked to start (a number above after) has finished, and fails
// when it failed. A request that started no transfer (everything was there already) counts as done after a
// short grace.
func waitIdle(f fetcher, node string, after int, every, limit time.Duration, say func(string)) error {
	start := time.Now()
	deadline := start.Add(limit)
	grace := 4 * every
	for {
		all, err := f.StoreAll()
		if err != nil {
			return err
		}
		var mine *server.NodeStore
		for i := range all {
			if all[i].Node == node {
				mine = &all[i]
			}
		}
		if mine == nil || mine.Store == nil {
			return fmt.Errorf("cannot see the transfers of %s", node)
		}
		var fresh []string
		running, failed := false, ""
		for _, t := range mine.Store.Transfers {
			if t.ID <= after {
				continue
			}
			fresh = append(fresh, t.What)
			if t.Running {
				running = true
				say(fmt.Sprintf("%s: %s %s %d%%", node, t.Kind, t.What, t.Done*100/max(t.Total, 1)))
			} else if t.Err != "" {
				failed = t.Err
			}
		}
		switch {
		case running:
		case failed != "":
			return fmt.Errorf("%s: %s", node, failed)
		case len(fresh) > 0, time.Since(start) > grace:
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s: the transfer takes too long", node)
		}
		time.Sleep(every)
	}
}

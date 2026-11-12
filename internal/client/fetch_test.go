package client

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/plan"
	"github.com/tevfik/fleet-router/internal/server"
	"github.com/tevfik/fleet-router/internal/store"
)

// fakeFleet is what FetchPlan talks to: machines with files, and transfers that take a few polls.
type fakeFleet struct {
	mu     sync.Mutex
	nodes  map[string]*server.NodeStore
	log    []string
	nextID int
	// behaviour
	failFill  map[string]string // node → the error its transfer ends with
	rejectAdd string            // StorePull/StoreFill fail at once with this
	polls     map[string]int    // node → polls left before its running transfer finishes
	noTrans   bool              // requests start no transfer (everything already there)
}

func newFleet(sha string, has ...string) *fakeFleet {
	f := &fakeFleet{nodes: map[string]*server.NodeStore{}, failFill: map[string]string{}, polls: map[string]int{}}
	for _, n := range []string{"dino", "p1", "vec"} {
		f.nodes[n] = &server.NodeStore{Node: n, Store: &store.Inventory{Files: []store.Entry{}}}
	}
	for _, n := range has {
		f.nodes[n].Store.Files = append(f.nodes[n].Store.Files, store.Entry{SHA256: sha, Name: "m", Complete: true})
	}
	return f
}

func (f *fakeFleet) StoreAll() ([]server.NodeStore, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []server.NodeStore
	for _, n := range []string{"dino", "p1", "vec"} {
		ns := *f.nodes[n]
		inv := *ns.Store
		inv.Transfers = append([]store.Transfer{}, inv.Transfers...)
		for i := range inv.Transfers {
			t := &f.nodes[n].Store.Transfers[i]
			if t.Running {
				if f.polls[n] > 0 {
					f.polls[n]--
					t.Done = t.Total / 2
				} else {
					t.Running, t.Done = false, t.Total
					if e := f.failFill[n]; e != "" {
						t.Err = e
					}
				}
				inv.Transfers[i] = *t
			}
		}
		ns.Store = &inv
		out = append(out, ns)
	}
	return out, nil
}

func (f *fakeFleet) start(node, kind, what string) {
	if f.noTrans {
		return
	}
	n := f.nodes[node]
	f.nextID++
	n.Store.Transfers = append([]store.Transfer{{ID: f.nextID, Kind: kind, What: what, Total: 100, Running: true}}, n.Store.Transfers...)
}

func (f *fakeFleet) StorePull(node, from, sha string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, fmt.Sprintf("pull %s from=%q", node, from))
	if f.rejectAdd != "" {
		return fmt.Errorf("%s", f.rejectAdd)
	}
	f.start(node, "pull", "m")
	f.nodes[node].Store.Files = append(f.nodes[node].Store.Files, store.Entry{SHA256: sha, Name: "m", Complete: f.failFill[node] == ""})
	return nil
}

func (f *fakeFleet) StoreFill(node, from, sha, blocks string, output bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, fmt.Sprintf("fill %s from=%s blocks=%s output=%v", node, from, blocks, output))
	if f.rejectAdd != "" {
		return fmt.Errorf("%s", f.rejectAdd)
	}
	f.start(node, "fill", "m: tensors")
	return nil
}

func planFor(sha string) plan.Result {
	return plan.Result{Model: "m", SHA256: sha, Known: true, Candidates: []plan.Candidate{
		{Kind: "split", Master: "dino", Parts: []plan.Part{{Node: "dino", First: 0, Layers: 40}, {Node: "p1", First: 40, Layers: 4}, {Node: "vec", First: 44, Layers: 4, Output: true}}},
		{Kind: "single", Master: "p1", Parts: []plan.Part{{Node: "p1", Layers: 48}}},
	}}
}

const fast = 6 * time.Millisecond

func TestFetchPlan_MasterHasTheFile_WorkersFill(t *testing.T) {
	sha := strings.Repeat("a", 64)
	f := newFleet(sha, "dino")
	f.polls["p1"], f.polls["vec"] = 2, 1
	var said []string
	if err := fetchPlan(f, planFor(sha), 1, func(s string) { said = append(said, s) }, fast, time.Minute); err != nil {
		t.Fatal(err)
	}
	want := []string{`fill p1 from=dino blocks=40-43 output=false`, `fill vec from=dino blocks=44-47 output=true`}
	if strings.Join(f.log, "|") != strings.Join(want, "|") {
		t.Fatalf("%v", f.log)
	}
	if said[len(said)-1] != "done" || !strings.Contains(strings.Join(said, "\n"), "p1: fill") {
		t.Fatalf("%v", said)
	}
}

func TestFetchPlan_MasterLacksTheFile_PullsItFirst(t *testing.T) {
	sha := strings.Repeat("b", 64)
	f := newFleet(sha, "vec") // another machine has it
	f.polls["dino"] = 1
	if err := fetchPlan(f, planFor(sha), 1, nil, fast, time.Minute); err != nil {
		t.Fatal(err)
	}
	if len(f.log) != 3 || f.log[0] != `pull dino from=""` || !strings.HasPrefix(f.log[1], "fill p1 from=dino") {
		t.Fatalf("the file reaches the master before the workers fill from it: %v", f.log)
	}
}

func TestFetchPlan_SingleMachineCandidate(t *testing.T) {
	sha := strings.Repeat("c", 64)
	f := newFleet(sha, "dino")
	if err := fetchPlan(f, planFor(sha), 2, nil, fast, time.Minute); err != nil {
		t.Fatal(err)
	}
	if len(f.log) != 1 || f.log[0] != `pull p1 from=""` {
		t.Fatalf("a single machine only needs the file: %v", f.log)
	}
}

func TestFetchPlan_FailuresStopIt(t *testing.T) {
	sha := strings.Repeat("d", 64)
	// a worker's transfer fails
	f := newFleet(sha, "dino")
	f.failFill["vec"] = "chunk 3 does not match the manifest"
	err := fetchPlan(f, planFor(sha), 1, nil, fast, time.Minute)
	if err == nil || !strings.Contains(err.Error(), "vec: chunk 3 does not match") {
		t.Fatalf("%v", err)
	}
	// the daemon refuses
	f = newFleet(sha, "dino")
	f.rejectAdd = "another transfer is running"
	if err := fetchPlan(f, planFor(sha), 1, nil, fast, time.Minute); err == nil || !strings.Contains(err.Error(), "p1: another transfer") {
		t.Fatalf("%v", err)
	}
	// the master's pull fails
	f = newFleet(sha, "vec")
	f.failFill["dino"] = "no space"
	if err := fetchPlan(f, planFor(sha), 1, nil, fast, time.Minute); err == nil || !strings.Contains(err.Error(), "no space") {
		t.Fatalf("%v", err)
	}
	// a transfer that never ends
	f = newFleet(sha, "dino")
	f.polls["p1"] = 1 << 30
	if err := fetchPlan(f, planFor(sha), 1, nil, fast, 80*time.Millisecond); err == nil || !strings.Contains(err.Error(), "takes too long") {
		t.Fatalf("%v", err)
	}
}

func TestFetchPlan_RefusesWhatItCannotDo(t *testing.T) {
	sha := strings.Repeat("e", 64)
	f := newFleet(sha, "dino")
	unknown := planFor(sha)
	unknown.Known = false
	if err := fetchPlan(f, unknown, 1, nil, fast, time.Minute); err == nil || !strings.Contains(err.Error(), "registered file") {
		t.Fatalf("%v", err)
	}
	for _, n := range []int{0, 3} {
		if err := fetchPlan(f, planFor(sha), n, nil, fast, time.Minute); err == nil || !strings.Contains(err.Error(), "no number") {
			t.Fatalf("pick %d: %v", n, err)
		}
	}
	if len(f.log) != 0 {
		t.Fatalf("nothing was asked of the daemon: %v", f.log)
	}
}

func TestFetchPlan_RequestThatStartsNoTransferIsDoneAfterAGrace(t *testing.T) {
	sha := strings.Repeat("f", 64)
	f := newFleet(sha, "dino")
	f.noTrans = true // the workers' caches already hold everything
	t0 := time.Now()
	if err := fetchPlan(f, planFor(sha), 1, nil, fast, time.Minute); err != nil {
		t.Fatal(err)
	}
	if time.Since(t0) > 2*time.Second {
		t.Fatal("it must not wait for a transfer that never started")
	}
}

func TestFetchPlan_AStaleFailureIsNotThisTransfers(t *testing.T) {
	sha := strings.Repeat("9", 64)
	f := newFleet(sha, "dino")
	f.nodes["p1"].Store.Transfers = []store.Transfer{{ID: 7, Kind: "fill", Err: "an old failure"}}
	f.nextID = 7
	if err := fetchPlan(f, planFor(sha), 1, nil, fast, time.Minute); err != nil {
		t.Fatalf("only transfers newer than the request count: %v", err)
	}
}

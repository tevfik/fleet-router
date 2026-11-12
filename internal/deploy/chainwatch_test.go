package deploy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
)

// stageRec records what the supervisor does to stages.
type stageRec struct {
	log []string
}

func (r *stageRec) Cleanup(node string, port int) error {
	r.log = append(r.log, fmt.Sprintf("cleanup %s:%d", node, port))
	return nil
}
func (r *stageRec) Launch(s StageLaunch) error {
	r.log = append(r.log, fmt.Sprintf("launch %d", s.Index))
	return nil
}
func (r *stageRec) WaitReady(s StageLaunch, _ time.Duration) error {
	r.log = append(r.log, fmt.Sprintf("ready %d", s.Index))
	return nil
}
func (r *stageRec) Exists(string, string) (bool, error) { return true, nil }

type chainDog struct {
	w      *Watchdog
	health []ChainHealth // one per look; the last repeats
	alive  map[int]bool
	run    *stageRec
	now    time.Time
}

func newChainDog(t *testing.T) *chainDog {
	t.Helper()
	cfg := config.Default()
	cfg.Endpoints = []config.Endpoint{
		{Name: "big", Node: "p1", URL: "http://10.0.0.1:52310/v1", Group: &config.Group{Engine: "chain", Workers: []string{"vec"}}},
		{Name: "ring-a", URL: "http://127.0.0.1:1/v1", Group: &config.Group{Engine: "llama-ring", StatsPort: 50155}},
	}
	d := &chainDog{alive: map[int]bool{0: true, 1: true}, run: &stageRec{}, now: time.Now()}
	w := NewWatchdog(cfg)
	w.logFile = filepath.Join(t.TempDir(), "watchdog.log")
	w.ChainsOnly = true
	w.Now = func() time.Time { return d.now }
	w.Redeploy = func(context.Context, config.Endpoint) error { t.Fatal("a chain is never redeployed whole"); return nil }
	w.Failed = func(context.Context, config.Endpoint) ([]string, error) {
		t.Fatal("rings are not watched (ChainsOnly)")
		return nil, nil
	}
	w.ChainRecord = func(ep string) (*DeployResult, error) {
		if ep != "big" {
			return nil, fmt.Errorf("none")
		}
		return &DeployResult{EndpointName: "big", Engine: "chain", Stages: []StageLaunch{
			{Index: 0, Node: "p1", Port: 52310}, {Index: 1, Node: "vec", Port: 5600}}}, nil
	}
	w.ChainHealth = func(context.Context, config.Endpoint) ChainHealth {
		h := d.health[0]
		if len(d.health) > 1 {
			d.health = d.health[1:]
		}
		return h
	}
	w.StageAlive = func(s StageLaunch) bool { return d.alive[s.Index] }
	w.Stages = d.run
	d.w = w
	return d
}

func (d *chainDog) looks(n int) {
	for i := 0; i < n; i++ {
		d.w.CheckOnce(context.Background())
	}
}

// The last stage died (its machine rebooted): after two bad looks only that stage is started again; the driver,
// which still holds its weights, is left alone.
func TestChainWatchRestartsOnlyTheDeadStage(t *testing.T) {
	d := newChainDog(t)
	d.health = []ChainHealth{ChainOK, ChainNextDown, ChainNextDown}
	d.alive[1] = false
	d.looks(2)
	if len(d.run.log) != 0 {
		t.Fatalf("one bad look must not restart anything: %v", d.run.log)
	}
	d.looks(1)
	if got := strings.Join(d.run.log, ","); got != "cleanup vec:5600,launch 1,ready 1" {
		t.Fatalf("got %s", got)
	}
}

// The driver died and the last stage is fine: only stage 0 starts again.
func TestChainWatchRestartsTheDriver(t *testing.T) {
	d := newChainDog(t)
	d.health = []ChainHealth{ChainOK, ChainGone}
	d.alive[0] = false
	d.looks(3)
	if got := strings.Join(d.run.log, ","); got != "cleanup p1:52310,launch 0,ready 0" {
		t.Fatalf("got %s", got)
	}
}

// Both died: the last stage first (the driver connects to it when it starts).
func TestChainWatchRestartsLastFirst(t *testing.T) {
	d := newChainDog(t)
	d.health = []ChainHealth{ChainOK, ChainGone}
	d.alive[0], d.alive[1] = false, false
	d.looks(3)
	if got := strings.Join(d.run.log, ","); got != "cleanup vec:5600,launch 1,ready 1,cleanup p1:52310,launch 0,ready 0" {
		t.Fatalf("got %s", got)
	}
}

// Never seen healthy (stopped before this daemon started, or parked): not brought back.
func TestChainWatchNeedsToHaveSeenItUp(t *testing.T) {
	d := newChainDog(t)
	d.health = []ChainHealth{ChainGone}
	d.alive[0], d.alive[1] = false, false
	d.looks(4)
	if len(d.run.log) != 0 {
		t.Fatalf("%v", d.run.log)
	}
}

// Unhealthy while every stage listens (the network between them): reported, nothing restarted.
func TestChainWatchLeavesLiveStagesAlone(t *testing.T) {
	d := newChainDog(t)
	d.health = []ChainHealth{ChainOK, ChainNextDown}
	d.looks(4)
	if len(d.run.log) != 0 {
		t.Fatalf("%v", d.run.log)
	}
	b, _ := os.ReadFile(d.w.logFile)
	if !strings.Contains(string(b), "every stage listens") {
		t.Fatalf("log:\n%s", b)
	}
}

// The restart limit and cooldown hold for chains too.
func TestChainWatchCooldownAndLimit(t *testing.T) {
	d := newChainDog(t)
	d.health = []ChainHealth{ChainOK, ChainNextDown}
	d.alive[1] = false
	d.w.MaxRestarts = 2
	d.looks(3) // restart 1
	d.looks(2) // within cooldown: postponed
	if n := strings.Count(strings.Join(d.run.log, ","), "launch 1"); n != 1 {
		t.Fatalf("cooldown: %d launches", n)
	}
	d.now = d.now.Add(6 * time.Minute)
	d.looks(2) // restart 2
	d.now = d.now.Add(6 * time.Minute)
	d.looks(2) // limit reached
	if n := strings.Count(strings.Join(d.run.log, ","), "launch 1"); n != 2 {
		t.Fatalf("limit: %d launches", n)
	}
}

// A record without stage commands (an older fleet) or a stopped run is not supervised.
func TestLastChainRecord(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	dir := filepath.Join(os.Getenv("XDG_CACHE_HOME"), "fleet", "runs")
	os.MkdirAll(dir, 0o755)
	write := func(name, body string) { os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644) }
	write("run-1.json", `{"endpoint_name":"big","engine":"chain","status":"running"}`)
	if _, err := LastChainRecord("big"); err == nil || !strings.Contains(err.Error(), "older fleet") {
		t.Fatalf("old record: %v", err)
	}
	write("run-2.json", `{"endpoint_name":"big","engine":"chain","status":"running","stages":[{"index":0,"node":"p1","port":1}]}`)
	if r, err := LastChainRecord("big"); err != nil || len(r.Stages) != 1 {
		t.Fatalf("%v %v", r, err)
	}
	write("run-3.json", `{"endpoint_name":"big","engine":"chain","status":"stopped","stages":[{"index":0,"node":"p1","port":1}]}`)
	if _, err := LastChainRecord("big"); err == nil {
		t.Fatal("a stopped run is not supervised")
	}
}

package deploy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
)

func ringCfg() *config.Config {
	cfg := config.Default()
	cfg.Endpoints = []config.Endpoint{
		{Name: "ring-a", URL: "http://127.0.0.1:1/v1", Group: &config.Group{Engine: "llama-ring", StatsPort: 50155}},
		{Name: "plain", URL: "http://127.0.0.1:1/v1"},
	}
	return cfg
}

func testDog(failed []string, restarts *[]string, now *time.Time) *Watchdog {
	w := NewWatchdog(ringCfg())
	w.logFile = filepath.Join(os.TempDir(), "fleet-watchdog-test.log")
	w.Failed = func(context.Context, config.Endpoint) ([]string, error) { return failed, nil }
	w.Redeploy = func(_ context.Context, ep config.Endpoint) error {
		*restarts = append(*restarts, ep.Name)
		return nil
	}
	w.Now = func() time.Time { return *now }
	w.Supervised = func(config.Endpoint) bool { return false }
	return w
}

func TestWatchdogRestartsRingWithFailedWorker(t *testing.T) {
	var got []string
	now := time.Now()
	w := testDog([]string{"127.0.0.1:50201"}, &got, &now)
	w.CheckOnce(context.Background())
	if len(got) != 1 || got[0] != "ring-a" {
		t.Fatalf("a ring with failed_workers must be redeployed (and only the ring): %v", got)
	}
}

func TestWatchdogLeavesHealthyRingAlone(t *testing.T) {
	var got []string
	now := time.Now()
	w := testDog(nil, &got, &now)
	w.CheckOnce(context.Background())
	if len(got) != 0 {
		t.Fatalf("no failed worker: nothing to restart: %v", got)
	}
}

func TestWatchdogCooldownAndGiveUp(t *testing.T) {
	var got []string
	now := time.Now()
	w := testDog([]string{"w"}, &got, &now)
	w.MaxRestarts = 2
	w.CheckOnce(context.Background())
	now = now.Add(time.Minute) // inside the cooldown
	w.CheckOnce(context.Background())
	if len(got) != 1 {
		t.Fatalf("a second restart inside the cooldown: %v", got)
	}
	now = now.Add(10 * time.Minute)
	w.CheckOnce(context.Background())
	now = now.Add(10 * time.Minute)
	w.CheckOnce(context.Background()) // over MaxRestarts
	if len(got) != 2 {
		t.Fatalf("want exactly 2 restarts (MaxRestarts), got %v", got)
	}
}

func TestParseFailedWorkers(t *testing.T) {
	f, err := parseFailedWorkers([]byte(`{"version":1,"failed_workers":["a:1","b:2"]}`))
	if err != nil || len(f) != 2 {
		t.Fatalf("%v %v", f, err)
	}
	f, err = parseFailedWorkers([]byte(`{"version":1}`)) // an older engine has no field
	if err != nil || len(f) != 0 {
		t.Fatalf("%v %v", f, err)
	}
	if _, err = parseFailedWorkers([]byte(`not json`)); err == nil {
		t.Fatal("garbage must be an error, not 'healthy'")
	}
}

func TestLastRunOptions(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", dir)
	runs := filepath.Join(dir, "fleet", "runs")
	_ = os.MkdirAll(runs, 0755)
	_ = os.WriteFile(filepath.Join(runs, "run-1000000001.json"), []byte(`{"endpoint_name":"ring-a","options":{"ModelPath":"old.gguf"}}`), 0644)
	_ = os.WriteFile(filepath.Join(runs, "run-1000000002.json"), []byte(`{"endpoint_name":"ring-a","options":{"ModelPath":"new.gguf","DryRun":true}}`), 0644)
	_ = os.WriteFile(filepath.Join(runs, "run-1000000003.json"), []byte(`{"endpoint_name":"other","options":{"ModelPath":"x.gguf"}}`), 0644)
	o, err := LastRunOptions("ring-a")
	if err != nil || o.ModelPath != "new.gguf" || o.DryRun {
		t.Fatalf("%+v %v", o, err)
	}
	if _, err := LastRunOptions("none"); err == nil {
		t.Fatal("unknown endpoint")
	}
}

func TestWatchdogRedeploysExitedSupervisedMaster(t *testing.T) {
	var got []string
	now := time.Now()
	w := testDog(nil, &got, &now) // /ring/info answers nothing failed, but the master (127.0.0.1:1) is gone
	w.Supervised = func(ep config.Endpoint) bool { return ep.Name == "ring-a" }
	w.stateOf("ring-a").seenUp = true // it was up while this watchdog looked
	w.CheckOnce(context.Background())
	if len(got) != 0 {
		t.Fatalf("one missed check is not enough: %v", got)
	}
	w.CheckOnce(context.Background())
	if len(got) != 1 || got[0] != "ring-a" {
		t.Fatalf("a supervised master that stays gone must be redeployed: %v", got)
	}
}

func TestWatchdogLeavesUnsupervisedUnreachableMasterAlone(t *testing.T) {
	var got []string
	now := time.Now()
	w := testDog(nil, &got, &now)
	for i := 0; i < 4; i++ {
		w.CheckOnce(context.Background())
	}
	if len(got) != 0 {
		t.Fatalf("a master without exit-on-fail may just be loading: %v", got)
	}
}

func TestWatchdogRedeployAsksForExitOnFail(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", dir)
	runs := filepath.Join(dir, "fleet", "runs")
	_ = os.MkdirAll(runs, 0755)
	_ = os.WriteFile(filepath.Join(runs, "run-1000000001.json"), []byte(`{"endpoint_name":"ring-a","options":{"ModelPath":"m.gguf","ExitOnFail":false}}`), 0644)
	if o, _ := LastRunOptions("ring-a"); o == nil || o.ExitOnFail {
		t.Fatalf("setup: %+v", o)
	}
	_ = os.WriteFile(filepath.Join(runs, "run-1000000002.json"), []byte(`{"endpoint_name":"ring-a","options":{"ModelPath":"m.gguf","ExitOnFail":true}}`), 0644)
	w := NewWatchdog(ringCfg())
	if !w.Supervised(config.Endpoint{Name: "ring-a"}) {
		t.Fatal("an endpoint whose last run had ExitOnFail is supervised")
	}
}

func TestParseFailedWorkers_Report(t *testing.T) {
	f, _ := parseFailedWorkers([]byte(`{"failed_workers":[],"failure_report":[{"worker":"a:1","role":"link"},{"worker":"b:2","role":"victim"}]}`))
	if len(f) != 1 || f[0] != "a:1" {
		t.Fatalf("a report without failed_workers still counts: %v", f)
	}
	f, _ = parseFailedWorkers([]byte(`{"failed_workers":[],"failure_report":[{"worker":"b:2","role":"victim"}]}`))
	if len(f) != 1 || !strings.Contains(f[0], "inconsistent report (no culprit named)") {
		t.Fatalf("victim-only must be flagged inconsistent: %v", f)
	}
	f, _ = parseFailedWorkers([]byte(`{"failed_workers":["c:3"],"failure_report":[{"worker":"b:2","role":"victim"}]}`))
	if len(f) != 1 || f[0] != "c:3" {
		t.Fatalf("failed_workers as before: %v", f)
	}
}

func TestWatchdogRedeploysOnVictimOnlyReport(t *testing.T) {
	var got []string
	now := time.Now()
	w := testDog(nil, &got, &now)
	w.Failed = func(context.Context, config.Endpoint) ([]string, error) {
		return parseFailedWorkers([]byte(`{"failure_report":[{"worker":"b:2","role":"victim"}]}`))
	}
	w.CheckOnce(context.Background())
	if len(got) != 1 || got[0] != "ring-a" {
		t.Fatalf("%v", got)
	}
}

// A ring on another machine whose LAN address is refused from here (not on the LAN) but whose master answers on its own
// machine is NOT gone: two such checks must not redeploy an exit-on-fail ring that is running.
func TestWatchdogDoesNotRedeployARingItCanOnlyReachOverSSH(t *testing.T) {
	var got []string
	now := time.Now()
	cfg := config.Default()
	cfg.Endpoints = []config.Endpoint{{Name: "far", Node: "dino", URL: "http://far.invalid:1/v1", Group: &config.Group{Engine: "llama-ring"}}}
	w := testDog(nil, &got, &now)
	w.cfg = cfg
	w.Supervised = func(config.Endpoint) bool { return true }
	asked := 0
	w.SSHHealth = func(_ context.Context, node string, port int) bool { asked++; return node == "dino" && port == 1 }
	w.CheckOnce(context.Background())
	w.CheckOnce(context.Background())
	if len(got) != 0 || asked != 2 {
		t.Fatalf("a master that answers on its own machine was redeployed (%v) or never asked (%d)", got, asked)
	}
	w.SSHHealth = func(context.Context, string, int) bool { return false } // really gone: two checks redeploy it
	w.CheckOnce(context.Background())
	w.CheckOnce(context.Background())
	if len(got) != 1 {
		t.Fatalf("a supervised master that answers nowhere must be redeployed: %v", got)
	}
}

// An endpoint registered as ssh + remote (what a deploy from outside the LAN writes) is checked on the master's machine,
// never at 127.0.0.1:<port> here.
func TestWatchdogChecksAnSSHEndpointOnTheMastersMachine(t *testing.T) {
	var got []string
	now := time.Now()
	cfg := config.Default()
	cfg.Endpoints = []config.Endpoint{{Name: "far", Node: "dino", SSH: "dino", Remote: "127.0.0.1:1", Group: &config.Group{Engine: "llama-ring"}}}
	w := testDog(nil, &got, &now)
	w.cfg = cfg
	w.Supervised = func(config.Endpoint) bool { return true }
	asked := 0
	w.SSHHealth = func(_ context.Context, node string, port int) bool { asked++; return node == "dino" && port == 1 }
	w.CheckOnce(context.Background())
	w.CheckOnce(context.Background())
	if len(got) != 0 || asked != 2 {
		t.Fatalf("redeployed (%v) or never asked (%d)", got, asked)
	}
	w.SSHHealth = func(context.Context, string, int) bool { return false }
	w.CheckOnce(context.Background())
	w.CheckOnce(context.Background())
	if len(got) != 1 {
		t.Fatalf("%v", got)
	}
}

// A master this watchdog never saw up (stopped on purpose, parked, never started) is not brought back, however long it
// stays gone: a freshly started watchdog once redeployed a 122B model that had been ended by hand.
func TestWatchdogDoesNotResurrectAMasterItNeverSawUp(t *testing.T) {
	var got []string
	now := time.Now()
	w := testDog(nil, &got, &now)
	w.Supervised = func(config.Endpoint) bool { return true }
	for i := 0; i < 5; i++ {
		w.CheckOnce(context.Background())
	}
	if len(got) != 0 {
		t.Fatalf("redeployed a master that was never up under this watchdog: %v", got)
	}
}

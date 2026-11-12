package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/plan"
)

// execEnv stubs everything ExecuteDeployment touches: the cache dir (run records and logs), the processes, the port
// scripts, the model metadata. Nothing is started, nothing leaves the machine.
type execEnv struct {
	t       *testing.T
	scripts []string // sh scripts run locally (port stops)
	procs   []string // processes "started": bin + args
	mu      sync.Mutex
	proc    func(logPath string) (int, <-chan error, error)
}

func newExecEnv(t *testing.T) *execEnv {
	t.Helper()
	e := &execEnv{t: t}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	stubMeta(t, &plan.Model{Name: "m", Arch: "llama", Layers: 8})
	stubNodes(t, testNodeAddrs, []string{"papa"}) // papa: a worker on this machine, with an address (no hub)
	oldS, oldP, oldPort, oldB, oldPre := localScript, startLocalProc, localPortOpen, findBackendPath, preflightDial
	localScript = func(_ context.Context, script string) ([]byte, error) {
		e.mu.Lock()
		e.scripts = append(e.scripts, script)
		e.mu.Unlock()
		return nil, nil
	}
	startLocalProc = func(_ context.Context, bin string, args, env []string, logPath string) (int, <-chan error, error) {
		e.mu.Lock()
		e.procs = append(e.procs, bin+" "+strings.Join(args, " "))
		e.mu.Unlock()
		if e.proc != nil {
			return e.proc(logPath)
		}
		return 4242, make(chan error), nil
	}
	localPortOpen = func(string) bool { return true }
	findBackendPath = func() (string, bool) { return "", false }
	t.Cleanup(func() {
		localScript, startLocalProc, localPortOpen, findBackendPath, preflightDial = oldS, oldP, oldPort, oldB, oldPre
	})
	return e
}

func (e *execEnv) stops() int {
	n := 0
	for _, s := range e.scripts {
		if strings.Contains(s, "fleet_stop_port\n") {
			n++
		}
	}
	return n
}

// record reads the run record file of the only run.
func (e *execEnv) record() DeployResult {
	e.t.Helper()
	cache, _ := os.UserCacheDir()
	files, _ := filepath.Glob(filepath.Join(cache, "fleet", "runs", "run-*.json"))
	if len(files) != 1 {
		e.t.Fatalf("run records: %v", files)
	}
	b, _ := os.ReadFile(files[0])
	var r DeployResult
	if err := json.Unmarshal(b, &r); err != nil {
		e.t.Fatal(err)
	}
	return r
}

func healthServer(t *testing.T, ok *bool) (port int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if *ok {
			w.WriteHeader(200)
			return
		}
		w.WriteHeader(503)
	}))
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().(*net.TCPAddr).Port
}

func execOpts(t *testing.T, port int, workers ...string) DeployOptions {
	return DeployOptions{ModelPath: "/m/x.gguf", MasterNode: "testhost", WorkerNodes: workers, Port: port, Ctx: 4096,
		ConfigPath: filepath.Join(t.TempDir(), "config.yaml")}
}

func TestExecuteWritesTheRecordBeforeStartingAndRegistersWhenHealthy(t *testing.T) {
	e := newExecEnv(t)
	up := true
	port := healthServer(t, &up)
	var atStart string
	e.proc = func(logPath string) (int, <-chan error, error) {
		atStart = e.record().Status // the master is being started: the record must already be there
		return 4242, make(chan error), nil
	}
	opts := execOpts(t, port)
	res, err := ExecuteDeploymentWithProgress(context.Background(), config.Default(), opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	if atStart != StatusStarting {
		t.Fatalf("record at master start: %q", atStart)
	}
	if res.Status != StatusRunning || e.record().Status != StatusRunning || e.record().MasterPID != 4242 {
		t.Fatalf("%s %+v", res.Status, e.record())
	}
	cfg, err := config.Load(opts.ConfigPath)
	if err != nil || len(cfg.Endpoints) != 1 || cfg.Endpoints[0].Name != res.EndpointName {
		t.Fatalf("%v %+v", err, cfg.Endpoints)
	}
	// the endpoint carries the stats port the master got (RING_STATS_PORT): the watchdog's failed_workers check and the
	// daemon's ring trace read it; 0 here meant neither could ever see a lost worker
	if sp := cfg.Endpoints[0].Group.StatsPort; sp != res.StatsPortUsed() || sp != 50155 || !strings.Contains(res.MasterEnv, "RING_STATS_PORT=50155") {
		t.Fatalf("endpoint stats port %d, master env %q", sp, res.MasterEnv)
	}
	// slots / ctx are what the master runs with (the strategy's -np), not the 0 of "not asked for"
	if res.Np < 1 || cfg.Endpoints[0].Slots != res.Np || cfg.Endpoints[0].Ctx != 4096 || !strings.Contains(res.MasterCmd, "-np "+strconv.Itoa(res.Np)+" ") {
		t.Fatalf("np %d, endpoint slots %d ctx %d: %s", res.Np, cfg.Endpoints[0].Slots, cfg.Endpoints[0].Ctx, res.MasterCmd)
	}
}

// A master that cannot even be started: failed record with the reason, the worker it had started is stopped.
func TestExecuteFailureStopsWhatItStartedAndKeepsTheRecord(t *testing.T) {
	e := newExecEnv(t)
	calls := 0
	e.proc = func(string) (int, <-chan error, error) {
		calls++
		if calls == 1 { // the local worker (isThisMachine "papa")
			return 777, make(chan error), nil
		}
		return 0, nil, errors.New("exec format error")
	}
	opts := execOpts(t, 18999, "papa")
	_, err := ExecuteDeploymentWithProgress(context.Background(), config.Default(), opts, nil)
	if err == nil || !strings.Contains(err.Error(), "exec format error") {
		t.Fatalf("%v", err)
	}
	r := e.record()
	if r.Status != StatusFailed || !strings.Contains(r.Error, "exec format error") || r.WorkerPIDs["papa"] != 777 {
		t.Fatalf("%+v", r)
	}
	// stop of the master port + the worker port before its start + the worker's stop in the teardown
	if got := e.stops(); got < 3 {
		t.Fatalf("worker not stopped by the teardown: %d stop scripts", got)
	}
}

// ROADMAP 2.4: Pre-flight reachability check fails fast if a worker cannot be connected to before master launch.
func TestExecuteDeployment_PreflightWorkerUnreachableFailsFast(t *testing.T) {
	e := newExecEnv(t)
	// Override preflightDial to report failure
	preflightDial = func(_ context.Context, _ string, _ bool, addr string) error {
		return fmt.Errorf("connection refused to %s", addr)
	}
	opts := execOpts(t, 50143, "papa")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := ExecuteDeploymentWithProgress(ctx, config.Default(), opts, nil)
	if err == nil {
		t.Fatal("expected preflight check error, got nil")
	}
	if !strings.Contains(err.Error(), "pre-flight check failed: worker at") {
		t.Fatalf("expected pre-flight error message, got: %v", err)
	}
	// Verify master process was never started
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, p := range e.procs {
		if strings.Contains(p, "llama-server") {
			t.Fatalf("master should not have been started on preflight failure, found: %s", p)
		}
	}
	// Record must be marked failed
	r := e.record()
	if r.Status != StatusFailed || !strings.Contains(r.Error, "pre-flight check failed") {
		t.Fatalf("unexpected record: %+v", r)
	}
}

// The wait runs out: nothing is stopped, the error says what keeps running and how to stop it, the record says "timeout".
func TestExecuteLoadTimeoutLeavesTheRingRunning(t *testing.T) {
	e := newExecEnv(t)
	down := false
	port := healthServer(t, &down)
	opts := execOpts(t, port, "papa")
	opts.LoadTimeoutS = 1
	_, err := ExecuteDeploymentWithProgress(context.Background(), config.Default(), opts, nil)
	var we *WaitError
	if !errors.As(err, &we) {
		t.Fatalf("want a WaitError, got %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"timed out after 1s", "NOT stopped, still running", "pid 4242", "worker papa", "NOT registered", "run-"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
	if r := e.record(); r.Status != StatusTimeout {
		t.Fatalf("%+v", r)
	}
	// stops: master port, worker port (before its start); none after the timeout
	if got := e.stops(); got != 2 {
		t.Fatalf("a timeout must not stop anything: %d stop scripts", got)
	}
	if cfg, err := config.Load(opts.ConfigPath); err == nil && len(cfg.Endpoints) != 0 {
		t.Fatalf("an endpoint was registered: %+v", cfg.Endpoints)
	}
}

func TestExecuteInterruptWhileLoadingLeavesItRunning(t *testing.T) {
	e := newExecEnv(t)
	down := false
	port := healthServer(t, &down)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	_, err := ExecuteDeploymentWithProgress(ctx, config.Default(), execOpts(t, port), nil)
	var we *WaitError
	if !errors.As(err, &we) || we.Reason != "interrupted" {
		t.Fatalf("%v", err)
	}
	if r := e.record(); r.Status != StatusStopped || r.MasterPID != 4242 {
		t.Fatalf("%+v", r)
	}
}

// The local master dies while it loads: no 20-minute wait; its log is in the error and the worker is stopped.
func TestExecuteMasterExitIsReportedAtOnce(t *testing.T) {
	e := newExecEnv(t)
	down := false
	port := healthServer(t, &down)
	calls := 0
	e.proc = func(logPath string) (int, <-chan error, error) {
		calls++
		if calls == 1 {
			return 777, make(chan error), nil
		}
		_ = os.WriteFile(logPath, []byte("loading...\nggml_cuda: out of memory\n"), 0o644)
		ch := make(chan error, 1)
		ch <- errors.New("exit status 1")
		return 4242, ch, nil
	}
	start := time.Now()
	_, err := ExecuteDeploymentWithProgress(context.Background(), config.Default(), execOpts(t, port, "papa"), nil)
	if err == nil || !strings.Contains(err.Error(), "out of memory") || !strings.Contains(err.Error(), "exited before it was healthy") {
		t.Fatalf("%v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("it waited")
	}
	if r := e.record(); r.Status != StatusFailed {
		t.Fatalf("%+v", r)
	}
	if e.stops() < 3 {
		t.Fatalf("worker not stopped: %d", e.stops())
	}
}

// A record of a deploy that did not come up is never what the watchdog redeploys from.
func TestLastRunOptionsSkipsRecordsThatDidNotComeUp(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	dir := runDir()
	write := func(id, status string) {
		b, _ := json.Marshal(DeployResult{RunID: id, EndpointName: "ep", Status: status, Options: &DeployOptions{ModelPath: id}})
		_ = os.WriteFile(filepath.Join(dir, id+".json"), b, 0o644)
	}
	write("run-100", "planned") // the status old versions left on healthy runs
	write("run-200", StatusFailed)
	write("run-300", StatusStarting)
	o, err := LastRunOptions("ep")
	if err != nil || o.ModelPath != "run-100" {
		t.Fatalf("%v %+v", err, o)
	}
}

// A dry run for a master on another machine must not show (or pass on) this machine's engine paths: the remote script
// resolves its own.
func TestDryRunRemoteMasterShowsNoLocalPaths(t *testing.T) {
	stubMeta(t, &plan.Model{Name: "m", Arch: "llama", Layers: 8})
	old := findBackendPath
	findBackendPath = func() (string, bool) { return "/home/laptopuser/llama-ring/build/bin/libggml-ring.so", true }
	t.Cleanup(func() { findBackendPath = old })

	remote := dry(t, DeployOptions{MasterNode: "dino"})
	if strings.Contains(remote.MasterEnv, "/home/laptopuser") || strings.Contains(remote.MasterEnv, "GGML_BACKEND_PATH") || strings.Contains(remote.MasterEnv, "LD_LIBRARY_PATH") {
		t.Fatalf("a remote master got this machine's paths: %s", remote.MasterEnv)
	}
	if !strings.Contains(remote.MasterEngine, "resolved on dino") || !strings.Contains(remote.MasterEngine, "llm-cluster/llama-ring/build/bin/llama-server") {
		t.Fatalf("%q", remote.MasterEngine)
	}
	local := dry(t, DeployOptions{MasterNode: "testhost"})
	if !strings.Contains(local.MasterEnv, "GGML_BACKEND_PATH=/home/laptopuser/llama-ring/build/bin/libggml-ring.so") || local.MasterEngine != "" {
		t.Fatalf("a local master keeps its resolved paths: %s / %q", local.MasterEnv, local.MasterEngine)
	}
}

func TestRemoteMasterScript(t *testing.T) {
	s := remoteMasterScript("export RING_SERVERS=a:1\n", "-m /m/x.gguf --port 18090", "run-1")
	// every candidate directory comes from masterEngineDirs
	for _, d := range masterEngineDirs {
		if !strings.Contains(s, d.Dir+"/llama-server") {
			t.Errorf("script lacks %s", d.Dir)
		}
	}
	if !strings.Contains(s, "$HOME/agent_ws/project/llama-ring/build/bin/llama-server") {
		t.Error("victor keeps its llama-ring checkout in agent_ws/project (singular): the master script must look there")
	}
	for _, want := range []string{"export RING_SERVERS=a:1", "FLEET_NOBIN", `SERVER_BIN="fleet ring serve"`, "FLEET_PID $PID", "FLEET_DIED", "-m /m/x.gguf --port 18090", "runs/run-1.log"} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q", want)
		}
	}
	if strings.Contains(s, `SERVER_BIN="llama-server"`) && !strings.Contains(s, "command -v llama-server") {
		t.Error("the bare llama-server fallback must be checked, not assumed (the silent failure)")
	}
	if strings.Contains(s, "/home/") {
		t.Error("no absolute home path")
	}
	// the answer is parsed with the common launch parser
	pid, status, tail := parseLaunchOutput("FLEET_PID 321\nFLEET_DIED\nbad flag\n")
	if pid != 321 || status != "FLEET_DIED" || len(tail) == 0 || tail[0] != "bad flag" {
		t.Fatalf("%d %s %v", pid, status, tail)
	}
	if err := noMasterEngineError("dino"); !strings.Contains(err.Error(), "no engine on dino") {
		t.Fatal(err)
	}
}

// The remote master's start goes through the script: no engine there is an error, not a silent failure.
func TestExecuteRemoteMasterWithoutEngineFails(t *testing.T) {
	e := newExecEnv(t)
	oldS := sshScript
	var scripts []string
	sshScript = func(_ context.Context, node, script string) ([]byte, error) {
		scripts = append(scripts, script)
		if strings.Contains(script, "SERVER_BIN") {
			return []byte("FLEET_NOBIN\n"), nil
		}
		return nil, nil
	}
	t.Cleanup(func() { sshScript = oldS })
	opts := execOpts(t, 18999)
	opts.MasterNode = "dino"
	_, err := ExecuteDeploymentWithProgress(context.Background(), config.Default(), opts, nil)
	if err == nil || !strings.Contains(err.Error(), "no engine on dino") {
		t.Fatalf("%v", err)
	}
	if r := e.record(); r.Status != StatusFailed || r.MasterNode != "dino" {
		t.Fatalf("%+v", r)
	}
}

// The master answers over ssh but not at its LAN address: the deploy says so and tags the endpoint.
func TestExecuteRemoteMasterReachableOnlyOverSSH(t *testing.T) {
	e := newExecEnv(t)
	_ = e
	oldS := sshScript
	sshScript = func(_ context.Context, node, script string) ([]byte, error) {
		switch {
		case strings.Contains(script, "SERVER_BIN"):
			return []byte("FLEET_PID 555\n"), nil
		case strings.Contains(script, "curl"):
			return []byte("200"), nil
		}
		return nil, nil
	}
	t.Cleanup(func() { sshScript = oldS })
	opts := execOpts(t, 1) // nothing answers at 10.0.1.13:1 from here
	opts.MasterNode = "dino"
	opts.LoadTimeoutS = 30
	res, err := ExecuteDeploymentWithProgress(context.Background(), config.Default(), opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Reach != "ssh-only" || res.MasterPID != 555 {
		t.Fatalf("%+v", res)
	}
	found := false
	for _, w := range res.Warnings {
		found = found || (strings.Contains(w, "cannot reach the master's address") && strings.Contains(w, "ssh -N -L"))
	}
	if !found {
		t.Fatalf("%v", res.Warnings)
	}
	cfg, _ := config.Load(opts.ConfigPath)
	if len(cfg.Endpoints) != 1 || !strings.Contains(strings.Join(cfg.Endpoints[0].Tags, ","), "lan-only") ||
		cfg.Endpoints[0].URL != "" || cfg.Endpoints[0].SSH != "dino" || cfg.Endpoints[0].Remote != "127.0.0.1:1" {
		t.Fatalf("%+v", cfg.Endpoints)
	}
}

func TestExitOnFailWarnsAboutTheBlastRadius(t *testing.T) {
	stubPlanner(t, "llama")
	on := dry(t, DeployOptions{WorkerNodes: []string{"papa"}, ExitOnFail: true})
	if !strings.Contains(on.MasterEnv, "GGML_RING_EXIT_ON_FAIL=1") || !strings.Contains(strings.Join(on.Warnings, "\n"), "WHOLE model server exit") {
		t.Fatalf("%s %v", on.MasterEnv, on.Warnings)
	}
	off := dry(t, DeployOptions{WorkerNodes: []string{"papa"}})
	if strings.Contains(off.MasterEnv, "EXIT_ON_FAIL") || strings.Contains(strings.Join(off.Warnings, "\n"), "exit-on-fail") {
		t.Fatalf("off by default: %s %v", off.MasterEnv, off.Warnings)
	}
}

// --via-hub from a local master with no hub API key here: refused before a worker is started (the stream it runs on
// this machine would die right after the worker's port was opened).
func TestExecuteViaHubLocalMasterWithoutKeyStartsNothing(t *testing.T) {
	e := newExecEnv(t)
	old := hubAPIKey
	hubAPIKey = func(*config.Config) string { return "" }
	t.Cleanup(func() { hubAPIKey = old })
	opts := execOpts(t, 18998, "dino")
	opts.ViaHub = true
	_, err := ExecuteDeploymentWithProgress(context.Background(), config.Default(), opts, nil)
	if err == nil || !strings.Contains(err.Error(), "hub API key") || !strings.Contains(err.Error(), "nothing was started") {
		t.Fatalf("want the missing key named before anything starts: %v", err)
	}
	if len(e.procs) != 0 || e.stops() != 0 {
		t.Fatalf("something was started: %v / %d stop scripts", e.procs, e.stops())
	}
	if r := e.record(); r.Status != StatusFailed {
		t.Fatalf("record %q", r.Status)
	}
}

// ssh fails while launching the remote master (a dropped connection after the script started it): the deploy tries
// to stop the master that may be running on that port before it reports the failure.
func TestExecuteRemoteMasterSSHFailureStopsWhatMayHaveStarted(t *testing.T) {
	_ = newExecEnv(t)
	oldS, oldStop := sshScript, stopMasterAfterSSHFailure
	var stopped []string
	stopMasterAfterSSHFailure = func(_ context.Context, node string, port int) error {
		stopped = append(stopped, fmt.Sprintf("%s:%d", node, port))
		return nil
	}
	sshScript = func(_ context.Context, node, script string) ([]byte, error) {
		if strings.Contains(script, "SERVER_BIN") {
			return nil, errors.New("ssh: connection reset")
		}
		return nil, nil
	}
	t.Cleanup(func() { sshScript, stopMasterAfterSSHFailure = oldS, oldStop })
	opts := execOpts(t, 18998)
	opts.MasterNode = "dino"
	_, err := ExecuteDeploymentWithProgress(context.Background(), config.Default(), opts, nil)
	if err == nil || !strings.Contains(err.Error(), "starting remote master on dino failed") {
		t.Fatalf("%v", err)
	}
	if len(stopped) != 1 || stopped[0] != "dino:18998" {
		t.Fatalf("a master that may have started must be stopped: %v", stopped)
	}
}

// A deploy on a port where another live ring of fleet's serves is refused (it would be stopped silently).
func TestDeployRefusesToStopAnotherLiveRing(t *testing.T) {
	_ = newExecEnv(t)
	old := endpointHealthy
	endpointHealthy = func(config.Endpoint) bool { return true }
	t.Cleanup(func() { endpointHealthy = old })
	cfg := config.Default()
	cfg.Endpoints = []config.Endpoint{{Name: "first", Model: "other.gguf", Node: "dino", URL: "http://10.0.1.5:18999/v1", Group: &config.Group{Engine: "llama-ring"}}}
	opts := execOpts(t, 18999)
	opts.MasterNode = "dino"
	_, err := ExecuteDeploymentWithProgress(context.Background(), cfg, opts, nil)
	if err == nil || !strings.Contains(err.Error(), "would stop it") {
		t.Fatalf("%v", err)
	}
	if !strings.Contains(err.Error(), "first") {
		t.Fatal("the other endpoint must be named")
	}
}

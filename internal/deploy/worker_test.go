package deploy

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
)

// stubNodes replaces the address sources of ResolveNodeIP (fleet config, ssh -G) and makes only the names in local
// count as this machine (plus localhost/127.0.0.1).
func stubNodes(t *testing.T, cfgAddrs map[string]string, local []string) {
	t.Helper()
	oldC, oldS, oldL := configNodeAddr, sshConfigAddr, isThisMachine
	configNodeAddr = func(n string) string { return cfgAddrs[n] }
	sshConfigAddr = func(string) string { return "" }
	isThisMachine = func(n string) bool {
		if n == "" || n == "localhost" || n == "127.0.0.1" {
			return true
		}
		for _, l := range local {
			if n == l {
				return true
			}
		}
		return false
	}
	t.Cleanup(func() { configNodeAddr, sshConfigAddr, isThisMachine = oldC, oldS, oldL })
}

func stubLocalWorker(t *testing.T, bin string, pre []string) {
	t.Helper()
	old := findLocalWorker
	findLocalWorker = func() (string, []string, string, error) { return bin, pre, filepath.Dir(bin), nil }
	t.Cleanup(func() { findLocalWorker = old })
}

type procCall struct {
	bin  string
	args []string
	env  []string
	log  string
}

// stubExec records every script and process start; nothing is run.
func stubExec(t *testing.T, sshOut string, portOpen bool, startHook func(c procCall) <-chan error) (*[]string, *[]string, *[]procCall) {
	t.Helper()
	var sshScripts, localScripts []string
	var procs []procCall
	oldS, oldL, oldP, oldO, oldR := sshScript, localScript, startLocalProc, localPortOpen, readyPoll
	sshScript = func(_ context.Context, node, script string) ([]byte, error) {
		sshScripts = append(sshScripts, node+"\n"+script)
		return []byte(sshOut), nil
	}
	localScript = func(_ context.Context, script string) ([]byte, error) {
		localScripts = append(localScripts, script)
		return nil, nil
	}
	startLocalProc = func(_ context.Context, bin string, args, env []string, logPath string) (int, <-chan error, error) {
		c := procCall{bin, args, env, logPath}
		procs = append(procs, c)
		var done <-chan error
		if startHook != nil {
			done = startHook(c)
		}
		return 4242, done, nil
	}
	localPortOpen = func(string) bool { return portOpen }
	readyPoll = time.Millisecond
	t.Cleanup(func() {
		sshScript, localScript, startLocalProc, localPortOpen, readyPoll = oldS, oldL, oldP, oldO, oldR
	})
	return &sshScripts, &localScripts, &procs
}

// Bug 1: no machine-wide killall; only this user's ring-worker/fleet on the deploy's own port is stopped.
func TestStopOnPortScriptOnlyStopsOwnWorkerOnPort(t *testing.T) {
	s := stopOnPortScript(50199, workerProcNames)
	for _, bad := range []string{"killall", "pkill", "fuser -k"} {
		if strings.Contains(s, bad) {
			t.Errorf("stop script must not use %q:\n%s", bad, s)
		}
	}
	for _, want := range []string{"fuser -n tcp 50199", "ring-worker|fleet|fleet-engine)", "[ -O /proc/$p ]", "FLEET_BUSY", "${p%%[!0-9]*}"} {
		if !strings.Contains(s, want) {
			t.Errorf("stop script lacks %q:\n%s", want, s)
		}
	}
	rs := remoteWorkerScript(50199, nil, 120)
	if strings.Contains(rs, "killall") || strings.Contains(rs, "fuser -k") {
		t.Errorf("remote start script kills by name or blindly:\n%s", rs)
	}
}

func TestStartWorkerRefusesForeignProcessOnPort(t *testing.T) {
	stubNodes(t, nil, nil)
	ssh, _, _ := stubExec(t, "FLEET_BUSY 777 python3\n", false, nil)
	_, _, err := NewRemoteWorkerManager().StartWorkerEnv(context.Background(), "dino", 50199, false, nil)
	if err == nil || !strings.Contains(err.Error(), "777 python3") || !strings.Contains(err.Error(), "not touching it") {
		t.Fatalf("want a busy-port error naming the process, got %v", err)
	}
	if len(*ssh) != 1 {
		t.Fatalf("one ssh call expected, got %d", len(*ssh))
	}
}

func TestStopWorkerUsesSafeScript(t *testing.T) {
	ssh, local, _ := stubExec(t, "", false, nil)
	m := NewRemoteWorkerManager()
	if err := m.StopWorker(context.Background(), "dino", 50199, false); err != nil {
		t.Fatal(err)
	}
	if err := m.StopWorker(context.Background(), "", 50199, true); err != nil {
		t.Fatal(err)
	}
	if len(*ssh) != 1 || len(*local) != 1 {
		t.Fatalf("ssh %d local %d", len(*ssh), len(*local))
	}
	for _, s := range append(*ssh, *local...) {
		if !strings.Contains(s, "fleet_stop_port\n") || strings.Contains(s, "fuser -k") {
			t.Errorf("unsafe stop:\n%s", s)
		}
	}
}

// Bug 2 + 6: the local branch runs the ring-worker binary itself (not `<llama-server> ring worker`), with
// LD_LIBRARY_PATH and the worker env (shard gguf), and returns the node's reachable address.
func TestStartLocalWorkerRunsRingWorkerWithEnv(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	stubNodes(t, map[string]string{"papa": "10.0.1.119"}, []string{"papa"})
	oldI := interfaceIPs // the configured address is on one of this machine's interfaces (not stale)
	interfaceIPs = func() []net.IP { return []net.IP{net.ParseIP("10.0.1.119")} }
	t.Cleanup(func() { interfaceIPs = oldI })
	stubLocalWorker(t, "/opt/ring/bin/ring-worker", nil)
	_, local, procs := stubExec(t, "", true, nil)
	addr, pid, err := NewRemoteWorkerManager().StartWorkerEnv(context.Background(), "papa", 50199, true,
		[]string{"GGML_RING_SHARD_GGUF=/models/m.gguf", "GGML_RING_SHARD_BLOCKS=0-13", "EVIL=$(x)"})
	if err != nil {
		t.Fatal(err)
	}
	if addr != "10.0.1.119:50199" || pid != 4242 {
		t.Errorf("addr %s pid %d (must not be loopback)", addr, pid)
	}
	if len(*local) != 1 || !strings.Contains((*local)[0], "fuser -n tcp 50199") {
		t.Errorf("the port must be cleared with the safe script: %v", *local)
	}
	if len(*procs) != 1 {
		t.Fatalf("one process expected, got %d", len(*procs))
	}
	c := (*procs)[0]
	if c.bin != "/opt/ring/bin/ring-worker" {
		t.Errorf("bin %s", c.bin)
	}
	if got := strings.Join(c.args, " "); got != "-H 0.0.0.0 -p 50199 -d CUDA0 -c" {
		t.Errorf("args %q", got)
	}
	env := strings.Join(c.env, "\n")
	for _, want := range []string{"LD_LIBRARY_PATH=/opt/ring/bin", "GGML_RING_SHARD_GGUF=/models/m.gguf", "GGML_RING_SHARD_BLOCKS=0-13"} {
		if !strings.Contains(env, want) {
			t.Errorf("env lacks %s", want)
		}
	}
	if strings.Contains(env, "EVIL") {
		t.Error("unsafe env value must be filtered the same way as on remote nodes")
	}
}

func TestStartLocalWorkerFleetEngineFallback(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	stubNodes(t, nil, nil)
	stubLocalWorker(t, "/usr/local/bin/fleet-engine", []string{"ring", "worker"})
	_, _, procs := stubExec(t, "", true, nil)
	if _, _, err := NewRemoteWorkerManager().StartWorkerEnv(context.Background(), "localhost", 50199, true, nil); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join((*procs)[0].args, " "); got != "ring worker -H 0.0.0.0 -p 50199 -d CUDA0 -c" {
		t.Errorf("args %q", got)
	}
}

// Bug 3: a worker that exits or never listens is an error carrying the log tail.
func TestStartLocalWorkerDiedReportsLog(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	stubNodes(t, nil, nil)
	stubLocalWorker(t, "/opt/ring/bin/ring-worker", nil)
	stubExec(t, "", false, func(c procCall) <-chan error {
		_ = os.WriteFile(c.log, []byte("loading\nerror: bind failed: address in use\n"), 0o644)
		ch := make(chan error, 1)
		ch <- os.ErrProcessDone
		return ch
	})
	_, _, err := NewRemoteWorkerManager().StartWorkerEnv(context.Background(), "localhost", 50199, true, nil)
	if err == nil || !strings.Contains(err.Error(), "exited before listening") || !strings.Contains(err.Error(), "bind failed") {
		t.Fatalf("got %v", err)
	}
}

func TestStartLocalWorkerTimeout(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	stubNodes(t, nil, nil)
	stubLocalWorker(t, "/opt/ring/bin/ring-worker", nil)
	stubExec(t, "", false, nil)
	m := &RemoteWorkerManager{ReadyTimeout: 30 * time.Millisecond}
	_, _, err := m.StartWorkerEnv(context.Background(), "localhost", 50199, true, nil)
	if err == nil || !strings.Contains(err.Error(), "did not listen") {
		t.Fatalf("got %v", err)
	}
}

func TestStartRemoteWorkerOutcomes(t *testing.T) {
	stubNodes(t, map[string]string{"dino": "10.0.1.13"}, nil)
	cases := []struct {
		out, wantErr string
	}{
		{"FLEET_PID 42\nFLEET_READY\n", ""},
		{"FLEET_PID 42\nFLEET_DIED\nggml: out of memory\n", "exited before listening"},
		{"FLEET_PID 42\nFLEET_TIMEOUT\nindexing shard 3/9\n", "did not listen"},
		{"FLEET_NOBIN\n", "no ring-worker on dino"},
		{"FLEET_PID 42\n", "no readiness answer"},
	}
	for _, c := range cases {
		ssh, _, _ := stubExec(t, c.out, false, nil)
		addr, _, err := NewRemoteWorkerManager().StartWorkerEnv(context.Background(), "dino", 50199, false, []string{"GGML_RING_SHARD_GGUF=/models/m.gguf"})
		if c.wantErr == "" {
			if err != nil || addr != "10.0.1.13:50199" {
				t.Errorf("%q: addr %s err %v", c.out, addr, err)
			}
		} else if err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%q: want %q, got %v", c.out, c.wantErr, err)
		}
		if c.wantErr == "exited before listening" && !strings.Contains(err.Error(), "out of memory") {
			t.Errorf("log tail missing: %v", err)
		}
		// Bug 6: the shard env is on the remote start line
		if s := (*ssh)[0]; !strings.Contains(s, "env LD_LIBRARY_PATH=\"$RW_DIR:${LD_LIBRARY_PATH:-}\" GGML_RING_SHARD_GGUF=/models/m.gguf \"$RW\" $RW_PRE -H 0.0.0.0 -p 50199") {
			t.Errorf("start line without the worker env:\n%s", s)
		}
	}
}

func TestRemoteReadyTimeoutIsConfigurable(t *testing.T) {
	if !strings.Contains(remoteWorkerScript(50199, nil, 120), "-ge 120 ]") {
		t.Error("default wait must be 120 s")
	}
	stubNodes(t, nil, nil)
	ssh, _, _ := stubExec(t, "FLEET_PID 1\nFLEET_READY\n", false, nil)
	m := &RemoteWorkerManager{ReadyTimeout: 300 * time.Second}
	if _, _, err := m.StartWorkerEnv(context.Background(), "dino", 50199, false, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains((*ssh)[0], "-ge 300 ]") {
		t.Error("ReadyTimeout must reach the remote wait loop")
	}
}

// Bug 4: address sources in order; a machine nobody declared keeps its name (there is no built-in table).
func TestResolveNodeIPOrder(t *testing.T) {
	oldC, oldS := configNodeAddr, sshConfigAddr
	t.Cleanup(func() { configNodeAddr, sshConfigAddr = oldC, oldS })
	cfg := config.Default()
	cfg.Nodes = map[string]config.NodeSpec{"dino": {Addr: "10.1.1.13"}}
	configNodeAddr = func(n string) string { return NodeAddrFromConfig(cfg, n) }
	sshConfigAddr = func(n string) string { return map[string]string{"dino": "10.9.9.9", "box": "10.2.2.2"}[n] }

	for in, want := range map[string]string{
		"dino":        "10.1.1.13", // the nodes: section first
		"DINO":        "10.1.1.13",
		"box":         "10.2.2.2", // then ssh config
		"papa":        "papa",     // nothing declared: the name (no built-in table)
		"bravo":       "bravo",
		"victor":      "victor", // remote site: no direct IP
		"10.0.0.7":    "10.0.0.7",
		"unknownhost": "unknownhost",
	} {
		if got := ResolveNodeIP(in); got != want {
			t.Errorf("ResolveNodeIP(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestParseSSHConfigAddr(t *testing.T) {
	for out, want := range map[string]string{
		"user x\nhostname 10.0.1.13\nport 22\nproxyjump none\n": "10.0.1.13",
		"hostname 10.0.1.47\nproxyjump bravo\n":                 "", // the far site's LAN
		"hostname 10.0.0.2\nproxycommand ssh -W %h:%p hub\n":    "",
		"hostname dino.lan\n":                                   "",
		"hostname 127.0.0.1\n":                                  "",
	} {
		if got := parseSSHConfigAddr(out); got != want {
			t.Errorf("%q: %q want %q", out, got, want)
		}
	}
}

func TestNodeAddrFromConfig(t *testing.T) {
	cfg := config.Default()
	cfg.Endpoints = []config.Endpoint{
		{Name: "a", Node: "dino", URL: "http://127.0.0.1:8080/v1"},           // loopback: no
		{Name: "b", Node: "victor", SSH: "victor", Remote: "127.0.0.1:8080"}, // ssh: no
		{Name: "c", Node: "dino", URL: "http://bravo:4711/v1", Peer: "hub"},  // peer: no
		{Name: "d", Node: "dino", URL: "http://10.0.1.13:18090/v1"},          // yes
		{Name: "e", Node: "papa", URL: "http://papa.local:8080/v1"},          // not an IP
	}
	if got := NodeAddrFromConfig(cfg, "dino"); got != "10.0.1.13" {
		t.Errorf("dino: %q", got)
	}
	for _, n := range []string{"victor", "papa", "nope"} {
		if got := NodeAddrFromConfig(cfg, n); got != "" {
			t.Errorf("%s: %q", n, got)
		}
	}
}

func TestPlanWarnsAboutWorkerWithoutDirectAddress(t *testing.T) {
	stubPlanner(t, "llama")
	stubNodes(t, map[string]string{"papa": "10.0.1.119", "dino": "10.0.1.13"}, nil)
	res := dry(t, DeployOptions{WorkerNodes: []string{"victor"}, NoHub: true}) // without --no-hub or --via-hub: refused (hubstream_test.go)
	if !strings.Contains(res.MasterEnv, "RING_SERVERS=victor:50152") {
		t.Errorf("victor must not get a made-up LAN IP: %s", res.MasterEnv)
	}
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "worker victor has no known direct address") {
		t.Errorf("warning expected: %v", res.Warnings)
	}
	res = dry(t, DeployOptions{WorkerNodes: []string{"papa"}})
	if strings.Contains(strings.Join(res.Warnings, "\n"), "no known direct address") {
		t.Errorf("papa has an IP: %v", res.Warnings)
	}
}

// Bug 5: the plan's worker command is the line StartWorkerEnv runs, with the worker env.
func TestWorkerCmdsAreTheRealCommand(t *testing.T) {
	stubPlanner(t, "llama")
	stubSizes(t, map[string]int64{"dino:/models/ornith-122b.gguf": 100, "victor:/models/ornith-122b.gguf": 100})
	res := dry(t, DeployOptions{WorkerNodes: []string{"victor"}, LocalWeights: true, TensorSplit: "36,12"})
	cmd := res.WorkerCmds["victor"]
	if strings.Contains(cmd, "fleet ring worker") {
		t.Errorf("stale command shown: %s", cmd)
	}
	line, _, _ := strings.Cut(cmd, "   # on victor")
	if !strings.Contains(line, "GGML_RING_SHARD_GGUF=/models/ornith-122b.gguf") || !strings.Contains(line, "GGML_RING_SHARD_BLOCKS=") {
		t.Errorf("worker env missing from the shown command: %s", cmd)
	}
	if script := remoteWorkerScript(res.WorkerPorts["victor"], strings.Fields(res.WorkerEnv["victor"]), 120); !strings.Contains(script, "nohup $FLEET_DETACH "+line+" >") {
		t.Errorf("shown command %q is not what the remote script runs:\n%s", line, script)
	}
	if res.WorkerPorts["victor"] != 50152 {
		t.Errorf("ports: %v", res.WorkerPorts)
	}
}

func TestWorkerCmdsForThisMachineShowResolvedBinary(t *testing.T) {
	stubPlanner(t, "llama")
	stubNodes(t, testNodeAddrs, []string{"papa"})
	res := dry(t, DeployOptions{WorkerNodes: []string{"papa"}, IOTimeoutS: 30})
	want := `env LD_LIBRARY_PATH="/opt/ring/bin:${LD_LIBRARY_PATH:-}" GGML_RING_IO_TIMEOUT_S=30 /opt/ring/bin/ring-worker -H 0.0.0.0 -p 50152 -d CUDA0 -c`
	if got := res.WorkerCmds["papa"]; got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestStartOrderFollowsRingPorts(t *testing.T) {
	r := &DeployResult{WorkerNodes: []string{"a", "b"}, WorkerPorts: map[string]int{"c": 50153, "b": 50152}}
	if got := strings.Join(r.startOrder(), ","); got != "b,c" {
		t.Errorf("order %s", got)
	}
	if r.workerPort("c") != 50153 {
		t.Error("port of c")
	}
	r = &DeployResult{WorkerNodes: []string{"a", "b"}}
	if got := strings.Join(r.startOrder(), ","); got != "a,b" || r.workerPort("b") != 50153 {
		t.Errorf("fallback order %s / port %d", got, r.workerPort("b"))
	}
}

package deploy

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tevfik/fleet-router/internal/ringengine"
)

// DefaultWorkerReady is how long a started worker gets to listen on its port. A worker that indexes a large
// --shard-gguf file (--local-weights) takes well over the few seconds an empty one needs.
const DefaultWorkerReady = 120 * time.Second

// workerCandidateDirs are where a ring-worker build is looked for on a remote node, in order (shell form, $HOME
// expands there). The local resolution (defaultFindLocalWorker) walks the same directories through ringengine.
var workerCandidateDirs = []string{
	"$HOME/llm-cluster/llama-ring/build/bin",
	"$HOME/agent_ws/projects/llama-ring/build/bin",
	"$HOME/agent_ws/project/llama-ring/build/bin",
	"$HOME/.local/bin",
}

// Process names (/proc/PID/comm) fleet may stop when they hold a port it is about to use: only its own kind.
var (
	workerProcNames = []string{"ring-worker", "fleet", "fleet-engine"}
	masterProcNames = []string{"llama-server", "fleet", "fleet-engine"}
)

// workerLaunch is one ring-worker command line. The same value renders the plan's WorkerCmds entry, the remote
// script's start line and the local exec arguments, so what is shown cannot drift from what runs.
type workerLaunch struct {
	Bin    string   // executable: a path, or a shell expression inside the remote script
	Pre    []string // arguments before the worker options ("ring worker" for a fleet-engine binary)
	LibDir string   // prepended to LD_LIBRARY_PATH ("" = left alone)
	Port   int
	Env    []string // KEY=VALUE for the worker, already filtered (filterWorkerEnv)
	// Devices is the worker's -d list ("": CUDA0). A worker that keeps MoE experts in its RAM (--worker-cpu-experts)
	// runs with CUDA0,CPU and so shows the master two devices, in this order.
	Devices string
}

// defaultWorkerDevices is the -d of a worker with one GPU; cpuExpertDevices that of a worker that also offers its CPU.
const (
	defaultWorkerDevices = "CUDA0"
	cpuExpertDevices     = "CUDA0,CPU"
)

func workerOptions(port int, devices string) []string {
	if devices == "" {
		devices = defaultWorkerDevices
	}
	return []string{"-H", "0.0.0.0", "-p", strconv.Itoa(port), "-d", devices, "-c"}
}

// Args are the arguments after Bin.
func (l workerLaunch) Args() []string {
	return append(append([]string{}, l.Pre...), workerOptions(l.Port, l.Devices)...)
}

// EnvAdd is what the worker gets on top of the environment it inherits; ldPrev is the inherited LD_LIBRARY_PATH
// (a shell expression in the remote script).
func (l workerLaunch) EnvAdd(ldPrev string) []string {
	var out []string
	if l.LibDir != "" {
		out = append(out, "LD_LIBRARY_PATH="+libPathWith(l.LibDir, ldPrev))
	}
	return append(out, l.Env...)
}

// ShellLine is the command as a shell runs it: env LD_LIBRARY_PATH=… KEY=VALUE… BIN ARGS.
func (l workerLaunch) ShellLine() string {
	parts := []string{"env"}
	for _, kv := range l.EnvAdd("${LD_LIBRARY_PATH:-}") {
		if strings.HasPrefix(kv, "LD_LIBRARY_PATH=") {
			kv = `LD_LIBRARY_PATH="` + strings.TrimPrefix(kv, "LD_LIBRARY_PATH=") + `"`
		}
		parts = append(parts, kv)
	}
	if len(parts) == 1 {
		parts = nil
	}
	parts = append(parts, l.Bin)
	parts = append(parts, l.Args()...)
	return strings.Join(parts, " ")
}

// remoteWorkerLaunch is the launch inside the remote script: $RW/$RW_PRE/$RW_DIR are set by its binary search.
func remoteWorkerLaunch(port int, env []string, devices string) workerLaunch {
	return workerLaunch{Bin: `"$RW"`, Pre: []string{"$RW_PRE"}, LibDir: "$RW_DIR", Port: port, Env: filterWorkerEnv(env), Devices: devices}
}

// localWorkerLaunch is the launch on this machine, with the binary resolved now.
func localWorkerLaunch(port int, env []string, devices string) (workerLaunch, error) {
	bin, pre, libDir, err := findLocalWorker()
	if err != nil {
		return workerLaunch{}, err
	}
	return workerLaunch{Bin: bin, Pre: pre, LibDir: libDir, Port: port, Env: filterWorkerEnv(env), Devices: devices}, nil
}

// workerCommandLine is the plan's WorkerCmds entry for node: exactly the line StartWorkerEnv runs there.
func workerCommandLine(node string, port int, env []string, devices string) string {
	if isThisMachine(node) {
		l, err := localWorkerLaunch(port, env, devices)
		if err != nil {
			return fmt.Sprintf("# %v", err)
		}
		return l.ShellLine()
	}
	return remoteWorkerLaunch(port, env, devices).ShellLine() + "   # on " + node + `: RW = first ring-worker of ` +
		strings.Join(workerCandidateDirs, ", ") + `, $PATH, else fleet-engine (RW_PRE="ring worker")`
}

// filterWorkerEnv keeps the KEY=VALUE pairs whose key is plain [A-Za-z0-9_.:,/-] text and whose value is a safeWord (also = and +): they go into a shell
// line unquoted on remote nodes, and the same filter is applied locally so both see the same environment.
func filterWorkerEnv(env []string) []string {
	var out []string
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" || !safeEnvText(k) || !safeWord(v) { // the same word rule as ValidateExtras: what it accepts must arrive
			continue
		}
		out = append(out, kv)
	}
	return out
}

func safeEnvText(s string) bool {
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_.:,/-", r)) {
			return false
		}
	}
	return true
}

// stopOnPortScript is a POSIX sh function fleet_stop_port that stops what listens on TCP port, but only processes
// named in names and owned by the current user (TERM, then KILL after 5 s). Anything else on the port is reported as
// "FLEET_BUSY <pid> <name>" and left alone. Nothing is killed by name machine-wide.
func stopOnPortScript(port int, names []string) string {
	return fmt.Sprintf(`fleet_stop_port() {
  pids=""
  # fuser is in psmisc, which a minimal node may lack: then the owners come from ss (users:(("name",pid=N,fd=M)))
  if command -v fuser >/dev/null 2>&1; then owners=$(fuser -n tcp %[1]d 2>/dev/null)
  else owners=$(ss -ltnpH 2>/dev/null | awk '$4 ~ /[:.]%[1]d$/' | grep -o 'pid=[0-9]*' | cut -d= -f2 | sort -u); fi
  for p in $owners; do
    p=${p%%%%[!0-9]*}
    [ -n "$p" ] || continue
    c=$(cat /proc/$p/comm 2>/dev/null)
    case "$c" in
      %s)
        if [ -O /proc/$p ]; then pids="$pids $p"; else echo "FLEET_BUSY $p $c (another user's)"; fi ;;
      *) echo "FLEET_BUSY $p $c" ;;
    esac
  done
  if [ -n "$pids" ]; then
    kill $pids 2>/dev/null
    i=0
    while [ $i -lt 20 ]; do
      alive=""
      for p in $pids; do kill -0 $p 2>/dev/null && alive="$alive $p"; done
      [ -n "$alive" ] || break
      sleep 0.25
      i=$((i+1))
    done
    [ -z "$alive" ] || { kill -9 $alive 2>/dev/null; sleep 0.3; }
  fi
  # Whatever still listens (an owner that could not be found or killed) would satisfy the readiness check of the NEW
  # process, which then dies on bind: say so instead.
  if { ss -ltn 2>/dev/null || netstat -ltn 2>/dev/null; } | awk '{print $4}' | grep -Eq '[:.]%[1]d$'; then
    echo "FLEET_BUSY - port %[1]d is still in use"
  fi
}
`, port, strings.Join(commNames(names), "|"))
}

// commNames are names as /proc/PID/comm shows them: the kernel keeps 15 bytes (TASK_COMM_LEN 16), so a
// fleet-engine-stage process reads "fleet-engine-st" and the full name never matched (its port was never freed, E10).
func commNames(names []string) []string {
	out := make([]string, 0, len(names))
	seen := map[string]bool{}
	for _, n := range names {
		if len(n) > 15 {
			n = n[:15]
		}
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// remoteWorkerScript stops a previous fleet worker on port, finds the ring-worker binary, starts it with the launch
// line and waits up to readyS seconds for it to listen. Its output is "FLEET_PID <pid>" followed by one of
// FLEET_READY, FLEET_DIED / FLEET_TIMEOUT (plus the log tail), FLEET_BUSY … or FLEET_NOBIN.
func remoteWorkerScript(port int, env []string, readyS int) string {
	return remoteWorkerScriptHub(port, env, readyS, "", "")
}

// remoteWorkerScriptHub is remoteWorkerScript for a worker the master reaches through hub (hub != ""): once the
// worker listens, the node's fleet daemon (only when it answers) is asked to add the port to its hub stream ports
// (hubJoinScript; its FLEET_HUB_* line follows FLEET_READY). The worker itself starts exactly as without a hub.
// devices is the worker's -d list ("": CUDA0).
func remoteWorkerScriptHub(port int, env []string, readyS int, hub, devices string) string {
	onReady := ""
	if hub != "" {
		onReady = hubJoinScript(hub, port)
	}
	var dirs []string
	for _, d := range workerCandidateDirs {
		dirs = append(dirs, `"`+d+`"`)
	}
	return fmt.Sprintf(`export PATH="$HOME/.local/bin:$PATH"
mkdir -p "$HOME/.cache/fleet"
LOG="$HOME/.cache/fleet/worker_%[1]d.log"
%[2]sbusy=$(fleet_stop_port)
if [ -n "$busy" ]; then echo "$busy"; exit 0; fi
RW=""; RW_DIR=""; RW_PRE=""
for d in %[3]s; do
  if [ -x "$d/ring-worker" ]; then RW="$d/ring-worker"; RW_DIR="$d"; break; fi
done
if [ -z "$RW" ] && command -v ring-worker >/dev/null 2>&1; then RW=$(command -v ring-worker); RW_DIR=$(dirname "$RW"); fi
if [ -z "$RW" ] && command -v fleet-engine >/dev/null 2>&1; then RW=$(command -v fleet-engine); RW_PRE="ring worker"; fi
if [ -z "$RW" ]; then echo "FLEET_NOBIN"; exit 0; fi
%[6]snohup $FLEET_DETACH %[4]s > "$LOG" 2>&1 < /dev/null &
PID=$!
echo "FLEET_PID $PID"
%[5]s`, port, stopOnPortScript(port, workerProcNames), strings.Join(dirs, " "), remoteWorkerLaunch(port, env, devices).ShellLine(), readyLoopScript(port, readyS, onReady), detachShell)
}

// readyLoopScript waits for $PID (output in $LOG) to listen on TCP port, at most readyS seconds: FLEET_READY (then
// onReady runs), FLEET_DIED or FLEET_TIMEOUT with the log tail.
func readyLoopScript(port, readyS int, onReady string) string {
	return fmt.Sprintf(`i=0
while :; do
  if ! kill -0 $PID 2>/dev/null || grep -q '^State:[[:space:]]*Z' /proc/$PID/status 2>/dev/null; then echo FLEET_DIED; tail -n 20 "$LOG"; exit 0; fi
  if { ss -ltn 2>/dev/null || netstat -ltn 2>/dev/null; } | awk '{print $4}' | grep -Eq '[:.]%[1]d$'; then echo FLEET_READY; %[3]sexit 0; fi
  i=$((i+1))
  if [ $i -ge %[2]d ]; then echo FLEET_TIMEOUT; tail -n 20 "$LOG"; exit 0; fi
  sleep 1
done
`, port, readyS, onReady)
}

// parseLaunchOutput reads a launch script's answer: the FLEET_PID, the readiness status and the lines after it.
func parseLaunchOutput(text string) (pid int, status string, tail []string) {
	for _, ln := range strings.Split(text, "\n") {
		t := strings.TrimSpace(ln)
		switch {
		case strings.HasPrefix(t, "FLEET_PID "):
			pid, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(t, "FLEET_PID ")))
		case t == "FLEET_READY" || t == "FLEET_DIED" || t == "FLEET_TIMEOUT":
			status = t
		case strings.HasPrefix(t, "FLEET_HUB_"):
		case status != "":
			tail = append(tail, ln)
		}
	}
	return pid, status, tail
}

// busyError turns FLEET_BUSY lines into an error ("" output: nil).
func busyError(where string, port int, out string) error {
	var held []string
	for _, ln := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(ln), "FLEET_BUSY "); ok {
			held = append(held, rest)
		}
	}
	if len(held) == 0 {
		return nil
	}
	return fmt.Errorf("port %d on %s is held by %s, which fleet did not start; not touching it (free the port or pick another)", port, where, strings.Join(held, ", "))
}

// Test seams: everything that runs a process or touches the network.
var (
	// sshScript runs a sh script on node (fed on stdin, so the login shell's syntax does not matter).
	sshScript = func(ctx context.Context, node, script string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "--", node, "sh -s")
		cmd.Stdin = strings.NewReader(script)
		return cmd.Output()
	}
	// localScript runs a sh script on this machine.
	localScript = func(ctx context.Context, script string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "sh", "-s")
		cmd.Stdin = strings.NewReader(script)
		return cmd.Output()
	}
	// startLocalProc starts bin in the background with the given full environment, output to logPath. done gets
	// the exit once the process ends.
	startLocalProc = func(ctx context.Context, bin string, args, env []string, logPath string) (int, <-chan error, error) {
		dbin, dargs, attr := detachedCommand(bin, args)
		// not CommandContext: the process must outlive a cancelled deploy (an interrupted wait leaves a loading ring)
		cmd := exec.Command(dbin, dargs...)
		cmd.SysProcAttr = attr
		cmd.Env = env
		if lf, err := os.Create(logPath); err == nil {
			cmd.Stdout, cmd.Stderr = lf, lf
			defer lf.Close() // the child has its own copy
		}
		if err := cmd.Start(); err != nil {
			return 0, nil, err
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		return cmd.Process.Pid, done, nil
	}
	localPortOpen = func(addr string) bool {
		c, err := net.DialTimeout("tcp", addr, 400*time.Millisecond)
		if err != nil {
			return false
		}
		_ = c.Close()
		return true
	}
	findLocalWorker = defaultFindLocalWorker
	isThisMachine   = defaultIsThisMachine
	readyPoll       = 300 * time.Millisecond
)

func defaultIsThisMachine(node string) bool {
	n := strings.ToLower(strings.TrimSpace(node))
	if n == "" || n == "localhost" || n == "127.0.0.1" || n == "::1" {
		return true
	}
	h, err := os.Hostname()
	if err != nil || h == "" {
		return false
	}
	h = strings.ToLower(h)
	short, _, _ := strings.Cut(h, ".")
	return n == h || n == short
}

func isExecFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir() && st.Mode()&0o111 != 0
}

// defaultFindLocalWorker resolves this machine's ring-worker: next to libggml-ring.so, next to the engine
// (llama-server), the standard build directories / $PATH (ringengine.FindWorkerPath), then a fleet-engine binary or
// this fleet itself when it has the engine linked in (both have `ring worker`). FindEnginePath alone is not enough:
// it usually is llama-server, which has no worker mode.
func defaultFindLocalWorker() (bin string, pre []string, libDir string, err error) {
	if bp, ok := ringengine.FindBackendPath(); ok {
		if p := filepath.Join(filepath.Dir(bp), "ring-worker"); isExecFile(p) {
			return p, nil, filepath.Dir(p), nil
		}
	}
	eng, engOK := ringengine.FindEnginePath()
	if engOK {
		if p := filepath.Join(filepath.Dir(eng), "ring-worker"); isExecFile(p) {
			return p, nil, filepath.Dir(p), nil
		}
	}
	if p, ok := ringengine.FindWorkerPath(); ok && isExecFile(p) {
		return p, nil, filepath.Dir(p), nil
	}
	if engOK && filepath.Base(eng) == "fleet-engine" {
		return eng, []string{"ring", "worker"}, "", nil
	}
	if ringengine.Available() {
		if exe, err := os.Executable(); err == nil {
			return exe, []string{"ring", "worker"}, "", nil
		}
	}
	return "", nil, "", fmt.Errorf("no ring-worker binary on this machine (looked next to libggml-ring.so and llama-server, in %s and $PATH; a fleet-engine binary would also do)", strings.Join(workerCandidateDirs, ", "))
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func workerLogPath(port int) string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	dir = filepath.Join(dir, "fleet")
	_ = os.MkdirAll(dir, 0o755)
	return filepath.Join(dir, fmt.Sprintf("worker_%d.log", port))
}

// libPathWith prepends dir to an inherited LD_LIBRARY_PATH; an empty inherited value adds no trailing ":" (an empty
// entry means the current directory, where a library of that name would be picked up).
func libPathWith(dir, prev string) string {
	if prev == "" {
		return dir
	}
	return dir + ":" + prev
}

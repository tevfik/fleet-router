package deploy

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/plan"
)

// A ring worker named by --via-hub / --via-hub-node is reached through the fleet hub's raw TCP stream mode
// (docs/networking_and_tunneling.md, Senaryo B2): the worker starts as always (0.0.0.0:port), its node declares the port
// as a hub stream port (fleet join <hub> --add-stream-port <port>, run in the same ssh session when the node's fleet
// daemon answers), and on the master `fleet stream connect <node> <port> --listen 127.0.0.1:<L> --hub <hub>` runs in
// the background; RING_SERVERS names 127.0.0.1:<L> for that worker.
// A worker without a direct LAN IP is never routed through the hub silently: deploy refuses unless --via-hub,
// --via-hub-node, --no-hub or --chain was given explicitly.

const (
	// DefaultHubListenBase is the first master-local port of the hub stream listeners: worker i of the ring (its
	// RING_SERVERS index) gets DefaultHubListenBase + i.
	DefaultHubListenBase = 51000
	// DefaultStreamReady is how long a started `fleet stream connect` gets to listen on its local port.
	DefaultStreamReady = 15 * time.Second
	// HubLossRefuseRatio is the estimated relative throughput drop ((directTPS - hubTPS) / directTPS) above which
	// `fleet deploy` refuses a hub stream deployment unless --force is given (0.50 = 50% speed loss).
	HubLossRefuseRatio = 0.50
)

// hubLatencyNote says what the hub path costs, without figures of any one hub or network.
const hubLatencyNote = "the hub path adds the hub round trip to every ring round trip (how much depends on where the hub is: a few ms near it, much more over a WAN); profile measurements (fleet profile bench) show what it costs for this layout"

// streamProcNames are the processes fleet stops on a stream listener port: only fleet itself (stream connect).
var streamProcNames = []string{"fleet"}

// Test seams for the hub path.
var (
	// fleetBinary is the fleet executable that runs `fleet stream connect` on this machine.
	fleetBinary = os.Executable
	// hubAPIKey is the key this machine's fleet has for the hub (config.ResolveHubAPIKey).
	hubAPIKey = func(cfg *config.Config) string {
		k, _ := config.ResolveHubAPIKey(cfg) // $FLEET_API_KEY > .env > hub_api_key > api_key_env, as everywhere
		return k
	}
	streamReady = DefaultStreamReady
)

// streamLaunch is one `fleet stream connect` on the master. The same value renders the plan's StreamCmds entry, the
// remote script's start line and the local exec arguments. It never carries the API key.
type streamLaunch struct {
	Node       string // the worker node, as the hub knows it
	Port       int    // the worker's ring port on its node
	ListenPort int    // 127.0.0.1:ListenPort on the master
	Hub        string
}

func (l streamLaunch) Listen() string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(l.ListenPort))
}

// Args are the arguments after the fleet binary.
func (l streamLaunch) Args() []string {
	return []string{"stream", "connect", l.Node, strconv.Itoa(l.Port), "--listen", l.Listen(), "--hub", l.Hub}
}

// ShellLine is the command as a shell runs it (fleet from $PATH).
func (l streamLaunch) ShellLine() string {
	return "fleet " + strings.Join(l.Args(), " ")
}

// hubJoinCommand declares port as a hub stream port of the node it runs on, keeping the ports it already has.
func hubJoinCommand(hub string, port int) string {
	return fmt.Sprintf("fleet join %s --add-stream-port %d", hub, port)
}

// hubJoinScript runs hubJoinCommand only when the node's fleet daemon answers (fleet status), and says what happened:
// FLEET_HUB_JOINED, FLEET_HUB_NODAEMON or FLEET_HUB_JOIN_FAILED <last line of its output>.
func hubJoinScript(hub string, port int) string {
	return fmt.Sprintf(`if command -v fleet >/dev/null 2>&1 && fleet status >/dev/null 2>&1; then if hj=$(%s 2>&1); then echo FLEET_HUB_JOINED; else echo "FLEET_HUB_JOIN_FAILED $(echo "$hj" | tail -n 1)"; fi; else echo FLEET_HUB_NODAEMON; fi; `, hubJoinCommand(hub, port))
}

// hubJoinNote turns the FLEET_HUB_* answer into "" (declared) or what to do on the node by hand.
func hubJoinNote(where, hub string, port int, out string) string {
	byHand := fmt.Sprintf("run `%s` there (or put %d into its hub_stream_ports and join), else the hub refuses the stream (403/404)", hubJoinCommand(hub, port), port)
	for _, ln := range strings.Split(out, "\n") {
		t := strings.TrimSpace(ln)
		switch {
		case t == "FLEET_HUB_JOINED":
			return ""
		case t == "FLEET_HUB_NODAEMON":
			return fmt.Sprintf("no fleet daemon answers on %s, so port %d was not declared to the hub by the deploy: %s", where, port, byHand)
		case strings.HasPrefix(t, "FLEET_HUB_JOIN_FAILED"):
			return fmt.Sprintf("declaring port %d to the hub on %s failed (%s): %s", port, where, strings.TrimSpace(strings.TrimPrefix(t, "FLEET_HUB_JOIN_FAILED")), byHand)
		}
	}
	return fmt.Sprintf("no answer about the hub stream port %d from %s: %s", port, where, byHand)
}

// hubShellSafe: the hub URL and node names go into shell lines unquoted.
func hubShellSafe(s string) bool {
	return s != "" && !strings.HasPrefix(s, "-") && safeEnvText(s)
}

// streamLogName is the log of a stream connect on the master, under ~/.cache/fleet/runs.
func streamLogName(runID, node string) string {
	return fmt.Sprintf("%s-stream-%s.log", runID, node)
}

// remoteStreamScript stops a previous fleet process on the listen port, starts the stream connect with nohup (log
// ~/.cache/fleet/runs/<run>-stream-<node>.log) and waits up to readyS seconds for it to listen. The key is not in it:
// the master's fleet takes its own ($FLEET_API_KEY or api_key_env of its config).
func remoteStreamScript(l streamLaunch, runID string, readyS int) string {
	return fmt.Sprintf(`export PATH="$HOME/.local/bin:$PATH"
mkdir -p "$HOME/.cache/fleet/runs"
LOG="$HOME/.cache/fleet/runs/%[1]s"
%[2]sbusy=$(fleet_stop_port)
if [ -n "$busy" ]; then echo "$busy"; exit 0; fi
if ! command -v fleet >/dev/null 2>&1; then echo FLEET_NOBIN; exit 0; fi
%[5]snohup $FLEET_DETACH %[3]s > "$LOG" 2>&1 < /dev/null &
PID=$!
echo "FLEET_PID $PID"
%[4]s`, streamLogName(runID, l.Node), stopOnPortScript(l.ListenPort, streamProcNames), l.ShellLine(), readyLoopScript(l.ListenPort, readyS, ""), detachShell)
}

// StartStream starts `fleet stream connect` for one hub worker on the master and waits until 127.0.0.1:ListenPort
// accepts there. A previous fleet process on that port (an older run's stream) is stopped first; anything else on it is
// an error. apiKey goes only into the environment of a local stream connect; a remote master uses its own key.
func (m *RemoteWorkerManager) StartStream(ctx context.Context, master string, isLocal bool, l streamLaunch, runID, apiKey string) (int, error) {
	readyS := int((streamReady + time.Second - 1) / time.Second)
	if !isLocal {
		sctx, cancel := context.WithTimeout(ctx, streamReady+30*time.Second)
		defer cancel()
		out, err := sshScript(sctx, master, remoteStreamScript(l, runID, readyS))
		text := string(out)
		if err != nil {
			return 0, fmt.Errorf("starting the hub stream to %s on %s failed: %w\n%s", l.Node, master, err, tailLines(text, 20))
		}
		if berr := busyError(master, l.ListenPort, text); berr != nil {
			return 0, berr
		}
		if strings.Contains(text, "FLEET_NOBIN") {
			return 0, fmt.Errorf("no fleet on %s for the hub stream to %s", master, l.Node)
		}
		pid, status, tail := parseLaunchOutput(text)
		logName := "~/.cache/fleet/runs/" + streamLogName(runID, l.Node)
		switch status {
		case "FLEET_READY":
			return pid, nil
		case "FLEET_DIED":
			return pid, fmt.Errorf("hub stream to %s on %s (pid %d) exited before listening on %s (does its fleet have the hub API key: FLEET_API_KEY or api_key_env?); %s:\n%s", l.Node, master, pid, l.Listen(), logName, strings.Join(tail, "\n"))
		case "FLEET_TIMEOUT":
			return pid, fmt.Errorf("hub stream to %s on %s (pid %d) did not listen on %s within %ds; %s:\n%s", l.Node, master, pid, l.Listen(), readyS, logName, strings.Join(tail, "\n"))
		}
		return pid, fmt.Errorf("hub stream start on %s gave no readiness answer:\n%s", master, tailLines(text, 20))
	}

	if err := stopOnPort(ctx, "", true, l.ListenPort, streamProcNames); err != nil {
		return 0, err
	}
	bin, err := fleetBinary()
	if err != nil {
		return 0, fmt.Errorf("no fleet binary for the hub stream: %w", err)
	}
	env := os.Environ()
	if apiKey != "" {
		env = append(env, "FLEET_API_KEY="+apiKey)
	}
	logPath := runLogPath(streamLogName(runID, l.Node))
	pid, done, err := startLocalProc(ctx, bin, l.Args(), env, logPath)
	if err != nil {
		return 0, fmt.Errorf("starting the hub stream to %s failed: %w", l.Node, err)
	}
	logTail := func() string {
		b, _ := os.ReadFile(logPath)
		return tailLines(string(b), 20)
	}
	deadline := time.Now().Add(streamReady)
	for {
		select {
		case werr := <-done:
			return pid, fmt.Errorf("hub stream to %s (pid %d) exited before listening on %s (%v); %s:\n%s", l.Node, pid, l.Listen(), werr, logPath, logTail())
		case <-ctx.Done():
			return pid, ctx.Err()
		default:
		}
		if localPortOpen(l.Listen()) {
			return pid, nil
		}
		if time.Now().After(deadline) {
			return pid, fmt.Errorf("hub stream to %s (pid %d) did not listen on %s within %s; %s:\n%s", l.Node, pid, l.Listen(), streamReady, logPath, logTail())
		}
		time.Sleep(readyPoll)
	}
}

// StopStream stops the fleet stream connect listening on 127.0.0.1:listenPort of the master (only a fleet process of
// this user; see stopOnPortScript).
func (m *RemoteWorkerManager) StopStream(ctx context.Context, master string, listenPort int, isLocal bool) error {
	return stopOnPort(ctx, master, isLocal, listenPort, streamProcNames)
}

// StopStreams stops the hub stream connects of a ring on its master: streams maps worker node -> listen address
// ("127.0.0.1:L", config Group.Streams). Every stream is tried; the first error is returned.
func StopStreams(ctx context.Context, master string, isLocal bool, streams map[string]string) error {
	m := NewRemoteWorkerManager()
	var first error
	for node, addr := range streams {
		_, p, err := net.SplitHostPort(addr)
		port, perr := strconv.Atoi(p)
		if err != nil || perr != nil || port <= 0 {
			if first == nil {
				first = fmt.Errorf("bad stream address %q of %s", addr, node)
			}
			continue
		}
		if err := m.StopStream(ctx, master, port, isLocal); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// runLogPath is a file under this machine's ~/.cache/fleet/runs.
func runLogPath(name string) string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	dir = filepath.Join(dir, "fleet", "runs")
	_ = os.MkdirAll(dir, 0o755)
	return filepath.Join(dir, name)
}

// stopStreamsOf stops the streams a deploy started (after a later step failed).
func (r *DeployResult) stopStreamsOf(ctx context.Context, isLocalMaster bool) {
	if len(r.HubWorkers) == 0 {
		return
	}
	_ = StopStreams(ctx, r.MasterNode, isLocalMaster, r.streamAddrs())
}

// streamAddrs maps each hub worker to its listen address on the master.
func (r *DeployResult) streamAddrs() map[string]string {
	if len(r.HubWorkers) == 0 {
		return nil
	}
	out := make(map[string]string, len(r.HubWorkers))
	for n, l := range r.HubWorkers {
		out[n] = net.JoinHostPort("127.0.0.1", strconv.Itoa(l))
	}
	return out
}

// hubPlan decides which ring workers go through the hub and gives each its listen port on the master.
type hubPlan struct {
	hub     string
	listen  map[string]int    // worker node -> listen port on the master
	reasons map[string]string // why the hub is used
}

func planHub(cfg *config.Config, opts DeployOptions, masterNode string, ringNodes []string, workerPorts map[string]int, port, statsPort int) (*hubPlan, error) {
	forced := map[string]bool{}
	for _, n := range opts.ViaHubNodes {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		found := false
		for _, r := range ringNodes {
			if r == n {
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("--via-hub-node %s is not a worker of this deployment (workers: %s)", n, strings.Join(ringNodes, ", "))
		}
		forced[n] = true
	}
	hp := &hubPlan{listen: map[string]int{}, reasons: map[string]string{}}
	var missingDirect []string
	for _, node := range ringNodes {
		switch {
		case opts.ViaHub:
			hp.reasons[node] = "--via-hub"
		case forced[node]:
			hp.reasons[node] = "--via-hub-node"
		case opts.NoHub, opts.Chain: // --chain: the workers dial each other, the hub cannot carry that; --no-hub: dial by name
		case isThisMachine(node) && isThisMachine(masterNode), strings.EqualFold(node, masterNode):
		case net.ParseIP(ResolveNodeIP(node)) == nil:
			missingDirect = append(missingDirect, node)
		}
	}
	if len(missingDirect) > 0 {
		return nil, fmt.Errorf("refusing implicit hub fallback: worker(s) %s have no direct LAN address (neither in config nodes.<node>.addr nor in direct ssh config), and routing ring traffic through the hub adds hub RTT to every token step. Configure a direct IP (`fleet config node set <node> addr=<IP>`), pass --via-hub / --via-hub-node %s to use the hub stream explicitly, or pass --no-hub to dial by hostname",
			strings.Join(missingDirect, ", "), strings.Join(missingDirect, ","))
	}
	if len(hp.reasons) == 0 {
		return hp, nil
	}
	hp.hub = opts.HubURL
	if hp.hub == "" && cfg != nil {
		hp.hub = cfg.HubURL
	}
	var err error
	if hp.hub, err = config.HubOrDefault(hp.hub); err != nil {
		return nil, err
	}
	if !hubShellSafe(hp.hub) {
		return nil, fmt.Errorf("hub URL %q has characters fleet does not put into a shell line", hp.hub)
	}
	if opts.Chain {
		return nil, fmt.Errorf("--chain needs the workers to dial each other directly; it cannot be combined with workers reached through the hub (%s)", strings.Join(sortedKeys(hp.reasons), ", "))
	}
	base := opts.HubListenBase
	if base <= 0 {
		base = DefaultHubListenBase
	}
	daemonPort := 0
	if cfg != nil && cfg.Listen != "" {
		if _, p, err := net.SplitHostPort(cfg.Listen); err == nil {
			daemonPort, _ = strconv.Atoi(p)
		}
	}
	for i, node := range ringNodes {
		if _, ok := hp.reasons[node]; !ok {
			continue
		}
		if !hubShellSafe(node) {
			return nil, fmt.Errorf("worker name %q has characters fleet does not put into a shell line; it cannot go through the hub", node)
		}
		l := base + i
		if l > 65535 || l == port || l == statsPort || l == daemonPort || l == 4711 {
			return nil, fmt.Errorf("hub stream listen port %d of worker %s collides with the master/stats/fleet port or is out of range; pick another --hub-listen-base", l, node)
		}
		for wn, wp := range workerPorts {
			if wp == l {
				return nil, fmt.Errorf("hub stream listen port %d of worker %s equals the ring port of %s; pick another --hub-listen-base", l, node, wn)
			}
		}
		hp.listen[node] = l
	}
	return hp, nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// effectiveHubRTT returns the hub round-trip latency (ms) assumed for hub stream workers.
func effectiveHubRTT(cfg *config.Config, opts DeployOptions) float64 {
	if opts.HubRTTms > 0 {
		return opts.HubRTTms
	}
	if cfg != nil && cfg.HubRTTms > 0 {
		return cfg.HubRTTms
	}
	return plan.DefaultHubRTTms
}

// checkHubSpeedLoss compares direct LAN estimated throughput against hub-routed throughput.
// When the relative throughput drop exceeds HubLossRefuseRatio (50%), blocker is non-empty so
// ExecuteDeployment refuses unless --force is given; smaller drops emit an informative warning.
func checkHubSpeedLoss(directTPS, hubTPS, hubRTT float64, hubWorkers map[string]int) (warn, blocker string) {
	if len(hubWorkers) == 0 || directTPS <= 0 || hubTPS <= 0 || hubTPS >= directTPS {
		return "", ""
	}
	nodes := make([]string, 0, len(hubWorkers))
	for n := range hubWorkers {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	loss := (directTPS - hubTPS) / directTPS
	msg := fmt.Sprintf("hub stream throughput drop for %s: estimated %.1f tok/s via hub (%.1f ms hub RTT/worker) vs %.1f tok/s over direct LAN (-%.0f%%)",
		strings.Join(nodes, ", "), hubTPS, hubRTT, directTPS, loss*100)
	if loss > HubLossRefuseRatio {
		return "", msg
	}
	return msg, ""
}

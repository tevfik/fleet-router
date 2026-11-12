package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
)

// DefaultLoadTimeout is how long a started master gets to become healthy (ultra-large models over wireless or remote
// workers need a long time to load).
const DefaultLoadTimeout = 20 * time.Minute

// Run states in the run's record file (~/.cache/fleet/runs/<id>.json), written before anything is started and
// updated at every step, so that an interrupted or crashed deploy leaves a trace of what it started.
const (
	StatusStarting = "starting" // processes are being started / the master is loading
	StatusFailed   = "failed"   // refused or failed; what this deploy had started was stopped
	StatusTimeout  = "timeout"  // the master did not become healthy in time; it and its workers were LEFT running
	StatusStopped  = "stopped"  // the user interrupted the wait; processes started so far were handled as the message says
	StatusRunning  = "running"
)

// WaitError is the outcome of a deploy that started a master but did not see it become healthy: the wait timed out or
// was interrupted. Nothing was torn down (a master that is still reading a 200 GB model is not killed because the
// wait ended); Error() says what runs and how to wait for it or stop it. Res carries the record.
type WaitError struct {
	Res     *DeployResult
	Reason  string // "timed out after 20m0s" | "interrupted"
	Elapsed time.Duration
}

func (e *WaitError) Error() string {
	r := e.Res
	var b strings.Builder
	fmt.Fprintf(&b, "the master on %s did not become healthy: %s (after %s)\n", r.MasterNode, e.Reason, e.Elapsed.Round(time.Second))
	b.WriteString("NOT stopped, still running: the master" + pidNote(r.MasterPID))
	for _, n := range sortedIntKeys(r.WorkerPIDs) {
		fmt.Fprintf(&b, ", worker %s%s", n, pidNote(r.WorkerPIDs[n]))
	}
	for _, n := range sortedIntKeys(r.StreamPIDs) {
		fmt.Fprintf(&b, ", hub stream to %s%s", n, pidNote(r.StreamPIDs[n]))
	}
	fmt.Fprintf(&b, " (a large model may still be loading: the master's log is ~/.cache/fleet/runs/%s.log on %s).\n", r.RunID, r.MasterNode)
	fmt.Fprintf(&b, "The endpoint was NOT registered, the record is %s (state %q). To wait: watch that log, then `curl http://%s/health`; ", filepath.Join("~/.cache/fleet/runs", r.RunID+".json"), r.Status, hostPortOf(r.URL))
	b.WriteString("to stop everything: deploy again on the same ports (a previous fleet master/worker there is replaced) or `fleet run end` / kill the PIDs above.")
	return b.String()
}

// preflightDial checks TCP reachability to addr from masterNode before the master is launched.
var preflightDial = func(ctx context.Context, masterNode string, isLocalMaster bool, addr string) error {
	if isLocalMaster {
		deadline := time.Now().Add(2 * time.Second)
		for {
			if localPortOpen(addr) {
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("worker at %s is unreachable (connection refused or timeout)", addr)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid worker address %q: %w", addr, err)
	}
	script := fmt.Sprintf(`python3 -c "import socket; s = socket.create_connection((%q, %s), timeout=2); s.close()" 2>/dev/null || nc -z -w 2 %s %s 2>/dev/null || bash -c 'exec 3<>/dev/tcp/%s/%s' 2>/dev/null`, host, portStr, host, portStr, host, portStr)
	cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	_, err = sshScript(cctx, masterNode, script+"\n")
	if err != nil {
		return fmt.Errorf("worker at %s is unreachable from master %s: %w", addr, masterNode, err)
	}
	return nil
}

func pidNote(pid int) string {
	if pid <= 0 {
		return ""
	}
	return fmt.Sprintf(" (pid %d)", pid)
}

func sortedIntKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func hostPortOf(u string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(u, "http://"), "https://")
	if i := strings.Index(s, "/"); i >= 0 {
		s = s[:i]
	}
	return s
}

// runDir is this machine's ~/.cache/fleet/runs.
func runDir() string {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		cacheDir = os.TempDir()
	}
	dir := filepath.Join(cacheDir, "fleet", "runs")
	_ = os.MkdirAll(dir, 0o700)
	_ = os.Chmod(dir, 0o700) // records carry env values (tokens): not readable by other users
	return dir
}

// saveState writes the run's record file. Best effort: a deploy does not fail because the record cannot be written.
func (r *DeployResult) saveState() {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return
	}
	path := filepath.Join(runDir(), r.RunID+".json")
	tmp := path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, path)
	}
	if OnState != nil {
		OnState(*r)
	}
}

// teardown stops what this deploy had started and nothing else: its hub streams and its workers (only fleet's own
// ring-worker/fleet processes on their ports, see stopOnPortScript). It uses its own context: the deploy's may be the
// cancelled one.
func (r *DeployResult) teardown(isLocalMaster bool, rwm *RemoteWorkerManager) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	r.stopStreamsOf(ctx, isLocalMaster)
	for _, n := range r.startOrder() {
		if _, started := r.WorkerPIDs[n]; started {
			_ = rwm.StopWorker(ctx, n, r.workerPort(n), isThisMachine(n))
		}
	}
}

// ExecuteDeployment launches remote workers, starts the master, polls health, and registers into Fleet.
func ExecuteDeployment(ctx context.Context, cfg *config.Config, opts DeployOptions) (*DeployResult, error) {
	return ExecuteDeploymentWithProgress(ctx, cfg, opts, nil)
}

// ExecuteDeploymentWithProgress launches deployment with real-time stage progress reporting.
//
// What happens when it does not finish (the run record ~/.cache/fleet/runs/<id>.json is written first, state
// "starting", and updated at every step):
//   - refused (memory) or failed before the master is started (a worker, a stream, the master's own start): everything
//     this deploy had started is stopped, state "failed";
//   - the master exited before it was healthy: its workers and streams are stopped, state "failed" (with the log tail);
//   - the load timeout (opts.LoadTimeoutS, default 20 min) or an interrupt (ctx cancelled: Ctrl+C, SIGTERM) while the
//     master is loading: nothing is stopped, a *WaitError says what runs and how to stop it, state "timeout"/"stopped";
//   - an interrupt before the master was started: the workers and streams started so far are stopped, state "stopped".
func ExecuteDeploymentWithProgress(ctx context.Context, cfg *config.Config, opts DeployOptions, progress func(string)) (*DeployResult, error) {
	report := func(step string) {
		if progress != nil {
			progress(step)
		}
	}

	report("[1/4] 📋 Calculating tensor split and allocation plan...")
	res, err := PlanDeployment(cfg, opts)
	if err != nil {
		return nil, err
	}

	// Memory & hub throughput pre-checks, before anything is started (a dry run only shows them).
	rep, merr := checkMemory(ctx, cfg, res, opts)
	for _, w := range rep.Warnings {
		res.Warnings = append(res.Warnings, w)
		report("⚠ " + w)
	}
	if opts.DryRun {
		if res.hubBlocker != "" {
			res.Warnings = append(res.Warnings, "WOULD BE REFUSED (use --force to go on): "+res.hubBlocker)
		}
		for _, b := range rep.Blockers {
			res.Warnings = append(res.Warnings, "WOULD BE REFUSED (use --force to go on): "+b)
		}
		res.Status = "dry-run"
		return res, nil
	}
	if res.hubBlocker != "" && !opts.Force {
		return nil, fmt.Errorf("refusing hub deployment (>%.0f%% estimated speed loss): %s\npass --force to deploy anyway, or configure a direct LAN address (`fleet config node set <node> addr=<IP>`)", HubLossRefuseRatio*100, res.hubBlocker)
	}
	if res.hubBlocker != "" { // --force
		res.Warnings = append(res.Warnings, "forced although: "+res.hubBlocker)
		report("⚠ forced although: " + res.hubBlocker)
	}
	if merr != nil {
		return nil, merr
	}
	for _, b := range rep.Blockers { // --force
		res.Warnings = append(res.Warnings, "forced although: "+b)
		report("⚠ forced although: " + b)
	}

	masterPort := opts.Port
	if masterPort <= 0 {
		masterPort = 18090
	}
	isLocalMaster := isLocalNode(res.MasterNode)

	// Deploying stops whatever fleet process listens on the master port: another registered ring that is alive there
	// would die silently (its endpoint stays registered, dead). --force goes on; a ring this deploy replaces is fine.
	if !opts.DryRun && !opts.Force && !opts.Replace {
		if other := otherRingOnPort(cfg, res, opts.ModelPath, masterPort); other != "" {
			return nil, fmt.Errorf("endpoint %s is running on port %d of %s: this deploy would stop it. Use another --port, `fleet endpoint stop %s` first, or --force", other, masterPort, res.MasterNode, other)
		}
	}

	// The record first: whatever happens next leaves a trace of what was started.
	saved := opts
	saved.DryRun = false
	res.Options = &saved
	res.Status = StatusStarting
	res.StartedAt = time.Now().UTC()
	res.saveState()

	rwm := NewRemoteWorkerManager()
	if opts.WorkerReadyS > 0 {
		rwm.ReadyTimeout = time.Duration(opts.WorkerReadyS) * time.Second
	}
	fail := func(stop bool, err error) (*DeployResult, error) {
		res.Status, res.Error = StatusFailed, err.Error()
		if ctx.Err() != nil {
			res.Status = StatusStopped
		}
		if stop {
			res.teardown(isLocalMaster, rwm)
			report("✗ stopped what this deploy had started (workers, hub streams)")
		}
		res.saveState()
		if ctx.Err() != nil && !errors.Is(err, context.Canceled) {
			err = fmt.Errorf("interrupted: %w", err)
		}
		return nil, err
	}

	// 0. A previous master of fleet's on the master port is stopped where the master runs (below); worker ports are
	// cleared per worker, on the worker's own machine, by StartWorkerEnv. Only fleet's own processes are stopped.
	report("[1/4] 🧹 Clearing stale processes on ports...")
	workerPIDs := make(map[string]int)
	res.WorkerPIDs = workerPIDs

	// A hub stream started on THIS machine needs this machine's hub API key; without one it dies right after the
	// workers (and their ports) are up. Say so before anything is started. (A remote master uses its own key.)
	if isLocalMaster && len(res.HubWorkers) > 0 && opts.HubKey == "" && hubAPIKey(cfg) == "" {
		return fail(false, fmt.Errorf("--via-hub needs this machine's hub API key for the stream(s) it runs here, and there is none: `fleet key set <key>`, $FLEET_API_KEY, hub_api_key / api_key_env in the config, or --key; nothing was started"))
	}

	// 1. Start Workers
	workers := res.startOrder()
	if len(workers) > 0 {
		report(fmt.Sprintf("[2/4] 🛰 Starting remote workers on %s...", strings.Join(workers, ", ")))
	}
	for _, wNode := range workers {
		wPort := res.workerPort(wNode)
		hub := ""
		if _, ok := res.HubWorkers[wNode]; ok {
			hub = res.HubURL // the worker's port is also declared as a hub stream port on its node
		}
		// a worker on the master's machine is local only when the master is this machine: isThisMachine, not == MasterNode
		_, pid, hubNote, err := rwm.startWorker(ctx, wNode, wPort, isThisMachine(wNode), strings.Fields(res.WorkerEnv[wNode]), hub, res.WorkerDevices[wNode])
		if pid > 0 {
			workerPIDs[wNode] = pid // also a failed start may have left a process: the record and the teardown know it
		}
		if err != nil {
			workerPIDs[wNode] = max(pid, 0)
			return fail(true, fmt.Errorf("starting worker %s failed: %w", wNode, err))
		}
		if hubNote != "" {
			res.Warnings = append(res.Warnings, hubNote)
			report("⚠ " + hubNote)
		}
		workerPIDs[wNode] = pid
		report(fmt.Sprintf("[2/4] ✓ worker %s listening on port %d (pid %d)", wNode, wPort, pid))
		res.saveState()
	}

	// 1b. Hub streams on the master, one per worker reached through the hub; they must listen before the master dials.
	if len(res.HubWorkers) > 0 {
		report(fmt.Sprintf("[2/4] 🔀 Opening hub streams through %s on %s...", res.HubURL, res.MasterNode))
		apiKey := ""
		if isLocalMaster {
			apiKey = opts.HubKey
			if apiKey == "" {
				apiKey = hubAPIKey(cfg) // only into the local stream's environment; a remote master uses its own key
			}
		}
		res.StreamPIDs = make(map[string]int)
		for _, wNode := range workers {
			l, ok := res.HubWorkers[wNode]
			if !ok {
				continue
			}
			sl := streamLaunch{Node: wNode, Port: res.workerPort(wNode), ListenPort: l, Hub: res.HubURL}
			pid, err := rwm.StartStream(ctx, res.MasterNode, isLocalMaster, sl, res.RunID, apiKey)
			if err != nil {
				return fail(true, fmt.Errorf("hub stream to worker %s failed: %w", wNode, err))
			}
			res.StreamPIDs[wNode] = pid
			res.saveState()
		}
	}

	// 1c. Pre-flight reachability check to all RING_SERVERS addresses before starting the master.
	// If any worker is unreachable, fail immediately with a clear error instead of letting llama-server hang for 20 seconds.
	ringServers := res.RingServersList()
	if len(ringServers) > 0 {
		report(fmt.Sprintf("[2/4] 🔍 Pre-flight reachability check to %d worker(s)...", len(ringServers)))
		for _, addr := range ringServers {
			if err := preflightDial(ctx, res.MasterNode, isLocalMaster, addr); err != nil {
				return fail(true, fmt.Errorf("pre-flight check failed: worker at %s is unreachable before master start: %w", addr, err))
			}
		}
		report("[2/4] ✓ all workers reachable")
	}

	// 2. Start Master Server
	report(fmt.Sprintf("[3/4] 🚀 Launching master on %s (port %d)...", res.MasterNode, masterPort))
	cmdParts := strings.Fields(res.MasterCmd)
	if len(cmdParts) < 3 {
		return fail(true, fmt.Errorf("invalid master command: %s", res.MasterCmd))
	}

	logFile := filepath.Join(runDir(), fmt.Sprintf("%s.log", res.RunID))
	masterHost := "127.0.0.1"
	if !isLocalMaster {
		masterHost = ResolveNodeIP(res.MasterNode)
	}

	var masterExited <-chan error // a local master's exit (a remote one is looked at over ssh); nil: never fires
	if isLocalMaster {
		if err := stopOnPort(ctx, "", true, masterPort, masterProcNames); err != nil {
			return fail(true, err)
		}
		masterBin, masterArgs := resolveLocalMaster(cmdParts)
		env := os.Environ()
		for _, envStr := range strings.Split(res.MasterEnv, " ") {
			if envStr != "" {
				env = append(env, envStr)
			}
		}
		if bPath, ok := findBackendPath(); ok {
			env = append(env, fmt.Sprintf("GGML_BACKEND_PATH=%s", bPath))
			env = append(env, "LD_LIBRARY_PATH="+libPathWith(filepath.Dir(bPath), os.Getenv("LD_LIBRARY_PATH")))
		}
		// startLocalProc (not tied to ctx): the master must outlive an interrupted deploy (see WaitError)
		pid, done, err := startLocalProc(ctx, masterBin, localArgs(masterArgs), env, logFile)
		if err != nil {
			return fail(true, fmt.Errorf("starting master process failed: %w", err))
		}
		res.MasterPID = pid
		masterExited = done
	} else {
		// Remote Master via SSH
		if err := stopOnPort(ctx, res.MasterNode, false, masterPort, masterProcNames); err != nil {
			return fail(true, err)
		}
		masterArgsStr := strings.TrimPrefix(strings.Join(cmdParts[1:], " "), "ring serve ")
		envPrefix := ""
		if res.MasterEnv != "" {
			envPrefix = "export " + res.MasterEnv + "\n"
		}
		sctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		out, err := sshScript(sctx, res.MasterNode, remoteMasterScript(envPrefix, masterArgsStr, res.RunID))
		cancel()
		if err != nil {
			// The script may have launched the master before ssh timed out or dropped: nobody recorded its pid, and
			// it would keep the port and VRAM. Stop fleet's master on that port (its own context: ctx may be done).
			cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
			if serr := stopMasterAfterSSHFailure(cctx, res.MasterNode, masterPort); serr != nil {
				report("⚠ could not check for a master left on " + res.MasterNode + ": " + serr.Error())
			}
			ccancel()
			return fail(true, fmt.Errorf("starting remote master on %s failed: %w", res.MasterNode, err))
		}
		text := string(out)
		if strings.Contains(text, "FLEET_NOBIN") {
			return fail(true, noMasterEngineError(res.MasterNode))
		}
		pid, status, logTail := parseLaunchOutput(text)
		res.MasterPID = pid
		if status == "FLEET_DIED" {
			return fail(true, fmt.Errorf("the master on %s (pid %d) exited right after it started; ~/.cache/fleet/runs/%s.log:\n%s", res.MasterNode, pid, res.RunID, strings.Join(logTail, "\n")))
		}
	}
	res.saveState()

	// 3. Health Poll
	healthURL := fmt.Sprintf("http://%s:%d/health", masterHost, masterPort)
	timeout := DefaultLoadTimeout
	if opts.LoadTimeoutS > 0 {
		timeout = time.Duration(opts.LoadTimeoutS) * time.Second
	}
	startTime := time.Now()
	deadline := startTime.Add(timeout)
	client := &http.Client{Timeout: 1 * time.Second}
	viaSSH := false
	ready := false
	lastCheck := startTime
	lastLog := startTime
	for !ready {
		if err := ctx.Err(); err != nil {
			return waitEnd(res, "interrupted", time.Since(startTime), report)
		}
		if time.Now().After(deadline) {
			return waitEnd(res, "timed out after "+timeout.String(), time.Since(startTime), report)
		}
		elapsed := int(time.Since(startTime).Seconds())
		report(fmt.Sprintf("[4/4] ⏳ Loading model layers into memory (%ds)...", elapsed))

		resp, err := client.Get(healthURL)
		if err == nil && resp.StatusCode == 200 {
			_ = resp.Body.Close()
			ready = true
			break
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
		// This machine may not reach the master's address (a laptop away from the LAN, a master reached only through
		// the hub): ask the master itself over ssh, from the second failed poll on.
		if !isLocalMaster && elapsed >= 3 && remoteHealthy(ctx, res.MasterNode, masterPort) {
			ready = true
			// healthy there: is it only that it just became healthy, or is this machine really cut off from it?
			if resp, err := client.Get(healthURL); err == nil && resp.StatusCode == 200 {
				_ = resp.Body.Close()
			} else {
				if resp != nil {
					_ = resp.Body.Close()
				}
				viaSSH = true
			}
			break
		}
		// Has the master died? (then the wait is pointless: say why now, not after the timeout)
		select {
		case werr := <-masterExited:
			return fail(true, fmt.Errorf("the master process (pid %d) exited before it was healthy (%v); %s:\n%s", res.MasterPID, werr, logFile, tailFile(logFile, 20)))
		default:
		}
		if !isLocalMaster && res.MasterPID > 0 && time.Since(lastCheck) >= 20*time.Second {
			lastCheck = time.Now()
			if remoteDead(ctx, res.MasterNode, res.MasterPID) {
				tail := remoteTail(ctx, res.MasterNode, res.RunID, 20)
				return fail(true, fmt.Errorf("the master on %s (pid %d) exited before it was healthy; ~/.cache/fleet/runs/%s.log:\n%s", res.MasterNode, res.MasterPID, res.RunID, tail))
			}
		}
		if time.Since(lastLog) >= 30*time.Second {
			lastLog = time.Now()
			var ln string
			if isLocalMaster {
				ln = lastLine(tailFile(logFile, 1))
			} else {
				ln = lastLine(remoteTail(ctx, res.MasterNode, res.RunID, 1))
			}
			if ln != "" {
				report("[4/4]   log: " + ln)
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(1500 * time.Millisecond):
		}
	}

	report("✓ Model loaded and healthy!")
	checkSpecFeatures(ctx, res, isLocalMaster)

	// 4. Register in Fleet Configuration
	configPath := opts.ConfigPath
	if configPath == "" {
		configPath = config.Path()
	}

	groupAddrs := make(map[string]string)
	for _, wNode := range workers {
		groupAddrs[wNode] = net.JoinHostPort(ResolveNodeIP(wNode), strconv.Itoa(res.workerPort(wNode)))
	}

	ep := config.Endpoint{
		Name:        res.EndpointName,
		Node:        res.MasterNode,
		URL:         res.URL,
		Model:       filepath.Base(opts.ModelPath),
		ModelSHA256: res.ModelSHA256,
		Ctx:         res.Ctx, // what the master runs with, not what was asked for (0 = the default / the strategy's)
		Slots:       res.Np,
		Group: &config.Group{
			Engine:    "llama-ring",
			Workers:   res.WorkerNodes,
			StatsPort: res.statsPort, // the port the master really got RING_STATS_PORT for (0 asked = the default), not what was asked
			Addrs:     groupAddrs,
			Streams:   res.streamAddrs(),
			Hub:       res.HubURL,
		},
		Caps: func() []string {
			caps := []string{"chat"}
			if opts.MMProj != "" {
				caps = append(caps, "vision")
			}
			return caps
		}(),
		Tags: []string{"distributed", res.Strategy, "llama-ring"},
	}
	// The master answered /health over ssh but not at its registered address: the URL is a LAN address this machine
	// cannot reach. Say so on the endpoint (tag) and in the output instead of letting a later probe from here
	// report "connection refused" as a dead ring.
	if viaSSH {
		res.Reach = "ssh-only"
		// This machine's daemon reaches it the way it reaches every other off-LAN endpoint: an ssh tunnel to the master
		// (ssh + remote), not the LAN address it cannot dial (that showed the endpoint as down here).
		ep.Tags = append(ep.Tags, "lan-only")
		ep.URL = ""
		ep.SSH = res.MasterNode
		ep.Remote = net.JoinHostPort("127.0.0.1", strconv.Itoa(masterPort))
		w := fmt.Sprintf("this machine cannot reach the master's address %s directly (not on its LAN?): the endpoint is registered as ssh %s -> 127.0.0.1:%d, so this machine's fleet daemon tunnels to it (for a manual connection: ssh -N -L %d:127.0.0.1:%d %s then http://127.0.0.1:%d/v1); `fleet profile bench` tunnels by itself", res.URL, res.MasterNode, masterPort, masterPort, masterPort, res.MasterNode, masterPort)
		res.Warnings = append(res.Warnings, w)
		report("⚠ " + w)
	}

	savedName, epWarns, saveErr := RegisterEndpointInConfig(configPath, ep, opts.Role, cfg, opts.EndpointName != "")
	ep.Name = savedName
	res.EndpointName = savedName
	res.Warnings = append(res.Warnings, epWarns...)
	if saveErr != nil {
		for _, w := range epWarns {
			report("⚠ " + w)
		}
	}

	res.Status = StatusRunning
	res.saveState()
	return res, nil
}

// RegisterEndpointInConfig updates or appends ep in the configuration at configPath,
// assigns it to targetRole if non-empty, and saves the file under a lock.
// If explicitName is false, it reuses any existing endpoint name matching the model with prefix "distributed-".
// It returns the saved endpoint name, any warnings encountered, and an error if saving failed.
func RegisterEndpointInConfig(configPath string, ep config.Endpoint, targetRole string, fallbackCfg *config.Config, explicitName bool) (string, []string, error) {
	if configPath == "" {
		configPath = config.Path()
	}
	var warnings []string
	unlockCfg, lerr := config.Lock(configPath)
	if lerr != nil {
		warnings = append(warnings, lerr.Error()+" (going on without the lock)")
		unlockCfg = func() {}
	}
	defer unlockCfg()

	cfgUpdated, err := config.Load(configPath)
	if err != nil {
		// only a missing file may start from scratch: an unreadable or half-edited one would be replaced wholesale
		// by the defaults plus this endpoint, silently dropping every other endpoint and role
		if _, serr := os.Stat(configPath); !os.IsNotExist(serr) {
			return "", warnings, fmt.Errorf("the configuration %s could not be read, the endpoint was NOT registered: %w", configPath, err)
		}
		if fallbackCfg != nil {
			cfgUpdated = fallbackCfg
		} else {
			cfgUpdated = config.Default()
		}
	}

	found := false
	for i, existing := range cfgUpdated.Endpoints {
		if existing.Name == ep.Name || (!explicitName && existing.Model == ep.Model && strings.HasPrefix(existing.Name, "distributed-")) {
			ep.Name = existing.Name
			cfgUpdated.Endpoints[i] = ep
			found = true
			break
		}
	}
	if !found {
		cfgUpdated.Endpoints = append(cfgUpdated.Endpoints, ep)
	}

	if targetRole != "" {
		if cfgUpdated.Roles == nil {
			cfgUpdated.Roles = make(map[string]config.Role)
		}
		roleObj := cfgUpdated.Roles[targetRole]
		if roleObj.Preferred == "" {
			roleObj.Preferred = ep.Name
		} else if roleObj.Preferred != ep.Name {
			roleObj.Fallback = append([]string{roleObj.Preferred}, roleObj.Fallback...)
			roleObj.Preferred = ep.Name
		}
		cfgUpdated.Roles[targetRole] = roleObj
	}

	if err := cfgUpdated.Save(configPath); err != nil {
		w := fmt.Sprintf("the endpoint %s could not be saved in %s (%v): the model runs, but fleet does not know it; add it by hand (fleet config endpoint …)", ep.Name, configPath, err)
		warnings = append(warnings, w)
		return ep.Name, warnings, err
	}
	return ep.Name, warnings, nil
}

// waitEnd ends a deploy whose master is started but not (yet) healthy because the wait timed out or was interrupted:
// nothing is stopped, the record says so, and the returned *WaitError tells the user what runs and how to stop it.
func waitEnd(res *DeployResult, reason string, elapsed time.Duration, report func(string)) (*DeployResult, error) {
	res.Status = StatusTimeout
	if reason == "interrupted" {
		res.Status = StatusStopped
	}
	res.Error = "master not healthy: " + reason
	res.saveState()
	e := &WaitError{Res: res, Reason: reason, Elapsed: elapsed}
	report("⚠ " + strings.SplitN(e.Error(), "\n", 2)[0])
	return nil, e
}

func tailFile(path string, n int) string {
	b, _ := os.ReadFile(path)
	return tailLines(string(b), n)
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		s = s[i+1:]
	}
	if len(s) > 160 {
		s = s[:160]
	}
	return s
}

// remoteTail is the last n lines of a run's master log on node.
func remoteTail(ctx context.Context, node, runID string, n int) string {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, _ := sshScript(cctx, node, fmt.Sprintf("tail -n %d ~/.cache/fleet/runs/%s.log 2>/dev/null\n", n, runID))
	return strings.TrimSpace(string(out))
}

// remoteDead says whether pid is certainly gone on node (an ssh failure is "unknown", not dead).
func remoteDead(ctx context.Context, node string, pid int) bool {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := sshScript(cctx, node, fmt.Sprintf("if kill -0 %d 2>/dev/null && ! grep -q '^State:[[:space:]]*Z' /proc/%d/status 2>/dev/null; then echo FLEET_ALIVE; else echo FLEET_GONE; fi\n", pid, pid))
	return err == nil && strings.Contains(string(out), "FLEET_GONE")
}

// Redacted returns a copy of the result safe to print (--json, logs): the values of --master-env / --worker-env
// (they may be tokens) are replaced by "***". The record file keeps them (a watchdog redeploy needs them) and is 0600.
func (r *DeployResult) Redacted() *DeployResult {
	if r == nil || r.Options == nil {
		return r
	}
	c := *r
	o := *r.Options
	if len(o.MasterEnvExtra) > 0 {
		o.MasterEnvExtra = map[string]string{}
		for k := range r.Options.MasterEnvExtra {
			o.MasterEnvExtra[k] = "***"
		}
	}
	if len(o.WorkerEnvExtra) > 0 {
		o.WorkerEnvExtra = map[string]map[string]string{}
		for n, m := range r.Options.WorkerEnvExtra {
			cp := map[string]string{}
			for k := range m {
				cp[k] = "***"
			}
			o.WorkerEnvExtra[n] = cp
		}
	}
	c.Options = &o
	return &c
}

// stopMasterAfterSSHFailure stops a fleet master that may have been started by a launch whose ssh failed (var: tests).
var stopMasterAfterSSHFailure = func(ctx context.Context, node string, port int) error {
	return stopOnPort(ctx, node, false, port, masterProcNames)
}

// endpointHealthy asks an endpoint's /health (var: tests).
var endpointHealthy = func(ep config.Endpoint) bool {
	base := strings.TrimSuffix(strings.TrimSuffix(ep.URL, "/"), "/v1")
	if base == "" {
		return false
	}
	c := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := c.Get(base + "/health")
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == 200
}

// otherRingOnPort names a registered, healthy ring endpoint other than the one being deployed that serves on port of
// the same machine as the new master ("" when none).
func otherRingOnPort(cfg *config.Config, res *DeployResult, modelPath string, port int) string {
	if cfg == nil {
		return ""
	}
	for _, ep := range cfg.Endpoints {
		if ep.Group == nil || ep.Group.Engine != "llama-ring" || ep.Name == res.EndpointName || sameModel(ep.Model, modelPath) {
			continue
		}
		if ep.Node != res.MasterNode && !(isThisMachine(ep.Node) && isThisMachine(res.MasterNode)) {
			continue
		}
		p := urlPort(ep.URL)
		if p == 0 && ep.Remote != "" {
			if _, ps, err := net.SplitHostPort(ep.Remote); err == nil {
				p, _ = strconv.Atoi(ps)
			}
		}
		if p == port && endpointHealthy(ep) {
			return ep.Name
		}
	}
	return ""
}

// sameModel: the endpoint's model is the file being deployed (a redeploy of it replaces the endpoint on purpose).
func sameModel(epModel, modelPath string) bool {
	b := filepath.Base(modelPath)
	return epModel == b || epModel == modelPath || (b != "" && b != "." && strings.HasSuffix(epModel, b))
}

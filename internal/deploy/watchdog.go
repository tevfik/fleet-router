package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
)

// Watchdog monitors distributed cluster health. A ring whose engine has lost a worker is unhealthy even though
// llama-server's /health says "ok": the engine's dispatcher stays failed until the process restarts, so the
// watchdog redeploys it from the options of its last run.
type Watchdog struct {
	cfg     *config.Config
	rwm     *RemoteWorkerManager
	logFile string

	// Replaceable for tests.
	Failed   func(ctx context.Context, ep config.Endpoint) ([]string, error) // GET /ring/info failed_workers
	Redeploy func(ctx context.Context, ep config.Endpoint) error
	// SSHHealth asks the master's own machine (ssh + curl on 127.0.0.1) whether its server answers /health: the
	// fallback when the registered LAN address is not reachable from here (this machine is not on that LAN).
	SSHHealth func(ctx context.Context, node string, port int) bool
	Now       func() time.Time
	// Supervised says whether the endpoint's master was started with GGML_RING_EXIT_ON_FAIL=1 (from its last run's
	// options): then a master that is gone after a failed request is expected and is redeployed.
	Supervised func(ep config.Endpoint) bool

	Cooldown    time.Duration // minimum time between two restarts of one endpoint (default 5m)
	MaxRestarts int           // restarts of one endpoint before the watchdog gives up and only logs (default 3)
	// StableAfter: an endpoint that has been healthy this long gets its restart count back (default 1h). Without it
	// the limit was for the watchdog's whole life: a ring that fails once a week was unsupervised after the third.
	StableAfter time.Duration

	// Chains (chainwatch.go). Replaceable for tests.
	ChainRecord func(endpoint string) (*DeployResult, error)
	ChainHealth func(ctx context.Context, ep config.Endpoint) ChainHealth
	StageAlive  func(s StageLaunch) bool
	Stages      StageRunner // starts a dead stage again
	// Config, when set, is read on every pass (a daemon's configuration changes; endpoints deployed later are watched too)
	Config func() *config.Config
	// ChainsOnly: watch chains, leave rings alone (the daemon supervises chains; rings stay with `fleet watchdog`).
	// SkipChains: the reverse (`fleet watchdog` when the daemon already supervises the chains).
	ChainsOnly, SkipChains bool

	state map[string]*wdState
	// A redeploy can take many minutes: it runs on its own goroutine (one per endpoint) so the other endpoints keep
	// being watched. CheckOnce waits for them; Run does not.
	wg sync.WaitGroup
	mu sync.Mutex // guards wdState.restarting (the redeploy goroutines clear it)
}

type wdState struct {
	restarts int
	down     int  // consecutive checks with the master unreachable
	seenUp   bool // this watchdog has seen the master answer: only such a master that is gone later is redeployed
	last     time.Time
	// healthySince: start of the current run of healthy checks (zero: not healthy now).
	healthySince time.Time
	restarting   bool // a redeploy of this endpoint is in progress
}

// NewWatchdog creates a new cluster Watchdog.
func NewWatchdog(cfg *config.Config) *Watchdog {
	cacheDir, _ := os.UserCacheDir()
	logDir := filepath.Join(cacheDir, "fleet", "runs")
	_ = os.MkdirAll(logDir, 0755)

	w := &Watchdog{
		cfg:         cfg,
		rwm:         NewRemoteWorkerManager(),
		logFile:     filepath.Join(logDir, "watchdog.log"),
		Now:         time.Now,
		Cooldown:    5 * time.Minute,
		MaxRestarts: 3,
		StableAfter: time.Hour,
		state:       map[string]*wdState{},
	}
	w.ChainRecord = LastChainRecord
	w.ChainHealth = chainIngressHealth
	w.StageAlive = stageAlive
	w.Stages = &LiveStageRunner{}
	w.Failed = w.ringFailed
	w.SSHHealth = sshHealth
	w.Redeploy = w.redeployFromLastRun
	w.Supervised = func(ep config.Endpoint) bool {
		o, err := LastRunOptions(ep.Name)
		return err == nil && o.ExitOnFail
	}
	return w
}

func (w *Watchdog) logf(format string, args ...any) {
	msg := fmt.Sprintf("[%s] %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
	f, err := os.OpenFile(w.logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err == nil {
		_, _ = f.WriteString(msg)
		_ = f.Close()
	}
}

// Run starts the watchdog loop.
func (w *Watchdog) Run(ctx context.Context, interval time.Duration) {
	w.logf("Cluster Watchdog started (interval %v)", interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	httpClient := &http.Client{Timeout: 2 * time.Second}

	for {
		select {
		case <-ctx.Done():
			w.logf("Cluster Watchdog stopped")
			return
		case <-ticker.C:
			w.checkCluster(ctx, httpClient)
		}
	}
}

// CheckOnce runs one pass over every ring endpoint.
func (w *Watchdog) CheckOnce(ctx context.Context) {
	w.checkCluster(ctx, &http.Client{Timeout: 2 * time.Second})
	w.wg.Wait() // a one-off pass reports what its redeploys did
}

func (w *Watchdog) checkCluster(ctx context.Context, client *http.Client) {
	cfg := w.cfg
	if w.Config != nil {
		cfg = w.Config()
	}
	if cfg == nil {
		return
	}
	for _, ep := range cfg.Endpoints {
		if ep.Group != nil && ep.Group.Engine == "chain" {
			if !w.SkipChains && ep.Peer == "" {
				w.checkChain(ctx, ep)
			}
			continue
		}
		if w.ChainsOnly || ep.Group == nil || ep.Group.Engine != "llama-ring" {
			continue
		}

		// Check master health
		healthURL := fmt.Sprintf("%s/health", strings.TrimSuffix(ep.URL, "/v1"))
		var resp *http.Response
		var err error
		healthy := false
		viaSSH := false
		if ep.URL == "" && ep.SSH != "" {
			// Registered as ssh + remote (a master this machine is not on the LAN of): 127.0.0.1:<port> here is NOT
			// the master, so ask the master's own machine.
			healthURL = fmt.Sprintf("ssh %s %s/health", ep.SSH, ep.Remote)
			if _, p, perr := net.SplitHostPort(ep.Remote); perr == nil && w.SSHHealth != nil {
				if n, _ := strconv.Atoi(p); n > 0 && w.SSHHealth(ctx, ep.SSH, n) {
					healthy, viaSSH = true, true
				}
			}
			if !healthy {
				err = fmt.Errorf("no answer over ssh")
			}
		} else {
			resp, err = client.Get(healthURL)
			healthy = err == nil && resp.StatusCode == 200
			if !healthy && !isLocalEndpoint(ep) && ep.Node != "" && w.SSHHealth != nil {
				// From outside the LAN the registered address is refused although the ring is fine: ask its own machine
				// before calling it gone (two such false alarms redeploy an exit-on-fail ring that is running).
				if p := urlPort(ep.URL); p > 0 && w.SSHHealth(ctx, ep.Node, p) {
					healthy, viaSSH = true, true
				}
			}
		}
		if !healthy {
			w.logf("WARNING: Master endpoint %s (%s) unreachable: %v", ep.Name, healthURL, err)
		}
		if resp != nil {
			_ = resp.Body.Close()
		}

		// A master started with GGML_RING_EXIT_ON_FAIL=1 leaves (status 75) after a failed ring request, so it
		// answers nothing instead of reporting failed_workers: two checks in a row without an answer redeploy it
		// (same cooldown and restart limit as the failed_workers path). A master without the flag is never
		// restarted just for being unreachable (it may be loading a large model). Nor is one this watchdog never saw
		// up: an endpoint that was stopped on purpose (fleet run end) or never started stays down, a watchdog that
		// starts later does not bring it back (it once redeployed a parked 122B that way).
		st := w.stateOf(ep.Name)
		if healthy {
			st.down, st.seenUp = 0, true
			if st.healthySince.IsZero() {
				st.healthySince = w.Now()
			} else if st.restarts > 0 && w.StableAfter > 0 && w.Now().Sub(st.healthySince) >= w.StableAfter {
				w.logf("%s has been healthy for %s: its restart count (%d) is reset", ep.Name, w.StableAfter, st.restarts)
				st.restarts = 0
			}
		} else {
			st.healthySince = time.Time{}
		}
		if healthy {
			// nothing more here
		} else if st.down++; st.down >= 2 && st.seenUp && w.Supervised != nil && w.Supervised(ep) {
			w.logf("ALERT: %s is gone and was started with exit-on-fail: redeploying", ep.Name)
			st.down = 0
			w.restart(ctx, ep)
			continue
		}

		// Check workers (their LAN addresses can only be judged from the LAN: not when the master was reached by ssh)
		for node, addr := range ep.Group.Addrs {
			if viaSSH {
				break
			}
			if s, ok := ep.Group.Streams[node]; ok {
				// a worker reached through the hub: only its stream listener on the master can be dialed, and only
				// from the master's machine
				if !isLocalEndpoint(ep) {
					continue
				}
				addr = s
			}
			if !w.rwm.IsTCPPortOpen(addr, 500*time.Millisecond) {
				w.logf("ALERT: Worker %s on %s is not answering on TCP %s", node, ep.Name, addr)
			}
		}

		// /health stays ok when the engine lost a worker; /ring/info says so.
		failed, err := w.Failed(ctx, ep)
		if err != nil || len(failed) == 0 {
			continue
		}
		w.logf("ALERT: %s lost its ring worker(s) %s: the engine stays failed until restarted", ep.Name, strings.Join(failed, ", "))
		st.healthySince = time.Time{}
		w.restart(ctx, ep)
	}
}

// urlPort is the port of an endpoint's URL (0: none).
func urlPort(raw string) int {
	u, err := url.Parse(raw)
	if err != nil {
		return 0
	}
	p, _ := strconv.Atoi(u.Port())
	return p
}

// sshHealth: `curl 127.0.0.1:<port>/health` on the master's machine.
func sshHealth(ctx context.Context, node string, port int) bool {
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=4", "--", node,
		fmt.Sprintf("curl -s -o /dev/null -w %%{http_code} --max-time 3 http://127.0.0.1:%d/health", port)).Output()
	return err == nil && strings.TrimSpace(string(out)) == "200"
}

func (w *Watchdog) stateOf(name string) *wdState {
	st := w.state[name]
	if st == nil {
		st = &wdState{}
		w.state[name] = st
	}
	return st
}

func (w *Watchdog) restart(ctx context.Context, ep config.Endpoint) {
	w.restartWith(ctx, ep, w.Redeploy)
}

// restartWith runs fix for ep under the restart rules: at most MaxRestarts, not within Cooldown of the last one, one
// at a time per endpoint (on its own goroutine: a reload can take many minutes).
func (w *Watchdog) restartWith(ctx context.Context, ep config.Endpoint, fix func(context.Context, config.Endpoint) error) {
	st := w.stateOf(ep.Name)
	now := w.Now()
	if w.MaxRestarts > 0 && st.restarts >= w.MaxRestarts {
		w.logf("GIVING UP: %s was restarted %d times and is failed again: needs a look", ep.Name, st.restarts)
		return
	}
	if !st.last.IsZero() && now.Sub(st.last) < w.Cooldown {
		w.logf("restart of %s postponed: last one %s ago (cooldown %s)", ep.Name, now.Sub(st.last).Round(time.Second), w.Cooldown)
		return
	}
	w.mu.Lock()
	if st.restarting {
		w.mu.Unlock()
		return // its redeploy is still running
	}
	st.restarting = true
	w.mu.Unlock()
	st.restarts++
	st.last = now
	w.logf("restarting %s (%d/%d)", ep.Name, st.restarts, w.MaxRestarts)
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		err := fix(ctx, ep)
		w.mu.Lock()
		st.restarting = false
		w.mu.Unlock()
		if err != nil {
			w.logf("restart of %s failed: %v", ep.Name, err)
			return
		}
		w.logf("%s redeployed", ep.Name)
	}()
}

// ringFailed reads failed_workers from the engine's statistics port (127.0.0.1 of the master's machine).
func (w *Watchdog) ringFailed(ctx context.Context, ep config.Endpoint) ([]string, error) {
	if ep.Group == nil || ep.Group.StatsPort <= 0 {
		return nil, fmt.Errorf("no stats port")
	}
	body, err := fetchRingInfo(ctx, ep.Node, isLocalEndpoint(ep), ep.Group.StatsPort)
	if err != nil {
		return nil, err
	}
	return parseFailedWorkers(body)
}

// fetchRingInfo returns the body of GET /ring/info from the statistics port, which listens on 127.0.0.1 of the
// master's machine: directly when that is this machine, through ssh+curl otherwise.
func fetchRingInfo(ctx context.Context, node string, local bool, statsPort int) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	statsURL := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(statsPort)) + "/ring/info"
	if local {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, statsURL, nil)
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		return io.ReadAll(resp.Body)
	}
	return exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=3", "--", node,
		"curl -s --max-time 2 "+statsURL).Output()
}

// parseFailedWorkers reads the failure from a /ring/info body (an engine without the fields has none). The dispatcher
// stays failed until restart, so ANY failure_report entry means a broken ring: when failed_workers is empty the report's
// workers are returned, and a report with only victims comes back as one "inconsistent report" line.
func parseFailedWorkers(body []byte) ([]string, error) {
	var info struct {
		Failed []string `json:"failed_workers"`
		Report []struct {
			Worker string `json:"worker"`
			Role   string `json:"role"`
		} `json:"failure_report"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, err
	}
	if len(info.Failed) > 0 || len(info.Report) == 0 {
		return info.Failed, nil
	}
	var blamed, victims []string
	for _, r := range info.Report {
		if r.Role == "victim" {
			victims = append(victims, r.Worker)
		} else {
			blamed = append(blamed, r.Worker)
		}
	}
	if len(blamed) == 0 {
		return []string{"(inconsistent report (no culprit named); victims: " + strings.Join(victims, ", ") + ")"}, nil
	}
	return blamed, nil
}

func isLocalEndpoint(ep config.Endpoint) bool {
	host, _ := os.Hostname()
	if ep.Node == "" || ep.Node == "localhost" || ep.Node == "127.0.0.1" || ep.Node == host {
		return true
	}
	if u, err := url.Parse(ep.URL); err == nil {
		h := u.Hostname()
		return h == "127.0.0.1" || h == "localhost"
	}
	return false
}

// redeployFromLastRun starts the endpoint again with the options of its newest saved run.
func (w *Watchdog) redeployFromLastRun(ctx context.Context, ep config.Endpoint) error {
	opts, err := LastRunOptions(ep.Name)
	if err != nil {
		return err
	}
	// A redeploy by the watchdog always asks the engine to exit on a failed ring, so the next failure is also
	// handled by a restart instead of leaving a permanently failed dispatcher behind.
	opts.ExitOnFail = true
	opts.Replace = true
	_, err = ExecuteDeployment(ctx, w.cfg, *opts)
	return err
}

// LastRunOptions returns the deploy options of the newest saved run of an endpoint.
func LastRunOptions(endpoint string) (*DeployOptions, error) {
	cacheDir, _ := os.UserCacheDir()
	files, _ := filepath.Glob(filepath.Join(cacheDir, "fleet", "runs", "run-*.json"))
	sort.Strings(files) // run-<unix seconds>: the newest sorts last
	for i := len(files) - 1; i >= 0; i-- {
		b, err := os.ReadFile(files[i])
		if err != nil {
			continue
		}
		var r DeployResult
		if json.Unmarshal(b, &r) != nil || r.EndpointName != endpoint || r.Options == nil {
			continue
		}
		switch r.Status { // a record of a deploy that did not come up is not something to redeploy from
		case StatusStarting, StatusFailed, StatusTimeout, StatusStopped:
			continue
		}
		o := *r.Options
		o.DryRun = false
		return &o, nil
	}
	return nil, fmt.Errorf("no saved run with options for %s (deploy it once with this fleet)", endpoint)
}

// sshHealthCode asks node itself for http://127.0.0.1:port/health and returns the status (0: no answer).
func sshHealthCode(ctx context.Context, node string, port int) int {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=4", "--", node,
		fmt.Sprintf("curl -s -o /dev/null -w %%{http_code} --max-time 15 http://127.0.0.1:%d/health", port)).Output()
	if err != nil && len(out) == 0 {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n
}

// splitHostPort is net.SplitHostPort with the port as a number.
func splitHostPort(hp string) (string, int, error) {
	h, p, err := net.SplitHostPort(hp)
	if err != nil {
		return "", 0, err
	}
	n, err := strconv.Atoi(p)
	return h, n, err
}

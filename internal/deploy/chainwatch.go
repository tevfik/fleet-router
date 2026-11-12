package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
)

// Chain supervision. A chain is stages that each hold a part of the model: a stage that dies (a machine that rebooted,
// an OOM kill, a crash) is started again alone from the command it was started with; the stages that kept running
// keep their loaded weights, and the driver (stage 0) makes its link to the next stage again by itself
// (fleet-engine ≥ 0.5.5: the link is remade at the next request or health check). Restarting only the dead stage
// matters: reloading a 122B split takes ten minutes.
//
// Health: the driver's /health answers 200 while its next stage answers, 503 when it does not, nothing when the
// driver itself is gone. Which stage is dead is told by its port: a stage that listens is alive.

// ChainHealth is what one look at a chain's ingress says.
type ChainHealth int

const (
	ChainOK       ChainHealth = iota
	ChainNextDown             // the driver answers but its next stage does not (503)
	ChainGone                 // the driver does not answer
)

func (h ChainHealth) String() string {
	switch h {
	case ChainOK:
		return "ok"
	case ChainNextDown:
		return "the driver answers, its next stage does not"
	}
	return "the driver does not answer"
}

// chainIngressHealth asks the chain's ingress for /health (the endpoint URL without /v1; an ssh-registered endpoint
// is asked on its own machine).
func chainIngressHealth(ctx context.Context, ep config.Endpoint) ChainHealth {
	code := 0
	if ep.URL != "" {
		c := &http.Client{Timeout: 15 * time.Second} // the driver probes its next stage before it answers
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(strings.TrimSuffix(ep.URL, "/"), "/v1")+"/health", nil)
		if resp, err := c.Do(req); err == nil {
			code = resp.StatusCode
			resp.Body.Close()
		}
	}
	if code == 0 && ep.Node != "" && !isLocalEndpoint(ep) {
		if p := urlPort(ep.URL); p > 0 {
			code = sshHealthCode(ctx, ep.Node, p)
		} else if ep.SSH != "" {
			if _, ps, err := splitHostPort(ep.Remote); err == nil && ps > 0 {
				code = sshHealthCode(ctx, ep.SSH, ps)
			}
		}
	}
	switch {
	case code == 200:
		return ChainOK
	case code == 503:
		return ChainNextDown
	}
	return ChainGone
}

// stageAlive: does the stage's process listen on its port? Stage 0 is asked by its port too (its /health also
// speaks for the next stage).
func stageAlive(s StageLaunch) bool {
	probe := s
	probe.Ready = Readiness{Kind: "tcp", Addr: s.Ready.Addr}
	if probe.Ready.Addr == "" {
		probe.Ready.Addr = fmt.Sprintf("%s:%d", s.Host, s.Port)
	}
	if defaultIsThisMachine(s.Node) {
		return directReady(loopbackReady(probe))
	}
	if directReady(probe) {
		return true
	}
	return remoteWaitReady(probe, time.Second) == nil // its own machine (a LAN this one is not on)
}

// LastChainRecord is the newest record of a chain deployed as endpoint that is still meant to run (not stopped, not
// failed), with its stages.
func LastChainRecord(endpoint string) (*DeployResult, error) {
	cacheDir, _ := os.UserCacheDir()
	files, _ := filepath.Glob(filepath.Join(cacheDir, "fleet", "runs", "run-*.json"))
	sort.Strings(files)
	for i := len(files) - 1; i >= 0; i-- {
		b, err := os.ReadFile(files[i])
		if err != nil {
			continue
		}
		var r DeployResult
		if json.Unmarshal(b, &r) != nil || r.EndpointName != endpoint || r.Engine != "chain" {
			continue
		}
		switch r.Status {
		case StatusStarting, StatusFailed, StatusTimeout, StatusStopped:
			return nil, fmt.Errorf("the newest run of %s is %s", endpoint, r.Status)
		}
		if len(r.Stages) == 0 {
			return nil, fmt.Errorf("the run of %s has no stage commands (deployed by an older fleet: deploy it again to have it supervised)", endpoint)
		}
		return &r, nil
	}
	return nil, fmt.Errorf("no chain run of %s on this machine", endpoint)
}

// deadStages are the stages of rec whose process does not listen, last first (the start order of a chain).
func (w *Watchdog) deadStages(rec *DeployResult) []StageLaunch {
	var dead []StageLaunch
	for i := len(rec.Stages) - 1; i >= 0; i-- {
		if !w.StageAlive(rec.Stages[i]) {
			dead = append(dead, rec.Stages[i])
		}
	}
	return dead
}

// restartStages starts the given stages again, in the given order (last first), each only after the one before it
// answers: the same start, port clean-up and readiness wait as the deploy.
func (w *Watchdog) restartStages(ctx context.Context, rec *DeployResult, dead []StageLaunch) error {
	timeout := rec.ReadyTimeout
	if timeout <= 0 {
		timeout = 20 * time.Minute
	}
	for _, s := range dead {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		w.logf("%s: starting stage %d on %s again (port %d)", rec.EndpointName, s.Index, s.Node, s.Port)
		if err := w.Stages.Cleanup(s.Node, s.Port); err != nil {
			return fmt.Errorf("stage %d on %s: %w", s.Index, s.Node, err)
		}
		if err := w.Stages.Launch(s); err != nil {
			return err
		}
		if err := w.Stages.WaitReady(s, timeout); err != nil {
			return fmt.Errorf("stage %d on %s: %w", s.Index, s.Node, err)
		}
		w.logf("%s: stage %d on %s is up again", rec.EndpointName, s.Index, s.Node)
	}
	return nil
}

// checkChain is one look at a chain endpoint: two unhealthy looks in a row, on a chain this watchdog has seen
// healthy, restart its dead stages (cooldown and restart limit as for rings). A chain whose stages all listen but
// that is still unhealthy (a network between them) is reported, not restarted.
func (w *Watchdog) checkChain(ctx context.Context, ep config.Endpoint) {
	rec, err := w.ChainRecord(ep.Name)
	if err != nil {
		return // not deployed from this machine, stopped, or too old to know its stages
	}
	h := w.ChainHealth(ctx, ep)
	st := w.stateOf(ep.Name)
	if h == ChainOK {
		if st.down > 0 {
			w.logf("%s is healthy again", ep.Name)
		}
		st.down, st.seenUp = 0, true
		if st.healthySince.IsZero() {
			st.healthySince = w.Now()
		} else if st.restarts > 0 && w.StableAfter > 0 && w.Now().Sub(st.healthySince) >= w.StableAfter {
			w.logf("%s has been healthy for %s: its restart count (%d) is reset", ep.Name, w.StableAfter, st.restarts)
			st.restarts = 0
		}
		return
	}
	st.healthySince = time.Time{}
	st.down++
	w.logf("WARNING: chain %s: %s (%d in a row)", ep.Name, h, st.down)
	if st.down < 2 || !st.seenUp {
		return
	}
	dead := w.deadStages(rec)
	if len(dead) == 0 {
		w.logf("chain %s is unhealthy but every stage listens: not restarting anything (the network between them?)", ep.Name)
		return
	}
	var names []string
	for _, s := range dead {
		names = append(names, fmt.Sprintf("%d on %s", s.Index, s.Node))
	}
	w.logf("ALERT: chain %s lost stage(s) %s", ep.Name, strings.Join(names, ", "))
	st.down = 0
	w.restartWith(ctx, ep, func(ctx context.Context, _ config.Endpoint) error { return w.restartStages(ctx, rec, dead) })
}

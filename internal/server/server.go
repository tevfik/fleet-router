// Package server is the fleet daemon — health monitoring, SSH tunnels, a role-based OpenAI-compatible router and the admin API.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/feature"
	"github.com/tevfik/fleet-router/internal/hostinfo"
	"github.com/tevfik/fleet-router/internal/mdns"
	"github.com/tevfik/fleet-router/internal/tunnel"
)

type EpState struct {
	OK       bool        `json:"ok"`
	Latency  int64       `json:"latency_ms"`
	Err      string      `json:"err,omitempty"`
	Checked  time.Time   `json:"checked"`
	penalty  time.Time   // passive failure: ordered last until this time
	Why      string      `json:"why,omitempty"`       // what set the penalty aside (a stall), while it lasts
	Group    *GroupState `json:"group,omitempty"`     // attached group: what the engine and the members say (group.go)
	OnDemand bool        `json:"on_demand,omitempty"` // a distributed endpoint whose run is not started (runs are on demand): not a failure
	noConn   bool        // the last check never reached a server (dial/transport failure)
}

type Server struct {
	mu           sync.RWMutex
	cfg          *config.Config
	path         string
	listenFile   string // the listen address the file had when SetListen overrode it
	listenOver   string // the override ("" = none)
	state        map[string]*EpState
	tunnels      *Tunnels
	client       *http.Client
	ctx          context.Context
	kick         chan struct{}
	lims         map[string]*limiter // "ep:<name>" and "role:<name>"
	bench        benchState
	live         liveStore
	pickMu       sync.Mutex // serialises strategy ordering + slot grabbing (see proxyV1)
	reqs         reqLog
	rr           sync.Map          // role → *atomic.Uint64 (round-robin counters)
	peerRT       map[string]peerRT // last sync result per peer
	peerKick     chan struct{}
	lan          *mdns.Node                    // local network discovery (nil when off)
	pings        pingStore                     // per-node round trips (nodes.go)
	probes       probeState                    // capability probes (probe.go)
	host         *hostinfo.Sampler             // this machine's readings (host.go)
	hosts        hostCache                     // other machines' readings, briefly cached
	planSnap     planSnapshot                  // the machines figures of the last plan, for the layer editor (plan.go)
	hwb          hwBenchState                  // this machine's benchmark (hwbench.go)
	warm         warmStore                     // loaded models and sizes, from the runtimes (warm.go)
	strain       strainStore                   // which machines are struggling (strain.go)
	pair         pairState                     // the open pairing code (pair.go)
	steps        map[string]stepMark           // per group endpoint: the ring's step counter and when it last moved (group.go)
	resumed      map[string]map[string]uint64  // per group endpoint -> worker addr -> last seen resumed count (group.go)
	runs         runStore                      // distributed runs this fleet planned (runs.go)
	files        fileStore                     // this machine's model files (store.go)
	routes       func(node string) []nodeRoute // tests: how to reach a machine (nil: from the configuration)
	hub          *tunnel.Hub                   // inbound WebSocket edge tunnels
	tmu          sync.Mutex                    // guards tunnelClient, tunnelHubURL, joinGen
	tunnelClient *tunnel.Client                // outbound tunnel client when joined to a Hub
	tunnelHubURL string
	joinGen      int       // bumped by every join/leave: a stale auto-join retry gives up
	joinState    joinState // the outcome of the latest join attempts (hubjoin.go)
}

func New(path string) (*Server, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg: cfg, path: path, state: map[string]*EpState{},
		tunnels: newTunnels(),
		// NO overall timeout: streams (SSE) run long; connection setup is bounded in the Transport. The dial needs its own
		// timeout: the Transport detaches the dial from the request's context, so without one a dial to a host that
		// drops SYNs outlives its request by ~2 minutes (tcp_syn_retries) and health rounds piled up dozens of them.
		client: &http.Client{Transport: &http.Transport{
			DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout: 10 * time.Second,
			MaxIdleConnsPerHost: 16, IdleConnTimeout: 90 * time.Second,
			ResponseHeaderTimeout: 10 * time.Minute,
		}},
		kick: make(chan struct{}, 1), peerKick: make(chan struct{}, 1),
		lims: map[string]*limiter{}, host: hostinfo.New(),
	}
	s.hub = tunnel.NewHub(func(reg tunnel.NodeRegistration) {
		s.onTunnelJoin(reg)
	}, func(nodeName string) {
		s.onTunnelDrop(nodeName)
	})
	s.hub.SetStreamAuth(s.streamAuth)
	s.hub.SetConnectAuth(s.connectAuth)
	return s, nil
}

func (s *Server) Config() *config.Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.cfg == nil { // a bare Server (tests): an empty configuration
		return &config.Config{}
	}
	c := *s.cfg
	return &c
}

// SetListen overrides the listen address in memory (e.g. from FLEET_LISTEN). The override is not configuration: SetConfig
// writes the address the file had, not this one.
func (s *Server) SetListen(addr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg != nil {
		s.listenFile, s.listenOver = s.cfg.Listen, addr
		s.cfg.Listen = addr
	}
}

// SetConfig validates the configuration, writes it to disk and applies it hot (sessions stay open).
func (s *Server) SetConfig(c *config.Config) error {
	unlock := s.lockFile()
	defer unlock()
	return s.setConfigLocked(c)
}

// UpdateConfig applies mut to the configuration as it is ON DISK (under the file lock) and saves it. For background
// writers (learned dimensions, measured speeds): editing the daemon's in-memory copy would write back a snapshot and
// erase anything `fleet deploy` or the CLI saved since — before the daemon reloaded. mut returns false for "no change".
func (s *Server) UpdateConfig(mut func(c *config.Config) bool) error {
	unlock := s.lockFile()
	defer unlock()
	c, err := config.Load(s.path)
	if err != nil {
		return fmt.Errorf("config: not saved, the file could not be read: %w", err)
	}
	s.mu.RLock()
	if s.listenOver != "" && c.Listen == s.listenFile {
		c.Listen = s.listenOver
	}
	s.mu.RUnlock()
	if !mut(c) {
		return nil
	}
	return s.setConfigLocked(c)
}

func (s *Server) lockFile() func() {
	unlock, err := config.Lock(s.path) // the same lock `fleet deploy` takes around its own load-modify-save
	if err != nil {
		log.Printf("config: %v (saving without the lock)", err)
		return func() {}
	}
	return unlock
}

func (s *Server) setConfigLocked(c *config.Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	// Endpoints that exist only because a node's tunnel is up are not configuration: they are not written to the file
	// (they used to stay in the YAML after the node left), and the ones in memory are the CURRENT ones, not those of
	// the copy this edit started from (a node that dropped meanwhile must not come back as a ghost endpoint).
	persist := *c
	persist.Endpoints = withoutTunnelEndpoints(c.Endpoints)
	s.mu.Lock()
	if s.listenOver != "" && persist.Listen == s.listenOver {
		persist.Listen = s.listenFile // a listen address from the environment stays out of the file
	}
	s.mu.Unlock()
	if err := persist.Save(s.path); err != nil {
		return err
	}
	s.adopt(c)
	return nil
}

// adopt makes c the configuration in memory, keeping what is not in the file: the endpoints of nodes joined over the
// tunnel and a listen address that came from the environment.
func (s *Server) adopt(c *config.Config) {
	s.mu.Lock()
	if s.cfg != nil {
		var live []config.Endpoint
		for _, ep := range s.cfg.Endpoints {
			if isTunnelEndpoint(ep) {
				live = append(live, ep)
			}
		}
		c.Endpoints = append(withoutTunnelEndpoints(c.Endpoints), live...)
	}
	if s.listenOver != "" && c.Listen == s.listenFile {
		c.Listen = s.listenOver
	}
	s.cfg = c
	s.mu.Unlock()
	s.syncTunnels()
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// Undo restores the previous configuration (see config.Undo) and applies it hot.
func (s *Server) Undo() (string, error) {
	name, err := config.Undo(s.path)
	if err != nil {
		return "", err
	}
	if err := s.Reload(); err != nil {
		return name, err
	}
	select {
	case s.kick <- struct{}{}:
	default:
	}
	return name, nil
}

func (s *Server) Reload() error {
	c, err := config.Load(s.path)
	if err != nil {
		return err
	}
	s.adopt(c) // a reload used to drop the endpoints of tunnel-joined nodes until they joined again
	return nil
}

// lim returns the named limiter, creating it if needed.
func (s *Server) lim(key string) *limiter {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.lims[key]
	if l == nil {
		l = &limiter{}
		s.lims[key] = l
	}
	return l
}

// syncLimits applies slots/max_parallel from the configuration to the live limiters (hot).
func (s *Server) syncLimits() {
	cfg := s.Config()
	for _, e := range cfg.Endpoints {
		s.lim("ep:" + e.Name).setCap(e.Slots)
	}
	for n, r := range cfg.Roles {
		s.lim("role:" + n).setCap(r.MaxParallel)
	}
}

func (s *Server) syncTunnels() {
	s.syncLimits()
	cfg := s.Config()
	s.tunnels.SetIdle(cfg.IdleDuration())
	for _, e := range cfg.Endpoints {
		if e.URL == "" && e.SSH != "" {
			s.tunnels.Ensure(s.ctx, e.SSH, e.Remote)
			// the machine's own fleet port rides along on the same connection (no extra login): the Nodes
			// screen reads the machine from it (host.go). Added with the models so the connection is not
			// restarted later, which would cut the requests running through it.
			s.tunnels.Ensure(s.ctx, e.SSH, e.FleetRemote())
			if sr := e.StatsRemote(); sr != "" { // the engine's statistics of a master on another machine
				s.tunnels.Ensure(s.ctx, e.SSH, sr)
			}
		}
	}
	for _, p := range cfg.Peers { // a peer reached over SSH needs its tunnel too
		if tr := p.Transport(); tr.URL == "" && tr.SSH != "" {
			s.tunnels.Ensure(s.ctx, tr.SSH, tr.Remote)
		}
	}
}

// BaseURL is the base (…/v1) used to reach the endpoint from this machine; "" if the tunnel is down.
func (s *Server) BaseURL(e *config.Endpoint) string {
	if e.URL != "" {
		return strings.TrimRight(e.URL, "/")
	}
	if p := s.tunnels.Port(e.SSH, e.Remote); p > 0 {
		return fmt.Sprintf("http://127.0.0.1:%d/v1", p)
	}
	return ""
}

// baseURLUse is BaseURL for real traffic: it reopens a tunnel that was closed for being idle, marks the
// tunnel as in use until release is called, and may therefore take a few seconds. Health checks use the
// plain BaseURL so that they do not keep tunnels open.
func (s *Server) baseURLUse(ctx context.Context, e *config.Endpoint) (string, func()) {
	if e.URL != "" {
		return strings.TrimRight(e.URL, "/"), func() {}
	}
	p, rel := s.tunnels.Use(ctx, e.SSH, e.Remote)
	if p > 0 {
		return fmt.Sprintf("http://127.0.0.1:%d/v1", p), rel
	}
	return "", rel
}

// statsLoop saves the speed statistics every 30 s (when they changed) and once more on shutdown.
func (s *Server) statsLoop(ctx context.Context, path string) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			if err := s.live.save(path); err != nil {
				log.Printf("could not save statistics: %v", err)
			}
			return
		case <-t.C:
			if err := s.live.save(path); err != nil {
				log.Printf("could not save statistics: %v", err)
			}
		}
	}
}

// Run starts the background work (tunnels + health) and serves HTTP.
func (s *Server) Run(ctx context.Context) error {
	s.ctx = ctx
	s.syncLimits()
	s.syncTunnels()
	statsFile := config.StatsPath()
	if err := s.live.load(statsFile); err != nil {
		log.Printf("could not read %s (starting with empty statistics): %v", statsFile, err)
	}
	s.ensureID()
	var loops sync.WaitGroup // Run returns only after they stop: nothing writes the stats or the config after it
	bg := []func(context.Context){func(c context.Context) { s.statsLoop(c, statsFile) }, s.healthLoop, s.peerLoop, s.lanLoop, s.hwBenchLoop, s.strainLoop, s.autoJoinLoop}
	if feature.Runner() {
		bg = append(bg, s.chainSuperviseLoop) // deployed chains exist only where fleet-engine is
	}
	for _, f := range bg {
		loops.Add(1)
		go func() {
			defer loops.Done()
			f(ctx)
		}()
	}
	cfg := s.Config()
	if host, _, err := net.SplitHostPort(cfg.Listen); err == nil && cfg.APIKeyEnv == "" {
		if ip := net.ParseIP(host); host == "" || (ip != nil && !ip.IsLoopback()) {
			log.Printf("WARNING: listening on %s without api_key_env — anyone who can reach this port can use your models", cfg.Listen)
		}
	}
	if host, _, err := net.SplitHostPort(cfg.Listen); err == nil && !cfg.HubRequireKey {
		if ip := net.ParseIP(host); host == "" || (ip != nil && !ip.IsLoopback()) {
			log.Printf("NOTE: hub_require_key is off: any machine that can reach %s may join this hub as a node (its models then receive requests). Set hub_require_key: true once every node has a hub key (fleet key set)", cfg.Listen)
		}
	}
	// Header/idle timeouts only: a body or a streamed answer may legitimately take long (no Read/WriteTimeout).
	srv := &http.Server{Addr: cfg.Listen, Handler: s.Handler(), ReadHeaderTimeout: 15 * time.Second, IdleTimeout: 2 * time.Minute}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		sc, c := context.WithTimeout(context.Background(), 3*time.Second)
		defer c()
		srv.Shutdown(sc) // lets running streams finish (up to 3 s) so their figures are counted
	}()
	err := srv.ListenAndServe()
	if err == http.ErrServerClosed {
		<-stopped
		err = nil
		done := make(chan struct{})
		go func() { loops.Wait(); close(done) }()
		select { // a loop stuck on a slow machine does not hold the shutdown up
		case <-done:
		case <-time.After(10 * time.Second):
			log.Printf("stopping without waiting for the background loops any longer")
		}
	}
	if e := s.live.save(statsFile); e != nil {
		log.Printf("could not save statistics: %v", e)
	}
	return err
}

// ---- health ----

// chatProbeEvery bounds the one-token chat probes of an endpoint (a var: tests shorten it).
var chatProbeEvery = 2 * time.Minute

// healthRoundWait bounds how long a round waits for its probes: an unreachable group costs up to 45 s (chat probe) plus
// its engine calls, and every other endpoint's health went stale meanwhile. A probe still running after this finishes
// on its own; its endpoint is skipped until it does.
var healthRoundWait = 12 * time.Second

// Health schedule (vars: tests shorten them). An endpoint is not polled every round: a healthy one is checked every
// healthEvery (config health_every_s), and not at all while real requests show it answering; a failing one is retried
// after healthFailFirst, doubling up to healthFailMax, so a dead or switched-off server costs almost nothing. A changed
// or new endpoint (a deploy, a run started) is checked at once.
var (
	healthTick      = 10 * time.Second
	healthEvery     = time.Minute
	healthFailFirst = 10 * time.Second
	healthFailMax   = 5 * time.Minute
)

type healthSched struct {
	next  time.Time
	fails int
	sig   string // what the endpoint points at: a change makes it due now
}

func healthSig(e config.Endpoint) string {
	return fmt.Sprintf("%s|%s|%s|%s|%s|%t", e.URL, e.SSH, e.Remote, e.Model, e.Probe, e.Group != nil)
}

func (s *Server) healthEveryFor(cfg *config.Config) time.Duration {
	if cfg.HealthEverySec > 0 {
		return time.Duration(cfg.HealthEverySec) * time.Second
	}
	return healthEvery
}

// healthNext: when to check an endpoint again after a probe left it in st.
func healthNext(now time.Time, st *EpState, sc *healthSched, every time.Duration) {
	if st != nil && st.OK {
		sc.fails = 0
		sc.next = now.Add(every)
		return
	}
	sc.fails++
	d := healthFailFirst
	for i := 1; i < sc.fails && d < healthFailMax; i++ {
		d *= 2
	}
	if d > healthFailMax {
		d = healthFailMax
	}
	sc.next = now.Add(d)
}

func (s *Server) healthLoop(ctx context.Context) {
	lastChat := map[string]time.Time{}
	var mu sync.Mutex
	inflight := map[string]bool{}
	sched := map[string]*healthSched{}
	var lastRuntimes time.Time
	t := time.NewTicker(healthTick)
	defer t.Stop()
	for {
		cfg := s.Config()
		every := s.healthEveryFor(cfg)
		now := time.Now()
		var wg sync.WaitGroup
		rctx := withModelsRound(ctx)
		live := map[string]bool{}
		for i := range cfg.Endpoints {
			e := cfg.Endpoints[i]
			if e.Peer != "" { // peer models are judged from the peer's own report (peers.go)
				continue
			}
			live[e.Name] = true
			mu.Lock()
			sc := sched[e.Name]
			if sc == nil || sc.sig != healthSig(e) {
				sc = &healthSched{sig: healthSig(e)} // new or re-pointed: due now
				sched[e.Name] = sc
			}
			due := !now.Before(sc.next)
			busy := inflight[e.Name]
			mu.Unlock()
			if !due || busy {
				continue
			}
			// a real request it answered within the interval says as much as a probe, without the traffic
			if st := s.getState(e.Name); st != nil && st.OK && now.Sub(s.live.lastOK(e.Name)) < every {
				mu.Lock()
				sc.next = s.live.lastOK(e.Name).Add(every)
				mu.Unlock()
				continue
			}
			// a chat probe costs a request on the model (for a ring group a whole step through every worker, and llama-server
			// keeps state for it): at most one per chatProbeEvery; between them a group gets the cheap /models check
			chatty := e.Probe == "chat" || (e.Probe == "" && e.Group != nil)
			if chatty && time.Since(lastChat[e.Name]) < chatProbeEvery {
				if st := s.getState(e.Name); st != nil {
					if e.Group == nil || e.Probe == "chat" {
						mu.Lock()
						sc.next = lastChat[e.Name].Add(chatProbeEvery)
						mu.Unlock()
						continue
					}
					e.Probe = "models" // this round: /models only (e is the loop's own copy)
				}
			}
			if chatty && e.Probe != "models" {
				lastChat[e.Name] = now
			}
			mu.Lock()
			inflight[e.Name] = true
			mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				s.probe(rctx, &e)
				mu.Lock()
				healthNext(time.Now(), s.getState(e.Name), sc, every)
				delete(inflight, e.Name)
				mu.Unlock()
			}()
		}
		mu.Lock()
		for name := range sched { // removed endpoints
			if !live[name] && !inflight[name] {
				delete(sched, name)
			}
		}
		mu.Unlock()
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		wait := time.NewTimer(healthRoundWait)
		select {
		case <-done:
		case <-wait.C:
		case <-ctx.Done():
			wait.Stop()
			return
		}
		wait.Stop()
		s.pings.record(cfg, s.getState)
		if time.Since(lastRuntimes) >= every/2 { // loaded models (/api/ps): routing prefers a warm one; no need every tick
			s.pollRuntimes(ctx, cfg)
			lastRuntimes = time.Now()
		}
		s.learnDims(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.kick:
		}
	}
}

func (s *Server) getState(name string) *EpState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if st := s.state[name]; st != nil {
		c := *st
		return &c
	}
	return nil
}

func (s *Server) probe(ctx context.Context, e *config.Endpoint) {
	if e.URL == "" && s.tunnels.IsIdle(e.SSH) {
		return // the tunnel is closed on purpose: keep the last known state, do not wake it for a health check
	}
	st := &EpState{Checked: time.Now()}
	ctx = context.WithValue(ctx, connErrKey{}, &st.noConn)
	base := s.BaseURL(e)
	switch {
	case strings.HasPrefix(base, "tunnel://"):
		// a node joined to this hub: it cannot be dialed (the scheme is not http), so it is up while its tunnel is
		// connected and it still announces the model; a forward that then fails is penalised like any upstream
		st.OK, st.Err = s.tunnelServes(strings.TrimPrefix(base, "tunnel://"), e.Model)
	case base == "":
		st.Err = "tunnel down"
		if m := s.tunnels.Err(e.SSH, e.Remote); m != "" {
			st.Err = m
		}
	// A group's master answers /models and /health while a worker is dead or stuck (found with the real engine: kill
	// the worker, the master stays "ok"), so only a token tells: groups are checked with a one-token completion.
	case (e.Probe == "chat" || (e.Probe == "" && e.Group != nil)) && !e.IsEmbed():
		st.OK, st.Latency, st.Err = s.call(ctx, e, base+"/chat/completions",
			map[string]any{"model": e.Wire(), "messages": []map[string]string{{"role": "user", "content": "hi"}}, "max_tokens": 1,
				"cache_prompt": false}, 45*time.Second)
	default:
		st.OK, st.Latency, st.Err = s.call(ctx, e, base+"/models", nil, 5*time.Second)
	}
	s.watchGroup(ctx, e, base, st)
	if e.Group != nil && st.noConn {
		// nothing listens: a distributed run that is not started. Runs are started on demand, so this is not a failure
		// to report as "down"; the schedule backs off and a deploy (which changes the endpoint) brings it back at once.
		st.OnDemand = true
	}
	s.mu.Lock()
	if old := s.state[e.Name]; old != nil {
		if time.Now().Before(old.penalty) {
			st.penalty, st.Why = old.penalty, old.Why
		}
	}

	s.state[e.Name] = st
	s.mu.Unlock()
}

// modelsRound shares GET /models answers inside one health round: many endpoints are usually registered on the same
// server (one Ollama serves every local model), and asking it once per endpoint every round multiplied the polling
// (Ollama re-reads every model's metadata for each /v1/models — 180k requests a day were seen on one laptop).
type modelsRound struct {
	mu sync.Mutex
	m  map[string]*modelsFetch
}

type modelsFetch struct {
	once   sync.Once
	status int
	body   []byte
	ms     int64
	err    string
}

type modelsRoundKey struct{}

// connErrKey: a *bool in the context that call sets when no server answered at all.
type connErrKey struct{}

func withModelsRound(ctx context.Context) context.Context {
	return context.WithValue(ctx, modelsRoundKey{}, &modelsRound{m: map[string]*modelsFetch{}})
}

func (s *Server) call(ctx context.Context, e *config.Endpoint, url string, body any, to time.Duration) (bool, int64, string) {
	var status int
	var b []byte
	var ms int64
	var errS string
	if r, _ := ctx.Value(modelsRoundKey{}).(*modelsRound); r != nil && body == nil {
		key := url + "\x00" + e.APIKey()
		r.mu.Lock()
		f := r.m[key]
		if f == nil {
			f = &modelsFetch{}
			r.m[key] = f
		}
		r.mu.Unlock()
		f.once.Do(func() { f.status, f.body, f.ms, f.err = s.fetch(ctx, e, url, nil, to) })
		status, b, ms, errS = f.status, f.body, f.ms, f.err
	} else {
		status, b, ms, errS = s.fetch(ctx, e, url, body, to)
	}
	if errS != "" {
		if ce, ok := ctx.Value(connErrKey{}).(*bool); ok {
			*ce = true
		}
		return false, ms, errS
	}
	if status != 200 {
		return false, ms, fmt.Sprintf("http %d", status)
	}
	if body == nil && e.Model != "" && e.Probe != "any" {
		var d struct {
			Data []struct {
				ID string `json:"id"`
				Up *bool  `json:"up"` // fleets say whether the model is up
			} `json:"data"`
		}
		if json.Unmarshal(b, &d) == nil && len(d.Data) > 0 {
			for _, m := range d.Data {
				// llama-server started with -m /dir/file.gguf lists the whole path as the model id while the endpoint
				// (a deploy, run-on) is registered under the file name: the same model.
				if m.ID == e.Wire() || (e.Peer == "" && strings.Contains(m.ID, "/") && path.Base(m.ID) == e.Model) {
					if m.Up != nil && !*m.Up {
						return false, ms, "down on the peer"
					}
					return true, ms, ""
				}
			}
			// A distributed engine (chain/ring group, or a lone chain stage tagged "distributed") serves exactly one model, and a chain stage lists the name from
			// the GGUF's own metadata (a fine-tune of Qwen3.5 reports "Qwen3.5-…" for Ornith-….gguf), not the file
			// name the endpoint is registered under: the only model it lists is the one it was started with.
			if (e.Group != nil || slices.Contains(e.Tags, "distributed")) && e.Peer == "" && len(d.Data) == 1 {
				if m := d.Data[0]; m.Up != nil && !*m.Up {
					return false, ms, "down on the peer"
				}
				return true, ms, ""
			}
			return false, ms, "model not listed"
		}
	}
	return true, ms, ""
}

// fetch does one request; err is "" on a transport success (any status).
func (s *Server) fetch(ctx context.Context, e *config.Endpoint, url string, body any, to time.Duration) (int, []byte, int64, string) {
	ctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()
	var rd io.Reader
	method := http.MethodGet
	if body != nil {
		b, _ := json.Marshal(body)
		rd, method = strings.NewReader(string(b)), http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return 0, nil, 0, err.Error()
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if k := e.APIKey(); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}
	t0 := time.Now()
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, nil, 0, short(err.Error())
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, b, time.Since(t0).Milliseconds(), ""
}

func short(s string) string {
	if len(s) > 90 {
		return s[:90]
	}
	return s
}

// healthy: known to be healthy (no state yet → don't be suspicious: true).
func (s *Server) healthy(name string) bool {
	st := s.getState(name)
	if c := s.Config(); c.Drain { // this machine has left the line: its own models come last
		if e := c.Endpoint(name); e != nil && e.Peer == "" && s.isSelfNode(e.Node) {
			return false
		}
	}
	return st == nil || (st.OK && time.Now().After(st.penalty))
}

// stallPenalty: how long a stalled endpoint stays at the back of the line (a health check may not clear it:
// a master whose worker link is stuck still answers /models).
const stallPenalty = 2 * time.Minute

func (s *Server) penalizeFor(name string, d time.Duration, why string) {
	s.mu.Lock()
	st := s.state[name]
	if st == nil {
		st = &EpState{OK: true}
		s.state[name] = st
	}
	st.penalty, st.Why = time.Now().Add(d), why
	s.mu.Unlock()
	s.runNote(name, why)
}

func (s *Server) penalize(name string) {
	s.mu.Lock()
	if st := s.state[name]; st != nil {
		st.penalty = time.Now().Add(20 * time.Second)
	} else {
		s.state[name] = &EpState{OK: true, penalty: time.Now().Add(20 * time.Second)}
	}
	s.mu.Unlock()
}

// learnDims measures the vector size of embedding endpoints that do not have one yet and persists it
// (roles then refuse to mix sizes). Runs sequentially after each health round.
func (s *Server) learnDims(ctx context.Context) {
	cfg := s.Config()
	learned := map[string]int{}
	for i := range cfg.Endpoints {
		e := &cfg.Endpoints[i]
		if !e.IsEmbed() || e.Dim > 0 || !s.healthy(e.Name) {
			continue
		}
		base := s.BaseURL(e)
		if base == "" {
			continue
		}
		cx, cancel := context.WithTimeout(ctx, 60*time.Second)
		raw, _ := json.Marshal(map[string]any{"model": e.Wire(), "input": "dimension probe"})
		req, _ := http.NewRequestWithContext(cx, "POST", base+"/embeddings", strings.NewReader(string(raw)))
		req.Header.Set("Content-Type", "application/json")
		if k := e.APIKey(); k != "" {
			req.Header.Set("Authorization", "Bearer "+k)
		}
		if resp, err := s.client.Do(req); err == nil {
			var d struct {
				Data []struct {
					Embedding []float64 `json:"embedding"`
				} `json:"data"`
			}
			if json.NewDecoder(resp.Body).Decode(&d) == nil && len(d.Data) > 0 && len(d.Data[0].Embedding) > 0 {
				learned[e.Name] = len(d.Data[0].Embedding)
			}
			resp.Body.Close()
		}
		cancel()
	}
	if len(learned) == 0 {
		return
	}
	err := s.UpdateConfig(func(c *config.Config) bool {
		changed := false
		for i := range c.Endpoints {
			if d, ok := learned[c.Endpoints[i].Name]; ok && c.Endpoints[i].Dim != d {
				c.Endpoints[i].Dim, changed = d, true
			}
		}
		return changed
	})
	if err != nil { // rejected if it would expose an existing dimension mix
		log.Printf("embedding dimensions not saved: %v", err)
	}
}

func (s *Server) onTunnelJoin(reg tunnel.NodeRegistration) {
	log.Printf("tunnel: node %q joined with models: %v", reg.NodeName, reg.Models)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg != nil {
		for _, m := range reg.Models {
			epName := fmt.Sprintf("%s/%s", reg.NodeName, m)
			if s.cfg.Endpoint(epName) == nil {
				// a new slice: copies handed out by Config() share the backing array, and an in-place append would
				// write into a slot another copy is about to append to
				s.cfg.Endpoints = append(append([]config.Endpoint(nil), s.cfg.Endpoints...), config.Endpoint{
					Name:  epName,
					Model: m,
					Node:  reg.NodeName,
					URL:   fmt.Sprintf("tunnel://%s", reg.NodeName),
				})
			}
		}
	}
}

func (s *Server) onTunnelDrop(nodeName string) {
	log.Printf("tunnel: node %q dropped", nodeName)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg != nil {
		var kept []config.Endpoint
		for _, ep := range s.cfg.Endpoints {
			if ep.Node != nodeName || !strings.HasPrefix(ep.URL, "tunnel://") {
				kept = append(kept, ep)
			}
		}
		s.cfg.Endpoints = kept
	}
}

func isTunnelEndpoint(ep config.Endpoint) bool { return strings.HasPrefix(ep.URL, "tunnel://") }

func withoutTunnelEndpoints(in []config.Endpoint) []config.Endpoint {
	out := make([]config.Endpoint, 0, len(in))
	for _, ep := range in {
		if !isTunnelEndpoint(ep) {
			out = append(out, ep)
		}
	}
	return out
}

// tunnelServes: node's tunnel is connected to this hub and its registration lists model.
func (s *Server) tunnelServes(node, model string) (bool, string) {
	for _, reg := range s.hub.ConnectedNodes() {
		if reg.NodeName != node {
			continue
		}
		for _, m := range reg.Models {
			if m == model {
				return true, ""
			}
		}
		return false, "node " + node + " no longer announces " + model
	}
	return false, "node " + node + " is not connected to this hub"
}

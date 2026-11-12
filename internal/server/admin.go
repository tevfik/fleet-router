package server

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/discover"
	"github.com/tevfik/fleet-router/internal/feature"
	"github.com/tevfik/fleet-router/internal/tunnel"
)

type EpStatus struct {
	config.Endpoint
	OK        bool      `json:"ok"`
	Known     bool      `json:"known"`
	Checked   time.Time `json:"checked,omitempty"`
	LatencyMS int64     `json:"latency_ms"`
	Err       string    `json:"err,omitempty"`
	Base      string    `json:"base,omitempty"`
	Idle      bool      `json:"idle,omitempty"`      // its SSH tunnel is closed because nothing used it; the next request opens it
	OnDemand  bool      `json:"on_demand,omitempty"` // a distributed run that is not started (started on demand): not "down"
	InUse     int       `json:"in_use"`
	Waiting   int       `json:"waiting"`
	TTFTMS    int64     `json:"ttft_ms,omitempty"`
	BenchErr  string    `json:"bench_err,omitempty"`
	// Figures from real traffic (tps/ttft_ms above are replaced by them once available).
	TPSSrc     string       `json:"tps_src,omitempty"` // "live" | "bench" | ""
	PrefillTPS float64      `json:"prefill_tps,omitempty"`
	LastPrompt int          `json:"last_prompt,omitempty"`
	MaxPrompt  int          `json:"max_prompt,omitempty"`
	Samples    int          `json:"samples,omitempty"`
	Requests   int          `json:"requests,omitempty"` // since the daemon started
	Errors     int          `json:"errors,omitempty"`
	TokensIn   int          `json:"tokens_in,omitempty"`
	TokensOut  int          `json:"tokens_out,omitempty"`
	Probe      *ProbeResult `json:"probe,omitempty"`       // the last capability probe (fleet probe)
	Info       *ModelInfo   `json:"info,omitempty"`        // loaded or not, size — what its runtime says
	Strained   []string     `json:"strained,omitempty"`    // why its machine is struggling (routing prefers others)
	Paused     string       `json:"paused,omitempty"`      // set aside after a stall, while it lasts
	GroupLive  *GroupState  `json:"group_state,omitempty"` // an attached group: what its engine and members say now
}

type RoleStatus struct {
	config.Role
	Active  string `json:"active"` // endpoint that will receive requests right now
	InUse   int    `json:"in_use"`
	Waiting int    `json:"waiting"`
}

type Status struct {
	Version   string                `json:"version"` // the daemon's own fleet version
	Engine    string                `json:"engine,omitempty"`
	Listen    string                `json:"listen"`
	Endpoints []EpStatus            `json:"endpoints"`
	Roles     map[string]RoleStatus `json:"roles"`
	Bench     BenchStatus           `json:"bench"`
	Probe     ProbeStatus           `json:"probe"`
	Nodes     []NodeStatus          `json:"nodes"`
	Draining  bool                  `json:"draining,omitempty"` // this machine has left the line (fleet leave)
	// Runner says whether this daemon has the runner side (fleet-engine: plans, runs, model files); nil from an older one.
	Runner *bool `json:"runner,omitempty"`
}

func (s *Server) Status() Status {
	cfg := s.Config()
	runner := feature.Runner()
	st := Status{Version: Version, Engine: Engine, Listen: cfg.Listen, Roles: map[string]RoleStatus{}, Bench: s.BenchStatus(), Probe: s.ProbeStatus(), Draining: cfg.Drain, Runner: &runner}
	probes := s.ProbeResults()
	for _, e := range cfg.Endpoints {
		es := EpStatus{Endpoint: e, Base: s.BaseURL(&e), Idle: e.URL == "" && s.tunnels.IsIdle(e.SSH)}
		if x := s.getState(e.Name); x != nil {
			es.Known, es.OK, es.LatencyMS, es.Err, es.Checked = true, x.OK, x.Latency, x.Err, x.Checked
			es.OnDemand = x.OnDemand
			es.GroupLive = x.Group
			if time.Now().Before(x.penalty) {
				es.Paused = x.Why
			}
		}
		es.InUse, es.Waiting, _ = s.lim("ep:" + e.Name).stats()
		if r, ok := probes[e.Name]; ok {
			es.Probe = &r
		}
		if mi, ok := s.warm.get(e.Name); ok {
			es.Info = &mi
		}
		if ns, ok := s.NodeStrainOf(e.Node); ok {
			es.Strained = ns.Reasons
		}
		if br, ok := s.benchResult(e.Name); ok {
			es.TTFTMS, es.BenchErr = br.TTFT.Milliseconds(), br.Err
		}
		if es.TPS > 0 {
			es.TPSSrc = "bench"
		}
		if lv, ok := s.live.get(e.Name); ok {
			es.PrefillTPS, es.LastPrompt, es.MaxPrompt, es.Samples = lv.PrefillTPS, lv.LastPrompt, lv.MaxPrompt, lv.Samples
			es.Requests, es.Errors, es.TokensIn, es.TokensOut = lv.Requests, lv.Errors, lv.TokensIn, lv.TokensOut
			if lv.TTFTMS > 0 {
				es.TTFTMS = int64(lv.TTFTMS)
			}
			if lv.Samples > 0 {
				es.TPS, es.TPSSrc = float64(int(lv.TPS*10+0.5))/10, "live"
			}
		}
		st.Endpoints = append(st.Endpoints, es)
	}
	for _, r := range cfg.RoleNames() {
		rs := RoleStatus{Role: cfg.Roles[r]}
		rs.InUse, rs.Waiting, _ = s.lim("role:" + r).stats()
		if c := s.Resolve(r); len(c) > 0 {
			rs.Active = c[0].Name
		}
		st.Roles[r] = rs
	}
	st.Nodes = s.Nodes(st.Endpoints)
	return st
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/", s.v1Auth(s.proxyV1))
	mux.HandleFunc("GET /v1/models", s.v1Auth(s.modelsList))
	mux.HandleFunc("GET /fleet/hello", s.hello)
	mux.HandleFunc("GET /fleet/export", s.v1Auth(s.export))
	mux.HandleFunc("GET /fleet/host", s.v1Auth(s.hostHandler))
	mux.HandleFunc("GET /fleet/strain", s.v1Auth(s.strainHandler))
	mux.HandleFunc("POST /fleet/pair", s.pairHandler) // open: the code is the proof
	s.ollamaRoutes(mux)
	mux.HandleFunc("GET /metrics", s.metrics)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	adm := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !s.adminAllowed(r) {
				httpErr(w, 403, "the admin API is loopback-only unless admin_token is set")
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("GET /fleet/route", adm(s.routeHandler))
	if feature.Runner() { // the runner side (fleet-engine): plans, model files, profiles, runs; absent = just the router
		mux.HandleFunc("POST /fleet/plan", adm(s.planHandler))
		mux.HandleFunc("POST /fleet/plan/evaluate", adm(s.planEvaluateHandler))
		s.storeHandlers(mux, adm)
		s.profileHandlers(mux, adm)
		s.nodeSpecHandler(mux, adm)
		s.runHandlers(mux, adm)
		s.stopHandlers(mux, adm)
	}
	mux.HandleFunc("GET /fleet/status", adm(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, s.Status()) }))
	mux.HandleFunc("GET /fleet/log", adm(func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.URL.Query().Get("n"))
		writeJSON(w, map[string]any{"entries": s.reqs.last(n)})
	}))
	mux.HandleFunc("GET /fleet/usage", adm(func(w http.ResponseWriter, r *http.Request) {
		since := ParseSince(r.URL.Query().Get("since"))
		by, split := r.URL.Query().Get("by"), r.URL.Query().Get("split")
		exclude := r.URL.Query().Get("exclude_client")
		writeJSON(w, map[string]any{"by": by, "since_s": int(since.Seconds()), "rows": s.UsageFiltered(by, split, since, exclude)})
	}))
	// under /fleet/tunnel/ on purpose: the hub's reverse proxy publishes only /v1, /api, /fleet/tunnel and /healthz (anything else gets the SSO login)
	mux.HandleFunc("GET /fleet/tunnel/usage", hubUsageHandler(s))
	mux.HandleFunc("GET /fleet/apikeys", adm(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"keys": GetKeyStore().List()})
	}))
	mux.HandleFunc("POST /fleet/apikeys", adm(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name   string   `json:"name"`
			QoS    string   `json:"qos"`
			Role   string   `json:"role"`
			Models []string `json:"models"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			httpErr(w, 400, err.Error())
			return
		}
		k, err := GetKeyStore().Create(in.Name, in.QoS, in.Role, in.Models...)
		if err != nil {
			httpErr(w, 500, err.Error())
			return
		}
		writeJSON(w, k)
	}))
	mux.HandleFunc("DELETE /fleet/apikeys/{key}", adm(func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("key")
		if err := GetKeyStore().Delete(key); err != nil {
			httpErr(w, 500, err.Error())
			return
		}
		writeJSON(w, map[string]any{"status": "deleted"})
	}))
	mux.HandleFunc("GET /fleet/peers", adm(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"peers": s.PeerStatuses(), "lan": s.LANFleets()})
	}))
	mux.HandleFunc("POST /fleet/peers", adm(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Add    *config.Peer `json:"add"`
			Pair   string       `json:"pair"` // pairing code shown by the peer (fleet peers pair there)
			Remove string       `json:"remove"`
			Sync   bool         `json:"sync"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			httpErr(w, 400, err.Error())
			return
		}
		var err error
		switch {
		case in.Add != nil && in.Pair != "":
			if in.Add.URL == "" {
				err = fmt.Errorf("pairing needs the peer's address (http://<ip>:4711/v1); over SSH no key is needed")
				break
			}
			var key string
			if key, err = s.pairWith(r.Context(), in.Add.URL, in.Pair); err == nil {
				in.Add.Key = key
				err = s.AddPeer(*in.Add)
			}
		case in.Add != nil:
			err = s.AddPeer(*in.Add)
		case in.Remove != "":
			err = s.RemovePeer(in.Remove)
		case in.Sync:
			s.SyncPeers(r.Context())
		}
		if err != nil {
			httpErr(w, 400, err.Error())
			return
		}
		writeJSON(w, map[string]any{"peers": s.PeerStatuses()})
	}))
	mux.HandleFunc("GET /fleet/config", adm(func(w http.ResponseWriter, r *http.Request) {
		c := s.Config()
		if !directLoopback(r) { // the menu on this machine needs the hub secrets to show/edit them; nobody else does
			c.HubToken, c.HubAPIKey = redactSecret(c.HubToken), redactSecret(c.HubAPIKey)
		}
		writeJSON(w, c)
	}))
	mux.HandleFunc("POST /fleet/reload", adm(func(w http.ResponseWriter, _ *http.Request) {
		if err := s.Reload(); err != nil {
			httpErr(w, 400, err.Error())
			return
		}
		writeJSON(w, map[string]string{"ok": "reloaded"})
	}))
	mux.HandleFunc("POST /fleet/undo", adm(func(w http.ResponseWriter, _ *http.Request) {
		name, err := s.Undo()
		if err != nil {
			httpErr(w, 400, err.Error())
			return
		}
		writeJSON(w, map[string]any{"restored": name, "left": len(config.Snapshots(s.path))})
	}))
	mux.HandleFunc("GET /fleet/history", adm(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"snapshots": config.Snapshots(s.path)})
	}))
	// role assignment: applied hot — sessions stay open, no reload needed
	mux.HandleFunc("POST /fleet/role", adm(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Role        string    `json:"role"`
			Preferred   *string   `json:"preferred"`
			Fallback    *[]string `json:"fallback"`
			Match       *[]string `json:"match"`
			Priority    *int      `json:"priority"`
			MaxParallel *int      `json:"max_parallel"`
			Overflow    *bool     `json:"overflow"`
			Strategy    *string   `json:"strategy"`
			Use         *string   `json:"use"`
			Delete      bool      `json:"delete"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Role == "" {
			httpErr(w, 400, "role is required")
			return
		}
		cfg := s.Config()
		roles := map[string]config.Role{}
		for k, v := range cfg.Roles {
			roles[k] = v
		}
		if in.Delete {
			delete(roles, in.Role)
		} else {
			cur := roles[in.Role] // fields not provided are preserved
			if in.Preferred != nil {
				cur.Preferred = *in.Preferred
			}
			if in.Fallback != nil {
				cur.Fallback = *in.Fallback
			}
			if in.Match != nil {
				cur.Match = *in.Match
			}
			if in.Priority != nil {
				cur.Priority = *in.Priority
			}
			if in.MaxParallel != nil {
				cur.MaxParallel = max(0, *in.MaxParallel)
			}
			if in.Overflow != nil {
				cur.Overflow = *in.Overflow
			}
			if in.Use != nil {
				cur.Use = *in.Use
			}
			if in.Strategy != nil {
				cur.Strategy = *in.Strategy
			}
			roles[in.Role] = cur
		}
		cfg.Roles = roles
		if err := s.SetConfig(cfg); err != nil {
			httpErr(w, 400, err.Error())
			return
		}
		writeJSON(w, s.Status().Roles[in.Role])
	}))
	// in-place endpoint setting: slots
	mux.HandleFunc("POST /fleet/endpoint-set", adm(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name  string    `json:"name"`
			Slots *int      `json:"slots"`
			Caps  *[]string `json:"caps"`
			Tags  *[]string `json:"tags"`
			// Group: an empty engine removes the group; Stall "" leaves the limit as it is.
			Group     *config.Group `json:"group"`
			FleetPort *int          `json:"fleet_port"`
			Stall     *string       `json:"stall"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Name == "" {
			httpErr(w, 400, "name is required")
			return
		}
		cfg := s.Config()
		eps := append([]config.Endpoint{}, cfg.Endpoints...)
		found := false
		for i := range eps {
			if eps[i].Name == in.Name {
				found = true
				if in.Slots != nil {
					eps[i].Slots = max(0, *in.Slots)
				}
				if in.Tags != nil {
					eps[i].Tags = *in.Tags
				}
				if in.Group != nil {
					if in.Group.Engine == "" {
						eps[i].Group = nil
					} else {
						g := *in.Group
						eps[i].Group = &g
					}
				}
				if in.FleetPort != nil {
					eps[i].FleetPort = max(0, *in.FleetPort)
				}
				if in.Stall != nil {
					eps[i].Stall = *in.Stall
				}
				if in.Caps != nil { // empty list = go back to automatic detection
					eps[i].Caps = *in.Caps
				}
			}
		}
		if !found {
			httpErr(w, 404, "no such endpoint: "+in.Name)
			return
		}
		cfg.Endpoints = eps
		if err := s.SetConfig(cfg); err != nil {
			httpErr(w, 400, err.Error())
			return
		}
		writeJSON(w, map[string]string{"ok": in.Name})
	}))
	mux.HandleFunc("POST /fleet/endpoints", adm(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Add    []config.Endpoint `json:"add"`
			Remove []string          `json:"remove"`
			// MendRoles: also take the removed endpoints out of the roles that name them (without it such a removal is
			// refused, naming the roles)
			MendRoles bool `json:"mend_roles"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			httpErr(w, 400, err.Error())
			return
		}
		cfg := s.Config()
		readded := map[string]bool{}
		for _, e := range in.Add {
			readded[e.Name] = true
		}
		var drop []string
		for _, n := range in.Remove {
			if !readded[n] {
				drop = append(drop, n)
			}
		}
		if used := rolesUsing(cfg, drop); len(used) > 0 && !in.MendRoles {
			httpErr(w, 400, fmt.Sprintf("%s is named by role(s) %s: remove it from them first (fleet role set / rm), or have the roles mended with --mend-roles (the preferred endpoint becomes the first fallback; a role left with nothing is deleted; fleet config undo restores)", strings.Join(drop, ", "), strings.Join(used, ", ")))
			return
		}
		// roles that named a removed endpoint are mended (a replacement keeps them as they are)
		notes := cfg.DropEndpoints(drop...)
		eps := cfg.Endpoints
		for _, n := range in.Remove {
			if readded[n] {
				var keep []config.Endpoint
				for _, e := range eps {
					if e.Name != n {
						keep = append(keep, e)
					}
				}
				eps = keep
			}
		}
		cfg.Endpoints = append(eps, in.Add...)
		if err := s.SetConfig(cfg); err != nil {
			httpErr(w, 400, err.Error())
			return
		}
		writeJSON(w, map[string]any{"endpoints": len(cfg.Endpoints), "roles": notes})
	}))
	// discovery runs in the daemon (it knows its own tunnels and its own machine name)
	mux.HandleFunc("POST /fleet/discover", adm(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Ports        string   `json:"ports"`
			SSH          []string `json:"ssh"`
			IncludeEmbed bool     `json:"include_embed"`
			Apply        bool     `json:"apply"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		cfg := s.Config()
		if in.Ports == "" {
			in.Ports = cfg.Discover.Ports
		}
		if in.SSH == nil {
			in.SSH = cfg.Discover.SSH
		}
		for _, h := range in.SSH {
			if err := config.CheckSSHTarget(h); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		discover.SetSelf(cfg.Name)
		found, notes := discover.Scan(r.Context(), discover.ParsePorts(in.Ports), in.SSH, s.tunnels.LocalPorts())
		ign := map[string]bool{}
		for _, x := range cfg.Discover.Ignore {
			ign[x] = true
		}
		var kept []discover.Found
		for _, f := range found {
			if ign[fmt.Sprintf("%s:%d", f.Node, f.Port)] {
				notes = append(notes, fmt.Sprintf("- %s:%d: on the ignore list, skipped", f.Node, f.Port))
				continue
			}
			kept = append(kept, f)
		}
		found = kept
		plan := discover.MakePlan(cfg, found, in.IncludeEmbed)
		if in.Apply && (len(plan.Add) > 0 || len(plan.Peers) > 0) {
			cfg.Endpoints = append(append([]config.Endpoint{}, cfg.Endpoints...), plan.Add...)
			cfg.Peers = append(append([]config.Peer{}, cfg.Peers...), plan.Peers...)
			if err := s.SetConfig(cfg); err != nil {
				httpErr(w, 400, err.Error())
				return
			}
			if len(plan.Peers) > 0 {
				select {
				case s.peerKick <- struct{}{}:
				default:
				}
			}
		}
		writeJSON(w, map[string]any{"found": found, "notes": notes, "plan": plan, "applied": in.Apply})
	}))
	mux.HandleFunc("POST /fleet/settings", adm(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Ports  string   `json:"ports"`
			SSH    []string `json:"ssh"`
			Ignore []string `json:"ignore"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			httpErr(w, 400, err.Error())
			return
		}
		cfg := s.Config()
		cfg.Discover = config.Discover{Ports: in.Ports, SSH: in.SSH, Ignore: in.Ignore}
		if err := s.SetConfig(cfg); err != nil {
			httpErr(w, 400, err.Error())
			return
		}
		writeJSON(w, cfg.Discover)
	}))
	mux.HandleFunc("POST /fleet/economy/settings", adm(func(w http.ResponseWriter, r *http.Request) {
		var in config.EconomyConfig
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			httpErr(w, 400, err.Error())
			return
		}
		cfg := s.Config()
		cfg.Economy = &in
		if err := s.SetConfig(cfg); err != nil {
			httpErr(w, 400, err.Error())
			return
		}
		writeJSON(w, cfg.Economy)
	}))
	mux.HandleFunc("POST /fleet/bench", adm(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Names []string `json:"names"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		if err := s.Bench(in.Names); err != nil {
			httpErr(w, 409, err.Error())
			return
		}
		writeJSON(w, s.BenchStatus())
	}))
	mux.HandleFunc("POST /fleet/pair/code", adm(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, s.NewPairCode()) }))
	mux.HandleFunc("POST /fleet/host/bench", adm(func(w http.ResponseWriter, _ *http.Request) {
		if err := s.RunHWBench("asked"); err != nil {
			httpErr(w, 409, err.Error())
			return
		}
		writeJSON(w, map[string]bool{"running": true})
	}))
	mux.HandleFunc("POST /fleet/probe", adm(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Names []string `json:"names"`
			Apply bool     `json:"apply"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		if in.Apply {
			ch, err := s.ApplyProbe(in.Names)
			if err != nil {
				httpErr(w, 400, err.Error())
				return
			}
			writeJSON(w, map[string]any{"changes": ch})
			return
		}
		if err := s.Probe(in.Names); err != nil {
			httpErr(w, 409, err.Error())
			return
		}
		writeJSON(w, s.ProbeStatus())
	}))
	mux.HandleFunc("GET /fleet/opencode", adm(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, s.OpenCodeProvider())
	}))
	if s.hub != nil {
		mux.Handle("GET /fleet/tunnel/connect", s.hub.Handler())
		mux.Handle("/fleet/tunnel/connect", s.hub.Handler())
		mux.Handle("GET "+tunnel.StreamDialPath, s.hub.StreamDialHandler())
		mux.Handle("GET "+tunnel.StreamAcceptPath, s.hub.StreamAcceptHandler())
	}
	mux.HandleFunc("POST /fleet/tunnel/join", adm(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			HubURL      string `json:"hub_url"`
			Token       string `json:"token"`
			StreamPorts []int  `json:"stream_ports"`
			// AddStreamPorts are added to the ports the node already allows (stream_ports, else hub_stream_ports)
			// instead of replacing them; without a token the remembered hub_token of the same hub is kept.
			AddStreamPorts []int `json:"add_stream_ports"`
			// Insecure skips verification of the hub's TLS certificate (self-signed hub); nil keeps the saved choice.
			Insecure *bool `json:"insecure"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.HubURL == "" {
			httpErr(w, 400, "hub_url is required")
			return
		}
		cur := s.Config()
		streamPorts := in.StreamPorts
		if len(streamPorts) == 0 {
			streamPorts = cur.HubStreamPorts
		}
		if len(in.AddStreamPorts) > 0 {
			streamPorts = mergePorts(streamPorts, in.AddStreamPorts)
			if in.Token == "" && in.HubURL == cur.HubURL {
				in.Token = cur.HubToken
			}
		}
		for _, p := range streamPorts {
			if p <= 0 || p > 65535 {
				httpErr(w, 400, fmt.Sprintf("stream port %d is out of range", p))
				return
			}
			if _, lp, err := net.SplitHostPort(cur.Listen); err == nil && lp == strconv.Itoa(p) {
				httpErr(w, 400, fmt.Sprintf("stream port %d is this fleet daemon: a raw stream to it would bypass its admin check", p))
				return
			}
		}
		insecure := joinInsecure(cur, in.HubURL, in.Insecure)
		if err := s.joinHub(s.bumpJoinGen(), in.HubURL, in.Token, streamPorts, insecure); err != nil {
			httpErr(w, 500, err.Error())
			return
		}
		saved := true
		if err := s.rememberJoin(in.HubURL, in.Token, streamPorts, insecure); err != nil {
			log.Printf("hub join: connected, but could not save it for the next start: %v", err)
			saved = false
		}
		writeJSON(w, map[string]any{"status": "connected", "hub_url": in.HubURL, "auto_join": saved})
	}))
	mux.HandleFunc("POST /fleet/tunnel/leave", adm(func(w http.ResponseWriter, _ *http.Request) {
		s.leaveHub()
		if err := s.forgetJoin(); err != nil {
			log.Printf("hub leave: could not clear hub_auto_join: %v", err)
		}
		writeJSON(w, map[string]any{"status": "disconnected"})
	}))
	mux.HandleFunc("GET /fleet/tunnel/status", adm(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, s.hubStatus())
	}))
	mux.HandleFunc("POST /fleet/hub/settings", adm(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			HubURL   string `json:"hub_url"`
			HubToken string `json:"hub_token"`
			// HubAPIKey is the hub API key; nil leaves it unchanged. It is independent of HubToken.
			HubAPIKey *string `json:"hub_api_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			httpErr(w, 400, err.Error())
			return
		}
		cfg := s.Config()
		if in.HubAPIKey != nil {
			// API-key-only update: leave URL and join token untouched.
			cfg.HubAPIKey = strings.TrimSpace(*in.HubAPIKey)
			if in.HubURL == "" && in.HubToken == "" {
				if err := s.SetConfig(cfg); err != nil {
					httpErr(w, 500, err.Error())
					return
				}
				writeJSON(w, map[string]any{"status": "ok", "hub_api_key_set": cfg.HubAPIKey != ""})
				return
			}
		}
		cfg.HubURL = strings.TrimSpace(in.HubURL)
		cfg.HubToken = strings.TrimSpace(in.HubToken)
		if err := s.SetConfig(cfg); err != nil {
			httpErr(w, 500, err.Error())
			return
		}
		writeJSON(w, map[string]any{"status": "ok", "hub_url": cfg.HubURL, "hub_token_set": cfg.HubToken != ""})
	}))
	return mux
}

// v1Auth guards the model API for clients that are not on this machine: when api_key_env is set they
// must present that key. Loopback callers (OpenCode, Claude's MCP, the menu) never need one.
func (s *Server) v1Auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if env := s.Config().APIKeyEnv; env != "" && !directLoopback(r) { // a reverse proxy on this host shows up as loopback: forwarded callers are not local
			want := config.Secret(env)
			got := bearer(r)
			authOK := false
			if want != "" && got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1 {
				authOK = true
			} else if s.pairedKey(got) {
				authOK = true
			} else if _, ok := GetKeyStore().Validate(got); ok {
				authOK = true
			}
			if !authOK {
				w.Header().Set("WWW-Authenticate", "Bearer")
				httpErr(w, 401, "missing or wrong API key (Authorization: Bearer …)")
				// refused callers belong in `fleet log` too: who tried to use the models is the question it answers
				s.reqs.add(LogEntry{Time: time.Now(), Path: r.URL.Path, Remote: !isLoopback(r), Client: clientLabel(r),
					Status: 401, Err: "missing or wrong API key"})
				return
			}
		}
		h(w, r)
	}
}

// streamAuth guards raw TCP streams through this hub (tunnel.StreamDialPath). Unlike v1Auth it always
// wants a key, from loopback too and when api_key_env is unset: a stream is a TCP connection into a
// joined node, and a hub behind a reverse proxy on the same machine sees every caller as loopback.
// Accepted: the api_key_env key, a paired fleet's key, or an API key without role restrictions (or with
// the admin or "stream" role).
func (s *Server) streamAuth(r *http.Request) bool {
	got := bearer(r)
	if got == "" {
		return false
	}
	if env := s.Config().APIKeyEnv; env != "" {
		if want := config.Secret(env); want != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1 {
			return true
		}
	}
	if s.pairedKey(got) {
		return true
	}
	if ak, ok := GetKeyStore().Validate(got); ok && ak.AllowsRole("stream") {
		return true
	}
	return false
}

// hubUsageHandler returns the hub usage statistics (same as /fleet/usage) to a node that holds a key the hub accepts
// for its streams and its control tunnel (streamAuth: api_key_env, a paired key or an API key with the stream role): the
// node needs no admin rights, only the credential it already joined with. No key, or a wrong one, is 401 (loopback
// is not enough: a reverse proxy can look like loopback).
func hubUsageHandler(s *Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.streamAuth(r) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			httpErr(w, 401, "missing or wrong hub key")
			return
		}
		since := ParseSince(r.URL.Query().Get("since"))
		by, split := r.URL.Query().Get("by"), r.URL.Query().Get("split")
		exclude := r.URL.Query().Get("exclude_client")
		writeJSON(w, map[string]any{"by": by, "since_s": int(since.Seconds()), "rows": s.UsageFiltered(by, split, since, exclude)})
	}
}

// connectAuth guards the control tunnel (/fleet/tunnel/connect). It is open unless hub_require_key is set, so that nodes
// that predate keys keep joining; then it wants the same key as a stream (streamAuth). The join token is a different
// thing: it travels in the registration and only authorises the node's own stream connections back to the hub.
func (s *Server) connectAuth(r *http.Request) bool {
	if !s.Config().HubRequireKey {
		return true
	}
	return s.streamAuth(r)
}

// bearer returns the token of an "Authorization: Bearer <token>" header ("" when absent or another scheme).
func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// redactSecret keeps "is it set" visible without the value.
func redactSecret(v string) string {
	if v == "" {
		return ""
	}
	return "***"
}

// directLoopback: the caller is on this machine itself, not a reverse proxy forwarding for someone else.
func directLoopback(r *http.Request) bool {
	if !isLoopback(r) {
		return false
	}
	for _, h := range []string{"X-Forwarded-For", "Forwarded", "X-Real-Ip"} {
		if r.Header.Get(h) != "" {
			return false
		}
	}
	return true
}

func isLoopback(r *http.Request) bool {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) adminAllowed(r *http.Request) bool {
	if directLoopback(r) { // not a reverse proxy on this host forwarding an outside caller (X-Forwarded-For & co.)
		return true
	}
	tok := s.Config().AdminToken
	return tok != "" && subtle.ConstantTimeCompare([]byte(bearer(r)), []byte(tok)) == 1
}

// OpenCodeProvider is the single entry to add under "providers" in opencode.json — roles show up as models.
func (s *Server) OpenCodeProvider() map[string]any {
	cfg := s.Config()
	models := map[string]any{}
	for _, r := range cfg.RoleNames() {
		models[r] = map[string]any{"name": "fleet role: " + r}
	}
	for _, e := range cfg.Endpoints {
		if e.IsEmbed() {
			continue
		}
		m := map[string]any{"name": e.Node + " · " + e.Model}
		if e.Ctx > 0 {
			m["limit"] = map[string]any{"context": e.Ctx, "output": 8192}
		}
		models[e.Name] = m
	}
	return map[string]any{"fleet": map[string]any{
		"package":  "@opencode/ai/providers/openai-compatible",
		"name":     "Fleet (router)",
		"settings": map[string]any{"baseURL": "http://" + cfg.Listen + "/v1", "apiKey": "fleet-local"},
		"models":   models,
	}}
}

// rolesUsing lists the roles whose preferred endpoint or fallbacks name one of the endpoints.
func rolesUsing(cfg *config.Config, names []string) []string {
	in := map[string]bool{}
	for _, n := range names {
		in[n] = true
	}
	var out []string
	for _, r := range cfg.RoleNames() {
		role := cfg.Roles[r]
		hit := in[role.Preferred]
		for _, f := range role.Fallback {
			hit = hit || in[f]
		}
		if hit {
			out = append(out, r)
		}
	}
	return out
}

// joinInsecure: whether a join to hubURL skips TLS verification. The setting in the configuration was granted for the
// hub configured there: it is not carried to a different hub URL (the join token would go there unverified);
// an explicit choice in the request wins.
func joinInsecure(cur *config.Config, hubURL string, explicit *bool) bool {
	if explicit != nil {
		return *explicit
	}
	return cur.HubInsecure && strings.TrimRight(hubURL, "/") == strings.TrimRight(cur.HubURL, "/")
}

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/plan"
)

// A Run is a model that was planned over several machines and is (or was) running: where each block is, what
// the plan expected, and what the engine has reported since. The fleet that planned it keeps the record; every
// fleet sees it through the machine readings, so the whole line can be looked at from anywhere.
type Run struct {
	ID       string    `json:"id"`
	Fleet    string    `json:"fleet,omitempty"` // the fleet that planned it
	At       time.Time `json:"at"`
	Ended    time.Time `json:"ended,omitempty"`
	Model    string    `json:"model"`
	SHA256   string    `json:"sha256,omitempty"`
	Engine   string    `json:"engine,omitempty"`
	Endpoint string    `json:"endpoint,omitempty"` // the endpoint (of that fleet) that serves it
	Ctx      int       `json:"ctx,omitempty"`
	Np       int       `json:"np,omitempty"`
	Master   string    `json:"master"`
	Parts    []RunPart `json:"parts"` // in the order of the line: the master first
	// What the plan expected.
	PredTokenMS float64 `json:"pred_token_ms,omitempty"`
	PredTPS     float64 `json:"pred_tps,omitempty"`
	// What the engine said since: newest last, the most recent maxSamples.
	Samples []RunSample `json:"samples,omitempty"`
	Events  []RunEvent  `json:"events,omitempty"`
	// Stale (never stored; set when the runs are listed): why an active run looks dead, "" when it does not. A
	// suggestion only: nothing is changed until `fleet run prune --stale` marks it lost.
	Stale string `json:"stale,omitempty"`
	// Lost: the run was marked ended because it was found dead (its engine stopped answering and nothing ended it).
	Lost string `json:"lost,omitempty"`
}

// RunPart is one machine in the line.
type RunPart struct {
	Node      string  `json:"node"`
	First     int     `json:"first"`
	Layers    int     `json:"layers"`
	GB        float64 `json:"gb,omitempty"`
	Output    bool    `json:"output,omitempty"`
	ComputeMS float64 `json:"compute_ms,omitempty"`
	RTTms     float64 `json:"rtt_ms,omitempty"`
	SendMS    float64 `json:"send_ms,omitempty"`
}

type RunSample struct {
	At     time.Time `json:"at"`
	TPS    float64   `json:"tps,omitempty"`
	Busy   int       `json:"busy,omitempty"`
	Slots  int       `json:"slots,omitempty"`
	KV     float64   `json:"kv,omitempty"`
	Health string    `json:"health,omitempty"`
	// From the engine's profile (when the group has a stats_port): what a token cost, and where.
	TokenP50MS float64                  `json:"token_p50_ms,omitempty"`
	LocalMS    float64                  `json:"local_ms,omitempty"` // the master's own time per token
	Nodes      []RingNode               `json:"nodes,omitempty"`
	Reconnects map[string]ReconnectStat `json:"reconnects,omitempty"`
}

type RunEvent struct {
	At   time.Time `json:"at"`
	What string    `json:"what"`
}

const (
	maxSamples    = 40
	maxEvents     = 30
	maxRuns       = 50
	maxSharedRuns = 10
)

const (
	// staleAfter: an active run whose engine has given no healthy answer for this long, and does not answer now, is
	// called stale.
	staleAfter = 15 * time.Minute
	// staleUnbound: a recorded plan that was never bound to an endpoint is called stale after this long.
	staleUnbound = 6 * time.Hour
)

// runAlive asks an engine's /health (replaceable for tests).
var runAlive = func(ctx context.Context, client *http.Client, base, key string) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, serverRoot(base)+"/health", nil)
	if err != nil {
		return false
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == 200
}

// staleWhy says why an active run of this fleet looks dead ("" when it does not): its endpoint is gone, it never got
// one, or the engine has not been healthy for staleAfter and does not answer now. Nothing is changed.
func (s *Server) staleWhy(ctx context.Context, r Run, now time.Time) string {
	if !r.Active() {
		return ""
	}
	if r.Endpoint == "" {
		if now.Sub(r.At) > staleUnbound {
			return fmt.Sprintf("recorded %s ago and never bound to an endpoint", now.Sub(r.At).Round(time.Minute))
		}
		return ""
	}
	e := s.Config().Endpoint(r.Endpoint)
	if e == nil {
		return fmt.Sprintf("its endpoint %s is no longer in the configuration", r.Endpoint)
	}
	last := r.At
	for _, x := range r.Samples {
		if x.Health == "ok" && x.At.After(last) {
			last = x.At
		}
	}
	if now.Sub(last) < staleAfter {
		return ""
	}
	if e.URL != "" && runAlive(ctx, s.client, e.URL, e.APIKey()) {
		return "" // the daemon just has not sampled it yet
	}
	return fmt.Sprintf("no healthy answer from %s since %s", r.Endpoint, last.Format("2006-01-02 15:04"))
}

// markStale sets Stale on the active runs of this fleet's own store in runs (a copy per run; the store is untouched).
func (s *Server) markStale(ctx context.Context, runs []Run) []Run {
	now := time.Now()
	out := make([]Run, len(runs))
	copy(out, runs)
	var wg sync.WaitGroup
	for i := range out {
		if !out[i].Active() {
			continue
		}
		if _, own := s.runs.get(out[i].ID); !own {
			continue // another fleet's record: only that fleet can tell
		}
		wg.Add(1)
		go func(r *Run) {
			defer wg.Done()
			r.Stale = s.staleWhy(ctx, *r, now)
		}(&out[i])
	}
	wg.Wait()
	return out
}

// endStale marks the stale active runs lost and ended (dry: only reports them). It stops no process: a stale run's
// processes are gone or unreachable; `fleet run end` is what stops them.
func (s *Server) endStale(ctx context.Context, dry bool) []Run {
	s.runs.load()
	s.runs.mu.Lock()
	snapshot := append([]Run(nil), s.runs.list...)
	s.runs.mu.Unlock()
	var gone []Run
	now := time.Now()
	for _, r := range snapshot {
		why := s.staleWhy(ctx, r, now)
		if why == "" {
			continue
		}
		r.Stale = why
		gone = append(gone, r)
		if dry {
			continue
		}
		s.runs.update(r.ID, func(x *Run) {
			if x.Active() {
				x.Ended, x.Lost = now, why
				x.event("lost: " + why)
			}
		})
	}
	return gone
}

// Active: not ended.
func (r Run) Active() bool { return r.Ended.IsZero() }

// MeasuredTPS is the mean decode speed of the samples that had one (0: none yet).
func (r Run) MeasuredTPS() float64 {
	var sum float64
	n := 0
	for _, x := range r.Samples {
		if x.TPS > 0 {
			sum += x.TPS
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

// Nodes lists the machines of the line, master first.
func (r Run) Nodes() []string {
	var out []string
	for _, p := range r.Parts {
		out = append(out, p.Node)
	}
	return out
}

type runStore struct {
	mu   sync.Mutex
	once sync.Once
	list []Run // oldest first
}

func (rs *runStore) load() {
	rs.once.Do(func() {
		b, err := os.ReadFile(config.RunsPath())
		if err == nil {
			json.Unmarshal(b, &rs.list)
		}
	})
}

// save writes the file atomically; the caller holds mu.
func (rs *runStore) save() {
	p := config.RunsPath()
	if os.MkdirAll(filepath.Dir(p), 0o755) != nil {
		return
	}
	b, _ := json.Marshal(rs.list)
	tmp := p + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, p)
	}
}

func (rs *runStore) add(r Run) {
	rs.load()
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.list = append(rs.list, r)
	if len(rs.list) > maxRuns {
		rs.list = rs.list[len(rs.list)-maxRuns:]
	}
	rs.save()
}

// recent returns the newest runs first (copies).
func (rs *runStore) recent(n int) []Run {
	rs.load()
	rs.mu.Lock()
	defer rs.mu.Unlock()
	var out []Run
	for i := len(rs.list) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, rs.list[i])
	}
	return out
}

func (rs *runStore) get(ref string) (Run, bool) {
	rs.load()
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if i := rs.find(ref); i >= 0 {
		return rs.list[i], true
	}
	return Run{}, false
}

// find locates a run by id or id prefix (newest first; "" or "last" = the newest), else by its endpoint's name (the
// active run of it, else its newest: `fleet run stop ornith122-chain` said "no such run"); mu held.
func (rs *runStore) find(ref string) int {
	for i := len(rs.list) - 1; i >= 0; i-- {
		if ref == "" || ref == "last" || strings.HasPrefix(rs.list[i].ID, ref) {
			return i
		}
	}
	newest := -1
	for i := len(rs.list) - 1; i >= 0; i-- {
		if rs.list[i].Endpoint != ref {
			continue
		}
		if rs.list[i].Active() {
			return i
		}
		if newest < 0 {
			newest = i
		}
	}
	return newest
}

func (rs *runStore) update(ref string, f func(r *Run)) bool {
	rs.load()
	rs.mu.Lock()
	defer rs.mu.Unlock()
	i := rs.find(ref)
	if i < 0 {
		return false
	}
	f(&rs.list[i])
	rs.save()
	return true
}

// forget deletes the record of an ended run chosen by id or unambiguous id prefix. It never touches an active
// run (those are ended first: fleet run end). Returns the run removed; errors: errRunNotFound, errRunActive,
// errRunAmbiguous. dry only reports.
func (rs *runStore) forget(ref string, dry bool) (Run, error) {
	rs.load()
	rs.mu.Lock()
	defer rs.mu.Unlock()
	idx := -1
	for i, r := range rs.list {
		if ref != "" && strings.HasPrefix(r.ID, ref) {
			if r.ID == ref {
				idx = i
				break
			}
			if idx >= 0 {
				return Run{}, errRunAmbiguous
			}
			idx = i
		}
	}
	if idx < 0 {
		return Run{}, errRunNotFound
	}
	r := rs.list[idx]
	if r.Active() {
		return r, errRunActive
	}
	if !dry {
		rs.list = append(rs.list[:idx:idx], rs.list[idx+1:]...)
		rs.save()
	}
	return r, nil
}

// prune deletes every ended run record (active ones are kept, whatever their state) and returns them.
func (rs *runStore) prune(dry bool) []Run {
	rs.load()
	rs.mu.Lock()
	defer rs.mu.Unlock()
	var keep, gone []Run
	for _, r := range rs.list {
		if r.Active() {
			keep = append(keep, r)
		} else {
			gone = append(gone, r)
		}
	}
	if !dry && len(gone) > 0 {
		rs.list = keep
		rs.save()
	}
	return gone
}

var (
	errRunNotFound  = fmt.Errorf("no such run here (only the fleet that planned a run can delete it)")
	errRunActive    = fmt.Errorf("the run is still active; end it first (fleet run end <id>)")
	errRunAmbiguous = fmt.Errorf("the id matches more than one run")
)

// active applies f to the runs bound to an endpoint that have not ended.
func (rs *runStore) active(endpoint string, f func(r *Run)) {
	rs.load()
	rs.mu.Lock()
	defer rs.mu.Unlock()
	changed := false
	for i := range rs.list {
		if rs.list[i].Endpoint == endpoint && rs.list[i].Active() {
			f(&rs.list[i])
			changed = true
		}
	}
	if changed {
		rs.save()
	}
}

func (r *Run) event(what string) {
	if n := len(r.Events); n > 0 && r.Events[n-1].What == what {
		return // the same thing is not news twice
	}
	r.Events = append(r.Events, RunEvent{At: time.Now(), What: what})
	if len(r.Events) > maxEvents {
		r.Events = r.Events[len(r.Events)-maxEvents:]
	}
}

// RunNote tells the active runs bound to an endpoint something happened (a stall, a worker going).
func (s *Server) runNote(endpoint, what string) {
	s.runs.active(endpoint, func(r *Run) { r.event(what) })
}

// runSample records what the engine of a group says now.
func (s *Server) runSample(endpoint string, g *GroupState) {
	smp := RunSample{At: time.Now(), TPS: g.TPS, Busy: g.Busy, Slots: g.Slots, KV: g.KVUsage, Health: g.Health}
	if g.Trace != nil {
		smp.TokenP50MS, smp.LocalMS, smp.Nodes, smp.Reconnects = g.Trace.TokenP50MS, g.Trace.LocalMS, g.Trace.Nodes, g.Trace.Reconnects
	}
	s.runs.active(endpoint, func(r *Run) {
		r.Samples = append(r.Samples, smp)
		if len(r.Samples) > maxSamples {
			r.Samples = r.Samples[len(r.Samples)-maxSamples:]
		}
	})
}

// RecordRun makes a record of a planned candidate (1-based pick) and says how it is going to be served.
func (s *Server) RecordRun(res plan.Result, pick int, endpoint, engine string) (Run, error) {
	if pick < 1 || pick > len(res.Candidates) {
		return Run{}, fmt.Errorf("the plan has %d candidate(s); there is no number %d", len(res.Candidates), pick)
	}
	if endpoint != "" && s.Config().Endpoint(endpoint) == nil {
		_ = s.Reload()
		if s.Config().Endpoint(endpoint) == nil {
			return Run{}, fmt.Errorf("no such endpoint: %s", endpoint)
		}
	}
	c := res.Candidates[pick-1]
	r := Run{
		ID: fmt.Sprintf("r%x", time.Now().UnixNano()/1e6), Fleet: s.Config().Name, At: time.Now(),
		Model: res.Model, SHA256: res.SHA256, Engine: engine, Endpoint: endpoint, Ctx: res.Ctx, Np: res.Np,
		Master: c.Master, PredTokenMS: c.TokenMS, PredTPS: c.TPS,
	}
	if r.Engine == "" {
		if e := s.Config().Endpoint(endpoint); e != nil && e.Group != nil {
			r.Engine = e.Group.Engine
		}
	}
	for _, p := range c.Parts {
		r.Parts = append(r.Parts, RunPart{Node: p.Node, First: p.First, Layers: p.Layers, GB: p.GB, Output: p.Output,
			ComputeMS: p.ComputeMS, RTTms: p.RTTms, SendMS: p.SendMS})
	}
	// the line starts at the master
	sort.SliceStable(r.Parts, func(i, j int) bool { return r.Parts[i].Node == c.Master && r.Parts[j].Node != c.Master })
	r.event("planned: " + fmt.Sprintf("%.1f t/s expected", c.TPS))
	s.runs.add(r)
	return r, nil
}

// AllRuns gathers the runs every machine's fleet reports (a fleet reports its own through each of its
// machines: one copy per id), the newest first.
func (s *Server) AllRuns(ctx context.Context) []Run {
	seen := map[string]bool{}
	var out []Run
	add := func(rs []Run) {
		for _, r := range rs {
			if !seen[r.ID] {
				seen[r.ID] = true
				out = append(out, r)
			}
		}
	}
	add(s.runs.recent(maxRuns))
	for _, ns := range s.Status().Nodes {
		if ns.Self {
			continue
		}
		add(s.HostOf(ctx, ns.Node, nil).Runs)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out
}

// Drain says whether this machine has left the line.
func (s *Server) Draining() bool { return s.Config().Drain }

// SetDrain leaves the line (or rejoins). The runs this fleet planned that use the machine are told.
func (s *Server) SetDrain(on bool) error {
	cfg := s.Config()
	if cfg.Drain == on {
		return nil
	}
	cfg.Drain = on
	if err := s.SetConfig(cfg); err != nil {
		return err
	}
	me := hostLabel()
	what := me + " left the line"
	if !on {
		what = me + " is back in the line"
	}
	s.runs.load()
	s.runs.mu.Lock()
	for i := range s.runs.list {
		r := &s.runs.list[i]
		if !r.Active() {
			continue
		}
		for _, n := range r.Nodes() {
			if s.isSelfNode(n) {
				r.event(what)
			}
		}
	}
	s.runs.save()
	s.runs.mu.Unlock()
	return nil
}

func (s *Server) runHandlers(mux *http.ServeMux, adm func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET /fleet/runs", adm(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"runs": s.markStale(r.Context(), s.AllRuns(r.Context()))})
	}))
	mux.HandleFunc("POST /fleet/runs", adm(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Plan     plan.Result `json:"plan"`
			Pick     int         `json:"pick"`
			Endpoint string      `json:"endpoint"`
			Engine   string      `json:"engine"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&in); err != nil {
			httpErr(w, 400, err.Error())
			return
		}
		if in.Pick == 0 {
			in.Pick = 1
		}
		run, err := s.RecordRun(in.Plan, in.Pick, in.Endpoint, in.Engine)
		if err != nil {
			httpErr(w, 400, err.Error())
			return
		}
		writeJSON(w, run)
	}))
	// a deploy started by `fleet deploy` on this machine opens (and later updates) its run here: the daemon is the only
	// writer of the run file, the deploy goes through this call
	mux.HandleFunc("POST /fleet/runs/deploy", adm(func(w http.ResponseWriter, r *http.Request) {
		var in DeployNote
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil || in.ID == "" {
			httpErr(w, 400, "id is required")
			return
		}
		writeJSON(w, s.NoteDeploy(in))
	}))
	// bind a run to an endpoint, or end it (only this fleet's own records)
	mux.HandleFunc("POST /fleet/runs/set", adm(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ID       string  `json:"id"`
			Endpoint *string `json:"endpoint"`
			End      bool    `json:"end"`
			// KeepProcesses: close the record only (the processes keep running; `fleet run stop` stops them)
			KeepProcesses bool `json:"keep_processes"`
			Local         bool `json:"local"` // set on a forwarded call
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil || in.ID == "" {
			httpErr(w, 400, "id is required")
			return
		}
		if _, here := s.runs.get(in.ID); !here && !in.Local { // planned by another fleet: it owns the record
			in.Local = true
			if s.forwardRun(w, r.Context(), "/fleet/runs/set", in, 30*time.Second) {
				return
			}
		}
		if in.Endpoint != nil && *in.Endpoint != "" && s.Config().Endpoint(*in.Endpoint) == nil {
			_ = s.Reload()
			if s.Config().Endpoint(*in.Endpoint) == nil {
				httpErr(w, 400, "no such endpoint: "+*in.Endpoint)
				return
			}
		}
		ok := s.runs.update(in.ID, func(x *Run) {
			if in.Endpoint != nil {
				x.Endpoint = *in.Endpoint
				if e := s.Config().Endpoint(*in.Endpoint); e != nil && e.Group != nil && x.Engine == "" {
					x.Engine = e.Group.Engine
				}
				x.event("bound to endpoint " + *in.Endpoint)
			}
			if in.End && x.Active() {
				x.Ended = time.Now()
				x.event("ended")
				if x.Endpoint != "" && !in.KeepProcesses {
					if ep := s.Config().Endpoint(x.Endpoint); ep != nil {
						go s.killRunProcesses(ep)
					}
				}
			}
		})
		if !ok {
			httpErr(w, 404, "no such run here (only the fleet that planned a run can change it): "+in.ID)
			return
		}
		run, _ := s.runs.get(in.ID)
		writeJSON(w, run)
	}))
	// delete the records of ended runs: one by id, or all of them (prune). Never ends or kills anything.
	mux.HandleFunc("POST /fleet/runs/delete", adm(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ID     string `json:"id"`
			Prune  bool   `json:"prune"`
			Stale  bool   `json:"stale"`
			DryRun bool   `json:"dry_run"`
			Local  bool   `json:"local"` // set on a forwarded call: do not forward it again
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil || (in.ID == "") == !(in.Prune || in.Stale) {
			httpErr(w, 400, "give either id or prune")
			return
		}
		if in.Stale { // mark the dead active runs lost (ended); the records stay until a prune
			writeJSON(w, map[string]any{"lost": s.endStale(r.Context(), in.DryRun), "dry_run": in.DryRun})
			return
		}
		var gone []Run
		if in.Prune {
			gone = s.runs.prune(in.DryRun)
			if !in.Local { // the list shows every fleet's runs: prune theirs too
				gone = append(gone, s.forwardRunDelete(r.Context(), map[string]any{"prune": true, "dry_run": in.DryRun, "local": true})...)
			}
		} else {
			run, err := s.runs.forget(in.ID, in.DryRun)
			switch err {
			case nil:
				gone = []Run{run}
			case errRunNotFound:
				if !in.Local { // planned by another fleet: ask the others
					if rr := s.forwardRunDelete(r.Context(), map[string]any{"id": in.ID, "dry_run": in.DryRun, "local": true}); len(rr) > 0 {
						gone = rr
						break
					}
				}
				httpErr(w, 404, err.Error())
				return
			default:
				httpErr(w, 409, err.Error())
				return
			}
		}
		writeJSON(w, map[string]any{"removed": gone, "dry_run": in.DryRun})
	}))
	mux.HandleFunc("POST /fleet/drain", adm(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			On bool `json:"on"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&in); err != nil {
			httpErr(w, 400, err.Error())
			return
		}
		if err := s.SetDrain(in.On); err != nil {
			httpErr(w, 400, err.Error())
			return
		}
		writeJSON(w, map[string]bool{"draining": s.Draining()})
	}))
}

// forwardRun sends a call about a run this fleet did not plan to the fleets on the other machines and writes the
// answer of the first that knows the run (not a 404). It reports whether it answered.
func (s *Server) forwardRun(w http.ResponseWriter, ctx context.Context, path string, body any, timeout time.Duration) bool {
	for _, ns := range s.Status().Nodes {
		if ns.Self {
			continue
		}
		code, out, err := s.forwardStoreT(ctx, ns.Node, path, body, timeout)
		if err != nil || code == 404 || code == 403 || code == 0 {
			continue
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		w.Write(out)
		return true
	}
	return false
}

// forwardRunDelete asks the fleet on every other machine to delete run records (one id, or a prune) and returns
// what they removed. A machine that cannot be reached, or does not know the run, is skipped.
func (s *Server) forwardRunDelete(ctx context.Context, body map[string]any) []Run {
	var out []Run
	for _, ns := range s.Status().Nodes {
		if ns.Self {
			continue
		}
		code, b, err := s.forwardStore(ctx, ns.Node, "/fleet/runs/delete", body)
		if err != nil || code != 200 {
			continue
		}
		var res struct {
			Removed []Run `json:"removed"`
		}
		if json.Unmarshal(b, &res) == nil {
			out = append(out, res.Removed...)
		}
	}
	return out
}

// killRunProcesses stops the engine master, hub streams and worker processes of an ended run (what `fleet run end`
// does besides closing the record): the same careful stop as `fleet run stop`, only without a report (the call is
// made in the background) and without the busy check. Nothing but fleet's own processes on the ports is stopped.
func (s *Server) killRunProcesses(ep *config.Endpoint) {
	if ep == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	rep := s.StopEndpoint(ctx, *ep, StopOptions{Force: true})
	if rep.Refused != "" {
		log.Printf("run end: %s: %s", ep.Name, rep.Refused)
	}
	for _, st := range rep.Steps {
		if !st.OK {
			log.Printf("run end: %s: %s %s: %s", ep.Name, st.Role, st.Node, st.Result)
		}
	}
}

// DeployNote is what `fleet deploy` tells the daemon about its run (the deploy's own record, in short).
type DeployNote struct {
	ID       string    `json:"id"`     // the deploy's run id (run-<unix>, single-<node>-<unix>)
	Status   string    `json:"status"` // starting | running | timeout | stopped | failed
	Error    string    `json:"error,omitempty"`
	Model    string    `json:"model"`
	SHA256   string    `json:"sha256,omitempty"`
	Engine   string    `json:"engine,omitempty"`
	Endpoint string    `json:"endpoint,omitempty"`
	Master   string    `json:"master"`
	Nodes    []string  `json:"nodes,omitempty"` // the master first, then the workers
	Parts    []RunPart `json:"parts,omitempty"`
	Ctx      int       `json:"ctx,omitempty"`
	Np       int       `json:"np,omitempty"`
	TPS      float64   `json:"tps,omitempty"`
}

// NoteDeploy opens the run of a deploy, or updates it: a deploy that failed or was interrupted closes it (what it had
// started was handled by the deploy), the others stay active until `fleet run stop` / `fleet run end`.
func (s *Server) NoteDeploy(in DeployNote) Run {
	apply := func(x *Run) {
		if in.Model != "" {
			x.Model = in.Model
		}
		if in.Endpoint != "" {
			x.Endpoint = in.Endpoint
		}
		if in.SHA256 != "" {
			x.SHA256 = in.SHA256
		}
		if in.Ctx > 0 {
			x.Ctx = in.Ctx
		}
		if in.Np > 0 {
			x.Np = in.Np
		}
		if len(in.Parts) > 0 {
			x.Parts = in.Parts
		}
		x.event("deploy: " + in.Status)
		if (in.Status == "failed" || in.Status == "stopped") && x.Active() {
			x.Ended = time.Now()
			if in.Error != "" {
				x.event("ended: " + in.Error)
			}
		}
	}
	if s.runs.update(in.ID, apply) {
		s.endReplacedRuns(in)
		r, _ := s.runs.get(in.ID)
		return r
	}
	r := Run{ID: in.ID, Fleet: s.Config().Name, At: time.Now(), Model: in.Model, SHA256: in.SHA256, Engine: in.Engine,
		Endpoint: in.Endpoint, Ctx: in.Ctx, Np: in.Np, Master: in.Master, PredTPS: in.TPS}
	if len(in.Parts) > 0 {
		r.Parts = in.Parts
	} else {
		for _, n := range in.Nodes {
			r.Parts = append(r.Parts, RunPart{Node: n})
		}
	}
	apply(&r)
	s.runs.add(r)
	s.endReplacedRuns(in)
	return r
}

// endReplacedRuns closes the earlier records of an endpoint once a deploy has come up on it: the deploy stopped what
// ran there before (--force), so that record describes nothing any more. Without it every redeploy left one "running"
// record behind (33 for one process on victor) and `fleet run prune --stale` could not tell, they all point at the
// live endpoint. A deploy that is only starting, or that failed, leaves the serving run alone.
func (s *Server) endReplacedRuns(in DeployNote) {
	if in.Status != "running" || in.Endpoint == "" {
		return
	}
	me := s.Config().Name
	s.runs.active(in.Endpoint, func(x *Run) {
		if x.ID == in.ID || (x.Fleet != "" && x.Fleet != me) {
			return
		}
		x.Ended = time.Now()
		x.event("ended: replaced by " + in.ID + " on the same endpoint")
	})
}

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
)

// GroupState is what an attached group (a model spread over machines, started by someone else) looks like
// from here: the engine's own server (stock llama-server endpoints) and the machines that hold the rest.
type GroupState struct {
	Engine  string        `json:"engine"`
	Members []GroupMember `json:"members,omitempty"`
	Health  string        `json:"health,omitempty"` // llama-server /health status
	Ctx     int           `json:"n_ctx,omitempty"`  // per slot
	Slots   int           `json:"slots,omitempty"`  // total_slots
	Busy    int           `json:"busy,omitempty"`   // slots processing
	Model   string        `json:"model,omitempty"`
	TPS     float64       `json:"tps,omitempty"` // from /metrics (predicted tokens per second)
	KVUsage float64       `json:"kv_usage,omitempty"`
	Trace   *RingTrace    `json:"trace,omitempty"` // the engine\'s own per-token profile (llama-ring /ring/trace)
	Warn    []string      `json:"warn,omitempty"`
}

type GroupMember struct {
	Node string `json:"node"`
	Up   bool   `json:"up"`
	Err  string `json:"err,omitempty"`
}

// serverRoot is the engine's address without the /v1 the endpoints carry (llama-server's /health, /props,
// /slots and /metrics live at the root).
func serverRoot(base string) string {
	return strings.TrimSuffix(strings.TrimRight(base, "/"), "/v1")
}

func (s *Server) engineJSON(ctx context.Context, e *config.Endpoint, url string, v any) error {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if k := e.APIKey(); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return fmt.Errorf("http %d", resp.StatusCode)
	}
	if v == nil {
		return nil
	}
	return json.Unmarshal(b, v)
}

// watchGroup fills st.Group for an endpoint that declares a group, and takes the endpoint down when a member
// machine is unreachable: the engine cannot answer without all of its workers, and cannot say so itself.
func (s *Server) watchGroup(ctx context.Context, e *config.Endpoint, base string, st *EpState) {
	g := e.Group
	if g == nil {
		return
	}
	gs := &GroupState{Engine: g.Engine}
	st.Group = gs
	var down []string
	for _, w := range g.Workers {
		hr := s.HostOf(ctx, w, nil)
		m := GroupMember{Node: w, Up: hr.Err == "", Err: hr.Err}
		if m.Up && hr.Draining {
			m.Up, m.Err = false, "left the line"
		}
		if !m.Up {
			down = append(down, w)
		}
		gs.Members = append(gs.Members, m)
	}
	if len(down) > 0 && st.OK {
		st.OK = false
		st.Err = "worker " + strings.Join(down, ", ") + " unreachable"
	}
	if len(down) > 0 {
		s.runNote(e.Name, "down: "+strings.Join(down, ", "))
	}
	if base == "" || st.noConn {
		return // no server reached (tunnel down, refused, dial timeout): /health, /props, /slots … would only dial again
	}
	root := serverRoot(base)
	var h struct {
		Status string `json:"status"`
	}
	if err := s.engineJSON(ctx, e, root+"/health", &h); err == nil {
		gs.Health = h.Status
	}
	var props struct {
		TotalSlots int    `json:"total_slots"`
		ModelPath  string `json:"model_path"`
		Default    struct {
			Params struct {
				NCtx int `json:"n_ctx"`
			} `json:"params"`
			NCtx int `json:"n_ctx"`
		} `json:"default_generation_settings"`
	}
	if err := s.engineJSON(ctx, e, root+"/props", &props); err == nil {
		gs.Slots, gs.Model = props.TotalSlots, props.ModelPath
		gs.Ctx = props.Default.NCtx
		if gs.Ctx == 0 {
			gs.Ctx = props.Default.Params.NCtx
		}
	}
	var slots []struct {
		Processing bool `json:"is_processing"`
	}
	if err := s.engineJSON(ctx, e, root+"/slots", &slots); err == nil {
		for _, sl := range slots {
			if sl.Processing {
				gs.Busy++
			}
		}
	}
	gs.TPS, gs.KVUsage = s.engineMetrics(ctx, e, root)
	if g.StatsPort > 0 {
		if tr := s.ringTrace(ctx, e, root, g); tr != nil {
			gs.Trace = tr
			if tr.Down() {
				// the engine lost a worker (a culprit or a broken link; victims alone are not the cause): it answers /health
				// but every request fails until it is restarted
				why := tr.FailureWhy()
				gs.Warn = append(gs.Warn, why)
				if st.OK {
					st.OK, st.Err = false, why
				}
				s.runNote(e.Name, why)
			}
			s.watchReconnects(e, tr)
			s.watchSteps(e, gs, tr)
			if gs.TPS == 0 && tr.TokenP50MS > 0 { // /metrics needs --metrics; the profile says it anyway
				gs.TPS = 1000 / tr.TokenP50MS
			}
		}
	}
	if gs.Slots > 0 && e.Slots > 0 && e.Slots != gs.Slots {
		gs.Warn = append(gs.Warn, fmt.Sprintf("slots is %d in the config, the engine runs %d", e.Slots, gs.Slots))
	}
	if gs.Health != "" && gs.Health != "ok" {
		gs.Warn = append(gs.Warn, "engine health: "+gs.Health)
	}
	s.runSample(e.Name, gs)
}

// metrics reads llama-server's Prometheus text (only when it was started with --metrics).
func (s *Server) engineMetrics(ctx context.Context, e *config.Endpoint, root string) (tps, kv float64) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, root+"/metrics", nil)
	if err != nil {
		return
	}
	if k := e.APIKey(); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	for _, ln := range strings.Split(string(b), "\n") {
		f := strings.Fields(ln)
		if len(f) != 2 || strings.HasPrefix(ln, "#") {
			continue
		}
		var x float64
		if _, err := fmt.Sscanf(f[1], "%g", &x); err != nil {
			continue
		}
		switch {
		case strings.HasSuffix(f[0], "predicted_tokens_seconds"):
			tps = x
		case strings.HasSuffix(f[0], "kv_cache_usage_ratio"):
			kv = x
		}
	}
	return
}

// watchSteps notices a frozen engine from its own counter: a request is running (a slot is processing) and the number of
// steps the ring has done has not moved for the stall time. /health stays "ok" and the master still answers
// /models in that state (a worker that stopped), so this is what tells, without waiting for a request of ours to hang.
func (s *Server) watchSteps(e *config.Endpoint, gs *GroupState, tr *RingTrace) {
	now := time.Now()
	s.mu.Lock()
	if s.steps == nil {
		s.steps = map[string]stepMark{}
	}
	prev, seen := s.steps[e.Name]
	if !seen || tr.StepsTotal != prev.total || gs.Busy == 0 { // moving, or nothing to do: the clock restarts
		s.steps[e.Name] = stepMark{total: tr.StepsTotal, at: now}
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	limit := e.StallAfter()
	if limit <= 0 || now.Sub(prev.at) < limit {
		return
	}
	why := fmt.Sprintf("stalled: a request is running and the ring has taken no step for %s", now.Sub(prev.at).Round(time.Second))
	gs.Warn = append(gs.Warn, why)
	s.penalizeFor(e.Name, stallPenalty, why)
}

type stepMark struct {
	total int
	at    time.Time
}

// watchReconnects records a runNote whenever a worker's session resume counter increases on GET /ring/info.
func (s *Server) watchReconnects(e *config.Endpoint, tr *RingTrace) {
	if tr == nil || len(tr.Reconnects) == 0 {
		return
	}
	s.mu.Lock()
	if s.resumed == nil {
		s.resumed = map[string]map[string]uint64{}
	}
	prev := s.resumed[e.Name]
	if prev == nil {
		prev = map[string]uint64{}
		s.resumed[e.Name] = prev
	}
	addrs := make([]string, 0, len(tr.Reconnects))
	for a := range tr.Reconnects {
		addrs = append(addrs, a)
	}
	sort.Strings(addrs)
	var notes []string
	for _, a := range addrs {
		st := tr.Reconnects[a]
		old := prev[a]
		if st.Resumed > old {
			prev[a] = st.Resumed
			label := a
			if e.Group != nil {
				for node, ad := range e.Group.Addrs {
					if ad == a {
						label = node
						break
					}
				}
				if label == a && len(e.Group.Workers) == 1 && len(tr.Reconnects) == 1 {
					label = e.Group.Workers[0]
				}
			}
			notes = append(notes, fmt.Sprintf("session resumed: %s (resumed %d×, replayed %d, resent %d, downtime %.0f ms)",
				label, st.Resumed, st.Replayed, st.ReplyResent, st.LastDowntimeMS))
		}
	}
	s.mu.Unlock()
	for _, n := range notes {
		s.runNote(e.Name, n)
	}
}

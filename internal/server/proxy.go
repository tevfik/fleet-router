package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/tevfik/fleet-router/internal/tunnel"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/economy"
	"github.com/tevfik/fleet-router/internal/vision"
)

// Resolve turns the request's "model" value into an ordered candidate list.
// Order: role → preferred, fallback…; endpoint name; raw model id. Healthy ones come first
// (if none is healthy, all are still tried — the health state may be stale).
func (s *Server) Resolve(model string) []config.Endpoint {
	c, _, _ := s.route(model)
	return c
}

// route: Resolve plus the role itself (for priority/parallel/overflow settings). role=="" when not a role.
func (s *Server) route(model string) (cands []config.Endpoint, role string, rc config.Role) {
	cfg := s.Config()
	var pool []config.Endpoint
	if r, ok := cfg.Roles[model]; ok {
		role, rc = model, r
		pool = cfg.Pool(r)
	} else {
		pool = cfg.Lookup(model)
	}
	var good, bad []config.Endpoint
	for _, e := range pool {
		if s.healthy(e.Name) {
			good = append(good, e)
		} else {
			bad = append(bad, e)
		}
	}
	return s.condition(rc, append(good, bad...)), role, rc
}

var hopHeaders = map[string]bool{"Connection": true, "Keep-Alive": true, "Transfer-Encoding": true,
	"Te": true, "Trailer": true, "Upgrade": true, "Content-Length": true}

// proxyV1: POST /v1/* — routes by model and passes the stream through untouched.
// Failover happens only BEFORE the first byte is sent (connection error / 5xx / 429).
func (s *Server) proxyV1(w http.ResponseWriter, r *http.Request) {
	rec := &statusRec{ResponseWriter: w}
	w = rec
	ent := LogEntry{Time: time.Now(), Path: r.URL.Path, Via: r.Header.Get("X-Fleet-Via"), Remote: !isLoopback(r), Client: clientLabel(r)}
	apiKeyToken := ""
	defer func() {
		ent.DurMS = time.Since(ent.Time).Milliseconds()
		// an outcome the handler recorded itself (client gone, stall, a failure after the headers) wins over what was
		// written to the client: those paths send nothing more, and the written code (often 200) would hide them
		if ent.Status < 400 {
			ent.Status = rec.code
			if ent.Status == 0 {
				ent.Status = 200
			}
		}
		if ep := rec.Header().Get("X-Fleet-Endpoint"); ep != "" || ent.Endpoint == "" {
			ent.Endpoint = ep
		}
		if ent.Status >= 400 && ent.Err == "" {
			ent.Err = clip(string(rec.errB), 1000)
		}
		s.reqs.add(ent)
		ref := ""
		if e := s.Config().Endpoint(ent.Endpoint); e != nil {
			ref = e.Ref()
		}
		if ref == "" {
			ref = ent.ref // a tunnel endpoint the configuration does not list
		}
		s.live.addUsage(ent, ref)
		if apiKeyToken != "" && ent.Status < 400 {
			GetKeyStore().RecordUsage(apiKeyToken, ent.In+ent.Out, ent.DurMS)
		}
	}()
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<20))
	if err != nil {
		httpErr(w, 400, "could not read body: "+err.Error())
		return
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		httpErr(w, 400, "invalid JSON: "+err.Error())
		return
	}
	model, _ := body["model"].(string)
	ent.Model = clip(model, 200) // the model string is the caller's: it lands in the request log, usage keys and stats file
	cands, role, rc := s.route(model)
	path := parsePath(r.Header.Get(headerPath))
	cands = dropLoops(cands, path)
	ent.Role = role

	// API key authorization: validate token and check model/role permissions
	reqPriority := rc.Priority
	keyed := false // the caller proved who it is (a registered API key); a header claim alone proves nothing
	if token := ExtractBearer(r); token != "" {
		if ak, ok := GetKeyStore().Validate(token); ok {
			keyed = true
			apiKeyToken = token
			if !ak.AllowsModel(model) {
				httpErr(w, 403, fmt.Sprintf("API key %q is not authorized to access model %q", ak.Name, model))
				return
			}
			if role != "" && !ak.AllowsRole(role) {
				httpErr(w, 403, fmt.Sprintf("API key %q is not authorized for role %q", ak.Name, role))
				return
			}
			switch ak.QoS {
			case "ultra-low-latency":
				reqPriority += 20
			case "max-throughput":
				reqPriority += 10
			}
		} else if strings.HasPrefix(token, "flt_live_") {
			httpErr(w, 401, "invalid API key")
			return
		}
	}
	// Fair Queuing & Token Slicing: Check X-Wallet-Address header for credit-weighted priority boost
	pcfg := s.Config() // a snapshot: s.cfg is swapped by SetConfig under s.mu
	if pcfg.Economy == nil || (pcfg.Economy.IsEnabled() && pcfg.Economy.IsFairQueuing()) {
		// X-Wallet-Address is a claim without proof (no signature): honored only for a keyed or a direct local caller
		if walletAddr := r.Header.Get("X-Wallet-Address"); walletAddr != "" && (keyed || directLoopback(r)) {
			if ledger, err := economy.LoadLedger(""); err == nil {
				if acc, err := ledger.GetAccount(walletAddr); err == nil && acc.Balance > 0 {
					boost := int(min(15, acc.Balance/1000))
					reqPriority += boost
				}
			}
		}
	}
	rc.Priority = reqPriority
	if len(cands) == 0 {
		httpErr(w, 404, fmt.Sprintf("model/role not found: %q (GET /v1/models)", model))
		return
	}
	// capability-aware: only candidates that can handle this kind of request (images need vision, …)
	need := requestNeeds(r.URL.Path, body)
	filtered := filterNeeds(cands, need)
	if len(filtered) == 0 && containsNeed(need, "vision") {
		// Try decoupled vision microservice pre-flight extraction if configured
		vURL := ""
		if pcfg.VisionURL != "" {
			vURL = pcfg.VisionURL
		} else if env := os.Getenv("FLEET_VISION_URL"); env != "" {
			vURL = env
		}
		if vURL != "" {
			vClient := vision.NewClient(vURL)
			if vClient.Ping(r.Context()) {
				if msgs, ok := body["messages"].([]any); ok {
					if newMsgs, modified, err := vision.ProcessMessages(r.Context(), vClient, msgs); err == nil && modified {
						body["messages"] = newMsgs
						if newRaw, err := json.Marshal(body); err == nil {
							raw = newRaw
							need = removeNeed(need, "vision")
							filtered = filterNeeds(cands, need)
						}
					}
				}
			}
		}
	}
	if len(filtered) == 0 {
		httpErr(w, 422, fmt.Sprintf("no endpoint for %q supports: %s — set `caps` on the endpoint if it does", model, strings.Join(need, ", ")))
		return
	}
	cands = filtered
	cands = filterCtx(cands, estimateTokens(raw, body))
	sz := sizeOf(raw, body)
	spill := rc.Overflow || (rc.Strategy != "" && rc.Strategy != "failover") // pooled strategies always use any free member
	// queue wait limit: cancelled if the client gives up or 10 minutes pass
	qstart := time.Now()
	qctx, qcancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer qcancel()
	if role != "" && rc.MaxParallel > 0 { // per-role parallel limit
		rl := s.lim("role:" + role)
		if err := rl.acquire(qctx, rc.Priority); err != nil {
			httpErr(w, 503, fmt.Sprintf("timed out in the queue of role %s: %v", role, err))
			return
		}
		defer rl.release()
	}
	urlPath := strings.TrimPrefix(r.URL.Path, "/v1")
	var lastErr string
	remaining := cands
	for len(remaining) > 0 {
		// Order by strategy and grab a free slot in one critical section: concurrent requests would
		// otherwise all see the same "least busy" endpoint before any of them has taken its slot.
		idx := -1
		s.pickMu.Lock()
		remaining = s.applyStrategySized(role, rc, remaining, sz)
		if spill {
			for i := range remaining {
				if s.lim("ep:" + remaining[i].Name).try() {
					idx = i
					break
				}
			}
		}
		s.pickMu.Unlock()
		var err error
		if idx < 0 { // nothing free: wait in the first candidate's priority queue
			idx, err = s.pickSlot(qctx, remaining, rc, false)
		}
		if err != nil {
			httpErr(w, 503, "timed out in the endpoint queue: "+err.Error())
			return
		}
		e := remaining[idx]
		ent.Attempts++
		ent.QueueMS = time.Since(qstart).Milliseconds()
		remaining = append(append([]config.Endpoint{}, remaining[:idx]...), remaining[idx+1:]...)
		relLimit := s.lim("ep:" + e.Name).release
		base, relTunnel := s.baseURLUse(r.Context(), &e)
		rel := func() { relLimit(); relTunnel() }
		if base == "" {
			rel()
			lastErr = e.Name + ": tunnel down"
			continue
		}
		b := cloneMap(body)
		b["model"] = e.Wire()
		for k, v := range e.Body { // endpoint defaults; leave alone if the client set them
			if _, ok := b[k]; !ok {
				b[k] = v
			}
		}
		out, _ := json.Marshal(b)
		if strings.HasPrefix(base, "tunnel://") {
			nodeName := strings.TrimPrefix(base, "tunnel://")
			if s.hub != nil {
				treq, err := http.NewRequestWithContext(r.Context(), r.Method, "/v1"+urlPath, bytes.NewReader(out)) // the node's daemon serves the same /v1 API
				if err != nil {
					rel()
					lastErr = e.Name + ": " + err.Error()
					continue
				}
				// Only what the node's /v1 needs: the caller's Authorization (their API key), cookies and forwarding
				// headers must not reach a third-party edge node.
				treq.Header = tunnelRequestHeaders(r.Header)
				w.Header().Set("X-Fleet-Endpoint", e.Name)
				if role != "" {
					w.Header().Set("X-Fleet-Role", role)
				}
				// Capture the endpoint's node/model reference for usage stats
				ent.ref = e.Ref()
				t0 := time.Now()
				tw := newTunnelWriter(t0, urlPath, w)
				err = s.hub.ForwardRequest(nodeName, treq, tw)
				rel()
				if err != nil {
					if r.Context().Err() != nil { // the client went away: the endpoint is healthy, nothing to retry
						ent.Endpoint, ent.Status, ent.Err = e.Name, 499, "client closed the request"
						return
					}
					s.penalize(e.Name)
					s.live.bump(e.Name, false)
					lastErr = e.Name + ": " + err.Error()
					if errors.Is(err, tunnel.ErrResponseStarted) { // bytes are already with the client: no second answer
						ent.Endpoint, ent.Status, ent.Err = e.Name, 502, clip(lastErr, 1000)
						return
					}
					continue
				}
				s.live.bump(e.Name, true)
				ent.Model, ent.Endpoint, ent.Status = e.Wire(), e.Name, 200
				// Record usage from the observer
				if tw.obs != nil {
					res := tw.obs.result(len(raw) / 4)
					if r.Context().Err() == nil { // an aborted stream would skew the figures
						s.live.record(e.Name, e.Model, res)
					}
					if res.TPS > 0 {
						ent.TPS = float64(int(res.TPS*10+0.5)) / 10
					}
					ent.TTFTMS, ent.In, ent.Out = res.TTFT.Milliseconds(), res.Prompt, res.Out
				}
				return
			}
		}
		stall := e.StallAfter()
		uctx, ucancel := context.WithCancel(r.Context())
		wd := newWatchdog(stall, ucancel)
		req, err := http.NewRequestWithContext(uctx, r.Method, base+urlPath, bytes.NewReader(out))
		if err != nil { // a malformed url in the configuration
			wd.stop()
			rel()
			lastErr = e.Name + ": " + short(err.Error())
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		if a := r.Header.Get("Accept"); a != "" {
			req.Header.Set("Accept", a)
		}
		if k := e.APIKey(); k != "" {
			req.Header.Set("Authorization", "Bearer "+k)
		}
		if e.Peer != "" { // tell the peer where the request has been, and who is really asking
			cfg := s.Config()
			req.Header.Set(headerPath, strings.Join(append(append([]string{}, path...), cfg.ID), ","))
			req.Header.Set("X-Fleet-Client", "fleet:"+cfg.Name)
		}
		t0 := time.Now()
		// The first token waits for the prompt: three times the patience, counted from the request (a plain answer
		// that is not streamed says nothing until it is whole: ten times).
		first := 3 * stall
		if streaming, _ := body["stream"].(bool); !streaming {
			first = 10 * stall
		}
		wd.arm(first)
		resp, err := s.client.Do(req)
		if err != nil {
			wd.stop()
			rel()
			if r.Context().Err() != nil {
				ent.Endpoint, ent.Status, ent.Err = e.Name, 499, "client closed the request"
				return // client gave up
			}
			if wd.fired() {
				s.penalizeFor(e.Name, stallPenalty, "stalled: no answer for "+first.String())
				s.live.bump(e.Name, false)
				lastErr = e.Name + ": stalled"
				continue
			}
			s.penalize(e.Name)
			s.live.bump(e.Name, false)
			lastErr = e.Name + ": " + short(err.Error())
			continue
		}
		if resp.StatusCode >= 500 || resp.StatusCode == 429 {
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
			resp.Body.Close()
			wd.stop()
			rel()
			s.penalize(e.Name)
			s.live.bump(e.Name, false)
			lastErr = fmt.Sprintf("%s: http %d %s", e.Name, resp.StatusCode, strings.TrimSpace(string(msg)))
			continue
		}
		// Wait for the first byte of the body before answering the client: llama-server sends the headers before the
		// first token, so they prove nothing, and a stall before any byte can still fail over to the next candidate.
		peek := make([]byte, 8192)
		pn, perr := resp.Body.Read(peek)
		if pn == 0 && wd.fired() && r.Context().Err() == nil {
			resp.Body.Close()
			wd.stop()
			rel()
			s.penalizeFor(e.Name, stallPenalty, "stalled: no token for "+first.String())
			s.live.bump(e.Name, false)
			lastErr = e.Name + ": stalled"
			continue
		}
		if pn == 0 && perr != nil && r.Context().Err() != nil {
			resp.Body.Close()
			wd.stop()
			rel()
			ent.Endpoint, ent.Status, ent.Err = e.Name, 499, "client closed the request"
			return // client gave up
		}
		for k, vs := range resp.Header {
			if !hopHeaders[k] {
				for _, v := range vs {
					w.Header().Add(k, v)
				}
			}
		}
		w.Header().Set("X-Fleet-Endpoint", e.Name)
		w.WriteHeader(resp.StatusCode)
		var obs *observer
		if resp.StatusCode == 200 && strings.HasSuffix(urlPath, "/completions") {
			isSSE := strings.Contains(resp.Header.Get("Content-Type"), "event-stream")
			obs = newObserver(t0, isSSE)
		}
		wd.arm(stall)                                                                           // the first byte is here: from now on the patience is the stall time
		stream(w, wd.reader(io.MultiReader(bytes.NewReader(peek[:pn]), resp.Body), stall), obs) // the slot is held until the stream ends
		wd.stop()
		resp.Body.Close()
		if r.Context().Err() != nil && !wd.fired() { // the client left mid-answer
			ent.Endpoint, ent.Status, ent.Err = e.Name, 499, "client closed the request mid-answer"
		}
		if wd.fired() && r.Context().Err() == nil { // cut mid-answer: the client has what came so far
			s.penalizeFor(e.Name, stallPenalty, "stalled: no data for "+stall.String())
			s.live.bump(e.Name, false)
			rel()
			ent.Endpoint, ent.Status, ent.Err = e.Name, 504, "stalled mid-answer: no data for "+stall.String()
			return
		}
		s.live.bump(e.Name, resp.StatusCode < 400)
		if obs != nil {
			res := obs.result(len(raw) / 4)
			if r.Context().Err() == nil { // an aborted stream would skew the figures
				s.live.record(e.Name, e.Model, res)
			}
			if res.TPS > 0 {
				ent.TPS = float64(int(res.TPS*10+0.5)) / 10
			}
			ent.TTFTMS, ent.In, ent.Out = res.TTFT.Milliseconds(), res.Prompt, res.Out
		}
		rel()
		return
	}
	httpErr(w, 502, "all candidates failed: "+lastErr)
}

// pickSlot takes an endpoint slot from the candidate list and returns its index (slot held).
// With overflow on, it first tries every candidate without waiting (preferred full → spill to a
// fallback); otherwise it waits in the first candidate's priority queue.
func (s *Server) pickSlot(ctx context.Context, cands []config.Endpoint, rc config.Role, spill bool) (int, error) {
	if spill {
		for i := range cands {
			if s.lim("ep:" + cands[i].Name).try() {
				return i, nil
			}
		}
	}
	if err := s.lim("ep:"+cands[0].Name).acquire(ctx, rc.Priority); err != nil {
		return 0, err
	}
	return 0, nil
}

// stream forwards every chunk to the client immediately (adds no token latency).
func stream(w http.ResponseWriter, rd io.Reader, obs *observer) {
	fl, _ := w.(http.Flusher)
	rc := http.NewResponseController(w)
	buf := make([]byte, 8192)
	for {
		n, err := rd.Read(buf)
		if n > 0 {
			// a client that stops reading must not hold the endpoint and role slots for ever (the server has no
			// WriteTimeout, answers can be long): each chunk has to be taken within streamWriteTimeout
			_ = rc.SetWriteDeadline(time.Now().Add(streamWriteTimeout))
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
			if obs != nil {
				obs.feed(buf[:n]) // after the flush: measuring never delays a token
			}
		}
		if err != nil {
			return
		}
	}
}

func cloneMap(m map[string]any) map[string]any {
	c := make(map[string]any, len(m)+2)
	for k, v := range m {
		c[k] = v
	}
	return c
}

func httpErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": msg, "type": "fleet_error"}})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// modelsList: GET /v1/models — roles first, then every real model as "node/model" (for client model pickers).
// Each model carries "up" (false = offline/penalised); ?up=1 drops the ones that are not up.
func (s *Server) modelsList(w http.ResponseWriter, r *http.Request) {
	onlyUp := r.URL.Query().Get("up") == "1" // ?up=1: leave out models known to be down
	cfg := s.Config()
	type m struct {
		ID      string   `json:"id"`
		Object  string   `json:"object"`
		OwnedBy string   `json:"owned_by"`
		Caps    []string `json:"caps,omitempty"`
		Tags    []string `json:"tags,omitempty"`
		Up      *bool    `json:"up,omitempty"`
	}
	var data []m
	for _, r := range cfg.RoleNames() {
		data = append(data, m{ID: r, Object: "model", OwnedBy: "fleet-role"})
	}
	for _, e := range cfg.Endpoints {
		up := s.healthy(e.Name)
		if onlyUp && !up {
			continue
		}
		e := e
		data = append(data, m{ID: e.Ref(), Object: "model", OwnedBy: e.Node, Caps: effCaps(&e), Tags: e.Tags, Up: &up})
	}
	writeJSON(w, map[string]any{"object": "list", "data": data})
}

// requestNeeds derives the capabilities a request requires from its path and body.
func requestNeeds(path string, body map[string]any) []string {
	switch {
	case strings.HasSuffix(path, "/embeddings"):
		return []string{"embed"}
	case strings.HasSuffix(path, "/images/generations"):
		return []string{"image_gen"}
	case strings.HasSuffix(path, "/chat/completions"), strings.HasSuffix(path, "/completions"):
		need := []string{"chat"}
		msgs, _ := body["messages"].([]any)
		var vision, audio bool
		for _, m := range msgs {
			mm, _ := m.(map[string]any)
			parts, _ := mm["content"].([]any) // plain-string content has no parts
			for _, p := range parts {
				pm, _ := p.(map[string]any)
				switch pm["type"] {
				case "image_url", "image":
					vision = true
				case "input_audio", "audio":
					audio = true
				}
			}
		}
		if vision {
			need = append(need, "vision")
		}
		if audio {
			need = append(need, "audio")
		}
		return need
	}
	return nil
}

func containsNeed(need []string, cap string) bool {
	for _, n := range need {
		if n == cap {
			return true
		}
	}
	return false
}

func removeNeed(need []string, cap string) []string {
	var out []string
	for _, n := range need {
		if n != cap {
			out = append(out, n)
		}
	}
	return out
}

func filterNeeds(cands []config.Endpoint, need []string) []config.Endpoint {
	var out []config.Endpoint
next:
	for _, e := range cands {
		for _, n := range need {
			if !e.Has(n) {
				continue next
			}
		}
		out = append(out, e)
	}
	return out
}

// estimateTokens is a cheap size estimate (≈4 bytes per token + requested output).
func estimateTokens(raw []byte, body map[string]any) int {
	n := len(raw) / 4
	if mt, ok := body["max_tokens"].(float64); ok {
		n += int(mt)
	}
	return n
}

// sizeOf: the prompt and the reply this request is expected to need, in tokens.
func sizeOf(raw []byte, body map[string]any) reqSize {
	sz := reqSize{In: len(raw) / 4}
	for _, k := range []string{"max_tokens", "max_completion_tokens"} {
		if mt, ok := body[k].(float64); ok && mt > 0 {
			sz.Out = int(mt)
		}
	}
	return sz
}

// filterCtx drops candidates whose context window cannot hold a large request; if that would
// leave nothing, the original list is kept (the backend then reports the real error).
func filterCtx(cands []config.Endpoint, est int) []config.Endpoint {
	if est < 1500 {
		return cands
	}
	var out []config.Endpoint
	for _, e := range cands {
		if e.Ctx == 0 || est <= e.Ctx*9/10 {
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		return cands
	}
	return out
}

// applyStrategy reorders the healthy part of the pool per the role's strategy.
func (s *Server) applyStrategy(role string, rc config.Role, cands []config.Endpoint) []config.Endpoint {
	return s.applyStrategySized(role, rc, cands, reqSize{})
}

func (s *Server) applyStrategySized(role string, rc config.Role, cands []config.Endpoint, sz reqSize) []config.Endpoint {
	if role == "" || rc.Strategy == "" || rc.Strategy == "failover" || len(cands) < 2 {
		return s.condition(rc, cands)
	}
	var good, bad []config.Endpoint
	for _, e := range cands {
		if s.healthy(e.Name) {
			good = append(good, e)
		} else {
			bad = append(bad, e)
		}
	}
	if len(good) < 2 {
		return s.condition(rc, cands)
	}
	switch rc.Strategy {
	case "cost":
		good, _ = s.byCost(good, sz) // warmth, strain, queue and size are in the cost: no separate conditioning
		return append(good, bad...)
	case "least-busy":
		load := func(e config.Endpoint) float64 {
			used, waiting, cap := s.lim("ep:" + e.Name).stats()
			if cap == 0 {
				cap = 8 // unlimited endpoints: compare raw load against a nominal capacity
			}
			return float64(used+waiting) / float64(cap)
		}
		sort.SliceStable(good, func(i, j int) bool { return load(good[i]) < load(good[j]) })
	case "fastest":
		// expected seconds for a typical reply from live figures (benchmark tps as fallback), stretched by current load;
		// models with no speed data yet sort last, so they only get traffic once the fast ones are saturated
		score := func(e config.Endpoint) float64 {
			used, waiting, cap := s.lim("ep:" + e.Name).stats()
			if cap == 0 {
				cap = 8
			}
			return s.replyEstimate(e) * (1 + float64(used+waiting)/float64(cap))
		}
		sort.SliceStable(good, func(i, j int) bool { return score(good[i]) < score(good[j]) })
	case "round-robin":
		c, _ := s.rr.LoadOrStore(role, new(atomic.Uint64))
		k := int(c.(*atomic.Uint64).Add(1)-1) % len(good)
		good = append(append([]config.Endpoint{}, good[k:]...), good[:k]...)
	}
	return s.condition(rc, append(good, bad...))
}

// replyEstimate: expected seconds for a 300-token reply, from live figures when measured,
// else the benchmark tps; +Inf when nothing is known.
func (s *Server) replyEstimate(e config.Endpoint) float64 {
	lv, ok := s.live.get(e.Name)
	tps := e.TPS
	if ok && lv.Samples > 0 {
		tps = lv.TPS
	}
	ttft := 0.5
	if ok && lv.TTFTMS > 0 {
		ttft = lv.TTFTMS / 1000
	}
	if tps <= 0 {
		return math.Inf(1)
	}
	return ttft + 300/tps
}

// watchdog cuts an upstream request that has gone quiet: a model spread over machines blocks for ever when
// a worker link stalls, and nothing on the way would ever time out. Zero duration = off.
type watchdog struct {
	mu     sync.Mutex
	t      *time.Timer
	cancel context.CancelFunc
	hit    bool
	off    bool
}

func newWatchdog(d time.Duration, cancel context.CancelFunc) *watchdog {
	return &watchdog{cancel: cancel, off: d <= 0}
}

func (w *watchdog) arm(d time.Duration) {
	if w.off || d <= 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.t != nil {
		w.t.Stop()
	}
	w.t = time.AfterFunc(d, func() {
		w.mu.Lock()
		w.hit = true
		w.mu.Unlock()
		w.cancel()
	})
}

func (w *watchdog) stop() {
	w.mu.Lock()
	if w.t != nil {
		w.t.Stop()
	}
	w.mu.Unlock()
	w.cancel() // releases the request context (the body is already finished or abandoned)
}

func (w *watchdog) fired() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.hit
}

type quietReader struct {
	rd io.Reader
	w  *watchdog
	d  time.Duration
}

func (q quietReader) Read(p []byte) (int, error) {
	n, err := q.rd.Read(p)
	if n > 0 {
		q.w.arm(q.d)
	}
	return n, err
}

func (w *watchdog) reader(rd io.Reader, d time.Duration) io.Reader {
	if w.off {
		return rd
	}
	return quietReader{rd, w, d}
}

// tunnelRequestHeaders is the allowlist of caller headers that travel over a hub tunnel to a node.
func tunnelRequestHeaders(in http.Header) http.Header {
	out := http.Header{"Content-Type": {"application/json"}}
	if a := in.Get("Accept"); a != "" {
		out.Set("Accept", a)
	}
	return out
}

// clip cuts s to at most n bytes (on a rune boundary) so caller-controlled strings cannot bloat the log and stats.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// streamWriteTimeout bounds one write of an answer chunk to the client.
var streamWriteTimeout = 2 * time.Minute

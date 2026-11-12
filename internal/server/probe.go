package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
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
)

// Probe is a micro benchmark per model that finds out what it can really do, instead of guessing from its
// name: a short streamed answer (works at all, first-token time, rough tokens/s, does it reason), a tool
// call, a picture to describe, and for embedding models one vector. Only this machine's models are probed
// (one after another: they share its GPU) — other machines' fleets probe theirs, and their results arrive
// with their readings. It runs only when asked (it makes the runtime load each model). Results are kept in probe.json;
// `fleet probe --apply` (menu: a) writes the found capabilities into the endpoints' caps.

// ProbeResult is what one probe found out about one endpoint.
type ProbeResult struct {
	At      time.Time         `json:"at"`
	Found   []string          `json:"found,omitempty"`   // capabilities that worked
	Missing []string          `json:"missing,omitempty"` // tested and did not work
	Why     map[string]string `json:"why,omitempty"`     // capability → what went wrong
	TTFTMS  int64             `json:"ttft_ms,omitempty"`
	TPS     float64           `json:"tps,omitempty"` // rough: a short answer
	Dim     int               `json:"dim,omitempty"`
	Err     string            `json:"err,omitempty"` // the model could not be reached at all
}

// merged is base with the missing capabilities taken out and the found ones added (in the usual order).
func (r *ProbeResult) merged(base []string) []string {
	have := map[string]bool{}
	for _, c := range base {
		have[c] = true
	}
	for _, c := range r.Missing {
		delete(have, c)
	}
	for _, c := range r.Found {
		have[c] = true
	}
	var out []string
	for _, c := range []string{"chat", "embed", "vision", "audio", "image_gen", "tools", "thinking"} {
		if have[c] {
			out = append(out, c)
		}
	}
	return out
}

// Differs reports whether the probe disagrees with the capabilities the endpoint has now.
func (r *ProbeResult) Differs(e *config.Endpoint) bool {
	if r == nil || r.Err != "" {
		return false
	}
	now := effCaps(e)
	return strings.Join(now, ",") != strings.Join(r.merged(now), ",")
}

// ProbeStatus is the progress of a running (or the last) probe.
type ProbeStatus struct {
	Running bool     `json:"running"`
	Done    int      `json:"done"`
	Total   int      `json:"total"`
	Current []string `json:"current,omitempty"` // endpoints being probed right now (one per machine)
}

type probeState struct {
	mu      sync.Mutex
	st      ProbeStatus
	results map[string]ProbeResult
	loaded  bool
}

func (p *probeState) load() {
	if p.loaded {
		return
	}
	p.loaded, p.results = true, map[string]ProbeResult{}
	if b, err := os.ReadFile(config.ProbePath()); err == nil {
		_ = json.Unmarshal(b, &p.results)
	}
}

func (p *probeState) save() error {
	b, err := json.MarshalIndent(p.results, "", "  ")
	if err != nil {
		return err
	}
	path := config.ProbePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ProbeResults returns the stored results by endpoint name.
func (s *Server) ProbeResults() map[string]ProbeResult {
	p := &s.probes
	p.mu.Lock()
	defer p.mu.Unlock()
	p.load()
	out := make(map[string]ProbeResult, len(p.results))
	for k, v := range p.results {
		out[k] = v
	}
	return out
}

func (s *Server) ProbeStatus() ProbeStatus {
	s.probes.mu.Lock()
	defer s.probes.mu.Unlock()
	st := s.probes.st
	st.Current = append([]string{}, st.Current...)
	return st
}

// Probe starts probing the named endpoints or nodes (none: every healthy endpoint). Returns at once;
// progress is in Status().Probe.
func (s *Server) Probe(names []string) error {
	cfg := s.Config()
	own := func(e config.Endpoint) bool { return e.Peer == "" && s.isSelfNode(e.Node) }
	var targets []config.Endpoint
	if len(names) == 0 {
		for _, e := range cfg.Endpoints {
			if own(e) && s.healthy(e.Name) {
				targets = append(targets, e)
			}
		}
	} else {
		list := NodeEndpoints(cfg, names)
		if len(list) == 0 {
			return fmt.Errorf("no such node or endpoint: %s", strings.Join(names, ", "))
		}
		for _, n := range list {
			if e := cfg.Endpoint(n); own(*e) {
				targets = append(targets, *e)
			}
		}
		if len(targets) == 0 {
			return fmt.Errorf("only this machine's models are probed here — the fleet on %s probes its own (its results show up on the Nodes screen)", strings.Join(names, ", "))
		}
	}
	if len(targets) == 0 {
		return fmt.Errorf("no healthy model on this machine to probe")
	}
	p := &s.probes
	p.mu.Lock()
	if p.st.Running {
		p.mu.Unlock()
		return fmt.Errorf("a probe is already running")
	}
	p.load()
	p.st = ProbeStatus{Running: true, Total: len(targets)}
	p.mu.Unlock()

	byNode := map[string][]config.Endpoint{}
	for _, e := range targets {
		byNode[e.Node] = append(byNode[e.Node], e)
	}
	go func() {
		var wg sync.WaitGroup
		for _, eps := range byNode {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for _, e := range eps {
					s.probeCurrent(e.Name, true)
					r := s.probeOne(s.ctx, &e)
					p.mu.Lock()
					p.results[e.Name] = r
					p.st.Done++
					p.mu.Unlock()
					s.probeCurrent(e.Name, false)
				}
			}()
		}
		wg.Wait()
		s.keepProbeSpeeds(targets)
		p.mu.Lock()
		if err := p.save(); err != nil {
			fmt.Fprintf(os.Stderr, "could not save probe results: %v\n", err)
		}
		p.st.Running, p.st.Current = false, nil
		p.mu.Unlock()
	}()
	return nil
}

func (s *Server) probeCurrent(name string, on bool) {
	p := &s.probes
	p.mu.Lock()
	defer p.mu.Unlock()
	var cur []string
	for _, c := range p.st.Current {
		if c != name {
			cur = append(cur, c)
		}
	}
	if on {
		cur = append(cur, name)
	}
	sort.Strings(cur)
	p.st.Current = cur
}

// keepProbeSpeeds stores the probe's tokens/s for endpoints that have no speed yet (a benchmark or real
// traffic gives better figures and is never overwritten).
func (s *Server) keepProbeSpeeds(targets []config.Endpoint) {
	res := s.ProbeResults()
	err := s.UpdateConfig(func(c *config.Config) bool {
		changed := false
		for i := range c.Endpoints {
			if r, ok := res[c.Endpoints[i].Name]; ok && c.Endpoints[i].TPS == 0 && r.TPS > 0 && c.Endpoints[i].Peer == "" {
				c.Endpoints[i].TPS, changed = r.TPS, true
			}
		}
		return changed
	})
	if err != nil {
		log.Printf("probe speeds not saved: %v", err)
	}
}

// ApplyProbe writes the probed capabilities into the endpoints' caps (names empty: every probed endpoint)
// and returns "endpoint: old → new" for each change. Models imported from a peer are left alone: their
// caps come from the peer.
func (s *Server) ApplyProbe(names []string) ([]string, error) {
	res := s.ProbeResults()
	c := s.Config()
	want := map[string]bool{}
	for _, n := range NodeEndpoints(c, names) {
		want[n] = true
	}
	eps := append([]config.Endpoint{}, c.Endpoints...)
	var changes []string
	for i := range eps {
		e := &eps[i]
		r, ok := res[e.Name]
		if !ok || r.Err != "" || e.Peer != "" || (len(names) > 0 && !want[e.Name]) || !r.Differs(e) {
			continue
		}
		old := effCaps(e)
		e.Caps = r.merged(old)
		changes = append(changes, fmt.Sprintf("%s: %s → %s", e.Name, strings.Join(old, " "), strings.Join(e.Caps, " ")))
	}
	if len(changes) == 0 {
		return nil, nil
	}
	c.Endpoints = eps
	return changes, s.SetConfig(c)
}

// ---- the tests ----

func (s *Server) probeOne(parent context.Context, e *config.Endpoint) ProbeResult {
	r := ProbeResult{At: time.Now(), Why: map[string]string{}}
	base, rel := s.baseURLUse(parent, e)
	defer rel()
	if base == "" {
		r.Err = "tunnel down"
		return r
	}
	l := s.lim("ep:" + e.Name) // its slots are respected; real requests go first
	ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
	defer cancel()
	if err := l.acquire(ctx, -1000); err != nil {
		r.Err = err.Error()
		return r
	}
	defer l.release()
	found := func(c string, ok bool, why string) {
		if ok {
			r.Found = append(r.Found, c)
		} else {
			r.Missing = append(r.Missing, c)
			if why != "" {
				r.Why[c] = why
			}
		}
	}
	if e.IsEmbed() {
		dim, err := s.probeEmbed(ctx, e, base)
		if err != nil {
			r.Err = err.Error()
			return r
		}
		r.Dim = dim
		found("embed", true, "")
		return r
	}
	ttft, tps, thinking, err := s.probeStream(ctx, e, base)
	if err != nil {
		r.Err = err.Error()
		return r
	}
	r.TTFTMS, r.TPS = ttft.Milliseconds(), float64(int(tps*10+0.5))/10
	found("chat", true, "")
	found("thinking", thinking, "no reasoning in the answer")
	ok, why := s.probeTools(ctx, e, base)
	found("tools", ok, why)
	ok, why = s.probeVision(ctx, e, base)
	found("vision", ok, why)
	if len(r.Why) == 0 {
		r.Why = nil
	}
	return r
}

// post sends a JSON request (the endpoint's default body fields merged in) and decodes the answer.
func (s *Server) post(ctx context.Context, e *config.Endpoint, url string, body map[string]any, out any) error {
	for k, v := range e.Body {
		if _, ok := body[k]; !ok {
			body[k] = v
		}
	}
	cx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(cx, "POST", url, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if k := e.APIKey(); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s", short(err.Error()))
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != 200 {
		return fmt.Errorf("http %d: %s", resp.StatusCode, short(errMessage(b)))
	}
	return json.Unmarshal(b, out)
}

type probeAnswer struct {
	Choices []struct {
		Message struct {
			Content   string `json:"content"`
			Reasoning string `json:"reasoning_content"`
			Thinking  string `json:"reasoning"`
			ToolCalls []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
}

// probeStream: a short streamed answer — does it work, how fast does it start and run, does it reason.
func (s *Server) probeStream(ctx context.Context, e *config.Endpoint, base string) (ttft time.Duration, tps float64, thinking bool, err error) {
	body := map[string]any{"model": e.Wire(), "stream": true, "temperature": 0, "max_tokens": 160,
		"stream_options": map[string]any{"include_usage": true},
		"messages":       []map[string]string{{"role": "user", "content": "In three sentences, explain why the sky is blue."}}}
	for k, v := range e.Body {
		if _, ok := body[k]; !ok {
			body[k] = v
		}
	}
	cx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(cx, "POST", base+"/chat/completions", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if k := e.APIKey(); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}
	t0 := time.Now()
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, 0, false, fmt.Errorf("%s", short(err.Error()))
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0, 0, false, fmt.Errorf("http %d", resp.StatusCode)
	}
	var first, last time.Time
	chunks, usage := 0, 0
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var ev struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					Reasoning string `json:"reasoning_content"`
					Thinking  string `json:"reasoning"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				Completion int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &ev) != nil {
			continue
		}
		if ev.Usage != nil && ev.Usage.Completion > 0 {
			usage = ev.Usage.Completion
		}
		if len(ev.Choices) == 0 {
			continue
		}
		d := ev.Choices[0].Delta
		if d.Reasoning != "" || d.Thinking != "" || strings.Contains(d.Content, "<think>") {
			thinking = true
		}
		if d.Content != "" || d.Reasoning != "" || d.Thinking != "" {
			now := time.Now()
			if chunks == 0 {
				first = now
			}
			last = now
			chunks++
		}
	}
	if chunks == 0 {
		return 0, 0, false, fmt.Errorf("empty answer")
	}
	ttft = first.Sub(t0)
	tokens := chunks
	if usage > chunks/2 && usage < chunks*4 {
		tokens = usage
	}
	if d := last.Sub(first).Seconds(); chunks >= 8 && d > 0 {
		tps = float64(tokens-1) / d
	}
	return ttft, tps, thinking, nil
}

// probeTools: offered one tool and asked something only it can answer, does the model call it?
func (s *Server) probeTools(ctx context.Context, e *config.Endpoint, base string) (bool, string) {
	body := map[string]any{"model": e.Wire(), "temperature": 0, "max_tokens": 600,
		"messages": []map[string]string{{"role": "user", "content": "What is the weather in Paris right now? Use the tool."}},
		"tools": []map[string]any{{"type": "function", "function": map[string]any{
			"name": "get_weather", "description": "Current weather for a city",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}, "required": []string{"city"}},
		}}}}
	var a probeAnswer
	if err := s.post(ctx, e, base+"/chat/completions", body, &a); err != nil {
		return false, err.Error()
	}
	if len(a.Choices) > 0 {
		for _, tc := range a.Choices[0].Message.ToolCalls {
			if tc.Function.Name == "get_weather" {
				return true, ""
			}
		}
		if c := a.Choices[0].Message.Content; strings.Contains(c, "get_weather") {
			return false, "wrote the call as text instead of a tool call"
		}
	}
	return false, "did not call the tool"
}

// redPNG is a 32×32 picture in one colour (made once).
var redPNG = func() string {
	img := image.NewRGBA(image.Rect(0, 0, 32, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			img.Set(x, y, color.RGBA{220, 20, 20, 255})
		}
	}
	var b bytes.Buffer
	png.Encode(&b, img)
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(b.Bytes())
}()

// probeVision: shown a red picture, does the model say red?
func (s *Server) probeVision(ctx context.Context, e *config.Endpoint, base string) (bool, string) {
	body := map[string]any{"model": e.Wire(), "temperature": 0, "max_tokens": 600,
		"messages": []map[string]any{{"role": "user", "content": []map[string]any{
			{"type": "text", "text": "What colour is this picture? Answer with one word."},
			{"type": "image_url", "image_url": map[string]string{"url": redPNG}},
		}}}}
	var a probeAnswer
	if err := s.post(ctx, e, base+"/chat/completions", body, &a); err != nil {
		return false, err.Error()
	}
	if len(a.Choices) == 0 {
		return false, "no answer"
	}
	m := a.Choices[0].Message
	ans := strings.ToLower(m.Content)
	if strings.Contains(ans, "red") || strings.Contains(ans, "kırmızı") {
		return true, ""
	}
	if ans == "" && (m.Reasoning != "" || m.Thinking != "") {
		return false, "only reasoned, no answer (token limit)"
	}
	return false, fmt.Sprintf("answered %q", trunc(strings.TrimSpace(m.Content), 30))
}

// probeEmbed: one vector, and its size.
func (s *Server) probeEmbed(ctx context.Context, e *config.Endpoint, base string) (int, error) {
	var d struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := s.post(ctx, e, base+"/embeddings", map[string]any{"model": e.Wire(), "input": "capability probe"}, &d); err != nil {
		return 0, err
	}
	if len(d.Data) == 0 || len(d.Data[0].Embedding) == 0 {
		return 0, fmt.Errorf("no vector in the answer")
	}
	return len(d.Data[0].Embedding), nil
}

func trunc(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// errMessage pulls the message out of an OpenAI-style error body ({"error":{"message":…}} or {"error":"…"}).
func errMessage(b []byte) string {
	var e struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && len(e.Error) > 0 {
		var m struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(e.Error, &m) == nil && m.Message != "" {
			return m.Message
		}
		var str string
		if json.Unmarshal(e.Error, &str) == nil && str != "" {
			return str
		}
	}
	return strings.TrimSpace(string(b))
}

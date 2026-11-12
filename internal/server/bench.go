package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
)

// BenchStatus is the progress of a running (or last) benchmark.
type BenchStatus struct {
	Running bool   `json:"running"`
	Done    int    `json:"done"`
	Total   int    `json:"total"`
	Current string `json:"current,omitempty"`
}

type benchResult struct {
	TTFT time.Duration
	At   time.Time
	Err  string
}

type benchState struct {
	mu      sync.Mutex
	st      BenchStatus
	results map[string]benchResult
}

// benchPrompt asks for varied prose on purpose: "count to 300" is so predictable that servers with
// speculative decoding / MTP draft almost every token right and report speeds far above real use.
const benchPrompt = "Write a detailed essay on how ocean currents shape climate, with concrete examples from at least five regions."

// Bench measures generation speed (tokens/s) and time-to-first-token of the given endpoints,
// one after another (so measurements don't disturb each other). The result is persisted to the
// endpoint's `tps` field. names empty → every healthy chat endpoint. Returns at once; poll Status().Bench.
func (s *Server) Bench(names []string) error {
	cfg := s.Config()
	var targets []config.Endpoint
	if len(names) == 0 {
		for _, e := range cfg.Endpoints {
			if !e.IsEmbed() && s.healthy(e.Name) {
				targets = append(targets, e)
			}
		}
	} else {
		for _, n := range names {
			e := cfg.Endpoint(n)
			if e == nil {
				return fmt.Errorf("no such endpoint: %s", n)
			}
			if e.IsEmbed() {
				return fmt.Errorf("%s is an embedding endpoint — nothing to benchmark", n)
			}
			targets = append(targets, *e)
		}
	}
	if len(targets) == 0 {
		return fmt.Errorf("no healthy chat endpoint to benchmark")
	}
	b := &s.bench
	b.mu.Lock()
	if b.st.Running {
		b.mu.Unlock()
		return fmt.Errorf("a benchmark is already running")
	}
	if b.results == nil {
		b.results = map[string]benchResult{}
	}
	b.st = BenchStatus{Running: true, Total: len(targets)}
	b.mu.Unlock()

	go func() {
		measured := map[string]float64{}
		for _, e := range targets {
			b.mu.Lock()
			b.st.Current = e.Name
			b.mu.Unlock()
			tps, ttft, err := s.measure(s.ctx, &e)
			b.mu.Lock()
			res := benchResult{TTFT: ttft, At: time.Now()}
			if err != nil {
				res.Err = err.Error()
			} else {
				measured[e.Name] = tps
			}
			b.results[e.Name] = res
			b.st.Done++
			b.mu.Unlock()
		}
		if len(measured) > 0 { // persist
			err := s.UpdateConfig(func(c *config.Config) bool {
				changed := false
				for i := range c.Endpoints {
					if v, ok := measured[c.Endpoints[i].Name]; ok {
						c.Endpoints[i].TPS, changed = float64(int(v*10+0.5))/10, true
					}
				}
				return changed
			})
			if err != nil {
				log.Printf("benchmark speeds not saved: %v", err)
			}
		}
		b.mu.Lock()
		b.st.Running, b.st.Current = false, ""
		b.mu.Unlock()
	}()
	return nil
}

func (s *Server) BenchStatus() BenchStatus {
	s.bench.mu.Lock()
	defer s.bench.mu.Unlock()
	return s.bench.st
}

func (s *Server) benchResult(name string) (benchResult, bool) {
	s.bench.mu.Lock()
	defer s.bench.mu.Unlock()
	r, ok := s.bench.results[name]
	return r, ok
}

// measure sends one streaming request and times the token chunks.
func (s *Server) measure(parent context.Context, e *config.Endpoint) (tps float64, ttft time.Duration, err error) {
	base, relTunnel := s.baseURLUse(parent, e)
	defer relTunnel()
	if base == "" {
		return 0, 0, fmt.Errorf("tunnel down")
	}
	l := s.lim("ep:" + e.Name) // respect the endpoint's slots; priority below every role
	ctx, cancel := context.WithTimeout(parent, 3*time.Minute)
	defer cancel()
	if err := l.acquire(ctx, -1000); err != nil {
		return 0, 0, err
	}
	defer l.release()

	body := map[string]any{
		"model": e.Wire(), "stream": true, "temperature": 0, "max_tokens": 320,
		"stream_options": map[string]any{"include_usage": true},
		"messages":       []map[string]string{{"role": "user", "content": benchPrompt}},
	}
	for k, v := range e.Body {
		if _, ok := body[k]; !ok {
			body[k] = v
		}
	}
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", base+"/chat/completions", strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	if k := e.APIKey(); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}
	t0 := time.Now()
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0, 0, fmt.Errorf("http %d", resp.StatusCode)
	}
	var first, last time.Time
	chunks, usageTokens := 0, 0
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
					Reasoning string `json:"reasoning_content"` // llama.cpp, vLLM, DeepSeek
					Thinking  string `json:"reasoning"`         // Ollama
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
			usageTokens = ev.Usage.Completion
		}
		if len(ev.Choices) > 0 && (ev.Choices[0].Delta.Content != "" || ev.Choices[0].Delta.Reasoning != "" || ev.Choices[0].Delta.Thinking != "") {
			now := time.Now()
			if chunks == 0 {
				first = now
			}
			last = now
			chunks++
		}
	}
	if chunks < 8 {
		return 0, 0, fmt.Errorf("too few tokens streamed (%d)", chunks)
	}
	tokens := chunks // one SSE chunk ≈ one token; the server's usage count is exact when present
	if usageTokens > chunks/2 && usageTokens < chunks*4 {
		tokens = usageTokens
	}
	dur := last.Sub(first).Seconds()
	if dur <= 0 {
		return 0, 0, fmt.Errorf("could not time the stream")
	}
	return float64(tokens-1) / dur, first.Sub(t0), nil
}

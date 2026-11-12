package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
)

// Warm models: Ollama loads a model on its first request and unloads it after a while; the first answer of
// a cold model waits for the load (seconds, for a big model a minute). The health loop asks each Ollama
// which models it has loaded (GET /api/ps) and, every few minutes, how big each one is (GET /api/tags).
// Runtimes that keep their model loaded (llama.cpp, vLLM, LiteLLM …) do not answer /api/ps and count as
// warm. Models behind a peer carry what the peer knows.

const tagsEvery = 5 * time.Minute

// ModelInfo is what the runtime says about a model.
type ModelInfo struct {
	Loaded *bool   `json:"loaded,omitempty"` // nil: the runtime does not say (it keeps its models loaded)
	Size   int64   `json:"size,omitempty"`   // bytes of weights
	Params string  `json:"params,omitempty"` // "7.5B"
	Active float64 `json:"active,omitempty"` // billions of parameters one token uses (mixture of experts), 0: all
}

type runtimeBase struct {
	ollama  bool
	checked time.Time // last /api/tags
	tags    map[string]ModelInfo
	loaded  map[string]bool
	psOK    bool
}

type warmStore struct {
	mu    sync.Mutex
	bases map[string]*runtimeBase // base URL (without /v1) → what it said
	byEp  map[string]ModelInfo    // endpoint → its model's info
}

func (w *warmStore) get(name string) (ModelInfo, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	m, ok := w.byEp[name]
	return m, ok
}

func (w *warmStore) set(name string, m ModelInfo) {
	w.mu.Lock()
	if w.byEp == nil {
		w.byEp = map[string]ModelInfo{}
	}
	w.byEp[name] = m
	w.mu.Unlock()
}

// cold reports whether the runtime said the endpoint's model is not loaded.
func (s *Server) cold(name string) bool {
	m, ok := s.warm.get(name)
	return ok && m.Loaded != nil && !*m.Loaded
}

// normModel drops Ollama's default tag, so "llama3" and "llama3:latest" are the same model.
func normModel(n string) string { return strings.TrimSuffix(strings.ToLower(n), ":latest") }

var activeRe = regexp.MustCompile(`(?i)(?:^|[-_:])a(\d+(?:\.\d+)?)b(?:$|[-_:.])`)

// activeParams reads the active parameter count from a mixture-of-experts name ("74b-a9b" → 9).
func activeParams(model string) float64 {
	if m := activeRe.FindStringSubmatch(model); m != nil {
		v, _ := strconv.ParseFloat(m[1], 64)
		return v
	}
	return 0
}

// pollRuntimes refreshes what the local runtimes say (loaded models, sizes). Idle tunnels are not woken.
func (s *Server) pollRuntimes(ctx context.Context, cfg *config.Config) {
	byBase := map[string][]config.Endpoint{}
	for _, e := range cfg.Endpoints {
		if e.Peer != "" || (e.URL == "" && s.tunnels.IsIdle(e.SSH)) {
			continue
		}
		b := s.BaseURL(&e)
		if b == "" {
			continue
		}
		b = strings.TrimSuffix(b, "/v1")
		byBase[b] = append(byBase[b], e)
	}
	var wg sync.WaitGroup
	for base, eps := range byBase {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.pollBase(ctx, base, eps)
		}()
	}
	wg.Wait()
}

func (s *Server) getJSON(ctx context.Context, url string, key string, v any) error {
	cx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(cx, "GET", url, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return errStatus(resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

type errStatus int

func (e errStatus) Error() string { return "http " + strconv.Itoa(int(e)) }

type ollamaList struct {
	Models []struct {
		Name    string `json:"name"`
		Model   string `json:"model"`
		Size    int64  `json:"size"`
		Details struct {
			ParameterSize string `json:"parameter_size"`
		} `json:"details"`
	} `json:"models"`
}

func (s *Server) pollBase(ctx context.Context, base string, eps []config.Endpoint) {
	s.warm.mu.Lock()
	if s.warm.bases == nil {
		s.warm.bases = map[string]*runtimeBase{}
	}
	rb := s.warm.bases[base]
	if rb == nil {
		rb = &runtimeBase{}
		s.warm.bases[base] = rb
	}
	due := time.Since(rb.checked) > tagsEvery
	isOllama := rb.ollama
	s.warm.mu.Unlock()
	key := eps[0].APIKey()
	if due {
		var tl ollamaList
		err := s.getJSON(ctx, base+"/api/tags", key, &tl)
		tags := map[string]ModelInfo{}
		for _, m := range tl.Models {
			tags[normModel(m.Name)] = ModelInfo{Size: m.Size, Params: m.Details.ParameterSize, Active: activeParams(m.Name)}
		}
		s.warm.mu.Lock()
		rb.checked = time.Now()
		var st errStatus
		switch {
		case err == nil:
			rb.ollama, rb.tags = true, tags
		case errors.As(err, &st): // it answers, but not the Ollama API
			rb.ollama, rb.tags = false, nil
		} // a timeout or a dropped connection keeps what was known; /api/ps below tells whether it is still there
		isOllama = rb.ollama
		s.warm.mu.Unlock()
	}
	if !isOllama {
		for _, e := range eps { // no word on loading: the model counts as warm, not as cold from an old answer
			if m, ok := s.warm.get(e.Name); ok && m.Loaded != nil {
				m.Loaded = nil
				s.warm.set(e.Name, m)
			}
		}
		return
	}
	var ps ollamaList
	err := s.getJSON(ctx, base+"/api/ps", key, &ps)
	loaded := map[string]bool{}
	for _, m := range ps.Models {
		loaded[normModel(m.Name)] = true
		loaded[normModel(m.Model)] = true
	}
	s.warm.mu.Lock()
	rb.loaded, rb.psOK = loaded, err == nil
	tags := rb.tags
	s.warm.mu.Unlock()
	for _, e := range eps {
		n := normModel(e.Wire())
		mi := tags[n]
		if err == nil {
			l := loaded[n]
			mi.Loaded = &l
		}
		s.warm.set(e.Name, mi)
	}
}

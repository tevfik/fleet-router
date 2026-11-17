//go:build e2e

package e2e

// Router behaviour that a real model server cannot be made to show on demand (errors, stalls, a client that leaves):
// the built binary is run against small fake OpenAI-compatible servers started by the test. Only FLEET_E2E_BIN is
// needed.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- a fake model server ------------------------------------------------------------------------------------------

type upstream struct {
	*httptest.Server
	mu     sync.Mutex
	hits   int
	auth   []string
	bodies []map[string]any
}

// newUpstream serves /v1/models (the health probe) and hands every chat request to chat after recording it.
func newUpstream(t *testing.T, chat func(w http.ResponseWriter, r *http.Request, body map[string]any)) *upstream {
	t.Helper()
	u := &upstream{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"object": "list", "data": [{"id": "fake"}]}`))
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		u.mu.Lock()
		u.hits++
		u.auth = append(u.auth, r.Header.Get("Authorization"))
		u.bodies = append(u.bodies, body)
		u.mu.Unlock()
		chat(w, r, body)
	})
	u.Server = httptest.NewServer(mux)
	t.Cleanup(u.Close)
	return u
}

func (u *upstream) count() int { u.mu.Lock(); defer u.mu.Unlock(); return u.hits }

func (u *upstream) last() (auth string, body map[string]any) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.hits == 0 {
		return "", nil
	}
	return u.auth[u.hits-1], u.bodies[u.hits-1]
}

func answer(text string) func(http.ResponseWriter, *http.Request, map[string]any) {
	return func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id": "x", "object": "chat.completion", "choices": [{"index": 0, "finish_reason": "stop",
			"message": {"role": "assistant", "content": %q}}], "usage": {"prompt_tokens": 5, "completion_tokens": 2}}`, text)
	}
}

func failWith(code int) func(http.ResponseWriter, *http.Request, map[string]any) {
	return func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		http.Error(w, fmt.Sprintf(`{"error": {"message": "fake %d"}}`, code), code)
	}
}

// sseChunk writes one OpenAI stream chunk and flushes it.
func sseChunk(w http.ResponseWriter, content string) {
	fmt.Fprintf(w, "data: {\"id\": \"x\", \"object\": \"chat.completion.chunk\", \"choices\": [{\"index\": 0, \"delta\": {\"content\": %q}}]}\n\n", content)
	w.(http.Flusher).Flush()
}

func sseStart(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	w.(http.Flusher).Flush()
}

func sseDone(w http.ResponseWriter) {
	fmt.Fprint(w, "data: {\"id\": \"x\", \"object\": \"chat.completion.chunk\", \"choices\": [{\"index\": 0, \"delta\": {}, \"finish_reason\": \"stop\"}], \"usage\": {\"prompt_tokens\": 5, \"completion_tokens\": 3}}\n\ndata: [DONE]\n\n")
	w.(http.Flusher).Flush()
}

// ---- the request log ------------------------------------------------------------------------------------------

type logEntry struct {
	Model    string `json:"model"`
	Role     string `json:"role"`
	Endpoint string `json:"endpoint"`
	Status   int    `json:"status"`
	Attempts int    `json:"attempts"`
	QueueMS  int64  `json:"queue_ms"`
	Err      string `json:"err"`
	Via      string `json:"via"`
}

// lastLog returns the newest request log entry (the log is written when the handler returns, so it is polled).
func (f *fleet) lastLog(t *testing.T, match func(logEntry) bool) logEntry {
	t.Helper()
	var found logEntry
	eventually(t, 5*time.Second, func() error {
		var l struct{ Entries []logEntry }
		if err := json.Unmarshal(f.get("/fleet/log?n=50"), &l); err != nil {
			return err
		}
		for i := len(l.Entries) - 1; i >= 0; i-- {
			if match(l.Entries[i]) {
				found = l.Entries[i]
				return nil
			}
		}
		return fmt.Errorf("no matching log entry in %+v", l.Entries)
	})
	return found
}

// ---- failover -------------------------------------------------------------------------------------------------

func TestRouterFailover(t *testing.T) {
	e500 := newUpstream(t, failWith(500))
	e429 := newUpstream(t, failWith(429))
	bad400 := newUpstream(t, failWith(400))
	good := newUpstream(t, answer("hi"))
	f := startFleet(t, "", fmt.Sprintf(`endpoints:
  - {name: e500, node: e2e, url: "%s/v1", model: fake}
  - {name: e429, node: e2e, url: "%s/v1", model: fake}
  - {name: refused, node: e2e, url: "http://127.0.0.1:%d/v1", model: fake}
  - {name: bad400, node: e2e, url: "%s/v1", model: fake}
  - {name: good, node: e2e, url: "%s/v1", model: fake}
roles:
  chain: {preferred: e500, fallback: [e429, refused, good]}
  clientfault: {preferred: bad400, fallback: [good]}
  hopeless: {preferred: e500, fallback: [refused]}
`, e500.URL, e429.URL, freePort(t), bad400.URL, good.URL))

	t.Run("5xx, 429 and a refused connection fall through to the next candidate", func(t *testing.T) {
		if got := f.chat("chain"); got != "good" {
			t.Fatalf("answered by %q, want good", got)
		}
		if e500.count() != 1 || e429.count() != 1 {
			t.Fatalf("hits e500=%d e429=%d, want 1 each", e500.count(), e429.count())
		}
		if e := f.lastLog(t, func(e logEntry) bool { return e.Role == "chain" }); e.Attempts < 3 || e.Status != 200 {
			// 4 when the refused one is tried in order; 3 when the health probe found it down first and moved it last
			t.Fatalf("log: %+v, want 3-4 attempts and status 200", e)
		}
	})

	t.Run("a failed endpoint is tried last afterwards", func(t *testing.T) {
		before500, before429 := e500.count(), e429.count()
		if got := f.chat("chain"); got != "good" {
			t.Fatalf("answered by %q, want good", got)
		}
		if e := f.lastLog(t, func(e logEntry) bool { return e.Role == "chain" }); e.Attempts != 1 {
			t.Fatalf("second request: %d attempts (%+v), want the healthy endpoint first", e.Attempts, e)
		}
		if e500.count() != before500 || e429.count() != before429 {
			t.Fatal("penalized endpoints were asked again before the healthy one")
		}
	})

	t.Run("a 4xx is the client's problem: passed through, no failover", func(t *testing.T) {
		before := good.count()
		code, _, b := call(t, "POST", f.base+"/v1/chat/completions", chatBody("clientfault", ""))
		if code != 400 || !strings.Contains(string(b), "fake 400") {
			t.Fatalf("got %d %s, want the upstream's 400", code, b)
		}
		if good.count() != before {
			t.Fatal("a 400 failed over to the fallback")
		}
	})

	t.Run("all candidates down: 502 naming each failure", func(t *testing.T) {
		code, _, b := call(t, "POST", f.base+"/v1/chat/completions", chatBody("hopeless", ""))
		if code != 502 || !strings.Contains(string(b), "all candidates failed") {
			t.Fatalf("got %d %s, want 502 all candidates failed", code, b)
		}
	})

	t.Run("bad requests", func(t *testing.T) {
		if code, _, b := call(t, "POST", f.base+"/v1/chat/completions", chatBody("no-such-model", "")); code != 404 {
			t.Fatalf("unknown model: %d %s, want 404", code, b)
		}
		if code, _, b := call(t, "POST", f.base+"/v1/chat/completions", `{"model": `); code != 400 {
			t.Fatalf("broken JSON: %d %s, want 400", code, b)
		}
	})
}

// ---- what reaches the model server ------------------------------------------------------------------------------

func TestRouterRewritesTheRequest(t *testing.T) {
	keyed := newUpstream(t, answer("hi"))
	open := newUpstream(t, answer("hi"))
	f := startFleet(t, "", fmt.Sprintf(`endpoints:
  - {name: keyed, node: e2e, url: "%s/v1", model: wire-model, key_env: FLEET_E2E_UPSTREAM_KEY, body: {temperature: 0.1, think: false}}
  - {name: open, node: e2e, url: "%s/v1", model: other-model}
roles:
  writer: {preferred: keyed}
  reader: {preferred: open}
`, keyed.URL, open.URL), "FLEET_E2E_UPSTREAM_KEY=upstream-secret")

	code, _, b := call(t, "POST", f.base+"/v1/chat/completions",
		`{"model": "writer", "temperature": 0.7, "messages": [{"role": "user", "content": "hi"}]}`,
		"Authorization", "Bearer caller-secret")
	if code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	auth, body := keyed.last()
	if auth != "Bearer upstream-secret" {
		t.Errorf("upstream Authorization %q, want the endpoint's key", auth)
	}
	if body["model"] != "wire-model" {
		t.Errorf("upstream model %v, want the endpoint's model (not the role name)", body["model"])
	}
	if body["temperature"] != 0.7 || body["think"] != false {
		t.Errorf("upstream body %v: the client's temperature must win, the endpoint's think default must be added", body)
	}

	call(t, "POST", f.base+"/v1/chat/completions", chatBody("reader", ""), "Authorization", "Bearer caller-secret")
	if auth, _ := open.last(); auth != "" {
		t.Errorf("the caller's Authorization reached a keyless upstream: %q", auth)
	}
}

// ---- stalls ---------------------------------------------------------------------------------------------------

func TestRouterStall(t *testing.T) {
	// headers at once, then nothing: llama-server does this while a worker link hangs
	silent := newUpstream(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		sseStart(w)
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	})
	// one token, then nothing
	cut := newUpstream(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		sseStart(w)
		sseChunk(w, "Hel")
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	})
	good := newUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		sseStart(w)
		sseChunk(w, "Hello")
		sseDone(w)
	})
	f := startFleet(t, "", fmt.Sprintf(`endpoints:
  - {name: silent, node: e2e, url: "%s/v1", model: fake, stall: 1s}
  - {name: cut, node: e2e, url: "%s/v1", model: fake, stall: 1s}
  - {name: good, node: e2e, url: "%s/v1", model: fake}
roles:
  quiet: {preferred: silent, fallback: [good]}
  broken: {preferred: cut, fallback: [good]}
`, silent.URL, cut.URL, good.URL))

	t.Run("no first token: fails over before answering", func(t *testing.T) {
		start := time.Now()
		h, b := f.ok("/v1/chat/completions", chatBody("quiet", `, "stream": true`))
		if ep := h.Get("X-Fleet-Endpoint"); ep != "good" || !strings.Contains(string(b), "[DONE]") {
			t.Fatalf("answered by %q:\n%s", ep, b)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("took %v: the stall limit (3 × 1s before the first token) did not apply", d)
		}
	})

	t.Run("cut mid-answer: the client keeps what came, no second answer", func(t *testing.T) {
		before := good.count()
		code, h, b := call(t, "POST", f.base+"/v1/chat/completions", chatBody("broken", `, "stream": true`))
		if code != 200 || h.Get("X-Fleet-Endpoint") != "cut" {
			t.Fatalf("%d from %q, want 200 from cut (headers were already sent)", code, h.Get("X-Fleet-Endpoint"))
		}
		if !strings.Contains(string(b), "Hel") || strings.Contains(string(b), "[DONE]") {
			t.Fatalf("body:\n%s\nwant the first token and no [DONE]", b)
		}
		if good.count() != before {
			t.Fatal("a half-sent answer was followed by a second one from the fallback")
		}
		if e := f.lastLog(t, func(e logEntry) bool { return e.Role == "broken" }); e.Status != 504 {
			t.Fatalf("log %+v, want status 504 (stalled mid-answer)", e)
		}
	})
}

// ---- a client that leaves ------------------------------------------------------------------------------------

func TestRouterClientGoesAway(t *testing.T) {
	gone := make(chan struct{}, 1)
	slow := newUpstream(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		if msgs, _ := body["messages"].([]any); len(msgs) > 0 && !strings.Contains(fmt.Sprint(msgs[0]), "slow") {
			answer("quick")(w, r, body)
			return
		}
		sseStart(w)
		for i := 0; ; i++ {
			select {
			case <-r.Context().Done():
				gone <- struct{}{}
				return
			case <-time.After(100 * time.Millisecond):
				sseChunk(w, fmt.Sprintf("t%d ", i))
			}
			if i > 200 {
				return
			}
		}
	})
	f := startFleet(t, "", fmt.Sprintf(`endpoints:
  - {name: single, node: e2e, url: "%s/v1", model: fake, slots: 1}
roles:
  one: {preferred: single}
`, slow.URL))

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", f.base+"/v1/chat/completions", strings.NewReader(
		`{"model": "one", "stream": true, "messages": [{"role": "user", "content": "slow please"}]}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
		t.Fatalf("no first chunk: %v", err)
	}
	cancel()
	resp.Body.Close()

	select {
	case <-gone:
	case <-time.After(5 * time.Second):
		t.Fatal("the model server kept generating after the client left (the request was not cancelled upstream)")
	}
	if e := f.lastLog(t, func(e logEntry) bool { return e.Role == "one" }); e.Status != 499 {
		t.Fatalf("log %+v, want 499", e)
	}
	// the single slot is free again: the next request does not wait
	start := time.Now()
	if got := f.chat("one"); got != "single" {
		t.Fatalf("answered by %q", got)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the next request waited %v: the slot of the abandoned request was not released", d)
	}
}

// ---- the Ollama API on top of an OpenAI server -------------------------------------------------------------------

func TestRouterOllamaTranslation(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		if s, _ := body["stream"].(bool); !s {
			answer("Hello!")(w, r, body)
			return
		}
		sseStart(w)
		sseChunk(w, "Hel")
		time.Sleep(1500 * time.Millisecond) // the client must see the first piece before this ends
		sseChunk(w, "lo!")
		sseDone(w)
	})
	f := startFleet(t, "", fmt.Sprintf(`endpoints:
  - {name: oai, node: e2e, url: "%s/v1", model: fake}
roles:
  talker: {preferred: oai}
`, up.URL))

	t.Run("tags list the role", func(t *testing.T) {
		if b := f.get("/api/tags"); !strings.Contains(string(b), `"talker"`) {
			t.Fatalf("/api/tags:\n%s", b)
		}
	})

	t.Run("chat streams NDJSON as it comes", func(t *testing.T) {
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "POST", f.base+"/api/chat", strings.NewReader(
			`{"model": "talker", "options": {"num_predict": 8}, "messages": [{"role": "user", "content": "hi"}]}`))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "ndjson") {
			t.Errorf("Content-Type %q, want application/x-ndjson", ct)
		}
		var text strings.Builder
		var firstAt time.Duration
		lines, done := 0, false
		for sc := bufio.NewScanner(resp.Body); sc.Scan(); {
			var m struct {
				Message struct{ Content string }
				Done    bool
			}
			if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
				t.Fatalf("not NDJSON: %q", sc.Text())
			}
			if lines == 0 {
				firstAt = time.Since(start)
			}
			lines++
			text.WriteString(m.Message.Content)
			done = done || m.Done
		}
		if text.String() != "Hello!" || !done {
			t.Fatalf("text %q done %v over %d lines", text.String(), done, lines)
		}
		if firstAt > time.Second {
			t.Fatalf("first line after %v: the answer was buffered instead of streamed", firstAt)
		}
		if _, body := up.last(); body["max_tokens"] != float64(8) {
			t.Errorf("num_predict was not passed as max_tokens: %v", body)
		}
	})

	t.Run("generate without streaming is one object", func(t *testing.T) {
		_, b := f.ok("/api/generate", `{"model": "talker", "prompt": "hi", "stream": false}`)
		var m struct {
			Response string
			Done     bool
		}
		if err := json.Unmarshal(b, &m); err != nil || m.Response != "Hello!" || !m.Done {
			t.Fatalf("%v %s", err, b)
		}
		if _, body := up.last(); fmt.Sprint(body["messages"]) != "[map[content:hi role:user]]" {
			t.Errorf("prompt not turned into a user message: %v", body["messages"])
		}
		if e := f.lastLog(t, func(e logEntry) bool { return e.Via == "ollama" }); e.Endpoint != "oai" {
			t.Errorf("log %+v", e)
		}
	})
}

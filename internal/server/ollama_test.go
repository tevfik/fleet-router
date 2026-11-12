package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
)

// fakeBackend answers /chat/completions with an SSE stream and /embeddings with a fixed vector.
func fakeBackend() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		json.NewDecoder(r.Body).Decode(&in)
		if strings.HasSuffix(r.URL.Path, "/embeddings") {
			fmt.Fprint(w, `{"data":[{"embedding":[0.5,0.25]}],"usage":{"prompt_tokens":3}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(s string) { fmt.Fprint(w, "data: "+s+"\n\n"); w.(http.Flusher).Flush() }
		if _, ok := in["tools"]; ok {
			send(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"weather","arguments":"{\"city\":"}}]}}]}`)
			send(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Rome\"}"}}]},"finish_reason":"tool_calls"}]}`)
		} else {
			send(`{"choices":[{"delta":{"reasoning_content":"hmm"}}]}`)
			send(`{"choices":[{"delta":{"content":"Hel"}}]}`)
			send(`{"choices":[{"delta":{"content":"lo"},"finish_reason":"stop"}]}`)
		}
		send(`{"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":2}}`)
		send(`[DONE]`)
	}))
}

func ollamaServer(t *testing.T) (*httptest.Server, func()) {
	be := fakeBackend()
	s := &Server{
		cfg: &config.Config{
			Endpoints: []config.Endpoint{{Name: "m1", Node: "n", URL: be.URL, Model: "real"}, {Name: "emb", Node: "n", URL: be.URL, Model: "e", Caps: []string{"embed"}}},
			Roles:     map[string]config.Role{"scout": {Preferred: "m1"}},
		},
		state: map[string]*EpState{}, lims: map[string]*limiter{}, client: http.DefaultClient,
	}
	front := httptest.NewServer(s.Handler())
	return front, func() { front.Close(); be.Close() }
}

func post(t *testing.T, url, body string) *http.Response {
	r, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestOllamaChatStream(t *testing.T) {
	front, done := ollamaServer(t)
	defer done()
	r := post(t, front.URL+"/api/chat", `{"model":"scout:latest","messages":[{"role":"user","content":"hi"}]}`)
	defer r.Body.Close()
	if ct := r.Header.Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("content type %q", ct)
	}
	var text, think string
	var last map[string]any
	sc := bufio.NewScanner(r.Body)
	for sc.Scan() {
		var v map[string]any
		if err := json.Unmarshal(sc.Bytes(), &v); err != nil {
			t.Fatalf("not NDJSON: %q", sc.Text())
		}
		m := v["message"].(map[string]any)
		text += m["content"].(string)
		if th, ok := m["thinking"].(string); ok {
			think += th
		}
		last = v
	}
	if text != "Hello" || think != "hmm" {
		t.Fatalf("text %q thinking %q", text, think)
	}
	if last["done"] != true || last["done_reason"] != "stop" || last["eval_count"].(float64) != 2 || last["prompt_eval_count"].(float64) != 7 {
		t.Fatalf("final object %v", last)
	}
}

func TestOllamaGenerateNonStream(t *testing.T) {
	front, done := ollamaServer(t)
	defer done()
	r := post(t, front.URL+"/api/generate", `{"model":"m1","prompt":"hi","system":"s","stream":false}`)
	var v map[string]any
	json.NewDecoder(r.Body).Decode(&v)
	if v["response"] != "Hello" || v["done"] != true {
		t.Fatalf("%v", v)
	}
}

func TestOllamaToolCalls(t *testing.T) {
	front, done := ollamaServer(t)
	defer done()
	r := post(t, front.URL+"/api/chat", `{"model":"scout","stream":false,"messages":[{"role":"user","content":"weather?"}],"tools":[{"type":"function","function":{"name":"weather"}}]}`)
	var v map[string]any
	json.NewDecoder(r.Body).Decode(&v)
	tc := v["message"].(map[string]any)["tool_calls"].([]any)
	f := tc[0].(map[string]any)["function"].(map[string]any)
	if f["name"] != "weather" || f["arguments"].(map[string]any)["city"] != "Rome" {
		t.Fatalf("%v", v)
	}
}

func TestOllamaTagsEmbedAndErrors(t *testing.T) {
	front, done := ollamaServer(t)
	defer done()
	r, _ := http.Get(front.URL + "/api/tags")
	var tags struct{ Models []struct{ Name string } }
	json.NewDecoder(r.Body).Decode(&tags)
	if len(tags.Models) != 3 { // role scout + endpoints m1, emb
		t.Fatalf("tags %v", tags)
	}
	r = post(t, front.URL+"/api/embed", `{"model":"emb","input":["a"]}`)
	var e struct{ Embeddings [][]float64 }
	json.NewDecoder(r.Body).Decode(&e)
	if len(e.Embeddings) != 1 || e.Embeddings[0][0] != 0.5 {
		t.Fatalf("embed %v", e)
	}
	r = post(t, front.URL+"/api/embeddings", `{"model":"emb","prompt":"a"}`)
	var le struct{ Embedding []float64 }
	json.NewDecoder(r.Body).Decode(&le)
	if len(le.Embedding) != 2 {
		t.Fatalf("legacy embeddings %v", le)
	}
	r = post(t, front.URL+"/api/chat", `{"model":"nope","messages":[]}`)
	var er map[string]any
	json.NewDecoder(r.Body).Decode(&er)
	if r.StatusCode != 404 || er["error"] == nil {
		t.Fatalf("unknown model: %d %v", r.StatusCode, er)
	}
	r = post(t, front.URL+"/api/chat", `{"model":"emb","messages":[{"role":"user","content":"x"}]}`) // an embedding model cannot chat
	if r.StatusCode != 422 {
		t.Fatalf("chat on an embedding-only model should be refused, got %d", r.StatusCode)
	}
	r, _ = http.Get(front.URL + "/")
	if b := make([]byte, 6); true {
		r.Body.Read(b)
		if string(b) != "Ollama" {
			t.Fatalf("root %q", b)
		}
	}
}

func TestRequestLog(t *testing.T) {
	be := fakeBackend()
	defer be.Close()
	s := &Server{
		cfg: &config.Config{
			Endpoints: []config.Endpoint{{Name: "dead", Node: "n", URL: "http://127.0.0.1:1", Model: "x"}, {Name: "m1", Node: "n", URL: be.URL, Model: "real"}},
			Roles:     map[string]config.Role{"scout": {Preferred: "dead", Fallback: []string{"m1"}}},
		},
		state: map[string]*EpState{}, lims: map[string]*limiter{}, client: http.DefaultClient,
	}
	front := httptest.NewServer(s.Handler())
	defer front.Close()
	post(t, front.URL+"/api/chat", `{"model":"scout","stream":false,"messages":[{"role":"user","content":"hi"}]}`).Body.Close()
	post(t, front.URL+"/v1/chat/completions", `{"model":"missing","messages":[]}`).Body.Close()
	log := s.reqs.last(10)
	if len(log) != 2 {
		t.Fatalf("%d entries", len(log))
	}
	a, b := log[0], log[1]
	if a.Via != "ollama" || a.Role != "scout" || a.Endpoint != "m1" || a.Attempts != 2 || a.Status != 200 || a.In != 7 || a.Out != 2 {
		t.Fatalf("failover entry %+v", a)
	}
	if b.Status != 404 || b.Model != "missing" || b.Err == "" {
		t.Fatalf("error entry %+v", b)
	}
}

func TestOllamaOutEdgeCases(t *testing.T) {
	rec := httptest.NewRecorder()
	o := &ollamaOut{w: rec, hdr: http.Header{}, kind: "chat", model: "m", start: time.Now()}
	// tool calls numbered from 1 and 3, and a final line with no newline
	o.Write([]byte(`data: {"choices":[{"delta":{"tool_calls":[{"index":3,"function":{"name":"b","arguments":"{}"}},{"index":1,"function":{"name":"a","arguments":"{}"}}]}}]}` + "\n"))
	o.Write([]byte(`data: {"choices":[{"delta":{"content":"tail"},"finish_reason":"stop"}]}`))
	o.finishUp()
	var v map[string]any
	json.Unmarshal(rec.Body.Bytes(), &v)
	m := v["message"].(map[string]any)
	if m["content"] != "tail" {
		t.Fatalf("unterminated last line lost: %v", v)
	}
	tc := m["tool_calls"].([]any)
	if len(tc) != 2 || tc[0].(map[string]any)["function"].(map[string]any)["name"] != "a" {
		t.Fatalf("tool calls %v", tc)
	}
}

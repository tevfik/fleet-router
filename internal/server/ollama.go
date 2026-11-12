package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Ollama-compatible API (/api/*). Clients written for Ollama — Open WebUI, Continue, the ollama
// libraries — can point at fleet unchanged; each call is translated to the OpenAI-style router path, so roles,
// failover, queues and metrics all apply. Model names may be roles ("scout"), endpoint names or raw ids; a
// ":latest" suffix is ignored.

const ollamaVersion = "0.9.0" // the Ollama API level this layer imitates

func (s *Server) ollamaRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("Ollama is running (fleet)")) })
	mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, map[string]any{"version": ollamaVersion}) })
	mux.HandleFunc("GET /api/tags", s.v1Auth(s.ollamaTags))
	mux.HandleFunc("GET /api/ps", s.v1Auth(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, map[string]any{"models": []any{}}) }))
	mux.HandleFunc("POST /api/show", s.v1Auth(s.ollamaShow))
	mux.HandleFunc("POST /api/chat", s.v1Auth(func(w http.ResponseWriter, r *http.Request) { s.ollamaGen(w, r, "chat") }))
	mux.HandleFunc("POST /api/generate", s.v1Auth(func(w http.ResponseWriter, r *http.Request) { s.ollamaGen(w, r, "generate") }))
	mux.HandleFunc("POST /api/embed", s.v1Auth(func(w http.ResponseWriter, r *http.Request) { s.ollamaEmbed(w, r, false) }))
	mux.HandleFunc("POST /api/embeddings", s.v1Auth(func(w http.ResponseWriter, r *http.Request) { s.ollamaEmbed(w, r, true) }))
}

func ollamaErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{"error": msg})
}

func (s *Server) ollamaTags(w http.ResponseWriter, _ *http.Request) {
	cfg := s.Config()
	now := time.Now().UTC().Format(time.RFC3339)
	type det struct {
		Family string `json:"family"`
		Format string `json:"format"`
	}
	type m struct {
		Name       string `json:"name"`
		Model      string `json:"model"`
		ModifiedAt string `json:"modified_at"`
		Size       int64  `json:"size"`
		Digest     string `json:"digest"`
		Details    det    `json:"details"`
	}
	var out []m
	for _, r := range cfg.RoleNames() {
		out = append(out, m{r, r, now, 0, digestOf("role:" + r), det{"fleet-role", "role"}})
	}
	for _, e := range cfg.Endpoints {
		out = append(out, m{e.Name, e.Name, now, 0, digestOf("endpoint:" + e.Name), det{e.Node, "endpoint"}})
	}
	writeJSON(w, map[string]any{"models": out})
}

// ollamaCaps maps fleet capabilities to Ollama's capability names.
func ollamaCaps(caps []string, embed bool) []string {
	if embed {
		return []string{"embedding"}
	}
	out := []string{"completion"}
	for _, c := range caps {
		switch c {
		case "vision":
			out = append(out, "vision")
		case "tools":
			out = append(out, "tools")
		case "thinking":
			out = append(out, "thinking")
		}
	}
	return out
}

func (s *Server) ollamaShow(w http.ResponseWriter, r *http.Request) {
	var in struct{ Model, Name string }
	json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in)
	name := firstNonEmpty(in.Model, in.Name)
	cands, role, _ := s.route(stripLatest(s, name))
	if len(cands) == 0 {
		ollamaErr(w, 404, fmt.Sprintf("model %q not found", name))
		return
	}
	e := cands[0]
	info := map[string]any{"general.architecture": "fleet"}
	if e.Ctx > 0 {
		info["fleet.context_length"] = e.Ctx
	}
	if e.Dim > 0 {
		info["fleet.embedding_length"] = e.Dim
	}
	writeJSON(w, map[string]any{
		"details":      map[string]any{"family": e.Node, "format": "fleet", "parent_model": role},
		"model_info":   info,
		"capabilities": ollamaCaps(e.Caps, e.IsEmbed()),
	})
}

// digestOf gives each listed model a stable sha256-style id (the ollama CLI slices the first 12 characters).
func digestOf(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func firstNonEmpty(v ...string) string {
	for _, x := range v {
		if x != "" {
			return x
		}
	}
	return ""
}

// stripLatest drops a ":latest" suffix when the name is only known without it.
func stripLatest(s *Server, name string) string {
	if t := strings.TrimSuffix(name, ":latest"); t != name {
		if c, _, _ := s.route(name); len(c) == 0 {
			return t
		}
	}
	return name
}

// ---------- request translation ----------

func toFloat(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

// openAIFromOllama builds an OpenAI chat body from an Ollama /api/chat or /api/generate body.
func openAIFromOllama(kind string, in map[string]any, model string) map[string]any {
	out := map[string]any{"model": model, "stream": true, "stream_options": map[string]any{"include_usage": true}}
	var msgs []any
	if kind == "chat" {
		var pending []string // ids of tool calls the next tool message answers
		n := 0
		raw, _ := in["messages"].([]any)
		for _, x := range raw {
			m, _ := x.(map[string]any)
			if m == nil {
				continue
			}
			role, _ := m["role"].(string)
			text, _ := m["content"].(string)
			om := map[string]any{"role": role}
			if imgs, _ := m["images"].([]any); len(imgs) > 0 {
				parts := []any{map[string]any{"type": "text", "text": text}}
				for _, im := range imgs {
					if b, ok := im.(string); ok {
						parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/jpeg;base64," + b}})
					}
				}
				om["content"] = parts
			} else {
				om["content"] = text
			}
			if tcs, _ := m["tool_calls"].([]any); len(tcs) > 0 {
				var calls []any
				for _, tc := range tcs {
					f, _ := tc.(map[string]any)["function"].(map[string]any)
					args, _ := json.Marshal(f["arguments"])
					n++
					id := fmt.Sprintf("call_%d", n)
					pending = append(pending, id)
					calls = append(calls, map[string]any{"id": id, "type": "function", "function": map[string]any{"name": f["name"], "arguments": string(args)}})
				}
				om["tool_calls"] = calls
			}
			if role == "tool" && len(pending) > 0 {
				om["tool_call_id"], pending = pending[0], pending[1:]
			}
			msgs = append(msgs, om)
		}
		if t, ok := in["tools"]; ok {
			out["tools"] = t
		}
	} else {
		if sys, _ := in["system"].(string); sys != "" {
			msgs = append(msgs, map[string]any{"role": "system", "content": sys})
		}
		p, _ := in["prompt"].(string)
		um := map[string]any{"role": "user", "content": p}
		if imgs, _ := in["images"].([]any); len(imgs) > 0 {
			parts := []any{map[string]any{"type": "text", "text": p}}
			for _, im := range imgs {
				if b, ok := im.(string); ok {
					parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/jpeg;base64," + b}})
				}
			}
			um["content"] = parts
		}
		msgs = append(msgs, um)
	}
	out["messages"] = msgs
	if o, _ := in["options"].(map[string]any); o != nil {
		for from, to := range map[string]string{"temperature": "temperature", "top_p": "top_p", "seed": "seed", "stop": "stop",
			"num_predict": "max_tokens", "presence_penalty": "presence_penalty", "frequency_penalty": "frequency_penalty"} {
			if v, ok := o[from]; ok {
				if f, isNum := toFloat(v); from == "num_predict" && isNum && f <= 0 {
					continue // -1 / 0 mean "no limit" in Ollama
				}
				out[to] = v
			}
		}
	}
	switch f := in["format"].(type) {
	case string:
		if f == "json" {
			out["response_format"] = map[string]any{"type": "json_object"}
		}
	case map[string]any:
		out["response_format"] = map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "output", "schema": f}}
	}
	return out
}

// ---------- response translation ----------

// ollamaOut adapts the router's SSE reply into Ollama's NDJSON (or one JSON object when stream=false).
type ollamaOut struct {
	w       http.ResponseWriter
	fl      http.Flusher
	hdr     http.Header
	kind    string
	model   string
	stream  bool
	start   time.Time
	code    int
	errBuf  bytes.Buffer
	partial []byte
	text    strings.Builder
	think   strings.Builder
	calls   map[int]*toolAcc
	finish  string
	usage   struct{ in, out int }
	first   time.Time
	started bool
}

type toolAcc struct{ name, args string }

func (o *ollamaOut) Header() http.Header { return o.hdr }
func (o *ollamaOut) WriteHeader(c int)   { o.code = c }
func (o *ollamaOut) Flush()              {}

func (o *ollamaOut) Write(p []byte) (int, error) {
	if o.code != 0 && o.code != 200 {
		o.errBuf.Write(p)
		return len(p), nil
	}
	o.partial = append(o.partial, p...)
	for {
		i := bytes.IndexByte(o.partial, '\n')
		if i < 0 {
			break
		}
		o.line(strings.TrimSpace(string(o.partial[:i])))
		o.partial = o.partial[i+1:]
	}
	return len(p), nil
}

func (o *ollamaOut) begin() {
	if o.started {
		return
	}
	o.started = true
	if ep := o.hdr.Get("X-Fleet-Endpoint"); ep != "" {
		o.w.Header().Set("X-Fleet-Endpoint", ep)
	}
	if o.stream {
		o.w.Header().Set("Content-Type", "application/x-ndjson")
	} else {
		o.w.Header().Set("Content-Type", "application/json")
	}
}

func (o *ollamaOut) emit(v map[string]any) {
	v["model"], v["created_at"] = o.model, time.Now().UTC().Format(time.RFC3339Nano)
	json.NewEncoder(o.w).Encode(v)
	if o.fl != nil {
		o.fl.Flush()
	}
}

func (o *ollamaOut) piece(content, thinking string, done bool, extra map[string]any) map[string]any {
	v := map[string]any{"done": done}
	if o.kind == "chat" {
		m := map[string]any{"role": "assistant", "content": content}
		if thinking != "" {
			m["thinking"] = thinking
		}
		for k, x := range extra {
			m[k] = x
		}
		v["message"] = m
	} else {
		v["response"] = content
		if thinking != "" {
			v["thinking"] = thinking
		}
	}
	return v
}

func (o *ollamaOut) line(l string) {
	if !strings.HasPrefix(l, "data:") {
		return
	}
	l = strings.TrimSpace(strings.TrimPrefix(l, "data:"))
	if l == "" || l == "[DONE]" {
		return
	}
	var ev struct {
		Choices []struct {
			Delta struct {
				Content   string `json:"content"`
				Reasoning string `json:"reasoning_content"`
				Thinking  string `json:"reasoning"`
				ToolCalls []struct {
					Index    int `json:"index"`
					Function struct {
						Name string `json:"name"`
						Args string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			Finish string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			Prompt     int `json:"prompt_tokens"`
			Completion int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal([]byte(l), &ev) != nil {
		return
	}
	if ev.Usage != nil {
		o.usage.in, o.usage.out = ev.Usage.Prompt, ev.Usage.Completion
	}
	if len(ev.Choices) == 0 {
		return
	}
	c := ev.Choices[0]
	if c.Finish != "" {
		o.finish = c.Finish
	}
	for _, tc := range c.Delta.ToolCalls {
		if o.calls == nil {
			o.calls = map[int]*toolAcc{}
		}
		a := o.calls[tc.Index]
		if a == nil {
			a = &toolAcc{}
			o.calls[tc.Index] = a
		}
		if tc.Function.Name != "" {
			a.name = tc.Function.Name
		}
		a.args += tc.Function.Args
	}
	think := c.Delta.Reasoning + c.Delta.Thinking
	if c.Delta.Content == "" && think == "" {
		return
	}
	if o.first.IsZero() {
		o.first = time.Now()
	}
	if o.stream {
		o.begin()
		o.emit(o.piece(c.Delta.Content, think, false, nil))
		return
	}
	o.text.WriteString(c.Delta.Content)
	o.think.WriteString(think)
}

func (o *ollamaOut) toolCalls() []any {
	var out []any
	idx := make([]int, 0, len(o.calls))
	for i := range o.calls { // servers number calls from 0 or 1 and may skip; keep their order
		idx = append(idx, i)
	}
	sort.Ints(idx)
	for _, i := range idx {
		a := o.calls[i]
		var args any = map[string]any{}
		json.Unmarshal([]byte(a.args), &args)
		out = append(out, map[string]any{"function": map[string]any{"name": a.name, "arguments": args}})
	}
	return out
}

// finishUp writes the closing object once the router has finished (or reports an error that came before any output).
func (o *ollamaOut) finishUp() {
	if len(o.partial) > 0 { // a last line without a trailing newline
		o.line(strings.TrimSpace(string(o.partial)))
		o.partial = nil
	}
	if o.code != 0 && o.code != 200 {
		msg := strings.TrimSpace(o.errBuf.String())
		var e struct {
			Error any `json:"error"`
		}
		if json.Unmarshal(o.errBuf.Bytes(), &e) == nil {
			switch x := e.Error.(type) {
			case string:
				msg = x
			case map[string]any:
				if m, ok := x["message"].(string); ok {
					msg = m
				}
			}
		}
		ollamaErr(o.w, o.code, msg)
		return
	}
	o.begin()
	reason := "stop"
	if o.finish == "length" {
		reason = "length"
	}
	total := time.Since(o.start).Nanoseconds()
	stats := map[string]any{"done_reason": reason, "total_duration": total, "load_duration": 0,
		"prompt_eval_count": o.usage.in, "eval_count": o.usage.out}
	if !o.first.IsZero() {
		stats["prompt_eval_duration"] = o.first.Sub(o.start).Nanoseconds()
		stats["eval_duration"] = time.Since(o.first).Nanoseconds()
	}
	tcs := o.toolCalls()
	if o.stream {
		if len(tcs) > 0 {
			o.emit(o.piece("", "", false, map[string]any{"tool_calls": tcs}))
		}
		v := o.piece("", "", true, nil)
		for k, x := range stats {
			v[k] = x
		}
		o.emit(v)
		return
	}
	var extra map[string]any
	if len(tcs) > 0 {
		extra = map[string]any{"tool_calls": tcs}
	}
	v := o.piece(o.text.String(), o.think.String(), true, extra)
	for k, x := range stats {
		v[k] = x
	}
	o.emit(v)
}

// callRouter runs an OpenAI-style request through proxyV1 in-process.
func (s *Server) callRouter(w http.ResponseWriter, r *http.Request, path string, body map[string]any) {
	raw, _ := json.Marshal(body)
	rr, _ := http.NewRequestWithContext(r.Context(), "POST", path, bytes.NewReader(raw))
	rr.RemoteAddr = r.RemoteAddr
	rr.Header.Set("Content-Type", "application/json")
	rr.Header.Set("X-Fleet-Via", "ollama")
	rr.Header.Set("User-Agent", r.Header.Get("User-Agent"))
	if c := r.Header.Get("X-Fleet-Client"); c != "" {
		rr.Header.Set("X-Fleet-Client", c)
	}
	// The caller's key: proxyV1 applies the key's allowed models/roles and QoS; without it /api/* sidestepped them.
	if a := r.Header.Get("Authorization"); a != "" {
		rr.Header.Set("Authorization", a)
	}
	s.proxyV1(w, rr)
}

func (s *Server) ollamaGen(w http.ResponseWriter, r *http.Request, kind string) {
	var in map[string]any
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<20)).Decode(&in); err != nil {
		ollamaErr(w, 400, "invalid JSON: "+err.Error())
		return
	}
	name, _ := in["model"].(string)
	model := stripLatest(s, name)
	if c, _, _ := s.route(model); len(c) == 0 {
		msg := fmt.Sprintf("model %q not found, try a role or endpoint name (GET /api/tags)", name)
		e := LogEntry{Time: time.Now(), Path: "/api/" + kind, Via: "ollama", Model: name, Status: 404, Err: msg, Remote: !isLoopback(r), Client: clientLabel(r)}
		s.reqs.add(e)
		s.live.addUsage(e, "")
		ollamaErr(w, 404, msg)
		return
	}
	stream := true
	if v, ok := in["stream"].(bool); ok {
		stream = v
	}
	fl, _ := w.(http.Flusher)
	o := &ollamaOut{w: w, fl: fl, hdr: http.Header{}, kind: kind, model: name, stream: stream, start: time.Now()}
	s.callRouter(o, r, "/v1/chat/completions", openAIFromOllama(kind, in, model))
	o.finishUp()
}

func (s *Server) ollamaEmbed(w http.ResponseWriter, r *http.Request, legacy bool) {
	var in map[string]any
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<20)).Decode(&in); err != nil {
		ollamaErr(w, 400, "invalid JSON: "+err.Error())
		return
	}
	name, _ := in["model"].(string)
	input := in["input"]
	if legacy {
		input = in["prompt"]
	}
	rec := &bufResp{hdr: http.Header{}}
	s.callRouter(rec, r, "/v1/embeddings", map[string]any{"model": stripLatest(s, name), "input": input})
	if rec.code != 0 && rec.code != 200 {
		var e struct {
			Error struct{ Message string } `json:"error"`
		}
		json.Unmarshal(rec.buf.Bytes(), &e)
		ollamaErr(w, rec.code, firstNonEmpty(e.Error.Message, strings.TrimSpace(rec.buf.String())))
		return
	}
	var d struct {
		Data  []struct{ Embedding []float64 } `json:"data"`
		Usage struct{ Prompt int }            `json:"usage"`
	}
	if json.Unmarshal(rec.buf.Bytes(), &d) != nil || len(d.Data) == 0 {
		ollamaErr(w, 502, "unexpected embeddings reply from the backend")
		return
	}
	if legacy {
		writeJSON(w, map[string]any{"embedding": d.Data[0].Embedding})
		return
	}
	embs := make([][]float64, len(d.Data))
	for i, x := range d.Data {
		embs[i] = x.Embedding
	}
	writeJSON(w, map[string]any{"model": name, "embeddings": embs})
}

// bufResp is a minimal in-memory ResponseWriter.
type bufResp struct {
	hdr  http.Header
	code int
	buf  bytes.Buffer
}

func (b *bufResp) Header() http.Header         { return b.hdr }
func (b *bufResp) WriteHeader(c int)           { b.code = c }
func (b *bufResp) Write(p []byte) (int, error) { return b.buf.Write(p) }

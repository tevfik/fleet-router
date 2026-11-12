package mcpsrv

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"github.com/tevfik/fleet-router/internal/discover"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/client"
	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/server"
)

// ---- fakes: an OpenAI upstream and an in-process daemon ----

func newUpstream(t *testing.T) *httptest.Server {
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/models"):
			fmt.Fprint(w, `{"object":"list","data":[{"id":"tiny"},{"id":"slow"},{"id":"thinker"},{"id":"long"}]}`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/chat/completions"):
			var in map[string]any
			json.NewDecoder(r.Body).Decode(&in)
			if st, _ := in["stream"].(bool); st {
				w.Header().Set("Content-Type", "text/event-stream")
				send := func(s string) { fmt.Fprint(w, "data: "+s+"\n\n"); w.(http.Flusher).Flush() }
				for i := 0; i < 20; i++ {
					send(`{"choices":[{"delta":{"content":"tok "}}]}`)
					time.Sleep(time.Millisecond)
				}
				send(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
				send(`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":20}}`)
				send(`[DONE]`)
				return
			}
			content := "pong"
			switch in["model"] {
			case "slow":
				select { // until the caller gives up
				case <-r.Context().Done():
					return
				case <-time.After(8 * time.Second):
				}
			case "thinker":
				fmt.Fprint(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"","reasoning":"hmm"},"finish_reason":"stop"}]}`)
				return
			case "long":
				content = strings.Repeat("a", 5000) + strings.Repeat("z", 5000)
			}
			b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": content}}}, "usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 1}})
			w.Header().Set("Content-Type", "application/json")
			w.Write(b)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(be.Close)
	return be
}

func startDaemon(t *testing.T) (addr string, be *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("FLEET_CONFIG", filepath.Join(dir, "config.yaml"))
	t.Setenv("FLEET_STATS", filepath.Join(dir, "state", "stats.json"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "xdg-config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "xdg-state"))
	t.Setenv("HOME", dir)
	be = newUpstream(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr = l.Addr().String()
	l.Close()
	off := false
	cfg := &config.Config{Name: "alpha", Listen: addr, MDNS: &off, AutoUpdate: &off,
		Endpoints: []config.Endpoint{
			{Name: "loc-tiny", Node: "box", URL: be.URL + "/v1", Model: "tiny"},
			{Name: "loc-slow", Node: "box", URL: be.URL + "/v1", Model: "slow"},
			{Name: "loc-thinker", Node: "box", URL: be.URL + "/v1", Model: "thinker"},
			{Name: "loc-long", Node: "box", URL: be.URL + "/v1", Model: "long"},
		},
		Roles: map[string]config.Role{"scout": {Preferred: "loc-tiny"}},
	}
	if err := cfg.Save(config.Path()); err != nil {
		t.Fatal(err)
	}
	s, err := server.New(config.Path())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(6 * time.Second):
		}
	})
	cl := client.New(addr)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st, err := cl.Status(); err == nil {
			ok := 0
			for _, e := range st.Endpoints {
				if e.OK {
					ok++
				}
			}
			if ok == 4 {
				return addr, be
			}
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal("daemon or endpoints did not come up")
	return
}

func newSrv(addr string) *Server {
	cl := client.New(addr)
	return &Server{cl: cl, jobs: newJobs(cl)}
}

// ---- JSON-RPC over stdio (Run) ----

// rpc runs Run with the given request lines on stdin and returns the decoded replies in order.
func rpc(t *testing.T, addr string, lines ...string) []map[string]any {
	t.Helper()
	inR, inW, _ := os.Pipe()
	outR, outW, _ := os.Pipe()
	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = inR, outW
	defer func() { os.Stdin, os.Stdout = oldIn, oldOut }()
	got := make(chan []byte)
	go func() { b, _ := io.ReadAll(outR); got <- b }()
	go func() {
		for _, l := range lines {
			io.WriteString(inW, l+"\n")
		}
		inW.Close()
	}()
	if err := Run(addr); err != nil {
		t.Fatalf("Run: %v", err)
	}
	outW.Close()
	os.Stdin, os.Stdout = oldIn, oldOut
	var replies []map[string]any
	sc := bufio.NewScanner(strings.NewReader(string(<-got)))
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("reply is not JSON: %v\n%s", err, sc.Text())
		}
		replies = append(replies, m)
	}
	return replies
}

func callText(t *testing.T, reply map[string]any) (string, bool) {
	t.Helper()
	res, ok := reply["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", reply)
	}
	content, _ := res["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content: %v", res)
	}
	c := content[0].(map[string]any)
	if c["type"] != "text" {
		t.Fatalf("content type: %v", c)
	}
	isErr, _ := res["isError"].(bool)
	return c["text"].(string), isErr
}

func TestRPCProtocol(t *testing.T) {
	addr, _ := startDaemon(t)
	replies := rpc(t, addr,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`, // a notification: no reply
		`this is not json`, // ignored
		`{"jsonrpc":"2.0","id":"two","method":"ping"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"fleet_status","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"no_such_tool","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"fleet_ask","arguments":{"role":"scout"}}}`,
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"fleet_ask","arguments":{"role":"scout","prompt":"hi","colour":"red"}}}`,
		`{"jsonrpc":"2.0","id":8,"method":"resources/list"}`,
		`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"fleet_ask","arguments":{"role":"scout","prompt":"hi"}}}`,
	)
	if len(replies) != 9 {
		t.Fatalf("want 9 replies (notification and garbage get none), got %d: %v", len(replies), replies)
	}
	ids := []any{1.0, "two", 3.0, 4.0, 5.0, 6.0, 7.0, 8.0, 9.0}
	for i, r := range replies {
		if r["jsonrpc"] != "2.0" || r["id"] != ids[i] {
			t.Fatalf("reply %d: id %v, want %v (%v)", i, r["id"], ids[i], r)
		}
	}
	init := replies[0]["result"].(map[string]any)
	if init["protocolVersion"] != "2024-11-05" || init["serverInfo"].(map[string]any)["name"] != "fleet" {
		t.Fatalf("initialize: %v", init)
	}
	if _, ok := init["capabilities"].(map[string]any)["tools"]; !ok {
		t.Fatalf("initialize must announce tools: %v", init)
	}
	if res, ok := replies[1]["result"].(map[string]any); !ok || len(res) != 0 {
		t.Fatalf("ping: %v", replies[1])
	}
	list := replies[2]["result"].(map[string]any)["tools"].([]any)
	if len(list) != len(tools) {
		t.Fatalf("tools/list: %d tools, want %d", len(list), len(tools))
	}
	checkSchemas(t, list)

	text, isErr := callText(t, replies[3])
	if isErr || !strings.Contains(text, "ROLLER") || !strings.Contains(text, "scout") || !strings.Contains(text, "loc-tiny") {
		t.Fatalf("fleet_status: %v %q", isErr, text)
	}
	text, isErr = callText(t, replies[4])
	if !isErr || text != `error: unknown tool "no_such_tool"` {
		t.Fatalf("unknown tool: %v %q", isErr, text)
	}
	text, isErr = callText(t, replies[5])
	if !isErr || !strings.Contains(text, `missing required argument "prompt" for fleet_ask`) {
		t.Fatalf("missing arg: %v %q", isErr, text)
	}
	text, isErr = callText(t, replies[6])
	if !isErr || !strings.Contains(text, `unknown argument "colour" for fleet_ask (known: max_tokens, prompt, role, system, timeout_s)`) {
		t.Fatalf("unknown arg: %v %q", isErr, text)
	}
	if e, ok := replies[7]["error"].(map[string]any); !ok || e["code"] != -32601.0 || e["message"] != "method not found: resources/list" {
		t.Fatalf("unknown method: %v", replies[7])
	}
	text, isErr = callText(t, replies[8])
	if isErr || text != "[scout → loc-tiny]\npong" {
		t.Fatalf("fleet_ask: %v %q", isErr, text)
	}
}

// checkSchemas: every tool's inputSchema is a valid JSON Schema object that Claude Code accepts.
func checkSchemas(t *testing.T, list []any) {
	t.Helper()
	seen := map[string]bool{}
	valid := map[string]bool{"string": true, "integer": true, "number": true, "boolean": true, "array": true, "object": true}
	var checkProp func(tool, name string, p map[string]any)
	checkProp = func(tool, name string, p map[string]any) {
		typ, _ := p["type"].(string)
		if !valid[typ] {
			t.Errorf("%s.%s: type %v", tool, name, p["type"])
		}
		if typ == "array" {
			items, ok := p["items"].(map[string]any)
			if !ok {
				t.Errorf("%s.%s: array without items", tool, name)
			} else {
				checkProp(tool, name+"[]", items)
			}
		}
		if e, ok := p["enum"]; ok {
			vals, ok := e.([]any)
			if !ok || len(vals) == 0 {
				t.Errorf("%s.%s: enum %v", tool, name, e)
			}
			for _, v := range vals {
				if _, ok := v.(string); !ok && typ == "string" {
					t.Errorf("%s.%s: enum value %v is not a string", tool, name, v)
				}
			}
		}
	}
	for _, x := range list {
		tl := x.(map[string]any)
		name, _ := tl["name"].(string)
		if name == "" || seen[name] {
			t.Errorf("tool name %q empty or duplicate", name)
		}
		seen[name] = true
		if d, _ := tl["description"].(string); len(d) < 10 {
			t.Errorf("%s: description %q", name, d)
		}
		sch, ok := tl["inputSchema"].(map[string]any)
		if !ok || sch["type"] != "object" {
			t.Errorf("%s: inputSchema %v", name, tl["inputSchema"])
			continue
		}
		props, ok := sch["properties"].(map[string]any)
		if !ok {
			t.Errorf("%s: properties %v", name, sch["properties"])
			continue
		}
		if sch["additionalProperties"] != false {
			t.Errorf("%s: additionalProperties should be false", name)
		}
		req, ok := sch["required"].([]any)
		if !ok {
			t.Errorf("%s: required %v", name, sch["required"])
		}
		for _, r := range req {
			if _, ok := props[r.(string)]; !ok {
				t.Errorf("%s: required %q is not a property", name, r)
			}
		}
		for pn, p := range props {
			pm, ok := p.(map[string]any)
			if !ok {
				t.Errorf("%s.%s: %v", name, pn, p)
				continue
			}
			checkProp(name, pn, pm)
		}
	}
}

// ---- tools (through Server.call) ----

func TestToolsAgainstDaemon(t *testing.T) {
	addr, be := startDaemon(t)
	s := newSrv(addr)
	call := func(name string, a obj) (string, error) {
		t.Helper()
		// arguments arrive as decoded JSON: numbers are float64, lists []any
		b, _ := json.Marshal(a)
		var back obj
		json.Unmarshal(b, &back)
		return s.call(name, back)
	}

	out, err := call("fleet_ask", obj{"role": "scout", "prompt": "hi", "system": "terse", "max_tokens": 10, "timeout_s": 20})
	if err != nil || out != "[scout → loc-tiny]\npong" {
		t.Fatalf("fleet_ask: %q %v", out, err)
	}
	_, err = call("fleet_ask", obj{"role": "loc-thinker", "prompt": "hi"})
	if err == nil || !strings.Contains(err.Error(), "the model only produced reasoning") {
		t.Fatalf("reasoning-only answer: %v", err)
	}
	_, err = call("fleet_ask", obj{"role": "loc-slow", "prompt": "hi", "timeout_s": 1})
	if err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("timeout_s: %v", err)
	}
	out, err = call("fleet_ask", obj{"role": "loc-long", "prompt": "hi"})
	if err != nil || !strings.Contains(out, "[4000 characters clipped]") || len(out) > maxOut+200 ||
		!strings.HasSuffix(out, "zzz") || !strings.Contains(out, "[loc-long → loc-long]\naaa") {
		t.Fatalf("long answers are clipped (head + tail): len %d %v", len(out), err)
	}

	out, err = call("fleet_status", obj{})
	if err != nil || !strings.Contains(out, "ENDPOINT'LER") || !strings.Contains(out, "✓ loc-tiny") {
		t.Fatalf("fleet_status: %q %v", out, err)
	}

	out, err = call("fleet_role_set", obj{"role": "coder", "preferred": "loc-long", "fallback": []string{"loc-tiny"}})
	if err != nil || out != "✓ role updated (applied hot)" {
		t.Fatalf("fleet_role_set: %q %v", out, err)
	}
	cfg, _ := s.cl.Config()
	if r := cfg.Roles["coder"]; r.Preferred != "loc-long" || len(r.Fallback) != 1 || r.Fallback[0] != "loc-tiny" {
		t.Fatalf("role not set: %+v", r)
	}
	if out, _ = call("fleet_status", obj{}); !strings.Contains(out, "coder") {
		t.Fatalf("status after role set: %q", out)
	}
	if _, err = call("fleet_role_set", obj{"role": "bad", "preferred": "ghost"}); err == nil || !strings.Contains(err.Error(), `unknown endpoint "ghost"`) {
		t.Fatalf("bad role: %v", err)
	}

	port := be.URL[strings.LastIndex(be.URL, ":")+1:]
	out, err = call("fleet_discover", obj{"ports": port, "ssh": []string{}, "include_embed": false})
	// the fake's models sit under node "box" in the config; discovery names this machine "alpha" (its
	// fleet name), and MakePlan only treats same node+port+model as known, so all four are offered again
	if err != nil || !strings.Contains(out, "to add/added: 4") || !strings.Contains(out, "+ alpha-tiny  (tiny)") || !strings.Contains(out, "set apply=true to register") {
		t.Fatalf("fleet_discover: %q %v", out, err)
	}

	out, err = call("fleet_leaderboard", obj{})
	if err != nil || out != "nothing measured yet — call with bench=true" {
		t.Fatalf("leaderboard before bench: %q %v", out, err)
	}
	out, err = call("fleet_leaderboard", obj{"bench": true})
	if err != nil || !strings.Contains(out, " 1. ") || !strings.Contains(out, "tok/s") {
		t.Fatalf("leaderboard after bench: %q %v", out, err)
	}
}

func TestJobs(t *testing.T) {
	addr, _ := startDaemon(t)
	s := newSrv(addr)
	call := func(name string, a obj) string {
		t.Helper()
		b, _ := json.Marshal(a)
		var back obj
		json.Unmarshal(b, &back)
		out, err := s.call(name, back)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return out
	}
	jobID := func(out string) string {
		t.Helper()
		if !strings.HasPrefix(out, "job_id=") {
			t.Fatalf("task_start: %q", out)
		}
		return strings.TrimPrefix(out, "job_id=")
	}

	if out := call("task_list", obj{}); out != "no jobs" {
		t.Fatalf("empty list: %q", out)
	}
	if out := call("task_result", obj{"job_id": "nope"}); out != "job not found: nope" {
		t.Fatalf("unknown job: %q", out)
	}
	if out := call("task_cancel", obj{"job_id": "nope"}); out != "job not found: nope" {
		t.Fatalf("cancel unknown job: %q", out)
	}

	// chat mode: done
	id := jobID(call("task_start", obj{"agent": "scout", "mode": "chat", "prompt": "hi", "max_tokens": 5}))
	out := call("task_result", obj{"job_id": id, "wait_s": 10})
	if !strings.HasPrefix(out, "["+id+"] scout · done · loc-tiny · ") || !strings.HasSuffix(out, "\npong") {
		t.Fatalf("chat job: %q", out)
	}
	if out := call("task_cancel", obj{"job_id": id}); out != "already finished: done" {
		t.Fatalf("cancel a finished job: %q", out)
	}

	// running → cancelled
	slow := jobID(call("task_start", obj{"agent": "loc-slow", "mode": "chat", "prompt": "hi"}))
	if out := call("task_result", obj{"job_id": slow}); !strings.Contains(out, "loc-slow · running") || !strings.Contains(out, "(not finished yet)") {
		t.Fatalf("running job: %q", out)
	}
	if out := call("task_cancel", obj{"job_id": slow}); out != "cancelled: "+slow {
		t.Fatalf("cancel: %q", out)
	}
	if out := call("task_result", obj{"job_id": slow, "wait_s": 5}); !strings.Contains(out, "loc-slow · cancelled") {
		t.Fatalf("cancelled job: %q", out)
	}

	// timeout
	to := jobID(call("task_start", obj{"agent": "loc-slow", "mode": "chat", "prompt": "hi", "timeout_s": 1}))
	if out := call("task_result", obj{"job_id": to, "wait_s": 5}); !strings.Contains(out, "loc-slow · timeout") {
		t.Fatalf("timed-out job: %q", out)
	}

	// chat failure (unknown role)
	bad := jobID(call("task_start", obj{"agent": "ghost", "mode": "chat", "prompt": "hi"}))
	if out := call("task_result", obj{"job_id": bad, "wait_s": 5}); !strings.Contains(out, "ghost · failed") || !strings.Contains(out, "[error] http 404") {
		t.Fatalf("failed job: %q", out)
	}

	// opencode mode (the default) without opencode installed: fails cleanly, nothing is run
	t.Setenv("PATH", t.TempDir())
	oc := jobID(call("task_start", obj{"agent": "coder", "prompt": "write a poem", "cwd": t.TempDir(), "model": "github.com/tevfik/fleet-router/scout"}))
	if out := call("task_result", obj{"job_id": oc, "wait_s": 5}); !strings.Contains(out, "coder · failed") || !strings.Contains(out, "[error]") ||
		!strings.Contains(out, "executable file not found") {
		t.Fatalf("opencode job without opencode: %q", out)
	}

	list := call("task_list", obj{})
	lines := strings.Split(strings.TrimSpace(list), "\n")
	if len(lines) != 5 {
		t.Fatalf("task_list has %d lines:\n%s", len(lines), list)
	}
	for _, want := range []string{"scout      chat      done", "loc-slow   chat      cancelled", "loc-slow   chat      timeout", "ghost      chat      failed", "coder      opencode  failed"} {
		if !strings.Contains(list, want) {
			t.Fatalf("task_list lacks %q:\n%s", want, list)
		}
	}
	// long prompts are cut to 60 characters in the list
	jobID(call("task_start", obj{"agent": "scout", "mode": "chat", "prompt": strings.Repeat("p", 100)}))
	if list := call("task_list", obj{}); !strings.Contains(list, `"`+strings.Repeat("p", 60)+`"`) {
		t.Fatalf("long prompt in list:\n%s", list)
	}
}

func TestClipAndArgHelpers(t *testing.T) {
	if clip("short") != "short" {
		t.Fatal("clip short")
	}
	long := strings.Repeat("a", maxOut) + "TAIL"
	c := clip(long)
	if !strings.HasSuffix(c, "TAIL") || !strings.HasPrefix(c, strings.Repeat("a", maxOut/4)) || !strings.Contains(c, "[4 characters clipped]") {
		t.Fatalf("clip: %q…", c[:80])
	}
	a := obj{"s": "x", "i": 3.0, "b": true, "l": []any{"p", 1.0, "q"}, "wrong": 7}
	if sArg(a, "s") != "x" || sArg(a, "i") != "" || iArg(a, "i", 9) != 3 || iArg(a, "wrong", 9) != 9 || iArg(a, "none", 9) != 9 ||
		!bArg(a, "b") || bArg(a, "s") || strings.Join(lArg(a, "l"), ",") != "p,q" || lArg(a, "s") != nil {
		t.Fatal("arg helpers")
	}
	if got := leaderboardText(nil); got != "nothing measured yet — call with bench=true" {
		t.Fatalf("empty leaderboard: %q", got)
	}
	eps := []server.EpStatus{{Endpoint: config.Endpoint{Name: "b", Node: "n2", TPS: 30}}, {Endpoint: config.Endpoint{Name: "a", Node: "n1", TPS: 50}}, {Endpoint: config.Endpoint{Name: "c"}}}
	if got := leaderboardText(eps); got != " 1. a                                 50.0 tok/s  n1\n 2. b                                 30.0 tok/s  n2\n" {
		t.Fatalf("leaderboard: %q", got)
	}
	// an empty required string counts as missing
	if err := checkArgs("fleet_ask", obj{"role": "", "prompt": "x"}); err == nil {
		t.Fatal("empty required argument accepted")
	}
}

func TestToolsWithoutDaemon(t *testing.T) {
	s := newSrv("127.0.0.1:1")
	for _, tc := range []struct {
		name string
		a    obj
	}{
		{"fleet_status", obj{}},
		{"fleet_leaderboard", obj{}},
		{"fleet_leaderboard", obj{"bench": true}},
		{"fleet_role_set", obj{"role": "r", "preferred": "e"}},
		{"fleet_discover", obj{}},
		{"fleet_ask", obj{"role": "r", "prompt": "p"}},
	} {
		if _, err := s.call(tc.name, tc.a); err == nil {
			t.Errorf("%s without a daemon: no error", tc.name)
		}
	}
}

func TestMain(m *testing.M) {
	discover.ScanFleetPort = 0 // scans never reach a real fleet on this machine's default port
	os.Exit(m.Run())
}

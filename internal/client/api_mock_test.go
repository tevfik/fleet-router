package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/plan"
	"github.com/tevfik/fleet-router/internal/server"
	"github.com/tevfik/fleet-router/internal/store"
	"github.com/tevfik/fleet-router/internal/tunnel"
)

// TestClientDoErrors verifies the error handling in Client.do (connection error,
// 404 older daemon message, structured error message, and plain HTTP error).
func TestClientDoErrors(t *testing.T) {
	// 1. Connection failure / daemon unreachable
	cDown := New("127.0.0.1:1")
	err := cDown.do("GET", "/fleet/status", nil, nil, 100*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "cannot reach the fleet daemon") {
		t.Fatalf("expected cannot reach error, got: %v", err)
	}

	// 2. Mux 404 "404 page not found" indicating older daemon
	ts404 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "404 page not found", http.StatusNotFound)
	}))
	defer ts404.Close()
	c404 := New(ts404.Listener.Addr().String())
	err = c404.do("GET", "/fleet/unknown-feature", nil, nil, 2*time.Second)
	if err == nil || !strings.Contains(err.Error(), "it is older than this fleet") {
		t.Fatalf("expected older daemon error, got: %v", err)
	}

	// 3. Structured JSON error {"error": {"message": "invalid parameters"}}
	tsJSONErr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"role does not exist"}}`))
	}))
	defer tsJSONErr.Close()
	cJSONErr := New(tsJSONErr.Listener.Addr().String())
	err = cJSONErr.do("POST", "/fleet/role", nil, nil, 2*time.Second)
	if err == nil || err.Error() != "role does not exist" {
		t.Fatalf("expected extracted error message, got: %v", err)
	}

	// 4. Raw non-JSON HTTP error (e.g. 500 Internal Server Error)
	ts500 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "database disk full", http.StatusInternalServerError)
	}))
	defer ts500.Close()
	c500 := New(ts500.Listener.Addr().String())
	err = c500.do("GET", "/fleet/status", nil, nil, 2*time.Second)
	if err == nil || !strings.Contains(err.Error(), "http 500: database disk full") {
		t.Fatalf("expected http 500 error, got: %v", err)
	}

	// 5. Corrupted JSON when out is expected
	tsBadJSON := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{bad json`))
	}))
	defer tsBadJSON.Close()
	cBadJSON := New(tsBadJSON.Listener.Addr().String())
	var out map[string]any
	err = cBadJSON.do("GET", "/fleet/status", nil, &out, 2*time.Second)
	if err == nil {
		t.Fatal("expected JSON decode error on corrupted body")
	}
}

// TestChatMessagesAndStreaming verifies SSE streaming chat, reasoning parsing,
// and chat conversation message helper.
func TestChatMessagesAndStreaming(t *testing.T) {
	// 1. ChatMessages construction helper
	hist := []ChatMessage{
		{Role: "user", Content: "earlier question"},
		{Role: "assistant", Content: "earlier answer"},
	}
	msgs := ChatMessages("you are helpful", hist, "new prompt")
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(msgs))
	}
	if msgs[0].Role != "system" || msgs[0].Content != "you are helpful" {
		t.Fatalf("unexpected system message: %+v", msgs[0])
	}
	if msgs[1].Content != "earlier question" || msgs[2].Content != "earlier answer" {
		t.Fatalf("unexpected history order: %+v", msgs)
	}
	if msgs[3].Role != "user" || msgs[3].Content != "new prompt" {
		t.Fatalf("unexpected user prompt: %+v", msgs[3])
	}

	// Empty system prompt
	msgsNoSys := ChatMessages("", nil, "just user")
	if len(msgsNoSys) != 1 || msgsNoSys[0].Role != "user" {
		t.Fatalf("unexpected messages with empty system: %+v", msgsNoSys)
	}

	// 2. StreamChatMsgs with thinking and content tokens
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if req["model"] == "error_model" {
			http.Error(w, "model failed", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Fleet-Endpoint", "gpu-worker-42")

		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("expected flusher")
		}

		send := func(chunk string) {
			fmt.Fprintf(w, "data: %s\n\n", chunk)
			flusher.Flush()
		}

		// Non-data comment line (bufio scanner should ignore)
		fmt.Fprintf(w, ": heartbeat\n\n")
		flusher.Flush()

		// Stream reasoning tokens
		send(`{"choices":[{"delta":{"reasoning":"I should think first."}}]}`)
		// Stream content tokens
		send(`{"choices":[{"delta":{"content":"Here "}}]}`)
		send(`{"choices":[{"delta":{"content":"is the answer."}}]}`)
		// [DONE]
		send(`[DONE]`)
	}))
	defer ts.Close()

	c := New(ts.Listener.Addr().String())

	var emitted []string
	emit := func(tok string) {
		emitted = append(emitted, tok)
	}

	res, err := c.StreamChatMsgs(context.Background(), "my-model", msgs, emit)
	if err != nil {
		t.Fatalf("stream chat error: %v", err)
	}
	if res.Endpoint != "gpu-worker-42" {
		t.Fatalf("expected endpoint gpu-worker-42, got %s", res.Endpoint)
	}
	if res.Content != "Here is the answer." {
		t.Fatalf("expected content 'Here is the answer.', got: %q", res.Content)
	}
	if res.Tokens != 3 { // 1 reasoning + 2 content tokens
		t.Fatalf("expected 3 tokens, got %d", res.Tokens)
	}

	// Check emitted token sequence
	expectedEmitted := []string{"<think>", "I should think first.", "</think>", "Here ", "is the answer."}
	if strings.Join(emitted, "") != strings.Join(expectedEmitted, "") {
		t.Fatalf("emitted stream mismatch: %v vs %v", emitted, expectedEmitted)
	}

	// 3. StreamChat convenience wrapper
	var tokensStreamed []string
	tokCount, err := c.StreamChat(context.Background(), "my-model", "sys", "prompt", func(s string) {
		tokensStreamed = append(tokensStreamed, s)
	})
	if err != nil {
		t.Fatalf("StreamChat failed: %v", err)
	}
	if tokCount != 3 {
		t.Fatalf("expected 3 tokens, got %d", tokCount)
	}

	// 4. StreamChatMsgs HTTP error
	_, err = c.StreamChatMsgs(context.Background(), "error_model", nil, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "http 500: model failed") {
		t.Fatalf("expected http 500 error, got: %v", err)
	}

	// 5. StreamChatMsgs connection error
	cUnreach := New("127.0.0.1:1")
	_, err = cUnreach.StreamChatMsgs(context.Background(), "model", nil, func(string) {})
	if err == nil {
		t.Fatal("expected network error on unreachable client")
	}

	// 6. Stream only reasoning before [DONE] (inThinking should close with </think>)
	tsReasonOnly := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"reasoning\":\"thinking only\"}}]}\n\n")
		flusher.Flush()
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer tsReasonOnly.Close()
	cReason := New(tsReasonOnly.Listener.Addr().String())
	var reasonTokens []string
	resReason, err := cReason.StreamChatMsgs(context.Background(), "reason", nil, func(s string) {
		reasonTokens = append(reasonTokens, s)
	})
	if err != nil {
		t.Fatal(err)
	}
	if resReason.Tokens != 1 || resReason.Content != "" {
		t.Fatalf("unexpected reason only result: %+v", resReason)
	}
	if len(reasonTokens) != 3 || reasonTokens[0] != "<think>" || reasonTokens[1] != "thinking only" || reasonTokens[2] != "</think>" {
		t.Fatalf("expected closed <think> tag, got: %v", reasonTokens)
	}
}

// TestChatTempThinkingEdgeCases verifies reasoning edge cases in ChatTemp (thinking model run out of tokens,
// explicit <think> tags, non-200 responses, etc.).
func TestChatTempThinkingEdgeCases(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		m := req["model"].(string)
		w.Header().Set("X-Fleet-Endpoint", "ep-"+m)

		switch m {
		case "exhausted_length":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []any{
					map[string]any{
						"message":       map[string]any{"content": "", "reasoning_content": "thinking deeply..."},
						"finish_reason": "length",
					},
				},
			})
		case "exhausted_stop":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []any{
					map[string]any{
						"message":       map[string]any{"content": "", "reasoning": "stopped while thinking"},
						"finish_reason": "stop",
					},
				},
			})
		case "with_think_tags":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []any{
					map[string]any{
						"message": map[string]any{
							"content":           "<think>already tagged</think>Final answer",
							"reasoning_content": "already tagged",
						},
						"finish_reason": "stop",
					},
				},
			})
		case "without_think_tags":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []any{
					map[string]any{
						"message": map[string]any{
							"content":   "Final answer",
							"reasoning": "raw thinking",
						},
						"finish_reason": "stop",
					},
				},
			})
		case "http_fail":
			http.Error(w, "gateway timeout", http.StatusGatewayTimeout)
		case "corrupt":
			w.Write([]byte(`{not-json`))
		case "empty_choices":
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	c := New(ts.Listener.Addr().String())

	// 1. Exhausted length
	_, ep, err := c.ChatCtx(context.Background(), "exhausted_length", "sys", "hi", 100)
	if err == nil || !strings.Contains(err.Error(), "the model used all its tokens on reasoning") {
		t.Fatalf("expected used all tokens error, got: %v", err)
	}
	if ep != "ep-exhausted_length" {
		t.Fatalf("expected endpoint, got: %s", ep)
	}

	// 2. Exhausted stop (produced only reasoning)
	_, _, err = c.ChatCtx(context.Background(), "exhausted_stop", "", "hi", 100)
	if err == nil || !strings.Contains(err.Error(), "the model only produced reasoning") {
		t.Fatalf("expected only produced reasoning error, got: %v", err)
	}

	// 3. With already present <think> tags in content
	ans, _, err := c.ChatCtx(context.Background(), "with_think_tags", "", "hi", 100)
	if err != nil {
		t.Fatal(err)
	}
	if ans != "<think>already tagged</think>Final answer" {
		t.Fatalf("expected untampered content, got: %s", ans)
	}

	// 4. Without <think> tags in content: client should wrap reasoning into <think>...</think>
	ans, _, err = c.ChatCtx(context.Background(), "without_think_tags", "", "hi", 100)
	if err != nil {
		t.Fatal(err)
	}
	if ans != "<think>raw thinking</think>Final answer" {
		t.Fatalf("expected wrapped think tags, got: %s", ans)
	}

	// 5. HTTP failure
	_, _, err = c.ChatCtx(context.Background(), "http_fail", "", "hi", 100)
	if err == nil || !strings.Contains(err.Error(), "http 504: gateway timeout") {
		t.Fatalf("expected 504 error, got: %v", err)
	}

	// 6. Corrupt response
	_, _, err = c.ChatCtx(context.Background(), "corrupt", "", "hi", 100)
	if err == nil || !strings.Contains(err.Error(), "unexpected response") {
		t.Fatalf("expected unexpected response error, got: %v", err)
	}

	// 7. Empty choices
	_, _, err = c.ChatCtx(context.Background(), "empty_choices", "", "hi", 100)
	if err == nil || !strings.Contains(err.Error(), "unexpected response") {
		t.Fatalf("expected unexpected response error, got: %v", err)
	}

	// 8. Unreachable
	cUnreach := New("127.0.0.1:1")
	_, _, err = cUnreach.ChatCtx(context.Background(), "any", "", "hi", 100)
	if err == nil {
		t.Fatal("expected connection error")
	}
}

// TestRunsAPI tests all client methods for distributed run management.
func TestRunsAPI(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/fleet/runs":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"runs": []server.Run{{ID: "run-1", Engine: "llama-ring", Endpoint: "model-a"}},
			})
		case r.Method == "POST" && r.URL.Path == "/fleet/runs":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			_ = json.NewEncoder(w).Encode(server.Run{
				ID:       "run-created",
				Endpoint: body["endpoint"].(string),
				Engine:   body["engine"].(string),
			})
		case r.Method == "POST" && r.URL.Path == "/fleet/runs/set":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			var ended time.Time
			if end, _ := body["end"].(bool); end {
				ended = time.Now()
			}
			_ = json.NewEncoder(w).Encode(server.Run{
				ID:    body["id"].(string),
				Ended: ended,
			})
		case r.Method == "POST" && r.URL.Path == "/fleet/runs/deploy":
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{}`))
		case r.Method == "POST" && r.URL.Path == "/fleet/runs/stop":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			_ = json.NewEncoder(w).Encode(server.StopReport{
				Run:   body["id"].(string),
				Ended: true,
			})
		case r.Method == "POST" && r.URL.Path == "/fleet/endpoints/stop":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			_ = json.NewEncoder(w).Encode(server.StopReport{
				Endpoint: body["name"].(string),
				Ended:    true,
			})
		case r.Method == "POST" && r.URL.Path == "/fleet/runs/delete":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["stale"] == true {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"lost": []server.Run{{ID: "run-stale", Stale: "unresponsive"}},
				})
			} else {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"removed": []server.Run{{ID: body["id"].(string)}},
				})
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	c := New(ts.Listener.Addr().String())

	// 1. Runs()
	runs, err := c.Runs()
	if err != nil || len(runs) != 1 || runs[0].ID != "run-1" {
		t.Fatalf("Runs error or mismatch: %v, %+v", err, runs)
	}

	// 2. RecordRun
	recRun, err := c.RecordRun(plan.Result{}, 1, "test-ep", "llama-ring")
	if err != nil || recRun.ID != "run-created" || recRun.Endpoint != "test-ep" {
		t.Fatalf("RecordRun error: %v, %+v", err, recRun)
	}

	// 3. SetRun
	epName := "ep-updated"
	setRun, err := c.SetRun("run-created", &epName, true)
	if err != nil || setRun.ID != "run-created" || setRun.Ended.IsZero() {
		t.Fatalf("SetRun error: %v, %+v", err, setRun)
	}

	// 4. EndRunRecordOnly
	endOnlyRun, err := c.EndRunRecordOnly("run-123")
	if err != nil || endOnlyRun.ID != "run-123" || endOnlyRun.Ended.IsZero() {
		t.Fatalf("EndRunRecordOnly error: %v, %+v", err, endOnlyRun)
	}

	// 5. NoteDeploy
	err = c.NoteDeploy(server.DeployNote{ID: "run-deploy", Model: "qwen"})
	if err != nil {
		t.Fatalf("NoteDeploy error: %v", err)
	}

	// 6. StopRun
	stopRep, err := c.StopRun("run-stop", server.StopOptions{Force: true, Remove: true})
	if err != nil || stopRep.Run != "run-stop" || !stopRep.Ended {
		t.Fatalf("StopRun error: %v, %+v", err, stopRep)
	}

	// 7. StopEndpoint
	stopEpRep, err := c.StopEndpoint("ep-stop", server.StopOptions{DryRun: true})
	if err != nil || stopEpRep.Endpoint != "ep-stop" || !stopEpRep.Ended {
		t.Fatalf("StopEndpoint error: %v, %+v", err, stopEpRep)
	}

	// 8. DeleteRuns
	delRuns, err := c.DeleteRuns("run-del", false)
	if err != nil || len(delRuns) != 1 || delRuns[0].ID != "run-del" {
		t.Fatalf("DeleteRuns error: %v, %+v", err, delRuns)
	}

	// 9. EndStaleRuns
	staleRuns, err := c.EndStaleRuns(false)
	if err != nil || len(staleRuns) != 1 || staleRuns[0].ID != "run-stale" {
		t.Fatalf("EndStaleRuns error: %v, %+v", err, staleRuns)
	}
}

// TestHubAndTunnelMethods verifies client methods for Hub connection, status,
// economy settings, and HubUsage queries.
func TestHubAndTunnelMethods(t *testing.T) {
	var receivedToken, receivedURL string
	var receivedPorts, receivedAddPorts []any
	var receivedInsecure bool

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/fleet/tunnel/join":
			var m map[string]any
			json.NewDecoder(r.Body).Decode(&m)
			receivedURL, _ = m["hub_url"].(string)
			receivedToken, _ = m["token"].(string)
			receivedInsecure, _ = m["insecure"].(bool)
			if p, ok := m["stream_ports"].([]any); ok {
				receivedPorts = p
			}
			if p, ok := m["add_stream_ports"].([]any); ok {
				receivedAddPorts = p
			}
			w.WriteHeader(http.StatusOK)
		case "/fleet/tunnel/leave":
			w.WriteHeader(http.StatusOK)
		case "/fleet/tunnel/status":
			_ = json.NewEncoder(w).Encode(HubStatus{
				Connected: true,
				HubURL:    "wss://hub.fleet.lan",
				AutoJoin:  true,
				Insecure:  false,
				Status:    tunnel.Status{Connected: true},
				Nodes:     []tunnel.NodeRegistration{{NodeName: "dino"}},
			})
		case "/fleet/economy/settings":
			var econ config.EconomyConfig
			json.NewDecoder(r.Body).Decode(&econ)
			if econ.Enabled == nil {
				http.Error(w, "invalid economy setting", 400)
				return
			}
			w.WriteHeader(http.StatusOK)
		case "/fleet/hub/settings":
			w.WriteHeader(http.StatusOK)
		case "/fleet/history":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"snapshots": []string{"snap-1", "snap-2"},
			})
		case "/fleet/node-spec":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["name"] == "" {
				http.Error(w, "missing name", 400)
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	c := New(ts.Listener.Addr().String())

	// 1. JoinHub
	err := c.JoinHub("https://hub.example.com", "tok123", 8080, 8081)
	if err != nil || receivedURL != "https://hub.example.com" || receivedToken != "tok123" || len(receivedPorts) != 2 {
		t.Fatalf("JoinHub failed: %v, ports: %v", err, receivedPorts)
	}

	// 2. JoinHubAdd
	err = c.JoinHubAdd("https://hub2.example.com", "tok456", []int{9090})
	if err != nil || receivedURL != "https://hub2.example.com" || len(receivedAddPorts) != 1 {
		t.Fatalf("JoinHubAdd failed: %v", err)
	}

	// 3. JoinHubInsecure
	err = c.JoinHubInsecure("https://insecure.hub", "tokInsecure", true, []int{1000}, []int{2000})
	if err != nil || !receivedInsecure || receivedToken != "tokInsecure" {
		t.Fatalf("JoinHubInsecure failed: %v", err)
	}

	// 4. LeaveHub
	if err := c.LeaveHub(); err != nil {
		t.Fatalf("LeaveHub error: %v", err)
	}

	// 5. TunnelStatus
	conn, hURL, err := c.TunnelStatus()
	if err != nil || !conn || hURL != "wss://hub.fleet.lan" {
		t.Fatalf("TunnelStatus error: %v, conn=%v, url=%s", err, conn, hURL)
	}

	// 6. HubStatus
	hs, err := c.HubStatus()
	if err != nil || !hs.Connected || len(hs.Nodes) != 1 || hs.Nodes[0].NodeName != "dino" {
		t.Fatalf("HubStatus error: %v, hs=%+v", err, hs)
	}

	// 7. SetEconomySettings
	enabled := true
	err = c.SetEconomySettings(config.EconomyConfig{Enabled: &enabled})
	if err != nil {
		t.Fatalf("SetEconomySettings error: %v", err)
	}

	// 8. SetHubAPIKey and SetHubSettings
	if err := c.SetHubAPIKey("hub_api_key_test"); err != nil {
		t.Fatalf("SetHubAPIKey error: %v", err)
	}
	if err := c.SetHubSettings("https://hub.set", "token_val"); err != nil {
		t.Fatalf("SetHubSettings error: %v", err)
	}

	// 9. History
	snaps, err := c.History()
	if err != nil || len(snaps) != 2 || snaps[0] != "snap-1" {
		t.Fatalf("History error: %v, snaps=%v", err, snaps)
	}

	// 10. SetNodeSpec
	if err := c.SetNodeSpec("dino-node", config.NodeSpec{Addr: "10.0.1.50"}, false); err != nil {
		t.Fatalf("SetNodeSpec error: %v", err)
	}

	// 11. HubUsage standalone check
	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fleet/tunnel/usage" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer hub-secret-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		q := r.URL.Query()
		if q.Get("by") != "role" || q.Get("split") != "model" || q.Get("since") != "7d" {
			http.Error(w, "unexpected query", 400)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"rows": []server.UsageRow{
				{Key: "coder", In: 1000, Out: 250, Requests: 5},
			},
		})
	}))
	defer hubSrv.Close()

	// Empty hub URL check
	_, err = c.HubUsage(context.Background(), "", "key", "by", "", "")
	if err == nil || !strings.Contains(err.Error(), "no hub url configured") {
		t.Fatalf("expected no hub url error, got: %v", err)
	}

	// Successful query
	rows, err := c.HubUsage(context.Background(), hubSrv.URL, "hub-secret-key", "role", "model", "7d")
	if err != nil || len(rows) != 1 || rows[0].Key != "coder" || rows[0].In != 1000 {
		t.Fatalf("HubUsage error or mismatch: %v, rows=%+v", err, rows)
	}

	// Unauthorized query
	_, err = c.HubUsage(context.Background(), hubSrv.URL, "wrong-key", "role", "model", "7d")
	if err == nil || !strings.Contains(err.Error(), "hub returned HTTP 401") {
		t.Fatalf("expected 401 unauthorized, got: %v", err)
	}
}

// TestDaemonControlsAndConfig verifies daemon control methods (Reload, Drain, SetFleetPort, SetGroup, SetMatch, SetCaps, SetTags).
func TestDaemonControlsAndConfig(t *testing.T) {
	var lastPath string
	var lastBody map[string]any

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastPath = r.URL.Path
		lastBody = make(map[string]any)
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&lastBody)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	}))
	defer ts.Close()

	c := New(ts.Listener.Addr().String())

	// 1. Reload
	if err := c.Reload(); err != nil || lastPath != "/fleet/reload" {
		t.Fatalf("Reload failed: %v, path: %s", err, lastPath)
	}

	// 2. Drain
	if err := c.Drain(true); err != nil || lastPath != "/fleet/drain" || lastBody["on"] != true {
		t.Fatalf("Drain failed: %v, body: %+v", err, lastBody)
	}

	// 3. SetFleetPort
	if err := c.SetFleetPort("my-ep", 4811); err != nil || lastPath != "/fleet/endpoint-set" || lastBody["fleet_port"] != float64(4811) {
		t.Fatalf("SetFleetPort failed: %v, body: %+v", err, lastBody)
	}

	// 4. SetGroup
	grp := config.Group{Engine: "llama-ring", Workers: []string{"node1"}}
	if err := c.SetGroup("my-ep", grp, "120s"); err != nil || lastPath != "/fleet/endpoint-set" || lastBody["stall"] != "120s" {
		t.Fatalf("SetGroup failed: %v, body: %+v", err, lastBody)
	}

	// 5. SetMatch
	if err := c.SetMatch("coder", []string{"gpu", "fast"}); err != nil || lastPath != "/fleet/role" {
		t.Fatalf("SetMatch failed: %v", err)
	}

	// 6. SetCaps (nil slice defaults to empty slice)
	if err := c.SetCaps("my-ep", nil); err != nil || lastPath != "/fleet/endpoint-set" {
		t.Fatalf("SetCaps failed: %v", err)
	}

	// 7. SetTags (nil slice defaults to empty slice)
	if err := c.SetTags("my-ep", nil); err != nil || lastPath != "/fleet/endpoint-set" {
		t.Fatalf("SetTags failed: %v", err)
	}
}

// TestAPIKeysMethods verifies ListAPIKeys, CreateAPIKey, and DeleteAPIKey.
func TestAPIKeysMethods(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/fleet/apikeys":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"keys": []*server.APIKey{
					{Key: "flt_live_123", Name: "agent-key", Role: "coder"},
				},
			})
		case r.Method == "POST" && r.URL.Path == "/fleet/apikeys":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			_ = json.NewEncoder(w).Encode(server.APIKey{
				Key:  "flt_live_new",
				Name: body["name"].(string),
				Role: body["role"].(string),
			})
		case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/fleet/apikeys/"):
			key := strings.TrimPrefix(r.URL.Path, "/fleet/apikeys/")
			if key == "flt_live_123" {
				w.WriteHeader(http.StatusOK)
				return
			}
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	c := New(ts.Listener.Addr().String())

	// 1. ListAPIKeys
	keys, err := c.ListAPIKeys()
	if err != nil || len(keys) != 1 || keys[0].Key != "flt_live_123" {
		t.Fatalf("ListAPIKeys error: %v, keys=%+v", err, keys)
	}

	// 2. CreateAPIKey
	created, err := c.CreateAPIKey("client-key", "high", "coder", "qwen", "llama")
	if err != nil || created.Key != "flt_live_new" || created.Name != "client-key" {
		t.Fatalf("CreateAPIKey error: %v, created=%+v", err, created)
	}

	// 3. DeleteAPIKey
	if err := c.DeleteAPIKey("flt_live_123"); err != nil {
		t.Fatalf("DeleteAPIKey error: %v", err)
	}
	if err := c.DeleteAPIKey("unknown_key"); err == nil {
		t.Fatal("expected error deleting nonexistent key")
	}
}

// TestStoreRemainingMethods verifies StoreLink, StoreDelete, StorePrune, StorePull, StoreFill, StoreAbort,
// and transferAll / PullAll / FillAll.
func TestStoreRemainingMethods(t *testing.T) {
	var step int32

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/fleet/store/link":
			_ = json.NewEncoder(w).Encode(server.LinkInfo{Kind: "lan", LimitMbps: 1000})
		case "/fleet/store/rm":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			_ = json.NewEncoder(w).Encode(store.Entry{Name: body["ref"].(string), SHA256: "abc"})
		case "/fleet/store/prune":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"forgotten": []store.Entry{{Name: "old_model", SHA256: "deadbeef"}},
			})
		case "/fleet/store/pull":
			w.WriteHeader(http.StatusOK)
		case "/fleet/store/fill":
			w.WriteHeader(http.StatusOK)
		case "/fleet/store/abort":
			w.WriteHeader(http.StatusOK)
		case "/fleet/store/all":
			curr := atomic.AddInt32(&step, 1)
			running := curr == 1 // Step 1: running, Step 2+: completed
			_ = json.NewEncoder(w).Encode(map[string]any{
				"nodes": []server.NodeStore{
					{
						Node:   "node-1",
						Source: "remote",
						Store: &store.Inventory{
							Transfers: []store.Transfer{
								{ID: 10, Running: running, Err: ""},
							},
						},
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	c := New(ts.Listener.Addr().String())

	// 1. StoreLink
	li, err := c.StoreLink("node-1", "node-2", "sha256abc")
	if err != nil || li.Kind != "lan" {
		t.Fatalf("StoreLink error: %v, li=%+v", err, li)
	}

	// 2. StoreDelete
	delEntry, err := c.StoreDelete("file.gguf", true)
	if err != nil || delEntry.Name != "file.gguf" {
		t.Fatalf("StoreDelete error: %v, entry=%+v", err, delEntry)
	}

	// 3. StorePrune
	pruned, err := c.StorePrune()
	if err != nil || len(pruned) != 1 || pruned[0].Name != "old_model" {
		t.Fatalf("StorePrune error: %v, pruned=%+v", err, pruned)
	}

	// 4. StorePull
	if err := c.StorePull("node-1", "node-2", "sha256abc"); err != nil {
		t.Fatalf("StorePull error: %v", err)
	}

	// 5. StoreFill
	if err := c.StoreFill("node-1", "node-2", "sha256abc", "0-10", true); err != nil {
		t.Fatalf("StoreFill error: %v", err)
	}

	// 6. StoreAbort
	if err := c.StoreAbort("node-1"); err != nil {
		t.Fatalf("StoreAbort error: %v", err)
	}

	// 7. PullAll & FillAll via transferAll
	atomic.StoreInt32(&step, 0)
	err = c.PullAll("node-1", []string{"sha1"})
	if err != nil {
		t.Fatalf("PullAll failed: %v", err)
	}

	atomic.StoreInt32(&step, 0)
	err = c.FillAll("node-1", []string{"sha1"}, "0-5", false)
	if err != nil {
		t.Fatalf("FillAll failed: %v", err)
	}
}

// TestFetchPlanEdge verifies FetchPlan in fetch.go.
func TestFetchPlanEdge(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/fleet/store":
			_ = json.NewEncoder(w).Encode(store.Inventory{})
		case "/fleet/store/all":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"nodes": []server.NodeStore{
					{
						Node: "node-1",
						Store: &store.Inventory{
							Files: []store.Entry{
								{SHA256: "sha-registered", Complete: true},
							},
						},
					},
				},
			})
		case "/fleet/store/pull":
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	c := New(ts.Listener.Addr().String())

	// 1. Unregistered plan
	err := c.FetchPlan(plan.Result{}, 1, nil)
	if err == nil || !strings.Contains(err.Error(), "not for a registered file") {
		t.Fatalf("expected unregistered plan error, got: %v", err)
	}

	// 2. Pick out of bounds
	planKnown := plan.Result{
		SHA256: "sha-registered",
		Known:  true,
		Candidates: []plan.Candidate{
			{Master: "node-1"},
		},
	}
	err = c.FetchPlan(planKnown, 5, nil)
	if err == nil || !strings.Contains(err.Error(), "there is no number 5") {
		t.Fatalf("expected pick out of bounds error, got: %v", err)
	}

	// 3. Successful plan fetch where master already holds file
	var progressMessages []string
	err = c.FetchPlan(planKnown, 1, func(msg string) {
		progressMessages = append(progressMessages, msg)
	})
	if err != nil {
		t.Fatalf("FetchPlan on valid plan failed: %v", err)
	}
}

// TestFormatTestOutput verifies FormatTest formatting of TestResult.
func TestFormatTestOutput(t *testing.T) {
	results := []TestResult{
		{
			Target:   "scout",
			Endpoint: "node-gpu-1",
			Answer:   "pong",
			Took:     150 * time.Millisecond,
			Err:      nil,
		},
		{
			Target:   "node-gpu-2",
			Endpoint: "node-gpu-2",
			Took:     500 * time.Millisecond,
			Err:      fmt.Errorf("connection refused"),
		},
	}
	lines := FormatTest(results)
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}
	if !strings.Contains(lines[0], "via router") || !strings.Contains(lines[0], "pong") {
		t.Fatalf("line 0 format error: %s", lines[0])
	}
	if !strings.Contains(lines[1], "✗") || !strings.Contains(lines[1], "connection refused") {
		t.Fatalf("line 1 format error: %s", lines[1])
	}
}

package vision

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestVisionServer_HealthAndDescribe(t *testing.T) {
	srv := NewServer(ServerOptions{
		Port:      19999,
		ModelPath: "/models/mmproj.gguf",
		CPUOnly:   true,
	})

	// Test Health handler
	recHealth := httptest.NewRecorder()
	reqHealth := httptest.NewRequest("GET", "/health", nil)
	srv.handleHealth(recHealth, reqHealth)
	if recHealth.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recHealth.Code)
	}
	if !strings.Contains(recHealth.Body.String(), "fleet-vision") {
		t.Fatalf("missing service name: %s", recHealth.Body.String())
	}

	// Test Describe handler
	payload := `{"image": "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY44YAAAAASUVORK5CYII="}`
	recDesc := httptest.NewRecorder()
	reqDesc := httptest.NewRequest("POST", "/v1/vision/describe", strings.NewReader(payload))
	srv.handleDescribe(recDesc, reqDesc)
	if recDesc.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recDesc.Code)
	}
	if !strings.Contains(recDesc.Body.String(), "could not be analyzed") {
		t.Fatalf("without a backend the text must say the image was not analyzed: %s", recDesc.Body.String())
	}
}

// A backend that fails is an error to the caller, never a made-up description.
func TestDescribeBackendFailureIsAnError(t *testing.T) {
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", 500) }))
	defer be.Close()
	srv := NewServer(ServerOptions{BackendURL: be.URL})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/vision/describe", strings.NewReader(`{"image":"data:image/png;base64,AAAA"}`))
	srv.handleDescribe(rec, req)
	if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "multimodal") {
		t.Fatalf("want 502, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestProcessMessages(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":"ok"}`))
		case "/v1/vision/describe":
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":"ok","description":"A small red dot on a white canvas"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	client := NewClient(ts.URL)
	if !client.Ping(context.Background()) {
		t.Fatal("expected ping to succeed")
	}

	// 1. Plain text messages should remain untouched
	plainMsgs := []any{
		map[string]any{"role": "user", "content": "hello world"},
	}
	resPlain, modified, err := ProcessMessages(context.Background(), client, plainMsgs)
	if err != nil {
		t.Fatalf("plain error: %v", err)
	}
	if modified {
		t.Fatal("expected plain message not to be marked modified")
	}
	if len(resPlain) != 1 {
		t.Fatalf("unexpected length: %d", len(resPlain))
	}

	// 2. Multimodal message with image_url should be replaced with visual context
	mmMsgs := []any{
		map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": "What is in this picture?"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}},
			},
		},
	}
	resMM, modified, err := ProcessMessages(context.Background(), client, mmMsgs)
	if err != nil {
		t.Fatalf("mm error: %v", err)
	}
	if !modified {
		t.Fatal("expected mm message to be marked modified")
	}
	m0 := resMM[0].(map[string]any)
	parts := m0["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(parts))
	}
	p1 := parts[1].(map[string]any)
	if p1["type"] != "text" {
		t.Fatalf("expected part 1 to be text, got %v", p1["type"])
	}
	textVal := p1["text"].(string)
	if !strings.Contains(textVal, "[Visual Context: A small red dot on a white canvas]") {
		t.Fatalf("unexpected visual context text: %s", textVal)
	}
}

func TestNewClientAndPingErrors(t *testing.T) {
	// 1. Default baseURL
	cDef := NewClient("")
	if cDef.BaseURL != "http://127.0.0.1:18095" {
		t.Fatalf("expected default baseURL http://127.0.0.1:18095, got %s", cDef.BaseURL)
	}

	// 2. Trailing slash trimmed
	cSlash := NewClient("http://localhost:8080/")
	if cSlash.BaseURL != "http://localhost:8080" {
		t.Fatalf("expected trimmed slash, got %s", cSlash.BaseURL)
	}

	// 3. Ping with invalid request context / URL
	cBadURL := &Client{BaseURL: "http://invalid-url\x7f:9999", HTTPClient: &http.Client{}}
	if cBadURL.Ping(context.Background()) {
		t.Fatal("expected Ping to fail with malformed URL")
	}

	// 4. Ping with connection failure (port 1 unlikely to be listening)
	cUnreachable := &Client{BaseURL: "http://127.0.0.1:1", HTTPClient: &http.Client{Timeout: 100 * time.Millisecond}}
	if cUnreachable.Ping(context.Background()) {
		t.Fatal("expected Ping to fail when server unreachable")
	}

	// 5. Ping with non-200 status code
	tsErr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}))
	defer tsErr.Close()
	cErr := NewClient(tsErr.URL)
	if cErr.Ping(context.Background()) {
		t.Fatal("expected Ping to return false when status is 500")
	}
}

func TestClientDescribeVariantsAndErrors(t *testing.T) {
	var lastPrompt string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/vision/describe":
			var req DescribeRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "bad req", 400)
				return
			}
			lastPrompt = req.Prompt
			if req.Image == "fail_status" {
				http.Error(w, "backend model exploded", http.StatusBadGateway)
				return
			}
			if req.Image == "fail_json" {
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`{not valid json`))
				return
			}
			if req.Image == "fail_res_err" {
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`{"status":"error","error":"model out of memory"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(DescribeResponse{
				Status:      "ok",
				Description: "Processed with: " + req.Prompt,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	c := NewClient(ts.URL)

	// Default prompt when empty
	desc, err := c.Describe(context.Background(), "data:image/png;base64,valid", "")
	if err != nil {
		t.Fatalf("unexpected describe error: %v", err)
	}
	if !strings.Contains(desc, "Describe this image in detail") {
		t.Fatalf("expected default prompt in result, got: %s", desc)
	}
	if !strings.Contains(lastPrompt, "Describe this image in detail") {
		t.Fatalf("expected default prompt sent, got: %s", lastPrompt)
	}

	// Custom prompt
	customPrompt := "What color is the car?"
	desc, err = c.Describe(context.Background(), "data:image/png;base64,valid", customPrompt)
	if err != nil {
		t.Fatalf("unexpected describe error: %v", err)
	}
	if !strings.Contains(desc, customPrompt) {
		t.Fatalf("expected custom prompt in result, got: %s", desc)
	}

	// HTTP error response (502)
	_, err = c.Describe(context.Background(), "fail_status", "")
	if err == nil || !strings.Contains(err.Error(), "vision service returned HTTP 502") {
		t.Fatalf("expected HTTP 502 error, got: %v", err)
	}

	// Corrupted JSON response
	_, err = c.Describe(context.Background(), "fail_json", "")
	if err == nil {
		t.Fatal("expected JSON decode error")
	}

	// res.Error returned
	_, err = c.Describe(context.Background(), "fail_res_err", "")
	if err == nil || !strings.Contains(err.Error(), "model out of memory") {
		t.Fatalf("expected vision analysis error, got: %v", err)
	}

	// Network error / unreachable
	cUnreach := NewClient("http://127.0.0.1:1")
	cUnreach.HTTPClient.Timeout = 100 * time.Millisecond
	_, err = cUnreach.Describe(context.Background(), "img", "")
	if err == nil {
		t.Fatal("expected network error")
	}

	// Malformed BaseURL leading to NewRequestWithContext error
	cBad := &Client{BaseURL: "http://invalid-url\x7f:9999", HTTPClient: &http.Client{}}
	_, err = cBad.Describe(context.Background(), "img", "")
	if err == nil {
		t.Fatal("expected request creation error")
	}
}

func TestProcessMessagesEdgeCases(t *testing.T) {
	// 1. client nil
	msgs := []any{map[string]any{"content": []any{map[string]any{"type": "image_url"}}}}
	out, mod, err := ProcessMessages(context.Background(), nil, msgs)
	if err != nil || mod || len(out) != len(msgs) {
		t.Fatalf("expected untouched msgs when client is nil")
	}

	// 2. msgs empty
	c := NewClient("http://127.0.0.1:18095")
	out, mod, err = ProcessMessages(context.Background(), c, nil)
	if err != nil || mod || out != nil {
		t.Fatalf("expected nil when msgs is nil")
	}
	out, mod, err = ProcessMessages(context.Background(), c, []any{})
	if err != nil || mod || len(out) != 0 {
		t.Fatalf("expected empty when msgs is empty")
	}

	// 3. non-map elements or non-slice content
	nonMapMsgs := []any{
		"plain string",
		map[string]any{"content": 12345},
		map[string]any{"other_field": "test"},
	}
	out, mod, err = ProcessMessages(context.Background(), c, nonMapMsgs)
	if err != nil || mod || len(out) != len(nonMapMsgs) {
		t.Fatalf("expected untouched msgs when no image parts exist")
	}

	// 4. image formats: pm["type"] == "image", string image_url, and pm["image"] string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(DescribeResponse{
			Status:      "ok",
			Description: "detected item",
		})
	}))
	defer ts.Close()
	cMock := NewClient(ts.URL)

	variousImages := []any{
		"not a map inside messages",
		map[string]any{"content": "string content"},
		map[string]any{
			"role": "user",
			"content": []any{
				"not a map part",
				map[string]any{"type": "text", "text": "intro"},
				map[string]any{"type": "image", "image": "data:image/jpeg;base64,img1"},
				map[string]any{"type": "image_url", "image_url": "data:image/jpeg;base64,img2"},
			},
		},
	}
	out, mod, err = ProcessMessages(context.Background(), cMock, variousImages)
	if err != nil || !mod {
		t.Fatalf("expected successful image processing, err=%v, mod=%v", err, mod)
	}
	if len(out) != 3 {
		t.Fatalf("expected 3 items in output, got %d", len(out))
	}
	m2, ok := out[2].(map[string]any)
	if !ok {
		t.Fatalf("expected map in item 2")
	}
	parts := m2["content"].([]any)
	if len(parts) != 4 {
		t.Fatalf("expected 4 parts, got %d", len(parts))
	}
	// Check visual context replacements
	p2 := parts[2].(map[string]any)
	if p2["text"] != "[Visual Context: detected item]" {
		t.Fatalf("unexpected text in p2: %v", p2["text"])
	}
	p3 := parts[3].(map[string]any)
	if p3["text"] != "[Visual Context: detected item]" {
		t.Fatalf("unexpected text in p3: %v", p3["text"])
	}

	// 5. Describe failure causes ProcessMessages to return error
	tsErr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gpu failure", 500)
	}))
	defer tsErr.Close()
	cFail := NewClient(tsErr.URL)
	failMsgs := []any{
		map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,abc"}},
			},
		},
	}
	_, _, err = ProcessMessages(context.Background(), cFail, failMsgs)
	if err == nil || !strings.Contains(err.Error(), "vision pre-flight failed") {
		t.Fatalf("expected pre-flight failed error, got: %v", err)
	}
}

func TestServerOptionsAndLifecycle(t *testing.T) {
	// NewServer defaults
	srvDef := NewServer(ServerOptions{})
	if srvDef.opts.Port != DefaultVisionPort {
		t.Fatalf("expected default port %d, got %d", DefaultVisionPort, srvDef.opts.Port)
	}
	if srvDef.opts.Host != "0.0.0.0" {
		t.Fatalf("expected default host 0.0.0.0, got %s", srvDef.opts.Host)
	}

	// Close when empty
	if err := srvDef.Close(); err != nil {
		t.Fatalf("expected clean close on empty server, got: %v", err)
	}

	// Close with mock process
	srvWithCmd := NewServer(ServerOptions{Port: 19998, Host: "127.0.0.1"})
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err == nil {
		srvWithCmd.cmd = cmd
		_ = srvWithCmd.Close()
		// Process should be terminated
		_ = cmd.Wait()
	}

	// Start and Close real server
	srvRun := NewServer(ServerOptions{Port: 19997, Host: "127.0.0.1"})
	startErrCh := make(chan error, 1)
	go func() {
		startErrCh <- srvRun.Start()
	}()

	// Wait briefly for server to bind or ping
	client := NewClient("http://127.0.0.1:19997")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for {
		if client.Ping(ctx) {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("timed out waiting for vision server to start")
		default:
			time.Sleep(20 * time.Millisecond)
		}
	}

	// Close server
	if err := srvRun.Close(); err != nil {
		t.Fatalf("error closing server: %v", err)
	}
	<-startErrCh
}

func TestHandleDescribeEdgeCasesAndBackend(t *testing.T) {
	srv := NewServer(ServerOptions{Port: 19996})

	// 1. Invalid method (GET instead of POST)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/vision/describe", nil)
	srv.handleDescribe(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 Method Not Allowed, got %d", rec.Code)
	}

	// 2. Corrupt JSON body
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/v1/vision/describe", strings.NewReader(`{invalid json`))
	srv.handleDescribe(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid json") {
		t.Fatalf("expected 400 invalid json, got %d %s", rec.Code, rec.Body.String())
	}

	// 3. Missing image field
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/v1/vision/describe", strings.NewReader(`{"prompt":"hello"}`))
	srv.handleDescribe(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "image required") {
		t.Fatalf("expected 400 image required, got %d %s", rec.Code, rec.Body.String())
	}

	// 4. Non-data:image prefix (unknown format)
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/v1/vision/describe", strings.NewReader(`{"image":"https://example.com/image.jpg"}`))
	srv.handleDescribe(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rec.Code)
	}
	var res4 DescribeResponse
	if err := json.NewDecoder(rec.Body).Decode(&res4); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res4.Description, "format unknown") {
		t.Fatalf("expected format unknown in description, got: %s", res4.Description)
	}

	// 5. Successful backend forwarding
	beOK := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{
				map[string]any{
					"message": map[string]any{
						"content": "A high resolution photo of an eagle in flight",
					},
				},
			},
		})
	}))
	defer beOK.Close()

	srvWithBackend := NewServer(ServerOptions{BackendURL: beOK.URL})
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/v1/vision/describe", strings.NewReader(`{"image":"data:image/png;base64,AAAA","prompt":"describe"}`))
	srvWithBackend.handleDescribe(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK with backend, got %d %s", rec.Code, rec.Body.String())
	}
	var res5 DescribeResponse
	if err := json.NewDecoder(rec.Body).Decode(&res5); err != nil {
		t.Fatal(err)
	}
	if res5.Description != "A high resolution photo of an eagle in flight" {
		t.Fatalf("unexpected description: %s", res5.Description)
	}

	// 6. Backend returns invalid json
	beBadJSON := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{not-json`))
	}))
	defer beBadJSON.Close()

	srvBadJSON := NewServer(ServerOptions{BackendURL: beBadJSON.URL})
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/v1/vision/describe", strings.NewReader(`{"image":"data:image/png;base64,AAAA"}`))
	srvBadJSON.handleDescribe(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 Bad Gateway on bad json from backend, got %d", rec.Code)
	}

	// 7. Backend returns choices with empty content
	beEmpty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{},
		})
	}))
	defer beEmpty.Close()

	srvEmpty := NewServer(ServerOptions{BackendURL: beEmpty.URL})
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/v1/vision/describe", strings.NewReader(`{"image":"data:image/png;base64,AAAA"}`))
	srvEmpty.handleDescribe(rec, req)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "no content in backend response") {
		t.Fatalf("expected 502 with 'no content', got %d %s", rec.Code, rec.Body.String())
	}
}

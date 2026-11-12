//go:build e2e

// Package e2e runs built fleet binaries against real model servers (CI starts llama-server with tiny models;
// locally any OpenAI-compatible server works):
//
//	FLEET_E2E_BIN=$PWD/fleet \
//	FLEET_E2E_CHAT_URL=http://127.0.0.1:8080/v1 FLEET_E2E_CHAT_MODEL=tiny-chat \
//	FLEET_E2E_EMBED_URL=http://127.0.0.1:8081/v1 FLEET_E2E_EMBED_MODEL=tiny-embed \
//	FLEET_E2E_SSH_HOST=localhost \
//	go test -tags e2e -v ./test/e2e
//
// The embedding server and the ssh host (key login to a host that reaches the chat server) are optional. Every
// fleet runs with its own temporary HOME and configuration, on free loopback ports.
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- a running fleet ----------------------------------------------------------------------------------------------

type fleet struct {
	t       *testing.T
	bin     string
	base    string // http://127.0.0.1:PORT
	cfgPath string
	environ []string
}

func need(t *testing.T, k string) string {
	t.Helper()
	v := os.Getenv(k)
	if v == "" {
		t.Skipf("%s is not set", k)
	}
	return v
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func chatServer(t *testing.T) (u, model string) {
	return need(t, "FLEET_E2E_CHAT_URL"), need(t, "FLEET_E2E_CHAT_MODEL")
}

// startFleet writes body (endpoints, roles, …) under a fixed header and runs `fleet serve` until the test ends.
// listen "" takes a free loopback port.
func startFleet(t *testing.T, listen, body string, extraEnv ...string) *fleet {
	t.Helper()
	bin, err := filepath.Abs(need(t, "FLEET_E2E_BIN"))
	if err != nil {
		t.Fatal(err)
	}
	if listen == "" {
		listen = fmt.Sprintf("127.0.0.1:%d", freePort(t))
	}
	home := t.TempDir()
	cfgPath := filepath.Join(home, "config.yaml")
	cfg := fmt.Sprintf("listen: %s\nauto_update: false\nmdns: false\n%s", listen, body)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(listen)
	f := &fleet{t: t, bin: bin, base: "http://127.0.0.1:" + port, cfgPath: cfgPath, environ: append(os.Environ(),
		"HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"), "XDG_STATE_HOME="+filepath.Join(home, ".state"),
		"XDG_CACHE_HOME="+filepath.Join(home, ".cache"), "FLEET_CONFIG="+cfgPath,
		"FLEET_REPO=", "FLEET_TOKEN=", "FLEET_VISION_URL=")}
	f.environ = append(f.environ, extraEnv...)
	// ssh (tunnels) needs the user's keys and ~/.ssh/config: the temporary HOME points at them
	if real, err := os.UserHomeDir(); err == nil {
		if _, err := os.Stat(filepath.Join(real, ".ssh")); err == nil {
			_ = os.Symlink(filepath.Join(real, ".ssh"), filepath.Join(home, ".ssh"))
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, bin, "serve")
	cmd.Env = f.environ // the daemon takes listen from the config (FLEET_LISTEN would override it)
	var logBuf syncBuffer
	cmd.Stdout, cmd.Stderr = &logBuf, &logBuf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("fleet serve (%s) log:\n%s", listen, logBuf.String())
		}
	})
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		if r, err := http.Get(f.base + "/healthz"); err == nil && r.StatusCode == 200 {
			r.Body.Close()
			return f
		}
		if time.Now().After(deadline) {
			t.Fatalf("fleet serve did not come up:\n%s", logBuf.String())
		}
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// call sends a request and returns status, headers and body (no status check).
func call(t *testing.T, method, u, body string, hdr ...string) (int, http.Header, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, u, err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return r.StatusCode, r.Header, b
}

// ok posts to the fleet and fails unless it answers 200.
func (f *fleet) ok(path, body string) (http.Header, []byte) {
	f.t.Helper()
	code, h, b := call(f.t, "POST", f.base+path, body)
	if code != 200 {
		f.t.Fatalf("POST %s: %d\n%s", path, code, b)
	}
	return h, b
}

func (f *fleet) get(path string) []byte {
	f.t.Helper()
	code, _, b := call(f.t, "GET", f.base+path, "")
	if code != 200 {
		f.t.Fatalf("GET %s: %d\n%s", path, code, b)
	}
	return b
}

func (f *fleet) run(args ...string) string {
	f.t.Helper()
	cmd := exec.Command(f.bin, args...)
	cmd.Env = f.cliEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("fleet %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// cliEnv is the environment of a CLI command: it talks to this fleet's daemon.
func (f *fleet) cliEnv() []string {
	return append(append([]string{}, f.environ...), "FLEET_LISTEN="+strings.TrimPrefix(f.base, "http://"))
}

// chat asks model a short question and returns the endpoint that answered.
func (f *fleet) chat(model string) string {
	f.t.Helper()
	h, b := f.ok("/v1/chat/completions", chatBody(model, ""))
	var out struct{ Choices []json.RawMessage }
	if err := json.Unmarshal(b, &out); err != nil || len(out.Choices) == 0 {
		f.t.Fatalf("answer of %s: %v\n%s", model, err, b)
	}
	return h.Get("X-Fleet-Endpoint")
}

func chatBody(model, extra string) string {
	return fmt.Sprintf(`{"model": %q, "max_tokens": 16, "temperature": 0,
		"messages": [{"role": "user", "content": "Say hello."}]%s}`, model, extra)
}

// eventually retries f until it returns nil or the time is up.
func eventually(t *testing.T, d time.Duration, f func() error) {
	t.Helper()
	var err error
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(300 * time.Millisecond) {
		if err = f(); err == nil {
			return
		}
	}
	t.Fatal(err)
}

// ---- one fleet, one real model --------------------------------------------------------------------------------

func TestRealServers(t *testing.T) {
	chatURL, chatModel := chatServer(t)
	embedURL, embedModel := os.Getenv("FLEET_E2E_EMBED_URL"), os.Getenv("FLEET_E2E_EMBED_MODEL")
	body := fmt.Sprintf(`endpoints:
  - {name: dead, node: e2e, url: "http://127.0.0.1:%d/v1", model: gone}
  - {name: chat, node: e2e, url: %q, model: %q}
`, freePort(t), chatURL, chatModel)
	if embedURL != "" {
		body += fmt.Sprintf("  - {name: emb, node: e2e, url: %q, model: %q, caps: [embed]}\n", embedURL, embedModel)
	}
	body += "roles:\n  scout: {preferred: dead, fallback: [chat]}\n"
	if embedURL != "" {
		body += "  embed: {preferred: emb}\n"
	}
	f := startFleet(t, "", body)

	t.Run("models list the role", func(t *testing.T) {
		if b := f.get("/v1/models"); !bytes.Contains(b, []byte(`"scout"`)) {
			t.Fatalf("/v1/models lacks the role:\n%s", b)
		}
	})
	t.Run("chat fails over from the dead model", func(t *testing.T) {
		if got := f.chat("scout"); got != "chat" {
			t.Fatalf("answered by %q, want chat", got)
		}
	})
	t.Run("chat streams", func(t *testing.T) {
		_, b := f.ok("/v1/chat/completions", chatBody("scout", `, "stream": true`))
		chunks, done := 0, false
		for sc := bufio.NewScanner(bytes.NewReader(b)); sc.Scan(); {
			switch l := sc.Text(); {
			case l == "data: [DONE]":
				done = true
			case strings.HasPrefix(l, "data: "):
				chunks++
			}
		}
		if chunks == 0 || !done {
			t.Fatalf("stream: %d chunks, done %v\n%s", chunks, done, b)
		}
	})
	t.Run("ollama api", func(t *testing.T) {
		_, b := f.ok("/api/chat", `{"model": "scout", "stream": false, "options": {"num_predict": 16, "temperature": 0},
			"messages": [{"role": "user", "content": "Say hello."}]}`)
		var out struct {
			Message struct{ Role, Content string }
			Done    bool
		}
		if err := json.Unmarshal(b, &out); err != nil || !out.Done || out.Message.Role != "assistant" {
			t.Fatalf("/api/chat: %v\n%s", err, b)
		}
	})
	t.Run("embeddings", func(t *testing.T) {
		if embedURL == "" {
			t.Skip("no embedding server")
		}
		_, b := f.ok("/v1/embeddings", `{"model": "embed", "input": "hello"}`)
		var out struct {
			Data []struct{ Embedding []float64 }
		}
		if err := json.Unmarshal(b, &out); err != nil || len(out.Data) != 1 || len(out.Data[0].Embedding) == 0 {
			t.Fatalf("embeddings: %v\n%.300s", err, b)
		}
	})
	t.Run("cli", func(t *testing.T) {
		// the role names its live model, and says it runs on the fallback (the preferred one is dead)
		if out := f.run("status"); !strings.Contains(out, "scout") || !strings.Contains(out, chatModel) ||
			!strings.Contains(out, "fallback active") {
			t.Fatalf("fleet status:\n%s", out)
		}
		if out := f.run("ask", "-n", "16", "scout", "Say hello."); strings.TrimSpace(out) == "" {
			t.Fatal("fleet ask printed nothing")
		}
	})
}

// ---- changes while it runs ----------------------------------------------------------------------------------------

// A role is re-pointed while the daemon runs: the next request goes to the new model, and `fleet config undo`
// brings the old one back, without a restart.
func TestHotConfigChange(t *testing.T) {
	chatURL, chatModel := chatServer(t)
	f := startFleet(t, "", fmt.Sprintf(`endpoints:
  - {name: first, node: e2e, url: %q, model: %q}
  - {name: second, node: e2e, url: %q, model: %q}
roles:
  scout: {preferred: first}
`, chatURL, chatModel, chatURL, chatModel))

	if got := f.chat("scout"); got != "first" {
		t.Fatalf("before: answered by %q", got)
	}
	f.run("role", "set", "scout", "second")
	if got := f.chat("scout"); got != "second" {
		t.Fatalf("after role set: answered by %q", got)
	}
	f.run("config", "undo")
	if got := f.chat("scout"); got != "first" {
		t.Fatalf("after undo: answered by %q", got)
	}
}

// ---- routing rules ------------------------------------------------------------------------------------------------

func TestRoutingRules(t *testing.T) {
	chatURL, chatModel := chatServer(t)
	f := startFleet(t, "", fmt.Sprintf(`endpoints:
  - {name: textonly, node: e2e, url: %[1]q, model: %[2]q, caps: [chat]}
  - {name: solo, node: e2e, url: %[1]q, model: %[2]q, caps: [chat], slots: 1}
  - {name: lb1, node: e2e, url: %[1]q, model: %[2]q, caps: [chat], slots: 1}
  - {name: lb2, node: e2e, url: %[1]q, model: %[2]q, caps: [chat], slots: 1}
roles:
  reader: {preferred: textonly}
  spread: {preferred: lb1, fallback: [lb2], strategy: least-busy}
  ordered: {preferred: lb1, fallback: [lb2]}
`, chatURL, chatModel))

	t.Run("an image to a text-only model is refused", func(t *testing.T) {
		img := `{"model": "reader", "messages": [{"role": "user", "content": [
			{"type": "text", "text": "What is this?"},
			{"type": "image_url", "image_url": {"url": "data:image/png;base64,iVBORw0KGgo="}}]}]}`
		code, _, b := call(t, "POST", f.base+"/v1/chat/completions", img)
		if code != 422 || !bytes.Contains(b, []byte("vision")) {
			t.Fatalf("want 422 naming vision, got %d\n%s", code, b)
		}
	})

	// n requests at once to model; returns the endpoints that answered
	burst := func(t *testing.T, model string, n int) []string {
		var wg sync.WaitGroup
		got := make([]string, n)
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				code, h, b := call(t, "POST", f.base+"/v1/chat/completions", fmt.Sprintf(
					`{"model": %q, "max_tokens": 48, "temperature": 0, "messages": [{"role": "user", "content": "Count from 1 to 30."}]}`, model))
				if code != 200 {
					t.Errorf("%s: %d %s", model, code, b)
				}
				got[i] = h.Get("X-Fleet-Endpoint")
			}()
		}
		wg.Wait()
		return got
	}

	t.Run("a full model queues the next request", func(t *testing.T) {
		burst(t, "solo", 2)
		var log struct {
			Entries []struct {
				Endpoint string `json:"endpoint"`
				QueueMS  int64  `json:"queue_ms"`
			}
		}
		if err := json.Unmarshal(f.get("/fleet/log?n=20"), &log); err != nil {
			t.Fatal(err)
		}
		queued := 0
		for _, e := range log.Entries {
			if e.Endpoint == "solo" && e.QueueMS > 0 {
				queued++
			}
		}
		if queued == 0 {
			t.Fatalf("no request waited for solo's single slot: %+v", log.Entries)
		}
	})

	t.Run("least-busy spreads, failover keeps the order", func(t *testing.T) {
		used := map[string]int{}
		for _, ep := range burst(t, "spread", 4) {
			used[ep]++
		}
		if used["lb1"] == 0 || used["lb2"] == 0 {
			t.Fatalf("least-busy used %v, want both lb1 and lb2", used)
		}
		used = map[string]int{}
		for _, ep := range burst(t, "ordered", 3) {
			used[ep]++
		}
		if used["lb1"] != 3 {
			t.Fatalf("failover used %v, want lb1 only (it waits for its slot)", used)
		}
	})
}

// ---- sharing with the LAN -----------------------------------------------------------------------------------------

func lanIP(t *testing.T) string {
	t.Helper()
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && n.IP.To4() != nil && !n.IP.IsLinkLocalUnicast() {
			return n.IP.String()
		}
	}
	t.Skip("no non-loopback IPv4 address")
	return ""
}

// Listening beyond loopback with api_key_env: a request from another address needs the key; this machine does not.
func TestAPIKeyOnTheLAN(t *testing.T) {
	chatURL, chatModel := chatServer(t)
	ip := lanIP(t)
	port := freePort(t)
	f := startFleet(t, fmt.Sprintf("0.0.0.0:%d", port), fmt.Sprintf(`api_key_env: FLEET_E2E_KEY
endpoints:
  - {name: chat, node: e2e, url: %q, model: %q}
roles:
  scout: {preferred: chat}
`, chatURL, chatModel), "FLEET_E2E_KEY=e2e-secret-key")
	lan := fmt.Sprintf("http://%s:%d/v1/chat/completions", ip, port)

	if code, h, _ := call(t, "POST", lan, chatBody("scout", "")); code != 401 || h.Get("WWW-Authenticate") == "" {
		t.Fatalf("no key from the LAN: %d, want 401", code)
	}
	if code, _, _ := call(t, "POST", lan, chatBody("scout", ""), "Authorization", "Bearer wrong"); code != 401 {
		t.Fatalf("wrong key: %d, want 401", code)
	}
	if code, _, b := call(t, "POST", lan, chatBody("scout", ""), "Authorization", "Bearer e2e-secret-key"); code != 200 {
		t.Fatalf("right key: %d\n%s", code, b)
	}
	f.chat("scout") // loopback: no key needed
}

// ---- several fleets -------------------------------------------------------------------------------------------

// Node B, which the hub cannot reach, joins hub A over an outgoing WebSocket: B's model answers through A.
func TestHubJoin(t *testing.T) {
	chatURL, chatModel := chatServer(t)
	hub := startFleet(t, "", "name: hub\n")
	node := startFleet(t, "", fmt.Sprintf(`name: nodeb
endpoints:
  - {name: chat, node: nodeb, url: %q, model: %q}
`, chatURL, chatModel))

	if out := node.run("join", hub.base); !strings.Contains(out, "Connected") {
		t.Fatalf("fleet join:\n%s", out)
	}
	model := "nodeb/" + chatModel
	if b := hub.get("/v1/models"); !bytes.Contains(b, []byte(model)) {
		t.Fatalf("hub does not list %s:\n%s", model, b)
	}
	hub.chat(model)
	// the request went through the tunnel and was served by the node's own endpoint
	var log struct {
		Entries []struct {
			Endpoint string `json:"endpoint"`
			Status   int    `json:"status"`
		}
	}
	if err := json.Unmarshal(node.get("/fleet/log?n=5"), &log); err != nil || len(log.Entries) == 0 ||
		log.Entries[len(log.Entries)-1].Endpoint != "chat" || log.Entries[len(log.Entries)-1].Status != 200 {
		t.Fatalf("the node did not serve the request: %v %+v", err, log.Entries)
	}
	node.run("join", "--leave")
	eventually(t, 10*time.Second, func() error {
		if b := hub.get("/v1/models"); bytes.Contains(b, []byte(model)) {
			return fmt.Errorf("hub still lists %s after the node left", model)
		}
		return nil
	})
}

// Fleet B adds fleet A as a peer with a one-time pairing code: A's model answers on B as A's node/model.
func TestPeersPairing(t *testing.T) {
	chatURL, chatModel := chatServer(t)
	a := startFleet(t, "", fmt.Sprintf(`name: nodea
api_key_env: FLEET_E2E_KEY
endpoints:
  - {name: chat, node: nodea, url: %q, model: %q}
`, chatURL, chatModel), "FLEET_E2E_KEY=peer-test-key")
	b := startFleet(t, "", "name: nodeb\n")

	m := regexp.MustCompile(`pairing code: (\d{6})`).FindStringSubmatch(a.run("peers", "pair"))
	if m == nil {
		t.Fatal("no pairing code printed")
	}
	b.run("peers", "add", "a", a.base+"/v1", "pair="+m[1])
	if cfg, _ := os.ReadFile(b.cfgPath); !bytes.Contains(cfg, []byte("key:")) {
		t.Fatalf("B did not keep the key the pairing gave it:\n%s", cfg)
	}
	model := "nodea/" + chatModel
	eventually(t, 30*time.Second, func() error {
		b.run("peers", "sync")
		if bb := b.get("/v1/models"); !bytes.Contains(bb, []byte(model)) {
			return fmt.Errorf("B does not list %s:\n%s", model, bb)
		}
		return nil
	})
	b.chat(model)

	// the code was used up: another fleet cannot pair with it
	c := startFleet(t, "", "name: nodec\n")
	cmd := exec.Command(c.bin, "peers", "add", "a", a.base+"/v1", "pair="+m[1])
	cmd.Env = c.cliEnv()
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("a used pairing code was accepted again:\n%s", out)
	}
}

// ---- ssh -------------------------------------------------------------------------------------------------------

// A model reached through `ssh <host>` (fleet opens the -L tunnel itself) answers like a local one.
func TestSSHTunnel(t *testing.T) {
	chatURL, chatModel := chatServer(t)
	host := need(t, "FLEET_E2E_SSH_HOST")
	u, err := url.Parse(chatURL)
	if err != nil {
		t.Fatal(err)
	}
	f := startFleet(t, "", fmt.Sprintf(`endpoints:
  - {name: viassh, node: sshbox, ssh: %q, remote: %q, model: %q}
roles:
  scout: {preferred: viassh}
`, host, u.Host, chatModel))
	// the first request, sent while the ssh connection is still coming up, waits for it (no "tunnel down")
	if got := f.chat("scout"); got != "viassh" {
		t.Fatalf("answered by %q, want viassh", got)
	}
}

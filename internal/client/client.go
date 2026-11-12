// Package client is the client for the daemon's admin/OpenAI API (used by the CLI, TUI and MCP).
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/discover"
	"github.com/tevfik/fleet-router/internal/plan"
	"github.com/tevfik/fleet-router/internal/server"
	"github.com/tevfik/fleet-router/internal/store"
	"github.com/tevfik/fleet-router/internal/tunnel"
)

type Client struct {
	Base string // http://127.0.0.1:4711
	hc   *http.Client
}

func New(listen string) *Client {
	return &Client{Base: "http://" + listen, hc: &http.Client{Timeout: 0}}
}

func (c *Client) Alive() bool {
	cx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(cx, "GET", c.Base+"/healthz", nil)
	resp, err := c.hc.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

func (c *Client) do(method, path string, in, out any, to time.Duration) error {
	var rd io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		rd = bytes.NewReader(b)
	}
	cx, cancel := context.WithTimeout(context.Background(), to)
	defer cancel()
	req, _ := http.NewRequestWithContext(cx, method, c.Base+path, rd)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach the fleet daemon (%s) — is `fleet serve` running? %v", c.Base, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		var e struct{ Error struct{ Message string } }
		if json.Unmarshal(b, &e) == nil && e.Error.Message != "" {
			return fmt.Errorf("%s", e.Error.Message)
		}
		if resp.StatusCode == 404 && strings.TrimSpace(string(b)) == "404 page not found" { // the mux's own answer: no such route
			if path != "/fleet/status" { // a daemon without fleet-engine has no runner routes: that is not an old daemon
				var st server.Status
				if c.do("GET", "/fleet/status", nil, &st, 5*time.Second) == nil && st.Runner != nil && !*st.Runner {
					return fmt.Errorf("the running daemon has no fleet-engine (the runner side: plans, runs, model files), so it has no %s", path)
				}
			}
			return fmt.Errorf("the running daemon does not know %s — it is older than this fleet; restart it (systemctl --user restart fleet)", path)
		}
		return fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

func (c *Client) Status() (st server.Status, err error) {
	err = c.do("GET", "/fleet/status", nil, &st, 10*time.Second)
	return
}

// Log returns the newest n router requests, oldest first.
func (c *Client) Log(n int) ([]server.LogEntry, error) {
	var r struct {
		Entries []server.LogEntry `json:"entries"`
	}
	err := c.do("GET", fmt.Sprintf("/fleet/log?n=%d", n), nil, &r, 10*time.Second)
	return r.Entries, err
}

func (c *Client) Config() (cfg config.Config, err error) {
	err = c.do("GET", "/fleet/config", nil, &cfg, 10*time.Second)
	return
}

// Undo restores the previous configuration; left is how many older snapshots remain.
func (c *Client) Undo() (left int, err error) {
	var out struct {
		Left int `json:"left"`
	}
	err = c.do("POST", "/fleet/undo", map[string]any{}, &out, 10*time.Second)
	return out.Left, err
}

func (c *Client) SetRole(role, preferred string, fallback []string) error {
	if fallback == nil {
		fallback = []string{} // empty list = clear fallbacks (null = leave untouched)
	}
	return c.do("POST", "/fleet/role", map[string]any{"role": role, "preferred": preferred, "fallback": fallback}, nil, 10*time.Second)
}

// UpdateRole updates only the given fields (nil = leave untouched).
func (c *Client) UpdateRole(role string, f map[string]any) error {
	f["role"] = role
	return c.do("POST", "/fleet/role", f, nil, 10*time.Second)
}

func (c *Client) SetSlots(endpoint string, slots int) error {
	return c.do("POST", "/fleet/endpoint-set", map[string]any{"name": endpoint, "slots": slots}, nil, 10*time.Second)
}

func (c *Client) DeleteRole(role string) error {
	return c.do("POST", "/fleet/role", map[string]any{"role": role, "delete": true}, nil, 10*time.Second)
}

func (c *Client) Endpoints(add []config.Endpoint, remove []string) error {
	_, err := c.EndpointsNotes(add, remove, false)
	return err
}

// EndpointsNotes is Endpoints that also returns what the removal changed in the roles that named the endpoints.
func (c *Client) EndpointsNotes(add []config.Endpoint, remove []string, mendRoles bool) ([]string, error) {
	var out struct {
		Roles []string `json:"roles"`
	}
	err := c.do("POST", "/fleet/endpoints", map[string]any{"add": add, "remove": remove, "mend_roles": mendRoles}, &out, 10*time.Second)
	return out.Roles, err
}

func (c *Client) OpenCode() (m map[string]any, err error) {
	err = c.do("GET", "/fleet/opencode", nil, &m, 10*time.Second)
	return
}

// Chat is a one-shot chat with a role/endpoint (through the router).
func (c *Client) Chat(model, system, prompt string, maxTokens int, to time.Duration) (string, string, error) {
	cx, cancel := context.WithTimeout(context.Background(), to)
	defer cancel()
	return c.ChatCtx(cx, model, system, prompt, maxTokens)
}

// ChatCtx is a cancellable chat (cancelling a job really aborts the request).
func (c *Client) ChatCtx(cx context.Context, model, system, prompt string, maxTokens int) (string, string, error) {
	return c.ChatTemp(cx, model, system, prompt, maxTokens, nil)
}

// ChatTemp is ChatCtx with an explicit sampling temperature (nil = the server's default). Evaluations use 0
// so that a repeated run measures the model, not the dice.
func (c *Client) ChatTemp(cx context.Context, model, system, prompt string, maxTokens int, temp *float64) (string, string, error) {
	msgs := []map[string]string{}
	if system != "" {
		msgs = append(msgs, map[string]string{"role": "system", "content": system})
	}
	msgs = append(msgs, map[string]string{"role": "user", "content": prompt})
	in := map[string]any{"model": model, "messages": msgs, "stream": false}
	if maxTokens > 0 {
		in["max_tokens"] = maxTokens
	}
	if temp != nil {
		in["temperature"] = *temp
	}
	b, _ := json.Marshal(in)
	req, _ := http.NewRequestWithContext(cx, "POST", c.Base+"/v1/chat/completions", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var d struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				Reasoning string `json:"reasoning_content"`
				Thinking  string `json:"reasoning"`
			} `json:"message"`
			Finish string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &d); err != nil || len(d.Choices) == 0 {
		return "", "", fmt.Errorf("unexpected response: %.200s", raw)
	}
	ch := d.Choices[0]
	ep := resp.Header.Get("X-Fleet-Endpoint")
	if strings.TrimSpace(ch.Message.Content) == "" && (ch.Message.Reasoning != "" || ch.Message.Thinking != "") {
		// a thinking model that ran out of tokens before answering: say so instead of returning nothing
		why := "the model only produced reasoning"
		if ch.Finish == "length" {
			why = "the model used all its tokens on reasoning"
		}
		return "", ep, fmt.Errorf("no answer: %s — raise max tokens (ask -n) or use a non-thinking model", why)
	}
	think := ch.Message.Reasoning
	if think == "" {
		think = ch.Message.Thinking
	}
	if strings.TrimSpace(think) != "" && !strings.Contains(ch.Message.Content, "<think>") {
		return "<think>" + think + "</think>" + ch.Message.Content, ep, nil
	}
	return ch.Message.Content, ep, nil
}

// StreamChat sends a streaming prompt to model/role, calling emit on each token chunk received.
func (c *Client) StreamChat(cx context.Context, model, system, prompt string, emit func(string)) (int, error) {
	r, err := c.StreamChatMsgs(cx, model, ChatMessages(system, nil, prompt), emit)
	return r.Tokens, err
}

// ChatMessage is one message of a conversation sent to the router.
type ChatMessage struct{ Role, Content string }

// ChatMessages builds a request: the system prompt, the earlier turns, then the new user prompt.
func ChatMessages(system string, history []ChatMessage, prompt string) []ChatMessage {
	var out []ChatMessage
	if system != "" {
		out = append(out, ChatMessage{"system", system})
	}
	out = append(out, history...)
	return append(out, ChatMessage{"user", prompt})
}

// StreamResult is what a streamed chat reports besides the text: the tokens streamed and the endpoint that
// really answered (the router's X-Fleet-Endpoint; differs from the model asked for after a failover).
type StreamResult struct {
	Tokens   int
	Endpoint string
	Content  string // the answer without the reasoning (what goes into the conversation history)
}

// StreamChatMsgs is StreamChat with the whole conversation, so a model change does not lose the context.
func (c *Client) StreamChatMsgs(cx context.Context, model string, conv []ChatMessage, emit func(string)) (StreamResult, error) {
	var res StreamResult
	msgs := make([]map[string]string, 0, len(conv))
	for _, m := range conv {
		msgs = append(msgs, map[string]string{"role": m.Role, "content": m.Content})
	}
	in := map[string]any{"model": model, "messages": msgs, "stream": true}
	b, _ := json.Marshal(in)
	req, _ := http.NewRequestWithContext(cx, "POST", c.Base+"/v1/chat/completions", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return res, err
	}
	defer resp.Body.Close()
	res.Endpoint = resp.Header.Get("X-Fleet-Endpoint")
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		return res, fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	totalTokens := 0
	var answer strings.Builder
	inThinking := false
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if strings.TrimSpace(data) == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					Reasoning string `json:"reasoning_content"`
					Thinking  string `json:"reasoning"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err == nil && len(chunk.Choices) > 0 {
			d := chunk.Choices[0].Delta
			reason := d.Reasoning
			if reason == "" {
				reason = d.Thinking
			}

			if reason != "" {
				if !inThinking {
					emit("<think>")
					inThinking = true
				}
				emit(reason)
				totalTokens++
			} else if d.Content != "" {
				if inThinking {
					emit("</think>")
					inThinking = false
				}
				emit(d.Content)
				answer.WriteString(d.Content)
				totalTokens++
			}
		}
	}
	if inThinking {
		emit("</think>")
		inThinking = false
	}
	res.Tokens, res.Content = totalTokens, answer.String()
	return res, scanner.Err()
}

type DiscoverResult struct {
	Found []discover.Found `json:"found"`
	Notes []string         `json:"notes"`
	Plan  discover.Plan    `json:"plan"`
}

func (c *Client) Discover(ports string, ssh []string, includeEmbed, apply bool) (r DiscoverResult, err error) {
	err = c.do("POST", "/fleet/discover", map[string]any{"ports": ports, "ssh": ssh, "include_embed": includeEmbed, "apply": apply}, &r, 3*time.Minute)
	return
}

func (c *Client) SetDiscover(ports string, ssh, ignore []string) error {
	return c.do("POST", "/fleet/settings", map[string]any{"ports": ports, "ssh": ssh, "ignore": ignore}, nil, 10*time.Second)
}

// Bench starts a benchmark (names empty → every healthy chat endpoint). Progress: Status().Bench.
func (c *Client) Bench(names []string) error {
	return c.do("POST", "/fleet/bench", map[string]any{"names": names}, nil, 10*time.Second)
}

// SetCaps sets an endpoint's capabilities (empty = automatic detection).
func (c *Client) SetCaps(endpoint string, caps []string) error {
	if caps == nil {
		caps = []string{}
	}
	return c.do("POST", "/fleet/endpoint-set", map[string]any{"name": endpoint, "caps": caps}, nil, 10*time.Second)
}

// SetTags replaces an endpoint's tags (empty = none).
func (c *Client) SetTags(endpoint string, tags []string) error {
	if tags == nil {
		tags = []string{}
	}
	return c.do("POST", "/fleet/endpoint-set", map[string]any{"name": endpoint, "tags": tags}, nil, 10*time.Second)
}

// Runs lists the distributed runs every fleet reports, newest first.
func (c *Client) Runs() ([]server.Run, error) {
	var r struct {
		Runs []server.Run `json:"runs"`
	}
	err := c.do("GET", "/fleet/runs", nil, &r, 30*time.Second)
	return r.Runs, err
}

// RecordRun makes a record of candidate pick (1-based) of a plan; endpoint says which endpoint serves it ("": not yet).
func (c *Client) RecordRun(res plan.Result, pick int, endpoint, engine string) (server.Run, error) {
	var r server.Run
	err := c.do("POST", "/fleet/runs", map[string]any{"plan": res, "pick": pick, "endpoint": endpoint, "engine": engine}, &r, 15*time.Second)
	return r, err
}

// SetRun binds a run to an endpoint (endpoint != nil) and/or ends it (the processes are stopped too).
func (c *Client) SetRun(id string, endpoint *string, end bool) (server.Run, error) {
	var r server.Run
	err := c.do("POST", "/fleet/runs/set", map[string]any{"id": id, "endpoint": endpoint, "end": end}, &r, 15*time.Second)
	return r, err
}

// EndRunRecordOnly closes a run's record and leaves its processes running.
func (c *Client) EndRunRecordOnly(id string) (server.Run, error) {
	var r server.Run
	err := c.do("POST", "/fleet/runs/set", map[string]any{"id": id, "end": true, "keep_processes": true}, &r, 15*time.Second)
	return r, err
}

// NoteDeploy opens or updates the run of a deploy in the daemon's run store.
func (c *Client) NoteDeploy(n server.DeployNote) error {
	return c.do("POST", "/fleet/runs/deploy", n, nil, 10*time.Second)
}

// StopRun stops the processes of a run (master, hub streams, workers) and closes its record.
func (c *Client) StopRun(id string, opt server.StopOptions) (server.StopReport, error) {
	var r server.StopReport
	err := c.do("POST", "/fleet/runs/stop", map[string]any{"id": id, "dry_run": opt.DryRun, "force": opt.Force, "remove_endpoint": opt.Remove, "mend_roles": opt.MendRoles}, &r, 3*time.Minute)
	return r, err
}

// StopEndpoint stops the processes behind an endpoint (the active run that serves it, if any, is closed).
func (c *Client) StopEndpoint(name string, opt server.StopOptions) (server.StopReport, error) {
	var r server.StopReport
	err := c.do("POST", "/fleet/endpoints/stop", map[string]any{"name": name, "dry_run": opt.DryRun, "force": opt.Force, "remove_endpoint": opt.Remove, "mend_roles": opt.MendRoles}, &r, 3*time.Minute)
	return r, err
}

// DeleteRuns removes the records of ended runs: the one named by id, or all of them (id == ""). Active runs are
// never removed. With dry, nothing is deleted; the runs that would be are returned.
func (c *Client) DeleteRuns(id string, dry bool) ([]server.Run, error) {
	var r struct {
		Removed []server.Run `json:"removed"`
	}
	err := c.do("POST", "/fleet/runs/delete", map[string]any{"id": id, "prune": id == "", "dry_run": dry}, &r, 15*time.Second)
	return r.Removed, err
}

// EndStaleRuns marks the active runs that look dead (their engine has stopped answering and nothing ended them) lost and
// ended; it stops nothing. With dry, nothing is changed; the runs that would be are returned, with Stale saying why.
func (c *Client) EndStaleRuns(dry bool) ([]server.Run, error) {
	var r struct {
		Lost []server.Run `json:"lost"`
	}
	err := c.do("POST", "/fleet/runs/delete", map[string]any{"stale": true, "dry_run": dry}, &r, 30*time.Second)
	return r.Lost, err
}

// Reload reloads the daemon configuration from disk.
func (c *Client) Reload() error {
	return c.do("POST", "/fleet/reload", nil, nil, 10*time.Second)
}

// Drain leaves the line (on) or rejoins it, on the machine the daemon runs on.
func (c *Client) Drain(on bool) error {
	return c.do("POST", "/fleet/drain", map[string]any{"on": on}, nil, 15*time.Second)
}

// SetFleetPort says on which port the fleet of an endpoint's machine listens (0: 4711).
func (c *Client) SetFleetPort(endpoint string, port int) error {
	return c.do("POST", "/fleet/endpoint-set", map[string]any{"name": endpoint, "fleet_port": port}, nil, 10*time.Second)
}

// SetGroup declares (or, with an empty engine, removes) the group an endpoint's model is spread over, and
// sets its stall limit ("" keeps it, "off" disables, "90s").
func (c *Client) SetGroup(endpoint string, g config.Group, stall string) error {
	body := map[string]any{"name": endpoint, "group": g}
	if stall != "" {
		body["stall"] = stall
	}
	return c.do("POST", "/fleet/endpoint-set", body, nil, 10*time.Second)
}

// SetMatch replaces a role's selectors (empty = none); the role is created when it does not exist.
func (c *Client) SetMatch(role string, sel []string) error {
	if sel == nil {
		sel = []string{}
	}
	return c.UpdateRole(role, map[string]any{"match": sel})
}

// Usage returns the usage report grouped by client | role | model | endpoint | day.
// split adds a second grouping level (e.g. by=role split=model); "" for none.
func (c *Client) Usage(by, split, since string) ([]server.UsageRow, error) {
	return c.UsageFiltered(by, split, since, "")
}

// UsageFiltered is Usage with an optional client filter (e.g. "hub-tunnel" to exclude tunnel requests).
func (c *Client) UsageFiltered(by, split, since, excludeClient string) ([]server.UsageRow, error) {
	var r struct {
		Rows []server.UsageRow `json:"rows"`
	}
	q := url.Values{}
	q.Set("by", by)
	if split != "" {
		q.Set("split", split)
	}
	if since != "" {
		q.Set("since", since)
	}
	if excludeClient != "" {
		q.Set("exclude_client", excludeClient)
	}
	err := c.do("GET", "/fleet/usage?"+q.Encode(), nil, &r, 10*time.Second)
	return r.Rows, err
}

// HubUsage queries GET /fleet/tunnel/usage on the hub (authorized by the node's hub credential, no admin rights) using the provided hubURL and hubKey.
func (c *Client) HubUsage(ctx context.Context, hubURL, hubKey, by, split, since string) ([]server.UsageRow, error) {
	if hubURL == "" {
		return nil, fmt.Errorf("no hub url configured")
	}
	q := url.Values{}
	q.Set("by", by)
	if split != "" {
		q.Set("split", split)
	}
	if since != "" {
		q.Set("since", since)
	}
	u := strings.TrimRight(hubURL, "/") + "/fleet/tunnel/usage?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	if hubKey != "" {
		req.Header.Set("Authorization", "Bearer "+hubKey)
	}
	hc := &http.Client{Timeout: 5 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("hub returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var r struct {
		Rows []server.UsageRow `json:"rows"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, err
	}
	return r.Rows, nil
}

// Peers lists the configured peer fleets and their state.
func (c *Client) Peers() ([]server.PeerStatus, error) {
	var r struct {
		Peers []server.PeerStatus `json:"peers"`
	}
	err := c.do("GET", "/fleet/peers", nil, &r, 10*time.Second)
	return r.Peers, err
}

// Host returns what a node's machine is doing (CPU, RAM, GPUs, temperatures, disks), read by its fleet.
func (c *Client) Host(node string) (r server.HostReport, err error) {
	err = c.do("GET", "/fleet/host?node="+url.QueryEscape(node), nil, &r, 12*time.Second)
	return
}

// HostBench benchmarks this machine again (AI score); progress shows in Host("").Benching.
func (c *Client) HostBench() error {
	return c.do("POST", "/fleet/host/bench", nil, nil, 10*time.Second)
}

// Probe starts the capability probe of the named nodes or endpoints (none: every healthy one).
func (c *Client) Probe(names []string) error {
	return c.do("POST", "/fleet/probe", map[string]any{"names": names}, nil, 10*time.Second)
}

// ApplyProbe writes the probed capabilities into the endpoints' caps and returns the changes.
func (c *Client) ApplyProbe(names []string) ([]string, error) {
	var r struct {
		Changes []string `json:"changes"`
	}
	err := c.do("POST", "/fleet/probe", map[string]any{"names": names, "apply": true}, &r, 10*time.Second)
	return r.Changes, err
}

// LAN lists the fleets the daemon hears on the local network (mDNS) that are not this one.
func (c *Client) LAN() ([]server.LANFleet, error) {
	var r struct {
		LAN []server.LANFleet `json:"lan"`
	}
	err := c.do("GET", "/fleet/peers", nil, &r, 10*time.Second)
	return r.LAN, err
}

// PairCode opens a pairing code on this fleet (fleet peers pair).
func (c *Client) PairCode() (pc server.PairCode, err error) {
	err = c.do("POST", "/fleet/pair/code", nil, &pc, 10*time.Second)
	return
}

// PeerPair adds a peer by its address and the pairing code it shows; the daemon fetches the peer's key.
func (c *Client) PeerPair(p *config.Peer, code string) ([]server.PeerStatus, error) {
	var r struct {
		Peers []server.PeerStatus `json:"peers"`
	}
	err := c.do("POST", "/fleet/peers", map[string]any{"add": p, "pair": code}, &r, 60*time.Second)
	return r.Peers, err
}

// PeerOp adds (p != nil), removes (remove != "") or re-syncs (sync) peers and returns the new state.
func (c *Client) PeerOp(p *config.Peer, remove string, sync bool) ([]server.PeerStatus, error) {
	var r struct {
		Peers []server.PeerStatus `json:"peers"`
	}
	err := c.do("POST", "/fleet/peers", map[string]any{"add": p, "remove": remove, "sync": sync}, &r, 60*time.Second)
	return r.Peers, err
}

// TestResult is one probe of TestRole.
type TestResult struct {
	Target   string // the role (through the router) or one endpoint
	Endpoint string // who answered
	Answer   string
	Took     time.Duration
	Err      error
}

// TestRole sends a tiny prompt to the role through the router (so you see who answers and that routing works)
// and to each member directly (so you see which ones are really alive). The probes run in parallel.
func (c *Client) TestRole(role string, members []string) []TestResult {
	targets := append([]string{role}, members...)
	out := make([]TestResult, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func(i int, t string) {
			defer wg.Done()
			start := time.Now()
			ans, ep, err := c.Chat(t, "", "Reply with exactly one word: pong", 256, 90*time.Second)
			out[i] = TestResult{Target: t, Endpoint: ep, Answer: strings.TrimSpace(ans), Took: time.Since(start), Err: err}
		}(i, t)
	}
	wg.Wait()
	return out
}

// FormatTest renders TestRole results, one line per probe.
func FormatTest(rs []TestResult) []string {
	var out []string
	for i, r := range rs {
		who := r.Target
		if i == 0 {
			who = "via router  (" + r.Target + " → " + r.Endpoint + ")"
		}
		if r.Err != nil {
			msg := r.Err.Error()
			if len(msg) > 90 {
				msg = msg[:90] + "…"
			}
			out = append(out, fmt.Sprintf("  ✗ %-60s %s", who, msg))
			continue
		}
		out = append(out, fmt.Sprintf("  ✓ %-60s %5.1fs  %q", who, r.Took.Seconds(), r.Answer))
	}
	return out
}

// Route asks where a request for model (a role or a model name) would go and what each candidate would cost.
func (c *Client) Route(model string, in, out int) (server.RouteExplain, error) {
	var r server.RouteExplain
	q := url.Values{"model": {model}, "in": {strconv.Itoa(in)}, "out": {strconv.Itoa(out)}}
	err := c.do("GET", "/fleet/route?"+q.Encode(), nil, &r, 10*time.Second)
	return r, err
}

// Plan asks where a model would run best: on one machine or split over several (nothing is started).
func (c *Client) Plan(req server.PlanRequest) (plan.Result, error) {
	var r plan.Result
	err := c.do("POST", "/fleet/plan", req, &r, 30*time.Second)
	return r, err
}

// PlanEvaluate checks and prices a split whose blocks were placed by hand.
func (c *Client) PlanEvaluate(req server.EvaluateRequest) (plan.Evaluation, error) {
	var r plan.Evaluation
	err := c.do("POST", "/fleet/plan/evaluate", req, &r, 30*time.Second)
	return r, err
}

// StoreAdd registers model files (or the .gguf files of a directory) in the daemon's machine store; hashing runs
// in the background (see StoreLocal for its progress). It returns how many files were queued.
func (c *Client) StoreAdd(paths []string) (int, error) {
	var r struct {
		Queued int `json:"queued"`
	}
	err := c.do("POST", "/fleet/store/add", map[string]any{"paths": paths}, &r, 10*time.Second)
	return r.Queued, err
}

// PullAll has node fetch every file in shas, one after the other (a machine runs one transfer at a time), and
// returns when the last one has ended. A failed transfer stops the sequence with its error; running it again resumes.
func (c *Client) PullAll(node string, shas []string) error {
	return c.transferAll(node, shas, func(sha string) error { return c.StorePull(node, "", sha) })
}

// FillAll is PullAll for worker-cache fills: the same blocks of every file (a split model's shards).
func (c *Client) FillAll(node string, shas []string, blocks string, output bool) error {
	return c.transferAll(node, shas, func(sha string) error { return c.StoreFill(node, "", sha, blocks, output) })
}

func (c *Client) transferAll(node string, shas []string, start func(sha string) error) error {
	for _, sha := range shas {
		before := 0
		if nodes, err := c.StoreAll(); err == nil {
			for _, n := range nodes {
				if n.Store != nil && (n.Node == node || (node == "" && n.Source == "this machine")) {
					for _, t := range n.Store.Transfers {
						before = max(before, t.ID)
					}
				}
			}
		}
		if err := start(sha); err != nil {
			return err
		}
		began := time.Now()
		for {
			time.Sleep(900 * time.Millisecond)
			nodes, err := c.StoreAll()
			if err != nil {
				return err
			}
			running, fresh := false, false
			for _, n := range nodes {
				if n.Store == nil || !(n.Node == node || (node == "" && n.Source == "this machine")) {
					continue
				}
				for _, t := range n.Store.Transfers {
					if t.ID <= before {
						continue
					}
					fresh = true
					if t.Running {
						running = true
					} else if t.Err != "" {
						return fmt.Errorf("%s", t.Err)
					}
				}
			}
			if !running && (fresh || time.Since(began) > 5*time.Second) {
				break
			}
		}
	}
	return nil
}

// StoreLink asks which line (lan, wan, vpn, peer, unknown) a pull of sha onto node would take.
func (c *Client) StoreLink(node, from, sha string) (server.LinkInfo, error) {
	var li server.LinkInfo
	err := c.do("POST", "/fleet/store/link", map[string]any{"node": node, "from": from, "sha256": sha}, &li, 20*time.Second)
	return li, err
}

// StoreDelete removes a file fleet fetched itself from the disk (and forgets it); force: although an endpoint serves it.
func (c *Client) StoreDelete(ref string, force bool) (store.Entry, error) {
	var e store.Entry
	err := c.do("POST", "/fleet/store/rm", map[string]any{"ref": ref, "delete_file": true, "force": force}, &e, 30*time.Second)
	return e, err
}

// StorePrune forgets the registered files that are gone or changed.
func (c *Client) StorePrune() ([]store.Entry, error) {
	var r struct {
		Forgotten []store.Entry `json:"forgotten"`
	}
	err := c.do("POST", "/fleet/store/prune", nil, &r, 30*time.Second)
	return r.Forgotten, err
}

// StoreRemove forgets a registered file (by sha256 prefix or name); the file itself stays.
func (c *Client) StoreRemove(ref string) (store.Entry, error) {
	var e store.Entry
	err := c.do("POST", "/fleet/store/rm", map[string]any{"ref": ref}, &e, 10*time.Second)
	return e, err
}

// StoreRemoveOn forgets a registered file on another machine (the daemon forwards it over its ssh route); node "" is this one.
func (c *Client) StoreRemoveOn(node, ref string) (store.Entry, error) {
	var e store.Entry
	err := c.do("POST", "/fleet/store/rm", map[string]any{"ref": ref, "node": node}, &e, 30*time.Second)
	return e, err
}

// StoreLocal is what the daemon's own machine has: files, hashing progress, worker cache.
func (c *Client) StoreLocal() (store.Inventory, error) {
	var inv store.Inventory
	err := c.do("GET", "/fleet/store", nil, &inv, 10*time.Second)
	return inv, err
}

// StoreAll is what every machine has, as its own fleet reports it.
func (c *Client) StoreAll() ([]server.NodeStore, error) {
	var r struct {
		Nodes []server.NodeStore `json:"nodes"`
	}
	err := c.do("GET", "/fleet/store/all", nil, &r, 30*time.Second)
	return r.Nodes, err
}

// StorePull asks the machine node ("" : the daemon's own) to fetch a file it lacks, from a machine that has it
// (from: "" tries them all). The transfer runs in the background: see StoreAll for its progress.
func (c *Client) StorePull(node, from, sha string) error {
	return c.do("POST", "/fleet/store/pull", server.PullRequest{Node: node, From: from, SHA: sha}, nil, 40*time.Second)
}

// StoreFill asks the machine node to fill its llama-ring worker cache with the tensors of some blocks.
func (c *Client) StoreFill(node, from, sha, blocks string, output bool) error {
	return c.do("POST", "/fleet/store/fill", server.FillRequest{Node: node, From: from, SHA: sha, Blocks: blocks, Output: output}, nil, 40*time.Second)
}

// StoreAbort asks node ("" : the daemon's own machine) to abort any running transfer (pull or cache fill).
func (c *Client) StoreAbort(node string) error {
	return c.do("POST", "/fleet/store/abort", server.AbortRequest{Node: node}, nil, 10*time.Second)
}

// StoreDiscard is StoreAbort plus deleting the unfinished copies on node and forgetting the finished transfers.
func (c *Client) StoreDiscard(node string) error {
	return c.do("POST", "/fleet/store/abort", server.AbortRequest{Node: node, Discard: true}, nil, 15*time.Second)
}

// ListAPIKeys retrieves all registered API keys and usage statistics from the daemon.
func (c *Client) ListAPIKeys() ([]*server.APIKey, error) {
	var r struct {
		Keys []*server.APIKey `json:"keys"`
	}
	err := c.do("GET", "/fleet/apikeys", nil, &r, 5*time.Second)
	return r.Keys, err
}

// CreateAPIKey requests the daemon to create and register a new API key.
func (c *Client) CreateAPIKey(name, qos, role string, models ...string) (*server.APIKey, error) {
	var ak server.APIKey
	payload := map[string]any{
		"name":   name,
		"qos":    qos,
		"role":   role,
		"models": models,
	}
	err := c.do("POST", "/fleet/apikeys", payload, &ak, 5*time.Second)
	return &ak, err
}

// DeleteAPIKey removes an API key from the daemon.
func (c *Client) DeleteAPIKey(key string) error {
	return c.do("DELETE", "/fleet/apikeys/"+key, nil, nil, 5*time.Second)
}

// JoinHub connects this node to a remote Fleet Hub over a reverse WebSocket tunnel. streamPorts are the
// local TCP ports the hub may open raw streams to (none: the daemon's hub_stream_ports).
func (c *Client) JoinHub(hubURL, token string, streamPorts ...int) error {
	payload := map[string]any{"hub_url": hubURL, "token": token}
	if len(streamPorts) > 0 {
		payload["stream_ports"] = streamPorts
	}
	return c.do("POST", "/fleet/tunnel/join", payload, nil, 10*time.Second)
}

// JoinHubAdd joins hubURL like JoinHub, but addStreamPorts are added to the ports the daemon already allows
// (hub_stream_ports) instead of replacing them; an empty token keeps the remembered token of the same hub.
func (c *Client) JoinHubAdd(hubURL, token string, addStreamPorts []int) error {
	payload := map[string]any{"hub_url": hubURL, "token": token, "add_stream_ports": addStreamPorts}
	return c.do("POST", "/fleet/tunnel/join", payload, nil, 10*time.Second)
}

// LeaveHub disconnects this node from the active Fleet Hub.
func (c *Client) LeaveHub() error {
	return c.do("POST", "/fleet/tunnel/leave", nil, nil, 5*time.Second)
}

// TunnelStatus checks if this node is currently connected to a Hub.
func (c *Client) TunnelStatus() (bool, string, error) {
	var res struct {
		Connected bool   `json:"connected"`
		HubURL    string `json:"hub_url"`
	}
	err := c.do("GET", "/fleet/tunnel/status", nil, &res, 5*time.Second)
	return res.Connected, res.HubURL, err
}

// HubStatus is the daemon's view of the hub tunnel: this node's side (connected, last error, next retry) and, on a hub,
// the nodes joined to it and the streams open.
type HubStatus struct {
	Connected     bool                      `json:"connected"`
	HubURL        string                    `json:"hub_url"`
	AutoJoin      bool                      `json:"auto_join"`
	Insecure      bool                      `json:"insecure"`
	Status        tunnel.Status             `json:"status"`
	Nodes         []tunnel.NodeRegistration `json:"nodes"`
	ActiveStreams map[string]int            `json:"active_streams"`
}

// HubStatus reads GET /fleet/tunnel/status in full.
func (c *Client) HubStatus() (HubStatus, error) {
	var res HubStatus
	err := c.do("GET", "/fleet/tunnel/status", nil, &res, 5*time.Second)
	return res, err
}

// JoinHubInsecure joins like JoinHub and sets whether the hub's TLS certificate is verified (insecure=true skips it).
func (c *Client) JoinHubInsecure(hubURL, token string, insecure bool, streamPorts, addStreamPorts []int) error {
	payload := map[string]any{"hub_url": hubURL, "token": token, "insecure": insecure}
	if len(streamPorts) > 0 {
		payload["stream_ports"] = streamPorts
	}
	if len(addStreamPorts) > 0 {
		payload["add_stream_ports"] = addStreamPorts
	}
	return c.do("POST", "/fleet/tunnel/join", payload, nil, 10*time.Second)
}

// SetEconomySettings updates the daemon's token economy configuration.
func (c *Client) SetEconomySettings(econ config.EconomyConfig) error {
	return c.do("POST", "/fleet/economy/settings", econ, nil, 5*time.Second)
}

// SetHubAPIKey stores the hub API key (not the tunnel join token) in the daemon's config.
func (c *Client) SetHubAPIKey(key string) error {
	return c.do("POST", "/fleet/hub/settings", map[string]any{"hub_api_key": key}, nil, 5*time.Second)
}

// SetHubSettings updates the daemon's Hub configuration (Hub URL and optional auth token).
func (c *Client) SetHubSettings(hubURL, hubToken string) error {
	payload := map[string]string{
		"hub_url":   hubURL,
		"hub_token": hubToken,
	}
	return c.do("POST", "/fleet/hub/settings", payload, nil, 5*time.Second)
}

// History lists the kept earlier configurations, oldest first.
func (c *Client) History() ([]string, error) {
	var out struct {
		Snapshots []string `json:"snapshots"`
	}
	err := c.do("GET", "/fleet/history", nil, &out, 10*time.Second)
	return out.Snapshots, err
}

// SetNodeSpec writes the nodes: entry of a machine (fleet config node set), or with remove drops it.
func (c *Client) SetNodeSpec(name string, spec config.NodeSpec, remove bool) error {
	return c.do("POST", "/fleet/node-spec", map[string]any{"name": name, "spec": spec, "remove": remove}, nil, 10*time.Second)
}

package main

import (
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/board"
	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/tunnel"
)

// G4 / G9: End-to-end integration test exercising the REAL hub -> WebSocket tunnel -> node path.
// The test never sets "X-Fleet-Client: hub-tunnel" itself: tunnel.Client injects it when forwarding
// hub requests to the local node daemon, so the node logs "hub-tunnel", the hub logs the real client
// ("alice-client") and "node-worker/tiny", and `fleet usage --hub` counts the request once.
func TestHubTunnelUsage_CombinedCountsOnce(t *testing.T) {
	dir := isolate(t)
	be := newUpstream(t)

	const hubKey = "test-hub-key-xyz"
	t.Setenv("TEST_HUB_USAGE_KEY", hubKey)

	// 1. Real Hub daemon (no local model endpoints: it can only serve node-worker/tiny over the tunnel)
	t.Setenv("FLEET_STATS", filepath.Join(dir, "hub-state", "stats.json"))
	hubCfg := &config.Config{
		Name:          "bravo-hub",
		APIKeyEnv:     "TEST_HUB_USAGE_KEY",
		HubRequireKey: true,
	}
	hubDaemon := startDaemon(t, hubCfg, false)

	// 2. Real Node daemon (primary: CLI commands talk to it, its own stats file)
	t.Setenv("FLEET_STATS", filepath.Join(dir, "node-state", "stats.json"))
	nodeCfg := baseConfig(be)
	nodeCfg.Name = "node-worker"
	for i := range nodeCfg.Endpoints {
		nodeCfg.Endpoints[i].Node = "node-worker"
	}
	nodeCfg.HubURL = "http://" + hubDaemon.addr
	nodeCfg.HubAPIKey = hubKey
	nodeDaemon := startDaemon(t, nodeCfg, true)
	nodeDaemon.waitHealthy(t, "loc-tiny")

	// 3. Join node-worker to bravo-hub via the real outbound WebSocket tunnel client
	tun, err := tunnel.StartClientOpts(
		"http://"+hubDaemon.addr,
		"http://"+nodeDaemon.addr,
		tunnel.NodeRegistration{
			NodeName: "node-worker",
			Models:   []string{"tiny"},
			JoinedAt: time.Now(),
		},
		tunnel.Options{APIKey: hubKey},
	)
	if err != nil {
		t.Fatalf("StartClientOpts: %v", err)
	}
	t.Cleanup(tun.Close)

	// Wait until the hub sees node-worker connected
	nodeJoined := false
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(15 * time.Millisecond) {
		if hs, err := hubDaemon.cl.HubStatus(); err == nil {
			for _, n := range hs.Nodes {
				if n.NodeName == "node-worker" {
					nodeJoined = true
					break
				}
			}
		}
		if nodeJoined {
			break
		}
	}
	if !nodeJoined {
		t.Fatal("hub never registered tunnel node node-worker")
	}

	sendChat := func(addr, model, clientName, bearer string) string {
		t.Helper()
		body := `{"model":"` + model + `","messages":[{"role":"user","content":"hello"}]}`
		req, err := http.NewRequest("POST", "http://"+addr+"/v1/chat/completions", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Fleet-Client", clientName)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST %s/v1/chat/completions (%s): status %d body %s", addr, model, resp.StatusCode, b)
		}
		return string(b)
	}

	// (1) Direct local request on the node
	sendChat(nodeDaemon.addr, "loc-tiny", "local-cli", "")

	// (2) Real hub request from "alice-client" routed over the WebSocket tunnel to node-worker
	hubResp := sendChat(hubDaemon.addr, "node-worker/tiny", "alice-client", hubKey)
	if !strings.Contains(hubResp, "echo[]: hello") {
		t.Fatalf("unexpected tunnel response body: %s", hubResp)
	}

	// Wait for records to settle in both independent usage stores
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		rLocal, _ := nodeDaemon.cl.Usage("client", "", "24h")
		rHub, _ := hubDaemon.cl.Usage("client", "", "24h")
		totLocal := 0
		for _, r := range rLocal {
			totLocal += r.Requests
		}
		if totLocal >= 2 && len(rHub) >= 1 {
			break
		}
	}

	// (A) Local raw usage has local-cli (1) and hub-tunnel (1), and never alice-client
	rawLocal, err := nodeDaemon.cl.Usage("client", "", "24h")
	if err != nil {
		t.Fatal(err)
	}
	seenRaw := map[string]int{}
	for _, r := range rawLocal {
		seenRaw[r.Key] = r.Requests
	}
	if seenRaw["local-cli"] != 1 || seenRaw["hub-tunnel"] != 1 || seenRaw["alice-client"] != 0 {
		t.Fatalf("local raw usage want local-cli=1 hub-tunnel=1 alice-client=0, got: %+v", seenRaw)
	}

	// (B) Filtered local usage (exclude_client=hub-tunnel) has ONLY local-cli
	filteredLocal, err := nodeDaemon.cl.UsageFiltered("client", "", "24h", "hub-tunnel")
	if err != nil {
		t.Fatal(err)
	}
	if len(filteredLocal) != 1 || filteredLocal[0].Key != "local-cli" || filteredLocal[0].Requests != 1 {
		t.Fatalf("local filtered usage should only have local-cli: %+v", filteredLocal)
	}

	// (C) Hub usage has alice-client with real token counts (5 in, 3 out) and model node-worker/tiny
	hubUsage, err := hubDaemon.cl.Usage("client", "", "24h")
	if err != nil {
		t.Fatal(err)
	}
	if len(hubUsage) != 1 || hubUsage[0].Key != "alice-client" || hubUsage[0].Requests != 1 || hubUsage[0].In != 5 || hubUsage[0].Out != 3 {
		t.Fatalf("hub client usage want alice-client (1 req, 5 in, 3 out), got: %+v", hubUsage)
	}
	hubModels, err := hubDaemon.cl.Usage("model", "", "24h")
	if err != nil {
		t.Fatal(err)
	}
	if len(hubModels) != 1 || hubModels[0].Key != "node-worker/tiny" || hubModels[0].Requests != 1 {
		t.Fatalf("hub model usage want node-worker/tiny (1 req), got: %+v", hubModels)
	}

	// (D) Combined usage via board.CombineUsage counts the tunnel request ONCE (1 local + 1 hub = 2)
	combined := board.CombineUsage(filteredLocal, hubUsage)
	var totReq, totIn, totOut int
	foundLocal, foundAlice := false, false
	for _, r := range combined {
		if r.Key == "local-cli" && r.Source == "local" {
			foundLocal = true
		}
		if r.Key == "alice-client" && r.Source == "hub" {
			foundAlice = true
		}
		if r.Key == "TOTAL" && r.Source == "combined" {
			totReq, totIn, totOut = r.Requests, r.In, r.Out
		}
	}
	if !foundLocal || !foundAlice {
		t.Fatalf("combined report missing local or hub row: %+v", combined)
	}
	if totReq != 2 || totIn != 10 || totOut != 6 {
		t.Fatalf("combined total want req=2 in=10 out=6, got req=%d in=%d out=%d (%+v)", totReq, totIn, totOut, combined)
	}

	// (E) End-to-end CLI `fleet usage --by client --hub` talks to /fleet/tunnel/usage with streamAuth
	out := runOK(t, func() error { return cmdUsage([]string{"--by", "client", "--hub"}) })
	for _, want := range []string{"SOURCE", "local", "local-cli", "hub", "alice-client", "TOTAL"} {
		if !strings.Contains(out, want) {
			t.Fatalf("cmdUsage --hub missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "hub-tunnel") {
		t.Fatalf("cmdUsage --hub leaked internal hub-tunnel client into output:\n%s", out)
	}
}

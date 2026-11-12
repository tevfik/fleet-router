package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/tunnel"
)

func newHubDaemon(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "hub.json")
	if err := (&config.Config{Name: "hub"}).Save(p); err != nil {
		t.Fatal(err)
	}
	s, err := New(p)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

func newNodeDaemon(t *testing.T, c *config.Config) (*Server, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "node.json")
	if err := c.Save(p); err != nil {
		t.Fatal(err)
	}
	s, err := New(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.leaveHub)
	return s, p
}

func waitNode(t *testing.T, hub *Server, name string) tunnel.NodeRegistration {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, n := range hub.hub.ConnectedNodes() {
			if n.NodeName == name {
				return n
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("node %q never joined the hub", name)
	return tunnel.NodeRegistration{}
}

// A daemon whose configuration remembers a join joins again at start, with its token and stream ports, and keeps
// retrying while the hub cannot be reached.
func TestAutoJoinAtStartRetries(t *testing.T) {
	hub, hubSrv := newHubDaemon(t)
	node, _ := newNodeDaemon(t, &config.Config{Name: "worker", HubURL: hubSrv.URL, HubToken: "tok", HubStreamPorts: []int{50160}, HubAutoJoin: true})
	oldF, oldW := startTunnelClient, autoJoinFirstWait
	var calls atomic.Int32
	startTunnelClient = func(u, l string, r tunnel.NodeRegistration, o tunnel.Options) (*tunnel.Client, error) {
		if calls.Add(1) <= 2 {
			return nil, errors.New("hub down")
		}
		return tunnel.StartClientOpts(u, l, r, o)
	}
	autoJoinFirstWait = 10 * time.Millisecond
	t.Cleanup(func() { startTunnelClient, autoJoinFirstWait = oldF, oldW })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { node.autoJoinLoop(ctx); close(done) }()
	reg := waitNode(t, hub, "worker")
	if len(reg.StreamPorts) != 1 || reg.StreamPorts[0] != 50160 {
		t.Errorf("stream ports not restored: %v", reg.StreamPorts)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the loop must end once joined")
	}
	if calls.Load() != 3 {
		t.Errorf("want 2 failures then a join, got %d calls", calls.Load())
	}
}

func TestNoAutoJoinWithoutTheFlag(t *testing.T) {
	_, hubSrv := newHubDaemon(t)
	node, _ := newNodeDaemon(t, &config.Config{Name: "worker", HubURL: hubSrv.URL})
	old := startTunnelClient
	var calls atomic.Int32
	startTunnelClient = func(u, l string, r tunnel.NodeRegistration, o tunnel.Options) (*tunnel.Client, error) {
		calls.Add(1)
		return nil, errors.New("must not be called")
	}
	t.Cleanup(func() { startTunnelClient = old })
	node.autoJoinLoop(context.Background())
	if calls.Load() != 0 {
		t.Error("hub_url alone must not join (only fleet join sets hub_auto_join)")
	}
}

// A leave while the auto-join is still retrying stops it: the node must not come back on its own.
func TestLeaveStopsAutoJoin(t *testing.T) {
	_, hubSrv := newHubDaemon(t)
	node, _ := newNodeDaemon(t, &config.Config{Name: "worker", HubURL: hubSrv.URL, HubAutoJoin: true})
	oldF, oldW := startTunnelClient, autoJoinFirstWait
	var calls atomic.Int32
	startTunnelClient = func(u, l string, r tunnel.NodeRegistration, o tunnel.Options) (*tunnel.Client, error) {
		calls.Add(1)
		return nil, errors.New("hub down")
	}
	autoJoinFirstWait = 20 * time.Millisecond
	t.Cleanup(func() { startTunnelClient, autoJoinFirstWait = oldF, oldW })
	done := make(chan struct{})
	go func() { node.autoJoinLoop(context.Background()); close(done) }()
	time.Sleep(30 * time.Millisecond)
	node.leaveHub()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("leave must end the retry loop")
	}
}

// POST /fleet/tunnel/join saves the join (hub_auto_join with url, token, ports); leave clears the flag.
func TestJoinIsRememberedAndLeaveForgets(t *testing.T) {
	hub, hubSrv := newHubDaemon(t)
	node, path := newNodeDaemon(t, &config.Config{Name: "worker"})
	nodeSrv := httptest.NewServer(node.Handler())
	defer nodeSrv.Close()
	body := `{"hub_url":"` + hubSrv.URL + `","token":"tok","stream_ports":[50160,50199]}`
	resp, err := http.Post(nodeSrv.URL+"/fleet/tunnel/join", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("join: %d", resp.StatusCode)
	}
	waitNode(t, hub, "worker")
	c, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !c.HubAutoJoin || c.HubURL != hubSrv.URL || c.HubToken != "tok" || len(c.HubStreamPorts) != 2 {
		t.Errorf("join not saved: auto %v url %q token %q ports %v", c.HubAutoJoin, c.HubURL, c.HubToken, c.HubStreamPorts)
	}
	resp, err = http.Post(nodeSrv.URL+"/fleet/tunnel/leave", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	c, _ = config.Load(path)
	if c.HubAutoJoin {
		t.Error("leave must clear hub_auto_join")
	}
	if c.HubURL != hubSrv.URL {
		t.Error("leave keeps the hub url (only the auto-join goes)")
	}
}

// add_stream_ports adds to the remembered hub_stream_ports (no duplicates) and keeps the remembered token of the
// same hub: fleet deploy declares a worker's ring port this way without dropping the node's other ports.
func TestJoinAddStreamPortsMerges(t *testing.T) {
	hub, hubSrv := newHubDaemon(t)
	node, path := newNodeDaemon(t, &config.Config{Name: "worker", HubURL: hubSrv.URL, HubToken: "tok", HubStreamPorts: []int{50160}})
	nodeSrv := httptest.NewServer(node.Handler())
	defer nodeSrv.Close()
	body := `{"hub_url":"` + hubSrv.URL + `","add_stream_ports":[50152,50160]}`
	resp, err := http.Post(nodeSrv.URL+"/fleet/tunnel/join", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("join: %d", resp.StatusCode)
	}
	reg := waitNode(t, hub, "worker")
	if len(reg.StreamPorts) != 2 || reg.StreamPorts[0] != 50160 || reg.StreamPorts[1] != 50152 {
		t.Errorf("registered ports %v, want [50160 50152]", reg.StreamPorts)
	}
	c, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.HubToken != "tok" || len(c.HubStreamPorts) != 2 || !c.HubAutoJoin {
		t.Errorf("saved: token %q ports %v auto %v", c.HubToken, c.HubStreamPorts, c.HubAutoJoin)
	}
}

func TestHubStatusNeverShowsJoinTokens(t *testing.T) {
	hub, hubSrv := newHubDaemon(t)
	node, _ := newNodeDaemon(t, &config.Config{Name: "worker", HubURL: hubSrv.URL, HubToken: "join-secret-token", HubAutoJoin: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go node.autoJoinLoop(ctx)
	waitNode(t, hub, "worker")
	b, _ := json.Marshal(hub.hubStatus())
	if strings.Contains(string(b), "join-secret-token") {
		t.Fatalf("the status shows a join token: %s", b)
	}
}

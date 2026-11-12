package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/feature"
)

// A configuration written by a fleet with the runner (deploys, chains, node specs, relays) loaded by one without it.
const runnerConfig = `listen: 127.0.0.1:0
auto_update: false
supervise_chains: true
ring_cache: /tmp/ring-cache
nodes:
  gpu1: {addr: 10.0.0.2, mem_gib: 24, weight_gbps: 900}
endpoints:
  - name: plain
    url: http://127.0.0.1:1/v1
    model: qwen3:8b
    node: gpu1
  - name: chain-ep
    url: http://127.0.0.1:2/v1
    model: big
    node: gpu1
    stall: 2m
    group:
      engine: llama-ring
      workers: [gpu2, gpu3]
      addrs: {gpu2: "10.0.0.3:50052"}
      relays: [{via: gpu2, from: gpu1, to: gpu3, port: 51000}]
      stages: [{node: gpu2, addr: "10.0.0.3:9100"}, {node: gpu3, addr: "10.0.0.4:9100"}]
roles:
  coder: {preferred: plain}
`

// The router-only fleet must keep what it does not use: a machine that loses fleet-engine (or a config copied to one
// without it) and saves its configuration must not drop the deploys' settings.
func TestCompat_LiteKeepsRunnerConfiguration(t *testing.T) {
	defer feature.SetRunner(false)()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "cache"))
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(runnerConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(path)
	if err != nil {
		t.Fatalf("a lite fleet cannot start on a runner configuration: %v", err)
	}
	c := s.Config()
	c.Roles["helper"] = config.Role{Preferred: "plain"} // a change the router side makes, saved through SetConfig
	if err := s.SetConfig(c); err != nil {
		t.Fatal(err)
	}
	after, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.Nodes, after.Nodes) || !reflect.DeepEqual(before.SuperviseChains, after.SuperviseChains) ||
		before.RingCache != after.RingCache {
		t.Errorf("runner settings changed:\nbefore %+v %v %q\nafter  %+v %v %q", before.Nodes, *before.SuperviseChains, before.RingCache,
			after.Nodes, after.SuperviseChains, after.RingCache)
	}
	if ep := after.Endpoint("chain-ep"); ep == nil || !reflect.DeepEqual(ep.Group, before.Endpoint("chain-ep").Group) || ep.Stall != "2m" {
		t.Errorf("the chain endpoint lost its group/stall: %+v", ep)
	}
	if _, ok := after.Roles["helper"]; !ok {
		t.Error("the change itself was not saved")
	}
}

// A fleet with the runner asks every other machine's fleet for its host report (runs, model files): a router-only fleet
// must still answer it, so a mixed fleet (engine hosts + plain machines) keeps working.
func TestCompat_LitePeerAnswersHostReport(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "cache"))
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(runnerConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	restore := feature.SetRunner(false)
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler() // routes are fixed when the handler is built
	restore()
	ts := httptest.NewServer(h)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/fleet/host")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /fleet/host on a lite fleet: %d", resp.StatusCode)
	}
	var rep HostReport
	if err := json.NewDecoder(resp.Body).Decode(&rep); err != nil {
		t.Fatalf("the host report does not decode for a runner fleet: %v", err)
	}
	if len(rep.Runs) != 0 {
		t.Errorf("a lite fleet reports runs: %+v", rep.Runs)
	}
}

// The status tells clients whether this daemon has the runner side (a newer client words its errors by it; an older
// one ignores the field).
func TestCompat_StatusSaysRunner(t *testing.T) {
	s := &Server{cfg: &config.Config{}, state: map[string]*EpState{}, lims: map[string]*limiter{}, client: http.DefaultClient}
	for _, on := range []bool{false, true} {
		restore := feature.SetRunner(on)
		st := s.Status()
		restore()
		if st.Runner == nil || *st.Runner != on {
			t.Errorf("runner %v: status says %v", on, st.Runner)
		}
	}
}

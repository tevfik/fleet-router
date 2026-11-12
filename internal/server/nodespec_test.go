package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/tevfik/fleet-router/internal/config"
)

func postJSON(t *testing.T, url string, body any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestNodeSpecEndpointSetsAndRemoves(t *testing.T) {
	s, ts := newHubDaemon(t)
	code, _ := postJSON(t, ts.URL+"/fleet/node-spec", map[string]any{"name": "dino", "spec": config.NodeSpec{Addr: "10.0.0.2", MemGiB: 96, WeightGBps: 250, ExpertGBps: 60}})
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	if got := s.Config().Nodes["dino"]; got.Addr != "10.0.0.2" || got.MemGiB != 96 || got.ExpertGBps != 60 {
		t.Fatalf("%+v", got)
	}
	// it is on disk too (what fleet config node list reads)
	if c, err := config.Load(s.path); err != nil || c.Nodes["dino"].WeightGBps != 250 {
		t.Fatalf("%v %+v", err, c)
	}
	if code, _ = postJSON(t, ts.URL+"/fleet/node-spec", map[string]any{"name": "dino", "spec": config.NodeSpec{MemGiB: -1}}); code != 400 {
		t.Fatalf("negative memory: %d", code)
	}
	if code, _ = postJSON(t, ts.URL+"/fleet/node-spec", map[string]any{"name": "", "spec": config.NodeSpec{}}); code != 400 {
		t.Fatalf("no name: %d", code)
	}
	if code, _ = postJSON(t, ts.URL+"/fleet/node-spec", map[string]any{"name": "nope", "remove": true}); code != 404 {
		t.Fatalf("unknown node: %d", code)
	}
	if code, _ = postJSON(t, ts.URL+"/fleet/node-spec", map[string]any{"name": "dino", "remove": true}); code != 200 || len(s.Config().Nodes) != 0 {
		t.Fatalf("remove: %d %+v", code, s.Config().Nodes)
	}
}

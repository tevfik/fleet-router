package discover

import (
	"testing"

	"github.com/tevfik/fleet-router/internal/config"
)

func TestMakePlanPortMatchIsExact(t *testing.T) {
	cfg := &config.Config{Endpoints: []config.Endpoint{
		{Name: "a", Node: "dino", URL: "http://127.0.0.1:30000/v1", Model: "m"},
		{Name: "b", Node: "dino", SSH: "dino", Remote: "127.0.0.1:30000", Model: "n"},
	}}
	pl := MakePlan(cfg, []Found{{Node: "dino", Port: 3000, Models: []string{"m", "n"}}}, true)
	if len(pl.Known) != 0 || len(pl.Add) != 2 {
		t.Fatalf("port 3000 must not be mistaken for 30000: known=%v add=%v", pl.Known, pl.Add)
	}
	pl = MakePlan(cfg, []Found{{Node: "dino", Port: 30000, Models: []string{"m", "n"}}}, true)
	if len(pl.Known) != 2 || len(pl.Add) != 0 {
		t.Fatalf("the same host:port and model is already registered: known=%v add=%v", pl.Known, pl.Add)
	}
}

func TestMakePlanFleetsBecomePeers(t *testing.T) {
	cfg := &config.Config{ID: "self-id", Peers: []config.Peer{{Name: "old", SSH: "old-host"}}}
	pl := MakePlan(cfg, []Found{
		{Node: "bravo", SSH: "bravo", Port: FleetPort, Fleet: &FleetInfo{ID: "b1", Name: "bravo"}},
		{Node: "me", Port: 4711, Fleet: &FleetInfo{ID: "self-id", Name: "me"}}, // this very fleet
		{Node: "old-host", SSH: "old-host", Port: FleetPort, Fleet: &FleetInfo{ID: "o1"}},
		{Node: "dino", SSH: "dino", Port: 4800, Fleet: &FleetInfo{ID: "d1"}}, // a fleet on a non-default port
	}, false)
	if len(pl.Add) != 0 {
		t.Fatalf("a fleet port is never a model endpoint: %v", pl.Add)
	}
	if len(pl.Peers) != 2 || pl.Peers[0].Name != "bravo" || pl.Peers[0].SSH != "bravo" || pl.Peers[0].Remote != "" {
		t.Fatalf("default-port fleet should become a peer without an explicit remote: %+v", pl.Peers)
	}
	if pl.Peers[1].Name != "dino" || pl.Peers[1].Remote != "127.0.0.1:4800" {
		t.Fatalf("non-default port must be kept as the remote: %+v", pl.Peers[1])
	}
	if len(pl.Known) != 1 || len(pl.Skip) != 1 {
		t.Fatalf("already-a-peer and this-fleet-itself must be reported: known=%v skip=%v", pl.Known, pl.Skip)
	}
}

func TestParseHelloAndPorts(t *testing.T) {
	if fi := parseHello([]byte(`{"fleet":true,"id":"x","name":"n"}`)); fi == nil || fi.ID != "x" || fi.Name != "n" {
		t.Fatalf("hello: %+v", fi)
	}
	for _, bad := range []string{``, `{}`, `{"fleet":false}`, `<html>`, `{"object":"list","data":[]}`} {
		if parseHello([]byte(bad)) != nil {
			t.Errorf("%q is not a fleet", bad)
		}
	}
	got := ParsePorts("8080, 30000-30002,bad,8080")
	want := []int{8080, 30000, 30001, 30002}
	if len(got) != len(want) {
		t.Fatalf("ports %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ports %v, want %v", got, want)
		}
	}
	if len(withPort(want, 8080)) != 4 || len(withPort(want, FleetPort)) != 5 {
		t.Error("withPort must add a port once")
	}
}

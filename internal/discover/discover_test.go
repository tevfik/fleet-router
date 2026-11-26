package discover

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
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

// llama-server answers GET /v1/models with both a `data` and a `models` array holding the same models.
// parseModels lists every model once, in the order of its first appearance.
func TestParseModelsDedupesDataAndModels(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{`{"object":"list","data":[{"id":"qwen3"},{"id":"gpt-oss"}],"models":[{"id":"qwen3"},{"id":"gpt-oss"}]}`, []string{"qwen3", "gpt-oss"}},
		{`{"data":[{"id":"b"},{"id":"a"}],"models":[{"name":"c"},{"id":"a"},{"name":"b"}]}`, []string{"b", "a", "c"}}, // models array: name is the id
		{`{"data":[{"id":"a"},{"id":"a"},{"id":"b"}]}`, []string{"a", "b"}},                                           // repeated within one array
		{`{"models":[{"name":"tiny"},{"id":"tiny"},{"name":""}]}`, []string{"tiny"}},                                  // an empty entry adds nothing
		{`{"data":[{"id":""},{"id":"a"},{}]}`, []string{"a"}},
		{`{"data":[{"id":"x"}],"models":[]}`, []string{"x"}},
		{`garbage`, nil},
	}
	for _, c := range cases {
		if got := parseModels([]byte(c.in)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseModels(%s) = %v, want %v", c.in, got, c.want)
		}
	}
}

// the scan of a llama-server style port registers each model once, not twice
func TestLocalModelsDedupesLlamaServer(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"object":"list","data":[{"id":"tinyllama"},{"id":"qwen3"}],"models":[{"id":"tinyllama"},{"id":"qwen3"}]}`)
	}))
	defer s.Close()
	got := localModels(context.Background(), portOf(t, s))
	if !reflect.DeepEqual(got, []string{"tinyllama", "qwen3"}) {
		t.Fatalf("localModels: %v", got)
	}
}

// MakePlan never creates two endpoints for the same (node, port, model): the model may be found twice,
// in both arrays of one answer or because the same port was probed twice.
func TestMakePlanSkipsDuplicateEndpoints(t *testing.T) {
	cfg := &config.Config{Endpoints: []config.Endpoint{
		{Name: "dino-old", Node: "dino", SSH: "dino", Remote: "127.0.0.1:8080", Model: "old"},
	}}
	pl := MakePlan(cfg, []Found{
		{Node: "dino", SSH: "dino", Port: 8080, Models: []string{"m", "m", "n"}}, // "m" in both arrays of the answer
		{Node: "dino", SSH: "dino", Port: 8080, Models: []string{"m", "n", "old"}},
	}, true)
	if len(pl.Add) != 2 || pl.Add[0].Name != "dino-m" || pl.Add[1].Name != "dino-n" {
		t.Fatalf("one endpoint per model, no name collision suffix: %+v", pl.Add)
	}
	if pl.Add[0].SSH != "dino" || pl.Add[0].Remote != "127.0.0.1:8080" || pl.Add[0].URL != "" {
		t.Errorf("ssh endpoint: %+v", pl.Add[0])
	}
	if len(pl.Known) != 1 || !strings.Contains(pl.Known[0], "old") {
		t.Errorf("registered endpoints are known: %v", pl.Known)
	}
	if len(pl.Skip) != 3 {
		t.Fatalf("the three repeats must be reported as duplicates: %v", pl.Skip)
	}
	for _, s := range pl.Skip {
		if !strings.HasSuffix(s, "(duplicate)") {
			t.Errorf("skip: %s", s)
		}
	}

	// local endpoints match on their URL host:port, ssh ones on the remote
	pl = MakePlan(&config.Config{}, []Found{
		{Node: "laptop", Port: 8080, Models: []string{"m", "m"}},
		{Node: "laptop", Port: 8080, Models: []string{"m"}},
	}, true)
	if len(pl.Add) != 1 || pl.Add[0].URL != "http://127.0.0.1:8080/v1" || pl.Add[0].SSH != "" || len(pl.Skip) != 2 {
		t.Fatalf("local duplicates: add=%+v skip=%v", pl.Add, pl.Skip)
	}

	// but a different port, node or model is never a duplicate
	pl = MakePlan(&config.Config{}, []Found{
		{Node: "laptop", Port: 8080, Models: []string{"m"}},
		{Node: "laptop", Port: 8081, Models: []string{"m"}},
		{Node: "desktop", SSH: "desktop", Port: 8080, Models: []string{"m"}},
		{Node: "laptop", Port: 8080, Models: []string{"m2"}},
	}, true)
	if len(pl.Add) != 4 || len(pl.Skip) != 0 {
		t.Fatalf("distinct endpoints must all be added: add=%+v skip=%v", pl.Add, pl.Skip)
	}
}

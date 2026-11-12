package discover

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
)

// portOf returns the TCP port an httptest server listens on (always 127.0.0.1).
func portOf(t *testing.T, s *httptest.Server) int {
	t.Helper()
	u, err := url.Parse(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := strconv.Atoi(u.Port())
	return p
}

// closedPort returns a 127.0.0.1 port nothing listens on.
func closedPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return p
}

func openAIServer() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"object":"list","data":[{"id":"qwen3-coder"}]}`)
	})
	return httptest.NewServer(mux) // /fleet/hello and /api/show → 404
}

func ollamaServer() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"id":"llama3:8b"},{"id":"nomic-embed-text"}]}`)
	})
	mux.HandleFunc("/api/show", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Model string }
		json.NewDecoder(r.Body).Decode(&req)
		switch req.Model {
		case "llama3:8b":
			io.WriteString(w, `{"capabilities":["completion","tools","vision","insert"]}`)
		case "nomic-embed-text":
			io.WriteString(w, `{"capabilities":["embedding"]}`)
		default:
			http.NotFound(w, r)
		}
	})
	return httptest.NewServer(mux)
}

func fleetServer() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/fleet/hello", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"fleet":true,"id":"f1","name":"other-fleet"}`)
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"id":"peer-model"}]}`) // a fleet is a peer even when it has models
	})
	return httptest.NewServer(mux)
}

func modelsFallbackServer() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/models", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"models":[{"name":"tiny"}]}`)
	})
	return httptest.NewServer(mux) // /v1/models → 404, so the scan falls back to /models
}

func TestFeature_Scan_LocalServers(t *testing.T) {
	SetSelf("testnode")
	defer SetSelf("")
	oa, ol, fl, fb := openAIServer(), ollamaServer(), fleetServer(), modelsFallbackServer()
	defer oa.Close()
	defer ol.Close()
	defer fl.Close()
	defer fb.Close()
	pOA, pOL, pFL, pFB, pNone := portOf(t, oa), portOf(t, ol), portOf(t, fl), portOf(t, fb), closedPort(t)
	tunnel := closedPort(t)

	// The default fleet port is always added to a scan; mark it as one of our tunnels so the test never
	// touches whatever really listens on 4711 on this machine. No SSH: only blank/self aliases, both skipped.
	skip := map[int]string{FleetPort: "test", tunnel: "dino"}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	found, notes := Scan(ctx, []int{pOA, pOL, pFL, pFB, pNone, tunnel}, []string{" ", "testnode"}, skip)

	byPort := map[int]Found{}
	for _, f := range found {
		byPort[f.Port] = f
	}
	if len(found) != 4 {
		t.Fatalf("want 4 findings (openai, ollama, fleet, /models), got %+v", found)
	}
	if f := byPort[pOA]; f.Node != "testnode" || f.SSH != "" || !reflect.DeepEqual(f.Models, []string{"qwen3-coder"}) || f.Fleet != nil || len(f.Caps) != 0 {
		t.Errorf("openai server: %+v", f)
	}
	if f := byPort[pOA]; f.URL() != "http://127.0.0.1:"+strconv.Itoa(pOA)+"/v1" {
		t.Errorf("URL: %s", f.URL())
	}
	f := byPort[pOL]
	if !reflect.DeepEqual(f.Models, []string{"llama3:8b", "nomic-embed-text"}) {
		t.Errorf("ollama models: %v", f.Models)
	}
	if !reflect.DeepEqual(f.Caps["llama3:8b"], []string{"chat", "vision", "tools"}) || !reflect.DeepEqual(f.Caps["nomic-embed-text"], []string{"embed"}) {
		t.Errorf("ollama caps (mapped, stable order, unknown dropped): %v", f.Caps)
	}
	if f := byPort[pFL]; f.Fleet == nil || f.Fleet.ID != "f1" || f.Fleet.Name != "other-fleet" || len(f.Models) != 0 {
		t.Errorf("fleet must be offered as a peer only: %+v", f)
	}
	if f := byPort[pFB]; !reflect.DeepEqual(f.Models, []string{"tiny"}) {
		t.Errorf("/models fallback: %+v", f)
	}
	if _, ok := byPort[pNone]; ok {
		t.Error("closed port reported")
	}
	if _, ok := byPort[tunnel]; ok {
		t.Error("own tunnel port scanned")
	}
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "tunnel (dino)") || !strings.Contains(joined, "this machine itself") {
		t.Errorf("notes: %v", notes)
	}
	if strings.Count(joined, "SSH skipped") != 1 {
		t.Errorf("a blank host must be ignored silently, self noted once: %v", notes)
	}

	// The plan: models become endpoints (embeddings skipped by default), the fleet becomes a peer.
	cfg := &config.Config{ID: "me"}
	pl := MakePlan(cfg, found, false)
	if len(pl.Peers) != 1 || pl.Peers[0].Name != "testnode" || pl.Peers[0].URL != "http://127.0.0.1:"+strconv.Itoa(pFL)+"/v1" {
		t.Errorf("peers: %+v", pl.Peers)
	}
	var names []string
	for _, e := range pl.Add {
		names = append(names, e.Model)
	}
	if len(pl.Add) != 3 || len(pl.Skip) != 1 || !strings.Contains(pl.Skip[0], "nomic-embed-text") {
		t.Errorf("add=%v skip=%v", names, pl.Skip)
	}
}

func TestFeature_Scan_SelfName(t *testing.T) {
	SetSelf("")
	if Self() == "" || Self() != hostname() {
		t.Errorf("without a name Self is the short hostname: %q vs %q", Self(), hostname())
	}
	if strings.Contains(Self(), ".") {
		t.Errorf("hostname must be cut at the first dot: %q", Self())
	}
	SetSelf("named")
	defer SetSelf("")
	if Self() != "named" {
		t.Error(Self())
	}
}

func TestFeature_ParseModels_Shapes(t *testing.T) {
	cases := map[string][]string{
		`{"data":[{"id":"a"},{"id":"b"}]}`:                         {"a", "b"},
		`{"models":[{"id":"x","name":"ignored"},{"name":"y"},{}]}`: {"x", "y"},
		`{"data":[{"id":"a"}],"models":[{"name":"b"}]}`:            {"a", "b"},
		`not json`:          nil,
		`{"object":"list"}`: nil,
	}
	for in, want := range cases {
		if got := parseModels([]byte(in)); !reflect.DeepEqual(got, want) {
			t.Errorf("parseModels(%s) = %v, want %v", in, got, want)
		}
	}
}

func TestFeature_ParseHello_Fields(t *testing.T) {
	fi := parseHello([]byte(`{"fleet":true}`))
	if fi == nil || fi.ID != "" || fi.Name != "" {
		t.Errorf("fleet:true with no id is still a fleet: %+v", fi)
	}
	if parseHello([]byte(`{"fleet":"yes"}`)) != nil {
		t.Error("non-bool fleet field accepted")
	}
}

func TestFeature_Caps_MapAndParse(t *testing.T) {
	got := mapCaps([]string{"embedding", "thinking", "tools", "audio", "vision", "completion", "insert", "tools"})
	if !reflect.DeepEqual(got, []string{"chat", "vision", "audio", "tools", "thinking", "embed"}) {
		t.Errorf("mapCaps: %v", got)
	}
	if mapCaps(nil) != nil || mapCaps([]string{"insert"}) != nil {
		t.Error("nothing known → nil")
	}
	if c := parseCaps([]byte(`{"capabilities":["completion"]}`)); !reflect.DeepEqual(c, []string{"chat"}) {
		t.Errorf("parseCaps: %v", c)
	}
	if parseCaps([]byte(`garbage`)) != nil || parseCaps([]byte(`{}`)) != nil {
		t.Error("bad /api/show body must give nil")
	}
}

func TestFeature_Caps_LocalNotOllama(t *testing.T) {
	oa := openAIServer()
	defer oa.Close()
	if c := localCaps(context.Background(), portOf(t, oa), []string{"m"}); c != nil {
		t.Errorf("a non-Ollama server (404 on /api/show) gives nil caps: %v", c)
	}
	if c := localCaps(context.Background(), closedPort(t), []string{"m"}); c != nil {
		t.Errorf("unreachable: %v", c)
	}
	ol := ollamaServer()
	defer ol.Close()
	// unsafe names are never sent; the safe one is.
	c := localCaps(context.Background(), portOf(t, ol), []string{"bad name;rm", "llama3:8b"})
	if _, ok := c["bad name;rm"]; ok || len(c["llama3:8b"]) == 0 {
		t.Errorf("caps: %v", c)
	}
}

func TestFeature_Ports_ParseFormat(t *testing.T) {
	if got := ParsePorts(" 9-5 , ,x-y,-3"); len(got) != 0 {
		t.Errorf("reversed/garbage ranges: %v", got)
	}
	if got := ParsePorts("1-100000"); len(got) != 5000 || got[0] != 1 || got[4999] != 5000 {
		t.Errorf("a range is capped at 5000 ports: len=%d", len(got))
	}
	if got := ParsePorts("30-31,29,31"); !reflect.DeepEqual(got, []int{29, 30, 31}) {
		t.Errorf("sorted unique: %v", got)
	}
	cases := []struct {
		in   []int
		want string
	}{
		{nil, ""},
		{[]int{8080}, "8080"},
		{[]int{2, 1}, "1,2"},
		{[]int{3, 1, 2, 2}, "1-3"},
		{[]int{11434, 30000, 30001, 30002, 30003, 8080, 1234, 1235}, "1234,1235,8080,11434,30000-30003"},
	}
	for _, c := range cases {
		if got := FormatPorts(c.in); got != c.want {
			t.Errorf("FormatPorts(%v) = %q, want %q", c.in, got, c.want)
		}
	}
	for _, p := range PortPresets {
		if spec := FormatPorts(ParsePorts(p.Spec)); spec != p.Spec {
			t.Errorf("preset %s does not round-trip: %q", p.Name, spec)
		}
	}
}

func TestFeature_MakePlan_Endpoints(t *testing.T) {
	cfg := &config.Config{Endpoints: []config.Endpoint{
		{Name: "dino-llama3", Node: "dino", SSH: "dino", Remote: "127.0.0.1:9000", Model: "other"}, // name taken
		{Name: "known", Node: "dino", SSH: "dino", Remote: "127.0.0.1:11434", Model: "qwen"},
	}}
	found := []Found{
		{Node: "dino", SSH: "dino", Port: 11434, Models: []string{"qwen", "llama3", "bge-m3", "reranker-x"},
			Caps: map[string][]string{"llama3": {"chat", "tools"}, "reranker-x": {"embed"}}},
		{Node: "box", Port: 8080, Models: []string{"llama3"}},
	}
	pl := MakePlan(cfg, found, false)
	if len(pl.Known) != 1 || !strings.Contains(pl.Known[0], "qwen") {
		t.Errorf("known: %v", pl.Known)
	}
	if len(pl.Skip) != 2 {
		t.Errorf("bge (by name) and reranker (caps without chat) are skipped: %v", pl.Skip)
	}
	if len(pl.Add) != 2 {
		t.Fatalf("add: %+v", pl.Add)
	}
	a := pl.Add[0]
	if a.Name != "dino-llama3-2" || a.SSH != "dino" || a.Remote != "127.0.0.1:11434" || a.URL != "" || !reflect.DeepEqual(a.Caps, []string{"chat", "tools"}) {
		t.Errorf("ssh endpoint (name collision → -2, caps from server): %+v", a)
	}
	b := pl.Add[1]
	if b.Name != "box-llama3" || b.URL != "http://127.0.0.1:8080/v1" || b.SSH != "" || b.Kind != "" {
		t.Errorf("local endpoint: %+v", b)
	}

	// includeEmbed: embeddings are added; a name-only embed model gets Kind "embed", a capped one its caps.
	pl = MakePlan(cfg, found[:1], true)
	kinds := map[string]config.Endpoint{}
	for _, e := range pl.Add {
		kinds[e.Model] = e
	}
	if kinds["bge-m3"].Kind != "embed" || len(kinds["bge-m3"].Caps) != 0 {
		t.Errorf("bge-m3: %+v", kinds["bge-m3"])
	}
	if !reflect.DeepEqual(kinds["reranker-x"].Caps, []string{"embed"}) {
		t.Errorf("reranker-x: %+v", kinds["reranker-x"])
	}
	if len(pl.Skip) != 0 {
		t.Errorf("nothing skipped with includeEmbed: %v", pl.Skip)
	}
}

func TestFeature_MakePlan_NameCollisionWithinPlan(t *testing.T) {
	cfg := &config.Config{}
	// Two models that slug to the same name on one node.
	pl := MakePlan(cfg, []Found{{Node: "n", Port: 1, Models: []string{"Qwen:3", "qwen-3", "QWEN 3"}}}, true)
	got := []string{}
	for _, e := range pl.Add {
		got = append(got, e.Name)
	}
	if !reflect.DeepEqual(got, []string{"n-qwen-3", "n-qwen-3-2", "n-qwen-3-3"}) {
		t.Errorf("names: %v", got)
	}
}

func TestFeature_MakePlan_PeerNamingAndDedupe(t *testing.T) {
	cfg := &config.Config{ID: "me", Peers: []config.Peer{
		{Name: "big-box", SSH: "elsewhere"},
		{Name: "urlpeer", URL: "http://127.0.0.1:5000/v1"},
		{Name: "explicit", SSH: "lab", Remote: "127.0.0.1:4800"},
	}}
	found := []Found{
		{Node: "Big Box", Port: 4711, Fleet: &FleetInfo{ID: "a"}},                          // name taken in cfg → -2
		{Node: "Big Box", Port: 4712, Fleet: &FleetInfo{ID: "b"}},                          // taken in plan → -3
		{Node: "me-host", Port: 5000, Fleet: &FleetInfo{ID: "c"}},                          // same URL → known
		{Node: "lab", SSH: "lab", Port: 4800, Fleet: &FleetInfo{ID: "d"}},                  // same ssh+remote → known
		{Node: "lab", SSH: "lab", Port: FleetPort, Fleet: &FleetInfo{ID: "e"}},             // default port, peer has explicit other remote → new
		{Node: "x", Port: 1, Fleet: &FleetInfo{ID: "", Name: "anon"}},                      // empty ID is not "this fleet"
		{Node: "self", Port: 2, Fleet: &FleetInfo{ID: "me", Name: "mine"}},                 // this fleet
		{Node: "elsewhere", SSH: "elsewhere", Port: FleetPort, Fleet: &FleetInfo{ID: "f"}}, // big-box peer, default port
	}
	pl := MakePlan(cfg, found, false)
	var names []string
	for _, p := range pl.Peers {
		names = append(names, p.Name)
	}
	if !reflect.DeepEqual(names, []string{"big-box-2", "big-box-3", "lab", "x"}) {
		t.Errorf("peer names: %v (%+v)", names, pl.Peers)
	}
	if pl.Peers[2].SSH != "lab" || pl.Peers[2].Remote != "" {
		t.Errorf("ssh fleet on the default port is registered without a remote: %+v", pl.Peers[2])
	}
	if len(pl.Known) != 3 {
		t.Errorf("known: %v", pl.Known)
	}
	if len(pl.Skip) != 1 || !strings.Contains(pl.Skip[0], "this fleet itself") || !strings.Contains(pl.Skip[0], `"mine"`) {
		t.Errorf("skip: %v", pl.Skip)
	}
}

func TestFeature_FirstLine(t *testing.T) {
	if firstLine("  a\nb\n") != "a" {
		t.Error("first line")
	}
	if got := firstLine(strings.Repeat("x", 200)); len(got) != 90 {
		t.Errorf("cut to 90: %d", len(got))
	}
}

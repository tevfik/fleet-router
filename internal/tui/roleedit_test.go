package tui

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/tevfik/fleet-router/internal/client"
	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/eval"
	"github.com/tevfik/fleet-router/internal/server"
)

// recorder is a fake daemon that remembers every POST (path → body).
type recorder struct {
	mu   sync.Mutex
	last map[string]map[string]any
}

func newRecorder(t *testing.T) (*recorder, *client.Client) {
	r := &recorder{last: map[string]map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		var body map[string]any
		json.Unmarshal(b, &body)
		r.mu.Lock()
		r.last[req.URL.Path] = body
		r.mu.Unlock()
		w.Write([]byte("{}"))
	}))
	t.Cleanup(srv.Close)
	return r, client.New(strings.TrimPrefix(srv.URL, "http://"))
}

func typeText(m model, text string) model {
	for _, r := range text {
		nm, _ := m.keyStr(string(r), string(r))
		m = nm.(model)
	}
	return m
}

func press(t *testing.T, m model, key string) (model, string) {
	nm, cmd := m.keyStr(key, "")
	m = nm.(model)
	note := ""
	if cmd != nil {
		if n, ok := cmd().(noteMsg); ok {
			note = string(n)
		}
	}
	return m, note
}

func editModel(cl *client.Client) model {
	eps := []server.EpStatus{
		{Endpoint: config.Endpoint{Name: "a", Node: "n1", Model: "m1", Tags: []string{"fast"}}},
		{Endpoint: config.Endpoint{Name: "b", Node: "n2", Model: "m2"}},
	}
	return model{cl: cl, st: server.Status{Endpoints: eps, Roles: map[string]server.RoleStatus{
		"scout": {Role: config.Role{Preferred: "a"}, Active: "a"},
	}}, sc: scRoles}
}

func TestNewRoleFromSelectors(t *testing.T) {
	rec, cl := newRecorder(t)
	m := editModel(cl)
	m, _ = press(t, m, "n")
	if m.editing != "newrole" {
		t.Fatal("n on the Roles screen must open the new-role input")
	}
	m = typeText(m, "summarizer tag:fast")
	m, note := press(t, m, "enter")
	if m.editing != "" || !strings.Contains(note, "created") {
		t.Fatalf("editing=%q note=%q", m.editing, note)
	}
	body := rec.last["/fleet/role"]
	if body["role"] != "summarizer" || len(body["match"].([]any)) != 1 || body["match"].([]any)[0] != "tag:fast" {
		t.Fatalf("daemon got %v", body)
	}
}

func TestNewRoleNameOnlyGetsAModelAndDuplicatesAreRefused(t *testing.T) {
	rec, cl := newRecorder(t)
	m := editModel(cl)
	m, _ = press(t, m, "n")
	m = typeText(m, "helper")
	m, _ = press(t, m, "enter")
	if rec.last["/fleet/role"]["preferred"] != "a" {
		t.Fatalf("a role needs a model: %v", rec.last["/fleet/role"])
	}
	delete(rec.last, "/fleet/role")
	m, _ = press(t, m, "n")
	m = typeText(m, "scout")
	m, _ = press(t, m, "enter")
	if rec.last["/fleet/role"] != nil || !strings.Contains(m.note, "already exists") {
		t.Fatalf("an existing name must be refused, note=%q", m.note)
	}
}

func TestDeleteRoleNeedsConfirmation(t *testing.T) {
	rec, cl := newRecorder(t)
	m := editModel(cl)
	m, _ = press(t, m, "x")
	if !m.confirm || rec.last["/fleet/role"] != nil {
		t.Fatal("x must only ask")
	}
	m, _ = press(t, m, "n") // any other key cancels
	if m.confirm || rec.last["/fleet/role"] != nil {
		t.Fatal("cancel must not delete")
	}
	m, _ = press(t, m, "x")
	m, note := press(t, m, "y")
	if rec.last["/fleet/role"]["delete"] != true || !strings.Contains(note, "deleted") {
		t.Fatalf("daemon got %v, note %q", rec.last["/fleet/role"], note)
	}
}

func TestEditSelectorsAndTags(t *testing.T) {
	rec, cl := newRecorder(t)
	m := editModel(cl)
	m.sc, m.roleCur = scRoleEdit, 0
	m, _ = press(t, m, "m")
	if m.editing != "match" || m.editFor != "scout" {
		t.Fatalf("editing=%q for=%q", m.editing, m.editFor)
	}
	m = typeText(m, "tag:fast node:n2")
	m, _ = press(t, m, "enter")
	if got := rec.last["/fleet/role"]["match"].([]any); len(got) != 2 {
		t.Fatalf("selectors sent: %v", got)
	}

	m.sc, m.cur = scDash, 0
	m, _ = press(t, m, "t")
	if m.editing != "tags" || m.editBuf != "fast" {
		t.Fatalf("tags editor should start from the current tags: %q %q", m.editing, m.editBuf)
	}
	m = typeText(m, " gpu")
	m, _ = press(t, m, "enter")
	if got := rec.last["/fleet/endpoint-set"]["tags"].([]any); len(got) != 2 || got[1] != "gpu" {
		t.Fatalf("tags sent: %v", got)
	}

	m.sc, m.cur = scDash, 0
	m, _ = press(t, m, "f")
	if m.editing != "fleetport" {
		t.Fatalf("editing %q", m.editing)
	}
	m = typeText(m, "4800")
	m, _ = press(t, m, "enter")
	if rec.last["/fleet/endpoint-set"]["fleet_port"] != float64(4800) {
		t.Fatalf("fleet_port sent: %v", rec.last["/fleet/endpoint-set"])
	}

	m.sc, m.cur = scDash, 0
	m, _ = press(t, m, "g")
	if m.editing != "group" || !strings.Contains(m.editBuf, "engine=") {
		t.Fatalf("group editor: %q %q", m.editing, m.editBuf)
	}
	m.editBuf = "engine=llama-ring workers=n2,n3 stall=60s"
	m, _ = press(t, m, "enter")
	g := rec.last["/fleet/endpoint-set"]["group"].(map[string]any)
	if g["engine"] != "llama-ring" || len(g["workers"].([]any)) != 2 || rec.last["/fleet/endpoint-set"]["stall"] != "60s" {
		t.Fatalf("group sent: %v", rec.last["/fleet/endpoint-set"])
	}
}

// A scan started from the button bar left the focus on the bar: enter/space then pressed buttons instead of selecting a result.
func TestScanResultsTakeFocusBackFromTheBar(t *testing.T) {
	m := testModel()
	m.sc, m.focus, m.inBar, m.barCur, m.busy = scDiscover, 0, true, 0, true
	nm, _ := m.Update(discMsg{r: client.DiscoverResult{Plan: m.disc.Plan}})
	m = nm.(model)
	if m.inBar || m.focus != 1 {
		t.Fatalf("inBar=%v focus=%d after the results arrived", m.inBar, m.focus)
	}
	rows := m.visRows()
	var item int = -1
	for i, r := range rows {
		if !r.header && r.act == "" {
			item = i
			break
		}
	}
	m.cur = item
	was := m.sel[rows[item].idx]
	for _, k := range []string{" ", "enter"} {
		nm, _ := m.keyStr(k, "")
		m = nm.(model)
		if now := m.sel[rows[item].idx]; now == was {
			t.Fatalf("%q did not toggle the highlighted result", k)
		}
		was = !was
	}
}

// Adding a port saved asynchronously; re-reading the configuration straight away brought the old one back
// and the new port only showed up after leaving and re-entering the screen.
func TestAddedPortAndHostShowImmediately(t *testing.T) {
	_, cl := newRecorder(t) // the fake daemon still reports an empty configuration
	m := model{cl: cl, sc: scDiscover, focus: 0, sel: map[int]bool{}}
	m.loadHosts()
	m.editing, m.editBuf = "addport", ""
	m = typeText(m, "9999")
	m, _ = press(t, m, "enter")
	found := false
	for _, r := range m.menuRows() {
		if r.kind == "port" && r.spec == "9999" {
			found = true
		}
	}
	if !found {
		t.Fatal("the new port is not in the menu")
	}
	m.editing, m.editBuf = "addhost", ""
	m = typeText(m, "newbox")
	m, _ = press(t, m, "enter")
	if !contains(m.hosts, "newbox") {
		t.Fatalf("the new host is not listed: %v", m.hosts)
	}
}

func searchModel(cl *client.Client) model {
	mk := func(name, node, model string, tags ...string) server.EpStatus {
		return server.EpStatus{Endpoint: config.Endpoint{Name: name, Node: node, Model: model, Tags: tags}}
	}
	return model{cl: cl, sc: scRoles, st: server.Status{
		Endpoints: []server.EpStatus{mk("dino-gemma", "dino", "gemma4:12b"), mk("dino-qwen", "dino", "qwen3:8b", "code"), mk("vec-gemma", "victor", "gemma3:4b")},
		Roles:     map[string]server.RoleStatus{"scout": {Role: config.Role{Preferred: "dino-gemma"}, Active: "dino-gemma"}},
	}}
}

func TestSearchNarrowsTheModelPicker(t *testing.T) {
	rec, cl := newRecorder(t)
	m := searchModel(cl)
	m, _ = press(t, m, "f") // add a fallback
	if !m.pick || len(m.chatEps()) != 3 {
		t.Fatalf("pick=%v models=%d", m.pick, len(m.chatEps()))
	}
	m, _ = press(t, m, "/")
	m = typeText(m, "victor gemma") // every word must match: node + model
	if got := m.chatEps(); len(got) != 1 || got[0].Name != "vec-gemma" {
		t.Fatalf("filtered = %v", got)
	}
	m, _ = press(t, m, "enter") // end typing, keep the filter
	m, _ = press(t, m, "enter") // choose the highlighted model
	if m.pick || m.filter != "" {
		t.Fatalf("pick=%v filter=%q after choosing", m.pick, m.filter)
	}
	fb, _ := rec.last["/fleet/role"]["fallback"].([]any)
	if len(fb) != 1 || fb[0] != "vec-gemma" {
		t.Fatalf("daemon got %v", rec.last["/fleet/role"])
	}
}

func TestSearchEscClearsThenLeaves(t *testing.T) {
	_, cl := newRecorder(t)
	m := searchModel(cl)
	m.sc = scDash
	m, _ = press(t, m, "/")
	m = typeText(m, "qwen")
	m, _ = press(t, m, "enter")
	if got := m.dashEps(); len(got) != 1 || got[0].Name != "dino-qwen" {
		t.Fatalf("dashboard rows = %v", got)
	}
	m, _ = press(t, m, "t") // acts on the highlighted FILTERED row
	if m.editFor != "dino-qwen" || m.editBuf != "code" {
		t.Fatalf("tags editor is for %q (%q)", m.editFor, m.editBuf)
	}
	m, _ = press(t, m, "esc") // leave the tags input
	m, _ = press(t, m, "esc") // first esc: clear the search, stay
	if m.sc != scDash || m.filter != "" || len(m.dashEps()) != 3 {
		t.Fatalf("sc=%v filter=%q rows=%d", m.sc, m.filter, len(m.dashEps()))
	}
	m, _ = press(t, m, "esc") // second esc: leave
	if m.sc != scHome {
		t.Fatalf("still on screen %v", m.sc)
	}
}

func TestRoleTestKeyProbesRouterAndMembers(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Model string `json:"model"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		mu.Lock()
		seen[in.Model] = true
		mu.Unlock()
		w.Header().Set("X-Fleet-Endpoint", "dino-gemma")
		w.Write([]byte(`{"choices":[{"message":{"content":"pong"}}]}`))
	}))
	defer srv.Close()
	m := searchModel(client.New(strings.TrimPrefix(srv.URL, "http://")))
	m.st.Roles["scout"] = server.RoleStatus{Role: config.Role{Preferred: "dino-gemma", Fallback: []string{"vec-gemma"}}}
	_, note := press(t, m, "T")
	for _, want := range []string{"scout", "dino-gemma", "vec-gemma", "pong"} {
		if !strings.Contains(note, want) {
			t.Fatalf("note lacks %q:\n%s", want, note)
		}
	}
	for _, target := range []string{"scout", "dino-gemma", "vec-gemma"} {
		if !seen[target] {
			t.Errorf("%s was never probed (saw %v)", target, seen)
		}
	}
}

func TestQualityLineShowsTheStoredEvalScore(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if qualityLine("a") != "" {
		t.Fatal("an endpoint that was never evaluated has no quality line")
	}
	r := eval.Result{Target: "a", Items: []eval.ItemResult{{Category: eval.CatFormat, Pass: true, Passed: 1, Total: 1}, {Category: eval.CatReasoning, Pass: false, Total: 1}}}
	if err := eval.Save(config.EvalPath(), []eval.Result{r}); err != nil {
		t.Fatal(err)
	}
	if q := qualityLine("a"); !strings.Contains(q, "50%") || !strings.Contains(q, "format 100") {
		t.Fatalf("quality line = %q", q)
	}
}

func TestVersionShownAndDaemonMismatchFlagged(t *testing.T) {
	old := server.Version
	defer func() { server.Version = old }()
	server.Version = "v1.2.3"
	m := searchModel(nil)
	m.cl = client.New("127.0.0.1:1")
	for _, sc := range []screen{scHome, scRoles, scDash} {
		m.sc = sc
		if v := m.View().Content; !strings.Contains(v, "fleet v1.2.3") {
			t.Errorf("screen %d does not show the version", sc)
		}
	}
	if strings.Contains(m.View().Content, "daemon runs") {
		t.Error("no mismatch warning while the daemon reports nothing")
	}
	m.st.Version = "v1.0.0"
	if v := m.View().Content; !strings.Contains(v, "daemon runs v1.0.0") {
		t.Error("an older daemon must be flagged")
	}
	m.st.Version = "v1.2.3"
	if strings.Contains(m.View().Content, "daemon runs") {
		t.Error("equal versions must not warn")
	}
}

func TestSearchOnUsageAndLeaderboard(t *testing.T) {
	_, cl := newRecorder(t)
	m := searchModel(cl)
	m.usage = []server.UsageRow{{Key: "dino/gemma4:12b", Requests: 5}, {Key: "victor/gemma3:4b", Requests: 2}, {Key: "dino/qwen3:8b", Requests: 1}}
	m.sc = scUsage
	m, _ = press(t, m, "/")
	m = typeText(m, "dino gemma")
	m, _ = press(t, m, "enter")
	if got := m.usageRows(); len(got) != 1 || got[0].Key != "dino/gemma4:12b" {
		t.Fatalf("usage rows = %v", got)
	}
	if m.listLen() != 1 {
		t.Fatalf("the cursor range must follow the filtered list, got %d", m.listLen())
	}
	m, _ = press(t, m, "esc")
	if m.sc != scUsage || len(m.usageRows()) != 3 {
		t.Fatalf("first esc clears the search (screen %v, %d rows)", m.sc, len(m.usageRows()))
	}

	m.sc, m.bTab = scBoard, 0
	m, _ = press(t, m, "/")
	m = typeText(m, "qwen")
	m, _ = press(t, m, "enter")
	if got := m.boardEps(); len(got) != 1 || got[0].Name != "dino-qwen" {
		t.Fatalf("leaderboard endpoints = %v", got)
	}
}

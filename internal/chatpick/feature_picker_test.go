package chatpick

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/tevfik/fleet-router/internal/client"
	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/server"
	"github.com/tevfik/fleet-router/internal/store"
)

func ep(name, node, model string, ok bool, tps float64) server.EpStatus {
	return server.EpStatus{Endpoint: config.Endpoint{Name: name, Node: node, Model: model, TPS: tps}, OK: ok}
}

func testStatus() server.Status {
	return server.Status{
		Endpoints: []server.EpStatus{
			ep("zeta", "n2", "qwen", true, 30),
			ep("alpha", "n1", "llama", true, 80),
			ep("dead", "n3", "qwen", false, 0),
			ep("distributed-up", "n1", "big", true, 12),
			ep("distributed-down", "n1", "big", false, 0),
		},
		Roles: map[string]server.RoleStatus{
			"scout": {Role: config.Role{Preferred: "dead", Fallback: []string{"zeta"}}, Active: "zeta"},
			"coder": {Role: config.Role{Preferred: "alpha"}, Active: "alpha"},
		},
		Nodes: []server.NodeStatus{{Node: "n1"}, {Node: "n2"}},
	}
}

func names(es []Entry) []string {
	var o []string
	for _, e := range es {
		o = append(o, e.Name)
	}
	return o
}

func TestEntriesGroupedAndOfflineListed(t *testing.T) {
	st := testStatus()
	st.Endpoints[2].Err = "connection refused"
	got := names(Entries(st, ""))
	want := []string{"distributed-up", "distributed-down", "coder", "scout", "alpha", "zeta", "dead"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	d, _ := Find(st, "dead")
	if d.Up || !strings.Contains(Label(d), "OFFLINE") || !strings.HasPrefix(Label(d), "[endpoint] ✗") {
		t.Fatalf("label %q", Label(d))
	}
	if l := Label(Entries(st, "scout")[0]); !strings.Contains(l, "→ zeta") {
		t.Fatalf("role label %q", l)
	}
}

func TestEntriesFilterAndResolve(t *testing.T) {
	st := testStatus()
	if got := names(Entries(st, "qwen")); !reflect.DeepEqual(got, []string{"zeta", "dead"}) {
		t.Fatalf("model filter: %v", got)
	}
	if got := names(Entries(st, "offline ring")); !reflect.DeepEqual(got, []string{"distributed-down"}) {
		t.Fatalf("state+kind filter: %v", got)
	}
	if got := names(Entries(st, "role zeta")); !reflect.DeepEqual(got, []string{"scout"}) {
		t.Fatalf("role by active: %v", got)
	}
	if e, hits := Resolve(st, "dead"); e.Name != "dead" || hits != nil {
		t.Fatal("exact name (even offline) switches")
	}
	if e, _ := Resolve(st, "llama"); e.Name != "alpha" {
		t.Fatalf("single match switches: %v", e)
	}
	if e, hits := Resolve(st, "qwen"); e.Name != "" || len(hits) != 2 {
		t.Fatal("several matches go to the picker")
	}
}

func TestEmbedNotOffered(t *testing.T) {
	st := testStatus()
	e := ep("emb", "n1", "nomic", true, 0)
	e.Kind = "embed"
	st.Endpoints = append(st.Endpoints, e)
	if _, ok := Find(st, "emb"); ok {
		t.Fatal("embedding model offered as chat target")
	}
}

func TestHeaderStates(t *testing.T) {
	st := testStatus()
	if h := Header(st, "alpha", ""); !strings.Contains(h, "alpha @n1 ✓ up") || !strings.Contains(h, "80 t/s") {
		t.Fatal(h)
	}
	if h := Header(st, "scout", "zeta"); !strings.HasPrefix(h, "role scout → zeta @n2 ✓ up") {
		t.Fatal(h)
	}
	if h := Header(st, "dead", ""); !strings.Contains(h, "✗ offline") {
		t.Fatal(h)
	}
	if h := Header(st, "gone", ""); !strings.Contains(h, "not known") {
		t.Fatal(h)
	}
	st.Roles["scout"] = server.RoleStatus{Role: config.Role{Preferred: "dead"}}
	if h := Header(st, "scout", ""); !strings.Contains(h, "no endpoint is up") {
		t.Fatal(h)
	}
}

func TestNotice(t *testing.T) {
	st := testStatus()
	if n := Notice(st, "alpha", "alpha"); n != "" {
		t.Fatal(n)
	}
	if n := Notice(st, "scout", "zeta"); !strings.Contains(n, "failover: preferred dead is down") {
		t.Fatal(n)
	}
	if n := Notice(st, "alpha", "zeta"); !strings.Contains(n, "rerouted: asked alpha, answered by zeta") {
		t.Fatal(n)
	}
}

func TestExplainSuggestsAlternatives(t *testing.T) {
	st := testStatus()
	msg := Explain(st, "dead", errors.New("http 502: upstream refused"))
	if !strings.Contains(msg, "dead is offline") || !strings.Contains(msg, "upstream refused") {
		t.Fatal(msg)
	}
	// same model elsewhere (zeta serves qwen) comes first
	if !strings.Contains(msg, "Try instead: zeta") {
		t.Fatal(msg)
	}
	if alt := Alternatives(st, "dead", 10); strings.Contains(strings.Join(alt, ","), "dead") || strings.Contains(strings.Join(alt, ","), "distributed-down") {
		t.Fatalf("offline/self suggested: %v", alt)
	}
	if m := Explain(st, "ghost", nil); !strings.Contains(m, "not an endpoint or role") {
		t.Fatal(m)
	}
	none := server.Status{Endpoints: []server.EpStatus{ep("x", "n", "m", false, 0)}}
	if m := Explain(none, "x", nil); !strings.Contains(m, "No other model is up") {
		t.Fatal(m)
	}
	if m := Explain(st, "alpha", errors.New("timeout")); !strings.Contains(m, "is up but the request failed: timeout") {
		t.Fatal(m)
	}
}

func TestHistorySurvivesAndWindows(t *testing.T) {
	h := History{Max: 20}
	h.Add("aaaaaaaa", "bbbbbbbb") // 16
	h.Add("cccc", "dddd")         // 8
	h.Add("e", "f")               // 2
	w, dropped := h.Window()
	if dropped != 1 || len(w) != 4 || w[0].Content != "cccc" {
		t.Fatalf("dropped %d window %v", dropped, w)
	}
	if h.Turns() != 3 {
		t.Fatal("turns")
	}
	big := History{Max: 1}
	big.Add("long question", "long answer")
	if w, d := big.Window(); len(w) != 2 || d != 0 {
		t.Fatal("the newest exchange is always sent, never half of one")
	}
	h.Clear()
	if h.Turns() != 0 {
		t.Fatal("clear")
	}
}

func TestRoleBindingHelpers(t *testing.T) {
	r := config.Role{Preferred: "a", Fallback: []string{"b", "c"}}
	p, fb := RebindPreferred(r, "c")
	if p != "c" || !reflect.DeepEqual(fb, []string{"a", "b"}) {
		t.Fatal(p, fb)
	}
	if _, fb, ch := AddFallback(r, "d"); !ch || !reflect.DeepEqual(fb, []string{"b", "c", "d"}) {
		t.Fatal(fb)
	}
	if _, _, ch := AddFallback(r, "b"); ch {
		t.Fatal("already a member")
	}
}

// ---- the actions, against a fake daemon -----------------------------------------------------------------

type fakeAPI struct {
	calls []string
	nodes []server.NodeStore
}

func (f *fakeAPI) rec(s string) { f.calls = append(f.calls, s) }
func (f *fakeAPI) Probe(n []string) error {
	f.rec("probe " + strings.Join(n, ","))
	return nil
}
func (f *fakeAPI) Bench(n []string) error {
	f.rec("bench " + strings.Join(n, ","))
	return nil
}
func (f *fakeAPI) SetSlots(e string, n int) error {
	f.rec("slots " + e + " " + string(rune('0'+n)))
	return nil
}
func (f *fakeAPI) SetTags(e string, t []string) error {
	f.rec("tags " + e + " " + strings.Join(t, "+"))
	return nil
}
func (f *fakeAPI) SetRole(r, p string, fb []string) error {
	f.rec("role " + r + " " + p + " [" + strings.Join(fb, " ") + "]")
	return nil
}
func (f *fakeAPI) StoreAll() ([]server.NodeStore, error) { return f.nodes, nil }
func (f *fakeAPI) StorePull(node, from, sha string) error {
	f.rec("pull " + node + " " + sha)
	return nil
}
func (f *fakeAPI) StoreRemove(ref string) (store.Entry, error) {
	f.rec("forget " + ref)
	return store.Entry{Name: "qwen.gguf", SHA256: ref}, nil
}
func (f *fakeAPI) TestRole(role string, m []string) []client.TestResult {
	f.rec("test " + role + " " + strings.Join(m, ","))
	return []client.TestResult{{Target: role, Endpoint: m[0], Answer: "pong"}}
}
func (f *fakeAPI) Route(model string, in, out int) (server.RouteExplain, error) {
	return server.RouteExplain{Order: []server.Cost{{Endpoint: "zeta"}, {Endpoint: "alpha"}}}, nil
}

func inv(node string, has ...string) server.NodeStore {
	s := &store.Inventory{}
	for _, h := range has {
		s.Files = append(s.Files, store.Entry{SHA256: "sha-" + h, Name: h + ".gguf", Size: 1 << 30, Complete: true})
	}
	return server.NodeStore{Node: node, Store: s}
}

func act(t *testing.T, e Entry, id string) Action {
	t.Helper()
	for _, a := range Actions(e) {
		if a.ID == id {
			return a
		}
	}
	t.Fatalf("no action %s for %v", id, e.Kind)
	return Action{}
}

func TestActionsEndpoint(t *testing.T) {
	st := testStatus()
	st.Endpoints[0].Slots = 2
	f := &fakeAPI{}
	z, _ := Find(st, "zeta")
	for _, c := range []struct{ id, arg, want string }{
		{"probe", "", "probe zeta"},
		{"bench", "", "bench zeta"},
		{"slotsup", "", "slots zeta 3"},
		{"slotsdown", "", "slots zeta 1"},
		{"tags", "fast, gpu", "tags zeta fast+gpu"},
		{"bindpref", "scout", "role scout zeta [dead]"},
		{"bindfb", "coder", "role coder alpha [zeta]"},
	} {
		if _, err := Do(f, st, nil, act(t, z, c.id), z, c.arg); err != nil {
			t.Fatal(c.id, err)
		}
		if got := f.calls[len(f.calls)-1]; got != c.want {
			t.Fatalf("%s: got %q want %q", c.id, got, c.want)
		}
	}
	if _, err := Do(f, st, nil, act(t, z, "bindpref"), z, ""); err == nil {
		t.Fatal("a missing argument must be refused")
	}
	if _, err := Do(f, st, nil, act(t, z, "bindpref"), z, "nope"); err == nil {
		t.Fatal("unknown role")
	}
	if out, _ := Do(f, st, nil, act(t, z, "bindfb"), z, "scout"); !strings.Contains(out, "already") {
		t.Fatal(out)
	}
}

func TestActionsRole(t *testing.T) {
	st := testStatus()
	f := &fakeAPI{}
	r, _ := Find(st, "scout")
	if _, err := Do(f, st, nil, act(t, r, "setpref"), r, "alpha"); err != nil {
		t.Fatal(err)
	}
	if f.calls[0] != "role scout alpha [dead zeta]" {
		t.Fatal(f.calls)
	}
	Do(f, st, nil, act(t, r, "addfb"), r, "alpha")
	Do(f, st, nil, act(t, r, "clearfb"), r, "")
	if f.calls[1] != "role scout dead [zeta alpha]" || f.calls[2] != "role scout dead []" {
		t.Fatal(f.calls)
	}
	if out, err := Do(f, st, nil, act(t, r, "test"), r, ""); err != nil || !strings.Contains(out, "pong") || f.calls[3] != "test scout dead,zeta" {
		t.Fatal(out, err, f.calls)
	}
	if out, _ := Do(f, st, nil, act(t, r, "route"), r, ""); !strings.Contains(out, "goes to zeta") {
		t.Fatal(out)
	}
}

func TestActionsStoreFiles(t *testing.T) {
	st := testStatus()
	f := &fakeAPI{nodes: []server.NodeStore{inv("n1", "qwen"), inv("n2")}}
	z, _ := Find(st, "zeta") // serves model "qwen"
	out, err := Do(f, st, nil, act(t, z, "where"), z, "")
	if err != nil || !strings.Contains(out, "on n1") || !strings.Contains(out, "not on n2") {
		t.Fatal(out, err)
	}
	if out, err = Do(f, st, nil, act(t, z, "pull"), z, "n2"); err != nil || f.calls[len(f.calls)-1] != "pull n2 sha-qwen" {
		t.Fatal(out, err, f.calls)
	}
	if out, _ = Do(f, st, nil, act(t, z, "pull"), z, "n1"); !strings.Contains(out, "already has") {
		t.Fatal(out)
	}
	if _, err = Do(f, st, nil, act(t, z, "forget"), z, ""); err != nil || f.calls[len(f.calls)-1] != "forget sha-qwen" {
		t.Fatal(err, f.calls)
	}
	a, _ := Find(st, "alpha") // model "llama": no file
	if _, err = Do(f, st, nil, act(t, a, "where"), a, ""); err == nil || !strings.Contains(err.Error(), "no model file") {
		t.Fatal(err)
	}
}

func TestActionStartUsesRunOnOnly(t *testing.T) {
	st := testStatus()
	z, _ := Find(st, "zeta")
	var got []string
	run := func(node, path string, dry, reg bool) (string, error) {
		got = append(got, node+" "+path+fmt.Sprint(reg))
		return "started", nil
	}
	if _, err := Do(&fakeAPI{}, st, run, act(t, z, "start"), z, "n1"); err == nil {
		t.Fatal("a path is required")
	}
	if out, err := Do(&fakeAPI{}, st, run, act(t, z, "start"), z, "n1\x00/m/x.gguf"); err != nil || out != "started" || got[0] != "n1 /m/x.gguffalse" {
		t.Fatal(out, err, got)
	}
	if _, err := Do(&fakeAPI{}, st, nil, act(t, z, "start"), z, "n1\x00/m/x.gguf"); err == nil {
		t.Fatal("no runner")
	}
}

func TestChoices(t *testing.T) {
	st := testStatus()
	if got := Choices(st, "role"); !reflect.DeepEqual(got, []string{"coder", "scout"}) {
		t.Fatal(got)
	}
	if got := Choices(st, "node"); !reflect.DeepEqual(got, []string{"n1", "n2"}) {
		t.Fatal(got)
	}
	if got := Choices(st, "endpoint"); len(got) != 5 {
		t.Fatal(got)
	}
}

func (f *fakeAPI) StopEndpoint(name string, opt server.StopOptions) (server.StopReport, error) {
	f.rec(fmt.Sprintf("stop %s force=%v", name, opt.Force))
	return server.StopReport{Endpoint: name, Steps: []server.StopStep{{Role: "master", Node: "n1", Result: "stopped", OK: true}}}, nil
}
func (f *fakeAPI) EndpointsNotes(add []config.Endpoint, remove []string, mend bool) ([]string, error) {
	f.rec(fmt.Sprintf("rm %s mend=%v", strings.Join(remove, ","), mend))
	return []string{"role coder: preferred zeta replaced by alpha"}, nil
}

func TestActionStopAndRemove(t *testing.T) {
	st := testStatus()
	z, _ := Find(st, "zeta")
	f := &fakeAPI{}
	out, err := Do(f, st, nil, act(t, z, "stop"), z, "force")
	if err != nil || !strings.Contains(out, "stopped") || f.calls[0] != "stop zeta force=true" {
		t.Fatal(out, err, f.calls)
	}
	out, err = Do(f, st, nil, act(t, z, "remove"), z, "mend")
	if err != nil || !strings.Contains(out, "replaced by alpha") || f.calls[1] != "rm zeta mend=true" {
		t.Fatal(out, err, f.calls)
	}
}

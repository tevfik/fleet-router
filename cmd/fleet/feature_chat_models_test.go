package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tevfik/agenttui"

	"github.com/tevfik/fleet-router/internal/chatpick"
	"github.com/tevfik/fleet-router/internal/client"
	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/server"
	"github.com/tevfik/fleet-router/internal/store"
)

// isolate keeps a test away from the real configuration.
func isolateChat(t *testing.T) {
	d := t.TempDir()
	t.Setenv("HOME", d)
	t.Setenv("FLEET_CONFIG", d+"/fleet.yaml")
	t.Setenv("XDG_CONFIG_HOME", d+"/xdg")
}

type fakeChatAPI struct {
	st     server.Status
	sent   [][]client.ChatMessage
	models []string
	answer string
	ep     string
	err    error
	calls  []string
}

func (f *fakeChatAPI) Status() (server.Status, error) { return f.st, nil }
func (f *fakeChatAPI) StreamChatMsgs(_ context.Context, model string, conv []client.ChatMessage, emit func(string)) (client.StreamResult, error) {
	f.sent = append(f.sent, conv)
	f.models = append(f.models, model)
	if f.err != nil {
		return client.StreamResult{}, f.err
	}
	emit(f.answer)
	return client.StreamResult{Tokens: 3, Endpoint: f.ep, Content: f.answer}, nil
}
func (f *fakeChatAPI) rec(s string) { f.calls = append(f.calls, s) }
func (f *fakeChatAPI) Probe(n []string) error {
	f.rec("probe " + strings.Join(n, ","))
	return nil
}
func (f *fakeChatAPI) Bench(n []string) error { f.rec("bench"); return nil }
func (f *fakeChatAPI) SetSlots(e string, n int) error {
	f.rec("slots " + e)
	return nil
}
func (f *fakeChatAPI) SetTags(e string, t []string) error {
	f.rec("tags " + e + " " + strings.Join(t, "+"))
	return nil
}
func (f *fakeChatAPI) SetRole(r, p string, fb []string) error {
	f.rec("role " + r + " " + p + " [" + strings.Join(fb, " ") + "]")
	return nil
}
func (f *fakeChatAPI) StoreAll() ([]server.NodeStore, error) {
	return []server.NodeStore{{Node: "n1", Store: &store.Inventory{Files: []store.Entry{{SHA256: "s1", Name: "qwen.gguf", Complete: true}}}}, {Node: "n2", Store: &store.Inventory{}}}, nil
}
func (f *fakeChatAPI) StorePull(node, from, sha string) error {
	f.rec("pull " + node + " " + sha)
	return nil
}
func (f *fakeChatAPI) StoreRemove(ref string) (store.Entry, error) {
	f.rec("forget " + ref)
	return store.Entry{Name: "qwen.gguf", SHA256: ref}, nil
}
func (f *fakeChatAPI) TestRole(role string, m []string) []client.TestResult {
	return []client.TestResult{{Target: role, Endpoint: m[0], Answer: "pong"}}
}
func (f *fakeChatAPI) Route(string, int, int) (server.RouteExplain, error) {
	return server.RouteExplain{}, nil
}

// scriptUI answers the picker's questions: each script item is a substring of the option to take ("" with no
// options = free text, taken from the item itself after "text:"), or "esc".
type scriptUI struct {
	t       *testing.T
	script  []string
	asked   []agenttui.Question
	modelAs []string
}

func (u *scriptUI) SetModelName(n string) { u.modelAs = append(u.modelAs, n) }
func (u *scriptUI) Ask(_ context.Context, q agenttui.Question) (string, error) {
	u.asked = append(u.asked, q)
	if len(u.script) == 0 {
		u.t.Fatalf("unexpected question %q", q.Text)
	}
	s := u.script[0]
	u.script = u.script[1:]
	if s == "esc" {
		return "", agenttui.ErrAskCancelled
	}
	if len(q.Options) == 0 {
		return strings.TrimPrefix(s, "text:"), nil
	}
	for _, o := range q.Options {
		if strings.Contains(o, s) {
			return o, nil
		}
	}
	u.t.Fatalf("no option containing %q in %v", s, q.Options)
	return "", nil
}

func epS(name, node, model string, ok bool) server.EpStatus {
	return server.EpStatus{Endpoint: config.Endpoint{Name: name, Node: node, Model: model, TPS: 40}, OK: ok}
}

func chatStatus() server.Status {
	return server.Status{
		Endpoints: []server.EpStatus{epS("alpha", "n1", "llama", true), epS("zeta", "n2", "qwen", true), epS("dead", "n3", "qwen", false)},
		Roles:     map[string]server.RoleStatus{"scout": {Role: config.Role{Preferred: "dead", Fallback: []string{"zeta"}}, Active: "zeta"}},
		Nodes:     []server.NodeStatus{{Node: "n1"}, {Node: "n2"}},
	}
}

func TestChatPickerListsOfflineAndSwitches(t *testing.T) {
	isolateChat(t)
	api := &fakeChatAPI{st: chatStatus()}
	ctl := newChatCtl(api, "alpha", "", nil)
	ui := &scriptUI{t: t, script: []string{"dead"}}
	out, local, err := ctl.modelCmd(context.Background(), nil, ui)
	if err != nil || !local {
		t.Fatal(err)
	}
	opts := ui.asked[0].Options
	if len(opts) != 5 || !strings.Contains(strings.Join(opts, "\n"), "[endpoint] ✗ dead") || opts[len(opts)-1] != manageEntry {
		t.Fatalf("options %q", opts)
	}
	if !strings.HasPrefix(opts[0], "[role]") {
		t.Fatalf("roles come first (no ring models here): %q", opts)
	}
	if ui.asked[0].Default != chatpick.Label(mustFind(t, api.st, "alpha")) {
		t.Fatalf("default %q", ui.asked[0].Default)
	}
	if !strings.Contains(out, "Active target: **dead**") || !strings.Contains(out, "offline") || !strings.Contains(out, "Try instead: zeta") {
		t.Fatal(out)
	}
	if _, tgt := ctl.snapshot(); tgt != "dead" || ui.modelAs[0] != "dead" {
		t.Fatal("target not switched")
	}
}

func mustFind(t *testing.T, st server.Status, n string) chatpick.Entry {
	e, ok := chatpick.Find(st, n)
	if !ok {
		t.Fatal(n)
	}
	return e
}

func TestChatModelArgFilters(t *testing.T) {
	isolateChat(t)
	ctl := newChatCtl(&fakeChatAPI{st: chatStatus()}, "alpha", "", nil)
	ui := &scriptUI{t: t}
	if out, _, _ := ctl.modelCmd(context.Background(), []string{"llama"}, ui); !strings.Contains(out, "**alpha**") {
		t.Fatal(out)
	}
	if out, _, _ := ctl.modelCmd(context.Background(), []string{"nothing-here"}, ui); !strings.Contains(out, "Nothing matches") {
		t.Fatal(out)
	}
	ui.script = []string{"esc"}
	ctl.modelCmd(context.Background(), []string{"qwen"}, ui)
	if len(ui.asked) != 1 || !strings.Contains(ui.asked[0].Text, "2 matches") || len(ui.asked[0].Options) != 3 {
		t.Fatalf("filtered picker: %+v", ui.asked)
	}
}

func TestChatKeepsConversationAcrossModelSwitch(t *testing.T) {
	isolateChat(t)
	api := &fakeChatAPI{st: chatStatus(), answer: "hi there", ep: "alpha"}
	ctl := newChatCtl(api, "alpha", "be brief", nil)
	var got []string
	if _, err := ctl.stream(context.Background(), "hello", func(s string) { got = append(got, s) }); err != nil {
		t.Fatal(err)
	}
	ui := &scriptUI{t: t}
	out, _, _ := ctl.modelCmd(context.Background(), []string{"zeta"}, ui)
	if !strings.Contains(out, "1 earlier exchange") {
		t.Fatal(out)
	}
	api.ep, api.answer = "zeta", "second"
	ctl.stream(context.Background(), "and now?", func(string) {})
	conv := api.sent[1]
	var roles []string
	for _, m := range conv {
		roles = append(roles, m.Role+":"+m.Content)
	}
	want := "system:be brief|user:hello|assistant:hi there|user:and now?"
	if strings.Join(roles, "|") != want || api.models[1] != "zeta" {
		t.Fatalf("sent %v to %v", roles, api.models)
	}
	ctl.clear()
	ctl.stream(context.Background(), "fresh", func(string) {})
	if len(api.sent[2]) != 2 {
		t.Fatalf("after clear: %v", api.sent[2])
	}
}

func TestChatShowsWhoAnswered(t *testing.T) {
	isolateChat(t)
	api := &fakeChatAPI{st: chatStatus(), answer: "ok", ep: "zeta"}
	ctl := newChatCtl(api, "scout", "", nil)
	var b strings.Builder
	ctl.stream(context.Background(), "q", func(s string) { b.WriteString(s) })
	if !strings.Contains(b.String(), "↪ zeta answered for role scout — failover: preferred dead is down") {
		t.Fatal(b.String())
	}
	rows := ctl.infoRows()
	flat := ""
	for _, r := range rows {
		flat += r[0] + "=" + r[1] + ";"
	}
	if !strings.Contains(flat, "Answered by=zeta") || !strings.Contains(flat, "Context=1 exchanges") {
		t.Fatal(flat, ctl.header())
	}
	if h := ctl.header(); !strings.Contains(h, "role scout → zeta") {
		t.Fatal(h)
	}
}

func TestChatExplainsFailureWithAlternatives(t *testing.T) {
	isolateChat(t)
	api := &fakeChatAPI{st: chatStatus(), err: errors.New("http 502: connection refused")}
	ctl := newChatCtl(api, "dead", "", nil)
	_, err := ctl.stream(context.Background(), "q", func(string) {})
	if err == nil || !strings.Contains(err.Error(), "dead is offline") || !strings.Contains(err.Error(), "Try instead: zeta") || !strings.Contains(err.Error(), "connection refused") {
		t.Fatal(err)
	}
	if ctl.hist.Turns() != 0 {
		t.Fatal("a failed exchange must not enter the history")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	api.err = context.Canceled
	if _, err := ctl.stream(ctx, "q", func(string) {}); !errors.Is(err, context.Canceled) {
		t.Fatal("an interrupted request stays a plain cancel")
	}
}

func TestChatKeepsPartialAnswerWhenConnectionDrops(t *testing.T) {
	isolateChat(t)
	api := &dropChatAPI{fakeChatAPI: fakeChatAPI{st: chatStatus(), answer: "full", ep: "alpha"}, part: "half an ans"}
	api.err = errors.New("unexpected EOF")
	ctl := newChatCtl(api, "alpha", "", nil)
	var shown strings.Builder
	_, err := ctl.stream(context.Background(), "q1", func(s string) { shown.WriteString(s) })
	if err == nil || !strings.Contains(err.Error(), "connection lost, partial answer kept") || !strings.Contains(err.Error(), "/retry") {
		t.Fatal(err)
	}
	if shown.String() != "half an ans" || ctl.hist.Turns() != 1 || ctl.hist.Msgs[1].Content != "half an ans\n\n"+lostMark {
		t.Fatalf("history %+v", ctl.hist.Msgs)
	}
	// A different next message sees the partial.
	api.part, api.err = "", nil
	ctl.stream(context.Background(), "q2", func(string) {})
	if c := api.sent[1]; c[len(c)-2].Content != "half an ans\n\n"+lostMark {
		t.Fatalf("next message did not see the partial: %v", c)
	}
	// Retrying the same prompt replaces the partial.
	api.part, api.err = "x", errors.New("reset")
	ctl.stream(context.Background(), "q3", func(string) {})
	api.part, api.err = "", nil
	ctl.stream(context.Background(), "q3", func(string) {})
	n := len(ctl.hist.Msgs)
	if n != 6 || ctl.hist.Msgs[n-2].Content != "q3" || ctl.hist.Msgs[n-1].Content != "full" {
		t.Fatalf("retry history %+v", ctl.hist.Msgs)
	}
	// No token at all: a plain failure, nothing recorded.
	before := ctl.hist.Turns()
	api.err = errors.New("http 502: connection refused")
	if _, err := ctl.stream(context.Background(), "q4", func(string) {}); err == nil || strings.Contains(err.Error(), "partial answer kept") || ctl.hist.Turns() != before {
		t.Fatal("no-token failure must not be recorded", err)
	}
}

// dropChatAPI streams part and then fails with err (a connection that breaks mid-answer).
type dropChatAPI struct {
	fakeChatAPI
	part string
}

func (d *dropChatAPI) StreamChatMsgs(cx context.Context, model string, conv []client.ChatMessage, emit func(string)) (client.StreamResult, error) {
	if d.err != nil && d.part != "" {
		d.sent = append(d.sent, conv)
		emit(d.part)
		return client.StreamResult{}, d.err
	}
	return d.fakeChatAPI.StreamChatMsgs(cx, model, conv, emit)
}

func TestChatManageFlow(t *testing.T) {
	isolateChat(t)
	api := &fakeChatAPI{st: chatStatus()}
	ctl := newChatCtl(api, "alpha", "", nil)
	ui := &scriptUI{t: t, script: []string{
		"Manage models", // from the picker
		"[endpoint] ● zeta", "Probe capabilities",
		"[endpoint] ● zeta", "preferred endpoint", "scout",
		"[endpoint] ● zeta", "Edit tags", "text:fast gpu",
		"[endpoint] ● zeta", "which machines have it",
		"[endpoint] ● zeta", "fetch to a machine", "n2",
		"[endpoint] ● zeta", "forget it", "Yes, forget",
		"[endpoint] ● zeta", "forget it", "No",
		"esc",
	}}
	out, _, err := ctl.modelCmd(context.Background(), nil, ui)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"probe zeta", "role scout zeta [dead]", "tags zeta fast+gpu", "pull n2 s1", "forget s1"}
	if strings.Join(api.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls %v", api.calls)
	}
	for _, s := range []string{"✓ Probe capabilities", "on n1", "cancelled: Model file: forget"} {
		if !strings.Contains(out, s) {
			t.Fatalf("missing %q in\n%s", s, out)
		}
	}
	if len(ui.script) != 0 {
		t.Fatalf("unused answers %v", ui.script)
	}
}

func TestChatManageStartAsksForConfirmation(t *testing.T) {
	isolateChat(t)
	api := &fakeChatAPI{st: chatStatus()}
	var runs []string
	run := func(node, path string, dry, reg bool) (string, error) {
		runs = append(runs, node+" "+path+" dry="+map[bool]string{true: "1", false: "0"}[dry])
		return "ok", nil
	}
	ctl := newChatCtl(api, "alpha", "", run)
	ui := &scriptUI{t: t, script: []string{"[endpoint] ● alpha", "Start a model", "n1", "text:/m/a.gguf", "No", "esc"}}
	out, _, _ := ctl.modelCmd(context.Background(), []string{"manage"}, ui)
	if len(runs) != 1 || !strings.HasSuffix(runs[0], "dry=1") || !strings.Contains(out, "cancelled") {
		t.Fatalf("a declined start must stay a dry run: %v / %s", runs, out)
	}
}

func TestChatManageRoleAndUse(t *testing.T) {
	isolateChat(t)
	api := &fakeChatAPI{st: chatStatus()}
	ctl := newChatCtl(api, "alpha", "", nil)
	ui := &scriptUI{t: t, script: []string{"[role] ● scout", "Add a fallback", "alpha", "[role] ● scout", "Use it in this chat"}}
	out, _, _ := ctl.modelCmd(context.Background(), []string{"manage"}, ui)
	if api.calls[0] != "role scout dead [zeta alpha]" || !strings.Contains(out, "Active target: **scout**") {
		t.Fatal(api.calls, out)
	}
	if _, tgt := ctl.snapshot(); tgt != "scout" {
		t.Fatal(tgt)
	}
}

func (f *fakeChatAPI) StopEndpoint(name string, opt server.StopOptions) (server.StopReport, error) {
	f.rec("stop " + name)
	return server.StopReport{Endpoint: name, DryRun: opt.DryRun, Steps: []server.StopStep{{Role: "master", Node: "n1", Result: "stopped", OK: true}}}, nil
}
func (f *fakeChatAPI) EndpointsNotes(add []config.Endpoint, remove []string, mend bool) ([]string, error) {
	f.rec("rm " + strings.Join(remove, ","))
	return nil, nil
}

// Stopping a model from the chat shows what would be stopped first and does nothing without the yes.
func TestChatManageStopAsksFirst(t *testing.T) {
	isolateChat(t)
	api := &fakeChatAPI{st: chatStatus()}
	ctl := newChatCtl(api, "alpha", "", nil)
	ui := &scriptUI{t: t, script: []string{"[endpoint] ● alpha", "Stop its processes", "No", "esc"}}
	out, _, _ := ctl.modelCmd(context.Background(), []string{"manage"}, ui)
	if len(api.calls) != 1 || api.calls[0] != "stop alpha" || !strings.Contains(out, "cancelled") {
		t.Fatalf("a declined stop must stay a dry run: %v / %s", api.calls, out)
	}
	ui = &scriptUI{t: t, script: []string{"[endpoint] ● alpha", "Stop its processes", "Yes, stop them", "esc"}}
	out, _, _ = ctl.modelCmd(context.Background(), []string{"manage"}, ui)
	if len(api.calls) != 3 || !strings.Contains(out, "✓ Stop its processes") {
		t.Fatalf("%v / %s", api.calls, out)
	}
}

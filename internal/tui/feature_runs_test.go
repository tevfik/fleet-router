package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/server"
)

func runFixture() []server.Run {
	return []server.Run{{
		ID: "r1a", Fleet: "home", At: time.Now(), Model: "big-70b", Engine: "llama-ring", Endpoint: "g", Master: "dino",
		PredTPS: 10, PredTokenMS: 100,
		Parts:   []server.RunPart{{Node: "dino", First: 0, Layers: 30, ComputeMS: 40}, {Node: "mac", First: 30, Layers: 20, Output: true, ComputeMS: 20, RTTms: 5}},
		Samples: []server.RunSample{{TPS: 8, Busy: 1, Slots: 2, KV: 0.3, Health: "ok"}},
		Events:  []server.RunEvent{{At: time.Now(), What: "down: mac"}},
	}, {ID: "r0b", Model: "old", Ended: time.Now(), Master: "dino", Parts: []server.RunPart{{Node: "dino", Layers: 4}}}}
}

func runsModel(t *testing.T) model {
	_, cl := newRecorder(t)
	m := model{cl: cl, sc: scHome}
	for i, it := range home {
		if it.title == "Runs" {
			m.cur = i
		}
	}
	nm, _ := m.keyStr("enter", "")
	m = nm.(model)
	if m.sc != scRuns {
		t.Fatalf("home → %v", m.sc)
	}
	m.runs = runFixture()
	return m
}

func TestFeature_Runs_ListAndPath(t *testing.T) {
	m := runsModel(t)
	if m.listLen() != 2 {
		t.Fatalf("listLen %d", m.listLen())
	}
	v := m.viewRuns()
	for _, want := range []string{"big-70b", "dino→mac", "8.0 t/s", "dino (master)", "mac (output)", "blocks 30-49", "back to dino", "down: mac", "-20%"} {
		if !strings.Contains(v, want) {
			t.Errorf("missing %q in\n%s", want, v)
		}
	}
	m.cur = 1
	if v := m.viewRuns(); !strings.Contains(v, "ended") || strings.Contains(v, "big-70b ·") {
		t.Fatalf("the highlighted run's path:\n%s", v)
	}
}

func TestFeature_Runs_EndAndBindByKey(t *testing.T) {
	rec, cl := newRecorder(t)
	m := runsModel(t)
	m.cl = cl
	m, _ = press(t, m, "e")
	if rec.last["/fleet/runs/set"]["id"] != "r1a" || rec.last["/fleet/runs/set"]["end"] != true {
		t.Fatalf("%v", rec.last)
	}
	m, _ = press(t, m, "b")
	if m.editing != "runbind" || m.editBuf != "g" {
		t.Fatalf("editor: %q %q", m.editing, m.editBuf)
	}
	m.editBuf = "other"
	m, _ = press(t, m, "enter")
	if rec.last["/fleet/runs/set"]["endpoint"] != "other" {
		t.Fatalf("%v", rec.last["/fleet/runs/set"])
	}
	m.cur = 1
	if m, _ = press(t, m, "e"); !strings.Contains(m.note, "already ended") { // an ended run: nothing is sent
		t.Fatal(m.note)
	}
}

func TestFeature_Nodes_LeaveTheLine(t *testing.T) {
	rec, cl := newRecorder(t)
	m := model{cl: cl, sc: scNodes, st: server.Status{Nodes: []server.NodeStatus{{Node: "me", Self: true}, {Node: "far"}}}}
	var keys []string
	for _, b := range m.buttons() {
		keys = append(keys, b.label)
	}
	if !strings.Contains(strings.Join(keys, "|"), "Leave the line") {
		t.Fatalf("%v", keys)
	}
	press(t, m, "d")
	if rec.last["/fleet/drain"]["on"] != true {
		t.Fatalf("%v", rec.last)
	}
	m.st.Draining = true
	if !strings.Contains(buttonLabels(m), "Rejoin the line") {
		t.Fatal(buttonLabels(m))
	}
	press(t, m, "d")
	if rec.last["/fleet/drain"]["on"] != false {
		t.Fatalf("%v", rec.last)
	}
	m.cur = 1 // another machine cannot be sent away from here
	rec.last["/fleet/drain"] = nil
	if m, _ = press(t, m, "d"); !strings.Contains(m.note, "own fleet") || rec.last["/fleet/drain"] != nil {
		t.Fatalf("note %q sent %v", m.note, rec.last["/fleet/drain"])
	}
}

func buttonLabels(m model) string {
	var l []string
	for _, b := range m.buttons() {
		l = append(l, b.label)
	}
	return strings.Join(l, "|")
}

func TestFeature_Runs_GroupMarksInThePath(t *testing.T) {
	m := runsModel(t)
	m.st = server.Status{Endpoints: []server.EpStatus{{Endpoint: config.Endpoint{Name: "g"}, GroupLive: &server.GroupState{
		Members: []server.GroupMember{{Node: "mac", Err: "left the line"}}}}}}
	if v := m.viewRuns(); !strings.Contains(v, "✗ mac") && !strings.Contains(v, "left the line") {
		t.Fatalf("a member that left must be marked:\n%s", v)
	}
}

func TestPasteGoesIntoTheInputLine(t *testing.T) {
	_, cl := newRecorder(t)
	m := model{cl: cl, sc: scStore, editing: "storeadd"}
	for _, c := range []struct{ in, want string }{
		{"/home/user/models/a.gguf", "/home/user/models/a.gguf"},
		{"/home/user/models/a.gguf\n", "/home/user/models/a.gguf"},
		{"  '/home/user/my models/a.gguf'\r\n", "/home/user/my models/a.gguf"},
		{"line one\nline two", "line one line two"},
		{"a\x1b[31mb", "a[31mb"},
	} {
		m.editBuf = "~/"
		nm, _ := m.Update(tea.PasteMsg{Content: c.in})
		if got := nm.(model).editBuf; got != "~/"+c.want {
			t.Errorf("paste %q → %q, want %q", c.in, got, "~/"+c.want)
		}
	}
	m.editing, m.editBuf = "", ""
	if nm, _ := m.Update(tea.PasteMsg{Content: "x"}); nm.(model).editBuf != "" {
		t.Fatal("a paste outside an input line goes nowhere")
	}
}

func TestFeature_Runs_DeleteEndedByKey(t *testing.T) {
	rec, cl := newRecorder(t)
	m := runsModel(t)
	m.cl = cl
	// the active run is refused and nothing is sent
	m, _ = press(t, m, "x")
	if rec.last["/fleet/runs/delete"] != nil || len(m.runs) != 2 || !strings.Contains(m.note, "still active") {
		t.Fatalf("active run must stay: %v %d %q", rec.last, len(m.runs), m.note)
	}
	m.cur = 1
	m, note := press(t, m, "x")
	if rec.last["/fleet/runs/delete"]["id"] != "r0b" || rec.last["/fleet/runs/delete"]["prune"] != false {
		t.Fatalf("%v", rec.last)
	}
	if len(m.runs) != 1 || m.runs[0].ID != "r1a" || m.cur != 0 || !strings.Contains(note, "deleted") {
		t.Fatalf("runs %+v cur %d note %q", m.runs, m.cur, note)
	}
}

// s on a run: a dry run is sent and shown, nothing stops before the y; n cancels; y sends the real stop.
func TestFeature_Runs_StopAsksFirst(t *testing.T) {
	rec, cl := newRecorder(t)
	m := runsModel(t)
	m.cl = cl
	nm, cmd := m.keyStr("s", "")
	m = nm.(model)
	if cmd == nil {
		t.Fatal("no preview command")
	}
	pm, ok := cmd().(stopPreviewMsg)
	if !ok || rec.last["/fleet/runs/stop"]["dry_run"] != true || rec.last["/fleet/runs/stop"]["id"] != "r1a" {
		t.Fatalf("the first call must be a dry run: %v", rec.last)
	}
	pm.rep = server.StopReport{Endpoint: "g", DryRun: true, Steps: []server.StopStep{{Role: "master", Node: "dino", Port: 18090, Result: "would stop", OK: true}}}
	m = m.onStopPreview(pm)
	if !m.confirm || m.pend == nil || !strings.Contains(m.outText, "master") {
		t.Fatalf("waiting for the y: %v %v %q", m.confirm, m.pend, m.outText)
	}
	if bs := m.buttons(); bs[0].label != "Yes, stop them" {
		t.Fatalf("buttons %v", bs)
	}
	nm, _ = m.keyStr("n", "")
	m = nm.(model)
	if m.confirm || m.pend != nil {
		t.Fatal("n must cancel")
	}
	m = m.onStopPreview(pm)
	rec.last["/fleet/runs/stop"] = nil
	nm, cmd = m.keyStr("y", "")
	if _, ok := cmd().(stopDoneMsg); !ok || rec.last["/fleet/runs/stop"]["dry_run"] != false {
		t.Fatalf("y must send the real stop: %v", rec.last["/fleet/runs/stop"])
	}
	// an ended run is not stopped again
	m = nm.(model)
	m.cur = 1
	if m, _ = press(t, m, "s"); !strings.Contains(m.note, "already ended") {
		t.Fatal(m.note)
	}
}

// x on the model list asks first and removes with the roles mended.
func TestFeature_Dash_RemoveAsksFirst(t *testing.T) {
	rec, cl := newRecorder(t)
	m := editModel(cl)
	m.sc = scDash
	nm, _ := m.keyStr("x", "")
	m = nm.(model)
	if !m.confirm || m.pend == nil || m.pend.kind != "rm-ep" || rec.last["/fleet/endpoints"] != nil {
		t.Fatalf("x must ask, not remove: %v", rec.last)
	}
	nm, cmd := m.keyStr("y", "")
	cmd()
	if rec.last["/fleet/endpoints"]["mend_roles"] != true {
		t.Fatalf("%v", rec.last["/fleet/endpoints"])
	}
	_ = nm
}

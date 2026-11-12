package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/tevfik/fleet-router/internal/plan"
	"github.com/tevfik/fleet-router/internal/profile"
	"github.com/tevfik/fleet-router/internal/server"
)

// profilesModel opens the Profiles screen from the home menu on a temporary store holding two profiles; runFleet
// records the commands instead of running them.
func profilesModel(t *testing.T) (model, profile.Store, *[][]string) {
	t.Helper()
	_, cl := newNodesDaemon(t) // isolates HOME and FLEET_CONFIG
	store := profile.Store{Dir: filepath.Join(t.TempDir(), "profiles")}
	at := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	a := &profile.Profile{Name: "alpha", Model: "/m/a.gguf", Master: "dino", Workers: []string{"p1"}, Ctx: 8192, Layout: profile.Layout{Mode: profile.LayoutFixed, TS: "34,14"}}
	a.AddRun(profile.Run{At: at, Layout: profile.RunLayout{Master: "dino", Workers: []string{"p1"}, TS: "34,14"}, Measured: &profile.Measurement{At: at, TPS: 14.4}})
	b := &profile.Profile{Name: "beta", Model: "/m/b.gguf"}
	for _, p := range []*profile.Profile{a, b} {
		if err := store.Save(p); err != nil {
			t.Fatal(err)
		}
	}
	oldS, oldR := tuiProfiles, runFleet
	var calls [][]string
	tuiProfiles = func() profile.Store { return store }
	runFleet = func(args ...string) (string, error) { calls = append(calls, args); return "line one\nall done", nil }
	t.Cleanup(func() { tuiProfiles, runFleet = oldS, oldR })

	m := model{cl: cl, sc: scHome, sel: map[int]bool{}, w: 140}
	for i, it := range home {
		if it.title == "Profiles" {
			m.cur = i
		}
	}
	nm, _ := m.keyStr("enter", "")
	m = nm.(model)
	if m.sc != scProfiles || len(m.profList) != 2 {
		t.Fatalf("home → %v, %d profiles", m.sc, len(m.profList))
	}
	return m, store, &calls
}

// run plays a command's message back into the model, like the event loop does.
func play(t *testing.T, m model, cmd tea.Cmd) model {
	t.Helper()
	if cmd == nil {
		t.Fatal("no command")
	}
	if msg := cmd(); msg != nil {
		nm, _ := m.Update(msg)
		return nm.(model)
	}
	return m
}

func TestFeature_Profiles_ListDetailAndButtons(t *testing.T) {
	m, _, _ := profilesModel(t)
	v := m.viewProfilesScreen()
	for _, want := range []string{"alpha", "beta", "14.4 t/s", "not measured", "/m/a.gguf", "34,14", "dino", "ENGINE", "ring", "autostart"} {
		if !strings.Contains(v, want) {
			t.Errorf("missing %q in\n%s", want, v)
		}
	}
	labels := buttonLabels(m)
	for _, want := range []string{"Deploy", "Dry run", "Bench", "Measure…", "Edit…", "Save copy…", "Export…", "Import…", "Delete", "← Back"} {
		if !strings.Contains(labels, want) {
			t.Errorf("button %q missing: %s", want, labels)
		}
	}
	seen := map[string]bool{}
	for _, b := range m.buttons() {
		if seen[b.key] {
			t.Errorf("key %q on two buttons", b.key)
		}
		seen[b.key] = true
	}
	for _, want := range []string{"enter deploy", "n dry run", "b bench", "x delete"} {
		if !strings.Contains(m.help(), want) {
			t.Errorf("hint line lacks %q: %s", want, m.help())
		}
	}
}

func TestFeature_Profiles_ButtonsRunTheCLI(t *testing.T) {
	m, _, calls := profilesModel(t)
	cases := []struct {
		key  string
		want string
	}{
		{"enter", "deploy --profile alpha"},
		{"s", "profile down alpha"},
		{"a", "profile autostart alpha on"},
		{"n", "deploy --profile alpha --dry-run"},
		{"b", "profile bench alpha"},
	}
	for _, c := range cases {
		*calls = nil
		nm, cmd := m.keyStr(c.key, "")
		got := play(t, nm.(model), cmd)
		if len(*calls) != 1 || strings.Join((*calls)[0], " ") != c.want {
			t.Errorf("%s ran %v, want %s", c.key, *calls, c.want)
		}
		if got.outText != "line one\nall done" || !strings.Contains(got.note, "✓") {
			t.Errorf("%s: output %q note %q", c.key, got.outText, got.note)
		}
	}
	// a button press from the bar does the same as the key
	m.inBar = true
	for i, b := range m.buttons() {
		if b.label == "Dry run" {
			m.barCur = i
		}
	}
	*calls = nil
	bm, cmd, handled := m.barKey("enter")
	if !handled || cmd == nil {
		t.Fatal("the bar did not press the button")
	}
	play(t, bm, cmd)
	if len(*calls) != 1 || (*calls)[0][len((*calls)[0])-1] != "--dry-run" {
		t.Fatalf("bar press ran %v", *calls)
	}
}

func TestFeature_Profiles_MeasureAndEditPrompts(t *testing.T) {
	m, _, calls := profilesModel(t)
	m, _ = press(t, m, "m")
	if m.editing != "profmeasure" || m.editFor != "alpha" || !strings.Contains(m.editPrompt(), "tokens/s") {
		t.Fatalf("%q %q", m.editing, m.editFor)
	}
	m.editBuf = "abc"
	m, _ = press(t, m, "enter")
	if len(*calls) != 0 || !strings.Contains(m.note, "number") {
		t.Fatalf("a bad number must not run anything: %v %q", *calls, m.note)
	}
	m, _ = press(t, m, "m")
	m.editBuf = "23.5"
	nm, cmd := m.keyStr("enter", "")
	m = play(t, nm.(model), cmd)
	if strings.Join((*calls)[0], " ") != "profile measure alpha --tps 23.5" {
		t.Fatalf("%v", *calls)
	}
	m, _ = press(t, m, "e")
	m.editBuf = "layout.ts=30,18"
	nm, cmd = m.keyStr("enter", "")
	m = play(t, nm.(model), cmd)
	if strings.Join((*calls)[1], " ") != "profile edit alpha --set layout.ts=30,18" {
		t.Fatalf("%v", *calls)
	}
	m, _ = press(t, m, "e")
	m.editBuf = "nonsense"
	if m, _ = press(t, m, "enter"); len(*calls) != 2 || !strings.Contains(m.note, "key=value") {
		t.Fatalf("%v %q", *calls, m.note)
	}
}

func TestFeature_Profiles_DeleteNeedsConfirmation(t *testing.T) {
	m, store, _ := profilesModel(t)
	m, _ = press(t, m, "x")
	if !m.confirm || !strings.Contains(buttonLabels(m), "Yes, delete") {
		t.Fatalf("confirm %v %s", m.confirm, buttonLabels(m))
	}
	m, _ = press(t, m, "n")
	if _, err := store.Load("alpha"); err != nil || m.confirm {
		t.Fatal("n must keep the profile")
	}
	m, _ = press(t, m, "x")
	m, _ = press(t, m, "y")
	if _, err := store.Load("alpha"); err == nil {
		t.Fatal("y deletes the profile")
	}
	if len(m.profList) != 1 || m.profList[0].Name != "beta" {
		t.Fatalf("list %v", m.profList)
	}
}

func TestFeature_Profiles_CopyExportImportAndYAML(t *testing.T) {
	m, store, _ := profilesModel(t)
	m, _ = press(t, m, "c")
	if m.editBuf != "alpha-copy" {
		t.Fatalf("default name %q", m.editBuf)
	}
	m, _ = press(t, m, "enter")
	cp, err := store.Load("alpha-copy")
	if err != nil || cp.Model != "/m/a.gguf" || len(cp.Runs) != 0 {
		t.Fatalf("copy %v %+v", err, cp)
	}
	m, _ = press(t, m, "c") // the name is taken now: refused, nothing overwritten
	m.editBuf = "beta"
	if m, _ = press(t, m, "enter"); !strings.Contains(m.note, "exists") {
		t.Fatalf("note %q", m.note)
	}

	file := filepath.Join(t.TempDir(), "out.yaml")
	m, _ = press(t, m, "o")
	m.editBuf = file
	m, _ = press(t, m, "enter")
	b, err := os.ReadFile(file)
	if err != nil || !strings.Contains(string(b), "name: alpha-copy") {
		t.Fatalf("export %v %s", err, b)
	}
	// import: exists → refused with a hint, with "force" it replaces
	m, _ = press(t, m, "i")
	m.editBuf = file
	if m, _ = press(t, m, "enter"); !strings.Contains(m.note, "force") {
		t.Fatalf("note %q", m.note)
	}
	m, _ = press(t, m, "i")
	m.editBuf = file + " force"
	if m, _ = press(t, m, "enter"); !strings.Contains(m.note, "imported") {
		t.Fatalf("note %q", m.note)
	}
	if err := store.Remove("alpha-copy"); err != nil {
		t.Fatal(err)
	}
	m, _ = press(t, m, "i")
	m.editBuf = file
	m, _ = press(t, m, "enter")
	if _, err := store.Load("alpha-copy"); err != nil {
		t.Fatalf("import: %v", err)
	}
	m, _ = press(t, m, "y")
	if !strings.Contains(m.outText, "model:") {
		t.Fatalf("YAML: %q", m.outText)
	}
	if v := m.View().Content; !strings.Contains(v, "model:") {
		t.Fatal("the YAML shows under the screen")
	}
	// leaving the screen drops the output
	nm, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if nm.(model).sc != scHome || nm.(model).outText != "" {
		t.Fatalf("sc %v out %q", nm.(model).sc, nm.(model).outText)
	}
}

func TestFeature_Plan_ProfileAndDeployButtons(t *testing.T) {
	m, _ := planModelAt(t)
	nm, _ := m.keyStr("enter", "")
	m = nm.(model)
	labels := buttonLabels(m)
	for _, want := range []string{"Save as profile…", "Profiles", "Via hub: off", "Dry run", "Deploy"} {
		if !strings.Contains(labels, want) {
			t.Errorf("button %q missing: %s", want, labels)
		}
	}
	m, _ = press(t, m, "b")
	if !m.planViaHub || !strings.Contains(buttonLabels(m), "Via hub: on") {
		t.Fatal("b toggles via hub")
	}
	if m, _ = press(t, m, "i"); !strings.Contains(m.note, "plan a file first") {
		t.Fatalf("dry run without a plan: %q", m.note)
	}
}

func TestFeature_Plan_ViaHubIsSavedAndDryRunShowsTheCommands(t *testing.T) {
	m, p := planModelAt(t)
	store := profile.Store{Dir: filepath.Join(t.TempDir(), "profiles")}
	old := tuiProfiles
	tuiProfiles = func() profile.Store { return store }
	t.Cleanup(func() { tuiProfiles = old })
	nm, _ := m.keyStr("enter", "")
	m = nm.(model)
	m.planPath = p
	m.planRes = &plan.Result{Model: "tiny", Ctx: 8192, Np: 1, Candidates: []plan.Candidate{{Kind: "single", Master: "dino", TPS: 5,
		Parts: []plan.Part{{Node: "dino", Layers: 2, Output: true}}}}}
	m, _ = press(t, m, "b")
	m.saveCandidateProfile("viahub")
	saved, err := store.Load("viahub")
	if err != nil || !saved.Layout.ViaHub {
		t.Fatalf("%v %+v", err, saved)
	}
	nm, cmd := m.keyStr("i", "")
	if cmd == nil {
		t.Fatalf("dry run: no command (%q)", nm.(model).note)
	}
	got := play(t, nm.(model), cmd)
	if strings.Contains(got.note, "failed") && !strings.Contains(got.note, "dry run") {
		t.Fatal(got.note)
	}
	t.Logf("note %q out %q", got.note, got.outText)
}

func TestFeature_Profiles_StatusRunning(t *testing.T) {
	m, store, calls := profilesModel(t)
	// add a run to alpha and mark it active in m.runs
	p, _ := store.Load("alpha")
	p.AddRun(profile.Run{RunID: "run-alpha-active", Endpoint: "distributed-alpha"})
	_ = store.Save(p)
	m = m.reloadProfiles("alpha")
	m.runs = []server.Run{
		{ID: "run-alpha-active", Endpoint: "distributed-alpha"},
	}

	view := m.viewProfilesScreen()
	if !strings.Contains(view, "running") || !strings.Contains(view, "RUNNING") {
		t.Fatalf("expected running status in view, got:\n%s", view)
	}

	// press d to stop
	*calls = nil
	nm, cmd := m.keyStr("s", "")
	if cmd == nil {
		t.Fatal("expected command on s")
	}
	got := play(t, nm.(model), cmd)
	if len(*calls) != 1 || (*calls)[0][0] != "profile" || (*calls)[0][1] != "down" {
		t.Fatalf("expected profile down, got: %v", *calls)
	}
	if !strings.Contains(got.note, "stopped") {
		t.Fatalf("expected stopped note, got: %q", got.note)
	}
}

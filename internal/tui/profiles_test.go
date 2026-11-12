package tui

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/plan"
	"github.com/tevfik/fleet-router/internal/profile"
)

func TestPlanScreenSavesCandidateAsProfileAndListsProfiles(t *testing.T) {
	_, cl := newNodesDaemon(t)
	store := profile.Store{Dir: filepath.Join(t.TempDir(), "profiles")}
	oldS, oldA := tuiProfiles, applyProfile
	tuiProfiles = func() profile.Store { return store }
	var applied string
	applyProfile = func(name string) (string, error) { applied = name; return "deployed\nURL http://x/v1", nil }
	t.Cleanup(func() { tuiProfiles, applyProfile = oldS, oldA })

	m := model{cl: cl, sel: map[int]bool{}, w: 140}.enterPlan()
	r := plan.Result{Model: "big", Ctx: 8192, Np: 1, Candidates: []plan.Candidate{{Kind: "split", Master: "dino", TPS: 14.4,
		Parts: []plan.Part{{Node: "dino", Layers: 34, Output: true}, {Node: "papa", Layers: 14, First: 34, CPUExperts: 7}}}}}
	m.planRes, m.planPath = &r, "/models/Ornith-122B.gguf"

	nm, _ := m.keyStr("a", "a")
	m = nm.(model)
	if m.editing != "planprofile" || !strings.HasPrefix(m.editBuf, "ornith-122b") {
		t.Fatalf("editing %q %q", m.editing, m.editBuf)
	}
	m.editBuf = "ornith-hub"
	nm, _ = m.keyStr("enter", "")
	m = nm.(model)
	p, err := store.Load("ornith-hub")
	if err != nil || p.Master != "dino" || p.Workers[0] != "papa" || p.Layout.TS != "34,14" || p.Layout.WorkerCPUExperts["papa"] != "41-47" || p.Layout.Mode != profile.LayoutFixed {
		t.Fatalf("%v %+v", err, p)
	}
	at := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	p.AddRun(profile.Run{At: at, Layout: profile.RunLayout{Master: "dino", Workers: []string{"papa"}, TS: "34,14"}, Measured: &profile.Measurement{At: at, TPS: 14.4}})
	store.Save(p)

	nm, _ = m.keyStr("L", "L")
	m = nm.(model)
	v := m.viewPlan()
	if !m.profOpen || !strings.Contains(v, "ornith-hub") || !strings.Contains(v, "14.4 t/s") {
		t.Fatalf("profiles panel:\n%s", v)
	}
	_, cmd := m.keyStr("enter", "")
	if cmd == nil {
		t.Fatal("enter deploys the profile")
	}
	if msg := cmd(); !strings.Contains(string(msg.(noteMsg)), "deployed") || applied != "ornith-hub" {
		t.Fatalf("%v %s", msg, applied)
	}
	nm, _ = m.keyStr("x", "x")
	m = nm.(model)
	if _, err := store.Load("ornith-hub"); err != nil {
		t.Fatal("x asks before deleting")
	}
	nm, _ = m.keyStr("y", "y")
	m = nm.(model)
	if _, err := store.Load("ornith-hub"); err == nil {
		t.Fatal("x deletes the profile")
	}
	nm, _ = m.keyStr("esc", "")
	if nm.(model).profOpen {
		t.Fatal("esc closes the panel")
	}
}

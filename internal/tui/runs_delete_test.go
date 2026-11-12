package tui

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tevfik/fleet-router/internal/client"
	"github.com/tevfik/fleet-router/internal/profile"
	"github.com/tevfik/fleet-router/internal/server"
)

// runsDaemon is a fake daemon: /fleet/status answers, /fleet/runs answers with runs (or fails when broken), every
// POST is recorded.
type runsDaemon struct {
	mu     sync.Mutex
	runs   []server.Run
	broken bool
	posts  []string
	bodies map[string]map[string]any
}

func newRunsDaemon(t *testing.T, runs []server.Run) (*runsDaemon, *client.Client) {
	d := &runsDaemon{runs: runs, bodies: map[string]map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		switch {
		case r.URL.Path == "/fleet/runs" && r.Method == "GET":
			if d.broken {
				http.Error(w, `{"error":{"message":"timeout asking the peers"}}`, 502)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"runs": d.runs})
		case r.Method == "POST":
			b, _ := io.ReadAll(r.Body)
			var body map[string]any
			json.Unmarshal(b, &body)
			d.posts = append(d.posts, r.URL.Path)
			d.bodies[r.URL.Path] = body
			w.Write([]byte("{}"))
		default:
			w.Write([]byte("{}"))
		}
	}))
	t.Cleanup(srv.Close)
	return d, client.New(strings.TrimPrefix(srv.URL, "http://"))
}

// A list fetched before d was pressed and answered after it must not bring the deleted row back (the GET of the
// runs asks every fleet and probes the engines, so a tick's fetch is often still out when the user deletes).
func TestRuns_DeletedRowDoesNotComeBackFromAnOlderFetch(t *testing.T) {
	d, cl := newRunsDaemon(t, runFixture())
	m := runsModel(t)
	m.cl = cl
	older := m.fetch() // a tick's fetch, still out
	m.cur = 1
	m, note := press(t, m, "x")
	if !strings.Contains(note, "deleted") || len(m.runs) != 1 {
		t.Fatalf("d: %q %d", note, len(m.runs))
	}
	nm, _ := m.Update(older()) // answers with the list as it was
	m = nm.(model)
	for _, r := range m.runs {
		if r.ID == "r0b" {
			t.Fatalf("the deleted run came back from an older fetch: %+v", m.runs)
		}
	}
	// the fetch made after the delete is taken (here the daemon still lists it, so it shows: the daemon is the truth)
	d.mu.Lock()
	d.runs = runFixture()[:1]
	d.mu.Unlock()
	nm, _ = m.Update(m.fetch()())
	if m = nm.(model); len(m.runs) != 1 || m.runs[0].ID != "r1a" {
		t.Fatalf("a newer fetch must be taken: %+v", m.runs)
	}
}

// A failed GET of the runs must not show an empty list (as if there were no runs) and must say why.
func TestRuns_FailedFetchKeepsTheListAndSaysWhy(t *testing.T) {
	d, cl := newRunsDaemon(t, runFixture())
	m := runsModel(t)
	m.cl = cl
	d.mu.Lock()
	d.broken = true
	d.mu.Unlock()
	nm, _ := m.Update(m.fetch()())
	m = nm.(model)
	if len(m.runs) != 2 {
		t.Fatalf("a failed fetch blanked the list: %+v", m.runs)
	}
	if v := m.viewRuns(); !strings.Contains(v, "timeout asking the peers") {
		t.Fatalf("the error is not shown:\n%s", v)
	}
	d.mu.Lock()
	d.broken = false
	d.mu.Unlock()
	nm, _ = m.Update(m.fetch()())
	if v := nm.(model).viewRuns(); strings.Contains(v, "timeout") {
		t.Fatalf("the error stays after a good fetch:\n%s", v)
	}
}

// d on an active run whose processes are gone (Stale set by the daemon) asks, then closes the record without
// stopping anything and deletes it; a healthy active run is still refused.
func TestRuns_DeleteStaleActiveRun(t *testing.T) {
	d, cl := newRunsDaemon(t, nil)
	m := runsModel(t)
	m.cl = cl
	m, _ = press(t, m, "x")
	if m.confirm || len(d.posts) != 0 || !strings.Contains(m.note, "still active") {
		t.Fatalf("a live active run must be refused: %v %q", d.posts, m.note)
	}
	m.runs[0].Stale = "no healthy answer from g since 2026-11-01 10:00"
	m, _ = press(t, m, "x")
	if !m.confirm || m.pend == nil || len(d.posts) != 0 || !strings.Contains(m.note, "looks dead") {
		t.Fatalf("d on a stale run must ask first: confirm %v pend %v posts %v note %q", m.confirm, m.pend, d.posts, m.note)
	}
	if bs := buttonLabels(m); !strings.Contains(bs, "Yes") {
		t.Fatal(bs)
	}
	m, note := press(t, m, "y")
	if strings.Join(d.posts, " ") != "/fleet/runs/set /fleet/runs/delete" {
		t.Fatalf("posts %v", d.posts)
	}
	if b := d.bodies["/fleet/runs/set"]; b["id"] != "r1a" || b["end"] != true || b["keep_processes"] != true {
		t.Fatalf("the record must be closed without stopping anything: %v", b)
	}
	if d.bodies["/fleet/runs/delete"]["id"] != "r1a" || !strings.Contains(note, "deleted") || m.confirm {
		t.Fatalf("%v %q", d.bodies["/fleet/runs/delete"], note)
	}
}

// esc while a stop waits for its y cancels it completely: the question goes and a later y does nothing.
func TestRuns_EscCancelsAPendingStop(t *testing.T) {
	d, cl := newRunsDaemon(t, nil)
	m := runsModel(t)
	m.cl = cl
	m = m.onStopPreview(stopPreviewMsg{sc: scRuns, act: pendingAct{kind: "stop-run", name: "r1a"},
		rep: server.StopReport{Endpoint: "g", DryRun: true, Steps: []server.StopStep{{Role: "master", Result: "would stop", OK: true}}}})
	nm, _ := m.keyStr("esc", "")
	m = nm.(model)
	if m.confirm || m.pend != nil || strings.Contains(m.note, "stop") || m.outText != "" || m.sc != scRuns {
		t.Fatalf("esc: confirm %v pend %v note %q out %q sc %v", m.confirm, m.pend, m.note, m.outText, m.sc)
	}
	press(t, m, "y")
	if len(d.posts) != 0 {
		t.Fatalf("y after esc sent %v", d.posts)
	}
}

// x on the Profiles screen asks about the highlighted profile; moving the cursor before the y must not delete
// another one.
func TestProfiles_DeleteTargetsTheProfileAskedAbout(t *testing.T) {
	m, store, _ := profilesModel(t)
	m, _ = press(t, m, "x") // alpha
	if !strings.Contains(m.note, "alpha") {
		t.Fatal(m.note)
	}
	m, _ = press(t, m, "down")
	m, _ = press(t, m, "y")
	if _, err := store.Load("beta"); err != nil {
		t.Fatalf("beta was deleted although alpha was asked about: %v", err)
	}
	if _, err := store.Load("alpha"); err == nil {
		t.Fatal("alpha must be deleted")
	}
	if len(m.profList) != 1 || m.profList[0].Name != "beta" || m.cur != 0 {
		t.Fatalf("list %v cur %d", m.profList, m.cur)
	}
}

// The profiles panel of the Plan screen: ↑/↓ move its cursor (not the file list's behind it), x asks first and
// deletes the highlighted profile.
func TestPlanProfilesPanel_MoveAndDeleteAsks(t *testing.T) {
	_, cl := newNodesDaemon(t)
	store := profile.Store{Dir: filepath.Join(t.TempDir(), "profiles")}
	for _, n := range []string{"alpha", "beta", "gamma"} {
		if err := store.Save(&profile.Profile{Name: n, Model: "/m/" + n + ".gguf"}); err != nil {
			t.Fatal(err)
		}
	}
	old := tuiProfiles
	tuiProfiles = func() profile.Store { return store }
	t.Cleanup(func() { tuiProfiles = old })

	m := model{cl: cl, sel: map[int]bool{}, w: 140}.enterPlan()
	m.planFiles = nil
	m, _ = press(t, m, "L")
	if !m.profOpen || len(m.profList) != 3 {
		t.Fatalf("panel %v %d", m.profOpen, len(m.profList))
	}
	m, _ = press(t, m, "down")
	m, _ = press(t, m, "down")
	if m.profCur != 2 || m.inBar {
		t.Fatalf("↓ must move the panel's cursor: profCur %d inBar %v", m.profCur, m.inBar)
	}
	m, _ = press(t, m, "up")
	if m.profCur != 1 {
		t.Fatalf("↑: profCur %d", m.profCur)
	}
	m, _ = press(t, m, "x")
	if _, err := store.Load("beta"); err != nil {
		t.Fatal("x must ask before deleting")
	}
	if !strings.Contains(m.note, "beta") {
		t.Fatal(m.note)
	}
	m, _ = press(t, m, "n")
	if _, err := store.Load("beta"); err != nil {
		t.Fatal("n keeps it")
	}
	m, _ = press(t, m, "x")
	m, _ = press(t, m, "y")
	if _, err := store.Load("beta"); err == nil {
		t.Fatal("y deletes it")
	}
	for _, n := range []string{"alpha", "gamma"} {
		if _, err := store.Load(n); err != nil {
			t.Fatalf("%s was deleted", n)
		}
	}
	if len(m.profList) != 2 || m.profCur != 1 || !strings.Contains(m.note, "beta deleted") {
		t.Fatalf("list %d cur %d note %q", len(m.profList), m.profCur, m.note)
	}
	// deleting the last row leaves the cursor on the new last row
	m, _ = press(t, m, "x")
	m, _ = press(t, m, "y")
	if len(m.profList) != 1 || m.profCur != 0 {
		t.Fatalf("list %d cur %d", len(m.profList), m.profCur)
	}
}

package tui

import (
	"context"
	"github.com/tevfik/fleet-router/internal/deploy"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/client"
	"github.com/tevfik/fleet-router/internal/plan"
	"github.com/tevfik/fleet-router/internal/server"
)

// The Hub screen must render from what fetch() stored, not call the daemon: a hung daemon made every keypress wait
// for the client timeout.
func TestHubViewDoesNotCallTheDaemon(t *testing.T) {
	hang := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-hang }))
	defer srv.Close()
	defer close(hang)
	m := testModel()
	m.sc, m.w = scHub, 120
	m.cl = client.New(srv.URL)
	start := time.Now()
	_ = m.viewHub()
	_, _ = m.keyHub("j")
	if d := time.Since(start); d > time.Second {
		t.Fatalf("viewHub/keyHub waited %s for the daemon", d)
	}
}

func TestProfilesAndNodesViewsDoNotCallTheDaemon(t *testing.T) {
	hang := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-hang }))
	defer srv.Close()
	defer close(hang)
	m := testModel()
	m.w = 120
	m.cl = client.New(srv.URL)
	start := time.Now()
	m.sc = scProfiles
	_ = m.viewProfilesScreen()
	m.sc = scNodes
	_ = m.viewNodes()
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("viewProfilesScreen/viewNodes waited %s for the daemon", d)
	}
}

// A failed status fetch keeps the last good status (the lists must not go blank).
func TestFailedFetchKeepsLastStatus(t *testing.T) {
	m := testModel()
	m.st = server.Status{Roles: map[string]server.RoleStatus{"coder": {}}}
	nm, _ := m.Update(statusMsg{err: http.ErrHandlerTimeout})
	got := nm.(model)
	if got.err == nil || len(got.st.Roles) != 1 {
		t.Fatalf("err=%v roles=%d", got.err, len(got.st.Roles))
	}
}

// Ticks do not stack fetches: with one out, the next tick starts none.
func TestTickDoesNotStackFetches(t *testing.T) {
	m := testModel()
	nm, _ := m.Update(tickMsg(time.Now()))
	m = nm.(model)
	if !m.fetching {
		t.Fatal("the first tick starts a fetch")
	}
	nm, _ = m.Update(tickMsg(time.Now()))
	m2 := nm.(model)
	if !m2.fetching || !m2.fetchSince.Equal(m.fetchSince) {
		t.Fatal("a tick while a fetch is out must not start another")
	}
	nm, _ = m2.Update(statusMsg{})
	if nm.(model).fetching {
		t.Fatal("the answer clears the flag")
	}
}

// The advertised j/k shortcuts of Economy, Hub and Nodes reach their screens (the global vim keys took them first).
func TestScreenLetterShortcutsAreNotSwallowed(t *testing.T) {
	m := testModel()
	m.sc, m.cur = scHub, 2
	nm, _ := m.keyStr("k", "k")
	if nm.(model).cur != 2 {
		t.Fatal("Hub k must not move the cursor (it edits the API key)")
	}
	m.sc, m.cur = scEconomy, 1
	nm, _ = m.keyStr("j", "j")
	if nm.(model).cur != 1 {
		t.Fatal("Economy j must not move the cursor (it toggles the tunnel)")
	}
	m.sc, m.cur = scRoles, 0
	if m.letterIsShortcut("j") {
		t.Fatal("other screens keep vim navigation")
	}
}

func TestSecondDeployIsRefusedWhileOneRuns(t *testing.T) {
	m := testModel()
	m.planRes = &plan.Result{Candidates: []plan.Candidate{{Master: "dino", Kind: "single"}}}
	m.deployChan = make(chan string, 1)
	nm, cmd := m.deployCandidate(false)
	if cmd != nil || nm.(model).deployChan != m.deployChan {
		t.Fatal("a running deploy must not be replaced or doubled")
	}
}

func TestLateDeployProgressDoesNotBringTheBannerBack(t *testing.T) {
	m := testModel()
	m.deployStatus = ""
	nm, _ := m.Update(deployProgressMsg("[3/4] late"))
	if nm.(model).deployStatus != "" {
		t.Fatal("a step after the deploy ended must be ignored")
	}
}

func TestXCancelsARunningDeployAndDoneShowsWarnings(t *testing.T) {
	m := testModel()
	m.sc = scPlan
	ctx, cancel := context.WithCancel(context.Background())
	m.deployChan, m.deployCancel = make(chan string, 1), cancel
	_, _ = m.keyPlan("x")
	if ctx.Err() == nil {
		t.Fatal("x must cancel the running deploy")
	}
	nm, _ := m.Update(deployDoneMsg{res: &deploy.DeployResult{EndpointName: "e"}, warn: " · the run was not recorded: boom"})
	if got := nm.(model); got.deployCancel != nil || got.deployChan != nil || !strings.Contains(got.note, "not recorded") {
		t.Fatalf("done: %+v note=%q", got.deployChan, got.note)
	}
}

func TestDropStreamRemovesOnlyThatListener(t *testing.T) {
	stopStreams()
	streamMu.Lock()
	streams = []streamInfo{{Listen: "127.0.0.1:1", stop: func() {}}, {Listen: "127.0.0.1:2", stop: func() {}}}
	streamMu.Unlock()
	dropStream("127.0.0.1:1")
	if l := activeStreams(); len(l) != 1 || l[0].Listen != "127.0.0.1:2" {
		t.Fatalf("%+v", l)
	}
	stopStreams()
}

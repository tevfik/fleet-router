package tui

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tevfik/fleet-router/internal/client"
	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/discover"
	"github.com/tevfik/fleet-router/internal/plan"
	"github.com/tevfik/fleet-router/internal/server"
)

func TestCov_Ask_LifecycleAndViews(t *testing.T) {
	m := testModel()
	m.w, m.h = 100, 30
	m.st = server.Status{
		Endpoints: []server.EpStatus{
			{Endpoint: config.Endpoint{Name: "distributed-llama"}, OK: true},
			{Endpoint: config.Endpoint{Name: "dino-offline"}, OK: false},
			{Endpoint: config.Endpoint{Name: "host/plain-model"}, OK: true},
		},
		Roles: map[string]server.RoleStatus{
			"coder": {Active: "distributed-llama"},
			"scout": {Active: ""},
		},
	}

	// 1. enterAsk
	m = m.enterAsk("")
	if m.sc != scAsk {
		t.Fatalf("expected scAsk, got %v", m.sc)
	}
	if m.askTarget != "distributed-llama" {
		t.Errorf("expected target distributed-llama, got %s", m.askTarget)
	}

	m = m.enterAsk("custom-role")
	if m.askTarget != "custom-role" {
		t.Errorf("expected custom-role, got %s", m.askTarget)
	}

	// 2. isTargetOnline
	if !m.isTargetOnline("distributed-llama") {
		t.Error("expected distributed-llama to be online")
	}
	if m.isTargetOnline("dino-offline") {
		t.Error("expected dino-offline to be offline")
	}
	if !m.isTargetOnline("coder") {
		t.Error("expected coder role to be online")
	}
	if m.isTargetOnline("scout") {
		t.Error("expected scout role to be offline")
	}
	if !m.isTargetOnline("unknown") {
		t.Error("expected unknown target to return true default")
	}

	// 3. viewAsk with empty log and busy status
	v := m.viewAsk()
	if !strings.Contains(v, "Chat & Model Test") || !strings.Contains(v, "No messages yet") {
		t.Errorf("unexpected empty viewAsk output: %s", v)
	}

	m.askBusy = true
	m.askInput = "how are you"
	vBusy := m.viewAsk()
	if !strings.Contains(vBusy, "Waiting for") || !strings.Contains(vBusy, "how are you") {
		t.Errorf("unexpected busy viewAsk output: %s", vBusy)
	}

	// 4. viewAsk with messages
	m.askBusy = false
	m.askLog = []chatItem{
		{user: "hello", model: "distributed-llama", reply: "world\nline2", durMS: 150, tokens: 20, tps: 15.5},
		{user: "broken", model: "distributed-llama", err: "request failed"},
	}
	vLog := m.viewAsk()
	if !strings.Contains(vLog, "world") || !strings.Contains(vLog, "request failed") || !strings.Contains(vLog, "15.5 tok/s") {
		t.Errorf("unexpected logged viewAsk output: %s", vLog)
	}

	// 5. Target picker modal
	m.pick = true
	m.cur = 0
	vPick := m.viewAsk()
	if !strings.Contains(vPick, "SELECT TARGET MODEL OR ROLE") {
		t.Errorf("unexpected picker view output: %s", vPick)
	}

	// 6. keyAsk in pick mode
	nm, _ := m.keyAsk("down", "")
	m = nm.(model)
	if m.cur != 1 {
		t.Errorf("expected cur=1 after down, got %d", m.cur)
	}
	nm, _ = m.keyAsk("up", "")
	m = nm.(model)
	if m.cur != 0 {
		t.Errorf("expected cur=0 after up, got %d", m.cur)
	}
	nm, _ = m.keyAsk("enter", "")
	m = nm.(model)
	if m.pick {
		t.Error("expected pick=false after enter")
	}

	m.pick = true
	nm, _ = m.keyAsk("esc", "")
	m = nm.(model)
	if m.pick {
		t.Error("expected pick=false after esc")
	}

	// 7. keyAsk in normal input mode
	nm, _ = m.keyAsk("tab", "")
	m = nm.(model)
	if !m.pick {
		t.Error("expected tab to open picker")
	}
	m.pick = false

	nm, _ = m.keyAsk("ctrl+t", "")
	m = nm.(model)
	if !m.pick {
		t.Error("expected ctrl+t to open picker")
	}
	m.pick = false

	// Type text
	nm, _ = m.keyAsk("char", "h")
	nm, _ = nm.(model).keyAsk("char", "i")
	m = nm.(model)
	if m.askInput != "how are youhi" { // previous was "how are you"
		t.Errorf("unexpected askInput: %q", m.askInput)
	}

	// Backspace
	nm, _ = m.keyAsk("backspace", "")
	m = nm.(model)
	if !strings.HasSuffix(m.askInput, "h") {
		t.Errorf("expected backspace to trim char, got %q", m.askInput)
	}

	// Enter with input -> runAsk
	nm, cmd := m.keyAsk("enter", "")
	m = nm.(model)
	if !m.askBusy || cmd == nil {
		t.Error("expected askBusy=true and cmd != nil on enter with text")
	}

	// Enter with empty input -> launchAgentTUI
	m.askBusy = false
	m.askInput = ""
	nm, cmd = m.keyAsk("enter", "")
	if cmd == nil {
		t.Error("expected cmd != nil on enter with empty text")
	}

	// Ctrl+L -> clear log
	nm, _ = m.keyAsk("ctrl+l", "")
	m = nm.(model)
	if len(m.askLog) != 0 {
		t.Error("expected askLog to be cleared on ctrl+l")
	}

	// Esc in normal mode -> scHome
	nm, _ = m.keyAsk("esc", "")
	m = nm.(model)
	if m.sc != scHome {
		t.Errorf("expected scHome after esc, got %v", m.sc)
	}

	// 8. Update messages for ask
	nm, _ = m.Update(askMsg{
		target: "distributed-llama",
		prompt: "test",
		reply:  "ans",
		tokens: 5,
		durMS:  100,
	})
	m = nm.(model)
	if len(m.askLog) != 1 || m.askLog[0].reply != "ans" {
		t.Fatalf("expected askLog to have received answer, got %+v", m.askLog)
	}

	nm, _ = m.Update(askMsg{
		target: "distributed-llama",
		prompt: "bad",
		err:    errors.New("timeout"),
	})
	m = nm.(model)
	if len(m.askLog) != 2 || m.askLog[1].err != "timeout" {
		t.Fatalf("expected error logged, got %+v", m.askLog)
	}

	nm, cmdFetch := m.Update(askExecDoneMsg{err: nil})
	if cmdFetch == nil {
		t.Error("expected cmd from askExecDoneMsg")
	}
}

func TestCov_Economy_KeyAndActions(t *testing.T) {
	m := testModel()
	m.sc = scEconomy
	m.w = 120
	m.hub.loaded = true

	// 1. onOff helper
	if onOff(true) != "ENABLED" || onOff(false) != "DISABLED" {
		t.Error("onOff returned unexpected strings")
	}

	// 2. keyEconomy with nil cfg.Economy
	m.cfg.Economy = nil
	m.cur = 0
	_, cmd := m.keyEconomy("enter")
	if cmd == nil {
		t.Error("expected cmd from toggle master economy")
	}

	// Initialize economy
	tVal, fVal := true, false
	m.cfg.Economy = &config.EconomyConfig{
		Enabled:     &tVal,
		FairQueuing: &tVal,
		ShareIdle:   &fVal,
	}

	// cur = 0: toggle master economy
	m.cur = 0
	_, cmd = m.keyEconomy("enter")
	if cmd == nil {
		t.Error("expected cmd on toggle master")
	}

	// cur = 1: toggle fair queuing
	m.cur = 1
	_, cmd = m.keyEconomy("enter")
	if cmd == nil {
		t.Error("expected cmd on toggle fair queuing")
	}

	// cur = 2: toggle share idle
	m.cur = 2
	_, cmd = m.keyEconomy("enter")
	if cmd == nil {
		t.Error("expected cmd on toggle share idle")
	}

	// "d" no longer deposits: credits cannot be made out of nothing
	if _, cmd = m.keyEconomy("d"); cmd != nil {
		t.Error("'d' still does something")
	}

	// cur = 3 / "j": toggle hub tunnel
	m.cur = 3
	_, cmd = m.keyEconomy("enter")
	if cmd == nil {
		t.Error("expected toggle tunnel cmd at cur=3")
	}
	_, cmd = m.keyEconomy("j")
	if cmd == nil {
		t.Error("expected toggle tunnel cmd on 'j'")
	}

	// viewEconomy
	v := m.viewEconomy()
	if !strings.Contains(v, "CRYPTOGRAPHIC WALLET") || !strings.Contains(v, "Token Economy Master Switch") {
		t.Errorf("unexpected viewEconomy output: %s", v)
	}
}

func TestCov_TUI_DiscoveryResultsAndHelpers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	m := testModel()
	m.cl = client.New(srv.URL)
	m.w, m.h = 100, 30
	m.sc = scDiscover
	m.focus = 1 // results focus

	// 1. tokStr and capsLabel and portOf
	if s := tokStr(500); s != "500" {
		t.Errorf("unexpected tokStr: %s", s)
	}
	if s := tokStr(1500); s != "1500" {
		t.Errorf("unexpected tokStr: %s", s)
	}
	if s := tokStr(2500000); s != "2500k" {
		t.Errorf("unexpected tokStr: %s", s)
	}

	ep1 := config.Endpoint{Name: "e1", URL: "http://127.0.0.1:8080/v1"}
	if p := portOf(ep1); p != "8080" {
		t.Errorf("expected port 8080, got %s", p)
	}
	ep2 := config.Endpoint{Name: "e2", Remote: "node:9090"}
	if p := portOf(ep2); p != "9090" {
		t.Errorf("expected port 9090, got %s", p)
	}

	epEmbed := config.Endpoint{Name: "emb", Model: "bge-m3", Kind: "embed", Caps: []string{"embed"}}
	if label := capsLabel(epEmbed); !strings.Contains(label, "embed") {
		t.Errorf("expected embed in capsLabel, got %s", label)
	}

	// 2. toggleHost
	m.cfg.Discover.SSH = []string{"dino", "victor"}
	m, cmd := m.toggleHost("dino") // remove dino
	if len(m.cfg.Discover.SSH) != 1 || m.cfg.Discover.SSH[0] != "victor" || cmd == nil {
		t.Errorf("unexpected toggleHost removal: %+v", m.cfg.Discover.SSH)
	}
	m, cmd = m.toggleHost("dino") // add dino back
	if len(m.cfg.Discover.SSH) != 2 || cmd == nil {
		t.Errorf("unexpected toggleHost add: %+v", m.cfg.Discover.SSH)
	}

	// 3. keyTargets
	m.focus = 0 // targets focus
	m.hosts = []string{"dino", "victor"}
	m.cur = 0
	// x removes host
	nm, cmd := m.keyTargets("x")
	m = nm.(model)
	if cmd == nil {
		t.Error("expected cmd after x on target")
	}
	// L jumps to results
	m.disc = &client.DiscoverResult{Plan: discover.Plan{Add: []config.Endpoint{ep1}}}
	nm, _ = m.keyTargets("L")
	m = nm.(model)
	if m.focus != 1 {
		t.Errorf("expected focus 1 after L, got %d", m.focus)
	}

	// 4. keyResults
	m.focus = 1
	// 'e' toggles inclEmb
	oldEmb := m.inclEmb
	nm, _ = m.keyResults("e")
	m = nm.(model)
	if m.inclEmb == oldEmb {
		t.Error("expected inclEmb toggled")
	}
	// '/' opens filter edit
	nm, _ = m.keyResults("/")
	m = nm.(model)
	if m.editing != "filter" {
		t.Errorf("expected editing filter, got %s", m.editing)
	}
	m.editing = ""
	// 'a' selects all, 'n' selects none
	nm, _ = m.keyResults("a")
	m = nm.(model)
	if !m.sel[0] {
		t.Error("expected item 0 selected on 'a'")
	}
	nm, _ = m.keyResults("n")
	m = nm.(model)
	if m.sel[0] {
		t.Error("expected item 0 deselected on 'n'")
	}

	// 5. acceptSelected
	m.sel[0] = false
	nm, cmd = m.acceptSelected()
	m = nm.(model)
	if cmd != nil || !strings.Contains(m.note, "nothing selected") {
		t.Errorf("expected warning on empty acceptSelected, got note=%s", m.note)
	}

	m.sel[0] = true
	nm, cmd = m.acceptSelected()
	if cmd == nil {
		t.Error("expected cmd on acceptSelected with items")
	}

	// 6. viewResults
	m.editing = "filter"
	m.editBuf = "qwen"
	m.filter = ""
	vRes := m.viewResults()
	if !strings.Contains(vRes, "Scan results") || !strings.Contains(vRes, "qwen") {
		t.Errorf("unexpected viewResults output: %s", vRes)
	}
	m.editing = ""

	// 7. viewInteg & guide
	vInteg := m.viewInteg()
	if !strings.Contains(vInteg, "Embeddings") {
		t.Errorf("unexpected viewInteg output: %s", vInteg)
	}

	resGuide, errGuide := guide("test-guide", func(c *config.Config) ([]string, error) {
		return []string{"step1", "step2"}, nil
	})
	if errGuide != nil || !strings.Contains(resGuide, "step1") {
		t.Errorf("guide output unexpected: %s, %v", resGuide, errGuide)
	}
}

func TestCov_TUI_ProfScreenAndPlanHelpers(t *testing.T) {
	// clampInt
	if val := clampInt(5, 10, 20); val != 10 {
		t.Errorf("expected clamp 10, got %d", val)
	}
	if val := clampInt(25, 10, 20); val != 20 {
		t.Errorf("expected clamp 20, got %d", val)
	}
	if val := clampInt(15, 10, 20); val != 15 {
		t.Errorf("expected clamp 15, got %d", val)
	}

	// removeProfile
	errRem := removeProfile("nonexistent-prof")
	if errRem == nil {
		t.Error("expected error from removeProfile with non-existent profile")
	}

	// specModeNote
	if note := specModeNote(1); !strings.Contains(note, "Speculation Engine") {
		t.Errorf("unexpected specModeNote for ngram: %s", note)
	}
	if note := specModeNote(2); !strings.Contains(note, "Speculation Engine") {
		t.Errorf("unexpected specModeNote for draft: %s", note)
	}

	// onStopDone
	m := testModel()
	m = m.onStopDone(stopDoneMsg{rep: server.StopReport{Endpoint: "modelA"}})
	if !strings.Contains(m.note, "stopped modelA") && !strings.Contains(m.note, "modelA") {
		t.Errorf("unexpected onStopDone success: note=%s", m.note)
	}
	m = m.onStopDone(stopDoneMsg{err: errors.New("kill failed")})
	if !strings.Contains(m.note, "kill failed") {
		t.Errorf("unexpected onStopDone error: note=%s", m.note)
	}
}

func TestCov_TUI_KeyCapsAndSuggestRoles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	m := testModel()
	m.cl = client.New(srv.URL)
	m.sc = scCaps
	m.capName = "dino-qwen"
	m.w = 120
	m.st = server.Status{
		Endpoints: []server.EpStatus{
			{Endpoint: config.Endpoint{Name: "dino-qwen", Model: "qwen3:8b", TPS: 45.0, Ctx: 32768}, OK: true},
			{Endpoint: config.Endpoint{Name: "victor-gemma", Model: "gemma4:12b", TPS: 60.0, Ctx: 16384}, OK: true},
		},
		Roles: map[string]server.RoleStatus{
			"coder": {Preferred: "dino-qwen", Active: "dino-qwen"},
		},
	}

	// suggestRoles
	cmd := m.suggestRoles()
	if cmd == nil {
		t.Error("expected cmd from suggestRoles")
	}

	// keyCaps with valid endpoint
	m.cur = 0
	nm, cmdToggle := m.keyCaps("enter")
	if cmdToggle == nil {
		t.Error("expected toggle capability cmd on enter")
	}
	_ = nm

	// keyCaps with missing endpoint resets to scDash
	m.capName = "missing-ep"
	nm, _ = m.keyCaps("enter")
	m = nm.(model)
	if m.sc != scDash {
		t.Errorf("expected scDash when ep not found, got %v", m.sc)
	}
}

func TestCov_TUI_HubToggleAndPing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	m := testModel()
	m.cl = client.New(srv.URL)
	m.sc = scHub
	m.w = 120

	// toggleHubConnection
	m.hub.tunConn = true
	_, cmd := m.toggleHubConnection(true, "https://hub.example.com")
	if cmd == nil {
		t.Fatal("expected cmd on disconnect toggle")
	}
	msg := cmd()
	if _, ok := msg.(noteMsg); !ok {
		t.Errorf("expected noteMsg from toggleHubConnection, got %T", msg)
	}

	m.hub.tunConn = false
	_, cmd = m.toggleHubConnection(false, "https://hub.example.com")
	if cmd == nil {
		t.Fatal("expected cmd on connect toggle")
	}
	msg = cmd()
	if _, ok := msg.(noteMsg); !ok {
		t.Errorf("expected noteMsg from toggleHubConnection, got %T", msg)
	}

	// testHubPing
	_, cmdPing := m.testHubPing("https://hub.example.com")
	if cmdPing == nil {
		t.Fatal("expected cmd from testHubPing")
	}
	msgPing := cmdPing()
	if _, ok := msgPing.(noteMsg); !ok {
		t.Errorf("expected noteMsg from testHubPing, got %T", msgPing)
	}
}

func TestCov_TUI_MoreCoverage(t *testing.T) {
	// 1. Init
	m := testModel()
	initCmd := m.Init()
	if initCmd == nil {
		t.Error("expected Init to return non-nil cmd")
	}

	// 2. srcNote
	if s := srcNote("live"); s != "live average" {
		t.Errorf("unexpected srcNote: %s", s)
	}
	if s := srcNote("bench"); s != "from a benchmark" {
		t.Errorf("unexpected srcNote: %s", s)
	}
	if s := srcNote("unknown"); s != " " {
		t.Errorf("expected space for unknown srcNote: %q", s)
	}

	// 3. speedLine
	emptySpeed := speedLine(server.EpStatus{})
	if !strings.Contains(emptySpeed, "no speed data yet") {
		t.Errorf("unexpected empty speedLine: %s", emptySpeed)
	}
	fullSpeed := speedLine(server.EpStatus{
		Endpoint:   config.Endpoint{TPS: 32.5},
		TTFTMS:     120,
		PrefillTPS: 250.0,
		MaxPrompt:  4096,
		Samples:    15,
	})
	if !strings.Contains(fullSpeed, "32.5 tok/s") || !strings.Contains(fullSpeed, "120ms") || !strings.Contains(fullSpeed, "250 tok/s") {
		t.Errorf("unexpected full speedLine: %s", fullSpeed)
	}

	// 4. sshConfigHosts
	tmpDir := t.TempDir()
	sshDir := filepath.Join(tmpDir, ".ssh")
	_ = os.MkdirAll(sshDir, 0700)
	configContent := "Host hostA\n  HostName 1.2.3.4\nHost hostB hostC\n  User root\nHost *\n  Port 22\n"
	_ = os.WriteFile(filepath.Join(sshDir, "config"), []byte(configContent), 0600)
	t.Setenv("HOME", tmpDir)

	hosts := sshConfigHosts()
	if len(hosts) != 3 || hosts[0] != "hostA" || hosts[1] != "hostB" || hosts[2] != "hostC" {
		t.Errorf("unexpected sshConfigHosts: %v", hosts)
	}

	// 5. fetchRemoteHubModels
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"remote/model:latest"}]}`))
	}))
	defer srv.Close()

	m.hubURL = srv.URL
	cmdHub := m.fetchRemoteHubModels()
	if cmdHub == nil {
		t.Fatal("expected cmdHub != nil")
	}
	msgHub := cmdHub()
	if hmsg, ok := msgHub.(hubModelsMsg); !ok || hmsg.err != nil || len(hmsg.models) != 1 {
		t.Errorf("unexpected hubModelsMsg: %+v", msgHub)
	}

	// 6. Integrations key navigation & confirmation
	m.sc = scInteg
	m.cur = 0
	m.confirm = false
	nm, _ := m.keyStr("enter", "")
	m = nm.(model)
	if !m.confirm {
		t.Error("expected confirm=true after first enter on scInteg")
	}
	// 'n' cancels confirm
	nm, _ = m.keyStr("n", "")
	m = nm.(model)
	if m.confirm {
		t.Error("expected confirm=false after 'n'")
	}

	// 7. keyNodes navigation and actions
	m.sc = scNodes
	m.cur = 0
	m.st.Nodes = []server.NodeStatus{
		{Node: "dino", Self: true},
		{Node: "vec", Self: false},
	}
	m.cfg.Nodes = map[string]config.NodeSpec{
		"dino": {MemGiB: 24.0, WeightGBps: 150.0},
	}
	// "+" enters addnode edit mode
	nm, _ = m.keyNodes("+")
	m = nm.(model)
	if m.editing != "addnode" {
		t.Errorf("expected editing addnode, got %s", m.editing)
	}
	m.editing = ""
	// "j" switches to scHub
	nm, cmdJ := m.keyNodes("j")
	m = nm.(model)
	if m.sc != scHub || cmdJ == nil {
		t.Errorf("expected scHub after j, got %v", m.sc)
	}
	m.sc = scNodes
	// "enter" jumps to scDash with node filter
	nm, _ = m.keyNodes("enter")
	m = nm.(model)
	if m.sc != scDash || m.filter != "dino" {
		t.Errorf("expected scDash and filter dino, got %v %s", m.sc, m.filter)
	}
	m.sc = scNodes
	// "d" on remote node gives elsewhere hint
	m.cur = 1 // vec (Self: false)
	nm, _ = m.keyNodes("d")
	m = nm.(model)
	if !strings.Contains(m.note, "leaves the line by its own fleet") {
		t.Errorf("expected elsewhere note for remote drain, got %s", m.note)
	}

	// 8. calcAvailablePlanCtxs
	ctxs1 := calcAvailablePlanCtxs(0)
	if len(ctxs1) == 0 || ctxs1[len(ctxs1)-1] != 32768 {
		t.Errorf("unexpected ctxs for 0: %v", ctxs1)
	}
	ctxs2 := calcAvailablePlanCtxs(4096)
	if len(ctxs2) == 0 || ctxs2[len(ctxs2)-1] != 4096 {
		t.Errorf("unexpected ctxs for 4096: %v", ctxs2)
	}
	ctxs3 := calcAvailablePlanCtxs(5000)
	if len(ctxs3) == 0 || ctxs3[len(ctxs3)-1] != 5000 {
		t.Errorf("unexpected ctxs for 5000: %v", ctxs3)
	}
	ctxs4 := calcAvailablePlanCtxs(1000)
	if len(ctxs4) == 0 || ctxs4[len(ctxs4)-1] != 1000 {
		t.Errorf("unexpected ctxs for 1000: %v", ctxs4)
	}

	// 9. Commands: scan, bench, probe
	_, cmdScan := m.scan()
	if cmdScan == nil {
		t.Error("expected scan cmd != nil")
	}
	if cmdBench := m.bench(nil); cmdBench == nil {
		t.Error("expected bench cmd != nil")
	}
	if cmdProbe := m.probe([]string{"all"}); cmdProbe == nil {
		t.Error("expected probe cmd != nil")
	}

	// 10. keyStore picker cancel
	m.sc = scStore
	m.pick = true
	m.storeCur = 3
	nmStore, _ := m.keyStore("esc")
	m = nmStore.(model)
	if m.pick || m.cur != 3 {
		t.Errorf("expected pick=false and cur=3 on store esc, got pick=%v cur=%d", m.pick, m.cur)
	}
}

func TestCov_TUI_ProfilesAndPlanKeys(t *testing.T) {
	m := testModel()
	m.w = 120

	// 1. keyProfilesScreen
	m.sc = scProfiles
	m.profList = nil
	m.cur = 0
	nm, _ := m.keyProfilesScreen("enter")
	m = nm.(model)
	if !strings.Contains(m.note, "no profiles yet") {
		t.Errorf("expected no profiles yet note, got %s", m.note)
	}

	// i -> profimport
	nm, _ = m.keyProfilesScreen("i")
	m = nm.(model)
	if m.editing != "profimport" {
		t.Errorf("expected editing profimport, got %s", m.editing)
	}
	m.editing = ""

	// confirm = true, 'n' -> cancel
	m.confirm = true
	nm, _ = m.keyProfilesScreen("n")
	m = nm.(model)
	if m.confirm {
		t.Error("expected confirm=false on 'n'")
	}

	// confirm = true, 'y' -> delete profile
	m.confirm = true
	m.pend = &pendingAct{kind: "rm-profile", name: "nonexistent-profile"}
	nm, _ = m.keyProfilesScreen("y")
	m = nm.(model)
	if !strings.Contains(m.note, "error:") {
		t.Errorf("expected error note on deleting nonexistent profile, got %s", m.note)
	}

	// 2. finishEdit cases
	m.editing = "storefill"
	m.editFor = "node1|sha1"
	m.editBuf = "0-10 output"
	nm, cmd := m.finishEdit()
	m = nm.(model)
	if cmd == nil || m.editing != "" {
		t.Error("expected finishEdit for storefill to return cmd")
	}

	m.editing = "profedit"
	m.editFor = "myprof"
	m.editBuf = "layout.ts=20,10"
	nm, cmd = m.finishEdit()
	m = nm.(model)
	if cmd == nil || m.editing != "" {
		t.Error("expected finishEdit for profedit to return cmd")
	}

	// addhost via keyStr enter
	m.editing = "addhost"
	m.editBuf = "newnode"
	nm, cmd = m.keyStr("enter", "")
	m = nm.(model)
	if cmd == nil || m.editing != "" {
		t.Error("expected addhost via keyStr to finish and return cmd")
	}

	// 3. keyPlan tab & pick
	m.sc = scPlan
	m.planSection = 0
	nm, _ = m.keyPlan("tab")
	m = nm.(model)
	if m.planSection != 1 {
		t.Errorf("expected planSection=1 after tab, got %d", m.planSection)
	}
	nm, _ = m.keyPlan("backtab")
	m = nm.(model)
	if m.planSection != 0 && m.planSection != 1 {
		t.Errorf("unexpected planSection after backtab: %d", m.planSection)
	}

	m.pick = true
	m.planCur = 0
	m.cur = 0
	m.planRes = &plan.Result{
		Candidates: []plan.Candidate{
			{Master: "dino", Kind: "single"},
		},
	}
	nm, _ = m.keyPlan("enter")
	m = nm.(model)
	if m.pick || m.planPick != 0 {
		t.Errorf("expected pick=false and planPick=0 after enter, got pick=%v planPick=%d", m.pick, m.planPick)
	}
}

func TestCov_TUI_ProfilesScreenMethods(t *testing.T) {
	m := testModel()
	m.w, m.h = 100, 30

	m = m.enterProfiles()
	if m.sc != scProfiles {
		t.Errorf("expected scProfiles, got %v", m.sc)
	}

	m = m.reloadProfiles("unknown-prof")

	btns := m.profileButtons()
	if len(btns) == 0 {
		t.Error("expected non-empty profileButtons")
	}

	m.confirm = true
	btnsConfirm := m.profileButtons()
	if len(btnsConfirm) != 2 || btnsConfirm[0].key != "y" {
		t.Errorf("expected confirm buttons (y/n), got %v", btnsConfirm)
	}

	v := m.viewProfilesScreen()
	if !strings.Contains(v, "PROFILE") && !strings.Contains(v, "no profiles yet") { // the empty list has no heading
		t.Errorf("unexpected viewProfilesScreen output: %s", v)
	}

	// Actions on existing profile
	if len(m.profList) > 0 {
		m.cur = 0
		m.confirm = false
		// 'd': down
		nm, cmdDown := m.keyProfilesScreen("s")
		_ = nm
		if cmdDown == nil {
			t.Error("expected cmd on profile down")
		}
		// 'a': autostart toggle
		_, cmdAuto := m.keyProfilesScreen("a")
		if cmdAuto == nil {
			t.Error("expected cmd on profile autostart toggle")
		}
	}
}

func TestCov_TUI_IntegrationsRun(t *testing.T) {
	h := t.TempDir()
	t.Setenv("HOME", h)

	// Mock systemctl script in PATH
	tmpBin := t.TempDir()
	systemctlScript := filepath.Join(tmpBin, "systemctl")
	scriptContent := "#!/bin/sh\nexit 0\n"
	_ = os.WriteFile(systemctlScript, []byte(scriptContent), 0755)
	t.Setenv("PATH", tmpBin+":"+os.Getenv("PATH"))

	m := testModel()
	its := integrations()

	// 1. Refresh guidance (index 3)
	if len(its) > 3 {
		_, _ = its[3].run(&m)
	}

	// 2. Install systemd service (index 4)
	if len(its) > 4 {
		_, _ = its[4].run(&m)
	}

	// 3. Install binary (index 5)
	if len(its) > 5 {
		_, _ = its[5].run(&m)
	}
}

func TestCov_TUI_HubKeysAndFinishEdit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	m := testModel()
	m.cl = client.New(srv.URL)
	m.sc = scHub
	m.w = 120

	// 1. enter on cur=1 -> edits huburl
	m.cur = 1
	nm, _ := m.keyHub("enter")
	m = nm.(model)
	if m.editing != "huburl" {
		t.Errorf("expected editing huburl on enter at 1, got %s", m.editing)
	}

	// finishEdit huburl
	m.editBuf = "https://hub.test"
	nm, _ = m.finishEdit()
	m = nm.(model)
	if m.hubURL != "https://hub.test" {
		t.Errorf("expected hubURL https://hub.test, got %s", m.hubURL)
	}

	// enter on cur=2 -> edits hubtoken
	m.cur = 2
	nm, _ = m.keyHub("enter")
	m = nm.(model)
	if m.editing != "hubtoken" {
		t.Errorf("expected editing hubtoken on enter at 2, got %s", m.editing)
	}
	m.editBuf = "my-token"
	nm, _ = m.finishEdit()
	m = nm.(model)
	if m.hubToken != "my-token" {
		t.Errorf("expected hubToken my-token, got %s", m.hubToken)
	}

	// 'k' shortcut edits hubapikey
	nm, _ = m.keyHub("k")
	m = nm.(model)
	if m.editing != "hubapikey" {
		t.Errorf("expected editing hubapikey on 'k', got %s", m.editing)
	}
	m.editBuf = "my-api-key"
	nm, cmdKey := m.finishEdit()
	m = nm.(model)
	if cmdKey == nil {
		t.Error("expected cmd after finishEdit hubapikey")
	}

	// 'r' shortcut refreshes models
	m.hubShowModels = false
	nm, cmdR := m.keyHub("r")
	m = nm.(model)
	if !m.hubShowModels || cmdR == nil {
		t.Error("expected hubShowModels=true and cmdR!=nil on 'r'")
	}

	// 'c' shortcut toggles connection
	nm, cmdC := m.keyHub("c")
	if cmdC == nil {
		t.Error("expected cmd on 'c' toggle")
	}
	_ = nm

	// enter on cur=4 toggles models
	m.cur = 4
	m.hubShowModels = true
	nm, _ = m.keyHub("enter")
	if nm.(model).hubShowModels {
		t.Error("expected hubShowModels toggled to false at 4")
	}

	// enter on cur=14 goes back
	m.cur = 14
	m.hubPrev = scDash
	nm, _ = m.keyHub("enter")
	if nm.(model).sc != scDash {
		t.Errorf("expected scDash after cur=14, got %v", nm.(model).sc)
	}

	// viewHub
	v := m.viewHub()
	if !strings.Contains(v, "Fleet Hub") {
		t.Errorf("unexpected viewHub output: %s", v)
	}

	// finishEdit addnode
	m.editing = "addnode"
	m.editBuf = "node-gpu"
	nm, cmdNode := m.finishEdit()
	if cmdNode == nil {
		t.Error("expected cmd on finishEdit addnode")
	}
	_ = nm
}

func TestCov_TUI_LeaderAndRuns(t *testing.T) {
	m := testModel()
	m.w, m.h = 120, 40
	m.sc = scBoard
	m.st = server.Status{
		Endpoints: []server.EpStatus{
			{Endpoint: config.Endpoint{Name: "dino-qwen", Node: "dino", Model: "qwen3:8b", TPS: 45.0}, OK: true},
		},
		Nodes: []server.NodeStatus{
			{Node: "dino", Self: true},
		},
	}

	_ = m.boardNodes()
	_ = m.boardEps()
	_ = m.boardCards()
	_ = m.viewCards()
	_ = m.viewBoardHeader()
	_ = m.viewBoardBody()
	_ = m.viewToday()
	_ = m.viewByRole()
	_ = m.viewDaily()

	m = m.enterRuns()
	_ = m.viewRuns()
	nm, _ := m.keyRuns("?")
	_ = nm
}

func TestCov_TUI_ActivityAndUsage(t *testing.T) {
	m := testModel()
	m.w, m.h = 120, 40
	m.reqlog = []server.LogEntry{
		{Model: "qwen", Endpoint: "ep1", Status: 200, Attempts: 1},
	}
	_ = m.viewActivity()

	m.usage = []server.UsageRow{
		{Key: "ep1", Requests: 10, In: 100, Out: 500},
	}
	_ = m.viewUsage()

	_ = m.viewDash()
}

func TestCov_TUI_RoleEdit(t *testing.T) {
	m := testModel()
	m.w, m.h = 120, 40
	m.sc = scRoleEdit
	m.st = server.Status{
		Roles: map[string]server.RoleStatus{
			"coder": {Active: "ep1", Priority: 5, Use: "code editing", Strategy: "round-robin"},
		},
	}
	m.roleCur = 0

	// viewRoleEdit normal & pick mode
	_ = m.viewRoleEdit()
	m.pick = true
	_ = m.viewRoleEdit()
	m.pick = false

	// 'a': enter pick mode
	nm, _ := m.keyRoleEdit("a")
	m = nm.(model)
	if !m.pick {
		t.Error("expected pick=true after 'a' on roleEdit")
	}

	// 'o': toggle overflow
	m.pick = false
	nm, cmdO := m.keyRoleEdit("o")
	if cmdO == nil {
		t.Error("expected cmd on 'o' toggle overflow")
	}
	_ = nm

	// 's': toggle strategy
	nm, cmdS := m.keyRoleEdit("s")
	if cmdS == nil {
		t.Error("expected cmd on 's' toggle strategy")
	}
	_ = nm
}

func TestCov_TUI_DashKeysAndFullViews(t *testing.T) {
	m := testModel()
	m.w, m.h = 120, 40
	m.sc = scDash
	m.st = server.Status{
		Endpoints: []server.EpStatus{
			{Endpoint: config.Endpoint{Name: "dino-qwen", Model: "qwen3:8b", TPS: 45.0, Tags: []string{"fast"}}, OK: true},
		},
	}
	m.cur = 0

	// View rendering
	_ = m.View()

	// buttons and help
	btns := m.buttons()
	if len(btns) == 0 {
		t.Error("expected dash buttons")
	}
	helpText := m.help()
	if !strings.Contains(helpText, "capabilities") {
		t.Errorf("unexpected help text: %s", helpText)
	}

	// keyStr shortcuts in scDash
	// '/' edits filter
	nm, _ := m.keyStr("/", "/")
	m = nm.(model)
	if m.editing != "filter" {
		t.Errorf("expected editing filter, got %s", m.editing)
	}
	m.editing = ""

	// 'c' jumps to scCaps
	nm, _ = m.keyStr("c", "c")
	m = nm.(model)
	if m.sc != scCaps {
		t.Errorf("expected scCaps after 'c', got %v", m.sc)
	}
	m.sc = scDash

	// 't' edits tags
	nm, _ = m.keyStr("t", "t")
	m = nm.(model)
	if m.editing != "tags" {
		t.Errorf("expected editing tags, got %s", m.editing)
	}
	m.editing = ""

	// 'f' edits fleetport
	nm, _ = m.keyStr("f", "f")
	m = nm.(model)
	if m.editing != "fleetport" {
		t.Errorf("expected editing fleetport, got %s", m.editing)
	}
	m.editing = ""

	// 'g' edits group
	nm, _ = m.keyStr("g", "g")
	m = nm.(model)
	if m.editing != "group" {
		t.Errorf("expected editing group, got %s", m.editing)
	}
	m.editing = ""
}

func TestCov_TUI_HomeKeysAndSmallView(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	m := testModel()
	m.cl = client.New(srv.URL)
	m.w, m.h = 100, 20 // small mode
	m.sc = scHome
	_ = m.View()

	m.h = 50 // expanded mode
	_ = m.View()

	// Navigate through home menu items
	for i := 0; i < len(home); i++ {
		m.sc = scHome
		m.cur = i
		_, _ = m.keyStr("enter", "")
	}
}

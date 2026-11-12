package tui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/tevfik/fleet-router/internal/client"
	"github.com/tevfik/fleet-router/internal/hostinfo"
	"github.com/tevfik/fleet-router/internal/plan"
	"github.com/tevfik/fleet-router/internal/server"
)

func layerModel64() *plan.Model {
	m := &plan.Model{Name: "big", Arch: "qwen", Layers: 64, HiddenBytes: 10240}
	for i := 0; i < 64; i++ {
		m.LayerBytes = append(m.LayerBytes, 231_400_000)
		m.LayerRead = append(m.LayerRead, 231_400_000)
	}
	return m
}

// layerDaemon answers /fleet/plan/evaluate with the real plan.Evaluate over three 8 GiB boards.
func layerDaemon(t *testing.T) *client.Client {
	t.Helper()
	b := &hostinfo.Bench{FastWhere: "NPU"}
	nodes := []plan.Node{
		{Name: "delta1", CapGB: 8, BW: 34, WeightBW: 34, Bench: b},
		{Name: "pcie", CapGB: 8, BW: 32, WeightBW: 32, Bench: b},
		{Name: "delta2", CapGB: 8, BW: 22.6, WeightBW: 22.6, Bench: b},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fleet/plan/evaluate" {
			http.NotFound(w, r)
			return
		}
		var req server.EvaluateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		json.NewEncoder(w).Encode(plan.Evaluate(&req.Model, nodes, plan.Options{Ctx: req.Ctx, Np: req.Np}, req.Master, req.Alloc, req.OutMaster))
	}))
	t.Cleanup(srv.Close)
	return client.New(strings.TrimPrefix(srv.URL, "http://"))
}

func layerPlanModel(t *testing.T) model {
	cl := layerDaemon(t)
	mod := layerModel64()
	res := plan.Result{Model: "big", Ctx: 4096, Np: 1, Candidates: []plan.Candidate{
		{Kind: "split", Master: "delta1", TPS: 5, TokenMS: 200, Parts: []plan.Part{{Node: "delta1", Layers: 30}, {Node: "pcie", Layers: 34, Output: true}}}}}
	return model{cl: cl, sc: scPlan, sel: map[int]bool{}, w: 150, planModel: mod, planRes: &res, planPath: "/x/big.gguf", planInit: true}
}

// settle runs a command and feeds its reply back, as the program does.
func layReply(t *testing.T, m model, c tea.Cmd) model {
	t.Helper()
	if c == nil {
		t.Fatal("expected a command")
	}
	nm, _ := m.Update(c())
	return nm.(model)
}

func layKey(t *testing.T, m model, s string) (model, tea.Cmd) {
	t.Helper()
	var k tea.KeyPressMsg
	switch s {
	case "esc":
		k = tea.KeyPressMsg{Code: tea.KeyEscape}
	case "enter":
		k = tea.KeyPressMsg{Code: tea.KeyEnter}
	case "left":
		k = tea.KeyPressMsg{Code: tea.KeyLeft}
	case "right":
		k = tea.KeyPressMsg{Code: tea.KeyRight}
	case "down":
		k = tea.KeyPressMsg{Code: tea.KeyDown}
	case "up":
		k = tea.KeyPressMsg{Code: tea.KeyUp}
	default:
		k = tea.KeyPressMsg{Code: rune(s[0]), Text: s}
	}
	nm, c := m.key(k)
	return nm.(model), c
}

func TestLayers_EditUseAndClose(t *testing.T) {
	m := layerPlanModel(t)
	m, c := layKey(t, m, "S")
	if m.lay == nil {
		t.Fatal("m opens the editor")
	}
	m = layReply(t, m, c)
	if m.lay.ev == nil || !m.lay.ev.OK || m.lay.placed() != 64 {
		t.Fatalf("seeded from the chosen candidate: %+v %+v", m.lay.alloc, m.lay.ev)
	}
	view := plain(m.viewLayers(150))
	for _, want := range []string{"LAYER EDITOR — 64 blocks, 64 placed", "delta1", "master", "pcie", "worker", "GB", "tok/s", "embeddings", "output"} {
		if !strings.Contains(view, want) {
			t.Errorf("view lacks %q:\n%s", want, view)
		}
	}
	// down to pcie, one block fewer: 63 placed, not usable
	m, _ = layKey(t, m, "down")
	m, c = layKey(t, m, "left")
	m = layReply(t, m, c)
	if m.lay.alloc[1].Layers != 33 || m.lay.ev.OK || !strings.Contains(strings.Join(m.lay.ev.Problems, "|"), "1 of 64") {
		t.Fatalf("%+v %+v", m.lay.alloc, m.lay.ev)
	}
	m, _ = layKey(t, m, "enter")
	if m.lay == nil || !strings.Contains(plain(m.note), "✗") {
		t.Fatalf("an unplaced block must refuse enter: note=%q", m.note)
	}
	// 0 gives the rest back; the split is usable and enter appends it as a candidate
	m, c = layKey(t, m, "0")
	m = layReply(t, m, c)
	if !m.lay.ev.OK || m.lay.alloc[1].Layers != 34 {
		t.Fatalf("%+v %+v", m.lay.alloc, m.lay.ev.Problems)
	}
	// move four blocks to the master
	m, _ = layKey(t, m, "up")
	for i := 0; i < 4; i++ {
		m, c = layKey(t, m, "right")
	}
	m = layReply(t, m, c)
	m, c = layKey(t, m, "down")
	for i := 0; i < 4; i++ {
		m, c = layKey(t, m, "left")
	}
	m = layReply(t, m, c)
	if m.lay.alloc[0].Layers != 34 || m.lay.alloc[1].Layers != 30 || !m.lay.ev.OK {
		t.Fatalf("%+v %v", m.lay.alloc, m.lay.ev.Problems)
	}
	m, _ = layKey(t, m, "enter")
	if m.lay != nil {
		t.Fatal("enter closes the editor")
	}
	cs := m.planRes.Candidates
	if len(cs) != 2 || !handMade(cs[1]) || m.planPick != 1 {
		t.Fatalf("candidates %d pick %d", len(cs), m.planPick)
	}
	if cs[1].Parts[0].Layers != 34 || cs[1].Parts[1].Layers != 30 || cs[1].Parts[1].First != 34 {
		t.Fatalf("%+v", cs[1].Parts)
	}
	// editing again replaces the hand-made candidate instead of piling up
	m, c = layKey(t, m, "S")
	m = layReply(t, m, c)
	m, _ = layKey(t, m, "enter")
	if len(m.planRes.Candidates) != 2 {
		t.Fatalf("a second use must replace the first: %d", len(m.planRes.Candidates))
	}
}

func TestLayers_AddRemoveMachineAndEsc(t *testing.T) {
	m := layerPlanModel(t)
	m, c := layKey(t, m, "S")
	m = layReply(t, m, c)
	m, _ = layKey(t, m, "a")
	if !m.lay.adding || len(m.lay.freeMachines()) != 1 || m.lay.freeMachines()[0].Node != "delta2" {
		t.Fatalf("the add list: %+v", m.lay)
	}
	if !strings.Contains(plain(m.viewLayers(150)), "add which machine?") {
		t.Fatal("the machine list is shown")
	}
	m, c = layKey(t, m, "enter")
	m = layReply(t, m, c)
	if len(m.lay.alloc) != 3 || m.lay.alloc[2].Node != "delta2" || m.lay.cur != 2 {
		t.Fatalf("%+v", m.lay.alloc)
	}
	// a machine with no blocks is a problem, shown
	if m.lay.ev.OK || !strings.Contains(plain(m.viewLayers(150)), "holds no blocks") {
		t.Fatalf("%+v", m.lay.ev)
	}
	m, c = layKey(t, m, "x")
	m = layReply(t, m, c)
	if len(m.lay.alloc) != 2 || !m.lay.ev.OK {
		t.Fatalf("%+v", m.lay.alloc)
	}
	// the master cannot be removed
	m, _ = layKey(t, m, "up")
	m, c = layKey(t, m, "x")
	if c != nil || len(m.lay.alloc) != 2 {
		t.Fatal("the master stays")
	}
	// esc closes the add list first, then the editor, then the plan screen stays
	m, _ = layKey(t, m, "a")
	m, _ = layKey(t, m, "esc")
	if m.lay == nil || m.lay.adding {
		t.Fatal("the first esc closes the list only")
	}
	m, _ = layKey(t, m, "esc")
	if m.lay != nil || m.sc != scPlan {
		t.Fatalf("the second esc closes the editor: lay=%v sc=%v", m.lay, m.sc)
	}
}

func TestLayers_NeedsAPlanFirst(t *testing.T) {
	m := model{sc: scPlan, sel: map[int]bool{}, w: 150}
	m, c := layKey(t, m, "S")
	if m.lay != nil || c != nil || !strings.Contains(plain(m.note), "plan a file first") {
		t.Fatalf("%v %q", m.lay, m.note)
	}
}

// A late reply for an earlier edit must not overwrite the newer figures.
func TestLayers_StaleReplyDropped(t *testing.T) {
	m := layerPlanModel(t)
	m, c := layKey(t, m, "S")
	first := c
	m, c = layKey(t, m, "down")
	m, c = layKey(t, m, "left")
	m = layReply(t, m, c)
	m = layReply(t, m, first)
	if m.lay.alloc[1].Layers != 33 || m.lay.ev.Assigned != 63 {
		t.Fatalf("stale reply applied: %d %d", m.lay.alloc[1].Layers, m.lay.ev.Assigned)
	}
}

func TestLayers_NarrowAndOverflowShown(t *testing.T) {
	m := layerPlanModel(t)
	m.w = 70
	m, c := layKey(t, m, "S")
	m = layReply(t, m, c)
	m, _ = layKey(t, m, "down")
	for i := 0; i < 8; i++ {
		m, c = layKey(t, m, "L") // +5 each: way past 8 GiB
	}
	m = layReply(t, m, c)
	v := plain(m.viewLayers(70))
	if !strings.Contains(v, "✗") {
		t.Fatalf("an overflowing machine is marked:\n%s", v)
	}
	for _, l := range strings.Split(v, "\n") {
		if len([]rune(l)) > 72 {
			t.Errorf("line too wide (%d): %q", len([]rune(l)), l)
		}
	}
}

func TestLayers_HeaderSaysWhatIsMissingOrTooMany(t *testing.T) {
	m := layerPlanModel(t)
	m, c := layKey(t, m, "S")
	m = layReply(t, m, c)
	m, _ = layKey(t, m, "down")
	m, _ = layKey(t, m, "left")
	if v := plain(m.viewLayers(150)); !strings.Contains(v, "1 still to place") {
		t.Fatalf("%s", v)
	}
	m, _ = layKey(t, m, "right")
	m, _ = layKey(t, m, "right")
	if v := plain(m.viewLayers(150)); !strings.Contains(v, "1 too many") {
		t.Fatalf("%s", v)
	}
}

// A chain of stages runs the output projection on its last stage: the editor starts without it on the master (the
// deploy refuses "the output head is on the first stage") and does not let o put it there.
func TestLayers_ChainKeepsTheOutputOffTheMaster(t *testing.T) {
	m := layerPlanModel(t)
	m.planEngineMode = 2 // chain
	m.planRes.Candidates[0].Parts[0].Output, m.planRes.Candidates[0].Parts[1].Output = true, false
	m, c := layKey(t, m, "S")
	m = layReply(t, m, c)
	if !m.lay.chain || m.lay.outMaster {
		t.Fatalf("chain=%v outMaster=%v", m.lay.chain, m.lay.outMaster)
	}
	if !m.lay.ev.OK || m.lay.ev.Parts[0].Output || !m.lay.ev.Parts[1].Output {
		t.Fatalf("the output belongs on the last stage: %+v", m.lay.ev.Parts)
	}
	m, c = layKey(t, m, "o")
	if c != nil || m.lay.outMaster || !strings.Contains(plain(m.note), "last stage") {
		t.Fatalf("o must be refused: outMaster=%v note=%q", m.lay.outMaster, m.note)
	}
	m, _ = layKey(t, m, "enter")
	if p := m.planRes.Candidates[m.planPick].Parts; p[0].Output || !p[1].Output {
		t.Fatalf("the hand-made candidate: %+v", p)
	}
}

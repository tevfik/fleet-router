package tui

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/plan"
	"github.com/tevfik/fleet-router/internal/server"
	"github.com/tevfik/fleet-router/internal/store"
)

// tinyGGUF writes a small valid GGUF file (a llama of 2 blocks).
func tinyGGUF(t *testing.T, path string) {
	t.Helper()
	var b bytes.Buffer
	u32 := func(v uint32) { binary.Write(&b, binary.LittleEndian, v) }
	u64 := func(v uint64) { binary.Write(&b, binary.LittleEndian, v) }
	str := func(s string) { u64(uint64(len(s))); b.WriteString(s) }
	b.WriteString("GGUF")
	u32(3)
	tensors := []string{"token_embd.weight", "blk.0.a.weight", "blk.1.a.weight", "output.weight"}
	u64(uint64(len(tensors)))
	kvs := []struct {
		k string
		s string
		n uint32
	}{{"general.architecture", "llama", 0}, {"llama.block_count", "", 2}, {"llama.embedding_length", "", 64}}
	u64(uint64(len(kvs)))
	for _, e := range kvs {
		str(e.k)
		if e.s != "" {
			u32(8)
			str(e.s)
		} else {
			u32(4)
			u32(e.n)
		}
	}
	for i, n := range tensors {
		str(n)
		u32(1)
		u64(8)
		u32(0)
		u64(uint64(i) * 1024)
	}
	for b.Len()%32 != 0 {
		b.WriteByte(0)
	}
	b.Write(make([]byte, 4*1024))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func planModelAt(t *testing.T) (model, string) {
	t.Helper()
	_, cl := newNodesDaemon(t) // isolates HOME as well
	home := os.Getenv("HOME")
	p := filepath.Join(home, "models", "tiny.gguf")
	tinyGGUF(t, p)
	m := model{cl: cl, sc: scHome, sel: map[int]bool{}, w: 120}
	for i, it := range home_() {
		if it.title == "Plan" {
			m.cur = i
		}
	}
	return m, p
}

func home_() []menuItem { return home }

func TestFeature_Plan_HomeOpensTheScreenAndListsFiles(t *testing.T) {
	m, p := planModelAt(t)
	nm, _ := m.keyStr("enter", "")
	m = nm.(model)
	if m.sc != scPlan {
		t.Fatalf("screen %v", m.sc)
	}
	if len(m.planFiles) != 1 || m.planFiles[0] != p || m.listLen() != 1 {
		t.Fatalf("files %v", m.planFiles)
	}
	v := m.viewPlan()
	for _, want := range []string{"GGUF files nearby", "tiny.gguf", "context", "8192", "slots", "any machine"} {
		if !strings.Contains(v, want) {
			t.Fatalf("missing %q in\n%s", want, v)
		}
	}
	if !strings.Contains(m.help(), "plan the highlighted file") {
		t.Fatal(m.help())
	}
	var keys []string
	for _, b := range m.buttons() {
		keys = append(keys, b.key)
	}
	if strings.Join(keys, ",") != "enter,c,e,p,S,r,f,w,a,L,t,b,i,d,esc" {
		t.Fatalf("buttons %v", keys)
	}
}

func TestFeature_Plan_EnterPlansTheFile(t *testing.T) {
	m, p := planModelAt(t)
	nm, _ := m.keyStr("enter", "")
	m = nm.(model)
	nm, cmd := m.keyStr("enter", "")
	m = nm.(model)
	if !m.planBusy || cmd == nil || m.planPath != p {
		t.Fatalf("busy %v path %q", m.planBusy, m.planPath)
	}
	if !strings.Contains(m.viewPlan(), "working out") {
		t.Fatal("no progress line")
	}
	msg := cmd()
	pm, ok := msg.(planMsg)
	if !ok || pm.err != nil {
		t.Fatalf("%T %+v", msg, msg)
	}
	nm, _ = m.Update(pm)
	m = nm.(model)
	if m.planBusy || m.planRes == nil || m.planModel == nil || m.planModel.Layers != 2 {
		t.Fatalf("%+v", m.planRes)
	}
	v := m.viewPlan()
	// no machine has a benchmark in a fresh test daemon: the answer says so
	if !strings.Contains(v, "no benchmark data yet") || !strings.Contains(v, "left out") || !strings.Contains(v, "← planned") {
		t.Fatalf("\n%s", v)
	}
}

func TestFeature_Plan_ParametersRerunWithTheKnownFacts(t *testing.T) {
	m, _ := planModelAt(t)
	m = m.enterPlan()
	m.st.Nodes = []server.NodeStatus{{Node: "a", Self: true}, {Node: "b"}}
	// nothing planned yet: changing a parameter only changes it
	nm, cmd := m.keyStr("tab", "") // the sliders take focus; ←/→ change the value
	m = nm.(model)
	nm, cmd = m.keyStr("right", "")
	m = nm.(model)
	if m.planCtx != 3 || cmd != nil { // 8192 → 16384
		t.Fatalf("ctx index %d cmd %v", m.planCtx, cmd)
	}
	nm, _ = m.keyStr("down", "")
	m = nm.(model)
	nm, _ = m.keyStr("right", "")
	m = nm.(model)
	if m.planNp != 1 {
		t.Fatalf("np %d", m.planNp)
	}
	m.planParamCur = 3 // master: any → a → b → any
	nm, _ = m.keyStr("right", "")
	m = nm.(model)
	if m.planMaster != "a" {
		t.Fatalf("master %q", m.planMaster)
	}
	nm, _ = m.keyStr("right", "")
	m = nm.(model)
	nm, _ = m.keyStr("right", "")
	m = nm.(model)
	if m.planMaster != "" {
		t.Fatalf("master cycles back to any: %q", m.planMaster)
	}
	// once a file is planned, a change reruns it
	m.planSection, m.planParamCur = 0, 0
	nm, cmd = m.keyStr("enter", "")
	m = nm.(model)
	pm := cmd().(planMsg)
	nm, _ = m.Update(pm)
	m = nm.(model)
	m.planSection, m.planParamCur = 1, 0
	nm, cmd = m.keyStr("right", "")
	m = nm.(model)
	if cmd == nil || !m.planBusy {
		t.Fatal("changing the context plans again")
	}
	pm = cmd().(planMsg)
	if pm.err != nil || pm.res.Ctx != planCtxs[m.planCtx] || pm.res.Np != 2 {
		t.Fatalf("%+v %v", pm.res, pm.err)
	}
}

func TestFeature_Plan_TypedPathAndErrors(t *testing.T) {
	m, p := planModelAt(t)
	m = m.enterPlan()
	nm, _ := m.keyStr("p", "")
	m = nm.(model)
	if m.editing != "planpath" || !strings.Contains(m.editPrompt(), "path of a GGUF file") {
		t.Fatalf("editing %q", m.editing)
	}
	for _, r := range "~/models/tiny.gguf" {
		nm, _ = m.keyStr(string(r), string(r))
		m = nm.(model)
	}
	nm, cmd := m.keyStr("enter", "")
	m = nm.(model)
	if m.editing != "" || cmd == nil || !m.planBusy {
		t.Fatalf("editing %q busy %v", m.editing, m.planBusy)
	}
	pm := cmd().(planMsg)
	if pm.err != nil || pm.path != p {
		t.Fatalf("~ is the home directory: %+v", pm)
	}
	// a file that is not a GGUF
	bad := filepath.Join(os.Getenv("HOME"), "notes.gguf")
	os.WriteFile(bad, []byte("hello"), 0o644)
	m.editing, m.editBuf = "planpath", bad
	nm, cmd = m.keyStr("enter", "")
	m = nm.(model)
	nm, _ = m.Update(cmd())
	m = nm.(model)
	if m.planErr == "" || !strings.Contains(m.viewPlan(), "not a GGUF file") {
		t.Fatalf("err %q\n%s", m.planErr, m.viewPlan())
	}
	// an empty path does nothing
	m.editing, m.editBuf = "planpath", "  "
	if _, cmd = m.keyStr("enter", ""); cmd != nil {
		t.Fatal("empty path")
	}
}

func TestFeature_Plan_NoFilesFound(t *testing.T) {
	_, cl := newNodesDaemon(t)
	m := model{cl: cl, sel: map[int]bool{}}.enterPlan()
	if len(m.planFiles) != 0 || !strings.Contains(m.viewPlan(), "no GGUF file found") {
		t.Fatalf("%v", m.planFiles)
	}
	nm, _ := m.keyStr("enter", "")
	if !strings.Contains(nm.(model).note, "no GGUF file") {
		t.Fatalf("%q", nm.(model).note)
	}
}

func TestFeature_Plan_ResultViewColoursTheVerdict(t *testing.T) {
	m := model{sc: scPlan, sel: map[int]bool{}, w: 100}
	r := plan.Result{Model: "big", Ctx: 8192, Np: 1, WeightsGB: 10, Verdict: "run it on dino alone (GPU): about 20.0 tok/s",
		Candidates: []plan.Candidate{{Kind: "single", Master: "dino", Where: "GPU", TPS: 20, TokenMS: 50, Parts: []plan.Part{{Node: "dino", Layers: 32}}}}}
	m.planRes = &r
	v := m.viewPlan()
	if !strings.Contains(v, "run it on dino alone") || !strings.Contains(v, "20.0") {
		t.Fatal(v)
	}
}

func TestFeature_Plan_SectionSwitching(t *testing.T) {
	_, cl := newNodesDaemon(t)
	m := model{cl: cl, sel: map[int]bool{}}.enterPlan()
	r := plan.Result{Model: "big", Ctx: 8192, Np: 1, WeightsGB: 10, Verdict: "run it on dino alone",
		Candidates: []plan.Candidate{{Kind: "single", Master: "dino", Where: "GPU", TPS: 20, TokenMS: 50, Parts: []plan.Part{{Node: "dino", Layers: 32}}}}}
	m.planRes = &r
	m.planPath = "/models/test.gguf"

	// 1. Tab moves to the parameter sliders
	nm, _ := m.keyStr("tab", "")
	m = nm.(model)
	if m.planSection != 1 {
		t.Fatalf("expected planSection 1, got %d", m.planSection)
	}
	if m.planParamCur != 0 {
		t.Fatalf("expected paramCur 0, got %d", m.planParamCur)
	}

	// 2. Adjust context slider using right arrow
	origCtx := m.planCtx
	nm, _ = m.keyStr("right", "")
	m = nm.(model)
	if m.planCtx == origCtx {
		t.Fatalf("expected context to change from %d", origCtx)
	}

	// 3. Move down to slots
	nm, _ = m.keyStr("down", "")
	m = nm.(model)
	if m.planParamCur != 1 {
		t.Fatalf("expected paramCur 1 (slots), got %d", m.planParamCur)
	}

	// 4. Move down to QoS mode
	nm, _ = m.keyStr("down", "")
	m = nm.(model)
	if m.planParamCur != 2 {
		t.Fatalf("expected paramCur 2 (QoS mode), got %d", m.planParamCur)
	}
	nm, _ = m.keyStr("right", "")
	m = nm.(model)
	if m.planStrat != 1 {
		t.Fatalf("expected QoS mode 1, got %d", m.planStrat)
	}

	// 5. Tab twice more reaches section 4 (Deploy)
	nm, _ = m.keyStr("tab", "")
	m = nm.(model)
	nm, _ = m.keyStr("tab", "")
	m = nm.(model)
	if m.planSection != 3 {
		t.Fatalf("expected planSection 3, got %d", m.planSection)
	}

	// 6. Esc takes back to section 0
	nm, _ = m.keyStr("esc", "")
	m = nm.(model)
	if m.planSection != 0 {
		t.Fatalf("expected planSection 0 after esc, got %d", m.planSection)
	}
}

func TestFeature_Plan_HardwareAndVisionSelectors(t *testing.T) {
	_, cl := newNodesDaemon(t)
	m := model{cl: cl, sel: map[int]bool{}}.enterPlan()
	r := plan.Result{Model: "big", Ctx: 8192, Np: 1, WeightsGB: 10, Verdict: "ok",
		Candidates: []plan.Candidate{{Kind: "single", Master: "dino", Where: "GPU", TPS: 20, TokenMS: 50, Parts: []plan.Part{{Node: "dino", Layers: 32}}}}}
	m.planRes = &r
	m.planPath = "/models/test.gguf"

	// Focus section 2 (params)
	m.planSection, m.planParamCur = 1, 4

	// Toggle CPU-Only with the slider'
	if m.planCPUOnly {
		t.Fatal("expected default planCPUOnly to be false")
	}
	nm, _ := m.keyStr("right", "")
	m = nm.(model)
	if !m.planCPUOnly {
		t.Fatal("expected planCPUOnly to be true after →")
	}

	// Toggle MMProj CPU on its slider
	m.planParamCur = 7
	if m.planMMProjCPU {
		t.Fatal("expected default planMMProjCPU to be false")
	}
	nm, _ = m.keyStr("right", "")
	m = nm.(model)
	if !m.planMMProjCPU {
		t.Fatal("expected planMMProjCPU to be true after →")
	}

	// Toggle mmap on its slider
	m.planParamCur = 8
	if m.planNoMMap {
		t.Fatal("expected default planNoMMap to be false (mmap active)")
	}
	nm, _ = m.keyStr("right", "")
	m = nm.(model)
	if !m.planNoMMap {
		t.Fatal("expected planNoMMap to be true after →")
	}

	// Companion MMProj detection
	tmpDir := t.TempDir()
	modelFile := filepath.Join(tmpDir, "DeepSeek-V4.1-Flash.gguf")
	mmprojFile := filepath.Join(tmpDir, "mmproj-model-f16.gguf")
	_ = os.WriteFile(modelFile, []byte("fake"), 0644)
	_ = os.WriteFile(mmprojFile, []byte("fake"), 0644)

	found := findCompanionMMProj(modelFile)
	if found != mmprojFile {
		t.Fatalf("companion mmproj: got %q, want %q", found, mmprojFile)
	}
}

// ---------------------------------------------------------------- the route table on the role screen

func routeFixture() *server.RouteExplain {
	return &server.RouteExplain{Model: "scout", Role: "scout", Strategy: "cost", In: 200, Out: 300, Order: []server.Cost{
		{Endpoint: "a", Node: "n1", Prefill: 0.3, Decode: 4, Total: 4.3, Notes: []string{"benchmark speed"}},
		{Endpoint: "b", Node: "n2", Unknown: true, Notes: []string{"no speed figure yet"}},
	}}
}

func roleModel() model {
	m := model{sc: scRoleEdit, sel: map[int]bool{}, w: 120}
	m.st.Roles = map[string]server.RoleStatus{"scout": {Role: config.Role{Preferred: "a", Fallback: []string{"b"}, Strategy: "cost"}, Active: "a"}}
	m.st.Endpoints = []server.EpStatus{{Endpoint: config.Endpoint{Name: "a", Node: "n1", Model: "ma"}}, {Endpoint: config.Endpoint{Name: "b", Node: "n2", Model: "mb"}}}
	return m
}

func TestFeature_RoleScreen_ShowsWhereARequestWouldGo(t *testing.T) {
	m := roleModel()
	if v := m.viewRoleEdit(); !strings.Contains(v, "Where a request would go now — short prompt") || !strings.Contains(v, "…") {
		t.Fatalf("a placeholder until the daemon answers:\n%s", v)
	}
	nm, _ := m.Update(statusMsg{st: m.st, route: routeFixture()})
	m = nm.(model)
	v := m.viewRoleEdit()
	for _, want := range []string{"Where a request would go now", "benchmark speed", "no speed figure yet", "4.3s", "wait", "prompt"} {
		if !strings.Contains(v, want) {
			t.Fatalf("missing %q in\n%s", want, v)
		}
	}
	// the table is only kept while the role screen is open
	h := model{sc: scHome, sel: map[int]bool{}}
	nm, _ = h.Update(statusMsg{route: routeFixture()})
	if nm.(model).route != nil {
		t.Fatal("route kept on another screen")
	}
	// and it must be the role's own
	m.route.Role = "other"
	if v := m.viewRoleEdit(); strings.Contains(v, "benchmark speed") {
		t.Fatal("another role's table shown")
	}
}

func TestFeature_RoleScreen_ZCyclesTheRequestSize(t *testing.T) {
	m := roleModel()
	m.route = routeFixture()
	m.roleCur = 0
	_, cl := newNodesDaemon(t)
	m.cl = cl
	for i := 1; i <= len(requestSizes); i++ {
		nm, cmd := m.keyStr("z", "")
		m = nm.(model)
		if m.route != nil || cmd == nil {
			t.Fatal("z drops the old table and asks again")
		}
		if m.routeSize != i%len(requestSizes) {
			t.Fatalf("size %d", m.routeSize)
		}
		m.route = routeFixture()
	}
	if !strings.Contains(m.help(), "z request size") {
		t.Fatal(m.help())
	}
	found := false
	for _, b := range m.buttons() {
		found = found || b.key == "z"
	}
	if !found {
		t.Fatal("no button for z")
	}
}

func TestFeature_RoleScreen_FetchAsksTheDaemonForTheRoute(t *testing.T) {
	f, cl := newNodesDaemon(t)
	if err := cl.Endpoints([]config.Endpoint{{Name: "a", Node: "n1", URL: "http://127.0.0.1:1/v1", Model: "ma", TPS: 40}, {Name: "b", Node: "n2", URL: "http://127.0.0.1:1/v1", Model: "mb", TPS: 90}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := cl.UpdateRole("scout", map[string]any{"preferred": "a", "fallback": []string{"b"}, "strategy": "cost"}); err != nil {
		t.Fatal(err)
	}
	st, err := cl.Status()
	if err != nil {
		t.Fatal(err)
	}
	m := model{cl: cl, sc: scRoleEdit, sel: map[int]bool{}, st: st, routeSize: 1}
	msg := m.fetch()()
	sm, ok := msg.(statusMsg)
	if !ok || sm.route == nil {
		t.Fatalf("%T %+v", msg, msg)
	}
	if sm.route.Role != "scout" || sm.route.In != requestSizes[1].in || sm.route.Out != requestSizes[1].out || len(sm.route.Order) != 2 {
		t.Fatalf("%+v", sm.route)
	}
	hit := false
	for _, p := range f.seen() {
		hit = hit || p == "GET /fleet/route"
	}
	if !hit {
		t.Fatal("no route request")
	}
	// on the Dashboard it is not asked
	m.sc = scDash
	if sm := m.fetch()().(statusMsg); sm.route != nil {
		t.Fatal("route fetched on the dashboard")
	}
	var _ tea.Cmd
}

// ---------------------------------------------------------------- Model files

func storeFixture() []server.NodeStore {
	a := store.Entry{SHA256: "aaaaaaaa11112222", Name: "qwen", Size: 100 << 30, Complete: true}
	b := store.Entry{SHA256: "bbbbbbbb33334444", Name: "llama", Size: 5 << 30, Complete: true}
	return []server.NodeStore{
		{Node: "dino", Store: &store.Inventory{Files: []store.Entry{a, b}}},
		{Node: "p1", Store: &store.Inventory{Files: []store.Entry{b}, Cache: &store.CacheInfo{Dir: "/c", Bytes: 3 << 30, Entries: 9}}},
		{Node: "vec", Err: "no route to a fleet on vec"},
	}
}

func TestFeature_Store_ScreenFromHomeWithArrows(t *testing.T) {
	_, cl := newNodesDaemon(t)
	m := model{cl: cl, sc: scHome, sel: map[int]bool{}, w: 120}
	var downs []string
	for i, it := range home {
		if it.title == "Model files" {
			for j := 0; j < i; j++ {
				downs = append(downs, "down")
			}
		}
	}
	m = arrows(t, m, append(downs, "enter")...)
	if m.sc != scStore {
		t.Fatalf("screen %v", m.sc)
	}
	// the fake daemon answers (an empty store on this machine)
	if v := m.viewStore(); !strings.Contains(v, "no model files registered") && !strings.Contains(v, "asking") {
		t.Fatalf("\n%s", v)
	}
	m.storeNodes = storeFixture()
	v := m.viewStore()
	for _, want := range []string{"FILE", "llama", "qwen", "✓", "worker caches: p1 3.0 GiB", "vec: no route", "sha256", "dino, p1"} {
		if !strings.Contains(v, want) {
			t.Fatalf("missing %q in\n%s", want, v)
		}
	}
	if m.listLen() != 2 {
		t.Fatalf("two files: %d", m.listLen())
	}
	// ↓ moves to the second file (qwen), whose detail names who lacks it
	m = arrows(t, m, "down")
	if v := m.viewStore(); !strings.Contains(v, "not on p1, vec") {
		t.Fatalf("\n%s", v)
	}
	var keys []string
	for _, b := range m.buttons() {
		keys = append(keys, b.key)
	}
	if strings.Join(keys, ",") != "n,f,c,a,x,esc" {
		t.Fatalf("buttons %v", keys)
	}
	if !strings.Contains(m.help(), "add a file") {
		t.Fatal(m.help())
	}
}

func TestFeature_Store_AddAndForgetByButtons(t *testing.T) {
	_, cl := newNodesDaemon(t)
	home := os.Getenv("HOME")
	p := filepath.Join(home, "models", "tiny.gguf")
	tinyGGUF(t, p)
	m := model{cl: cl, sc: scStore, sel: map[int]bool{}, w: 120}
	m = arrows(t, m, "tab") // to the bar: Add file… first
	nm, cmd := m.keyStr("n", "")
	m = nm.(model)
	if m.editing != "storeadd" || cmd != nil || !strings.Contains(m.editPrompt(), "directory of them") {
		t.Fatalf("editing %q", m.editing)
	}
	for _, r := range "~/models" {
		nm, _ = m.keyStr(string(r), string(r))
		m = nm.(model)
	}
	nm, cmd = m.keyStr("enter", "")
	m = nm.(model)
	if m.editing != "" || cmd == nil {
		t.Fatal("enter runs the add")
	}
	if n := cmd().(noteMsg); !strings.Contains(string(n), "hashing 1 file(s)") {
		t.Fatalf("%q", n)
	}
	var nodes []server.NodeStore
	for i := 0; i < 200 && len(nodes) == 0; i++ {
		time.Sleep(20 * time.Millisecond)
		got, err := cl.StoreAll()
		if err == nil && len(got) > 0 && got[0].Store != nil && len(got[0].Store.Files) == 1 && got[0].Store.Hashing == nil {
			nodes = got
		}
	}
	if len(nodes) == 0 {
		t.Fatal("the file was not registered")
	}
	m.storeNodes = nodes
	m.inBar = false
	nm, cmd = m.keyStr("x", "")
	m = nm.(model)
	if n := cmd().(noteMsg); !strings.Contains(string(n), "forgot tiny") {
		t.Fatalf("%q", n)
	}
	if got, _ := cl.StoreAll(); len(got[0].Store.Files) != 0 {
		t.Fatalf("%+v", got[0].Store)
	}
	// an empty path adds nothing
	m.editing, m.editBuf = "storeadd", "  "
	if _, cmd := m.keyStr("enter", ""); cmd != nil {
		t.Fatal("empty path")
	}
}

func TestFeature_Store_FetchAndFillByPicking(t *testing.T) {
	_, cl := newNodesDaemon(t)
	m := model{cl: cl, sc: scStore, sel: map[int]bool{}, w: 120, storeNodes: storeFixture()}
	m.cur = 1 // qwen: only dino has it
	// f: the machines that lack it, chosen with the arrow keys
	nm, _ := m.keyStr("f", "")
	m = nm.(model)
	if !m.pick || m.pickMode != "storepull" || strings.Join(m.pickNodes, ",") != "p1" || m.storeCur != 1 || m.cur != 0 {
		t.Fatalf("pick %v mode %q nodes %v storeCur %d cur %d", m.pick, m.pickMode, m.pickNodes, m.storeCur, m.cur)
	}
	if v := m.viewStore(); !strings.Contains(v, "Machine to fetch the file to") || !strings.Contains(v, "p1") || strings.Contains(v, "vec") {
		t.Fatalf("\n%s", v)
	}
	if m.listLen() != 1 {
		t.Fatalf("the picker has its own list (a machine whose fleet cannot say is not offered): %d", m.listLen())
	}
	var keys []string
	for _, b := range m.buttons() {
		keys = append(keys, b.key)
	}
	if strings.Join(keys, ",") != "enter,esc" {
		t.Fatalf("%v", keys)
	}
	m = arrows(t, m, "down", "esc") // the cursor returns to the file
	if m.pick || m.cur != 1 {
		t.Fatalf("pick %v cur %d", m.pick, m.cur)
	}
	// choose p1: the daemon cannot reach it (not a machine of this test daemon), and says so
	m = arrows(t, m, "tab")
	nm, cmd := m.keyStr("f", "")
	m = nm.(model)
	nm, cmd = m.keyStr("enter", "")
	m = nm.(model)
	if m.pick || cmd == nil {
		t.Fatal("enter asks the machine")
	}
	if n := string(cmd().(noteMsg)); !strings.Contains(n, "error") || !strings.Contains(n, "p1") {
		t.Fatalf("%q", n)
	}
	// c: fill a worker cache: pick the machine, then type the blocks
	m.cur = 1
	nm, _ = m.keyStr("c", "")
	m = nm.(model)
	if m.pickMode != "storefill" || len(m.pickNodes) != 2 { // machines that answered: dino and p1
		t.Fatalf("%q %v", m.pickMode, m.pickNodes)
	}
	nm, _ = m.keyStr("down", "")
	m = nm.(model)
	nm, _ = m.keyStr("enter", "")
	m = nm.(model)
	if m.editing != "storefill" || !strings.Contains(m.editPrompt(), "blocks to cache on p1") {
		t.Fatalf("editing %q %q", m.editing, m.editPrompt())
	}
	for _, r := range "0-3 output" {
		nm, _ = m.keyStr(string(r), string(r))
		m = nm.(model)
	}
	nm, cmd = m.keyStr("enter", "")
	m = nm.(model)
	if n := string(cmd().(noteMsg)); !strings.Contains(n, "error") {
		t.Fatalf("%q", n)
	}
	// nothing typed: nothing asked
	m.editing, m.editFor, m.editBuf = "storefill", "p1|"+strings.Repeat("a", 64), "  "
	if _, cmd := m.keyStr("enter", ""); cmd != nil {
		t.Fatal("no blocks, no request")
	}
	// a file every machine has (llama: dino and p1)
	m.editing, m.note = "", ""
	m.cur = 0
	m.storeNodes = storeFixture()[:2]
	nm, _ = m.keyStr("f", "")
	if n := nm.(model).note; !strings.Contains(n, "every machine has this file") {
		t.Fatalf("%q", n)
	}
}

func TestFeature_Plan_FetchNeedsARegisteredFile(t *testing.T) {
	m := model{sc: scPlan, sel: map[int]bool{}}
	nm, cmd := m.keyStr("f", "")
	if cmd != nil || !strings.Contains(nm.(model).note, "plan a registered file first") {
		t.Fatalf("%q", nm.(model).note)
	}
	r := planResultN(2)
	m.planRes = &r // not known
	if _, cmd := m.keyStr("f", ""); cmd != nil {
		t.Fatal("an unregistered file cannot be fetched")
	}
	r.Known, r.SHA256 = true, strings.Repeat("a", 64)
	_, cl := newNodesDaemon(t)
	m.cl = cl
	nm, cmd = m.keyStr("f", "")
	if cmd == nil || !strings.Contains(nm.(model).note, "fetching") {
		t.Fatalf("%q", nm.(model).note)
	}
	// the daemon has no such machines: the answer is an error note, not a hang
	if n := string(cmd().(noteMsg)); !strings.Contains(n, "error") {
		t.Fatalf("%q", n)
	}
}

func TestFeature_Plan_ChooseTheCandidateWithArrows(t *testing.T) {
	rec, cl := newRecorder(t)
	m, _ := planModelAt(t)
	m.cl = cl
	res := plan.Result{Model: "x", Known: false, Candidates: []plan.Candidate{
		{Kind: "single", Where: "GPU", Master: "a", TPS: 10, Parts: []plan.Part{{Node: "a", Layers: 4}}},
		{Kind: "single", Where: "GPU", Master: "b", TPS: 8, Parts: []plan.Part{{Node: "b", Layers: 4}}},
		{Kind: "split", Master: "c", TPS: 3, Parts: []plan.Part{{Node: "c", Layers: 2}, {Node: "a", Layers: 2, First: 2}}}}}
	m.sc, m.planRes, m.planPath = scPlan, &res, "p"
	m, _ = press(t, m, "c")
	if !m.pick || m.listLen() != 3 {
		t.Fatalf("choosing: pick=%v len=%d", m.pick, m.listLen())
	}
	if got := strings.Join(buttonLabelsOf(m), "|"); got != "Choose|Cancel" {
		t.Fatal(got)
	}
	m, _ = press(t, m, "down")
	m, _ = press(t, m, "down")
	m, _ = press(t, m, "up")
	if !strings.Contains(m.viewPlan(), "▸") {
		t.Fatal("the highlighted candidate needs a mark")
	}
	m, _ = press(t, m, "enter")
	if m.pick || m.planPick != 1 {
		t.Fatalf("pick=%v planPick=%d", m.pick, m.planPick)
	}
	if v := m.viewPlan(); !strings.Contains(v, "✓") {
		t.Fatalf("the chosen candidate is marked:\n%s", v)
	}
	m, _ = press(t, m, "w")
	if rec.last["/fleet/runs"]["pick"] != float64(2) {
		t.Fatalf("w must record candidate 2: %v", rec.last["/fleet/runs"])
	}
	// esc leaves the choice as it was
	m, _ = press(t, m, "c")
	m, _ = press(t, m, "down")
	m, _ = press(t, m, "esc")
	if m.pick || m.planPick != 1 {
		t.Fatalf("esc: pick=%v planPick=%d", m.pick, m.planPick)
	}
}

func buttonLabelsOf(m model) []string {
	var l []string
	for _, b := range m.buttons() {
		l = append(l, b.label)
	}
	return l
}

func TestFeature_Plan_ContextSliderCapsAtModelMax(t *testing.T) {
	// Case 1: No model planned -> defaults to 32k max
	m := model{sc: scPlan}
	ctxsDefault := m.availablePlanCtxs()
	if len(ctxsDefault) != 5 || ctxsDefault[len(ctxsDefault)-1] != 32768 {
		t.Fatalf("expected default 32k max, got: %v", ctxsDefault)
	}

	// Case 2: Model with 256k max -> caps at 262144 (256k), does not reach 524288 or 1048576
	m.planModel = &plan.Model{CtxMax: 262144}
	ctxs256 := m.availablePlanCtxs()
	if len(ctxs256) != 8 || ctxs256[len(ctxs256)-1] != 262144 {
		t.Fatalf("expected 256k max, got: %v", ctxs256)
	}
	for _, c := range ctxs256 {
		if c > 262144 {
			t.Fatalf("context %d exceeds model max 256k", c)
		}
	}

	// View should show 256k and NOT 524288 or 1048576
	view := m.viewPlan()
	if strings.Contains(view, "524288") || strings.Contains(view, "1048576") {
		t.Fatalf("view should not contain contexts above model max:\n%s", view)
	}
	if !strings.Contains(view, "model max: 256k") {
		t.Fatalf("view should report model max 256k:\n%s", view)
	}

	// Case 3: Model with 8k max -> caps at 8192
	m.planModel = &plan.Model{CtxMax: 8192}
	ctxs8 := m.availablePlanCtxs()
	if len(ctxs8) != 3 || ctxs8[len(ctxs8)-1] != 8192 {
		t.Fatalf("expected 8k max, got: %v", ctxs8)
	}
}

// No undocumented keys on the plan screen: every letter or digit the screen reacts to is a button, and each
// button's key is named once in the help line. The sliders answer ←/→ only (no letter twin).
func TestFeature_Plan_NoUndocumentedKeys(t *testing.T) {
	_, cl := newNodesDaemon(t)
	base := model{cl: cl, sel: map[int]bool{}, w: 120, h: 40}.enterPlan()
	r := plan.Result{Model: "big", Ctx: 8192, Np: 1, WeightsGB: 10, Known: true, Verdict: "run it on dino alone",
		Candidates: []plan.Candidate{{Kind: "single", Master: "dino", Where: "GPU", TPS: 20, TokenMS: 50, Parts: []plan.Part{{Node: "dino", Layers: 32}}}}}
	base.planRes = &r
	base.planPath = "/models/test.gguf"
	base.planFiles = []string{"/models/test.gguf"}

	help := base.help()
	buttons := map[string]bool{}
	for _, b := range base.buttons() {
		buttons[b.key] = true
	}
	const navAliases = "hjklq1234" // global aliases of the arrows and esc, the same on every screen; 1-4 jump to a section
	for _, sec := range []int{0, 1, 2, 3} {
		for _, k := range strings.Split("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789", "") {
			if strings.Contains(navAliases, k) {
				continue
			}
			m := base
			m.planSection = sec
			nm, cmd := m.keyStr(k, k)
			after := nm.(model)
			if cmd == nil && after.planSection == m.planSection && after.planCtx == m.planCtx && after.planNp == m.planNp &&
				after.planStrat == m.planStrat && after.planMaster == m.planMaster && after.planCPUOnly == m.planCPUOnly &&
				after.planMoECache == m.planMoECache && after.planSpecMode == m.planSpecMode && after.planMMProjCPU == m.planMMProjCPU &&
				after.planNoMMap == m.planNoMMap && after.planEngineMode == m.planEngineMode && after.planViaHub == m.planViaHub && after.planExpl == m.planExpl &&
				after.editing == m.editing && after.sc == m.sc && after.profOpen == m.profOpen && after.note == m.note && after.pick == m.pick {
				continue // not handled
			}
			if !buttons[k] {
				t.Errorf("section %d: key %q does something but has no button", sec, k)
			}
		}
	}
	for k := range buttons {
		if k == "esc" || k == "enter" {
			continue
		}
		if n := strings.Count(" "+help, " "+k+" "); n != 1 {
			t.Errorf("button key %q is named %d times in the help line (want once): %s", k, n, help)
		}
	}
	// the slider letters are gone
	for _, k := range []string{"n", "o", "m", "u", "v", "g", "y", "D", "A", "T", "O", "U", "V", "G", "Y", "X"} {
		if buttons[k] || strings.Contains(" "+help, " "+k+" ") {
			t.Errorf("%q is still documented", k)
		}
	}
}

func TestFeature_Plan_EngineModeAndRTTSmart(t *testing.T) {
	m, _ := planModelAt(t)
	nm, _ := m.keyStr("enter", "")
	m = nm.(model)
	m.planSection = 1
	m.planParamCur = 9 // Engine selector

	// 1. Selector cycling via right / left / enter
	if m.planEngineMode != 0 {
		t.Fatalf("expected default planEngineMode to be 0 (Auto), got %d", m.planEngineMode)
	}
	nm, _ = m.keyStr("right", "")
	m = nm.(model)
	if m.planEngineMode != 1 {
		t.Fatalf("expected planEngineMode 1 (ring) after right, got %d", m.planEngineMode)
	}
	nm, _ = m.keyStr("right", "")
	m = nm.(model)
	if m.planEngineMode != 2 {
		t.Fatalf("expected planEngineMode 2 (chain) after right, got %d", m.planEngineMode)
	}
	nm, _ = m.keyStr("right", "")
	m = nm.(model)
	if m.planEngineMode != 0 {
		t.Fatalf("expected planEngineMode 0 (Auto) after cycling, got %d", m.planEngineMode)
	}
	nm, _ = m.keyStr("left", "")
	m = nm.(model)
	if m.planEngineMode != 2 {
		t.Fatalf("expected planEngineMode 2 (chain) after left, got %d", m.planEngineMode)
	}
	nm, _ = m.keyStr("enter", "")
	m = nm.(model)
	if m.planEngineMode != 0 {
		t.Fatalf("expected planEngineMode 0 (Auto) after enter, got %d", m.planEngineMode)
	}

	// 2. Candidate evaluation with low RTT (< 15ms)
	m.planModel = &plan.Model{Arch: "llama"}
	candLow := plan.Candidate{
		Kind:   "ring",
		Master: "local",
		TPS:    42.0,
		Parts: []plan.Part{
			{Node: "local", RTTms: 1.2},
			{Node: "worker1", RTTms: 4.5},
		},
	}
	m.st.Nodes = []server.NodeStatus{
		{Node: "local", Self: true, Up: 1, Total: 1},
		{Node: "worker1", PingMS: 5, Up: 1, Total: 1},
	}

	eng, highRTT, rtt, _ := m.effectiveEngine(candLow)
	if eng != "ring" || highRTT {
		t.Fatalf("low RTT: expected ring with highRTT=false, got %s highRTT=%v rtt=%v", eng, highRTT, rtt)
	}

	// 3. Candidate evaluation with high RTT (>= 15ms)
	candHigh := plan.Candidate{
		Kind:   "ring",
		Master: "local",
		TPS:    28.0,
		Parts: []plan.Part{
			{Node: "local", RTTms: 1.0},
			{Node: "worker-wan", RTTms: 24.5},
		},
	}
	m.st.Nodes = []server.NodeStatus{
		{Node: "local", Self: true, Up: 1, Total: 1},
		{Node: "worker-wan", PingMS: 25, Up: 1, Total: 1},
	}

	eng, highRTT, rtt, _ = m.effectiveEngine(candHigh)
	if eng != "chain" || !highRTT || rtt < 24.0 {
		t.Fatalf("high RTT: expected chain with highRTT=true, got %s highRTT=%v rtt=%v", eng, highRTT, rtt)
	}

	// 4. planViaHub forces chain recommendation even if direct ping is low
	m.planViaHub = true
	eng, highRTT, _, _ = m.effectiveEngine(candLow)
	if eng != "chain" || !highRTT {
		t.Fatalf("via hub: expected chain with highRTT=true, got %s highRTT=%v", eng, highRTT)
	}
	m.planViaHub = false

	// 5. Manual override: ring forced even under high RTT; highRTT is still reported so the view can warn
	m.planEngineMode = 1 // ring
	eng, highRTT, _, _ = m.effectiveEngine(candHigh)
	if eng != "ring" || !highRTT {
		t.Fatalf("manual ring: expected ring with highRTT=true, got %s highRTT=%v", eng, highRTT)
	}
	m.planRes = &plan.Result{Candidates: []plan.Candidate{candHigh}}
	v := m.viewPlan()
	m.planRes = nil
	if !strings.Contains(v, "chain recommended") {
		t.Errorf("manual ring under high RTT: no warning in the view")
	}

	// 6. Manual override: chain forced even under low RTT
	m.planEngineMode = 2 // chain
	eng, highRTT, _, _ = m.effectiveEngine(candLow)
	if eng != "chain" || highRTT {
		t.Fatalf("manual chain: expected chain with highRTT=false, got %s highRTT=%v", eng, highRTT)
	}

	// 7. Auto + high RTT + draft-dflash without draft model -> fallback to ring with reason
	m.planEngineMode = 0 // Auto
	m.planSpecMode = 1   // draft-dflash
	eng, highRTT, _, why := m.effectiveEngine(candHigh)
	if eng != "ring" {
		t.Fatalf("Auto + draft-dflash: expected ring fallback, got %s", eng)
	}
	if !strings.Contains(why, "draft-dflash") {
		t.Fatalf("Auto + draft-dflash: expected reason about draft-dflash, got %q", why)
	}
	m.planSpecMode = 0

	// 8. Manual chain + unsupported architecture -> deployCandidate blocks deploy
	m.planEngineMode = 2 // chain
	m.planModel = &plan.Model{Arch: "unsupported_xyz"}
	m.planRes = &plan.Result{
		Model:      "test-model",
		Arch:       "unsupported_xyz",
		Candidates: []plan.Candidate{candLow},
	}
	m.planPick = 0
	nmRes, deployCmd := m.deployCandidate(false)
	m = nmRes.(model)
	if deployCmd != nil {
		t.Fatalf("manual chain + unsupported: expected deployCmd to be nil (blocked), got non-nil")
	}
	if !strings.Contains(m.note, "chain not supported") || !strings.Contains(m.note, "unsupported_xyz") {
		t.Fatalf("expected note to explain chain not supported, got %q", m.note)
	}

	// 9. Manual chain + supported -> deployCandidate(dry=true) actually executes chain dry run
	tmpDir := t.TempDir()
	d := plan.Descriptor{
		Version: 1, Name: "toy-chain", Architecture: "llama", Runtime: plan.RuntimeStagePipe,
		LayerCount: 64, HiddenSize: 4096, VocabSize: 32000, ContextMax: 4096,
		EmbeddingAt: "first", EmbeddingBytes: 1 << 20, HeadAt: "last", HeadBytes: 2 << 20,
		Layers: []plan.DescriptorLayer{{Index: 0, File: "l.bin", Bytes: 10 << 20, State: plan.StateAttentionKV, KVBytesPerToken: 128}},
	}
	for i := 1; i < 64; i++ {
		d.Layers = append(d.Layers, plan.DescriptorLayer{Index: i, File: "l.bin", Bytes: 10 << 20, State: plan.StateAttentionKV, KVBytesPerToken: 128})
	}
	db, _ := json.Marshal(d)
	_ = os.WriteFile(filepath.Join(tmpDir, plan.DescriptorFile), db, 0o644)

	candChain := plan.Candidate{
		Kind:   "split",
		Master: "local",
		TPS:    42.0,
		Parts: []plan.Part{
			{Node: "local", Layers: 32, First: 0},
			{Node: "worker1", Layers: 32, First: 32, Output: true},
		},
	}
	m.planPath = tmpDir
	m.planModel = &plan.Model{Arch: "llama", Layers: 64}
	m.planRes = &plan.Result{
		Arch:       "llama",
		Candidates: []plan.Candidate{candChain},
	}
	m.planPick = 0
	_, chainDryCmd := m.deployCandidate(true)
	if chainDryCmd == nil {
		t.Fatalf("expected non-nil deployCmd for chain dry run")
	}
	dryMsg := chainDryCmd()
	out, ok := dryMsg.(outMsg)
	if !ok || !strings.Contains(out.note, "dry run (chain)") {
		t.Fatalf("expected chain dry run note, got: %+v", dryMsg)
	}

	// 10. UI View contains engine selector and RTT recommendations
	m.planModel = &plan.Model{Arch: "llama"}
	m.planEngineMode = 0 // back to Auto
	m.planRes = &plan.Result{
		Arch:       "llama",
		Candidates: []plan.Candidate{candHigh},
	}
	m.planPick = 0
	m.w = 120
	view := m.viewPlan()
	if !strings.Contains(view, "engine") {
		t.Fatalf("expected view to contain 'engine' selector: %s", view)
	}
	if !strings.Contains(view, "chain recommended") {
		t.Fatalf("expected view to contain 'chain recommended' hint on high RTT: %s", view)
	}
	if !strings.Contains(view, "Engine: chain (auto: high RTT 25ms)") && !strings.Contains(view, "Engine: chain (auto: high RTT 24ms)") {
		t.Fatalf("expected deploy summary in view to show chain recommendation: %s", view)
	}
}

// The DFlash spec mode needs a draft model: it is found next to the model or one directory up, with or without an
// embedded head as asked.
func TestFindCompanionDFlash(t *testing.T) {
	root := t.TempDir()
	quant := filepath.Join(root, "IQ1_M")
	if err := os.MkdirAll(quant, 0o755); err != nil {
		t.Fatal(err)
	}
	model := filepath.Join(quant, "Model-IQ1_M-00001-of-00002.gguf")
	for _, f := range []string{model, filepath.Join(root, "Model-DFlash2-Q4_K_M.gguf"), filepath.Join(root, "Model-DFlash2-Q4_K_M-head.gguf"), filepath.Join(root, "dflash-notes.txt")} {
		if err := os.WriteFile(f, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := findCompanionDFlash(model, false); filepath.Base(got) != "Model-DFlash2-Q4_K_M.gguf" {
		t.Errorf("ring: got %q, want the plain draft", got)
	}
	if got := findCompanionDFlash(model, true); filepath.Base(got) != "Model-DFlash2-Q4_K_M-head.gguf" {
		t.Errorf("chain: got %q, want the headed draft", got)
	}
	if got := findCompanionDFlash(filepath.Join(t.TempDir(), "x.gguf"), false); got != "" {
		t.Errorf("no draft: got %q", got)
	}
}

// DFlash without a draft next to the model is refused before anything starts (llama-server would fail on it).
func TestDeploy_DFlashWithoutDraftRefused(t *testing.T) {
	mpath := filepath.Join(t.TempDir(), "m.gguf")
	m := model{sc: scPlan, sel: map[int]bool{}, w: 120, planPath: mpath, planSpecMode: 1, planEngineMode: 1,
		planRes: &plan.Result{Candidates: []plan.Candidate{{Kind: "single", Master: "local"}}}}
	nm, cmd := m.deployCandidate(false)
	after := nm.(model)
	if cmd != nil || !strings.Contains(after.note, "no *dflash*.gguf draft") || after.deployChan != nil {
		t.Fatalf("want a refusal and no running deploy, got cmd=%v note=%q", cmd != nil, after.note)
	}
}

// A model gets its own vision projector only: not the projector of another model in a shared directory.
func TestFindCompanionMMProj(t *testing.T) {
	touch := func(p string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	shared := t.TempDir() // ~/models with several models and DeepSeek's projector
	for _, f := range []string{"LFM2-8B-A1B-Q4_K_M.gguf", "DeepSeek-V4.1-Flash-IQ2_XXS-00001-of-00006.gguf", "mmproj-DeepSeek-V4.1-Flash-BF16.gguf"} {
		touch(filepath.Join(shared, f))
	}
	if got := findCompanionMMProj(filepath.Join(shared, "LFM2-8B-A1B-Q4_K_M.gguf")); got != "" {
		t.Errorf("LFM2 got another model's projector %q", got)
	}
	if got := findCompanionMMProj(filepath.Join(shared, "DeepSeek-V4.1-Flash-IQ2_XXS-00001-of-00006.gguf")); filepath.Base(got) != "mmproj-DeepSeek-V4.1-Flash-BF16.gguf" {
		t.Errorf("DeepSeek: got %q", got)
	}
	split := t.TempDir() // Qwen layout: shards in a quant subdirectory, projector one up
	touch(filepath.Join(split, "IQ1_M", "Qwen3.8-Flash-Next-IQ1_M-00001-of-00002.gguf"))
	touch(filepath.Join(split, "IQ1_M", "Qwen3.8-Flash-Next-IQ1_M-00002-of-00002.gguf"))
	touch(filepath.Join(split, "mmproj-Qwen3.8-Flash-Next-BF16.gguf"))
	touch(filepath.Join(split, "Qwen3.8-Flash-Next-DFlash2-Q4_K_M.gguf"))
	if got := findCompanionMMProj(filepath.Join(split, "IQ1_M", "Qwen3.8-Flash-Next-IQ1_M-00001-of-00002.gguf")); filepath.Base(got) != "mmproj-Qwen3.8-Flash-Next-BF16.gguf" {
		t.Errorf("split Qwen: got %q", got)
	}
	own := t.TempDir() // one model with a generic projector name
	touch(filepath.Join(own, "gemma-3-4b-it-Q4_K_M.gguf"))
	touch(filepath.Join(own, "mmproj-model-f16.gguf"))
	if got := findCompanionMMProj(filepath.Join(own, "gemma-3-4b-it-Q4_K_M.gguf")); filepath.Base(got) != "mmproj-model-f16.gguf" {
		t.Errorf("generic projector, one model: got %q", got)
	}
	touch(filepath.Join(own, "qwen2.5-14b.gguf"))
	if got := findCompanionMMProj(filepath.Join(own, "qwen2.5-14b.gguf")); got != "" {
		t.Errorf("generic projector, two models: qwen2.5 got %q", got)
	}
}

// the plan screen's chain deploy: the hub toggle reaches the stages, and --auto only when the MoE cache is left on auto
func TestChainStageOptionsFollowTheScreen(t *testing.T) {
	o := chainStageOptions("/m/a.gguf", 8192, 0, 0, "none", "", true)
	if !o.ViaHub || !o.Auto || o.CPUMoE || o.Ctx != 8192 {
		t.Errorf("auto: %+v", o)
	}
	o = chainStageOptions("/m/a.gguf", 0, 2, 3300, "none", "", false)
	if o.ViaHub || o.Auto || !o.CPUMoE || o.MoECacheMiB != 3300 {
		t.Errorf("a size: %+v", o)
	}
	if o = chainStageOptions("/m/a.gguf", 0, 4, 0, "none", "", false); o.Auto || o.CPUMoE {
		t.Errorf("disabled: %+v", o)
	}
}

// 1-4 jump to the sections (files, sliders, candidates, deploy), also from the button bar.
func TestFeature_Plan_DigitsJumpSections(t *testing.T) {
	_, cl := newNodesDaemon(t)
	m := model{cl: cl, sel: map[int]bool{}, w: 120, h: 40}.enterPlan()
	r := plan.Result{Model: "big", Ctx: 8192, Np: 1, Candidates: []plan.Candidate{{Kind: "single", Master: "dino", Parts: []plan.Part{{Node: "dino", Layers: 32}}}}}
	m.planRes, m.planFiles, m.inBar = &r, []string{"/models/a.gguf"}, true
	for k, want := range map[string]int{"1": 0, "2": 1, "3": 2, "4": 3} {
		nm, _ := m.keyStr(k, k)
		if got := nm.(model); got.planSection != want || got.inBar {
			t.Errorf("key %s: section %d inBar %v, want %d off the bar", k, got.planSection, got.inBar, want)
		}
	}
}

// Before a plan has run (or when it failed) the context choices still reach the model's own maximum, read from the
// header of the file in view — they used to shrink to 32k until the plan came back.
func TestFeature_Plan_CtxMaxFromHeaderBeforePlan(t *testing.T) {
	p := os.ExpandEnv("$HOME/models/Ornith-1.5-122B-DFlash2-Q4_K_M.gguf")
	mod, err := plan.ReadGGUF(p)
	if err != nil || mod.CtxMax <= 32768 {
		t.Skip("no suitable model file here")
	}
	m := model{sel: map[int]bool{}}.enterPlan()
	m.planFiles, m.planPath, m.planModel = []string{p}, "", nil
	ctxs := m.availablePlanCtxs()
	if ctxs[len(ctxs)-1] != mod.CtxMax {
		t.Fatalf("max %d, choices %v", mod.CtxMax, ctxs)
	}
}

// Forget acts on the machines that hold the file, not only on this one: a file of dino used to answer "no file in the store
// matches" from this machine's store.
func TestFeature_Store_ForgetReachesTheMachineThatHoldsIt(t *testing.T) {
	_, cl := newNodesDaemon(t)
	m := model{cl: cl, sc: scStore, sel: map[int]bool{}, w: 120, storeNodes: storeFixture()}
	m.cur = 1 // qwen: only dino has it
	nm, cmd := m.keyStr("x", "")
	_ = nm
	if cmd == nil {
		t.Fatal("x forgets the file")
	}
	got := string(cmd().(noteMsg))
	if !strings.Contains(got, "dino") || strings.Contains(got, "no file in the store matches") {
		t.Fatalf("the request must go to dino (this test daemon has no route to it): %s", got)
	}
	// a file that no reachable machine holds is said so
	m.storeNodes = []server.NodeStore{{Node: "vec", Err: "no route"}}
	m.storeNodes = append(m.storeNodes, storeFixture()[0])
	m.storeNodes[1].Store = &store.Inventory{}
	if rows := storeModels(m.storeNodes); len(rows) != 0 {
		t.Fatalf("rows %d", len(rows))
	}
}

// A file whose record is not complete on its machine (gone or changed on disk) shows on no machine, yet it sits in that
// machine's store: it is forgotten there.
func TestFeature_Store_ForgetsAnIncompleteRecordOnItsMachine(t *testing.T) {
	_, cl := newNodesDaemon(t)
	e := store.Entry{SHA256: "cccccccc55556666", Name: "deepseek-00001-of-00006", Size: 5 << 20, Shard: 1, ShardOf: 6}
	nodes := []server.NodeStore{
		{Node: "p1", Store: &store.Inventory{}},
		{Node: "dino", Store: &store.Inventory{Files: []store.Entry{e}}},
	}
	rows := storeModels(nodes)
	if len(rows) != 1 {
		t.Fatalf("rows %d", len(rows))
	}
	if h := storeHolders(nodes, rows[0]); strings.Join(h, ",") != "dino" {
		t.Fatalf("holders %v", h)
	}
	m := model{cl: cl, sc: scStore, sel: map[int]bool{}, w: 120, storeNodes: nodes}
	_, cmd := m.keyStr("x", "")
	got := string(cmd().(noteMsg))
	if strings.Contains(got, "nothing to forget") || !strings.Contains(got, "dino") {
		t.Fatalf("the request must go to dino: %s", got)
	}
}

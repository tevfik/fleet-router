package profile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/plan"
)

// The example of docs/profiles.md: Ornith-122B over the hub, master dino, worker papa with CPU experts 34-40, DFlash n2.
const ornithYAML = `name: ornith-hub
notes: Ornith-122B over the hub
model: /models/ornith-122b/Ornith-1.5-122B-Q4_K_M.gguf
master: dino
workers: [papa]
ctx: 8192
np: 1
layout:
  mode: fixed
  ts: 34,14
  worker_cpu_experts:
    papa: 34-40
  via_hub: true
spec:
  type: draft-dflash
  draft_model: /models/ornith-122b/dflash-draft.gguf
  draft_max: 2
cache:
  k: q8_0
  v: q8_0
io_timeout: 120
exit_on_fail: true
env:
  workers:
    papa:
      GGML_CUDA_GRAPHS: "0"
`

func TestStoreCRUD(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "profiles")}
	p, err := Parse([]byte(ornithYAML))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load("ornith-hub")
	if err != nil || got.Master != "dino" || got.Layout.WorkerCPUExperts["papa"] != "34-40" || got.Spec.DraftMax != 2 || got.Env.Workers["papa"]["GGML_CUDA_GRAPHS"] != "0" {
		t.Fatalf("%v %+v", err, got)
	}
	if got.EffectiveMode() != LayoutFixed {
		t.Error("fixed layout")
	}
	p2 := &Profile{Name: "second", Model: "/m/x.gguf"}
	if err := s.Save(p2); err != nil {
		t.Fatal(err)
	}
	if p2.EffectiveMode() != LayoutAuto {
		t.Error("a new profile without a split is auto")
	}
	list, bad, err := s.List()
	if err != nil || len(bad) != 0 || len(list) != 2 || list[0].Name != "ornith-hub" || list[1].Name != "second" {
		t.Fatalf("%v %v %v", list, bad, err)
	}
	// atomic write: no temporary files are left, the file is private
	ents, _ := os.ReadDir(s.Dir)
	for _, e := range ents {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("left over: %s", e.Name())
		}
	}
	if st, _ := os.Stat(filepath.Join(s.Dir, "second.yaml")); st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode())
	}
	if err := s.Remove("second"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load("second"); err == nil {
		t.Error("removed")
	}
	if err := s.Remove("second"); err == nil {
		t.Error("removing a missing profile is an error")
	}
	// a broken file is reported, not hidden
	os.WriteFile(filepath.Join(s.Dir, "broken.yaml"), []byte("model: [\n"), 0o600)
	if _, bad, _ := s.List(); bad["broken"] == nil {
		t.Error("broken profile must be reported")
	}
}

func TestNameValidation(t *testing.T) {
	for _, ok := range []string{"a", "ornith-hub", "Q3.5_test", "x1"} {
		if ValidName(ok) != nil {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", ".hidden", "-x", "a/b", "../x", "a b", "x.yaml", "a..b", strings.Repeat("a", 65), "a;b"} {
		if ValidName(bad) == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
	if err := (Store{Dir: t.TempDir()}).Save(&Profile{Name: "../evil", Model: "m"}); err == nil {
		t.Error("save must refuse a bad name")
	}
}

func TestParseRefusesUnknownKeysAndBadValues(t *testing.T) {
	for _, y := range []string{
		"model: m\nmastr: dino\n",
		"master: dino\n",
		"model: m\nlayout: {mode: sometimes}\n",
		"model: m\nlayout: {worker_cpu_experts: {w: 40-34}}\n",
	} {
		if _, err := Parse([]byte(y)); err == nil {
			t.Errorf("must be refused: %q", y)
		}
	}
}

func TestSet(t *testing.T) {
	p, _ := Parse([]byte(ornithYAML))
	p.Runs = []Run{{At: time.Now(), Layout: RunLayout{Master: "dino"}}}
	steps := []struct{ k, v string }{
		{"ctx", "16384"}, {"layout.ts", "33,15"}, {"spec.type", "none"}, {"workers", "papa,victor"},
		{"env.master.GGML_RING_X", "1"}, {"worker_models.victor", "/data/o.gguf"}, {"layout.worker_cpu_experts.papa", "36-40"},
		{"extra_master_args", "--temp 0"}, {"exit_on_fail", "false"}, {"force", "true"}, {"cache.k", ""}, {"notes", "123"},
	}
	for _, s := range steps {
		if err := p.Set(s.k, s.v); err != nil {
			t.Fatalf("%s=%s: %v", s.k, s.v, err)
		}
	}
	if p.Ctx != 16384 || p.Layout.TS != "33,15" || p.Spec.Type != "none" || strings.Join(p.Workers, ",") != "papa,victor" ||
		p.Env.Master["GGML_RING_X"] != "1" || p.WorkerModels["victor"] != "/data/o.gguf" || p.Layout.WorkerCPUExperts["papa"] != "36-40" ||
		strings.Join(p.ExtraMasterArgs, " ") != "--temp 0" || p.ExitOnFail || !p.Force || p.Cache.K != "" || p.Cache.V != "q8_0" || p.Notes != "123" || len(p.Runs) != 1 {
		t.Fatalf("%+v", p)
	}
	// a list key given a single value, also after the key was removed
	if err := p.Set("workers", ""); err != nil {
		t.Fatal(err)
	}
	if err := p.Set("workers", "papa"); err != nil || strings.Join(p.Workers, ",") != "papa" {
		t.Fatalf("workers=papa: %v %v", err, p.Workers)
	}
	for _, bad := range []struct{ k, v string }{{"ctxx", "1"}, {"ctx", "many"}, {"runs", "x"}, {"name", "y"}, {"layout.mode", "maybe"}, {"model", ""}} {
		if err := p.Set(bad.k, bad.v); err == nil {
			t.Errorf("%s=%s must be refused", bad.k, bad.v)
		}
	}
}

func TestExportImport(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	p, _ := Parse([]byte(ornithYAML))
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	b, _ := p.Marshal()
	f := filepath.Join(t.TempDir(), "x.yaml")
	os.WriteFile(f, b, 0o644)
	if _, err := s.Import(f, "", false); err == nil {
		t.Error("an existing profile is only replaced with force")
	}
	q, err := s.Import(f, "copy", false)
	if err != nil || q.Name != "copy" {
		t.Fatal(err)
	}
	got, _ := s.Load("copy")
	if got.Layout.TS != "34,14" || got.Master != "dino" {
		t.Fatalf("%+v", got)
	}
	if _, err := s.Import(f, "ornith-hub", true); err != nil {
		t.Fatal(err)
	}
}

func TestMeasuredLine(t *testing.T) {
	p := &Profile{Name: "x", Model: "m"}
	if p.MeasuredLine() != "" {
		t.Error("nothing measured")
	}
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	p.AddRun(Run{At: at, Layout: RunLayout{Master: "dino", Workers: []string{"papa"}, TS: "42,6", SpecType: "draft-dflash"}, Measured: &Measurement{At: at, TPS: 33.3}})
	if l := p.MeasuredLine(); !strings.Contains(l, "last measured on 2026-10-30: 33.3 t/s") || !strings.Contains(l, "-ts 42,6") {
		t.Error(l)
	}
}

// Fixtures measured on Ornith-122B over the hub (dino master + papa): GPU-only 6 blocks on papa 23.5 t/s without
// speculation, 33.3 with DFlash; papa GPU + CPU experts with 14 blocks 14.4 / 20.4. They are test data only.
func ornithFixture() (plan.Result, *Profile) {
	gpu := plan.Candidate{Kind: "split", Master: "dino", TPS: 21, Parts: []plan.Part{{Node: "dino", Layers: 42, First: 0, Output: true}, {Node: "papa", Layers: 6, First: 42}}}
	cpx := plan.Candidate{Kind: "split", Master: "dino", TPS: 25, Parts: []plan.Part{{Node: "dino", Layers: 34, First: 0, Output: true}, {Node: "papa", Layers: 14, First: 34, CPUExperts: 7}}}
	r := plan.Result{Candidates: []plan.Candidate{cpx, gpu}}
	p := &Profile{Name: "ornith", Model: "/m/o.gguf", Master: "dino", Workers: []string{"papa"}, Spec: Spec{Type: "draft-dflash", DraftMax: 2}}
	return r, p
}

func measuredRun(c plan.Candidate, spec string, tps float64, day int) Run {
	at := time.Date(2026, 10, day, 0, 0, 0, 0, time.UTC)
	l := CandidateLayout(c, spec, 2, []string{"papa"})
	return Run{At: at, Layout: l, Measured: &Measurement{At: at, TPS: tps}}
}

func TestChooseAutoRules(t *testing.T) {
	r, p := ornithFixture()
	cpx, gpu := r.Candidates[0], r.Candidates[1]

	// nothing measured: the planner's best GPU-only candidate, even though the CPU-expert one is estimated faster
	ch, err := Choose(r, p, false, []string{"papa"})
	if err != nil || ch.Candidate == nil || usesCPUExperts(*ch.Candidate) || !strings.HasPrefix(ch.Reason, "planned") {
		t.Fatalf("%+v %v", ch, err)
	}
	// only CPU-expert candidates fit: that one
	only := plan.Result{Candidates: []plan.Candidate{cpx}}
	if ch, _ := Choose(only, p, false, nil); ch.Candidate == nil || !usesCPUExperts(*ch.Candidate) {
		t.Fatalf("%+v", ch)
	}

	// measured: the best measured layout with the same speculation wins over the plan
	p.Runs = []Run{measuredRun(gpu, "none", 23.5, 28), measuredRun(gpu, "draft-dflash", 33.3, 29), measuredRun(cpx, "none", 14.4, 30), measuredRun(cpx, "draft-dflash", 20.4, 31)}
	ch, _ = Choose(r, p, false, []string{"papa"})
	if ch.Measured == nil || ch.Measured.Measured.TPS != 33.3 || ch.Measured.Layout.TS != "42,6" {
		t.Fatalf("best measured DFlash layout expected: %+v", ch)
	}
	p.Spec.Type = "none"
	if ch, _ = Choose(r, p, false, nil); ch.Measured == nil || ch.Measured.Measured.TPS != 23.5 {
		t.Fatalf("without speculation the 23.5 run counts: %+v", ch)
	}
	p.Spec.Type = "draft-dflash"

	// no longer feasible (the planner has no candidate on those machines now): the plan decides
	other := plan.Result{Candidates: []plan.Candidate{{Kind: "single", Master: "dino", TPS: 10, Parts: []plan.Part{{Node: "dino", Layers: 48}}}}}
	if ch, _ = Choose(other, p, false, nil); ch.Candidate == nil || ch.Candidate.Kind != "single" {
		t.Fatalf("%+v", ch)
	}

	// explore: the next candidate without a measurement
	p.Runs = []Run{measuredRun(gpu, "draft-dflash", 33.3, 29)}
	ch, _ = Choose(r, p, true, []string{"papa"})
	if ch.Candidate == nil || !usesCPUExperts(*ch.Candidate) || !strings.HasPrefix(ch.Reason, "explore") {
		t.Fatalf("explore must pick the untested CPU-expert layout: %+v", ch)
	}
	p.Runs = append(p.Runs, measuredRun(cpx, "draft-dflash", 20.4, 31))
	if ch, _ = Choose(r, p, true, []string{"papa"}); ch.Measured == nil || ch.Measured.Measured.TPS != 33.3 {
		t.Fatalf("everything measured: the best measured: %+v", ch)
	}
	if _, err := Choose(plan.Result{Verdict: "does not fit"}, p, false, nil); err == nil {
		t.Error("no candidate: an error")
	}
}

func TestCalibrationFromMeasuredTrace(t *testing.T) {
	r, p := ornithFixture()
	gpu := r.Candidates[1]
	gpu.TokenMS, gpu.MasterLocalMS = 40, 20
	r.Candidates[1] = gpu
	run := measuredRun(gpu, "none", 23.5, 28)
	run.Measured.Trace = &plan.Measured{TokenMS: 42.5, LocalMS: 22, RingMS: 20.5}
	p.Runs = []Run{run}
	k, used, ok := CalibrationFor(r, p, 0)
	if !ok || used == nil || k.FixedMS <= plan.DefaultFixedMS {
		t.Fatalf("%+v %v", k, ok)
	}
}

func TestProfileEngine_RoundTripAndValidation(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "profiles")}

	// 1. Validation accepts empty, ring, and chain
	pRing := &Profile{Name: "prof-ring", Model: "/m/r.gguf", Engine: "ring"}
	if err := pRing.Validate(); err != nil {
		t.Fatalf("expected ring engine to be valid, got: %v", err)
	}
	pChain := &Profile{Name: "prof-chain", Model: "/m/c.gguf", Engine: "chain"}
	if err := pChain.Validate(); err != nil {
		t.Fatalf("expected chain engine to be valid, got: %v", err)
	}
	pEmpty := &Profile{Name: "prof-empty", Model: "/m/e.gguf", Engine: ""}
	if err := pEmpty.Validate(); err != nil {
		t.Fatalf("expected empty engine to be valid, got: %v", err)
	}

	// 2. Validation rejects invalid engine
	pInvalid := &Profile{Name: "prof-invalid", Model: "/m/i.gguf", Engine: "unknown-engine"}
	if err := pInvalid.Validate(); err == nil {
		t.Fatalf("expected validation error for invalid engine, got nil")
	}

	// 3. Save & Load round-trip preserves Engine field
	if err := s.Save(pChain); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.Load("prof-chain")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Engine != "chain" {
		t.Fatalf("expected loaded engine to be 'chain', got %q", loaded.Engine)
	}

	// 4. YAML round-trip
	yamlBytes, err := loaded.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(yamlBytes)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Engine != "chain" {
		t.Fatalf("expected parsed YAML engine to be 'chain', got %q", parsed.Engine)
	}
}

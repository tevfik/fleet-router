package profile

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/plan"
)

func TestCandidate_RebuildAndErrors(t *testing.T) {
	// 1. Success case with CPUExperts and OutputOnMaster = true
	layoutMasterOutput := RunLayout{
		FromPlan:       true,
		Master:         "dino",
		Workers:        []string{"vec"},
		TS:             "20,10",
		CPUExperts:     map[string]string{"vec": "25-29"}, // 20..29 are blocks for vec, last 5 blocks are 25-29
		OutputOnMaster: true,
	}
	cand, err := layoutMasterOutput.Candidate()
	if err != nil {
		t.Fatalf("unexpected error from Candidate: %v", err)
	}
	if len(cand.Parts) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(cand.Parts))
	}
	if !cand.Parts[0].Output {
		t.Errorf("expected master part to have Output=true")
	}
	if cand.Parts[1].CPUExperts != 5 {
		t.Errorf("expected worker CPUExperts=5, got %d", cand.Parts[1].CPUExperts)
	}

	// 2. Success case with OutputOnMaster = false (output on last worker)
	layoutWorkerOutput := RunLayout{
		FromPlan:       true,
		Master:         "dino",
		Workers:        []string{"vec"},
		TS:             "10,10",
		OutputOnMaster: false,
	}
	cand2, err := layoutWorkerOutput.Candidate()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cand2.Parts[1].Output {
		t.Errorf("expected worker part to have Output=true")
	}

	// 3. Error: TS count mismatch
	badTSCount := RunLayout{
		Master:  "dino",
		Workers: []string{"vec"},
		TS:      "10", // 1 entry but 2 machines
	}
	if _, err := badTSCount.Candidate(); err == nil {
		t.Error("expected error when TS count does not match machines, got nil")
	}

	// 4. Error: TS integer parse error or negative
	badTSVal := RunLayout{
		Master:  "dino",
		Workers: []string{"vec"},
		TS:      "10,invalid",
	}
	if _, err := badTSVal.Candidate(); err == nil {
		t.Error("expected error for non-integer TS, got nil")
	}

	// 5. Error: CPUExperts invalid range syntax
	badCPURange := RunLayout{
		Master:     "dino",
		Workers:    []string{"vec"},
		TS:         "10,10",
		CPUExperts: map[string]string{"vec": "invalid-range"},
	}
	if _, err := badCPURange.Candidate(); err == nil {
		t.Error("expected error for invalid CPU experts range, got nil")
	}

	// 6. Error: CPUExperts not on last blocks
	notLastBlocks := RunLayout{
		Master:     "dino",
		Workers:    []string{"vec"},
		TS:         "10,10",                           // vec has blocks 10..19
		CPUExperts: map[string]string{"vec": "10-15"}, // not ending at 19
	}
	if _, err := notLastBlocks.Candidate(); err == nil {
		t.Error("expected error when CPU experts are not last blocks, got nil")
	}
}

func TestDirAndDefault(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("FLEET_PROFILES", tmpDir)
	if d := Dir(); d != tmpDir {
		t.Errorf("expected Dir() to return FLEET_PROFILES %q, got %q", tmpDir, d)
	}

	defStore := Default()
	if defStore.Dir != tmpDir {
		t.Errorf("expected Default().Dir to be %q, got %q", tmpDir, defStore.Dir)
	}

	// Unset FLEET_PROFILES to hit fallback path
	t.Setenv("FLEET_PROFILES", "")
	t.Setenv("FLEET_CONFIG", filepath.Join(tmpDir, "config.yaml"))
	dFallback := Dir()
	expected := filepath.Join(tmpDir, "profiles")
	if dFallback != expected {
		t.Errorf("expected fallback %q, got %q", expected, dFallback)
	}
}

func TestStore_SaveRemoveListAndAtomicWriteErrors(t *testing.T) {
	tmpDir := t.TempDir()
	s := Store{Dir: filepath.Join(tmpDir, "profiles")}

	// 1. List on non-existent directory returns nil, nil, nil
	list, bad, err := s.List()
	if err != nil || list != nil || bad != nil {
		t.Fatalf("expected nil list from missing dir, got %v, %v, %v", list, bad, err)
	}

	// 2. Save invalid name
	pBadName := &Profile{Name: "bad/name", Model: "test"}
	if err := s.Save(pBadName); err == nil {
		t.Error("expected error saving profile with bad name")
	}

	// 3. Save invalid profile validation (e.g. empty model)
	pBadVal := &Profile{Name: "goodname", Model: ""}
	if err := s.Save(pBadVal); err == nil {
		t.Error("expected validation error for empty model")
	}

	// 4. Save success
	pValid := &Profile{Name: "valid-prof", Model: "llama-3-8b"}
	if err := s.Save(pValid); err != nil {
		t.Fatalf("failed to save valid profile: %v", err)
	}

	// 5. List with valid and corrupted yaml files
	corruptFile := filepath.Join(s.Dir, "corrupted.yaml")
	if err := os.WriteFile(corruptFile, []byte("bad: [yaml"), 0644); err != nil {
		t.Fatal(err)
	}
	// Also ignore non-yaml file and directory
	if err := os.WriteFile(filepath.Join(s.Dir, "ignored.txt"), []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(s.Dir, "subdir.yaml"), 0755); err != nil {
		t.Fatal(err)
	}

	list, bad, err = s.List()
	if err != nil {
		t.Fatalf("List error: %v", err)
	}
	if len(list) != 1 || list[0].Name != "valid-prof" {
		t.Errorf("expected 1 valid profile, got %d", len(list))
	}
	if bad == nil || bad["corrupted"] == nil {
		t.Errorf("expected bad entry for corrupted profile, got %v", bad)
	}

	// 6. Remove errors: bad name, not found
	if err := s.Remove("bad/name"); err == nil {
		t.Error("expected error removing invalid name")
	}
	if err := s.Remove("nonexistent"); err == nil {
		t.Error("expected error removing non-existent profile")
	}
	// Remove success
	if err := s.Remove("valid-prof"); err != nil {
		t.Fatalf("failed to remove profile: %v", err)
	}
}

func TestStore_Import(t *testing.T) {
	tmpDir := t.TempDir()
	s := Store{Dir: filepath.Join(tmpDir, "profiles")}

	// Write profile yaml
	srcPath := filepath.Join(tmpDir, "source.yaml")
	yamlContent := "name: orig-name\nmodel: qwen-72b\n"
	if err := os.WriteFile(srcPath, []byte(yamlContent), 0644); err != nil {
		t.Fatal(err)
	}

	// 1. Missing file
	if _, err := s.Import("non-existent-file.yaml", "", false); err == nil {
		t.Error("expected error importing missing file")
	}

	// 2. Corrupted file
	corruptPath := filepath.Join(tmpDir, "bad.yaml")
	_ = os.WriteFile(corruptPath, []byte("broken: ["), 0644)
	if _, err := s.Import(corruptPath, "", false); err == nil {
		t.Error("expected error importing broken yaml")
	}

	// 3. File without name and no name override
	noNamePath := filepath.Join(tmpDir, "noname.yaml")
	_ = os.WriteFile(noNamePath, []byte("model: llama\n"), 0644)
	if _, err := s.Import(noNamePath, "", false); err == nil {
		t.Error("expected error importing file without name")
	}

	// 4. Successful import with name override
	p, err := s.Import(srcPath, "custom-name", false)
	if err != nil {
		t.Fatalf("import failed: %v", err)
	}
	if p.Name != "custom-name" {
		t.Errorf("expected name custom-name, got %s", p.Name)
	}

	// 5. Existing file without force fails
	if _, err := s.Import(srcPath, "custom-name", false); err == nil {
		t.Error("expected error importing existing without force")
	}

	// 6. Existing file with force succeeds
	if _, err := s.Import(srcPath, "custom-name", true); err != nil {
		t.Errorf("expected force import to succeed, got %v", err)
	}
}

func TestProfile_AddRun_MaxRunsLimit(t *testing.T) {
	p := &Profile{Name: "test-prof", Model: "test"}
	// Add 55 runs, some unmeasured, some measured
	for i := 0; i < 55; i++ {
		r := Run{
			At: time.Now(),
			Layout: RunLayout{
				Master: fmt.Sprintf("m%d", i),
			},
		}
		if i >= 10 { // first 10 unmeasured, rest measured
			r.Measured = &Measurement{TPS: float64(i)}
		}
		p.AddRun(r)
	}

	if len(p.Runs) != maxRuns {
		t.Errorf("expected maxRuns=%d, got %d", maxRuns, len(p.Runs))
	}
}

func TestProfile_Set_Cases(t *testing.T) {
	p := &Profile{
		Name:  "test-prof",
		Model: "llama",
		Layout: Layout{
			Mode: LayoutFixed,
			TS:   "10,10",
		},
		Spec: Spec{
			Type: "ngram",
		},
	}

	// 1. Error: empty key
	if err := p.Set("", "foo"); err == nil {
		t.Error("expected error on empty key")
	}

	// 2. Error: setting name or runs
	if err := p.Set("name", "new"); err == nil {
		t.Error("expected error setting name")
	}
	if err := p.Set("runs", "new"); err == nil {
		t.Error("expected error setting runs")
	}
	if err := p.Set("runs.0", "new"); err == nil {
		t.Error("expected error setting runs.0")
	}

	// 3. Error: traversing through non-section scalar
	if err := p.Set("model.invalid.sub", "val"); err == nil {
		t.Error("expected error when setting child of scalar")
	}

	// 4. Value = "" removes key
	if err := p.Set("layout.ts", ""); err != nil {
		t.Fatalf("failed to delete layout.ts: %v", err)
	}
	if p.Layout.TS != "" {
		t.Errorf("expected empty layout.ts after delete, got %q", p.Layout.TS)
	}

	// 5. extra_master_args
	if err := p.Set("extra_master_args", "--temp 0.7 --ctx 4096"); err != nil {
		t.Fatalf("failed to set extra_master_args: %v", err)
	}
	if len(p.ExtraMasterArgs) != 4 {
		t.Errorf("expected 4 extra_master_args, got %v", p.ExtraMasterArgs)
	}

	// 6. List with comma-separated string
	if err := p.Set("workers", "vec,dino"); err != nil {
		t.Fatalf("failed to set comma-separated workers: %v", err)
	}
	if len(p.Workers) != 2 || p.Workers[0] != "vec" || p.Workers[1] != "dino" {
		t.Errorf("unexpected workers: %v", p.Workers)
	}

	// 7. List with single value fallback
	if err := p.Set("workers", "papa"); err != nil {
		t.Fatalf("failed to set single worker: %v", err)
	}
	if len(p.Workers) != 1 || p.Workers[0] != "papa" {
		t.Errorf("unexpected workers: %v", p.Workers)
	}
}

func TestChoose_NoCandidatesAndSpec(t *testing.T) {
	p := &Profile{
		Name:  "test-prof",
		Model: "llama",
	}

	// Empty candidates in plan.Result
	emptyRes := plan.Result{
		Verdict: "no resources",
	}
	_, err := Choose(emptyRes, p, false, nil)
	if err == nil {
		t.Error("expected error when Result has no candidates")
	}

	// sameSpec verification
	r1 := Run{Layout: RunLayout{SpecType: "auto", DraftMax: 0}}
	if !sameSpec(r1, "", 0) {
		t.Error("expected sameSpec to be true for auto and empty")
	}
	if sameSpec(r1, "ngram", 0) {
		t.Error("expected sameSpec to be false for auto vs ngram")
	}
}

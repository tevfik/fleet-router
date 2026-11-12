package eval

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFeature_Scores_EmptyResultIsZero(t *testing.T) {
	var r Result
	if r.Score() != 0 || r.CheckScore() != 0 || r.AvgSeconds() != 0 || len(r.ByCategory()) != 0 {
		t.Fatal("an empty result must score 0 everywhere")
	}
	r.Items = []ItemResult{{Category: "x"}} // items without checks
	if r.CheckScore() != 0 {
		t.Fatal("no checks: check score 0")
	}
}

func TestFeature_Save_SkipsEmptyResultsAndReportsErrors(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "deep", "eval.json")
	ok := Result{Target: "good", Items: []ItemResult{{Category: CatFormat, Pass: true, Passed: 1, Total: 1, Seconds: 2}}}
	if err := Save(p, []Result{ok, {Target: "empty"}}); err != nil {
		t.Fatal(err)
	}
	m := Load(p)
	if _, has := m["empty"]; has || m["good"].Score != 1 {
		t.Fatalf("stored = %+v", m)
	}
	if _, err := os.Stat(p + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temporary file left behind")
	}
	// the parent is a file: saving fails instead of silently dropping the scores
	blocker := filepath.Join(dir, "file")
	os.WriteFile(blocker, nil, 0o600)
	if err := Save(filepath.Join(blocker, "eval.json"), []Result{ok}); err == nil {
		t.Fatal("expected an error")
	}
	// a corrupt file reads as empty and is replaced on the next save
	os.WriteFile(p, []byte("{nope"), 0o600)
	if len(Load(p)) != 0 {
		t.Fatal("corrupt file should load empty")
	}
}

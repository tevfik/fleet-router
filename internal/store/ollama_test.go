package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An Ollama directory: the manifest says which blob is the model and which the projector, and the blobs are plain GGUFs.
func ollamaDir(t *testing.T) (dir, model, proj string) {
	t.Helper()
	dir = t.TempDir()
	model = ggufFile(t, filepath.Join(dir, "blobs"), "sha256-aaaa", 1*mib, 1*mib)
	proj = ggufFile(t, filepath.Join(dir, "blobs"), "sha256-bbbb", 2*mib)
	mf := filepath.Join(dir, "manifests", "registry.ollama.ai", "library", "qwen3-vl", "8b")
	os.MkdirAll(filepath.Dir(mf), 0o755)
	os.WriteFile(mf, []byte(`{"layers":[
	 {"mediaType":"application/vnd.ollama.image.model","digest":"sha256:aaaa"},
	 {"mediaType":"application/vnd.ollama.image.projector","digest":"sha256:bbbb"},
	 {"mediaType":"application/vnd.ollama.image.template","digest":"sha256:cccc"}]}`), 0o644)
	mf2 := filepath.Join(dir, "manifests", "registry.ollama.ai", "someone", "tiny", "latest")
	os.MkdirAll(filepath.Dir(mf2), 0o755)
	os.WriteFile(mf2, []byte(`{"layers":[{"mediaType":"application/vnd.ollama.image.model","digest":"sha256:aaaa"}]}`), 0o644)
	return
}

func TestScanOllama(t *testing.T) {
	dir, model, proj := ollamaDir(t)
	if !IsOllamaDir(dir) || IsOllamaDir(filepath.Join(dir, "blobs")) {
		t.Fatal("IsOllamaDir")
	}
	ms, err := ScanOllama(dir)
	if err != nil || len(ms) != 2 {
		t.Fatalf("%v %v", ms, err)
	}
	byName := map[string]OllamaModel{}
	for _, m := range ms {
		byName[m.Name] = m
	}
	q := byName["qwen3-vl:8b"]
	if q.Model != model || q.Projector != proj || q.FileName(false) != "qwen3-vl-8b.gguf" || q.FileName(true) != "qwen3-vl-8b.mmproj.gguf" {
		t.Fatalf("%+v", q)
	}
	if !strings.Contains(q.Describe(), "--mmproj") || byName["someone/tiny:latest"].Describe() != "" {
		t.Fatalf("a model with a projector is warned about, one without is not: %q", q.Describe())
	}
}

// A blob is registered under its manifest's name; a projector is told apart by its architecture (clip), not its name.
func TestAddNamedAndKind(t *testing.T) {
	dir := t.TempDir()
	s := Open(filepath.Join(dir, "store"))
	p := ggufFile(t, dir, "sha256-1234", 1*mib)
	e, err := s.AddNamed(context.Background(), p, "qwen3-8b.gguf")
	if err != nil || e.Name != "qwen3-8b.gguf" || e.Kind != "" {
		t.Fatalf("%+v %v", e, err)
	}
	inv := s.Inventory("")
	if len(inv.Files) != 1 || inv.Files[0].Name != "qwen3-8b.gguf" {
		t.Fatalf("%+v", inv.Files)
	}
	e2 := Entry{}
	m := &Manifest{Arch: "clip", Name: "x"}
	if e2 = entryOf(m, true); e2.Kind != "mmproj" {
		t.Fatalf("a clip GGUF is a projector: %+v", e2)
	}
}

func TestPruneAndDeleteFile(t *testing.T) {
	dir := t.TempDir()
	s := Open(filepath.Join(dir, "store"))
	files := filepath.Join(dir, "store", "files")
	fetched := ggufFile(t, files, "a.gguf", 1*mib)
	mine := ggufFile(t, filepath.Join(dir, "mine"), "b.gguf", 2*mib)
	ctx := context.Background()
	if _, err := s.Add(ctx, fetched); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(ctx, mine); err != nil {
		t.Fatal(err)
	}
	// a file the user keeps elsewhere is never deleted
	if _, _, err := s.DeleteFile("b.gguf", files); err == nil || !strings.Contains(err.Error(), "only deletes what it fetched") {
		t.Fatalf("%v", err)
	}
	if _, err := os.Stat(mine); err != nil {
		t.Fatal("the user's file must be untouched")
	}
	if _, path, err := s.DeleteFile("a.gguf", files); err != nil || path != fetched {
		t.Fatal(path, err)
	}
	if _, err := os.Stat(fetched); err == nil {
		t.Fatal("the fetched file must be gone")
	}
	os.Remove(mine) // gone behind fleet's back
	gone, err := s.Prune()
	if err != nil || len(gone) != 1 || gone[0].Name != "b" && gone[0].Name != "b.gguf" {
		t.Fatalf("%+v %v", gone, err)
	}
	if len(s.Inventory("").Files) != 0 {
		t.Fatal("nothing left to list")
	}
}

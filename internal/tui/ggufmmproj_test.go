package tui

import (
	"os"
	"path/filepath"
	"testing"
)

// The file picker offers models only: a vision projector next to them is not something to plan.
func TestGGUFFilesSkipsMMProj(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(t.TempDir())
	dir := filepath.Join(home, "models")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"DeepSeek-V4.1-Flash-IQ2.gguf", "mmproj-DeepSeek-V4.1-Flash-BF16.gguf"} {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := ggufFiles()
	if len(got) != 1 || filepath.Base(got[0]) != "DeepSeek-V4.1-Flash-IQ2.gguf" {
		t.Fatalf("ggufFiles = %v, want only the model", got)
	}
}

package ringengine

import (
	"os"
	"path/filepath"
	"testing"
)

// FindChainPath looks in installed places only: a developer's build tree or an old fleet copy named fleet-engine in
// ~/.local/bin (papa had one) is not a stage binary.
func TestFindChainPathInstalledOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())
	touch := func(p string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	touch(filepath.Join(home, "agent_ws", "projects", "fleet-engine", "build", "bin", "fleet-engine-stage"))
	touch(filepath.Join(home, ".local", "bin", "fleet-engine"))
	if p, ok := FindChainPath(); ok {
		t.Fatalf("found %s: not an installed stage binary", p)
	}
	want := filepath.Join(home, ".local", "bin", "fleet-engine-stage")
	touch(want)
	if p, ok := FindChainPath(); !ok || p != want {
		t.Fatalf("installed link not found: %q %v", p, ok)
	}
}

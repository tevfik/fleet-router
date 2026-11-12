package ringengine

import (
	"os"
	"path/filepath"
	"testing"
)

func touchFile(t *testing.T, p string, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("touchFile mkdir %s: %v", filepath.Dir(p), err)
	}
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), perm); err != nil {
		t.Fatalf("touchFile write %s: %v", p, err)
	}
}

func TestFindEnginePath(t *testing.T) {
	t.Run("home candidates", func(t *testing.T) {
		candidates := []string{
			filepath.Join("agent_ws", "projects", "fleet-engine", "build", "bin", "llama-server"),
			filepath.Join("llm-cluster", "llama-ring", "build", "bin", "llama-server"),
			filepath.Join("agent_ws", "projects", "llama-ring", "build", "bin", "llama-server"),
			filepath.Join("agent_ws", "project", "llama-ring", "build", "bin", "llama-server"),
			filepath.Join("agent_ws", "projects", "fleet", "fleet-engine"),
			filepath.Join(".local", "bin", "fleet-engine"),
			filepath.Join(".local", "bin", "llama-server"),
		}
		for _, rel := range candidates {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("PATH", t.TempDir())
			full := filepath.Join(home, rel)
			touchFile(t, full, 0o755)

			got, ok := FindEnginePath()
			if !ok || got != full {
				t.Errorf("FindEnginePath() for %s = (%q, %v), want (%q, true)", rel, got, ok, full)
			}
		}
	})

	t.Run("lookpath fallback", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)

		binDir := t.TempDir()
		t.Setenv("PATH", binDir)

		// 1. fleet-engine in PATH
		fe := filepath.Join(binDir, "fleet-engine")
		touchFile(t, fe, 0o755)
		if got, ok := FindEnginePath(); !ok || got != fe {
			t.Errorf("expected to find %s in PATH, got (%q, %v)", fe, got, ok)
		}

		// 2. llama-server in PATH
		if err := os.Remove(fe); err != nil {
			t.Fatal(err)
		}
		ls := filepath.Join(binDir, "llama-server")
		touchFile(t, ls, 0o755)
		if got, ok := FindEnginePath(); !ok || got != ls {
			t.Errorf("expected to find %s in PATH, got (%q, %v)", ls, got, ok)
		}
	})

	t.Run("executable dir fallback", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("PATH", t.TempDir())

		exe, err := os.Executable()
		if err != nil {
			t.Skipf("cannot get executable: %v", err)
		}
		exeDir := filepath.Dir(exe)
		target := filepath.Join(exeDir, "fleet-engine")
		if err := os.WriteFile(target, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Skipf("executable dir %s is not writable: %v", exeDir, err)
		}
		defer os.Remove(target)

		if got, ok := FindEnginePath(); !ok || got != target {
			t.Errorf("expected to find %s next to exe, got (%q, %v)", target, got, ok)
		}
	})

	t.Run("not found", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("PATH", t.TempDir())

		exe, err := os.Executable()
		if err == nil {
			target := filepath.Join(filepath.Dir(exe), "fleet-engine")
			_ = os.Remove(target)
		}

		if got, ok := FindEnginePath(); ok {
			t.Errorf("expected not found, got (%q, %v)", got, ok)
		}
	})
}

func TestFindWorkerPath(t *testing.T) {
	t.Run("home candidates", func(t *testing.T) {
		candidates := []string{
			filepath.Join("agent_ws", "projects", "fleet-engine", "build", "bin", "ring-worker"),
			filepath.Join("llm-cluster", "llama-ring", "build", "bin", "ring-worker"),
			filepath.Join("agent_ws", "projects", "llama-ring", "build", "bin", "ring-worker"),
			filepath.Join("agent_ws", "project", "llama-ring", "build", "bin", "ring-worker"),
			filepath.Join(".local", "bin", "ring-worker"),
		}
		for _, rel := range candidates {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("PATH", t.TempDir())
			full := filepath.Join(home, rel)
			touchFile(t, full, 0o755)

			got, ok := FindWorkerPath()
			if !ok || got != full {
				t.Errorf("FindWorkerPath() for %s = (%q, %v), want (%q, true)", rel, got, ok, full)
			}
		}
	})

	t.Run("lookpath fallback", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		binDir := t.TempDir()
		t.Setenv("PATH", binDir)

		rw := filepath.Join(binDir, "ring-worker")
		touchFile(t, rw, 0o755)
		if got, ok := FindWorkerPath(); !ok || got != rw {
			t.Errorf("expected to find %s in PATH, got (%q, %v)", rw, got, ok)
		}
	})

	t.Run("not found", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("PATH", t.TempDir())

		if got, ok := FindWorkerPath(); ok {
			t.Errorf("expected not found, got (%q, %v)", got, ok)
		}
	})
}

func TestFindChainPathExtended(t *testing.T) {
	t.Run("lookpath fleet-engine-stage", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		binDir := t.TempDir()
		t.Setenv("PATH", binDir)

		target := filepath.Join(binDir, "fleet-engine-stage")
		touchFile(t, target, 0o755)
		if got, ok := FindChainPath(); !ok || got != target {
			t.Errorf("expected to find %s, got (%q, %v)", target, got, ok)
		}
	})

	t.Run("lookpath fleet-engine fallback", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		binDir := t.TempDir()
		t.Setenv("PATH", binDir)

		target := filepath.Join(binDir, "fleet-engine")
		touchFile(t, target, 0o755)
		if got, ok := FindChainPath(); !ok || got != target {
			t.Errorf("expected to find %s, got (%q, %v)", target, got, ok)
		}
	})

	t.Run("executable dir fallback", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("PATH", t.TempDir())

		exe, err := os.Executable()
		if err != nil {
			t.Skipf("cannot get executable: %v", err)
		}
		exeDir := filepath.Dir(exe)
		target := filepath.Join(exeDir, "fleet-engine-stage")
		if err := os.WriteFile(target, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Skipf("executable dir %s is not writable: %v", exeDir, err)
		}
		defer os.Remove(target)

		if got, ok := FindChainPath(); !ok || got != target {
			t.Errorf("expected to find %s next to exe, got (%q, %v)", target, got, ok)
		}
	})

	t.Run("not found", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("PATH", t.TempDir())

		exe, err := os.Executable()
		if err == nil {
			_ = os.Remove(filepath.Join(filepath.Dir(exe), "fleet-engine-stage"))
			_ = os.Remove(filepath.Join(filepath.Dir(exe), "fleet-engine"))
		}

		if got, ok := FindChainPath(); ok {
			t.Errorf("expected not found, got (%q, %v)", got, ok)
		}
	})
}

func TestFindBackendPath(t *testing.T) {
	t.Run("beside engine", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("PATH", t.TempDir())

		engineDir := filepath.Join(home, ".local", "bin")
		engine := filepath.Join(engineDir, "fleet-engine")
		touchFile(t, engine, 0o755)

		so := filepath.Join(engineDir, "libggml-ring.so")
		touchFile(t, so, 0o644)

		if got, ok := FindBackendPath(); !ok || got != so {
			t.Errorf("FindBackendPath() = (%q, %v), want (%q, true)", got, ok, so)
		}
	})

	t.Run("home candidates", func(t *testing.T) {
		candidates := []string{
			filepath.Join("llm-cluster", "llama-ring", "build", "bin", "libggml-ring.so"),
			filepath.Join("agent_ws", "projects", "llama-ring", "build", "bin", "libggml-ring.so"),
			filepath.Join("agent_ws", "project", "llama-ring", "build", "bin", "libggml-ring.so"),
			filepath.Join(".local", "bin", "libggml-ring.so"),
			filepath.Join(".local", "lib", "libggml-ring.so"),
		}
		for _, rel := range candidates {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("PATH", t.TempDir())

			full := filepath.Join(home, rel)
			touchFile(t, full, 0o644)

			got, ok := FindBackendPath()
			if !ok || got != full {
				t.Errorf("FindBackendPath() for %s = (%q, %v), want (%q, true)", rel, got, ok, full)
			}
		}
	})

	t.Run("not found", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("PATH", t.TempDir())

		if got, ok := FindBackendPath(); ok {
			t.Errorf("expected not found, got (%q, %v)", got, ok)
		}
	})
}

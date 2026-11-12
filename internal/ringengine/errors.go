package ringengine

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
)

// ErrNotBuilt: this binary has no llama-ring engine linked in (build it with `make build-engine`).
var ErrNotBuilt = errors.New("this fleet was built without the llama-ring engine — `make build-engine` builds one that has it")

// FindEnginePath searches for a standalone fleet-engine binary or llama-server if available.
func FindEnginePath() (string, bool) {
	// 1. check home directory standard build paths
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates := []string{
			filepath.Join(home, "agent_ws", "projects", "fleet-engine", "build", "bin", "llama-server"),
			filepath.Join(home, "llm-cluster", "llama-ring", "build", "bin", "llama-server"),
			filepath.Join(home, "agent_ws", "projects", "llama-ring", "build", "bin", "llama-server"),
			filepath.Join(home, "agent_ws", "project", "llama-ring", "build", "bin", "llama-server"),
			filepath.Join(home, "agent_ws", "projects", "fleet", "fleet-engine"),
			filepath.Join(home, ".local", "bin", "fleet-engine"),
			filepath.Join(home, ".local", "bin", "llama-server"),
		}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				return c, true
			}
		}
	}
	// 2. check PATH
	if p, err := exec.LookPath("fleet-engine"); err == nil {
		return p, true
	}
	if p, err := exec.LookPath("llama-server"); err == nil {
		return p, true
	}
	// 3. check next to current executable
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "fleet-engine")
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}
	return "", false
}

// FindWorkerPath searches for a standalone ring-worker binary if available.
func FindWorkerPath() (string, bool) {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates := []string{
			filepath.Join(home, "agent_ws", "projects", "fleet-engine", "build", "bin", "ring-worker"),
			filepath.Join(home, "llm-cluster", "llama-ring", "build", "bin", "ring-worker"),
			filepath.Join(home, "agent_ws", "projects", "llama-ring", "build", "bin", "ring-worker"),
			filepath.Join(home, "agent_ws", "project", "llama-ring", "build", "bin", "ring-worker"),
			filepath.Join(home, ".local", "bin", "ring-worker"),
		}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				return c, true
			}
		}
	}
	if p, err := exec.LookPath("ring-worker"); err == nil {
		return p, true
	}
	return "", false
}

// FindChainPath searches for a standalone fleet-engine-stage (or fleet-engine) binary for Protocol 2 (chain / FSP1).
func FindChainPath() (string, bool) {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		// installed places only: a developer's build tree goes on PATH (an old fleet copy named fleet-engine, or a stale
		// build directory, used to be picked up here)
		candidates := []string{
			filepath.Join(home, ".local", "bin", "fleet-engine-stage"),
		}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				return c, true
			}
		}
	}
	if p, err := exec.LookPath("fleet-engine-stage"); err == nil {
		return p, true
	}
	if p, err := exec.LookPath("fleet-engine"); err == nil {
		return p, true
	}
	if exe, err := os.Executable(); err == nil {
		for _, name := range []string{"fleet-engine-stage", "fleet-engine"} {
			p := filepath.Join(filepath.Dir(exe), name)
			if _, err := os.Stat(p); err == nil {
				return p, true
			}
		}
	}
	return "", false
}

// FindBackendPath searches for libggml-ring.so or the backend directory containing it.
func FindBackendPath() (string, bool) {
	if eng, ok := FindEnginePath(); ok {
		p := filepath.Join(filepath.Dir(eng), "libggml-ring.so")
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates := []string{
			filepath.Join(home, "llm-cluster", "llama-ring", "build", "bin", "libggml-ring.so"),
			filepath.Join(home, "agent_ws", "projects", "llama-ring", "build", "bin", "libggml-ring.so"),
			filepath.Join(home, "agent_ws", "project", "llama-ring", "build", "bin", "libggml-ring.so"),
			filepath.Join(home, ".local", "bin", "libggml-ring.so"),
			filepath.Join(home, ".local", "lib", "libggml-ring.so"),
		}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				return c, true
			}
		}
	}
	return "", false
}

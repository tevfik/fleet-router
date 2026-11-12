// Package feature answers which optional parts of fleet this machine has.
//
// fleet is first a router for model servers that already run (ollama, llama-server, vLLM…): that part is always there.
// The runner side — planning, deploying and supervising models over several machines (plan, deploy, runs, profiles,
// model files, ring/chain stages) — needs fleet-engine. Without it those menus, commands and admin routes are not
// shown at all: a fleet without the engine is just the router.
package feature

import (
	"os"
	"strings"
	"sync"

	"github.com/tevfik/fleet-router/internal/deploy"
	"github.com/tevfik/fleet-router/internal/ringengine"
)

// EnvRunner forces the answer: FLEET_RUNNER=1 shows the runner side, FLEET_RUNNER=0 hides it (e.g. to see what a
// machine without the engine sees).
const EnvRunner = "FLEET_RUNNER"

var (
	once     sync.Once
	detected bool

	mu       sync.RWMutex
	override *bool // tests and SetRunner
)

// Runner reports whether the runner side is available on this machine: fleet-engine linked into this binary, an
// installed fleet-engine stage binary, or a linked stage module (an adapter module, pkg/stagemod). FLEET_RUNNER
// overrides the detection.
func Runner() bool {
	mu.RLock()
	o := override
	mu.RUnlock()
	if o != nil {
		return *o
	}
	once.Do(func() { detected = detect() })
	return detected
}

func detect() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvRunner))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	if ringengine.Available() {
		return true
	}
	if len(deploy.StageModuleNames()) > 0 {
		return true
	}
	_, ok := ringengine.FindChainPath()
	return ok
}

// SetRunner fixes the answer (tests); the returned func restores the previous state.
func SetRunner(on bool) (restore func()) {
	mu.Lock()
	prev := override
	override = &on
	mu.Unlock()
	return func() {
		mu.Lock()
		override = prev
		mu.Unlock()
	}
}

// RunnerHint is what a command or screen of the runner side says when the runner is not available.
const RunnerHint = "this needs fleet-engine (the runner side: plan, deploy, runs, model files); this fleet only routes to model servers that already run"

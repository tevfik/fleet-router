package deploy

import (
	"fmt"
	"sort"
	"sync"
)

// A StageModule is a pluggable stage runtime for `fleet deploy --engine chain --proto <Name>`: the strings of its
// binaries (Adapter) and, when its machines are not reached over ssh, the runner that starts and watches the
// stages (NewRunner). The core knows only this type; a module registers itself from its own package's init, and
// main imports it in ONE file (modules_<name>.go, behind a build tag). Removing a module = delete its package and
// that file; `go build -tags no<name>` leaves it out without touching a file.
type StageModule struct {
	Name    string           // the --proto value
	Doc     string           // one line for the --proto help text
	Adapter StagePipeAdapter // names, ports and paths of the runtime's binaries
	// NewRunner returns what really starts the stages (nil: the ssh runner, LiveStageRunner).
	NewRunner func() (StageRunner, error)
	// Unsupported names options the runtime cannot do ("moe", "lanes", "np"...): the CLI refuses them up front.
	Unsupported []string
}

var (
	stageModulesMu sync.RWMutex
	stageModules   = map[string]StageModule{}
)

// RegisterStageModule adds a module; a second one with the same name is a programming error.
func RegisterStageModule(m StageModule) {
	stageModulesMu.Lock()
	defer stageModulesMu.Unlock()
	if m.Name == "" {
		panic("deploy: stage module without a name")
	}
	if _, dup := stageModules[m.Name]; dup {
		panic("deploy: stage module registered twice: " + m.Name)
	}
	stageModules[m.Name] = m
}

// LookupStageModule finds a registered module.
func LookupStageModule(name string) (StageModule, bool) {
	stageModulesMu.RLock()
	defer stageModulesMu.RUnlock()
	m, ok := stageModules[name]
	return m, ok
}

// StageModuleNames lists the registered modules, sorted (for usage and error texts).
func StageModuleNames() []string {
	stageModulesMu.RLock()
	defer stageModulesMu.RUnlock()
	names := make([]string, 0, len(stageModules))
	for n := range stageModules {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Refuses reports the first option of opts the module cannot do, or nil.
func (m StageModule) Refuses(opts map[string]bool) error {
	for _, u := range m.Unsupported {
		if opts[u] {
			return fmt.Errorf("--proto %s: --%s is not supported by this runtime", m.Name, u)
		}
	}
	return nil
}

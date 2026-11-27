package deploy

import (
	"fmt"
	"os"
	"strings"

	"github.com/tevfik/fleet-router/internal/ringengine"
)

// Where the master's engine is looked for, in ONE place: the script a remote master is started with, the dry run's
// description of it and the failure message all come from masterEngineDirs. (A local master resolves through
// ringengine.FindEnginePath, which walks the same directories.)

// legacyEngineDirsFirst keeps the pre-2026 search order (build directories before the installed engine-bearing
// fleet). A stale dev build used to shadow a newer installed engine — the master then died on "unknown model
// architecture" because the picked llama-server predated the model. On a dev machine that still wants its own
// build tree first, set FLEET_LEGACY_ENGINE_DIRS=1 when running fleet.
func legacyEngineDirsFirst() bool { return os.Getenv("FLEET_LEGACY_ENGINE_DIRS") == "1" }

// engineFleetTest is the shell condition for "the fleet on PATH carries an engine": a fleet with the engine linked
// in (every engine package build, `make build-engine`) answers `ring info` with the engine's JSON; a plain fleet
// fails or says nothing of the kind.
const engineFleetTest = `if FLEET_BIN=$(command -v fleet) && [ -n "$FLEET_BIN" ] && "$FLEET_BIN" ring info 2>/dev/null | grep -q '"engine"'; then
`

// masterEngineDir is a directory (shell form, $HOME expands on the master) that may hold a llama-server build.
type masterEngineDir struct {
	Dir string
	// BackendFrom is the directory whose libggml-ring.so is exported with this server ("": the same Dir). The
	// stock llama.cpp-rpc build has no ring backend of its own, so it borrows the llama-ring build's when it exists.
	BackendFrom string
	// BackendOptional: export the backend only when that file exists (else the server runs without it).
	BackendOptional bool
}

var masterEngineDirs = []masterEngineDir{
	{Dir: "$HOME/llm-cluster/llama-ring/build/bin"},
	{Dir: "$HOME/llama.cpp-rpc/build/bin", BackendFrom: "$HOME/llm-cluster/llama-ring/build/bin", BackendOptional: true},
	{Dir: "$HOME/agent_ws/projects/llama-ring/build/bin"},
	{Dir: "$HOME/agent_ws/project/llama-ring/build/bin"}, // one machine's checkout is "project" (workerCandidateDirs has it too)
}

func (d masterEngineDir) backendDir() string {
	if d.BackendFrom != "" {
		return d.BackendFrom
	}
	return d.Dir
}

// remoteMasterScript starts the master on a remote machine: finds the engine (the installed engine-bearing fleet
// first, then masterEngineDirs, else `fleet ring serve`, else a llama-server on PATH), starts it detached with its
// output in ~/.cache/fleet/runs/<runID>.log and checks it is still alive after a moment. Its answer: FLEET_NOBIN
// when no engine exists, else "FLEET_PID <pid>" (followed by FLEET_DIED and the log tail when it exited at once).
func remoteMasterScript(envPrefix, masterArgs, runID string) string {
	var b strings.Builder
	b.WriteString("export PATH=$HOME/.local/bin:$PATH\n")
	b.WriteString(envPrefix)
	b.WriteString("mkdir -p ~/.cache/fleet/runs\n")
	first := true
	if !legacyEngineDirsFirst() {
		b.WriteString(engineFleetTest)
		b.WriteString("    SERVER_BIN=\"$FLEET_BIN ring serve\"\n")
		first = false
	}
	for _, d := range masterEngineDirs {
		kw := "elif"
		if first {
			kw = "if"
			first = false
		}
		fmt.Fprintf(&b, "%s [ -x \"%s/llama-server\" ]; then\n    SERVER_BIN=\"%s/llama-server\"\n", kw, d.Dir, d.Dir)
		lib := d.backendDir()
		export := fmt.Sprintf("export GGML_BACKEND_PATH=\"%s/libggml-ring.so\"\n    export LD_LIBRARY_PATH=\"%s:$LD_LIBRARY_PATH\"\n", lib, lib)
		if d.BackendOptional {
			fmt.Fprintf(&b, "    if [ -f \"%s/libggml-ring.so\" ]; then\n        %s    fi\n", lib, strings.ReplaceAll(export, "\n    ", "\n        "))
		} else {
			fmt.Fprintf(&b, "    %s", export)
		}
	}
	b.WriteString(`elif command -v fleet >/dev/null 2>&1; then
    SERVER_BIN="fleet ring serve"
elif command -v llama-server >/dev/null 2>&1; then
    SERVER_BIN="llama-server"
else
    echo FLEET_NOBIN
    exit 0
fi
`)
	fmt.Fprintf(&b, "LOG=\"$HOME/.cache/fleet/runs/%s.log\"\n%snohup $FLEET_DETACH $SERVER_BIN %s > \"$LOG\" 2>&1 < /dev/null &\nPID=$!\necho \"FLEET_PID $PID\"\nsleep 2\nif ! kill -0 $PID 2>/dev/null; then echo FLEET_DIED; tail -n 20 \"$LOG\"; fi\n", runID, detachShell, masterArgs)
	return b.String()
}

// describeRemoteMaster is what a dry run says about the engine of a remote master: it is resolved there, at start.
func describeRemoteMaster(node string) string {
	var dirs []string
	for _, d := range masterEngineDirs {
		dirs = append(dirs, d.Dir+"/llama-server")
	}
	embedded := "the installed fleet whose `fleet ring info` answers (its linked engine), else "
	if legacyEngineDirsFirst() {
		embedded = ""
	}
	return fmt.Sprintf("resolved on %s when it starts (nothing of this machine's paths is used): %sthe first llama-server of %s, else `fleet ring serve`, else llama-server on its PATH; GGML_BACKEND_PATH / LD_LIBRARY_PATH then point at that build's libggml-ring.so", node, embedded, strings.Join(dirs, ", "))
}

// noMasterEngineError is the failure when the remote script found no engine.
func noMasterEngineError(node string) error {
	var dirs []string
	for _, d := range masterEngineDirs {
		dirs = append(dirs, d.Dir)
	}
	return fmt.Errorf("no engine on %s for the master: no fleet with an engine, no llama-server in %s, no fleet and no llama-server on its PATH (install fleet there or build llama-ring)", node, strings.Join(dirs, ", "))
}

// resolveLocalMaster turns the words of the master command into the program and arguments run on this machine: the
// `fleet` word becomes the engine found here (a llama-server binary has no `ring serve` subcommand: those words go).
// A fleet with the engine linked in re-executes itself: a stale llama-ring build directory must not shadow it, just
// as the remote script puts the engine-bearing fleet first.
func resolveLocalMaster(cmdParts []string) (bin string, args []string) {
	bin, args = cmdParts[0], cmdParts[1:]
	if bin == "fleet" {
		if ringengine.Available() {
			if exe, err := os.Executable(); err == nil {
				return exe, args
			}
		}
		if eng, ok := ringengine.FindEnginePath(); ok {
			bin = eng
			if strings.HasSuffix(eng, "llama-server") && len(cmdParts) >= 3 && cmdParts[1] == "ring" && cmdParts[2] == "serve" {
				args = cmdParts[3:]
			}
		}
	}
	return bin, args
}

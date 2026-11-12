package deploy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// OnState, when set, is told about every state the deploy writes to its record file (best effort, the deploy does not
// wait on its result being useful). `fleet deploy` uses it to open / update the run in the daemon's run store, which
// is the daemon's own file: only the daemon writes it.
var OnState func(DeployResult)

// RunRecorder records a deploy result in a run store or recorder mock.
type RunRecorder interface {
	RecordRun(res DeployResult) error
}

// DefaultRunRecorder writes the run state to ~/.cache/fleet/runs and invokes OnState.
type DefaultRunRecorder struct{}

func (DefaultRunRecorder) RecordRun(res DeployResult) error {
	res.saveState()
	return nil
}

// ReadRecord reads the recorded DeployResult for a run ID from ~/.cache/fleet/runs.
func ReadRecord(id string) (*DeployResult, error) {
	path := filepath.Join(runDir(), id+".json")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r DeployResult
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// MarkEndpointStopped rewrites the record files of an endpoint's deploys that still say they run (starting, running,
// timeout, planned) as "stopped": what `fleet run stop` / `fleet run end` did on purpose. LastRunOptions skips such
// records, so the watchdog does not bring a deliberately stopped master back. It returns how many records changed.
func MarkEndpointStopped(endpoint, why string) int {
	files, _ := filepath.Glob(filepath.Join(runDir(), "*.json"))
	n := 0
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var r DeployResult
		if json.Unmarshal(b, &r) != nil || r.EndpointName != endpoint || r.RunID == "" {
			continue
		}
		switch r.Status {
		case StatusFailed, StatusStopped:
			continue
		}
		r.Status, r.Error = StatusStopped, why+" ("+time.Now().Format(time.RFC3339)+")"
		r.saveState()
		n++
	}
	return n
}

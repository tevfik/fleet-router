package deploy

import (
	"fmt"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/plan"
)

// LocalHostname is the name of this machine as fleet deploy uses it for a master that was not named.
func LocalHostname() string { return localHostname() }

// ReadModelMeta reads the GGUF metadata of path: here, else on master over ssh (the header is streamed).
func ReadModelMeta(master, path string) (*plan.Model, error) {
	m, err := readGGUF(path)
	if err == nil {
		return m, nil
	}
	if isLocalNode(master) {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	rm, rerr := readRemoteMeta(master, path)
	if rerr != nil {
		return nil, fmt.Errorf("reading %s here (%v) and on %s (%v)", path, err, master, rerr)
	}
	if len(rm.LayerRead) != rm.Layers {
		return nil, fmt.Errorf("%s on %s: only the basic metadata could be read (no tensor table); the planner needs the GGUF header", path, master)
	}
	return rm, nil
}

// StatsPortUsed is the engine statistics port of the master (RING_STATS_PORT, /ring/trace).
func (r *DeployResult) StatsPortUsed() int { return r.statsPort }

// CandidateCommands is what `fleet plan --cmd` prints for a plan candidate: the commands a deploy of it runs, from the
// same builder as `fleet deploy` (PlanDeployment: one source for the flags, the engine path and the worker lines).
// Nothing is started. spec is the plan's speculation type ("": the deploy's own rule).
func CandidateCommands(cfg *config.Config, modelPath string, c plan.Candidate, ctx, np int, spec string) (*DeployResult, error) {
	o := DeployOptions{ModelPath: modelPath, MasterNode: c.Master, Ctx: ctx, Np: np, SpecType: spec, DryRun: true, Candidate: &c}
	seen := map[string]bool{}
	for _, p := range c.Parts {
		if p.Node != c.Master && !seen[p.Node] {
			seen[p.Node] = true
			o.WorkerNodes = append(o.WorkerNodes, p.Node)
		}
	}
	return PlanDeployment(cfg, o)
}

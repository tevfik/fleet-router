package deploy

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/plan"
)

// stubPlanner removes every disk, SSH and network dependency from PlanDeployment.
func stubPlanner(t *testing.T, arch string) {
	t.Helper()
	stubMeta(t, &plan.Model{Name: "m", Arch: arch, Layers: 48, Recurrent: testRecurrent[arch]})
}

// testRecurrent stands in for the GGUF metadata of the test architectures: in a real file the ssm.* keys or tensors
// say it (plan.Model.Recurrent); the planner never looks at the architecture name.
var testRecurrent = map[string]bool{"qwen35moe": true}

// testNodeAddrs is what a user's nodes: section would say about the test machines (fleet has no built-in table).
var testNodeAddrs = map[string]string{"papa": "10.0.1.119", "dino": "10.0.1.13", "victor": "10.0.1.30"}

// stubMeta makes every GGUF read (local or remote, model or draft) return a copy of m.
func stubMeta(t *testing.T, m *plan.Model) {
	t.Helper()
	oldR, oldM, oldL, oldH := readGGUF, readRemoteMeta, lanProbe, localHostname
	readGGUF = func(string) (*plan.Model, error) { c := *m; return &c, nil }
	readRemoteMeta = func(string, string) (*plan.Model, error) { c := *m; return &c, nil }
	lanProbe = func(*RemoteWorkerManager, string) bool { return false }
	localHostname = func() string { return "testhost" }
	t.Cleanup(func() { readGGUF, readRemoteMeta, lanProbe, localHostname = oldR, oldM, oldL, oldH })
	stubNodes(t, testNodeAddrs, nil)
	stubLocalWorker(t, "/opt/ring/bin/ring-worker", nil)
	oldP := probeMemory
	probeMemory = func(context.Context, string, bool) (MemFree, error) {
		return MemFree{}, errors.New("no memory probe in tests")
	}
	t.Cleanup(func() { probeMemory = oldP })
}

func dry(t *testing.T, o DeployOptions) *DeployResult {
	t.Helper()
	if o.ModelPath == "" {
		o.ModelPath = "/models/ornith-122b.gguf"
	}
	o.DryRun = true
	if o.MasterNode == "" {
		o.MasterNode = "dino"
	}
	res, err := PlanDeployment(config.Default(), o)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestDflashDefaultsToNMax2(t *testing.T) {
	stubPlanner(t, "qwen35moe")
	res := dry(t, DeployOptions{WorkerNodes: []string{"victor"}, SpecType: "draft-dflash", DraftModel: "/m/dflash2.gguf"})
	if !strings.Contains(res.MasterCmd, "--spec-type draft-dflash --spec-draft-n-max 2") {
		t.Errorf("dflash default must be n-max 2: %s", res.MasterCmd)
	}
	if !strings.Contains(res.MasterCmd, "--spec-draft-ngl 99") {
		t.Errorf("draft model flags missing: %s", res.MasterCmd)
	}
}

func TestAutoDraftNeverAboveFour(t *testing.T) {
	stubPlanner(t, "llama")
	for _, spec := range []string{"draft-mtp", "ngram-mod", "draft-dflash", "draft-simple"} {
		res := dry(t, DeployOptions{WorkerNodes: []string{"victor"}, SpecType: spec, DraftModel: "/m/d.gguf"})
		for _, big := range []string{"n-max 5", "n-max 6", "n-max 7", "n-max 8"} {
			if strings.Contains(res.MasterCmd, big) {
				t.Errorf("%s: automatic draft window above 4: %s", spec, res.MasterCmd)
			}
		}
	}
}

func TestExplicitDraftMaxIsAuthoritative(t *testing.T) {
	stubPlanner(t, "qwen35moe")
	res := dry(t, DeployOptions{WorkerNodes: []string{"victor"}, SpecType: "draft-dflash", DraftModel: "/m/dflash2.gguf", DraftMax: 8})
	if !strings.Contains(res.MasterCmd, "--spec-draft-n-max 8") {
		t.Errorf("explicit --draft-max must win: %s", res.MasterCmd)
	}
}

func TestSpecEstimateHasNoBoostAndWarns(t *testing.T) {
	stubPlanner(t, "qwen35moe")
	spec := dry(t, DeployOptions{WorkerNodes: []string{"victor"}, SpecType: "draft-dflash", DraftModel: "/m/dflash2.gguf"})
	none := dry(t, DeployOptions{WorkerNodes: []string{"victor"}, SpecType: "none"})
	if spec.EstimatedTPS != none.EstimatedTPS {
		t.Errorf("speculation must not change the estimate: %v vs %v", spec.EstimatedTPS, none.EstimatedTPS)
	}
	all := strings.Join(spec.Warnings, "\n")
	for _, want := range []string{"WITHOUT speculation", "indicative", "STATE_SNAP"} {
		if !strings.Contains(all, want) {
			t.Errorf("warnings lack %q:\n%s", want, all)
		}
	}
	if w := strings.Join(none.Warnings, "\n"); strings.Contains(w, "STATE_SNAP") {
		t.Errorf("no speculation, no snapshot warning: %s", w)
	}
}

// The engine keeps state snapshots off unless asked: fleet asks only for speculation on a hybrid model over the ring.
func TestStateSnapEnvOnlyForHybridSpecOnRing(t *testing.T) {
	stubPlanner(t, "qwen35moe")
	if res := dry(t, DeployOptions{WorkerNodes: []string{"victor"}, SpecType: "draft-dflash", DraftModel: "/m/dflash2.gguf"}); !strings.Contains(res.MasterEnv, "GGML_RING_STATE_SNAP=1") {
		t.Errorf("hybrid + speculation + ring: snapshots expected on: %s", res.MasterEnv)
	}
	if res := dry(t, DeployOptions{WorkerNodes: []string{"victor"}, SpecType: "none"}); strings.Contains(res.MasterEnv, "STATE_SNAP") {
		t.Errorf("no speculation: snapshots must stay off: %s", res.MasterEnv)
	}
	stubPlanner(t, "llama")
	if res := dry(t, DeployOptions{WorkerNodes: []string{"victor"}, SpecType: "draft-mtp", ModelPath: "/m/dense.gguf"}); strings.Contains(res.MasterEnv, "STATE_SNAP") {
		t.Errorf("dense model: snapshots must stay off: %s", res.MasterEnv)
	}
}

func TestNoStateSnapWarningForNonHybridOrNoWorkers(t *testing.T) {
	stubPlanner(t, "llama")
	res := dry(t, DeployOptions{WorkerNodes: []string{"victor"}, SpecType: "draft-mtp", ModelPath: "/m/dense.gguf"})
	if strings.Contains(strings.Join(res.Warnings, "\n"), "STATE_SNAP") {
		t.Error("dense model: no hybrid warning")
	}
	stubPlanner(t, "qwen35moe")
	res = dry(t, DeployOptions{WorkerNodes: []string{}, SpecType: "draft-mtp"})
	if strings.Contains(strings.Join(res.Warnings, "\n"), "STATE_SNAP") {
		t.Error("single machine: no worker, no hybrid warning")
	}
}

func TestChainEnv(t *testing.T) {
	stubPlanner(t, "llama")
	stubNodes(t, map[string]string{"papa": "10.0.1.119", "dino": "10.0.1.13"}, nil)
	res := dry(t, DeployOptions{WorkerNodes: []string{"victor", "papa"}, MasterNode: "dino", Chain: true})
	if !strings.Contains(res.MasterEnv, "RING_SERVERS=victor:50152,10.0.1.119:50153") {
		t.Errorf("ring servers: %s", res.MasterEnv)
	}
	if !strings.Contains(res.MasterEnv, "RING_PEER_SERVERS=victor:50252,10.0.1.119:50253") {
		t.Errorf("peer servers (ring port + 100, same order): %s", res.MasterEnv)
	}
	if got := res.WorkerEnv["victor"]; got != "GGML_RING_PEER_LISTEN=0.0.0.0:50252" {
		t.Errorf("victor env: %q", got)
	}
	if got := res.WorkerEnv["papa"]; got != "GGML_RING_PEER_LISTEN=0.0.0.0:50253" {
		t.Errorf("papa env: %q", got)
	}
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "experimental") {
		t.Error("chain must carry the experimental warning")
	}
}

func TestChainIsOffByDefault(t *testing.T) {
	stubPlanner(t, "llama")
	res := dry(t, DeployOptions{WorkerNodes: []string{"victor", "papa"}, MasterNode: "dino"})
	if strings.Contains(res.MasterEnv, "PEER") || len(res.WorkerEnv) != 0 {
		t.Errorf("no --chain, no peer env: %q %v", res.MasterEnv, res.WorkerEnv)
	}
}

func TestChainRefusedBelowTwoWorkers(t *testing.T) {
	stubPlanner(t, "llama")
	for _, w := range [][]string{{"victor"}, {}} {
		_, err := PlanDeployment(config.Default(), DeployOptions{ModelPath: "/m/x.gguf", MasterNode: "dino", WorkerNodes: w, Chain: true, DryRun: true})
		if err == nil || !strings.Contains(err.Error(), "--chain needs at least 2 workers") {
			t.Errorf("%d workers: want a clear error, got %v", len(w), err)
		}
	}
}

func TestChainPortCollision(t *testing.T) {
	stubPlanner(t, "llama")
	_, err := PlanDeployment(config.Default(), DeployOptions{ModelPath: "/m/x.gguf", MasterNode: "dino", WorkerNodes: []string{"victor", "papa"}, Chain: true, Port: 50252, DryRun: true})
	if err == nil || !strings.Contains(err.Error(), "collides") {
		t.Errorf("peer port equal to the master port must be refused: %v", err)
	}
}

func TestIOTimeoutAndExitOnFailEnv(t *testing.T) {
	stubPlanner(t, "llama")
	res := dry(t, DeployOptions{WorkerNodes: []string{"victor", "papa"}, IOTimeoutS: 120, ExitOnFail: true})
	if !strings.Contains(res.MasterEnv, "GGML_RING_IO_TIMEOUT_S=120") || !strings.Contains(res.MasterEnv, "GGML_RING_EXIT_ON_FAIL=1") {
		t.Errorf("master env: %s", res.MasterEnv)
	}
	for _, n := range []string{"victor", "papa"} {
		if res.WorkerEnv[n] != "GGML_RING_IO_TIMEOUT_S=120" {
			t.Errorf("%s env: %q", n, res.WorkerEnv[n])
		}
	}
	if strings.Contains(res.WorkerEnv["victor"], "EXIT_ON_FAIL") {
		t.Error("exit-on-fail is a master setting")
	}
	plain := dry(t, DeployOptions{WorkerNodes: []string{"victor"}})
	if strings.Contains(plain.MasterEnv, "TIMEOUT") || strings.Contains(plain.MasterEnv, "EXIT_ON_FAIL") {
		t.Errorf("defaults must stay untouched: %s", plain.MasterEnv)
	}
	if _, err := PlanDeployment(config.Default(), DeployOptions{ModelPath: "/m/x.gguf", IOTimeoutS: -1, DryRun: true, WorkerNodes: []string{}}); err == nil {
		t.Error("negative timeout must be refused")
	}
}

func TestFilterWorkerEnv(t *testing.T) {
	got := filterWorkerEnv([]string{"GGML_RING_PEER_LISTEN=0.0.0.0:50252", "BAD=a;rm -rf /", "X"})
	if strings.Join(got, " ") != "GGML_RING_PEER_LISTEN=0.0.0.0:50252" {
		t.Errorf("%q", got)
	}
}

func TestWorkerPortFlag(t *testing.T) {
	stubPlanner(t, "llama")
	res := dry(t, DeployOptions{WorkerNodes: []string{"papa", "10.0.1.50"}, WorkerPort: 50170, MasterNode: "dino"})
	if !strings.Contains(res.MasterEnv, ":50170") || !strings.Contains(res.MasterEnv, ":50171") || strings.Contains(res.MasterEnv, "50152") {
		t.Errorf("worker ports must start at --worker-port: %s", res.MasterEnv)
	}
}

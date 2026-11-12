package deploy

import (
	"errors"
	"strings"
	"testing"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/plan"
)

// The file names below would have triggered the old name rules (moe, a3b, ornith, qwen, flash, mtp, dflash); the
// metadata says the opposite, and the metadata wins.
func TestPlannerUsesMetadataNotNames(t *testing.T) {
	stubPlanner(t, "llama")
	stubMeta(t, &plan.Model{Name: "m", Arch: "llama", Layers: 40})
	res := dry(t, DeployOptions{ModelPath: "/m/Ornith-Qwen3-MoE-A3B-flash-mtp.gguf", WorkerNodes: []string{"papa"}})
	if strings.Contains(res.MasterCmd, "--spec-type") {
		t.Errorf("a dense model without NextN heads and without a draft gets no speculation: %s", res.MasterCmd)
	}
	if res.Devices != "CUDA0,RING0" {
		t.Errorf("no NextN heads in the metadata: the master stays first, got %s", res.Devices)
	}
	one := dry(t, DeployOptions{ModelPath: "/m/qwen3moe-a3b.gguf", WorkerNodes: []string{}, CPUMoE: false, Strategy: StrategyMemory})
	if strings.Contains(one.MasterCmd, "-cmoe") {
		t.Errorf("dense by its metadata: no host experts: %s", one.MasterCmd)
	}
}

func TestMoEFromExpertCount(t *testing.T) {
	stubPlanner(t, "llama")
	stubMeta(t, &plan.Model{Name: "m", Arch: "anyarch", Layers: 40, Experts: 64, ExpertsUsed: 8})
	res := dry(t, DeployOptions{ModelPath: "/m/plain.gguf", WorkerNodes: []string{}, Strategy: StrategyMemory})
	if !strings.Contains(res.MasterCmd, "-cmoe") || !strings.Contains(res.MasterCmd, "--moe-cache-mib") {
		t.Errorf("expert_count > 0 on one machine with the memory strategy: host experts expected: %s", res.MasterCmd)
	}
	if strings.Contains(res.MasterCmd, "--spec-type") {
		t.Errorf("single-machine host experts: speculation off under auto: %s", res.MasterCmd)
	}
}

func TestHybridFromMetadata(t *testing.T) {
	stubPlanner(t, "llama")
	stubMeta(t, &plan.Model{Name: "m", Arch: "whatever", Layers: 48, Recurrent: true})
	res := dry(t, DeployOptions{ModelPath: "/m/dense-looking.gguf", WorkerNodes: []string{"papa"}, SpecType: "ngram-mod"})
	if !strings.Contains(res.MasterEnv, "GGML_RING_STATE_SNAP=1") || !strings.Contains(strings.Join(res.Warnings, "\n"), "STATE_SNAP") {
		t.Errorf("recurrent blocks in the metadata: snapshots and the generic warning expected: %s %v", res.MasterEnv, res.Warnings)
	}
	stubMeta(t, &plan.Model{Name: "m", Arch: "qwen35moe", Layers: 48})
	res = dry(t, DeployOptions{ModelPath: "/m/ornith.gguf", WorkerNodes: []string{"papa"}, SpecType: "ngram-mod"})
	if strings.Contains(res.MasterEnv, "STATE_SNAP") {
		t.Errorf("no recurrent blocks in the metadata: no snapshots, whatever the name: %s", res.MasterEnv)
	}
}

func TestNextNFromMetadata(t *testing.T) {
	stubPlanner(t, "llama")
	stubMeta(t, &plan.Model{Name: "m", Arch: "anyarch", Layers: 48, HasMTP: true, MTPLayers: 1})
	res := dry(t, DeployOptions{ModelPath: "/m/x.gguf", WorkerNodes: []string{"papa"}})
	if res.Devices != "RING0,CUDA0" {
		t.Errorf("NextN heads: the master holds the tail: %s", res.Devices)
	}
	if !strings.Contains(res.MasterCmd, "--spec-type draft-mtp") {
		t.Errorf("auto with NextN heads: draft-mtp: %s", res.MasterCmd)
	}
}

func TestDraftTypeFromDraftMetadata(t *testing.T) {
	stubPlanner(t, "llama")
	old := readGGUF
	readGGUF = func(p string) (*plan.Model, error) {
		if strings.Contains(p, "draft") {
			return &plan.Model{Name: "d", Arch: "qwen3-dflash", Layers: 2}, nil
		}
		return &plan.Model{Name: "m", Arch: "llama", Layers: 48}, nil
	}
	t.Cleanup(func() { readGGUF = old })
	res := dry(t, DeployOptions{ModelPath: "/m/x.gguf", WorkerNodes: []string{"papa"}, DraftModel: "/m/draft.gguf"})
	if !strings.Contains(res.MasterCmd, "--spec-type draft-dflash --spec-draft-n-max 2") {
		t.Errorf("a dflash draft architecture: %s", res.MasterCmd)
	}
	readGGUF = func(p string) (*plan.Model, error) { return &plan.Model{Name: "m", Arch: "llama", Layers: 48}, nil }
	res = dry(t, DeployOptions{ModelPath: "/m/x.gguf", WorkerNodes: []string{"papa"}, DraftModel: "/m/dflash-named.gguf"})
	if !strings.Contains(res.MasterCmd, "--spec-type draft-simple") {
		t.Errorf("a draft named dflash whose metadata is a plain llama is draft-simple: %s", res.MasterCmd)
	}
}

func TestNoWorkerIsAdded(t *testing.T) {
	stubPlanner(t, "llama")
	cfg := config.Default()
	cfg.Endpoints = []config.Endpoint{{Name: "e", Node: "victor", SSH: "victor", Remote: "127.0.0.1:8080", Model: "x"}}
	res, err := PlanDeployment(cfg, DeployOptions{ModelPath: "/m/x.gguf", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.WorkerNodes) != 0 || len(res.WorkerCmds) != 0 || res.Devices != "CUDA0" {
		t.Errorf("no workers given: none added (%v %v %s)", res.WorkerNodes, res.WorkerCmds, res.Devices)
	}
	if res.MasterNode != "testhost" || !strings.HasPrefix(res.URL, "http://127.0.0.1:") {
		t.Errorf("no master given: this machine (%s, %s)", res.MasterNode, res.URL)
	}
}

func TestMemorySplitFromNodesSection(t *testing.T) {
	stubPlanner(t, "llama")
	stubMeta(t, &plan.Model{Name: "m", Arch: "llama", Layers: 40})
	cfg := config.Default()
	cfg.Nodes = map[string]config.NodeSpec{"big": {MemGiB: 120}, "small": {MemGiB: 12}, "tiny": {MemGiB: 8}}
	plan2 := func(master string, ws ...string) *DeployResult {
		res, err := PlanDeployment(cfg, DeployOptions{ModelPath: "/m/x.gguf", MasterNode: master, WorkerNodes: ws, DryRun: true, NoHub: true})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if r := plan2("big", "small"); r.TensorSplit != "36,4" {
		t.Errorf("120:12 of 40 blocks: %s", r.TensorSplit)
	}
	if r := plan2("big", "small", "tiny"); r.TensorSplit != "34,4,2" {
		t.Errorf("120:12:8 of 40 blocks: %s", r.TensorSplit)
	}
	if r := plan2("big", "undeclared"); r.TensorSplit == "36,4" {
		t.Errorf("a machine without mem_gib: the strategy split, not the memory one: %s", r.TensorSplit)
	}
}

func TestNoGuessWithoutMetadata(t *testing.T) {
	stubPlanner(t, "llama")
	old, oldR := readGGUF, readRemoteMeta
	readGGUF = func(string) (*plan.Model, error) { return nil, errors.New("not here") }
	readRemoteMeta = func(string, string) (*plan.Model, error) { return nil, errors.New("no ssh") }
	t.Cleanup(func() { readGGUF, readRemoteMeta = old, oldR })
	if _, err := PlanDeployment(config.Default(), DeployOptions{ModelPath: "/m/x.gguf", MasterNode: "far", DryRun: true}); err == nil {
		t.Error("unreadable metadata and no --ts: an error, not 40 made-up layers")
	}
	res, err := PlanDeployment(config.Default(), DeployOptions{ModelPath: "/m/x.gguf", MasterNode: "far", WorkerNodes: []string{"papa"}, TensorSplit: "30,10", DryRun: true})
	if err != nil || !strings.Contains(strings.Join(res.Warnings, "\n"), "block count of --ts (40)") {
		t.Errorf("--ts gives the block count: %v %v", err, res)
	}
}

func TestEstimateIsThePlannersOrNone(t *testing.T) {
	stubPlanner(t, "llama")
	stubMeta(t, &plan.Model{Name: "m", Arch: "llama", Layers: 4, LayerRead: []int64{1 << 30, 1 << 30, 1 << 30, 1 << 30}, HiddenBytes: 16384})
	if r := dry(t, DeployOptions{ModelPath: "/m/x.gguf", WorkerNodes: []string{"papa"}}); r.EstimatedTPS != 0 {
		t.Errorf("no declared rates: no estimate, got %v", r.EstimatedTPS)
	}
	cfg := config.Default()
	cfg.Nodes = map[string]config.NodeSpec{"dino": {WeightGBps: 200}, "papa": {WeightGBps: 200}}
	r, err := PlanDeployment(cfg, DeployOptions{ModelPath: "/m/x.gguf", MasterNode: "dino", WorkerNodes: []string{"papa"}, DryRun: true})
	if err != nil || r.EstimatedTPS <= 0 || r.LatencyMS <= 20 {
		t.Errorf("declared rates: the cost model's estimate: %v %+v", err, r)
	}
	c := &plan.Candidate{Kind: "split", Master: "dino", TPS: 23.5, TokenMS: 42.5, Parts: []plan.Part{{Node: "dino", Layers: 2, Output: true}, {Node: "papa", Layers: 2}}}
	if r := dry(t, DeployOptions{ModelPath: "/m/x.gguf", WorkerNodes: []string{"papa"}, Candidate: c}); r.EstimatedTPS != 23.5 {
		t.Errorf("a plan candidate brings its own estimate: %v", r.EstimatedTPS)
	}
}

func TestExtrasValidatedAndApplied(t *testing.T) {
	stubPlanner(t, "llama")
	res := dry(t, DeployOptions{ModelPath: "/m/x.gguf", WorkerNodes: []string{"papa"},
		MasterEnvExtra: map[string]string{"GGML_CUDA_X": "1"}, WorkerEnvExtra: map[string]map[string]string{"papa": {"CUDA_VISIBLE_DEVICES": "0"}},
		MasterArgs: []string{"--temp", "0"}})
	if !strings.Contains(res.MasterEnv, "GGML_CUDA_X=1") || !strings.Contains(res.WorkerEnv["papa"], "CUDA_VISIBLE_DEVICES=0") || !strings.Contains(res.MasterCmd, " --temp 0 ") {
		t.Errorf("extras: %s | %v | %s", res.MasterEnv, res.WorkerEnv, res.MasterCmd)
	}
	for _, bad := range []DeployOptions{
		{MasterEnvExtra: map[string]string{"X": "a;rm -rf /"}},
		{MasterEnvExtra: map[string]string{"RING_SERVERS": "1.2.3.4:1"}},
		{MasterEnvExtra: map[string]string{"1X": "a"}},
		{WorkerEnvExtra: map[string]map[string]string{"w": {"X": "$(id)"}}},
		{MasterArgs: []string{"--port"}},
		{MasterArgs: []string{"-ts=1,2"}},
		{MasterArgs: []string{"a b"}},
	} {
		if err := ValidateExtras(bad); err == nil {
			t.Errorf("must be refused: %+v", bad)
		}
	}
}

// A plan candidate with the CPU-expert tier deploys as --worker-cpu-experts over the candidate's own split.
func TestCandidateCPUExpertsBecomeWorkerCPUExperts(t *testing.T) {
	stubPlanner(t, "llama")
	c := &plan.Candidate{Kind: "split", Master: "dino", TPS: 14.4, Parts: []plan.Part{{Node: "dino", Layers: 34, Output: true}, {Node: "papa", Layers: 14, First: 34, CPUExperts: 7}}}
	res := dry(t, DeployOptions{WorkerNodes: []string{"papa"}, Candidate: c})
	if !strings.Contains(res.MasterCmd, "-ts 34,14,0") || !strings.Contains(res.MasterCmd, `blk\.(41|42|43|44|45|46|47)\.ffn_(up|down|gate)_exps\.weight=RING1`) || res.WorkerDevices["papa"] != "CUDA0,CPU" {
		t.Errorf("%s %v", res.MasterCmd, res.WorkerDevices)
	}
}

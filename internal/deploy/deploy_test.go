package deploy

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/plan"
)

// stubAddrs keeps the real-model tests off the network and the user's config: the test machines' addresses come from
// a stand-in nodes: section, nothing is probed.
func stubAddrs(t *testing.T) {
	t.Helper()
	oldL := lanProbe
	lanProbe = func(*RemoteWorkerManager, string) bool { return false }
	t.Cleanup(func() { lanProbe = oldL })
	stubNodes(t, testNodeAddrs, nil)
	oldP := probeMemory
	probeMemory = func(context.Context, string, bool) (MemFree, error) { return MemFree{RAMGiB: 1024}, nil }
	t.Cleanup(func() { probeMemory = oldP })
}

func TestStrategyParams(t *testing.T) {
	lat := ParamsForStrategy(StrategyLatency, 24, true)
	if lat.MicroBatch != 512 {
		t.Errorf("expected MicroBatch 512 for latency (16 only slowed prefill, E2/E7), got %d", lat.MicroBatch)
	}
	if lat.WireFP8 {
		t.Errorf("a strategy must not turn the lossy wire on by itself (it is opt-in: deploy --wire)")
	}
	if !lat.AsymSplit {
		t.Errorf("expected AsymSplit true for latency")
	}

	tp := ParamsForStrategy(StrategyThroughput, 24, true)
	if tp.MicroBatch != 512 {
		t.Errorf("expected MicroBatch 512 for throughput, got %d", tp.MicroBatch)
	}
	if tp.Np != 8 {
		t.Errorf("expected Np 8 for throughput, got %d", tp.Np)
	}

	mem := ParamsForStrategy(StrategyMemory, 24, true)
	if mem.NGL != 12 {
		t.Errorf("expected NGL 12 for memory, got %d", mem.NGL)
	}
}

func TestPlanDeploymentDryRun(t *testing.T) {
	modelPath := "/home/user/models/LFM2-8B-A1B-Q4_K_M.gguf"
	if _, err := os.Stat(modelPath); os.IsNotExist(err) {
		t.Skipf("skipping test: %s does not exist", modelPath)
	}
	stubAddrs(t)
	cfg := config.Default()
	opts := DeployOptions{
		ModelPath:   modelPath,
		Strategy:    StrategyLatency,
		MasterNode:  "papa",
		WorkerNodes: []string{"victor"},
		Port:        18090,
		Ctx:         4096,
		DryRun:      true,
	}

	res, err := ExecuteDeployment(context.Background(), cfg, opts)
	if err != nil {
		t.Fatalf("dry run deploy failed: %v", err)
	}

	if res.Status != "dry-run" {
		t.Errorf("expected status dry-run, got %s", res.Status)
	}
	if !strings.Contains(res.MasterCmd, "-ub 512") {
		t.Errorf("expected -ub 512 in master cmd: %s", res.MasterCmd)
	}
	if !strings.Contains(res.MasterCmd, "-fa on") {
		t.Errorf("expected -fa on in master cmd: %s", res.MasterCmd)
	}
	if strings.Contains(res.MasterEnv, "GGML_RING_WIRE") {
		t.Errorf("the default wire is f32: no lossy wire variable expected in master env: %s", res.MasterEnv)
	}
	if res.WorkerCmds["victor"] == "" {
		t.Errorf("expected worker cmd for victor")
	}
}

func TestPlanDeploymentWireOptIn(t *testing.T) {
	modelPath := "/home/user/models/LFM2-8B-A1B-Q4_K_M.gguf"
	if _, err := os.Stat(modelPath); os.IsNotExist(err) {
		t.Skipf("skipping test: %s does not exist", modelPath)
	}
	stubAddrs(t)
	for wire, want := range map[string]string{"f16": "GGML_RING_WIRE_F16=1", "fp8": "GGML_RING_WIRE_FP8=1", "FP8": "GGML_RING_WIRE_FP8=1"} {
		res, err := ExecuteDeployment(context.Background(), config.Default(), DeployOptions{
			ModelPath: modelPath, Strategy: StrategyLatency, MasterNode: "papa", WorkerNodes: []string{"victor"},
			Port: 18090, Ctx: 4096, Wire: wire, DryRun: true,
		})
		if err != nil {
			t.Fatalf("dry run deploy failed: %v", err)
		}
		if !strings.Contains(res.MasterEnv, want) {
			t.Errorf("--wire %s: expected %s in master env: %s", wire, want, res.MasterEnv)
		}
	}
}

func TestPlanDeploymentDeepSeekOptions(t *testing.T) {
	modelPath := "/home/user/models/LFM2-8B-A1B-Q4_K_M.gguf"
	if _, err := os.Stat(modelPath); os.IsNotExist(err) {
		t.Skipf("skipping test: %s does not exist", modelPath)
	}
	stubAddrs(t)

	cfg := config.Default()
	opts := DeployOptions{
		ModelPath:   modelPath,
		Strategy:    StrategyLatency,
		MasterNode:  "dino",
		WorkerNodes: []string{"victor", "papa"},
		Port:        18090,
		Ctx:         8192,
		NGL:         15,
		MMap:        true,
		MMProj:      "/home/user/models/mmproj.gguf",
		MMProjCPU:   true,
		DryRun:      true,
	}

	res, err := ExecuteDeployment(context.Background(), cfg, opts)
	if err != nil {
		t.Fatalf("dry run deploy failed: %v", err)
	}

	if res.URL != "http://10.0.1.13:18090/v1" {
		t.Errorf("expected remote master URL http://10.0.1.13:18090/v1, got %s", res.URL)
	}
	if !strings.Contains(res.MasterCmd, "-ngl 15") {
		t.Errorf("expected -ngl 15 in master cmd: %s", res.MasterCmd)
	}
	if !strings.Contains(res.MasterCmd, "--mmproj /home/user/models/mmproj.gguf") {
		t.Errorf("expected --mmproj in master cmd: %s", res.MasterCmd)
	}
	if !strings.Contains(res.MasterCmd, "--no-mmproj-offload") {
		t.Errorf("expected --no-mmproj-offload (CPU) in master cmd: %s", res.MasterCmd)
	}
	if !strings.Contains(res.MasterCmd, "--load-mode mmap") {
		t.Errorf("expected --load-mode mmap in master cmd: %s", res.MasterCmd)
	}

	// Test custom TS and MMProjDevice
	optsCustom := DeployOptions{
		ModelPath:    modelPath,
		Strategy:     StrategyLatency,
		MasterNode:   "dino",
		WorkerNodes:  []string{"papa", "victor"},
		Port:         18090,
		NGL:          15,
		TensorSplit:  "15,13,12",
		MMProj:       "/home/user/models/mmproj.gguf",
		MMProjDevice: "RING0",
		DryRun:       true,
	}
	resCustom, err := ExecuteDeployment(context.Background(), cfg, optsCustom)
	if err != nil {
		t.Fatalf("custom deploy plan failed: %v", err)
	}
	if !strings.Contains(resCustom.MasterCmd, "-ts 15,13,12") {
		t.Errorf("expected custom -ts 15,13,12, got: %s", resCustom.MasterCmd)
	}
	if !strings.Contains(resCustom.MasterCmd, "-mmdev RING0") {
		t.Errorf("expected -mmdev RING0, got: %s", resCustom.MasterCmd)
	}

	// Test CPUOnly deployment
	optsCPU := DeployOptions{
		ModelPath:  modelPath,
		Strategy:   StrategyLatency,
		MasterNode: "papa",
		Port:       18090,
		CPUOnly:    true,
		MMProj:     "/home/user/models/mmproj.gguf",
		DryRun:     true,
	}
	resCPU, err := ExecuteDeployment(context.Background(), cfg, optsCPU)
	if err != nil {
		t.Fatalf("cpu only deploy plan failed: %v", err)
	}
	if !strings.Contains(resCPU.MasterCmd, "-ngl 0") {
		t.Errorf("expected -ngl 0 for CPU deployment, got: %s", resCPU.MasterCmd)
	}
	if !strings.Contains(resCPU.MasterCmd, "--no-mmproj-offload") {
		t.Errorf("expected --no-mmproj-offload for CPU deployment, got: %s", resCPU.MasterCmd)
	}

	stubPlanner(t, "qwen3moe")
	// Test Speculative Decoding and KV Cache flags
	optsSpec := DeployOptions{
		ModelPath:  "/models/DeepSeek-V4.1-Flash-MTP.gguf",
		Strategy:   StrategyLatency,
		MasterNode: "dino",
		SpecType:   "draft-mtp",
		DraftMax:   5,
		CacheTypeK: "q8_0",
		CacheTypeV: "q8_0",
		DryRun:     true,
	}
	resSpec, err := ExecuteDeployment(context.Background(), cfg, optsSpec)
	if err != nil {
		t.Fatalf("speculative deploy plan failed: %v", err)
	}
	if !strings.Contains(resSpec.MasterCmd, "--spec-type draft-mtp") {
		t.Errorf("expected --spec-type draft-mtp, got: %s", resSpec.MasterCmd)
	}
	if !strings.Contains(resSpec.MasterCmd, "--spec-draft-n-max 5") {
		t.Errorf("expected --spec-draft-n-max 5, got: %s", resSpec.MasterCmd)
	}
	if !strings.Contains(resSpec.MasterCmd, "--cache-type-k q8_0") {
		t.Errorf("expected --cache-type-k q8_0, got: %s", resSpec.MasterCmd)
	}
	if !strings.Contains(resSpec.MasterCmd, "--cache-type-v q8_0") {
		t.Errorf("expected --cache-type-v q8_0, got: %s", resSpec.MasterCmd)
	}
	if resSpec.EstimatedTPS > 60.0 {
		t.Errorf("the estimate must not include an (unmeasured) speculative boost, got: %f", resSpec.EstimatedTPS)
	}
}

func TestPlanDeploymentOutputOnMasterAddsOverride(t *testing.T) {
	modelPath := "/home/user/models/LFM2-8B-A1B-Q4_K_M.gguf"
	if _, err := os.Stat(modelPath); os.IsNotExist(err) {
		t.Skipf("skipping test: %s does not exist", modelPath)
	}
	stubAddrs(t)
	for _, onMaster := range []bool{true, false} {
		cand := &plan.Candidate{Kind: "split", Master: "papa", Parts: []plan.Part{
			{Node: "papa", Layers: 10, Output: onMaster}, {Node: "victor", Layers: 6, Output: !onMaster}}}
		res, err := ExecuteDeployment(context.Background(), config.Default(), DeployOptions{
			ModelPath: modelPath, Strategy: StrategyLatency, MasterNode: "papa", WorkerNodes: []string{"victor"},
			Candidate: cand, Port: 18090, Ctx: 4096, DryRun: true,
		})
		if err != nil {
			t.Fatalf("dry run deploy failed: %v", err)
		}
		has := strings.Contains(res.MasterCmd, `-ot '^output\.weight$=CUDA0'`)
		if has != onMaster {
			t.Errorf("output on master = %v, -ot present = %v: %s", onMaster, has, res.MasterCmd)
		}
	}
}

func TestWorkerDeploymentOffloadsEveryLayer(t *testing.T) {
	modelPath := "/home/user/models/LFM2-8B-A1B-Q4_K_M.gguf"
	if _, err := os.Stat(modelPath); os.IsNotExist(err) {
		t.Skipf("skipping test: %s does not exist", modelPath)
	}
	stubAddrs(t)
	cand := &plan.Candidate{Kind: "split", Master: "papa", Parts: []plan.Part{{Node: "papa", Layers: 10}, {Node: "victor", Layers: 14}}}
	res, err := ExecuteDeployment(context.Background(), config.Default(), DeployOptions{
		ModelPath: modelPath, Strategy: StrategyLatency, MasterNode: "papa", WorkerNodes: []string{"victor"},
		Candidate: cand, Port: 18091, Ctx: 4096, DryRun: true,
	})
	if err != nil {
		t.Fatalf("dry run deploy failed: %v", err)
	}
	if !strings.Contains(res.MasterCmd, "-ngl 99") {
		t.Errorf("block_count is not the offload count, want -ngl 99: %s", res.MasterCmd)
	}
}

func TestKeepOutputOnMaster(t *testing.T) {
	cases := []struct {
		name string
		c    *plan.Candidate
		devs []string
		want bool
	}{
		{"fixed split, master first", nil, []string{"CUDA0", "RING0"}, true},
		{"fixed three stages", nil, []string{"CUDA0", "RING0", "RING1"}, true},
		{"master last (MTP/Ornith order)", nil, []string{"RING0", "CUDA0"}, false},
		{"single node", nil, []string{"CUDA0"}, false},
		{"plan says worker", &plan.Candidate{Parts: []plan.Part{{Layers: 1}, {Layers: 1, Output: true}}}, []string{"CUDA0", "RING0"}, false},
		{"plan says master", &plan.Candidate{Parts: []plan.Part{{Layers: 1, Output: true}, {Layers: 1}}}, []string{"CUDA0", "RING0"}, true},
	}
	for _, tc := range cases {
		if got := keepOutputOnMaster(tc.c, tc.devs, strings.Join(tc.devs, ",")); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestPlanDeploymentFixedSplitKeepsOutputOnMaster(t *testing.T) {
	modelPath := "/home/user/models/LFM2-8B-A1B-Q4_K_M.gguf"
	if _, err := os.Stat(modelPath); os.IsNotExist(err) {
		t.Skipf("skipping test: %s does not exist", modelPath)
	}
	stubAddrs(t)
	res, err := ExecuteDeployment(context.Background(), config.Default(), DeployOptions{
		ModelPath: modelPath, Strategy: StrategyLatency, MasterNode: "dino", WorkerNodes: []string{"victor"},
		Port: 18090, Ctx: 4096, DryRun: true,
	})
	if err != nil {
		t.Fatalf("dry run deploy failed: %v", err)
	}
	if !strings.Contains(res.MasterCmd, `-ot '^output\.weight$=CUDA0'`) {
		t.Errorf("a fixed split must keep the output on the master: %s", res.MasterCmd)
	}
}

func TestPlanDeploymentWarnsAboutLossyWireOnFailedArch(t *testing.T) {
	modelPath := "/home/user/models/LFM2-8B-A1B-Q4_K_M.gguf"
	if _, err := os.Stat(modelPath); os.IsNotExist(err) {
		t.Skipf("skipping test: %s does not exist", modelPath)
	}
	stubAddrs(t)
	for wire, want := range map[string]string{"": "", "f16": "FAILED the gate"} {
		res, err := ExecuteDeployment(context.Background(), config.Default(), DeployOptions{
			ModelPath: modelPath, Strategy: StrategyLatency, MasterNode: "papa", WorkerNodes: []string{"victor"},
			Port: 18090, Ctx: 4096, Wire: wire, DryRun: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		var gate []string // victor has no direct address here (hub path / warning), which is reported separately
		for _, w := range res.Warnings {
			if !strings.Contains(w, "no known direct address") && !strings.Contains(w, "hub") {
				gate = append(gate, w)
			}
		}
		if (want == "") != (len(gate) == 0) || (want != "" && !strings.Contains(gate[0], want)) {
			t.Errorf("wire %q: warnings %v, want %q", wire, res.Warnings, want)
		}
	}
}

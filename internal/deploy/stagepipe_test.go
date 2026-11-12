package deploy

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/ggufhead"
	"github.com/tevfik/fleet-router/internal/hostinfo"
	"github.com/tevfik/fleet-router/internal/plan"
)

// the layer split of the 3-board report: 24 + 12 + 28 layers of 64 [PDF]
func reportCandidate() plan.Candidate {
	return plan.Candidate{Kind: "split", Master: "delta1", Parts: []plan.Part{
		{Node: "delta1", Layers: 24, First: 0},
		{Node: "pcie", Layers: 12, First: 24},
		{Node: "delta2", Layers: 28, First: 36, Output: true},
	}}
}

func toyModel() *plan.Model { return &plan.Model{Name: "Qwen-27B", Layers: 64} }

func hostMap(n string) string {
	return map[string]string{"delta1": "10.0.4.20", "pcie": "10.0.4.21", "delta2": "10.0.4.22"}[n]
}

func TestStagePipeCommands(t *testing.T) {
	p, err := PlanStagePipe(reportCandidate(), toyModel(), testNPUAdapter, StagePipeOptions{Host: hostMap, EmbeddingOffload: true})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"STAGE_LAYERS=0:24 STAGE_NEXT=tcp://10.0.4.21:5600 STAGE_TIMEOUT_S=600 NPU_EMBEDDING_OFFLOAD=on npu-run run /mnt/nvme/Qwen-27B --mode web",
		"STAGE_LAYERS=24:36 STAGE_NEXT=tcp://10.0.4.22:5600 STAGE_TIMEOUT_S=600 npu-stage-server /mnt/nvme/Qwen-27B/target 'tcp://*:5600'",
		"STAGE_LAYERS=36:64 npu-stage-server /mnt/nvme/Qwen-27B/target 'tcp://*:5600'",
	}
	for i, s := range p.Stages {
		if s.Command != want[i] {
			t.Errorf("stage %d:\n got %s\nwant %s", i, s.Command, want[i])
		}
	}
	roles := p.Stages[0].Role + p.Stages[1].Role + p.Stages[2].Role
	if roles != "ingressmiddlelast" {
		t.Fatal(roles)
	}
	if p.Stages[0].Ready.Kind != "tcp" || p.Stages[0].Ready.Addr != "10.0.4.20:9998" || p.Stages[2].Ready.Kind != "tcp" || p.Stages[2].Ready.Addr != "10.0.4.22:5600" {
		t.Fatalf("%+v", p.Stages)
	}
	if len(p.Warnings) == 0 || !strings.Contains(p.Warnings[0], "PROVISIONAL") {
		t.Fatalf("a provisional adapter must say so: %v", p.Warnings)
	}
}

func TestStagePipeStartsLastStageFirst(t *testing.T) {
	p, err := PlanStagePipe(reportCandidate(), toyModel(), testNPUAdapter, StagePipeOptions{Host: hostMap})
	if err != nil {
		t.Fatal(err)
	}
	if got := p.StartOrder; len(got) != 3 || got[0] != 2 || got[1] != 1 || got[2] != 0 {
		t.Fatalf("start order %v", got)
	}
	d := &DryRunner{}
	if err := RunStagePipe(p, d, 90*time.Second); err != nil {
		t.Fatal(err)
	}
	if len(d.Lines) != 9 {
		t.Fatalf("%d lines: %v", len(d.Lines), d.Lines)
	}
	// per stage: free the port, start, wait; the stage behind is waited for before the next one starts
	for i, kind := range []string{"free port", "start stage 2 (last", "tcp connect 10.0.4.22:5600", "free port", "start stage 1 (middle", "tcp connect 10.0.4.21:5600", "free port", "start stage 0 (ingress", "tcp connect 10.0.4.20:9998"} {
		if !strings.Contains(d.Lines[i], kind) {
			t.Errorf("line %d: %q lacks %q", i, d.Lines[i], kind)
		}
	}
}

type failing struct {
	DryRunner
	failReady int
}

func (f *failing) WaitReady(s StageLaunch, d time.Duration) error {
	if s.Index == f.failReady {
		return errors.New("refused")
	}
	return f.DryRunner.WaitReady(s, d)
}

func TestStagePipeStopsWhenAStageIsNotReady(t *testing.T) {
	p, _ := PlanStagePipe(reportCandidate(), toyModel(), testNPUAdapter, StagePipeOptions{Host: hostMap})
	f := &failing{failReady: 1}
	err := RunStagePipe(p, f, time.Second)
	if err == nil || !strings.Contains(err.Error(), "stage 1") || !strings.Contains(err.Error(), "2 of 3") {
		t.Fatalf("%v", err)
	}
	for _, l := range f.Lines {
		if strings.Contains(l, "stage 0") {
			t.Fatalf("the first stage must not start before the one behind it is up: %v", f.Lines)
		}
	}
}

func TestStagePipeRejectsBadCandidates(t *testing.T) {
	m := toyModel()
	gap := reportCandidate()
	gap.Parts[1].First = 25
	short := reportCandidate()
	short.Parts[2].Layers = 27
	head := reportCandidate()
	head.Parts[0].Output, head.Parts[2].Output = true, false
	empty := reportCandidate()
	empty.Parts[1].Layers = 0
	for name, c := range map[string]plan.Candidate{"follow each other": gap, "the model has 64": short, "output head is on the first": head, "holds no layers": empty, "no parts": {}} {
		if _, err := PlanStagePipe(c, m, testNPUAdapter, StagePipeOptions{}); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestStagePipeSwappableAdapter(t *testing.T) {
	ad := testNPUAdapter
	ad.Provisional = false
	ad.LayersEnv, ad.NextEnv, ad.TimeoutEnv = "X_LAYERS", "X_NEXT", "X_TIMEOUT"
	ad.StageBin, ad.StagePort, ad.ListenFmt = "xstage", 7000, "tcp://0.0.0.0:{port}"
	p, err := PlanStagePipe(reportCandidate(), toyModel(), ad, StagePipeOptions{Host: hostMap, ModelDir: "/m", TimeoutS: 30})
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Stages[1].Command; got != "X_LAYERS=24:36 X_NEXT=tcp://10.0.4.22:7000 X_TIMEOUT=30 xstage /m/target tcp://0.0.0.0:7000" {
		t.Fatal(got)
	}
	if len(p.Warnings) != 0 {
		t.Fatal(p.Warnings)
	}
}

func TestStagePipeOneMachineTwoStagesAndOneStage(t *testing.T) {
	c := plan.Candidate{Parts: []plan.Part{{Node: "a", Layers: 10, First: 0}, {Node: "b", Layers: 10, First: 10}, {Node: "b", Layers: 4, First: 20, Output: true}}}
	p, err := PlanStagePipe(c, &plan.Model{Name: "m", Layers: 24}, testNPUAdapter, StagePipeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Stages[1].Port != 5600 || p.Stages[2].Port != 5601 || p.Stages[1].Next != "tcp://b:5601" {
		t.Fatalf("%+v", p.Stages)
	}
	one, err := PlanStagePipe(plan.Candidate{Parts: []plan.Part{{Node: "a", Layers: 24, Output: true}}}, &plan.Model{Name: "m", Layers: 24}, testNPUAdapter, StagePipeOptions{})
	if err != nil || one.Stages[0].Next != "" || strings.Contains(one.Stages[0].Command, "NEXT") || len(one.StartOrder) != 1 {
		t.Fatalf("%v %+v", err, one)
	}
}

func TestStagePipeFromPlannerOutput(t *testing.T) {
	// planner -> stage plan: the 3-board shape with the PDF's bandwidths, balanced
	m := &plan.Model{Name: "pdf-27b", Arch: "q", Layers: 64, HiddenBytes: 10240, Runtime: plan.RuntimeStagePipe}
	for i := 0; i < 64; i++ {
		m.LayerBytes = append(m.LayerBytes, 231_400_000)
		m.LayerRead = append(m.LayerRead, 231_400_000)
	}
	b := &hostinfo.Bench{FastWhere: "NPU"}
	nodes := []plan.Node{
		{Name: "delta1", CapGB: 8, BW: 34.0, WeightBW: 34.0, Bench: b},
		{Name: "pcie", CapGB: 8, BW: 32.0, WeightBW: 32.0, Bench: b},
		{Name: "delta2", CapGB: 8, BW: 22.6, WeightBW: 22.6, Bench: b},
	}
	r := plan.Plan(m, nodes, plan.Options{Ctx: 512, Master: "delta1", Balance: true})
	var c *plan.Candidate
	for i := range r.Candidates {
		if len(r.Candidates[i].Parts) == 3 {
			c = &r.Candidates[i]
		}
	}
	if c == nil {
		t.Fatal("no 3-stage candidate")
	}
	p, err := PlanStagePipe(*c, m, testNPUAdapter, StagePipeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Stages[0].Node != "delta1" || p.Stages[2].End != 64 {
		t.Fatalf("%+v", p.Stages)
	}
	if p.Stages[2].End-p.Stages[2].Begin >= p.Stages[0].End-p.Stages[0].Begin {
		t.Fatalf("the 600 MHz board must be given fewer layers than delta1: %+v", p.Stages)
	}
}

func TestLiveStageRunner_WaitReadyTimeout(t *testing.T) {
	runner := &LiveStageRunner{}
	s := StageLaunch{
		Index: 0,
		Node:  "localhost",
		Ready: Readiness{Kind: "tcp", Addr: "127.0.0.1:59999"},
	}
	start := time.Now()
	err := runner.WaitReady(s, 100*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("expected timeout error, got: %v", err)
	}
	if time.Since(start) < 90*time.Millisecond {
		t.Fatalf("did not wait for timeout: %v", time.Since(start))
	}
}

func TestChainStageV1Commands(t *testing.T) {
	p, err := PlanStagePipe(reportCandidate(), toyModel(), ChainStageV1, StagePipeOptions{Host: hostMap, ModelDir: "/models/Qwen-27B.gguf", EmbeddingOffload: true})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"STAGE_LAYERS=0:24 STAGE_NEXT=tcp://10.0.4.21:5600 STAGE_TIMEOUT_S=600 STAGE_EMBEDDING_OFFLOAD=on fleet-engine-stage chain -m /models/Qwen-27B.gguf --http 0.0.0.0:52310",
		"STAGE_LAYERS=24:36 STAGE_NEXT=tcp://10.0.4.22:5600 STAGE_TIMEOUT_S=600 fleet-engine-stage serve -m /models/Qwen-27B.gguf 'tcp://*:5600'",
		"STAGE_LAYERS=36:64 fleet-engine-stage serve -m /models/Qwen-27B.gguf 'tcp://*:5600'",
	}
	for i, s := range p.Stages {
		if s.Command != want[i] {
			t.Errorf("stage %d:\n got %s\nwant %s", i, s.Command, want[i])
		}
		want := "tcp"
		if i == 0 { // the chain driver's HTTP front
			want = "http"
		}
		if s.Ready.Kind != want || (i == 0 && s.Ready.Path != "/health") {
			t.Errorf("stage %d readiness = %s %s, want %s", i, s.Ready.Kind, s.Ready.Path, want)
		}
	}
}

func TestStagePipeMoECommands(t *testing.T) {
	// MoE with CPUMoE and MoECacheMiB
	p, err := PlanStagePipe(reportCandidate(), toyModel(), ChainStageV1, StagePipeOptions{
		Host:        hostMap,
		ModelDir:    "/models/Qwen-27B.gguf",
		CPUMoE:      true,
		MoECacheMiB: 3900,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"STAGE_LAYERS=0:24 STAGE_NEXT=tcp://10.0.4.21:5600 STAGE_TIMEOUT_S=600 fleet-engine-stage chain -m /models/Qwen-27B.gguf --http 0.0.0.0:52310 --cpu-moe --moe-cache-mib 3900",
		"STAGE_LAYERS=24:36 STAGE_NEXT=tcp://10.0.4.22:5600 STAGE_TIMEOUT_S=600 fleet-engine-stage serve -m /models/Qwen-27B.gguf 'tcp://*:5600' --cpu-moe --moe-cache-mib 3900",
		"STAGE_LAYERS=36:64 fleet-engine-stage serve -m /models/Qwen-27B.gguf 'tcp://*:5600' --cpu-moe --moe-cache-mib 3900",
	}
	for i, s := range p.Stages {
		if s.Command != want[i] {
			t.Errorf("stage %d:\n got %s\nwant %s", i, s.Command, want[i])
		}
	}

	// MoE with NCPUMoE and MoECacheMiB
	p2, err := PlanStagePipe(reportCandidate(), toyModel(), ChainStageV1, StagePipeOptions{
		Host:        hostMap,
		ModelDir:    "/models/Qwen-27B.gguf",
		NCPUMoE:     4,
		MoECacheMiB: 2048,
	})
	if err != nil {
		t.Fatal(err)
	}
	want2 := []string{
		"STAGE_LAYERS=0:24 STAGE_NEXT=tcp://10.0.4.21:5600 STAGE_TIMEOUT_S=600 fleet-engine-stage chain -m /models/Qwen-27B.gguf --http 0.0.0.0:52310 --n-cpu-moe 4 --moe-cache-mib 2048",
		"STAGE_LAYERS=24:36 STAGE_NEXT=tcp://10.0.4.22:5600 STAGE_TIMEOUT_S=600 fleet-engine-stage serve -m /models/Qwen-27B.gguf 'tcp://*:5600' --n-cpu-moe 4 --moe-cache-mib 2048",
		"STAGE_LAYERS=36:64 fleet-engine-stage serve -m /models/Qwen-27B.gguf 'tcp://*:5600' --n-cpu-moe 4 --moe-cache-mib 2048",
	}
	for i, s := range p2.Stages {
		if s.Command != want2[i] {
			t.Errorf("stage %d:\n got %s\nwant %s", i, s.Command, want2[i])
		}
	}
}

func TestStagePipeMoEValidation(t *testing.T) {
	cand := reportCandidate()
	m := toyModel()

	tests := []struct {
		name string
		opts StagePipeOptions
		ad   StagePipeAdapter
		want string
	}{
		{
			name: "both-cpu-and-n-cpu",
			opts: StagePipeOptions{CPUMoE: true, NCPUMoE: 4},
			ad:   ChainStageV1,
			want: "cannot be used together",
		},
		{
			name: "negative-n-cpu",
			opts: StagePipeOptions{NCPUMoE: -1},
			ad:   ChainStageV1,
			want: "cannot be negative",
		},
		{
			name: "negative-moe-cache",
			opts: StagePipeOptions{CPUMoE: true, MoECacheMiB: -100},
			ad:   ChainStageV1,
			want: "cannot be negative",
		},
		{
			name: "cache-without-moe",
			opts: StagePipeOptions{MoECacheMiB: 3900},
			ad:   ChainStageV1,
			want: "--moe-cache-mib requires --cpu-moe or --n-cpu-moe",
		},
		{
			name: "module-runtime-moe-rejection",
			opts: StagePipeOptions{CPUMoE: true},
			ad:   testNPUAdapter,
			want: "not supported",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := PlanStagePipe(cand, m, tc.ad, tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got: %v", tc.want, err)
			}
		})
	}
}

func TestStagePipeMoEWarning(t *testing.T) {
	cand := reportCandidate()
	moeM := &plan.Model{Name: "MoE-Model", Layers: 64, Experts: 8}
	denseM := &plan.Model{Name: "Dense-Model", Layers: 64, Experts: 0}

	// 1. MoE model without MoE flags -> warning expected
	p1, err := PlanStagePipe(cand, moeM, ChainStageV1, StagePipeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range p1.Warnings {
		if strings.Contains(w, "--cpu-moe --moe-cache-mib N is recommended") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected MoE warning in plan warnings, got: %v", p1.Warnings)
	}

	// 2. MoE model WITH MoE flags -> no MoE warning
	p2, err := PlanStagePipe(cand, moeM, ChainStageV1, StagePipeOptions{CPUMoE: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range p2.Warnings {
		if strings.Contains(w, "--cpu-moe --moe-cache-mib N is recommended") {
			t.Errorf("unexpected MoE warning when CPUMoE is set: %s", w)
		}
	}

	// 3. Dense model without MoE flags -> no MoE warning
	p3, err := PlanStagePipe(cand, denseM, ChainStageV1, StagePipeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range p3.Warnings {
		if strings.Contains(w, "--cpu-moe --moe-cache-mib N is recommended") {
			t.Errorf("unexpected MoE warning on dense model: %s", w)
		}
	}
}

type fakeRecorder struct {
	records []DeployResult
}

func (f *fakeRecorder) RecordRun(res DeployResult) error {
	f.records = append(f.records, res)
	return nil
}

func writeChainRecorderDescriptor(t *testing.T, tmpDir string) {
	t.Helper()
	d := plan.Descriptor{
		Version:        1,
		Name:           "toy-chain-model",
		Architecture:   "llama",
		Runtime:        plan.RuntimeStagePipe,
		LayerCount:     64,
		HiddenSize:     4096,
		VocabSize:      32000,
		ContextMax:     4096,
		EmbeddingAt:    "first",
		EmbeddingBytes: 1 << 20,
		HeadAt:         "last",
		HeadBytes:      2 << 20,
		Layers: []plan.DescriptorLayer{
			{Index: 0, File: "l0.bin", Bytes: 10 << 20, State: plan.StateAttentionKV, KVBytesPerToken: 128},
		},
	}
	for i := 1; i < 64; i++ {
		d.Layers = append(d.Layers, plan.DescriptorLayer{
			Index: i, File: "l.bin", Bytes: 10 << 20, State: plan.StateAttentionKV, KVBytesPerToken: 128,
		})
	}
	descBytes, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, plan.DescriptorFile), descBytes, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDeployChain_RunRecorder(t *testing.T) {
	tmpDir := t.TempDir()
	writeChainRecorderDescriptor(t, tmpDir)

	cand := reportCandidate()
	opts := StagePipeOptions{Host: hostMap, EndpointName: "chain-test-ep"}

	// 1. Successful dry run records StatusRunning with Engine "chain"
	runner := &DryRunner{}
	rec := &fakeRecorder{}
	res, err := DeployChain(context.Background(), cand, tmpDir, opts, runner, rec)
	if err != nil {
		t.Fatalf("DeployChain failed: %v", err)
	}
	if res == nil {
		t.Fatal("expected non-nil DeployResult")
	}
	if res.Engine != "chain" {
		t.Fatalf("expected Engine 'chain', got %q", res.Engine)
	}
	if res.Status != StatusRunning {
		t.Fatalf("expected StatusRunning, got %q", res.Status)
	}
	// the "starting" record (which holds the ports for the other deploys) and the final one
	if len(rec.records) != 2 {
		t.Fatalf("expected 2 records in RunRecorder (starting, running), got %d", len(rec.records))
	}
	if rec.records[0].Status != StatusStarting || rec.records[0].RunID != res.RunID || len(rec.records[0].Ports) == 0 {
		t.Fatalf("first record must be the starting one with its ports: %+v", rec.records[0])
	}
	recorded := rec.records[1]
	if recorded.RunID != res.RunID {
		t.Fatalf("expected RunID %s, got %s", res.RunID, recorded.RunID)
	}
	if recorded.Engine != "chain" {
		t.Fatalf("expected recorded Engine 'chain', got %q", recorded.Engine)
	}
	if recorded.EndpointName != "chain-test-ep" {
		t.Fatalf("expected EndpointName 'chain-test-ep', got %q", recorded.EndpointName)
	}
	if recorded.MasterNode != "delta1" {
		t.Fatalf("expected MasterNode 'delta1', got %q", recorded.MasterNode)
	}
	if len(recorded.WorkerNodes) != 2 {
		t.Fatalf("expected 2 worker nodes, got %d", len(recorded.WorkerNodes))
	}
	if recorded.Status != StatusRunning {
		t.Fatalf("expected recorded StatusRunning, got %q", recorded.Status)
	}
	if !strings.HasPrefix(recorded.URL, "http://10.0.4.20:") {
		t.Fatalf("expected URL pointing to delta1, got %q", recorded.URL)
	}

	// 2. Failed run records StatusFailed
	failingRunner := &failing{failReady: 1}
	failRec := &fakeRecorder{}
	resFail, err := DeployChain(context.Background(), cand, tmpDir, opts, failingRunner, failRec)
	if err == nil {
		t.Fatal("expected error from failing runner")
	}
	if resFail.Status != StatusFailed {
		t.Fatalf("expected StatusFailed, got %q", resFail.Status)
	}
	if len(failRec.records) != 2 {
		t.Fatalf("expected 2 records in failRec (starting, failed), got %d", len(failRec.records))
	}
	if failRec.records[1].Status != StatusFailed {
		t.Fatalf("expected recorded StatusFailed, got %q", failRec.records[1].Status)
	}
}

// The live runner runs a stage command as the plan prints it (VAR=value assignments first) in a real shell: before,
// `nohup STAGE_LAYERS=… bin` made nohup look for a program named STAGE_LAYERS=… and every chain deploy timed out.
func TestLiveStageRunnerStartsEnvPrefixedCommand(t *testing.T) {
	port := 40000 + os.Getpid()%20000
	t.Cleanup(func() { os.Remove(fmt.Sprintf("/tmp/fleet-stage-%d.log", port)) })
	r := &LiveStageRunner{}
	if err := r.Launch(StageLaunch{Index: 0, Node: "localhost", Port: port, Command: "FLEET_TEST_STAGE=1 sleep 3"}); err != nil {
		t.Fatalf("an env-prefixed command must start: %v", err)
	}
	if len(r.Started) != 1 {
		t.Fatalf("started stages: %d", len(r.Started))
	}
}

// A stage that exits at once is reported by Launch with its log, not by the ready timeout minutes later.
func TestLiveStageRunnerReportsStageThatDies(t *testing.T) {
	port := 40001 + os.Getpid()%20000
	t.Cleanup(func() { os.Remove(fmt.Sprintf("/tmp/fleet-stage-%d.log", port)) })
	r := &LiveStageRunner{}
	err := r.Launch(StageLaunch{Index: 1, Node: "localhost", Port: port, Command: "FLEET_TEST_STAGE=1 /nonexistent/fleet-engine-stage serve"})
	if err == nil || !strings.Contains(err.Error(), "exited at start") || !strings.Contains(err.Error(), "nonexistent") {
		t.Fatalf("a stage that dies must be reported with its log: %v", err)
	}
	if len(r.Started) != 0 {
		t.Fatalf("a dead stage is not started: %d", len(r.Started))
	}
}

func writeTestGGUF(t *testing.T, path string, arch string, targetLayers []uint32, tensors ...string) {
	t.Helper()
	var b bytes.Buffer
	u32 := func(v uint32) { binary.Write(&b, binary.LittleEndian, v) }
	u64 := func(v uint64) { binary.Write(&b, binary.LittleEndian, v) }
	str := func(s string) { u64(uint64(len(s))); b.WriteString(s) }
	b.WriteString("GGUF")
	u32(3)                    // version
	u64(uint64(len(tensors))) // tensor count
	kvs := 1
	if len(targetLayers) > 0 {
		kvs++
	}
	u64(uint64(kvs)) // kv count
	str("general.architecture")
	u32(8) // string
	str(arch)
	if len(targetLayers) > 0 {
		str("dflash.target_layers")
		u32(9) // array
		u32(4) // uint32
		u64(uint64(len(targetLayers)))
		for _, tl := range targetLayers {
			u32(tl)
		}
	}
	for _, name := range tensors {
		str(name)
		u32(1) // 1 dim
		u64(4) // ne[0] = 4
		u32(0) // type F32
		u64(0) // offset 0
	}
	pad := (32 - (b.Len() % 32)) % 32
	b.Write(make([]byte, pad))
	b.Write(make([]byte, 16))
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPlanStagePipeDFlashCommands(t *testing.T) {
	dir := t.TempDir()
	tgtModel := filepath.Join(dir, "model.gguf")
	writeTestGGUF(t, tgtModel, "qwen2", nil, "output.weight")
	draftModel := filepath.Join(dir, "draft.gguf")
	writeTestGGUF(t, draftModel, "dflash", []uint32{1, 2}, "fc.weight")

	// Mock embedHeadFn so it creates a headed dummy file without needing real weights
	oldEmbed := embedHeadFn
	embedCalls := 0
	embedHeadFn = func(draft, target, out string) (ggufhead.Result, error) {
		embedCalls++
		writeTestGGUF(t, out, "dflash", []uint32{1, 2}, "fc.weight", "output.weight")
		return ggufhead.Result{}, nil
	}
	t.Cleanup(func() { embedHeadFn = oldEmbed; dflashCacheDir = "" })
	dflashCacheDir = filepath.Join(dir, "cache")

	// 1. Multi-stage pipeline: default DraftMax for GPU = 4
	cand := reportCandidate() // 3 stages: delta1, pcie, delta2
	opts := StagePipeOptions{
		Host:       hostMap,
		ModelDir:   tgtModel,
		DraftModel: draftModel,
		Spec:       "draft-dflash",
	}
	p, err := PlanStagePipe(cand, toyModel(), ChainStageV1, opts)
	if err != nil {
		t.Fatalf("PlanStagePipe failed: %v", err)
	}
	if len(p.Stages) != 3 {
		t.Fatalf("expected 3 stages, got %d", len(p.Stages))
	}
	// Ingress stage must have headed draft and draft-max 4
	s0 := p.Stages[0].Command
	if !strings.Contains(s0, "--dflash-draft") || !strings.Contains(s0, "-head.gguf") {
		t.Errorf("ingress command lacks headed draft: %s", s0)
	}
	if !strings.Contains(s0, "--draft-max 4") {
		t.Errorf("ingress command lacks default --draft-max 4: %s", s0)
	}
	if strings.Contains(s0, "-c") || strings.Contains(s0, "--wire") {
		t.Errorf("ingress should not contain -c or --wire when not set: %s", s0)
	}
	// Middle and last stages must have --dflash-layers 1,2 and no --n-rs-seq
	for i := 1; i <= 2; i++ {
		cmd := p.Stages[i].Command
		if !strings.Contains(cmd, "--dflash-layers 1,2") {
			t.Errorf("stage %d command lacks --dflash-layers 1,2: %s", i, cmd)
		}
		if strings.Contains(cmd, "--n-rs-seq") {
			t.Errorf("stage %d command should not have --n-rs-seq when draft-max <= 4: %s", i, cmd)
		}
	}

	// 2. CPU-MoE default DraftMax = 2
	optsMoE := StagePipeOptions{
		Host:        hostMap,
		ModelDir:    tgtModel,
		DraftModel:  draftModel,
		Spec:        "draft-dflash",
		CPUMoE:      true,
		MoECacheMiB: 2048,
	}
	pMoE, err := PlanStagePipe(cand, toyModel(), ChainStageV1, optsMoE)
	if err != nil {
		t.Fatalf("PlanStagePipe MoE failed: %v", err)
	}
	if !strings.Contains(pMoE.Stages[0].Command, "--draft-max 2") {
		t.Errorf("expected --draft-max 2 for CPU-MoE, got %s", pMoE.Stages[0].Command)
	}

	// 3. DraftMax > 4: passes --n-rs-seq to serve stages
	optsMax6 := StagePipeOptions{
		Host:       hostMap,
		ModelDir:   tgtModel,
		DraftModel: draftModel,
		DraftMax:   6,
	}
	pMax6, err := PlanStagePipe(cand, toyModel(), ChainStageV1, optsMax6)
	if err != nil {
		t.Fatalf("PlanStagePipe max 6 failed: %v", err)
	}
	if !strings.Contains(pMax6.Stages[0].Command, "--draft-max 6") {
		t.Errorf("ingress lacks --draft-max 6: %s", pMax6.Stages[0].Command)
	}
	for i := 1; i <= 2; i++ {
		if !strings.Contains(pMax6.Stages[i].Command, "--n-rs-seq 6") {
			t.Errorf("stage %d lacks --n-rs-seq 6: %s", i, pMax6.Stages[i].Command)
		}
	}

	// 4. Ctx and Wire passed to all stages
	optsCtxWire := StagePipeOptions{
		Host:       hostMap,
		ModelDir:   tgtModel,
		DraftModel: draftModel,
		Ctx:        4096,
		Wire:       "bf16",
	}
	pCtxWire, err := PlanStagePipe(cand, toyModel(), ChainStageV1, optsCtxWire)
	if err != nil {
		t.Fatalf("PlanStagePipe ctx/wire failed: %v", err)
	}
	for i, st := range pCtxWire.Stages {
		if !strings.Contains(st.Command, "-c 4096") {
			t.Errorf("stage %d lacks -c 4096: %s", i, st.Command)
		}
		if !strings.Contains(st.Command, "--wire bf16") {
			t.Errorf("stage %d lacks --wire bf16: %s", i, st.Command)
		}
	}

	// 5. Single stage: head is not needed, draft used as-is
	singleCand := plan.Candidate{
		Kind:   "single",
		Master: "delta1",
		Parts:  []plan.Part{{Node: "delta1", First: 0, Layers: 64, Output: true}},
	}
	pSingle, err := PlanStagePipe(singleCand, toyModel(), ChainStageV1, opts)
	if err != nil {
		t.Fatalf("PlanStagePipe single failed: %v", err)
	}
	if !strings.Contains(pSingle.Stages[0].Command, "--dflash-draft "+draftModel) {
		t.Errorf("single stage should use draft directly without head: %s", pSingle.Stages[0].Command)
	}
}

func TestPlanStagePipeDFlashValidation(t *testing.T) {
	dir := t.TempDir()
	tgtModel := filepath.Join(dir, "model.gguf")
	writeTestGGUF(t, tgtModel, "qwen2", nil, "output.weight")
	cand := reportCandidate()

	// Missing draft
	_, err := PlanStagePipe(cand, toyModel(), ChainStageV1, StagePipeOptions{
		ModelDir:   tgtModel,
		Spec:       "draft-dflash",
		DraftModel: filepath.Join(dir, "missing.gguf"),
	})
	if err == nil || !strings.Contains(err.Error(), "draft model") {
		t.Fatalf("expected missing draft error, got %v", err)
	}

	// Spec draft-dflash without DraftModel
	_, err = PlanStagePipe(cand, toyModel(), ChainStageV1, StagePipeOptions{
		ModelDir: tgtModel,
		Spec:     "draft-dflash",
	})
	if err == nil || !strings.Contains(err.Error(), "--spec draft-dflash requires a draft model") {
		t.Fatalf("expected requires draft model error, got %v", err)
	}

	// Invalid wire
	_, err = PlanStagePipe(cand, toyModel(), ChainStageV1, StagePipeOptions{
		ModelDir: tgtModel,
		Wire:     "fp8",
	})
	if err == nil || !strings.Contains(err.Error(), "invalid wire format") {
		t.Fatalf("expected invalid wire format error, got %v", err)
	}

	// DraftMax < 1
	_, err = PlanStagePipe(cand, toyModel(), ChainStageV1, StagePipeOptions{
		ModelDir: tgtModel,
		DraftMax: -1,
	})
	if err == nil || !strings.Contains(err.Error(), "--draft-max must be >= 1") {
		t.Fatalf("expected draft-max error, got %v", err)
	}

	// Non-dflash architecture
	badArchDraft := filepath.Join(dir, "badarch.gguf")
	writeTestGGUF(t, badArchDraft, "llama", []uint32{1, 2}, "fc.weight")
	_, err = PlanStagePipe(cand, toyModel(), ChainStageV1, StagePipeOptions{
		ModelDir:   tgtModel,
		DraftModel: badArchDraft,
	})
	if err == nil || !strings.Contains(err.Error(), "expected dflash") {
		t.Fatalf("expected arch error, got %v", err)
	}

	// Missing dflash.target_layers
	noLayersDraft := filepath.Join(dir, "nolayers.gguf")
	writeTestGGUF(t, noLayersDraft, "dflash", nil, "fc.weight")
	_, err = PlanStagePipe(cand, toyModel(), ChainStageV1, StagePipeOptions{
		ModelDir:   tgtModel,
		DraftModel: noLayersDraft,
	})
	if err == nil || !strings.Contains(err.Error(), "has no dflash.target_layers") {
		t.Fatalf("expected missing target_layers error, got %v", err)
	}

	// ZMQ protocol rejects draft-dflash
	validDraft := filepath.Join(dir, "valid_draft.gguf")
	writeTestGGUF(t, validDraft, "dflash", []uint32{1, 2}, "fc.weight")
	_, err = PlanStagePipe(cand, toyModel(), testNPUAdapter, StagePipeOptions{
		ModelDir:   tgtModel,
		DraftModel: validDraft,
	})
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("expected dflash rejection, got %v", err)
	}
}

func TestDFlashHeadCacheReuse(t *testing.T) {
	dir := t.TempDir()
	tgtModel := filepath.Join(dir, "model.gguf")
	writeTestGGUF(t, tgtModel, "qwen2", nil, "output.weight")
	draftModel := filepath.Join(dir, "draft.gguf")
	writeTestGGUF(t, draftModel, "dflash", []uint32{1, 2}, "fc.weight")

	oldEmbed := embedHeadFn
	embedCalls := 0
	embedHeadFn = func(draft, target, out string) (ggufhead.Result, error) {
		embedCalls++
		writeTestGGUF(t, out, "dflash", []uint32{1, 2}, "fc.weight", "output.weight")
		return ggufhead.Result{}, nil
	}
	t.Cleanup(func() { embedHeadFn = oldEmbed; dflashCacheDir = "" })
	dflashCacheDir = filepath.Join(dir, "cache")

	cand := reportCandidate()
	opts := StagePipeOptions{
		Host:       hostMap,
		ModelDir:   tgtModel,
		DraftModel: draftModel,
	}

	// First call -> calls embedHeadFn
	p1, err := PlanStagePipe(cand, toyModel(), ChainStageV1, opts)
	if err != nil {
		t.Fatalf("first call failed: %v", err)
	}
	if embedCalls != 1 {
		t.Fatalf("expected embedCalls = 1 on first call, got %d", embedCalls)
	}

	// Second call -> reuses cached file, embedHeadFn not called again
	p2, err := PlanStagePipe(cand, toyModel(), ChainStageV1, opts)
	if err != nil {
		t.Fatalf("second call failed: %v", err)
	}
	if embedCalls != 1 {
		t.Fatalf("expected embedCalls = 1 on second call (cached), got %d", embedCalls)
	}
	if p1.Stages[0].Command != p2.Stages[0].Command {
		t.Fatalf("commands differ between cached runs: %s vs %s", p1.Stages[0].Command, p2.Stages[0].Command)
	}
}

func writeTestDescriptor(t *testing.T, dir, name string, layers int) string {
	t.Helper()
	d := plan.Descriptor{
		Version:        1,
		Name:           name,
		Architecture:   "llama",
		Runtime:        plan.RuntimeStagePipe,
		LayerCount:     layers,
		HiddenSize:     4096,
		VocabSize:      32000,
		ContextMax:     4096,
		EmbeddingAt:    "first",
		EmbeddingBytes: 1 << 20,
		HeadAt:         "last",
		HeadBytes:      2 << 20,
	}
	for i := 0; i < layers; i++ {
		d.Layers = append(d.Layers, plan.DescriptorLayer{
			Index: i, File: "l.bin", Bytes: 10 << 20, State: plan.StateAttentionKV, KVBytesPerToken: 128,
		})
	}
	descBytes, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, plan.DescriptorFile), descBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestDeployChainModelMissingCheck(t *testing.T) {
	dir := t.TempDir()
	modelDir := writeTestDescriptor(t, dir, "toy", 64)

	cand := reportCandidate() // delta1, pcie, delta2
	dryRunner := &DryRunner{
		FakeExists: func(node, path string) bool {
			// pcie is missing the model file
			return node != "pcie"
		},
	}

	_, err := DeployChain(context.Background(), cand, modelDir, StagePipeOptions{Host: hostMap}, dryRunner, nil)
	if err == nil || !strings.Contains(err.Error(), "model missing on pcie") || !strings.Contains(err.Error(), "fleet store") {
		t.Fatalf("expected model missing on pcie error, got %v", err)
	}

	// Verify that NO stages were launched
	for _, l := range dryRunner.Lines {
		if strings.Contains(l, "start stage") {
			t.Fatalf("no stages should be launched when model is missing on a node, found line: %s", l)
		}
	}
}

func TestDeployChainEndpointRegistration(t *testing.T) {
	dir := t.TempDir()
	modelDir := writeTestDescriptor(t, dir, "toy", 64)
	cand := reportCandidate()

	// 1. Successful deploy registers endpoint in config
	cfgPath := filepath.Join(dir, "fleet.yaml")
	dryRunner := &DryRunner{}
	opts := StagePipeOptions{
		Host:         hostMap,
		ConfigPath:   cfgPath,
		Role:         "coder",
		Port:         52310,
		EndpointName: "custom-chain-ep",
	}

	res, err := DeployChain(context.Background(), cand, modelDir, opts, dryRunner, nil)
	if err != nil {
		t.Fatalf("DeployChain failed: %v", err)
	}
	if res.EndpointName != "custom-chain-ep" {
		t.Fatalf("expected EndpointName 'custom-chain-ep', got %q", res.EndpointName)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("failed to load saved config: %v", err)
	}
	if len(cfg.Endpoints) != 1 {
		t.Fatalf("expected 1 endpoint in config, got %d", len(cfg.Endpoints))
	}
	ep := cfg.Endpoints[0]
	if ep.Name != "custom-chain-ep" {
		t.Fatalf("expected ep.Name 'custom-chain-ep', got %q", ep.Name)
	}
	if ep.URL != "http://10.0.4.20:52310/v1" {
		t.Fatalf("expected ep.URL with /v1, got %q", ep.URL)
	}
	if ep.Group == nil || ep.Group.Engine != "chain" {
		t.Fatalf("expected Group with Engine 'chain', got %+v", ep.Group)
	}
	if len(ep.Group.Workers) != 2 {
		t.Fatalf("expected 2 workers in group, got %d", len(ep.Group.Workers))
	}
	if ep.Group.Addrs["pcie"] != "10.0.4.21:5600" || ep.Group.Addrs["delta2"] != "10.0.4.22:5600" {
		t.Fatalf("unexpected group addrs: %v", ep.Group.Addrs)
	}
	if cfg.Roles["coder"].Preferred != "custom-chain-ep" {
		t.Fatalf("expected role 'coder' to prefer 'custom-chain-ep', got %q", cfg.Roles["coder"].Preferred)
	}

	// 2. Failed deploy does not register endpoint
	cfgPathFail := filepath.Join(dir, "fleet-fail.yaml")
	failingRunner := &failing{failReady: 1}
	optsFail := StagePipeOptions{
		Host:       hostMap,
		ConfigPath: cfgPathFail,
	}
	_, err = DeployChain(context.Background(), cand, modelDir, optsFail, failingRunner, nil)
	if err == nil {
		t.Fatal("expected deploy error with failing runner")
	}
	if _, serr := os.Stat(cfgPathFail); serr == nil {
		cfgFail, _ := config.Load(cfgPathFail)
		if len(cfgFail.Endpoints) > 0 {
			t.Fatalf("failed deploy must not register endpoint, found %d", len(cfgFail.Endpoints))
		}
	}
}

// The head-embedded draft is made on the deploying machine: a first stage elsewhere must have it at that path, else no
// stage starts (before, the ingress started with a --dflash-draft path that did not exist there).
func TestDeployChainDraftMissingOnIngress(t *testing.T) {
	dir := t.TempDir()
	modelDir := writeTestDescriptor(t, dir, "toy", 64)
	draftModel := filepath.Join(dir, "draft.gguf")
	writeTestGGUF(t, draftModel, "dflash", []uint32{1, 2}, "fc.weight")
	oldEmbed := embedHeadFn
	embedHeadFn = func(draft, target, out string) (ggufhead.Result, error) {
		writeTestGGUF(t, out, "dflash", []uint32{1, 2}, "fc.weight", "output.weight")
		return ggufhead.Result{}, nil
	}
	t.Cleanup(func() { embedHeadFn = oldEmbed; dflashCacheDir = "" })
	dflashCacheDir = filepath.Join(dir, "cache")

	dryRunner := &DryRunner{FakeExists: func(node, path string) bool {
		return !(node == "delta1" && strings.HasSuffix(path, "-head.gguf"))
	}}
	opts := StagePipeOptions{Host: hostMap, DraftModel: draftModel, Spec: "draft-dflash", ConfigPath: filepath.Join(dir, "fleet.yaml")}
	_, err := DeployChain(context.Background(), reportCandidate(), modelDir, opts, dryRunner, nil)
	if err == nil || !strings.Contains(err.Error(), "draft missing on delta1") {
		t.Fatalf("a draft missing on the ingress machine must stop the deploy: %v", err)
	}
	for _, l := range dryRunner.Lines {
		if strings.Contains(l, "start stage") {
			t.Fatalf("no stage may start: %s", l)
		}
	}
}

// fleet-engine-stage --wire knows f32, bf16 and auto; f16 would silently become bf16 there.
func TestPlanStagePipeWireValues(t *testing.T) {
	for wire, ok := range map[string]bool{"": true, "f32": true, "bf16": true, "auto": true, "f16": false, "fp8": false} {
		_, err := PlanStagePipe(reportCandidate(), toyModel(), ChainStageV1, StagePipeOptions{Host: hostMap, Wire: wire})
		if (err == nil) != ok {
			t.Errorf("--wire %q: err %v, want ok=%v", wire, err, ok)
		}
	}
}

// --dry-run prints only: before, it registered the endpoint and wrote a run record like a real deploy.
func TestDeployChainDryRunRegistersNothing(t *testing.T) {
	dir := t.TempDir()
	modelDir := writeTestDescriptor(t, dir, "toy", 64)
	cfgPath := filepath.Join(dir, "fleet.yaml")
	rec := &fakeRecorder{}
	res, err := DeployChain(context.Background(), reportCandidate(), modelDir, StagePipeOptions{Host: hostMap, ConfigPath: cfgPath, DryRun: true}, &DryRunner{}, rec)
	if err != nil || res.Status != "dry-run" {
		t.Fatalf("dry run: %v %+v", err, res)
	}
	if _, err := os.Stat(cfgPath); err == nil {
		t.Fatal("a dry run must not write the configuration")
	}
	if len(rec.records) != 0 {
		t.Fatalf("a dry run must not record a run: %d", len(rec.records))
	}
}

// --np N: every stage takes --n-seq N (a sequence per concurrent request), the endpoint gets N slots; a DFlash draft takes it too (engine 0.5.2: a draft per sequence).
func TestPlanStagePipeNp(t *testing.T) {
	p, err := PlanStagePipe(reportCandidate(), toyModel(), ChainStageV1, StagePipeOptions{Host: hostMap, Np: 3})
	if err != nil {
		t.Fatal(err)
	}
	for i, st := range p.Stages {
		if !strings.Contains(st.Command, "--n-seq 3") {
			t.Errorf("stage %d without --n-seq 3: %s", i, st.Command)
		}
	}
	p, err = PlanStagePipe(reportCandidate(), toyModel(), ChainStageV1, StagePipeOptions{Host: hostMap, Np: 1})
	if err != nil {
		t.Fatal(err)
	}
	for i, st := range p.Stages {
		if strings.Contains(st.Command, "--n-seq") {
			t.Errorf("stage %d: one sequence needs no --n-seq (an older engine would not know it): %s", i, st.Command)
		}
	}
	if _, err := PlanStagePipe(reportCandidate(), toyModel(), ChainStageV1, StagePipeOptions{Host: hostMap, Np: -1}); err == nil {
		t.Error("--np -1 must be refused")
	}
	// a DFlash draft takes --np (one draft per sequence in the engine); --lanes stays refused (its rounds do not batch)
	if _, err := PlanStagePipe(reportCandidate(), toyModel(), ChainStageV1, StagePipeOptions{Host: hostMap, Np: 2, Spec: "draft-dflash", DraftModel: "x.gguf"}); err != nil && strings.Contains(err.Error(), "--np 2") {
		t.Errorf("--np 2 with a draft must no longer be refused for --np: %v", err)
	}
}

// --lanes L: ingress stage takes --lanes L (when L > 1), subsequent stages do not; needs Np >= 2.
func TestPlanStagePipeLanes(t *testing.T) {
	// L > 1 with Np >= 2: only ingress has --lanes
	p, err := PlanStagePipe(reportCandidate(), toyModel(), ChainStageV1, StagePipeOptions{Host: hostMap, Np: 4, Lanes: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Stages) < 2 {
		t.Fatalf("expected at least 2 stages, got %d", len(p.Stages))
	}
	if !strings.Contains(p.Stages[0].Command, "--lanes 3") {
		t.Errorf("ingress stage without --lanes 3: %s", p.Stages[0].Command)
	}
	for i := 1; i < len(p.Stages); i++ {
		if strings.Contains(p.Stages[i].Command, "--lanes") {
			t.Errorf("stage %d must NOT have --lanes: %s", i, p.Stages[i].Command)
		}
	}

	// Regression: Lanes 0 or Lanes 1 produces no --lanes on any stage
	for _, l := range []int{0, 1} {
		p, err = PlanStagePipe(reportCandidate(), toyModel(), ChainStageV1, StagePipeOptions{Host: hostMap, Np: 4, Lanes: l})
		if err != nil {
			t.Fatalf("lanes %d failed: %v", l, err)
		}
		for i, st := range p.Stages {
			if strings.Contains(st.Command, "--lanes") {
				t.Errorf("lanes %d: stage %d must NOT have --lanes: %s", l, i, st.Command)
			}
		}
	}
}

func TestPlanStagePipeLanesValidation(t *testing.T) {
	cases := []struct {
		name string
		opts StagePipeOptions
		ad   StagePipeAdapter
		want string
	}{
		{
			name: "negative-lanes",
			opts: StagePipeOptions{Host: hostMap, Np: 2, Lanes: -1},
			ad:   ChainStageV1,
			want: "--lanes must be >= 0",
		},
		{
			name: "lanes-exceeds-max-8",
			opts: StagePipeOptions{Host: hostMap, Np: 10, Lanes: 9},
			ad:   ChainStageV1,
			want: "maximum is 8",
		},
		{
			name: "lanes-needs-np-2-with-np-0",
			opts: StagePipeOptions{Host: hostMap, Np: 0, Lanes: 2},
			ad:   ChainStageV1,
			want: "--lanes needs --np >= 2",
		},
		{
			name: "lanes-needs-np-2-with-np-1",
			opts: StagePipeOptions{Host: hostMap, Np: 1, Lanes: 2},
			ad:   ChainStageV1,
			want: "--lanes needs --np >= 2",
		},
		{
			name: "lanes-with-speculative-decoding",
			opts: StagePipeOptions{Host: hostMap, Np: 4, Lanes: 2, Spec: "draft-dflash", DraftModel: "x.gguf"},
			ad:   ChainStageV1,
			want: "--lanes 2 with speculative decoding: its rounds do not batch",
		},
		{
			name: "lanes-with-module-runtime",
			opts: StagePipeOptions{Host: hostMap, Np: 4, Lanes: 2},
			ad:   testNPUAdapter,
			want: "not supported",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := PlanStagePipe(reportCandidate(), toyModel(), tc.ad, tc.opts)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got: %v", tc.want, err)
			}
		})
	}
}

type failingLaunch struct {
	DryRunner
	failStart int
}

func (f *failingLaunch) Launch(s StageLaunch) error {
	if s.Index == f.failStart {
		return errors.New("exited")
	}
	return f.DryRunner.Launch(s)
}

// freed lists the "free port" lines: one before each launch, then one per stage stopped after a failure.
func freed(lines []string) []string {
	var out []string
	for _, l := range lines {
		if strings.Contains(l, "free port") {
			out = append(out, l)
		}
	}
	return out
}

// A failed deploy must not leave the stages it already started running (E10: a stage stayed up on dino).
func TestStagePipeStopsStartedStagesOnFailure(t *testing.T) {
	p, _ := PlanStagePipe(reportCandidate(), toyModel(), testNPUAdapter, StagePipeOptions{Host: hostMap})
	// start order 2, 1, 0: stage 1 not ready -> stages 2 and 1 are stopped, stage 0 never started
	f := &failing{failReady: 1}
	if err := RunStagePipe(p, f, time.Second); err == nil || !strings.Contains(err.Error(), "stopped again") {
		t.Fatalf("%v", err)
	}
	// before launch: stages 2, 1; after the failure: stages 2, 1 again
	if got := freed(f.Lines); len(got) != 4 || got[2] != got[0] || got[3] != got[1] {
		t.Fatalf("not ready: want stages 2 and 1 stopped, got %v", got)
	}
	// stage 1 does not start -> only stage 2 (running) is stopped
	g := &failingLaunch{failStart: 1}
	if err := RunStagePipe(p, g, time.Second); err == nil || !strings.Contains(err.Error(), "1 of 3 started") {
		t.Fatalf("%v", err)
	}
	// before launch: stages 2, 1; after the failure: stage 2 only
	if got := freed(g.Lines); len(got) != 3 || got[2] != got[0] || !strings.HasPrefix(got[2], "["+p.Stages[2].Node+"]") {
		t.Fatalf("start failure: want only stage 2 stopped, got %v", got)
	}
}

// A stage on a machine this one cannot reach directly is checked on that machine over ssh (E10: papa cannot dial
// dino's LAN address, the stage was up and the deploy timed out).
func TestStageReadyFallsBackToSSH(t *testing.T) {
	oldS, oldP := sshScript, readyPoll
	t.Cleanup(func() { sshScript, readyPoll = oldS, oldP })
	readyPoll = time.Millisecond
	var scripts []string
	reply := "FLEET_READY\n"
	sshScript = func(_ context.Context, node, script string) ([]byte, error) {
		if node != "e10-remote-node" {
			t.Errorf("ssh to %s", node)
		}
		scripts = append(scripts, script)
		return []byte(reply), nil
	}
	r := &LiveStageRunner{}
	tcp := StageLaunch{Node: "e10-remote-node", Port: 5600, Ready: Readiness{Kind: "tcp", Addr: "127.0.0.1:1"}}
	if err := r.WaitReady(tcp, 3*time.Second); err != nil {
		t.Fatalf("tcp: %v", err)
	}
	if len(scripts) != 1 || !strings.Contains(scripts[0], "[:.]1$") || !strings.Contains(scripts[0], "-ge 3 ]") {
		t.Fatalf("tcp script (port from the ready address, 3 s): %v", scripts)
	}
	web := StageLaunch{Node: "e10-remote-node", Port: 18092, Ready: Readiness{Kind: "http", Addr: "127.0.0.1:1", Path: "/health"}}
	if err := r.WaitReady(web, time.Second); err != nil {
		t.Fatalf("http: %v", err)
	}
	if !strings.Contains(scripts[1], "curl -fsS -o /dev/null --max-time 3 http://127.0.0.1:1/health") {
		t.Fatalf("http script: %s", scripts[1])
	}
	reply = "FLEET_TIMEOUT\n"
	if err := r.WaitReady(tcp, time.Second); err == nil || !strings.Contains(err.Error(), "checked on e10-remote-node") {
		t.Fatalf("timeout: %v", err)
	}
}

// /proc/PID/comm keeps 15 bytes: a stage process reads "fleet-engine-st", and the port stop must match that.
func TestStopOnPortMatchesTruncatedComm(t *testing.T) {
	stageModulesMu.Lock()
	stageModules["testnpu"] = StageModule{Name: "testnpu", Adapter: testNPUAdapter}
	stageModulesMu.Unlock()
	defer func() { stageModulesMu.Lock(); delete(stageModules, "testnpu"); stageModulesMu.Unlock() }()
	sc := stopOnPortScript(5600, stageProcNames()) // a stage module's binaries are stopped too
	if !strings.Contains(sc, "fleet-engine-st|") || !strings.Contains(sc, "npu-stage-serve|") || strings.Contains(sc, "fleet-engine-stage") {
		t.Fatalf("case pattern must use 15-byte comm names:\n%s", sc)
	}
	if got := commNames([]string{"fleet-engine-stage", "fleet-engine-stagex", "fleet"}); len(got) != 2 || got[0] != "fleet-engine-st" || got[1] != "fleet" {
		t.Fatalf("commNames: %v", got)
	}
}

// A port the stop could not free is an error before the launch, not a bind failure in the stage's log.
func TestStageCleanupReportsBusyPort(t *testing.T) {
	oldS := sshScript
	t.Cleanup(func() { sshScript = oldS })
	reply := "FLEET_BUSY 42 python3\n"
	sshScript = func(context.Context, string, string) ([]byte, error) { return []byte(reply), nil }
	r := &LiveStageRunner{}
	if err := r.Cleanup("e10-remote-node", 5600); err == nil || !strings.Contains(err.Error(), "42 python3") || !strings.Contains(err.Error(), "5600") {
		t.Fatalf("busy: %v", err)
	}
	reply = ""
	if err := r.Cleanup("e10-remote-node", 5600); err != nil {
		t.Fatalf("free: %v", err)
	}
}

type unreachableIngress struct{ DryRunner }

func (unreachableIngress) DirectlyReachable(StageLaunch) bool { return false }

// An ingress this machine cannot dial is registered as an ssh tunnel to its node (E10: papa could not route to the
// chain on bravo's LAN address); one it can dial keeps the direct URL.
func TestDeployChainUnreachableIngressUsesSSH(t *testing.T) {
	dir := t.TempDir()
	modelDir := writeTestDescriptor(t, dir, "toy", 64)
	cfgPath := filepath.Join(dir, "fleet.yaml")
	res, err := DeployChain(context.Background(), reportCandidate(), modelDir, StagePipeOptions{Host: hostMap, ConfigPath: cfgPath, Port: 52310, EndpointName: "far"}, &unreachableIngress{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil || len(cfg.Endpoints) != 1 {
		t.Fatalf("%v %v", err, cfg)
	}
	ep := cfg.Endpoints[0]
	master := reportCandidate().Parts[0].Node
	if ep.URL != "" || ep.SSH != master || ep.Remote != "127.0.0.1:52310" || !slices.Contains(ep.Tags, "lan-only") {
		t.Fatalf("want ssh %s -> 127.0.0.1:52310, no URL, tag lan-only: %+v", master, ep)
	}
	if res.Reach != "ssh-only" || len(res.Warnings) == 0 {
		t.Fatalf("reach %q warnings %v", res.Reach, res.Warnings)
	}
}

// A GGUF whose block_count counts the NextN head (Qwen3.5 MoE: 49 = 48 + 1): the stage engine loads only the 48
// trunk layers, so the last stage must end there ("invalid layer range [25, 49) for a model with 48 layers").
func TestPlanStagePipeTrimsNextNTail(t *testing.T) {
	m := &plan.Model{Name: "Qwen3.5-122B", Layers: 49, MTPLayers: 1, HasMTP: true}
	c := plan.Candidate{Parts: []plan.Part{
		{Node: "a", First: 0, Layers: 25},
		{Node: "b", First: 25, Layers: 24, Output: true},
	}}
	p, err := PlanStagePipe(c, m, ChainStageV1, StagePipeOptions{Host: hostMap})
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Stages[1]; got.Begin != 25 || got.End != 48 {
		t.Errorf("last stage = %d:%d, want 25:48", got.Begin, got.End)
	}
	// the head alone on the last part: that part goes, the one before it holds the output
	c = plan.Candidate{Parts: []plan.Part{
		{Node: "a", First: 0, Layers: 48},
		{Node: "b", First: 48, Layers: 1, Output: true},
	}}
	p, err = PlanStagePipe(c, m, ChainStageV1, StagePipeOptions{Host: hostMap})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Stages) != 1 || p.Stages[0].End != 48 {
		t.Errorf("stages = %+v, want one stage 0:48", p.Stages)
	}
	// no NextN: the parts stay as they are
	p, err = PlanStagePipe(c, &plan.Model{Name: "x", Layers: 49}, ChainStageV1, StagePipeOptions{Host: hostMap})
	if err != nil || len(p.Stages) != 2 || p.Stages[1].End != 49 {
		t.Errorf("without NextN: %v %+v", err, p)
	}
}

// --mmproj: only the ingress stage takes it; refused with a draft.
func TestPlanStagePipeMMProj(t *testing.T) {
	p, err := PlanStagePipe(reportCandidate(), toyModel(), ChainStageV1, StagePipeOptions{Host: hostMap, MMProj: "/m/proj.gguf"})
	if err != nil {
		t.Fatal(err)
	}
	for i, st := range p.Stages {
		has := strings.Contains(st.Command, "--mmproj /m/proj.gguf")
		if (i == 0) != has {
			t.Errorf("stage %d: --mmproj present=%v: %s", i, has, st.Command)
		}
	}
	p, err = PlanStagePipe(reportCandidate(), toyModel(), ChainStageV1, StagePipeOptions{Host: hostMap})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p.Stages[0].Command, "--mmproj") {
		t.Errorf("no --mmproj requested: %s", p.Stages[0].Command)
	}
	if _, err := PlanStagePipe(reportCandidate(), toyModel(), ChainStageV1, StagePipeOptions{Host: hostMap, MMProj: "/m/p.gguf", Spec: "draft-dflash", DraftModel: "x.gguf"}); err == nil || !strings.Contains(err.Error(), "--mmproj") {
		t.Errorf("--mmproj with a draft must be refused for that reason: %v", err)
	}
}

func fakeNodePortRunner(busyPorts map[string]map[int]bool) func() {
	old := foreignRunPorts
	foreignRunPorts = func(string) map[string]map[int]bool {
		out := map[string]map[int]bool{}
		for node, ports := range busyPorts {
			out[canonicalNode(node)] = ports
		}
		return out
	}
	return func() { foreignRunPorts = old }
}

// TestStagePipePortConflictAllFree verifies that when all ports are free, the plan matches
// the default allocation (case a: ingress 52310, stages 5600, relay 53100).
func TestStagePipePortConflictAllFree(t *testing.T) {
	restore := fakeNodePortRunner(nil)
	t.Cleanup(restore)

	p, err := PlanStagePipe(reportCandidate(), toyModel(), ChainStageV1, StagePipeOptions{Host: hostMap, Relay: true})
	if err != nil {
		t.Fatalf("PlanStagePipe failed: %v", err)
	}
	if p.Stages[0].Port != 52310 {
		t.Errorf("ingress port = %d, want 52310", p.Stages[0].Port)
	}
	if p.Stages[1].Port != 5600 {
		t.Errorf("stage 1 port = %d, want 5600", p.Stages[1].Port)
	}
	if p.Stages[2].Port != 5600 {
		t.Errorf("stage 2 port = %d, want 5600", p.Stages[2].Port)
	}
	if len(p.Relays) != 2 || p.Relays[0].Port != 53100 || p.Relays[1].Port != 53101 {
		t.Errorf("relays = %+v, want 53100 and 53101", p.Relays)
	}
	if p.Stages[0].Next != "tcp://127.0.0.1:53100" {
		t.Errorf("stage 0 next = %q, want tcp://127.0.0.1:53100", p.Stages[0].Next)
	}
}

// TestStagePipePortConflictStagePortBusy verifies case b: 5600 busy on stage node -> shifts to 5601.
func TestStagePipePortConflictStagePortBusy(t *testing.T) {
	restore := fakeNodePortRunner(map[string]map[int]bool{
		"pcie": {5600: true},
	})
	t.Cleanup(restore)

	p, err := PlanStagePipe(reportCandidate(), toyModel(), ChainStageV1, StagePipeOptions{Host: hostMap})
	if err != nil {
		t.Fatalf("PlanStagePipe failed: %v", err)
	}
	// Stage 1 is on pcie: port 5600 was busy, should shift to 5601
	if got := p.Stages[1].Port; got != 5601 {
		t.Errorf("stage 1 port = %d, want 5601", got)
	}
	// Stage 0 next should point to pcie:5601
	if got := p.Stages[0].Next; got != "tcp://10.0.4.21:5601" {
		t.Errorf("stage 0 next = %q, want tcp://10.0.4.21:5601", got)
	}
	if !strings.Contains(p.Stages[1].Command, "tcp://*:5601") {
		t.Errorf("stage 1 command missing tcp://*:5601: %s", p.Stages[1].Command)
	}
	// Stage 2 on delta2 was free, should stay 5600
	if got := p.Stages[2].Port; got != 5600 {
		t.Errorf("stage 2 port = %d, want 5600", got)
	}
}

// TestStagePipePortConflictRelayBusy verifies case c: relay 53100 busy -> shifts to 53101.
func TestStagePipePortConflictRelayBusy(t *testing.T) {
	restore := fakeNodePortRunner(map[string]map[int]bool{
		":local:": {53100: true},
	})
	t.Cleanup(restore)

	p, err := PlanStagePipe(reportCandidate(), toyModel(), ChainStageV1, StagePipeOptions{Host: hostMap, Relay: true})
	if err != nil {
		t.Fatalf("PlanStagePipe failed: %v", err)
	}
	if len(p.Relays) < 2 {
		t.Fatalf("expected at least 2 relays, got %+v", p.Relays)
	}
	// Relay 0 had 53100 busy, so it must shift to 53101
	if got := p.Relays[0].Port; got != 53101 {
		t.Errorf("relay 0 port = %d, want 53101", got)
	}
	if got := p.Stages[0].Next; got != "tcp://127.0.0.1:53101" {
		t.Errorf("stage 0 next = %q, want tcp://127.0.0.1:53101", got)
	}
	// Relay 1 must avoid 53100 (busy) and 53101 (reserved by relay 0), shifting to 53102
	if got := p.Relays[1].Port; got != 53102 {
		t.Errorf("relay 1 port = %d, want 53102", got)
	}
	if got := p.Stages[1].Next; got != "tcp://127.0.0.1:53102" {
		t.Errorf("stage 1 next = %q, want tcp://127.0.0.1:53102", got)
	}
}

// TestStagePipePortConflictIngressBusy verifies case d: default ingress 52310 busy -> shifts to 52311.
func TestStagePipePortConflictIngressBusy(t *testing.T) {
	restore := fakeNodePortRunner(map[string]map[int]bool{
		"delta1": {52310: true},
	})
	t.Cleanup(restore)

	p, err := PlanStagePipe(reportCandidate(), toyModel(), ChainStageV1, StagePipeOptions{Host: hostMap})
	if err != nil {
		t.Fatalf("PlanStagePipe failed: %v", err)
	}
	if got := p.Stages[0].Port; got != 52311 {
		t.Errorf("ingress port = %d, want 52311", got)
	}
	if !strings.Contains(p.Stages[0].Command, "--http 0.0.0.0:52311") {
		t.Errorf("ingress command missing --http 0.0.0.0:52311: %s", p.Stages[0].Command)
	}
	if got := p.Stages[0].Ready.Addr; got != "10.0.4.20:52311" {
		t.Errorf("ingress ready addr = %q, want 10.0.4.20:52311", got)
	}
}

// TestStagePipePortConflictManualPortBusyError verifies case e: manually specified --port or StagePort busy -> clear error.
func TestStagePipePortConflictManualPortBusyError(t *testing.T) {
	restore := fakeNodePortRunner(map[string]map[int]bool{
		"delta1": {8080: true, 52310: true},
		"pcie":   {7000: true, 5600: true},
	})
	t.Cleanup(restore)

	// Ingress port manually specified with --port 8080
	_, err := PlanStagePipe(reportCandidate(), toyModel(), ChainStageV1, StagePipeOptions{Host: hostMap, Port: 8080})
	if err == nil || !strings.Contains(err.Error(), "port 8080 is already in use") {
		t.Fatalf("expected busy error for --port 8080, got: %v", err)
	}

	// StagePort manually specified with StagePort: 7000
	_, err = PlanStagePipe(reportCandidate(), toyModel(), ChainStageV1, StagePipeOptions{Host: hostMap, StagePort: 7000})
	if err == nil || !strings.Contains(err.Error(), "stage port 7000 is already in use") {
		t.Fatalf("expected busy error for StagePort 7000, got: %v", err)
	}
}

// TestStagePipePortConflictTwoStagesSameNode verifies case f: two stages on the same node do not collide.
func TestStagePipePortConflictTwoStagesSameNode(t *testing.T) {
	c := plan.Candidate{Parts: []plan.Part{
		{Node: "a", Layers: 10, First: 0},
		{Node: "b", Layers: 10, First: 10},
		{Node: "b", Layers: 4, First: 20, Output: true},
	}}

	// f1: All free on node b -> stage 1 gets 5600, stage 2 gets 5601
	restore := fakeNodePortRunner(nil)
	p, err := PlanStagePipe(c, &plan.Model{Name: "m", Layers: 24}, ChainStageV1, StagePipeOptions{})
	restore()
	if err != nil {
		t.Fatalf("PlanStagePipe failed: %v", err)
	}
	if p.Stages[1].Port != 5600 || p.Stages[2].Port != 5601 {
		t.Errorf("ports = %d, %d, want 5600, 5601", p.Stages[1].Port, p.Stages[2].Port)
	}
	if p.Stages[1].Next != "tcp://b:5601" {
		t.Errorf("stage 1 next = %q, want tcp://b:5601", p.Stages[1].Next)
	}

	// f2: Port 5600 is busy on node b -> stage 1 gets 5601, stage 2 gets 5602
	restore = fakeNodePortRunner(map[string]map[int]bool{
		"b": {5600: true},
	})
	defer restore()
	p2, err := PlanStagePipe(c, &plan.Model{Name: "m", Layers: 24}, ChainStageV1, StagePipeOptions{})
	if err != nil {
		t.Fatalf("PlanStagePipe with busy 5600 failed: %v", err)
	}
	if p2.Stages[1].Port != 5601 || p2.Stages[2].Port != 5602 {
		t.Errorf("ports = %d, %d, want 5601, 5602", p2.Stages[1].Port, p2.Stages[2].Port)
	}
	if p2.Stages[1].Next != "tcp://b:5602" {
		t.Errorf("stage 1 next = %q, want tcp://b:5602", p2.Stages[1].Next)
	}
}

// TestStagePipePortConflictDryRunIdentical verifies that when ports are free, dry-run output
// is identical (regression test).
func TestStagePipePortConflictDryRunIdentical(t *testing.T) {
	restore := fakeNodePortRunner(nil)
	t.Cleanup(restore)

	p, err := PlanStagePipe(reportCandidate(), toyModel(), testNPUAdapter, StagePipeOptions{Host: hostMap})
	if err != nil {
		t.Fatal(err)
	}
	d := &DryRunner{}
	if err := RunStagePipe(p, d, 90*time.Second); err != nil {
		t.Fatal(err)
	}
	if len(d.Lines) != 9 {
		t.Fatalf("%d lines: %v", len(d.Lines), d.Lines)
	}
	for i, kind := range []string{
		"free port 5600",
		"start stage 2 (last",
		"tcp connect 10.0.4.22:5600",
		"free port 5600",
		"start stage 1 (middle",
		"tcp connect 10.0.4.21:5600",
		"free port 9998",
		"start stage 0 (ingress",
		"tcp connect 10.0.4.20:9998",
	} {
		if !strings.Contains(d.Lines[i], kind) {
			t.Errorf("line %d: %q lacks %q", i, d.Lines[i], kind)
		}
	}
}

// recordedForeignPorts: only deploys that still run under ANOTHER endpoint name hold their ports; the same name (a redeploy
// that replaces in place), stopped and failed ones do not.
func TestRecordedForeignPorts(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	ports := []NodePort{{Node: "dino", Port: 5600}, {Node: "dino", Port: 52310}}
	for i, r := range []DeployResult{
		{RunID: "run-1", EndpointName: "other", Status: StatusRunning, Ports: ports},
		{RunID: "run-2", EndpointName: "mine", Status: StatusRunning, Ports: []NodePort{{Node: "dino", Port: 5700}}},
		{RunID: "run-3", EndpointName: "dead", Status: StatusStopped, Ports: []NodePort{{Node: "dino", Port: 5800}}},
		{RunID: "run-4", EndpointName: "bad", Status: StatusFailed, Ports: []NodePort{{Node: "dino", Port: 5900}}},
		{RunID: "run-5", EndpointName: "fresh", Status: StatusStarting, StartedAt: time.Now(), Ports: []NodePort{{Node: "dino", Port: 6000}}},
		{RunID: "run-6", EndpointName: "killed", Status: StatusStarting, StartedAt: time.Now().Add(-2 * time.Hour), Ports: []NodePort{{Node: "dino", Port: 6100}}},
	} {
		r.saveState()
		_ = i
	}
	got := recordedForeignPorts("mine")
	if !got["dino"][5600] || !got["dino"][52310] {
		t.Errorf("another running endpoint's ports must be foreign: %v", got)
	}
	if !got["dino"][6000] {
		t.Errorf("a fresh starting deploy must hold its port: %v", got)
	}
	for _, port := range []int{5700, 5800, 5900, 6100} {
		if got["dino"][port] {
			t.Errorf("port %d must not be foreign (same name, stopped or failed): %v", port, got)
		}
	}
}

// Two deploys planned at the same moment used to read "no other record" both and take the same default port (found live
// on dino: two engines on 52310). The plan and the "starting" record are one step under a lock now.
func TestDeployChainConcurrentPortsDiffer(t *testing.T) {
	prev, prevProbe := foreignRunPorts, ingressAnswers
	t.Cleanup(func() { foreignRunPorts, ingressAnswers = prev, prevProbe })
	ingressAnswers = func(string) bool { return true } // the dry-run deploys' ingress is not a real server
	foreignRunPorts = func(ep string) map[string]map[int]bool {
		got := recordedForeignPorts(ep)
		time.Sleep(150 * time.Millisecond) // a slow plan: the other deploy gets its turn between the read and its record
		return got
	}
	tmpDir := t.TempDir()
	writeChainRecorderDescriptor(t, tmpDir)
	cand := reportCandidate()
	hostMap := func(n string) string { return "10.0.0.1" }
	var wg sync.WaitGroup
	results := make([]*DeployResult, 2)
	errs := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			opts := StagePipeOptions{Host: hostMap, EndpointName: fmt.Sprintf("race-ep-%d", i)}
			results[i], errs[i] = DeployChain(context.Background(), cand, tmpDir, opts, &DryRunner{}, DefaultRunRecorder{})
		}(i)
	}
	wg.Wait()
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("deploy %d: %v", i, errs[i])
		}
	}
	seen := map[NodePort]string{}
	for i, r := range results {
		for _, np := range r.Ports {
			if other, dup := seen[np]; dup {
				t.Fatalf("%s and race-ep-%d both hold %s:%d", other, i, np.Node, np.Port)
			}
			seen[np] = fmt.Sprintf("race-ep-%d", i)
		}
	}
}

// A running record whose ingress is dead (the machine died, no `fleet run stop`) must not hold its ports; a live one does,
// and so does one that cannot be probed (ssh-only reach, no URL).
func TestRecordedForeignPortsDeadIngress(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	alive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer alive.Close()
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	for _, r := range []DeployResult{
		{RunID: "run-a", EndpointName: "alive", Status: StatusRunning, URL: alive.URL + "/v1", Ports: []NodePort{{Node: "dino", Port: 7001}}},
		{RunID: "run-d", EndpointName: "dead", Status: StatusRunning, URL: deadURL + "/v1", Ports: []NodePort{{Node: "dino", Port: 7002}}},
		{RunID: "run-s", EndpointName: "ssh", Status: StatusRunning, URL: deadURL + "/v1", Reach: "ssh-only", Ports: []NodePort{{Node: "dino", Port: 7003}}},
		{RunID: "run-n", EndpointName: "nourl", Status: StatusRunning, Ports: []NodePort{{Node: "dino", Port: 7004}}},
	} {
		r.saveState()
	}
	got := recordedForeignPorts("mine")
	for port, want := range map[int]bool{7001: true, 7002: false, 7003: true, 7004: true} {
		if got["dino"][port] != want {
			t.Errorf("port %d: held=%v, want %v (%v)", port, got["dino"][port], want, got)
		}
	}
}

// A master without layers (a plan made for the ring) is dropped for the chain: the first machine with layers takes requests.
func TestPlanStagePipe_LayerlessMasterIsDropped(t *testing.T) {
	c := reportCandidate()
	c.Parts = append([]plan.Part{{Node: "front", First: 0, Layers: 0}}, c.Parts...)
	c.Master = "front"
	p, err := PlanStagePipe(c, toyModel(), testNPUAdapter, StagePipeOptions{Host: hostMap})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Stages) != 3 || !strings.Contains(strings.Join(p.Warnings, " "), "front holds none") {
		t.Fatalf("%d stages, warnings %v", len(p.Stages), p.Warnings)
	}
}

// the fixture runtime's ingress has no --port: the first stage serves on its fixed port, and asking for another one is an error.
func TestStagePipeFixedIngressPort(t *testing.T) {
	if _, err := PlanStagePipe(reportCandidate(), toyModel(), testNPUAdapter, StagePipeOptions{Host: hostMap, Port: 18090}); err == nil || !strings.Contains(err.Error(), "9998 only") {
		t.Fatalf("another --port: %v", err)
	}
	p, err := PlanStagePipe(reportCandidate(), toyModel(), testNPUAdapter, StagePipeOptions{Host: hostMap, Port: 9998})
	if err != nil || p.Stages[0].Port != 9998 {
		t.Fatalf("the fixed port itself: %v", err)
	}
	if strings.Contains(p.Stages[0].Command, "--port") || strings.Contains(p.Stages[0].Command, "--host") {
		t.Fatal(p.Stages[0].Command)
	}
}

// testNPUAdapter is a stage runtime other than fleet-engine (an NPU board runtime, as a stage module would
// register): fixed ingress port, a compiled target subdirectory, no MoE/draft/lanes.
var testNPUAdapter = StagePipeAdapter{
	Provisional:         true,
	LayersEnv:           "STAGE_LAYERS",
	NextEnv:             "STAGE_NEXT",
	TimeoutEnv:          "STAGE_TIMEOUT_S",
	EmbeddingOffloadEnv: "NPU_EMBEDDING_OFFLOAD",
	StageBin:            "npu-stage-server",
	StageArgs:           []string{"{stage_model}", "{listen}"},
	IngressBin:          "npu-run",
	IngressArgs:         []string{"run", "{model}", "--mode", "web"},
	ListenFmt:           "tcp://*:{port}",
	NextFmt:             "tcp://{host}:{port}",
	StagePort:           5600,
	IngressPort:         9998,
	IngressPortFixed:    true,
	TimeoutS:            600,
	ModelRoot:           "/mnt/nvme",
	StageModelSub:       "target",
}

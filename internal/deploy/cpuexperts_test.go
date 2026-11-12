package deploy

import (
	"context"
	"strings"
	"testing"

	"github.com/tevfik/fleet-router/internal/config"
)

const p1gen6Experts = `blk\.(29|30|31|32|33|34|35)\.ffn_(up|down|gate)_exps\.weight`

// The layout verified on hardware: 48 blocks, master dino + papa (GPU+CPU) + victor, both workers through the hub
// on 127.0.0.1:51160/51161.
func TestWorkerCPUExpertsVerifiedLayout(t *testing.T) {
	stubPlanner(t, "qwen35moe")
	res := dry(t, DeployOptions{
		WorkerNodes: []string{"papa", "victor"}, ViaHub: true, HubURL: "https://hub.example", HubListenBase: 51160,
		TensorSplit: "29,14,5", WorkerCPUExperts: map[string][2]int{"papa": {29, 35}},
	})
	want := ` -dev CUDA0,RING0,RING2,RING1 -ts 29,14,5,0 -ot '` + p1gen6Experts + `=RING1[127.0.0.1:51160]' -ot '^output\.weight$=CUDA0' -c `
	if !strings.Contains(res.MasterCmd, want) {
		t.Errorf("master command\n got %s\nwant …%s…", res.MasterCmd, want)
	}
	if !strings.Contains(res.MasterEnv, "RING_SERVERS=127.0.0.1:51160,127.0.0.1:51161 ") {
		t.Errorf("ring servers: %s", res.MasterEnv)
	}
	if res.Devices != "CUDA0,RING0,RING2,RING1" || res.TensorSplit != "29,14,5,0" {
		t.Errorf("devices %s ts %s", res.Devices, res.TensorSplit)
	}
	if !strings.Contains(res.WorkerCmds["papa"], " -d CUDA0,CPU -c") || !strings.Contains(res.WorkerCmds["victor"], " -d CUDA0 -c") {
		t.Errorf("worker -d:\n%s\n%s", res.WorkerCmds["papa"], res.WorkerCmds["victor"])
	}
	if len(res.WorkerDevices) != 1 || res.WorkerDevices["papa"] != "CUDA0,CPU" {
		t.Errorf("worker devices: %v", res.WorkerDevices)
	}
	// the ts may also be given with the CPU's 0 already in it
	res2 := dry(t, DeployOptions{
		WorkerNodes: []string{"papa", "victor"}, ViaHub: true, HubURL: "https://hub.example", HubListenBase: 51160,
		TensorSplit: "29,14,5,0", WorkerCPUExperts: map[string][2]int{"papa": {29, 35}},
	})
	if res2.MasterCmd != res.MasterCmd {
		t.Errorf("trailing 0 given: %s", res2.MasterCmd)
	}
}

// Direct workers: the -ot target is the worker's own RING_SERVERS entry; the worker order decides the numbering.
func TestWorkerCPUExpertsDirectAndOrder(t *testing.T) {
	stubPlanner(t, "qwen35moe")
	stubNodes(t, map[string]string{"papa": "10.0.1.119", "victor": "10.0.1.30"}, nil)
	res := dry(t, DeployOptions{WorkerNodes: []string{"papa", "victor"}, TensorSplit: "29,14,5", WorkerCPUExperts: map[string][2]int{"papa": {29, 35}}})
	if !strings.Contains(res.MasterCmd, `-dev CUDA0,RING0,RING2,RING1 -ts 29,14,5,0 -ot '`+p1gen6Experts+`=RING1[10.0.1.119:50152]'`) {
		t.Errorf("direct: %s", res.MasterCmd)
	}
	// the CPU worker second: its GPU is RING1, its CPU RING2
	res = dry(t, DeployOptions{WorkerNodes: []string{"victor", "papa"}, TensorSplit: "29,5,14", WorkerCPUExperts: map[string][2]int{"papa": {40, 41}}})
	if !strings.Contains(res.MasterCmd, `-dev CUDA0,RING0,RING1,RING2 -ts 29,5,14,0 -ot 'blk\.(40|41)\.ffn_(up|down|gate)_exps\.weight=RING2[10.0.1.119:50153]'`) {
		t.Errorf("second worker: %s", res.MasterCmd)
	}
	// both workers with CPU experts: RING0 papa GPU, RING1 papa CPU, RING2 victor GPU, RING3 victor CPU
	res = dry(t, DeployOptions{WorkerNodes: []string{"papa", "victor"}, TensorSplit: "29,14,5", WorkerCPUExperts: map[string][2]int{"papa": {29, 35}, "victor": {44, 47}}})
	if !strings.Contains(res.MasterCmd, `-dev CUDA0,RING0,RING2,RING1,RING3 -ts 29,14,5,0,0 -ot '`+p1gen6Experts+`=RING1[10.0.1.119:50152]' -ot 'blk\.(44|45|46|47)\.ffn_(up|down|gate)_exps\.weight=RING3[10.0.1.30:50153]'`) {
		t.Errorf("two CPU workers: %s", res.MasterCmd)
	}
}

// Via one hub worker only: that worker's entry is 127.0.0.1:<listen>, the direct one keeps its address.
func TestWorkerCPUExpertsViaHubNode(t *testing.T) {
	stubPlanner(t, "qwen35moe")
	stubNodes(t, map[string]string{"papa": "10.0.1.119", "victor": "10.0.1.30"}, nil)
	res := dry(t, DeployOptions{WorkerNodes: []string{"victor", "papa"}, ViaHubNodes: []string{"papa"}, HubURL: "https://hub.example", TensorSplit: "29,5,14", WorkerCPUExperts: map[string][2]int{"papa": {34, 42}}})
	if !strings.Contains(res.MasterEnv, "RING_SERVERS=10.0.1.30:50152,127.0.0.1:51001 ") {
		t.Errorf("ring servers: %s", res.MasterEnv)
	}
	if !strings.Contains(res.MasterCmd, `=RING2[127.0.0.1:51001]'`) {
		t.Errorf("-ot must name the hub entry: %s", res.MasterCmd)
	}
}

func TestWorkerCPUExpertsRefused(t *testing.T) {
	stubPlanner(t, "qwen35moe")
	for _, c := range []struct {
		o    DeployOptions
		want string
	}{
		{DeployOptions{TensorSplit: "29,14,5", WorkerCPUExperts: map[string][2]int{"papa": {28, 35}}}, "outside the blocks papa's GPU runs, 29-42"},
		{DeployOptions{TensorSplit: "29,14,5", WorkerCPUExperts: map[string][2]int{"papa": {40, 43}}}, "29-42"},
		{DeployOptions{TensorSplit: "29,14,5", WorkerCPUExperts: map[string][2]int{"victor": {40, 47}}}, "outside the blocks victor's GPU runs, 43-47"},
		{DeployOptions{TensorSplit: "29,14,5", WorkerCPUExperts: map[string][2]int{"victor": {44, 48}}}, "the model has 48 blocks"},
		{DeployOptions{TensorSplit: "29,0,19", WorkerCPUExperts: map[string][2]int{"papa": {29, 30}}}, "gets no blocks"},
		{DeployOptions{TensorSplit: "29,14", WorkerCPUExperts: map[string][2]int{"papa": {29, 35}}}, "--ts 29,14 has 2 values but -dev has 3 GPU devices"},
		{DeployOptions{TensorSplit: "29,14,5", WorkerCPUExperts: map[string][2]int{"dgx2": {29, 35}}}, "not a worker of this ring"},
		{DeployOptions{TensorSplit: "29,14,5", CPUOnly: true, WorkerCPUExperts: map[string][2]int{"papa": {29, 35}}}, "--cpu"},
	} {
		c.o.ModelPath, c.o.MasterNode, c.o.DryRun = "/models/ornith-122b.gguf", "dino", true
		if c.o.WorkerNodes == nil {
			c.o.WorkerNodes = []string{"papa", "victor"}
		}
		_, err := PlanDeployment(config.Default(), c.o)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: want error with %q, got %v", c.o.WorkerCPUExperts, c.want, err)
		}
	}
}

func TestParseWorkerCPUExperts(t *testing.T) {
	m, err := ParseWorkerCPUExperts("papa=29-35, victor=44-47")
	if err != nil || len(m) != 2 || m["papa"] != [2]int{29, 35} || m["victor"] != [2]int{44, 47} {
		t.Fatalf("%v %v", m, err)
	}
	if m, err := ParseWorkerCPUExperts(""); err != nil || len(m) != 0 {
		t.Errorf("empty: %v %v", m, err)
	}
	for _, bad := range []string{"papa", "papa=29", "papa=35-29", "=1-2", "papa=a-b", "papa=-1-3", "a=1-2,a=3-4"} {
		if _, err := ParseWorkerCPUExperts(bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}

// Without the flag the plan is what it was: the same -dev/-ts/-ot, -d CUDA0 on every worker, no WorkerDevices.
func TestWorkerCPUExpertsAbsentUnchanged(t *testing.T) {
	stubPlanner(t, "qwen35moe")
	for _, o := range []DeployOptions{
		{WorkerNodes: []string{"papa", "victor"}, TensorSplit: "29,14,5"},
		{WorkerNodes: []string{"papa", "victor"}, ViaHub: true, HubURL: "https://hub.example"},
		{WorkerNodes: []string{"victor"}},
		{WorkerNodes: []string{}},
	} {
		base := dry(t, o)
		o.WorkerCPUExperts = map[string][2]int{}
		empty := dry(t, o)
		if base.MasterCmd != empty.MasterCmd || base.Devices != empty.Devices || base.TensorSplit != empty.TensorSplit {
			t.Errorf("empty flag changed the plan:\n%s\n%s", base.MasterCmd, empty.MasterCmd)
		}
		if base.WorkerDevices != nil || strings.Contains(base.MasterCmd, "_exps") {
			t.Errorf("no flag: %v %s", base.WorkerDevices, base.MasterCmd)
		}
		for n, c := range base.WorkerCmds {
			if !strings.Contains(c, " -d CUDA0 -c") {
				t.Errorf("%s: %s", n, c)
			}
		}
	}
	res := dry(t, DeployOptions{WorkerNodes: []string{"papa", "victor"}, TensorSplit: "29,14,5"})
	if !strings.Contains(res.MasterCmd, ` -dev CUDA0,RING0,RING1 -ts 29,14,5 -ot '^output\.weight$=CUDA0' -c `) {
		t.Errorf("unchanged layout: %s", res.MasterCmd)
	}
}

// --local-weights: the block range of a worker with two devices comes from its GPU; the CPU device (ts 0) adds nothing.
func TestWorkerCPUExpertsLocalWeightsBlocks(t *testing.T) {
	stubPlanner(t, "qwen35moe")
	stubSizes(t, map[string]int64{"dino:/models/ornith-122b.gguf": 100, "papa:/models/ornith-122b.gguf": 100, "victor:/models/ornith-122b.gguf": 100})
	with := dry(t, DeployOptions{WorkerNodes: []string{"papa", "victor"}, TensorSplit: "29,14,5", LocalWeights: true, WorkerCPUExperts: map[string][2]int{"papa": {29, 35}}})
	without := dry(t, DeployOptions{WorkerNodes: []string{"papa", "victor"}, TensorSplit: "29,14,5", LocalWeights: true})
	for _, n := range []string{"papa", "victor"} {
		if with.WorkerEnv[n] != without.WorkerEnv[n] || !strings.Contains(with.WorkerEnv[n], "GGML_RING_SHARD_BLOCKS=") {
			t.Errorf("%s: %q vs %q", n, with.WorkerEnv[n], without.WorkerEnv[n])
		}
	}
	if !strings.Contains(with.WorkerEnv["papa"], "GGML_RING_SHARD_BLOCKS=28-44") {
		t.Errorf("papa range: %s", with.WorkerEnv["papa"])
	}
	r := workerBlockRanges("CUDA0,RING0,RING2,RING1", "29,14,5,0", []string{"papa", "papa", "victor"}, 48, 0)
	if r["papa"] != [2]int{28, 44} || r["victor"] != [2]int{42, 47} {
		t.Errorf("ranges %v", r)
	}
}

// The worker start (remote script and local exec) uses the same -d as the plan's command.
func TestWorkerCPUExpertsLaunch(t *testing.T) {
	stubNodes(t, nil, nil)
	stubLocalWorker(t, "/opt/ring/bin/ring-worker", nil)
	if s := remoteWorkerScriptHub(50152, nil, 120, "", cpuExpertDevices); !strings.Contains(s, "nohup $FLEET_DETACH "+remoteWorkerLaunch(50152, nil, cpuExpertDevices).ShellLine()+" >") || !strings.Contains(s, " -d CUDA0,CPU -c") {
		t.Errorf("remote script:\n%s", s)
	}
	ssh, _, procs := stubExec(t, "FLEET_PID 7\nFLEET_READY\n", true, nil)
	if _, _, _, err := NewRemoteWorkerManager().startWorker(context.Background(), "papa", 50152, false, nil, "", cpuExpertDevices); err != nil {
		t.Fatal(err)
	}
	if len(*ssh) != 1 || !strings.Contains((*ssh)[0], " -d CUDA0,CPU -c") {
		t.Errorf("remote start: %v", *ssh)
	}
	if _, _, _, err := NewRemoteWorkerManager().startWorker(context.Background(), "localhost", 50152, true, nil, "", cpuExpertDevices); err != nil {
		t.Fatal(err)
	}
	if len(*procs) != 1 || !strings.Contains(strings.Join((*procs)[0].args, " "), "-d CUDA0,CPU -c") {
		t.Errorf("local start: %v", *procs)
	}
}

// A local master is exec'd from the words of MasterCmd: the shell quotes around the -ot values must go.
func TestLocalArgsDropShellQuotes(t *testing.T) {
	got := localArgs([]string{"-ot", `'^output\.weight$=CUDA0'`, "-ot", `'blk\.(29)\.ffn_(up|down|gate)_exps\.weight=RING1[127.0.0.1:51160]'`, "-c", "4096", "'", "a'b'"})
	want := []string{"-ot", `^output\.weight$=CUDA0`, "-ot", `blk\.(29)\.ffn_(up|down|gate)_exps\.weight=RING1[127.0.0.1:51160]`, "-c", "4096", "'", "a'b'"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("got %q", got)
	}
}

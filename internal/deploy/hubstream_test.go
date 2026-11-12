package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/plan"
)

const testHub = "https://hub.example.com"

// the tests below plan like a build with a default hub (a private build); TestHubPathWithoutAnyHub covers the public one
func init() { config.DefaultHubURL = testHub }

// No hub named, none in the configuration and none built in (the public build): --via-hub says so instead of
// planning streams to nowhere.
func TestHubPathWithoutAnyHub(t *testing.T) {
	defer func(u string) { config.DefaultHubURL = u }(config.DefaultHubURL)
	config.DefaultHubURL = ""
	stubPlanner(t, "llama")
	stubNodes(t, map[string]string{"papa": "10.0.1.119", "dino": "10.0.1.13"}, nil)
	_, err := PlanDeployment(config.Default(), DeployOptions{ModelPath: "/models/m.gguf", MasterNode: "dino", WorkerNodes: []string{"victor"}, ViaHub: true, DryRun: true})
	if !errors.Is(err, config.ErrNoHub) {
		t.Fatalf("err = %v, want ErrNoHub", err)
	}
}

// A worker without a direct address (victor: no config IP, no ssh IP) must NOT silently fall back to the hub;
// it is refused unless --via-hub, --via-hub-node, --no-hub, or --chain is given explicitly.
func TestHubPathForWorkerWithoutDirectAddress(t *testing.T) {
	stubPlanner(t, "llama")
	stubNodes(t, map[string]string{"papa": "10.0.1.119", "dino": "10.0.1.13"}, nil)
	_, err := PlanDeployment(config.Default(), DeployOptions{ModelPath: "/models/ornith-122b.gguf", MasterNode: "dino", WorkerNodes: []string{"victor"}, DryRun: true})
	if err == nil {
		t.Fatal("expected refusal when worker has no direct LAN address and --via-hub is not given")
	}
	for _, want := range []string{"refusing implicit hub fallback", "victor", "--via-hub", "--via-hub-node victor", "--no-hub"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err.Error(), want)
		}
	}

	// With explicit --via-hub, the hub stream path is planned cleanly.
	res := dry(t, DeployOptions{WorkerNodes: []string{"victor"}, ViaHub: true})
	if !strings.Contains(res.MasterEnv, "RING_SERVERS=127.0.0.1:51000") {
		t.Errorf("RING_SERVERS must name the local stream: %s", res.MasterEnv)
	}
	want := "fleet stream connect victor 50152 --listen 127.0.0.1:51000 --hub " + testHub
	if res.StreamCmds["victor"] != want {
		t.Errorf("stream command %q, want %q", res.StreamCmds["victor"], want)
	}
	if res.HubWorkers["victor"] != 51000 || res.HubURL != testHub {
		t.Errorf("hub workers %v hub %q", res.HubWorkers, res.HubURL)
	}
	if !strings.Contains(res.WorkerCmds["victor"], "fleet join "+testHub+" --add-stream-port 50152") {
		t.Errorf("worker command must show the join: %s", res.WorkerCmds["victor"])
	}
	all := strings.Join(res.Warnings, "\n")
	for _, w := range []string{"goes through the hub", "--via-hub", "adds the hub round trip", "use ITS fleet's hub API key"} {
		if !strings.Contains(all, w) {
			t.Errorf("warnings lack %q: %v", w, res.Warnings)
		}
	}
	if strings.Contains(all, "no known direct address") {
		t.Errorf("the hub path replaces the dial-by-name warning: %v", res.Warnings)
	}
	// the worker itself starts exactly as before
	if strings.Contains(res.WorkerCmds["victor"], "127.0.0.1") || !strings.Contains(res.WorkerCmds["victor"], "-H 0.0.0.0 -p 50152") {
		t.Errorf("worker start changed: %s", res.WorkerCmds["victor"])
	}
}

func TestHubSpeedLossGuard(t *testing.T) {
	// 8-layer model where each layer reads ~125 MiB (total ~1 GiB/token across 8 layers).
	// At 500 GB/s on both machines: compute = ~2 ms + 1.5 ms fixed + 0.5 ms LAN RTT ≈ 4.0 ms (~250 tok/s direct).
	// With --via-hub and HubRTTms = 25 ms: ~28.5 ms (~35 tok/s) -> ~86% drop (> 50% HubLossRefuseRatio).
	layerRead := make([]int64, 8)
	layerBytes := make([]int64, 8)
	for i := range layerRead {
		layerRead[i] = 128 << 20
		layerBytes[i] = 128 << 20
	}
	e := newExecEnv(t)
	stubMeta(t, &plan.Model{Name: "fast", Arch: "llama", Layers: 8, Embd: 2048, LayerRead: layerRead, LayerBytes: layerBytes})
	stubNodes(t, map[string]string{"dino": "10.0.1.13", "w1": "10.0.1.20"}, nil)

	cfg := config.Default()
	cfg.Nodes = map[string]config.NodeSpec{
		"dino": {Addr: "10.0.1.13", WeightGBps: 500, MemGiB: 64},
		"w1":   {Addr: "10.0.1.20", WeightGBps: 500, MemGiB: 64},
	}

	// 1) Dry run shows WOULD BE REFUSED when speed loss > 50%
	dryRes, err := ExecuteDeployment(context.Background(), cfg, DeployOptions{
		ModelPath: "/m/fast.gguf", MasterNode: "dino", WorkerNodes: []string{"w1"},
		ViaHub: true, HubRTTms: 25, DryRun: true,
	})
	if err != nil {
		t.Fatalf("dry run should report blocker in warnings, got err: %v", err)
	}
	if !strings.Contains(strings.Join(dryRes.Warnings, "\n"), "WOULD BE REFUSED (use --force to go on): hub stream throughput drop") {
		t.Errorf("expected WOULD BE REFUSED warning, got: %v", dryRes.Warnings)
	}

	// 2) Live deploy without --force refuses before starting anything
	_, err = ExecuteDeployment(context.Background(), cfg, DeployOptions{
		ModelPath: "/m/fast.gguf", MasterNode: "dino", WorkerNodes: []string{"w1"},
		ViaHub: true, HubRTTms: 25,
	})
	if err == nil || !strings.Contains(err.Error(), "refusing hub deployment (>50% estimated speed loss)") {
		t.Fatalf("expected >50%% hub speed loss refusal, got: %v", err)
	}
	if len(e.procs) != 0 {
		t.Errorf("nothing must start when hub speed loss refuses deploy: %v", e.procs)
	}

	// 3) Small hub RTT (<= 50% speed loss, above DefaultRTTms=2ms) warns without refusing
	warnRes, err := ExecuteDeployment(context.Background(), cfg, DeployOptions{
		ModelPath: "/m/fast.gguf", MasterNode: "dino", WorkerNodes: []string{"w1"},
		ViaHub: true, HubRTTms: 5, DryRun: true,
	})
	if err != nil {
		t.Fatalf("dry run with small hub RTT failed: %v", err)
	}
	joined := strings.Join(warnRes.Warnings, "\n")
	if strings.Contains(joined, "WOULD BE REFUSED") || !strings.Contains(joined, "hub stream throughput drop") {
		t.Errorf("expected non-blocking hub drop warning, got: %v", warnRes.Warnings)
	}
}

func TestViaHubForcesAndViaHubNodeSelects(t *testing.T) {
	stubPlanner(t, "llama")
	stubNodes(t, map[string]string{"w1": "10.0.1.20", "w2": "10.0.1.21"}, nil)
	res := dry(t, DeployOptions{WorkerNodes: []string{"w1", "w2"}, ViaHub: true, HubURL: "https://hub.example"})
	if !strings.Contains(res.MasterEnv, "RING_SERVERS=127.0.0.1:51000,127.0.0.1:51001") {
		t.Errorf("--via-hub: %s", res.MasterEnv)
	}
	if res.StreamCmds["w2"] != "fleet stream connect w2 50153 --listen 127.0.0.1:51001 --hub https://hub.example" {
		t.Errorf("w2 stream: %q", res.StreamCmds["w2"])
	}
	res = dry(t, DeployOptions{WorkerNodes: []string{"w1", "w2"}, ViaHubNodes: []string{"w2"}, HubListenBase: 52000})
	if !strings.Contains(res.MasterEnv, "RING_SERVERS=10.0.1.20:50152,127.0.0.1:52001") {
		t.Errorf("--via-hub-node w2: %s", res.MasterEnv)
	}
	if _, ok := res.StreamCmds["w1"]; ok || len(res.StreamCmds) != 1 {
		t.Errorf("only w2 gets a stream: %v", res.StreamCmds)
	}
	if strings.Contains(res.WorkerCmds["w1"], "fleet join") || !strings.Contains(res.WorkerCmds["w2"], "--add-stream-port 50153") {
		t.Errorf("join only on w2: %v", res.WorkerCmds)
	}
	if _, err := PlanDeployment(config.Default(), DeployOptions{ModelPath: "/m.gguf", MasterNode: "dino", WorkerNodes: []string{"w1"}, ViaHubNodes: []string{"nope"}}); err == nil || !strings.Contains(err.Error(), "not a worker") {
		t.Errorf("unknown --via-hub-node: %v", err)
	}
	if _, err := PlanDeployment(config.Default(), DeployOptions{ModelPath: "/m.gguf", MasterNode: "dino", WorkerNodes: []string{"w1", "w2"}, ViaHub: true, Chain: true}); err == nil || !strings.Contains(err.Error(), "--chain") {
		t.Errorf("--via-hub with --chain: %v", err)
	}
	if _, err := PlanDeployment(config.Default(), DeployOptions{ModelPath: "/m.gguf", MasterNode: "dino", WorkerNodes: []string{"w1"}, ViaHub: true, Port: 51000}); err == nil || !strings.Contains(err.Error(), "collides") {
		t.Errorf("listen port equal to the master port: %v", err)
	}
}

func TestDirectWorkersUnchanged(t *testing.T) {
	stubPlanner(t, "llama")
	stubNodes(t, map[string]string{"w1": "10.0.1.20", "w2": "10.0.1.21"}, nil)
	res := dry(t, DeployOptions{WorkerNodes: []string{"w1", "w2"}})
	if !strings.Contains(res.MasterEnv, "RING_SERVERS=10.0.1.20:50152,10.0.1.21:50153") {
		t.Errorf("direct: %s", res.MasterEnv)
	}
	if len(res.StreamCmds) != 0 || len(res.HubWorkers) != 0 || res.HubURL != "" {
		t.Errorf("no hub for direct workers: %v %v %q", res.StreamCmds, res.HubWorkers, res.HubURL)
	}
	for _, c := range res.WorkerCmds {
		if strings.Contains(c, "fleet join") {
			t.Errorf("no join for a direct worker: %s", c)
		}
	}
	if strings.Contains(strings.Join(res.Warnings, "\n"), "hub") {
		t.Errorf("no hub warnings: %v", res.Warnings)
	}
	// --no-hub (and --chain) keep a worker without an address on the old dial-by-name path
	res = dry(t, DeployOptions{WorkerNodes: []string{"victor", "w1"}, Chain: true})
	if !strings.Contains(res.MasterEnv, "RING_SERVERS=victor:50152,10.0.1.20:50153") || len(res.StreamCmds) != 0 {
		t.Errorf("--chain must not pick the hub: %s", res.MasterEnv)
	}
}

func TestWorkerScriptDeclaresStreamPortOnlyWithHub(t *testing.T) {
	plain := remoteWorkerScript(50152, nil, 120)
	if strings.Contains(plain, "fleet join") || plain != remoteWorkerScriptHub(50152, nil, 120, "", "") {
		t.Errorf("a direct worker's script must not change:\n%s", plain)
	}
	hs := remoteWorkerScriptHub(50152, nil, 120, testHub, "")
	join := hubJoinScript(testHub, 50152)
	if !strings.Contains(hs, "echo FLEET_READY; "+join+"exit 0") {
		t.Errorf("join must run after the worker listens:\n%s", hs)
	}
	for _, w := range []string{"fleet status >/dev/null 2>&1", "fleet join " + testHub + " --add-stream-port 50152", "FLEET_HUB_NODAEMON"} {
		if !strings.Contains(join, w) {
			t.Errorf("join script lacks %q: %s", w, join)
		}
	}
	// the worker's own start line is the same with and without the hub
	if !strings.Contains(hs, "nohup $FLEET_DETACH "+remoteWorkerLaunch(50152, nil, "").ShellLine()+" >") {
		t.Errorf("worker start changed:\n%s", hs)
	}
}

func TestStartWorkerHubNote(t *testing.T) {
	stubNodes(t, nil, nil)
	for out, want := range map[string]string{
		"FLEET_PID 9\nFLEET_READY\nFLEET_HUB_JOINED\n":                   "",
		"FLEET_PID 9\nFLEET_READY\nFLEET_HUB_NODAEMON\n":                 "no fleet daemon answers on victor",
		"FLEET_PID 9\nFLEET_READY\nFLEET_HUB_JOIN_FAILED bad flag\n":     "failed (bad flag)",
		"FLEET_PID 9\nFLEET_READY\n":                                     "no answer",
		"FLEET_PID 9\nFLEET_READY\nFLEET_HUB_NODAEMON\nsomething else\n": "--add-stream-port 50152",
	} {
		stubExec(t, out, false, nil)
		_, pid, note, err := NewRemoteWorkerManager().startWorker(context.Background(), "victor", 50152, false, nil, testHub, "")
		if err != nil || pid != 9 {
			t.Fatalf("%q: pid %d err %v", out, pid, err)
		}
		if (want == "") != (note == "") || !strings.Contains(note, want) {
			t.Errorf("%q: note %q, want %q", out, note, want)
		}
	}
}

func TestStartStreamLocalPassesKeyOnlyInEnv(t *testing.T) {
	_, local, procs := stubExec(t, "", true, nil)
	old := fleetBinary
	fleetBinary = func() (string, error) { return "/opt/fleet/fleet", nil }
	t.Cleanup(func() { fleetBinary = old })
	l := streamLaunch{Node: "victor", Port: 50152, ListenPort: 51000, Hub: testHub}
	pid, err := NewRemoteWorkerManager().StartStream(context.Background(), "", true, l, "run-1", "sk-SECRET")
	if err != nil || pid != 4242 {
		t.Fatalf("pid %d err %v", pid, err)
	}
	if len(*local) != 1 || !strings.Contains((*local)[0], "fuser -n tcp 51000") || !strings.Contains((*local)[0], "      fleet)") {
		t.Errorf("a previous fleet stream on the port must be stopped first (fleet processes only): %v", *local)
	}
	p := (*procs)[0]
	if p.bin != "/opt/fleet/fleet" || strings.Join(p.args, " ") != strings.Join(l.Args(), " ") {
		t.Errorf("started %s %v", p.bin, p.args)
	}
	if strings.Contains(strings.Join(p.args, " "), "sk-SECRET") {
		t.Errorf("key in the command line: %v", p.args)
	}
	if !strings.Contains(strings.Join(p.env, "\n"), "FLEET_API_KEY=sk-SECRET") {
		t.Error("the local stream gets the key in its environment")
	}
	if !strings.HasSuffix(p.log, "runs/run-1-stream-victor.log") {
		t.Errorf("log %s", p.log)
	}
}

func TestStartStreamRemoteUsesMastersOwnKey(t *testing.T) {
	ssh, _, _ := stubExec(t, "FLEET_PID 77\nFLEET_READY\n", false, nil)
	l := streamLaunch{Node: "victor", Port: 50152, ListenPort: 51000, Hub: testHub}
	pid, err := NewRemoteWorkerManager().StartStream(context.Background(), "dino", false, l, "run-1", "sk-SECRET")
	if err != nil || pid != 77 {
		t.Fatalf("pid %d err %v", pid, err)
	}
	s := (*ssh)[0]
	for _, w := range []string{"dino\n", "nohup $FLEET_DETACH " + l.ShellLine() + " >", `.cache/fleet/runs/run-1-stream-victor.log`, "fuser -n tcp 51000", "[:.]51000$"} {
		if !strings.Contains(s, w) {
			t.Errorf("remote stream script lacks %q:\n%s", w, s)
		}
	}
	if strings.Contains(s, "sk-SECRET") || strings.Contains(s, "FLEET_API_KEY") {
		t.Errorf("no key goes over ssh:\n%s", s)
	}
	stubExec(t, "FLEET_PID 77\nFLEET_DIED\nno API key for the hub\n", false, nil)
	if _, err := NewRemoteWorkerManager().StartStream(context.Background(), "dino", false, l, "run-1", ""); err == nil || !strings.Contains(err.Error(), "no API key for the hub") {
		t.Errorf("a stream that died must say why: %v", err)
	}
}

func TestStopStreamsStopsOnlyFleetOnListenPorts(t *testing.T) {
	ssh, local, _ := stubExec(t, "", false, nil)
	err := StopStreams(context.Background(), "dino", false, map[string]string{"victor": "127.0.0.1:51000", "b": "127.0.0.1:51001"})
	if err != nil {
		t.Fatal(err)
	}
	if len(*ssh) != 2 || len(*local) != 0 {
		t.Fatalf("ssh %d local %d", len(*ssh), len(*local))
	}
	joined := strings.Join(*ssh, "\n")
	for _, w := range []string{"fuser -n tcp 51000", "fuser -n tcp 51001", "      fleet)"} {
		if !strings.Contains(joined, w) {
			t.Errorf("stop scripts lack %q", w)
		}
	}
	if strings.Contains(joined, "ring-worker") || strings.Contains(joined, "llama-server") {
		t.Error("a stream stop must not touch workers or masters")
	}
	if err := NewRemoteWorkerManager().StopStream(context.Background(), "", 51000, true); err != nil || len(*local) != 1 {
		t.Errorf("local stop: %v %d", err, len(*local))
	}
	// a deploy that fails after its streams started stops them again (on the master)
	res := &DeployResult{MasterNode: "dino", HubWorkers: map[string]int{"victor": 51000}}
	res.stopStreamsOf(context.Background(), false)
	if len(*ssh) != 3 || !strings.Contains((*ssh)[2], "fuser -n tcp 51000") {
		t.Errorf("stopStreamsOf: %d", len(*ssh))
	}
}

// The key is never in a shown command, the plan or its JSON (dry-run, local and remote master).
func TestHubKeyNeverPrinted(t *testing.T) {
	stubPlanner(t, "llama")
	t.Setenv("FLEET_API_KEY", "sk-SECRET")
	old := hubAPIKey
	hubAPIKey = func(*config.Config) string { return "sk-SECRET" }
	t.Cleanup(func() { hubAPIKey = old })
	for _, master := range []string{"dino", "localhost"} {
		res, err := ExecuteDeployment(context.Background(), config.Default(), DeployOptions{ModelPath: "/m.gguf", MasterNode: master, WorkerNodes: []string{"victor"}, ViaHub: true, DryRun: true})
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(res)
		if strings.Contains(string(b), "sk-SECRET") || strings.Contains(res.MasterEnv, "FLEET_API_KEY") {
			t.Errorf("master %s: key in the plan: %s", master, b)
		}
		if len(res.StreamCmds) != 1 {
			t.Errorf("master %s: stream commands %v", master, res.StreamCmds)
		}
	}
}

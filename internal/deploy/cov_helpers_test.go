package deploy

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/plan"
)

func TestCovStrategyParseAndParams(t *testing.T) {
	// Test ParseStrategy aliases
	aliases := map[string]Strategy{
		"latency":           StrategyLatency,
		"low-latency":       StrategyLatency,
		"ultra-low-latency": StrategyLatency,
		"fast":              StrategyLatency,
		"throughput":        StrategyThroughput,
		"max-throughput":    StrategyThroughput,
		"batch":             StrategyThroughput,
		"memory":            StrategyMemory,
		"memory-saver":      StrategyMemory,
		"ram":               StrategyMemory,
		"cpu-gpu":           StrategyMemory,
		"unknown":           StrategyAuto,
		"":                  StrategyAuto,
	}
	for in, expected := range aliases {
		if got := ParseStrategy(in); got != expected {
			t.Errorf("ParseStrategy(%q) = %v; want %v", in, got, expected)
		}
	}

	// Test ParamsForStrategy
	lat := ParamsForStrategy(StrategyLatency, 32, true)
	if lat.Strategy != StrategyLatency || lat.MicroBatch != 512 || lat.Np != 1 || !lat.UseDirectLAN || !lat.AsymSplit {
		t.Errorf("ParamsForStrategy(StrategyLatency) unexpected: %+v", lat)
	}

	thr := ParamsForStrategy(StrategyThroughput, 32, false)
	if thr.Strategy != StrategyThroughput || thr.MicroBatch != 512 || thr.Np != 8 || thr.UseDirectLAN || thr.AsymSplit {
		t.Errorf("ParamsForStrategy(StrategyThroughput) unexpected: %+v", thr)
	}

	mem := ParamsForStrategy(StrategyMemory, 32, true)
	if mem.Strategy != StrategyMemory || mem.MicroBatch != 64 || mem.Np != 2 || mem.NGL != 16 {
		t.Errorf("ParamsForStrategy(StrategyMemory) unexpected: %+v", mem)
	}

	autoLAN := ParamsForStrategy(StrategyAuto, 32, true)
	if autoLAN.Strategy != StrategyAuto || !autoLAN.UseDirectLAN {
		t.Errorf("ParamsForStrategy(StrategyAuto, LAN) unexpected: %+v", autoLAN)
	}

	autoWAN := ParamsForStrategy(StrategyAuto, 32, false)
	if autoWAN.Strategy != StrategyAuto || autoWAN.UseDirectLAN {
		t.Errorf("ParamsForStrategy(StrategyAuto, WAN) unexpected: %+v", autoWAN)
	}
}

func TestCovRunRecord(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", tmp)

	// Test DefaultRunRecorder & ReadRecord
	rec := DefaultRunRecorder{}
	res := DeployResult{
		RunID:        "run-cov-test-1",
		EndpointName: "test-ep",
		Status:       StatusRunning,
	}

	calledState := false
	oldOnState := OnState
	OnState = func(r DeployResult) {
		if r.RunID == "run-cov-test-1" {
			calledState = true
		}
	}
	t.Cleanup(func() { OnState = oldOnState })

	if err := rec.RecordRun(res); err != nil {
		t.Fatalf("RecordRun failed: %v", err)
	}
	if !calledState {
		t.Errorf("OnState was not invoked")
	}

	got, err := ReadRecord("run-cov-test-1")
	if err != nil {
		t.Fatalf("ReadRecord failed: %v", err)
	}
	if got.EndpointName != "test-ep" || got.Status != StatusRunning {
		t.Errorf("ReadRecord returned unexpected result: %+v", got)
	}

	// ReadRecord non-existent
	if _, err := ReadRecord("nonexistent"); err == nil {
		t.Errorf("ReadRecord expected error for missing file")
	}

	// ReadRecord corrupt json
	corruptPath := filepath.Join(runDir(), "corrupt.json")
	_ = os.WriteFile(corruptPath, []byte("not-json"), 0600)
	if _, err := ReadRecord("corrupt"); err == nil {
		t.Errorf("ReadRecord expected error for corrupt json")
	}

	// MarkEndpointStopped
	// Add another stopped and another failed record to test skip conditions
	rStopped := DeployResult{RunID: "run-stopped", EndpointName: "test-ep", Status: StatusStopped}
	rStopped.saveState()
	rFailed := DeployResult{RunID: "run-failed", EndpointName: "test-ep", Status: StatusFailed}
	rFailed.saveState()
	rOther := DeployResult{RunID: "run-other", EndpointName: "other-ep", Status: StatusRunning}
	rOther.saveState()

	n := MarkEndpointStopped("test-ep", "maintenance")
	if n != 1 {
		t.Errorf("MarkEndpointStopped changed %d records, want 1", n)
	}

	updated, err := ReadRecord("run-cov-test-1")
	if err != nil {
		t.Fatalf("ReadRecord after MarkEndpointStopped failed: %v", err)
	}
	if updated.Status != StatusStopped {
		t.Errorf("expected StatusStopped, got %v", updated.Status)
	}
}

func TestCovRelayHub(t *testing.T) {
	rel := StageRelay{
		From:   "node-a",
		To:     "node-b",
		Port:   19001,
		ToPort: 19002,
		Hub:    "https://hub.example.com",
	}

	launch := hubStreamLaunch(rel)
	if launch.Node != "node-b" || launch.Port != 19002 || launch.ListenPort != 19001 || launch.Hub != "https://hub.example.com" {
		t.Errorf("hubStreamLaunch returned unexpected struct: %+v", launch)
	}

	line := hubRelayLine(rel)
	if line == "" {
		t.Errorf("hubRelayLine returned empty string")
	}

	if got := configPathOr("/custom/path"); got != "/custom/path" {
		t.Errorf("configPathOr(/custom/path) = %q, want /custom/path", got)
	}
	if got := configPathOr(""); got != config.Path() {
		t.Errorf("configPathOr(\"\") = %q, want %q", got, config.Path())
	}
}

func TestCovDetach(t *testing.T) {
	bin, args, attr := detachedCommand("echo", []string{"hello"})
	if bin == "" || len(args) == 0 || attr == nil || !attr.Setsid {
		t.Errorf("detachedCommand invalid: bin=%v, args=%v, attr=%v", bin, args, attr)
	}
}

func TestCovExports(t *testing.T) {
	if h := LocalHostname(); h == "" {
		t.Errorf("LocalHostname returned empty string")
	}

	res := &DeployResult{statsPort: 12345}
	if p := res.StatsPortUsed(); p != 12345 {
		t.Errorf("StatsPortUsed = %d, want 12345", p)
	}

	// ReadModelMeta tests
	oldReadGGUF := readGGUF
	oldReadRemote := readRemoteMeta
	oldThis := isThisMachine
	oldLocalHost := localHostname
	t.Cleanup(func() {
		readGGUF = oldReadGGUF
		readRemoteMeta = oldReadRemote
		isThisMachine = oldThis
		localHostname = oldLocalHost
	})

	// Case 1: local success
	readGGUF = func(p string) (*plan.Model, error) {
		return &plan.Model{Name: "model1", Layers: 10}, nil
	}
	m, err := ReadModelMeta("localhost", "/path/to/model")
	if err != nil || m == nil || m.Name != "model1" {
		t.Fatalf("ReadModelMeta local success failed: %v", err)
	}

	// Case 2: local error on local master
	readGGUF = func(p string) (*plan.Model, error) {
		return nil, errors.New("file not found")
	}
	isThisMachine = func(node string) bool { return true }
	if _, err := ReadModelMeta("localhost", "/path/to/model"); err == nil {
		t.Errorf("ReadModelMeta expected error when local read fails on local node")
	}

	// Case 3: local error on remote master, remote succeeds
	isThisMachine = func(node string) bool { return false }
	localHostname = func() string { return "othermachine" }
	readRemoteMeta = func(node, p string) (*plan.Model, error) {
		return &plan.Model{Name: "remote-model", Layers: 2, LayerRead: []int64{1, 2}}, nil
	}
	rm, err := ReadModelMeta("remote-host", "/path/to/model")
	if err != nil || rm == nil || rm.Name != "remote-model" {
		t.Fatalf("ReadModelMeta remote success failed: %v", err)
	}

	// Case 4: local error on remote master, remote fails
	readRemoteMeta = func(node, p string) (*plan.Model, error) {
		return nil, errors.New("remote fail")
	}
	if _, err := ReadModelMeta("remote-host", "/path/to/model"); err == nil {
		t.Errorf("ReadModelMeta expected error when remote fails")
	}

	// Case 5: remote succeeds but incomplete layers
	readRemoteMeta = func(node, p string) (*plan.Model, error) {
		return &plan.Model{Name: "incomplete", Layers: 4, LayerRead: []int64{1, 2}}, nil
	}
	if _, err := ReadModelMeta("remote-host", "/path/to/model"); err == nil {
		t.Errorf("ReadModelMeta expected error for incomplete layer metadata")
	}

	// CandidateCommands test
	readGGUF = func(string) (*plan.Model, error) {
		return &plan.Model{Name: "test-model", Layers: 32}, nil
	}
	oldCfgNode := configNodeAddr
	t.Cleanup(func() { configNodeAddr = oldCfgNode })
	configNodeAddr = func(node string) string {
		if node == "worker-node" {
			return "10.0.1.55"
		}
		return ""
	}
	cand := plan.Candidate{
		Master: "localhost",
		Parts: []plan.Part{
			{Node: "localhost", Layers: 16},
			{Node: "worker-node", Layers: 16},
		},
	}
	cfg := &config.Config{
		Nodes: map[string]config.NodeSpec{
			"worker-node": {Addr: "10.0.1.55"},
		},
	}
	resCand, err := CandidateCommands(cfg, "/path/to/model", cand, 2048, 1, "")
	if err != nil || resCand == nil {
		t.Errorf("CandidateCommands failed: %v", err)
	}

	// DeployResult.RingServersList
	var nilRes *DeployResult
	if nilRes.RingServersList() != nil {
		t.Errorf("nil DeployResult.RingServersList() want nil")
	}
	rDirect := &DeployResult{RingServers: []string{"s1", "s2"}}
	if list := rDirect.RingServersList(); len(list) != 2 || list[0] != "s1" {
		t.Errorf("RingServersList direct failed: %v", list)
	}
	rEnv := &DeployResult{MasterEnv: "FOO=bar RING_SERVERS=10.0.0.1:50152,10.0.0.2:50152 BAZ=1"}
	if list := rEnv.RingServersList(); len(list) != 2 || list[0] != "10.0.0.1:50152" {
		t.Errorf("RingServersList env failed: %v", list)
	}
	rEmptyEnv := &DeployResult{MasterEnv: "RING_SERVERS="}
	if list := rEmptyEnv.RingServersList(); list != nil {
		t.Errorf("RingServersList empty env want nil, got: %v", list)
	}

	// lanProbe
	_ = lanProbe(NewRemoteWorkerManager(), "127.0.0.1")
}

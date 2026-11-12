package deploy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/plan"
)

func fastRunOn(t *testing.T) {
	t.Helper()
	old := RunOnReady
	RunOnReady = 2 * time.Second
	t.Cleanup(func() { RunOnReady = old })
}

// A remote machine without any engine used to be a silent failure (the script ran `fleet ring serve` that did not
// exist, in the background, and the call said "running").
func TestRunOnNodeRemoteWithoutEngineIsAnError(t *testing.T) {
	newExecEnv(t)
	oldS := sshScript
	sshScript = func(_ context.Context, node, script string) ([]byte, error) {
		if !strings.Contains(script, "SERVER_BIN") {
			t.Errorf("unexpected script %q", script)
		}
		return []byte("FLEET_NOBIN\n"), nil
	}
	t.Cleanup(func() { sshScript = oldS })
	_, err := RunOnNode(context.Background(), config.Default(), "dino", "/m/x.gguf", 0, 0, 0, false)
	if err == nil || !strings.Contains(err.Error(), "no engine on dino") {
		t.Fatalf("%v", err)
	}
}

func TestRunOnNodeRemoteURLIsTheMachinesAddressAndServerListensThere(t *testing.T) {
	newExecEnv(t)
	res, err := RunOnNode(context.Background(), config.Default(), "dino", "/m/x.gguf", 0, 0, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.URL != "http://10.0.1.13:18092/v1" || !strings.Contains(res.MasterCmd, "--host 0.0.0.0") || !strings.Contains(res.MasterEngine, "resolved on dino") {
		t.Fatalf("%s | %s | %s", res.URL, res.MasterCmd, res.MasterEngine)
	}
	loc, _ := RunOnNode(context.Background(), config.Default(), "testhost", "/m/x.gguf", 0, 0, 0, true)
	if loc.URL != "http://127.0.0.1:18092/v1" || strings.Contains(loc.MasterCmd, "--host") {
		t.Fatalf("%s | %s", loc.URL, loc.MasterCmd)
	}
}

func TestRunOnNodeRemoteDiedAtOnceShowsTheLog(t *testing.T) {
	newExecEnv(t)
	oldS := sshScript
	sshScript = func(_ context.Context, node, script string) ([]byte, error) {
		return []byte("FLEET_PID 90\nFLEET_DIED\nerror: unknown argument --foo\n"), nil
	}
	t.Cleanup(func() { sshScript = oldS })
	_, err := RunOnNode(context.Background(), config.Default(), "dino", "/m/x.gguf", 0, 0, 0, false)
	if err == nil || !strings.Contains(err.Error(), "unknown argument --foo") {
		t.Fatalf("%v", err)
	}
}

func TestRunOnNodeRemoteHealthyAndStillLoading(t *testing.T) {
	newExecEnv(t)
	fastRunOn(t)
	healthy := false
	oldS := sshScript
	sshScript = func(_ context.Context, node, script string) ([]byte, error) {
		switch {
		case strings.Contains(script, "SERVER_BIN"):
			return []byte("FLEET_PID 90\n"), nil
		case strings.Contains(script, "curl"):
			if healthy {
				return []byte("200"), nil
			}
			return []byte("000"), nil
		case strings.Contains(script, "tail"):
			return []byte("load_tensors: offloading 40 layers\n"), nil
		}
		return nil, nil
	}
	t.Cleanup(func() { sshScript = oldS })
	res, err := RunOnNode(context.Background(), config.Default(), "dino", "/m/x.gguf", 0, 0, 0, false)
	if err != nil || res.Status != StatusStarting || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "offloading 40 layers") || !strings.Contains(res.Warnings[0], "Nothing was stopped") {
		t.Fatalf("%v %+v", err, res)
	}
	healthy = true
	res, err = RunOnNode(context.Background(), config.Default(), "dino", "/m/x.gguf", 0, 0, 0, false)
	if err != nil || res.Status != StatusRunning || res.MasterPID != 90 {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestRunOnNodeLocalStartFailureAndPathSafety(t *testing.T) {
	e := newExecEnv(t)
	e.proc = func(string) (int, <-chan error, error) { return 0, nil, errors.New("no such file") }
	_, err := RunOnNode(context.Background(), config.Default(), "testhost", "/m/x.gguf", 0, 0, 0, false)
	if err == nil || !(strings.Contains(err.Error(), "failed to start local model") || strings.Contains(err.Error(), "no engine on this machine")) {
		t.Fatalf("%v", err)
	}
	if _, err := RunOnNode(context.Background(), config.Default(), "dino", "/m/x; rm -rf ~.gguf", 0, 0, 0, true); err == nil {
		t.Fatal("an unsafe model path must be refused")
	}
}

// `fleet plan --cmd` and `fleet deploy` are one builder: the candidate's commands carry the deploy's flags (the old
// plan.Candidate.Commands built its own worker line and flags).
func TestCandidateCommandsAreTheDeploysCommands(t *testing.T) {
	stubMeta(t, bigModel())
	c := plan.Candidate{Kind: "split", Master: "dino", Parts: []plan.Part{{Node: "dino", Layers: 30, First: 10, Output: true}, {Node: "papa", Layers: 10, First: 0}}}
	res, err := CandidateCommands(config.Default(), "/m/x.gguf", c, 4096, 1, "none")
	if err != nil {
		t.Fatal(err)
	}
	want := dry(t, DeployOptions{ModelPath: "/m/x.gguf", MasterNode: "dino", WorkerNodes: []string{"papa"}, Candidate: &c, Ctx: 4096, Np: 1, SpecType: "none"})
	if res.MasterCmd != want.MasterCmd || res.MasterEnv != want.MasterEnv || res.WorkerCmds["papa"] != want.WorkerCmds["papa"] {
		t.Fatalf("%s\n%s", res.MasterCmd, want.MasterCmd)
	}
	if !strings.Contains(res.MasterCmd, "--fit off") || !strings.Contains(res.WorkerCmds["papa"], "ring-worker") && !strings.Contains(res.WorkerCmds["papa"], "RW") {
		t.Fatalf("not the deploy's lines: %s / %s", res.MasterCmd, res.WorkerCmds["papa"])
	}
	// a remote worker's line resolves the binary on ITS machine (the old plan line was 'fleet ring worker -H 127.0.0.1')
	if strings.Contains(res.WorkerCmds["papa"], "127.0.0.1") {
		t.Fatalf("%s", res.WorkerCmds["papa"])
	}
}

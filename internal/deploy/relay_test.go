package deploy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
)

func relayPlan(t *testing.T, relay bool) *StagePipePlan {
	t.Helper()
	p, err := PlanStagePipe(reportCandidate(), toyModel(), ChainStageV1, StagePipeOptions{Host: hostMap, Relay: relay})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// --relay: a stage dials 127.0.0.1:<tunnel port> instead of the next machine's address.
func TestPlanRelayRewritesNext(t *testing.T) {
	direct := relayPlan(t, false)
	if len(direct.Relays) != 0 || !strings.Contains(direct.Stages[0].Next, "10.0.4.21") {
		t.Fatalf("without --relay: %v %q", direct.Relays, direct.Stages[0].Next)
	}
	p := relayPlan(t, true)
	if len(p.Relays) != 2 {
		t.Fatalf("relays: %+v", p.Relays)
	}
	if got := p.Stages[0].Next; got != "tcp://127.0.0.1:53100" {
		t.Errorf("stage 0 next: %q", got)
	}
	if got := p.Stages[1].Next; got != "tcp://127.0.0.1:53101" {
		t.Errorf("stage 1 next: %q", got)
	}
	if !strings.Contains(p.Stages[0].Command, "STAGE_NEXT=tcp://127.0.0.1:53100") {
		t.Errorf("command: %s", p.Stages[0].Command)
	}
	r := p.Relays[1]
	if r.From != "pcie" || r.To != "delta2" || r.FromStage != 1 || r.Port != 53101 || r.ToPort != p.Stages[2].Port || r.Via == "" {
		t.Errorf("relay 1: %+v", r)
	}
}

// the tunnel legs: -L at Via to the next stage, -R at the stage that dials; none for an end that is Via.
func TestRelayLegArgs(t *testing.T) {
	rel := StageRelay{Relay: config.Relay{Via: "hub", From: "a", To: "b", Port: 53100}, ToPort: 5600}
	l, r := relayLegArgs(rel)
	if got := strings.Join(l, " "); !strings.Contains(got, "-L 127.0.0.1:53100:127.0.0.1:5600 -- b") || !strings.Contains(got, "ExitOnForwardFailure=no") {
		t.Errorf("L: %s", got)
	}
	if got := strings.Join(r, " "); !strings.Contains(got, "-R 127.0.0.1:53100:127.0.0.1:53100 -- a") {
		t.Errorf("R: %s", got)
	}
	// the ingress is on Via: only the -L leg
	rel.From = "hub"
	if l, r = relayLegArgs(rel); l == nil || r != nil {
		t.Errorf("From is Via: L=%v R=%v", l, r)
	}
	// the next stage is on Via: the -R leg goes straight to its port
	rel.From, rel.To = "a", "hub"
	l, r = relayLegArgs(rel)
	if l != nil || !strings.Contains(strings.Join(r, " "), "-R 127.0.0.1:53100:127.0.0.1:5600 -- a") {
		t.Errorf("To is Via: L=%v R=%v", l, r)
	}
}

func TestRelayLegsForStop(t *testing.T) {
	got := RelayLegs(config.Relay{Via: "hub", From: "a", To: "b", Port: 53100})
	// the -R client first, then the -L listener, then the -R listener on the stage machine
	if len(got) != 3 || got[0].Names[0] != RelayClientName || !got[0].Local || got[1].Node != "hub" || got[1].Names[0] != "ssh" || got[2].Node != "a" || got[2].Local || got[2].Port != 53100 {
		t.Errorf("%+v", got)
	}
	if got := RelayLegs(config.Relay{Via: "hub", From: "hub", To: "b", Port: 1}); len(got) != 1 || got[0].Node != "hub" {
		t.Errorf("From is Via: %+v", got)
	}
}

// recRunner records the order of tunnel and stage steps.
type recRunner struct {
	DryRunner
	log       []string
	failReady int
}

func (r *recRunner) Launch(s StageLaunch) error {
	r.log = append(r.log, fmt.Sprintf("launch %d", s.Index))
	return nil
}
func (r *recRunner) WaitReady(s StageLaunch, d time.Duration) error {
	if s.Index == r.failReady {
		return errors.New("refused")
	}
	return nil
}
func (r *recRunner) OpenRelay(rel StageRelay) error {
	r.log = append(r.log, fmt.Sprintf("open %d", rel.FromStage))
	return nil
}
func (r *recRunner) CloseRelay(rel StageRelay) {
	r.log = append(r.log, fmt.Sprintf("close %d", rel.FromStage))
}

// a tunnel opens before the stage that dials it starts; a failed deploy closes the tunnels again.
func TestRunStagePipeOpensAndClosesRelays(t *testing.T) {
	p := relayPlan(t, true)
	r := &recRunner{failReady: -1}
	if err := RunStagePipe(p, r, time.Second); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(r.log, ","); got != "launch 2,open 1,launch 1,open 0,launch 0" {
		t.Errorf("order: %s", got)
	}
	f := &recRunner{failReady: 0}
	if err := RunStagePipe(p, f, time.Second); err == nil {
		t.Fatal("want an error")
	}
	if got := strings.Join(f.log, ","); !strings.HasSuffix(got, "close 1,close 0") && !strings.HasSuffix(got, "close 0,close 1") {
		t.Errorf("a failed deploy must close both tunnels: %s", got)
	}
	// a runner that cannot open tunnels is refused before anything starts
	if err := RunStagePipe(p, &plainRunner{}, time.Second); err == nil || !strings.Contains(err.Error(), "tunnels") {
		t.Errorf("plain runner: %v", err)
	}
}

type plainRunner struct{ DryRunner }

func (plainRunner) OpenRelay() {} // wrong signature on purpose: not a relayOpener

// OpenRelay starts the legs detached, waits for the ports and closes a stale tunnel first; an ssh that exits at once fails it.
func TestLiveOpenRelay(t *testing.T) {
	oldS, oldL, oldP, oldPoll := sshScript, localScript, startLocalProc, readyPoll
	oldOpen := localPortOpen
	t.Cleanup(func() {
		sshScript, localScript, startLocalProc, readyPoll, localPortOpen = oldS, oldL, oldP, oldPoll, oldOpen
	})
	readyPoll = time.Millisecond
	var started [][]string
	var scripts []string
	exit := false
	startLocalProc = func(_ context.Context, bin string, args, _ []string, _ string) (int, <-chan error, error) {
		started = append(started, append([]string{bin}, args...))
		ch := make(chan error, 1)
		if exit {
			ch <- errors.New("exit 255")
		}
		return 4242, ch, nil
	}
	localScript = func(_ context.Context, s string) ([]byte, error) {
		scripts = append(scripts, "local:"+s)
		return nil, nil
	}
	sshScript = func(_ context.Context, node, s string) ([]byte, error) {
		scripts = append(scripts, node+":"+s)
		return []byte("FLEET_READY\n"), nil
	}
	localPortOpen = func(string) bool { return true }
	rel := StageRelay{Relay: config.Relay{Via: "hub", From: "a", To: "b", Port: 53100}, ToPort: 5600}
	r := &LiveStageRunner{}
	if err := r.OpenRelay(rel); err != nil {
		t.Fatal(err)
	}
	if len(started) != 2 || started[0][0] != "ssh" || !strings.Contains(strings.Join(started[0], " "), "-L") || !strings.Contains(strings.Join(started[1], " "), "-R") {
		t.Errorf("started: %v", started)
	}
	if len(scripts) < 3 || !strings.Contains(scripts[0], "-R 127.0.0.1:53100:") || !strings.Contains(scripts[1], "fleet_stop_port") || !strings.Contains(scripts[2], "fleet_stop_port") {
		t.Errorf("a stale tunnel on the port must be closed first: %v", scripts)
	}
	started, exit = nil, true
	if err := r.OpenRelay(rel); err == nil || !strings.Contains(err.Error(), "exited") {
		t.Errorf("an ssh that exits at once: %v", err)
	}
}

// The -R client is found by its command line and stopped; another tunnel port's client stays.
func TestRelayClientScriptStopsOnlyThePort(t *testing.T) {
	dir := t.TempDir()
	// a script named ssh has comm "ssh" (no exec: that would rename it) and an argv like the real client's
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	start := func(port int) *exec.Cmd {
		c := exec.Command(filepath.Join(dir, "ssh"), "-N", "-R", fmt.Sprintf("127.0.0.1:%d:127.0.0.1:5600", port), "--", "somewhere")
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })
		return c
	}
	a, _ := start(59871), start(59872)
	time.Sleep(300 * time.Millisecond)
	run := func(port int, dry bool) string {
		out, err := exec.Command("sh", "-c", RelayClientScript(port, dry)).Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	if got := run(59871, true); got != "FLEET_FOUND" {
		t.Fatalf("dry run: %q", got)
	}
	if got := run(59873, false); got != "FLEET_NONE" {
		t.Fatalf("no client on the port: %q", got)
	}
	if got := run(59871, false); got != "FLEET_STOPPED" {
		t.Fatalf("stop: %q", got)
	}
	done := make(chan error, 1)
	go func() { done <- a.Wait() }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the client of the port is still running")
	}
	if got := run(59872, true); got != "FLEET_FOUND" {
		t.Fatalf("the client of the other port must stay: %q", got)
	}
}

func hubChainPlan(t *testing.T) *StagePipePlan {
	t.Helper()
	p, err := PlanStagePipe(reportCandidate(), toyModel(), ChainStageV1, StagePipeOptions{Host: hostMap, ViaHub: true, HubURL: "https://hub.example"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// --via-hub: the same tunnel ports as --relay, but the relay carries the hub and no Via leg exists.
func TestPlanViaHubRelays(t *testing.T) {
	p := hubChainPlan(t)
	if len(p.Relays) != 2 || p.Relays[0].Hub != "https://hub.example" || p.Stages[0].Next != "tcp://127.0.0.1:53100" {
		t.Fatalf("%+v next %q", p.Relays, p.Stages[0].Next)
	}
	if r := relayPlan(t, true); r.Relays[0].Hub != "" {
		t.Errorf("--relay must not carry a hub: %+v", r.Relays[0])
	}
}

// a hub link has one process: the stream connect on the machine of the stage that dials.
func TestRelayLegsViaHub(t *testing.T) {
	got := RelayLegs(config.Relay{Via: "hub", From: "a", To: "b", Port: 53100, Hub: "https://h"})
	if len(got) != 1 || got[0].Node != "a" || got[0].Port != 53100 || len(got[0].Names) != 1 || got[0].Names[0] != "fleet" {
		t.Errorf("%+v", got)
	}
}

// OpenRelay through the hub: the next stage's port is declared on its node, the stream connect runs on the dialing stage's machine.
func TestLiveOpenHubRelay(t *testing.T) {
	oldS, oldL, oldP, oldPoll, oldOpen := sshScript, localScript, startLocalProc, readyPoll, localPortOpen
	t.Cleanup(func() {
		sshScript, localScript, startLocalProc, readyPoll, localPortOpen = oldS, oldL, oldP, oldPoll, oldOpen
	})
	readyPoll = time.Millisecond
	var ssh []string
	sshScript = func(_ context.Context, node, s string) ([]byte, error) {
		ssh = append(ssh, node+":"+s)
		switch {
		case strings.Contains(s, "--add-stream-port"):
			if !strings.HasPrefix(s, "export PATH=$HOME/.local/bin:$PATH\n") {
				t.Errorf("the join script must put ~/.local/bin on PATH: %q", s)
			}
			return []byte("FLEET_HUB_JOINED\n"), nil
		case strings.Contains(s, "FLEET_PID") || strings.Contains(s, "nohup"):
			return []byte("FLEET_PID 77\nFLEET_READY\n"), nil
		}
		return nil, nil
	}
	localScript = func(context.Context, string) ([]byte, error) { return nil, nil }
	rel := StageRelay{Relay: config.Relay{Via: "hub", From: "e12-a", To: "e12-b", Port: 53100, Hub: "https://h"}, ToPort: 5600}
	r := &LiveStageRunner{}
	if err := r.OpenRelay(rel); err != nil {
		t.Fatal(err)
	}
	var joinNode, streamNode, streamLine string
	for _, s := range ssh {
		node, body, _ := strings.Cut(s, ":")
		if strings.Contains(body, "fleet join https://h --add-stream-port 5600") {
			joinNode = node
		}
		if strings.Contains(body, "stream connect e12-b 5600 --listen 127.0.0.1:53100 --hub https://h") {
			streamNode, streamLine = node, body
		}
	}
	if joinNode != "e12-b" || streamNode != "e12-a" || streamLine == "" {
		t.Errorf("join on %q, stream on %q: %v", joinNode, streamNode, ssh)
	}
	if len(r.Warnings) != 0 {
		t.Errorf("warnings %v", r.Warnings)
	}
}

// --relay and --via-hub together are refused; a hub link without the join answer is a warning, not a failure.
func TestDeployChainRelayAndHubExclusive(t *testing.T) {
	_, err := DeployChain(context.Background(), reportCandidate(), "/nonexistent.gguf", StagePipeOptions{Relay: true, ViaHub: true}, &DryRunner{}, nil)
	if err == nil || !strings.Contains(err.Error(), "pick one") {
		t.Fatalf("%v", err)
	}
}

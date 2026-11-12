package deploy

import (
	"context"
	"strings"
	"testing"
	"time"
)

func stubReach(t *testing.T, answers func(addr string) bool) *[][2]int {
	t.Helper()
	oldD, oldT, oldF, oldH := dialTCP, startTunnel, freePort, isThisMachine
	var started [][2]int
	dialTCP = func(_ context.Context, addr string, _ time.Duration) bool { return answers(addr) }
	next := 40000
	freePort = func() (int, error) { next++; return next, nil }
	startTunnel = func(_ context.Context, node string, f [][2]int) (func(), error) {
		started = append(started, f...)
		return func() {}, nil
	}
	isThisMachine = func(n string) bool { return n == "" || n == "laptop" }
	t.Cleanup(func() { dialTCP, startTunnel, freePort, isThisMachine = oldD, oldT, oldF, oldH })
	return &started
}

func TestReachEndpointDirectWhenItAnswers(t *testing.T) {
	started := stubReach(t, func(string) bool { return true })
	r, err := ReachEndpoint(context.Background(), "dino", "http://10.0.1.13:18090/v1", 50155)
	if err != nil || !r.Direct || r.URL != "http://10.0.1.13:18090/v1" || r.StatsPort != 50155 || r.Note != "" || len(*started) != 0 {
		t.Fatalf("%+v %v %v", r, err, started)
	}
	r.Close()
}

// The master is on a LAN this machine is not on: the registered URL is refused, so the endpoint is reached through
// an ssh tunnel to the master's machine, stats server included (the incident: "connection refused" read as a dead ring).
func TestReachEndpointTunnelsWhenLANAddressIsRefused(t *testing.T) {
	started := stubReach(t, func(addr string) bool { return !strings.HasPrefix(addr, "10.0.") })
	r, err := ReachEndpoint(context.Background(), "dino", "http://10.0.1.13:18090/v1", 50155)
	if err != nil {
		t.Fatal(err)
	}
	if r.Direct || r.URL != "http://127.0.0.1:40001/v1" || r.StatsPort != 40002 {
		t.Fatalf("%+v", r)
	}
	want := [][2]int{{40001, 18090}, {40002, 50155}}
	if len(*started) != 2 || (*started)[0] != want[0] || (*started)[1] != want[1] {
		t.Fatalf("forwards %v", *started)
	}
	if !strings.Contains(r.Note, "ssh tunnel via dino") || r.Close == nil {
		t.Fatalf("%q", r.Note)
	}
}

func TestReachEndpointNeedsAMachineToTunnelThrough(t *testing.T) {
	stubReach(t, func(string) bool { return false })
	if _, err := ReachEndpoint(context.Background(), "", "http://10.0.1.13:18090/v1", 0); err == nil || !strings.Contains(err.Error(), "--via") {
		t.Fatalf("%v", err)
	}
	// the master is this machine: nothing to tunnel through either
	if _, err := ReachEndpoint(context.Background(), "laptop", "http://10.0.1.13:18090/v1", 0); err == nil {
		t.Fatal("want an error")
	}
	// loopback is always direct
	r, err := ReachEndpoint(context.Background(), "dino", "http://127.0.0.1:18090/v1", 0)
	if err != nil || !r.Direct {
		t.Fatalf("%+v %v", r, err)
	}
}

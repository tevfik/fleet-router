package main

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/tevfik/fleet-router/internal/config"
)

func TestEndpointCommands(t *testing.T) {
	if _, err := exec.LookPath("fuser"); err != nil {
		t.Skip("fuser (psmisc) is not installed: the stop dry run cannot name a port owner")
	}
	isolate(t)
	t.Setenv("LLAMA_CACHE", "")
	startDaemon(t, &config.Config{Name: "alpha"}, true)

	out := runOK(t, func() error {
		return cmdEndpoint([]string{"add", "w-qwen", "http://127.0.0.1:1/v1", "--model", "qwen.gguf", "--node", "w", "--ctx", "8192", "--tags", "fast, gpu"})
	})
	contains(t, out, "endpoint w-qwen registered")
	out = runOK(t, func() error { return cmdEndpoint(nil) })
	contains(t, out, "ENDPOINT", "w-qwen", "qwen.gguf", "8k", "fast,gpu", "caps guessed from the name")
	_, _, err := run(t, func() error { return cmdEndpoint([]string{"add", "x"}) })
	wantErr(t, err, "usage: fleet endpoint add")

	// a role that names the endpoint blocks its removal, with the role named; --mend-roles mends it
	runOK(t, func() error { return cmdRole([]string{"set", "coder", "w-qwen"}) })
	_, _, err = run(t, func() error { return cmdEndpoint([]string{"rm", "w-qwen"}) })
	wantErr(t, err, "role(s) coder")
	out = runOK(t, func() error { return cmdEndpoint([]string{"rm", "w-qwen", "--mend-roles"}) })
	contains(t, out, "removed from the configuration", "role coder", "deleted", "no process was stopped")
	out = runOK(t, func() error { return cmdEndpoint([]string{"list"}) })
	if strings.Contains(out, "w-qwen") {
		t.Fatalf("removed:\n%s", out)
	}

	// stop: a dry run says what it would do and touches nothing (nothing listens on port 1; the port is not the daemon's)
	runOK(t, func() error { return cmdEndpoint([]string{"add", "loc", "http://127.0.0.1:1/v1", "--node", "alpha"}) })
	out = runOK(t, func() error { return cmdEndpoint([]string{"stop", "loc", "--dry-run"}) })
	contains(t, out, "stop loc", "dry run", "master", ":1")
	_, _, err = run(t, func() error { return cmdEndpoint([]string{"stop", "ghost"}) })
	wantErr(t, err, "no such endpoint")
	_, _, err = run(t, func() error { return cmdRun([]string{"stop", "nope"}) })
	wantErr(t, err, "no such run")

	// D1: fleet endpoint prune
	// loc is down (connection refused on :1); default dry-run lists it without removing:
	out = runOK(t, func() error { return cmdEndpoint([]string{"prune"}) })
	contains(t, out, "would remove 1 endpoint(s)", "loc", "dry-run")

	// Verify it was NOT deleted
	out = runOK(t, func() error { return cmdEndpoint([]string{"list"}) })
	contains(t, out, "loc")

	// Prune with --dry-run=false removes it:
	out = runOK(t, func() error { return cmdEndpoint([]string{"prune", "--dry-run=false"}) })
	contains(t, out, "removed from the configuration: loc")

	// Verify it IS deleted
	out = runOK(t, func() error { return cmdEndpoint([]string{"list"}) })
	if strings.Contains(out, "loc") {
		t.Fatalf("loc should have been pruned:\n%s", out)
	}

	// Now with no down endpoints left, prune reports nothing to prune:
	out = runOK(t, func() error { return cmdEndpoint([]string{"prune"}) })
	contains(t, out, "nothing to prune")
}

func TestParseDurationDays(t *testing.T) {
	d, err := parseDurationDays("7d")
	if err != nil || d.Hours() != 7*24 {
		t.Fatalf("7d: %v %v", d, err)
	}
	d, err = parseDurationDays("3days")
	if err != nil || d.Hours() != 3*24 {
		t.Fatalf("3days: %v %v", d, err)
	}
	d, err = parseDurationDays("12h")
	if err != nil || d.Hours() != 12 {
		t.Fatalf("12h: %v %v", d, err)
	}
	_, err = parseDurationDays("invalid")
	if err == nil {
		t.Fatal("expected error for invalid duration")
	}
}

func TestEndpointPruneOlderThan(t *testing.T) {
	isolate(t)
	t.Setenv("LLAMA_CACHE", "")
	startDaemon(t, &config.Config{Name: "alpha"}, true)

	runOK(t, func() error { return cmdEndpoint([]string{"add", "loc", "http://127.0.0.1:1/v1", "--node", "alpha"}) })
	runOK(t, func() error { return cmdEndpoint([]string{"list"}) })

	// Prune with --older-than 7d: loc was probed just now, so it is not older than 7d
	out := runOK(t, func() error { return cmdEndpoint([]string{"prune", "--older-than", "7d"}) })
	contains(t, out, "nothing to prune")

	// Prune without --older-than: matches loc
	out = runOK(t, func() error { return cmdEndpoint([]string{"prune"}) })
	contains(t, out, "would remove 1 endpoint(s)", "loc")
}

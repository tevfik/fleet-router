package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/profile"
	"github.com/tevfik/fleet-router/internal/server"
	"github.com/tevfik/fleet-router/internal/setup"
)

func TestDoctorLingerCheck(t *testing.T) {
	// 1. Error from LoginctlLinger -> ok=true, msg="" (graceful skip on non-systemd)
	ok, msg := checkLinger(func() (string, error) {
		return "", fmt.Errorf("command not found")
	}, "alice")
	if !ok || msg != "" {
		t.Fatalf("expected skip on error, got ok=%v, msg=%q", ok, msg)
	}

	// 2. Linger is "yes" -> ok=true, msg="systemd linger is enabled"
	ok, msg = checkLinger(func() (string, error) {
		return "yes", nil
	}, "alice")
	if !ok || msg != "systemd linger is enabled" {
		t.Fatalf("expected linger enabled, got ok=%v, msg=%q", ok, msg)
	}

	// 3. Linger is "no" -> ok=false, warning recommending loginctl enable-linger
	ok, msg = checkLinger(func() (string, error) {
		return "no", nil
	}, "alice")
	if ok {
		t.Fatalf("expected linger failure for 'no', got ok=true")
	}
	if !strings.Contains(msg, "systemd linger is off (Linger=no)") {
		t.Errorf("expected warning to mention Linger=no, got: %s", msg)
	}
	if !strings.Contains(msg, "loginctl enable-linger alice") {
		t.Errorf("expected suggestion to run loginctl enable-linger alice, got: %s", msg)
	}
}

func TestDoctorDuplicateEndpointRunsCheck(t *testing.T) {
	// Empty runs -> no warnings
	if w := checkDuplicateEndpointRuns(nil); len(w) != 0 {
		t.Fatalf("expected 0 warnings, got %v", w)
	}

	// Single run per endpoint -> no warnings
	runs := []server.Run{
		{ID: "run-1", Endpoint: "model-a"},
		{ID: "run-2", Endpoint: "model-b"},
	}
	if w := checkDuplicateEndpointRuns(runs); len(w) != 0 {
		t.Fatalf("expected 0 warnings, got %v", w)
	}

	// Ended run + active run on same endpoint -> no warnings
	runs = []server.Run{
		{ID: "run-1", Endpoint: "model-a", Ended: time.Now()},
		{ID: "run-2", Endpoint: "model-a"},
	}
	if w := checkDuplicateEndpointRuns(runs); len(w) != 0 {
		t.Fatalf("expected 0 warnings for ended run, got %v", w)
	}

	// Lost run + active run on same endpoint -> no warnings
	runs = []server.Run{
		{ID: "run-1", Endpoint: "model-a", Lost: "crashed"},
		{ID: "run-2", Endpoint: "model-a"},
	}
	if w := checkDuplicateEndpointRuns(runs); len(w) != 0 {
		t.Fatalf("expected 0 warnings for lost run, got %v", w)
	}

	// Two active runs pointing to same endpoint -> warning
	runs = []server.Run{
		{ID: "run-2", Endpoint: "model-a"},
		{ID: "run-1", Endpoint: "model-a"},
	}
	w := checkDuplicateEndpointRuns(runs)
	if len(w) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(w), w)
	}
	if !strings.Contains(w[0], `endpoint "model-a" has 2 active runs (run-1, run-2)`) {
		t.Errorf("unexpected warning text: %s", w[0])
	}
	if !strings.Contains(w[0], "fleet run end <id> --record-only") {
		t.Errorf("expected hint to use fleet run end <id> --record-only, got: %s", w[0])
	}
}

func TestDoctorUnmanagedRunsCheck(t *testing.T) {
	// Ended or lost runs -> no warnings
	runs := []server.Run{
		{ID: "run-ended", Endpoint: "ep1", Model: "m.gguf", Ended: time.Now()},
		{ID: "run-lost", Endpoint: "ep2", Model: "m.gguf", Lost: "dead"},
	}
	if w := checkUnmanagedRuns(runs, nil, nil); len(w) != 0 {
		t.Fatalf("expected 0 warnings for inactive runs, got %v", w)
	}

	// Active run with no associated profile -> warning
	runs = []server.Run{
		{ID: "run-solo", Endpoint: "solo-ep", Model: "solo.gguf"},
	}
	w := checkUnmanagedRuns(runs, nil, nil)
	if len(w) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(w), w)
	}
	if !strings.Contains(w[0], "active run run-solo (solo-ep, model solo.gguf) is running without systemd oversight (PPID 1, no associated fleet-profile-*.service)") {
		t.Errorf("unexpected warning text: %s", w[0])
	}

	// Active run belonging to profile "qwen", autostart is disabled
	prof := &profile.Profile{
		Name: "qwen",
		Runs: []profile.Run{{RunID: "run-qwen"}},
	}
	runs = []server.Run{
		{ID: "run-qwen", Endpoint: "qwen-ep", Model: "qwen.gguf"},
	}
	autostartMap := map[string]bool{"qwen": false}
	mockAutostart := func(name string) bool { return autostartMap[name] }

	w = checkUnmanagedRuns(runs, []*profile.Profile{prof}, mockAutostart)
	if len(w) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(w), w)
	}
	if !strings.Contains(w[0], "fleet-profile-qwen.service") || !strings.Contains(w[0], "fleet profile autostart qwen on") {
		t.Errorf("expected warning mentioning unit and autostart command, got: %s", w[0])
	}

	// Active run belonging to profile "qwen", autostart is enabled -> NO warnings!
	autostartMap["qwen"] = true
	w = checkUnmanagedRuns(runs, []*profile.Profile{prof}, mockAutostart)
	if len(w) != 0 {
		t.Fatalf("expected 0 warnings when autostart is enabled, got %v", w)
	}

	// Active run matching profile by endpoint name
	prof2 := &profile.Profile{
		Name: "qwen-ep",
	}
	autostartMap["qwen-ep"] = false
	runs = []server.Run{
		{ID: "run-matched-by-ep", Endpoint: "qwen-ep", Model: "qwen.gguf"},
	}
	w = checkUnmanagedRuns(runs, []*profile.Profile{prof2}, mockAutostart)
	if len(w) != 1 || !strings.Contains(w[0], "fleet profile autostart qwen-ep on") {
		t.Fatalf("expected warning for matched profile with disabled autostart, got %v", w)
	}

	autostartMap["qwen-ep"] = true
	w = checkUnmanagedRuns(runs, []*profile.Profile{prof2}, mockAutostart)
	if len(w) != 0 {
		t.Fatalf("expected 0 warnings when matched profile autostart enabled, got %v", w)
	}
}

func TestDoctorCmdLifecycleOutput(t *testing.T) {
	isolate(t)

	// Save original LoginctlLinger and restore
	origLinger := setup.LoginctlLinger
	t.Cleanup(func() { setup.LoginctlLinger = origLinger })

	// Stub linger to "no"
	setup.LoginctlLinger = func() (string, error) {
		return "no", nil
	}
	t.Setenv("USER", "testuser")

	cfg := config.Default()
	cfg.Listen = "127.0.0.1:0"
	cfg.Save(config.Path())

	out := runOK(t, cmdDoctor)
	contains(t, out, "✗ systemd linger is off (Linger=no): user services (fleet.service) will not start at boot without active login — run `loginctl enable-linger testuser`")

	// Now stub linger to "yes"
	setup.LoginctlLinger = func() (string, error) {
		return "yes", nil
	}
	out = runOK(t, cmdDoctor)
	contains(t, out, "✓ systemd linger is enabled")
}

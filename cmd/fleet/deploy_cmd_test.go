package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"
)

func TestCmdDeployDryRun(t *testing.T) {
	modelPath := "/home/user/models/LFM2-8B-A1B-Q4_K_M.gguf"
	if _, err := os.Stat(modelPath); os.IsNotExist(err) {
		t.Skipf("skipping test: %s does not exist", modelPath)
	}
	args := []string{
		modelPath,
		"--strategy", "latency",
		"--workers", "victor",
		"--role", "coder",
		"--via-hub",     // victor has no direct LAN IP on this host; implicit hub fallback is refused without --via-hub
		"--force-split", // the live planner may prefer one machine; this test is about the split commands
		"--dry-run",
	}

	err := cmdDeploy(args)
	if err != nil {
		t.Fatalf("cmdDeploy dry-run failed: %v", err)
	}
}

func TestCmdRunOnDryRun(t *testing.T) {
	modelPath := "/home/user/models/LFM2-8B-A1B-Q4_K_M.gguf"
	if _, err := os.Stat(modelPath); os.IsNotExist(err) {
		t.Skipf("skipping test: %s does not exist", modelPath)
	}
	args := []string{
		"victor",
		modelPath,
		"--port", "18092",
		"--dry-run",
	}

	err := cmdRunOn(args)
	if err != nil {
		t.Fatalf("cmdRunOn dry-run failed: %v", err)
	}
}

func TestCmdDeployErrors(t *testing.T) {
	// Missing args
	err := cmdDeploy([]string{})
	if err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Errorf("expected usage error, got %v", err)
	}
}

// Without a terminal (ssh without a tty, nohup, CI) the deploy's progress comes line by line, as it happens; the
// loading line is throttled, on a terminal it is redrawn in place.
func TestProgressPrinter(t *testing.T) {
	now := time.Unix(1000, 0)
	var pipe bytes.Buffer
	p := newProgressPrinter(&pipe, false, func() time.Time { return now })
	p("[2/4] starting workers")
	p("[4/4] ⏳ Loading model layers into memory (0s)...")
	now = now.Add(2 * time.Second)
	p("[4/4] ⏳ Loading model layers into memory (2s)...")
	now = now.Add(31 * time.Second)
	p("[4/4] ⏳ Loading model layers into memory (33s)...")
	p("✓ Model loaded and healthy!")
	lines := strings.Split(strings.TrimSpace(pipe.String()), "\n")
	if len(lines) != 4 || strings.Contains(pipe.String(), "\r") || !strings.Contains(lines[1], "(0s)") || !strings.Contains(lines[2], "(33s)") {
		t.Fatalf("%q", lines)
	}
	var tty bytes.Buffer
	q := newProgressPrinter(&tty, true, time.Now)
	q("[4/4] ⏳ Loading model layers into memory (1s)...")
	q("[4/4] ⏳ Loading model layers into memory (2s)...")
	q("done")
	if strings.Count(tty.String(), "\r") != 2 || !strings.HasSuffix(tty.String(), "\ndone\n") {
		t.Fatalf("%q", tty.String())
	}
}

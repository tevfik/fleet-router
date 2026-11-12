package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tevfik/fleet-router/internal/plan"
)

func TestEngineOf(t *testing.T) {
	for args, want := range map[string]string{
		"m.gguf":                          "",
		"m.gguf --engine ring":            "ring",
		"m.gguf --engine chain":           "chain",
		"--engine=chain m":                "chain",
		"--engine=stage-pipe m":           "stage-pipe",
		"m --engine stage-pipe --dry-run": "stage-pipe",
	} {
		if got, err := engineOf(strings.Fields(args)); err != nil || got != want {
			t.Errorf("%q: %q %v", args, got, err)
		}
	}
	if _, err := engineOf([]string{"--engine", "mesh"}); err == nil {
		t.Error("an unknown engine is refused")
	}
}

func TestDeployStagePipeDryRunOnly(t *testing.T) {
	// the live start below must not reach the developer's real daemon (it recorded a fake run "a→b" there on every go test)
	t.Setenv("FLEET_LISTEN", "127.0.0.1:1")
	t.Setenv("FLEET_CONFIG", filepath.Join(t.TempDir(), "fleet.yaml"))
	dir := t.TempDir()
	desc := map[string]any{"version": 1, "name": "toy", "architecture": "llama", "runtime_kind": "stage-pipe", "layer_count": 2,
		"hidden_size": 8, "vocab_size": 10, "embedding_at": "first", "head_at": "last",
		"layers": []map[string]any{
			{"index": 0, "bytes": 100, "state": "none"}, {"index": 1, "bytes": 100, "state": "none"}}}
	b, _ := json.Marshal(desc)
	mp := filepath.Join(dir, "fleet-model.json")
	os.WriteFile(mp, b, 0o644)
	res := plan.Result{Candidates: []plan.Candidate{{Kind: "split", Parts: []plan.Part{
		{Node: "a", Layers: 1, First: 0}, {Node: "b", Layers: 1, First: 1, Output: true}}}}}
	pb, _ := json.Marshal(res)
	pp := filepath.Join(dir, "plan.json")
	os.WriteFile(pp, pb, 0o644)

	if err := cmdDeployStagePipe([]string{mp, "--plan", pp, "--engine", "stage-pipe", "--dry-run"}); err != nil {
		t.Fatal(err)
	}
	// A live start runs in reverse order (stage 1 on node b first); with fake node b it fails to reach it
	if err := cmdDeployStagePipe([]string{mp, "--plan", pp, "--ready-timeout", "100ms"}); err == nil || !strings.Contains(err.Error(), "stage 1 (b)") {
		t.Fatalf("expected live start failure on stage 1 (node b): %v", err)
	}
	if err := cmdDeployStagePipe([]string{mp, "--plan", pp, "--pick", "3", "--dry-run"}); err == nil {
		t.Fatal("a candidate that is not in the plan")
	}

	// Test --proto flag variants: default (csp1), explicit v1, and invalid
	for _, proto := range []string{"csp1", "v1"} {
		if err := cmdDeployStagePipe([]string{mp, "--plan", pp, "--engine", "chain", "--proto", proto, "--dry-run"}); err != nil {
			t.Fatalf("--proto %s failed: %v", proto, err)
		}
	}
	if err := cmdDeployStagePipe([]string{mp, "--plan", pp, "--engine", "chain", "--proto", "invalid_proto", "--dry-run"}); err == nil {
		t.Fatal("expected error on invalid --proto")
	}

	// Test MoE flags in dry-run mode (M2)
	moeCases := [][]string{
		{"--cpu-moe"},
		{"--cpu-moe", "--moe-cache-mib", "3900"},
		{"--n-cpu-moe", "4", "--moe-cache-mib", "2048"},
	}
	for _, extra := range moeCases {
		cmdArgs := append([]string{mp, "--plan", pp, "--engine", "chain", "--dry-run"}, extra...)
		if err := cmdDeployStagePipe(cmdArgs); err != nil {
			t.Fatalf("cmdDeployStagePipe failed with %v: %v", extra, err)
		}
	}

	// Test MoE validation error cases (M2)
	badMoECases := []struct {
		name string
		args []string
		want string
	}{
		{"both-cpu-and-n-cpu", []string{"--cpu-moe", "--n-cpu-moe", "4"}, "cannot be used together"},
		{"negative-n-cpu", []string{"--n-cpu-moe", "-1"}, "cannot be negative"},
		{"negative-cache", []string{"--cpu-moe", "--moe-cache-mib", "-100"}, "cannot be negative"},
		{"cache-without-moe", []string{"--moe-cache-mib", "3900"}, "--moe-cache-mib requires"},
	}
	for _, tc := range badMoECases {
		t.Run(tc.name, func(t *testing.T) {
			cmdArgs := append([]string{mp, "--plan", pp, "--engine", "chain", "--dry-run"}, tc.args...)
			err := cmdDeployStagePipe(cmdArgs)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got: %v", tc.want, err)
			}
		})
	}

	// Test deploying without --plan (U3): planner generates candidate
	if err := cmdDeployStagePipe([]string{mp, "--engine", "chain", "--dry-run"}); err != nil {
		t.Fatalf("cmdDeployStagePipe without --plan failed: %v", err)
	}
}

func TestDeployStagePipeNewFlags(t *testing.T) {
	dir := t.TempDir()
	desc := map[string]any{
		"version": 1, "name": "toy", "architecture": "qwen2", "runtime_kind": "stage-pipe", "layer_count": 4,
		"hidden_size": 8, "vocab_size": 10, "embedding_at": "first", "head_at": "last",
		"layers": []map[string]any{
			{"index": 0, "bytes": 100, "state": "none"},
			{"index": 1, "bytes": 100, "state": "none"},
			{"index": 2, "bytes": 100, "state": "none"},
			{"index": 3, "bytes": 100, "state": "none"},
		},
	}
	b, _ := json.Marshal(desc)
	mp := filepath.Join(dir, "fleet-model.json")
	os.WriteFile(mp, b, 0o644)

	// 1. --port, --ctx, --wire, --name
	if err := cmdDeployStagePipe([]string{
		mp, "--engine", "chain", "--port", "8080", "--ctx", "4096",
		"--wire", "bf16", "--name", "my-ep", "--dry-run",
	}); err != nil {
		t.Fatalf("new flags failed: %v", err)
	}

	// 2. --master and --workers split layers evenly
	if err := cmdDeployStagePipe([]string{
		mp, "--engine", "chain", "--master", "nodeA", "--workers", "nodeB", "--dry-run",
	}); err != nil {
		t.Fatalf("--master and --workers failed: %v", err)
	}

	// 3. Unsupported flags must report "--flag: not supported with --engine chain"
	unsupportedCases := []struct {
		name string
		args []string
		flag string
	}{
		{"ngl", []string{"--ngl", "20"}, "ngl"},
		{"mmap", []string{"--mmap"}, "mmap"},
		{"ts", []string{"--ts", "10,10"}, "ts"},
	}
	for _, tc := range unsupportedCases {
		t.Run(tc.name, func(t *testing.T) {
			cmdArgs := append([]string{mp, "--engine", "chain", "--dry-run"}, tc.args...)
			err := cmdDeployStagePipe(cmdArgs)
			if err == nil {
				t.Fatalf("expected error for unsupported flag %s", tc.flag)
			}
			wantMsg := fmt.Sprintf("--%s: not supported with --engine chain", tc.flag)
			if !strings.Contains(err.Error(), wantMsg) {
				t.Fatalf("expected error containing %q, got: %v", wantMsg, err)
			}
		})
	}

	// 4. Unsupported architecture blocked by ChainSupport
	badArchDesc := map[string]any{
		"version": 1, "name": "badarch", "architecture": "lfm2moe", "runtime_kind": "stage-pipe", "layer_count": 1,
		"hidden_size": 8, "vocab_size": 10, "embedding_at": "first", "head_at": "last",
		"layers": []map[string]any{
			{"index": 0, "bytes": 100, "state": "none"},
		},
	}
	badB, _ := json.Marshal(badArchDesc)
	badMP := filepath.Join(dir, "bad-model.json")
	os.WriteFile(badMP, badB, 0o644)

	err := cmdDeployStagePipe([]string{badMP, "--engine", "chain", "--dry-run"})
	if err == nil {
		t.Fatal("expected error for unsupported arch lfm2moe")
	}
	if !strings.Contains(err.Error(), "chain deploy not supported") || !strings.Contains(err.Error(), "lfm2moe") {
		t.Fatalf("expected ChainSupport error, got: %v", err)
	}
}

func TestDeployStagePipeLanes(t *testing.T) {
	dir := t.TempDir()
	desc := map[string]any{
		"version": 1, "name": "toy", "architecture": "qwen2", "runtime_kind": "stage-pipe", "layer_count": 4,
		"hidden_size": 8, "vocab_size": 10, "embedding_at": "first", "head_at": "last",
		"layers": []map[string]any{
			{"index": 0, "bytes": 100, "state": "none"},
			{"index": 1, "bytes": 100, "state": "none"},
			{"index": 2, "bytes": 100, "state": "none"},
			{"index": 3, "bytes": 100, "state": "none"},
		},
	}
	b, _ := json.Marshal(desc)
	mp := filepath.Join(dir, "fleet-model.json")
	os.WriteFile(mp, b, 0o644)
	res := plan.Result{Candidates: []plan.Candidate{{Kind: "split", Parts: []plan.Part{
		{Node: "a", Layers: 2, First: 0}, {Node: "b", Layers: 2, First: 2, Output: true}}}}}
	pb, _ := json.Marshal(res)
	pp := filepath.Join(dir, "plan.json")
	os.WriteFile(pp, pb, 0o644)

	// 1. Valid --lanes 2 with --np 3: dry-run should succeed and output --lanes 2 ONLY on ingress
	stdout, _, err := run(t, func() error {
		return cmdDeployStagePipe([]string{mp, "--plan", pp, "--engine", "chain", "--np", "3", "--lanes", "2", "--dry-run"})
	})
	if err != nil {
		t.Fatalf("cmdDeployStagePipe with --np 3 --lanes 2 failed: %v", err)
	}
	lines := strings.Split(stdout, "\n")
	var ingressCmd, stageCmd string
	for _, l := range lines {
		if strings.Contains(l, "chain") && strings.Contains(l, "--http") {
			ingressCmd = l
		} else if strings.Contains(l, "serve") {
			stageCmd = l
		}
	}
	if ingressCmd == "" {
		t.Fatalf("ingress command not found in dry-run output:\n%s", stdout)
	}
	if !strings.Contains(ingressCmd, "--lanes 2") {
		t.Errorf("ingress command missing --lanes 2: %s", ingressCmd)
	}
	if stageCmd != "" && strings.Contains(stageCmd, "--lanes") {
		t.Errorf("stage command must not contain --lanes: %s", stageCmd)
	}

	// 2. Regression: when --lanes is 1 or omitted, no --lanes appears anywhere
	for _, args := range [][]string{
		{mp, "--plan", pp, "--engine", "chain", "--np", "3", "--dry-run"},
		{mp, "--plan", pp, "--engine", "chain", "--np", "3", "--lanes", "1", "--dry-run"},
	} {
		out, _, err := run(t, func() error {
			return cmdDeployStagePipe(args)
		})
		if err != nil {
			t.Fatalf("dry-run failed for args %v: %v", args, err)
		}
		if strings.Contains(out, "--lanes") {
			t.Errorf("dry run output for %v should not contain --lanes, got:\n%s", args, out)
		}
	}

	// 3. Validation errors
	badLanesCases := []struct {
		name string
		args []string
		want string
	}{
		{"negative-lanes", []string{"--lanes", "-1"}, "--lanes must be >= 0"},
		{"lanes-without-np", []string{"--lanes", "2"}, "--lanes needs --np >= 2"},
		{"lanes-with-np-1", []string{"--lanes", "2", "--np", "1"}, "--lanes needs --np >= 2"},
		{"lanes-exceeds-8", []string{"--lanes", "9", "--np", "10"}, "maximum is 8"},
		{"lanes-with-spec", []string{"--lanes", "2", "--np", "2", "--spec", "draft-dflash", "--draft-model", mp}, "speculative decoding"},
	}
	for _, tc := range badLanesCases {
		t.Run(tc.name, func(t *testing.T) {
			cmdArgs := append([]string{mp, "--plan", pp, "--engine", "chain", "--dry-run"}, tc.args...)
			err := cmdDeployStagePipe(cmdArgs)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got: %v", tc.want, err)
			}
		})
	}
}

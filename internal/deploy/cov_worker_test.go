package deploy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCovWorkerUtils(t *testing.T) {
	tmp := t.TempDir()

	// 1. isExecFile
	missing := filepath.Join(tmp, "missing")
	if isExecFile(missing) {
		t.Errorf("isExecFile(missing) = true, want false")
	}

	dir := filepath.Join(tmp, "subdir")
	_ = os.Mkdir(dir, 0755)
	if isExecFile(dir) {
		t.Errorf("isExecFile(dir) = true, want false")
	}

	nonExec := filepath.Join(tmp, "file.txt")
	_ = os.WriteFile(nonExec, []byte("hello"), 0644)
	if isExecFile(nonExec) {
		t.Errorf("isExecFile(0644) = true, want false")
	}

	execFile := filepath.Join(tmp, "bin.sh")
	_ = os.WriteFile(execFile, []byte("#!/bin/sh\n"), 0755)
	if !isExecFile(execFile) {
		t.Errorf("isExecFile(0755) = false, want true")
	}

	// 2. tailLines
	text := "line1\nline2\nline3\nline4\n"
	if got := tailLines(text, 2); got != "line3\nline4" {
		t.Errorf("tailLines(2) = %q, want line3\\nline4", got)
	}
	if got := tailLines(text, 10); got != "line1\nline2\nline3\nline4" {
		t.Errorf("tailLines(10) = %q", got)
	}

	// 3. libPathWith
	if got := libPathWith("/my/lib", ""); got != "/my/lib" {
		t.Errorf("libPathWith empty = %q, want /my/lib", got)
	}
	if got := libPathWith("/my/lib", "/usr/lib"); got != "/my/lib:/usr/lib" {
		t.Errorf("libPathWith non-empty = %q, want /my/lib:/usr/lib", got)
	}

	// 4. defaultIsThisMachine
	host, _ := os.Hostname()
	casesMachine := []struct {
		node string
		want bool
	}{
		{"", true},
		{"localhost", true},
		{"127.0.0.1", true},
		{"::1", true},
		{host, true},
		{"nonexistent-node-xyz", false},
	}
	for _, tc := range casesMachine {
		if got := defaultIsThisMachine(tc.node); got != tc.want {
			t.Errorf("defaultIsThisMachine(%q) = %v, want %v", tc.node, got, tc.want)
		}
	}

	// 5. workerLogPath
	t.Setenv("XDG_CACHE_HOME", tmp)
	logP := workerLogPath(50152)
	if !strings.HasSuffix(logP, "worker_50152.log") {
		t.Errorf("workerLogPath unexpected: %s", logP)
	}

	// 6. filterWorkerEnv
	envIn := []string{
		"VALID_KEY=valid_val",
		"ANOTHER_KEY=val+1=2",
		"=missing_key",
		"bad key=val",
		"KEY=unsafe$val",
		"KEY2=val with space",
		"KEY3=semi;colon",
	}
	filtered := filterWorkerEnv(envIn)
	if len(filtered) != 2 || filtered[0] != "VALID_KEY=valid_val" || filtered[1] != "ANOTHER_KEY=val+1=2" {
		t.Errorf("filterWorkerEnv unexpected result: %v", filtered)
	}

	// 7. workerLaunch methods
	wl := workerLaunch{
		Bin:     "/path/to/worker",
		Pre:     []string{"pre1"},
		LibDir:  "/lib/dir",
		Port:    50152,
		Env:     []string{"K=V"},
		Devices: "CUDA0",
	}
	args := wl.Args()
	if len(args) == 0 || args[0] != "pre1" {
		t.Errorf("workerLaunch.Args unexpected: %v", args)
	}
	envAdd := wl.EnvAdd("")
	if len(envAdd) < 2 || !strings.HasPrefix(envAdd[0], "LD_LIBRARY_PATH=") {
		t.Errorf("workerLaunch.EnvAdd unexpected: %v", envAdd)
	}
	line := wl.ShellLine()
	if !strings.Contains(line, "/path/to/worker") || !strings.Contains(line, "LD_LIBRARY_PATH=") {
		t.Errorf("workerLaunch.ShellLine unexpected: %s", line)
	}

	// 8. workerCommandLine
	oldThis := isThisMachine
	oldFind := findLocalWorker
	t.Cleanup(func() {
		isThisMachine = oldThis
		findLocalWorker = oldFind
	})

	isThisMachine = func(node string) bool { return true }
	findLocalWorker = func() (string, []string, string, error) {
		return "/usr/bin/ring-worker", nil, "/usr/lib", nil
	}
	cmdLocal := workerCommandLine("localhost", 50152, []string{"K=V"}, "CUDA0")
	if !strings.Contains(cmdLocal, "ring-worker") {
		t.Errorf("workerCommandLine local unexpected: %s", cmdLocal)
	}

	findLocalWorker = func() (string, []string, string, error) {
		return "", nil, "", errors.New("not found")
	}
	cmdErr := workerCommandLine("localhost", 50152, []string{"K=V"}, "CUDA0")
	if !strings.HasPrefix(cmdErr, "# ") {
		t.Errorf("workerCommandLine local error expected '# ...', got: %s", cmdErr)
	}

	isThisMachine = func(node string) bool { return false }
	cmdRemote := workerCommandLine("remote-box", 50152, []string{"K=V"}, "CUDA0")
	if !strings.Contains(cmdRemote, "on remote-box") {
		t.Errorf("workerCommandLine remote unexpected: %s", cmdRemote)
	}
}

func TestCovFileSizeOnAndLocalWeights(t *testing.T) {
	tmp := t.TempDir()
	existingFile := filepath.Join(tmp, "model.gguf")
	_ = os.WriteFile(existingFile, []byte("dummy-weights-file-data"), 0644)

	// 1. fileSizeOn
	// Local existing
	sz, err := fileSizeOn("127.0.0.1", existingFile)
	if err != nil || sz != int64(len("dummy-weights-file-data")) {
		t.Errorf("fileSizeOn local existing = %d, %v", sz, err)
	}

	// Local missing
	missing := filepath.Join(tmp, "not-here.gguf")
	if _, err := fileSizeOn("127.0.0.1", missing); err == nil {
		t.Errorf("fileSizeOn local missing expected error")
	}

	// Remote with unsafe path characters
	if _, err := fileSizeOn("remote-node", "/unsafe;path/file"); err == nil {
		t.Errorf("fileSizeOn remote unsafe expected error")
	}

	// Remote with safe path but ssh failure
	if _, err := fileSizeOn("nonexistent-node-12345", "/safe/path/file"); err == nil {
		t.Errorf("fileSizeOn remote expected error on failed ssh")
	}

	// 2. localWeightsEnv
	oldFileSize := fileSizeOn
	t.Cleanup(func() { fileSizeOn = oldFileSize })

	// Master size error -> returns warning
	fileSizeOn = func(node, path string) (int64, error) {
		return 0, errors.New("cannot stat")
	}
	warns := localWeightsEnv(DeployOptions{ModelPath: "/path/model"}, "master", []string{"w1"}, []string{"w1"}, "0", "0", 32, 0, nil)
	if len(warns) == 0 || !strings.Contains(warns[0], "--local-weights: cannot read the size") {
		t.Errorf("localWeightsEnv expected size error warning, got: %v", warns)
	}

	// Master size > 0, worker path has unsafe characters
	fileSizeOn = func(node, path string) (int64, error) {
		return 1000, nil
	}
	optsUnsafe := DeployOptions{
		ModelPath: "/path/model",
		WorkerModels: map[string]string{
			"w1": "/unsafe;path",
		},
	}
	warns2 := localWeightsEnv(optsUnsafe, "master", []string{"w1"}, []string{"w1"}, "0", "0", 32, 0, nil)
	if len(warns2) == 0 || !strings.Contains(warns2[0], "has characters outside") {
		t.Errorf("localWeightsEnv expected unsafe characters warning, got: %v", warns2)
	}
}

func TestCovHubStreamLaunchAndStop(t *testing.T) {
	ctx := context.Background()
	m := NewRemoteWorkerManager()

	oldSSH := sshScript
	oldLocal := localScript
	t.Cleanup(func() {
		sshScript = oldSSH
		localScript = oldLocal
	})

	sl := streamLaunch{
		Node:       "worker-node",
		Port:       50152,
		ListenPort: 19001,
		Hub:        "https://hub.example.com",
	}

	// Test StartStream remote branches:
	// Case 1: ssh error
	sshScript = func(_ context.Context, _ string, _ string) ([]byte, error) {
		return []byte("connection refused"), errors.New("ssh failed")
	}
	if _, err := m.StartStream(ctx, "master-remote", false, sl, "run1", ""); err == nil {
		t.Errorf("StartStream expected error on ssh failure")
	}

	// Case 2: busy error (FLEET_BUSY)
	sshScript = func(_ context.Context, _ string, _ string) ([]byte, error) {
		return []byte("FLEET_BUSY someone-else"), nil
	}
	if _, err := m.StartStream(ctx, "master-remote", false, sl, "run1", ""); err == nil {
		t.Errorf("StartStream expected busy error")
	}

	// Case 3: FLEET_NOBIN
	sshScript = func(_ context.Context, _ string, _ string) ([]byte, error) {
		return []byte("FLEET_NOBIN"), nil
	}
	if _, err := m.StartStream(ctx, "master-remote", false, sl, "run1", ""); err == nil {
		t.Errorf("StartStream expected FLEET_NOBIN error")
	}

	// Case 4: FLEET_READY
	sshScript = func(_ context.Context, _ string, _ string) ([]byte, error) {
		return []byte("FLEET_PID 4567\nFLEET_READY"), nil
	}
	pid, err := m.StartStream(ctx, "master-remote", false, sl, "run1", "")
	if err != nil || pid != 4567 {
		t.Errorf("StartStream FLEET_READY expected 4567, got pid=%d, err=%v", pid, err)
	}

	// Case 5: FLEET_DIED
	sshScript = func(_ context.Context, _ string, _ string) ([]byte, error) {
		return []byte("FLEET_PID 4567\nFLEET_DIED\nerror tail line"), nil
	}
	if _, err := m.StartStream(ctx, "master-remote", false, sl, "run1", ""); err == nil {
		t.Errorf("StartStream expected FLEET_DIED error")
	}

	// Case 6: FLEET_TIMEOUT
	sshScript = func(_ context.Context, _ string, _ string) ([]byte, error) {
		return []byte("FLEET_PID 4567\nFLEET_TIMEOUT\ntimeout tail line"), nil
	}
	if _, err := m.StartStream(ctx, "master-remote", false, sl, "run1", ""); err == nil {
		t.Errorf("StartStream expected FLEET_TIMEOUT error")
	}

	// Case 7: Unknown status
	sshScript = func(_ context.Context, _ string, _ string) ([]byte, error) {
		return []byte("FLEET_PID 4567\nFLEET_UNKNOWN"), nil
	}
	if _, err := m.StartStream(ctx, "master-remote", false, sl, "run1", ""); err == nil {
		t.Errorf("StartStream expected unknown answer error")
	}

	// Test StopStream
	sshScript = func(_ context.Context, _ string, _ string) ([]byte, error) {
		return []byte("ok"), nil
	}
	if err := m.StopStream(ctx, "master-remote", 19001, false); err != nil {
		t.Errorf("StopStream remote failed: %v", err)
	}

	localScript = func(_ context.Context, _ string) ([]byte, error) {
		return []byte("ok"), nil
	}
	if err := m.StopStream(ctx, "localhost", 19001, true); err != nil {
		t.Errorf("StopStream local failed: %v", err)
	}
}

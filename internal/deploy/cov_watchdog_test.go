package deploy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
)

func TestCovWatchdogParseFailedWorkers(t *testing.T) {
	// 1. Invalid JSON
	if _, err := parseFailedWorkers([]byte("invalid json")); err == nil {
		t.Errorf("parseFailedWorkers expected error on invalid json")
	}

	// 2. failed_workers populated
	body1 := []byte(`{"failed_workers": ["worker-1", "worker-2"]}`)
	got1, err := parseFailedWorkers(body1)
	if err != nil || len(got1) != 2 || got1[0] != "worker-1" || got1[1] != "worker-2" {
		t.Errorf("parseFailedWorkers failed_workers unexpected: %v, %v", got1, err)
	}

	// 3. failed_workers empty, failure_report has culprits and victims
	body2 := []byte(`{
		"failure_report": [
			{"worker": "culprit-node", "role": "culprit"},
			{"worker": "victim-node", "role": "victim"}
		]
	}`)
	got2, err := parseFailedWorkers(body2)
	if err != nil || len(got2) != 1 || got2[0] != "culprit-node" {
		t.Errorf("parseFailedWorkers failure_report unexpected: %v, %v", got2, err)
	}

	// 4. failure_report has only victims (inconsistent report)
	body3 := []byte(`{
		"failure_report": [
			{"worker": "victim-only", "role": "victim"}
		]
	}`)
	got3, err := parseFailedWorkers(body3)
	if err != nil || len(got3) != 1 || got3[0] != "(inconsistent report (no culprit named); victims: victim-only)" {
		t.Errorf("parseFailedWorkers inconsistent report unexpected: %v, %v", got3, err)
	}

	// 5. empty report and empty failed_workers
	body4 := []byte(`{"failed_workers": []}`)
	got4, err := parseFailedWorkers(body4)
	if err != nil || len(got4) != 0 {
		t.Errorf("parseFailedWorkers empty unexpected: %v, %v", got4, err)
	}
}

func TestCovWatchdogRingFailedAndFetch(t *testing.T) {
	ctx := context.Background()
	w := NewWatchdog(&config.Config{})

	// Case 1: nil Group
	epNoGroup := config.Endpoint{Name: "ep1"}
	if _, err := w.ringFailed(ctx, epNoGroup); err == nil {
		t.Errorf("ringFailed expected error when ep.Group == nil")
	}

	// Case 2: StatsPort <= 0
	epZeroPort := config.Endpoint{Name: "ep2", Group: &config.Group{StatsPort: 0}}
	if _, err := w.ringFailed(ctx, epZeroPort); err == nil {
		t.Errorf("ringFailed expected error when StatsPort <= 0")
	}

	// Case 3: local fetchRingInfo success via httptest server
	ts := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/ring/info" {
			rw.Header().Set("Content-Type", "application/json")
			_, _ = rw.Write([]byte(`{"failed_workers": ["worker-xyz"]}`))
			return
		}
		http.NotFound(rw, req)
	}))
	defer ts.Close()

	u, _ := url.Parse(ts.URL)
	port, _ := strconv.Atoi(u.Port())

	bytes, err := fetchRingInfo(ctx, "localhost", true, port)
	if err != nil || len(bytes) == 0 {
		t.Fatalf("fetchRingInfo local failed: %v", err)
	}

	epValid := config.Endpoint{
		Name:  "ep3",
		Node:  "localhost",
		Group: &config.Group{StatsPort: port},
	}
	failed, err := w.ringFailed(ctx, epValid)
	if err != nil || len(failed) != 1 || failed[0] != "worker-xyz" {
		t.Errorf("ringFailed unexpected result: %v, %v", failed, err)
	}
}

func TestCovWatchdogLastRunOptionsAndRedeploy(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", tmp)
	runsDir := filepath.Join(tmp, "fleet", "runs")
	_ = os.MkdirAll(runsDir, 0700)

	// 1. Empty directory
	if _, err := LastRunOptions("my-ep"); err == nil {
		t.Errorf("LastRunOptions expected error on empty dir")
	}

	// 2. Corrupt file
	_ = os.WriteFile(filepath.Join(runsDir, "run-100.json"), []byte("bad-json"), 0600)
	// 3. Different endpoint
	diffEp := DeployResult{RunID: "run-200", EndpointName: "other-ep", Options: &DeployOptions{}}
	bDiff, _ := json.Marshal(diffEp)
	_ = os.WriteFile(filepath.Join(runsDir, "run-200.json"), bDiff, 0600)
	// 4. Nil options
	nilOpts := DeployResult{RunID: "run-300", EndpointName: "my-ep", Options: nil}
	bNil, _ := json.Marshal(nilOpts)
	_ = os.WriteFile(filepath.Join(runsDir, "run-300.json"), bNil, 0600)
	// 5. StatusFailed
	failedStatus := DeployResult{RunID: "run-400", EndpointName: "my-ep", Status: StatusFailed, Options: &DeployOptions{}}
	bFailed, _ := json.Marshal(failedStatus)
	_ = os.WriteFile(filepath.Join(runsDir, "run-400.json"), bFailed, 0600)

	if _, err := LastRunOptions("my-ep"); err == nil {
		t.Errorf("LastRunOptions expected error when no valid run exists")
	}

	// 6. Valid running result
	validRes := DeployResult{
		RunID:        "run-500",
		EndpointName: "my-ep",
		Status:       StatusRunning,
		Options:      &DeployOptions{ModelPath: "/models/test.gguf", DryRun: true},
	}
	bValid, _ := json.Marshal(validRes)
	_ = os.WriteFile(filepath.Join(runsDir, "run-500.json"), bValid, 0600)

	opts, err := LastRunOptions("my-ep")
	if err != nil || opts == nil {
		t.Fatalf("LastRunOptions failed: %v", err)
	}
	if opts.DryRun {
		t.Errorf("LastRunOptions expected DryRun to be reset to false")
	}
	if opts.ModelPath != "/models/test.gguf" {
		t.Errorf("LastRunOptions ModelPath = %q, want /models/test.gguf", opts.ModelPath)
	}

	// redeployFromLastRun
	w := NewWatchdog(&config.Config{})
	ep := config.Endpoint{Name: "my-ep"}
	// ExecuteDeployment will fail because model doesn't exist, but it executes redeployFromLastRun
	_ = w.redeployFromLastRun(context.Background(), ep)
}

func TestCovWatchdogRunAndCheckOnce(t *testing.T) {
	tmp := t.TempDir()
	logFile := filepath.Join(tmp, "watchdog.log")

	cfg := &config.Config{
		Endpoints: []config.Endpoint{
			{Name: "ep-skip", Group: nil},
			{Name: "ep-not-ring", Group: &config.Group{Engine: "vllm"}},
			{Name: "ep-ring-ssh", Group: &config.Group{Engine: "llama-ring"}, SSH: "user@host", Remote: "127.0.0.1:8080"},
		},
	}

	w := NewWatchdog(cfg)
	w.logFile = logFile

	// Check isLocalEndpoint
	host, _ := os.Hostname()
	if !isLocalEndpoint(config.Endpoint{Node: ""}) {
		t.Errorf("isLocalEndpoint empty node want true")
	}
	if !isLocalEndpoint(config.Endpoint{Node: "localhost"}) {
		t.Errorf("isLocalEndpoint localhost want true")
	}
	if !isLocalEndpoint(config.Endpoint{Node: "127.0.0.1"}) {
		t.Errorf("isLocalEndpoint 127.0.0.1 want true")
	}
	if !isLocalEndpoint(config.Endpoint{Node: host}) {
		t.Errorf("isLocalEndpoint host want true")
	}
	if !isLocalEndpoint(config.Endpoint{Node: "remote", URL: "http://127.0.0.1:9090/v1"}) {
		t.Errorf("isLocalEndpoint 127.0.0.1 URL want true")
	}
	if isLocalEndpoint(config.Endpoint{Node: "remote-box", URL: "http://10.0.1.100:9090/v1"}) {
		t.Errorf("isLocalEndpoint remote box want false")
	}

	// Run with context timeout
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	w.Run(ctx, 10*time.Millisecond)

	// CheckOnce
	w.CheckOnce(context.Background())

	// Verify logFile was written
	if b, err := os.ReadFile(logFile); err != nil || len(b) == 0 {
		t.Errorf("watchdog logFile not written: %v", err)
	}
}

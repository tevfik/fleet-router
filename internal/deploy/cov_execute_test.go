package deploy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tevfik/fleet-router/internal/config"
)

func TestCovEndpointHealthyAndOtherRing(t *testing.T) {
	// 1. endpointHealthy tests
	// Empty URL
	if endpointHealthy(config.Endpoint{URL: ""}) {
		t.Errorf("endpointHealthy empty URL want false")
	}

	// Offline URL
	if endpointHealthy(config.Endpoint{URL: "http://127.0.0.1:59997/v1"}) {
		t.Errorf("endpointHealthy offline URL want false")
	}

	// Online URL returning 200 OK
	ts200 := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/health" {
			rw.WriteHeader(http.StatusOK)
			return
		}
		rw.WriteHeader(http.StatusNotFound)
	}))
	defer ts200.Close()

	if !endpointHealthy(config.Endpoint{URL: ts200.URL + "/v1"}) {
		t.Errorf("endpointHealthy 200 want true")
	}

	// Online URL returning 500
	ts500 := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts500.Close()

	if endpointHealthy(config.Endpoint{URL: ts500.URL + "/v1"}) {
		t.Errorf("endpointHealthy 500 want false")
	}

	// 2. sameModel tests
	if !sameModel("model.gguf", "/path/to/model.gguf") {
		t.Errorf("sameModel basename match want true")
	}
	if !sameModel("/path/to/model.gguf", "/path/to/model.gguf") {
		t.Errorf("sameModel full match want true")
	}
	if !sameModel("other-prefix-model.gguf", "model.gguf") {
		t.Errorf("sameModel suffix match want true")
	}
	if sameModel("completely-different.gguf", "model.gguf") {
		t.Errorf("sameModel mismatch want false")
	}

	// 3. otherRingOnPort tests
	if otherRingOnPort(nil, &DeployResult{}, "/path/model", 8080) != "" {
		t.Errorf("otherRingOnPort nil config want empty")
	}

	cfg := &config.Config{
		Endpoints: []config.Endpoint{
			// skipped: nil group
			{Name: "ep-nil-group"},
			// skipped: wrong engine
			{Name: "ep-wrong-engine", Group: &config.Group{Engine: "vllm"}},
			// skipped: same endpoint name
			{Name: "ep-current", Group: &config.Group{Engine: "llama-ring"}},
			// skipped: same model
			{Name: "ep-same-model", Group: &config.Group{Engine: "llama-ring"}, Model: "model.gguf"},
			// skipped: different node
			{Name: "ep-diff-node", Group: &config.Group{Engine: "llama-ring"}, Node: "other-node"},
			// matching candidate by URL (healthy)
			{
				Name:  "ep-match-url",
				Group: &config.Group{Engine: "llama-ring"},
				Node:  "localhost",
				URL:   ts200.URL + "/v1",
				Model: "alpha.gguf",
			},
			// matching candidate by Remote (unhealthy)
			{
				Name:   "ep-match-remote-unhealthy",
				Group:  &config.Group{Engine: "llama-ring"},
				Node:   "localhost",
				Remote: "127.0.0.1:59997",
				Model:  "alpha.gguf",
			},
		},
	}

	res := &DeployResult{
		EndpointName: "ep-current",
		MasterNode:   "localhost",
	}

	// Extract port of ts200
	tsPort := urlPort(ts200.URL)
	if got := otherRingOnPort(cfg, res, "model.gguf", tsPort); got != "ep-match-url" {
		t.Errorf("otherRingOnPort expected ep-match-url, got %q", got)
	}

	// Test port with no healthy match
	if got := otherRingOnPort(cfg, res, "model.gguf", 59997); got != "" {
		t.Errorf("otherRingOnPort expected empty for unhealthy remote, got %q", got)
	}
}

func TestCovRemoteDeadAndRedacted(t *testing.T) {
	ctx := context.Background()

	// 1. remoteDead
	oldSSH := sshScript
	t.Cleanup(func() { sshScript = oldSSH })

	sshScript = func(_ context.Context, _ string, _ string) ([]byte, error) {
		return []byte("FLEET_GONE\n"), nil
	}
	if !remoteDead(ctx, "remote-node", 1234) {
		t.Errorf("remoteDead FLEET_GONE want true")
	}

	sshScript = func(_ context.Context, _ string, _ string) ([]byte, error) {
		return []byte("FLEET_ALIVE\n"), nil
	}
	if remoteDead(ctx, "remote-node", 1234) {
		t.Errorf("remoteDead FLEET_ALIVE want false")
	}

	sshScript = func(_ context.Context, _ string, _ string) ([]byte, error) {
		return nil, errors.New("ssh failed")
	}
	if remoteDead(ctx, "remote-node", 1234) {
		t.Errorf("remoteDead ssh error want false")
	}

	// 2. DeployResult.Redacted
	var nilRes *DeployResult
	if nilRes.Redacted() != nil {
		t.Errorf("nil DeployResult.Redacted() want nil")
	}

	noOptsRes := &DeployResult{}
	if noOptsRes.Redacted().Options != nil {
		t.Errorf("no opts DeployResult.Redacted() want nil options")
	}

	res := &DeployResult{
		Options: &DeployOptions{
			MasterEnvExtra: map[string]string{
				"SECRET_TOKEN": "my-secret-value",
			},
			WorkerEnvExtra: map[string]map[string]string{
				"worker-1": {
					"WORKER_TOKEN": "worker-secret",
				},
			},
		},
	}
	redacted := res.Redacted()
	if redacted.Options.MasterEnvExtra["SECRET_TOKEN"] != "***" {
		t.Errorf("Redacted MasterEnvExtra not masked: %v", redacted.Options.MasterEnvExtra)
	}
	if redacted.Options.WorkerEnvExtra["worker-1"]["WORKER_TOKEN"] != "***" {
		t.Errorf("Redacted WorkerEnvExtra not masked: %v", redacted.Options.WorkerEnvExtra)
	}
	// Original should not be modified
	if res.Options.MasterEnvExtra["SECRET_TOKEN"] != "my-secret-value" {
		t.Errorf("Original res mutated by Redacted()")
	}
}

func TestCovProbeMemory(t *testing.T) {
	ctx := context.Background()

	oldLocal := localScript
	oldSSH := sshScript
	t.Cleanup(func() {
		localScript = oldLocal
		sshScript = oldSSH
	})

	// Case 1: local script success
	localScript = func(_ context.Context, _ string) ([]byte, error) {
		return []byte("FLEET_MEM_RAM_KB 16777216\nFLEET_MEM_VRAM_MIB 8192\n"), nil
	}
	mem, err := probeMemory(ctx, "localhost", true)
	if err != nil || mem.RAMGiB != 16 || mem.VRAMGiB != 8 {
		t.Errorf("probeMemory local unexpected: %+v, %v", mem, err)
	}

	// Case 2: script error
	localScript = func(_ context.Context, _ string) ([]byte, error) {
		return nil, errors.New("script failed")
	}
	if _, err := probeMemory(ctx, "localhost", true); err == nil {
		t.Errorf("probeMemory expected error on script fail")
	}

	// Case 3: remote script missing RAM info
	sshScript = func(_ context.Context, _ string, _ string) ([]byte, error) {
		return []byte("SOMETHING_ELSE 123\n"), nil
	}
	if _, err := probeMemory(ctx, "remote-node", false); err == nil {
		t.Errorf("probeMemory expected error when no MemAvailable")
	}
}

func TestCovRunOnNodeDryRunAndValidation(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{}

	// Validation: unsafe model path
	if _, err := RunOnNode(ctx, cfg, "localhost", "/unsafe;path/model.gguf", 0, 0, 0, false); err == nil {
		t.Errorf("RunOnNode expected error on unsafe path characters")
	}

	// Dry-run local
	resLocal, err := RunOnNode(ctx, cfg, "localhost", "/models/test.gguf", 0, 0, 0, true)
	if err != nil || resLocal.Status != "dry-run" {
		t.Fatalf("RunOnNode local dry-run failed: %+v, %v", resLocal, err)
	}
	if resLocal.URL != "http://127.0.0.1:18092/v1" {
		t.Errorf("RunOnNode local dry-run default port URL = %q", resLocal.URL)
	}

	// Dry-run remote
	resRemote, err := RunOnNode(ctx, cfg, "remote-node", "/models/test.gguf", 18095, 2048, 40, true)
	if err != nil || resRemote.Status != "dry-run" {
		t.Fatalf("RunOnNode remote dry-run failed: %+v, %v", resRemote, err)
	}
}

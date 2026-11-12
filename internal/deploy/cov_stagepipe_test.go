package deploy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tevfik/fleet-router/internal/ggufhead"
	"github.com/tevfik/fleet-router/internal/plan"
)

func TestCovLiveStageRunnerExists(t *testing.T) {
	r := &LiveStageRunner{}
	tmp := t.TempDir()

	// Local file exists
	existing := filepath.Join(tmp, "exists.txt")
	_ = os.WriteFile(existing, []byte("ok"), 0644)
	ok, err := r.Exists("localhost", existing)
	if err != nil || !ok {
		t.Errorf("Exists(localhost, existing) = %v, %v; want true, nil", ok, err)
	}

	// Local file missing
	ok, err = r.Exists("localhost", filepath.Join(tmp, "missing.txt"))
	if err != nil || ok {
		t.Errorf("Exists(localhost, missing) = %v, %v; want false, nil", ok, err)
	}

	// Remote file checks using sshScript seam
	oldSSH := sshScript
	t.Cleanup(func() { sshScript = oldSSH })

	// Remote file exists
	sshScript = func(_ context.Context, _ string, _ string) ([]byte, error) {
		return []byte("FLEET_EXISTS\n"), nil
	}
	ok, err = r.Exists("remote-node", "/remote/path")
	if err != nil || !ok {
		t.Errorf("Exists(remote-node, exists) = %v, %v; want true, nil", ok, err)
	}

	// Remote file missing
	sshScript = func(_ context.Context, _ string, _ string) ([]byte, error) {
		return []byte(""), nil
	}
	ok, err = r.Exists("remote-node", "/remote/missing")
	if err != nil || ok {
		t.Errorf("Exists(remote-node, missing) = %v, %v; want false, nil", ok, err)
	}

	// Remote ssh error
	sshScript = func(_ context.Context, _ string, _ string) ([]byte, error) {
		return nil, errors.New("ssh connection failed")
	}
	ok, err = r.Exists("remote-node", "/remote/error")
	if err == nil || ok {
		t.Errorf("Exists(remote-node, error) expected error, got %v, %v", ok, err)
	}
}

func TestCovLiveStageRunnerDirectlyReachable(t *testing.T) {
	r := &LiveStageRunner{}

	// Case 1: local node is always directly reachable
	sLocal := StageLaunch{Node: "localhost"}
	if !r.DirectlyReachable(sLocal) {
		t.Errorf("DirectlyReachable(localhost) = false, want true")
	}

	// Case 2: remote node with http ready probe
	// 2a. HTTP 200 OK
	tsOK := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/health" {
			rw.WriteHeader(http.StatusOK)
			return
		}
		rw.WriteHeader(http.StatusNotFound)
	}))
	defer tsOK.Close()

	uOK, _ := url.Parse(tsOK.URL)
	sRemoteHTTP := StageLaunch{
		Node: "remote-node-1",
		Ready: Readiness{
			Kind: "http",
			Addr: uOK.Host,
			Path: "/health",
		},
	}
	if !r.DirectlyReachable(sRemoteHTTP) {
		t.Errorf("DirectlyReachable(remote http 200) = false, want true")
	}

	// 2b. HTTP 500 error
	tsErr := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.WriteHeader(http.StatusInternalServerError)
	}))
	defer tsErr.Close()

	uErr, _ := url.Parse(tsErr.URL)
	sRemoteHTTPErr := StageLaunch{
		Node: "remote-node-2",
		Ready: Readiness{
			Kind: "http",
			Addr: uErr.Host,
			Path: "/health",
		},
	}
	if r.DirectlyReachable(sRemoteHTTPErr) {
		t.Errorf("DirectlyReachable(remote http 500) = true, want false")
	}

	// 2c. Dial fails
	sRemoteHTTPDead := StageLaunch{
		Node: "remote-node-3",
		Ready: Readiness{
			Kind: "http",
			Addr: "127.0.0.1:59998",
			Path: "/health",
		},
	}
	if r.DirectlyReachable(sRemoteHTTPDead) {
		t.Errorf("DirectlyReachable(dead http) = true, want false")
	}

	// Case 3: remote node with tcp ready probe
	// 3a. TCP open
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen failed: %v", err)
	}
	defer l.Close()

	sRemoteTCP := StageLaunch{
		Node: "remote-node-4",
		Ready: Readiness{
			Kind: "tcp",
			Addr: l.Addr().String(),
		},
	}
	if !r.DirectlyReachable(sRemoteTCP) {
		t.Errorf("DirectlyReachable(remote tcp open) = false, want true")
	}

	// 3b. TCP closed
	l.Close()
	if r.DirectlyReachable(sRemoteTCP) {
		t.Errorf("DirectlyReachable(remote tcp closed) = true, want false")
	}
}

func TestCovDFlashCacheDirAndEnsureCachedHead(t *testing.T) {
	tmp := t.TempDir()

	// 1. getDFlashCacheDir
	oldCacheDir := dflashCacheDir
	t.Cleanup(func() { dflashCacheDir = oldCacheDir })

	dflashCacheDir = "/custom/dflash/cache"
	got, err := getDFlashCacheDir()
	if err != nil || got != "/custom/dflash/cache" {
		t.Errorf("getDFlashCacheDir() = %q, %v; want /custom/dflash/cache", got, err)
	}

	dflashCacheDir = ""
	got, err = getDFlashCacheDir()
	if err != nil || !strings.Contains(got, "dflash") {
		t.Errorf("getDFlashCacheDir() default = %q, %v", got, err)
	}

	// 2. ensureCachedHead validation
	// Missing draft
	if _, err := ensureCachedHead("/missing/draft", "/missing/target", tmp); err == nil {
		t.Errorf("ensureCachedHead expected error on missing draft")
	}

	// Existing draft, missing target
	draftFile := filepath.Join(tmp, "draft.gguf")
	_ = os.WriteFile(draftFile, []byte("draft"), 0644)
	if _, err := ensureCachedHead(draftFile, "/missing/target", tmp); err == nil {
		t.Errorf("ensureCachedHead expected error on missing target")
	}

	// Both draft and target exist; and cache already contains the output file
	targetFile := filepath.Join(tmp, "target.gguf")
	_ = os.WriteFile(targetFile, []byte("target"), 0644)

	oldEmbed := embedHeadFn
	t.Cleanup(func() { embedHeadFn = oldEmbed })
	embedHeadFn = func(draft, target, out string) (ggufhead.Result, error) {
		_ = os.WriteFile(out, []byte("cached-head-content"), 0644)
		return ggufhead.Result{}, nil
	}

	cacheResult, err := ensureCachedHead(draftFile, targetFile, tmp)
	if err != nil || cacheResult == "" {
		t.Fatalf("ensureCachedHead failed: %v", err)
	}

	// Call again: should return cacheResult directly without re-embedding
	embedCalled := false
	embedHeadFn = func(draft, target, out string) (ggufhead.Result, error) {
		embedCalled = true
		return ggufhead.Result{}, nil
	}
	cacheResult2, err := ensureCachedHead(draftFile, targetFile, tmp)
	if err != nil || cacheResult2 != cacheResult || embedCalled {
		t.Errorf("ensureCachedHead second call did not use cache: %v, %v, called=%v", cacheResult2, err, embedCalled)
	}
}

func TestCovDeployChainValidation(t *testing.T) {
	ctx := context.Background()
	cand := plan.Candidate{
		Parts: []plan.Part{
			{Node: "node-a"},
			{Node: "node-b"},
			{Node: "node-a"}, // duplicate to test chainNodes
		},
	}

	// chainNodes check
	nodes := chainNodes(cand)
	if len(nodes) != 2 || nodes[0] != "node-a" || nodes[1] != "node-b" {
		t.Errorf("chainNodes unexpected: %v", nodes)
	}

	// Error 1: Relay && ViaHub
	opts := StagePipeOptions{
		Relay:  true,
		ViaHub: true,
	}
	if _, err := DeployChain(ctx, cand, "/path/model", opts, nil, nil); err == nil || !strings.Contains(err.Error(), "do the same job") {
		t.Errorf("DeployChain expected relay/viahub conflict error, got: %v", err)
	}

	// Error 2: modelPath empty
	opts2 := StagePipeOptions{}
	if _, err := DeployChain(ctx, cand, "", opts2, nil, nil); err == nil || !strings.Contains(err.Error(), "model path is required") {
		t.Errorf("DeployChain expected error when modelPath empty, got: %v", err)
	}

	// Error 3: model load error
	if _, err := DeployChain(ctx, cand, "/missing/nonexistent/model.gguf", opts2, nil, nil); err == nil || !strings.Contains(err.Error(), "load model") {
		t.Errorf("DeployChain expected error when LoadModel fails, got: %v", err)
	}

	// Error 4: ViaHub with unsafe HubURL
	tmp := t.TempDir()
	dummyModel := filepath.Join(tmp, "model.json")
	descriptorJSON := `{
		"version": 1,
		"name": "test-desc",
		"architecture": "test-arch",
		"runtime_kind": "stage-pipe",
		"layer_count": 1,
		"hidden_size": 1024,
		"vocab_size": 32000,
		"embedding_at": "first",
		"head_at": "last",
		"layers": [
			{"index": 0, "file": "layer.elf", "bytes": 1048576, "resident_bytes": 1048576, "state": "none"}
		]
	}`
	_ = os.WriteFile(dummyModel, []byte(descriptorJSON), 0644)
	opts4 := StagePipeOptions{
		ViaHub: true,
		HubURL: "https://hub.example.com;rm -rf",
	}
	if _, err := DeployChain(ctx, cand, dummyModel, opts4, nil, nil); err == nil || !strings.Contains(err.Error(), "characters fleet does not put") {
		t.Errorf("DeployChain expected error on unsafe HubURL, got: %v", err)
	}
}

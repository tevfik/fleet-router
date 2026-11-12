package discover

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/tevfik/fleet-router/internal/config"
)

func TestCovURLHostPortAndContains(t *testing.T) {
	if got := urlHostPort("http://localhost:8080/v1"); got != "localhost:8080" {
		t.Fatalf("urlHostPort got %q, want localhost:8080", got)
	}
	if got := urlHostPort("https://example.com/api"); got != "example.com" {
		t.Fatalf("urlHostPort got %q, want example.com", got)
	}
	// Invalid URL
	if got := urlHostPort("http://[invalid-ipv6"); got != "" {
		t.Fatalf("urlHostPort on invalid url got %q, want empty", got)
	}

	// contains
	list := []string{"apple", "banana", "cherry"}
	if !contains(list, "apple") {
		t.Error("contains should return true for apple")
	}
	if !contains(list, "banana") {
		t.Error("contains should return true for banana")
	}
	if contains(list, "date") {
		t.Error("contains should return false for date")
	}
	if contains(nil, "anything") {
		t.Error("contains should return false for nil slice")
	}
}

func TestCovFormatPorts(t *testing.T) {
	// Discrete and ranges
	ports := []int{8080, 30000, 30001, 30002, 8080, 9000, 9001}
	got := FormatPorts(ports)
	want := "8080,9000,9001,30000-30002"
	if got != want {
		t.Fatalf("FormatPorts() got %q, want %q", got, want)
	}
}

func TestCovLocalScanWithHTTPTest(t *testing.T) {
	// Server 1: Fleet server (/fleet/hello)
	mux1 := http.NewServeMux()
	mux1.HandleFunc("/fleet/hello", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"fleet": true,
			"id":    "fleet-local-1",
			"name":  "local-fleet-node",
		})
	})
	l1, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l1.Close()
	srv1 := &http.Server{Handler: mux1}
	go func() { _ = srv1.Serve(l1) }()
	defer srv1.Close()
	port1 := l1.Addr().(*net.TCPAddr).Port

	// Server 2: Model server with /v1/models and Ollama /api/show
	mux2 := http.NewServeMux()
	mux2.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "llama3.2:1b"},
			},
		})
	})
	mux2.HandleFunc("/api/show", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"capabilities": []string{"completion", "vision", "thinking"},
		})
	})
	l2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	srv2 := &http.Server{Handler: mux2}
	go func() { _ = srv2.Serve(l2) }()
	defer srv2.Close()
	port2 := l2.Addr().(*net.TCPAddr).Port

	// Server 3: Model server with fallback /models (without Ollama caps)
	mux3 := http.NewServeMux()
	mux3.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux3.HandleFunc("/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]any{
				{"name": "mistral-embed"},
			},
		})
	})
	mux3.HandleFunc("/api/show", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	l3, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l3.Close()
	srv3 := &http.Server{Handler: mux3}
	go func() { _ = srv3.Serve(l3) }()
	defer srv3.Close()
	port3 := l3.Addr().(*net.TCPAddr).Port

	// Port 4: Closed port
	l4, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port4 := l4.Addr().(*net.TCPAddr).Port
	l4.Close() // closed immediately

	// Port 5: Port to skip via skipLocal
	port5 := 19999

	ctx := context.Background()
	ports := []int{port1, port2, port3, port4, port5}
	skipLocal := map[int]string{port5: "tunnel-owner"}

	// Set ScanFleetPort to test that code path
	ScanFleetPort = port1
	defer func() { ScanFleetPort = 0 }()

	found, notes := Scan(ctx, ports, nil, skipLocal)
	if len(found) < 3 {
		t.Fatalf("expected at least 3 found endpoints, got %d: %+v", len(found), found)
	}

	hasFleet := false
	hasLlama := false
	hasMistral := false
	for _, f := range found {
		if f.Fleet != nil && f.Fleet.ID == "fleet-local-1" {
			hasFleet = true
		}
		for _, m := range f.Models {
			if m == "llama3.2:1b" {
				hasLlama = true
				if len(f.Caps["llama3.2:1b"]) == 0 {
					t.Errorf("expected capabilities for llama3.2:1b, got none")
				}
			}
			if m == "mistral-embed" {
				hasMistral = true
			}
		}
	}
	if !hasFleet {
		t.Error("Scan did not discover fleet peer")
	}
	if !hasLlama {
		t.Error("Scan did not discover llama3.2:1b model")
	}
	if !hasMistral {
		t.Error("Scan did not discover mistral-embed model from fallback /models")
	}

	hasSkipNote := false
	for _, n := range notes {
		if contains([]string{n}, fmt.Sprintf("- %s:%d: tunnel (tunnel-owner) — registered under its owner, skipped", Self(), port5)) {
			hasSkipNote = true
			break
		}
	}
	if !hasSkipNote {
		t.Errorf("expected skip note for port %d, got: %v", port5, notes)
	}
}

func TestCovSSHScanAndCapsWithFakeSSH(t *testing.T) {
	// Create a fake ssh executable in a temp directory
	tmpDir := t.TempDir()
	sshBin := filepath.Join(tmpDir, "ssh")
	script := `#!/bin/sh
for arg in "$@"; do
    if [ "$arg" = "unreachable-host" ]; then
        echo "ssh: connect to host unreachable-host port 22: Connection refused" >&2
        exit 255
    fi
    if [ "$arg" = "fail-scan-host" ]; then
        echo "remote-scanned-host"
        exit 0
    fi
done
for arg in "$@"; do
    if [ "$arg" = "hostname" ]; then
        echo "remote-box"
        exit 0
    fi
done
case "$*" in
    *"bash -s"*)
        printf "8080\t{\"data\":[{\"id\":\"remote-llama\"}]}\t{\"fleet\":false}\n"
        printf "8081\t{}\t{\"fleet\":true,\"id\":\"remote-fid\",\"name\":\"rfleet\"}\n"
        exit 0
        ;;
    *"/api/show"*)
        printf "remote-llama\t\"capabilities\":[\"completion\",\"vision\"]\n"
        exit 0
        ;;
esac
exit 0
`
	if err := os.WriteFile(sshBin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	// Prepend tmpDir to PATH so sshScan calls our fake ssh
	origPath := os.Getenv("PATH")
	t.Setenv("PATH", tmpDir+":"+origPath)

	ctx := context.Background()
	self := Self()

	// 1. Host that is self or hostname
	_, notes := Scan(ctx, nil, []string{self, "   "}, nil)
	if len(notes) == 0 {
		t.Errorf("expected skip note for self host")
	}

	// 2. Unreachable host
	foundUnreach, notesUnreach := Scan(ctx, []int{8080}, []string{"unreachable-host"}, nil)
	if len(foundUnreach) != 0 || len(notesUnreach) == 0 {
		t.Errorf("unreachable host should return error note: found=%v notes=%v", foundUnreach, notesUnreach)
	}

	// 3. Normal remote host
	foundRemote, _ := Scan(ctx, []int{8080, 8081}, []string{"good-remote-host"}, nil)
	if len(foundRemote) != 2 {
		t.Fatalf("expected 2 discovered endpoints on good-remote-host, got %d: %+v", len(foundRemote), foundRemote)
	}
	foundModel := false
	foundPeer := false
	for _, f := range foundRemote {
		if f.Port == 8080 && len(f.Models) == 1 && f.Models[0] == "remote-llama" {
			foundModel = true
			if len(f.Caps["remote-llama"]) == 0 {
				t.Errorf("expected caps for remote-llama, got %+v", f.Caps)
			}
		}
		if f.Port == 8081 && f.Fleet != nil && f.Fleet.ID == "remote-fid" {
			foundPeer = true
		}
	}
	if !foundModel || !foundPeer {
		t.Errorf("expected both model and peer found: model=%v peer=%v", foundModel, foundPeer)
	}

	// 4. Test planPeer branches in MakePlan
	cfg := &config.Config{
		ID: "self-id",
		Peers: []config.Peer{
			{Name: "existing-peer", URL: "http://127.0.0.1:9090"},
		},
	}
	pl := MakePlan(cfg, []Found{
		{Node: "node-dup", Port: 9090, Fleet: &FleetInfo{ID: "dup-id", Name: "dup"}, SSH: ""},
		{Node: "existing-peer", Port: 9090, Fleet: &FleetInfo{ID: "dup-id2", Name: "dup2"}, SSH: ""},
	}, false)
	if len(pl.Peers) == 0 {
		t.Errorf("expected peer to be planned")
	}
}

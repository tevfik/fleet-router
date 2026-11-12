package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/tunnel"
)

func TestTunnelEndpointsAreNotPersistedAndNoGhostsReturn(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := (&config.Config{Name: "hub", Endpoints: []config.Endpoint{{Name: "e", Model: "m", URL: "http://127.0.0.1:1"}}}).Save(p); err != nil {
		t.Fatal(err)
	}
	s, err := New(p)
	if err != nil {
		t.Fatal(err)
	}
	s.onTunnelJoin(tunnel.NodeRegistration{NodeName: "n1", Models: []string{"qwen"}})
	cfg := s.Config() // an edit starts from a copy that has the tunnel endpoint
	if cfg.Endpoint("n1/qwen") == nil {
		t.Fatal("the joined node's endpoint must be visible")
	}
	s.onTunnelDrop("n1") // the node leaves while the edit is going on
	cfg.Name = "renamed"
	if err := s.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if s.Config().Endpoint("n1/qwen") != nil {
		t.Fatal("a node that dropped must not come back as a ghost endpoint")
	}
	s.onTunnelJoin(tunnel.NodeRegistration{NodeName: "n2", Models: []string{"x"}})
	cfg = s.Config()
	cfg.Name = "again"
	if err := s.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if strings.Contains(string(b), "tunnel://") || strings.Contains(string(b), "n2/x") {
		t.Fatalf("tunnel endpoints leaked into the file:\n%s", b)
	}
	if s.Config().Endpoint("n2/x") == nil || s.Config().Endpoint("e") == nil {
		t.Fatal("live tunnel endpoints and configured ones must stay in memory")
	}
}

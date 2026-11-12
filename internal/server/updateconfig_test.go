package server

import (
	"testing"

	"github.com/tevfik/fleet-router/internal/config"
)

// A background save (learned dimension, measured speed) must not erase what the CLI or `fleet deploy` wrote to the
// file after the daemon last loaded it.
func TestUpdateConfig_KeepsChangesMadeOnDisk(t *testing.T) {
	s, path := rtFileServer(t, &config.Config{Endpoints: []config.Endpoint{rtEp("a", "n1", "http://127.0.0.1:1/v1", "m")}})
	disk, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	disk.Endpoints = append(disk.Endpoints, rtEp("deployed", "n1", "http://127.0.0.1:2/v1", "m2"))
	if err := disk.Save(path); err != nil { // the CLI's save; the daemon has not reloaded yet
		t.Fatal(err)
	}
	if err := s.UpdateConfig(func(c *config.Config) bool {
		for i := range c.Endpoints {
			if c.Endpoints[i].Name == "a" {
				c.Endpoints[i].TPS = 42
			}
		}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	got, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Endpoint("deployed") == nil {
		t.Fatal("the endpoint saved by the CLI was erased")
	}
	if e := got.Endpoint("a"); e == nil || e.TPS != 42 {
		t.Fatalf("the update was not saved: %+v", e)
	}
	if s.Config().Endpoint("deployed") == nil {
		t.Fatal("the daemon did not take the file's endpoint into memory")
	}
}

// Reload keeps the endpoints of nodes joined over the tunnel (they are not in the file).
func TestReload_KeepsTunnelEndpoints(t *testing.T) {
	s, _ := rtFileServer(t, &config.Config{Endpoints: []config.Endpoint{rtEp("a", "n1", "http://127.0.0.1:1/v1", "m")}})
	s.mu.Lock()
	s.cfg.Endpoints = append(s.cfg.Endpoints, config.Endpoint{Name: "remote-m", Node: "remote", URL: "tunnel://remote", Model: "m"})
	s.mu.Unlock()
	if err := s.Reload(); err != nil {
		t.Fatal(err)
	}
	if s.Config().Endpoint("remote-m") == nil {
		t.Fatal("reload dropped the tunnel endpoint")
	}
}

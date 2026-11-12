package server

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/tunnel"
)

func TestStreamAuth(t *testing.T) {
	t.Setenv("FLEET_TEST_KEY", "s3cret")
	ks := newKeyStore(filepath.Join(t.TempDir(), "apikeys.json"))
	SetKeyStore(ks)
	open, err := ks.Create("ring-master", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	coder, err := ks.Create("cursor", "default", "coder", "qwen*")
	if err != nil {
		t.Fatal(err)
	}
	streamer, err := ks.Create("ring", "default", "stream")
	if err != nil {
		t.Fatal(err)
	}
	try := func(cfg *config.Config, remote, tok string) bool {
		s := &Server{cfg: cfg, state: map[string]*EpState{}, lims: map[string]*limiter{}}
		r := httptest.NewRequest("GET", tunnel.StreamDialPath+"?node=n&port=1", nil)
		r.RemoteAddr = remote
		if tok != "" {
			r.Header.Set("Authorization", "Bearer "+tok)
		}
		return s.streamAuth(r)
	}
	withEnv := &config.Config{APIKeyEnv: "FLEET_TEST_KEY"}
	noEnv := &config.Config{}
	cases := []struct {
		name   string
		cfg    *config.Config
		remote string
		tok    string
		want   bool
	}{
		{"env key", withEnv, "203.0.113.5:1", "s3cret", true},
		{"loopback still needs a key", withEnv, "127.0.0.1:1", "", false},
		{"no api_key_env still needs a key", noEnv, "127.0.0.1:1", "", false},
		{"wrong key", withEnv, "203.0.113.5:1", "nope", false},
		{"unrestricted api key", noEnv, "203.0.113.5:1", open.Key, true},
		{"stream-role api key", noEnv, "203.0.113.5:1", streamer.Key, true},
		{"role-restricted api key", withEnv, "203.0.113.5:1", coder.Key, false},
	}
	for _, c := range cases {
		if got := try(c.cfg, c.remote, c.tok); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// The daemon mounts the stream endpoints next to the control tunnel and guards them with streamAuth.
func TestStreamThroughDaemonHub(t *testing.T) {
	t.Setenv("FLEET_TEST_KEY", "s3cret")
	dir := t.TempDir()
	SetKeyStore(newKeyStore(filepath.Join(dir, "apikeys.json")))
	cfgPath := filepath.Join(dir, "fleet.json")
	if err := (&config.Config{APIKeyEnv: "FLEET_TEST_KEY"}).Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	s, err := New(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	hubSrv := httptest.NewServer(s.Handler())
	defer hubSrv.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); c.Close() }()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	cli, err := tunnel.StartClient(hubSrv.URL, "http://127.0.0.1:1", tunnel.NodeRegistration{NodeName: "worker", StreamPorts: []int{port}})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	deadline := time.Now().Add(3 * time.Second)
	for len(s.hub.ConnectedNodes()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	u := hubSrv.URL + tunnel.StreamDialPath + "?node=worker&port=" + strconv.Itoa(port)
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no key: status %d, want 401", resp.StatusCode)
	}

	local, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sc := &tunnel.StreamConnector{HubURL: hubSrv.URL, Node: "worker", Port: port, Key: "s3cret"}
	go func() { _ = sc.Serve(ctx, local) }()
	c, err := net.Dial("tcp", local.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("ring")); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 4)
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(c, b); err != nil || string(b) != "ring" {
		t.Fatalf("echo through the daemon's hub: %q %v", b, err)
	}
}

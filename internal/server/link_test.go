package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tevfik/fleet-router/internal/config"
)

func TestRouteKinds(t *testing.T) {
	old := sshHostName
	sshHostName = func(a string) string {
		return map[string]string{"home": "10.0.1.20", "far": "203.0.113.9", "ts": "100.101.2.3"}[a]
	}
	t.Cleanup(func() { sshHostName = old })
	for _, c := range []struct {
		r    nodeRoute
		want string
	}{
		{nodeRoute{source: "lan 10.0.1.5", base: "http://10.0.1.5:4711"}, "lan"},
		{nodeRoute{source: "url 10.0.0.7", base: "http://10.0.0.7:4711"}, "lan"},
		{nodeRoute{source: "url 8.8.8.8", base: "http://8.8.8.8:4711"}, "wan"},
		{nodeRoute{source: "ssh home", ssh: "home"}, "lan"},
		{nodeRoute{source: "ssh far", ssh: "far"}, "wan"},
		{nodeRoute{source: "ssh ts", ssh: "ts"}, "vpn"},
		{nodeRoute{source: "⇄ other", peer: &config.Peer{Name: "other"}}, "peer"},
	} {
		if got := routeKind(c.r); got != c.want {
			t.Errorf("%s: %s, want %s", c.r.source, got, c.want)
		}
	}
}

// The line of a pull is the first route to the machine that has the file; a limit is reported.
func TestLinkForUsesTheRouteOfTheHolder(t *testing.T) {
	_, dst, _, sha := twoFleets(t, nil)
	cfg := dst.Config()
	cfg.PullMbps = 80
	dst.cfg = cfg
	li, err := dst.LinkFor(context.Background(), "", sha)
	if err != nil || li.From != "nodeS" || li.Route != "test" || li.LimitMbps != 80 {
		t.Fatalf("%+v %v", li, err)
	}
	// the test route is a loopback address: a LAN
	if li.Kind != "lan" {
		t.Fatalf("kind %q", li.Kind)
	}
	if _, err := dst.LinkFor(context.Background(), "", "00"); err == nil {
		t.Fatal("nobody has that file")
	}
	var got LinkInfo
	rtJSON(t, rtDo(t, dst, "POST", "/fleet/store/link", map[string]any{"sha256": sha}), &got)
	if got.From != "nodeS" {
		t.Fatalf("%+v", got)
	}
}

// An Ollama directory handed to the store is registered through its manifests, with the projector noted.
func TestAddFilesReadsAnOllamaDirectory(t *testing.T) {
	s, _ := newNodeDaemon(t, &config.Config{Name: "laptop"})
	s.files.dir = t.TempDir()
	dir := t.TempDir()
	srvGGUF(t, filepath.Join(dir, "blobs", "sha256-aaaa"), 1)
	srvGGUF(t, filepath.Join(dir, "blobs", "sha256-bbbb"), 2)
	mf := filepath.Join(dir, "manifests", "registry.ollama.ai", "library", "vl", "7b")
	os.MkdirAll(filepath.Dir(mf), 0o755)
	os.WriteFile(mf, []byte(`{"layers":[{"mediaType":"application/vnd.ollama.image.model","digest":"sha256:aaaa"},{"mediaType":"application/vnd.ollama.image.projector","digest":"sha256:bbbb"}]}`), 0o644)
	n, err := s.AddFiles([]string{dir})
	if err != nil || n != 2 {
		t.Fatalf("%d %v", n, err)
	}
	rtEventually(t, "both blobs are registered", func() bool { return len(s.LocalStore().Files) == 2 })
	inv := s.LocalStore()
	names := map[string]bool{}
	for _, f := range inv.Files {
		names[f.Name] = true
	}
	if !names["vl-7b.gguf"] || !names["vl-7b.mmproj.gguf"] {
		t.Fatalf("%+v", inv.Files)
	}
	if len(inv.Notes) != 1 || !strings.Contains(inv.Notes[0], "projector") {
		t.Fatalf("the projector warning: %v", inv.Notes)
	}
}

package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

// A build without an update source (a plain `go build`, or the public build before the public repository exists)
// neither looks for nor installs releases, and says why.
func TestSource_NoneMeansNoUpdate(t *testing.T) {
	isolateUpdate(t)
	t.Setenv("FLEET_REPO", "")
	defer func(r string) { Repo = r }(Repo)
	Repo = ""
	if Source() != "" {
		t.Fatalf("Source() = %q", Source())
	}
	if _, err := Latest(context.Background()); !errors.Is(err, ErrNoSource) {
		t.Errorf("Latest: %v", err)
	}
	if err := Apply(context.Background(), "v9.9.9", filepath.Join(t.TempDir(), "fleet")); !errors.Is(err, ErrNoSource) {
		t.Errorf("Apply: %v", err)
	}
}

// FLEET_REPO wins over the built-in source (how an owner points a public binary somewhere else).
func TestSource_EnvOverridesBuiltin(t *testing.T) {
	defer func(r string) { Repo = r }(Repo)
	Repo = "https://git.example/private/fleet"
	t.Setenv("FLEET_REPO", "https://github.com/someone/fleet/")
	if Source() != "https://github.com/someone/fleet" {
		t.Fatalf("Source() = %q", Source())
	}
	t.Setenv("FLEET_REPO", "")
	if Source() != "https://git.example/private/fleet" {
		t.Fatalf("Source() = %q", Source())
	}
}

func TestSource_APIURL_GitHub(t *testing.T) {
	got, err := apiURL("https://github.com/someone/fleet", "/releases/latest")
	if err != nil || got != "https://api.github.com/repos/someone/fleet/releases/latest" {
		t.Fatalf("apiURL = %q %v", got, err)
	}
	for _, bad := range []string{"https://github.com/someone", "https://github.com/a/b/c"} {
		if _, err := apiURL(bad, "/x"); err == nil {
			t.Errorf("apiURL(%q) should fail", bad)
		}
	}
	// a Gitea repository keeps the Gitea API
	got, err = apiURL("https://git.example/o/fleet", "/releases/latest")
	if err != nil || got != "https://git.example/api/v1/repos/o/fleet/releases/latest" {
		t.Fatalf("Gitea apiURL = %q %v", got, err)
	}
}

func TestSource_TokenNeverToGitHub(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://git.example/o/r":                             true,
		"https://github.com/o/r/releases/download/v1/x":       false,
		"https://api.github.com/repos/o/r/releases/latest":    false,
		"https://objects.githubusercontent.com/github-prod/x": false,
		"http://git.example/o/r":                              false, // plain http: never (tokenMaySendTo)
	} {
		u, _ := url.Parse(raw)
		if got := tokenMaySendTo(u) && !isGitHub(u); got != want {
			t.Errorf("%s: token sent = %v, want %v", raw, got, want)
		}
	}
}

// A whole update from a GitHub-shaped server (API under /repos/…, assets under <repo>/releases/download/…): the
// release is found, the binary is checked against SHA256SUMS and installed, and the Gitea token is not sent.
func TestSource_UpdateFromGitHub(t *testing.T) {
	isolateUpdate(t)
	t.Setenv("FLEET_TOKEN", "private-gitea-token")
	bin := []byte("#!/bin/sh\necho new fleet\n")
	sum := sha256.Sum256(bin)
	name := "fleet-" + runtime.GOOS + "-" + runtime.GOARCH
	var tokenSeen atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			tokenSeen.Store(true)
		}
		switch r.URL.Path {
		case "/repos/someone/fleet/releases/latest":
			fmt.Fprint(w, `{"tag_name":"v9.9.9","body":"new"}`)
		case "/someone/fleet/releases/download/v9.9.9/" + name:
			w.Write(bin)
		case "/someone/fleet/releases/download/v9.9.9/SHA256SUMS":
			fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), name)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	defer func(h, a string) { githubHost, githubAPI = h, a }(githubHost, githubAPI)
	githubHost, githubAPI = u.Host, srv.URL
	t.Setenv("FLEET_REPO", srv.URL+"/someone/fleet")

	rel, err := Latest(context.Background())
	if err != nil || rel.Tag != "v9.9.9" {
		t.Fatalf("Latest = %+v %v", rel, err)
	}
	exe := filepath.Join(t.TempDir(), "fleet")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Apply(context.Background(), rel.Tag, exe); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); string(b) != string(bin) {
		t.Errorf("installed %q", b)
	}
	if tokenSeen.Load() {
		t.Error("the Gitea token was sent to GitHub")
	}
}

// The private Gitea path is unchanged: the token is still sent there.
func TestSource_GiteaStillGetsToken(t *testing.T) {
	isolateUpdate(t)
	t.Setenv("FLEET_TOKEN", "private-gitea-token")
	var auth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.Store(r.Header.Get("Authorization"))
		if strings.HasSuffix(r.URL.Path, "/api/v1/repos/o/fleet/releases/latest") {
			fmt.Fprint(w, `{"tag_name":"v1.2.3"}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	t.Setenv("FLEET_REPO", srv.URL+"/o/fleet")
	if rel, err := Latest(context.Background()); err != nil || rel.Tag != "v1.2.3" {
		t.Fatalf("Latest = %+v %v", rel, err)
	}
	if a, _ := auth.Load().(string); a != "token private-gitea-token" {
		t.Errorf("Authorization = %q", a)
	}
}

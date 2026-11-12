package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		tag, cur string
		force    bool
		want     bool
	}{
		{"v0.3.0", "v0.2.0", false, true},
		{"v0.2.0", "v0.2.0", false, false},
		{"v0.2.0", "v0.3.0", false, false},
		{"v0.10.0", "v0.9.9", false, true},
		{"v0.2.0", "dev", false, false}, // a dev build is not auto-replaced
		{"v0.2.0", "dev", true, true},
		{"v0.2.0", "4341344-dirty", true, true},
		{"v0.2.0", "v0.2.0-3-gabc", false, false},
		{"nonsense", "v0.1.0", true, false},
	}
	for _, c := range cases {
		if got := Newer(c.tag, c.cur, c.force); got != c.want {
			t.Errorf("Newer(%q,%q,%v)=%v want %v", c.tag, c.cur, c.force, got, c.want)
		}
	}
}

func fakeRepo(t *testing.T, bin []byte, sum string) *httptest.Server {
	name := "fleet-" + runtime.GOOS + "-" + runtime.GOARCH
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/repos/o/fleet/releases/latest":
			fmt.Fprint(w, `{"tag_name":"v9.0.0"}`)
		case "/o/fleet/releases/download/v9.0.0/" + name:
			w.Write(bin)
		case "/o/fleet/releases/download/v9.0.0/SHA256SUMS":
			fmt.Fprintf(w, "%s  %s\n", sum, name)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestLatestAndApply(t *testing.T) {
	bin := []byte("new binary")
	s := sha256.Sum256(bin)
	srv := fakeRepo(t, bin, hex.EncodeToString(s[:]))
	defer srv.Close()
	t.Setenv("FLEET_REPO", srv.URL+"/o/fleet")
	r, err := Latest(context.Background())
	if err != nil || r.Tag != "v9.0.0" {
		t.Fatalf("Latest = %v, %v", r, err)
	}
	exe := filepath.Join(t.TempDir(), "fleet")
	os.WriteFile(exe, []byte("old"), 0o755)
	if err := Apply(context.Background(), r.Tag, exe); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "new binary" {
		t.Fatalf("binary not replaced: %q", b)
	}
}

func TestApplyRejectsBadChecksum(t *testing.T) {
	srv := fakeRepo(t, []byte("tampered"), "00ff")
	defer srv.Close()
	t.Setenv("FLEET_REPO", srv.URL+"/o/fleet")
	exe := filepath.Join(t.TempDir(), "fleet")
	os.WriteFile(exe, []byte("old"), 0o755)
	if err := Apply(context.Background(), "v9.0.0", exe); err == nil {
		t.Fatal("a checksum mismatch must fail")
	}
	if b, _ := os.ReadFile(exe); string(b) != "old" {
		t.Fatalf("the old binary must stay: %q", b)
	}
	if _, err := os.Stat(exe + ".new"); err == nil {
		t.Fatal("temporary file left behind")
	}
}

func TestTokenFromEnvThenFile(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("FLEET_TOKEN", "")
	if token() != "" {
		t.Fatal("no token expected")
	}
	os.MkdirAll(filepath.Join(cfg, "fleet"), 0o700)
	os.WriteFile(filepath.Join(cfg, "fleet", "update_token"), []byte("from-file\nignored\n"), 0o600)
	if got := token(); got != "from-file" {
		t.Fatalf("token from file = %q", got)
	}
	t.Setenv("FLEET_TOKEN", "from-env")
	if got := token(); got != "from-env" {
		t.Fatalf("env must win, got %q", got)
	}
}

func TestRequestsCarryTheToken(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		fmt.Fprint(w, `{"tag_name":"v1.0.0"}`)
	}))
	defer srv.Close()
	t.Setenv("FLEET_TOKEN", "s3cret")
	t.Setenv("FLEET_REPO", srv.URL+"/o/fleet")
	if _, err := Latest(context.Background()); err != nil {
		t.Fatal(err)
	}
	if auth != "token s3cret" {
		t.Fatalf("Authorization = %q", auth)
	}
}

func TestInstalledStamp(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	os.MkdirAll(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "fleet"), 0o700)
	if Installed() != "" {
		t.Fatal("nothing installed yet")
	}
	MarkInstalled("v1.2.3")
	if Installed() != "v1.2.3" {
		t.Fatalf("Installed = %q", Installed())
	}
}

func TestApplyKeepsPreviousAndRollbackRestoresIt(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	os.MkdirAll(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "fleet"), 0o700)
	bin := []byte("v2 binary")
	s := sha256.Sum256(bin)
	srv := fakeRepo(t, bin, hex.EncodeToString(s[:]))
	defer srv.Close()
	t.Setenv("FLEET_REPO", srv.URL+"/o/fleet")
	exe := filepath.Join(t.TempDir(), "fleet")
	os.WriteFile(exe, []byte("v1 binary"), 0o755)
	if err := Apply(context.Background(), "v9.0.0", exe); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe + ".prev"); string(b) != "v1 binary" {
		t.Fatalf("previous binary not kept: %q", b)
	}
	MarkPending("v9.0.0")
	if err := Rollback(exe, "v9.0.0"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "v1 binary" {
		t.Fatalf("rollback left %q", b)
	}
	if Installed() != "v9.0.0" {
		t.Fatal("the rolled-back tag must be skipped by the auto-update")
	}
	if _, err := os.Stat(pendingPath()); err == nil {
		t.Fatal("pending marker should be gone")
	}
}

func TestCrashLoopRollsBackAutomatically(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	os.MkdirAll(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "fleet"), 0o700)
	exe := filepath.Join(t.TempDir(), "fleet")
	os.WriteFile(exe, []byte("bad"), 0o755)
	os.WriteFile(exe+".prev", []byte("good"), 0o755)
	if tag, err := OnStart(exe); tag != "" || err != nil { // no marker: a normal start
		t.Fatalf("unexpected %q %v", tag, err)
	}
	MarkPending("v9.9.9")
	for i := 1; i <= MaxStarts; i++ { // the first starts of a new version are tolerated
		if tag, err := OnStart(exe); tag != "" || err != nil {
			t.Fatalf("start %d rolled back too early: %q %v", i, tag, err)
		}
	}
	tag, err := OnStart(exe) // one more without having proven itself
	if err != nil || tag != "v9.9.9" {
		t.Fatalf("want a rollback of v9.9.9, got %q %v", tag, err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "good" {
		t.Fatalf("binary = %q", b)
	}
}

func TestHealthyVersionClearsThePendingMarker(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	os.MkdirAll(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "fleet"), 0o700)
	MarkPending("v1.0.0")
	ClearPending()
	exe := filepath.Join(t.TempDir(), "fleet")
	os.WriteFile(exe, []byte("x"), 0o755)
	for i := 0; i < 2*MaxStarts; i++ {
		if tag, _ := OnStart(exe); tag != "" {
			t.Fatal("a confirmed version must never roll back")
		}
	}
}

func TestReleaseNotesAreTrimmed(t *testing.T) {
	r := Release{Tag: "v1", Body: "first\n\n\nsecond  \r\n" + strings.Repeat("x\n", 20)}
	n := r.Notes()
	lines := strings.Split(n, "\n")
	if lines[0] != "  first" || lines[1] != "  second" || len(lines) != 13 || lines[12] != "  …" {
		t.Fatalf("notes = %q", n)
	}
	if (Release{}).Notes() != "" {
		t.Fatal("no body, no notes")
	}
}

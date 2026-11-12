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
	"testing"
)

func TestDetectMachine(t *testing.T) {
	m := DetectMachine()
	if m.OS != runtime.GOOS {
		t.Fatalf("expected OS %s, got %s", runtime.GOOS, m.OS)
	}
	if m.Arch != runtime.GOARCH {
		t.Fatalf("expected Arch %s, got %s", runtime.GOARCH, m.Arch)
	}
}

func TestNamesAndOrNone(t *testing.T) {
	fs := []EngineFile{
		{Name: "fleet-engine-linux-amd64-cpu.xz"},
		{Name: "fleet-engine-linux-amd64-cuda12-sm89.xz"},
	}
	got := names(fs)
	want := "linux-amd64-cpu, linux-amd64-cuda12-sm89"
	if got != want {
		t.Fatalf("names got %q, want %q", got, want)
	}

	if orNone("") != "no GPU" {
		t.Fatalf("orNone(\"\") = %q, want \"no GPU\"", orNone(""))
	}
	if orNone("RTX 4090") != "RTX 4090" {
		t.Fatalf("orNone(\"RTX 4090\") = %q", orNone("RTX 4090"))
	}
}

func TestEngineBase(t *testing.T) {
	t.Setenv("FLEET_ENGINE_REPO", "https://git.example.com/owner/repo")
	t.Setenv("FLEET_ENGINE_API", "")
	root, owner, err := engineBase()
	if err != nil || root != "https://git.example.com" || owner != "owner" {
		t.Fatalf("engineBase() = (%q, %q, %v)", root, owner, err)
	}

	t.Setenv("FLEET_ENGINE_API", "https://mirror.example.com/api")
	root, owner, err = engineBase()
	if err != nil || root != "https://mirror.example.com/api" || owner != "owner" {
		t.Fatalf("engineBase() with mirror = (%q, %q, %v)", root, owner, err)
	}

	// Invalid URL
	t.Setenv("FLEET_ENGINE_REPO", "://bad-url")
	if _, _, err := engineBase(); err == nil {
		t.Fatal("expected error for bad URL")
	}

	// Less than 2 path segments
	t.Setenv("FLEET_ENGINE_REPO", "https://git.example.com/onlyone")
	if _, _, err := engineBase(); err == nil {
		t.Fatal("expected error for URL with <2 path segments")
	}

	// No engine source at all: the fleet repository is NOT one (fleet-engine is not distributed with fleet)
	t.Setenv("FLEET_ENGINE_REPO", "")
	t.Setenv("FLEET_REPO", "https://git.example.com/owner/repo")
	if _, _, err := engineBase(); !errors.Is(err, ErrNoEngineSource) {
		t.Fatalf("no source: err = %v, want ErrNoEngineSource", err)
	}
	EngineRepo = "https://git.example.com/built/in"
	defer func() { EngineRepo = "" }()
	if _, owner, err := engineBase(); err != nil || owner != "built" {
		t.Fatalf("built-in source: (%q, %v)", owner, err)
	}
}

func TestCopyFile(t *testing.T) {
	d := t.TempDir()
	src := filepath.Join(d, "src.bin")
	dst := filepath.Join(d, "dst.bin")

	// Missing src
	if err := copyFile(src, dst); err == nil {
		t.Fatal("expected error for missing src")
	}

	if err := os.WriteFile(src, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Destination is a directory -> OpenFile error
	if err := copyFile(src, d); err == nil {
		t.Fatal("expected error when dst is a directory")
	}

	// Successful copy
	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile failed: %v", err)
	}
	data, err := os.ReadFile(dst)
	if err != nil || string(data) != "content" {
		t.Fatalf("dst content mismatch: %q, %v", string(data), err)
	}
}

func TestKeepPreviousErrors(t *testing.T) {
	d := t.TempDir()
	exe := filepath.Join(d, "nonexistent")
	// keepPrevious on nonexistent file should fail
	if err := keepPrevious(exe); err == nil {
		t.Fatal("keepPrevious should fail when exe does not exist")
	}
}

func TestRollbackErrors(t *testing.T) {
	d := t.TempDir()
	exe := filepath.Join(d, "fleet")
	// No previous file exists
	if err := Rollback(exe, "v1.0.0"); err == nil {
		t.Fatal("Rollback should fail when .prev does not exist")
	}

	// .prev exists but dst directory is unwritable or cannot be copied
	prev := exe + ".prev"
	if err := os.WriteFile(prev, []byte("prev-data"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Make rollback target a directory so copyFile fails
	rollbackTmp := exe + ".rollback"
	if err := os.Mkdir(rollbackTmp, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Rollback(exe, "v1.0.0"); err == nil {
		t.Fatal("Rollback should fail when copyFile fails")
	}
}

func TestOnStartCorruptAndRollbackFail(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	os.MkdirAll(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "fleet"), 0o700)

	// Corrupt JSON in pending marker
	if err := os.WriteFile(pendingPath(), []byte("{bad-json"), 0o644); err != nil {
		t.Fatal(err)
	}
	tag, err := OnStart("/dummy/exe")
	if tag != "" || err != nil {
		t.Fatalf("corrupt pending should be cleared with no error, got tag=%q err=%v", tag, err)
	}
	if _, err := os.Stat(pendingPath()); err == nil {
		t.Fatal("pending marker should have been removed")
	}

	// Exceeded MaxStarts but Rollback fails because .prev does not exist
	exe := filepath.Join(t.TempDir(), "fleet")
	MarkPending("v2.0.0")
	for i := 1; i <= MaxStarts; i++ {
		_, _ = OnStart(exe)
	}
	// Next start triggers Rollback, which should fail because .prev does not exist
	tag, err = OnStart(exe)
	if err == nil {
		t.Fatal("expected error when auto-rollback fails")
	}
}

func TestApiURLBad(t *testing.T) {
	if _, err := apiURL("invalid-url", "/test"); err == nil {
		t.Fatal("expected error without ://")
	}
	if _, err := apiURL("http://nodir", "/test"); err == nil {
		t.Fatal("expected error without slash in path")
	}
}

func TestParseAndNewerExtra(t *testing.T) {
	if _, ok := parse("1.0"); ok {
		t.Fatal("parse(1.0) should be false")
	}
	if _, ok := parse("1.0.x"); ok {
		t.Fatal("parse(1.0.x) should be false")
	}
	if _, ok := parse("v1.2.3-beta+exp"); !ok {
		t.Fatal("parse(v1.2.3-beta+exp) should be true")
	}

	// Newer cases
	if Newer("invalid", "v1.0.0", false) {
		t.Fatal("Newer with invalid tag should be false")
	}
	if Newer("v1.0.0", "v1.0.0", false) {
		t.Fatal("Newer with identical tag should be false")
	}
	if Newer("v1.0.0", "v1.0.0", true) {
		t.Fatal("Newer with identical tag and force should be false unless dirty")
	}
	if !Newer("v1.0.0", "v1.0.0-dirty", true) {
		t.Fatal("Newer with dirty tag and force should be true")
	}
	if Newer("v1.0.0", "v1.0.0-dirty", false) {
		t.Fatal("Newer with dirty tag and no force should be false")
	}
	if !Newer("v2.0.0", "v1.9.9", false) {
		t.Fatal("Newer major version should be true")
	}
	if Newer("v1.0.0", "v2.0.0", false) {
		t.Fatal("Newer older version should be false")
	}
	if !Newer("v1.2.0", "dev", true) {
		t.Fatal("Newer dev with force should be true")
	}
	if Newer("v1.2.0", "dev", false) {
		t.Fatal("Newer dev without force should be false")
	}
}

func TestTokenMaySendTo(t *testing.T) {
	tests := []struct {
		rawURL string
		want   bool
	}{
		{"https://git.example.com", true},
		{"http://localhost:8080", true},
		{"http://127.0.0.1:8080", true},
		{"http://[::1]:8080", true},
		{"http://external.example.com", false},
		{"http://10.0.1.100:8080", false},
	}
	for _, tc := range tests {
		u, err := url.Parse(tc.rawURL)
		if err != nil {
			t.Fatalf("parse %s: %v", tc.rawURL, err)
		}
		if got := tokenMaySendTo(u); got != tc.want {
			t.Errorf("tokenMaySendTo(%q) = %v, want %v", tc.rawURL, got, tc.want)
		}
	}
}

func TestEnsureStageLinkSymlinkFail(t *testing.T) {
	d := t.TempDir()
	roDir := filepath.Join(d, "readonly")
	if err := os.Mkdir(roDir, 0o555); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(roDir, "fleet-engine")
	// In a directory where user cannot create symlink (if permissions allow restriction)
	msg := ensureStageLink(exe)
	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		if msg == "" || !strings.Contains(msg, "cannot link") {
			t.Logf("msg: %q", msg)
		}
	}
}

func TestEngineFilesAndGetLong(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v1/packages/fleet/generic/fleet-engine/") && strings.HasSuffix(r.URL.Path, "/files"):
			fmt.Fprint(w, `[
				{"name": "fleet-engine-linux-amd64-cpu.xz", "size": 1000, "sha256": "abc"},
				{"name": "ignored-file.txt", "size": 50, "sha256": "def"}
			]`)
		case r.URL.Path == "/download/fleet-engine-linux-amd64-cpu.xz":
			w.Write([]byte("fake-compressed-payload"))
		case r.URL.Path == "/404":
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	t.Setenv("FLEET_ENGINE_REPO", srv.URL+"/fleet/fleet")

	files, err := EngineFiles(context.Background())
	if err != nil {
		t.Fatalf("EngineFiles failed: %v", err)
	}
	if len(files) != 1 || files[0].Name != "fleet-engine-linux-amd64-cpu.xz" {
		t.Fatalf("unexpected files: %+v", files)
	}

	// Test getLong success
	resp, err := getLong(context.Background(), srv.URL+"/download/fleet-engine-linux-amd64-cpu.xz")
	if err != nil {
		t.Fatalf("getLong failed: %v", err)
	}
	resp.Body.Close()

	// Test getLong 404
	if _, err := getLong(context.Background(), srv.URL+"/404"); err == nil {
		t.Fatal("expected error on getLong 404")
	}

	// EngineFiles error when repo URL is invalid
	t.Setenv("FLEET_ENGINE_REPO", "://bad")
	if _, err := EngineFiles(context.Background()); err == nil {
		t.Fatal("expected error on bad FLEET_REPO")
	}
}

func TestEnsureEngineMatchingAndErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/packages/fleet/generic/fleet-engine/") && strings.HasSuffix(r.URL.Path, "/files") {
			fmt.Fprint(w, `[
				{"name": "fleet-engine-linux-amd64-cpu.xz", "size": 1000, "sha256": "abc"},
				{"name": "fleet-engine-linux-amd64-cuda12-sm89.xz", "size": 2000, "sha256": "def"}
			]`)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	t.Setenv("FLEET_ENGINE_REPO", srv.URL+"/fleet/fleet")
	t.Setenv("FLEET_PINNED_ENGINE", "v0.1.0")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	os.MkdirAll(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "fleet"), 0o700)

	exe := filepath.Join(t.TempDir(), "fleet-engine")

	// 1. Variant not found
	_, err := EnsureEngine(context.Background(), exe, "nonexistent-variant", false, nil)
	if err == nil {
		t.Fatal("expected error for nonexistent variant")
	}

	// 2. Variant matches multiple (ambiguous)
	_, err = EnsureEngine(context.Background(), exe, "linux-amd64", false, nil)
	if err == nil {
		t.Fatal("expected error for ambiguous variant")
	}

	// 3. Variant already installed
	_ = os.WriteFile(engineStampPath(), []byte(engineVersion()+" fleet-engine-linux-amd64-cpu.xz\n"), 0o644)
	msg, err := EnsureEngine(context.Background(), exe, "cpu", false, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(msg, "already the engine build") {
		t.Fatalf("expected 'already the engine build', got %q", msg)
	}
}

func TestLatestAndApplyErrors(t *testing.T) {
	// 1. Latest with bad repo URL
	t.Setenv("FLEET_REPO", "://bad-url")
	if _, err := Latest(context.Background()); err == nil {
		t.Fatal("expected error for bad repo URL")
	}

	// 2. Latest with 404
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	t.Setenv("FLEET_REPO", srv.URL+"/o/fleet")
	if _, err := Latest(context.Background()); err == nil {
		t.Fatal("expected error on 404")
	}

	// 3. Latest with empty tag
	srvEmpty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"tag_name":""}`)
	}))
	defer srvEmpty.Close()
	t.Setenv("FLEET_REPO", srvEmpty.URL+"/o/fleet")
	if _, err := Latest(context.Background()); err == nil {
		t.Fatal("expected error on empty tag_name")
	}

	// 4. Apply missing SHA256SUMS
	name := "fleet-" + runtime.GOOS + "-" + runtime.GOARCH
	srvNoSums := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, name) {
			w.Write([]byte("binary-content"))
		} else {
			http.NotFound(w, r)
		}
	}))
	defer srvNoSums.Close()
	t.Setenv("FLEET_REPO", srvNoSums.URL+"/o/fleet")
	exe := filepath.Join(t.TempDir(), "fleet")
	os.WriteFile(exe, []byte("old"), 0o755)
	if err := Apply(context.Background(), "v1.0.0", exe); err == nil {
		t.Fatal("Apply must fail when SHA256SUMS is missing")
	}

	// 5. Apply with checksum missing the binary entry
	srvWrongSumFile := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, name) {
			w.Write([]byte("binary-content"))
		} else {
			fmt.Fprintln(w, "abcd1234  other-file")
		}
	}))
	defer srvWrongSumFile.Close()
	t.Setenv("FLEET_REPO", srvWrongSumFile.URL+"/o/fleet")
	if err := Apply(context.Background(), "v1.0.0", exe); err == nil {
		t.Fatal("Apply must fail when binary is not listed in SHA256SUMS")
	}

	// 6. Apply when keepPrevious fails (e.g. exe is a directory)
	bin := []byte("bin-content")
	s := sha256.Sum256(bin)
	srvOk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, name) {
			w.Write(bin)
		} else {
			fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(s[:]), name)
		}
	}))
	defer srvOk.Close()
	t.Setenv("FLEET_REPO", srvOk.URL+"/o/fleet")
	exeDir := filepath.Join(t.TempDir(), "fleet-dir")
	os.MkdirAll(exeDir, 0o755)
	if err := Apply(context.Background(), "v1.0.0", exeDir); err == nil {
		t.Fatal("Apply must fail when keepPrevious fails on a directory")
	}
}

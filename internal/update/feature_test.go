package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

// isolateUpdate keeps every marker file (last_update, update_pending, update_token) in a temp dir.
func isolateUpdate(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	t.Setenv("HOME", d)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(d, "cfg"))
	t.Setenv("FLEET_CONFIG", filepath.Join(d, "config.yaml"))
	t.Setenv("FLEET_STATS", filepath.Join(d, "stats.json"))
	t.Setenv("FLEET_TOKEN", "")
	t.Setenv("FLEET_REPO", "http://127.0.0.1:1/never/used") // never the real server
	return d
}

func TestFeature_Version_ParseEdgeCases(t *testing.T) {
	cases := map[string]struct {
		n  [3]int
		ok bool
	}{
		"v1.2.3":        {[3]int{1, 2, 3}, true},
		"1.2.3":         {[3]int{1, 2, 3}, true},
		"v1.2.3-4-gabc": {[3]int{1, 2, 3}, true},
		"v1.2.3+meta":   {[3]int{1, 2, 3}, true},
		"v1.2.3-dirty":  {[3]int{1, 2, 3}, true},
		"v1.2":          {ok: false},
		"v1.x.3":        {ok: false},
		"":              {ok: false},
		"dev":           {ok: false},
		"v1.2.3.4":      {ok: false}, // SplitN keeps "3.4" in the last part
		"vv1.2.3":       {ok: false},
	}
	for in, want := range cases {
		n, ok := parse(in)
		if ok != want.ok || (ok && n != want.n) {
			t.Errorf("parse(%q) = %v %v, want %v %v", in, n, ok, want.n, want.ok)
		}
	}
}

func TestFeature_Version_NewerEdgeCases(t *testing.T) {
	cases := []struct {
		tag, cur string
		force    bool
		want     bool
	}{
		{"v1.0.0", "v0.99.99", false, true},
		{"v1.2.10", "v1.2.9", false, true},
		{"v2.0.0", "v10.0.0", false, false},
		{"v0.2.0", "v0.2.0", true, false},      // same release: even force does nothing
		{"v0.2.0", "v0.2.0-dirty", true, true}, // dirty build of same number: force replaces
		{"v0.2.0", "v0.2.0-dirty", false, false},
		{"v0.2.0", "v0.2.0+x", true, true},
		{"v0.2.0-rc1", "v0.2.0-rc1", true, false}, // identical strings never "newer"
		{"v0.1.0", "v0.2.0-dirty", true, false},   // older tag never wins
		{"", "v1.0.0", true, false},
		{"v1.0.0", "", false, false},
		{"v1.0.0", "", true, true},
	}
	for _, c := range cases {
		if got := Newer(c.tag, c.cur, c.force); got != c.want {
			t.Errorf("Newer(%q,%q,%v)=%v want %v", c.tag, c.cur, c.force, got, c.want)
		}
	}
}

func TestFeature_APIURL(t *testing.T) {
	got, err := apiURL("https://git.example/owner/name", "/releases/latest")
	if err != nil || got != "https://git.example/api/v1/repos/owner/name/releases/latest" {
		t.Fatalf("apiURL = %q %v", got, err)
	}
	got, err = apiURL("http://127.0.0.1:3000/a/b", "")
	if err != nil || got != "http://127.0.0.1:3000/api/v1/repos/a/b" {
		t.Fatalf("apiURL port = %q %v", got, err)
	}
	for _, bad := range []string{"git.example/owner/name", "https://hostonly", ""} {
		if _, err := apiURL(bad, "/x"); err == nil {
			t.Errorf("apiURL(%q) should fail", bad)
		}
	}
}

func TestFeature_Repo_EnvOverrideTrimsSlash(t *testing.T) {
	t.Setenv("FLEET_REPO", "")
	defer func(r string) { Repo = r }(Repo)
	Repo = "https://git.example/o/r/"
	if repo() != "https://git.example/o/r" {
		t.Fatalf("built-in repo = %q", repo())
	}
	t.Setenv("FLEET_REPO", "http://127.0.0.1:9/o/r///")
	if repo() != "http://127.0.0.1:9/o/r" {
		t.Fatalf("repo() = %q", repo())
	}
}

func TestFeature_Latest_Errors(t *testing.T) {
	isolateUpdate(t)
	var body atomic.Value
	var status atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/o/fleet/releases/latest" {
			http.NotFound(w, r)
			return
		}
		if s := status.Load(); s != 0 {
			w.WriteHeader(int(s))
		}
		fmt.Fprint(w, body.Load().(string))
	}))
	defer srv.Close()
	t.Setenv("FLEET_REPO", srv.URL+"/o/fleet")
	ctx := context.Background()

	body.Store(`{"tag_name":"v3.1.4","body":"notes here"}`)
	if r, err := Latest(ctx); err != nil || r.Tag != "v3.1.4" || r.Body != "notes here" {
		t.Fatalf("Latest = %+v %v", r, err)
	}
	body.Store(`{"tag_name":""}`)
	if _, err := Latest(ctx); err == nil || !strings.Contains(err.Error(), "no release") {
		t.Fatalf("empty tag: %v", err)
	}
	body.Store(`not json`)
	if _, err := Latest(ctx); err == nil {
		t.Fatal("bad JSON must fail")
	}
	body.Store(`{}`)
	status.Store(500)
	if _, err := Latest(ctx); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("HTTP 500: %v", err)
	}
	t.Setenv("FLEET_REPO", "not-a-url")
	if _, err := Latest(ctx); err == nil {
		t.Fatal("bad repo URL must fail")
	}
}

func TestFeature_Apply_ChecksumFormatsAndFailures(t *testing.T) {
	isolateUpdate(t)
	name := "fleet-" + runtime.GOOS + "-" + runtime.GOARCH
	bin := []byte("brand new")
	s := sha256.Sum256(bin)
	sum := hex.EncodeToString(s[:])
	var sums atomic.Value
	var noBinary atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/o/fleet/releases/download/v1.0.0/" + name:
			if noBinary.Load() {
				http.NotFound(w, r)
				return
			}
			w.Write(bin)
		case "/o/fleet/releases/download/v1.0.0/SHA256SUMS":
			v := sums.Load().(string)
			if v == "404" {
				http.NotFound(w, r)
				return
			}
			fmt.Fprint(w, v)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t.Setenv("FLEET_REPO", srv.URL+"/o/fleet")
	ctx := context.Background()
	newExe := func() string {
		exe := filepath.Join(t.TempDir(), "fleet")
		os.WriteFile(exe, []byte("old"), 0o755)
		return exe
	}
	content := func(p string) string { b, _ := os.ReadFile(p); return string(b) }

	// binary-mode "*name" entries among others are accepted
	sums.Store("deadbeef  fleet-other-arch\n" + sum + " *" + name + "\n")
	exe := newExe()
	if err := Apply(ctx, "v1.0.0", exe); err != nil {
		t.Fatal(err)
	}
	if content(exe) != "brand new" || content(exe+".prev") != "old" {
		t.Fatalf("after apply: %q prev %q", content(exe), content(exe+".prev"))
	}
	if fi, _ := os.Stat(exe); fi.Mode().Perm()&0o100 == 0 {
		t.Fatal("new binary must be executable")
	}

	// no line for this platform: refused
	sums.Store(sum + "  fleet-plan9-mips\n")
	exe = newExe()
	if err := Apply(ctx, "v1.0.0", exe); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("missing entry: %v", err)
	}
	if content(exe) != "old" {
		t.Fatal("old binary must stay")
	}

	// no SHA256SUMS at all: refused, nothing left behind
	sums.Store("404")
	exe = newExe()
	if err := Apply(ctx, "v1.0.0", exe); err == nil || !strings.Contains(err.Error(), "SHA256SUMS") {
		t.Fatalf("no sums: %v", err)
	}
	if content(exe) != "old" {
		t.Fatal("old binary must stay")
	}
	if _, err := os.Stat(exe + ".new"); err == nil {
		t.Fatal("temp file left behind")
	}

	// the binary is missing for this tag
	noBinary.Store(true)
	exe = newExe()
	if err := Apply(ctx, "v1.0.0", exe); err == nil {
		t.Fatal("missing binary must fail")
	}
	if _, err := os.Stat(exe + ".new"); err == nil {
		t.Fatal("temp file left behind")
	}

	// unwritable target directory
	if err := Apply(ctx, "v1.0.0", filepath.Join(t.TempDir(), "no", "such", "fleet")); err == nil {
		t.Fatal("unwritable exe path must fail")
	}
}

func TestFeature_Apply_SendsTokenOnDownloads(t *testing.T) {
	d := isolateUpdate(t)
	os.WriteFile(filepath.Join(d, "update_token"), []byte("filetok\n"), 0o600) // next to FLEET_CONFIG
	name := "fleet-" + runtime.GOOS + "-" + runtime.GOARCH
	bin := []byte("x")
	s := sha256.Sum256(bin)
	var bad atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token filetok" {
			bad.Add(1)
		}
		if strings.HasSuffix(r.URL.Path, "SHA256SUMS") {
			fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(s[:]), name)
			return
		}
		w.Write(bin)
	}))
	defer srv.Close()
	t.Setenv("FLEET_REPO", srv.URL+"/o/fleet")
	exe := filepath.Join(t.TempDir(), "fleet")
	os.WriteFile(exe, []byte("old"), 0o755)
	if err := Apply(context.Background(), "v1.0.0", exe); err != nil {
		t.Fatal(err)
	}
	if bad.Load() != 0 {
		t.Fatalf("%d requests without the token from update_token", bad.Load())
	}
}

func TestFeature_Rollback_NoPreviousAndKeepsWorking(t *testing.T) {
	isolateUpdate(t)
	exe := filepath.Join(t.TempDir(), "fleet")
	os.WriteFile(exe, []byte("current"), 0o755)
	if err := Rollback(exe, "v2"); err == nil || !strings.Contains(err.Error(), "no previous") {
		t.Fatalf("no .prev: %v", err)
	}
	if Installed() != "" {
		t.Fatal("a failed rollback must not mark anything")
	}
	os.WriteFile(exe+".prev", []byte("previous"), 0o755)
	if err := Rollback(exe, ""); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "previous" {
		t.Fatalf("exe = %q", b)
	}
	if Installed() != "" {
		t.Fatal("empty fromTag must not be stamped")
	}
	if _, err := os.Stat(exe + ".prev"); err != nil {
		t.Fatal(".prev is copied, not moved: a second rollback still works")
	}
	if _, err := os.Stat(exe + ".rollback"); err == nil {
		t.Fatal("temp file left behind")
	}
}

func TestFeature_KeepPrevious_ReplacesStaleCopy(t *testing.T) {
	isolateUpdate(t)
	exe := filepath.Join(t.TempDir(), "fleet")
	os.WriteFile(exe, []byte("now"), 0o755)
	os.WriteFile(exe+".prev", []byte("ancient"), 0o755)
	if err := keepPrevious(exe); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe + ".prev"); string(b) != "now" {
		t.Fatalf(".prev = %q", b)
	}
	if err := keepPrevious(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing exe must fail")
	}
}

func TestFeature_OnStart_PendingLogic(t *testing.T) {
	isolateUpdate(t)
	exe := filepath.Join(t.TempDir(), "fleet")
	os.WriteFile(exe, []byte("new"), 0o755)

	// a corrupt marker is dropped, nothing happens
	os.WriteFile(pendingPath(), []byte("{garbage"), 0o644)
	if tag, err := OnStart(exe); tag != "" || err != nil {
		t.Fatalf("corrupt marker: %q %v", tag, err)
	}
	if _, err := os.Stat(pendingPath()); err == nil {
		t.Fatal("corrupt marker must be removed")
	}

	// attempts are counted in the marker file
	MarkPending("v5.0.0")
	OnStart(exe)
	OnStart(exe)
	var p pending
	b, _ := os.ReadFile(pendingPath())
	if json.Unmarshal(b, &p) != nil || p.Tag != "v5.0.0" || p.Attempts != 2 {
		t.Fatalf("marker = %s", b)
	}

	// limit exceeded but no .prev: the error is returned and the marker cleared (no endless retry)
	OnStart(exe)
	tag, err := OnStart(exe)
	if err == nil || tag != "" {
		t.Fatalf("rollback without .prev: %q %v", tag, err)
	}
	if _, err := os.Stat(pendingPath()); err == nil {
		t.Fatal("marker must be cleared after a failed rollback")
	}
	if b, _ := os.ReadFile(exe); string(b) != "new" {
		t.Fatal("binary untouched")
	}
	if tag, err := OnStart(exe); tag != "" || err != nil {
		t.Fatal("subsequent starts are normal")
	}

	// a successful auto-rollback stamps the bad tag so it is not installed again
	os.WriteFile(exe+".prev", []byte("old"), 0o755)
	MarkPending("v6.0.0")
	for i := 0; i < MaxStarts; i++ {
		OnStart(exe)
	}
	if tag, err := OnStart(exe); tag != "v6.0.0" || err != nil {
		t.Fatalf("rollback: %q %v", tag, err)
	}
	if Installed() != "v6.0.0" {
		t.Fatalf("Installed = %q", Installed())
	}
	if _, err := os.Stat(pendingPath()); err == nil {
		t.Fatal("marker cleared after rollback")
	}
}

func TestFeature_Paths_FollowFleetConfig(t *testing.T) {
	d := isolateUpdate(t)
	for _, p := range []string{stampPath(), pendingPath()} {
		if filepath.Dir(p) != d {
			t.Errorf("%s is not next to FLEET_CONFIG", p)
		}
	}
	MarkInstalled("v7.7.7")
	if b, _ := os.ReadFile(filepath.Join(d, "last_update")); strings.TrimSpace(string(b)) != "v7.7.7" {
		t.Fatalf("stamp = %q", b)
	}
}

func TestFeature_Notes_Shapes(t *testing.T) {
	if got := (Release{Body: "  \n\n "}).Notes(); got != "" {
		t.Fatalf("blank body = %q", got)
	}
	body := strings.Repeat("l\n", 12)
	if got := strings.Split((Release{Body: body}).Notes(), "\n"); len(got) != 12 || got[11] != "  l" {
		t.Fatalf("exactly 12 lines are not truncated: %q", got)
	}
	if got := (Release{Body: "a\r\nb\r\n"}).Notes(); got != "  a\n  b" {
		t.Fatalf("CRLF = %q", got)
	}
	if got := (Release{Body: "    indented"}).Notes(); got != "  indented" {
		// leading body whitespace is trimmed as a whole (TrimSpace on the body); a later indented line keeps it
		t.Fatalf("leading = %q", got)
	}
	if got := (Release{Body: "x\n    indented"}).Notes(); got != "  x\n      indented" {
		t.Fatalf("inner indent = %q", got)
	}
}

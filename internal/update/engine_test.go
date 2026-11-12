package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func files(names ...string) []EngineFile {
	var out []EngineFile
	for _, n := range names {
		out = append(out, EngineFile{Name: n})
	}
	return out
}

var realFiles = files("fleet-engine-linux-amd64-cuda12-sm89.xz", "fleet-engine-linux-amd64-cuda13.xz", "fleet-engine-linux-arm64-cuda13.xz")

func TestChooseVariant(t *testing.T) {
	cpu := files("fleet-engine-linux-amd64-cpu.xz")
	for _, c := range []struct {
		name  string
		m     Machine
		files []EngineFile
		want  string // "" = an error
	}{
		{"the card the build was made for", Machine{OS: "linux", Arch: "amd64", GPU: "RTX 4090", Cap: "89", CUDA: 13}, realFiles, "fleet-engine-linux-amd64-cuda12-sm89.xz"},
		{"an exact build needs a driver that knows its CUDA", Machine{OS: "linux", Arch: "amd64", Cap: "89", CUDA: 11}, append(realFiles, cpu...), "fleet-engine-linux-amd64-cpu.xz"},
		{"a card among those of the general build", Machine{OS: "linux", Arch: "amd64", Cap: "120", CUDA: 13}, realFiles, "fleet-engine-linux-amd64-cuda13.xz"},
		{"a general build needs its CUDA too", Machine{OS: "linux", Arch: "amd64", Cap: "120", CUDA: 12}, append(realFiles, cpu...), "fleet-engine-linux-amd64-cpu.xz"},
		{"the arm64 GPU machine", Machine{OS: "linux", Arch: "arm64", Cap: "121", CUDA: 13}, realFiles, "fleet-engine-linux-arm64-cuda13.xz"},
		{"no GPU, a CPU build", Machine{OS: "linux", Arch: "amd64"}, append(realFiles, cpu...), "fleet-engine-linux-amd64-cpu.xz"},
		{"no GPU and no CPU build", Machine{OS: "linux", Arch: "amd64"}, realFiles, ""},
		{"a card the builds do not cover and no CPU build", Machine{OS: "linux", Arch: "amd64", Cap: "75", CUDA: 13}, realFiles, ""},
		{"another system", Machine{OS: "darwin", Arch: "arm64"}, realFiles, ""},
	} {
		f, why, err := ChooseVariant(c.m, c.files)
		if c.want == "" {
			if err == nil {
				t.Errorf("%s: want an error, got %s", c.name, f.Name)
			}
			continue
		}
		if err != nil || f.Name != c.want || why == "" {
			t.Errorf("%s: %q (%s) %v, want %s", c.name, f.Name, why, err, c.want)
		}
	}
}

func TestCompatibleChecksTheKernels(t *testing.T) {
	m := Machine{OS: "linux", Arch: "amd64", Cap: "89"}
	if compatible(EngineInfo{OS: "linux", Arch: "amd64", Backend: "cuda", CudaArchs: "80;86;89;120"}, m) != nil {
		t.Fatal("sm_89 is in the list")
	}
	if compatible(EngineInfo{OS: "linux", Arch: "amd64", Backend: "cuda", CudaArchs: "86"}, m) == nil {
		t.Fatal("no kernel for this card")
	}
	if compatible(EngineInfo{OS: "linux", Arch: "amd64", Backend: "cuda", CudaArchs: "native"}, m) == nil {
		t.Fatal("native is forbidden in release packages")
	}
	if compatible(EngineInfo{OS: "linux", Arch: "arm64", Backend: "cpu"}, m) == nil {
		t.Fatal("wrong architecture")
	}
	if compatible(EngineInfo{OS: "linux", Arch: "amd64", Backend: "cpu"}, m) != nil {
		t.Fatal("a CPU build runs anywhere")
	}
}

// packageServer plays the git server: the file list and the files of one package version.
func packageServer(t *testing.T, name string, body []byte, lieAboutSum bool) *httptest.Server {
	sum := sha256.Sum256(body)
	hexsum := hex.EncodeToString(sum[:])
	if lieAboutSum {
		hexsum = strings.Repeat("0", 64)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token t0k" {
			http.Error(w, "token is required", 401)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/fleet-engine/9.9.9/files"):
			json.NewEncoder(w).Encode([]EngineFile{{Name: name, Size: int64(len(body)), SHA256: hexsum}, {Name: "readme.txt"}})
		case strings.HasSuffix(r.URL.Path, "/fleet-engine/9.9.9/"+name):
			w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func xzOf(t *testing.T, script string) []byte {
	if _, err := exec.LookPath("xz"); err != nil {
		t.Skip("no xz")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "bin")
	os.WriteFile(p, []byte(script), 0o755)
	if out, err := exec.Command("xz", "-k", "-f", p).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	b, _ := os.ReadFile(p + ".xz")
	return b
}

func engineEnv(t *testing.T, srv *httptest.Server) string {
	d := t.TempDir()
	t.Setenv("HOME", d)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(d, "xc"))
	t.Setenv("FLEET_CONFIG", filepath.Join(d, "config.yaml"))
	t.Setenv("FLEET_REPO", "https://example.invalid/fleet/fleet")
	t.Setenv("FLEET_ENGINE_REPO", "https://example.invalid/fleet/fleet")
	t.Setenv("FLEET_ENGINE_API", srv.URL)
	t.Setenv("FLEET_ENGINE_VERSION", "9.9.9")
	t.Setenv("FLEET_TOKEN", "t0k")
	return d
}

const fakeEngine = "#!/bin/sh\n# engine-binary\nif [ \"$1 $2\" = \"ring info\" ]; then echo '{\"engine\":\"llama-ring\",\"protocol\":101,\"os\":\"linux\",\"arch\":\"amd64\",\"backend\":\"cpu\"}'; fi\n"

func TestInstallEngine_DownloadsChecksUnpacksAndReplaces(t *testing.T) {
	name := "fleet-engine-linux-amd64-cpu.xz"
	srv := packageServer(t, name, xzOf(t, fakeEngine), false)
	d := engineEnv(t, srv)
	list, err := EngineFiles(context.Background())
	if err != nil || len(list) != 1 || list[0].Name != name {
		t.Fatalf("only the xz builds are variants: %v %v", list, err)
	}
	exe := filepath.Join(d, "fleet")
	os.WriteFile(exe, []byte("old"), 0o755)
	var said []string
	if err := InstallEngine(context.Background(), list[0], Machine{OS: "linux", Arch: "amd64"}, exe, func(s string) { said = append(said, s) }); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); !strings.Contains(string(b), "engine-binary") {
		t.Fatalf("the binary was not replaced: %q", b)
	}
	if b, _ := os.ReadFile(exe + ".prev"); string(b) != "old" {
		t.Fatalf("the previous binary is kept: %q", b)
	}
	if !strings.HasPrefix(EngineInstalled(), "9.9.9 "+name) {
		t.Fatalf("stamp %q", EngineInstalled())
	}
	if _, err := os.Stat(filepath.Join(d, ".fleet-engine.xz")); err == nil {
		t.Fatal("the download must not stay behind")
	}
	if len(said) < 2 {
		t.Fatalf("progress: %v", said)
	}
}

func TestInstallEngine_RefusesWhatDoesNotCheckOut(t *testing.T) {
	name := "fleet-engine-linux-amd64-cpu.xz"
	good := xzOf(t, fakeEngine)
	cases := map[string]struct {
		body []byte
		lie  bool
		m    Machine
		want string
	}{
		"checksum":      {good, true, Machine{OS: "linux", Arch: "amd64"}, "checksum mismatch"},
		"not an engine": {xzOf(t, "#!/bin/sh\necho hi\n"), false, Machine{OS: "linux", Arch: "amd64"}, "no engine"},
		"wrong machine": {good, false, Machine{OS: "linux", Arch: "arm64"}, "this is linux/arm64"},
	}
	for label, c := range cases {
		srv := packageServer(t, name, c.body, c.lie)
		d := engineEnv(t, srv)
		list, _ := EngineFiles(context.Background())
		exe := filepath.Join(d, "fleet")
		os.WriteFile(exe, []byte("old"), 0o755)
		err := InstallEngine(context.Background(), list[0], c.m, exe, nil)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", label, err)
		}
		if b, _ := os.ReadFile(exe); string(b) != "old" {
			t.Errorf("%s: the running binary must stay as it was: %q", label, b)
		}
	}
}

func TestEngineFiles_WithoutTheTokenScope(t *testing.T) {
	srv := packageServer(t, "x.xz", nil, false)
	engineEnv(t, srv)
	t.Setenv("FLEET_TOKEN", "wrong")
	if _, err := EngineFiles(context.Background()); err == nil || !strings.Contains(err.Error(), "read:package") {
		t.Fatalf("the error must say what is missing: %v", err)
	}
}

// the name a chain deploy starts its stages by is a link to the installed fleet (which carries the stage mode)
func TestEnsureStageLink(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "fleet")
	if err := os.WriteFile(exe, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if msg := ensureStageLink(exe); msg != "" {
		t.Fatal(msg)
	}
	link := filepath.Join(dir, "fleet-engine-stage")
	if to, err := os.Readlink(link); err != nil || to != "fleet" {
		t.Fatalf("link: %q %v", to, err)
	}
	if msg := ensureStageLink(exe); msg != "" { // again: replaced, not an error
		t.Fatal(msg)
	}
	// a standalone build of that name is not overwritten
	_ = os.Remove(link)
	if err := os.WriteFile(link, []byte("standalone"), 0o755); err != nil {
		t.Fatal(err)
	}
	if msg := ensureStageLink(exe); !strings.Contains(msg, "standalone") {
		t.Fatalf("message: %q", msg)
	}
	if b, _ := os.ReadFile(link); string(b) != "standalone" {
		t.Fatal("the standalone file was replaced")
	}
}

// `fleet engine install --version`: only a version number is accepted (it reaches an URL and, with --all, a remote
// shell line), and it is what the package lookups then use
func TestPinEngineVersion(t *testing.T) {
	t.Setenv("FLEET_ENGINE_VERSION", "")
	for _, bad := range []string{"", "0.4", "latest", "0.4.3; rm -rf /", "0.4.3 x", "../0.4.3", "$(id)"} {
		if err := PinEngineVersion(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
	if got := PinnedEngineVersion(); got != EngineVersion {
		t.Errorf("a refused value changed the version: %q", got)
	}
	if err := PinEngineVersion(" 0.9.1 "); err != nil || PinnedEngineVersion() != "0.9.1" {
		t.Errorf("0.9.1: %v, in use %q", err, PinnedEngineVersion())
	}
	if err := PinEngineVersion("1.0.0-rc.1"); err != nil || PinnedEngineVersion() != "1.0.0-rc.1" {
		t.Errorf("a pre-release: %v, in use %q", err, PinnedEngineVersion())
	}
}

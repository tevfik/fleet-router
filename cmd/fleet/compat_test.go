package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// `fleet engine install --all`: older binaries on the other machines must get a line they understand (the version and
// source go as environment variables, which they ignore when unknown), newer plain builds must learn the source.
func TestCompat_EngineRemoteCommand(t *testing.T) {
	src := "https://git.example.com/owner/fleet"
	line := engineRemoteCommand(" --variant cpu", "0.5.6", src)
	for _, want := range []string{"FLEET_ENGINE_VERSION=0.5.6 ", "FLEET_ENGINE_REPO=" + src + " ", "fleet engine install --variant cpu"} {
		if !strings.Contains(line, want) {
			t.Errorf("remote line lacks %q:\n%s", want, line)
		}
	}
	if strings.Index(line, "FLEET_ENGINE_REPO=") > strings.Index(line, "fleet engine install") {
		t.Error("the source must be set before the command runs")
	}
	if l := engineRemoteCommand("", "", ""); strings.Contains(l, "FLEET_ENGINE") {
		t.Errorf("no pin, no source: %s", l)
	}
	if l := engineRemoteCommand("", "", "https://x.example/a/b; rm -rf ~"); strings.Contains(l, "FLEET_ENGINE_REPO") {
		t.Errorf("an unsafe source reached the remote shell: %s", l)
	}
}

// buildFleet builds this package into dir with extra link flags (the real binaries the users get).
func buildFleet(t *testing.T, dir, name, ldflags string) string {
	t.Helper()
	out := filepath.Join(dir, name)
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags", ldflags, "-o", out, ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", name, err, b)
	}
	return out
}

// runFleet runs a built fleet on a fresh machine: an empty HOME (no config, no engine) unless the case installs one.
func runFleet(t *testing.T, bin, home string, env []string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append([]string{"HOME=" + home, "PATH=/usr/bin:/bin", "XDG_CACHE_HOME=" + filepath.Join(home, ".cache"),
		"FLEET_CONFIG=" + filepath.Join(home, "config.yaml"), "FLEET_LISTEN=127.0.0.1:1"}, env...)
	b, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(b), code
}

// The three kinds of binary that will exist, on the three kinds of machine: what each must show.
func TestCompat_BuildFlavoursOnRealBinaries(t *testing.T) {
	if testing.Short() {
		t.Skip("builds fleet twice")
	}
	dir := t.TempDir()
	const src = "https://git.example.com/owner/fleet"
	public := buildFleet(t, dir, "fleet-public", "-s -w") // the public release
	private := buildFleet(t, dir, "fleet-private", "-s -w -X github.com/tevfik/fleet-router/internal/update.EngineRepo="+src+
		" -X github.com/tevfik/fleet-router/internal/update.Repo="+src) // make build with private.env

	plainHome := t.TempDir()
	engineHome := t.TempDir() // a machine after `fleet engine install` (today's machines)
	if err := os.MkdirAll(filepath.Join(engineHome, ".local", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(private, filepath.Join(engineHome, ".local", "bin", "fleet-engine-stage")); err != nil {
		t.Fatal(err)
	}

	// public binary, new machine: the router only; the runner and `fleet engine` do not exist
	out, _ := runFleet(t, public, plainHome, nil, "help")
	if strings.Contains(out, "fleet deploy") || strings.Contains(out, "fleet engine") || !strings.Contains(out, "fleet serve") {
		t.Errorf("public help on a plain machine:\n%s", out)
	}
	if out, code := runFleet(t, public, plainHome, nil, "engine", "status"); code != 2 || !strings.Contains(out, "unknown command: engine") {
		t.Errorf("public `fleet engine` on a plain machine: code %d\n%s", code, out)
	}
	if out, code := runFleet(t, public, plainHome, nil, "deploy"); code != 2 || !strings.Contains(out, "needs fleet-engine") {
		t.Errorf("public `fleet deploy`: code %d\n%s", code, out)
	}
	// ... but the owner can still put the engine there with one variable
	if out, code := runFleet(t, public, plainHome, []string{"FLEET_ENGINE_REPO=" + src}, "engine", "source"); code != 0 || strings.TrimSpace(out) != src {
		t.Errorf("public binary + FLEET_ENGINE_REPO: code %d %q", code, out)
	}

	// update source: the public build (until the public repository is set) has none and says so; FLEET_REPO gives one;
	// the private build updates from the private repository
	if out, code := runFleet(t, public, plainHome, []string{"FLEET_REPO="}, "update", "--check"); code == 0 || !strings.Contains(out, "no update source") {
		t.Errorf("public `fleet update --check` without a source: code %d\n%s", code, out)
	}
	const gh = "https://github.com/someone/fleet"
	if out, code := runFleet(t, public, plainHome, []string{"FLEET_REPO=" + gh}, "update", "--source"); code != 0 || strings.TrimSpace(out) != gh {
		t.Errorf("public binary + FLEET_REPO: code %d %q", code, out)
	}
	if out, code := runFleet(t, private, plainHome, []string{"FLEET_REPO="}, "update", "--source"); code != 0 || strings.TrimSpace(out) != src {
		t.Errorf("private build: update source code %d %q", code, out)
	}

	// hub: the public build has no default hub and says so; a hub named on the command line is used
	if out, code := runFleet(t, public, plainHome, nil, "stream", "connect", "node", "5000"); code == 0 || !strings.Contains(out, "no hub") {
		t.Errorf("public `fleet stream connect` without a hub: code %d\n%s", code, out)
	}
	if out, _ := runFleet(t, public, plainHome, nil, "stream"); !strings.Contains(out, "the build's default hub, if any") {
		t.Errorf("public `fleet stream` help:\n%s", out)
	}

	// private build, new machine: `fleet engine` is there (it knows the source), the runner not yet (no engine)
	if out, code := runFleet(t, private, plainHome, nil, "engine", "source"); code != 0 || strings.TrimSpace(out) != src {
		t.Errorf("private build: engine source code %d %q", code, out)
	}
	if out, _ := runFleet(t, private, plainHome, nil, "help"); strings.Contains(out, "fleet deploy") {
		t.Errorf("private build without the engine shows the runner:\n%s", out)
	}

	// any build on a machine with the engine installed (today's machines): everything as before
	for _, bin := range []string{public, private} {
		out, _ := runFleet(t, bin, engineHome, nil, "help")
		for _, c := range []string{"fleet deploy", "fleet plan", "fleet run [", "fleet engine"} {
			if !strings.Contains(out, c) {
				t.Errorf("%s on an engine machine lacks %q", filepath.Base(bin), c)
			}
		}
		if out, code := runFleet(t, bin, engineHome, nil, "run", "list"); strings.Contains(out, "needs fleet-engine") {
			t.Errorf("%s: `fleet run` refused on an engine machine (code %d): %s", filepath.Base(bin), code, out)
		}
	}
}

// The private build wiring: make passes the source from private.env, and an engine build checks it got it.
func TestCompat_PrivateBuildWiring(t *testing.T) {
	b, err := os.ReadFile("../../private.env")
	if err != nil {
		t.Skip("no private.env: this is the public fleet")
	}
	want, wantUpd, wantHub := "", "", ""
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "FLEET_ENGINE_REPO_BUILTIN="); ok {
			want = v
		}
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "FLEET_UPDATE_REPO_BUILTIN="); ok {
			wantUpd = v
		}
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "FLEET_HUB_URL_BUILTIN="); ok {
			wantHub = v
		}
	}
	if want == "" || wantUpd == "" || wantHub == "" {
		t.Fatal("private.env lacks FLEET_ENGINE_REPO_BUILTIN, FLEET_UPDATE_REPO_BUILTIN or FLEET_HUB_URL_BUILTIN")
	}
	mk := exec.Command("make", "-n", "build", "build-amd64", "build-arm64")
	mk.Dir = "../.."
	out, err := mk.CombinedOutput()
	if err != nil {
		t.Fatalf("make -n: %v\n%s", err, out)
	}
	for _, l := range strings.Split(string(out), "\n") {
		if strings.Contains(l, "go build") && !strings.Contains(l, "internal/update.EngineRepo="+want) {
			t.Errorf("a release build has no engine source:\n%s", l)
		}
		// without it today's machines would stop updating (or update from the public repository)
		if strings.Contains(l, "go build") && !strings.Contains(l, "internal/update.Repo="+wantUpd) {
			t.Errorf("a release build has no update source:\n%s", l)
		}
		// without it today's nodes would lose the hub they join and deploy through
		if strings.Contains(l, "go build") && !strings.Contains(l, "internal/config.DefaultHubURL="+wantHub) {
			t.Errorf("a release build has no default hub:\n%s", l)
		}
	}
	sh, err := os.ReadFile("../../scripts/build-engine.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`. "$ROOT/private.env"`, "internal/update.EngineRepo=", "engine source", "internal/update.Repo=", "update --source", "internal/config.DefaultHubURL="} {
		if !strings.Contains(string(sh), want) {
			t.Errorf("scripts/build-engine.sh lacks %q", want)
		}
	}
}

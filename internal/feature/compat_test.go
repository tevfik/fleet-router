package feature

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tevfik/fleet-router/internal/deploy"
)

// machine sets up a HOME and PATH with nothing on them but what the case asks for.
func machine(t *testing.T, engineInstalled bool) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir()) // no fleet-engine on PATH either
	t.Setenv(EnvRunner, "")
	if engineInstalled { // what `fleet engine install` leaves: ~/.local/bin/fleet-engine-stage (a link to fleet)
		bin := filepath.Join(home, ".local", "bin")
		if err := os.MkdirAll(bin, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bin, "fleet-engine-stage"), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// The machines that run fleet today all had `fleet engine install`: they must keep the whole fleet (runner included).
func TestCompat_InstalledEngineKeepsTheRunner(t *testing.T) {
	machine(t, true)
	if !detect() {
		t.Fatal("a machine with fleet-engine-stage installed lost the runner side")
	}
	t.Setenv(EnvRunner, "0") // ...unless asked to show the router-only fleet
	if detect() {
		t.Fatal("FLEET_RUNNER=0 did not hide the runner on an engine machine")
	}
}

// A new machine with the plain (public) fleet: only the router.
func TestCompat_PlainMachineIsRouterOnly(t *testing.T) {
	machine(t, false)
	if detect() {
		t.Fatal("a machine without fleet-engine shows the runner side")
	}
	t.Setenv(EnvRunner, "1")
	if !detect() {
		t.Fatal("FLEET_RUNNER=1 did not show the runner")
	}
}

// A build with a stage module linked (make ext, an adapter module) has a runner without fleet-engine.
// Last in the file: the module registry is global and cannot be emptied again.
func TestCompat_StageModuleBuildHasTheRunner(t *testing.T) {
	machine(t, false)
	deploy.RegisterStageModule(deploy.StageModule{Name: "compat-test", Doc: "test module"})
	if !detect() {
		t.Fatal("a build with a linked stage module lost the runner side")
	}
}

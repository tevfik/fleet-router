package setup

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeHost replaces systemctl/loginctl and records the calls.
func fakeHost(t *testing.T, linger string, lingerErr error) *[]string {
	t.Helper()
	var calls []string
	oldS, oldL := SystemctlUser, LoginctlLinger
	SystemctlUser = func(args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		return nil, nil
	}
	LoginctlLinger = func() (string, error) { return linger, lingerErr }
	t.Cleanup(func() { SystemctlUser, LoginctlLinger = oldS, oldL })
	return &calls
}

func TestProfileAutostartOnWritesEnablesAndNeverStarts(t *testing.T) {
	h := tempHome(t)
	t.Setenv("FLEET_CONFIG", filepath.Join(h, "cfg.yaml"))
	t.Setenv("FLEET_PROFILES", "")
	t.Setenv("USER", "tester")
	calls := fakeHost(t, "no", nil)

	out, err := ProfileAutostartOn("/opt/bin/fleet", "qwen38-64k")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(h, ".config", "systemd", "user", "fleet-profile-qwen38-64k.service")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("unit not written: %v", err)
	}
	unit := string(b)
	for _, want := range []string{
		"Type=oneshot", "RemainAfterExit=yes", "After=fleet.service", "Wants=fleet.service",
		"ExecStart=/opt/bin/fleet profile up qwen38-64k", "ExecStop=/opt/bin/fleet profile down qwen38-64k",
		"Environment=FLEET_CONFIG=" + filepath.Join(h, "cfg.yaml"), "WantedBy=default.target", "managed by fleet",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit misses %q:\n%s", want, unit)
		}
	}
	if strings.Contains(unit, "FLEET_PROFILES") {
		t.Error("an empty FLEET_PROFILES must not be written")
	}
	// enabled, never started (a profile that is already up must not be deployed twice); linger never changed
	if got := strings.Join(*calls, "|"); got != "daemon-reload|enable fleet-profile-qwen38-64k.service" {
		t.Fatalf("systemctl calls = %q", got)
	}
	all := strings.Join(out, "\n")
	if !strings.Contains(all, "loginctl enable-linger tester") || !strings.Contains(all, "Linger=no") {
		t.Errorf("linger hint missing:\n%s", all)
	}
	if !ProfileAutostartEnabled("qwen38-64k") || strings.Join(AutostartProfiles(), ",") != "qwen38-64k" {
		t.Errorf("enabled=%v list=%v", ProfileAutostartEnabled("qwen38-64k"), AutostartProfiles())
	}
}

func TestProfileAutostartLingerOnHasNoHint(t *testing.T) {
	tempHome(t)
	fakeHost(t, "yes", nil)
	out, err := ProfileAutostartOn("/x/fleet", "p")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(out, "\n"), "linger") {
		t.Errorf("no hint expected with Linger=yes: %v", out)
	}
	fakeHost(t, "", fmt.Errorf("no loginctl"))
	if h := LingerHint(); !strings.Contains(h, "could not read") || !strings.Contains(h, "enable-linger") {
		t.Errorf("hint on error = %q", h)
	}
}

func TestProfileAutostartOffRemovesUnitOnly(t *testing.T) {
	tempHome(t)
	calls := fakeHost(t, "yes", nil)
	if out, err := ProfileAutostartOff("ghost"); err != nil || !strings.Contains(out[0], "no autostart unit") {
		t.Fatalf("off without unit: %v %v", out, err)
	}
	if len(*calls) != 0 {
		t.Fatalf("off without unit called systemctl: %v", *calls)
	}
	if _, err := ProfileAutostartOn("/x/fleet", "p"); err != nil {
		t.Fatal(err)
	}
	*calls = nil
	out, err := ProfileAutostartOff("p")
	if err != nil {
		t.Fatal(err)
	}
	if ProfileAutostartEnabled("p") {
		t.Fatal("unit file still there")
	}
	if got := strings.Join(*calls, "|"); got != "disable fleet-profile-p.service|daemon-reload" {
		t.Fatalf("systemctl calls = %q", got)
	}
	if !strings.Contains(out[0], "keeps running") {
		t.Errorf("off must say the model keeps running: %v", out)
	}
}

func TestProfileAutostartRefusesDanglingUnitDir(t *testing.T) {
	h := tempHome(t)
	fakeHost(t, "yes", nil)
	if err := os.MkdirAll(filepath.Join(h, ".config", "systemd"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(h, ".config", "systemd", "user")
	if err := os.Symlink(filepath.Join(h, "gone"), link); err != nil {
		t.Fatal(err)
	}
	if _, err := ProfileAutostartOn("/x/fleet", "p"); err == nil || !strings.Contains(err.Error(), "dangling symlink") {
		t.Fatalf("err = %v", err)
	}
	if tgt, _ := os.Readlink(link); tgt != filepath.Join(h, "gone") {
		t.Fatal("the symlink was changed")
	}
}

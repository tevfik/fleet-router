package setup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tevfik/fleet-router/internal/config"
)

func tempHome(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(h, ".config"))
	return h
}

func backups(t *testing.T, path string) []string {
	t.Helper()
	m, _ := filepath.Glob(path + ".bak.*")
	return m
}

func TestFeature_Install_BinaryCopiedIntoDir(t *testing.T) {
	tempHome(t)
	exe, err := Exe()
	if err != nil || !filepath.IsAbs(exe) {
		t.Fatalf("Exe = %q, %v", exe, err)
	}
	dir := filepath.Join(t.TempDir(), "bin")
	dst, err := InstallBinary(dir)
	if err != nil {
		t.Fatal(err)
	}
	if dst != filepath.Join(dir, "fleet") {
		t.Fatalf("dst = %s", dst)
	}
	a, _ := os.Stat(exe)
	b, err := os.Stat(dst)
	if err != nil || a.Size() != b.Size() || b.Mode().Perm()&0o100 == 0 {
		t.Fatalf("copy wrong: %v %v", b, err)
	}
	if _, err := os.Stat(dst + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temporary file left behind")
	}
	// default dir is ~/.local/bin
	dst, err = InstallBinary("")
	if err != nil || dst != filepath.Join(os.Getenv("HOME"), ".local", "bin", "fleet") {
		t.Fatalf("default install = %s, %v", dst, err)
	}
}

func TestFeature_OpenCode_MergesProviderWithBackup(t *testing.T) {
	h := tempHome(t)
	path := filepath.Join(h, ".config", "opencode", "opencode.json")
	// no file yet: created with only the provider
	p, err := OpenCode(map[string]any{"fleet": map[string]any{"name": "fleet"}})
	if err != nil || p != path {
		t.Fatalf("OpenCode = %s, %v", p, err)
	}
	if len(backups(t, path)) != 0 {
		t.Fatal("nothing to back up on a fresh file")
	}
	// existing file: other keys and providers kept, fleet replaced, the old file backed up
	os.WriteFile(path, []byte(`{"theme":"dark","providers":{"other":{"x":1},"fleet":{"old":true}}}`), 0o600)
	if _, err := OpenCode(map[string]any{"fleet": map[string]any{"name": "new"}}); err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	b, _ := os.ReadFile(path)
	if err := json.Unmarshal(b, &root); err != nil {
		t.Fatal(err)
	}
	provs := root["providers"].(map[string]any)
	if root["theme"] != "dark" || provs["other"] == nil || provs["fleet"].(map[string]any)["name"] != "new" || provs["fleet"].(map[string]any)["old"] != nil {
		t.Fatalf("merged = %s", b)
	}
	if len(backups(t, path)) != 1 {
		t.Fatalf("backups = %v", backups(t, path))
	}
}

func TestFeature_OpenCode_JSONCRefused(t *testing.T) {
	h := tempHome(t)
	path := filepath.Join(h, ".config", "opencode", "opencode.json")
	os.MkdirAll(filepath.Dir(path), 0o755)
	orig := "{\n  // a comment\n  \"theme\": \"dark\"\n}\n"
	os.WriteFile(path, []byte(orig), 0o600)
	if _, err := OpenCode(map[string]any{"fleet": 1}); err == nil || !strings.Contains(err.Error(), "JSONC") {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != orig {
		t.Fatal("a file that cannot be parsed must not be touched")
	}
}

func TestFeature_Guidance_WriteOwnedBackupRules(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "fleet-x.md")
	if st, err := writeOwned(p, "v1 managed by fleet"); err != nil || st != "written" {
		t.Fatalf("first write = %s, %v", st, err)
	}
	if st, _ := writeOwned(p, "v1 managed by fleet"); st != "unchanged" {
		t.Fatalf("same content = %s", st)
	}
	// fleet's own older content: replaced without a backup
	if st, _ := writeOwned(p, "v2 managed by fleet"); st != "written" || len(backups(t, p)) != 0 {
		t.Fatalf("managed rewrite st=%s backups=%v", st, backups(t, p))
	}
	// hand-written content: backed up first
	os.WriteFile(p, []byte("my own notes"), 0o644)
	if st, _ := writeOwned(p, "v3 managed by fleet"); st != "written" {
		t.Fatal(st)
	}
	bk := backups(t, p)
	if len(bk) != 1 {
		t.Fatalf("backups = %v", bk)
	}
	if b, _ := os.ReadFile(bk[0]); string(b) != "my own notes" {
		t.Fatalf("backup = %q", b)
	}
}

func TestFeature_Guidance_MissingRolesSkippedAndSymlinkNoted(t *testing.T) {
	h := tempHome(t)
	real := filepath.Join(h, "dotfiles", "agents")
	os.MkdirAll(real, 0o755)
	os.MkdirAll(filepath.Join(h, ".config", "opencode"), 0o755)
	if err := os.Symlink(real, filepath.Join(h, ".config", "opencode", "agents")); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Roles: map[string]config.Role{"coder": {Preferred: "a"}}}
	out, err := OpenCodeGuidance(cfg)
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(out, "\n")
	for _, want := range []string{"skipped: no such role → scout", "skipped: no such role → hunter", "symlink → " + real} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in\n%s", want, all)
		}
	}
	if strings.Contains(all, "no such role → coder") {
		t.Error("present role reported as skipped")
	}
	b, err := os.ReadFile(filepath.Join(real, "fleet-coder.md"))
	if err != nil || !strings.Contains(string(b), DefaultUse["coder"]) {
		t.Fatalf("coder agent (default use) not written through the symlink: %v", err)
	}
	if _, err := os.Stat(filepath.Join(real, "fleet-scout.md")); !os.IsNotExist(err) {
		t.Fatal("agent for a missing role written")
	}
	// running again changes nothing
	out, _ = OpenCodeGuidance(cfg)
	for _, l := range out {
		if strings.HasPrefix(l, "written:") {
			t.Fatalf("second run rewrote %s", l)
		}
	}
}

func TestFeature_Guidance_RenderUnknownAsset(t *testing.T) {
	if _, err := render("does-not-exist.md", nil); err == nil {
		t.Fatal("missing asset must fail")
	}
	if s, err := render("claude_cmd_review.md", nil); err != nil || s == "" {
		t.Fatalf("plain asset: %q %v", s, err)
	}
}

func TestFeature_Setup_RefusalsWithoutSideEffects(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no claude, no systemctl, no getent
	if _, err := Claude("/x/fleet", ""); err == nil || !strings.Contains(err.Error(), "not in PATH") {
		t.Fatalf("Claude err = %v", err)
	}
	t.Setenv("SUDO_USER", "")
	t.Setenv("USER", "root")
	if _, _, err := SystemUser(); err == nil || !strings.Contains(err.Error(), "sudo fleet setup service --system") {
		t.Fatalf("SystemUser as root: %v", err)
	}
	t.Setenv("USER", "")
	if _, _, err := SystemUser(); err == nil {
		t.Fatal("no user must be refused")
	}
	t.Setenv("SUDO_USER", "someone")
	if _, _, err := SystemUser(); err == nil || !strings.Contains(err.Error(), "getent") {
		t.Fatalf("no getent in PATH: %v", err)
	}
	if os.Geteuid() != 0 {
		if _, err := SystemServiceInstall("/usr/local/bin/fleet"); err == nil || !strings.Contains(err.Error(), "needs root") {
			t.Fatalf("SystemServiceInstall as non-root: %v", err)
		}
	}
}

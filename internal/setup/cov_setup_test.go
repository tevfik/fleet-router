package setup

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallBinary_Errors(t *testing.T) {
	// Test mkdir error on bad path
	tmpDir := t.TempDir()
	blocker := filepath.Join(tmpDir, "file")
	if err := os.WriteFile(blocker, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := InstallBinary(filepath.Join(blocker, "invalid"))
	if err == nil {
		t.Error("expected error when MkdirAll fails on InstallBinary")
	}

	// Test read-only dir for file creation
	if os.Geteuid() == 0 {
		return // root writes into a 0500 directory: the read-only case cannot fail
	}
	roDir := filepath.Join(tmpDir, "ro_dir")
	if err := os.Mkdir(roDir, 0500); err != nil {
		t.Fatal(err)
	}
	_, err = InstallBinary(roDir)
	if err == nil {
		t.Error("expected error writing to read-only dir")
	}
}

func TestClaude_WithMockScript(t *testing.T) {
	tmpDir := t.TempDir()
	claudeScript := filepath.Join(tmpDir, "claude")
	content := "#!/bin/sh\necho \"claude args: $@\"\nexit 0\n"
	if err := os.WriteFile(claudeScript, []byte(content), 0755); err != nil {
		t.Fatal(err)
	}

	// Prepend tmpDir to PATH so our mock claude is found
	origPath := os.Getenv("PATH")
	t.Setenv("PATH", tmpDir+":"+origPath)

	// 1. With listen != ""
	out, err := Claude("/opt/fleet", "127.0.0.1:8080")
	if err != nil {
		t.Fatalf("Claude failed: %v", err)
	}
	if !strings.Contains(out, "FLEET_LISTEN=127.0.0.1:8080") || !strings.Contains(out, "mcp add") {
		t.Errorf("unexpected output: %s", out)
	}

	// 2. Without listen
	out, err = Claude("/opt/fleet", "")
	if err != nil {
		t.Fatalf("Claude without listen failed: %v", err)
	}
	if strings.Contains(out, "FLEET_LISTEN") {
		t.Errorf("expected no FLEET_LISTEN, got %s", out)
	}
}

func TestServiceInstall_WithMockSystemctl(t *testing.T) {
	h := tempHome(t)
	tmpBin := t.TempDir()
	systemctlScript := filepath.Join(tmpBin, "systemctl")
	scriptContent := "#!/bin/sh\necho \"systemctl user $@\"\nexit 0\n"
	if err := os.WriteFile(systemctlScript, []byte(scriptContent), 0755); err != nil {
		t.Fatal(err)
	}

	origPath := os.Getenv("PATH")
	t.Setenv("PATH", tmpBin+":"+origPath)

	servicePath, err := ServiceInstall("/opt/fleet")
	if err != nil {
		t.Fatalf("ServiceInstall failed: %v", err)
	}

	expectedPath := filepath.Join(h, ".config", "systemd", "user", "fleet.service")
	if servicePath != expectedPath {
		t.Errorf("expected service path %s, got %s", expectedPath, servicePath)
	}

	content, err := os.ReadFile(servicePath)
	if err != nil {
		t.Fatalf("failed to read service file: %v", err)
	}
	if !strings.Contains(string(content), "ExecStart=/opt/fleet serve") {
		t.Errorf("unexpected service content: %s", string(content))
	}

	// Test failing systemctl
	failScript := "#!/bin/sh\necho \"mock failure\"\nexit 1\n"
	if err := os.WriteFile(systemctlScript, []byte(failScript), 0755); err != nil {
		t.Fatal(err)
	}
	_, err = ServiceInstall("/opt/fleet")
	if err == nil || !strings.Contains(err.Error(), "mock failure") {
		t.Errorf("expected systemctl error, got %v", err)
	}

	// Test mkdir error on ServiceInstall
	t.Setenv("HOME", filepath.Join(tmpBin, "file-home"))
	if err := os.WriteFile(filepath.Join(tmpBin, "file-home"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err = ServiceInstall("/opt/fleet")
	if err == nil {
		t.Error("expected error when MkdirAll fails in ServiceInstall")
	}
}

func TestSystemUser_WithMockGetent(t *testing.T) {
	tmpBin := t.TempDir()
	getentScript := filepath.Join(tmpBin, "getent")
	origPath := os.Getenv("PATH")
	t.Setenv("PATH", tmpBin+":"+origPath)
	t.Setenv("SUDO_USER", "mockuser")

	// 1. Success case: standard passwd line
	content := "#!/bin/sh\necho \"mockuser:x:1001:1001::/home/mockuser:/bin/bash\"\nexit 0\n"
	if err := os.WriteFile(getentScript, []byte(content), 0755); err != nil {
		t.Fatal(err)
	}
	user, homeDir, err := SystemUser()
	if err != nil {
		t.Fatalf("SystemUser failed: %v", err)
	}
	if user != "mockuser" || homeDir != "/home/mockuser" {
		t.Errorf("unexpected user %s or homeDir %s", user, homeDir)
	}

	// 2. Malformed passwd entry (< 6 fields)
	contentBad := "#!/bin/sh\necho \"mockuser:x:1001\"\nexit 0\n"
	if err := os.WriteFile(getentScript, []byte(contentBad), 0755); err != nil {
		t.Fatal(err)
	}
	_, _, err = SystemUser()
	if err == nil || !strings.Contains(err.Error(), "unexpected passwd entry") {
		t.Errorf("expected malformed passwd entry error, got %v", err)
	}
}

func TestUserUnitDir_DefaultFallback(t *testing.T) {
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("XDG_CONFIG_HOME", "")

	expected := filepath.Join(h, ".config", "systemd", "user")
	if dir := UserUnitDir(); dir != expected {
		t.Errorf("expected UserUnitDir %s, got %s", expected, dir)
	}
}

func TestSymlinkIssue_ErrorFormatting(t *testing.T) {
	dirIssue := &SymlinkIssue{
		Path:    "/path/to/dir",
		Target:  "/target/dir",
		Reason:  "target does not exist",
		WantDir: true,
	}
	msgDir := dirIssue.Error()
	if !strings.Contains(msgDir, "mkdir -p /path/to/dir") {
		t.Errorf("expected dir fix instruction in error, got: %s", msgDir)
	}

	fileIssue := &SymlinkIssue{
		Path:    "/path/to/file",
		Target:  "/target/file",
		Reason:  "broken file",
		WantDir: false,
	}
	msgFile := fileIssue.Error()
	if !strings.Contains(msgFile, "ln -sfn <valid-file>") || strings.Contains(msgFile, "mkdir") {
		t.Errorf("expected file fix instruction without mkdir in error, got: %s", msgFile)
	}
}

func TestCheckFileSymlink(t *testing.T) {
	tmpDir := t.TempDir()

	// 1. Regular file (not a symlink)
	regFile := filepath.Join(tmpDir, "regular.txt")
	if err := os.WriteFile(regFile, []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	if iss := checkFileSymlink(regFile); iss != nil {
		t.Errorf("expected nil issue for regular file, got %+v", iss)
	}

	// 2. Valid file symlink
	validLink := filepath.Join(tmpDir, "valid-link.txt")
	if err := os.Symlink(regFile, validLink); err != nil {
		t.Fatal(err)
	}
	if iss := checkFileSymlink(validLink); iss != nil {
		t.Errorf("expected nil issue for valid symlink, got %+v", iss)
	}

	// 3. Dangling file symlink
	danglingLink := filepath.Join(tmpDir, "dangling-link.txt")
	if err := os.Symlink(filepath.Join(tmpDir, "nonexistent.txt"), danglingLink); err != nil {
		t.Fatal(err)
	}
	iss := checkFileSymlink(danglingLink)
	if iss == nil || iss.WantDir {
		t.Errorf("expected dangling file issue with WantDir=false, got %+v", iss)
	}
}

func TestCheckDirSymlinks_PointingToFile(t *testing.T) {
	tmpDir := t.TempDir()
	regFile := filepath.Join(tmpDir, "regular.txt")
	if err := os.WriteFile(regFile, []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}

	// Symlink pointing to a file when dir expected
	fileLink := filepath.Join(tmpDir, "dir-link")
	if err := os.Symlink(regFile, fileLink); err != nil {
		t.Fatal(err)
	}

	iss := checkDirSymlinks(fileLink)
	if iss == nil || !strings.Contains(iss.Reason, "points to a file") {
		t.Errorf("expected 'points to a file' error, got %+v", iss)
	}
}

func TestClaudeGuidance_DanglingDirSymlink(t *testing.T) {
	h := tempHome(t)
	// Make ~/.claude/skills a broken symlink
	claudePath := filepath.Join(h, ".claude")
	if err := os.MkdirAll(claudePath, 0755); err != nil {
		t.Fatal(err)
	}
	badSkills := filepath.Join(claudePath, "skills")
	if err := os.Symlink(filepath.Join(h, "nonexistent-skills"), badSkills); err != nil {
		t.Fatal(err)
	}

	cfg := qaConfig()
	_, err := ClaudeGuidance(cfg)
	if err == nil || !strings.Contains(err.Error(), "dangling symlink") {
		t.Errorf("expected dangling symlink error in ClaudeGuidance, got %v", err)
	}
}

func TestOpenCode_SymlinkIssuesAndReadErrors(t *testing.T) {
	h := tempHome(t)
	cfgDir := filepath.Join(h, ".config", "opencode")
	if err := os.MkdirAll(cfgDir, 0755); err != nil {
		t.Fatal(err)
	}
	cfgFile := filepath.Join(cfgDir, "opencode.json")

	// 1. checkFileSymlink triggered when opencode.json is dangling
	if err := os.Symlink(filepath.Join(h, "nonexistent.json"), cfgFile); err != nil {
		t.Fatal(err)
	}
	_, err := OpenCode(map[string]any{"fleet": "val"})
	if err == nil || !strings.Contains(err.Error(), "dangling symlink") {
		t.Errorf("expected dangling symlink error on OpenCode, got %v", err)
	}
	_ = os.Remove(cfgFile)

	// 2. checkDirSymlinks triggered when directory is dangling
	badDir := filepath.Join(h, "bad_cfg")
	if err := os.Symlink(filepath.Join(h, "nonexistent_dir"), badDir); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", badDir)
	_, err = OpenCode(map[string]any{"fleet": "val"})
	if err == nil || !strings.Contains(err.Error(), "dangling symlink") {
		t.Errorf("expected dangling dir symlink error on OpenCode, got %v", err)
	}
}

func TestProfileAutostart_ErrorsWithMockSystemctl(t *testing.T) {
	tempHome(t)
	oldSystemctl := SystemctlUser
	defer func() { SystemctlUser = oldSystemctl }()

	// 1. SystemctlUser error on ProfileAutostartOn
	SystemctlUser = func(args ...string) ([]byte, error) {
		return []byte("mock systemctl error"), errors.New("exit 1")
	}
	_, err := ProfileAutostartOn("/opt/fleet", "p1")
	if err == nil || !strings.Contains(err.Error(), "mock systemctl error") {
		t.Errorf("expected systemctl error on autostart on, got %v", err)
	}

	// 2. ProfileAutostartOff daemon-reload failure
	// First write a mock unit file so Off finds it
	unitPath := ProfileUnitPath("p2")
	_ = os.MkdirAll(filepath.Dir(unitPath), 0755)
	_ = os.WriteFile(unitPath, []byte("unit"), 0644)

	SystemctlUser = func(args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "daemon-reload" {
			return []byte("reload failed"), errors.New("reload failed")
		}
		return nil, nil
	}
	out, err := ProfileAutostartOff("p2")
	if err == nil || !strings.Contains(err.Error(), "reload failed") {
		t.Errorf("expected reload error on autostart off, got %v (out: %v)", err, out)
	}
}

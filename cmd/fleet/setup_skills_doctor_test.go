package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tevfik/fleet-router/internal/config"
)

func TestSetupSkillsAndDoctorReportDanglingSymlinks(t *testing.T) {
	dir := isolate(t)
	cfg := config.Default()
	cfg.Endpoints = []config.Endpoint{{Name: "loc-tiny", URL: "http://127.0.0.1:1/v1", Model: "tiny"}}
	cfg.Roles = map[string]config.Role{
		"scout": {Preferred: "loc-tiny"},
		"coder": {Preferred: "loc-tiny"},
	}
	if err := cfg.Save(config.Path()); err != nil {
		t.Fatal(err)
	}

	ocDir := filepath.Join(dir, ".config", "opencode")
	if err := os.MkdirAll(ocDir, 0o755); err != nil {
		t.Fatal(err)
	}
	agentsLink := filepath.Join(ocDir, "agents")
	cmdsLink := filepath.Join(ocDir, "commands")
	missingAgents := filepath.Join(dir, "gone", "agents")
	missingCmds := filepath.Join(dir, "gone", "commands")
	if err := os.Symlink(missingAgents, agentsLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(missingCmds, cmdsLink); err != nil {
		t.Fatal(err)
	}

	// 1) `fleet setup skills` still writes healthy ~/.claude files, diagnoses both dangling symlinks, and touches neither
	stdout, _, err := run(t, func() error { return cmdSetup([]string{"skills"}) })
	if err == nil {
		t.Fatal("cmdSetup skills should fail when opencode symlinks are dangling")
	}
	for _, want := range []string{"dangling symlink", agentsLink, missingAgents, cmdsLink, missingCmds, "ln -sfn"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("cmdSetup skills error missing %q:\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), "file exists") {
		t.Errorf("cmdSetup skills leaked raw mkdir 'file exists' error: %v", err)
	}
	if !strings.Contains(stdout, filepath.Join(dir, ".claude", "skills", "fleet", "SKILL.md")) {
		t.Errorf("expected healthy Claude skill to be written before reporting opencode symlink error; stdout:\n%s", stdout)
	}
	for _, link := range []string{agentsLink, cmdsLink} {
		if fi, lerr := os.Lstat(link); lerr != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("symlink %s was modified without user consent: %v", link, lerr)
		}
	}

	// 2) `fleet doctor` reports both dangling symlinks with fix guidance
	docOut := runOK(t, cmdDoctor)
	for _, want := range []string{
		"✗ dangling symlink " + agentsLink + " → " + missingAgents,
		"✗ dangling symlink " + cmdsLink + " → " + missingCmds,
		"ln -sfn",
	} {
		if !strings.Contains(docOut, want) {
			t.Errorf("cmdDoctor output missing %q:\n%s", want, docOut)
		}
	}
}

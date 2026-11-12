package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tevfik/fleet-router/internal/config"
)

func qaConfig() *config.Config {
	ep := func(n string) config.Endpoint {
		return config.Endpoint{Name: n, Node: "dino", Model: n, URL: "http://x/v1", Ctx: 32768, TPS: 50}
	}
	return &config.Config{Listen: "127.0.0.1:4711",
		Endpoints: []config.Endpoint{ep("a"), ep("b")},
		Roles: map[string]config.Role{
			"scout": {Preferred: "a"}, "architect": {Preferred: "b"}, "coder": {Preferred: "a"}, "verifier": {Preferred: "a"},
			"tester": {Preferred: "a"}, "validator": {Preferred: "b"}, "hunter": {Preferred: "b", Use: "custom hunter text"},
		}}
}

func TestGuidanceCoversQARoles(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := qaConfig()
	if _, err := ClaudeGuidance(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenCodeGuidance(cfg); err != nil {
		t.Fatal(err)
	}
	h := os.Getenv("HOME")
	skill, _ := os.ReadFile(filepath.Join(h, ".claude", "skills", "fleet", "SKILL.md"))
	for _, want := range []string{"`tester`", "`validator`", "`hunter`", "/fleet-qa", "custom hunter text"} {
		if !strings.Contains(string(skill), want) {
			t.Errorf("skill misses %q", want)
		}
	}
	for _, f := range []string{
		".claude/commands/fleet-qa.md", ".config/opencode/commands/fleet-qa.md",
		".config/opencode/agents/fleet-tester.md", ".config/opencode/agents/fleet-validator.md", ".config/opencode/agents/fleet-hunter.md",
	} {
		b, err := os.ReadFile(filepath.Join(h, f))
		if err != nil || !strings.Contains(string(b), "managed by fleet") {
			t.Errorf("%s not written: %v", f, err)
		}
	}
	// the tester may only edit test files; the validator and hunter never edit
	tester, _ := os.ReadFile(filepath.Join(h, ".config/opencode/agents/fleet-tester.md"))
	if !strings.Contains(string(tester), `resource: "*_test.go", effect: allow`) || !strings.Contains(string(tester), `{action: edit, resource: "*", effect: deny}`) {
		t.Error("tester permissions must allow test files and deny other edits")
	}
	for _, r := range []string{"validator", "hunter"} {
		b, _ := os.ReadFile(filepath.Join(h, ".config/opencode/agents/fleet-"+r+".md"))
		if strings.Contains(string(b), "{action: edit, resource: \"*\", effect: allow}") || !strings.Contains(string(b), `{action: edit, resource: "*", effect: deny}`) {
			t.Errorf("%s must not be able to edit", r)
		}
	}
}

func TestDanglingSymlinksReportedAndNeverModified(t *testing.T) {
	h := t.TempDir()
	t.Setenv("HOME", h)
	ocDir := filepath.Join(h, ".config", "opencode")
	if err := os.MkdirAll(ocDir, 0o755); err != nil {
		t.Fatal(err)
	}
	agentsLink := filepath.Join(ocDir, "agents")
	cmdsLink := filepath.Join(ocDir, "commands")
	oldAgents := filepath.Join(h, "moved-dotfiles", "agents")
	oldCmds := filepath.Join(h, "moved-dotfiles", "commands")
	if err := os.Symlink(oldAgents, agentsLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(oldCmds, cmdsLink); err != nil {
		t.Fatal(err)
	}

	_, err := OpenCodeGuidance(qaConfig())
	if err == nil {
		t.Fatal("expected error when agents and commands are dangling symlinks")
	}
	msg := err.Error()
	for _, want := range []string{
		"dangling symlink",
		agentsLink, oldAgents,
		cmdsLink, oldCmds,
		"target does not exist",
		"ln -sfn",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "file exists") {
		t.Errorf("raw mkdir EEXIST leaked instead of dangling-symlink diagnosis:\n%s", msg)
	}

	// User symlinks must NEVER be deleted or repointed without explicit user action
	for _, pair := range [][2]string{{agentsLink, oldAgents}, {cmdsLink, oldCmds}} {
		fi, lerr := os.Lstat(pair[0])
		if lerr != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("symlink %s was removed or replaced: %v", pair[0], lerr)
		}
		gotTarget, _ := os.Readlink(pair[0])
		if gotTarget != pair[1] {
			t.Fatalf("symlink %s target changed to %q, want %q", pair[0], gotTarget, pair[1])
		}
	}

	issues := CheckSkillSymlinks()
	if len(issues) != 2 {
		t.Fatalf("CheckSkillSymlinks returned %d issues, want 2: %+v", len(issues), issues)
	}
}

package setup

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
)

//go:embed assets/*
var assets embed.FS

// DefaultUse maps role → "what it is for" (overridden by roles.<role>.use in the configuration).
var DefaultUse = map[string]string{
	"scout":     "Mechanical discovery: file/symbol search, inventory, log summaries. Cheap and fast; makes no decisions, never edits.",
	"architect": "Deep advisor: architecture decisions, plans, review axes, hard bugs. Give it narrow, targeted context; never edits.",
	"coder":     "Implementation within a single module; modifies the working tree and returns a diff. Code-trained model.",
	"verifier":  "Runs build/tests and summarizes failures only. Never carries raw logs.",
	"tester":    "Writes unit tests for existing code (test files only) and runs them; reports suspected bugs instead of fixing them.",
	"validator": "Checks claims (review findings, bug reports) against the real code: CONFIRMED / REFUTED / UNCLEAR with evidence. Never edits.",
	"hunter":    "Reads code looking for concrete bugs (races, edge cases, error handling). Reports candidates with path:line; never edits.",
	"parent":    "Main loop / dialogue; long context and tool use.",
	"embed":     "Embeddings (for RAG tools).",
}

type roleRow struct {
	Name, Active, Model, Node, Ctx, TPS, Use string
}

type guideData struct {
	Base, Date string
	Roles      []roleRow
}

func buildData(cfg *config.Config) guideData {
	d := guideData{Base: "http://" + cfg.Listen, Date: time.Now().Format("2006-01-02")}
	for _, r := range cfg.RoleNames() {
		role := cfg.Roles[r]
		pool := cfg.Pool(role)
		active := role.Preferred
		if len(pool) > 0 {
			active = pool[0].Name
		}
		row := roleRow{Name: r, Active: active, Model: "-", Node: "-", Ctx: "-", TPS: "-", Use: role.Use}
		if row.Use == "" {
			row.Use = DefaultUse[r]
		}
		if row.Use == "" {
			row.Use = "(no description — add `roles." + r + ".use`)"
		}
		if e := cfg.Endpoint(active); e != nil {
			row.Model, row.Node = e.Model, e.Node
			if e.Ctx > 0 {
				row.Ctx = fmt.Sprintf("%dk", e.Ctx/1024)
			}
			if e.TPS > 0 {
				row.TPS = fmt.Sprintf("%.0f", e.TPS)
			}
		}
		d.Roles = append(d.Roles, row)
	}
	return d
}

func render(name string, data any) (string, error) {
	b, err := assets.ReadFile("assets/" + name)
	if err != nil {
		return "", err
	}
	if !strings.HasSuffix(name, ".tmpl") {
		return string(b), nil
	}
	t, err := template.New(name).Parse(string(b))
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	if err := t.Execute(&out, data); err != nil {
		return "", err
	}
	return out.String(), nil
}

// SymlinkIssue describes a dangling (broken) or non-directory symlink on a setup path.
// fleet never removes or modifies user symlinks without explicit user action; it reports what is broken and how to fix it.
type SymlinkIssue struct {
	Path    string
	Target  string
	Reason  string
	WantDir bool
}

func (s *SymlinkIssue) Error() string {
	if s.WantDir {
		return fmt.Sprintf(
			"dangling symlink %s → %s (%s); fleet will not remove or modify symlinks automatically — fix by repointing (`ln -sfn <valid-dir> %s`) or replacing with a directory (`rm %s && mkdir -p %s`), then re-run `fleet setup skills`",
			s.Path, s.Target, s.Reason, s.Path, s.Path, s.Path,
		)
	}
	return fmt.Sprintf(
		"dangling symlink %s → %s (%s); fleet will not remove or modify symlinks automatically — fix by repointing (`ln -sfn <valid-file> %s`) or removing (`rm %s`), then re-run `fleet setup skills`",
		s.Path, s.Target, s.Reason, s.Path, s.Path,
	)
}

// checkDirSymlinks walks every prefix component of dir and reports the first broken or non-directory symlink.
func checkDirSymlinks(dir string) *SymlinkIssue {
	clean := filepath.Clean(dir)
	var prefixes []string
	for cur := clean; ; {
		prefixes = append(prefixes, cur)
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	for i := len(prefixes) - 1; i >= 0; i-- {
		p := prefixes[i]
		fi, err := os.Lstat(p)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			continue
		}
		target, _ := os.Readlink(p)
		st, statErr := os.Stat(p)
		if os.IsNotExist(statErr) {
			return &SymlinkIssue{Path: p, Target: target, Reason: "target does not exist", WantDir: true}
		}
		if statErr != nil {
			return &SymlinkIssue{Path: p, Target: target, Reason: statErr.Error(), WantDir: true}
		}
		if !st.IsDir() {
			return &SymlinkIssue{Path: p, Target: target, Reason: "points to a file, expected a directory", WantDir: true}
		}
	}
	return nil
}

func checkFileSymlink(path string) *SymlinkIssue {
	fi, err := os.Lstat(path)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		return nil
	}
	target, _ := os.Readlink(path)
	if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
		return &SymlinkIssue{Path: path, Target: target, Reason: "target does not exist", WantDir: false}
	}
	return nil
}

// CheckSkillSymlinks inspects the Claude Code and OpenCode skill/agent/command paths for dangling symlinks
// without modifying anything on disk (used by `fleet doctor`).
func CheckSkillSymlinks() []SymlinkIssue {
	dirs := []string{
		filepath.Join(claudeDir(), "skills", "fleet"),
		filepath.Join(claudeDir(), "commands"),
		filepath.Join(opencodeDir(), "agents"),
		filepath.Join(opencodeDir(), "commands"),
	}
	seen := map[string]bool{}
	var out []SymlinkIssue
	add := func(iss *SymlinkIssue) {
		if iss != nil && !seen[iss.Path] {
			seen[iss.Path] = true
			out = append(out, *iss)
		}
	}
	for _, d := range dirs {
		if iss := checkDirSymlinks(d); iss != nil {
			add(iss)
			continue
		}
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "fleet") || e.Name() == "SKILL.md" {
				add(checkFileSymlink(filepath.Join(d, e.Name())))
			}
		}
	}
	add(checkFileSymlink(filepath.Join(opencodeDir(), "opencode.json")))
	return out
}

// writeOwned writes a file owned by fleet (named fleet*); backs up different hand-written content first.
func writeOwned(path, content string) (string, error) {
	if iss := checkDirSymlinks(filepath.Dir(path)); iss != nil {
		return "", iss
	}
	if iss := checkFileSymlink(path); iss != nil {
		return "", iss
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if old, err := os.ReadFile(path); err == nil {
		if string(old) == content {
			return "unchanged", nil
		}
		if !strings.Contains(string(old), "managed by fleet") { // hand-written → back up
			os.WriteFile(fmt.Sprintf("%s.bak.%s", path, time.Now().Format("20060102-150405")), old, 0o644)
		}
	}
	return "written", os.WriteFile(path, []byte(content), 0o644)
}

func claudeDir() string   { return filepath.Join(home(), ".claude") }
func opencodeDir() string { return filepath.Join(home(), ".config", "opencode") }

// ClaudeGuidance writes the skill (with a live role table) + /fleet-review, /fleet-implement.
func ClaudeGuidance(cfg *config.Config) ([]string, error) {
	data := buildData(cfg)
	files := []struct{ asset, dest string }{
		{"claude_skill.md.tmpl", filepath.Join(claudeDir(), "skills", "fleet", "SKILL.md")},
		{"claude_cmd_review.md", filepath.Join(claudeDir(), "commands", "fleet-review.md")},
		{"claude_cmd_implement.md", filepath.Join(claudeDir(), "commands", "fleet-implement.md")},
		{"claude_cmd_qa.md", filepath.Join(claudeDir(), "commands", "fleet-qa.md")},
	}
	return writeAll(files, data)
}

// OpenCodeGuidance writes the fleet-<role> agents (model: fleet/<role>) + /fleet-review, /fleet-implement.
// Names are fleet-*, so they never collide with existing agents/commands.
func OpenCodeGuidance(cfg *config.Config) ([]string, error) {
	var files []struct{ asset, dest string }
	notes := []string{}
	for _, role := range []string{"scout", "architect", "coder", "verifier", "tester", "validator", "hunter"} {
		if _, ok := cfg.Roles[role]; !ok {
			notes = append(notes, "skipped: no such role → "+role+" (fleet role set "+role+" <endpoint>)")
			continue
		}
		files = append(files, struct{ asset, dest string }{"opencode_agent_" + role + ".md.tmpl", filepath.Join(opencodeDir(), "agents", "fleet-"+role+".md")})
	}
	files = append(files,
		struct{ asset, dest string }{"opencode_cmd_review.md", filepath.Join(opencodeDir(), "commands", "fleet-review.md")},
		struct{ asset, dest string }{"opencode_cmd_implement.md", filepath.Join(opencodeDir(), "commands", "fleet-implement.md")},
		struct{ asset, dest string }{"opencode_cmd_qa.md", filepath.Join(opencodeDir(), "commands", "fleet-qa.md")})
	out, err := writeAllRole(files, cfg)
	for _, sub := range []string{"agents", "commands"} {
		p := filepath.Join(opencodeDir(), sub)
		if fi, e := os.Lstat(p); e == nil && fi.Mode()&os.ModeSymlink != 0 {
			if st, se := os.Stat(p); se == nil && st.IsDir() {
				t, _ := os.Readlink(p)
				notes = append(notes, "note: the "+sub+" directory is a symlink → "+t+" (the fleet-*.md files were created there)")
			}
		}
	}
	return append(out, notes...), err
}

func writeAll(files []struct{ asset, dest string }, data any) ([]string, error) {
	var res []string
	var errs []error
	brokenDirs := map[string]bool{}
	for _, f := range files {
		dir := filepath.Dir(f.dest)
		if brokenDirs[dir] {
			continue
		}
		if iss := checkDirSymlinks(dir); iss != nil {
			brokenDirs[iss.Path] = true
			brokenDirs[dir] = true
			errs = append(errs, iss)
			continue
		}
		c, err := render(f.asset, data)
		if err != nil {
			return res, errors.Join(append(errs, err)...)
		}
		st, err := writeOwned(f.dest, c)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		res = append(res, fmt.Sprintf("%s: %s", st, f.dest))
	}
	return res, errors.Join(errs...)
}

// writeAllRole: in agent templates {{.Use}} is the role description.
func writeAllRole(files []struct{ asset, dest string }, cfg *config.Config) ([]string, error) {
	var res []string
	var errs []error
	brokenDirs := map[string]bool{}
	for _, f := range files {
		dir := filepath.Dir(f.dest)
		if brokenDirs[dir] {
			continue
		}
		if iss := checkDirSymlinks(dir); iss != nil {
			brokenDirs[iss.Path] = true
			brokenDirs[dir] = true
			errs = append(errs, iss)
			continue
		}
		data := map[string]string{"Use": ""}
		if strings.HasPrefix(f.asset, "opencode_agent_") {
			role := strings.TrimSuffix(strings.TrimPrefix(f.asset, "opencode_agent_"), ".md.tmpl")
			data["Use"] = cfg.Roles[role].Use
			if data["Use"] == "" {
				data["Use"] = DefaultUse[role]
			}
		}
		c, err := render(f.asset, data)
		if err != nil {
			return res, errors.Join(append(errs, err)...)
		}
		st, err := writeOwned(f.dest, c)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		res = append(res, fmt.Sprintf("%s: %s", st, f.dest))
	}
	return res, errors.Join(errs...)
}

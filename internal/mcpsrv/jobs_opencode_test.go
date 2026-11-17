package mcpsrv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeOpenCode puts an `opencode` script first in PATH. It records its working directory and one argument per
// line in <dir>/args, then runs body.
func fakeOpenCode(t *testing.T, body string) (argsFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	script := "#!/bin/sh\npwd > '" + argsFile + "'\nfor a in \"$@\"; do printf '%s\\n' \"$a\" >> '" + argsFile + "'; done\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, "opencode"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argsFile
}

func finish(t *testing.T, j *Jobs, id string) string {
	t.Helper()
	return j.Result(id, 10*time.Second, func(s string) string { return s })
}

func recorded(t *testing.T, f string) []string {
	t.Helper()
	b, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

func TestOpenCodeJobArguments(t *testing.T) {
	cwd := t.TempDir()
	cwd, _ = filepath.EvalSymlinks(cwd)

	t.Run("a role becomes the fleet-<role> agent, the model and the directory are passed", func(t *testing.T) {
		args := fakeOpenCode(t, "echo all done")
		j := newJobs(nil)
		id := j.Start("coder", "opencode", "fix the bug", "", "fleet/ep1", cwd, 0, 10*time.Second)
		out := finish(t, j, id)
		if !strings.Contains(out, "done") || !strings.Contains(out, "all done") {
			t.Fatalf("result: %q", out)
		}
		want := []string{cwd, "run", "--agent", "fleet-coder", "--model", "fleet/ep1", "fix the bug"}
		if got := recorded(t, args); strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("opencode got %q, want %q", got, want)
		}
	})

	t.Run("an agent already named fleet-<role> is not prefixed twice, no model means no --model", func(t *testing.T) {
		args := fakeOpenCode(t, "")
		j := newJobs(nil)
		finish(t, j, j.Start("fleet-scout", "opencode", "look", "", "", cwd, 0, 10*time.Second))
		want := []string{cwd, "run", "--agent", "fleet-scout", "look"}
		if got := recorded(t, args); strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("opencode got %q, want %q", got, want)
		}
	})

	t.Run("a prompt that starts with a dash is never taken for an option", func(t *testing.T) {
		args := fakeOpenCode(t, "")
		j := newJobs(nil)
		finish(t, j, j.Start("coder", "opencode", "--help me", "", "", cwd, 0, 10*time.Second))
		got := recorded(t, args)
		if last := got[len(got)-1]; last != " --help me" {
			t.Fatalf("prompt reached opencode as %q, want it with a leading space", last)
		}
	})
}

func TestOpenCodeJobOutcomes(t *testing.T) {
	cwd := t.TempDir()

	t.Run("a failing opencode is a failed job that keeps its output", func(t *testing.T) {
		fakeOpenCode(t, "echo boom; exit 3")
		j := newJobs(nil)
		out := finish(t, j, j.Start("coder", "opencode", "x", "", "", cwd, 0, 10*time.Second))
		if !strings.Contains(out, "failed") || !strings.Contains(out, "boom") || !strings.Contains(out, "[error]") {
			t.Fatalf("result: %q", out)
		}
	})

	t.Run("opencode missing from PATH is a failed job, not a crash", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		j := newJobs(nil)
		out := finish(t, j, j.Start("coder", "opencode", "x", "", "", cwd, 0, 10*time.Second))
		if !strings.Contains(out, "failed") || !strings.Contains(out, "opencode") {
			t.Fatalf("result: %q", out)
		}
	})

	t.Run("a job that outlives its timeout is stopped with its child processes", func(t *testing.T) {
		pidFile := filepath.Join(t.TempDir(), "child.pid")
		fakeOpenCode(t, "sleep 30 & echo $! > '"+pidFile+"'; wait")
		j := newJobs(nil)
		start := time.Now()
		out := finish(t, j, j.Start("coder", "opencode", "x", "", "", cwd, 0, 1*time.Second))
		if !strings.Contains(out, "timeout") || time.Since(start) > 8*time.Second {
			t.Fatalf("after %v: %q", time.Since(start), out)
		}
		assertGone(t, pidFile)
	})

	t.Run("cancel stops the job and its child processes", func(t *testing.T) {
		pidFile := filepath.Join(t.TempDir(), "child.pid")
		fakeOpenCode(t, "sleep 30 & echo $! > '"+pidFile+"'; wait")
		j := newJobs(nil)
		id := j.Start("coder", "opencode", "x", "", "", cwd, 0, time.Minute)
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
			if b, _ := os.ReadFile(pidFile); len(b) > 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the fake opencode did not start")
			}
		}
		j.Cancel(id)
		if out := finish(t, j, id); !strings.Contains(out, "cancelled") {
			t.Fatalf("result: %q", out)
		}
		assertGone(t, pidFile)
	})
}

// assertGone fails unless the process whose pid is in file has exited (it was a child of the job's process group).
func assertGone(t *testing.T, file string) {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("no child pid: %v", err)
	}
	pid := strings.TrimSpace(string(b))
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if _, err := os.Stat("/proc/" + pid); err != nil {
			return
		}
		if st, _ := os.ReadFile("/proc/" + pid + "/stat"); strings.Contains(string(st), ") Z ") {
			return // a zombie has exited; init reaps it
		}
	}
	t.Fatalf("child process %s is still running", pid)
}

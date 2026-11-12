package main

import (
	"errors"
	"strings"
	"testing"
)

func TestClassifyUpdate(t *testing.T) {
	exit := errors.New("exit status 1")
	cases := []struct {
		out   string
		err   error
		check bool
		state string
	}{
		{"fleet v0.10.1-50-gabc is up to date (latest release: v0.10.1-50-gabc)", nil, false, "current"},
		{"✓ fleet v0.10.1-32-gaaa → v0.10.1-50-gbbb   (go back with `fleet update --rollback`)\n  service restarted", nil, false, "updated"},
		{"fleet v0.10.1-32-gaaa → v0.10.1-50-gbbb available: run `fleet update`", nil, true, "behind"},
		{"no fleet installed (install.sh)", exit, false, "skipped"},
		{"this hub carries 3 open stream(s) (dino:1): updating restarts it", exit, false, "skipped"},
		{"the update restarts the fleet service, which would kill 2 process(es)", exit, false, "skipped"},
		{"ssh: Could not resolve hostname nope", exit, false, "failed"},
		{"boom", exit, false, "failed"},
	}
	for _, c := range cases {
		if got := classifyUpdate("h", c.out, c.err, c.check); got.State != c.state {
			t.Errorf("%q: %s want %s (%s)", c.out, got.State, c.state, got.Detail)
		}
	}
}

func TestUpdateSummaryNamesEveryOutcome(t *testing.T) {
	rs := []updateResult{
		{"victor", "updated", "v0.10.1-32 → v0.10.1-50"},
		{"hub", "skipped", "not updated: 2 active streams"},
		{"papa", "current", "v0.10.1-50"},
		{"dino", "failed", "unreachable over ssh: timed out"},
	}
	all := strings.Join(updateSummary(rs, false), "\n")
	for _, w := range []string{"updated (1): victor", "SKIPPED (1)", "hub (not updated", "already current (1): papa", "FAILED (1)", "dino (unreachable", "still run the old version"} {
		if !strings.Contains(all, w) {
			t.Errorf("missing %q in:\n%s", w, all)
		}
	}
}

// "permission denied" from writing the binary locally is not an ssh problem.
func TestUpdateReportLocalPermissionDeniedIsNotUnreachable(t *testing.T) {
	r := classifyUpdate("dino", "open /home/u/.local/bin/fleet.new: permission denied", errors.New("exit 1"), false)
	if strings.Contains(r.Detail, "unreachable over ssh") {
		t.Fatalf("%+v", r)
	}
	r = classifyUpdate("dino", "user@dino: Permission denied (publickey).", errors.New("exit 255"), false)
	if !strings.Contains(r.Detail, "unreachable over ssh") {
		t.Fatalf("%+v", r)
	}
}

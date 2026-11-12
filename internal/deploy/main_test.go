package deploy

import (
	"os"
	"testing"
)

// The tests must not write run records into (or read foreign ports from) the user's real cache: every deploy writes
// ~/.cache/fleet/runs/<id>.json, and the next deploy keeps off the ports of the records that still run.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "fleet-deploy-test-cache-")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_CACHE_HOME", dir)
	// planning must not depend on records other tests left: the lookup is a stub unless a test sets its own
	foreignRunPorts = func(string) map[string]map[int]bool { return nil }
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

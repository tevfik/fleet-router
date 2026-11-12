package board

import (
	"strings"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/server"
)

func TestParseVersion(t *testing.T) {
	for in, want := range map[string]Ver{
		"v0.10.1":                   {Major: 0, Minor: 10, Patch: 1},
		"v0.10.1-32-g1a2b3c4":       {Minor: 10, Patch: 1, Ahead: 32},
		"v0.10.1-36-g1a2b3c4-dirty": {Minor: 10, Patch: 1, Ahead: 36, Dirty: true},
	} {
		got := ParseVersion(in)
		got.Raw = ""
		if got != want {
			t.Errorf("%s: %+v want %+v", in, got, want)
		}
	}
	if !ParseVersion("dev").IsDev() || !ParseVersion("").IsDev() || !ParseVersion("abc1234").Unknown {
		t.Error("dev / unknown")
	}
	if !ParseVersion("v0.10.1-32-gabc").Less(ParseVersion("v0.10.1-50-gdef")) || ParseVersion("v0.10.2").Less(ParseVersion("v0.10.1-90-gabc")) {
		t.Error("ordering")
	}
}

// The case from the session: victor -32, hub -36, papa and dino -41 forgotten while the newest is -50.
func TestVersionDriftNamesStaleNodes(t *testing.T) {
	lines := VersionDrift([]NodeVersion{
		{Node: "victor", Fleet: "v0.10.1-32-gaaaaaaa"},
		{Node: "hub", Fleet: "v0.10.1-50-gbbbbbbb"},
		{Node: "papa", Fleet: "v0.10.1-41-gccccccc"},
		{Node: "local", Fleet: "v0.10.1-50-gbbbbbbb"},
		{Node: "scratch", Fleet: "dev"},
	})
	all := strings.Join(lines, "\n")
	for _, want := range []string{"victor v0.10.1-32-gaaaaaaa", "papa v0.10.1-41-gccccccc", "newest hub v0.10.1-50-gbbbbbbb, local", "dev build", "scratch"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in:\n%s", want, all)
		}
	}
	if strings.Contains(all, "hub v0.10.1-50-gbbbbbbb,") && strings.Contains(all, "behind") {
		t.Error("the newest are not behind")
	}
	if got := VersionDrift([]NodeVersion{{Node: "a", Fleet: "v0.10.1"}, {Node: "b", Fleet: "v0.10.1"}}); len(got) != 0 {
		t.Errorf("no drift expected: %v", got)
	}
}

func TestVersionDriftEngines(t *testing.T) {
	l := VersionDrift([]NodeVersion{{Node: "a", Fleet: "v1.0.0", Engine: "e1"}, {Node: "b", Fleet: "v1.0.0", Engine: "e2"}})
	if len(l) != 1 || !strings.Contains(l[0], "engine versions differ") {
		t.Fatalf("%v", l)
	}
}

func TestVersionDriftEngineIgnoresArchAndBackendDifferences(t *testing.T) {
	// 1. Same package version installed via fleet engine install on amd64 vs arm64 -> no warning
	nodes := []NodeVersion{
		{Node: "papa", Fleet: "v0.11.1", Engine: "0.3.0 fleet-engine-linux-amd64-cuda12-sm89.xz"},
		{Node: "victor", Fleet: "v0.11.1", Engine: "0.3.0 fleet-engine-linux-amd64-cuda13.xz"},
		{Node: "dino", Fleet: "v0.11.1", Engine: "0.3.0 fleet-engine-linux-arm64-cuda13.xz"},
	}
	if got := VersionDrift(nodes); len(got) != 0 {
		t.Errorf("expected no warning for same engine version across architectures, got: %v", got)
	}

	// 2. Different engine package versions (0.2.0 vs 0.3.0) -> warning
	nodes = []NodeVersion{
		{Node: "papa", Fleet: "v0.11.1", Engine: "0.2.0 fleet-engine-linux-amd64-cuda12-sm89.xz"},
		{Node: "dino", Fleet: "v0.11.1", Engine: "0.3.0 fleet-engine-linux-arm64-cuda13.xz"},
	}
	if got := VersionDrift(nodes); len(got) != 1 || !strings.Contains(got[0], "engine versions differ") {
		t.Errorf("expected engine versions differ warning for 0.2.0 vs 0.3.0, got: %v", got)
	}

	// 3. Embedded engine with same protocol version on amd64 vs arm64 -> no warning
	nodes = []NodeVersion{
		{Node: "papa", Fleet: "v0.11.1", Engine: "llama-ring (embedded engine, ring protocol 101, linux/amd64, cuda)"},
		{Node: "dino", Fleet: "v0.11.1", Engine: "llama-ring (embedded engine, ring protocol 101, linux/arm64, cuda)"},
	}
	if got := VersionDrift(nodes); len(got) != 0 {
		t.Errorf("expected no warning for same embedded protocol across architectures, got: %v", got)
	}

	// 4. Embedded engine with differing protocol version -> warning
	nodes = []NodeVersion{
		{Node: "papa", Fleet: "v0.11.1", Engine: "llama-ring (embedded engine, ring protocol 101, linux/amd64, cuda)"},
		{Node: "dino", Fleet: "v0.11.1", Engine: "llama-ring (embedded engine, ring protocol 102, linux/arm64, cuda)"},
	}
	if got := VersionDrift(nodes); len(got) != 1 || !strings.Contains(got[0], "engine versions differ") {
		t.Errorf("expected warning for protocol 101 vs 102, got: %v", got)
	}
}

func TestVersionCell(t *testing.T) {
	if VersionCell(server.HostReport{Err: "x"}) != "—" {
		t.Error("err")
	}
	r := server.HostReport{Version: "dev", At: time.Now()}
	if VersionCell(r) != "dev" {
		t.Error("dev")
	}
	r.Version = ""
	if VersionCell(r) != "old" {
		t.Error("old")
	}
}

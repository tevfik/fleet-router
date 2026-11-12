package board

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/tevfik/fleet-router/internal/server"
)

// Fleet versions are `git describe` strings: v0.10.1 (a release), v0.10.1-32-g1a2b3c4 (32 commits after it),
// with -dirty for uncommitted changes; a build without ldflags says "dev".
var verRe = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)(?:-(\d+)-g[0-9a-f]+)?(-dirty)?$`)

// Ver is a parsed fleet version.
type Ver struct {
	Raw     string
	Major   int
	Minor   int
	Patch   int
	Ahead   int  // commits after the tag
	Dirty   bool // built with uncommitted changes
	Unknown bool // "dev", empty or anything that is not a git describe: cannot be compared
}

// ParseVersion reads a fleet version string.
func ParseVersion(s string) Ver {
	s = strings.TrimSpace(s)
	v := Ver{Raw: s}
	m := verRe.FindStringSubmatch(s)
	if m == nil {
		v.Unknown = true
		return v
	}
	v.Major, _ = strconv.Atoi(m[1])
	v.Minor, _ = strconv.Atoi(m[2])
	v.Patch, _ = strconv.Atoi(m[3])
	v.Ahead, _ = strconv.Atoi(m[4])
	v.Dirty = m[5] != ""
	return v
}

// IsDev: a build made without a version (go build without ldflags): nobody can tell what it contains.
func (v Ver) IsDev() bool { return v.Raw == "" || v.Raw == "dev" }

// Less: v is older than o (both comparable).
func (v Ver) Less(o Ver) bool {
	a, b := [4]int{v.Major, v.Minor, v.Patch, v.Ahead}, [4]int{o.Major, o.Minor, o.Patch, o.Ahead}
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// short is a version for a table cell.
func (v Ver) short() string {
	if v.IsDev() {
		return "dev"
	}
	return strings.TrimSuffix(v.Raw, "-dirty") + map[bool]string{true: "*", false: ""}[v.Dirty]
}

// VersionCell is the fleet version column of a node row ("—" while unknown, "dev" for a build without version).
func VersionCell(r server.HostReport) string {
	if r.Err != "" || (r.Version == "" && r.At.IsZero()) {
		return "—"
	}
	if r.Version == "" {
		return "old" // a fleet too old to report its version
	}
	return ParseVersion(r.Version).short()
}

// NodeVersion is what one machine runs.
type NodeVersion struct {
	Node, Fleet, Engine string
}

// VersionDrift compares the machines' fleet versions and returns one line per finding: builds without a
// version ("dev"), machines behind the newest (listed together with the newest, e.g. "victor v0.10.1-32,
// hub v0.10.1-50"), and engines that differ. Empty when everything agrees.
func VersionDrift(nodes []NodeVersion) []string {
	var lines []string
	var known []NodeVersion
	var dev []string
	var newest Ver
	have := false
	for _, n := range nodes {
		if n.Fleet == "" {
			continue
		}
		v := ParseVersion(n.Fleet)
		if v.IsDev() {
			dev = append(dev, n.Node)
			continue
		}
		if v.Unknown {
			continue
		}
		known = append(known, n)
		if !have || newest.Less(v) {
			newest, have = v, true
		}
	}
	sort.Strings(dev)
	if len(dev) > 0 {
		lines = append(lines, fmt.Sprintf("dev build (no version, built without ldflags) on: %s — `fleet update --force` or install a release", strings.Join(dev, ", ")))
	}
	if have {
		var behind []string
		for _, n := range known {
			if ParseVersion(n.Fleet).Less(newest) {
				behind = append(behind, fmt.Sprintf("%s %s", n.Node, n.Fleet))
			}
		}
		if len(behind) > 0 {
			sort.Strings(behind)
			var ahead []string
			for _, n := range known {
				if !ParseVersion(n.Fleet).Less(newest) {
					ahead = append(ahead, fmt.Sprintf("%s %s", n.Node, n.Fleet))
				}
			}
			sort.Strings(ahead)
			lines = append(lines, fmt.Sprintf("version drift: %s, newest %s — `fleet update --all`", strings.Join(behind, ", "), strings.Join(ahead, ", ")))
		}
	}
	engines := map[string][]string{}
	for _, n := range nodes {
		if n.Engine != "" && n.Engine != "not built in" {
			ver := normalizeEngine(n.Engine)
			engines[ver] = append(engines[ver], n.Node)
		}
	}
	if len(engines) > 1 {
		var parts []string
		for e, ns := range engines {
			sort.Strings(ns)
			parts = append(parts, fmt.Sprintf("%s: %s", strings.Join(ns, ","), e))
		}
		sort.Strings(parts)
		lines = append(lines, "engine versions differ ("+strings.Join(parts, " · ")+") — `fleet engine install --all`")
	}
	return lines
}

// normalizeEngine extracts the version component of an engine string so that os/arch/backend differences
// (e.g. linux/amd64 vs linux/arm64) do not trigger false "engine versions differ" warnings.
func normalizeEngine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || s == "not built in" {
		return s
	}

	// 1. "installed by fleet engine install" stamp format: "<version> fleet-engine-<os>-<arch>-<backend>.xz"
	fields := strings.Fields(s)
	if len(fields) >= 2 && strings.HasPrefix(fields[1], "fleet-engine-") {
		return fields[0] // e.g. "0.3.0"
	}

	// 2. Embedded engine string: "llama-ring (embedded engine, ring protocol 101, linux/amd64, cuda)"
	if idx := strings.Index(s, "ring protocol "); idx != -1 {
		rest := s[idx:]
		proto := strings.TrimPrefix(rest, "ring protocol ")
		if end := strings.IndexAny(proto, ",)"); end != -1 {
			proto = proto[:end]
		}
		return "llama-ring protocol " + strings.TrimSpace(proto)
	}

	return s
}

// DriftOfReports is VersionDrift over what the nodes' fleets reported.
func DriftOfReports(nodes []server.NodeStatus, reps []server.HostReport) []string {
	var vs []NodeVersion
	for i, n := range nodes {
		if i < len(reps) && reps[i].Err == "" {
			vs = append(vs, NodeVersion{Node: n.Node, Fleet: reps[i].Version, Engine: reps[i].Engine})
		}
	}
	return VersionDrift(vs)
}

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCovEndpointMethods(t *testing.T) {
	// FleetRemote & FleetHostPort
	epDefault := &Endpoint{}
	if got := epDefault.FleetRemote(); got != DefaultListen {
		t.Fatalf("FleetRemote() default got %q, want %q", got, DefaultListen)
	}
	if got := epDefault.FleetHostPort(); got != "4711" {
		t.Fatalf("FleetHostPort() default got %q, want %q", got, "4711")
	}

	epCustom := &Endpoint{FleetPort: 8888}
	if got := epCustom.FleetRemote(); got != "127.0.0.1:8888" {
		t.Fatalf("FleetRemote() custom got %q, want %q", got, "127.0.0.1:8888")
	}
	if got := epCustom.FleetHostPort(); got != "8888" {
		t.Fatalf("FleetHostPort() custom got %q, want %q", got, "8888")
	}

	// StatsRemote
	// 1. Group nil
	if got := epDefault.StatsRemote(); got != "" {
		t.Fatalf("StatsRemote() with nil group got %q, want empty", got)
	}
	// 2. Group StatsPort <= 0
	epGroupNoPort := &Endpoint{Group: &Group{StatsPort: 0}}
	if got := epGroupNoPort.StatsRemote(); got != "" {
		t.Fatalf("StatsRemote() with StatsPort 0 got %q, want empty", got)
	}
	// 3. URL is not empty
	epGroupURL := &Endpoint{Group: &Group{StatsPort: 9000}, URL: "http://127.0.0.1"}
	if got := epGroupURL.StatsRemote(); got != "" {
		t.Fatalf("StatsRemote() with URL set got %q, want empty", got)
	}
	// 4. SSH is empty
	epGroupNoSSH := &Endpoint{Group: &Group{StatsPort: 9000}, Remote: "127.0.0.1:8000"}
	if got := epGroupNoSSH.StatsRemote(); got != "" {
		t.Fatalf("StatsRemote() with SSH empty got %q, want empty", got)
	}
	// 5. Valid SSH + Remote with host
	epValidSSH := &Endpoint{
		Group:  &Group{StatsPort: 9000},
		SSH:    "node1",
		Remote: "10.0.1.50:8000",
	}
	if got := epValidSSH.StatsRemote(); got != "10.0.1.50:9000" {
		t.Fatalf("StatsRemote() valid got %q, want %q", got, "10.0.1.50:9000")
	}
	// 6. Valid SSH + Remote without parseable host
	epValidSSHBadHost := &Endpoint{
		Group:  &Group{StatsPort: 9000},
		SSH:    "node1",
		Remote: "invalid-split",
	}
	if got := epValidSSHBadHost.StatsRemote(); got != "127.0.0.1:9000" {
		t.Fatalf("StatsRemote() fallback got %q, want %q", got, "127.0.0.1:9000")
	}

	// StallAfter
	stallTests := []struct {
		stall string
		group *Group
		want  time.Duration
	}{
		{"off", nil, 0},
		{"never", nil, 0},
		{"0", nil, 0},
		{"", nil, 0},
		{"", &Group{}, DefaultGroupStall},
		{"30s", nil, 30 * time.Second},
		{"1m", nil, time.Minute},
	}
	for _, tc := range stallTests {
		ep := &Endpoint{Stall: tc.stall, Group: tc.group}
		if got := ep.StallAfter(); got != tc.want {
			t.Errorf("StallAfter(%q) got %v, want %v", tc.stall, got, tc.want)
		}
	}
}

func TestCovNodeAndEconomyConfig(t *testing.T) {
	var nilCfg *Config
	if _, ok := nilCfg.Node("test"); ok {
		t.Fatal("Node() on nil config should return false")
	}
	cfg := &Config{
		Nodes: map[string]NodeSpec{
			"MyHost": {Addr: "127.0.0.1", MemGiB: 32},
		},
	}
	if _, ok := cfg.Node(""); ok {
		t.Fatal("Node(\"\") should return false")
	}
	if n, ok := cfg.Node("MyHost"); !ok || n.MemGiB != 32 {
		t.Fatalf("Node(exact) got %+v, ok=%v", n, ok)
	}
	if n, ok := cfg.Node("myhost"); !ok || n.MemGiB != 32 {
		t.Fatalf("Node(case-insensitive) got %+v, ok=%v", n, ok)
	}
	if _, ok := cfg.Node("nonexistent"); ok {
		t.Fatal("Node(nonexistent) should return false")
	}

	// EconomyConfig
	var nilEcon *EconomyConfig
	if !nilEcon.IsEnabled() {
		t.Error("nil EconomyConfig should be IsEnabled == true")
	}
	if !nilEcon.IsFairQueuing() {
		t.Error("nil EconomyConfig should be IsFairQueuing == true")
	}
	if nilEcon.IsShareIdle() {
		t.Error("nil EconomyConfig should be IsShareIdle == false")
	}

	tVal, fVal := true, false
	econ := &EconomyConfig{
		Enabled:     &fVal,
		FairQueuing: &fVal,
		ShareIdle:   &tVal,
	}
	if econ.IsEnabled() {
		t.Error("IsEnabled with false pointer should be false")
	}
	if econ.IsFairQueuing() {
		t.Error("IsFairQueuing with false pointer should be false")
	}
	if !econ.IsShareIdle() {
		t.Error("IsShareIdle with true pointer should be true")
	}
}

func TestCovPathsAndVRAMIdleDuration(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if r := RunsPath(); r == "" || !strings.Contains(r, "fleet") {
		t.Errorf("RunsPath() unexpected: %s", r)
	}
	if rtt := RTTPath(); rtt == "" || !strings.Contains(rtt, "rtt.json") {
		t.Errorf("RTTPath() unexpected: %s", rtt)
	}

	cfg := &Config{}
	if d := cfg.VRAMIdleDuration(); d != 10*time.Minute {
		t.Errorf("VRAMIdleDuration() default got %v, want 10m", d)
	}
	cfg.VRAMIdle = "off"
	if d := cfg.VRAMIdleDuration(); d != 0 {
		t.Errorf("VRAMIdleDuration(off) got %v, want 0", d)
	}
	cfg.VRAMIdle = "never"
	if d := cfg.VRAMIdleDuration(); d != 0 {
		t.Errorf("VRAMIdleDuration(never) got %v, want 0", d)
	}
	cfg.VRAMIdle = "0"
	if d := cfg.VRAMIdleDuration(); d != 0 {
		t.Errorf("VRAMIdleDuration(0) got %v, want 0", d)
	}
	cfg.VRAMIdle = "15m"
	if d := cfg.VRAMIdleDuration(); d != 15*time.Minute {
		t.Errorf("VRAMIdleDuration(15m) got %v, want 15m", d)
	}
	cfg.VRAMIdle = "invalid-dur"
	if d := cfg.VRAMIdleDuration(); d != 10*time.Minute {
		t.Errorf("VRAMIdleDuration(invalid) got %v, want fallback 10m", d)
	}
}

func TestCovHistory30SnapshotsAndUndo(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")

	// Verify nothing to undo initially
	if _, err := Undo(p); err == nil {
		t.Fatal("expected error from Undo when no snapshots exist")
	}

	// Create a base config and save it
	cfg := &Config{
		Endpoints: []Endpoint{
			{Name: "ep-0", URL: "http://127.0.0.1:8000", Model: "m0"},
		},
	}
	if err := cfg.Save(p); err != nil {
		t.Fatalf("initial Save failed: %v", err)
	}

	// Now save 35 incremental versions to test keepSnapshots (30 versions)
	for i := 1; i <= 35; i++ {
		cfg.Endpoints = []Endpoint{
			{Name: fmt.Sprintf("ep-%d", i), URL: "http://127.0.0.1:8000", Model: fmt.Sprintf("m%d", i)},
		}
		if err := cfg.Save(p); err != nil {
			t.Fatalf("Save version %d failed: %v", i, err)
		}
	}

	snaps := Snapshots(p)
	if len(snaps) != 30 {
		t.Fatalf("expected exactly 30 snapshots, got %d", len(snaps))
	}

	// Test Undo sequentially
	for len(snaps) > 0 {
		undone, err := Undo(p)
		if err != nil {
			t.Fatalf("Undo failed with %d snapshots left: %v", len(snaps), err)
		}
		if undone == "" {
			t.Fatal("Undo returned empty snapshot name")
		}
		snaps = Snapshots(p)
	}

	// When all snapshots are undone, next Undo should fail
	if _, err := Undo(p); err == nil {
		t.Fatal("Undo should fail when all history is exhausted")
	}

	// Test corrupt snapshot handling in Undo
	histDir := historyDir(p)
	corruptFile := filepath.Join(histDir, "20260101-000000.000000.yaml")
	if err := os.WriteFile(corruptFile, []byte("bad: yaml: ["), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Undo(p); err == nil {
		t.Fatal("Undo on corrupt snapshot should fail")
	}
	os.Remove(corruptFile)

	// Test snapshot with invalid config (fails Validate)
	invalidCfgFile := filepath.Join(histDir, "20260101-000000.000001.yaml")
	if err := os.WriteFile(invalidCfgFile, []byte("endpoints:\n  - name: ''\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Undo(p); err == nil {
		t.Fatal("Undo on invalid config snapshot should fail")
	}
	os.Remove(invalidCfgFile)
}

func TestCovConflictingWritersLock(t *testing.T) {
	p := filepath.Join(t.TempDir(), "conflicting.yaml")
	un1, err := Lock(p)
	if err != nil {
		t.Fatalf("first Lock failed: %v", err)
	}

	// Try acquiring lock with a short deadline inside a goroutine
	acquired := make(chan bool, 1)
	go func() {
		// Replace deadline behavior simulation by checking lock failure
		un2, err2 := Lock(p)
		if err2 == nil {
			acquired <- true
			un2()
		} else {
			acquired <- false
		}
	}()

	time.Sleep(100 * time.Millisecond)
	select {
	case <-acquired:
		t.Fatal("second lock should not succeed while first is held")
	default:
	}

	// Release first lock
	un1()
	select {
	case ok := <-acquired:
		if !ok {
			t.Fatal("second lock failed after first released")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for second lock acquisition")
	}
}

func TestCovDropEndpoints(t *testing.T) {
	cfg := &Config{
		Endpoints: []Endpoint{
			{Name: "ep1", URL: "http://127.0.0.1:8001", Model: "m1"},
			{Name: "ep2", URL: "http://127.0.0.1:8002", Model: "m2"},
			{Name: "ep3", URL: "http://127.0.0.1:8003", Model: "m3"},
		},
		Roles: map[string]Role{
			"roleA": {
				Preferred: "ep1",
				Fallback:  []string{"ep2", "ep3"},
			},
			"roleB": {
				Preferred: "ep2",
				Fallback:  []string{"ep3"},
			},
			"roleC": {
				Preferred: "ep3",
				Match:     []string{"node:dino"},
			},
			"roleD": {
				Preferred: "ep3",
			},
		},
	}

	notes := cfg.DropEndpoints("ep1", "ep3")
	if len(notes) == 0 {
		t.Fatal("expected notes from DropEndpoints")
	}

	// ep1 and ep3 should be gone from Endpoints
	if len(cfg.Endpoints) != 1 || cfg.Endpoints[0].Name != "ep2" {
		t.Fatalf("unexpected endpoints remaining: %+v", cfg.Endpoints)
	}

	// In roleA: ep1 was preferred, ep2 fallback, ep3 fallback.
	// ep1 dropped -> replaced by ep2. ep3 dropped from fallback.
	roleA := cfg.Roles["roleA"]
	if roleA.Preferred != "ep2" || len(roleA.Fallback) != 0 {
		t.Fatalf("roleA unexpected: %+v", roleA)
	}

	// In roleB: ep2 was preferred, ep3 fallback.
	// ep3 dropped from fallback.
	roleB := cfg.Roles["roleB"]
	if roleB.Preferred != "ep2" || len(roleB.Fallback) != 0 {
		t.Fatalf("roleB unexpected: %+v", roleB)
	}

	// In roleC: ep3 was preferred, match selector present.
	// ep3 dropped -> preferred becomes empty, but match selector keeps role alive!
	roleC, okC := cfg.Roles["roleC"]
	if !okC || roleC.Preferred != "" || len(roleC.Match) != 1 {
		t.Fatalf("roleC should remain with empty preferred: %+v", roleC)
	}

	// In roleD: ep3 was preferred, no fallback, no match selector.
	// ep3 dropped -> roleD deleted!
	if _, okD := cfg.Roles["roleD"]; okD {
		t.Fatal("roleD should have been deleted because nothing left to route to")
	}
}

func TestCovValidationErrors(t *testing.T) {
	tests := []struct {
		name string
		cfg  *Config
		err  string
	}{
		{
			name: "endpoint missing name",
			cfg: &Config{
				Endpoints: []Endpoint{{Model: "m"}},
			},
			err: "without a name",
		},
		{
			name: "duplicate endpoint name",
			cfg: &Config{
				Endpoints: []Endpoint{
					{Name: "ep1", URL: "http://127.0.0.1:8001"},
					{Name: "ep1", URL: "http://127.0.0.1:8002"},
				},
			},
			err: "duplicate endpoint name",
		},
		{
			name: "endpoint missing both url and ssh+remote",
			cfg: &Config{
				Endpoints: []Endpoint{{Name: "ep1", Model: "m"}},
			},
			err: "url or (ssh + remote) is required",
		},
		{
			name: "endpoint invalid ssh target",
			cfg: &Config{
				Endpoints: []Endpoint{{Name: "ep1", SSH: "-oProxyCommand=evil", Remote: "127.0.0.1:8000"}},
			},
			err: "starts with -",
		},
		{
			name: "node invalid name characters",
			cfg: &Config{
				Nodes: map[string]NodeSpec{"bad:name": {}},
			},
			err: "node name",
		},
		{
			name: "node invalid IP",
			cfg: &Config{
				Nodes: map[string]NodeSpec{"node1": {Addr: "not-an-ip"}},
			},
			err: "not an IP address",
		},
		{
			name: "node negative resources",
			cfg: &Config{
				Nodes: map[string]NodeSpec{"node1": {MemGiB: -1}},
			},
			err: "must not be negative",
		},
		{
			name: "discover ssh invalid target",
			cfg: &Config{
				Discover: Discover{SSH: []string{"-bad"}},
			},
			err: "discover.ssh",
		},
		{
			name: "peer invalid name",
			cfg: &Config{
				Peers: []Peer{{Name: "bad/name", URL: "http://127.0.0.1"}},
			},
			err: "peer name",
		},
		{
			name: "duplicate peer name",
			cfg: &Config{
				Peers: []Peer{
					{Name: "peer1", URL: "http://127.0.0.1:8001"},
					{Name: "peer1", URL: "http://127.0.0.1:8002"},
				},
			},
			err: "duplicate peer name",
		},
		{
			name: "peer missing url and ssh",
			cfg: &Config{
				Peers: []Peer{{Name: "peer1"}},
			},
			err: "url or ssh is required",
		},
		{
			name: "peer invalid ssh",
			cfg: &Config{
				Peers: []Peer{{Name: "peer1", SSH: "; rm -rf"}},
			},
			err: "contains a space",
		},
		{
			name: "peer invalid selector",
			cfg: &Config{
				Peers: []Peer{{Name: "peer1", URL: "http://127.0.0.1", Only: []string{"bad::sel:extra"}}},
			},
			err: "bad selector",
		},
		{
			name: "endpoint with unknown peer",
			cfg: &Config{
				Endpoints: []Endpoint{{Name: "ep1", URL: "http://127.0.0.1", Peer: "ghost"}},
			},
			err: "belongs to unknown peer",
		},
		{
			name: "invalid tunnel_idle",
			cfg: &Config{
				TunnelIdle: "not-a-duration",
			},
			err: "tunnel_idle",
		},
		{
			name: "invalid vram_idle",
			cfg: &Config{
				VRAMIdle: "not-a-duration",
			},
			err: "vram_idle",
		},
		{
			name: "endpoint invalid fleet_port",
			cfg: &Config{
				Endpoints: []Endpoint{{Name: "ep1", URL: "http://127.0.0.1", FleetPort: 70000}},
			},
			err: "is not a port",
		},
		{
			name: "endpoint invalid stall duration",
			cfg: &Config{
				Endpoints: []Endpoint{{Name: "ep1", URL: "http://127.0.0.1", Stall: "100ms"}},
			},
			err: "stall",
		},
		{
			name: "endpoint group missing engine",
			cfg: &Config{
				Endpoints: []Endpoint{{Name: "ep1", URL: "http://127.0.0.1", Group: &Group{}}},
			},
			err: "group needs an engine",
		},
		{
			name: "endpoint group empty worker name",
			cfg: &Config{
				Endpoints: []Endpoint{{Name: "ep1", URL: "http://127.0.0.1", Group: &Group{Engine: "llama-ring", Workers: []string{""}}}},
			},
			err: "group worker has no name",
		},
		{
			name: "endpoint group invalid stats_port",
			cfg: &Config{
				Endpoints: []Endpoint{{Name: "ep1", URL: "http://127.0.0.1", Group: &Group{Engine: "llama-ring", StatsPort: 99999}}},
			},
			err: "stats_port",
		},
		{
			name: "role unknown strategy",
			cfg: &Config{
				Roles: map[string]Role{"role1": {Strategy: "quantum-magic"}},
			},
			err: "unknown strategy",
		},
		{
			name: "role name collides with endpoint",
			cfg: &Config{
				Endpoints: []Endpoint{{Name: "shared", URL: "http://127.0.0.1"}},
				Roles:     map[string]Role{"shared": {Preferred: "shared"}},
			},
			err: "collides with an endpoint name",
		},
		{
			name: "role unknown endpoint",
			cfg: &Config{
				Roles: map[string]Role{"role1": {Preferred: "ghost-ep"}},
			},
			err: "unknown endpoint",
		},
		{
			name: "role invalid match selector",
			cfg: &Config{
				Roles: map[string]Role{"role1": {Match: []string{"bad::sel:extra"}}},
			},
			err: "bad selector",
		},
		{
			name: "role missing preferred and match",
			cfg: &Config{
				Roles: map[string]Role{"role1": {}},
			},
			err: "needs a preferred endpoint or a match selector",
		},
		{
			name: "embedding role vector dimension mismatch",
			cfg: &Config{
				Endpoints: []Endpoint{
					{Name: "emb1", URL: "http://127.0.0.1:8001", Model: "text-embedding-3-small", Dim: 512},
					{Name: "emb2", URL: "http://127.0.0.1:8002", Model: "text-embedding-3-large", Dim: 1536},
				},
				Roles: map[string]Role{
					"embed": {Preferred: "emb1", Fallback: []string{"emb2"}},
				},
			},
			err: "mixing sizes would corrupt vector indexes",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.err)
			}
			if !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("expected error containing %q, got %q", tc.err, err.Error())
			}
		})
	}
}

func TestCovHubKeyGenerationAndSave(t *testing.T) {
	key := GenerateHubKey()
	if !strings.HasPrefix(key, "flt_hub_") || len(key) != len("flt_hub_")+48 {
		t.Fatalf("GenerateHubKey() unexpected format: %s (len %d)", key, len(key))
	}

	tempDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tempDir)
	t.Setenv("HOME", tempDir)

	cfg := &Config{
		Endpoints: []Endpoint{
			{Name: "ep1", URL: "http://127.0.0.1:8001", Model: "m1"},
		},
	}
	if err := cfg.Save(Path()); err != nil {
		t.Fatal(err)
	}

	// EnsureHubKey generates key when none exists
	gotKey, gen, err := EnsureHubKey(cfg)
	if err != nil {
		t.Fatalf("EnsureHubKey failed: %v", err)
	}
	if !gen || !strings.HasPrefix(gotKey, "flt_hub_") {
		t.Fatalf("EnsureHubKey got %q, gen=%v", gotKey, gen)
	}

	// Call EnsureHubKey again, should resolve existing without generating
	gotKey2, gen2, err := EnsureHubKey(cfg)
	if err != nil {
		t.Fatalf("EnsureHubKey second call failed: %v", err)
	}
	if gen2 || gotKey2 != gotKey {
		t.Fatalf("EnsureHubKey second call returned %q (gen=%v), want %q", gotKey2, gen2, gotKey)
	}

	// SaveHubKey directly
	newKey := GenerateHubKey()
	if err := SaveHubKey(newKey); err != nil {
		t.Fatalf("SaveHubKey failed: %v", err)
	}
	loaded, err := Load(Path())
	if err != nil {
		t.Fatal(err)
	}
	if loaded.HubAPIKey != newKey {
		t.Fatalf("SaveHubKey did not persist key to config: got %s, want %s", loaded.HubAPIKey, newKey)
	}
}

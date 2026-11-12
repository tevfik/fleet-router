package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// isolate points every path the package reads at a temp dir, so the real ~/.config and ~/.local are never touched.
func isolate(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	t.Setenv("HOME", d)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(d, "cfg"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(d, "state"))
	t.Setenv("FLEET_CONFIG", "")
	t.Setenv("FLEET_STATS", "")
	return d
}

func bp(b bool) *bool { return &b }

func TestFeature_Validate_EdgeCases(t *testing.T) {
	ok := Endpoint{Name: "a", Node: "n", Model: "m", URL: "http://x"}
	cases := []struct {
		name string
		c    Config
		want string // substring of the error, "" = valid
	}{
		{"valid", Config{Endpoints: []Endpoint{ok}}, ""},
		{"no name", Config{Endpoints: []Endpoint{{Model: "m", URL: "u"}}}, "without a name"},
		{"duplicate", Config{Endpoints: []Endpoint{ok, ok}}, "duplicate endpoint"},
		{"ssh without remote", Config{Endpoints: []Endpoint{{Name: "a", SSH: "h"}}}, "url or (ssh + remote)"},
		{"ssh+remote ok", Config{Endpoints: []Endpoint{{Name: "a", SSH: "h", Remote: "127.0.0.1:1"}}}, ""},
		{"peer bad name", Config{Peers: []Peer{{Name: "a/b", URL: "u"}}}, "peer name"},
		{"peer empty name", Config{Peers: []Peer{{URL: "u"}}}, "peer name"},
		{"peer dup", Config{Peers: []Peer{{Name: "p", URL: "u"}, {Name: "p", URL: "u"}}}, "duplicate peer"},
		{"peer no transport", Config{Peers: []Peer{{Name: "p"}}}, "url or ssh"},
		{"peer bad selector", Config{Peers: []Peer{{Name: "p", URL: "u", Only: []string{"bogus"}}}}, "bad selector"},
		{"peer bad skip", Config{Peers: []Peer{{Name: "p", URL: "u", Skip: []string{"tag:"}}}}, "bad selector"},
		{"unknown peer", Config{Endpoints: []Endpoint{{Name: "a", URL: "u", Peer: "ghost"}}}, "unknown peer"},
		{"tunnel idle off", Config{TunnelIdle: "OFF"}, ""},
		{"tunnel idle never", Config{TunnelIdle: "never"}, ""},
		{"tunnel idle bad", Config{TunnelIdle: "soon"}, "tunnel_idle"},
		{"tunnel idle negative", Config{TunnelIdle: "-5m"}, "tunnel_idle"},
		{"bad strategy", Config{Endpoints: []Endpoint{ok}, Roles: map[string]Role{"r": {Preferred: "a", Strategy: "random"}}}, "unknown strategy"},
		{"fastest ok", Config{Endpoints: []Endpoint{ok}, Roles: map[string]Role{"r": {Preferred: "a", Strategy: "fastest"}}}, ""},
		{"role collides", Config{Endpoints: []Endpoint{ok}, Roles: map[string]Role{"a": {Preferred: "a"}}}, "collides"},
		{"unknown fallback", Config{Endpoints: []Endpoint{ok}, Roles: map[string]Role{"r": {Preferred: "a", Fallback: []string{"z"}}}}, "unknown endpoint"},
		{"bad match", Config{Endpoints: []Endpoint{ok}, Roles: map[string]Role{"r": {Match: []string{"colour:red"}}}}, "bad selector"},
		{"empty role", Config{Endpoints: []Endpoint{ok}, Roles: map[string]Role{"r": {}}}, "needs a preferred"},
		{"match only ok", Config{Endpoints: []Endpoint{ok}, Roles: map[string]Role{"r": {Match: []string{"node:n"}}}}, ""},
		{"embed dims mixed", Config{Endpoints: []Endpoint{
			{Name: "e1", Model: "bge-m3", URL: "u", Dim: 1024},
			{Name: "e2", Model: "nomic-embed-text", URL: "u", Dim: 768},
		}, Roles: map[string]Role{"emb": {Preferred: "e1", Fallback: []string{"e2"}}}}, "dimensions"},
		{"embed dims unknown ok", Config{Endpoints: []Endpoint{
			{Name: "e1", Model: "bge-m3", URL: "u", Dim: 1024},
			{Name: "e2", Model: "nomic-embed-text", URL: "u"},
		}, Roles: map[string]Role{"emb": {Preferred: "e1", Fallback: []string{"e2"}}}}, ""},
	}
	for _, c := range cases {
		err := c.c.Validate()
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: unexpected error %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: error %v, want containing %q", c.name, err, c.want)
		}
	}
}

func TestFeature_SaveLoad_RoundTripNewFields(t *testing.T) {
	d := isolate(t)
	p := filepath.Join(d, "sub", "config.yaml")
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	c := Default()
	c.ID, c.Name = "id-1", "home"
	c.AvoidStrain = bp(false)
	c.MDNS = bp(false)
	c.AutoUpdate = bp(false)
	c.TunnelIdle = "30m"
	c.GPUBandwidth = map[string]float64{"rtx 9090": 2000, "*": 500}
	c.Paired = []Paired{{ID: "pid", Name: "laptop", KeyHash: "abc123", At: at}}
	c.Peers = []Peer{{Name: "dino", URL: "http://dino:4711/v1", Key: "peer-secret", MaxHops: 1, Only: []string{"tag:fast"}}}
	c.Endpoints = []Endpoint{
		{Name: "a", Node: "n", Model: "m", URL: "http://x/v1", Key: "ep-secret", Caps: []string{"chat", "vision"}, Tags: []string{"fast"}},
		{Name: "dino/b", Node: "dino", Model: "b", URL: "http://dino:4711/v1", Peer: "dino", Path: []string{"x", "dino"}},
	}
	c.Roles = map[string]Role{"chat": {Preferred: "a", PreferWarm: bp(true), Strategy: "least-busy"}}
	if err := c.Save(p); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("saved file mode: %v %v", fi, err)
	}
	if _, err := os.Stat(p + ".tmp"); err == nil {
		t.Fatal("temp file left behind")
	}
	got, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.AvoidStrainOn() || got.MDNSOn() || got.AutoUpdateOn() {
		t.Fatal("false switches lost in round trip")
	}
	if got.IdleDuration() != 30*time.Minute {
		t.Fatalf("idle %v", got.IdleDuration())
	}
	if !reflect.DeepEqual(got.GPUBandwidth, c.GPUBandwidth) {
		t.Fatalf("gpu_bandwidth %v", got.GPUBandwidth)
	}
	if len(got.Paired) != 1 || got.Paired[0].KeyHash != "abc123" || !got.Paired[0].At.Equal(at) {
		t.Fatalf("paired %+v", got.Paired)
	}
	if got.Peers[0].Key != "peer-secret" || got.Peers[0].Hops() != 1 {
		t.Fatalf("peer %+v", got.Peers[0])
	}
	if got.Endpoints[0].Key != "ep-secret" || got.Endpoints[1].Peer != "dino" {
		t.Fatalf("endpoints %+v", got.Endpoints)
	}
	if r := got.Roles["chat"]; r.PreferWarm == nil || !*r.PreferWarm {
		t.Fatalf("prefer_warm lost: %+v", r)
	}
	raw, _ := os.ReadFile(p)
	for _, k := range []string{"avoid_strain: false", "gpu_bandwidth:", "key_sha256: abc123", "key: peer-secret"} {
		if !strings.Contains(string(raw), k) {
			t.Errorf("yaml lacks %q:\n%s", k, raw)
		}
	}
}

func TestFeature_JSON_SecretsNeverSerialised(t *testing.T) {
	c := Config{
		AdminToken: "admintok",
		Paired:     []Paired{{ID: "p", Name: "n", KeyHash: "HASHVALUE"}},
		Peers:      []Peer{{Name: "p", URL: "u", Key: "PEERKEY"}},
		Endpoints:  []Endpoint{{Name: "a", URL: "u", Key: "EPKEY"}},
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"admintok", "HASHVALUE", "PEERKEY", "EPKEY", "key_sha256"} {
		if strings.Contains(string(b), s) {
			t.Errorf("JSON leaks %q: %s", s, b)
		}
	}
}

func TestFeature_Load_MissingAndInvalid(t *testing.T) {
	d := isolate(t)
	c, err := Load(filepath.Join(d, "nope.yaml"))
	if err != nil || c.Listen != DefaultListen || c.Roles == nil || c.Discover.Ports == "" {
		t.Fatalf("missing file must give defaults: %+v %v", c, err)
	}
	bad := filepath.Join(d, "bad.yaml")
	os.WriteFile(bad, []byte("endpoints: [this is: not: valid"), 0o600)
	if _, err := Load(bad); err == nil || !strings.Contains(err.Error(), bad) {
		t.Fatalf("parse error should name the file: %v", err)
	}
	inv := filepath.Join(d, "inv.yaml")
	os.WriteFile(inv, []byte("endpoints:\n- name: a\n  model: m\n"), 0o600)
	if _, err := Load(inv); err == nil {
		t.Fatal("invalid config must fail validation on load")
	}
	empty := filepath.Join(d, "roles.yaml")
	os.WriteFile(empty, []byte("roles: null\nendpoints: []\n"), 0o600)
	if c, err := Load(empty); err != nil || c.Roles == nil {
		t.Fatalf("null roles must become an empty map: %v", err)
	}
	if err := (&Config{Endpoints: []Endpoint{{Name: "x"}}}).Save(filepath.Join(d, "x.yaml")); err == nil {
		t.Fatal("Save must refuse an invalid config")
	}
	if _, err := os.Stat(filepath.Join(d, "x.yaml")); err == nil {
		t.Fatal("invalid config must not be written")
	}
}

func TestFeature_SSHHosts_SortedUniqueNoPeerImports(t *testing.T) {
	c := Config{
		Endpoints: []Endpoint{
			{Name: "a", SSH: "zeta"},
			{Name: "b", SSH: "alpha"},
			{Name: "c", SSH: "zeta"},
			{Name: "d", URL: "u"},
			{Name: "e", SSH: "imported-host", Peer: "p"},
		},
		Peers: []Peer{{Name: "p", SSH: "mid"}, {Name: "q", URL: "u"}, {Name: "r", SSH: "alpha"}},
	}
	if got := c.SSHHosts(); !reflect.DeepEqual(got, []string{"alpha", "mid", "zeta"}) {
		t.Fatalf("SSHHosts = %v", got)
	}
	if (&Config{}).SSHHosts() != nil {
		t.Fatal("no hosts expected")
	}
}

func TestFeature_Role_WarmFirstDefaults(t *testing.T) {
	for strat, want := range map[string]bool{"": false, "failover": false, "least-busy": true, "round-robin": true, "fastest": true} {
		if got := (Role{Strategy: strat}).WarmFirst(); got != want {
			t.Errorf("WarmFirst(%q)=%v want %v", strat, got, want)
		}
	}
	if !(Role{PreferWarm: bp(true)}).WarmFirst() || (Role{Strategy: "fastest", PreferWarm: bp(false)}).WarmFirst() {
		t.Fatal("explicit prefer_warm must win")
	}
}

func TestFeature_Switches_DefaultOn(t *testing.T) {
	c := &Config{}
	if !c.AvoidStrainOn() || !c.MDNSOn() || !c.AutoUpdateOn() {
		t.Fatal("all switches default on")
	}
	c.AvoidStrain, c.MDNS, c.AutoUpdate = bp(true), bp(true), bp(true)
	if !c.AvoidStrainOn() || !c.MDNSOn() || !c.AutoUpdateOn() {
		t.Fatal("explicit true")
	}
	c.AvoidStrain, c.MDNS, c.AutoUpdate = bp(false), bp(false), bp(false)
	if c.AvoidStrainOn() || c.MDNSOn() || c.AutoUpdateOn() {
		t.Fatal("explicit false")
	}
}

func TestFeature_IdleDuration_Values(t *testing.T) {
	for in, want := range map[string]time.Duration{"": 10 * time.Minute, "off": 0, "Never": 0, "0": 0, " 1h ": time.Hour, "garbage": 10 * time.Minute, "-1m": 10 * time.Minute} {
		if got := (&Config{TunnelIdle: in}).IdleDuration(); got != want {
			t.Errorf("IdleDuration(%q)=%v want %v", in, got, want)
		}
	}
}

func TestFeature_Selectors_MatchesAndParse(t *testing.T) {
	e := &Endpoint{Name: "a", Node: "dino", Model: "gemma4:26b", Tags: []string{"fast", "gpu"}, Caps: []string{"chat", "vision"}}
	for sel, want := range map[string]bool{
		"tag:fast": true, "tag:slow": false, "node:dino": true, "node:victor": false,
		"cap:vision": true, "cap:audio": false, "model:gemma4*": true, "model:qwen*": false,
		"model:[": false, "bogus": false, "tag:": false, "color:red": false,
	} {
		if got := e.Matches(sel); got != want {
			t.Errorf("Matches(%q)=%v want %v", sel, got, want)
		}
	}
	if k, v, err := ParseSelector("model:a:b"); err != nil || k != "model" || v != "a:b" {
		t.Fatalf("ParseSelector keeps the rest: %q %q %v", k, v, err)
	}
}

func TestFeature_Pool_DirectBeforePeerAndNoDuplicates(t *testing.T) {
	c := &Config{Endpoints: []Endpoint{
		{Name: "remote", Node: "x", Model: "m1", URL: "u", Tags: []string{"t"}, Peer: "p", Path: []string{"a", "p"}},
		{Name: "local", Node: "y", Model: "m2", URL: "u", Tags: []string{"t"}},
		{Name: "pref", Node: "z", Model: "m3", URL: "u", Tags: []string{"t"}},
	}}
	got := names(c.Pool(Role{Preferred: "pref", Fallback: []string{"pref", "missing"}, Match: []string{"tag:t", "node:y"}}))
	if !reflect.DeepEqual(got, []string{"pref", "local", "remote"}) {
		t.Fatalf("Pool = %v", got)
	}
}

func TestFeature_Lookup_Forms(t *testing.T) {
	c := &Config{Endpoints: []Endpoint{
		{Name: "far", Node: "dino", Model: "qwen3:latest", Path: []string{"p"}},
		{Name: "near", Node: "box", Model: "qwen3"},
		{Name: "x", Node: "box", Model: "other"},
	}}
	if got := names(c.Lookup("qwen3")); !reflect.DeepEqual(got, []string{"near", "far"}) {
		t.Fatalf("bare id pool = %v", got)
	}
	if got := names(c.Lookup("dino/qwen3")); !reflect.DeepEqual(got, []string{"far"}) {
		t.Fatalf("node/model = %v", got)
	}
	if got := names(c.Lookup("x")); !reflect.DeepEqual(got, []string{"x"}) {
		t.Fatalf("by name = %v", got)
	}
	if got := c.Lookup("nothing"); len(got) != 0 {
		t.Fatalf("unknown = %v", got)
	}
}

func TestFeature_Endpoint_WireRefCaps(t *testing.T) {
	e := Endpoint{Node: "dino", Model: "m"}
	if e.Wire() != "m" || e.Ref() != "dino/m" {
		t.Fatal("direct wire")
	}
	e.Peer = "p"
	if e.Wire() != "dino/m" {
		t.Fatal("peer endpoints send node/model")
	}
	cases := []struct {
		e    Endpoint
		cap  string
		want bool
	}{
		{Endpoint{Model: "bge-m3"}, "embed", true},
		{Endpoint{Model: "bge-m3"}, "chat", false},
		{Endpoint{Model: "llama3"}, "chat", true},
		{Endpoint{Model: "qwen2.5-vl:7b"}, "vision", true},
		{Endpoint{Model: "llava"}, "audio", false},
		{Endpoint{Model: "qwen2.5-omni"}, "audio", true},
		{Endpoint{Model: "bge-m3", Kind: "chat"}, "chat", true},
		{Endpoint{Model: "x", Kind: "embed"}, "embed", true},
		{Endpoint{Model: "x"}, "tools", false},
		{Endpoint{Model: "bge-m3", Caps: []string{"chat", "embed"}}, "embed", true},
		{Endpoint{Model: "llava", Caps: []string{"chat"}}, "vision", false},
	}
	for _, c := range cases {
		if got := c.e.Has(c.cap); got != c.want {
			t.Errorf("%+v Has(%s)=%v want %v", c.e, c.cap, got, c.want)
		}
	}
	if (&Endpoint{Model: "bge-m3", Caps: []string{"chat", "embed"}}).IsEmbed() {
		t.Fatal("chat+embed caps are not embed-only")
	}
	if !IsEmbedModel("Snowflake-Arctic-Embed") || IsEmbedModel("llama3") {
		t.Fatal("IsEmbedModel")
	}
	if Slug("  Gemma 4: 26B / Q4 ") != "gemma-4-26b-q4" {
		t.Fatalf("Slug = %q", Slug("  Gemma 4: 26B / Q4 "))
	}
}

func TestFeature_Peer_HopsAndTransport(t *testing.T) {
	if (Peer{}).Hops() != 2 || (Peer{MaxHops: 3}).Hops() != 3 {
		t.Fatal("Hops default 2")
	}
	tr := Peer{Name: "p", SSH: "host", KeyEnv: "K", Key: "k"}.Transport()
	if tr.Name != "peer:p" || tr.Remote != DefaultListen || tr.KeyEnv != "K" || tr.Key != "k" {
		t.Fatalf("ssh transport %+v", tr)
	}
	if tr := (Peer{Name: "p", SSH: "h", Remote: "127.0.0.1:9"}).Transport(); tr.Remote != "127.0.0.1:9" {
		t.Fatal("explicit remote kept")
	}
	if tr := (Peer{Name: "p", URL: "http://u"}).Transport(); tr.Remote != "" || tr.URL != "http://u" {
		t.Fatal("url peer gets no remote")
	}
}

func TestFeature_RoleNames_Sorted(t *testing.T) {
	c := &Config{Roles: map[string]Role{"z": {}, "a": {}, "m": {}}}
	if got := c.RoleNames(); !reflect.DeepEqual(got, []string{"a", "m", "z"}) {
		t.Fatalf("RoleNames = %v", got)
	}
}

func TestFeature_History_SnapshotsUndoChain(t *testing.T) {
	d := isolate(t)
	p := filepath.Join(d, "config.yaml")
	if _, err := Undo(p); err == nil {
		t.Fatal("nothing to undo")
	}
	c := Default()
	for _, n := range []string{"one", "two", "three"} {
		c.Name = n
		if err := c.Save(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Save(p); err != nil { // same content: no new snapshot
		t.Fatal(err)
	}
	if n := len(Snapshots(p)); n != 2 {
		t.Fatalf("snapshots = %d, want 2", n)
	}
	for _, want := range []string{"two", "one"} {
		if _, err := Undo(p); err != nil {
			t.Fatal(err)
		}
		got, _ := Load(p)
		if got.Name != want {
			t.Fatalf("after undo Name=%q want %q", got.Name, want)
		}
	}
	if _, err := Undo(p); err == nil {
		t.Fatal("history exhausted")
	}
	// an invalid snapshot is refused and the file left alone
	os.WriteFile(filepath.Join(historyDir(p), "99999999-000000.000000.yaml"), []byte("endpoints:\n- name: a\n"), 0o600)
	if _, err := Undo(p); err == nil || !strings.Contains(err.Error(), "not valid") {
		t.Fatalf("invalid snapshot: %v", err)
	}
	os.WriteFile(filepath.Join(historyDir(p), "99999999-000000.000000.yaml"), []byte(": : :"), 0o600)
	if _, err := Undo(p); err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("unreadable snapshot: %v", err)
	}
	if got, _ := Load(p); got.Name != "one" {
		t.Fatalf("failed undo changed the file: %q", got.Name)
	}
}

func TestFeature_Snapshots_SuffixOrdering(t *testing.T) {
	d := isolate(t)
	p := filepath.Join(d, "config.yaml")
	h := historyDir(p)
	os.MkdirAll(h, 0o700)
	for _, n := range []string{"20260101-000000.000001-1.yaml", "20260101-000000.000001.yaml", "20260101-000000.000000.yaml", "notes.txt"} {
		os.WriteFile(filepath.Join(h, n), nil, 0o600)
	}
	want := []string{"20260101-000000.000000.yaml", "20260101-000000.000001.yaml", "20260101-000000.000001-1.yaml"}
	if got := Snapshots(p); !reflect.DeepEqual(got, want) {
		t.Fatalf("Snapshots = %v", got)
	}
}

func TestFeature_Paths_FollowEnv(t *testing.T) {
	d := isolate(t)
	if Dir() != filepath.Join(d, "cfg", "fleet") || Path() != filepath.Join(d, "cfg", "fleet", "config.yaml") {
		t.Fatalf("Dir/Path = %s %s", Dir(), Path())
	}
	if StatsPath() != filepath.Join(d, "state", "fleet", "stats.json") {
		t.Fatalf("StatsPath = %s", StatsPath())
	}
	t.Setenv("XDG_STATE_HOME", "")
	if StatsPath() != filepath.Join(d, ".local", "state", "fleet", "stats.json") {
		t.Fatalf("StatsPath HOME fallback = %s", StatsPath())
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	if Dir() != filepath.Join(d, ".config", "fleet") {
		t.Fatalf("Dir HOME fallback = %s", Dir())
	}
	cfg := filepath.Join(d, "custom", "fleet.yaml")
	t.Setenv("FLEET_CONFIG", cfg)
	if Path() != cfg || StatsPath() != cfg+".stats.json" {
		t.Fatalf("FLEET_CONFIG: %s %s", Path(), StatsPath())
	}
	stats := filepath.Join(d, "s", "stats.json")
	t.Setenv("FLEET_STATS", stats)
	if StatsPath() != stats {
		t.Fatal("FLEET_STATS wins")
	}
	sd := filepath.Join(d, "s")
	if EvalPath() != filepath.Join(sd, "eval.json") || HWBenchPath() != filepath.Join(sd, "machine.json") || ProbePath() != filepath.Join(sd, "probe.json") {
		t.Fatalf("side files: %s %s %s", EvalPath(), HWBenchPath(), ProbePath())
	}
}

func TestFeature_Secret_EnvThenDotEnvFiles(t *testing.T) {
	d := isolate(t)
	const k = "FLEET_FEATURE_TEST_SECRET"
	t.Setenv(k, "")
	if Secret(k) != "" {
		t.Fatal("no secret anywhere")
	}
	oc := filepath.Join(d, ".config", "opencode")
	os.MkdirAll(oc, 0o700)
	os.WriteFile(filepath.Join(oc, ".env"), []byte(k+"=from-opencode\n"), 0o600)
	if got := Secret(k); got != "from-opencode" {
		t.Fatalf("opencode .env: %q", got)
	}
	os.MkdirAll(Dir(), 0o700)
	os.WriteFile(filepath.Join(Dir(), ".env"), []byte("# comment\nOTHER=1\n  "+k+"=\"quoted value\"  \n"), 0o600)
	if got := Secret(k); got != "quoted value" {
		t.Fatalf("fleet .env must win and be unquoted: %q", got)
	}
	t.Setenv(k, "from-env")
	if got := Secret(k); got != "from-env" {
		t.Fatalf("env must win: %q", got)
	}
	e := Endpoint{KeyEnv: k, Key: "inline"}
	if e.APIKey() != "from-env" {
		t.Fatal("key_env wins over key")
	}
	if (&Endpoint{Key: "inline"}).APIKey() != "inline" {
		t.Fatal("inline key")
	}
	// prefix of another variable must not match
	t.Setenv(k, "")
	os.WriteFile(filepath.Join(Dir(), ".env"), []byte(k+"_LONGER=nope\n"), 0o600)
	os.Remove(filepath.Join(oc, ".env"))
	if got := Secret(k); got != "" {
		t.Fatalf("prefix match: %q", got)
	}
}

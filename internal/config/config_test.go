package config

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testCfg() *Config {
	return &Config{
		Endpoints: []Endpoint{
			{Name: "a", Node: "dino", Model: "gemma4:26b", URL: "http://x", Tags: []string{"fast"}},
			{Name: "b", Node: "victor", Model: "gemma4:26b:latest", URL: "http://x"},
			{Name: "c", Node: "victor", Model: "unsloth/qwen-vl", URL: "http://x", Tags: []string{"fast"}},
			{Name: "d", Node: "dino", Model: "bge-m3", URL: "http://x", Dim: 1024},
		},
		Roles: map[string]Role{},
	}
}

func names(es []Endpoint) (o []string) {
	for _, e := range es {
		o = append(o, e.Name)
	}
	return
}

func TestLookup(t *testing.T) {
	c := testCfg()
	cases := map[string]string{
		"a":                      "a",
		"dino/gemma4:26b":        "a",
		"victor/gemma4:26b":      "b", // :latest is optional on either side
		"gemma4:26b":             "a,b",
		"victor/unsloth/qwen-vl": "c", // the model id itself contains a slash
		"unsloth/qwen-vl":        "c",
		"dino/nothing":           "",
	}
	for ref, want := range cases {
		got := ""
		for i, n := range names(c.Lookup(ref)) {
			if i > 0 {
				got += ","
			}
			got += n
		}
		if got != want {
			t.Errorf("Lookup(%q) = %q, want %q", ref, got, want)
		}
	}
}

func TestPoolAndSelectors(t *testing.T) {
	c := testCfg()
	r := Role{Preferred: "b", Fallback: []string{"a"}, Match: []string{"tag:fast", "model:bge*"}}
	if got := names(c.Pool(r)); len(got) != 4 || got[0] != "b" || got[1] != "a" || got[2] != "c" || got[3] != "d" {
		t.Errorf("pool = %v", got)
	}
	if got := names(c.Pool(Role{Match: []string{"node:dino", "cap:embed"}})); len(got) != 2 || got[0] != "a" || got[1] != "d" {
		t.Errorf("pool = %v", got)
	}
	for _, bad := range []string{"fast", "tag:", "color:red"} {
		if _, _, err := ParseSelector(bad); err == nil {
			t.Errorf("selector %q should be rejected", bad)
		}
	}
	c.Roles["x"] = Role{}
	if c.Validate() == nil {
		t.Error("a role with neither preferred nor match must be invalid")
	}
	c.Roles["x"] = Role{Match: []string{"node:dino"}} // mixes a chat model with a 1024-dim embedder: allowed (only embed dims are compared)
	c.Endpoints = append(c.Endpoints, Endpoint{Name: "e", Node: "dino", Model: "bge-small", URL: "http://x", Dim: 384})
	if c.Validate() == nil {
		t.Error("an embedding pool mixing 1024 and 384 dimensions must be rejected")
	}
}

func TestHistoryAndUndo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	mk := func(listen string) *Config { c := Default(); c.Listen = listen; return c }
	for _, l := range []string{"127.0.0.1:1", "127.0.0.1:2", "127.0.0.1:3"} {
		if err := mk(l).Save(path); err != nil {
			t.Fatal(err)
		}
	}
	if err := mk("127.0.0.1:3").Save(path); err != nil { // identical content: no new snapshot
		t.Fatal(err)
	}
	if n := len(Snapshots(path)); n != 2 {
		t.Fatalf("want 2 snapshots (the replaced :1 and :2), got %d", n)
	}
	for _, want := range []string{"127.0.0.1:2", "127.0.0.1:1"} { // one step back at a time
		if _, err := Undo(path); err != nil {
			t.Fatal(err)
		}
		c, err := Load(path)
		if err != nil || c.Listen != want {
			t.Fatalf("after undo listen = %v (%v), want %s", c.Listen, err, want)
		}
	}
	if _, err := Undo(path); err == nil {
		t.Fatal("nothing left to undo must be an error")
	}
}

func TestHistoryKeepsOnlyTheLatest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	for i := 0; i < keepSnapshots+8; i++ {
		c := Default()
		c.Listen = fmt.Sprintf("127.0.0.1:%d", 2000+i)
		if err := c.Save(path); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if n := len(Snapshots(path)); n != keepSnapshots {
		t.Fatalf("kept %d snapshots, want %d", n, keepSnapshots)
	}
}

func TestPoolWarnings(t *testing.T) {
	c := &Config{
		Endpoints: []Endpoint{
			{Name: "a", Node: "dino", Model: "m1", URL: "http://x"},
			{Name: "b", Node: "dino", Model: "m2", URL: "http://x"},
			{Name: "c", Node: "victor", Model: "m3", URL: "http://x"},
		},
		Roles: map[string]Role{
			"solo":    {Preferred: "a"},
			"onehost": {Preferred: "a", Fallback: []string{"b"}},
			"good":    {Preferred: "a", Fallback: []string{"c"}},
			"tagged":  {Match: []string{"node:victor"}},
		},
	}
	got := strings.Join(c.PoolWarnings(), "\n")
	for _, want := range []string{"role solo has a single model", "role onehost: all 2 models are on dino", "role tagged has a single model"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "role good") {
		t.Errorf("a role with models on two machines must not be flagged:\n%s", got)
	}
}

func TestTunnelIdleSetting(t *testing.T) {
	for in, want := range map[string]time.Duration{"": 10 * time.Minute, "5m": 5 * time.Minute, "90s": 90 * time.Second, "off": 0, "never": 0, "0": 0} {
		if got := (&Config{TunnelIdle: in}).IdleDuration(); got != want {
			t.Errorf("tunnel_idle %q = %v, want %v", in, got, want)
		}
	}
	c := Default()
	c.TunnelIdle = "soon"
	if c.Validate() == nil {
		t.Error("a nonsense tunnel_idle must be rejected")
	}
	c.TunnelIdle = "15m"
	if err := c.Validate(); err != nil {
		t.Errorf("15m rejected: %v", err)
	}
}

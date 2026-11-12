package config

import "testing"

func TestCheckSSHTarget(t *testing.T) {
	for _, ok := range []string{"dino", "user@host", "10.0.0.5", "alice@dino.local", "fe80::1"} {
		if err := CheckSSHTarget(ok); err != nil {
			t.Errorf("%q should be fine: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-oProxyCommand=touch /tmp/x", "-J", "a b", "host;id", "$(id)", "h`id`", "a\nb"} {
		if CheckSSHTarget(bad) == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}

func TestValidateRefusesOptionLikeSSH(t *testing.T) {
	c := &Config{Endpoints: []Endpoint{{Name: "e", SSH: "-oProxyCommand=x", Remote: "127.0.0.1:1"}}}
	if c.Validate() == nil {
		t.Fatal("endpoint ssh")
	}
	c = &Config{Peers: []Peer{{Name: "p", SSH: "-oProxyCommand=x"}}}
	if c.Validate() == nil {
		t.Fatal("peer ssh")
	}
	c = &Config{Discover: Discover{SSH: []string{"-oProxyCommand=x"}}}
	if c.Validate() == nil {
		t.Fatal("discover ssh")
	}
	c = &Config{Nodes: map[string]NodeSpec{"-x": {}}}
	if c.Validate() == nil {
		t.Fatal("node name")
	}
}

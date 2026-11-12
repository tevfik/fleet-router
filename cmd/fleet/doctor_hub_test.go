package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tevfik/fleet-router/internal/board"
	"github.com/tevfik/fleet-router/internal/config"
)

func TestDoctorSecretLinesNeverPrintValuesAndFlagBadModes(t *testing.T) {
	d := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", d)
	t.Setenv("HOME", d)
	t.Setenv("FLEET_API_KEY", "")
	env := filepath.Join(d, "fleet", ".env")
	_ = os.MkdirAll(filepath.Dir(env), 0o700)
	const secret = "doctor-SECRET-value-0123456789"
	_ = os.WriteFile(env, []byte("FLEET_API_KEY="+secret+"\n"), 0o644)
	cfg := &config.Config{HubAPIKey: "cfg-SECRET-9876543210"}
	all := strings.Join(doctorSecretLines(cfg), "\n")
	if strings.Contains(all, secret) || strings.Contains(all, "cfg-SECRET") {
		t.Fatalf("doctor printed a secret:\n%s", all)
	}
	for _, w := range []string{"environment > ", "used from " + env, "mode 644", "chmod 600"} {
		if !strings.Contains(all, w) {
			t.Errorf("missing %q in:\n%s", w, all)
		}
	}
	_ = os.WriteFile(env, nil, 0o600)
	if all := strings.Join(doctorSecretLines(&config.Config{}), "\n"); !strings.Contains(all, "0 bytes") || !strings.Contains(all, "none") {
		t.Errorf("empty .env / no key:\n%s", all)
	}
}

func TestDoctorVersionLinesDevAndDrift(t *testing.T) {
	l := strings.Join(doctorVersionLines("dev", []board.NodeVersion{{Node: "victor", Fleet: "v0.10.1-32-gaaaaaaa"}, {Node: "hub", Fleet: "v0.10.1-50-gbbbbbbb"}}), "\n")
	if !strings.Contains(l, "dev build") || !strings.Contains(l, "victor v0.10.1-32-gaaaaaaa") {
		t.Fatal(l)
	}
}

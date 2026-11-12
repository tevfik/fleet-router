package tui

import (
	"strings"
	"testing"

	"github.com/tevfik/fleet-router/internal/config"
)

func TestTUI_Economy_ViewAndKeys(t *testing.T) {
	t.Setenv("FLEET_CONFIG", t.TempDir()+"/config.yaml")
	t.Setenv("FLEET_LEDGER", t.TempDir()+"/ledger.json")

	tTrue := true
	cfg := config.Config{
		Economy: &config.EconomyConfig{
			Enabled:     &tTrue,
			FairQueuing: &tTrue,
		},
	}
	m := model{
		sc:  scEconomy,
		cfg: cfg,
		w:   120,
	}

	// before the first fetch the screen says so instead of reading the wallet inside View
	if v := m.viewEconomy(); !strings.Contains(v, "loading") {
		t.Fatalf("expected a loading line before the fetch, got: %s", v)
	}
	m.hub = m.readHubInfo() // what fetch() does on its own goroutine

	view := m.viewEconomy()
	if !strings.Contains(view, "CRYPTOGRAPHIC WALLET") {
		t.Fatalf("expected wallet header, got: %s", view)
	}
	if !strings.Contains(view, "Token Economy Master Switch") {
		t.Fatalf("expected master switch, got: %s", view)
	}

	// Test key toggle
	m2, _ := m.keyEconomy("q")
	mod := m2.(model)
	if mod.cfg.Economy == nil {
		t.Fatal("expected economy config")
	}
}

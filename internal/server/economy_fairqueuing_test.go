package server

import (
	"net/http"
	"path/filepath"
	"testing"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/economy"
)

func TestRouting_FairQueuing_WalletPriorityBoost(t *testing.T) {
	d := rtEnv(t)
	ledgerPath := filepath.Join(d, "ledger.json")
	t.Setenv("FLEET_LEDGER", ledgerPath)

	ledger, err := economy.LoadLedger(ledgerPath)
	if err != nil {
		t.Fatalf("LoadLedger: %v", err)
	}

	walletAddr := "0x1111222233334444555566667777888899990000"
	_, err = ledger.Deposit(walletAddr, 10000) // 10,000 credits -> boost = min(15, 10000/1000) = 10
	if err != nil {
		t.Fatalf("Deposit: %v", err)
	}
	if err := ledger.Save(ledgerPath); err != nil {
		t.Fatalf("Save: %v", err)
	}

	up := rtNewUp(t, "up")
	s := rtServer(t, &config.Config{
		Endpoints: []config.Endpoint{rtEp("up", "local", up.URL(), "test-model")},
		Roles: map[string]config.Role{
			"default": {Preferred: "up", Priority: 5},
		},
	})

	rec := rtDo(t, s, "POST", "/v1/chat/completions", map[string]any{
		"model": "default",
		"messages": []any{
			map[string]any{"role": "user", "content": "hello economy"},
		},
	}, func(r *http.Request) {
		r.Header.Set("X-Wallet-Address", walletAddr)
	})
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if up.hits() != 1 {
		t.Fatalf("expected 1 hit on upstream, got %d", up.hits())
	}
}

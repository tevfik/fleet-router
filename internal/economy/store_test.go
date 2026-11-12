package economy

import (
	"path/filepath"
	"testing"
)

func TestLedgerPersistence(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "economy.json")

	// 1. Save state
	l1 := NewLedger()
	l1.Deposit("0xabcd", 7500)
	l1.SetShareConfig("node-1", ShareConfig{
		Enabled:         true,
		PricePerMillion: 1200,
	})

	if err := l1.Save(path); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// 2. Load state
	l2, err := LoadLedger(path)
	if err != nil {
		t.Fatalf("LoadLedger failed: %v", err)
	}

	acc, err := l2.GetAccount("0xabcd")
	if err != nil || acc.Balance != 7500 {
		t.Errorf("expected 7500 balance, got %v (err=%v)", acc, err)
	}

	sc := l2.GetShareConfig("node-1")
	if !sc.Enabled || sc.PricePerMillion != 1200 {
		t.Errorf("share config mismatch after reload: %+v", sc)
	}
}

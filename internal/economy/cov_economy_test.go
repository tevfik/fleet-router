package economy

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/crypto"
)

func TestCovEconomyDefaultsAndPaths(t *testing.T) {
	// DefaultStatePath
	p := DefaultStatePath()
	if !strings.HasSuffix(p, filepath.Join(".config", "fleet", "economy.json")) {
		t.Fatalf("DefaultStatePath() unexpected: %s", p)
	}

	// DefaultShareConfig
	cfg := DefaultShareConfig()
	if cfg.Enabled {
		t.Error("DefaultShareConfig.Enabled should be false")
	}
	if cfg.PricePerMillion != 1000 {
		t.Errorf("DefaultShareConfig.PricePerMillion got %d, want 1000", cfg.PricePerMillion)
	}

	// GetShareConfig fallback to default
	l := NewLedger()
	nodeCfg := l.GetShareConfig("nonexistent-node")
	if nodeCfg.PricePerMillion != 1000 {
		t.Errorf("GetShareConfig fallback got %d, want 1000", nodeCfg.PricePerMillion)
	}
}

func TestCovCalculateCostAndGetAccount(t *testing.T) {
	// Negative or zero tokens
	if c := CalculateCost(1000, 0, 0); c != 0 {
		t.Errorf("CalculateCost(1000, 0, 0) got %d, want 0", c)
	}
	if c := CalculateCost(-500, 100, 100); c != 0 {
		t.Errorf("CalculateCost(-500, 100, 100) got %d, want 0", c)
	}
	// Small tokens rounding up to 1
	if c := CalculateCost(10, 1, 0); c != 1 {
		t.Errorf("CalculateCost(10, 1, 0) got %d, want 1 (minimum)", c)
	}

	// GetAccount
	l := NewLedger()
	if _, err := l.GetAccount("missing"); err != ErrAccountNotFound {
		t.Fatalf("GetAccount(missing) got %v, want ErrAccountNotFound", err)
	}
	l.Deposit("acc1", 500)
	acc, err := l.GetAccount("acc1")
	if err != nil || acc.Balance != 500 {
		t.Fatalf("GetAccount(acc1) got %+v, err: %v", acc, err)
	}
}

func TestCovHoldAndRelease(t *testing.T) {
	l := NewLedger()
	consumer := "consumer-1"

	// Negative or zero hold amount
	if _, err := l.Hold(consumer, 0); err != ErrInvalidAmount {
		t.Errorf("Hold(0) got %v, want ErrInvalidAmount", err)
	}
	if _, err := l.Hold(consumer, -100); err != ErrInvalidAmount {
		t.Errorf("Hold(-100) got %v, want ErrInvalidAmount", err)
	}

	// Insufficient balance (account doesn't exist)
	if _, err := l.Hold(consumer, 100); err != ErrInsufficientBalance {
		t.Errorf("Hold without account got %v, want ErrInsufficientBalance", err)
	}

	// Deposit funds
	if _, err := l.Deposit(consumer, 500); err != nil {
		t.Fatal(err)
	}

	// Insufficient balance (more than deposited)
	if _, err := l.Hold(consumer, 600); err != ErrInsufficientBalance {
		t.Errorf("Hold(600) with 500 balance got %v, want ErrInsufficientBalance", err)
	}

	// Valid hold
	holdID, err := l.Hold(consumer, 300)
	if err != nil {
		t.Fatalf("Hold(300) failed: %v", err)
	}
	acc, _ := l.GetAccount(consumer)
	if acc.Balance != 200 || acc.Locked != 300 {
		t.Fatalf("after hold: balance=%d locked=%d", acc.Balance, acc.Locked)
	}

	// Release hold
	if err := l.ReleaseHold("nonexistent-hold"); err != ErrHoldNotFound {
		t.Errorf("ReleaseHold(nonexistent) got %v, want ErrHoldNotFound", err)
	}

	if err := l.ReleaseHold(holdID); err != nil {
		t.Fatalf("ReleaseHold failed: %v", err)
	}
	acc, _ = l.GetAccount(consumer)
	if acc.Balance != 500 || acc.Locked != 0 {
		t.Fatalf("after release: balance=%d locked=%d", acc.Balance, acc.Locked)
	}
}

func TestCovSettleReceiptNegativeAndExcessiveValues(t *testing.T) {
	providerId, _ := crypto.GenerateIdentity()
	consumerId, _ := crypto.GenerateIdentity()

	l := NewLedger()
	l.Deposit(consumerId.Address(), 1000)

	// nil receipt
	if err := l.SettleReceipt(nil, ""); err == nil {
		t.Fatal("SettleReceipt(nil) should fail")
	}

	// Negative values
	makeReceipt := func(cost int64, prompt, comp int) *InferenceReceipt {
		r := &InferenceReceipt{
			ReceiptID:        fmt.Sprintf("rcpt_%d", time.Now().UnixNano()),
			RequestID:        "req_1",
			ConsumerAddress:  consumerId.Address(),
			Model:            "model-1",
			PromptTokens:     prompt,
			CompletionTokens: comp,
			CostCredits:      cost,
			Timestamp:        time.Now().Unix(),
			Nonce:            12345,
		}
		r.Sign(providerId)
		return r
	}

	// 1. Negative cost
	rNegCost := makeReceipt(-50, 10, 10)
	if err := l.SettleReceipt(rNegCost, ""); err != ErrInvalidReceipt {
		t.Errorf("SettleReceipt negative cost got %v, want ErrInvalidReceipt", err)
	}

	// 2. Negative prompt tokens
	rNegPrompt := makeReceipt(50, -10, 10)
	if err := l.SettleReceipt(rNegPrompt, ""); err != ErrInvalidReceipt {
		t.Errorf("SettleReceipt negative prompt tokens got %v, want ErrInvalidReceipt", err)
	}

	// 3. Negative completion tokens
	rNegComp := makeReceipt(50, 10, -5)
	if err := l.SettleReceipt(rNegComp, ""); err != ErrInvalidReceipt {
		t.Errorf("SettleReceipt negative completion tokens got %v, want ErrInvalidReceipt", err)
	}
}

func TestCovSignatureVerificationFailures(t *testing.T) {
	providerId, _ := crypto.GenerateIdentity()
	consumerId, _ := crypto.GenerateIdentity()
	attackerId, _ := crypto.GenerateIdentity()

	l := NewLedger()

	// 1. Corrupted signature hex
	rBadHex := &InferenceReceipt{
		ReceiptID:         "r1",
		ProviderAddress:   providerId.Address(),
		ProviderPubKey:    providerId.HexPublicKey(),
		ConsumerAddress:   consumerId.Address(),
		CostCredits:       10,
		ProviderSignature: "not-a-valid-hex-signature!",
	}
	if valid, err := rBadHex.Verify(); valid || err == nil {
		t.Errorf("Verify bad signature hex got valid=%v, err=%v", valid, err)
	}
	if err := l.SettleReceipt(rBadHex, ""); err != ErrInvalidReceiptSignature {
		t.Errorf("SettleReceipt bad hex sig got %v, want ErrInvalidReceiptSignature", err)
	}

	// 2. Bad public key (wrong length / unparseable)
	rBadPub := &InferenceReceipt{
		ReceiptID:         "r2",
		ProviderAddress:   providerId.Address(),
		ProviderPubKey:    "0x1234", // too short for ed25519
		ConsumerAddress:   consumerId.Address(),
		CostCredits:       10,
		ProviderSignature: hex.EncodeToString(make([]byte, 64)),
	}
	if valid, err := rBadPub.Verify(); valid || err == nil {
		t.Errorf("Verify bad pubkey got valid=%v, err=%v", valid, err)
	}

	// 3. Provider Address mismatch (key belongs to attacker, but address claims to be provider)
	rSpoofed := &InferenceReceipt{
		ReceiptID:       "r3",
		ProviderAddress: providerId.Address(), // claims to be provider
		ConsumerAddress: consumerId.Address(),
		CostCredits:     10,
	}
	rSpoofed.Sign(attackerId)                       // signed by attacker
	rSpoofed.ProviderAddress = providerId.Address() // overwrite address
	if valid, _ := rSpoofed.Verify(); valid {
		t.Error("Verify should fail when pubkey does not match ProviderAddress")
	}

	// 4. Forged digest / tampered receipt after signing
	rTampered := &InferenceReceipt{
		ReceiptID:       "r4",
		ProviderAddress: providerId.Address(),
		ConsumerAddress: consumerId.Address(),
		CostCredits:     10,
	}
	rTampered.Sign(providerId)
	rTampered.CostCredits = 999999 // tampered after sign
	if valid, _ := rTampered.Verify(); valid {
		t.Error("Verify should fail when receipt fields are tampered")
	}
}

func TestCovSettleReceiptHoldMismatchAndExcessiveCosts(t *testing.T) {
	providerId, _ := crypto.GenerateIdentity()
	consumerId, _ := crypto.GenerateIdentity()
	otherConsumerId, _ := crypto.GenerateIdentity()

	l := NewLedger()
	l.Deposit(consumerId.Address(), 1000)
	l.Deposit(otherConsumerId.Address(), 1000)

	// Create hold for other consumer
	holdOther, err := l.Hold(otherConsumerId.Address(), 200)
	if err != nil {
		t.Fatal(err)
	}

	// Receipt for consumerId
	r := &InferenceReceipt{
		ReceiptID:       "r_settle_1",
		RequestID:       "req_1",
		ProviderAddress: providerId.Address(),
		ConsumerAddress: consumerId.Address(),
		CostCredits:     150,
		Timestamp:       time.Now().Unix(),
	}
	r.Sign(providerId)

	// 1. Hold mismatch (someone else's hold)
	if err := l.SettleReceipt(r, holdOther); err != ErrHoldMismatch {
		t.Errorf("SettleReceipt with mismatched hold got %v, want ErrHoldMismatch", err)
	}

	// 2. Expired hold
	expiredHoldID := "hold_expired"
	l.activeHolds[expiredHoldID] = &Hold{
		HoldID:          expiredHoldID,
		ConsumerAddress: consumerId.Address(),
		EstimatedCost:   100,
		ExpiresAt:       time.Now().Add(-1 * time.Hour), // expired
	}
	if err := l.SettleReceipt(r, expiredHoldID); err != ErrHoldNotFound {
		t.Errorf("SettleReceipt with expired hold got %v, want ErrHoldNotFound", err)
	}

	// 3. Self-hosted execution with hold refund
	rSelf := &InferenceReceipt{
		ReceiptID:       "r_self_1",
		ProviderAddress: consumerId.Address(),
		ConsumerAddress: consumerId.Address(),
		CostCredits:     500,
		Timestamp:       time.Now().Unix(),
	}
	rSelf.Sign(consumerId)

	holdSelf, _ := l.Hold(consumerId.Address(), 300)
	accBefore, _ := l.GetAccount(consumerId.Address())
	if err := l.SettleReceipt(rSelf, holdSelf); err != nil {
		t.Fatalf("self-hosted settlement failed: %v", err)
	}
	accAfter, _ := l.GetAccount(consumerId.Address())
	// Hold was refunded, no compute cost charged
	if accAfter.Balance != accBefore.Balance+300 || accAfter.Locked != 0 {
		t.Fatalf("self-hosted refund failed: before=%+v after=%+v", accBefore, accAfter)
	}

	// Replay attack check
	if err := l.SettleReceipt(rSelf, ""); err != ErrReceiptAlreadySettled {
		t.Errorf("replaying settled receipt got %v, want ErrReceiptAlreadySettled", err)
	}

	// 4. Remote settlement with cost exceeding hold AND exceeding remaining balance
	consumer2, _ := crypto.GenerateIdentity()
	l.Deposit(consumer2.Address(), 100)           // total balance 100
	holdLow, _ := l.Hold(consumer2.Address(), 80) // 20 left in balance, 80 locked

	rExcessive := &InferenceReceipt{
		ReceiptID:       "r_excessive",
		ProviderAddress: providerId.Address(),
		ConsumerAddress: consumer2.Address(),
		CostCredits:     500, // cost 500 > hold 80 + balance 20
		Timestamp:       time.Now().Unix(),
	}
	rExcessive.Sign(providerId)
	if err := l.SettleReceipt(rExcessive, holdLow); err != ErrInsufficientBalance {
		t.Errorf("settling excessive cost got %v, want ErrInsufficientBalance", err)
	}
}

func TestCovStoreLoadAndSaveEdgeCases(t *testing.T) {
	tempDir := t.TempDir()
	p := filepath.Join(tempDir, "economy.json")

	// 1. Load non-existent file -> empty ledger, no error
	l, err := LoadLedger(p)
	if err != nil {
		t.Fatalf("LoadLedger(nonexistent) failed: %v", err)
	}
	if len(l.accounts) != 0 {
		t.Errorf("expected 0 accounts in fresh ledger, got %d", len(l.accounts))
	}

	// 2. Corrupt JSON in file -> moved aside to .corrupt-*, fresh ledger returned
	if err := os.WriteFile(p, []byte("{bad-json: broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	lCorrupt, err := LoadLedger(p)
	if err != nil {
		t.Fatalf("LoadLedger on corrupt JSON should move aside and succeed, got err: %v", err)
	}
	if len(lCorrupt.accounts) != 0 {
		t.Errorf("expected empty ledger from corrupt file recovery")
	}

	// 3. Save with noSave set should refuse
	l.noSave = fmt.Errorf("read-only locked")
	if err := l.Save(p); err == nil || !strings.Contains(err.Error(), "read-only locked") {
		t.Fatalf("Save with noSave set should return error, got %v", err)
	}

	// 4. Save and Load roundtrip restores locked funds to balance
	lNormal := NewLedger()
	acc := lNormal.GetOrCreateAccount("locked-acc")
	acc.Balance = 100
	acc.Locked = 50
	lNormal.SetShareConfig("node-1", ShareConfig{Enabled: true, PricePerMillion: 2500})

	if err := lNormal.Save(p); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	lLoaded, err := LoadLedger(p)
	if err != nil {
		t.Fatalf("LoadLedger failed: %v", err)
	}
	loadedAcc, err := lLoaded.GetAccount("locked-acc")
	if err != nil {
		t.Fatal(err)
	}
	// Locked was 50, after reload should be refunded to Balance (100 + 50 = 150)
	if loadedAcc.Balance != 150 || loadedAcc.Locked != 0 {
		t.Fatalf("expected locked funds refunded: balance=%d locked=%d", loadedAcc.Balance, loadedAcc.Locked)
	}
	loadedShare := lLoaded.GetShareConfig("node-1")
	if !loadedShare.Enabled || loadedShare.PricePerMillion != 2500 {
		t.Fatalf("share config mismatch: %+v", loadedShare)
	}
}

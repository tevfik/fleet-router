package economy

import (
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/crypto"
)

func TestDepositAndAccountBalance(t *testing.T) {
	ledger := NewLedger()
	addr := "0x1111222233334444555566667777888899990000"

	acc, err := ledger.Deposit(addr, 5000)
	if err != nil {
		t.Fatalf("deposit failed: %v", err)
	}
	if acc.Balance != 5000 {
		t.Errorf("expected balance 5000, got %d", acc.Balance)
	}

	// Invalid deposit
	_, err = ledger.Deposit(addr, -100)
	if err == nil {
		t.Errorf("negative deposit should fail")
	}
}

func TestSelfHostingIsFree(t *testing.T) {
	ledger := NewLedger()
	nodeId, _ := crypto.GenerateIdentity()
	addr := nodeId.Address()

	// Initial deposit
	ledger.Deposit(addr, 1000)

	// Hold for inference
	holdID, err := ledger.Hold(addr, 200)
	if err != nil {
		t.Fatalf("hold failed: %v", err)
	}

	receipt := &InferenceReceipt{
		ReceiptID:        "rec_self_001",
		RequestID:        "req_001",
		ProviderAddress:  addr, // Same as consumer!
		ConsumerAddress:  addr,
		Model:            "deepseek-v4",
		PromptTokens:     500,
		CompletionTokens: 200,
		CostCredits:      70,
		Timestamp:        time.Now().Unix(),
		Nonce:            1,
	}
	receipt.Sign(nodeId)

	// Settle
	err = ledger.SettleReceipt(receipt, holdID)
	if err != nil {
		t.Fatalf("self-host settlement failed: %v", err)
	}

	acc, _ := ledger.GetAccount(addr)
	if acc.Balance != 1000 {
		t.Errorf("self-hosted compute should be free! Expected 1000 balance, got %d", acc.Balance)
	}
	if acc.Locked != 0 {
		t.Errorf("hold was not refunded, locked is %d", acc.Locked)
	}
}

func TestRemoteComputeSettlement(t *testing.T) {
	ledger := NewLedger()
	providerId, _ := crypto.GenerateIdentity()
	consumerId, _ := crypto.GenerateIdentity()

	provAddr := providerId.Address()
	consAddr := consumerId.Address()

	// Consumer has 1000 credits, Provider has 0
	ledger.Deposit(consAddr, 1000)

	// Hold 300 credits
	holdID, err := ledger.Hold(consAddr, 300)
	if err != nil {
		t.Fatalf("hold failed: %v", err)
	}

	consAcc, _ := ledger.GetAccount(consAddr)
	if consAcc.Balance != 700 || consAcc.Locked != 300 {
		t.Errorf("expected 700 balance and 300 locked, got bal=%d locked=%d", consAcc.Balance, consAcc.Locked)
	}

	// Cost ends up being 250 credits
	receipt := &InferenceReceipt{
		ReceiptID:        "rec_remote_001",
		RequestID:        "req_remote_001",
		ProviderAddress:  provAddr,
		ConsumerAddress:  consAddr,
		Model:            "qwen2.5-coder",
		PromptTokens:     1500,
		CompletionTokens: 1000,
		CostCredits:      250,
		Timestamp:        time.Now().Unix(),
		Nonce:            42,
	}
	receipt.Sign(providerId)

	err = ledger.SettleReceipt(receipt, holdID)
	if err != nil {
		t.Fatalf("remote settlement failed: %v", err)
	}

	// Consumer should have 1000 - 250 = 750 (50 refunded from 300 hold)
	consPost, _ := ledger.GetAccount(consAddr)
	if consPost.Balance != 750 {
		t.Errorf("expected consumer balance 750, got %d", consPost.Balance)
	}
	if consPost.Locked != 0 {
		t.Errorf("expected 0 locked after settle, got %d", consPost.Locked)
	}
	if consPost.TotalSpent != 250 {
		t.Errorf("expected TotalSpent 250, got %d", consPost.TotalSpent)
	}

	// Provider should have earned 250 credits
	provPost, _ := ledger.GetAccount(provAddr)
	if provPost.Balance != 250 || provPost.TotalEarned != 250 {
		t.Errorf("expected provider balance 250, got %d (earned=%d)", provPost.Balance, provPost.TotalEarned)
	}
}

func TestReplayAttackPrevention(t *testing.T) {
	ledger := NewLedger()
	providerId, _ := crypto.GenerateIdentity()
	consumerId, _ := crypto.GenerateIdentity()

	ledger.Deposit(consumerId.Address(), 1000)

	receipt := &InferenceReceipt{
		ReceiptID:        "rec_replay_001",
		RequestID:        "req_replay_001",
		ProviderAddress:  providerId.Address(),
		ConsumerAddress:  consumerId.Address(),
		Model:            "llama-3.3-70b",
		PromptTokens:     100,
		CompletionTokens: 100,
		CostCredits:      50,
		Timestamp:        time.Now().Unix(),
		Nonce:            101,
	}
	receipt.Sign(providerId)

	// First settlement succeeds
	err := ledger.SettleReceipt(receipt, "")
	if err != nil {
		t.Fatalf("first settlement failed: %v", err)
	}

	// Replay attempt must fail!
	err = ledger.SettleReceipt(receipt, "")
	if err != ErrReceiptAlreadySettled {
		t.Fatalf("expected ErrReceiptAlreadySettled on replay, got: %v", err)
	}
}

func TestTamperedReceiptRejection(t *testing.T) {
	ledger := NewLedger()
	providerId, _ := crypto.GenerateIdentity()
	consumerId, _ := crypto.GenerateIdentity()

	receipt := &InferenceReceipt{
		ReceiptID:        "rec_tamper_001",
		RequestID:        "req_tamper_001",
		ProviderAddress:  providerId.Address(),
		ConsumerAddress:  consumerId.Address(),
		Model:            "deepseek-v4",
		PromptTokens:     100,
		CompletionTokens: 100,
		CostCredits:      50,
		Timestamp:        time.Now().Unix(),
		Nonce:            77,
	}
	receipt.Sign(providerId)

	// Malicious actor tampers with CostCredits after signing!
	receipt.CostCredits = 5000

	err := ledger.SettleReceipt(receipt, "")
	if err != ErrInvalidReceiptSignature {
		t.Fatalf("tampered receipt must be rejected with ErrInvalidReceiptSignature, got: %v", err)
	}
}

func TestInsufficientBalanceRejection(t *testing.T) {
	ledger := NewLedger()
	consumerId, _ := crypto.GenerateIdentity()

	ledger.Deposit(consumerId.Address(), 50)

	// Attempting to hold 100 with only 50 available
	_, err := ledger.Hold(consumerId.Address(), 100)
	if err != ErrInsufficientBalance {
		t.Fatalf("expected ErrInsufficientBalance, got %v", err)
	}
}

func TestCalculateCost(t *testing.T) {
	// 1000 credits per 1M tokens -> 1 token = 0.001 credit
	// 500,000 tokens = 500 credits
	cost := CalculateCost(1000, 250_000, 250_000)
	if cost != 500 {
		t.Errorf("expected cost 500, got %d", cost)
	}

	// Tiny token count gets rounded up to minimum 1 credit
	minCost := CalculateCost(1000, 10, 10)
	if minCost != 1 {
		t.Errorf("expected minimum cost 1, got %d", minCost)
	}

	// 0 tokens = 0 cost
	zeroCost := CalculateCost(1000, 0, 0)
	if zeroCost != 0 {
		t.Errorf("expected 0 cost, got %d", zeroCost)
	}
}

func TestShareConfigManagement(t *testing.T) {
	ledger := NewLedger()
	node := "dino"

	cfg := ShareConfig{
		Enabled:             true,
		PricePerMillion:     2500,
		MaxIdleVRAMMiB:      32768,
		AllowedModels:       []string{"deepseek-v4.1", "qwen2.5-coder"},
		AutoStopOnLocalLoad: true,
	}

	ledger.SetShareConfig(node, cfg)
	retrieved := ledger.GetShareConfig(node)

	if !retrieved.Enabled || retrieved.PricePerMillion != 2500 {
		t.Errorf("share config mismatch: %+v", retrieved)
	}
}

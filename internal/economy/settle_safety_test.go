package economy

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/crypto"
)

func signedReceipt(t *testing.T, id *crypto.Identity, consumer string, cost int64) *InferenceReceipt {
	t.Helper()
	r := &InferenceReceipt{ReceiptID: "rec-" + t.Name() + "-" + consumer[:6], RequestID: "q", ConsumerAddress: consumer, Model: "m", CostCredits: cost, Timestamp: 1, Nonce: 1}
	r.Sign(id)
	return r
}

// Signing with one's own key while naming someone else as the provider must not verify.
func TestReceiptWithAnotherProvidersAddressIsRefused(t *testing.T) {
	l := NewLedger()
	attacker, _ := crypto.GenerateIdentity()
	victim, _ := crypto.GenerateIdentity()
	consumer, _ := crypto.GenerateIdentity()
	_, _ = l.Deposit(consumer.Address(), 100)
	r := &InferenceReceipt{ReceiptID: "forged", ConsumerAddress: consumer.Address(), CostCredits: 50, Timestamp: 1}
	r.Sign(attacker)
	r.ProviderAddress = victim.Address() // not the signer's address: the digest changes, so re-sign over the lie
	r.ProviderSignature = ""
	sig := attacker.Sign(r.Digest())
	r.ProviderSignature = hexOf(sig)
	if err := l.SettleReceipt(r, ""); !errors.Is(err, ErrInvalidReceiptSignature) {
		t.Fatalf("a receipt whose provider address is not its signer's must be refused, got %v", err)
	}
	if a, _ := l.GetAccount(consumer.Address()); a.Balance != 100 {
		t.Fatalf("nothing may be charged: %d", a.Balance)
	}
}

func TestNegativeCostIsRefused(t *testing.T) {
	l := NewLedger()
	prov, _ := crypto.GenerateIdentity()
	cons, _ := crypto.GenerateIdentity()
	_, _ = l.Deposit(cons.Address(), 10)
	if err := l.SettleReceipt(signedReceipt(t, prov, cons.Address(), -1000), ""); !errors.Is(err, ErrInvalidReceipt) {
		t.Fatalf("got %v", err)
	}
}

func TestHoldOfAnotherConsumerIsRefusedAndKept(t *testing.T) {
	l := NewLedger()
	prov, _ := crypto.GenerateIdentity()
	a, _ := crypto.GenerateIdentity()
	b, _ := crypto.GenerateIdentity()
	_, _ = l.Deposit(a.Address(), 500)
	_, _ = l.Deposit(b.Address(), 500)
	hold, _ := l.Hold(a.Address(), 300)
	err := l.SettleReceipt(signedReceipt(t, prov, b.Address(), 100), hold)
	if !errors.Is(err, ErrHoldMismatch) {
		t.Fatalf("got %v", err)
	}
	if _, ok := l.activeHolds[hold]; !ok {
		t.Fatal("the refused settlement must not consume A's hold")
	}
}

func TestExpiredHoldIsRefused(t *testing.T) {
	l := NewLedger()
	prov, _ := crypto.GenerateIdentity()
	a, _ := crypto.GenerateIdentity()
	_, _ = l.Deposit(a.Address(), 500)
	hold, _ := l.Hold(a.Address(), 300)
	l.activeHolds[hold].ExpiresAt = time.Now().Add(-time.Second)
	if err := l.SettleReceipt(signedReceipt(t, prov, a.Address(), 100), hold); !errors.Is(err, ErrHoldNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestCostAboveHoldBeyondBalanceChangesNothing(t *testing.T) {
	l := NewLedger()
	prov, _ := crypto.GenerateIdentity()
	a, _ := crypto.GenerateIdentity()
	_, _ = l.Deposit(a.Address(), 100)
	hold, _ := l.Hold(a.Address(), 100) // balance 0, locked 100
	if err := l.SettleReceipt(signedReceipt(t, prov, a.Address(), 5000), hold); !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("got %v", err)
	}
	if acc, _ := l.GetAccount(a.Address()); acc.Balance != 0 || acc.Locked != 100 {
		t.Fatalf("state changed: %+v", acc)
	}
	if _, ok := l.activeHolds[hold]; !ok {
		t.Fatal("the hold must stay")
	}
}

// Holds are not persisted: credits locked at a restart go back to the balance.
func TestLockedCreditsReturnAfterReload(t *testing.T) {
	p := filepath.Join(t.TempDir(), "e.json")
	l := NewLedger()
	_, _ = l.Deposit("0xa", 500)
	if _, err := l.Hold("0xa", 200); err != nil {
		t.Fatal(err)
	}
	if err := l.Save(p); err != nil {
		t.Fatal(err)
	}
	l2, err := LoadLedger(p)
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := l2.GetAccount("0xa"); a.Balance != 500 || a.Locked != 0 {
		t.Fatalf("%+v", a)
	}
}

func hexOf(b []byte) string {
	const d = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, d[c>>4], d[c&15])
	}
	return string(out)
}

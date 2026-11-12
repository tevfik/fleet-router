package crypto

import (
	"crypto/ed25519"
	"path/filepath"
	"testing"
)

func TestIdentityGenerationAndSigning(t *testing.T) {
	id, err := GenerateIdentity()
	if err != nil {
		t.Fatalf("GenerateIdentity failed: %v", err)
	}

	if len(id.PublicKey) != ed25519.PublicKeySize {
		t.Errorf("expected pubkey size %d, got %d", ed25519.PublicKeySize, len(id.PublicKey))
	}

	addr := id.Address()
	if len(addr) != 42 || addr[:2] != "0x" {
		t.Errorf("invalid address format: %s", addr)
	}

	payload := []byte("inference:req-1234:prompt=100:completion=250")
	sig := id.Sign(payload)

	// Valid verification
	valid, err := VerifySignature(id.HexPublicKey(), payload, sig)
	if err != nil || !valid {
		t.Fatalf("signature verification failed: valid=%v, err=%v", valid, err)
	}

	// Tampered payload
	tampered := []byte("inference:req-1234:prompt=100:completion=999999")
	validTampered, err := VerifySignature(id.HexPublicKey(), tampered, sig)
	if err != nil {
		t.Fatalf("tampered check error: %v", err)
	}
	if validTampered {
		t.Errorf("tampered payload must not verify!")
	}

	// Wrong public key
	other, _ := GenerateIdentity()
	validOther, _ := VerifySignature(other.HexPublicKey(), payload, sig)
	if validOther {
		t.Errorf("signature must not verify under another public key!")
	}
}

func TestLoadOrGenerateIdentity(t *testing.T) {
	tmpDir := t.TempDir()
	keyPath := filepath.Join(tmpDir, "node.key")

	// 1. Generate & persist
	id1, err := LoadOrGenerateIdentity(keyPath)
	if err != nil {
		t.Fatalf("first load failed: %v", err)
	}

	// 2. Load existing
	id2, err := LoadOrGenerateIdentity(keyPath)
	if err != nil {
		t.Fatalf("second load failed: %v", err)
	}

	if id1.HexPublicKey() != id2.HexPublicKey() {
		t.Errorf("expected loaded key to match: %s vs %s", id1.HexPublicKey(), id2.HexPublicKey())
	}
	if id1.Address() != id2.Address() {
		t.Errorf("expected loaded address to match: %s vs %s", id1.Address(), id2.Address())
	}
}

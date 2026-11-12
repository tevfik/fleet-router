package crypto

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOrGenerateIdentity_DefaultPath(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	id1, err := LoadOrGenerateIdentity("")
	if err != nil {
		t.Fatalf("LoadOrGenerateIdentity with empty path failed: %v", err)
	}

	expectedPath := filepath.Join(fakeHome, ".config", "fleet", "node_identity.key")
	if _, err := os.Stat(expectedPath); err != nil {
		t.Fatalf("expected key file to be created at %s, err: %v", expectedPath, err)
	}

	// Load again with empty path, should load the existing key
	id2, err := LoadOrGenerateIdentity("")
	if err != nil {
		t.Fatalf("second LoadOrGenerateIdentity failed: %v", err)
	}
	if id1.HexPublicKey() != id2.HexPublicKey() {
		t.Errorf("expected same public key, got %s vs %s", id1.HexPublicKey(), id2.HexPublicKey())
	}
}

func TestLoadOrGenerateIdentity_CorruptedFileRegenerates(t *testing.T) {
	tmpDir := t.TempDir()
	keyPath := filepath.Join(tmpDir, "corrupted.key")

	// Write short/invalid content
	if err := os.WriteFile(keyPath, []byte("too short"), 0600); err != nil {
		t.Fatalf("failed to write corrupted file: %v", err)
	}

	id, err := LoadOrGenerateIdentity(keyPath)
	if err != nil {
		t.Fatalf("expected regeneration on corrupted file, got err: %v", err)
	}

	data, err := os.ReadFile(keyPath)
	if err != nil || len(data) != 64 {
		t.Fatalf("expected file to be rewritten with 64 bytes, got len %d, err %v", len(data), err)
	}
	if id == nil {
		t.Fatal("expected valid identity")
	}
}

func TestLoadOrGenerateIdentity_WriteErrors(t *testing.T) {
	tmpDir := t.TempDir()
	// Create a file where directory is expected
	blockerFile := filepath.Join(tmpDir, "blocker")
	if err := os.WriteFile(blockerFile, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}

	// Attempting MkdirAll on a path where a parent component is a regular file
	badDirKeyPath := filepath.Join(blockerFile, "subdir", "key.pem")
	_, err := LoadOrGenerateIdentity(badDirKeyPath)
	if err == nil {
		t.Error("expected error when MkdirAll fails, got nil")
	}

	// Read-only directory error when writing file
	if os.Geteuid() == 0 {
		return // root writes into a 0500 directory: the read-only case cannot fail
	}
	roDir := filepath.Join(tmpDir, "ro_dir")
	if err := os.Mkdir(roDir, 0500); err != nil {
		t.Fatal(err)
	}
	roKeyPath := filepath.Join(roDir, "node.key")
	_, err = LoadOrGenerateIdentity(roKeyPath)
	if err == nil {
		t.Error("expected error writing to read-only dir, got nil")
	}
}

func TestVerifySignature_InvalidKeys(t *testing.T) {
	data := []byte("hello")
	sig := []byte("dummy-signature")

	// Invalid hex
	valid, err := VerifySignature("invalid-hex-zz!", data, sig)
	if valid || err != ErrInvalidPublicKey {
		t.Errorf("expected ErrInvalidPublicKey for invalid hex, got valid=%v, err=%v", valid, err)
	}

	// Valid hex but wrong size (e.g. 16 bytes instead of 32)
	valid, err = VerifySignature("0102030405060708090a0b0c0d0e0f10", data, sig)
	if valid || err != ErrInvalidPublicKey {
		t.Errorf("expected ErrInvalidPublicKey for wrong length, got valid=%v, err=%v", valid, err)
	}

	// Hex with 0x prefix and valid size
	id, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	realSig := id.Sign(data)
	valid, err = VerifySignature("0x"+id.HexPublicKey(), data, realSig)
	if err != nil || !valid {
		t.Errorf("expected verification with 0x prefix to succeed, got valid=%v, err=%v", valid, err)
	}
}

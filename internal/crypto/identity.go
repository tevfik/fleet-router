// Package crypto provides Ed25519-based cryptographic identities, signing,
// and verification for zero-trust node settlement and token economy.
package crypto

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var (
	ErrInvalidSignature = errors.New("invalid cryptographic signature")
	ErrInvalidPublicKey = errors.New("invalid public key format")
)

// Identity represents a local cryptographic identity (keypair).
type Identity struct {
	PublicKey  ed25519.PublicKey
	PrivateKey ed25519.PrivateKey
}

// GenerateIdentity creates a new random Ed25519 keypair.
func GenerateIdentity() (*Identity, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("failed to generate ed25519 key: %w", err)
	}
	return &Identity{
		PublicKey:  pub,
		PrivateKey: priv,
	}, nil
}

// LoadOrGenerateIdentity loads an identity from the given path or creates a new one.
func LoadOrGenerateIdentity(keyPath string) (*Identity, error) {
	if keyPath == "" {
		home, _ := os.UserHomeDir()
		keyPath = filepath.Join(home, ".config", "fleet", "node_identity.key")
	}

	data, err := os.ReadFile(keyPath)
	if err == nil && len(data) == ed25519.PrivateKeySize {
		priv := ed25519.PrivateKey(data)
		pub := priv.Public().(ed25519.PublicKey)
		return &Identity{
			PublicKey:  pub,
			PrivateKey: priv,
		}, nil
	}

	// Create new
	id, err := GenerateIdentity()
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(filepath.Dir(keyPath), 0700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath, id.PrivateKey, 0600); err != nil {
		return nil, fmt.Errorf("failed to persist node identity: %w", err)
	}
	return id, nil
}

// Sign signs the provided payload with the private key.
func (id *Identity) Sign(message []byte) []byte {
	return ed25519.Sign(id.PrivateKey, message)
}

// Address returns an Ethereum-style address representation: "0x" + 20-byte hex.
func AddressFromPubKey(pub ed25519.PublicKey) string {
	h := sha256.Sum256(pub)
	return "0x" + hex.EncodeToString(h[:20])
}

// Address returns the identity's public address.
func (id *Identity) Address() string {
	return AddressFromPubKey(id.PublicKey)
}

// HexPublicKey returns the hex-encoded 32-byte public key.
func (id *Identity) HexPublicKey() string {
	return hex.EncodeToString(id.PublicKey)
}

// VerifySignature verifies that signature was produced by the given public key on data.
func VerifySignature(pubKeyHex string, data, signature []byte) (bool, error) {
	pubBytes, err := hex.DecodeString(strings.TrimPrefix(pubKeyHex, "0x"))
	if err != nil || len(pubBytes) != ed25519.PublicKeySize {
		return false, ErrInvalidPublicKey
	}
	return ed25519.Verify(pubBytes, data, signature), nil
}

// Package economy provides a decentralized, signature-verified credit and token
// economy for sharing idle GPUs and settling inference compute across fleet nodes.
package economy

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/tevfik/fleet-router/internal/crypto"
)

var (
	ErrInsufficientBalance     = errors.New("insufficient credit balance for inference")
	ErrInvalidReceiptSignature = errors.New("invalid provider signature on inference receipt")
	ErrReceiptAlreadySettled   = errors.New("inference receipt has already been settled (replay attempt)")
	ErrHoldNotFound            = errors.New("escrow hold not found or expired")
	ErrInvalidAmount           = errors.New("amount must be strictly positive")
	ErrAccountNotFound         = errors.New("account not found")
	ErrInvalidReceipt          = errors.New("inference receipt is malformed (negative amounts, or the provider address is not that of its public key)")
	ErrHoldMismatch            = errors.New("the escrow hold belongs to another consumer")
)

// Account stores credit balances and usage stats for a user/node address.
type Account struct {
	Address     string `json:"address"`           // 0x...
	Balance     int64  `json:"balance"`           // Available credits
	Locked      int64  `json:"locked"`            // Escrowed for in-flight requests
	TotalEarned int64  `json:"total_earned"`      // Total credits earned by hosting GPU
	TotalSpent  int64  `json:"total_spent"`       // Total credits spent on remote GPUs
	NodeID      string `json:"node_id,omitempty"` // Associated fleet node name if local
	UpdatedAt   int64  `json:"updated_at"`
}

// ShareConfig determines how a node exposes idle compute to the network.
type ShareConfig struct {
	Enabled             bool     `json:"enabled"`                 // Whether idle GPU compute is offered
	PricePerMillion     int64    `json:"price_per_million"`       // Credit cost per 1M tokens (prompt + completion)
	MaxIdleVRAMMiB      int64    `json:"max_idle_vram_mib"`       // Cap on VRAM shared
	AllowedModels       []string `json:"allowed_models"`          // Models exposed for public leasing
	AutoStopOnLocalLoad bool     `json:"auto_stop_on_local_load"` // Pause leasing when owner issues requests
}

// DefaultShareConfig returns safe defaults for GPU sharing.
func DefaultShareConfig() ShareConfig {
	return ShareConfig{
		Enabled:             false,
		PricePerMillion:     1000, // 1000 credits per 1M tokens
		MaxIdleVRAMMiB:      0,    // 0 = all idle VRAM
		AllowedModels:       []string{"*"},
		AutoStopOnLocalLoad: true,
	}
}

// InferenceReceipt represents cryptographic proof that a node executed inference for a consumer.
type InferenceReceipt struct {
	ReceiptID         string `json:"receipt_id"`
	RequestID         string `json:"request_id"`
	ProviderAddress   string `json:"provider_address"`
	ProviderPubKey    string `json:"provider_pubkey"`
	ConsumerAddress   string `json:"consumer_address"`
	Model             string `json:"model"`
	PromptTokens      int    `json:"prompt_tokens"`
	CompletionTokens  int    `json:"completion_tokens"`
	CostCredits       int64  `json:"cost_credits"`
	Timestamp         int64  `json:"timestamp"`
	Nonce             int64  `json:"nonce"`
	ProviderSignature string `json:"provider_signature"`
}

// Digest computes the deterministic SHA256 canonical hash of the receipt.
func (r *InferenceReceipt) Digest() []byte {
	canonical := fmt.Sprintf("%s:%s:%s:%s:%s:%d:%d:%d:%d:%d",
		r.ReceiptID,
		r.RequestID,
		r.ProviderAddress,
		r.ConsumerAddress,
		r.Model,
		r.PromptTokens,
		r.CompletionTokens,
		r.CostCredits,
		r.Timestamp,
		r.Nonce,
	)
	h := sha256.Sum256([]byte(canonical))
	return h[:]
}

// Sign signs the receipt using the provider's private identity.
func (r *InferenceReceipt) Sign(id *crypto.Identity) {
	r.ProviderPubKey = id.HexPublicKey()
	r.ProviderAddress = id.Address()
	sigBytes := id.Sign(r.Digest())
	r.ProviderSignature = hex.EncodeToString(sigBytes)
}

// Verify checks the cryptographic validity of the provider signature.
func (r *InferenceReceipt) Verify() (bool, error) {
	sigBytes, err := hex.DecodeString(r.ProviderSignature)
	if err != nil {
		return false, fmt.Errorf("failed to decode signature hex: %w", err)
	}
	// The address in the receipt must be the one of the key that signed: without this anyone could sign with their own
	// key and name another party as provider or consumer-side counterpart.
	pub, err := hex.DecodeString(strings.TrimPrefix(r.ProviderPubKey, "0x"))
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return false, fmt.Errorf("bad provider public key")
	}
	if crypto.AddressFromPubKey(ed25519.PublicKey(pub)) != r.ProviderAddress {
		return false, nil
	}
	return crypto.VerifySignature(r.ProviderPubKey, r.Digest(), sigBytes)
}

// Hold represents temporary escrow on consumer funds while an inference request runs.
type Hold struct {
	HoldID          string
	ConsumerAddress string
	EstimatedCost   int64
	ExpiresAt       time.Time
}

// Ledger is the in-memory / persistent ledger verifying signatures, holding escrows,
// and settling balances between compute providers and consumers.
type Ledger struct {
	mu           sync.RWMutex
	accounts     map[string]*Account
	settledTx    map[string]*InferenceReceipt
	activeHolds  map[string]*Hold
	shareConfigs map[string]*ShareConfig // nodeID -> ShareConfig
	noSave       error                   // set when the ledger on disk could not be loaded: Save refuses to overwrite it
}

// NewLedger initializes a fresh credit ledger.
func NewLedger() *Ledger {
	return &Ledger{
		accounts:     make(map[string]*Account),
		settledTx:    make(map[string]*InferenceReceipt),
		activeHolds:  make(map[string]*Hold),
		shareConfigs: make(map[string]*ShareConfig),
	}
}

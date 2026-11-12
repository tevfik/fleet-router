package economy

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// CalculateCost computes credit cost based on total tokens and price per million.
// Minimum cost is 1 credit if tokens > 0.
func CalculateCost(pricePerMillion int64, promptTokens, completionTokens int) int64 {
	totalTokens := int64(promptTokens + completionTokens)
	if totalTokens <= 0 || pricePerMillion <= 0 {
		return 0
	}
	cost := (totalTokens * pricePerMillion) / 1_000_000
	if cost == 0 && totalTokens > 0 {
		return 1 // round up minimum
	}
	return cost
}

// GetOrCreateAccount retrieves an account by address or initializes it with 0 balance.
func (l *Ledger) GetOrCreateAccount(address string) *Account {
	l.mu.Lock()
	defer l.mu.Unlock()

	acc, exists := l.accounts[address]
	if !exists {
		acc = &Account{
			Address:   address,
			Balance:   0,
			Locked:    0,
			UpdatedAt: time.Now().Unix(),
		}
		l.accounts[address] = acc
	}
	return acc
}

// GetAccount returns a copy of the account if it exists.
func (l *Ledger) GetAccount(address string) (*Account, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	acc, exists := l.accounts[address]
	if !exists {
		return nil, ErrAccountNotFound
	}
	cpy := *acc
	return &cpy, nil
}

// Deposit credits funds to an address.
func (l *Ledger) Deposit(address string, amount int64) (*Account, error) {
	if amount <= 0 {
		return nil, ErrInvalidAmount
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	acc, exists := l.accounts[address]
	if !exists {
		acc = &Account{
			Address: address,
			Balance: 0,
			Locked:  0,
		}
		l.accounts[address] = acc
	}

	acc.Balance += amount
	acc.UpdatedAt = time.Now().Unix()
	cpy := *acc
	return &cpy, nil
}

// Hold escrows estimatedCost from consumer's available balance before dispatching inference.
func (l *Ledger) Hold(consumerAddress string, estimatedCost int64) (string, error) {
	if estimatedCost <= 0 {
		return "", ErrInvalidAmount
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	acc, exists := l.accounts[consumerAddress]
	if !exists || acc.Balance < estimatedCost {
		return "", ErrInsufficientBalance
	}

	acc.Balance -= estimatedCost
	acc.Locked += estimatedCost
	acc.UpdatedAt = time.Now().Unix()

	// Generate unique hold ID
	buf := make([]byte, 16)
	rand.Read(buf)
	holdID := "hold_" + hex.EncodeToString(buf)

	l.activeHolds[holdID] = &Hold{
		HoldID:          holdID,
		ConsumerAddress: consumerAddress,
		EstimatedCost:   estimatedCost,
		ExpiresAt:       time.Now().Add(5 * time.Minute),
	}

	return holdID, nil
}

// ReleaseHold cancels an escrow hold and returns locked credits to the consumer's balance.
func (l *Ledger) ReleaseHold(holdID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	hold, exists := l.activeHolds[holdID]
	if !exists {
		return ErrHoldNotFound
	}

	acc, ok := l.accounts[hold.ConsumerAddress]
	if ok {
		acc.Balance += hold.EstimatedCost
		acc.Locked -= hold.EstimatedCost
		if acc.Locked < 0 {
			acc.Locked = 0
		}
		acc.UpdatedAt = time.Now().Unix()
	}

	delete(l.activeHolds, holdID)
	return nil
}

// SettleReceipt cryptographically verifies proof of work and settles balances.
// If consumerAddress == providerAddress, it's considered self-hosting (free of charge).
func (l *Ledger) SettleReceipt(receipt *InferenceReceipt, holdID string) error {
	if receipt == nil {
		return fmt.Errorf("receipt cannot be nil")
	}

	// 1. Verify cryptographic signature from provider
	valid, err := receipt.Verify()
	if err != nil || !valid {
		return ErrInvalidReceiptSignature
	}
	if receipt.CostCredits < 0 || receipt.PromptTokens < 0 || receipt.CompletionTokens < 0 {
		return ErrInvalidReceipt // a negative cost would mint credits for the consumer
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	// 2. Prevent replay attacks
	if _, already := l.settledTx[receipt.ReceiptID]; already {
		return ErrReceiptAlreadySettled
	}

	// 3. Reconcile with hold if present
	var hold *Hold
	if holdID != "" {
		h, exists := l.activeHolds[holdID]
		if !exists || time.Now().After(h.ExpiresAt) {
			return ErrHoldNotFound
		}
		if h.ConsumerAddress != receipt.ConsumerAddress {
			return ErrHoldMismatch // somebody else's escrow must not pay for this receipt
		}
		hold = h
	}

	consumer, okConsumer := l.accounts[receipt.ConsumerAddress]
	if !okConsumer {
		consumer = &Account{Address: receipt.ConsumerAddress}
		l.accounts[receipt.ConsumerAddress] = consumer
	}

	provider, okProvider := l.accounts[receipt.ProviderAddress]
	if !okProvider {
		provider = &Account{Address: receipt.ProviderAddress}
		l.accounts[receipt.ProviderAddress] = provider
	}

	// 4. Check for self-hosted execution
	if receipt.ConsumerAddress == receipt.ProviderAddress {
		// Refund hold in full
		if hold != nil {
			consumer.Balance += hold.EstimatedCost
			consumer.Locked -= hold.EstimatedCost
			delete(l.activeHolds, holdID)
		}
		l.settledTx[receipt.ReceiptID] = receipt
		return nil
	}

	// 5. Remote compute settlement
	cost := receipt.CostCredits
	if hold != nil {
		diff := hold.EstimatedCost - cost
		if diff < 0 && consumer.Balance+diff < 0 {
			return ErrInsufficientBalance // the cost above the hold is not covered: nothing changes, the hold stays
		}
		consumer.Locked -= hold.EstimatedCost
		consumer.Balance += diff // leftover of the hold back, or the part of the cost above it taken
		delete(l.activeHolds, holdID)
	} else {
		// Direct deduction without pre-hold
		if consumer.Balance < cost {
			return ErrInsufficientBalance
		}
		consumer.Balance -= cost
	}

	consumer.TotalSpent += cost
	consumer.UpdatedAt = time.Now().Unix()

	// Credit provider
	provider.Balance += cost
	provider.TotalEarned += cost
	provider.UpdatedAt = time.Now().Unix()

	// Record settlement to prevent reuse
	l.settledTx[receipt.ReceiptID] = receipt
	return nil
}

// SetShareConfig updates a node's idle GPU sharing rules.
func (l *Ledger) SetShareConfig(nodeID string, cfg ShareConfig) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.shareConfigs[nodeID] = &cfg
}

// GetShareConfig retrieves a node's idle GPU sharing configuration.
func (l *Ledger) GetShareConfig(nodeID string) ShareConfig {
	l.mu.RLock()
	defer l.mu.RUnlock()
	cfg, exists := l.shareConfigs[nodeID]
	if !exists {
		return DefaultShareConfig()
	}
	return *cfg
}

package main

import (
	"fmt"
	"os"

	"github.com/tevfik/fleet-router/internal/crypto"
	"github.com/tevfik/fleet-router/internal/economy"
)

func cmdWallet(args []string) error {
	sub := "status"
	if len(args) > 0 {
		sub = args[0]
		args = args[1:]
	}

	id, err := crypto.LoadOrGenerateIdentity("")
	if err != nil {
		return fmt.Errorf("failed to load wallet identity: %w", err)
	}

	ledger, lerr := economy.LoadLedger("")
	if lerr != nil {
		fmt.Fprintf(os.Stderr, "warning: %v (read-only: nothing will be saved)\n", lerr)
	}

	switch sub {
	case "address", "addr":
		fmt.Printf("Wallet Address: %s\n", id.Address())
		fmt.Printf("Public Key:     0x%s\n", id.HexPublicKey())
		return nil

	case "status", "info", "balance":
		acc := ledger.GetOrCreateAccount(id.Address())
		fmt.Printf("WALLET & TOKEN ECONOMY\n\n")
		fmt.Printf("  Address:      %s\n", id.Address())
		fmt.Printf("  Public Key:   0x%s\n", id.HexPublicKey())
		fmt.Printf("  Balance:      %d credits\n", acc.Balance)
		fmt.Printf("  Locked Hold:  %d credits\n", acc.Locked)
		fmt.Printf("  Total Earned: %d credits (hosting idle GPU)\n", acc.TotalEarned)
		fmt.Printf("  Total Spent:  %d credits (leasing remote compute)\n", acc.TotalSpent)
		fmt.Printf("\nTips:\n")
		fmt.Printf("  - Using your own node's GPU is always 100%% free.\n")
		fmt.Printf("  - Share idle GPU compute with `fleet share on` to earn credits.\n")
		return nil

	default:
		return fmt.Errorf("unknown wallet command %q (use address | status)", sub)
	}
}

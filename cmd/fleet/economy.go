package main

import (
	"fmt"
	"strings"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/crypto"
	"github.com/tevfik/fleet-router/internal/economy"
)

func cmdEconomy(args []string) error {
	sub := "status"
	if len(args) > 0 {
		sub = args[0]
		args = args[1:]
	}

	cfg, err := config.Load(config.Path())
	if err != nil {
		cfg = &config.Config{}
	}
	if cfg.Economy == nil {
		t := true
		f := false
		cfg.Economy = &config.EconomyConfig{
			Enabled:     &t,
			FairQueuing: &t,
			ShareIdle:   &f,
		}
	}

	switch sub {
	case "on", "enable":
		t := true
		cfg.Economy.Enabled = &t
		if err := cfg.Save(config.Path()); err != nil {
			return err
		}
		if cl, err := mustClient(); err == nil {
			_ = cl.Reload()
		}
		fmt.Println("✓ Token economy ENABLED (p2p accounting & fair queuing active)")
		return nil

	case "off", "disable":
		f := false
		cfg.Economy.Enabled = &f
		if err := cfg.Save(config.Path()); err != nil {
			return err
		}
		if cl, err := mustClient(); err == nil {
			_ = cl.Reload()
		}
		fmt.Println("✓ Token economy DISABLED (free/equal queuing across all requests)")
		return nil

	case "queuing", "fair-queuing":
		if len(args) == 0 {
			return fmt.Errorf("usage: fleet economy queuing [on|off]")
		}
		switch strings.ToLower(args[0]) {
		case "on", "enable", "true":
			t := true
			cfg.Economy.FairQueuing = &t
			fmt.Println("✓ Fair Queuing & QoS Priority Boost ENABLED")
		case "off", "disable", "false":
			f := false
			cfg.Economy.FairQueuing = &f
			fmt.Println("✓ Fair Queuing & QoS Priority Boost DISABLED")
		default:
			return fmt.Errorf("usage: fleet economy queuing [on|off]")
		}
		if err := cfg.Save(config.Path()); err != nil {
			return err
		}
		if cl, err := mustClient(); err == nil {
			_ = cl.Reload()
		}
		return nil

	case "status", "info":
		id, err := crypto.LoadOrGenerateIdentity("")
		if err != nil {
			return fmt.Errorf("failed to load identity: %w", err)
		}
		ledger, _ := economy.LoadLedger("")
		acc := ledger.GetOrCreateAccount(id.Address())

		statusStr := "ENABLED"
		if !cfg.Economy.IsEnabled() {
			statusStr = "DISABLED"
		}
		queuingStr := "ENABLED"
		if !cfg.Economy.IsFairQueuing() {
			queuingStr = "DISABLED"
		}
		shareStr := "DISABLED"
		if cfg.Economy.IsShareIdle() {
			shareStr = "ENABLED"
		}

		fmt.Println("TOKEN ECONOMY SETTINGS & STATUS")
		fmt.Printf("  Economy Master Switch:  %s\n", statusStr)
		fmt.Printf("  Fair Queuing QoS Boost: %s\n", queuingStr)
		fmt.Printf("  Idle Compute Sharing:   %s (recorded only, not enforced yet: see fleet share status)\n", shareStr)
		fmt.Printf("  Wallet Address:         %s\n", id.Address())
		fmt.Printf("  Balance:                %d credits\n", acc.Balance)
		fmt.Println("\nUsage:")
		fmt.Println("  fleet economy on / off          - Toggle token economy system")
		fmt.Println("  fleet economy queuing on / off  - Toggle credit-weighted fair queuing")
		fmt.Println("  fleet share on / off            - Toggle idle GPU compute leasing")
		fmt.Println("  fleet wallet status             - View detailed wallet ledger")
		return nil

	default:
		return fmt.Errorf("unknown economy command %q — use status, on, off, queuing", sub)
	}
}

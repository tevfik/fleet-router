package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/tevfik/fleet-router/internal/crypto"
	"github.com/tevfik/fleet-router/internal/economy"
)

// shareNotEnforcedNote: the share settings are recorded and shown, but the router does not consult them yet.
const shareNotEnforcedNote = `
  NOTE: these settings are not enforced yet. Requests from the hub, peers and the LAN are served
  the same whether sharing is on or off; price, VRAM cap, model list and auto-stop are not applied.
  Who used what: fleet usage --by client (hub traffic shows as "hub-tunnel").`

func cmdShare(args []string) error {
	sub := "status"
	if len(args) > 0 {
		sub = args[0]
		args = args[1:]
	}

	id, err := crypto.LoadOrGenerateIdentity("")
	if err != nil {
		return fmt.Errorf("failed to load identity: %w", err)
	}

	selfNode, _ := os.Hostname()
	if selfNode == "" {
		selfNode = "local"
	}

	ledger, _ := economy.LoadLedger("")

	switch sub {
	case "on", "enable", "start":
		fs := flag.NewFlagSet("fleet share on", flag.ExitOnError)
		price := fs.Int64("price", 1000, "Price in credits per 1M tokens (prompt + completion)")
		vram := fs.Int64("vram", 0, "Max idle VRAM in MiB to share (0 = all idle)")
		models := fs.String("models", "*", "Comma-separated list of model names to expose for leasing")
		if err := fs.Parse(args); err != nil {
			return err
		}

		var allowed []string
		for _, m := range strings.Split(*models, ",") {
			m = strings.TrimSpace(m)
			if m != "" {
				allowed = append(allowed, m)
			}
		}

		sc := economy.ShareConfig{
			Enabled:             true,
			PricePerMillion:     *price,
			MaxIdleVRAMMiB:      *vram,
			AllowedModels:       allowed,
			AutoStopOnLocalLoad: true,
		}
		ledger.SetShareConfig(selfNode, sc)
		if err := ledger.Save(""); err != nil {
			return fmt.Errorf("the setting was NOT saved: %w", err)
		}

		fmt.Printf("✓ GPU compute sharing ENABLED on [%s]\n", selfNode)
		fmt.Printf("  Provider Address:  %s\n", id.Address())
		fmt.Printf("  Price:             %d credits / 1M tokens\n", sc.PricePerMillion)
		fmt.Printf("  Allowed Models:    %s\n", strings.Join(sc.AllowedModels, ", "))
		fmt.Printf("  Auto-Stop on Load: YES (pauses leasing when local tasks run)\n")
		fmt.Println(shareNotEnforcedNote)
		return nil

	case "off", "disable", "stop":
		sc := ledger.GetShareConfig(selfNode)
		sc.Enabled = false
		ledger.SetShareConfig(selfNode, sc)
		if err := ledger.Save(""); err != nil {
			return fmt.Errorf("the setting was NOT saved: %w", err)
		}
		fmt.Printf("✓ GPU compute sharing DISABLED on [%s]\n", selfNode)
		return nil

	case "status":
		sc := ledger.GetShareConfig(selfNode)
		statusStr := "DISABLED"
		if sc.Enabled {
			statusStr = "ACTIVE (Leasing idle GPU for token credits)"
		}

		fmt.Printf("IDLE GPU COMPUTE SHARING\n\n")
		fmt.Printf("  Node:              %s\n", selfNode)
		fmt.Printf("  Wallet Address:    %s\n", id.Address())
		fmt.Printf("  Sharing Status:    %s\n", statusStr)
		fmt.Printf("  Rate:              %d credits / 1M tokens\n", sc.PricePerMillion)
		fmt.Printf("  Shared VRAM Cap:   %d MiB (0 = unrestricted idle)\n", sc.MaxIdleVRAMMiB)
		fmt.Printf("  Exposed Models:    %s\n", strings.Join(sc.AllowedModels, ", "))
		fmt.Println(shareNotEnforcedNote)
		return nil

	default:
		return fmt.Errorf("unknown share command %q (use on | off | status)", sub)
	}
}

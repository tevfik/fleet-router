package deploy

import (
	"strings"
)

// Strategy defines the deployment QoS optimization profile.
type Strategy string

const (
	// StrategyLatency maximizes single-user decode speed and minimizes latency.
	// Uses asymmetric layer splitting (TS=16,8), -ub 512, lossless wire (f32; --wire f16|fp8 opts in),
	// direct LAN routing, and FlashAttention.
	StrategyLatency Strategy = "ultra-low-latency"

	// StrategyThroughput optimizes concurrent requests using pipeline concurrency.
	// Uses -np 8, -ub 512, balanced tensor split, and multi-slot scheduling.
	StrategyThroughput Strategy = "max-throughput"

	// StrategyMemory accommodates large models (or MoE experts) by tiering between
	// GPU VRAM and CPU DDR5 RAM with VRAM governor protection.
	StrategyMemory Strategy = "memory-saver"

	// StrategyAuto autonomously selects the best strategy based on measured network RTT
	// and available VRAM across candidate nodes.
	StrategyAuto Strategy = "auto-adaptive"
)

// ParseStrategy normalizes strategy string aliases.
func ParseStrategy(s string) Strategy {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "latency", "low-latency", "ultra-low-latency", "fast":
		return StrategyLatency
	case "throughput", "max-throughput", "batch":
		return StrategyThroughput
	case "memory", "memory-saver", "ram", "cpu-gpu":
		return StrategyMemory
	default:
		return StrategyAuto
	}
}

// StrategyParams contains the tuned runtime flags derived from the chosen strategy.
type StrategyParams struct {
	Strategy     Strategy
	MicroBatch   int
	Np           int
	WireFP8      bool
	FlashAttn    bool
	UseDirectLAN bool
	AsymSplit    bool
	SpecType     string
	DraftN       int
	NGL          int
	Description  string
}

// ParamsForStrategy returns optimal tuning parameters based on benchmark discoveries.
func ParamsForStrategy(strat Strategy, totalLayers int, isLAN bool) StrategyParams {
	switch strat {
	case StrategyLatency:
		return StrategyParams{
			Strategy: StrategyLatency,
			// -ub only sizes prompt batches (decode is one token either way). 16 bought no decode speed and slowed
			// prefill: one machine 2150 vs 9700 tok/s (E2); dino+bravo LAN ring, qwen2.5-0.5b, 2195-token prompt,
			// same split: 370 vs 156 ms, decode ~200 tok/s both (E7, 2026-11-06)
			MicroBatch:   512,
			Np:           1,
			WireFP8:      false, // opt-in only (deploy --wire): F16/FP8 failed the correctness gate (docs/DEVELOPMENT-PLAN.md §3)
			FlashAttn:    true,
			UseDirectLAN: isLAN,
			AsymSplit:    true, // Master gets fewer layers (1/3) to balance embedding/head compute
			SpecType:     "ngram-mod",
			DraftN:       3,
			NGL:          99,
			Description:  "Single-stream ultra-low-latency (TS=16,8, -ub 512, f32 wire, LAN)",
		}
	case StrategyThroughput:
		return StrategyParams{
			Strategy:     StrategyThroughput,
			MicroBatch:   512,
			Np:           8,     // Pipeline concurrency scaling (3.96x aggregate speedup)
			WireFP8:      false, // opt-in only (deploy --wire): F16/FP8 failed the correctness gate (docs/DEVELOPMENT-PLAN.md §3)
			FlashAttn:    true,
			UseDirectLAN: isLAN,
			AsymSplit:    false, // Balanced split across pipeline stages
			SpecType:     "ngram-mod",
			DraftN:       3,
			NGL:          99,
			Description:  "Multi-slot max-throughput (np=8, -ub 512, pipelined concurrency)",
		}
	case StrategyMemory:
		return StrategyParams{
			Strategy:     StrategyMemory,
			MicroBatch:   64,
			Np:           2,
			WireFP8:      false, // opt-in only (deploy --wire): F16/FP8 failed the correctness gate (docs/DEVELOPMENT-PLAN.md §3)
			FlashAttn:    true,
			UseDirectLAN: isLAN,
			AsymSplit:    false,
			SpecType:     "ngram-mod",
			DraftN:       2,
			NGL:          totalLayers / 2, // Offload overflow to host RAM
			Description:  "Hybrid CPU-GPU memory tiering (MoE offload & VRAM governor)",
		}
	default: // Auto
		if isLAN {
			p := ParamsForStrategy(StrategyLatency, totalLayers, isLAN)
			p.Strategy = StrategyAuto
			p.Description = "Auto-adaptive (LAN detected -> ultra-low-latency mode selected)"
			return p
		}
		p := ParamsForStrategy(StrategyThroughput, totalLayers, isLAN)
		p.Strategy = StrategyAuto
		p.Description = "Auto-adaptive (WAN/Relay detected -> max-throughput mode selected)"
		return p
	}
}

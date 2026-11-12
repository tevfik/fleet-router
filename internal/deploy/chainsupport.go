package deploy

import (
	"fmt"
	"strings"
)

// SupportedChainArchs is the set of model architectures supported by the 0003 layer-range patch for chain stage-pipe.
var SupportedChainArchs = map[string]bool{
	"llama":      true,
	"qwen":       true,
	"qwen2":      true,
	"qwen2moe":   true,
	"qwen3":      true,
	"qwen3moe":   true,
	"qwen35":     true,
	"qwen35moe":  true,
	"qwen3next":  true,
	"qwen4exp":   true,
	"deepseek":   true,
	"deepseek2":  true,
	"deepseek32": true,
	"gemma":      true,
	"gemma2":     true,
	"minicpm3":   true,
}

// ChainSupportInput carries the options to check whether chain mode can execute the deployment.
type ChainSupportInput struct {
	Architecture string // model architecture (e.g. "llama", "qwen2", "qwen4exp")
	Stages       int    // number of stages (len(workers)+1 or len(parts))
	Spec         string // spec mode (e.g. "draft-dflash", "draft-mtp", "none")
	DraftModel   string // path to draft model file (when spec is draft-dflash or draft-*)
	MMProj       bool   // whether mmproj is requested
	CPUMoE       bool   // whether CPU MoE is requested (--cpu-moe)
	NCPUMoE      int    // whether N CPU MoE is requested (--n-cpu-moe)
	Proto        string // stage protocol: "csp1", "v1" or a stage module's name (empty string defaults to csp1)
}

// ChainSupportResult reports whether chain mode supports the configuration, and why not if rejected.
type ChainSupportResult struct {
	OK      bool
	Reasons []string
}

// ChainSupport evaluates whether the given model architecture, stage count, and feature options
// are supported by the chain stage-pipe execution engine.
func ChainSupport(in ChainSupportInput) ChainSupportResult {
	var reasons []string

	// Rule 1: draft-dflash requires a draft model
	spec := strings.ToLower(strings.TrimSpace(in.Spec))
	isDFlash := spec == "draft-dflash" || strings.HasPrefix(spec, "draft-dflash")
	if isDFlash {
		if strings.TrimSpace(in.DraftModel) == "" {
			reasons = append(reasons, "draft-dflash requires a draft model")
		}
	}

	// Rule 2: mmproj is not supported in chain mode (requires --embed-in)
	if in.MMProj {
		reasons = append(reasons, "mmproj is not supported in chain mode (requires --embed-in)")
	}

	// (qwen4exp: 2 and 3 stages are bit-identical to one process since fleet-engine 80ad523, F32 hidden state on the
	// wire for hyper-connection models; no rule)
	arch := strings.ToLower(strings.TrimSpace(in.Architecture))

	// Rule 4: Architecture must be supported by 0003 layer-range patch
	if arch == "" || !SupportedChainArchs[arch] {
		reasons = append(reasons, fmt.Sprintf("architecture %q is not supported by layer-range pipeline (requires 0003 patch support)", in.Architecture))
	}

	return ChainSupportResult{
		OK:      len(reasons) == 0,
		Reasons: reasons,
	}
}

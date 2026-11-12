package deploy

import (
	"strings"
	"testing"
)

func TestChainSupportTable(t *testing.T) {
	tests := []struct {
		name       string
		input      ChainSupportInput
		wantOK     bool
		wantReason string
	}{
		{
			name: "valid-llama-2-stages",
			input: ChainSupportInput{
				Architecture: "llama",
				Stages:       2,
				Spec:         "none",
				Proto:        "csp1",
			},
			wantOK: true,
		},
		{
			name: "valid-qwen2-with-moe-csp1",
			input: ChainSupportInput{
				Architecture: "qwen2moe",
				Stages:       3,
				CPUMoE:       true,
				Proto:        "csp1",
			},
			wantOK: true,
		},
		{
			name: "reject-draft-dflash-no-draft",
			input: ChainSupportInput{
				Architecture: "qwen2",
				Stages:       2,
				Spec:         "draft-dflash",
			},
			wantOK:     false,
			wantReason: "draft-dflash requires a draft model",
		},
		{
			name: "allow-draft-dflash-with-draft",
			input: ChainSupportInput{
				Architecture: "qwen2",
				Stages:       2,
				Spec:         "draft-dflash",
				DraftModel:   "/path/to/draft.gguf",
			},
			wantOK: true,
		},
		{
			name: "reject-mmproj",
			input: ChainSupportInput{
				Architecture: "qwen2",
				Stages:       2,
				MMProj:       true,
			},
			wantOK:     false,
			wantReason: "mmproj is not supported in chain mode",
		},
		{
			name: "allow-qwen4exp-three-stages",
			input: ChainSupportInput{
				Architecture: "qwen4exp",
				Stages:       3,
			},
			wantOK: true,
		},
		{
			name: "allow-qwen4exp-two-stages",
			input: ChainSupportInput{
				Architecture: "qwen4exp",
				Stages:       2,
				Spec:         "none",
				Proto:        "csp1",
			},
			wantOK: true,
		},
		{
			name: "allow-qwen4exp-single-stage",
			input: ChainSupportInput{
				Architecture: "qwen4exp",
				Stages:       1,
			},
			wantOK: true,
		},
		{
			name: "reject-unsupported-arch",
			input: ChainSupportInput{
				Architecture: "falcon",
				Stages:       2,
			},
			wantOK:     false,
			wantReason: "architecture \"falcon\" is not supported",
		},
		{
			name: "reject-empty-arch",
			input: ChainSupportInput{
				Architecture: "",
				Stages:       2,
			},
			wantOK:     false,
			wantReason: "architecture \"\" is not supported",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := ChainSupport(tc.input)
			if res.OK != tc.wantOK {
				t.Fatalf("ChainSupport(%+v) OK = %v, want %v (reasons: %v)", tc.input, res.OK, tc.wantOK, res.Reasons)
			}
			if tc.wantReason != "" {
				found := false
				for _, r := range res.Reasons {
					if strings.Contains(r, tc.wantReason) {
						found = true
						break
					}
				}
				if !found {
					t.Fatalf("expected reason containing %q, got: %v", tc.wantReason, res.Reasons)
				}
			}
		})
	}
}

func TestChainSupportMultipleReasons(t *testing.T) {
	// Combines mmproj + draft-dflash + unknown arch
	res := ChainSupport(ChainSupportInput{
		Architecture: "unknown_model",
		Stages:       2,
		Spec:         "draft-dflash",
		MMProj:       true,
	})
	if res.OK {
		t.Fatal("expected failure")
	}
	if len(res.Reasons) != 3 {
		t.Fatalf("expected 3 reasons, got %d: %v", len(res.Reasons), res.Reasons)
	}
}

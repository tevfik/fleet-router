package roles

import (
	"testing"

	"github.com/tevfik/fleet-router/internal/config"
)

func suggestMap(cfg *config.Config) map[string]Suggestion {
	got := map[string]Suggestion{}
	for _, s := range Suggest(cfg) {
		got[s.Role] = s
	}
	return got
}

func TestFeature_Suggest_EmptyConfigProposesNothing(t *testing.T) {
	if got := Suggest(&config.Config{}); len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestFeature_Suggest_NoReasoningNoCodeModels(t *testing.T) {
	chat := []string{"chat"}
	cfg := &config.Config{Endpoints: []config.Endpoint{
		{Name: "small", Model: "llama3:3b", TPS: 90, Caps: chat},
		{Name: "large", Model: "llama3:70b", TPS: 8, Caps: chat},
		{Name: "midctx", Model: "mystery", TPS: 30, Ctx: 128000, Caps: chat},
	}}
	got := suggestMap(cfg)
	if s := got["scout"]; s.Value.Preferred != "small" || len(s.Value.Fallback) != 2 || s.Value.Fallback[0] != "midctx" {
		t.Errorf("scout (by speed) = %+v", s.Value)
	}
	if s := got["architect"]; s.Value.Preferred != "large" || s.Reason != "largest models by parameter count / context (deep reasoning)" {
		t.Errorf("architect without thinkers = %+v", s)
	}
	if s := got["coder"]; s.Value.Preferred != "large" || len(s.Value.Fallback) != 1 || s.Reason[:7] != "no code" {
		t.Errorf("coder without code models = %+v", s)
	}
	if s := got["tester"]; s.Value.Preferred != "large" || s.Reason[:7] != "no code" {
		t.Errorf("tester without code models = %+v", s)
	}
	for _, r := range []string{"hunter", "validator", "vision", "embed"} {
		if _, ok := got[r]; ok {
			t.Errorf("%s suggested without a suitable model", r)
		}
	}
}

func TestFeature_Suggest_SingleThinkerValidatorSameModel(t *testing.T) {
	cfg := &config.Config{Endpoints: []config.Endpoint{
		{Name: "only", Model: "qwen3:32b", Caps: []string{"chat", "thinking"}},
	}}
	got := suggestMap(cfg)
	if got["hunter"].Value.Preferred != "only" || got["validator"].Value.Preferred != "only" {
		t.Fatalf("with one reasoning model both use it: %+v / %+v", got["hunter"], got["validator"])
	}
}

func TestFeature_Suggest_CoderPlusLargestDeduped(t *testing.T) {
	cfg := &config.Config{Endpoints: []config.Endpoint{
		{Name: "bigcode", Model: "qwen3-coder:30b", Caps: []string{"chat"}},
		{Name: "small", Model: "llama3:3b", Caps: []string{"chat"}},
	}}
	got := suggestMap(cfg)
	// the code model is also the largest: it must appear once only
	if c := got["coder"].Value; c.Preferred != "bigcode" || len(c.Fallback) != 0 {
		t.Fatalf("coder = %+v", c)
	}
}

func TestFeature_Suggest_VisionAndEmbedSameDim(t *testing.T) {
	cfg := &config.Config{Endpoints: []config.Endpoint{
		{Name: "vl-slow", Model: "qwen2.5-vl:7b", TPS: 10, Caps: []string{"chat", "vision"}},
		{Name: "vl-fast", Model: "gemma3:4b", TPS: 50, Caps: []string{"chat", "vision"}},
		{Name: "emb-nodim", Model: "nomic-embed-text", Caps: []string{"embed"}},
		{Name: "emb-768", Model: "bge-base", Dim: 768, Caps: []string{"embed"}},
		{Name: "emb-1024", Model: "bge-large", Dim: 1024, Caps: []string{"embed"}},
		{Name: "emb-768b", Model: "e5-base", Dim: 768, Caps: []string{"embed"}},
	}}
	got := suggestMap(cfg)
	if v := got["vision"].Value; v.Preferred != "vl-fast" || len(v.Fallback) != 1 || v.Fallback[0] != "vl-slow" {
		t.Errorf("vision (fastest first) = %+v", v)
	}
	e := got["embed"].Value
	if e.Preferred != "emb-768" || len(e.Fallback) != 1 || e.Fallback[0] != "emb-768b" {
		t.Errorf("embed must prefer a known size and fall back only to the same size: %+v", e)
	}
	// embedding models are never chat candidates
	for _, r := range []string{"scout", "architect", "coder"} {
		for _, n := range append([]string{got[r].Value.Preferred}, got[r].Value.Fallback...) {
			if len(n) > 3 && n[:4] == "emb-" {
				t.Errorf("%s lists embedding model %s", r, n)
			}
		}
	}
	// no size known at all: a single embed model, no fallbacks
	cfg.Endpoints = []config.Endpoint{
		{Name: "a", Model: "nomic-embed-text", Caps: []string{"embed"}},
		{Name: "b", Model: "mxbai-embed-large", Caps: []string{"embed"}},
	}
	if e := suggestMap(cfg)["embed"].Value; e.Preferred != "a" || len(e.Fallback) != 0 {
		t.Errorf("embed with unknown sizes = %+v", e)
	}
}

func TestFeature_Suggest_SizeFromName(t *testing.T) {
	for in, want := range map[string]float64{"qwen3:30b-a3b": 30, "llama3.1:8B": 8, "gemma-2.5b": 2.5, "mystery": 0} {
		if got := size(in); got != want {
			t.Errorf("size(%q) = %v, want %v", in, got, want)
		}
	}
}

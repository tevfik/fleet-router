package roles

import (
	"testing"

	"github.com/tevfik/fleet-router/internal/config"
)

func TestSuggestQARoles(t *testing.T) {
	think := []string{"chat", "thinking"}
	cfg := &config.Config{Roles: map[string]config.Role{}, Endpoints: []config.Endpoint{
		{Name: "big", Node: "dino", Model: "qwen3:80b", URL: "http://x", TPS: 10, Caps: think},
		{Name: "mid", Node: "dino", Model: "gemma4:26b", URL: "http://x", TPS: 40, Caps: think},
		{Name: "fast", Node: "v", Model: "bonsai-9b", URL: "http://x", TPS: 120, Caps: think},
		{Name: "coder", Node: "b", Model: "qwen2.5-coder:7b", URL: "http://x", TPS: 60, Caps: []string{"chat"}},
	}}
	got := map[string]config.Role{}
	for _, s := range Suggest(cfg) {
		got[s.Role] = s.Value
	}
	if got["tester"].Preferred != "coder" {
		t.Errorf("tester should start with the code model, got %+v", got["tester"])
	}
	if got["hunter"].Preferred != "big" {
		t.Errorf("hunter should start with the largest reasoning model, got %+v", got["hunter"])
	}
	if v := got["validator"]; v.Preferred != "mid" {
		t.Errorf("validator should start with the second-largest reasoning model, got %+v", v)
	}
	for _, n := range append([]string{got["validator"].Preferred}, got["validator"].Fallback...) {
		if n == "big" {
			t.Errorf("validator must not include the hunter's first choice: %+v", got["validator"])
		}
	}
	if got["tester"].Preferred != "coder" {
		t.Errorf("tester: %+v", got["tester"])
	}
	for _, s := range Suggest(cfg) { // no endpoint may appear twice in one role
		seen := map[string]bool{}
		for _, n := range append([]string{s.Value.Preferred}, s.Value.Fallback...) {
			if seen[n] {
				t.Errorf("role %s lists %s twice: %+v", s.Role, n, s.Value)
			}
			seen[n] = true
		}
	}
	cfg.Roles["tester"] = config.Role{Preferred: "mid"}
	for _, s := range Suggest(cfg) {
		if s.Role == "tester" {
			t.Error("an existing role must not be suggested again")
		}
	}
}

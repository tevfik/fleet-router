package main

import (
	"strings"
	"testing"

	"github.com/tevfik/fleet-router/internal/feature"
)

func TestLite_HelpHidesRunnerCommands(t *testing.T) {
	restore := feature.SetRunner(false)
	lite := helpText()
	restore()
	full := helpText()
	for _, c := range []string{"fleet deploy ", "fleet plan ", "fleet store ", "fleet run [", "fleet engine ", "fleet chain ", "fleet config node "} {
		if strings.Contains(lite, c) {
			t.Errorf("lite help lists %q", c)
		}
		if !strings.Contains(full, c) {
			t.Errorf("full help lacks %q", c)
		}
	}
	for _, c := range []string{"fleet serve ", "fleet role set", "fleet discover", "fleet join "} {
		if !strings.Contains(lite, c) {
			t.Errorf("lite help lacks %q", c)
		}
	}
	for c := range runnerCmds {
		if c == "runs" || c == "profiles" || c == "stage" {
			continue // aliases
		}
		if !strings.Contains(full, "fleet "+c+" ") && !strings.Contains(full, "fleet "+c+"|") {
			t.Errorf("runner command %q is not in the full help", c)
		}
	}
}

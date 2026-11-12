package tui

import (
	"strings"
	"testing"
)

// The same action has the same key on every screen: x deletes/removes, s stops, b benchmarks, e edits, u undoes,
// n adds, p probes, / searches. A new screen that reuses one of these letters for something else (or names the
// action with another letter) fails here.
func TestKeys_SameActionSameKeyEverywhere(t *testing.T) {
	canon := []struct {
		label string   // the start of the button's label
		keys  []string // the keys it may have
	}{
		{"Delete", []string{"x"}}, {"Remove", []string{"x"}}, {"Forget file", []string{"x"}},
		{"Stop", []string{"s"}}, {"Down (stop)", []string{"s"}},
		{"Bench", []string{"b", "B"}}, {"Benchmark", []string{"b", "B"}},
		{"Edit", []string{"e", "enter"}}, {"Declare spec", []string{"e"}},
		{"Undo", []string{"u"}},
		{"Search", []string{"/"}},
		{"Probe", []string{"p"}},
		{"New role", []string{"n"}}, {"Add file", []string{"n"}}, {"Connect machine", []string{"+"}},
		{"Save copy", []string{"c"}}, {"Choose candidate", []string{"c"}},
	}
	for _, sc := range []screen{scDash, scNodes, scProfiles, scRuns, scStore, scPlan, scUsage, scBoard, scRoles, scRoleEdit, scDiscover} {
		m := model{sc: sc, sel: map[int]bool{}}
		for _, b := range m.buttons() {
			for _, c := range canon {
				if !strings.HasPrefix(b.label, c.label) {
					continue
				}
				ok := false
				for _, k := range c.keys {
					ok = ok || b.key == k
				}
				if !ok {
					t.Errorf("screen %d: %q has key %q, want one of %v", sc, b.label, b.key, c.keys)
				}
			}
		}
	}
}

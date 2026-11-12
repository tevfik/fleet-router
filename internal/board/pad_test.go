package board

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestPadRightCountsCells(t *testing.T) {
	for _, s := range []string{"abc", "日本語", "a🙂b", "\x1b[31mred\x1b[0m"} {
		if got := ansi.StringWidth(PadRight(s, 12)); got != 12 {
			t.Errorf("%q padded to %d cells, want 12", s, got)
		}
	}
	if got := PadRight("longer than the width", 5); got != "longer than the width" {
		t.Fatal("never truncates")
	}
}

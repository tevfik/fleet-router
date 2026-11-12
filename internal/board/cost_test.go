package board_test

import (
	"math"
	"strings"
	"testing"

	"github.com/tevfik/fleet-router/internal/board"
	"github.com/tevfik/fleet-router/internal/server"
)

func TestRouteLines(t *testing.T) {
	r := server.RouteExplain{Model: "r", Role: "r", Strategy: "cost", In: 100, Out: 300, Order: []server.Cost{
		{Endpoint: "a", Node: "n1", Prefill: 0.4, Decode: 3, Total: 3.4, Notes: []string{"strained"}},
		{Endpoint: "b", Node: "n2", Unknown: true, Total: math.Inf(1), Notes: []string{"no speed figure yet"}},
	}}
	out := strings.Join(board.RouteLines(r), "\n")
	for _, want := range []string{"role r", "strategy cost", "100 tokens in / 300 out", "3.4s", "strained", "?", "1.", "2."} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "set strategy=cost") {
		t.Fatalf("no hint when the strategy already is cost:\n%s", out)
	}
	r.Strategy = "failover"
	if out := strings.Join(board.RouteLines(r), "\n"); !strings.Contains(out, "set strategy=cost") {
		t.Fatalf("other strategies get the hint:\n%s", out)
	}
	if out := strings.Join(board.RouteLines(server.RouteExplain{Model: "x"}), "\n"); !strings.Contains(out, "no candidates") {
		t.Fatal(out)
	}
}

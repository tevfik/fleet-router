package server

import (
	"testing"
	"time"
)

func TestRunMeasuredAndLast(t *testing.T) {
	r := Run{ID: "a", Model: "m", At: time.Unix(100, 0), Samples: []RunSample{{TPS: 5}, {TokenP50MS: 60, LocalMS: 20}, {TokenP50MS: 50, LocalMS: 20}, {TokenP50MS: 70, LocalMS: 20}}}
	m, n := r.Measured()
	if n != 3 || m.TokenMS != 60 || m.LocalMS != 20 || m.RingMS != 40 {
		t.Fatalf("%+v %d", m, n)
	}
	if _, n := (Run{Samples: []RunSample{{TPS: 3}}}).Measured(); n != 0 {
		t.Fatal("no trace, no measurement")
	}
	other := Run{ID: "b", Model: "x", At: time.Unix(200, 0), Samples: r.Samples}
	if got, ok := LastMeasuredRun([]Run{r, other}, "m"); !ok || got.ID != "a" {
		t.Fatalf("model preferred: %v", got.ID)
	}
	if got, ok := LastMeasuredRun([]Run{r, other}, ""); !ok || got.ID != "b" {
		t.Fatalf("newest: %v", got.ID)
	}
	if _, ok := LastMeasuredRun([]Run{{ID: "z"}}, ""); ok {
		t.Fatal("none")
	}
}

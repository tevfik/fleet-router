package plan

import (
	"math"
	"strings"
	"testing"
)

func TestCalibrate_OrnithNumbers(t *testing.T) {
	// the plan said 40 ms local + 25 ms ring; the trace (Ornith over a WAN) said 114 = 61 + 55 ms
	c := Candidate{Kind: "split", Master: "dino", MasterLocalMS: 40, TokenMS: 65}
	k, err := Calibrate(c, Measured{TokenMS: 114, LocalMS: 61, RingMS: 55}, 1.5)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(k.ErrorPct-(-43.0)) > 0.1 {
		t.Errorf("error %.1f%%", k.ErrorPct)
	}
	if math.Abs(k.FixedMS-22.5) > 1e-9 || math.Abs(k.RingMS-30) > 1e-9 || math.Abs(k.LocalRatio-1.525) > 1e-9 {
		t.Errorf("%+v", k)
	}
}

func TestCalibrate_NeverNegative(t *testing.T) {
	c := Candidate{MasterLocalMS: 30, TokenMS: 60, RingMS: 5}
	k, _ := Calibrate(c, Measured{TokenMS: 40, LocalMS: 10, RingMS: 5}, 0)
	if k.FixedMS != 0 || k.RingMS != 0 {
		t.Errorf("%+v", k)
	}
	if _, err := Calibrate(c, Measured{}, 0); err == nil {
		t.Error("no token time must be an error")
	}
}

func TestApplyTarget(t *testing.T) {
	r := Result{Candidates: []Candidate{{Master: "a", TokenMS: 20}, {Master: "b", TokenMS: 50}}}
	ApplyTarget(&r, 33)
	if len(r.Candidates[0].Notes) != 0 || len(r.Candidates[1].Notes) != 1 || len(r.Warnings) != 0 {
		t.Errorf("%+v", r)
	}
	r = Result{Candidates: []Candidate{{Master: "b", TokenMS: 50}, {Master: "a", TokenMS: 45}}}
	ApplyTarget(&r, 33)
	if len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], "45.0 ms") {
		t.Errorf("%v", r.Warnings)
	}
}

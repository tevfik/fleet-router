package hostinfo

import (
	"math"
	"strings"
	"testing"
)

func TestScoreNPU(t *testing.T) {
	npu := Accelerator{Name: "Test NPU", Kind: "npu", PoolBytes: 8 << 30, BW: 34}
	b := Bench{RAMTotal: 16 << 30, RAMBW: 12, GPUs: []BenchGPU{npu.BenchGPU()}}
	b.score()
	if b.FastWhere != "NPU" || b.FastMem != 8<<30 || b.FastBW != 34 {
		t.Fatalf("%+v", b)
	}
	// a 7 GB model fits the pool (no overhead held back there) and is read at the full 34 GB/s
	where, tps := b.Estimate(7, 0)
	if where != "NPU" || math.Abs(tps-34.0/7) > 1e-9 {
		t.Fatalf("%s %v", where, tps)
	}
	// 9 GB does not fit: the NPU pool cannot spill into system RAM
	if where, _ := b.Estimate(9, 0); where != "" {
		t.Fatalf("spilled: %s", where)
	}
	// a card of another kind keeps priority and its own behaviour
	g := Bench{RAMTotal: 64 << 30, RAMBW: 60, GPUs: []BenchGPU{{Name: "RTX 4090", VRAM: 24 << 30, Bandwidth: 1008}, npu.BenchGPU()}}
	g.score()
	if g.FastWhere != "GPU" || g.FastBW != 1008 {
		t.Fatalf("%+v", g)
	}
}

// A registered probe that finds its device changes the hardware fingerprint (a new board means a new benchmark);
// one that finds nothing does not.
func TestRegisteredAcceleratorInFingerprint(t *testing.T) {
	defer func(p []func() (Accelerator, bool)) { accelProbes = p }(accelProbes)
	accelProbes = nil
	fp0, r0 := Fingerprint(nil)
	RegisterAccelerator(func() (Accelerator, bool) { return Accelerator{}, false })
	if fp, _ := Fingerprint(nil); fp != fp0 {
		t.Fatal("a probe that finds nothing changed the fingerprint")
	}
	RegisterAccelerator(func() (Accelerator, bool) {
		return Accelerator{Name: "Test NPU", Kind: "npu", PoolBytes: 8 << 30, ClockMHz: 1000}, true
	})
	fp, r := Fingerprint(nil)
	if fp == fp0 || !strings.Contains(r, "Test NPU 8 GiB 1000 MHz") || !strings.HasPrefix(r, r0) {
		t.Fatalf("fingerprint %s %q (before %q)", fp, r, r0)
	}
}

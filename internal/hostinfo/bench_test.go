package hostinfo

import (
	"context"
	"math"
	"runtime"
	"testing"
	"time"
)

func TestGPUSpec(t *testing.T) {
	for name, want := range map[string]float64{
		"NVIDIA GeForce RTX 4090 Laptop GPU": 576, "NVIDIA GeForce RTX 4090": 1008, "NVIDIA GB10": 273,
		"NVIDIA L40S": 864, "NVIDIA L4": 300, "NVIDIA A100-SXM4-80GB": 2039, "Some Unknown Card": 0,
	} {
		if got, _ := gpuSpec(name); got != want {
			t.Errorf("%s: %v, want %v", name, got, want)
		}
	}
	if _, uni := gpuSpec("NVIDIA GB10"); !uni {
		t.Error("GB10 uses unified memory")
	}
}

func TestScore(t *testing.T) {
	laptop := Bench{RAMTotal: 64 << 30, RAMBW: 60, GPUs: []BenchGPU{{Name: "RTX 4090 Laptop", VRAM: 16 << 30, Bandwidth: 576}}}
	laptop.score()
	spark := Bench{RAMTotal: 128 << 30, RAMBW: 100, GPUs: []BenchGPU{{Name: "GB10", Bandwidth: 273, Unified: true}}}
	spark.score()
	cpu := Bench{RAMTotal: 32 << 30, RAMBW: 50}
	cpu.score()
	if laptop.MaxFastB != 24 || laptop.Speed8B < 70 || laptop.Speed8B > 85 {
		t.Fatalf("laptop: %+v", laptop)
	}
	if spark.MaxFastB < 150 || spark.Score <= laptop.Score {
		t.Fatalf("a DGX Spark holds far bigger models: spark %d laptop %d", spark.Score, laptop.Score)
	}
	if cpu.Score >= laptop.Score || cpu.FastWhere != "RAM" {
		t.Fatalf("cpu only: %+v", cpu)
	}
	where := map[string]string{}
	for _, f := range laptop.Fits {
		where[f.Size] = f.Where
	}
	if where["8B"] != "GPU" || where["32B"] != "GPU+RAM" || where["70B"] != "GPU+RAM" || where["235B"] != "" {
		t.Fatalf("laptop fits: %+v", laptop.Fits)
	}
}

func TestRunBench(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping real hardware benchmark in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	b := RunBench(ctx, nil)
	if b.RAMBW <= 0 || b.Fingerprint == "" {
		t.Fatalf("expected positive RAMBW and non-empty Fingerprint: %+v", b)
	}
	// CPUGFLOPS: verify measured (> 0) or explicitly unmeasurable (== 0), no invalid/negative/NaN values.
	if b.CPUGFLOPS < 0 || math.IsNaN(b.CPUGFLOPS) || math.IsInf(b.CPUGFLOPS, 0) {
		t.Fatalf("invalid CPUGFLOPS: %+v", b)
	}
	t.Logf("took %.1fs · %s · RAM %.1f GB/s · CPU-MoE %.1f GB/s · PCIe %.1f GB/s · CPU %.1f GFLOPS · fast %s %.0f GB/s · 8B %.1f tok/s · max %gB · score %d · fits %+v · notes %v",
		b.Took, b.Hardware, b.RAMBW, b.CPUMoEBW, b.PCIeBW, b.CPUGFLOPS, b.FastWhere, b.FastBW, b.Speed8B, b.MaxFastB, b.Score, b.Fits, b.Notes)
}

func TestCPUGFLOPS_CancelledCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := cpuGFLOPS(ctx)
	if got != 0 {
		t.Fatalf("cancelled ctx should return 0 (unmeasurable), got %v", got)
	}
}

func TestCPUGFLOPS_FakeClock(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the real multiply-add loop three times")
	}
	orig := benchTimeSince
	defer func() { benchTimeSince = orig }()

	// Fixed fake elapsed time: 2.0s
	benchTimeSince = func(time.Time) time.Duration {
		return 2 * time.Second
	}
	n := runtime.NumCPU()
	if n < 1 {
		n = 1
	}
	want := math.Round(float64(n)*20_000_000*8/2.0/1e9*10) / 10
	got := cpuGFLOPS(context.Background())
	if got != want {
		t.Fatalf("fake 2s clock: got %v, want %v", got, want)
	}

	// Zero elapsed time must return 0 (unmeasurable, avoid division by zero)
	benchTimeSince = func(time.Time) time.Duration {
		return 0
	}
	if got := cpuGFLOPS(context.Background()); got != 0 {
		t.Fatalf("fake 0s clock: got %v, want 0", got)
	}

	// Negative elapsed time must return 0
	benchTimeSince = func(time.Time) time.Duration {
		return -1 * time.Second
	}
	if got := cpuGFLOPS(context.Background()); got != 0 {
		t.Fatalf("fake negative clock: got %v, want 0", got)
	}
}

func TestCPUGFLOPS_TimeoutPartialResult(t *testing.T) {
	// A short timeout context should yield a valid partial result or unmeasurable (0), never negative/NaN.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	got := cpuGFLOPS(ctx)
	if got < 0 || math.IsNaN(got) || math.IsInf(got, 0) {
		t.Fatalf("invalid result on timeout: %v", got)
	}
}

func TestRunBench_CancelledCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b := RunBench(ctx, nil)
	if b.CPUGFLOPS != 0 {
		t.Fatalf("expected CPUGFLOPS 0 on cancelled ctx, got %v", b.CPUGFLOPS)
	}
}

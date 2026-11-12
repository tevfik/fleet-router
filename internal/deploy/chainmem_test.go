package deploy

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/plan"
)

// ornithLike is a 48-block MoE model of about 75 GiB, nearly all of it experts (as Ornith-1.5-122B Q4_K_M).
func ornithLike() *plan.Model {
	m := &plan.Model{Name: "ornith", Layers: 48, KVPerTokenLayer: 8192, KVBlocks: 12, OtherBytes: int64(1.5 * gib), EmbdBytes: int64(0.75 * gib)}
	for i := 0; i < 48; i++ {
		m.LayerBytes = append(m.LayerBytes, int64(1664299827))
		m.ExpertBytes = append(m.ExpertBytes, int64(1556925645))
	}
	return m
}

func twoStages() plan.Candidate {
	return plan.Candidate{Parts: []plan.Part{{Node: "p1", First: 0, Layers: 25}, {Node: "vec", First: 25, Layers: 23, Output: true}}}
}

func fakeMem(t *testing.T, free map[string]MemFree) {
	t.Helper()
	old := probeMemory
	probeMemory = func(_ context.Context, node string, _ bool) (MemFree, error) {
		if f, ok := free[node]; ok {
			return f, nil
		}
		return MemFree{}, fmt.Errorf("ssh: no route")
	}
	t.Cleanup(func() { probeMemory = old })
}

// The run that failed: --cpu-moe --moe-cache-mib 6000 on a 16 GiB GPU that also holds the embeddings, while
// something else held part of it. Each stage now gets the cache its own GPU leaves, not the one figure for all.
func TestChainMemSizesTheCachePerStage(t *testing.T) {
	fakeMem(t, map[string]MemFree{"p1": {RAMGiB: 50, VRAMGiB: 8}, "vec": {RAMGiB: 55, VRAMGiB: 11.5}})
	o := StagePipeOptions{CPUMoE: true, MoECacheMiB: 6000, Ctx: 2048}
	notes, st, err := CheckChainMemory(context.Background(), twoStages(), ornithLike(), &config.Config{}, &o)
	if err != nil {
		t.Fatal(err)
	}
	c0, c1 := o.StageMoECacheMiB[0], o.StageMoECacheMiB[25]
	if c0 <= 0 || c0 >= 6000 || c1 != 6000 {
		t.Fatalf("caches %d / %d (%+v)\n%s", c0, c1, st, strings.Join(notes, "\n"))
	}
	// what the stage needs without the cache, plus the cache, stays inside the free VRAM less the headroom
	if st[0].VRAMGiB+float64(c0)/1024 > 8-chainVRAMHeadroomGiB+1e-9 {
		t.Fatalf("stage 0 overflows: %.2f + %d MiB", st[0].VRAMGiB, c0)
	}
	if !strings.Contains(strings.Join(notes, "\n"), "not 6000") {
		t.Fatalf("the smaller cache is not said:\n%s", strings.Join(notes, "\n"))
	}
	// and the stage command lines carry them
	p, err := PlanStagePipe(twoStages(), ornithLike(), ChainStageV1, o)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Stages[0].Command, fmt.Sprintf("--moe-cache-mib %d", c0)) || !strings.Contains(p.Stages[1].Command, "--moe-cache-mib 6000") {
		t.Fatalf("commands:\n%s\n%s", p.Stages[0].Command, p.Stages[1].Command)
	}
}

// Without --moe-cache-mib a stage gets the cache its VRAM leaves (the engine has none otherwise).
func TestChainMemGivesACacheWhenNoneIsAsked(t *testing.T) {
	fakeMem(t, map[string]MemFree{"p1": {RAMGiB: 50, VRAMGiB: 15}, "vec": {RAMGiB: 55, VRAMGiB: 11.5}})
	o := StagePipeOptions{CPUMoE: true}
	if _, _, err := CheckChainMemory(context.Background(), twoStages(), ornithLike(), &config.Config{}, &o); err != nil {
		t.Fatal(err)
	}
	if o.StageMoECacheMiB[0] < minAutoMoECacheMiB || o.StageMoECacheMiB[0] > maxAutoMoECacheMiB {
		t.Fatalf("%v", o.StageMoECacheMiB)
	}
}

// Without --cpu-moe the experts would go to the GPU: a 16 GiB card cannot hold 39 GiB of them. Refused before
// anything starts, with the way out named; --force deploys anyway and says so.
func TestChainMemRefusesWhatCannotFit(t *testing.T) {
	fakeMem(t, map[string]MemFree{"p1": {RAMGiB: 50, VRAMGiB: 15}, "vec": {RAMGiB: 55, VRAMGiB: 11.5}})
	o := StagePipeOptions{}
	_, _, err := CheckChainMemory(context.Background(), twoStages(), ornithLike(), &config.Config{}, &o)
	if err == nil || !strings.Contains(err.Error(), "--cpu-moe") || !strings.Contains(err.Error(), "p1 (stage 0)") {
		t.Fatalf("%v", err)
	}
	o = StagePipeOptions{Force: true}
	notes, _, err := CheckChainMemory(context.Background(), twoStages(), ornithLike(), &config.Config{}, &o)
	if err != nil || !strings.Contains(strings.Join(notes, "\n"), "(--force)") {
		t.Fatalf("%v\n%s", err, strings.Join(notes, "\n"))
	}
}

// The experts of --cpu-moe live in RAM: a machine without that much available is refused (paging them from disk on
// every token, or worse); reserve_gib counts.
func TestChainMemChecksRAMAndReserve(t *testing.T) {
	fakeMem(t, map[string]MemFree{"p1": {RAMGiB: 40, VRAMGiB: 15}, "vec": {RAMGiB: 55, VRAMGiB: 11.5}})
	o := StagePipeOptions{CPUMoE: true}
	if _, _, err := CheckChainMemory(context.Background(), twoStages(), ornithLike(), &config.Config{}, &o); err != nil {
		t.Fatalf("36 GiB of experts in 40 GiB: %v", err)
	}
	cfg := &config.Config{Nodes: map[string]config.NodeSpec{"p1": {ReserveGiB: 8}}}
	o = StagePipeOptions{CPUMoE: true}
	_, _, err := CheckChainMemory(context.Background(), twoStages(), ornithLike(), cfg, &o)
	if err == nil || !strings.Contains(err.Error(), "experts in RAM") {
		t.Fatalf("reserve_gib 8 leaves 30.5 GiB for 36 GiB of experts: %v", err)
	}
}

// Unified memory (GB10: nvidia-smi reports no free VRAM): everything is one pool, the DFlash drafts included, one per
// sequence.
func TestChainMemUnifiedAndDrafts(t *testing.T) {
	fakeMem(t, map[string]MemFree{"dino": {RAMGiB: 90}})
	old := draftFileGiB
	draftFileGiB = func(context.Context, string, string) float64 { return 2 }
	t.Cleanup(func() { draftFileGiB = old })
	one := plan.Candidate{Parts: []plan.Part{{Node: "dino", First: 0, Layers: 48, Output: true}}}
	o := StagePipeOptions{DraftModel: "/m/draft.gguf", Np: 4}
	_, st, err := CheckChainMemory(context.Background(), one, ornithLike(), &config.Config{}, &o)
	if err != nil {
		t.Fatal(err)
	}
	if st[0].DraftGiB < 9 || st[0].DraftGiB > 9.5 { // 2 GiB x 1.15 x 4 sequences
		t.Fatalf("draft %.2f", st[0].DraftGiB)
	}
	fakeMem(t, map[string]MemFree{"dino": {RAMGiB: 80}})
	o = StagePipeOptions{DraftModel: "/m/draft.gguf", Np: 4}
	if _, _, err := CheckChainMemory(context.Background(), one, ornithLike(), &config.Config{}, &o); err == nil || !strings.Contains(err.Error(), "draft") {
		t.Fatalf("76 GiB of weights + 9 GiB of drafts in 80 GiB: %v", err)
	}
}

// A machine that cannot be read is not checked (said in the notes), its cache stays as given.
func TestChainMemUnreadMachine(t *testing.T) {
	fakeMem(t, map[string]MemFree{"p1": {RAMGiB: 50, VRAMGiB: 15}})
	o := StagePipeOptions{CPUMoE: true, MoECacheMiB: 3000}
	notes, _, err := CheckChainMemory(context.Background(), twoStages(), ornithLike(), &config.Config{}, &o)
	if err != nil || o.StageMoECacheMiB[25] != 3000 || !strings.Contains(strings.Join(notes, "\n"), "could not be read") {
		t.Fatalf("%v %v\n%s", err, o.StageMoECacheMiB, strings.Join(notes, "\n"))
	}
}

func TestMemGuard(t *testing.T) {
	g := newMemGuard()
	tot := int64(64 << 20) // 64 GiB in KiB: floor 1.28 GiB
	ok := memSample{availKiB: 20 << 20, totalKiB: tot, stallPct: 0, ok: true}
	low := memSample{availKiB: 1 << 20, totalKiB: tot, stallPct: 5, ok: true}
	if g.check(low) != "" || g.check(low) != "" || g.check(ok) != "" || g.check(low) != "" || g.check(low) != "" {
		t.Fatal("a dip that recovers must not stop the load")
	}
	if why := g.check(low); !strings.Contains(why, "MemAvailable 1024 MiB") {
		t.Fatalf("three low seconds in a row: %q", why)
	}
	g = newMemGuard()
	stall := memSample{availKiB: 20 << 20, totalKiB: tot, stallPct: 55, ok: true}
	g.check(stall)
	g.check(stall)
	if why := g.check(stall); !strings.Contains(why, "stall 55%") {
		t.Fatalf("thrashing: %q", why)
	}
	if newMemGuard().check(memSample{}) != "" {
		t.Fatal("no reading, no verdict")
	}
}

// The guard as it runs on the stage's machine: valid sh; a process that is gone is reported; a live one on a machine
// with memory to spare goes on.
func TestMemGuardScript(t *testing.T) {
	sleeper := exec.Command("sleep", "30")
	if err := sleeper.Start(); err != nil {
		t.Skip(err)
	}
	defer sleeper.Process.Kill()
	run := func(pid int) string {
		out, err := exec.Command("sh", "-c", "for x in 1 2 3; do\n  "+memGuardScript(pid)+"\ndone; echo GOES_ON").CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		return string(out)
	}
	if out := run(sleeper.Process.Pid); !strings.Contains(out, "GOES_ON") || strings.Contains(out, "FLEET_") {
		t.Fatalf("live stage, memory to spare: %q", out)
	}
	if out := run(999999); !strings.Contains(out, "FLEET_EXITED") {
		t.Fatalf("gone: %q", out)
	}
	if out, err := exec.Command("sh", "-n", "-c", stageReadyScriptGuarded(StageLaunch{Port: 1, Ready: Readiness{Kind: "http", Addr: "h:1", Path: "/health"}}, 2, 42)).CombinedOutput(); err != nil {
		t.Fatalf("ready script: %v %s", err, out)
	}
}

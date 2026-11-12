package deploy

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/plan"
)

// bigModel is 40 blocks of 3 GiB with a 2 GiB embedding/output: 122 GiB of weights.
func bigModel() *plan.Model {
	m := &plan.Model{Name: "big", Arch: "llama", Layers: 40, Embd: 4096, KVPerTokenLayer: 2 * 8 * 128 * 2, OtherBytes: 2 << 30}
	m.LayerBytes = make([]int64, 40)
	m.LayerRead = make([]int64, 40)
	for i := range m.LayerBytes {
		m.LayerBytes[i] = 3 << 30
	}
	return m
}

func TestMemNeedsSplitsBlocksAndKV(t *testing.T) {
	m := bigModel()
	needs := memNeeds(m, []string{"CUDA0", "RING0"}, "10,30", "dino", []string{"victor"}, 4096, 1, "", "")
	if len(needs) != 2 || needs[0].Node != "dino" || needs[1].Node != "victor" {
		t.Fatalf("%+v", needs)
	}
	if needs[0].Blocks != 10 || needs[1].Blocks != 30 {
		t.Fatalf("blocks %d %d", needs[0].Blocks, needs[1].Blocks)
	}
	// master: 10 blocks of 3 GiB + 2 GiB of embedding/output; worker: 30 blocks
	if w := needs[0].WeightsGiB; w < 31.99 || w > 32.01 {
		t.Fatalf("master weights %.2f", w)
	}
	if w := needs[1].WeightsGiB; w < 89.99 || w > 90.01 {
		t.Fatalf("worker weights %.2f", w)
	}
	// 2*8*128*2 = 4096 B per token per block; 4096 tokens; 30 blocks
	wantKV := 4096.0 * 4096 * 30 / (1 << 30)
	if d := needs[1].KVGiB - wantKV; d > 1e-9 || d < -1e-9 {
		t.Fatalf("kv %.4f want %.4f", needs[1].KVGiB, wantKV)
	}
	// -c is the total context shared by the slots: more slots do not mean more cache
	if n8 := memNeeds(m, []string{"CUDA0", "RING0"}, "10,30", "dino", []string{"victor"}, 4096, 8, "", ""); n8[1].KVGiB != needs[1].KVGiB {
		t.Fatalf("np must not multiply the cache: %.3f vs %.3f", n8[1].KVGiB, needs[1].KVGiB)
	}
	// a quantized cache is smaller, q8_0 about half
	q := memNeeds(m, []string{"CUDA0", "RING0"}, "10,30", "dino", []string{"victor"}, 4096, 1, "q8_0", "q8_0")
	if r := q[1].KVGiB / needs[1].KVGiB; r < 0.5 || r > 0.56 {
		t.Fatalf("q8_0 ratio %.3f", r)
	}
	// a CPU device with ts 0 (--worker-cpu-experts) holds nothing of its own; the NextN order has the worker first
	n3 := memNeeds(m, []string{"RING0", "CUDA0"}, "20,20", "dino", []string{"victor"}, 4096, 1, "", "")
	if n3[0].Node != "victor" || n3[0].Blocks != 20 {
		t.Fatalf("%+v", n3)
	}
	if memNeeds(&plan.Model{Layers: 4}, []string{"CUDA0"}, "4", "dino", nil, 1, 1, "", "") != nil {
		t.Fatal("unknown weights: no verdict")
	}
}

func TestEvaluateMemory(t *testing.T) {
	needs := memNeeds(bigModel(), []string{"CUDA0"}, "40", "dino", nil, 4096, 1, "", "")
	cfg := config.Default()
	// the incident: 122+ GiB of model on a machine with 121 GiB of RAM
	rep := evaluateMemory(needs, map[string]MemFree{"dino": {RAMGiB: 110}}, nil, cfg, false)
	if len(rep.Blockers) != 1 || !strings.Contains(rep.Blockers[0], "does not fit") || !strings.Contains(rep.Blockers[0], "very slow") {
		t.Fatalf("%v", rep)
	}
	// enough room: no verdict at all
	rep = evaluateMemory(needs, map[string]MemFree{"dino": {RAMGiB: 200}}, nil, cfg, false)
	if len(rep.Blockers) != 0 || len(rep.Warnings) != 0 {
		t.Fatalf("%v", rep)
	}
	// RAM and VRAM add up (offload), but not with --cpu
	live := map[string]MemFree{"dino": {RAMGiB: 100, VRAMGiB: 30}}
	if rep = evaluateMemory(needs, live, nil, cfg, false); len(rep.Blockers) != 0 {
		t.Fatalf("100+30 holds 122: %v", rep)
	}
	if rep = evaluateMemory(needs, live, nil, cfg, true); len(rep.Blockers) != 1 {
		t.Fatalf("cpu only ignores the VRAM: %v", rep)
	}
	// tight: a warning, not a refusal
	rep = evaluateMemory(needs, map[string]MemFree{"dino": {RAMGiB: 127}}, nil, cfg, false)
	if len(rep.Blockers) != 0 || len(rep.Warnings) != 1 || !strings.Contains(rep.Warnings[0], "tight") {
		t.Fatalf("%v", rep)
	}
	// a declared mem_gib alone only warns (a model may spill into RAM on purpose); nothing known says so
	cfg.Nodes = map[string]config.NodeSpec{"dino": {MemGiB: 121}}
	rep = evaluateMemory(needs, nil, map[string]error{"dino": errors.New("ssh: refused")}, cfg, false)
	if len(rep.Blockers) != 0 || len(rep.Warnings) != 1 || !strings.Contains(rep.Warnings[0], "mem_gib of dino is 121.0") || !strings.Contains(rep.Warnings[0], "refused") {
		t.Fatalf("%v", rep)
	}
	rep = evaluateMemory(needs, nil, nil, config.Default(), false)
	if len(rep.Warnings) != 1 || !strings.Contains(rep.Warnings[0], "not checked") {
		t.Fatalf("%v", rep)
	}
	// a model with recurrent blocks is refused on its weights only (the KV figure is an upper bound)
	rm := bigModel()
	rm.Recurrent = true
	rm.KVPerTokenLayer = 1 << 20
	rn := memNeeds(rm, []string{"CUDA0"}, "40", "dino", nil, 8192, 1, "", "")
	rep = evaluateMemory(rn, map[string]MemFree{"dino": {RAMGiB: 200}}, nil, config.Default(), false)
	if len(rep.Blockers) != 0 || len(rep.Warnings) == 0 {
		t.Fatalf("%v", rep)
	}
}

func TestParseMemProbe(t *testing.T) {
	f, ok := parseMemProbe("FLEET_MEM_RAM_KB 125829120\nFLEET_MEM_VRAM_MIB 2048\n")
	if !ok || f.RAMGiB != 120 || f.VRAMGiB != 2 {
		t.Fatalf("%+v %v", f, ok)
	}
	if _, ok := parseMemProbe("garbage"); ok {
		t.Fatal("no RAM line")
	}
	// the probe script reads real files: run it here
	out, err := localScript(context.Background(), memProbeScript)
	if err != nil {
		t.Skip("sh not available")
	}
	if _, ok := parseMemProbe(string(out)); !ok {
		t.Logf("no /proc/meminfo here: %q", out)
	}
}

// ExecuteDeployment refuses a dry run's real twin before anything starts, and --force lets it through.
func TestExecuteDeploymentMemoryPreCheck(t *testing.T) {
	stubMeta(t, bigModel())
	probeMemory = func(_ context.Context, node string, _ bool) (MemFree, error) {
		return MemFree{RAMGiB: 110}, nil
	}
	cfg := config.Default()
	opts := DeployOptions{ModelPath: "/m/big.gguf", MasterNode: "dino", Port: 18090, Ctx: 4096}
	ctxb := context.Background()

	// not a dry run: the refusal comes before any process is started (no ssh/exec seam is stubbed: reaching one would fail differently)
	_, err := ExecuteDeployment(ctxb, cfg, opts)
	if err == nil || !strings.Contains(err.Error(), "not enough memory") || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("want a memory refusal, got %v", err)
	}
	// a dry run shows it as a warning
	opts.DryRun = true
	res, err := ExecuteDeployment(ctxb, cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range res.Warnings {
		found = found || strings.Contains(w, "WOULD BE REFUSED")
	}
	if !found || len(res.Memory) != 1 || !res.Memory[0].Probed {
		t.Fatalf("%v %+v", res.Warnings, res.Memory)
	}
}

// ggufFile is a minimal GGUF: string/u32 metadata and one 1-D tensor per name (each size bytes of data).
func ggufFile(kvs map[string]any, tensors [][2]any) []byte {
	var b bytes.Buffer
	u32 := func(v uint32) { _ = binary.Write(&b, binary.LittleEndian, v) }
	u64 := func(v uint64) { _ = binary.Write(&b, binary.LittleEndian, v) }
	str := func(s string) { u64(uint64(len(s))); b.WriteString(s) }
	b.WriteString("GGUF")
	u32(3)
	u64(uint64(len(tensors)))
	u64(uint64(len(kvs)))
	for k, v := range kvs {
		str(k)
		switch x := v.(type) {
		case string:
			u32(8)
			str(x)
		case uint32:
			u32(4)
			u32(x)
		}
	}
	var off uint64
	for _, t := range tensors {
		str(t[0].(string))
		u32(1)
		u64(8)
		u32(0)
		u64(off)
		off += uint64(t[1].(int))
	}
	for b.Len()%32 != 0 {
		b.WriteByte(0)
	}
	b.Write(make([]byte, off))
	return b.Bytes()
}

func TestAddRemoteShardsSumsEveryFile(t *testing.T) {
	first := ggufFile(map[string]any{"general.architecture": "llama", "llama.block_count": uint32(2), "llama.embedding_length": uint32(64)},
		[][2]any{{"token_embd.weight", 4096}, {"blk.0.ffn_down.weight", 8192}})
	second := ggufFile(map[string]any{"split.no": uint32(1)}, [][2]any{{"blk.1.ffn_down.weight", 8192}, {"output.weight", 4096}})
	m, err := plan.ParseGGUF(bytes.NewReader(first), int64(len(first)))
	if err != nil {
		t.Fatal(err)
	}
	if m.Weights() != 4096+8192 {
		t.Fatalf("first file only: %d", m.Weights())
	}
	names := []string{"/m/X-00001-of-00003.gguf", "/m/X-00002-of-00003.gguf", "/m/X-00003-of-00003.gguf"}
	sizes := map[string]int64{names[0]: int64(len(first)), names[1]: int64(len(second))} // the third is missing
	opened := 0
	err = addRemoteShards(context.Background(), m, names, sizes, func(name string) (io.ReadCloser, func(), error) {
		opened++
		return io.NopCloser(bytes.NewReader(second)), func() {}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if opened != 1 || m.Weights() != 4096+8192+8192+4096 || m.LayerBytes[1] != 8192 {
		t.Fatalf("opened %d weights %d layers %v", opened, m.Weights(), m.LayerBytes)
	}
	if m.Shards == nil || len(m.Shards.Missing) != 1 || m.Shards.Missing[0] != 3 || m.Shards.FirstShard != names[0] {
		t.Fatalf("%+v", m.Shards)
	}
	if w := plan.ShardWarnings(m); len(w) != 1 || !strings.Contains(w[0], "missing") {
		t.Fatalf("%v", w)
	}
}

// A watchdog redeploy replaces the endpoint's own processes: the probe still sees their memory as used, which must
// not refuse the redeploy (it would burn the restarts); a fresh deploy of the same size is still refused.
func TestMemoryPreCheckReplaceTurnsRefusalIntoWarning(t *testing.T) {
	stubMeta(t, bigModel())
	probeMemory = func(_ context.Context, node string, _ bool) (MemFree, error) {
		return MemFree{RAMGiB: 110}, nil
	}
	cfg := config.Default()
	opts := DeployOptions{ModelPath: "/m/big.gguf", MasterNode: "dino", Port: 18090, Ctx: 4096}
	res, err := PlanDeployment(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := checkMemory(context.Background(), cfg, res, opts); err == nil {
		t.Fatal("a fresh deploy must still be refused")
	}
	opts.Replace = true
	rep, err := checkMemory(context.Background(), cfg, res, opts)
	if err != nil || len(rep.Blockers) != 0 {
		t.Fatalf("a replacing deploy must go on: %v %+v", err, rep)
	}
	warned := false
	for _, w := range rep.Warnings {
		warned = warned || strings.Contains(w, "replacing the previous run")
	}
	if !warned {
		t.Fatalf("the shortfall must still be shown: %v", rep.Warnings)
	}
}

// With --worker-cpu-experts the RING devices are renumbered (a worker's GPU, then its CPU): memNeeds must read the
// renumbered device->machine list, or the last worker's blocks fall out of the accounting.
func TestMemNeedsFollowsCPUExpertsRenumbering(t *testing.T) {
	m := bigModel()
	// workers A (cpu experts) and B: devList CUDA0,RING0(A gpu),RING2(B gpu),RING1(A cpu, ts 0)
	devs, ts := []string{"CUDA0", "RING0", "RING2", "RING1"}, "10,10,10,0"
	devNode := []string{"A", "A", "B"}
	got := map[string]int{}
	for _, n := range memNeeds(m, devs, ts, "m", devNode, 4096, 1, "", "") {
		got[n.Node] = n.Blocks
	}
	if got["B"] == 0 || got["A"] == 0 || got["m"] == 0 || got["m"]+got["A"]+got["B"] != 40 {
		t.Fatalf("every device's blocks must be accounted: %v", got)
	}
	// the old input (workers in RING_SERVERS order) loses B
	lost := map[string]int{}
	for _, n := range memNeeds(m, devs, ts, "m", []string{"A", "B"}, 4096, 1, "", "") {
		lost[n.Node] = n.Blocks
	}
	if lost["m"]+lost["A"]+lost["B"] == 40 && lost["B"] > 0 {
		t.Skip("old numbering happened to work for this layout")
	}
}

func TestMemNeedsHybridKVBlocksAndMoEVRAMBudget(t *testing.T) {
	m := bigModel()
	m.Recurrent = true
	m.KVBlocks = 10 // 10 of 40 blocks have attention KV
	m.Experts = 64
	m.ExpertBytes = make([]int64, 40)
	for i := range m.ExpertBytes {
		m.ExpertBytes[i] = 2800 << 20 // ~2.73 GiB of 3 GiB per block is experts
	}
	needs := memNeeds(m, []string{"CUDA0"}, "40", "victor", nil, 4096, 1, "q8_0", "q8_0")
	needs = applyMoECacheBudget(needs, m, "victor", true, 0, 3072)
	if len(needs) != 1 {
		t.Fatalf("%+v", needs)
	}
	// KV is scaled by 10/40 and q8_0 factor
	fullKV := float64(m.KVPerTokenLayer) * 4096 * 40 / gib * KVCacheFactor("q8_0", "q8_0")
	if d := needs[0].KVGiB - fullKV*0.25; d > 1e-6 || d < -1e-6 {
		t.Fatalf("KVGiB %.4f, want %.4f", needs[0].KVGiB, fullKV*0.25)
	}
	if needs[0].VRAMNeedGiB <= 0 || needs[0].MoECacheGiB != 3.0 || needs[0].ComputeBufGiB != plan.MoEComputeBufferGB {
		t.Fatalf("VRAM budget not populated: %+v", needs[0])
	}
	// When RAM+VRAM fits total weights (200 GiB RAM) but GPU VRAM (6 GiB) is smaller than VRAMNeedGiB (~16.2 GiB), evaluateMemory blocks
	rep := evaluateMemory(needs, map[string]MemFree{"victor": {RAMGiB: 200, VRAMGiB: 6}}, nil, config.Default(), false)
	if len(rep.Blockers) != 1 || !strings.Contains(rep.Blockers[0], "exceeds VRAM") {
		t.Fatalf("expected VRAM budget blocker, got %+v", rep)
	}
}

func TestKVCacheFactor(t *testing.T) {
	// each type's factor is looked up lower-cased and trimmed, and the result is the average of the K and V factors: (one(k) + one(v)) / 2
	cases := []struct {
		k, v string
		want float64
	}{
		{"", "", 1},
		{"f16", "f16", 1},
		{"bf16", "bf16", 1},
		{"f32", "f32", 2},
		{"q8_0", "q8_0", 34.0 / 32 / 2},
		{"q4_0", "q4_0", 18.0 / 32 / 2},
		{" Q8_0 ", " Q8_0 ", 34.0 / 32 / 2},
		// the mixed pair averages the two factors: one(q8_0)=34/32/2, one(q4_0)=18/32/2
		{"q8_0", "q4_0", (34.0/32/2 + 18.0/32/2) / 2},
		// an unknown type takes the default branch (1), like f16
		{"bogus", "bogus", 1},
		{"bogus", "f32", (1.0 + 2) / 2},
	}
	for _, c := range cases {
		if d := kvFactor(c.k, c.v) - c.want; d > 1e-9 || d < -1e-9 {
			t.Errorf("kvFactor(%q, %q) = %v, want %v", c.k, c.v, kvFactor(c.k, c.v), c.want)
		}
	}
	// the exported wrapper is the same figure
	if d := KVCacheFactor("q8_0", "q4_0") - kvFactor("q8_0", "q4_0"); d > 1e-9 || d < -1e-9 {
		t.Errorf("KVCacheFactor wrapper: %v", d)
	}
}
